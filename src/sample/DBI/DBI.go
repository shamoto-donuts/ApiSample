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
)

var (
	slaveWeights []int

	shardIds = [...]int{1, 2}
)

const (
	MASTER = iota
	SHARD
)

const (
	MODE_W   = "W"   // master
	MODE_R   = "R"   // slave
	MODE_BAK = "BAK" // backup
)

const (
	FOR_UPDATE = "FOR_UPDATE"
)

/**
 * コンテキストで一意にするためのキー
 */
type contextKey string

const (
	dbMasterW    contextKey = "dbMasterW"
	dbShardWMap             = "dbShardWMap"
	dbMasterRs              = "dbMasterRs"
	dbShardRMaps            = "dbShardRMaps"
	txMaster                = "txMaster"
	txShardMap              = "txShardMap"

	slaveIndex = "slaveIndex"
)

type DBIRepo struct {
}

func NewDBIRepo() *DBIRepo {
	return new(DBIRepo)
}

// masterは1つのハンドラをもち、slaveは複数のハンドラを持つ
// master
//  master *db
//  shard map[int]*db
// ----------------
// slave
//  master []*db
//  shard []map[int]*db
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
	ctx = context.WithValue(ctx, dbMasterW, masterW)
	ctx = context.WithValue(ctx, dbShardWMap, shardWMap)

	ctx = context.WithValue(ctx, dbMasterRs, masterRs)
	ctx = context.WithValue(ctx, dbShardRMaps, shardRMaps)

	// TODO:BAK MODE

	return ctx, err
}

func StartTx(c *gin.Context) {
	gc := c.Value("globalContext").(context.Context)
	dbShardWMap := gc.Value(dbShardWMap).(map[int]*gorp.DbMap)

	// すでに開始中の場合は何もしない
	iFace, valid := c.Get(txShardMap)
	if valid && iFace != nil {
		return
	}

	var txMap = map[int]*gorp.Transaction{}
	// txのマップを作成
	for k, v := range dbShardWMap {
		log.Info(k, " start tx!!")
		txMap[k], _ = v.Begin()
	}
	c.Set(txShardMap, txMap)
	// errを返す
}

func Commit(c *gin.Context) {
	txMap := c.Value(txShardMap).(map[int]*gorp.Transaction)
	for k, v := range txMap {
		log.Info(k, " commit!!")
		/*err :=*/ v.Commit()
		// txMap[k] = nil
	}
	c.Set(txShardMap, nil)
	// errを返す
}

func RollBack(c *gin.Context) {
	iFace, valid := c.Get(txShardMap)

	if valid && iFace != nil {
		txMap := iFace.(map[int]*gorp.Transaction)
		for _, v := range txMap {
			v.Rollback()
		}
		c.Set(txShardMap, nil)
	}
	// errを返す
}

// 使うslaveを決める
func DecideUseSlave() int {
	slaveIndex := rand.Intn(len(slaveWeights))
	return slaveWeights[slaveIndex]
}

// エラー表示
func checkErr(err error, msg string) {
	if err != nil {
		log.Error(msg, err)
	}
}

/**
 * get transaction function
 */
func GetTransaction(c *gin.Context, isShard bool, shardId int) (*gorp.Transaction, error) {
	var err error
	var tx *gorp.Transaction

	switch isShard {
	case true:
		// shard
		iFace, valid := c.Get(txShardMap)
		if valid {
			sMap := iFace.(map[int]*gorp.Transaction)
			tx = sMap[shardId]
		}

	case false:
		// master
		iFace, valid := c.Get(txMaster)
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
		conn = gc.Value(dbMasterW).(*gorp.DbMap)

	case MODE_R:
		slaveIndex := c.Value(slaveIndex).(int)
		masterRs := gc.Value(dbMasterRs).([]*gorp.DbMap)
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
		shardMap = gc.Value(dbShardWMap).(map[int]*gorp.DbMap)

	case MODE_R:
		slaveIndex := c.Value(slaveIndex).(int)
		dbShardRMaps := gc.Value(dbShardRMaps).([]map[int]*gorp.DbMap)
		shardMap = dbShardRMaps[slaveIndex]

	case MODE_BAK:
	// TODO:実装

	default:
		err = errors.New("invalid mode!!")
	}
	return shardMap, err
}
