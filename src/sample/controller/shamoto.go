package controller

import (
	"sample/model"

	"net/http"
	"sample/DBI"

	"time"

	log "github.com/cihub/seelog"
	"github.com/gin-gonic/gin"
	"github.com/k0kubun/pp"
)

func Shamoto(c *gin.Context) {
	defer DBI.RollBack(c)

	option := model.Option{"for_update": 1}
	userRepo := model.NewUserRepo()
	user := userRepo.FindById(c, 3, option)

	// unixtimeに変換
	log.Debug(time.Now())
	log.Debug(user.CreatedAt.Unix())
	log.Debug(user.UpdatedAt.Location())

	log.Info(user)

	user.Score += 1000
	userRepo.Update(c, user)

	option = model.Option{"mode": DBI.MODE_W}
	logRepo := model.NewUserTestLogRepo()
	userLog := logRepo.FindByID(c, 1, option)
	if userLog == nil {
		log.Error("log not found!")
	}
	log.Info(pp.Println(userLog))

	DBI.Commit(c)

	c.String(http.StatusOK, "hi!!")
}
