package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresExportReconcileIntegration 覆盖 v0.22.0 的崩溃对账：
// 超时 pending 转 failed、ready/新任务不受影响、重复与并发对账幂等。
func TestPostgresExportReconcileIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	ctx := context.Background()
	st1, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer st1.Close()
	// 第二个实例模拟多副本同时启动。
	st2, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接第二个数据库实例失败: %v", err)
	}
	defer st2.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	userID, _ := createAccount(t, st1, db, "reconcile")
	cleanupAccount(t, db, userID)
	// 对账是全局 UPDATE，先清空遗留任务，避免其他测试留下的 pending 混入计数。
	if _, err := db.Exec(`DELETE FROM personal_data_export_tasks`); err != nil {
		t.Fatalf("清理遗留导出任务: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM personal_data_export_tasks WHERE user_id = $1`, userID)
	})

	oldTask, err := st1.CreateExportTask(ctx, userID, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("创建旧 pending 任务: %v", err)
	}
	newTask, err := st1.CreateExportTask(ctx, userID, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("创建新 pending 任务: %v", err)
	}
	readyTask, err := st1.CreateExportTask(ctx, userID, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("创建 ready 任务: %v", err)
	}
	if err := st1.FinishExportTask(ctx, readyTask.ID, model.PersonalDataExport{}); err != nil {
		t.Fatalf("完成任务: %v", err)
	}

	// 模拟进程在旧任务执行中退出：创建时间早于阈值；新任务保持 recent。
	if _, err := db.Exec(`UPDATE personal_data_export_tasks SET created_at = now() - interval '10 minutes' WHERE id = $1`, oldTask.ID); err != nil {
		t.Fatalf("回拨任务时间: %v", err)
	}
	if _, err := db.Exec(`UPDATE personal_data_export_tasks SET created_at = now() - interval '10 minutes' WHERE id = $1`, readyTask.ID); err != nil {
		t.Fatalf("回拨 ready 任务时间: %v", err)
	}

	threshold := time.Now().UTC().Add(-5 * time.Minute)
	reason := "导出任务因服务重启中断，请重新发起导出"

	// 两个实例并发对账，同一任务只能被推进一次。
	var wg sync.WaitGroup
	counts := make([]int64, 2)
	errs := make([]error, 2)
	for i, st := range []store.Store{st1, st2} {
		wg.Add(1)
		go func(i int, st store.Store) {
			defer wg.Done()
			counts[i], errs[i] = st.ReconcileExportTasks(ctx, threshold, reason)
		}(i, st)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("实例 %d 对账失败: %v", i, err)
		}
	}
	if total := counts[0] + counts[1]; total != 1 {
		t.Fatalf("并发对账总条数=%d（实例分别 %d/%d），want=1", total, counts[0], counts[1])
	}

	oldStatus, err := st1.GetExportTask(ctx, userID, oldTask.ID)
	if err != nil {
		t.Fatalf("查询旧任务: %v", err)
	}
	if oldStatus.Status != "failed" || oldStatus.FailureReason != reason || oldStatus.CompletedAt == nil {
		t.Fatalf("旧任务对账结果异常: %+v", oldStatus)
	}

	newStatus, err := st1.GetExportTask(ctx, userID, newTask.ID)
	if err != nil {
		t.Fatalf("查询新任务: %v", err)
	}
	if newStatus.Status != "pending" || newStatus.FailureReason != "" {
		t.Fatalf("未超时任务被对账影响: %+v", newStatus)
	}

	readyStatus, err := st1.GetExportTask(ctx, userID, readyTask.ID)
	if err != nil {
		t.Fatalf("查询 ready 任务: %v", err)
	}
	if readyStatus.Status != "ready" || readyStatus.FailureReason != "" {
		t.Fatalf("ready 任务被对账影响: %+v", readyStatus)
	}

	// 再次对账应为 0，验证幂等。
	n, err := st1.ReconcileExportTasks(ctx, threshold, reason)
	if err != nil {
		t.Fatalf("重复对账失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("重复对账条数=%d want=0", n)
	}
}

// TestPostgresTokenLifecycleIntegration 覆盖 v0.22.0 的刷新轮换与会话吊销。
func TestPostgresTokenLifecycleIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer st.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	userID, hash1 := createAccount(t, st, db, "token")
	cleanupAccount(t, db, userID)

	otherUserID, _ := createAccount(t, st, db, "token-other")
	cleanupAccount(t, db, otherUserID)

	// 通过配对码签发第二个会话。
	codeHash := hashOf("pair-" + uuid.NewString())
	if err := st.CreatePairingCode(ctx, userID, codeHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatalf("创建配对码: %v", err)
	}
	hash2 := hashOf("session2-" + uuid.NewString())
	if _, err := st.ClaimPairingCode(ctx, codeHash, hash2, time.Now().UTC(), 180*24*time.Hour, time.Now().UTC().Add(180*24*time.Hour)); err != nil {
		t.Fatalf("配对 claim: %v", err)
	}

	sessions, err := st.ListSessions(ctx, userID, hash1)
	if err != nil {
		t.Fatalf("列会话: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("会话数=%d want=2: %+v", len(sessions), sessions)
	}
	var current, second model.Session
	for _, s := range sessions {
		if s.Current {
			current = s
		} else {
			second = s
		}
	}
	if current.ID == "" || second.ID == "" {
		t.Fatalf("current 标记异常: %+v", sessions)
	}

	// 刷新：旧 token 立即失效，新 token 可用；旧 token 不能再次刷新。
	hash3 := hashOf("refreshed-" + uuid.NewString())
	now := time.Now().UTC()
	if _, err := st.RefreshToken(ctx, hash1, hash3, now, now.Add(180*24*time.Hour)); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if _, err := st.UserIDByTokenHash(ctx, hash1); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("旧 token 应为 401，实际 err=%v", err)
	}
	if got, err := st.UserIDByTokenHash(ctx, hash3); err != nil || got != userID {
		t.Fatalf("新 token 不可用: user=%s err=%v", got, err)
	}
	if _, err := st.RefreshToken(ctx, hash1, hashOf("revive-"+uuid.NewString()), now, now.Add(180*24*time.Hour)); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("旧 token 刷新应 401，实际 err=%v", err)
	}

	// 单会话吊销：被吊销 token 立即 401；重复吊销幂等；跨用户 404。
	if err := st.RevokeSession(ctx, userID, second.ID, time.Now().UTC()); err != nil {
		t.Fatalf("吊销会话: %v", err)
	}
	if _, err := st.UserIDByTokenHash(ctx, hash2); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("被吊销 token 应 401，实际 err=%v", err)
	}
	if err := st.RevokeSession(ctx, userID, second.ID, time.Now().UTC()); err != nil {
		t.Fatalf("重复吊销应幂等，实际 err=%v", err)
	}
	if err := st.RevokeSession(ctx, otherUserID, current.ID, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("跨用户吊销应 404，实际 err=%v", err)
	}

	// 删除账号时 tokens 随账号级联清理（完整删除账号流程由账号删除集成测试覆盖）。
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("删除账号: %v", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM tokens WHERE user_id = $1`, userID); n != 0 {
		t.Fatalf("删除账号后 tokens=%d want=0", n)
	}
}
