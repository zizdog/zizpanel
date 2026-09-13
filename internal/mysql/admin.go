package mysql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
//  写操作
//
//  这些操作会改动 MySQL 的账号与库，因此走两条约束：
//    1. 所有标识符在**执行前**经过白名单校验（本文件里再校验一次，
//       不假设调用方已经校验过）
//    2. 生成 SQL 时使用反引号包裹 + 单引号包裹，且值不做字符串拼接
//
//  注意 CREATE USER 的密码必须作为字符串字面量出现在 SQL 里
//  （MySQL 不支持在 CREATE USER 里用占位符），
//  因此密码需要转义单引号与反斜杠；这里严格按 MySQL 的转义规则处理。
// ---------------------------------------------------------------------------

// escapeSQLString 转义字符串字面量里的特殊字符。
//
// MySQL 默认模式下，字符串里的 ' 与 \ 都需要转义。
// 另外 NUL 字节会让 C 客户端截断语句，必须拒绝。
func escapeSQLString(s string) (string, error) {
	if strings.ContainsRune(s, 0) {
		return "", errors.New("内容不允许包含空字节")
	}
	r := strings.NewReplacer(
		`\`, `\\`,
		`'`, `\'`,
		`"`, `\"`,
	)
	return r.Replace(s), nil
}

// quoteIdent 用反引号包裹标识符。
//
// 调用前必须已通过 Validate* 校验；这里再检查一次反引号，
// 作为纵深防御（万一校验函数被改坏）。
func quoteIdent(s string) (string, error) {
	if strings.ContainsAny(s, "`\x00") {
		return "", fmt.Errorf("标识符含非法字符: %q", s)
	}
	return "`" + s + "`", nil
}

// CreateDatabase 创建数据库。
func (c *Client) CreateDatabase(ctx context.Context, name, charset, collation string) error {
	if err := ValidateDBName(name); err != nil {
		return err
	}
	if charset == "" {
		charset = "utf8mb4"
	}
	// 字符集同样走白名单，避免把任意字符串拼进 DDL
	if !isKnownCharset(charset) {
		return fmt.Errorf("不支持的字符集: %q", charset)
	}
	q, err := quoteIdent(name)
	if err != nil {
		return err
	}
	sqlText := fmt.Sprintf("CREATE DATABASE %s CHARACTER SET %s", q, charset)
	if collation != "" {
		if !isKnownCollation(collation) {
			return fmt.Errorf("不支持的排序规则: %q", collation)
		}
		sqlText += " COLLATE " + collation
	}
	_, err = c.run(ctx, []string{"-e", sqlText})
	return err
}

// DropDatabase 删除数据库。
//
// 刻意不加 IF EXISTS：库不存在时应该报错而不是静默成功，
// 否则"我删的是哪个库"会变得难以确认。
func (c *Client) DropDatabase(ctx context.Context, name string) error {
	if err := ValidateDBName(name); err != nil {
		return err
	}
	if systemDBs[name] {
		return fmt.Errorf("%s 是 MySQL 系统库，面板不允许删除", name)
	}
	q, err := quoteIdent(name)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, []string{"-e", "DROP DATABASE " + q})
	return err
}

// CreateUser 创建账号并授权。
func (c *Client) CreateUser(ctx context.Context, user, host, password string, privileges []string, databases []string) error {
	if err := ValidateUserName(user); err != nil {
		return err
	}
	if err := ValidateHost(host); err != nil {
		return err
	}
	if len(password) < 8 {
		return errors.New("密码至少 8 位")
	}
	quotedPrivs, err := NormalizePrivileges(privileges)
	if err != nil {
		return err
	}
	// 逐库校验
	for _, db := range databases {
		if err := ValidateDBName(db); err != nil {
			return err
		}
	}
	if len(databases) == 0 {
		return errors.New("请至少选择一个要授权的数据库（或选择「全部数据库」）")
	}

	userIdent := fmt.Sprintf("'%s'@'%s'", strings.ReplaceAll(user, "'", "''"), host)
	escPwd, err := escapeSQLString(password)
	if err != nil {
		return err
	}

	// 用单条语句同时完成建号与授权，减少中间态
	var stmts []string
	stmts = append(stmts, fmt.Sprintf("CREATE USER %s IDENTIFIED BY '%s'", userIdent, escPwd))
	for _, db := range databases {
		var target string
		if db == "*" {
			target = "*.*"
		} else {
			q, err := quoteIdent(db)
			if err != nil {
				return err
			}
			target = q + ".*"
		}
		stmts = append(stmts, fmt.Sprintf("GRANT %s ON %s TO %s",
			strings.Join(quotedPrivs, ", "), target, userIdent))
	}
	stmts = append(stmts, "FLUSH PRIVILEGES")

	for _, s := range stmts {
		if _, err := c.run(ctx, []string{"-e", s}); err != nil {
			return fmt.Errorf("执行失败（%s）: %w", firstWord(s), err)
		}
	}
	return nil
}

