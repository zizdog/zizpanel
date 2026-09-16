package web

// db_credentials_test.go —— 数据库口令"保管与回读"的单元测试。
//
// 覆盖的硬性要求：
//   ① 面板创建账号成功后，口令能被读回；
//   ② **MySQL 操作失败时绝不记录口令**（防"面板显示一个没生效的口令"）；
//   ③ 未知口令的账号 password_known=false，且响应里**不含伪造的 password**；
//   ④ 口令不出现在审计里；
//   ⑤ 改口令会覆盖旧记录（upsert），删账号会清理记录；
//   ⑥ 写库失败必须如实告知（warning），不许静默吞掉。
//
// 沙箱纪律（与 api_database_test.go 一致）：
//   · 不碰真实 MySQL：用假 mysql 客户端（shell 脚本）模拟服务器；
//   · 不碰真实家目录/生产配置：newTestServer 已把 UserHome/WWWRoot/BrewPrefix
//     全部指向 t.TempDir()（见 TestTestServerSandboxedAwayFromRealHome）。
//
// 口令纪律：本文件里出现的口令全部是测试用的假值，真实口令不入库、不入仓库。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCredMySQLScript 是比 api_database_test.go 里那个更完整的假 mysql 客户端：
// 它除了认证与 ALTER USER，还能应答 ListUsers 需要的两条查询
// （mysql.user 与 information_schema.SCHEMA_PRIVILEGES），
// 这样"账号列表 → 口令合并"这条路径也能在无真实 MySQL 的情况下被验证。
//
// 服务器"当前口令"放在 $MYSQL_FAKE_STATE；账号列表放在 $MYSQL_FAKE_USERS
// （每行 user<TAB>host<TAB>locked<TAB>has_password<TAB>plugin）。
const fakeCredMySQLScript = `#!/bin/sh
LOG="${MYSQL_FAKE_LOG:-/dev/null}"
STATE="${MYSQL_FAKE_STATE:-/dev/null}"
USERS="${MYSQL_FAKE_USERS:-/dev/null}"
printf 'ARGS: %s\n' "$*" >> "$LOG"
# 只有不带 -e 的调用才从 stdin 读语句（-e 的查询没写 stdin，
# 无条件 cat 会去读测试进程的 stdin 并可能卡住）。
SQL=""
case "$*" in
  *-e*) ;;
  *) SQL=$(cat) ;;
esac
if [ -n "$SQL" ]; then printf 'SQL: %s\n' "$SQL" >> "$LOG"; fi
cur=$(cat "$STATE" 2>/dev/null)
# 先认证：真实 MySQL 下"连接都不通"是不可能执行 ALTER/CREATE 的。
if [ "${MYSQL_PWD-}" != "$cur" ]; then
  if [ -z "${MYSQL_PWD-}" ]; then
    echo "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: NO)" >&2
  else
    echo "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)" >&2
  fi
  exit 1
fi
if [ -n "$SQL" ]; then
  case "$SQL" in
    *"ALTER USER"*)
      # 只有改 **root**（面板登录身份）才动"服务器当前口令"状态文件。
      # 改别的账号（如 appuser）不该影响面板自己的连接 —— 真实 MySQL 也是如此。
      who=$(printf '%s' "$SQL" | sed -e "s/.*ALTER USER '//" -e "s/'@'.*//")
      if [ "$who" = "root" ]; then
        pw=$(printf '%s' "$SQL" | sed -e "s/.*IDENTIFIED BY '//" -e "s/'.*//")
        printf '%s' "$pw" > "$STATE"
      fi
      ;;
  esac
  exit 0
fi
case "$*" in
  *"FROM mysql.user"*) cat "$USERS" 2>/dev/null; exit 0 ;;
  *"SCHEMA_PRIVILEGES"*) exit 0 ;;
  *"SELECT VERSION"*) echo "8.4.11"; exit 0 ;;
esac
exit 0
`

