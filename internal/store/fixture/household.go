package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// createPersonalHouseholdLocked 为新建用户创建个人家庭；调用方必须已持有写锁。
func (s *Store) createPersonalHouseholdLocked(userID string) {
	hid := uuid.NewString()
	s.households[hid] = &householdRecord{
		Household: model.Household{
			ID: hid, Name: "我的家", OwnerUserID: userID, Role: "owner",
			Personal: true, MemberCount: 1, CreatedAt: time.Now().UTC(),
		},
		members: map[string]string{userID: "owner"},
	}
}

// personalHouseholdIDLocked 返回用户个人家庭 id；不存在时返回空串。
func (s *Store) personalHouseholdIDLocked(userID string) string {
	for id, h := range s.households {
		if h.Personal && h.OwnerUserID == userID {
			return id
		}
	}
	return ""
}

// visibleHouseholdIDsLocked 返回用户作为成员的全部家庭 id。
func (s *Store) visibleHouseholdIDsLocked(userID string) map[string]bool {
	out := map[string]bool{}
	for id, h := range s.households {
		if _, ok := h.members[userID]; ok {
			out[id] = true
		}
	}
	return out
}

func (s *Store) householdViewLocked(h *householdRecord, viewer string) model.Household {
	v := h.Household
	v.Role = h.members[viewer]
	v.MemberCount = len(h.members)
	v.DeviceCount = 0
	v.RoomCount = 0
	for _, data := range s.users {
		for _, d := range data.byID {
			if d.DeletedAt == nil && d.HouseholdID != nil && *d.HouseholdID == h.ID {
				v.DeviceCount++
			}
		}
	}
	for _, r := range s.rooms {
		if r.HouseholdID == h.ID {
			v.RoomCount++
		}
	}
	return v
}

