package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/model"
)

// handleRefreshToken 用当前有效 token 换取新 token（滚动轮换，旧 token 立即失效）。
//
// 选择旋转而不是保留旧 token 到过期：180 天 TTL 太长，如果旧 token 继续可用，
// 刷新只是多签发一个凭据，不能缩小设备丢失/泄漏后的攻击窗口。旋转后旧 token
// 立刻 401，且刷新接口无法把已吊销的 token 复活（吊销是数据库事实）。
// 并发刷新同一 token 时，只有第一个事务能成功，其余读到 revoked_at 非空返回 401。
func (s *Server) handleRefreshToken(c *gin.Context) {
	token, hash, err := auth.NewToken()
	if err != nil {
		s.storeFailure(c, "生成 token 失败", err)
		return
	}
	now := time.Now().UTC()
	expiresAt := now.Add(s.cfg.TokenTTL())
	userID, err := s.store.RefreshToken(c.Request.Context(), tokenHash(c), hash, now, expiresAt)
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusOK, anonymousResponse{UserID: userID, Token: token, ExpiresAt: expiresAt})
}

// handleListSessions 返回当前账号全部有效会话；只暴露会话 id 与时间，不暴露 token。
func (s *Server) handleListSessions(c *gin.Context) {
	sessions, err := s.store.ListSessions(c.Request.Context(), userID(c), tokenHash(c))
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusOK, struct {
		Sessions []model.Session `json:"sessions"`
	}{Sessions: sessions})
}

// handleRevokeSession 吊销指定会话；跨用户与不存在统一 404，不泄露会话是否存在。
// 已吊销的会话重复吊销按幂等成功返回 204。
func (s *Server) handleRevokeSession(c *gin.Context) {
	if err := s.store.RevokeSession(c.Request.Context(), userID(c), c.Param("id"), time.Now().UTC()); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.Status(http.StatusNoContent)
}
