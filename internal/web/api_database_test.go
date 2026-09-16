package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/config"
)

// ============================================================================
//  数据库页的三态凭据 + "改自己的口令要写回配置"
//
//  背景（2026-09-16 mini）：
//    · 页面把"面板配置里是空口令"显示成了"root 无密码"，把用户引向了错误结论；
//    · 用户点"改密码"后 ALTER USER 成功，但面板没把新口令写回配置，
//      从此所有库操作 1045 —— 面板把自己锁在了门外。
//
//  这里用一个**假 mysql 客户端**（shell 脚本）模拟服务器：
//  它把"服务器当前口令"存在一个状态文件里，只有 MYSQL_PWD 与之相等才应答成功。
//  这样整条路径（构造参数 → 执行 → 解析 1045 → 状态判定 → 写回配置）
//  都是真实代码，而**不需要真实 MySQL**。
// ============================================================================

// fakeMySQLScript 是假 mysql 客户端的实现。
//
// 行为：
//   - 记录每次调用的 argv 与 stdin 到 $MYSQL_FAKE_LOG（方便断言/排障）；
//   - 收到 ALTER USER … IDENTIFIED BY 'x' 时把 $MYSQL_FAKE_STATE 改成 x（成功）；
//   - 其它情况按"查询"处理：口令与状态文件一致才输出，否则返回 1045
//     （并按有没有带口令给出 using password: NO/YES）。
const fakeMySQLScript = `#!/bin/sh
LOG="${MYSQL_FAKE_LOG:-/dev/null}"
STATE="${MYSQL_FAKE_STATE:-/dev/null}"
printf 'ARGS: %s\n' "$*" >> "$LOG"
# 只有不带 -e 的调用才从 stdin 读语句（-e 的查询没写 stdin，
# 若无条件 cat 会去读测试进程的 stdin 并可能卡住）。
SQL=""
case "$*" in
  *-e*) ;;
  *) SQL=$(cat) ;;
esac
if [ -n "$SQL" ]; then printf 'SQL: %s\n' "$SQL" >> "$LOG"; fi
cur=$(cat "$STATE" 2>/dev/null)
# 先认证：真实 MySQL 下"连接都不通"是不可能执行 ALTER 的。
if [ "${MYSQL_PWD-}" != "$cur" ]; then
  if [ -z "${MYSQL_PWD-}" ]; then
    echo "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: NO)" >&2
  else
    echo "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)" >&2
  fi
  exit 1
fi
case "$SQL" in
  *"ALTER USER"*)
    pw=$(printf '%s' "$SQL" | sed -e "s/.*IDENTIFIED BY '//" -e "s/'.*//")
    printf '%s' "$pw" > "$STATE"
    exit 0 ;;
esac
printf '8.4.11\n'
exit 0
`

// installFakeMySQL 在测试服务器的沙箱 brew 前缀里放一个假 mysql 客户端。
// serverPassword 是"服务器当前的口令"（空串＝服务器确实无口令）。
// 返回状态文件路径。
func installFakeMySQL(t *testing.T, srv *Server, serverPassword string) string {
	t.Helper()
	binDir := filepath.Join(srv.Cfg.BrewPrefix, "opt", "mysql@8.4", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(srv.Cfg.DataDir, "fake-mysql.state")
	if err := os.WriteFile(statePath, []byte(serverPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mysql"), []byte(fakeMySQLScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYSQL_FAKE_STATE", statePath)
	t.Setenv("MYSQL_FAKE_LOG", filepath.Join(srv.Cfg.DataDir, "fake-mysql.log"))
	// 防止开发机上恰好设了 MYSQL_PWD（会让"空口令"分支拿错值）。
	t.Setenv("MYSQL_PWD", "")
	return statePath
}

// loginPanel 初始化并登录测试面板，返回会话 Cookie。
func loginPanel(t *testing.T, ts *httptest.Server) []*http.Cookie {
	t.Helper()
	res, out, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("初始化失败 %d: %v", res.StatusCode, out)
	}
	return cookies
}

// credentialOf 从 /api/v1/database 的响应里取出 credential 块。
func credentialOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	data, _ := out["data"].(map[string]any)
	cred, _ := data["credential"].(map[string]any)
	if cred == nil {
		t.Fatalf("响应里缺少 credential 块（页面靠它区分三态）：%v", out)
	}
	return cred
}

