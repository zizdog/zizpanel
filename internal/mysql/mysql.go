// Package mysql 实现数据库管理。
//
// 安全模型（这是本包的核心）：
//
//	SQL 里最危险的不是"值"，而是"标识符"（库名、表名、用户名、主机名）。
//	值可以用占位符参数化，但标识符不行 —— 它直接进入 SQL 语法结构。
//	所以本包对每一个标识符都做**字符白名单校验**，而不是转义：
//
//	  库名/表名  ^[A-Za-z0-9_$]{1,64}$   （允许 $ 是因为 MySQL 允许）
//	  用户名     ^[A-Za-z0-9_.-]{1,32}$
//	  主机名     ^[A-Za-z0-9_.%:-]{1,60}$
//	  权限名     必须落在已知权限集合内
//
// 白名单校验比"加反引号转义"更可靠：反引号转义要考虑内部反引号、
// 字符集、NO_BACKSLASH_ESCAPES 模式等边界，而白名单只有"允许/拒绝"两种结果。
//
// 实现方式：调用 mysql CLI 并传递结构化参数（不经 shell）。
// 这样做的取舍：
//   - 不引入 MySQL 驱动依赖（面板坚持极简依赖）
//   - 与运维人员手工执行完全一致的行为，出错信息也一致
//   - 代价是每次查询多一次进程创建，对管理面板的频次完全可接受
package mysql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options 是连接参数。
type Options struct {
	// BinDir 是 mysql/mysqldump 所在目录
	BinDir string
	// Host / Port / Socket
	Host   string
	Port   int
	Socket string
	// User / Password 是管理账号（默认 root）
	User     string
	Password string
	// Timeout 是单条查询超时
	Timeout time.Duration
	// UserName / UserHome 用于把导出的备份文件归属真实用户
	UserName string
	UserHome string
}

