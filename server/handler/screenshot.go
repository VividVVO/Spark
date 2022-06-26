package handler

import (
	"github.com/VividVVO/Spark/modules"
	"github.com/VividVVO/Spark/server/common"
	"github.com/VividVVO/Spark/utils"
	"github.com/VividVVO/Spark/utils/melody"
	"github.com/gin-gonic/gin"
	"net/http"
	"time"
)

// getScreenshot will call client to screenshot.
func getScreenshot(ctx *gin.Context) {
	target, ok := checkForm(ctx, nil)
	if !ok {
		return
	}
	bridgeID := utils.GetStrUUID()
	trigger := utils.GetStrUUID()
	wait := make(chan bool)
	called := false
	common.SendPackByUUID(modules.Packet{Code: 0, Act: `screenshot`, Data: gin.H{`bridge`: bridgeID}, Event: trigger}, target)
	common.AddEvent(func(p modules.Packet, _ *melody.Session) {
		wait <- false
		called = true
		removeBridge(bridgeID)
		common.RemoveEvent(trigger)
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, modules.Packet{Code: 1, Msg: p.Msg})
	}, target, trigger)
	instance := addBridgeWithDst(nil, bridgeID, ctx)
	instance.OnPush = func(bridge *bridge) {
		called = true
		common.RemoveEvent(trigger)
		ctx.Header(`Content-Type`, `image/png`)
	}
	instance.OnFinish = func(bridge *bridge) {
		wait <- false
	}
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		if !called {
			removeBridge(bridgeID)
			common.RemoveEvent(trigger)
			ctx.AbortWithStatusJSON(http.StatusGatewayTimeout, modules.Packet{Code: 1, Msg: `${i18n|responseTimeout}`})
		} else {
			<-wait
		}
	}
	close(wait)
}
