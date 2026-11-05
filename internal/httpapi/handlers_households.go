package httpapi

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/auth"
)

// householdInvitationTTL 与配对码一致：10 分钟。
const householdInvitationTTL = 10 * time.Minute

const (
	maxHouseholdNameLength = 64
	maxRoomNameLength      = 64
	minRoomSortOrder       = -10000
	maxRoomSortOrder       = 10000
)

type householdCreateRequest struct {
	Name string `json:"name"`
}

type householdPatchRequest struct {
	Name *string `json:"name"`
}

type householdInvitationRequest struct {
	Code string `json:"code"`
}

type householdTransferRequest struct {
	UserID string `json:"userId"`
}

type householdInvitationResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type roomCreateRequest struct {
	Name      string `json:"name"`
	SortOrder *int   `json:"sortOrder"`
}

type roomPatchRequest struct {
	Name      *string `json:"name"`
	SortOrder *int    `json:"sortOrder"`
}

type roomOrderRequest struct {
	RoomIDs []string `json:"roomIds"`
}

type deviceMetaPatchRequest struct {
	Alias       *string `json:"alias"`
	RoomID      *string `json:"roomId"`
	HouseholdID *string `json:"householdId"`
}

func validHouseholdName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > maxHouseholdNameLength {
		return "", false
	}
	return name, true
}

func validRoomName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > maxRoomNameLength {
		return "", false
	}
	return name, true
}

func validSortOrder(v int) bool {
	return v >= minRoomSortOrder && v <= maxRoomSortOrder
}

// handleListHouseholds 列出当前用户加入的全部家庭（含个人家庭）。
func (s *Server) handleListHouseholds(c *gin.Context) {
	items, err := s.store.ListHouseholds(c.Request.Context(), userID(c))
	if err != nil {
		s.storeFailure(c, "查询家庭列表失败", err)
		return
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

// handleCreateHousehold 创建一个非个人家庭，创建者成为 owner。
func (s *Server) handleCreateHousehold(c *gin.Context) {
	var req householdCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	name, ok := validHouseholdName(req.Name)
	if !ok {
		fail(c, http.StatusBadRequest, CodeBadRequest, "家庭名长度需为 1-64 个字符")
		return
	}
	h, err := s.store.CreateHousehold(c.Request.Context(), userID(c), name)
	if err != nil {
		s.storeFailure(c, "创建家庭失败", err)
		return
	}
	c.JSON(http.StatusCreated, h)
}

// handleGetHousehold 仅家庭成员可见；跨家庭统一 404。
func (s *Server) handleGetHousehold(c *gin.Context) {
	h, err := s.store.GetHousehold(c.Request.Context(), userID(c), c.Param("id"))
	if err != nil {
		s.storeFailure(c, "查询家庭失败", err)
		return
	}
	c.JSON(http.StatusOK, h)
}

// handlePatchHousehold 仅 owner 可改名。
func (s *Server) handlePatchHousehold(c *gin.Context) {
	var req householdPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	if req.Name == nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "name 不能缺失")
		return
	}
	name, ok := validHouseholdName(*req.Name)
	if !ok {
		fail(c, http.StatusBadRequest, CodeBadRequest, "家庭名长度需为 1-64 个字符")
		return
	}
	if err := s.store.RenameHousehold(c.Request.Context(), userID(c), c.Param("id"), name); err != nil {
		s.storeFailure(c, "更新家庭失败", err)
		return
	}
	h, err := s.store.GetHousehold(c.Request.Context(), userID(c), c.Param("id"))
	if err != nil {
		s.storeFailure(c, "查询家庭失败", err)
		return
	}
	c.JSON(http.StatusOK, h)
}

