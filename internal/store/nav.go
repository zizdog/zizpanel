package store

// nav.go —— 「导航页」（sun-panel 风格的图标网格首页）的数据层。
//
// 为什么自己开发而不是原生部署 sun-panel / home-dash 这类现成项目：
//   · 现成项目要么带 Node/Vue 构建链（违背本项目「无构建步骤」的路线），
//     要么自带一套运行时、数据库、鉴权与端口 —— 等于在面板之外再养一套
//     「要备份、要升级、要鉴权、要单独记口令」的系统；
//   · 而导航页的数据量极小（几个组、几十条链接），做进面板可以直接复用
//     面板已有的鉴权、风格、备份/恢复、日志与审计，不需要第二套凭据。
// 所以这里只用两张表 + 一组面板 API，数据随 panel.db 一起走现有的备份链路。
//
// 表结构刻意不写进 store.go 的 schema 常量（那里是多个并行任务的热点），
// 由 EnsureNavTables 在面板启动时用 CREATE TABLE IF NOT EXISTS 单独建。
//
// 备份/恢复：备份是整库 VACUUM INTO，导航数据会随之进备份；而恢复的兼容性
// 判定只认 store.KnownTables()，所以本文件的建表语句必须登记进去 ——
// 由文件末尾的 init() 调 registerSchemaSource(navSchema) 完成，KnownTables()
// 会把它与主 schema 合并。漏登记会让含导航数据的备份被判成「备份比程序新」而
// 拒绝恢复；store/schema_known_tables_test.go 用一条遍历全目录的门禁锁死这类遗漏。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNavNotFound 表示指定的导航组/站点不存在。
// HTTP 层据此返回 404，而不是把「本来就没有」报成 500。
var ErrNavNotFound = errors.New("导航条目不存在")

// navSchema 是导航页的两张表。全部语句幂等，可在任何旧库上重复执行。
//
// nav_items.group_id 带 ON DELETE CASCADE：面板开启 foreign_keys=ON，
// 组被删时子项不会变成孤儿。DeleteNavGroup 里仍显式删一次子项 ——
// 即使有人关了外键约束，语义也不会漂。
const navSchema = `
CREATE TABLE IF NOT EXISTS nav_groups (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    sort       INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL DEFAULT (datetime('now','localtime'))
);

CREATE TABLE IF NOT EXISTS nav_items (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id    INTEGER NOT NULL REFERENCES nav_groups(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    url         TEXT    NOT NULL,
    icon        TEXT    NOT NULL DEFAULT '',
    description TEXT    NOT NULL DEFAULT '',
    open_new_tab INTEGER NOT NULL DEFAULT 1,
    sort        INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_nav_items_group ON nav_items(group_id);

CREATE TABLE IF NOT EXISTS nav_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// NavGroup 是一个分组（导航页上的一个区段）。
type NavGroup struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Sort      int    `json:"sort"`
	CreatedAt string `json:"created_at"`
}

// NavItem 是一个站点卡片。
//
// Icon 是「emoji 或图片 URL，二选一或都为空」，具体校验在 web 层
// （见 validateNavIcon）：store 层只负责原样存取。
type NavItem struct {
	ID          int64  `json:"id"`
	GroupID     int64  `json:"group_id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	OpenNewTab  bool   `json:"open_new_tab"`
	Sort        int    `json:"sort"`
	CreatedAt   string `json:"created_at"`
}

// NavKnownTables 返回导航页建的表名。
//
// 从 navSchema 派生而不是抄一份名字列表：抄一份就多一处会走样的真相源，
// 而备份兼容性判定完全依赖这份名单（见 restore.go 的 KnownTables）。
func NavKnownTables() []string {
	var out []string
	for _, stmt := range splitStatements(navSchema) {
		if name := createTableName(stmt); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// init 把导航页的建表语句登记进 KnownTables()（备份兼容性判定的唯一依据）。
func init() { registerSchemaSource(navSchema) }

// EnsureNavTables 建表（幂等）。由面板启动时调用，各个 nav handler 也会调用一次。
//
// 为什么 handler 里也调用：单测与「面板已启动但还没跑过 migrate 后的新代码」
// 这两种场景都要求「第一次请求就能用」；CREATE TABLE IF NOT EXISTS 是空操作，
// 代价可以忽略。真正的建表时机仍是启动时那一次。
func EnsureNavTables(ctx context.Context, db *sql.DB) error {
	for _, stmt := range splitStatements(navSchema) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("导航页建表失败: %w\nSQL: %s", err, firstLine(stmt))
		}
	}
	return nil
}

// ---------- 分组 ----------