// TestDatabaseOverviewStatesAreVerified 是本轮页面语义的核心断言：
// 状态必须由**真连接的结果**得出，而不是"配置里有没有写口令"。
func TestDatabaseOverviewStatesAreVerified(t *testing.T) {
	cases := []struct {
		name           string
		serverPassword string // 服务器真实口令
		panelPassword  string // 面板持有的口令
		wantState      string
		wantConnected  bool
		wantStatus     int // 认证类失败故意是 400：前端只有抛错才会渲染「连接设置」表单
	}{
		{"面板没口令+服务器要口令", "server-pw-123", "", "unconfigured", false, 400},
		{"面板有口令+口令不对", "server-pw-123", "wrong-pw-456", "auth_failed", false, 400},
		{"面板有口令+口令正确", "server-pw-123", "server-pw-123", "ok", true, 200},
		{"服务器确实无口令", "", "", "ok_no_password", true, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := newTestServer(t)
			installFakeMySQL(t, srv, tc.serverPassword)
			cookies := loginPanel(t, ts)

			srv.Cfg.SetMySQLPassword(tc.panelPassword)
			res, out, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("状态码应为 %d，实际 %d: %v", tc.wantStatus, res.StatusCode, out)
			}
			// 认证类失败必须是 400 + 带 msg：前端 catch 分支靠 msg 渲染
			// 「连接设置」表单 —— 那是用户唯一能自救的入口。
			if tc.wantStatus == 400 {
				if msg := asString(out["msg"]); !strings.Contains(msg, "1045") {
					t.Errorf("400 的 msg 必须带原始错误，实际 %q", msg)
				}
			}
			cred := credentialOf(t, out)
			if cred["state"] != tc.wantState {
				t.Fatalf("状态应为 %s，实际 %v（%v）", tc.wantState, cred["state"], out)
			}
			data, _ := out["data"].(map[string]any)
			if data["connected"] != tc.wantConnected {
				t.Errorf("connected 应为 %v，实际 %v", tc.wantConnected, data["connected"])
			}
			if cred["verified"] != tc.wantConnected {
				t.Errorf("verified 应等于真连结果 %v，实际 %v", tc.wantConnected, cred["verified"])
			}
			// 只有"验证过确实无口令"才允许给人的印象是"没有口令"
			if tc.wantState != "ok_no_password" {
				hint := asString(cred["hint"])
				if strings.Contains(hint, "确实没有设置口令") {
					t.Errorf("没验证过就不许说'确实没有口令'：%s", hint)
				}
			}
			// 认证类失败必须给出可操作提示 + 原始错误
			if tc.wantState == "unconfigured" || tc.wantState == "auth_failed" {
				hint := asString(cred["hint"])
				if !strings.Contains(hint, "连接设置") {
					t.Errorf("提示要指出去哪里改口令：%s", hint)
				}
				if !strings.Contains(asString(cred["error"]), "1045") {
					t.Errorf("要附上原始错误（用户/我们排障都靠它）：%v", cred["error"])
				}
			}
			// has_password 的语义只是"面板手里有没有口令"，页面别拿它当"MySQL 有没有口令"
			wantConfigured := tc.panelPassword != ""
			if data["has_password"] != wantConfigured {
				t.Errorf("has_password 应为 %v（面板是否持有口令），实际 %v", wantConfigured, data["has_password"])
			}
		})
	}
}

// TestDatabaseOverviewUnreachable 服务没起来时是第四态（与口令无关）。
func TestDatabaseOverviewUnreachable(t *testing.T) {
	srv, ts := newTestServer(t)
	// 不放假客户端：客户端本身找不到 ⇒ 归到"连不上"，而不是"口令不对"
	srv.Cfg.SetMySQLPassword("whatever-123")
	cookies := loginPanel(t, ts)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("概览应 200，实际 %d", res.StatusCode)
	}
	cred := credentialOf(t, out)
	if cred["state"] != "unreachable" {
		t.Fatalf("找不到客户端/服务没起来应为 unreachable，实际 %v", cred["state"])
	}
	if !strings.Contains(asString(cred["hint"]), "服务管理") {
		t.Errorf("应给出排查入口：%v", cred["hint"])
	}
}

// TestDatabasePasswordResetWritesBackToPanelConfig 是这次事故的**回归锁**：
// 改的若是面板自己的登录账号，新口令必须写回 config.json ——
// 否则下一次操作就是 1045，而且再也改不回来。
func TestDatabasePasswordResetWritesBackToPanelConfig(t *testing.T) {
	srv, ts := newTestServer(t)
	statePath := installFakeMySQL(t, srv, "") // 服务器初始无口令
	cookies := loginPanel(t, ts)
	// 面板初始以为无口令（与服务器一致）
	srv.Cfg.SetMySQLPassword("")

	const newPw = "Brand-New-RootPw-2026"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user/password",
		map[string]any{"user": "root", "host": "localhost", "password": newPw}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("改口令应成功，实际 %d: %v", res.StatusCode, out)
	}
	if msg := dataMsg(out); !strings.Contains(msg, "写入面板配置") {
		t.Errorf("应告诉用户面板已记住新口令，实际 %q", msg)
	}
	// 服务器端确实改了
	if b, _ := os.ReadFile(statePath); string(b) != newPw {
		t.Fatalf("MySQL 端口令应为 %q，实际 %q", newPw, string(b))
	}
	// 面板配置也改了（内存 + 磁盘）
	if got := srv.Cfg.MySQLPasswordValue(); got != newPw {
		t.Errorf("面板内存里的口令应更新，实际 %q", got)
	}
	if got := readConfigMySQLPassword(t, srv.Cfg); got != newPw {
		t.Errorf("config.json 里应写入新口令（否则重启面板就锁死），实际 %q", got)
	}
	// 之后概览必须还是"已验证可用"
	_, out2, _ := doJSON(t, ts, "GET", "/api/v1/database", nil, cookies)
	if cred := credentialOf(t, out2); cred["state"] != "ok" {
		t.Errorf("改完口令后应仍是已连接状态，实际 %v", cred["state"])
	}
	// 审计里不许有口令
	assertAuditHasNoSecret(t, srv, newPw)
}

