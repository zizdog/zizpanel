// Package store 是面板的数据层，基于单文件 SQLite（纯 Go 驱动，无 CGO）。
//
// 为什么用 SQLite 而不是 JSON/MySQL：
//   - JSON：并发写会损坏，而面板要管理会话、审计、定时任务，写入是常态。
//   - MySQL：面板依赖它管理的数据库，MySQL 一停面板就进不去，形成循环依赖。
//   - SQLite 单文件：零外部依赖，可直接随备份带走，跨机器迁移只需拷一个文件。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store 持有数据库句柄。
type Store struct {
	db   *sql.DB
	path string
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "panel.db")
	// _pragma 参数由 modernc.org/sqlite 识别：
	//   busy_timeout 避免并发写立刻报 SQLITE_BUSY
	//   journal_mode=WAL 读写不互相阻塞
	//   foreign_keys=ON 让外键约束真的生效
	dsn := "file:" + path +
		"?_pragma=busy_timeout(8000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 写操作串行，连接数不宜过多，否则容易锁冲突
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("数据库连接失败: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// DB 暴露底层句柄，供少数需要复杂查询的模块使用。
func (s *Store) DB() *sql.DB { return s.db }

// schema 是当前的完整表结构。所有语句必须幂等（IF NOT EXISTS）。
const schema = `
-- 用户
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    totp_secret   TEXT    NOT NULL DEFAULT '',
    totp_enabled  INTEGER NOT NULL DEFAULT 0,
    is_admin      INTEGER NOT NULL DEFAULT 1,
    last_login_at TEXT    NOT NULL DEFAULT '',
    last_login_ip TEXT    NOT NULL DEFAULT '',
    fail_count    INTEGER NOT NULL DEFAULT 0,
    locked_until  TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL DEFAULT (datetime('now','localtime')),
    updated_at    TEXT    NOT NULL DEFAULT (datetime('now','localtime'))
);

-- 会话
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ip         TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
    expires_at TEXT NOT NULL,
    last_seen  TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- 站点（面板是权威来源，nginx conf 由面板生成）
CREATE TABLE IF NOT EXISTS sites (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    domain      TEXT    NOT NULL UNIQUE,
    aliases     TEXT    NOT NULL DEFAULT '',   -- 逗号分隔的附加域名
    root        TEXT    NOT NULL,
    php_version TEXT    NOT NULL DEFAULT '',   -- 空=纯静态
    rewrite     TEXT    NOT NULL DEFAULT 'none',
    ssl_enabled INTEGER NOT NULL DEFAULT 0,
    ssl_cert    TEXT    NOT NULL DEFAULT '',
    ssl_key     TEXT    NOT NULL DEFAULT '',
    ssl_provider TEXT   NOT NULL DEFAULT '',   -- self/le/manual
    ssl_expires TEXT    NOT NULL DEFAULT '',
    proxy_pass  TEXT    NOT NULL DEFAULT '',   -- 非空则整站反代
    extra_conf  TEXT    NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    remark      TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now','localtime')),
    updated_at  TEXT    NOT NULL DEFAULT (datetime('now','localtime'))
);

-- 服务注册表：裸装与 Docker 统一抽象
-- 反向代理规则（独立的「反向代理」功能）
--
-- 为什么要单独一张表：站点表（sites）的模型是"域名 + 根目录 + PHP"，
-- 而反代规则里很多根本没有站点目录（只是把端口/域名转到别的机器）。
-- 硬塞进站点表会出现"必须填一个并不存在的根目录"这种别扭。
CREATE TABLE IF NOT EXISTS proxies (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL,
    listen        INTEGER NOT NULL,
    domains       TEXT    NOT NULL DEFAULT '',
    path          TEXT    NOT NULL DEFAULT '',
    target        TEXT    NOT NULL,
    preserve_host INTEGER NOT NULL DEFAULT 0,
    websocket     INTEGER NOT NULL DEFAULT 1,
    enabled       INTEGER NOT NULL DEFAULT 1,
    remark        TEXT    NOT NULL DEFAULT '',
    -- 反向代理的 HTTPS（2026-09 新增）。
    -- 默认全关：老规则升级后 ssl_enabled=0，vhost 输出与加这个功能之前逐字一致。
    -- 列名与 sites 表一致，便于复用同一套证书逻辑。
    ssl_enabled   INTEGER NOT NULL DEFAULT 0,
    ssl_cert      TEXT    NOT NULL DEFAULT '',
    ssl_key       TEXT    NOT NULL DEFAULT '',
    ssl_provider  TEXT    NOT NULL DEFAULT '',   -- self/mkcert/manual/acme
    ssl_expires   TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL DEFAULT '',
    updated_at    TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS services (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,   -- 英文标识，如 stt
    display_name TEXT    NOT NULL,          -- 中文展示名
    kind         TEXT    NOT NULL,          -- native | docker | compose
    category     TEXT    NOT NULL DEFAULT 'other',
    icon         TEXT    NOT NULL DEFAULT '',
    description  TEXT    NOT NULL DEFAULT '',
    port         INTEGER NOT NULL DEFAULT 0,
    -- native 专用
    launch_label TEXT NOT NULL DEFAULT '',
    plist_path   TEXT NOT NULL DEFAULT '',
    work_dir     TEXT NOT NULL DEFAULT '',
    start_cmd    TEXT NOT NULL DEFAULT '',
    -- docker / compose 专用
    container    TEXT NOT NULL DEFAULT '',
    compose_file TEXT NOT NULL DEFAULT '',
    image        TEXT NOT NULL DEFAULT '',
    -- 通用
    health_url   TEXT NOT NULL DEFAULT '',
    health_expect TEXT NOT NULL DEFAULT '',
    log_path     TEXT NOT NULL DEFAULT '',
    autostart    INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1,
    managed      INTEGER NOT NULL DEFAULT 0,  -- 1=面板创建，0=仅纳管
    created_at   TEXT NOT NULL DEFAULT (datetime('now','localtime')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);

-- API Key（供外部调用面板/内部服务）
CREATE TABLE IF NOT EXISTS api_keys (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL DEFAULT '',
    scope      TEXT NOT NULL DEFAULT 'read',
    last_used  TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);

-- 操作审计
CREATE TABLE IF NOT EXISTS audit_logs (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       TEXT NOT NULL DEFAULT (datetime('now','localtime')),
    actor    TEXT NOT NULL DEFAULT '',
    ip       TEXT NOT NULL DEFAULT '',
    action   TEXT NOT NULL,
    target   TEXT NOT NULL DEFAULT '',
    detail   TEXT NOT NULL DEFAULT '',
    ok       INTEGER NOT NULL DEFAULT 1,
    message  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_logs(ts);
-- 审计页按 action 做精确筛选（下拉框），单列索引比全表扫划算
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs(action);

-- 键值设置（面板自身可调项）
CREATE TABLE IF NOT EXISTS settings (
    k          TEXT PRIMARY KEY,
    v          TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);

-- 计划任务
CREATE TABLE IF NOT EXISTS cron_jobs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL DEFAULT 'shell', -- shell|backup|url
    schedule   TEXT NOT NULL,                 -- 5 段 cron 表达式
    command    TEXT NOT NULL DEFAULT '',
    work_dir   TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_run   TEXT NOT NULL DEFAULT '',
    last_status TEXT NOT NULL DEFAULT '',
    last_output TEXT NOT NULL DEFAULT '',
    run_count  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);

-- 证书申请记录
CREATE TABLE IF NOT EXISTS certificates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    domain     TEXT NOT NULL,
    provider   TEXT NOT NULL DEFAULT 'self',
    cert_path  TEXT NOT NULL DEFAULT '',
    key_path   TEXT NOT NULL DEFAULT '',
    issued_at  TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    auto_renew INTEGER NOT NULL DEFAULT 1,
    status     TEXT NOT NULL DEFAULT '',
    message    TEXT NOT NULL DEFAULT ''
);

-- 数据库凭据：面板自己下发过的数据库账号口令的**唯一**保存处。
--
-- 为什么需要这张表（这是本功能能诚实存在的唯一前提）：
--   MySQL 里存的是口令的哈希（mysql.user.authentication_string），
--   **任何面板都无法从数据库反推出原文**。宝塔之所以能"显示密码"，
--   是因为它在创建库/账号时把明文自己存了下来。面板要诚实地做到同一件事，
--   也必须自己存 —— 而且**只存面板自己下发过的**：别人手工建的账号这里没有记录，
--   页面必须显示"不可回显"，绝不假装知道。
--
-- 唯一键 (user, host)：与 mysql.user 的账号身份一一对应；改口令就是 UPSERT 覆盖，
-- 不会留下旧口令。注意 SQLite 的 TEXT 主键是区分大小写的，而 MySQL 的用户名
-- 也区分大小写（主机名不区分），所以这里按原样保存，不额外做大小写折叠，
-- 避免把两个真实存在的不同账号合并成一条。
CREATE TABLE IF NOT EXISTS db_credentials (
    user       TEXT NOT NULL,
    host       TEXT NOT NULL,
    password   TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'panel',   -- 谁写的：panel / site-install / import
    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
    PRIMARY KEY (user, host)
);
CREATE INDEX IF NOT EXISTS idx_db_credentials_user ON db_credentials(user);
`

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range splitStatements(schema) {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("数据库迁移失败: %w\nSQL: %s", err, firstLine(stmt))
		}
	}
	// 增量迁移：老库的 proxies 表是上面 CREATE TABLE IF NOT EXISTS 建不出来的
	// （表已存在时整条语句被忽略），所以新增列必须显式 ALTER TABLE 补。
	//
	// 为什么必须向后兼容：用户已经有一批在用的反代规则，升级面板时不能因为
	// 多了几列就让规则失效或让启动报错。ADD COLUMN 带 NOT NULL DEFAULT 会给
	// 已有行填默认值 —— ssl_enabled=0 正是"老规则默认关 SSL"。
	if err := s.ensureColumns(ctx, "proxies", map[string]string{
		"ssl_enabled":  "INTEGER NOT NULL DEFAULT 0",
		"ssl_cert":     "TEXT NOT NULL DEFAULT ''",
		"ssl_key":      "TEXT NOT NULL DEFAULT ''",
		"ssl_provider": "TEXT NOT NULL DEFAULT ''",
		"ssl_expires":  "TEXT NOT NULL DEFAULT ''",
		// HTTPS 上游的 SNI 与"一键常用请求头"（详见 proxies.Rule）。
		"tls_name":         "TEXT NOT NULL DEFAULT ''",
		"standard_headers": "INTEGER NOT NULL DEFAULT 0",
		"redirect_http":    "INTEGER NOT NULL DEFAULT 0",
	}); err != nil {
		return err
	}
	// 记录 schema 版本，后续增量迁移用
	return s.setMeta(ctx, "schema_version", "2")
}

