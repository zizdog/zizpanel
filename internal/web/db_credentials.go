package web

// db_credentials.go —— 数据库账号口令的"保管与回读"。
//
// ============================================================================
//  为什么需要这个文件（诚实原则的落点）
//
//  MySQL 里存的是口令的哈希（mysql.user.authentication_string），
//  **任何面板都无法从数据库反推出口令原文**。宝塔之所以能"显示密码"，
//  是因为它在创建库/账号时把明文自己存了下来。面板要诚实地做到同一件事，
//  也必须自己存 —— 而且只能存**面板自己下发过的**那些。
//
//  由此产生两种必须如实区分的状态：
//    · 面板创建 / 改过的账号  → 面板知道口令     → password_known=true，可以显示
//    · 别人手工建、或面板旧版本建的 → 不可知       → password_known=false，显示"不可回显"
//
//  **绝不允许**在不可知时返回空串或编一个值冒充 —— 能谎报成功的功能比没做更糟。
//
// ============================================================================
//  口令的处置纪律（本仓库硬性约定）
//
//   1. 口令只允许出现在这里与 mysql 客户端的调用参数中；
//      **绝不进日志、审计、任务步骤、错误消息**（审计里只写 source 这类元信息）。
//   2. 写库失败必须如实返回错误，不许静默吞掉 ——
//      否则用户下次看不到口令，却以为面板存了。
//   3. 读失败（签名没有 error 位）一律按"不可知"处理，宁可显示"不知道"，
//      也不许把读失败伪装成"没有口令"。
// ============================================================================

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zizdog/zizpanel/internal/mysql"
)

// 口令来源。审计里可以写它（它不敏感），口令本身不行。
const (
	// DBCredSourcePanel 面板自己在「数据库 → 账号与权限」里创建/改写的口令。
	DBCredSourcePanel = "panel"
	// DBCredSourceSiteInstall 一键建站（WordPress / Typecho）创建的库账号口令。
	DBCredSourceSiteInstall = "site-install"
	// DBCredSourceImport 从备份/迁移导入的账号口令。
	DBCredSourceImport = "import"
)

// dbCredentialRecord 是 db_credentials 表的一行。
//
// Password 是明文口令 —— 这是这张表存在的全部意义。它**绝不允许**出现在
// 任何日志、审计、任务步骤或错误消息里；本文件里的所有错误信息都不带它。
type dbCredentialRecord struct {
	User      string
	Host      string
	Password  string
	Source    string
	CreatedAt string
	UpdatedAt string
}

// dbCredKey 是内存里合并"账号 → 口令记录"用的键。
//
// 用 NUL 分隔而不是 "@"：MySQL 的账号名/主机名都可能含 "@" 类字符，
// 用 "@" 拼键会有撞键风险（一旦撞键就会把一个账号的口令显示到另一个账号上）。
func dbCredKey(user, host string) string { return user + "\x00" + host }

// normalizeDBCredIdentity 归一化账号身份。
// host 为空时按 MySQL 的惯例落到 localhost（与 handleDatabaseUserCreate 的默认值一致）。
func normalizeDBCredIdentity(user, host string) (string, string, error) {
	user = strings.TrimSpace(user)
	host = strings.TrimSpace(host)
	if user == "" {
		return "", "", errors.New("账号名为空，拒绝保存/查询口令")
	}
	if host == "" {
		host = "localhost"
	}
	return user, host, nil
}

