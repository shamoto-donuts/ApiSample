package DBI

import (
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/net/context"
	"math/rand"
	"sample/conf/gameConf"
	"strconv"

	"database/sql"
	"errors"
	log "github.com/cihub/seelog"
	"github.com/gin-gonic/gin"
	"gopkg.in/gorp.v1"
	ckey "sample/conf/context"
)

var (
	slaveWeights []int
	shardIds     []int
)

// TODO:この辺ちゃんとする
const (
	MODE_W   = "W"   // master
	MODE_R   = "R"   // slave
	MODE_BAK = "BAK" // backup
)

const (
	FOR_UPDATE = "FOR_UPDATE"
)

type DBIRepo struct {
}

func NewDBIRepo() *DBIRepo {
	return new(DBIRepo)
}

/**************************************************************************************************/
/*!
 *  dbハンドラを生成する
 *
 *  masterは1つのハンドラをもち、slaveは複数のハンドラを持つ
 *  master
 *   master *db
 *   shard map[int]*db
 * ----------------
 *  slave
 *   master []*db
 *   shard []map[int]*db
 *
 *
 *  \param   ctx : グローバルなコンテキスト
 *  \return  ハンドラ登録済みのコンテキスト、エラー
 */
/**************************************************************************************************/
func BuildInstances(ctx context.Context) (context.Context, error) {
	var err error

	gc := ctx.Value("gameConf").(*gameConf.GameConfig)

	// gorpのオブジェクトを取得
	getGorp := func(dbConf gameConf.DbConfig, host, port, dbName string) (*gorp.DbMap, error) {

		dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8", dbConf.User, dbConf.Pass, host, port, dbName)

		db, err := sql.Open("mysql", dsn)
		if err != nil {
			log.Critical(err)
		}

		// construct a gorp DbMap
		dbmap := &gorp.DbMap{Db: db, Dialect: gorp.MySQLDialect{"InnoDB", "UTF8"}}
		return dbmap, err
	}

	// make shards
	for i := 0; i < gc.Db.Shard; i++ {
		shardIds = append(shardIds, i+1)
	}

	// master - master
	masterW, err := getGorp(gc.Db, gc.Server.Host, gc.Server.Port, "game_master")
	if err != nil {
		log.Critical("master : game_master setup failed!!")
		return ctx, err
	}

	// master - shard
	var shardWMap = map[int]*gorp.DbMap{}
	for _, shardId := range shardIds {
		// database
		dbName := "game_shard_" + strconv.Itoa(shardId)

		// mapping
		shardWMap[shardId], err = getGorp(
			gc.Db,
			gc.Server.Host,
			gc.Server.Port,
			dbName)

		// error
		if err != nil {
			log.Critical("master : " + dbName + " setup failed!!")
			return ctx, err
		}
	}

	// read-only database
	// slave
	var masterRs []*gorp.DbMap
	var shardRMaps []map[int]*gorp.DbMap
	for slave_index, slaveConf := range gc.Server.Slave {
		///////////////////////////////////
		// MASTER
		// mapping
		masterR, err := getGorp(
			gc.Db,
			slaveConf.Host,
			slaveConf.Port,
			"game_master")

		// error
		if err != nil {
			log.Critical("slave : game_master setup failed!!")
			return ctx, err
		}

		// add slave masters
		masterRs = append(masterRs, masterR)

		///////////////////////////////////
		// SHARD
		var shardMap = map[int]*gorp.DbMap{}

		for _, shardId := range shardIds {
			// database
			dbName := "game_shard_" + strconv.Itoa(shardId)

			// mapping
			shardMap[shardId], err = getGorp(
				gc.Db,
				slaveConf.Host,
				slaveConf.Port,
				dbName)

			// error
			if err != nil {
				log.Critical("slave : " + dbName + " setup failed!!")
				return ctx, err
			}
		}
		shardRMaps = append(shardRMaps, shardMap)

		// slaveの選択比重
		for i := 0; i < slaveConf.Weight; i++ {
			slaveWeights = append(slaveWeights, slave_index)
		}
	}

	// contextに設定
	ctx = context.WithValue(ctx, ckey.DbMasterW, masterW)
	ctx = context.WithValue(ctx, ckey.DbShardWMap, shardWMap)

	ctx = context.WithValue(ctx, ckey.DbMasterRs, masterRs)
	ctx = context.WithValue(ctx, ckey.DbShardRMaps, shardRMaps)

	// TODO:BAK MODE

	return ctx, err
}

/**************************************************************************************************/
/*!
 *  リクエスト中に使用するslaveを決める
 *
 *  \return  使用するslaveのindex
 */
/**************************************************************************************************/
func DecideUseSlave() int {
	slaveIndex := rand.Intn(len(slaveWeights))
	return slaveWeights[slaveIndex]
}

/**
 * BEGIN function
 */
