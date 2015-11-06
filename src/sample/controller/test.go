package controller

import (
	"net/http"
	"sample/DBI"
	"sample/model"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	log "github.com/cihub/seelog"
	"github.com/garyburd/redigo/redis"
	"github.com/gin-gonic/gin"
	"github.com/go-xorm/core"
	"golang.org/x/net/context"
)

// JSON from POST
type PostJSON struct {
	Name  string `json:"Name" binding:"required"`
	Score int    `json:"Score" binding:"required"`
}

type postData struct {
	Name  string `json:"Name"`
	Score int    `json:"Score"`
}

func Test(c *gin.Context) {

	var json PostJSON
	err := c.BindJSON(&json)
	if checkErr(c, err, "json error") {
		return
	}

	ctx := c.Value("globalContext").(context.Context)

	// データをselect
	res, _ := model.Find(c, 3)
	log.Info(res)

	// use redis
	redisTest(ctx)

	// データをupdate
	DBI.StartTx(c)
	defer DBI.RollBack(c)

	tx, err := DBI.GetDBSession(c)
	if checkErr(c, err, "begin error") {
		return
	}

	var u []model.User
	err = tx.Where("id = ?", 3).ForUpdate().Find(&u)
	if checkErr(c, err, "user not found") {
		return
	}

	user := u[0]
	user.Score += 1

	//time.Sleep(3 * time.Second)

	//res, e := session.Id(user.Id).Cols("score").Update(&user) // 単一 PK
	_, err = tx.Id(core.PK{user.Id, user.Name}).Update(&user) // 複合PK
	if checkErr(c, err, "update error") {
		return
	}

	DBI.Commit(c)

	/*
		err = session.Commit()
		if checkErr(c, err, "commit error") {
			return
		}*/

	c.JSON(http.StatusOK, &user)
}

func TokenTest(c *gin.Context) {

	var hoge postData
	data := c.PostForm("data")
	dd := []byte(data)
	json.Unmarshal(dd, &hoge)
	log.Info(hoge)

	token := c.PostForm("token")
	log.Info(token)

	// tokenをjsonにもどす
	tokenData, _ := base64.StdEncoding.DecodeString(token)

	var d postData
	err := json.Unmarshal(tokenData, &d)
	log.Info(d)

	checkErr(c, err, "token test error")

	// sha256
	recv_sha := c.PostForm("sha")
	log.Info(recv_sha)

	hash := hmac.New(sha256.New, []byte("secret_key"))
	hash.Write([]byte("apple"))
	hashsum := fmt.Sprintf("%x", hash.Sum(nil))
	log.Infof(hashsum)

	if recv_sha == hashsum {
		log.Info("sha correct!!")
	}

	c.JSON(http.StatusOK, gin.H{"message": "hello"})
}

func redisTest(ctx context.Context) {

	redis_pool := ctx.Value("redis").(*redis.Pool)
	redis_conn := redis_pool.Get()

	_, e2 := redis_conn.Do("SET", "message", "this is value")
	if e2 != nil {
		log.Error("set message", e2)
	}
	s, err := redis.String(redis_conn.Do("GET", "message"))
	if err != nil {
		log.Error("get message", err)
	}
	log.Info("%#v\n", s)
}

// エラー表示
func checkErr(c *gin.Context, err error, msg string) bool {
	if err != nil {
		log.Error(msg, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msg})
		return true
	}
	return false
}