// installCredFakeMySQL 在测试服务器的沙箱 brew 前缀里放一个假 mysql 客户端。
// serverPassword 是"服务器当前口令"（空串＝确实无口令）；
// users 是账号列表行（user<TAB>host<TAB>locked<TAB>has_password<TAB>plugin）。
func installCredFakeMySQL(t *testing.T, srv *Server, serverPassword string, users ...string) {
	t.Helper()
	binDir := filepath.Join(srv.Cfg.BrewPrefix, "opt", "mysql@8.4", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(srv.Cfg.DataDir, "cred-fake-mysql.state")
	if err := os.WriteFile(statePath, []byte(serverPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	usersPath := filepath.Join(srv.Cfg.DataDir, "cred-fake-mysql.users")
	body := ""
	for _, u := range users {
		body += u + "\n"
	}
	if err := os.WriteFile(usersPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mysql"), []byte(fakeCredMySQLScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYSQL_FAKE_STATE", statePath)
	t.Setenv("MYSQL_FAKE_LOG", filepath.Join(srv.Cfg.DataDir, "cred-fake-mysql.log"))
	t.Setenv("MYSQL_FAKE_USERS", usersPath)
	// 防止开发机上恰好设了 MYSQL_PWD（会让"空口令"分支拿错值）。
	t.Setenv("MYSQL_PWD", "")
}

// ---------- 小工具 ----------

// usersOf 从 /api/v1/database 响应里取出 users 数组（每项是 map）。
func usersOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应里没有 data 块：%v", out)
	}
	raw, ok := data["users"].([]any)
	if !ok {
		t.Fatalf("users 不是数组（页面靠它渲染账号表）：%v", data["users"])
	}
	list := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("users 元素不是对象：%v", it)
		}
		list = append(list, m)
	}
	return list
}

// userEntry 按 user@host 找一条账号记录。
func userEntry(t *testing.T, users []map[string]any, user, host string) map[string]any {
	t.Helper()
	for _, u := range users {
		if asString(u["user"]) == user && asString(u["host"]) == host {
			return u
		}
	}
	t.Fatalf("账号列表里找不到 %s@%s：%v", user, host, users)
	return nil
}

