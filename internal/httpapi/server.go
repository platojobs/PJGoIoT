package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// Server 持有路由所需的依赖。
type Server struct {
	store        store.Store
	log          *slog.Logger
	cfg          config.Config
	limiter      rateLimiter
	claimLimiter *ipLimiter
	metrics      *metricsRegistry
	// draining 为 true 时 readiness 立即返回 503，用于滚动发布时先摘流量再退出。
	draining *atomic.Bool
}

// New 构建基于 Gin 的 HTTP handler（含全局中间件与路由分组）。
// draining 为 nil 表示不参与下线下线流程（测试与单次运行使用）。
func New(cfg config.Config, st store.Store, logger *slog.Logger, draining *atomic.Bool) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	s := &Server{
		store:        st,
		log:          logger,
		cfg:          cfg,
		limiter:      newRateLimiter(cfg, st, logger),
		claimLimiter: newIPLimiter(cfg.PairingClaimPerMin, cfg.PairingClaimBurst),
		metrics:      newMetricsRegistry(),
		draining:     draining,
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
	// /healthz 仅用于 liveness：只证明进程在跑，不访问数据库；readiness 必须用 /api/v1/health。
	r.GET("/healthz", s.handleLiveness)

	authGroup := api.Group("/auth")
	authGroup.POST("/anonymous", s.handleAnonymous)
	authGroup.POST("/login", s.handleLogin)
	authGroup.POST("/logout", s.requireAuth(), s.handleLogout)
	authGroup.POST("/pairing-code", s.requireAuth(), s.handleCreatePairingCode)
	authGroup.POST("/pairing-code/claim", s.claimRateLimit(), s.handleClaimPairingCode)

	meGroup := api.Group("/me", s.requireAuth())
	meGroup.GET("/export", s.handleExport)
	meGroup.DELETE("", s.handleDeleteMe)

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

	households := api.Group("/households", s.requireAuth())
	households.GET("", s.handleListHouseholds)
	households.POST("", s.handleCreateHousehold)
	households.POST("/invitations/claim", s.claimRateLimit(), s.handleClaimHouseholdInvitation)
	households.GET("/:id", s.handleGetHousehold)
	households.PATCH("/:id", s.handlePatchHousehold)
	households.DELETE("/:id", s.handleDeleteHousehold)
	households.GET("/:id/members", s.handleListHouseholdMembers)
	households.POST("/:id/invitations", s.handleCreateHouseholdInvitation)
	households.GET("/:id/rooms", s.handleListHouseholdRooms)
	households.POST("/:id/rooms", s.handleCreateHouseholdRoom)
	households.PATCH("/:id/rooms/:roomId", s.handlePatchHouseholdRoom)
	households.DELETE("/:id/rooms/:roomId", s.handleDeleteHouseholdRoom)

	myGroup.PATCH("/devices/*key", s.handlePatchMyDevice)

	if s.cfg.MetricsEnabled {
		r.GET("/metrics", s.handleMetrics)
	}

	r.NoRoute(func(c *gin.Context) {
		writeError(c.Writer, http.StatusNotFound, CodeNotFound, "接口不存在")
	})
	r.NoMethod(func(c *gin.Context) {
		writeError(c.Writer, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "请求方法不被支持")
	})
}

// handleLiveness 是存活探针：只检查进程能响应，不 touch 数据库。
func (s *Server) handleLiveness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "pjgoiot-api",
		"version": s.cfg.Version,
		"time":    time.Now().UTC(),
	})
}

// handleHealth 是就绪探针：数据库 Ping 失败或实例正在下线时返回 503。
func (s *Server) handleHealth(c *gin.Context) {
	// 滚动发布时先置 draining，让反向代理在实例仍可服务时摘流量。
	if s.draining != nil && s.draining.Load() {
		writeError(c.Writer, http.StatusServiceUnavailable, CodeUnavailable, "实例正在下线")
		return
	}
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
