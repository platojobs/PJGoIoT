// Package migrate 提供进程内迁移入口，供 API 启动时自动升级数据库。
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/db/migrations"
)

// Up 执行全部 up 迁移，返回最终版本与 dirty 状态。
// 已是最新版本时视为成功（ErrNoChange）。
func Up(ctx context.Context, dsn string) (version uint, dirty bool, err error) {
	_ = ctx // 迁移本身不支持 context；由调用方用连接超时与启动超时控制。
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return 0, false, fmt.Errorf("加载内嵌迁移失败: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return 0, false, fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.Close()
	db.SetConnMaxLifetime(5 * time.Minute)

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return 0, false, fmt.Errorf("初始化迁移驱动失败: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return 0, false, fmt.Errorf("初始化迁移失败: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, false, fmt.Errorf("执行迁移失败: %w", err)
	}
	version, dirty, verr := m.Version()
	if verr != nil {
		return 0, false, fmt.Errorf("读取迁移版本失败: %w", verr)
	}
	return version, dirty, nil
}
