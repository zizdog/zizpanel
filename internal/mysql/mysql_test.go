package mysql

import (
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
//  标识符校验测试
//
//  这是本包最重要的测试。SQL 注入在数据库管理模块的后果是灾难性的
//  （DROP DATABASE、读取任意数据），而标识符无法用占位符参数化，
//  所以唯一可靠的做法就是字符白名单 —— 这里逐条锁死。
// ---------------------------------------------------------------------------

func TestValidateDBNameBlocksInjection(t *testing.T) {
	bad := []string{
		"",
		"my db",                   // 空格
		"db; DROP DATABASE mysql", // 多语句
		"db'--",                   // 注释
		"db`",                     // 反引号
		"db\nDROP",                // 换行
		"../../etc/passwd",        // 路径穿越（有些实现会拼路径）
		"db*",                     // 通配符
		"数据库",                     // 非 ASCII
		strings.Repeat("a", 65),   // 超长（MySQL 上限 64）
		"db\x00",                  // NUL
	}
	for _, s := range bad {
		if err := ValidateDBName(s); err == nil {
			t.Fatalf("非法库名未被拒绝: %q", s)
		}
	}
	good := []string{"mydb", "my_db", "db123", "my$db", "D1", strings.Repeat("a", 64)}
	for _, s := range good {
		if err := ValidateDBName(s); err != nil {
			t.Fatalf("合法库名被拒绝: %q (%v)", s, err)
		}
	}
}

func TestValidateUserNameBlocksInjection(t *testing.T) {
	bad := []string{
		"", "user name", "user'; DROP USER root; --", "user`x", "user\n",
		"user@host", // @ 由单独的 host 字段提供，不应出现在用户名里
		strings.Repeat("u", 33),
		"用户名",
	}
	for _, s := range bad {
		if err := ValidateUserName(s); err == nil {
			t.Fatalf("非法用户名未被拒绝: %q", s)
		}
	}
	good := []string{"appuser", "app_user", "app.user", "app-user", "u1"}
	for _, s := range good {
		if err := ValidateUserName(s); err != nil {
			t.Fatalf("合法用户名被拒绝: %q (%v)", s, err)
		}
	}
}

func TestValidateHostBlocksInjection(t *testing.T) {
	bad := []string{"", "local host", "localhost'; GRANT ALL; --", "host`", "host\n", strings.Repeat("h", 61)}
	for _, s := range bad {
		if err := ValidateHost(s); err == nil {
			t.Fatalf("非法主机名未被拒绝: %q", s)
		}
	}
	good := []string{"localhost", "127.0.0.1", "%", "192.168.1.%", "::1", "10.0.0.0/255.0.0.0"}
	for _, s := range good {
		if err := ValidateHost(s); err != nil {
			t.Fatalf("合法主机名被拒绝: %q (%v)", s, err)
		}
	}
}

func TestNormalizePrivileges(t *testing.T) {
	// 合法权限应被规范化（大写、去重、排序）
	got, err := NormalizePrivileges([]string{"select", "INSERT", "select"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "INSERT" || got[1] != "SELECT" {
		t.Fatalf("权限规范化结果不正确: %v", got)
	}
	// ALL 应短路为 ALL PRIVILEGES
	all, err := NormalizePrivileges([]string{"select", "ALL"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0] != "ALL PRIVILEGES" {
		t.Fatalf("ALL 应归一化: %v", all)
	}
	// 未知权限必须拒绝 —— 这里是最容易被注入的位置
	for _, bad := range []string{
		"SELECT; DROP DATABASE mysql",
		"SELECT, DELETE",           // 逗号应由调用方拆开，不应出现在单个权限里
		"ALL PRIVILEGES ON *.* --", // 试图附加子句
		"GRANT",
		"",
	} {
		if _, err := NormalizePrivileges([]string{bad}); err == nil {
			t.Fatalf("非法权限未被拒绝: %q", bad)
		}
	}
	// 空列表应拒绝
	if _, err := NormalizePrivileges(nil); err == nil {
		t.Fatal("空权限列表应被拒绝")
	}
}

func TestKnownPrivilegesRejectArbitraryStrings(t *testing.T) {
	// 任意字符串都不应被当作合法权限
	for _, s := range []string{"FOO", "SELECTX", "DROP TABLE user", "1=1", "*"} {
		if KnownPrivileges[strings.ToUpper(s)] {
			t.Fatalf("%q 不应在已知权限集合里", s)
		}
	}
}

// ---------------------------------------------------------------------------
//  SQL 语句结构检查
// ---------------------------------------------------------------------------

func TestCheckSingleStatement(t *testing.T) {
	// 单语句应通过
	good := []string{
		"SELECT 1",
		"SELECT * FROM users WHERE name = 'a;b'", // 字符串里的分号是合法的
		"SHOW TABLES",
		"SELECT \"x;y\" AS v",
	}
	for _, s := range good {
		if err := checkSingleStatement(s); err != nil {
			t.Fatalf("合法单语句被拒绝: %q (%v)", s, err)
		}
	}
	// 多语句应拒绝
	bad := []string{
		"SELECT 1; DROP DATABASE mysql",
		"SELECT 1; SELECT 2",
		"DELETE FROM t; DELETE FROM t",
	}
	for _, s := range bad {
		if err := checkSingleStatement(s); err == nil {
			t.Fatalf("多语句未被拒绝: %q", s)
		}
	}
	// 注释应拒绝（常被用来绕过单语句检查）
	for _, s := range []string{
		"SELECT 1 -- comment",
		"SELECT /* comment */ 1",
		"SELECT 1 # comment", // # 不是我们的检查目标，但应保持可通过
	} {
		err := checkSingleStatement(s)
		if strings.Contains(s, "--") || strings.Contains(s, "/*") {
			if err == nil {
				t.Fatalf("含注释的语句未被拒绝: %q", s)
			}
		}
	}
	// 结尾的单个分号是允许的（用户习惯写法）
	if err := checkSingleStatement("SELECT 1;"); err != nil {
		t.Fatalf("结尾单个分号应被允许: %v", err)
	}
}

func TestStripStringLiterals(t *testing.T) {
	cases := map[string]string{
		"SELECT 'a;b'":           "SELECT ",
		`SELECT "a--b"`:          "SELECT ",
		"SELECT `x;y`":           "SELECT ",
		"SELECT 'a', 'b' FROM t": "SELECT ,  FROM t",
	}
	for in, want := range cases {
		if got := stripStringLiterals(in); got != want {
			t.Fatalf("stripStringLiterals(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
//  转义
// ---------------------------------------------------------------------------

func TestEscapeSQLString(t *testing.T) {
	got, err := escapeSQLString(`pa'ss\word`)
	if err != nil {
		t.Fatal(err)
	}
	// 单引号与反斜杠都必须被转义
	if !strings.Contains(got, `\'`) {
		t.Fatalf("单引号未转义: %q", got)
	}
	if !strings.Contains(got, `\\`) {
		t.Fatalf("反斜杠未转义: %q", got)
	}
	// NUL 必须拒绝（会让 C 客户端截断语句）
	if _, err := escapeSQLString("a\x00b"); err == nil {
		t.Fatal("含 NUL 的字符串应被拒绝")
	}
}

func TestQuoteIdent(t *testing.T) {
	got, err := quoteIdent("mydb")
	if err != nil || got != "`mydb`" {
		t.Fatalf("quoteIdent 结果不正确: %q (%v)", got, err)
	}
	// 反引号必须拒绝（纵深防御）
	if _, err := quoteIdent("my`db"); err == nil {
		t.Fatal("含反引号的标识符应被拒绝")
	}
	if _, err := quoteIdent("my\x00db"); err == nil {
		t.Fatal("含 NUL 的标识符应被拒绝")
	}
}

// ---------------------------------------------------------------------------
//  字符集 / 排序规则
// ---------------------------------------------------------------------------

func TestCharsetWhitelist(t *testing.T) {
	for _, s := range []string{"utf8mb4", "UTF8MB4", "utf8", "latin1", "gbk"} {
		if !isKnownCharset(s) {
			t.Fatalf("合法字符集被拒绝: %q", s)
		}
	}
	for _, s := range []string{
		"", "utf8mb4; DROP DATABASE mysql", "unknown", "utf8mb4 COLLATE x",
	} {
		if isKnownCharset(s) {
			t.Fatalf("非法字符集未被拒绝: %q", s)
		}
	}
}

func TestCollationValidation(t *testing.T) {
	good := []string{"utf8mb4_general_ci", "utf8mb4_unicode_520_ci", "latin1_swedish_ci", "utf8mb4_bin"}
	for _, s := range good {
		if !isKnownCollation(s) {
			t.Fatalf("合法排序规则被拒绝: %q", s)
		}
	}
	bad := []string{
		"", "general_ci", // 缺少字符集前缀
		"utf8mb4_general_ci; DROP DATABASE", // 注入
		"nope_general_ci",                   // 未知字符集
		"utf8mb4_",                          // 空后缀
	}
	for _, s := range bad {
		if isKnownCollation(s) {
			t.Fatalf("非法排序规则未被拒绝: %q", s)
		}
	}
}

// ---------------------------------------------------------------------------
//  客户端构造
// ---------------------------------------------------------------------------

func TestNewClientDefaults(t *testing.T) {
	c := NewClient(Options{})
	if c.opt.User != "root" {
		t.Fatalf("默认账号应为 root，实际 %s", c.opt.User)
	}
	if c.opt.Timeout <= 0 {
		t.Fatal("应设置默认超时")
	}
}

func TestBaseArgsPrefersSocket(t *testing.T) {
	// socket 不存在时应回退到 TCP
	c := NewClient(Options{Host: "127.0.0.1", Port: 3306, Socket: "/nonexistent/mysql.sock"})
	args := strings.Join(c.baseArgs(), " ")
	if !strings.Contains(args, "-h") || !strings.Contains(args, "3306") {
		t.Fatalf("socket 不存在时应使用 TCP: %s", args)
	}
	// 端口为空时应回退到 3306
	c2 := NewClient(Options{Host: "", Port: 0, Socket: "/nonexistent"})
	if !strings.Contains(strings.Join(c2.baseArgs(), " "), "3306") {
		t.Fatal("端口缺省时应回退到 3306")
	}
}

func TestFormatMySQLError(t *testing.T) {
	// 带了口令仍被拒：说明口令不对，应引导去改配置
	err := formatMySQLError("ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)", true, "root")
	if !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("认证错误提示不友好: %v", err)
	}
	if !strings.Contains(err.Error(), "连接设置") {
		t.Errorf("应指出去哪里改口令: %v", err)
	}
	// 连接失败同理
	err2 := formatMySQLError("ERROR 2002 (HY000): Can't connect to local MySQL server", false, "root")
	if !strings.Contains(err2.Error(), "无法连接") {
		t.Fatalf("连接错误提示不友好: %v", err2)
	}
	// 其它错误原样返回，不掩盖信息
	err3 := formatMySQLError("ERROR 1064 (42000): You have an error in your SQL syntax", false, "root")
	if !strings.Contains(err3.Error(), "1064") {
		t.Fatalf("其它错误应保留原始信息: %v", err3)
	}
}

// TestFormatMySQLErrorDistinguishesNoPassword 是本轮故障的核心断言：
// "面板没带口令"与"面板带错口令"必须给出**不同**的解释，
// 而且都要明确说"不能据此认为 root 没有密码"。
//
// 2026-09-16 真机原话就是被一句泛泛的"请检查密码"误导的。
func TestFormatMySQLErrorDistinguishesNoPassword(t *testing.T) {
	raw := "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: NO)"
	err := formatMySQLError(raw, false, "root")
	msg := err.Error()
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("1045 必须能用 errors.Is 判成 ErrAuth（上层据此区分服务没起来）: %v", err)
	}
	for _, want := range []string{"没有带口令", "两者不一致", "不能", "连接设置", raw} {
		if !strings.Contains(msg, want) {
			t.Errorf("提示里缺少 %q：\n%s", want, msg)
		}
	}
	// 恢复向导必须给出可操作命令（用户可能真的想不起口令）
	if !strings.Contains(msg, "skip-grant-tables") {
		t.Errorf("应给出忘记口令时的恢复向导：\n%s", msg)
	}
	// 带了口令的那种不许说"面板没有口令"
	withPw := formatMySQLError(
		"ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)", true, "root")
	if strings.Contains(withPw.Error(), "没有带口令") {
		t.Errorf("带了口令的错误不该说成'没有带口令'：%v", withPw)
	}
}

func TestRecoveryGuideIsActionable(t *testing.T) {
	g := RecoveryGuide("root")
	for _, want := range []string{"skip-grant-tables", "FLUSH PRIVILEGES", "ALTER USER", "连接设置"} {
		if !strings.Contains(g, want) {
			t.Errorf("恢复向导缺少 %q：%s", want, g)
		}
	}
	// 缺省账号也要能用（调用方可能传空串）
	if !strings.Contains(RecoveryGuide(""), "root") {
		t.Error("账号为空时应回退到 root")
	}
}

func TestFirstWord(t *testing.T) {
	cases := map[string]string{
		"CREATE DATABASE `x`": "CREATE DATABASE",
		"GRANT SELECT ON *.*": "GRANT SELECT",
		"FLUSH":               "FLUSH",
		"":                    "",
	}
	for in, want := range cases {
		if got := firstWord(in); got != want {
			t.Fatalf("firstWord(%q) = %q，期望 %q", in, got, want)
		}
	}
}
