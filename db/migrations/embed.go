// Package migrations 通过 embed 把 SQL 迁移文件打包进二进制，
// 使 cmd/migrate 与 CI 无需依赖额外的 migrate CLI。
package migrations

import "embed"

// FS 包含 db/migrations 下的全部迁移文件。
//
//go:embed *.sql
var FS embed.FS