// RememberDBPassword 在**MySQL 侧的操作确实成功之后**记下账号口令，供日后回显。
//
// 调用时机（顺序不能反）：
//   - 创建账号：c.CreateUser(...) 返回 nil 之后；
//   - 改口令：  c.SetUserPassword(...) 返回 nil 之后；
//   - 一键建站：c.CreateUser(...) 返回 nil 之后、写站点配置文件之前。
//
// 反例：先记后做。那样一旦 MySQL 侧失败，面板就会显示一个"其实没生效"的口令 ——
// 这正是用户最容易被误导的地方。
//
// source 取值见 DBCredSource* 常量；为空时按 panel 处理。
// 写失败**原样返回错误**（由调用方决定如何提示用户），绝不吞掉。
func (s *Server) RememberDBPassword(ctx context.Context, user, host, password, source string) error {
	user, host, err := normalizeDBCredIdentity(user, host)
	if err != nil {
		return err
	}
	if source == "" {
		source = DBCredSourcePanel
	}
	db := s.panelDB()
	if db == nil {
		return errors.New("面板数据库不可用，无法保存口令")
	}
	// UPSERT：改口令就是覆盖，唯一键 (user,host) 保证不会留下旧口令。
	// created_at 保持首次写入的值，updated_at 每次刷新。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO db_credentials(user, host, password, source, created_at, updated_at)
		 VALUES(?, ?, ?, ?, datetime('now','localtime'), datetime('now','localtime'))
		 ON CONFLICT(user, host) DO UPDATE SET
		     password   = excluded.password,
		     source     = excluded.source,
		     updated_at = excluded.updated_at`,
		user, host, password, source); err != nil {
		// 错误信息里只会出现表名/列名，不会出现被写入的口令值。
		return fmt.Errorf("保存口令失败: %w", err)
	}
	return nil
}

// RecallDBPassword 读回面板为某个账号保存的口令。
//
// known=false 表示**面板不知道**这个账号的口令（从没存过、已被清理，或读取失败）。
// 它**不等于**"这个账号没有口令"。调用方必须据此显示"不可回显"，
// 而不是显示空串。
func (s *Server) RecallDBPassword(ctx context.Context, user, host string) (password string, known bool) {
	rec, ok, err := s.recallDBPasswordRecord(ctx, user, host)
	if err != nil {
		// 签名没有 error 位，且读失败必须按"不可知"处理（宁可显示"不知道"）。
		// 这条日志里不含口令；写它是为了排障时能看出"是读取失败而非没存过"。
		s.logWarn("读取数据库口令记录失败（按不可知处理）: %v", err)
		return "", false
	}
	if !ok {
		return "", false
	}
	return rec.Password, true
}

// ForgetDBPassword 在账号被删除后清理它保存的口令。
//
// 幂等：没有记录也算成功（删一个没存过的账号不该报错）。
func (s *Server) ForgetDBPassword(ctx context.Context, user, host string) error {
	user, host, err := normalizeDBCredIdentity(user, host)
	if err != nil {
		return err
	}
	db := s.panelDB()
	if db == nil {
		return errors.New("面板数据库不可用，无法清理口令记录")
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM db_credentials WHERE user = ? AND host = ?`, user, host); err != nil {
		return fmt.Errorf("清理口令记录失败: %w", err)
	}
	return nil
}

// recallDBPasswordRecord 是 RecallDBPassword 的内部实现（带 error，供视图合并用）。
func (s *Server) recallDBPasswordRecord(ctx context.Context, user, host string) (dbCredentialRecord, bool, error) {
	user, host, err := normalizeDBCredIdentity(user, host)
	if err != nil {
		return dbCredentialRecord{}, false, err
	}
	db := s.panelDB()
	if db == nil {
		return dbCredentialRecord{}, false, errors.New("面板数据库不可用")
	}
	var rec dbCredentialRecord
	err = db.QueryRowContext(ctx,
		`SELECT user, host, password, source, created_at, updated_at
		   FROM db_credentials WHERE user = ? AND host = ?`, user, host).
		Scan(&rec.User, &rec.Host, &rec.Password, &rec.Source, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return dbCredentialRecord{}, false, nil
	}
	if err != nil {
		return dbCredentialRecord{}, false, err
	}
	return rec, true, nil
}

