package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ============================================================================
//  恢复：把备份里的一致性数据库快照灌回**正在使用的** panel.db
//
//  为什么不是"替换 panel.db 文件"：面板自己正持有这个文件的连接
//  （MaxOpenConns(1)，WAL 模式下还有 -wal/-shm），进程内替换文件句柄会指向
//  已被删除的 inode —— 表现为"恢复成功但页面还是旧数据"，或者更糟，
//  下次 checkpoint 把旧页写回新文件。正确做法是**保持同一个数据库文件**，
//  用 ATTACH + 事务逐表复制，把数据换掉。
//
//  为什么末尾还要重放 migrate()：备份可能来自更旧的版本（缺新列）。
//  复制时缺失的列由 ADD COLUMN 的默认值补上；再跑一次 migrate() 是幂等的，
//  用于兜住"目标库结构也偏旧"的情况（例如从命令行对一个手工建的老库恢复）。
// ============================================================================

// ReplaceReport 是一次数据库恢复的如实回读。
type ReplaceReport struct {
	Tables          []TableReplaceStat `json:"tables"`
	SessionsCleared int64              `json:"sessions_cleared"`
	// SkippedTables 是备份里不存在、因此没有动过的表（如备份比程序旧）。
	SkippedTables []string `json:"skipped_tables"`
}

// TableReplaceStat 是单表恢复结果。
type TableReplaceStat struct {
	Table   string   `json:"table"`
	Rows    int64    `json:"rows"`
	Columns []string `json:"columns"` // 实际复制的列（两库交集）
	Missing []string `json:"missing"` // 备份缺、目标有（由默认值补齐）的列
	HadSeq  bool     `json:"had_seq"` // 是否恢复了 sqlite_sequence
}

// replaceFromBackupHook 是**仅供测试**的失败注入点：返回非 nil 时整个事务回滚。
//
// 为什么需要它：主键冲突这类"天然失败"在自洽的备份里构造不出来
// （备份库自己就带 UNIQUE 约束），而"失败必须整事务回滚"是恢复功能最关键的
// 安全性质，不能只靠肉眼。生产代码永远不设置它。
var replaceFromBackupHook func(stage string) error

// SetReplaceFromBackupHookForTest 安装失败注入钩子，返回恢复函数。
func SetReplaceFromBackupHookForTest(fn func(stage string) error) func() {
	prev := replaceFromBackupHook
	replaceFromBackupHook = fn
	return func() { replaceFromBackupHook = prev }
}

// extraSchemaSources 是"不在 store.go 的 schema 常量里"的建表来源。
//
// ⚠️ 每个自带建表的文件都必须注册自己（见 nav.go 的 init）：KnownTables() 是备份
// 兼容性判定的唯一依据 —— 漏登记一张表，恢复时就会把备份里的那批数据判成
// "当前程序不认识的表 → 备份比程序新"，于是**整包拒绝恢复**。
// 2026-09-18 导航页接入时就踩到过这个（导航页的备份一律恢复不了），
// 现在还有一条门禁 `TestKnownTablesCoversEverySchemaSource` 扫源码兜底。
var extraSchemaSources []string

// registerSchemaSource 把一个额外的建表语句集合登记进 KnownTables()。
func registerSchemaSource(src string) { extraSchemaSources = append(extraSchemaSources, src) }

// schemaSources 返回当前程序全部建表来源（主 schema + 各文件自己注册的）。
func schemaSources() []string { return append([]string{schema}, extraSchemaSources...) }