// handleDeleteHousehold 仅 owner 可删除；个人家庭不可删除。
// 删除后成员/房间/邀请码级联清理，设备回到各自 owner 的个人家庭且房间清空。
func (s *Server) handleDeleteHousehold(c *gin.Context) {
	if err := s.store.DeleteHousehold(c.Request.Context(), userID(c), c.Param("id")); err != nil {
		s.storeFailure(c, "删除家庭失败", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleListHouseholdMembers 家庭成员均可读。
func (s *Server) handleListHouseholdMembers(c *gin.Context) {
	items, err := s.store.ListHouseholdMembers(c.Request.Context(), userID(c), c.Param("id"))
	if err != nil {
		s.storeFailure(c, "查询家庭成员失败", err)
		return
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

// handleRemoveHouseholdMember 仅 owner 可移除成员；owner 自己与唯一成员不可移除。
func (s *Server) handleRemoveHouseholdMember(c *gin.Context) {
	if err := s.store.RemoveHouseholdMember(c.Request.Context(), userID(c), c.Param("id"), c.Param("userId")); err != nil {
		s.storeFailure(c, "移除家庭成员失败", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleLeaveHousehold 成员自己退出；personal 家庭不可退出，owner 有其他成员时返回 409。
func (s *Server) handleLeaveHousehold(c *gin.Context) {
	if err := s.store.LeaveHousehold(c.Request.Context(), userID(c), c.Param("id")); err != nil {
		s.storeFailure(c, "退出家庭失败", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleTransferHouseholdOwnership 仅 owner 可把所有权转让给同家庭另一成员。
// 目标不是成员返回 404，个人家庭 400；转让后原 owner 降为 member，可正常退出。
func (s *Server) handleTransferHouseholdOwnership(c *gin.Context) {
	var req householdTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	if !validUUID(req.UserID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "userId 必须是 UUID")
		return
	}
	if err := s.store.TransferHouseholdOwnership(c.Request.Context(), userID(c), c.Param("id"), req.UserID); err != nil {
		s.storeFailure(c, "转让家庭所有权失败", err)
		return
	}
	h, err := s.store.GetHousehold(c.Request.Context(), userID(c), c.Param("id"))
	if err != nil {
		s.storeFailure(c, "查询家庭失败", err)
		return
	}
	c.JSON(http.StatusOK, h)
}

// handleCreateHouseholdInvitation 仅 owner 可签发一次性邀请码，只返回明文一次。
func (s *Server) handleCreateHouseholdInvitation(c *gin.Context) {
	code, err := auth.NewPairingCode()
	if err != nil {
		s.log.Error("生成家庭邀请码失败", "error", err, "request_id", c.GetString(ctxKeyRequestID))
		fail(c, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	expiresAt := time.Now().UTC().Add(householdInvitationTTL)
	if _, err := s.store.CreateHouseholdInvitation(c.Request.Context(), userID(c), c.Param("id"),
		auth.PairingCodeHash(code), expiresAt); err != nil {
		s.storeFailure(c, "创建家庭邀请码失败", err)
		return
	}
	c.JSON(http.StatusCreated, householdInvitationResponse{Code: code, ExpiresAt: expiresAt})
}

// handleClaimHouseholdInvitation 用邀请码加入家庭；无效/过期/已用统一 404。
func (s *Server) handleClaimHouseholdInvitation(c *gin.Context) {
	var req householdInvitationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	code := auth.NormalizePairingCode(req.Code)
	if code == "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, "邀请码格式错误")
		return
	}
	h, err := s.store.ClaimHouseholdInvitation(c.Request.Context(), userID(c),
		auth.PairingCodeHash(code), time.Now().UTC(), householdInvitationTTL)
	if err != nil {
		s.storeFailure(c, "领取家庭邀请码失败", err)
		return
	}
	c.JSON(http.StatusOK, h)
}

// handleListHouseholdInvitations 仅 owner 可查看；只返回哈希前缀与状态，绝不返回明文。
func (s *Server) handleListHouseholdInvitations(c *gin.Context) {
	items, err := s.store.ListHouseholdInvitations(c.Request.Context(), userID(c), c.Param("id"))
	if err != nil {
		s.storeFailure(c, "查询家庭邀请码失败", err)
		return
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

// handleRevokeHouseholdInvitation 仅 owner 可撤销未使用的邀请码。
func (s *Server) handleRevokeHouseholdInvitation(c *gin.Context) {
	if err := s.store.RevokeHouseholdInvitation(c.Request.Context(), userID(c), c.Param("id"), c.Param("invitationId")); err != nil {
		s.storeFailure(c, "撤销家庭邀请码失败", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleListHouseholdRooms 家庭成员均可读。
func (s *Server) handleListHouseholdRooms(c *gin.Context) {
	items, err := s.store.ListHouseholdRooms(c.Request.Context(), userID(c), c.Param("id"))
	if err != nil {
		s.storeFailure(c, "查询房间失败", err)
		return
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

// handleCreateHouseholdRoom 仅 owner 可建房间。
func (s *Server) handleCreateHouseholdRoom(c *gin.Context) {
	var req roomCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	name, ok := validRoomName(req.Name)
	if !ok {
		fail(c, http.StatusBadRequest, CodeBadRequest, "房间名长度需为 1-64 个字符")
		return
	}
	sortOrder := 0
	if req.SortOrder != nil {
		if !validSortOrder(*req.SortOrder) {
			fail(c, http.StatusBadRequest, CodeBadRequest, "sortOrder 超出允许范围")
			return
		}
		sortOrder = *req.SortOrder
	}
	r, err := s.store.CreateHouseholdRoom(c.Request.Context(), userID(c), c.Param("id"), name, sortOrder)
	if err != nil {
		s.storeFailure(c, "创建房间失败", err)
		return
	}
	c.JSON(http.StatusCreated, r)
}

// handlePatchHouseholdRoom 仅 owner 可改房间；name/sortOrder 缺失表示不变。
func (s *Server) handlePatchHouseholdRoom(c *gin.Context) {
	var req roomPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	if req.Name == nil && req.SortOrder == nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "至少提供 name 或 sortOrder")
		return
	}
	var name *string
	if req.Name != nil {
		v, ok := validRoomName(*req.Name)
		if !ok {
			fail(c, http.StatusBadRequest, CodeBadRequest, "房间名长度需为 1-64 个字符")
			return
		}
		name = &v
	}
	if req.SortOrder != nil && !validSortOrder(*req.SortOrder) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "sortOrder 超出允许范围")
		return
	}
	r, err := s.store.UpdateHouseholdRoom(c.Request.Context(), userID(c), c.Param("id"), c.Param("roomId"), name, req.SortOrder)
	if err != nil {
		s.storeFailure(c, "更新房间失败", err)
		return
	}
	c.JSON(http.StatusOK, r)
}

// handleDeleteHouseholdRoom 仅 owner 可删房间；房间内设备 room_id 置 NULL。
func (s *Server) handleDeleteHouseholdRoom(c *gin.Context) {
	if err := s.store.DeleteHouseholdRoom(c.Request.Context(), userID(c), c.Param("id"), c.Param("roomId")); err != nil {
		s.storeFailure(c, "删除房间失败", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleReorderHouseholdRooms 仅 owner 可整段重排；请求必须覆盖全部房间，按数组顺序重写 0..n-1。
func (s *Server) handleReorderHouseholdRooms(c *gin.Context) {
	var req roomOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	if req.RoomIDs == nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "roomIds 不能为空")
		return
	}
	items, err := s.store.ReorderHouseholdRooms(c.Request.Context(), userID(c), c.Param("id"), req.RoomIDs)
	if err != nil {
		s.storeFailure(c, "重排房间失败", err)
		return
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

// handlePatchMyDevice 更新设备 alias/roomId/householdId（仅设备 owner）。
// 三个字段均为三态：缺失=不变，空串=清除；householdId 清除表示回到个人家庭。
func (s *Server) handlePatchMyDevice(c *gin.Context) {
	parts := splitKey(c.Param("key"))
	if len(parts) != 2 {
		fail(c, http.StatusNotFound, CodeNotFound, "资源不存在或无权访问")
		return
	}
	var req deviceMetaPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	if req.Alias == nil && req.RoomID == nil && req.HouseholdID == nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "至少提供 alias、roomId 或 householdId")
		return
	}
	if req.Alias != nil {
		v := strings.TrimSpace(*req.Alias)
		if utf8.RuneCountInString(v) > maxAliasLength {
			fail(c, http.StatusBadRequest, CodeBadRequest, "alias 长度不能超过 100 个字符")
			return
		}
		req.Alias = &v
	}
	if req.RoomID != nil && *req.RoomID != "" && !validUUID(*req.RoomID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "roomId 必须是 UUID 或空串")
		return
	}
	if req.HouseholdID != nil && *req.HouseholdID != "" && !validUUID(*req.HouseholdID) {
		fail(c, http.StatusBadRequest, CodeBadRequest, "householdId 必须是 UUID 或空串")
		return
	}
	d, err := s.store.UpdateUserDeviceMeta(c.Request.Context(), userID(c), parts[0], parts[1],
		req.Alias, req.RoomID, req.HouseholdID)
	if err != nil {
		s.storeFailure(c, "更新设备失败", err)
		return
	}
	c.JSON(http.StatusOK, d)
}

// splitKey 与 GET /my/devices 的 catch-all 解析保持一致。
func splitKey(raw string) []string {
	parts := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validUUID(raw string) bool {
	if len(raw) != 36 {
		return false
	}
	for i, r := range raw {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}
