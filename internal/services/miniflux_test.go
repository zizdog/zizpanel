package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  Miniflux 安装器的回归测试
//
//  三条不能动的约定（都在这里钉死）：
//    1. 生成的配置形状（8 个键、单行 KEY=VALUE、端口 8087、sslmode=disable）；
//    2. 幂等的建角色 / 建库决策（在就 ALTER、不在就 CREATE；库在就跳过）；
//    3. **口令绝不出现在任务步骤 / 错误信息里**，只能进 0600 配置文件与
//       安装结果的凭据区块（InstallResult.Credentials）。
//
//  单测不许碰真实服务：brex 用假脚本、pg_isready/psql/miniflux 与 HTTP 探测
//  都用 Manager 上的 override 替换，配置写在临时 brew 前缀下。
// ============================================================================

func alnumOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ---------- 纯函数 ----------

// TestMinifluxConfigContentShape 把配置文件的形状逐字节钉死。
func TestMinifluxConfigContentShape(t *testing.T) {
	got := minifluxConfigContent("192.168.1.4", "DbPass0123456789abcdef", "AdminPass0123456789a")
	want := "DATABASE_URL=postgres://miniflux:DbPass0123456789abcdef@127.0.0.1:5432/miniflux?sslmode=disable\n" +
		"LISTEN_ADDR=:8087\n" +
		"BASE_URL=http://192.168.1.4:8087\n" +
		"RUN_MIGRATIONS=1\n" +
		"CREATE_ADMIN=1\n" +
		"ADMIN_USERNAME=admin\n" +
		"ADMIN_PASSWORD=AdminPass0123456789a\n" +
		"LOG_LEVEL=info\n"
	if got != want {
		t.Errorf("配置内容与约定不一致\n实际：\n%s\n期望：\n%s", got, want)
	}
	// 口令是按 KEY=VALUE 裸写的，绝不能带引号（带了就会被当成口令的一部分）。
	if strings.Contains(got, `"`) || strings.Contains(got, "'") {
		t.Errorf("配置里不该出现引号：\n%s", got)
	}
}

// TestGenerateMinifluxSecret 锁住"只出 [A-Za-z0-9] 且长度正确"。
func TestGenerateMinifluxSecret(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		s, err := generateMinifluxSecret(minifluxDBPasswordLen)
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if len(s) != minifluxDBPasswordLen {
			t.Fatalf("长度应为 %d，实际 %d（%q）", minifluxDBPasswordLen, len(s), s)
		}
		if !alnumOnly(s) {
			t.Fatalf("只允许 [A-Za-z0-9]，实际 %q", s)
		}
		if seen[s] {
			t.Fatalf("两次生成出现了相同的随机串：%q", s)
		}
		seen[s] = true
	}
	if _, err := generateMinifluxSecret(0); err == nil {
		t.Error("长度 0 应报错")
	}
}

// TestReuseMinifluxDBPassword 锁住"重跑时复用已有口令"的解析。
func TestReuseMinifluxDBPassword(t *testing.T) {
	conf := minifluxConfigContent("10.0.0.1", "OldPass0123456789abcdef", "Admin0123456789abcd")
	if got := reuseMinifluxDBPassword(conf); got != "OldPass0123456789abcdef" {
		t.Errorf("应从 DATABASE_URL 里复用口令，实际 %q", got)
	}
	cases := map[string]string{
		"没有 DATABASE_URL":  "LISTEN_ADDR=:8087\nADMIN_PASSWORD=x\n",
		"DATABASE_URL 无口令": "DATABASE_URL=postgres://miniflux@127.0.0.1:5432/miniflux\n",
		"注释与空行":            "# DATABASE_URL=postgres://miniflux:comment@x/y\n\n",
	}
	for name, in := range cases {
		if got := reuseMinifluxDBPassword(in); got != "" {
			t.Errorf("%s：应返回空串，实际 %q", name, got)
		}
	}
	// 键名必须精确匹配（miniflux 读环境变量名，大小写不匹配就是没读到）。
	if got := reuseMinifluxDBPassword("database_url=postgres://miniflux:pw@x/y\n"); got != "" {
		t.Errorf("小写键名不该被当成 DATABASE_URL，实际 %q", got)
	}
}

