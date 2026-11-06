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
	// 反馈接口按账号与来源 IP 双维度限流，额度独立于全局 / 配对码限流。
	feedbackUserLimiter rateLimiter
	feedbackIPLimiter   rateLimiter
	metrics             *metricsRegistry
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
		feedbackUserLimiter: newRateLimiterFor(cfg.RateLimitBackend, cfg.FeedbackUserPerMin,
			cfg.FeedbackUserBurst, st, logger),
		feedbackIPLimiter: newRateLimiterFor(cfg.RateLimitBackend, cfg.FeedbackIPPerMin,
			cfg.FeedbackIPBurst, st, logger),
		metrics:  newMetricsRegistry(),
		draining: draining,
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
	// 刷新使用滚动轮换：旧 token 立即失效，新 token 获得新的 TTL。
	authGroup.POST("/refresh", s.requireAuth(), s.handleRefreshToken)
	authGroup.POST("/pairing-code", s.requireAuth(), s.handleCreatePairingCode)
	authGroup.POST("/pairing-code/claim", s.claimRateLimit(), s.handleClaimPairingCode)

	// 登录会话：列出与单独吊销。删除账号时 tokens 表随账号一并清理。
	sessionsGroup := api.Group("/sessions", s.requireAuth())
	sessionsGroup.GET("", s.handleListSessions)
	sessionsGroup.DELETE("/:id", s.handleRevokeSession)

	// 账号数据：删除与导出。
	// DELETE /me 只用一次性删除令牌鉴权（账号删除后 Bearer 已失效，重放必须得到 404 而不是 401）。
	meGroup := api.Group("/me")
	meGroup.POST("/deletion-request", s.requireAuth(), s.handleCreateDeletionRequest)
	meGroup.DELETE("", s.handleDeleteMe)
	meGroup.POST("/export", s.requireAuth(), s.handleCreateExport)
	meGroup.GET("/export/:id", s.requireAuth(), s.handleGetExport)
	// 下载链接本身即凭据（短期 + 一次性），不使用 Bearer，便于浏览器直接打开。
	meGroup.GET("/export/:id/download", s.handleDownloadExport)

	devicesGroup := api.Group("/devices")
	devicesGroup.GET("/:mac", s.handlePublicDevice)

	deviceTypesGroup := api.Group("/device-types")
	deviceTypesGroup.GET("", s.handleDeviceTypes)
	deviceTypesGroup.GET("/:type", s.handleDeviceType)
	deviceTypesGroup.GET("/:type/catalog", s.handleDeviceCatalog)
	deviceTypesGroup.GET("/:type/firmwares", s.handleFirmwares)
	deviceTypesGroup.GET("/:type/accessories", s.handleAccessories)

	// 设备类型目录：服务端事实来源，公开只读，匿名可读；带有效 token 时记录访问用于灰度。
	catalogGroup := api.Group("/catalog")
	catalogGroup.GET("/device-types", s.optionalAuth(), s.handleDeviceTypeCatalog)

	myGroup := api.Group("/my", s.requireAuth())
	myGroup.GET("/devices", s.handleMyDevices)
	// Gin 不允许同一段同时存在 :mac 与 :platform/:externalId 两个参数节点，
	// 因此用 catch-all 统一分发，对外仍保持 /devices/{mac} 与 /devices/{platform}/{externalId} 两种路径。
	myGroup.GET("/devices/*key", s.handleMyDeviceRoute)

	syncGroup := api.Group("/sync", s.requireAuth())
	syncGroup.GET("/changes", s.handleChanges)
	syncGroup.POST("/push", s.handlePush)

	feedbackGroup := api.Group("/feedback", s.requireAuth())
	feedbackGroup.POST("", s.feedbackRateLimit(), s.handleCreateFeedback)
	feedbackGroup.GET("/mine", s.handleListMyFeedback)
	// 静态路径 /mine 与参数路径 /:feedbackId 同级并存；Gin 优先匹配静态段。
	feedbackGroup.GET("/:feedbackId", s.handleGetFeedback)
	feedbackGroup.PATCH("/:feedbackId", s.handleUpdateFeedback)

	// 运营侧受理入口：不复用客户端 Bearer token，改用独立 ADMIN_TOKEN。
	// 未配置时中间件统一返回 404，对外与不存在的路由不可区分。
	adminFeedback := api.Group("/admin/feedback", s.requireAdmin())
	adminFeedback.GET("", s.handleAdminListFeedback)
	adminFeedback.PATCH("/:feedbackId", s.handleAdminUpdateFeedback)

	// 运营侧目录管理：与 admin/feedback 共用 ADMIN_TOKEN 与未配置 404 语义。
	adminCatalog := api.Group("/admin/catalog/device-types", s.requireAdmin())
	adminCatalog.PUT("/:type", s.handleAdminUpdateCatalogDeviceType)
	adminCatalog.DELETE("/:type", s.handleAdminDeleteCatalogDeviceType)

	// 服务端推送：客户端注册/停用设备令牌；告警由运营侧入口产生（当前无内建告警产生者）。
	pushGroup := api.Group("/push", s.requireAuth())
	pushGroup.POST("/devices", s.handleRegisterPushDevice)
	pushGroup.DELETE("/devices/:deviceId", s.handleDeactivatePushDevice)

	adminAlerts := api.Group("/admin/alerts", s.requireAdmin())
	adminAlerts.POST("", s.handleCreateAlert)

	// 场景（本地触发条件 + 动作）上云：全部限定本人，软删除参与同步 tombstone。
	scenesGroup := api.Group("/scenes", s.requireAuth())
	scenesGroup.GET("", s.handleListScenes)
	scenesGroup.POST("", s.handleCreateScene)
	scenesGroup.GET("/:sceneId", s.handleGetScene)
	scenesGroup.PUT("/:sceneId", s.handleUpdateScene)
	scenesGroup.DELETE("/:sceneId", s.handleDeleteScene)

	households := api.Group("/households", s.requireAuth())
	households.GET("", s.handleListHouseholds)
	households.POST("", s.handleCreateHousehold)
	households.POST("/invitations/claim", s.claimRateLimit(), s.handleClaimHouseholdInvitation)
	households.GET("/:id", s.handleGetHousehold)
	households.PATCH("/:id", s.handlePatchHousehold)
	households.DELETE("/:id", s.handleDeleteHousehold)
	households.GET("/:id/members", s.handleListHouseholdMembers)
	households.DELETE("/:id/members/:userId", s.handleRemoveHouseholdMember)
	households.POST("/:id/leave", s.handleLeaveHousehold)
	households.POST("/:id/transfer-ownership", s.handleTransferHouseholdOwnership)
	households.POST("/:id/invitations", s.handleCreateHouseholdInvitation)
	households.GET("/:id/invitations", s.handleListHouseholdInvitations)
	households.DELETE("/:id/invitations/:invitationId", s.handleRevokeHouseholdInvitation)
	households.GET("/:id/rooms", s.handleListHouseholdRooms)
	households.POST("/:id/rooms", s.handleCreateHouseholdRoom)
	households.PUT("/:id/rooms/order", s.handleReorderHouseholdRooms)
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
