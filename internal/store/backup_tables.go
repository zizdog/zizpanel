package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// backup_tables.go 集中放「备份与恢复」这一功能需要的 schema 增量。
//
// 为什么单独一个文件：store.go 的 schema 常量是多个任务都会碰的热点，
// 把新增列隔离在这里可以避免互相覆盖。EnsureBackupTables 由 migrate() 调用，
// 必须在任何老库上可重复执行（只加列，不删列、不改类型）。
//
// 修的是什么缺陷（用户报障「我明明选了 mysql，它却没备」）：
// cron_jobs 表原本**没有** backup_targets / backup_dir / keep_days 三列，
// 而 Repository.Create/Update 也不写它们 —— 用户在「计划任务」里选的备份范围、
// 输出目录、保留天数**面板一重启就静默回落到默认值**。这属于「能谎报成功」，
// 比没做更糟：界面显示的是用户选的值，实际执行的却是默认值。
func EnsureBackupTables(ctx context.Context, db *sql.DB) error {
	return ensureColumnsDB(ctx, db, "cron_jobs", map[string]string{
		// 逗号分隔的目标列表（sites,mysql,nginx,panel,...）。用 TEXT 而不是 JSON：
		// 与 launchd 脚本里的写法一一对应，人可读、可手工修。
		"backup_targets": "TEXT NOT NULL DEFAULT ''",
		"backup_dir":     "TEXT NOT NULL DEFAULT ''",
		"keep_days":      "INTEGER NOT NULL DEFAULT 0",
	})
}

// ensureColumnsDB 给已存在的表补齐缺失的列（SQLite 的 ADD COLUMN）。
//
// 与 Store.ensureColumns 是同一套实现，区别只是显式接收 *sql.DB，
// 便于在 migrate() 之外（备份专用表）复用。只做「加列」，
// 不改类型、不删列：迁移要能在任何老库上重复执行而不出错，
// 且绝不能动用户已有数据。列名/定义都来自本包内的常量，不经用户输入，无注入面。
func ensureColumnsDB(ctx context.Context, db *sql.DB, table string, cols map[string]string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("解析 %s 表结构失败: %w", table, err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}
	_ = rows.Close()

	// 固定顺序执行，避免 map 迭代顺序让"失败的库"每次停在不同列上。
	names := make([]string, 0, len(cols))
	for n := range cols {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if have[n] {
			continue
		}
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, n, cols[n])); err != nil {
			return fmt.Errorf("迁移 %s.%s 失败: %w", table, n, err)
		}
	}
	return nil
}