// ListNavGroups 按 (sort, id) 返回全部分组。
//
// 排序键把 id 作为第二列：sort 相同的行（历史数据或手工改库）也要有稳定顺序，
// 否则每次渲染网格的顺序都可能变，用户会觉得「自己排的序没保存」。
func (s *Store) ListNavGroups(ctx context.Context) ([]NavGroup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, sort, created_at FROM nav_groups ORDER BY sort ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []NavGroup{}
	for rows.Next() {
		var g NavGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Sort, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateNavGroup 新建分组。sort 传 -1 表示「追加到末尾」。
func (s *Store) CreateNavGroup(ctx context.Context, name string, sort int) (*NavGroup, error) {
	if sort < 0 {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(sort), -1) + 1 FROM nav_groups`).Scan(&sort); err != nil {
			return nil, err
		}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nav_groups(name, sort) VALUES(?, ?)`, name, sort)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetNavGroup(ctx, id)
}

// GetNavGroup 读一个分组。
func (s *Store) GetNavGroup(ctx context.Context, id int64) (*NavGroup, error) {
	var g NavGroup
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, sort, created_at FROM nav_groups WHERE id=?`, id).
		Scan(&g.ID, &g.Name, &g.Sort, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNavNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// UpdateNavGroup 改名。sort 传 -1 表示不改。
func (s *Store) UpdateNavGroup(ctx context.Context, id int64, name string, sort int) error {
	cur, err := s.GetNavGroup(ctx, id)
	if err != nil {
		return err
	}
	if sort < 0 {
		sort = cur.Sort
	}
	_, err = s.db.ExecContext(ctx, `UPDATE nav_groups SET name=?, sort=? WHERE id=?`, name, sort, id)
	return err
}

// DeleteNavGroup 删除分组及其下所有站点（一个事务）。
func (s *Store) DeleteNavGroup(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // 已提交时是空操作
	if _, err := tx.ExecContext(ctx, `DELETE FROM nav_items WHERE group_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM nav_groups WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNavNotFound
	}
	return tx.Commit()
}

// ReorderNavGroups 按给定 id 顺序重排（sort = 下标）。
//
// ids 只包含需要调整的分组，未出现在 ids 里的分组保持原 sort ——
// 前端只需把当前可见顺序整串发回来。
func (s *Store) ReorderNavGroups(ctx context.Context, ids []int64) error {
	return s.reorder(ctx, "nav_groups", ids)
}

// ---------- 站点 ----------

// ListNavItems 按 (group_id, sort, id) 返回全部站点。
func (s *Store) ListNavItems(ctx context.Context) ([]NavItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, group_id, name, url, icon, description, open_new_tab, sort, created_at
		   FROM nav_items ORDER BY group_id ASC, sort ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []NavItem{}
	for rows.Next() {
		it, err := scanNavItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

func scanNavItem(sc interface{ Scan(...any) error }) (*NavItem, error) {
	var it NavItem
	var open int
	if err := sc.Scan(&it.ID, &it.GroupID, &it.Name, &it.URL, &it.Icon,
		&it.Description, &open, &it.Sort, &it.CreatedAt); err != nil {
		return nil, err
	}
	it.OpenNewTab = open == 1
	return &it, nil
}

// GetNavItem 读一个站点。
func (s *Store) GetNavItem(ctx context.Context, id int64) (*NavItem, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, group_id, name, url, icon, description, open_new_tab, sort, created_at
		   FROM nav_items WHERE id=?`, id)
	it, err := scanNavItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNavNotFound
	}
	return it, err
}

// CreateNavItem 新建站点。Sort 传 -1 表示追加到该组末尾。
func (s *Store) CreateNavItem(ctx context.Context, it NavItem) (*NavItem, error) {
	if it.Sort < 0 {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(sort), -1) + 1 FROM nav_items WHERE group_id=?`,
			it.GroupID).Scan(&it.Sort); err != nil {
			return nil, err
		}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nav_items(group_id, name, url, icon, description, open_new_tab, sort)
		 VALUES(?,?,?,?,?,?,?)`,
		it.GroupID, it.Name, it.URL, it.Icon, it.Description, boolToInt(it.OpenNewTab), it.Sort)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetNavItem(ctx, id)
}

// UpdateNavItem 整条更新。Sort 传 -1 表示保持原值。
func (s *Store) UpdateNavItem(ctx context.Context, it NavItem) error {
	cur, err := s.GetNavItem(ctx, it.ID)
	if err != nil {
		return err
	}
	if it.Sort < 0 {
		it.Sort = cur.Sort
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE nav_items SET group_id=?, name=?, url=?, icon=?, description=?, open_new_tab=?, sort=?
		  WHERE id=?`,
		it.GroupID, it.Name, it.URL, it.Icon, it.Description, boolToInt(it.OpenNewTab), it.Sort, it.ID)
	return err
}