// countCredentialRows 数按 user 过滤后的口令记录行数。
func countCredentialRows(t *testing.T, srv *Server, user string) int {
	t.Helper()
	var n int
	if err := srv.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM db_credentials WHERE user = ?`, user).Scan(&n); err != nil {
		t.Fatalf("统计口令记录失败: %v", err)
	}
	return n
}

// ---------- ① / ⑤ 保管、回读、upsert、清理 ----------

func TestDBPasswordRememberRecallForget(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()

	// 没存过 → 必须 known=false（页面据此显示"不可回显"，而不是空串）
	if pw, known := srv.RecallDBPassword(ctx, "appuser", "localhost"); known || pw != "" {
		t.Fatalf("没存过时必须 known=false，实际 known=%v pw=%q", known, pw)
	}

	if err := srv.RememberDBPassword(ctx, "appuser", "localhost", "S3cret-Pw-0001", DBCredSourcePanel); err != nil {
		t.Fatalf("RememberDBPassword 失败: %v", err)
	}
	pw, known := srv.RecallDBPassword(ctx, "appuser", "localhost")
	if !known || pw != "S3cret-Pw-0001" {
		t.Fatalf("读回应为 known=true pw=S3cret-Pw-0001，实际 known=%v pw=%q", known, pw)
	}

	// 改口令 = UPSERT 覆盖，不新增行
	if err := srv.RememberDBPassword(ctx, "appuser", "localhost", "R0tated-Pw-0002", DBCredSourceSiteInstall); err != nil {
		t.Fatalf("覆盖保存失败: %v", err)
	}
	if pw, known = srv.RecallDBPassword(ctx, "appuser", "localhost"); !known || pw != "R0tated-Pw-0002" {
		t.Fatalf("改口令应覆盖旧值，实际 known=%v pw=%q", known, pw)
	}
	if n := countCredentialRows(t, srv, "appuser"); n != 1 {
		t.Fatalf("(user,host) 唯一键应保证覆盖而非新增，实际 %d 行", n)
	}

	// host 缺省按 localhost 处理（与创建账号的默认值一致）
	if err := srv.RememberDBPassword(ctx, "otheruser", "", "Other-Pw-0003", DBCredSourcePanel); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if pw, known := srv.RecallDBPassword(ctx, "otheruser", "localhost"); !known || pw != "Other-Pw-0003" {
		t.Fatalf("空 host 应等同 localhost，实际 known=%v pw=%q", known, pw)
	}

	// 账号为空必须拒绝（否则会写出无主的口令记录）
	if err := srv.RememberDBPassword(ctx, "  ", "localhost", "x", DBCredSourcePanel); err == nil {
		t.Fatal("账号名为空时必须报错，不能写库")
	}

	// 删除账号 → 清理记录；重复清理必须幂等
	if err := srv.ForgetDBPassword(ctx, "appuser", "localhost"); err != nil {
		t.Fatalf("ForgetDBPassword 失败: %v", err)
	}
	if _, known := srv.RecallDBPassword(ctx, "appuser", "localhost"); known {
		t.Fatal("清理后不该再读回口令")
	}
	if err := srv.ForgetDBPassword(ctx, "appuser", "localhost"); err != nil {
		t.Fatalf("重复清理应幂等（删一个没存过的账号不该报错）: %v", err)
	}
}

// ---------- ① 建号成功后口令可读回 + ④ 审计无口令 ----------

func TestCreateUserRemembersPassword(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "panel-pw-123",
		"root\tlocalhost\t0\t1\tcaching_sha2_password",
		"appuser\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")

	const newPw = "App-Pw-123456"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user", map[string]any{
		"user": "appuser", "host": "localhost", "password": newPw,
		"privileges": []string{"SELECT"}, "databases": []string{"blog"},
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("建号应成功，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if w := asString(data["warning"]); w != "" {
		t.Fatalf("正常路径不该带 warning，实际 %q", w)
	}
	if pw, known := srv.RecallDBPassword(context.Background(), "appuser", "localhost"); !known || pw != newPw {
		t.Fatalf("建号成功后口令必须可读回，实际 known=%v pw=%q", known, pw)
	}
	// 概览里的密码列也要反映出来
	_, out2, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	entry := userEntry(t, usersOf(t, out2), "appuser", "localhost")
	if entry["password_known"] != true || asString(entry["password"]) != newPw {
		t.Fatalf("概览应带出已知口令，实际 %v", entry)
	}
	if asString(entry["password_source"]) != DBCredSourcePanel {
		t.Fatalf("来源应为 panel，实际 %q", entry["password_source"])
	}
	assertAuditHasNoSecret(t, srv, newPw)
}

// ---------- ② MySQL 失败时不许记录 ----------

func TestCreateUserFailureDoesNotRememberPassword(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "server-pw-123",
		"root\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	// 面板手里是空口令 → 假服务器返回 1045 → CreateUser 必失败
	srv.Cfg.SetMySQLPassword("")

	const neverPw = "Never-Saved-Pw-1"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user", map[string]any{
		"user": "appuser", "host": "localhost", "password": neverPw,
		"privileges": []string{"SELECT"}, "databases": []string{"blog"},
	}, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("MySQL 操作失败时建号必须报错：%v", out)
	}
	if n := countCredentialRows(t, srv, "appuser"); n != 0 {
		t.Fatalf("MySQL 失败时**绝不许**记录口令（否则页面会显示一个没生效的口令），实际 %d 行", n)
	}
	if _, known := srv.RecallDBPassword(context.Background(), "appuser", "localhost"); known {
		t.Fatal("MySQL 失败后不该能读回口令")
	}
	assertAuditHasNoSecret(t, srv, neverPw)
}

// ---------- ③ 未知口令不伪造 ----------

func TestUnknownPasswordIsNotFaked(t *testing.T) {
	srv, ts := newTestServer(t)
	// extuser 是"别人在 MySQL 里手工建的"：面板从没存过它的口令。
	installCredFakeMySQL(t, srv, "panel-pw-123",
		"root\tlocalhost\t0\t1\tcaching_sha2_password",
		"extuser\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("概览应 200，实际 %d: %v", res.StatusCode, out)
	}
	entry := userEntry(t, usersOf(t, out), "extuser", "localhost")
	if entry["password_known"] != false {
		t.Fatalf("面板没存过口令时 password_known 必须为 false，实际 %v", entry["password_known"])
	}
	// 关键：未知时**不许**放一个空串或任何值冒充
	if v, has := entry["password"]; has {
		t.Fatalf("未知口令时响应里不许出现 password 字段（更不能是空串冒充），实际 %#v", v)
	}
	if src := asString(entry["password_source"]); src != "" {
		t.Fatalf("未知口令时不该有来源，实际 %q", src)
	}
	if entry["panel_account"] == true {
		t.Fatalf("extuser 不是面板连接账号，不该被标成 panel_account：%v", entry)
	}
}

// ---------- root 行：面板自己的口令必须显示且标成 panel ----------

func TestPanelAccountPasswordShownAsPanelSource(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "panel-pw-123",
		"root\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	root := userEntry(t, usersOf(t, out), "root", "localhost")
	if root["password_known"] != true {
		t.Fatalf("面板连接账号(root)的口令必须能显示，实际 %v", root)
	}
	if got := asString(root["password"]); got != "panel-pw-123" {
		t.Fatalf("root 口令应取面板配置里的值，实际 %q", got)
	}
	if asString(root["password_source"]) != DBCredSourcePanel {
		t.Fatalf("root 来源应为 panel，实际 %q", root["password_source"])
	}
	if root["panel_account"] != true {
		t.Fatalf("root 应被标成 panel_account（页面据此注明它同时是面板连接口令）：%v", root)
	}
}

// "真连接验证过服务器确实无口令" = 已知为空，而不是未知。
func TestPanelAccountVerifiedEmptyPasswordIsKnownEmpty(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "",
		"root\tlocalhost\t0\t0\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("")

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	if cred := credentialOf(t, out); cred["state"] != "ok_no_password" {
		t.Fatalf("前置条件应是已验证无口令，实际 %v", cred["state"])
	}
	root := userEntry(t, usersOf(t, out), "root", "localhost")
	if root["password_known"] != true {
		t.Fatalf("验证过确实无口令时应为已知（空），而不是不可回显：%v", root)
	}
	if got := asString(root["password"]); got != "" {
		t.Fatalf("已知为空口令时 password 应为空，实际 %q", got)
	}
}

// ---------- ⑤ 改口令覆盖旧记录，并且概览刷新到新值 ----------

func TestChangePasswordOverwritesRemembered(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "panel-pw-123",
		"appuser\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")
	ctx := context.Background()

	if err := srv.RememberDBPassword(ctx, "appuser", "localhost", "Old-Pw-000001", DBCredSourcePanel); err != nil {
		t.Fatal(err)
	}
	const newPw = "New-Pw-000002"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user/password",
		map[string]any{"user": "appuser", "host": "localhost", "password": newPw}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("改口令应成功，实际 %d: %v", res.StatusCode, out)
	}
	if pw, known := srv.RecallDBPassword(ctx, "appuser", "localhost"); !known || pw != newPw {
		t.Fatalf("改口令应覆盖旧记录，实际 known=%v pw=%q", known, pw)
	}
	if n := countCredentialRows(t, srv, "appuser"); n != 1 {
		t.Fatalf("改口令不该留下旧记录，实际 %d 行", n)
	}
	_, out2, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	entry := userEntry(t, usersOf(t, out2), "appuser", "localhost")
	if asString(entry["password"]) != newPw {
		t.Fatalf("概览应刷新为新口令，实际 %q", entry["password"])
	}
	assertAuditHasNoSecret(t, srv, newPw)
	assertAuditHasNoSecret(t, srv, "Old-Pw-000001")
}

// 改面板自己账号的口令：记入口令 + 写回配置 + 概览显示新口令。
func TestChangePanelAccountPasswordUpdatesOverview(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "old-panel-pw-1",
		"root\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("old-panel-pw-1")
	ctx := context.Background()

	const newPw = "Brand-New-Pw-2026"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user/password",
		map[string]any{"user": "root", "host": "localhost", "password": newPw}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("改面板账号口令应成功，实际 %d: %v", res.StatusCode, out)
	}
	if got := srv.Cfg.MySQLPasswordValue(); got != newPw {
		t.Fatalf("面板账号的口令必须写回配置（否则会把自己锁在门外），实际 %q", got)
	}
	if pw, known := srv.RecallDBPassword(ctx, "root", "localhost"); !known || pw != newPw {
		t.Fatalf("改口令后应能读回新口令，实际 known=%v pw=%q", known, pw)
	}
	_, out2, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	root := userEntry(t, usersOf(t, out2), "root", "localhost")
	if asString(root["password"]) != newPw {
		t.Fatalf("概览里的 root 口令应刷新为新值，实际 %q", root["password"])
	}
	assertAuditHasNoSecret(t, srv, newPw)
}

// ---------- ⑤ 删除账号会清理口令记录 ----------

func TestDropUserForgetsPassword(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "panel-pw-123",
		"appuser\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")
	ctx := context.Background()

	if err := srv.RememberDBPassword(ctx, "appuser", "localhost", "To-Be-Dropped-1", DBCredSourcePanel); err != nil {
		t.Fatal(err)
	}
	res, out, _ := doJSON(t, ts, "DELETE",
		"/api/v1/database/user?user=appuser&host=localhost", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除账号应成功，实际 %d: %v", res.StatusCode, out)
	}
	if _, known := srv.RecallDBPassword(ctx, "appuser", "localhost"); known {
		t.Fatal("账号删除后必须清掉保存的口令，不留一份永不使用的明文")
	}
	if n := countCredentialRows(t, srv, "appuser"); n != 0 {
		t.Fatalf("账号删除后不该还有口令记录，实际 %d 行", n)
	}
}

// ---------- ⑥ 写库失败必须如实告知，不许静默吞掉 ----------

func TestRememberFailureIsSurfacedNotSwallowed(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "panel-pw-123",
		"root\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")

	// 制造"面板库写不进去"：删掉 db_credentials 表本身。
	// 比关掉 SQLite 句柄更精准 —— 会话/审计表照常可用，请求能正常走到处理器。
	if _, err := srv.Store.DB().Exec(`DROP TABLE db_credentials`); err != nil {
		t.Fatal(err)
	}
	const newPw = "Unsaveable-Pw-01"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user", map[string]any{
		"user": "appuser", "host": "localhost", "password": newPw,
		"privileges": []string{"SELECT"}, "databases": []string{"blog"},
	}, cookies)
	// 账号确实在 MySQL 里建好了 → 主操作成功，不能报成"创建失败"（那是另一种撒谎）；
	// 但**必须**带上 warning，把"口令没能保存"这件事说清楚。
	if res.StatusCode != 200 {
		t.Fatalf("账号已真实创建，不该报成失败（会误导用户以为没建成）：%d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	warn := asString(data["warning"])
	if warn == "" || !strings.Contains(warn, "无法保存") {
		t.Fatalf("写库失败必须如实告知（warning 里要有原因），实际 %v", out)
	}
	assertAuditHasNoSecret(t, srv, newPw)
}

func TestForgetFailureIsSurfacedNotSwallowed(t *testing.T) {
	srv, ts := newTestServer(t)
	installCredFakeMySQL(t, srv, "panel-pw-123",
		"appuser\tlocalhost\t0\t1\tcaching_sha2_password")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")

	if _, err := srv.Store.DB().Exec(`DROP TABLE db_credentials`); err != nil {
		t.Fatal(err)
	}
	res, out, _ := doJSON(t, ts, "DELETE",
		"/api/v1/database/user?user=appuser&host=localhost", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("账号已真实删除，不该报成失败：%d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if warn := asString(data["warning"]); warn == "" || !strings.Contains(warn, "清理") {
		t.Fatalf("清理失败必须如实告知，实际 %v", out)
	}
}
