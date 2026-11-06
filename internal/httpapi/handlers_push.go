package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
)

type alertCreatedResponse struct {
	Alert         model.Alert `json:"alert"`
	DeliveryCount int         `json:"deliveryCount"`
}

// handleRegisterPushDevice 注册设备推送令牌；同一用户同一 token 幂等更新。
func (s *Server) handleRegisterPushDevice(c *gin.Context) {
	var input model.PushRegistrationInput
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	device, err := s.store.UpsertPushDevice(c.Request.Context(), userID(c), input, time.Now().UTC())
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusOK, device)
}

// handleDeactivatePushDevice 把令牌置为 inactive（不硬删）；已停用时幂等成功。
func (s *Server) handleDeactivatePushDevice(c *gin.Context) {
	if err := s.store.DeactivatePushDevice(c.Request.Context(), userID(c), c.Param("deviceId"), time.Now().UTC()); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.Status(http.StatusNoContent)
}

// handleCreateAlert 由运营侧创建告警，并在同一事务内为全部 active 令牌入队投递。
// 当前服务端没有内建告警产生者，先用该入口打通「告警 → 队列 → worker → 推送适配层」链路。
func (s *Server) handleCreateAlert(c *gin.Context) {
	var input model.AlertInput
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	input.UserID = strings.TrimSpace(input.UserID)
	input.Kind = strings.TrimSpace(input.Kind)
	input.Title = strings.TrimSpace(input.Title)
	input.Body = strings.TrimSpace(input.Body)
	input.Severity = strings.TrimSpace(strings.ToLower(input.Severity))
	if input.Severity == "" {
		input.Severity = "info"
	}
	if input.UserID == "" || input.Kind == "" || len(input.Kind) > 64 {
		fail(c, http.StatusBadRequest, CodeAlertInvalid, "userId 与 kind 必填，kind 最长 64 字符")
		return
	}
	if input.Title == "" || len(input.Title) > 200 {
		fail(c, http.StatusBadRequest, CodeAlertInvalid, "title 必填且最长 200 字符")
		return
	}
	if len(input.Body) > 2000 {
		fail(c, http.StatusBadRequest, CodeAlertInvalid, "body 最长 2000 字符")
		return
	}
	switch input.Severity {
	case "info", "warning", "critical":
	default:
		fail(c, http.StatusBadRequest, CodeAlertInvalid, "severity 只能是 info、warning 或 critical")
		return
	}
	if len(input.Payload) > 20 {
		fail(c, http.StatusBadRequest, CodeAlertInvalid, "payload 最多 20 个键")
		return
	}
	for k, v := range input.Payload {
		if len(k) == 0 || len(k) > 64 || len(v) > 1024 {
			fail(c, http.StatusBadRequest, CodeAlertInvalid, "payload 键最长 64、值最长 1024 字符")
			return
		}
	}

	alert, count, err := s.store.CreateAlertWithDeliveries(c.Request.Context(), input, time.Now().UTC())
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusCreated, alertCreatedResponse{Alert: alert, DeliveryCount: count})
}