// DeleteNavItem 删除一个站点。
func (s *Store) DeleteNavItem(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nav_items WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNavNotFound
	}
	return nil
}

// ReorderNavItems 按给定 id 顺序重排（只动 sort，不动所属分组）。
func (s *Store) ReorderNavItems(ctx context.Context, ids []int64) error {
	return s.reorder(ctx, "nav_items", ids)
}

// reorder 是两张表共用的排序实现（表名只来自本文件常量，无注入面）。
func (s *Store) reorder(ctx context.Context, table string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET sort=? WHERE id=?`, i, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- 导入（整份替换） ----------

// ReplaceNav 用给定内容**整份替换**导航数据（一个事务）。
//
// 语义与用户确认文案一致：先清空两张表，再按传入顺序写入。
// id > 0 时沿用原 id（导出→导入往返后 id 不变，便于比对与增量迁移），
// 否则交给 AUTOINCREMENT。
func (s *Store) ReplaceNav(ctx context.Context, groups []NavGroup, items []NavItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM nav_items`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nav_groups`); err != nil {
		return err
	}
	// 旧行已清空，显式写入 id 不会冲突；sqlite_sequence 会用 max(id) 兜底。
	for i, g := range groups {
		sort := g.Sort
		if sort < 0 {
			sort = i
		}
		if g.ID > 0 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO nav_groups(id, name, sort) VALUES(?,?,?)`, g.ID, g.Name, sort); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nav_groups(name, sort) VALUES(?,?)`, g.Name, sort); err != nil {
			return err
		}
	}
	for i, it := range items {
		sort := it.Sort
		if sort < 0 {
			sort = i
		}
		if it.ID > 0 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO nav_items(id, group_id, name, url, icon, description, open_new_tab, sort)
				 VALUES(?,?,?,?,?,?,?,?)`,
				it.ID, it.GroupID, it.Name, it.URL, it.Icon, it.Description,
				boolToInt(it.OpenNewTab), sort); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nav_items(group_id, name, url, icon, description, open_new_tab, sort)
			 VALUES(?,?,?,?,?,?,?)`,
			it.GroupID, it.Name, it.URL, it.Icon, it.Description,
			boolToInt(it.OpenNewTab), sort); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- 外观设置（标题 / 副标题 / 主题色 / 背景图）----------

// NavSettings 是导航页的外观设置。
//
// 用户 2026-09-18 要求：「标题要可以改！要可以自定义背景图！要可以指定主题色！」
//
// 为什么存**导航页自己的表**而不是面板 config：
//   - 它属于导航页的数据，应该跟分组/站点一起进备份与恢复 ——
//     nav_settings 由 navSchema 派生，自动出现在 KnownTables()（备份兼容性判定的唯一依据）；
//   - 面板 config 是**面板级**设置（监听地址、TLS、镜像…）。把页面外观塞进去，
//     "恢复导航页数据"就变成了"改面板全局配置"，那是两件不同的事。
//
// 空值有明确含义 = "用默认"：标题回落到「导航页」、背景回落到无图、
// 主题色回落到面板品牌色。所以保存空串是**有效操作**（清除自定义），不是异常。
type NavSettings struct {
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle"`
	Accent     string `json:"accent"`
	Background string `json:"background"`
}

// navSettingKeys 是允许写入的键（白名单）。
//
// 不用"随便存"：这张表将来可能被别处复用，放开键名等于放开一个任意键值存储，
// 备份/恢复时的语义也就说不清了。
var navSettingKeys = []string{"title", "subtitle", "accent", "background"}

// NavSettings 读取外观设置（缺的键回落到空串 = 用默认）。
func (s *Store) NavSettings(ctx context.Context) (NavSettings, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM nav_settings`)
	if err != nil {
		return NavSettings{}, err
	}
	defer func() { _ = rows.Close() }()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return NavSettings{}, err
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return NavSettings{}, err
	}
	return NavSettings{
		Title:      m["title"],
		Subtitle:   m["subtitle"],
		Accent:     m["accent"],
		Background: m["background"],
	}, nil
}

// SaveNavSettings 整体写入外观设置（单事务；空值也写，表示"用默认"）。
func (s *Store) SaveNavSettings(ctx context.Context, n NavSettings) error {
	vals := map[string]string{
		"title": n.Title, "subtitle": n.Subtitle,
		"accent": n.Accent, "background": n.Background,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, k := range navSettingKeys {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nav_settings(key, value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, vals[k]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
