package model

/**
 * @file userShard.go
 * @brief userShardテーブル操作
 */

import (
	"errors"
	builder "github.com/Masterminds/squirrel"
	log "github.com/cihub/seelog"
	"github.com/gin-gonic/gin"
	"sample/DBI"
)

/**
 *
 */

type shardingType int

const (
	USER shardingType = iota
	GROUP
)

// table
type UserShard struct {
	Id      int
	ShardId int `db:"shard_id"`
}

// user shard
/////////////////////////////
type UserShardRepo interface {
	Find(*gin.Context, shardingType, interface{}) (int, error)
}

func NewUserShardRepo() UserShardRepo {
	return UserShardRepoImpl{}
}

type UserShardRepoImpl struct {
}

//
func (r UserShardRepoImpl) Find(c *gin.Context, st shardingType, value interface{}) (int, error) {
	var shardId int
	var err error

	switch st {
	case USER:
		// ハンドル取得
		conn, err := DBI.GetDBMasterConnection(c, DBI.MODE_R)
		if err != nil {
			log.Error("not found master connection!!")
			break
		}

		// user_shardを検索
		sql, args, err := builder.Select("id, shard_id").From("user_shard").Where("id = ?", value).ToSql()
		if err != nil {
			log.Error("query build error!!")
			break
		}

		var us = new(UserShard)
		err = conn.SelectOne(us, sql, args...)
		if err != nil {
			log.Info("not found user shard id")
			break
		}
		shardId = us.ShardId

	case GROUP:
	// TODO:実装
	default:
		err = errors.New("undefined shard type!!")
	}

	return shardId, err
}