// KnownTables 返回当前程序 schema 定义的全部表名（供备份兼容性判定使用）。
func KnownTables() []string {
	var out []string
	for _, src := range schemaSources() {
		for _, stmt := range splitStatements(src) {
			if name := createTableName(stmt); name != "" {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

var reCreateTable = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `\[]?([A-Za-z_][A-Za-z0-9_]*)`)

func createTableName(stmt string) string {
	m := reCreateTable.FindStringSubmatch(stmt)
	if m == nil {
		return ""
	}
	return m[1]
}

// SnapshotTo 用 `VACUUM INTO` 生成一个**一致性**数据库快照。
//
// 为什么不能用 cp：仓库里没有任何 checkpoint 代码，WAL 模式下已提交的事务
// 可能还只在 -wal 里；单独拷 panel.db 会得到一个缺数据的库，而且
// "三个文件分别拷"也不是同一瞬间的状态。VACUUM INTO 是单语句原子快照
// （SQLite ≥ 3.27），且读者不被写者阻塞 —— 面板可以边跑边备。
func (s *Store) SnapshotTo(ctx context.Context, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("创建快照目录失败: %w", err)
	}
	// VACUUM INTO 要求目标文件不存在。
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理旧快照失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("生成数据库一致性快照失败: %w", err)
	}
	return nil
}

// SnapshotToFile 是"给不持有 Store 的调用方"的薄封装（实现 Snapshotter 接口）。
func (s *Store) SnapshotToFile(ctx context.Context, dest string) error {
	return s.SnapshotTo(ctx, dest)
}

// ReplaceFromBackup 用备份库的数据替换当前库的所有业务数据。
//
//	backupDBPath 必须是 VACUUM INTO 生成的一致性快照（不是 live 的 panel.db）。
//
// 关键顺序（每一步都是实测踩出来的）：
//  1. `PRAGMA foreign_keys=OFF` 必须在 Begin **之前** —— 该 pragma 是连接级的，
//     事务内切换无效；不关掉它，逐表 DELETE 会因外键顺序问题失败。
//  2. ATTACH 备份库，先做**兼容性判定**（备份里有当前程序不认识的表 → 拒绝），
//     再看列（备份有目标没有的列 → 拒绝；备份缺目标有的列 → 允许，由默认值补）。
//  3. 单事务内逐表 DELETE + INSERT（顺序 users → sessions → 其余），
//     任何一步失败都 COMMIT 不了 → 目标库一字未变（有测试注入验证）。
//  4. 恢复 sqlite_sequence（否则恢复后新建的站点会拿到重复/回退的主键）。
//  5. 提交后 `foreign_keys=ON` + `PRAGMA foreign_key_check` 必须为空。
//  6. 清空 sessions（强制重新登录），重放 migrate()。
func (s *Store) ReplaceFromBackup(ctx context.Context, backupDBPath string) (*ReplaceReport, error) {
	st, err := os.Stat(backupDBPath)
	if err != nil {
		return nil, fmt.Errorf("读取备份数据库失败: %w", err)
	}
	if st.Size() == 0 {
		return nil, errors.New("备份数据库为空文件，拒绝恢复")
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接失败: %w", err)
	}
	// 连接的关闭统一由下面那个 defer 负责（它要先 DETACH 再关，顺序错了会
	// 把 ATTACH 残留进连接池，第二次恢复报 "database backup is already in use"）。

	known := KnownTables()
	knownSet := map[string]bool{}
	for _, t := range known {
		knownSet[t] = true
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return nil, fmt.Errorf("关闭外键约束失败: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS backup`, backupDBPath); err != nil {
		return nil, fmt.Errorf("挂载备份数据库失败: %w", err)
	}
	// 收尾必须**显式**做，不能只靠 defer：
	//
	// 这里恢复成功后要在 migrate() 之前把连接还给连接池（池里只有一条连接，
	// 不还就会自我死锁）。一旦提前 conn.Close()，函数返回时才执行的 defer
	// 就会打在已关闭的连接上（sql.ErrConnDone，被静默忽略）—— 于是 ATTACH 的
	// `backup` 会残留在池里那条连接上，**第二次恢复**直接报
	// "database backup is already in use"（2026-09-18 本机真机实测）。
	// 所以：正常路径显式 DETACH + 恢复外键后再 Close；defer 只兜异常路径。
	// 真机上允许恢复期间几秒"数据库排队"，不做维护模式（用户 2026-09-21 拍板）。
	closed := false
	detached := false
	cleanup := func() {
		if detached {
			return
		}
		_, _ = conn.ExecContext(context.Background(), `DETACH DATABASE backup`)
		_, _ = conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)
		detached = true
	}
	defer func() {
		cleanup()
		if !closed {
			_ = conn.Close()
		}
	}()

	backupTables, err := tableNames(ctx, conn, "backup")
	if err != nil {
		return nil, err
	}
	if len(backupTables) == 0 {
		return nil, errors.New("备份数据库里没有任何表，拒绝恢复")
	}
	var unknown []string
	for _, t := range backupTables {
		if !knownSet[t] {
			unknown = append(unknown, t)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("备份包含当前程序不认识的表 %s —— 备份比本程序新，拒绝恢复"+
			"（请先把面板升级到不低于备份来源的版本）", strings.Join(unknown, ", "))
	}

	// 依赖顺序：users 必须先于 sessions（唯一外键 sessions.user_id → users.id）。
	order := []string{"users", "sessions"}
	for _, t := range known {
		if t != "users" && t != "sessions" {
			order = append(order, t)
		}
	}

	report := &ReplaceReport{}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启恢复事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 先删空所有**将被替换**的表。反向顺序删除，读起来与外键依赖一致。
	var toReplace []string
	for _, t := range order {
		if contains(backupTables, t) {
			toReplace = append(toReplace, t)
		} else if knownSet[t] {
			report.SkippedTables = append(report.SkippedTables, t)
		}
	}
	for i := len(toReplace) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(ctx, `DELETE FROM main."`+toReplace[i]+`"`); err != nil {
			return nil, fmt.Errorf("清空表 %s 失败: %w", toReplace[i], err)
		}
	}

	for _, t := range toReplace {
		stat, err := copyTable(ctx, tx, t)
		if err != nil {
			return nil, err
		}
		report.Tables = append(report.Tables, stat)
		if replaceFromBackupHook != nil {
			if err := replaceFromBackupHook("after:" + t); err != nil {
				return nil, fmt.Errorf("恢复表 %s 后注入失败: %w", t, err)
			}
		}
	}

	// sqlite_sequence：AUTOINCREMENT 的下一号。不恢复它，恢复后新建记录会
	// 复用旧主键，或者 seq 比实际最大 id 小 —— 都是难查的数据问题。
	seq, err := restoreSequence(ctx, tx, toReplace)
	if err != nil {
		return nil, err
	}
	for i := range report.Tables {
		report.Tables[i].HadSeq = seq[report.Tables[i].Table]
	}

	if replaceFromBackupHook != nil {
		if err := replaceFromBackupHook("before-commit"); err != nil {
			return nil, fmt.Errorf("提交前注入失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交恢复事务失败: %w", err)
	}
	committed = true

	// 外键一致性必须为空；不为空说明备份本身不自洽（或复制漏了表），
	// 这时**如实报错**，让用户知道库现在是可疑的。
	if err := verifyForeignKeys(ctx, conn); err != nil {
		return report, err
	}

	// 清空会话：旧的 session cookie 在恢复后必须失效，
	// 否则等于把认证状态回退到备份那一刻。
	res, err := conn.ExecContext(ctx, `DELETE FROM sessions`)
	if err != nil {
		return report, fmt.Errorf("清空会话失败: %w", err)
	}
	report.SessionsCleared, _ = res.RowsAffected()

	// 释放唯一连接后再重放迁移（db 只有 1 条连接，不释放会自我死锁）。
	// 顺序**必须**是 DETACH → 恢复外键 → Close：漏掉 DETACH 会让 `backup`
	// 残留在这条池化连接上，第二次恢复直接失败（真机实测）。
	cleanup()
	_ = conn.Close()
	closed = true
	if err := s.migrate(ctx); err != nil {
		return report, fmt.Errorf("恢复后重放迁移失败: %w", err)
	}
	return report, nil
}

// copyTable 在一个事务里把 backup.<table> 的（两库交集）列复制到 main.<table>。
func copyTable(ctx context.Context, tx *sql.Tx, table string) (TableReplaceStat, error) {
	stat := TableReplaceStat{Table: table}
	targetCols, err := tableColumns(ctx, tx, "main", table)
	if err != nil {
		return stat, err
	}
	backupCols, err := tableColumns(ctx, tx, "backup", table)
	if err != nil {
		return stat, err
	}
	backupSet := map[string]bool{}
	for _, c := range backupCols {
		backupSet[c] = true
	}
	var extra []string
	for _, c := range backupCols {
		if !contains(targetCols, c) {
			extra = append(extra, c)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return stat, fmt.Errorf("备份表 %s 含当前程序没有的列 %s —— 备份比本程序新，拒绝恢复",
			table, strings.Join(extra, ", "))
	}
	// 实际复制的列 = 目标列 ∩ 备份列，保持目标列顺序（自增主键等在首位）。
	var cols []string
	for _, c := range targetCols {
		if backupSet[c] {
			cols = append(cols, c)
		} else {
			stat.Missing = append(stat.Missing, c)
		}
	}
	if len(cols) == 0 {
		return stat, fmt.Errorf("备份表 %s 与当前程序没有共同列，拒绝恢复", table)
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + c + `"`
	}
	list := strings.Join(quoted, ",")
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO main."`+table+`" (`+list+`) SELECT `+list+` FROM backup."`+table+`"`); err != nil {
		return stat, fmt.Errorf("复制表 %s 失败: %w", table, err)
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main."`+table+`"`).Scan(&n); err != nil {
		return stat, fmt.Errorf("回读表 %s 行数失败: %w", table, err)
	}
	stat.Rows = n
	stat.Columns = cols
	return stat, nil
}

// restoreSequence 把 backup.sqlite_sequence 的 seq 原样搬到 main。
//
// 逐表处理而不是整表复制：main 里可能还有备份不包含的表（备份比程序旧），
// 它们的 seq 必须保留，不能被一次 DELETE FROM sqlite_sequence 抹掉。
func restoreSequence(ctx context.Context, tx *sql.Tx, tables []string) (map[string]bool, error) {
	out := map[string]bool{}
	hasMain, err := tableExists(ctx, tx, "main", "sqlite_sequence")
	if err != nil {
		return nil, err
	}
	hasBackup, err := tableExists(ctx, tx, "backup", "sqlite_sequence")
	if err != nil {
		return nil, err
	}
	if !hasMain {
		return out, nil
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.sqlite_sequence WHERE name=?`, t); err != nil {
			return nil, fmt.Errorf("清理 %s 的自增序号失败: %w", t, err)
		}
		if !hasBackup {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO main.sqlite_sequence(name,seq) SELECT name,seq FROM backup.sqlite_sequence WHERE name=?`, t)
		if err != nil {
			return nil, fmt.Errorf("恢复 %s 的自增序号失败: %w", t, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out[t] = true
		}
	}
	return out, nil
}

// CheckForeignKeys 跑 `PRAGMA foreign_key_check`；返回非 nil 表示存在外键违规。
//
// 恢复后的"如实回读"要用它：只在写入时查一次不够，用户可能在任务结束后
// 还想确认库现在是干净的（界面上的回读清单）。
func (s *Store) CheckForeignKeys(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("外键自检失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var table, parent string
		var rowid, fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("外键自检读取失败: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("外键自检读取失败: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("外键自检不通过：%d 条违规", n)
	}
	return nil
}

// CountRows 返回若干业务表的行数（恢复后回读站点/反代/任务等数量）。
func (s *Store) CountRows(ctx context.Context, tables ...string) (map[string]int, error) {
	out := map[string]int{}
	known := map[string]bool{}
	for _, t := range KnownTables() {
		known[t] = true
	}
	for _, t := range tables {
		if !known[t] {
			return nil, fmt.Errorf("未知的表: %s", t)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+t+`"`).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

// verifyForeignKeys 跑 PRAGMA foreign_key_check，任何一行都是错误。
func verifyForeignKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("外键自检失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var bad []string
	for rows.Next() {
		var table, parent string
		var rowid, fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("外键自检读取失败: %w", err)
		}
		bad = append(bad, fmt.Sprintf("%s(rowid=%d)→%s", table, rowid, parent))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("外键自检读取失败: %w", err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("恢复后外键自检不通过（%d 条）：%s —— 备份可能不自洽，已保留现场",
			len(bad), strings.Join(head(bad, 5), ", "))
	}
	return nil
}

func head(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------- 小工具 ----------

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func tableNames(ctx context.Context, q querier, schemaName string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM `+schemaName+`.sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的表清单失败: %w", schemaName, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func tableColumns(ctx context.Context, q querier, schemaName, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA `+schemaName+`.table_info("`+table+`")`)
	if err != nil {
		return nil, fmt.Errorf("读取 %s.%s 的列失败: %w", schemaName, table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func tableExists(ctx context.Context, q querier, schemaName, table string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+schemaName+`.sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