// TestDatabasePasswordResetFailureGivesActionableError：
// 认证失败时必须说清"面板口令与 MySQL 不一致"以及怎么补救，
// 而不是泛泛一句"请检查密码"；并且**不能**偷偷改配置。
func TestDatabasePasswordResetFailureGivesActionableError(t *testing.T) {
	srv, ts := newTestServer(t)
	installFakeMySQL(t, srv, "server-pw-123") // 服务器要口令
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("") // 面板手里是空的 ⇒ 改口令时会 1045

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user/password",
		map[string]any{"user": "root", "host": "localhost", "password": "Another-Pw-123456"}, cookies)
	if res.StatusCode == 200 {
		t.Fatal("认证失败必须报错")
	}
	msg := asString(out["msg"])
	for _, want := range []string{"没有带口令", "两者不一致", "连接设置"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误说明缺少 %q：%s", want, msg)
		}
	}
	if !strings.Contains(msg, "1045") {
		t.Errorf("要附原始错误：%s", msg)
	}
	// 配置不能被改动（否则就把面板口令改成错的了）
	if got := readConfigMySQLPassword(t, srv.Cfg); got != "" {
		t.Errorf("失败时不该动面板配置，实际 %q", got)
	}
	assertAuditHasNoSecret(t, srv, "Another-Pw-123456")
}

// TestDatabasePasswordResetOtherIdentityKeepsConfig：
// 改的不是面板用的身份（例如 root@% ）→ 面板配置保持不变，且如实说明。
func TestDatabasePasswordResetOtherIdentityKeepsConfig(t *testing.T) {
	srv, ts := newTestServer(t)
	installFakeMySQL(t, srv, "panel-pw-123")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("panel-pw-123")
	before := readConfigMySQLPassword(t, srv.Cfg)

	// 改 root@%：假客户端照做（会把"服务器口令"改掉），但面板连的是 localhost，
	// 所以面板不该把自己的口令改掉。
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database/user/password",
		map[string]any{"user": "root", "host": "%", "password": "Remote-Pw-123456"}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("改别的身份应成功，实际 %d: %v", res.StatusCode, out)
	}
	if msg := dataMsg(out); !strings.Contains(msg, "面板配置未改动") {
		t.Errorf("应说明面板配置未改动，实际 %q", msg)
	}
	if got := readConfigMySQLPassword(t, srv.Cfg); got != before {
		t.Errorf("面板配置不该被改动：改前 %q 改后 %q", before, got)
	}
}

// TestDatabaseCreateReflectsAuthFailure：新建库在口令不一致时也要给可操作错误。
func TestDatabaseCreateReflectsAuthFailure(t *testing.T) {
	srv, ts := newTestServer(t)
	installFakeMySQL(t, srv, "server-pw-123")
	cookies := loginPanel(t, ts)
	srv.Cfg.SetMySQLPassword("")

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/database",
		map[string]any{"name": "blog", "charset": "utf8mb4"}, cookies)
	if res.StatusCode == 200 {
		t.Fatal("口令不一致时建库必须失败")
	}
	msg := asString(out["msg"])
	if !strings.Contains(msg, "两者不一致") || !strings.Contains(msg, "连接设置") {
		t.Errorf("建库失败的说明要可操作，实际：%s", msg)
	}
}

// ---------- 小工具 ----------

// dataMsg 取成功响应（{"ok":true,"data":{...}}）里的 msg 字段。
func dataMsg(out map[string]any) string {
	data, _ := out["data"].(map[string]any)
	return asString(data["msg"])
}

// readConfigMySQLPassword 从**磁盘上的 config.json** 读口令。
// 断言磁盘而不是内存：内存值重启就没了，用户真正依赖的是落盘那一份。
func readConfigMySQLPassword(t *testing.T, cfg *config.Config) string {
	t.Helper()
	b, err := os.ReadFile(cfg.Path())
	if err != nil {
		if os.IsNotExist(err) {
			// 还没落盘过 ⇒ 等价于"配置里没有口令"
			return ""
		}
		t.Fatalf("读配置文件失败: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("解析配置文件失败: %v", err)
	}
	v, _ := raw["mysql_password"].(string)
	return v
}

// assertAuditHasNoSecret 断言审计里没有明文口令。
func assertAuditHasNoSecret(t *testing.T, srv *Server, secret string) {
	t.Helper()
	rows, err := srv.Store.DB().Query(`SELECT action, target, detail, message FROM audit_logs`)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var action, target, detail, message string
		if err := rows.Scan(&action, &target, &detail, &message); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{action, target, detail, message} {
			if strings.Contains(f, secret) {
				t.Fatalf("审计里出现了口令（会被长期保存）：action=%s detail=%s", action, detail)
			}
		}
	}
}