// lookupUserIDs 把用户名解析为 uid/gid。
func lookupUserIDs(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// Client 是 MySQL 管理客户端。
type Client struct {
	opt Options
}

// NewClient 创建客户端。
func NewClient(opt Options) *Client {
	if opt.User == "" {
		opt.User = "root"
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}
	return &Client{opt: opt}
}

// ---------- 标识符校验 ----------

var (
	reDBName   = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)
	reUserName = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}$`)
	// 主机名允许：字母数字、点、下划线、%、冒号（IPv6）、连字符，
	// 以及 /（MySQL 支持网段形式的 host，如 192.0.2.0/255.255.255.0）
	reHost  = regexp.MustCompile(`^[A-Za-z0-9_.%:/\-]{1,60}$`)
	reTable = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)
)

// ValidateDBName 校验数据库名。
func ValidateDBName(s string) error {
	if !reDBName.MatchString(s) {
		return fmt.Errorf("数据库名只允许字母、数字、下划线与 $（最多 64 字符）: %q", s)
	}
	return nil
}

// ValidateUserName 校验用户名。
func ValidateUserName(s string) error {
	if !reUserName.MatchString(s) {
		return fmt.Errorf("用户名只允许字母、数字、下划线、点与连字符（最多 32 字符）: %q", s)
	}
	return nil
}

// ValidateHost 校验主机名（用于 user@host）。
func ValidateHost(s string) error {
	if !reHost.MatchString(s) {
		return fmt.Errorf("主机名格式不合法: %q", s)
	}
	return nil
}

// ValidateTableName 校验表名。
func ValidateTableName(s string) error {
	if !reTable.MatchString(s) {
		return fmt.Errorf("表名只允许字母、数字、下划线与 $（最多 64 字符）: %q", s)
	}
	return nil
}

// KnownPrivileges 是允许授予的权限集合。
//
// 用白名单而不是透传：GRANT 后面接的权限名是语法关键字位置，
// 透传任意字符串等于把 SQL 片段交给用户拼接。
var KnownPrivileges = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "INDEX": true,
	"CREATE TEMPORARY TABLES": true, "LOCK TABLES": true,
	"EXECUTE": true, "CREATE VIEW": true, "SHOW VIEW": true,
	"CREATE ROUTINE": true, "ALTER ROUTINE": true, "EVENT": true,
	"TRIGGER": true, "REFERENCES": true, "CREATE USER": true,
	"PROCESS": true, "RELOAD": true, "SHUTDOWN": true, "FILE": true,
	"SUPER": true, "REPLICATION CLIENT": true, "REPLICATION SLAVE": true,
}

// NormalizePrivileges 校验并规范化权限列表。
func NormalizePrivileges(list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, errors.New("请至少选择一个权限")
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range list {
		p = strings.ToUpper(strings.TrimSpace(p))
		// 允许 ALL（等价于全部权限）
		if p == "ALL" || p == "ALL PRIVILEGES" {
			return []string{"ALL PRIVILEGES"}, nil
		}
		if !KnownPrivileges[p] {
			return nil, fmt.Errorf("不支持的权限: %q", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---------- 执行 ----------

// query 执行一条 SQL 并返回制表符分隔的结果行。
func (c *Client) query(ctx context.Context, sqlText string) ([]string, error) {
	return c.run(ctx, []string{"-N", "-B", "-e", sqlText})
}

// run 调用 mysql CLI。
//
// 密码通过 MYSQL_PWD 环境变量传递而不是 -p 参数：
// 命令行参数在同机的 `ps` 输出里可见，环境变量只有同用户能读。
func (c *Client) run(ctx context.Context, extra []string) ([]string, error) {
	return c.exec(ctx, extra, "")
}

// runSQL 把一条 SQL 从 **stdin** 送进 mysql CLI（argv 里只有连接参数）。
//
// 为什么需要它：CREATE USER / ALTER USER 的口令必须作为字符串字面量出现在
// SQL 里（MySQL 不支持占位符），而 `-e` 会把整条语句放进 argv ——
// 同机任何用户 `ps` 一下就能看到明文口令。面板本身就是本机常驻进程，
// 这个窗口是真实存在的。改走 stdin 之后 argv 里只剩连接参数。
func (c *Client) runSQL(ctx context.Context, sqlText string) ([]string, error) {
	return c.exec(ctx, nil, sqlText)
}

func (c *Client) exec(ctx context.Context, extra []string, stdin string) ([]string, error) {
	bin := filepath.Join(c.opt.BinDir, "mysql")
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("未找到 mysql 客户端: %s", bin)
	}
	ctx, cancel := context.WithTimeout(ctx, c.opt.Timeout)
	defer cancel()

	args := c.baseArgs()
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if c.opt.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+c.opt.Password)
	}
	out, err := cmd.Output()
	text := string(out)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, formatMySQLError(string(ee.Stderr), c.opt.Password != "", c.opt.User)
		}
		return nil, fmt.Errorf("执行 mysql 命令失败: %w", err)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	return lines, nil
}

// baseArgs 构造连接参数。
//
// 优先用 socket：本机管理场景下 socket 不受"只监听 127.0.0.1"的端口限制，
// 而且绕开了 TCP 层的认证差异。
func (c *Client) baseArgs() []string {
	args := []string{"-u", c.opt.User, "--default-character-set=utf8mb4"}
	if c.opt.Socket != "" {
		if _, err := os.Stat(c.opt.Socket); err == nil {
			return append(args, "-S", c.opt.Socket)
		}
	}
	return append(args, "-h", orDefault(c.opt.Host, "127.0.0.1"),
		"-P", strconv.Itoa(orDefaultInt(c.opt.Port, 3306)))
}

// ErrAuth 表示"认证失败（1045）"这一类错误。
//
// 为什么要一个哨兵错误而不是让上层匹配文案：上层要据此区分
//   - 口令不对/为空（面板配置与服务器不一致 → 要引导用户去改配置）
//   - 服务没起来（那是另一件事）
//
// 用 strings.Contains 匹配自己写的中文文案，改一次文案就会静默失效。
var ErrAuth = errors.New("认证失败")

// formatMySQLError 把 mysql CLI 的报错转成可读信息。
//
// passwordSent 是"这次连接有没有带口令"。两种 1045 的补救动作完全不同，
// 所以必须区分开：
//   - using password: NO  → 面板持有的口令是**空的**，而服务器要口令。
//     也就是"面板以为无密码、MySQL 其实要密码"的不一致。
//     2026-09-16 真机（mini）就是这样：面板把 root 改成了随机口令却没记住，
//     之后所有库操作都是这句话，用户完全不知道发生了什么。
//   - using password: YES → 带了口令但服务器不认（口令不对）。
func formatMySQLError(s string, passwordSent bool, userName string) error {
	s = strings.TrimSpace(s)
	who := orDefault(userName, "root")
	switch {
	case strings.Contains(s, "Access denied"):
		if !passwordSent {
			return fmt.Errorf("%w：面板连接 MySQL 时没有带口令（面板持有的口令为空），"+
				"但服务器要求口令 —— 两者不一致，所以**不能**据此认为 %s 没有密码。"+
				"请到「数据库 → 连接设置」填入正确的 %s 口令（填完保存即可）。%s原始信息：%s",
				ErrAuth, who, who, RecoveryGuide(who), s)
		}
		return fmt.Errorf("%w：面板持有的口令被服务器拒绝（口令不正确，或与这个账号不匹配）。"+
			"请在「数据库 → 连接设置」更新 %s 的口令。%s原始信息：%s",
			ErrAuth, who, RecoveryGuide(who), s)
	case strings.Contains(s, "Can't connect"), strings.Contains(s, "Connection refused"):
		return errors.New("无法连接 MySQL：请确认数据库服务正在运行")
	}
	return errors.New(s)
}

// RecoveryGuide 是"确实想不起 MySQL 口令"时的恢复向导（纯文本）。
//
// 为什么放在这里：页面提示、接口报错、安装自检三处都要给出同一份指引，
// 各写一份迟早走样（本项目最典型的教训是"日志说一套、实现做一套"）。
//
// **面板绝不自动执行这段动作**：它要停掉 mysqld 并跳过权限表，属于破坏性操作，
// 必须由用户明确决定。这里只提供可复制、可回滚的命令。
func RecoveryGuide(user string) string {
	if user == "" {
		user = "root"
	}
	return fmt.Sprintf("若确实想不起口令，可在终端恢复（会短暂停掉 MySQL，请先确认没有正在写的业务）："+
		"① 停掉 mysqld；② 用 `mysqld_safe --skip-grant-tables --skip-networking` 启动；"+
		"③ 连上后执行 `FLUSH PRIVILEGES; ALTER USER '%s'@'localhost' IDENTIFIED BY '新口令';`；"+
		"④ 恢复成正常启动；⑤ 把新口令填回「数据库 → 连接设置」（面板不会替你执行这组命令）。", user)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// Ping 检查连接是否正常。
func (c *Client) Ping(ctx context.Context) error {
	lines, err := c.query(ctx, "SELECT VERSION();")
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return errors.New("MySQL 未返回版本信息")
	}
	return nil
}

// Version 返回服务端版本。
func (c *Client) Version(ctx context.Context) (string, error) {
	lines, err := c.query(ctx, "SELECT VERSION();")
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", errors.New("无法读取版本")
	}
	return lines[0], nil
}

// ---------- 数据库 ----------

// Database 是一个数据库及其概况。
type Database struct {
	Name      string `json:"name"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
	// Size 字节数（information_schema 估算值）
	Size int64 `json:"size"`
	// Tables 表数量
	Tables int `json:"tables"`
	// System 为 true 表示是 MySQL 自带的系统库
	System bool `json:"system"`
}

