package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

const maxPushBodyBytes = 1 << 20 // 1 MiB

func (s *Server) handlePush(c *gin.Context) {
	idemKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idemKey) > maxIdempotencyKey {
		fail(c, http.StatusBadRequest, CodeBadRequest, "Idempotency-Key 长度不能超过 128")
		return
	}

	raw, err := c.GetRawData()
	if err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "读取请求体失败")
		return
	}
	if len(raw) > maxPushBodyBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "请求体过大")
		return
	}
	// GetRawData 会读完 body；重置后交给 ShouldBindJSON 做 JSON 绑定与 validator 校验。
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	var payload pushPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		failValidation(c, err)
		return
	}
	req := payload.toModel()
	if err := normalizePush(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}

	hash := sha256.Sum256(raw)
	result, replayed, err := s.store.Push(c.Request.Context(), userID(c), req, store.PushOptions{
		IdempotencyKey: idemKey,
		RequestHash:    hex.EncodeToString(hash[:]),
	})
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	if replayed {
		c.Header("Idempotent-Replayed", "true")
	}
	if result.Applied == nil {
		result.Applied = []model.AppliedChange{}
	}
	if result.Conflicts == nil {
		result.Conflicts = []model.Conflict{}
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) handleChanges(c *gin.Context) {
	var query changesQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		failValidation(c, err)
		return
	}
	cursor, err := parseCursor(query.Cursor)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	limit := 100
	if query.Limit != nil {
		limit = *query.Limit
	}

	page, err := s.store.Changes(c.Request.Context(), userID(c), cursor, limit)
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	if page.Changes == nil {
		page.Changes = []model.Change{}
	}
	// change_log 的 user_device payload 是写入时的快照；presence 是时间相关推导，
	// 必须在响应生成时按当前时间重算，避免客户端拿到「写入时 online、回放时已离线」
	// 的错误状态。重新计算只影响 presence 一个字段，payload 其余内容保持原样。
	now := time.Now().UTC()
	for i := range page.Changes {
		if page.Changes[i].Entity != "user_device" {
			continue
		}
		var d model.UserDevice
		if err := json.Unmarshal(page.Changes[i].Payload, &d); err != nil {
			continue
		}
		d.Presence = model.DerivePresence(now, d.LastSeenAt)
		if raw, err := json.Marshal(d); err == nil {
			page.Changes[i].Payload = raw
		}
	}
	c.JSON(http.StatusOK, page)
}