/**************************************************************************************************/
/*!
 *  masterでトランザクションを開始する
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func MasterTxStart(c *gin.Context) error {
	var err error

	// すでに開始中の場合は何もしない
	if isTransactonStart(c, ckey.IsMasterTxStart) {
		return err
	}

	// dbハンドル取得
	gc := c.Value("globalContext").(context.Context)
	dbMap := gc.Value(ckey.DbMasterW).(*gorp.DbMap)

	// transaction start
	var tx *gorp.Transaction
	tx, err = dbMap.Begin()
	if err != nil {
		return err
	}

	// リクエストコンテキストに保存
	c.Set(ckey.TxMaster, tx)
	c.Set(ckey.IsMasterTxStart, true)

	return err
}

/**************************************************************************************************/
/*!
 *  すべてのshardでトランザクションを開始する
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func ShardAllTxStart(c *gin.Context) error {
	var err error

	// すでに開始中の場合は何もしない
	if isTransactonStart(c, ckey.IsShardTxStart) {
		return err
	}

	// dbハンドルマップを取得
	gc := c.Value("globalContext").(context.Context)
	dbShardWMap := gc.Value(ckey.DbShardWMap).(map[int]*gorp.DbMap)

	var txMap = map[int]*gorp.Transaction{}
	// txのマップを作成
	for k, v := range dbShardWMap {
		log.Info(k, " start tx!!")
		txMap[k], err = v.Begin()

		// エラーが起きた時点でおかしいのでreturn
		if err != nil {
			return err
		}
	}

	// リクエストコンテキストに保存
	c.Set(ckey.TxShardMap, txMap)
	c.Set(ckey.IsShardTxStart, true)

	return err
}

/**
 * COMMIT function
 */
/**************************************************************************************************/
/*!
 *  開始した全てのtransactionをcommitする
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func Commit(c *gin.Context) error {
	err := masterCommit(c)
	err = shardCommit(c)
	return err
}

/**************************************************************************************************/
/*!
 *  masterの開始したtransactionをcommitする
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func masterCommit(c *gin.Context) error {
	var err error
	iFace, valid := c.Get(ckey.TxMaster)

	if valid && iFace != nil {
		tx := iFace.(*gorp.Transaction)
		err = tx.Commit()

		// エラーじゃなければ削除
		if err == nil {
			c.Set(ckey.TxMaster, nil)
			c.Set(ckey.IsMasterTxStart, false)
		}
	}
	return err
}

/**************************************************************************************************/
/*!
 *  shardの開始したtransactionをcommitする
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func shardCommit(c *gin.Context) error {
	var err error
	var hasError = false

	iFace, valid := c.Get(ckey.TxShardMap)

	if valid && iFace != nil {
		// 取得してすべてcommitする
		txMap := iFace.(map[int]*gorp.Transaction)
		for k, v := range txMap {
			log.Debug(k, " commit!!")
			err = v.Commit()
			// 正常な場合、削除する
			if err == nil {
				delete(txMap, k)
			}
		}

		// エラーが起きてなければ削除
		if !hasError {
			c.Set(ckey.TxShardMap, nil)
			c.Set(ckey.IsShardTxStart, false)
		}
	}
	return err
}

/**
 * ROLLBACK function
 */
/**************************************************************************************************/
/*!
 *  開始した全てのtransactionをrollbackする
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func RollBack(c *gin.Context) error {
	err := masterRollback(c)
	err = shardRollback(c)
	return err
}

/**************************************************************************************************/
/*!
 *  masterの開始したtransactionをrollbackする
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func masterRollback(c *gin.Context) error {
	var err error
	iFace, valid := c.Get(ckey.TxMaster)

	if valid && iFace != nil {
		tx := iFace.(*gorp.Transaction)
		err = tx.Rollback()

		// エラーじゃなければ削除
		if err == nil {
			c.Set(ckey.TxMaster, nil)
		}
	}
	return err
}

/**************************************************************************************************/
/*!
 *  shardの開始したtransactionをrollbackする
 *
 *  \param   c : コンテキスト
 *  \return  エラー
 */
/**************************************************************************************************/
func shardRollback(c *gin.Context) error {
	var err error
	var hasError = false

	iFace, valid := c.Get(ckey.TxShardMap)

	if valid && iFace != nil {
		// 取得してすべてrollbackする
		txMap := iFace.(map[int]*gorp.Transaction)
		for k, v := range txMap {
			log.Debug(k, " rollback!!")
			err = v.Rollback()
			// 正常な場合、削除する
			if err == nil {
				delete(txMap, k)
			}
		}

		// エラーが起きてなければ削除
		if !hasError {
			c.Set(ckey.TxShardMap, nil)
		}
	}
	return err
}

/**
 * get transaction function
 */
/**************************************************************************************************/
/*!
 *  トランザクションを取得する(開始してない場合、開始する)
 *
 *  \param   c       : コンテキスト
 *  \param   isShard : trueの場合shardのDBハンドルを取得する
 *  \param   shardId : 存在するshard ID
 *  \return  トランザクション、エラー
 */
