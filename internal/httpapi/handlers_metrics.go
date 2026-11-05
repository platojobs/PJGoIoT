package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
)

// handleMetrics 输出 Prometheus 文本格式指标。
// 业务计数为近似值（pg_class.reltuples），不暴露精确业务数据。
func (s *Server) handleMetrics(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	counts, err := s.store.ApproxCounts(ctx)
	if err != nil {
		// 近似计数失败不影响指标端点可用性，输出零值并计数数据库错误。
		s.metrics.incDBErrors()
		s.log.Warn("查询近似行数失败", "error", err, "request_id", c.GetString(ctxKeyRequestID))
		counts = model.StoreCounts{}
	}
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.String(http.StatusOK, s.metrics.render(counts))
}