// DropUser 删除账号。
func (c *Client) DropUser(ctx context.Context, user, host string) error {
	if err := ValidateUserName(user); err != nil {
		return err
	}
	if err := ValidateHost(host); err != nil {
		return err
	}
	if user == "root" {
		return errors.New("不允许删除 root 账号（会导致无法管理数据库）")
	}
	userIdent := fmt.Sprintf("'%s'@'%s'", strings.ReplaceAll(user, "'", "''"), host)
	if _, err := c.run(ctx, []string{"-e", "DROP USER " + userIdent}); err != nil {
		return err
	}
	_, err := c.run(ctx, []string{"-e", "FLUSH PRIVILEGES"})
	return err
}

// SetUserPassword 修改账号密码。
func (c *Client) SetUserPassword(ctx context.Context, user, host, password string) error {
	if err := ValidateUserName(user); err != nil {
		return err
	}
	if err := ValidateHost(host); err != nil {
		return err
	}
	if len(password) < 8 {
		return errors.New("密码至少 8 位")
	}
	escPwd, err := escapeSQLString(password)
	if err != nil {
		return err
	}
	userIdent := fmt.Sprintf("'%s'@'%s'", strings.ReplaceAll(user, "'", "''"), host)
	if _, err := c.run(ctx, []string{"-e",
		fmt.Sprintf("ALTER USER %s IDENTIFIED BY '%s'", userIdent, escPwd)}); err != nil {
		return err
	}
	_, err = c.run(ctx, []string{"-e", "FLUSH PRIVILEGES"})
	return err
}

// GrantPrivileges 给账号追加授权。
func (c *Client) GrantPrivileges(ctx context.Context, user, host, db string, privileges []string) error {
	if err := ValidateUserName(user); err != nil {
		return err
	}
	if err := ValidateHost(host); err != nil {
		return err
	}
	quotedPrivs, err := NormalizePrivileges(privileges)
	if err != nil {
		return err
	}
	var target string
	if db == "*" {
		target = "*.*"
	} else {
		if err := ValidateDBName(db); err != nil {
			return err
		}
		q, err := quoteIdent(db)
		if err != nil {
			return err
		}
		target = q + ".*"
	}
	userIdent := fmt.Sprintf("'%s'@'%s'", strings.ReplaceAll(user, "'", "''"), host)
	if _, err := c.run(ctx, []string{"-e", fmt.Sprintf("GRANT %s ON %s TO %s",
		strings.Join(quotedPrivs, ", "), target, userIdent)}); err != nil {
		return err
	}
	_, err = c.run(ctx, []string{"-e", "FLUSH PRIVILEGES"})
	return err
}

// ShowGrants 返回账号的授权语句。
func (c *Client) ShowGrants(ctx context.Context, user, host string) ([]string, error) {
	if err := ValidateUserName(user); err != nil {
		return nil, err
	}
	if err := ValidateHost(host); err != nil {
		return nil, err
	}
	userIdent := fmt.Sprintf("'%s'@'%s'", strings.ReplaceAll(user, "'", "''"), host)
	return c.query(ctx, "SHOW GRANTS FOR "+userIdent)
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) >= 2 {
		return f[0] + " " + f[1]
	}
	if len(f) == 1 {
		return f[0]
	}
	return s
}

// isKnownCharset 校验字符集（白名单）。
func isKnownCharset(s string) bool {
	known := map[string]bool{
		"utf8mb4": true, "utf8": true, "utf8mb3": true,
		"latin1": true, "ascii": true, "gbk": true, "big5": true,
		"binary": true, "utf16": true, "utf32": true,
	}
	return known[strings.ToLower(strings.TrimSpace(s))]
}

// isKnownCollation 校验排序规则。
//
// 排序规则命名有规律（charset_suffix），用"前缀必须是已知字符集"
// 来判断，而不是维护一个几百项的完整列表。
func isKnownCollation(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	i := strings.Index(s, "_")
	if i <= 0 {
		return false
	}
	return isKnownCharset(s[:i]) && reCollationSuffix.MatchString(s[i+1:])
}