// TestMinifluxProvisionStatements 锁住建角色 / 建库的幂等决策。
func TestMinifluxProvisionStatements(t *testing.T) {
	const pw = "DbPass0123456789abcdef"

	// 都不存在 → CREATE ROLE + CREATE DATABASE
	fresh := minifluxProvisionStatements(false, false, pw)
	if len(fresh) != 2 || !fresh[0].Run || !fresh[1].Run {
		t.Fatalf("全新环境应执行两条语句，实际 %+v", fresh)
	}
	if !strings.HasPrefix(fresh[0].SQL, "CREATE ROLE miniflux WITH LOGIN PASSWORD '") {
		t.Errorf("角色不存在时应 CREATE，实际 %q", fresh[0].SQL)
	}
	if fresh[1].SQL != "CREATE DATABASE miniflux OWNER miniflux" {
		t.Errorf("库不存在时应建库并给属主，实际 %q", fresh[1].SQL)
	}

	// 都存在 → ALTER ROLE（更新口令）+ 跳过建库
	existing := minifluxProvisionStatements(true, true, pw)
	if len(existing) != 2 {
		t.Fatalf("应给出两条决策，实际 %+v", existing)
	}
	if !strings.HasPrefix(existing[0].SQL, "ALTER ROLE miniflux WITH LOGIN PASSWORD '") {
		t.Errorf("角色已存在时应 ALTER，实际 %q", existing[0].SQL)
	}
	if existing[1].Run || existing[1].SQL != "" {
		t.Errorf("库已存在时应跳过且不产生 SQL，实际 %+v", existing[1])
	}

	// 口令只允许出现在 SQL 里；给用户看的 Desc 绝不能带口令。
	for _, st := range append(append([]minifluxSQLStatement{}, fresh...), existing...) {
		if strings.Contains(st.Desc, pw) {
			t.Errorf("步骤文案里出现了口令：%q", st.Desc)
		}
	}
	if !strings.Contains(fresh[0].SQL, pw) {
		t.Error("角色语句里应当带上新口令")
	}
	// 生成的口令只有字母数字 → 单引号包裹是安全的（没有转义/注入风险）。
	if strings.Contains(pw, "'") {
		t.Fatal("测试口令本身就不该含单引号")
	}
}

// TestPgIsReadyOutput 只看"是否接受连接"，不把"启动了但没就绪"当成就绪。
func TestPgIsReadyOutput(t *testing.T) {
	if !pgIsReadyOutput("127.0.0.1:5432 - accepting connections\n") {
		t.Error("accepting connections 应判为就绪")
	}
	for _, out := range []string{
		"127.0.0.1:5432 - no response",
		"127.0.0.1:5432 - rejecting connections",
		"",
	} {
		if pgIsReadyOutput(out) {
			t.Errorf("%q 不该判为就绪", out)
		}
	}
}

// TestRedactSecrets 锁住"写进日志/错误的文本里的口令必须被抹掉"。
func TestRedactSecrets(t *testing.T) {
	got := redactSecrets("failed: password=Secret0123456789 ok", "Secret0123456789")
	if strings.Contains(got, "Secret0123456789") || !strings.Contains(got, "***") {
		t.Errorf("口令应被替换成 ***，实际 %q", got)
	}
	// 太短的值不替换，避免误伤正常文本（例如把 "info" 抹掉）。
	if got := redactSecrets("log level info", "info"); got != "log level info" {
		t.Errorf("短值不该被替换，实际 %q", got)
	}
}

// ============================================================================
//  安装流程（假 brew + override 命令/HTTP 出口，不碰真实服务）
// ============================================================================

type mfExecCall struct {
	name  string
	args  []string
	stdin string
}

type minifluxRecorder struct {
	calls      []mfExecCall
	roleExists bool
	dbExists   bool
	healthCode int
	meCode     int
	meUser     string
	mePassword string
}