func (s *Store) requireHouseholdLocked(userID, householdID string, ownerOnly bool) (*householdRecord, error) {
	h, ok := s.households[householdID]
	if !ok {
		return nil, store.ErrNotFound
	}
	role, ok := h.members[userID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if ownerOnly && role != "owner" {
		return nil, store.ErrForbidden
	}
	return h, nil
}

// appendStructuralChangeLocked 写入一条面向单个接收者的结构变更。
// 结构变更按「写入时刻的成员集合」逐人一条，因此不回放历史、也不会在成员变动后触发 410。
func (s *Store) appendStructuralChangeLocked(recipient, entity, entityID, op string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	seq := s.nextSeq
	s.nextSeq++
	s.changes = append(s.changes, changeRecord{
		seq: seq, userID: recipient, entity: entity, entityID: entityID,
		op: op, payload: raw, serverTS: time.Now().UTC(),
	})
}

// appendHouseholdChangeLocked 按家庭当前成员集合扇出 household 结构变更。
func (s *Store) appendHouseholdChangeLocked(h *householdRecord, op string, deleted bool) {
	for uid, role := range h.members {
		s.appendStructuralChangeLocked(uid, "household", h.ID, op, model.HouseholdChangePayload{
			ID: h.ID, Name: h.Name, OwnerUserID: h.OwnerUserID, Role: role,
			MemberCount: len(h.members), Deleted: deleted,
		})
	}
}

// appendRoomChangeLocked 按家庭当前成员集合扇出 room 结构变更。
func (s *Store) appendRoomChangeLocked(h *householdRecord, op string, deleted bool, room model.HouseholdRoom) {
	for uid := range h.members {
		s.appendStructuralChangeLocked(uid, "room", room.ID, op, model.RoomChangePayload{
			ID: room.ID, HouseholdID: room.HouseholdID, Name: room.Name,
			SortOrder: room.SortOrder, Deleted: deleted,
		})
	}
}

// CreateHousehold 实现 store.Store。
func (s *Store) CreateHousehold(ctx context.Context, ownerUserID string, name string) (model.Household, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[ownerUserID]; !ok {
		return model.Household{}, store.ErrUnauthorized
	}
	hid := uuid.NewString()
	h := &householdRecord{
		Household: model.Household{
			ID: hid, Name: name, OwnerUserID: ownerUserID, Role: "owner",
			CreatedAt: time.Now().UTC(),
		},
		members: map[string]string{ownerUserID: "owner"},
	}
	s.households[hid] = h
	s.appendHouseholdChangeLocked(h, "upsert", false)
	return s.householdViewLocked(h, ownerUserID), nil
}

// ListHouseholds 实现 store.Store。
func (s *Store) ListHouseholds(ctx context.Context, userID string) ([]model.Household, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []model.Household{}
	for _, h := range s.households {
		if _, ok := h.members[userID]; !ok {
			continue
		}
		out = append(out, s.householdViewLocked(h, userID))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Personal != out[j].Personal {
			return out[i].Personal
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// GetHousehold 实现 store.Store。
func (s *Store) GetHousehold(ctx context.Context, userID string, householdID string) (model.Household, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, err := s.requireHouseholdLocked(userID, householdID, false)
	if err != nil {
		return model.Household{}, err
	}
	return s.householdViewLocked(h, userID), nil
}

// RenameHousehold 实现 store.Store。
func (s *Store) RenameHousehold(ctx context.Context, userID string, householdID string, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.requireHouseholdLocked(userID, householdID, true)
	if err != nil {
		return err
	}
	h.Name = name
	s.appendHouseholdChangeLocked(h, "upsert", false)
	return nil
}

// DeleteHousehold 实现 store.Store。
func (s *Store) DeleteHousehold(ctx context.Context, userID string, householdID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.requireHouseholdLocked(userID, householdID, true)
	if err != nil {
		return err
	}
	if h.Personal {
		return store.ErrForbidden
	}
	// 设备回到各自 owner 的个人家庭，房间归属清空。
	for ownerID, data := range s.users {
		for _, d := range data.byID {
			if d.HouseholdID != nil && *d.HouseholdID == householdID {
				if pid := s.personalHouseholdIDLocked(ownerID); pid != "" {
					v := pid
					d.HouseholdID = &v
				} else {
					d.HouseholdID = nil
				}
				d.RoomID = nil
			}
		}
	}
	delete(s.households, householdID)
	s.appendHouseholdChangeLocked(h, "delete", true)
	for rid, r := range s.rooms {
		if r.HouseholdID == householdID {
			delete(s.rooms, rid)
		}
	}
	for hash, inv := range s.invitations {
		if inv.householdID == householdID {
			delete(s.invitations, hash)
		}
	}
	return nil
}

// ListHouseholdMembers 实现 store.Store。
func (s *Store) ListHouseholdMembers(ctx context.Context, userID string, householdID string) ([]model.HouseholdMember, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, err := s.requireHouseholdLocked(userID, householdID, false)
	if err != nil {
		return nil, err
	}
	out := make([]model.HouseholdMember, 0, len(h.members))
	for uid, role := range h.members {
		m := model.HouseholdMember{UserID: uid, Role: role, Kind: "anonymous", JoinedAt: h.CreatedAt, IsSelf: uid == userID}
		if data, ok := s.users[uid]; ok {
			m.Kind = data.kind
			m.JoinedAt = data.createdAt
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].JoinedAt.Equal(out[j].JoinedAt) {
			return out[i].JoinedAt.Before(out[j].JoinedAt)
		}
		return out[i].UserID < out[j].UserID
	})
	return out, nil
}

// RemoveHouseholdMember 实现 store.Store。
func (s *Store) RemoveHouseholdMember(ctx context.Context, userID string, householdID string, targetUserID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.requireHouseholdLocked(userID, householdID, true)
	if err != nil {
		return err
	}
	role, ok := h.members[targetUserID]
	if !ok {
		return store.ErrNotFound
	}
	if targetUserID == userID || role == "owner" {
		return store.ErrHouseholdOwnerCannotRemoveSelf
	}
	s.moveUserDevicesToPersonalLocked(targetUserID, householdID)
	delete(h.members, targetUserID)
	s.appendHouseholdChangeLocked(h, "upsert", false)
	return nil
}

// TransferHouseholdOwnership 实现 store.Store。
// 单机 fixture 用全局写锁串行化：并发转让只有第一笔成功，后到者角色已变为 member 而得到 ErrForbidden。
func (s *Store) TransferHouseholdOwnership(ctx context.Context, userID string, householdID string, targetUserID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.requireHouseholdLocked(userID, householdID, false)
	if err != nil {
		return err
	}
	if h.members[userID] != "owner" {
		return store.ErrForbidden
	}
	if h.Personal {
		return store.ErrPersonalHouseholdImmutable
	}
	if targetUserID == userID {
		return store.ErrHouseholdTargetAlreadyOwner
	}
	role, ok := h.members[targetUserID]
	if !ok {
		return store.ErrHouseholdTargetNotMember
	}
	if role == "owner" {
		return store.ErrHouseholdTargetAlreadyOwner
	}
	h.members[userID] = "member"
	h.members[targetUserID] = "owner"
	h.OwnerUserID = targetUserID
	s.appendHouseholdChangeLocked(h, "upsert", false)
	return nil
}

// LeaveHousehold 实现 store.Store。
func (s *Store) LeaveHousehold(ctx context.Context, userID string, householdID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.requireHouseholdLocked(userID, householdID, false)
	if err != nil {
		return err
	}
	if h.Personal {
		return store.ErrPersonalHouseholdImmutable
	}
	if h.members[userID] == "owner" {
		if len(h.members) > 1 {
			return store.ErrHouseholdOwnerMustTransfer
		}
		s.moveUserDevicesToPersonalLocked(userID, householdID)
		delete(s.households, householdID)
		s.appendHouseholdChangeLocked(h, "delete", true)
		for rid, r := range s.rooms {
			if r.HouseholdID == householdID {
				delete(s.rooms, rid)
			}
		}
		for hash, inv := range s.invitations {
			if inv.householdID == householdID {
				delete(s.invitations, hash)
			}
		}
		return nil
	}
	s.moveUserDevicesToPersonalLocked(userID, householdID)
	// 退出者本人收到 delete（同账号其他设备也能立刻移除该家庭）。
	role := h.members[userID]
	s.appendStructuralChangeLocked(userID, "household", h.ID, "delete", model.HouseholdChangePayload{
		ID: h.ID, Name: h.Name, OwnerUserID: h.OwnerUserID, Role: role,
		MemberCount: len(h.members), Deleted: true,
	})
	delete(h.members, userID)
	s.appendHouseholdChangeLocked(h, "upsert", false)
	return nil
}

func (s *Store) moveUserDevicesToPersonalLocked(userID, householdID string) {
	data, ok := s.users[userID]
	if !ok {
		return
	}
	pid := s.personalHouseholdIDLocked(userID)
	for _, d := range data.byID {
		if d.HouseholdID != nil && *d.HouseholdID == householdID {
			if pid != "" {
				v := pid
				d.HouseholdID = &v
			} else {
				d.HouseholdID = nil
			}
			d.RoomID = nil
		}
	}
}

// ListHouseholdInvitations 实现 store.Store。
func (s *Store) ListHouseholdInvitations(ctx context.Context, userID string, householdID string) ([]model.HouseholdInvitationInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, true); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := []model.HouseholdInvitationInfo{}
	for hash, inv := range s.invitations {
		if inv.householdID != householdID {
			continue
		}
		out = append(out, model.HouseholdInvitationInfo{
			ID:             inv.id,
			CodeHashPrefix: hashPrefix(hash),
			Status:         invitationStatus(inv.attempts, inv.expiresAt, now),
			Attempts:       inv.attempts,
			ExpiresAt:      inv.expiresAt.UTC(),
			CreatedAt:      inv.createdAt.UTC(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// RevokeHouseholdInvitation 实现 store.Store。
func (s *Store) RevokeHouseholdInvitation(ctx context.Context, userID string, householdID string, invitationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, true); err != nil {
		return err
	}
	for hash, inv := range s.invitations {
		if inv.id == invitationID && inv.householdID == householdID {
			delete(s.invitations, hash)
			return nil
		}
	}
	return store.ErrNotFound
}

// ReorderHouseholdRooms 实现 store.Store。
func (s *Store) ReorderHouseholdRooms(ctx context.Context, userID string, householdID string, roomIDs []string) ([]model.HouseholdRoom, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, true); err != nil {
		return nil, err
	}
	current := map[string]bool{}
	for _, r := range s.rooms {
		if r.HouseholdID == householdID {
			current[r.ID] = true
		}
	}
	if len(roomIDs) != len(current) {
		return nil, store.ErrRoomOrderMismatch
	}
	seen := map[string]bool{}
	for _, rid := range roomIDs {
		if !current[rid] || seen[rid] {
			return nil, store.ErrRoomOrderMismatch
		}
		seen[rid] = true
	}
	for i, rid := range roomIDs {
		r := s.rooms[rid]
		r.SortOrder = i
		s.rooms[rid] = r
	}
	out := []model.HouseholdRoom{}
	for _, r := range s.rooms {
		if r.HouseholdID == householdID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	h := s.households[householdID]
	for _, r := range out {
		s.appendRoomChangeLocked(h, "upsert", false, r)
	}
	return out, nil
}

// CreateHouseholdInvitation 实现 store.Store。
func (s *Store) CreateHouseholdInvitation(ctx context.Context, userID string, householdID string, codeHash string, expiresAt time.Time) (model.HouseholdInvitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, true); err != nil {
		return model.HouseholdInvitation{}, err
	}
	now := time.Now().UTC()
	for hash, inv := range s.invitations {
		if inv.householdID == householdID && inv.expiresAt.Before(now) {
			delete(s.invitations, hash)
		}
	}
	s.invitations[codeHash] = &householdInvitationRecord{
		id: uuid.NewString(), householdID: householdID, invitedBy: userID,
		expiresAt: expiresAt.UTC(), createdAt: now,
	}
	return model.HouseholdInvitation{HouseholdID: householdID, ExpiresAt: expiresAt.UTC()}, nil
}

const maxHouseholdInvitationAttempts = 5

// ClaimHouseholdInvitation 实现 store.Store。
func (s *Store) ClaimHouseholdInvitation(ctx context.Context, userID string, codeHash string, now time.Time, ttl time.Duration) (model.Household, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return model.Household{}, store.ErrUnauthorized
	}
	inv, ok := s.invitations[codeHash]
	if !ok {
		s.invitations[codeHash] = &householdInvitationRecord{id: uuid.NewString(), attempts: 1, expiresAt: now.Add(ttl).UTC(), createdAt: now}
		return model.Household{}, store.ErrNotFound
	}
	if inv.householdID == "" || inv.attempts >= maxHouseholdInvitationAttempts || !inv.expiresAt.After(now) {
		if inv.attempts >= maxHouseholdInvitationAttempts {
			delete(s.invitations, codeHash)
			return model.Household{}, store.ErrNotFound
		}
		inv.attempts++
		return model.Household{}, store.ErrNotFound
	}
	h, ok := s.households[inv.householdID]
	if !ok {
		delete(s.invitations, codeHash)
		return model.Household{}, store.ErrNotFound
	}
	if _, exists := h.members[userID]; !exists {
		h.members[userID] = "member"
	}
	delete(s.invitations, codeHash)
	s.appendHouseholdChangeLocked(h, "upsert", false)
	return s.householdViewLocked(h, userID), nil
}

// ListHouseholdRooms 实现 store.Store。
func (s *Store) ListHouseholdRooms(ctx context.Context, userID string, householdID string) ([]model.HouseholdRoom, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, false); err != nil {
		return nil, err
	}
	out := []model.HouseholdRoom{}
	for _, r := range s.rooms {
		if r.HouseholdID == householdID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// CreateHouseholdRoom 实现 store.Store。
func (s *Store) CreateHouseholdRoom(ctx context.Context, userID string, householdID string, name string, sortOrder int) (model.HouseholdRoom, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, true); err != nil {
		return model.HouseholdRoom{}, err
	}
	r := model.HouseholdRoom{
		ID: uuid.NewString(), HouseholdID: householdID, Name: name,
		SortOrder: sortOrder, CreatedAt: time.Now().UTC(),
	}
	s.rooms[r.ID] = r
	s.appendRoomChangeLocked(s.households[householdID], "upsert", false, r)
	return r, nil
}

// UpdateHouseholdRoom 实现 store.Store。
func (s *Store) UpdateHouseholdRoom(ctx context.Context, userID string, householdID string, roomID string, name *string, sortOrder *int) (model.HouseholdRoom, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, true); err != nil {
		return model.HouseholdRoom{}, err
	}
	r, ok := s.rooms[roomID]
	if !ok || r.HouseholdID != householdID {
		return model.HouseholdRoom{}, store.ErrNotFound
	}
	if name != nil {
		r.Name = *name
	}
	if sortOrder != nil {
		r.SortOrder = *sortOrder
	}
	s.rooms[roomID] = r
	s.appendRoomChangeLocked(s.households[householdID], "upsert", false, r)
	return r, nil
}

// DeleteHouseholdRoom 实现 store.Store。
func (s *Store) DeleteHouseholdRoom(ctx context.Context, userID string, householdID string, roomID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireHouseholdLocked(userID, householdID, true); err != nil {
		return err
	}
	r, ok := s.rooms[roomID]
	if !ok || r.HouseholdID != householdID {
		return store.ErrNotFound
	}
	delete(s.rooms, roomID)
	for _, data := range s.users {
		for _, d := range data.byID {
			if d.RoomID != nil && *d.RoomID == roomID {
				d.RoomID = nil
			}
		}
	}
	s.appendRoomChangeLocked(s.households[householdID], "delete", true, r)
	return nil
}

// hashPrefix 返回哈希前 8 位，仅用于管理列表展示。
func hashPrefix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

// invitationStatus 计算邀请码状态：attempts 达上限为 locked，否则过期/有效。
func invitationStatus(attempts int, expiresAt, now time.Time) string {
	if attempts >= maxHouseholdInvitationAttempts {
		return "locked"
	}
	if !expiresAt.After(now) {
		return "expired"
	}
	return "active"
}

// UpdateUserDeviceMeta 实现 store.Store。
func (s *Store) UpdateUserDeviceMeta(ctx context.Context, userID string, platform string, externalID string, alias *string, roomID *string, householdID *string) (model.UserDevice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.users[userID]
	if !ok {
		return model.UserDevice{}, store.ErrUnauthorized
	}
	d, ok := data.byKey[deviceKey(platform, externalID)]
	if !ok || d.DeletedAt != nil {
		return model.UserDevice{}, store.ErrNotFound
	}

	targetHousehold := d.HouseholdID
	if householdID != nil {
		if *householdID == "" {
			v := s.personalHouseholdIDLocked(userID)
			if v == "" {
				return model.UserDevice{}, store.ErrNotFound
			}
			targetHousehold = &v
		} else {
			if _, err := s.requireHouseholdLocked(userID, *householdID, false); err != nil {
				return model.UserDevice{}, err
			}
			v := *householdID
			targetHousehold = &v
		}
	}

	targetRoom := d.RoomID
	if roomID != nil {
		if *roomID == "" {
			targetRoom = nil
		} else {
			r, ok := s.rooms[*roomID]
			if !ok {
				return model.UserDevice{}, store.ErrNotFound
			}
			if targetHousehold == nil || r.HouseholdID != *targetHousehold {
				return model.UserDevice{}, store.ErrForbidden
			}
			v := *roomID
			targetRoom = &v
		}
	} else if householdID != nil && targetRoom != nil {
		if r, ok := s.rooms[*targetRoom]; !ok || targetHousehold == nil || r.HouseholdID != *targetHousehold {
			targetRoom = nil
		}
	}

	if alias != nil {
		d.Alias = *alias
	}
	d.HouseholdID = targetHousehold
	d.RoomID = targetRoom
	s.nextSeq++
	d.ServerSeq = s.nextSeq - 1
	d.UpdatedAt = time.Now().UTC()
	*d = s.enrichUserDevice(*d)

	hh := ""
	if d.HouseholdID != nil {
		hh = *d.HouseholdID
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return model.UserDevice{}, fmt.Errorf("序列化变更记录失败: %w", err)
	}
	s.changes = append(s.changes, changeRecord{
		seq: d.ServerSeq, userID: userID, householdID: hh, entity: "user_device",
		entityID: userID + ":" + platform + ":" + externalID,
		op:       "upsert", payload: payload, serverTS: time.Now().UTC(),
	})
	return *d, nil
}