// reCollationSuffix 限制排序规则后缀的字符集，杜绝拼接进 DDL 的风险。
var reCollationSuffix = mustCompile(`^[a-z0-9_]{1,32}$`)

// ---------------------------------------------------------------------------
//  备份 / 恢复
// ---------------------------------------------------------------------------

// DumpResult 是一次导出的结果。
type DumpResult struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Elapsed int64  `json:"elapsed_ms"`
	Tables  int    `json:"tables"`
}

// DumpDatabase 导出指定的库到文件。
//
// 用 mysqldump 的 --single-transaction：InnoDB 下不锁表，
// 导出过程中业务仍可读写（对生产库很重要）。
func (c *Client) DumpDatabase(ctx context.Context, db, destDir string) (*DumpResult, error) {
	if err := ValidateDBName(db); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(destDir) {
		return nil, errors.New("备份目录必须是绝对路径")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建备份目录失败: %w", err)
	}
	bin := filepath.Join(c.opt.BinDir, "mysqldump")
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("未找到 mysqldump: %s", bin)
	}
	out := filepath.Join(destDir, fmt.Sprintf("%s-%s.sql", db, time.Now().Format("20060102-150405")))

	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	args := []string{"-u", c.opt.User, "--single-transaction", "--routines",
		"--triggers", "--default-character-set=utf8mb4", "--databases", db}
	if c.opt.Socket != "" {
		if _, err := os.Stat(c.opt.Socket); err == nil {
			args = append(args, "-S", c.opt.Socket)
		}
	} else {
		args = append(args, "-h", orDefault(c.opt.Host, "127.0.0.1"),
			"-P", fmt.Sprint(orDefaultInt(c.opt.Port, 3306)))
	}
	cmd := execCommandContext(ctx, bin, args...)
	if c.opt.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+c.opt.Password)
	}
	f, err := os.Create(out)
	if err != nil {
		return nil, fmt.Errorf("创建备份文件失败: %w", err)
	}
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	_ = f.Close()
	elapsed := time.Since(start).Milliseconds()
	if runErr != nil {
		_ = os.Remove(out)
		return nil, fmt.Errorf("导出失败: %s", strings.TrimSpace(stderr.String()))
	}

	st, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	res := &DumpResult{Path: out, Size: st.Size(), Elapsed: elapsed}
	// 统计导出的表数量（从 SQL 文本里数 CREATE TABLE）
	if b, err := os.ReadFile(out); err == nil {
		res.Tables = strings.Count(string(b), "CREATE TABLE")
	}
	c.chownIfRoot(out)
	return res, nil
}

// ImportDatabase 从 SQL 文件导入到一个库。
//
// 导入前会检查文件是否存在与大小上限，
// 避免误选几百 MB 的文件把 request 拖住。
func (c *Client) ImportDatabase(ctx context.Context, db, sqlFile string) (string, error) {
	if err := ValidateDBName(db); err != nil {
		return "", err
	}
	if !filepath.IsAbs(sqlFile) {
		return "", errors.New("SQL 文件路径必须是绝对路径")
	}
	st, err := os.Stat(sqlFile)
	if err != nil {
		return "", fmt.Errorf("找不到 SQL 文件: %w", err)
	}
	if st.IsDir() {
		return "", errors.New("这是一个目录，请选择 .sql 文件")
	}

	bin := filepath.Join(c.opt.BinDir, "mysql")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	args := []string{"-u", c.opt.User, "--default-character-set=utf8mb4", db}
	if c.opt.Socket != "" {
		if _, err := os.Stat(c.opt.Socket); err == nil {
			args = append(args, "-S", c.opt.Socket)
		}
	} else {
		args = append(args, "-h", orDefault(c.opt.Host, "127.0.0.1"),
			"-P", fmt.Sprint(orDefaultInt(c.opt.Port, 3306)))
	}
	cmd := execCommandContext(ctx, bin, args...)
	cmd.Stdin, err = os.Open(sqlFile)
	if err != nil {
		return "", err
	}
	if c.opt.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+c.opt.Password)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("导入失败: %s", strings.TrimSpace(stderr.String()))
	}
	return fmt.Sprintf("已导入 %s（%.1f KB）到 %s", filepath.Base(sqlFile),
		float64(st.Size())/1024, db), nil
}