// systemDBs 是 MySQL 自带的库，界面上标注但不阻止操作（用户可能有特殊需求）。
var systemDBs = map[string]bool{
	"information_schema": true, "mysql": true,
	"performance_schema": true, "sys": true,
}

// ListDatabases 列出所有数据库。
func (c *Client) ListDatabases(ctx context.Context) ([]Database, error) {
	sqlText := `SELECT s.SCHEMA_NAME,
       IFNULL(s.DEFAULT_CHARACTER_SET_NAME,''),
       IFNULL(s.DEFAULT_COLLATION_NAME,''),
       IFNULL(SUM(t.DATA_LENGTH + t.INDEX_LENGTH),0),
       COUNT(t.TABLE_NAME)
FROM information_schema.SCHEMATA s
LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME
GROUP BY s.SCHEMA_NAME, s.DEFAULT_CHARACTER_SET_NAME, s.DEFAULT_COLLATION_NAME
ORDER BY s.SCHEMA_NAME;`
	lines, err := c.query(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	var out []Database
	for _, ln := range lines {
		f := strings.Split(ln, "\t")
		if len(f) < 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		tables, _ := strconv.Atoi(f[4])
		out = append(out, Database{
			Name: f[0], Charset: f[1], Collation: f[2],
			Size: size, Tables: tables,
			System: systemDBs[f[0]],
		})
	}
	if out == nil {
		out = []Database{}
	}
	return out, nil
}

// TableInfo 是一张表的概况。
type TableInfo struct {
	Name      string `json:"name"`
	Engine    string `json:"engine"`
	Rows      int64  `json:"rows"`
	Size      int64  `json:"size"`
	Collation string `json:"collation"`
	Comment   string `json:"comment"`
	UpdatedAt string `json:"updated_at"`
}

// ListTables 列出指定库的表。
func (c *Client) ListTables(ctx context.Context, db string) ([]TableInfo, error) {
	if err := ValidateDBName(db); err != nil {
		return nil, err
	}
	// 库名已通过白名单校验，可以安全地内联到 SQL 里
	sqlText := fmt.Sprintf(`SELECT TABLE_NAME, IFNULL(ENGINE,''), IFNULL(TABLE_ROWS,0),
       IFNULL(DATA_LENGTH + INDEX_LENGTH,0), IFNULL(TABLE_COLLATION,''),
       IFNULL(TABLE_COMMENT,''), IFNULL(DATE_FORMAT(UPDATE_TIME,'%%Y-%%m-%%d %%H:%%i:%%s'),'')
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = '%s'
ORDER BY TABLE_NAME;`, db)
	lines, err := c.query(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	var out []TableInfo
	for _, ln := range lines {
		f := strings.Split(ln, "\t")
		if len(f) < 7 {
			continue
		}
		rows, _ := strconv.ParseInt(f[2], 10, 64)
		size, _ := strconv.ParseInt(f[3], 10, 64)
		out = append(out, TableInfo{
			Name: f[0], Engine: f[1], Rows: rows, Size: size,
			Collation: f[4], Comment: f[5], UpdatedAt: f[6],
		})
	}
	if out == nil {
		out = []TableInfo{}
	}
	return out, nil
}

// ---------- 用户与权限 ----------

// DBUser 是一个 MySQL 账号。
type DBUser struct {
	User        string   `json:"user"`
	Host        string   `json:"host"`
	Privileges  []string `json:"privileges"`
	IsLocked    bool     `json:"is_locked"`
	HasPassword bool     `json:"has_password"`
	// AuthPlugin 是认证插件（mysql.user.plugin）。
	//
	// 为什么要回传它：HasPassword 只反映 authentication_string 是否为空，
	// 而 auth_socket 这类插件**本来就不需要口令** —— 只看 "无密码" 会把
	// "这个账号用 socket 认证"误读成"这个账号谁都能连"。界面据此区分文案。
	AuthPlugin string `json:"auth_plugin"`
	// Databases 该账号有权限的库（汇总）
	Databases []string `json:"databases"`
	// System 为 true 表示是 MySQL 自带账号
	System bool `json:"system"`
}

// systemUsers 是 MySQL 内置账号。
var systemUsers = map[string]bool{
	"mysql.sys": true, "mysql.session": true,
	"mysql.infoschema": true, "root": true, "mysql": true,
}

// ListUsers 列出所有账号及其全局权限。
func (c *Client) ListUsers(ctx context.Context) ([]DBUser, error) {
	// 先从 mysql.user 拿账号基础信息（MySQL 8 用 authentication_string 判断是否有密码）
	sqlText := `SELECT User, Host,
       IF(account_locked='Y','1','0'),
       IF(LENGTH(IFNULL(authentication_string,''))>0,'1','0'),
       IFNULL(plugin,'')
FROM mysql.user ORDER BY User, Host;`
	lines, err := c.query(ctx, sqlText)
	if err != nil {
		return nil, err
	}

	// 再拿每个账号的库级授权（比逐个 SHOW GRANTS 快得多）
	privSQL := `SELECT GRANTEE, TABLE_SCHEMA, PRIVILEGE_TYPE
FROM information_schema.SCHEMA_PRIVILEGES ORDER BY GRANTEE;`
	privLines, _ := c.query(ctx, privSQL)

	// GRANTEE 形如 'user'@'host'
	dbPrivs := map[string][]string{}
	globalPrivs := map[string]map[string]bool{}
	for _, ln := range privLines {
		f := strings.Split(ln, "\t")
		if len(f) < 3 {
			continue
		}
		key := f[0]
		if !containsStr(dbPrivs[key], f[1]) {
			dbPrivs[key] = append(dbPrivs[key], f[1])
		}
		if globalPrivs[key] == nil {
			globalPrivs[key] = map[string]bool{}
		}
		globalPrivs[key][f[2]] = true
	}

	var out []DBUser
	for _, ln := range lines {
		f := strings.Split(ln, "\t")
		if len(f) < 5 {
			continue
		}
		u := DBUser{
			User: f[0], Host: f[1],
			IsLocked:    f[2] == "1",
			HasPassword: f[3] == "1",
			AuthPlugin:  f[4],
			System:      systemUsers[f[0]],
		}
		key := "'" + u.User + "'@'" + u.Host + "'"
		u.Databases = dbPrivs[key]
		if u.Databases == nil {
			u.Databases = []string{}
		}
		for p := range globalPrivs[key] {
			u.Privileges = append(u.Privileges, p)
		}
		sort.Strings(u.Privileges)
		out = append(out, u)
	}
	if out == nil {
		out = []DBUser{}
	}
	return out, nil
}

// ---------- SQL 执行 ----------

// QueryResult 是一次查询的结果。
type QueryResult struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	// Affected 受影响行数（写语句）
	Affected string `json:"affected"`
	// Elapsed 耗时（毫秒）
	Elapsed int64 `json:"elapsed_ms"`
	// IsSelect 是否为查询语句
	IsSelect bool `json:"is_select"`
}

// Execute 执行一段 SQL（供高级用户使用）。
//
// 限制：
//   - 只允许单条语句（避免一次性执行脚本带来的不可控影响）
//   - 语句里的注释会被拒绝（注释常被用来绕过"看起来只有一条语句"的检查）
func (c *Client) Execute(ctx context.Context, db, sqlText string) (*QueryResult, error) {
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return nil, errors.New("SQL 不能为空")
	}
	if err := checkSingleStatement(sqlText); err != nil {
		return nil, err
	}
	// 注意：这里**不能**加 -N。
	// -B（batch）会输出制表符分隔的结果，且第一行是列名；
	// -N 会抑制列名，导致代码把第一行数据误当成列名
	// （表现为"列名是数据、数据少一行"，实测踩到过）。
	args := []string{"-B", "-e", sqlText}
	if db != "" {
		if err := ValidateDBName(db); err != nil {
			return nil, err
		}
		args = []string{"-B", "-D", db, "-e", sqlText}
	}

	start := time.Now()
	lines, err := c.run(ctx, args)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}
	res := &QueryResult{Elapsed: elapsed, Columns: []string{}, Rows: [][]string{}}
	upper := strings.ToUpper(strings.TrimLeft(sqlText, " \t\n("))
	res.IsSelect = strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "SHOW") ||
		strings.HasPrefix(upper, "DESC") ||
		strings.HasPrefix(upper, "EXPLAIN") ||
		strings.HasPrefix(upper, "WITH")

	if res.IsSelect && len(lines) > 0 {
		// -B 模式下第一行就是列名
		res.Columns = strings.Split(lines[0], "\t")
		for _, ln := range lines[1:] {
			res.Rows = append(res.Rows, strings.Split(ln, "\t"))
		}
	} else {
		res.Affected = "执行成功"
	}
	// 结果行数上限，避免把巨表拉进内存
	const maxRows = 2000
	if len(res.Rows) > maxRows {
		res.Rows = res.Rows[:maxRows]
		res.Affected = fmt.Sprintf("仅显示前 %d 行", maxRows)
	}
	return res, nil
}

