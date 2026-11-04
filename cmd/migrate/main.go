// Command migrate 使用 golang-migrate 执行内嵌 SQL 迁移（含 seed）。
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/db/migrations"
)

func main() {
	dsn := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL 连接串，默认读取 DATABASE_URL")
	direction := flag.String("direction", "up", "迁移方向：up 或 down")
	steps := flag.Int("steps", 0, "迁移步数；0 表示执行到最新/回滚全部")
	printVersion := flag.Bool("version", false, "只打印当前迁移版本")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 DATABASE_URL 或 -database-url")
		os.Exit(1)
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载内嵌迁移失败:", err)
		os.Exit(1)
	}

	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer db.Close()

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化迁移驱动失败:", err)
		os.Exit(1)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化迁移失败:", err)
		os.Exit(1)
	}
	defer m.Close()

	if *printVersion {
		version, dirty, verr := m.Version()
		if verr != nil {
			fmt.Fprintln(os.Stderr, "读取迁移版本失败:", verr)
			os.Exit(1)
		}
		fmt.Printf("version=%d dirty=%v\n", version, dirty)
		return
	}

	switch *direction {
	case "up":
		if *steps > 0 {
			err = m.Steps(*steps)
		} else {
			err = m.Up()
		}
	case "down":
		if *steps > 0 {
			err = m.Steps(-*steps)
		} else {
			err = m.Down()
		}
	default:
		fmt.Fprintln(os.Stderr, "direction 只能是 up 或 down")
		os.Exit(1)
	}

	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		fmt.Fprintln(os.Stderr, "执行迁移失败:", err)
		os.Exit(1)
	}
	version, dirty, verr := m.Version()
	if verr != nil {
		fmt.Fprintln(os.Stderr, "读取迁移版本失败:", verr)
		os.Exit(1)
	}
	fmt.Printf("迁移完成 version=%d dirty=%v\n", version, dirty)
}