func (r *minifluxRecorder) exec(_ context.Context, _ time.Duration, stdin, name string, args ...string) (string, error) {
	r.calls = append(r.calls, mfExecCall{name: name, args: append([]string(nil), args...), stdin: stdin})
	joined := strings.Join(args, " ")
	switch {
	case strings.HasSuffix(name, "pg_isready"):
		return "127.0.0.1:5432 - accepting connections\n", nil
	case strings.Contains(joined, "pg_roles"):
		if r.roleExists {
			return "1\n", nil
		}
		return "", nil
	case strings.Contains(joined, "pg_database"):
		if r.dbExists {
			return "1\n", nil
		}
		return "", nil
	}
	return "", nil // psql -f - / miniflux -migrate
}

func (r *minifluxRecorder) http(_ context.Context, url, user, password string) (int, error) {
	switch {
	case strings.HasSuffix(url, "/healthz"):
		return r.healthCode, nil
	case strings.HasSuffix(url, "/v1/me"):
		r.meUser, r.mePassword = user, password
		return r.meCode, nil
	}
	return 404, nil
}

// writeMinifluxFakeBrew 写一个假 brew：记录调用，按需报告两个 formula 是否已装。
//
// plistPath 是 `brew services info --json` 里回的 file 字段 —— 用真实布局
// （<家目录>/Library/LaunchAgents/homebrew.mxcl.miniflux.plist）才能让
// brewServiceInfo 推出正确 label，进而验证"登记进服务管理"这一步。
func writeMinifluxFakeBrew(t *testing.T, marker string, minifluxInstalled, pgInstalled bool, plistPath string) string {
	t.Helper()
	code := func(ok bool) string {
		if ok {
			return "0"
		}
		return "1"
	}
	p := filepath.Join(t.TempDir(), "brew")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + marker + `'
if [ "$1" = "list" ] && [ "$2" = "--versions" ]; then
  case "$3" in
    miniflux) exit ` + code(minifluxInstalled) + ` ;;
    postgresql@17) exit ` + code(pgInstalled) + ` ;;
  esac
fi
if [ "$1" = "services" ] && [ "$2" = "info" ]; then
  printf '%s' '[{"name":"miniflux","status":"started","file":"` + plistPath + `"}]'
fi
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newMinifluxHarness(t *testing.T, minifluxInstalled, pgInstalled bool) (*Manager, *minifluxRecorder, string) {
	t.Helper()
	// 面板在真实机器上是 root，系统化会成功；单测跑在普通用户下，
	// 用注入点模拟"系统化成功"，这样"顺利路径不该有告警"才是有效断言。
	stubSystemDaemonEnsure(t, nil)
	m, _ := sandboxIdempotentManager(t)
	marker := filepath.Join(t.TempDir(), "brew-calls")
	// 让 brewServiceInfo 能推出 homebrew.mxcl.miniflux；plist 真写进沙箱家目录，
	// 这样"登记进服务管理"走成功路径（AdoptCandidate 不需要 launchctl）。
	plistPath := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", minifluxLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("<plist><dict></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.opt.BrewBin = writeMinifluxFakeBrew(t, marker, minifluxInstalled, pgInstalled, plistPath)
	rec := &minifluxRecorder{healthCode: 200, meCode: 200}
	m.minifluxExecOverride = rec.exec
	m.minifluxHTTPOverride = rec.http
	// 真机上是 60 秒；单测把"健康检查失败"这条分支缩短到 2 秒。
	m.minifluxHealthTimeoutOverride = 2 * time.Second
	return m, rec, marker
}

func credValue(res *InstallResult, key string) string {
	for _, c := range res.Credentials {
		if c.Key == key {
			return c.Value
		}
	}
	return ""
}

// TestInstallMinifluxProvisionsAndKeepsSecretsOutOfSteps 是主路径回归。
func TestInstallMinifluxProvisionsAndKeepsSecretsOutOfSteps(t *testing.T) {
	m, rec, marker := newMinifluxHarness(t, true, true)
	ctx := context.Background()
	res := &InstallResult{App: "miniflux", Name: "Miniflux（RSS 阅读器）", Steps: []string{}}

	if err := m.InstallMiniflux(ctx, res); err != nil {
		t.Fatalf("安装应成功，实际: %v", err)
	}
	adminPw := credValue(res, "miniflux_admin_password")
	dbPw := credValue(res, "miniflux_db_password")
	if len(adminPw) != minifluxAdminPasswordLen || !alnumOnly(adminPw) {
		t.Fatalf("管理员口令形状不对：%q", adminPw)
	}
	if len(dbPw) != minifluxDBPasswordLen || !alnumOnly(dbPw) {
		t.Fatalf("数据库口令形状不对：%q", dbPw)
	}

	// 配置文件：内容与权限
	confPath := m.minifluxConfigPath()
	raw, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("应写出配置文件 %s: %v", confPath, err)
	}
	if got, want := string(raw), minifluxConfigContent(m.primaryIP(), dbPw, adminPw); got != want {
		t.Errorf("配置内容不对\n实际：\n%s\n期望：\n%s", got, want)
	}
	st, err := os.Stat(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("配置必须是 0600，实际 %o", perm)
	}

	// 口令只能出现在凭据区块与配置文件里，**绝不能**出现在步骤文案里
	steps := strings.Join(res.Steps, "\n")
	for _, secret := range []string{adminPw, dbPw} {
		if strings.Contains(steps, secret) {
			t.Errorf("步骤里出现了口令（会被写进任务日志与审计）：\n%s", steps)
		}
	}
	// argv 里也不许出现口令（streamCmd 会把 argv 写进任务日志）
	for _, c := range rec.calls {
		argv := strings.Join(c.args, " ")
		for _, secret := range []string{adminPw, dbPw} {
			if strings.Contains(argv, secret) {
				t.Errorf("命令行参数里出现了口令：%s %s", c.name, argv)
			}
			if strings.Contains(c.stdin, secret) && !strings.Contains(c.stdin, "PASSWORD") {
				t.Errorf("口令出现在了不该出现的位置：%s", c.stdin)
			}
		}
	}

	// 建角色 / 建库都走 psql 的 stdin，且语句正确
	var roleSQL, dbSQL string
	for _, c := range rec.calls {
		if strings.HasSuffix(c.name, "psql") && strings.Contains(c.stdin, "CREATE ROLE miniflux") {
			roleSQL = c.stdin
			if !strings.Contains(strings.Join(c.args, " "), "-f -") {
				t.Errorf("带口令的语句必须走 stdin（-f -），实际参数：%v", c.args)
			}
		}
		if strings.HasSuffix(c.name, "psql") && strings.Contains(c.stdin, "CREATE DATABASE miniflux") {
			dbSQL = c.stdin
		}
	}
	if roleSQL == "" || !strings.Contains(roleSQL, "PASSWORD '"+dbPw+"'") {
		t.Errorf("应通过 stdin 执行 CREATE ROLE 且带新口令，实际 %q", roleSQL)
	}
	if dbSQL == "" {
		t.Error("应执行 CREATE DATABASE miniflux")
	}

	// 自检用的是 admin + 上面那个口令
	if rec.meUser != minifluxAdmin || rec.mePassword != adminPw {
		t.Errorf("凭据自检应使用 admin/口令，实际 %q/%q", rec.meUser, rec.mePassword)
	}
	if res.Warning != "" {
		t.Errorf("一切正常时不该有告警，实际 %q", res.Warning)
	}
	if !strings.Contains(res.Message, "已安装") {
		t.Errorf("成功时应如实说明已安装，实际 %q", res.Message)
	}
	if !strings.Contains(res.Address, ":8087") {
		t.Errorf("结果里应给出访问地址，实际 %q", res.Address)
	}
	// 服务登记必须走成功路径（而不是回落到"可在「可纳管」里手动加入"）
	if strings.Contains(steps, "自动登记到服务管理失败") {
		t.Errorf("沙箱里 plist 已就位，登记不该失败：\n%s", steps)
	}
	if !strings.Contains(steps, "健康检查通过") {
		t.Errorf("应如实记录健康检查通过：\n%s", steps)
	}

	// 已装时不该再跑 brew install，但必须启动服务
	calls, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("假 brew 没被调用: %v", err)
	}
	if strings.Contains(string(calls), "install miniflux") {
		t.Errorf("包已装时不该再 brew install：\n%s", calls)
	}
	for _, want := range []string{"services start postgresql@17", "services start miniflux"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("应执行 %q，实际调用：\n%s", want, calls)
		}
	}
}

// TestInstallMinifluxInstallsMissingFormulas 锁住"缺包时才 brew install"。
func TestInstallMinifluxInstallsMissingFormulas(t *testing.T) {
	m, _, marker := newMinifluxHarness(t, false, false)
	ctx := context.Background()
	res := &InstallResult{App: "miniflux", Steps: []string{}}
	if err := m.InstallMiniflux(ctx, res); err != nil {
		t.Fatalf("安装应成功，实际: %v", err)
	}
	calls, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"install miniflux", "install postgresql@17"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("缺包时应执行 %q，实际调用：\n%s", want, calls)
		}
	}
}

// TestInstallMinifluxReusesExistingPasswordAndPreservesConfig 锁住重跑幂等。
func TestInstallMinifluxReusesExistingPasswordAndPreservesConfig(t *testing.T) {
	m, rec, _ := newMinifluxHarness(t, true, true)
	ctx := context.Background()

	// 上一次安装留下的配置与数据。
	before := minifluxConfigContent("10.0.0.9", "OldPass0123456789abcdef", "OldAdmin0123456789ab")
	confPath := m.minifluxConfigPath()
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	// 角色与库都已存在 → 必须走 ALTER，而且不许把口令换成新值。
	rec.roleExists, rec.dbExists = true, true

	res := &InstallResult{App: "miniflux", Steps: []string{}}
	if err := m.InstallMiniflux(ctx, res); err != nil {
		t.Fatalf("重跑安装应成功，实际: %v", err)
	}

	after, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("已有配置必须原样保留（用户改动不能被冲掉）\n之前：\n%s\n之后：\n%s", before, after)
	}
	if strings.Contains(strings.Join(res.Steps, "\n"), "已写入配置") {
		t.Errorf("已有配置时不该重写：\n%s", strings.Join(res.Steps, "\n"))
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "已保留现有配置") {
		t.Errorf("应如实说明保留了现有配置：\n%s", strings.Join(res.Steps, "\n"))
	}

	// 复用：ALTER 语句里必须是旧口令
	var alter string
	for _, c := range rec.calls {
		if strings.Contains(c.stdin, "ALTER ROLE miniflux") {
			alter = c.stdin
		}
	}
	if !strings.Contains(alter, "PASSWORD 'OldPass0123456789abcdef'") {
		t.Errorf("重跑必须复用配置里的口令，实际 ALTER：%q", alter)
	}
	if got := credValue(res, "miniflux_db_password"); got != "OldPass0123456789abcdef" {
		t.Errorf("凭据区块应给出复用的口令，实际 %q", got)
	}
	if got := credValue(res, "miniflux_admin_password"); got != "OldAdmin0123456789ab" {
		t.Errorf("凭据区块应给出配置里真正生效的管理员口令，实际 %q", got)
	}
	steps := strings.Join(res.Steps, "\n")
	if strings.Contains(steps, "OldPass0123456789abcdef") || strings.Contains(steps, "OldAdmin0123456789ab") {
		t.Errorf("步骤里出现了口令：\n%s", steps)
	}
}

// TestInstallMinifluxHealthFailureIsRealError 健康检查失败必须是真失败。
func TestInstallMinifluxHealthFailureIsRealError(t *testing.T) {
	m, rec, _ := newMinifluxHarness(t, true, true)
	ctx := context.Background()

	// 已有配置（口令已知），日志里故意带上口令，验证错误信息会脱敏。
	const dbPw = "OldPass0123456789abcdef"
	confPath := m.minifluxConfigPath()
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confPath,
		[]byte(minifluxConfigContent("10.0.0.9", dbPw, "OldAdmin0123456789ab")), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := m.minifluxLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("failed to connect: postgres://miniflux:"+dbPw+"@127.0.0.1:5432/miniflux\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.healthCode = 503

	res := &InstallResult{App: "miniflux", Steps: []string{}}
	err := m.InstallMiniflux(ctx, res)
	if err == nil {
		t.Fatal("健康检查失败必须返回错误，不许谎报成功")
	}
	if !strings.Contains(err.Error(), "/healthz") {
		t.Errorf("错误里应点名健康检查地址，实际：%v", err)
	}
	if !strings.Contains(err.Error(), logPath) {
		t.Errorf("错误里应给出日志路径 %s，实际：%v", logPath, err)
	}
	if strings.Contains(err.Error(), dbPw) {
		t.Errorf("错误信息里不许出现口令（日志可能回显它）：%v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("日志里的口令应被脱敏成 ***，实际：%v", err)
	}
	if strings.Contains(res.Message, "已安装") {
		t.Errorf("失败时绝不能写「已安装」：%q", res.Message)
	}
}

// TestInstallMinifluxAuthFailureWarnsInsteadOfFailing 401 只告警、不推翻服务。
func TestInstallMinifluxAuthFailureWarnsInsteadOfFailing(t *testing.T) {
	m, rec, _ := newMinifluxHarness(t, true, true)
	ctx := context.Background()
	rec.meCode = 401

	res := &InstallResult{App: "miniflux", Steps: []string{}}
	if err := m.InstallMiniflux(ctx, res); err != nil {
		t.Fatalf("凭据自检失败不该让整个安装失败，实际: %v", err)
	}
	if !strings.Contains(res.Warning, "reset-password") {
		t.Errorf("告警里必须告诉用户怎么重置口令，实际 %q", res.Warning)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "⚠️") {
		t.Errorf("步骤里应有显式告警：\n%s", joined)
	}
	adminPw := credValue(res, "miniflux_admin_password")
	if adminPw == "" {
		t.Fatal("凭据区块仍应给出口令（它写进了配置文件）")
	}
	if strings.Contains(joined, adminPw) || strings.Contains(res.Warning, adminPw) {
		t.Errorf("步骤/告警里不许出现口令：\n%s\n%s", joined, res.Warning)
	}
}

// TestInstallMinifluxRequiresHomebrew：没有 Homebrew 必须立刻失败，且什么都不做。
func TestInstallMinifluxRequiresHomebrew(t *testing.T) {
	m, rec, _ := newMinifluxHarness(t, true, true)
	m.opt.BrewBin = ""
	res := &InstallResult{App: "miniflux", Steps: []string{}}
	err := m.InstallMiniflux(context.Background(), res)
	if err == nil || !strings.Contains(err.Error(), "Homebrew") {
		t.Fatalf("缺 Homebrew 应报错并点名，实际 %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("缺 Homebrew 时不该执行任何命令，实际 %+v", rec.calls)
	}
	if _, statErr := os.Stat(m.minifluxConfigPath()); !os.IsNotExist(statErr) {
		t.Error("缺 Homebrew 时不该写出配置文件")
	}
}

// TestMinifluxUninstallPlanKeepsDatabase：确认框里必须写清"保留数据库"。
//
// 注意：`installerPlan` 里的 `case "miniflux"` 由父代理按协调分工粘贴（本代理
// 不编辑 uninstall_app.go）。这一条测试同时是"那个 case 有没有接上"的探针：
// 没接上时它必须以清晰的提示失败，而不是悄悄跳过。
func TestMinifluxUninstallPlanKeepsDatabase(t *testing.T) {
	m, _ := sandboxManager(t)
	plan := m.PlanUninstallFor(context.Background(), App{
		ID: "miniflux", Name: "Miniflux（RSS 阅读器）", PanelInstaller: "miniflux",
	}, nil)
	if plan.Kind != "installer" {
		t.Fatalf("应给出 installer 计划，实际 %q（blocked=%s）—— 说明 uninstall_app.go 的 "+
			"installerPlan 里还没有 case \"miniflux\"（父代理需按协调消息粘贴）", plan.Kind, plan.Blocked)
	}
	joined := strings.Join(plan.Steps, "\n")
	for _, want := range []string{"brew uninstall miniflux", "PostgreSQL", "数据库"} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载步骤里应写清 %q：\n%s", want, joined)
		}
	}
	if !strings.Contains(plan.KeepNote, "保留") || !strings.Contains(plan.KeepNote, "miniflux") {
		t.Errorf("KeepNote 必须写明保留数据库，实际 %q", plan.KeepNote)
	}
	// 数据路径为空 = 面板不会删任何数据目录
	if len(plan.DataPaths) != 0 {
		t.Errorf("miniflux 卸载不该声明待删数据路径，实际 %v", plan.DataPaths)
	}
}

// ============================================================================
//  pg_isready 的就绪判据：**退出码优先，文本只是兜底**
//
//  真机事故（2026-09-17 Mac mini）：PostgreSQL 其实早已就绪（日志里每分钟一条
//  成功探测、端口在听），但 mini 的系统语言是中文，`pg_isready` 打的是
//  "127.0.0.1:5432 - 接受连接"，而这里只看英文 "accepting connections" ——
//  于是 60 秒后任务必然失败，用户看到的是"PostgreSQL 没有就绪"这种假结论。
// ============================================================================

// TestWaitMinifluxPostgresAcceptsLocalizedOutput 锁住"输出语言不该影响结论"。
func TestWaitMinifluxPostgresAcceptsLocalizedOutput(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)
	m.minifluxExecOverride = func(_ context.Context, _ time.Duration, _ string, _ string, _ ...string) (string, error) {
		// 退出码 0（err == nil）+ 中文输出：语义上就是"接受连接"。
		return "127.0.0.1:5432 - 接受连接\n", nil
	}
	if !m.waitMinifluxPostgres(context.Background(), 3*time.Second) {
		t.Fatal("pg_isready 退出码 0 就该判为就绪，与输出文本的语言无关")
	}
}

// TestWaitMinifluxPostgresRejectsNonZeroExit 锁住反方向：非 0 退出码不得判成就绪。
func TestWaitMinifluxPostgresRejectsNonZeroExit(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)
	m.minifluxExecOverride = func(_ context.Context, _ time.Duration, _ string, _ string, _ ...string) (string, error) {
		return "127.0.0.1:5432 - 无响应\n", errFake("exit status 2")
	}
	if m.waitMinifluxPostgres(context.Background(), 2*time.Second) {
		t.Fatal("pg_isready 退出码非 0（no response / rejecting）不得判为就绪")
	}
}

// TestWaitMinifluxPostgresPassesDatabaseNameToPgIsReady 锁住 -d postgres。
//
// 不指定库名时 pg_isready 会去连"与登录用户同名的库"，brew 的 cluster 里没有它，
// 服务端回 FATAL（真机日志 `database "zizdog" does not exist`）—— 必须显式给库名。
func TestWaitMinifluxPostgresPassesDatabaseNameToPgIsReady(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)
	var gotArgs []string
	m.minifluxExecOverride = func(_ context.Context, _ time.Duration, _ string, _ string, args ...string) (string, error) {
		gotArgs = append([]string(nil), args...)
		return "accepting connections\n", nil
	}
	if !m.waitMinifluxPostgres(context.Background(), 3*time.Second) {
		t.Fatal("应判为就绪")
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "-d postgres") {
		t.Errorf("pg_isready 必须带 -d postgres（否则会去连与登录用户同名的库），实际参数：%v", gotArgs)
	}
}

// errFake 是测试里用的最小 error（避免为一行引入额外依赖）。
type errFake string

func (e errFake) Error() string { return string(e) }