// checkSingleStatement 拒绝多语句与注释。
//
// 为什么拒绝注释：`SELECT 1; -- x` 这类输入在人工审查时容易被忽略，
// 而 mysql CLI 的 -e 会把分号后的内容也执行掉。
// 面板的 SQL 执行是给"查一下数据"用的，不需要注释。
func checkSingleStatement(s string) error {
	// 去掉字符串字面量内容后再检查分号，避免误判 'a;b' 这种合法值
	stripped := stripStringLiterals(s)
	if strings.Contains(stripped, ";") {
		trimmed := strings.TrimRight(stripped, " \t\n")
		if strings.Contains(strings.TrimSuffix(trimmed, ";"), ";") {
			return errors.New("只允许执行单条 SQL 语句")
		}
	}
	if strings.Contains(stripped, "--") || strings.Contains(stripped, "/*") {
		return errors.New("SQL 里不允许包含注释（-- 或 /*），以免绕过单语句检查")
	}
	return nil
}

// stripStringLiterals 把字符串字面量与反引号标识符替换掉，便于做结构检查。
func stripStringLiterals(s string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++ // 跳过被转义的下一个字符
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// mustCompile 是 regexp.MustCompile 的别名（供 admin.go 使用）。
func mustCompile(expr string) *regexp.Regexp { return regexp.MustCompile(expr) }

// execCommandContext 是 exec.CommandContext 的薄封装。
func execCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// chownIfRoot 把面板创建的文件归属真实用户（面板以 root 运行）。
func (m *Client) chownIfRoot(p string) {
	if os.Geteuid() != 0 || m.opt.UserHome == "" {
		return
	}
	// 仅当目标在用户家目录下时才需要改归属
	if strings.HasPrefix(p, m.opt.UserHome) {
		if uid, gid, err := lookupUserIDs(m.opt.UserName); err == nil {
			_ = os.Chown(p, uid, gid)
		}
	}
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