// allDBPasswordRecords 一次读出全部已知口令，按 dbCredKey 索引。
//
// 一次查完再在内存里合并：账号列表可能有几十上百个，
// 逐个 SELECT 会让"打开数据库页"变成 N+1 次查询。
func (s *Server) allDBPasswordRecords(ctx context.Context) (map[string]dbCredentialRecord, error) {
	db := s.panelDB()
	if db == nil {
		return nil, errors.New("面板数据库不可用")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT user, host, password, source, created_at, updated_at FROM db_credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]dbCredentialRecord{}
	for rows.Next() {
		var rec dbCredentialRecord
		if err := rows.Scan(&rec.User, &rec.Host, &rec.Password,
			&rec.Source, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out[dbCredKey(rec.User, rec.Host)] = rec
	}
	return out, rows.Err()
}

// dbUserView 是 GET /api/v1/database 返回给页面的账号条目。
//
// 内嵌 mysql.DBUser 保留原有全部字段（user/host/privileges/is_locked/has_password/
// auth_plugin/databases/system），在其上追加口令可见性字段：
//
//	password         已知时才出现（omitempty）—— **未知时字段缺席**，
//	                 不用空串冒充；前端靠 password_known 判断
//	password_known   true=面板知道口令（可以显示/复制）
//	password_source  面板给的来源：panel / site-install / import
//	panel_account    true=这是面板连接 MySQL 自己用的身份（root@localhost 等）
type dbUserView struct {
	mysql.DBUser
	PasswordKnown  bool   `json:"password_known"`
	Password       string `json:"password,omitempty"`
	PasswordSource string `json:"password_source,omitempty"`
	PanelAccount   bool   `json:"panel_account,omitempty"`
}

// databaseUsersView 把 MySQL 的真实账号列表与面板保管的口令合并成页面视图。
//
// panelCred 是这次真连接验证出来的凭据状态（见 mysqlCredentialState）。
// 它只用来判断**面板自己的账号**在"配置里没有口令"时，
// 是不是"已验证确实无口令"（ok_no_password）—— 那是"已知为空"，
// 而不是"未知"，两者在页面上的说法完全不同。
//
// 返回的 error 只表示"读口令记录失败"；此时仍返回视图（password_known 全为 false，
// 即降级为"不可回显"），由调用方如实告知页面，而不是让整个账号列表打不开。
func (s *Server) databaseUsersView(ctx context.Context, users []mysql.DBUser, panelCred mysqlCredentialView) ([]dbUserView, error) {
	records, recErr := s.allDBPasswordRecords(ctx)
	if records == nil {
		records = map[string]dbCredentialRecord{}
	}
	views := make([]dbUserView, 0, len(users))
	for _, u := range users {
		v := dbUserView{DBUser: u}
		if rec, ok := records[dbCredKey(u.User, u.Host)]; ok {
			v.PasswordKnown = true
			v.Password = rec.Password
			v.PasswordSource = rec.Source
		}
		// 面板连接 MySQL 用的身份（通常是 root@localhost）：
		// 它的口令就是面板配置里那一个。root 必须在列表里能看到口令，
		// 所以这里为它单独兜底两条路径。
		if s.isPanelMySQLAccount(u.User, u.Host) {
			v.PanelAccount = true
			if cfgPw := s.mysqlPassword(); cfgPw != "" {
				// 配置里的口令是面板**实际在用**的那个，也是 credential.state
				// 所描述的对象；有它就以它为准（可能比历史记录更新）。
				v.PasswordKnown = true
				v.Password = cfgPw
				v.PasswordSource = DBCredSourcePanel
			} else if !v.PasswordKnown && panelCred.State == "ok_no_password" {
				// 真连接验证过服务器确实无口令 —— 这是"已知为空"而非"未知"：
				// 显示为空口令，而不是"不可回显"。
				v.PasswordKnown = true
				v.Password = ""
				v.PasswordSource = DBCredSourcePanel
			}
		}
		views = append(views, v)
	}
	return views, recErr
}

// panelDB 返回面板 SQLite 句柄（对 Store 缺失做保护，便于单测注入半成品 Server）。
func (s *Server) panelDB() *sql.DB {
	if s == nil || s.Store == nil {
		return nil
	}
	return s.Store.DB()
}

// logWarn 写一条面板日志。日志里**只允许**出现账号名与错误原因，绝不含口令。
// s.Log 在个别单测构造的 Server 上可能为 nil，这里做保护。
func (s *Server) logWarn(format string, args ...any) {
	if s == nil || s.Log == nil {
		return
	}
	s.Log.Warn(format, args...)
}
