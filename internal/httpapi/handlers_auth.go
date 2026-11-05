package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/auth"
)

type anonymousResponse struct {
	UserID    string    `json:"userId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Server) handleAnonymous(c *gin.Context) {
	token, hash, err := auth.NewToken()
	if err != nil {
		s.log.Error("生成 token 失败", "error", err, "request_id", c.GetString(ctxKeyRequestID))
		fail(c, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	expiresAt := time.Now().UTC().Add(s.cfg.TokenTTL())
	userID, err := s.store.CreateUserWithToken(c.Request.Context(), hash, expiresAt)
	if err != nil {
		s.storeFailure(c, "创建匿名用户失败", err)
		return
	}
	c.JSON(http.StatusCreated, anonymousResponse{UserID: userID, Token: token, ExpiresAt: expiresAt})
}

// handleLogin 预留给后续账号体系统一登录，当前明确返回 501。
func (s *Server) handleLogin(c *gin.Context) {
	fail(c, http.StatusNotImplemented, CodeNotImplemented, "账号密码登录尚未实现，请使用 /api/v1/auth/anonymous")
}

func (s *Server) handleLogout(c *gin.Context) {
	if err := s.store.RevokeToken(c.Request.Context(), tokenHash(c)); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.Status(http.StatusNoContent)
}
