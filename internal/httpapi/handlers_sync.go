package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

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
	c.JSON(http.StatusOK, page)
}
