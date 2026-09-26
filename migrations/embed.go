// Package migrations 将 SQL 迁移文件嵌入二进制，供 store.Migrate 使用。
//
// 说明：Go 的 embed 指令无法向上跨越包目录（不能用 ".."），
// 因此 embed 入口必须与被嵌入文件同目录。此处 package 名与目录名一致。
package migrations

import "embed"

// FS 包含 migrations/ 下全部 .sql 迁移文件。
//
//go:embed *.sql
var FS embed.FS