// ensureColumns 给已存在的表补齐缺失的列（SQLite 的 ADD COLUMN）。
//
// 只做"加列"，不改类型、不删列：迁移要能在任何老库上重复执行而不出错，
// 且绝不能动用户已有数据。列名/定义都来自本包内的常量，不经用户输入，无注入面。
func (s *Store) ensureColumns(ctx context.Context, table string, cols map[string]string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
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
		if _, err := s.db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, n, cols[n])); err != nil {
			return fmt.Errorf("迁移 %s.%s 失败: %w", table, n, err)
		}
	}
	return nil
}

// splitStatements 按分号切分 SQL。表结构里不含字符串字面量中的分号，
// 所以简单切分是安全的；这里仍做了空语句过滤。
func splitStatements(sqlText string) []string {
	parts := strings.Split(sqlText, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// 去掉纯注释行
		lines := strings.Split(p, "\n")
		keep := make([]string, 0, len(lines))
		for _, ln := range lines {
			t := strings.TrimSpace(ln)
			if t == "" || strings.HasPrefix(t, "--") {
				continue
			}
			keep = append(keep, ln)
		}
		if len(keep) == 0 {
			continue
		}
		out = append(out, strings.Join(keep, "\n"))
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// ---------- 元信息 ----------

func (s *Store) setMeta(ctx context.Context, k, v string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(k,v,updated_at) VALUES(?,?,datetime('now','localtime'))
		 ON CONFLICT(k) DO UPDATE SET v=excluded.v, updated_at=excluded.updated_at`, k, v)
	return err
}

// GetSetting 读取一个设置项，不存在返回空串。
func (s *Store) GetSetting(ctx context.Context, k string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting 写入一个设置项。
func (s *Store) SetSetting(ctx context.Context, k, v string) error {
	return s.setMeta(ctx, k, v)
}

// Stats 返回一些基础计数，用于仪表盘。
func (s *Store) Stats(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	queries := map[string]string{
		"users":    `SELECT COUNT(*) FROM users`,
		"sites":    `SELECT COUNT(*) FROM sites`,
		"services": `SELECT COUNT(*) FROM services`,
		"api_keys": `SELECT COUNT(*) FROM api_keys`,
		"cron":     `SELECT COUNT(*) FROM cron_jobs`,
	}
	for k, q := range queries {
		var n int
		if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, nil
}

// PurgeExpiredSessions 清理过期会话，启动时和定时调用。
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < datetime('now','localtime')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