/**************************************************************************************************/
func GetTransaction(c *gin.Context, isShard bool, shardId int) (*gorp.Transaction, error) {
	var err error
	var tx *gorp.Transaction

	switch isShard {
	case true:
		// shard
		// トランザクションを開始してない場合、開始する
		if !isTransactonStart(c, ckey.IsShardTxStart) {
			err = ShardAllTxStart(c)

			if err != nil {
				log.Error("shard transaction start failed!!")
				return tx, err
			}
		}
		// shard
		iFace, valid := c.Get(ckey.TxShardMap)
		if valid && iFace != nil {
			sMap := iFace.(map[int]*gorp.Transaction)
			tx = sMap[shardId]
		}

	case false:
		// master
		// トランザクションを開始してない場合、開始する
		if isTransactonStart(c, ckey.IsMasterTxStart) {
			err = MasterTxStart(c)

			if err != nil {
				log.Error("master transaction start failed!!")
				return tx, err
			}
		}
		// master
		iFace, valid := c.Get(ckey.TxMaster)
		if valid && iFace != nil {
			tx = iFace.(*gorp.Transaction)
		}

	default:
		// to do nothing
	}

	if tx == nil {
		err = errors.New("not found transaction!!")
		log.Error(err)
	}

	return tx, err
}

/**************************************************************************************************/
/*!
 *  トランザクションを開始したか確認する
 *
 *  \param   c      : コンテキスト
 *  \param   key    : コンテキスト内キー
 *  \return  true/false
 */
/**************************************************************************************************/
func isTransactonStart(c *gin.Context, key string) bool {
	iFace, valid := c.Get(key)
	if valid && iFace != nil {
		return iFace.(bool)
	}
	return false
}

/**
 * get db connection function
 */
/**************************************************************************************************/
/*!
 *  各DBへのハンドルを取得する
 *
 *  \param   c       : コンテキスト
 *  \param   mode    : W, R, BAK
 *  \param   isShard : trueの場合shardのDBハンドルを取得する
 *  \param   shardId : 存在するshard ID
 *  \return  DBハンドル、エラー
 */
/**************************************************************************************************/
func GetDBConnection(c *gin.Context, mode string, isShard bool, shardId int) (*gorp.DbMap, error) {
	var err error
	var conn *gorp.DbMap

	switch isShard {
	case true:
		// shard
		conn, err = GetDBShardConnection(c, mode, shardId)

	case false:
		// master
		conn, err = GetDBMasterConnection(c, mode)

	default:
		// to do nothing
	}

	if conn == nil {
		err = errors.New("not found db connection!!")
	}
	return conn, err
}

/**************************************************************************************************/
/*!
 *  masterのDBハンドルを取得する
 *
 *  \param   c : コンテキスト
 *  \param   mode : W, R, BAK
 *  \return  DBハンドル、エラー
 */
/**************************************************************************************************/
func GetDBMasterConnection(c *gin.Context, mode string) (*gorp.DbMap, error) {
	var conn *gorp.DbMap
	var err error

	gc := c.Value("globalContext").(context.Context)

	switch mode {
	case MODE_W:
		conn = gc.Value(ckey.DbMasterW).(*gorp.DbMap)

	case MODE_R:
		slaveIndex := c.Value(ckey.SlaveIndex).(int)
		masterRs := gc.Value(ckey.DbMasterRs).([]*gorp.DbMap)
		conn = masterRs[slaveIndex]

	case MODE_BAK:
	// TODO:実装

	default:
		err = errors.New("invalid mode!!")
	}

	//
	if conn == nil {
		err = errors.New("connection is nil!!")
	}

	return conn, err
}

/**************************************************************************************************/
/*!
 *  指定したShardIDのハンドルを取得する
 *
 *  \param   c : コンテキスト
 *  \param   mode : W, R, BAK
 *  \param   shardId : shard ID
 *  \return  DBハンドル、エラー
 */
/**************************************************************************************************/
func GetDBShardConnection(c *gin.Context, mode string, shardId int) (*gorp.DbMap, error) {
	var conn *gorp.DbMap
	var err error

	shardMap, err := GetDBShardMap(c, mode)
	if err != nil {
		return nil, err
	}
	conn = shardMap[shardId]

	return conn, err
}

/**************************************************************************************************/
/*!
 *  ShardのDBハンドルマップを取得する
 *
 *  \param   c : コンテキスト
 *  \param   mode : W, R, BAK
 *  \return  DBハンドルマップ、エラー
 */
/**************************************************************************************************/
func GetDBShardMap(c *gin.Context, mode string) (map[int]*gorp.DbMap, error) {
	var err error
	var shardMap map[int]*gorp.DbMap

	gc := c.Value("globalContext").(context.Context)

	switch mode {
	case MODE_W:
		shardMap = gc.Value(ckey.DbShardWMap).(map[int]*gorp.DbMap)

	case MODE_R:
		slaveIndex := c.Value(ckey.SlaveIndex).(int)
		dbShardRMaps := gc.Value(ckey.DbShardRMaps).([]map[int]*gorp.DbMap)
		shardMap = dbShardRMaps[slaveIndex]

	case MODE_BAK:
	// TODO:実装

	default:
		err = errors.New("invalid mode!!")
	}
	return shardMap, err
}
