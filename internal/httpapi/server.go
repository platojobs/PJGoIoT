package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// Server 持有路由所需的依赖。
type Server struct {
	store   store.Store
	log     *slog.Logger
	cfg     config.Config
	limiter *ipLimiter
}

// New 构建基于 Gin 的 HTTP handler（含全局中间件与路由分组）。
func New(cfg config.Config, st store.Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	s := &Server{
		store:   st,
		log:     logger,
		cfg:     cfg,
		limiter: newIPLimiter(cfg.RateLimitPerMin, cfg.RateLimitBurst),
	}

	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.Use(s.globalMiddleware()...)
	s.registerRoutes(r)
	return r
}

// registerRoutes 使用 Gin 路由分组组织接口：/auth、/devices、/device-types、/my、/sync。
func (s *Server) registerRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")

	api.GET("/health", s.handleHealth)

	authGroup := api.Group("/auth")
	authGroup.POST("/anonymous", s.handleAnonymous)
	authGroup.POST("/login", s.handleLogin)
	authGroup.POST("/logout", s.requireAuth(), s.handleLogout)

	devicesGroup := api.Group("/devices")
	devicesGroup.GET("/:mac", s.handlePublicDevice)

	deviceTypesGroup := api.Group("/device-types")
	deviceTypesGroup.GET("", s.handleDeviceTypes)
	deviceTypesGroup.GET("/:type", s.handleDeviceType)
	deviceTypesGroup.GET("/:type/catalog", s.handleDeviceCatalog)
	deviceTypesGroup.GET("/:type/firmwares", s.handleFirmwares)
	deviceTypesGroup.GET("/:type/accessories", s.handleAccessories)

	myGroup := api.Group("/my", s.requireAuth())
	myGroup.GET("/devices", s.handleMyDevices)
	// Gin 不允许同一段同时存在 :mac 与 :platform/:externalId 两个参数节点，
	// 因此用 catch-all 统一分发，对外仍保持 /devices/{mac} 与 /devices/{platform}/{externalId} 两种路径。
	myGroup.GET("/devices/*key", s.handleMyDeviceRoute)

	syncGroup := api.Group("/sync", s.requireAuth())
	syncGroup.GET("/changes", s.handleChanges)
	syncGroup.POST("/push", s.handlePush)

	r.NoRoute(func(c *gin.Context) {
		writeError(c.Writer, http.StatusNotFound, CodeNotFound, "接口不存在")
	})
	r.NoMethod(func(c *gin.Context) {
		writeError(c.Writer, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "请求方法不被支持")
	})
}

func (s *Server) handleHealth(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.Error("健康检查失败", "error", err, "request_id", c.GetString(ctxKeyRequestID))
		fail(c, http.StatusServiceUnavailable, CodeUnavailable, "存储不可用")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "pjgoiot-api",
		"version": s.cfg.Version,
		"store":   s.store.Backend(),
		"time":    time.Now().UTC(),
	})
}
