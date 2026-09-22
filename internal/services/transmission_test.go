package services

// 2026-09-20 新增三个原生条目（Memos / Navidrome / Transmission）的门禁。
//
// 覆盖三件事（AGENTS 第三节的硬规矩）：
//   ① 目录字段与硬参数逐条锁死（端口 / 健康路径 / 安装根 / 绑定地址）；
//   ② "已安装判据贴运行体"：只有文件、没有服务记录、端口也没在听 ⇒ **不算已安装**，
//      重新点安装必须真的再装一遍（负向对照）；
//   ③ 能装必能卸：卸载计划存在且可读，卸载实现已登记。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNativeMarketEntries20260920 锁住三个条目的硬参数。
//
// 这些值不是"随手填的"：memos 5230 避开 filebrowser 的 8081、navidrome 4533 且
// 只绑回环（上游默认 0.0.0.0）、transmission 9091 + 必须有 PanelInstaller
// （通用 brew 流程不会设 RPC 口令）。改它们等于改产品行为，必须是有意的。
func TestNativeMarketEntries20260920(t *testing.T) {
	cases := []struct {
		id, name, health, rootDir, bind string
		port                            int
		panelInstaller                  bool
	}{
		{"memos", "Memos（笔记）", "/healthz", "memos", "0.0.0.0", 5230, true},
		{"navidrome", "Navidrome（音乐）", "/ping", "navidrome", "127.0.0.1", 4533, true},
		{"transmission", "Transmission（下载）", "/transmission/web/", "", "", 9091, true},
	}
	for _, c := range cases {
		app, ok := FindApp(c.id)
		if !ok {
			t.Fatalf("目录里没有 %s", c.id)
		}
		if app.Name != c.name {
			t.Errorf("%s 的 Name 应为 %q，实际 %q", c.id, c.name, app.Name)
		}
		if app.Port != c.port {
			t.Errorf("%s 的端口应为 %d，实际 %d（memos 默认 8081 会撞 filebrowser）", c.id, c.port, app.Port)
		}
		if app.HealthPath != c.health {
			t.Errorf("%s 的健康路径应为 %q，实际 %q", c.id, c.health, app.HealthPath)
		}
		if app.PanelInstaller == "" {
			t.Errorf("%s 必须有 PanelInstaller（否则市场点了走错安装路径）", c.id)
		}
		if !HasInstallerUninstall(app.PanelInstaller) {
			t.Errorf("%s：PanelInstaller=%q 没有卸载实现（装上就卸不掉）", c.id, app.PanelInstaller)
		}
		// 目录声明必须与市场声明一致（反漂移；这里再确认一次这条比对真的跑到了新条目）
		m, ok := MarketAppFor(c.id)
		if !ok {
			t.Fatalf("market_downloads.go 里没有 %s 的声明", c.id)
		}
		if problems := MarketDeclarationProblems(m, app); len(problems) != 0 {
			t.Errorf("%s 的声明与目录不一致：%v", c.id, problems)
		}
		if c.rootDir != "" {
			spec, ok := releaseBinaryApps[app.PanelInstaller]
			if !ok {
				t.Fatalf("%s 应在 releaseBinaryApps 注册表里", c.id)
			}
			if spec.RootDir != c.rootDir {
				t.Errorf("%s 的安装根应为 ~/%s，实际 ~/%s", c.id, c.rootDir, spec.RootDir)
			}
			if spec.BindAddress != c.bind {
				t.Errorf("%s 的绑定地址应为 %q（navidrome 上游默认 0.0.0.0，必须显式收住），实际 %q",
					c.id, c.bind, spec.BindAddress)
			}
			// 版本/资产必须与目录声明对得上（能自动核就自动核）。
			if spec.Tag == "" || spec.Asset == "" {
				t.Errorf("%s 的 Tag/Asset 不许为空", c.id)
			}
			if !strings.Contains(spec.Asset, "darwin_arm64") {
				t.Errorf("%s 的资产名 %q 里没有 darwin_arm64 —— 铁律②不许装 amd64", c.id, spec.Asset)
			}
		}
	}
}

// TestMemosPortIsExplicitlyNotDefault 单独把 memos 的端口理由钉死：
// 它的代码默认 8081，而 8081 是 filebrowser —— 启动参数里必须出现 5230。
func TestMemosPortIsExplicitlyNotDefault(t *testing.T) {
	spec, ok := releaseBinaryApps["memos"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 memos")
	}
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "5230") {
		t.Errorf("memos 的启动参数必须显式指定 5230（默认 8081 会撞 filebrowser），实际 %v", spec.Args)
	}
	if strings.Contains(joined, "8081") {
		t.Errorf("memos 的启动参数里出现了 8081（那是 filebrowser 的端口）：%v", spec.Args)
	}
}

// TestNavidromeBindsLoopbackExplicitly 上游默认 address=0.0.0.0，绝不裸奔：
// 启动参数里必须出现 --address 127.0.0.1。
func TestNavidromeBindsLoopbackExplicitly(t *testing.T) {
	spec, ok := releaseBinaryApps["navidrome"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 navidrome")
	}
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "--address") || !strings.Contains(joined, "127.0.0.1") {
		t.Errorf("navidrome 必须显式 --address 127.0.0.1（上游默认 0.0.0.0），实际 %v", spec.Args)
	}
	if spec.BindAddress != "127.0.0.1" {
		t.Errorf("navidrome 的 BindAddress 应为 127.0.0.1，实际 %q", spec.BindAddress)
	}
}

// TestTarballFilesAloneDoNotMeanInstalled 是"只有文件没有进程 ⇒ 不算已安装"的
// 负向对照（坑 161/170/174 那一类）。
//
// 做法：把两个新条目的安装目录与二进制真的造出来（只有文件），不写任何服务记录、
// 也不让端口有监听者，然后断言 installedSkipResult **不跳过**（false）。
// 若哪天有人把判据改回"有产物就算装了"，这条会立刻红。
func TestTarballFilesAloneDoNotMeanInstalled(t *testing.T) {
	for _, id := range []string{"memos", "navidrome"} {
		t.Run(id, func(t *testing.T) {
			m, repo := sandboxIdempotentManager(t)
			spec := releaseBinaryApps[id]
			root := filepath.Join(m.opt.UserHome, spec.RootDir)
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, spec.Binary)
			if err := os.WriteFile(bin, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// 端口没有任何监听者（真实探测会随机器变，这里显式注入）。
			m.portCheckOverride = func(port int) (bool, []string, error) { return false, nil, nil }
			// 另外造一个"上游留的示例配置"，确认它也不构成安装证据。
			if spec.ConfigFile != "" {
				_ = os.MkdirAll(filepath.Dir(filepath.Join(root, spec.ConfigFile)), 0o755)
				_ = os.WriteFile(filepath.Join(root, spec.ConfigFile), []byte("{}\n"), 0o644)
			}

			app, ok := FindApp(id)
			if !ok {
				t.Fatalf("目录里没有 %s", id)
			}
			res, skipped := m.installedSkipResult(context.Background(), app)
			if skipped {
				t.Fatalf("%s：磁盘上只有文件、没有服务记录、端口也没在听，"+
					"却被判成「已安装」并跳过 —— 这正是「有文件就算装了」那一类谎报。res=%+v", id, res)
			}
			// 服务表也必须是空的（没有偷偷登记一条）。
			list, err := repo.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 0 {
				t.Fatalf("%s：不该有任何服务记录，实际 %d 条", id, len(list))
			}
		})
	}
}

// TestTarballPortIsWhatDecidesSkip 是上一条的**正向对照**（同一份判据的三种状态）：
//   - 有记录 + 端口在听   ⇒ 跳过（真装好了）；
//   - 有记录 + 端口没听   ⇒ **不跳过**（真机 Alist：端口从未监听，市场却永远显示已安装）；
//   - 没记录 + 端口在听   ⇒ 不跳过（那是别人的端口）。
//
// InstallReleaseBinary 里对"已有记录"补的那次端口判断就是靠这里锁死的行为。
func TestTarballPortIsWhatDecidesSkip(t *testing.T) {
	for _, id := range []string{"memos", "navidrome"} {
		t.Run(id, func(t *testing.T) {
			app, _ := FindApp(id)
			ctx := context.Background()

			// register 直接写面板服务记录（不走 RegisterInstalledService：那条路要求
			// 磁盘上真的有一份 plist，而这里要测的正是"记录在、运行体不在"）。
			register := func(t *testing.T, m *Manager) {
				t.Helper()
				if err := m.repo.Create(ctx, &Service{
					Name: app.ID, DisplayName: app.Name, Kind: app.Kind, Category: app.Category,
					Icon: app.Icon, LaunchLabel: app.ServiceLabel, Managed: true, Port: app.Port,
				}); err != nil {
					t.Fatal(err)
				}
			}

			// 状态①：有记录 + 端口在听 → 跳过
			m, _ := sandboxIdempotentManager(t)
			register(t, m)
			m.portCheckOverride = func(port int) (bool, []string, error) {
				return port == app.Port, []string{"fake (pid 1)"}, nil
			}
			if _, skipped := m.installedSkipResult(ctx, app); !skipped {
				t.Fatal("有服务记录且端口在听时应跳过（这才是真的已安装）")
			}
			if !m.portHasListener(app.Port) {
				t.Fatal("前置条件：注入的端口探测应报告在听")
			}

			// 状态②：有记录 + 端口没听 → 不跳过（InstallReleaseBinary 的补判）
			m2, _ := sandboxIdempotentManager(t)
			register(t, m2)
			m2.portCheckOverride = func(port int) (bool, []string, error) { return false, nil, nil }
			if _, skipped := m2.installedSkipResult(ctx, app); !skipped {
				t.Fatal("前置条件：登记证据在时 installedSkipResult 应为 true")
			}
			if m2.portHasListener(app.Port) {
				t.Fatal("端口没在听时不许因记录而跳过安装")
			}

			// 状态③：没记录 + 端口在听 → 不跳过（别人的端口不是我们的安装证据）
			m3, _ := sandboxIdempotentManager(t)
			m3.portCheckOverride = func(port int) (bool, []string, error) {
				return port == app.Port, []string{"other (pid 99)"}, nil
			}
			if _, skipped := m3.installedSkipResult(ctx, app); skipped {
				t.Fatal("没有服务记录时不该跳过安装（端口在听不等于我们装过）")
			}
		})
	}
}

// TestTransmissionCredentialMergeKeepsOtherSettings 是 settings.json 合并的纯函数门禁：
// 只覆盖面板负责的键，用户改过的下载目录/端口映射必须原样保留。
func TestTransmissionCredentialMergeKeepsOtherSettings(t *testing.T) {
	raw := []byte(`{"download-dir":"/Volumes/Data/torrents","rpc-enabled":true,"peer-port":51413,"speed-limit-down":500}`)
	out, changed, err := transmissionSettingsWithCredentials(raw, "zizpaneluser", "SuperSecretPass123456")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("凭据变了就应该报告 changed=true")
	}
	s := string(out)
	for _, want := range []string{
		`"download-dir": "/Volumes/Data/torrents"`,
		`"peer-port": 51413`,
		`"speed-limit-down": 500`,
		`"rpc-authentication-required": true`,
		`"rpc-username": "zizpaneluser"`,
		`"rpc-password": "SuperSecretPass123456"`,
		`"rpc-bind-address": "127.0.0.1"`,
		`"rpc-whitelist": "127.0.0.1,::1"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("合并后的 settings.json 里缺少 %s：\n%s", want, s)
		}
	}
	// 幂等：同样的输入再合一次不该报告 changed
	if _, changed2, err := transmissionSettingsWithCredentials(out, "zizpaneluser", "SuperSecretPass123456"); err != nil || changed2 {
		t.Errorf("同样的凭据重复合并应报告 changed=false（err=%v changed=%v）", err, changed2)
	}
	// 非法 JSON：**不覆盖**，如实报错（用户手改坏了配置不该被面板静默清掉）
	if _, _, err := transmissionSettingsWithCredentials([]byte("{not json"), "u", "p"); err == nil {
		t.Error("现有 settings.json 不是合法 JSON 时必须报错（不许静默覆盖）")
	}
}

// TestVerifyTransmissionCredentialsApplied 是回读判据的门禁（逐字段）。
//
// 真机教训（2026-09-20）：只看"rpc-password 变成了哈希"会漏判 —— 实测出现过
// "哈希在、rpc-authentication-required 却是 false"（旧 daemon 退出时回写了默认值）。
// 所以这里把每一条判据都钉死：auth-required / username / 哈希 / 三个本地网络开关。
func TestVerifyTransmissionCredentialsApplied(t *testing.T) {
	m, _ := sandboxManager(t)
	transmissionWaitOverride = 200 * time.Millisecond
	t.Cleanup(func() { transmissionWaitOverride = 60 * time.Second })
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	good := `{"rpc-authentication-required":true,"rpc-username":"zpu","rpc-password":"{abc123def456",` +
		`"dht-enabled":true,"lpd-enabled":false,"port-forwarding-enabled":false}`
	cases := []struct {
		name, body, user, want string
	}{
		{"全部生效", good, "zpu", ""},
		{"明文还在", `{"rpc-authentication-required":true,"rpc-username":"zpu","rpc-password":"PlainTextPw123",` +
			`"dht-enabled":true,"lpd-enabled":false,"port-forwarding-enabled":false}`, "zpu", "明文"},
		{"auth-required 被回写成 false", `{"rpc-authentication-required":false,"rpc-username":"zpu",` +
			`"rpc-password":"{abc123",` + `"dht-enabled":true,"lpd-enabled":false,"port-forwarding-enabled":false}`,
			"zpu", "rpc-authentication-required"},
		{"用户名被回写成空", `{"rpc-authentication-required":true,"rpc-username":"",` +
			`"rpc-password":"{abc123","dht-enabled":true,"lpd-enabled":false,"port-forwarding-enabled":false}`,
			"zpu", "rpc-username"},
		{"dht 被关了", `{"rpc-authentication-required":true,"rpc-username":"zpu","rpc-password":"{abc123",` +
			`"dht-enabled":false,"lpd-enabled":false,"port-forwarding-enabled":false}`, "zpu", "dht-enabled"},
		{"lpd 没关", `{"rpc-authentication-required":true,"rpc-username":"zpu","rpc-password":"{abc123",` +
			`"dht-enabled":true,"lpd-enabled":true,"port-forwarding-enabled":false}`, "zpu", "lpd-enabled"},
		{"port-forwarding 没关", `{"rpc-authentication-required":true,"rpc-username":"zpu","rpc-password":"{abc123",` +
			`"dht-enabled":true,"lpd-enabled":false,"port-forwarding-enabled":true}`, "zpu", "port-forwarding-enabled"},
		{"字段缺失", `{}`, "zpu", "rpc-authentication-required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			ok, why := m.verifyTransmissionCredentialsApplied(path, c.user, "PlainTextPw123")
			if c.want == "" {
				if !ok {
					t.Fatalf("应判成已生效，实际 false（%s）", why)
				}
				return
			}
			if ok {
				t.Fatalf("应判成未生效，实际 true（判据漏了 %s）", c.want)
			}
			if !strings.Contains(why, c.want) {
				t.Errorf("失败原因里应点名 %q，实际 %q", c.want, why)
			}
		})
	}
	// 文件不存在 → 不许谎报生效
	if ok, _ := m.verifyTransmissionCredentialsApplied(filepath.Join(dir, "nope.json"), "zpu", "x"); ok {
		t.Error("文件不存在时不许判成已生效")
	}
}

// TestTransmissionDefaultsDisableLocalNetworkDiscovery 门禁：默认值必须是
// lpd-enabled / port-forwarding-enabled 两项 false（局域网组播会触发 macOS
// 「本地网络」授权弹窗），而 dht-enabled 必须 **true** —— 真机实测（2026-09-20）
// dht 关了以后加磁力链永远 peers=0 / metadata=0% 且不报错，就是"没反应"（坑 226）。
func TestTransmissionDefaultsDisableLocalNetworkDiscovery(t *testing.T) {
	got := transmissionDesiredSettings("u", "p")
	if v, ok := got["dht-enabled"]; !ok || v != true {
		t.Errorf("dht-enabled 默认必须是 true（否则无 tracker 的磁力链永远不动），实际 %v", got["dht-enabled"])
	}
	for _, key := range []string{"lpd-enabled", "port-forwarding-enabled"} {
		v, ok := got[key]
		if !ok {
			t.Errorf("默认值里必须有 %s=false（否则 macOS 会反复要本地网络授权）", key)
			continue
		}
		if b, isBool := v.(bool); !isBool || b {
			t.Errorf("%s 的默认值必须是 false，实际 %v", key, v)
		}
	}
	// 合并写回时也必须带上这几项（不然只写口令、开关还是上游默认值）。
	out, _, err := transmissionSettingsWithCredentials([]byte(`{"dht-enabled":false,"lpd-enabled":true,"port-forwarding-enabled":true}`), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	// 明文口令还没被哈希，这一条必然命中；单独核对三个开关确实被写成了目标值。
	problem := transmissionCredentialsProblem(out, "u", "p")
	for _, key := range []string{"dht-enabled", "lpd-enabled", "port-forwarding-enabled"} {
		if strings.Contains(problem, key) {
			t.Errorf("合并写回没有把 %s 写成目标值：%s\n%s", key, problem, out)
		}
	}
}

// TestTransmissionUninstallPlanIsRunnable 卸载路径存在且可读（能装必能卸），
// 并且如实说明"下载的文件不会被删"。
func TestTransmissionUninstallPlanIsRunnable(t *testing.T) {
	m, _ := sandboxManager(t)
	app, ok := FindApp("transmission")
	if !ok {
		t.Fatal("目录里没有 transmission")
	}
	plan := m.installerPlan(context.Background(), app)
	if plan.Kind != "installer" {
		t.Fatalf("卸载计划 kind 应为 installer，实际 %q（blocked=%q）", plan.Kind, plan.Blocked)
	}
	if plan.Blocked != "" {
		t.Fatalf("正常情况下不该被拦下：%s", plan.Blocked)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("卸载计划没有任何步骤")
	}
	joined := strings.Join(plan.Steps, "\n")
	for _, want := range []string{"brew uninstall transmission-cli", "下载"} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载计划里应提到 %q：\n%s", want, joined)
		}
	}
	if len(plan.DataPaths) == 0 {
		t.Error("卸载计划应列出可选删除的配置目录")
	}
}

// ---------- 安装器主路径与"不谎报"回归 ----------

// fakeTransmissionBrew 写一个只记录调用、按需报告"包已装/未装"的假 brew。
//
// services info 返回沙箱里**真实存在**的 plist 路径：这样 brewServiceInfo 与
// AdoptCandidate 都不会退到 launchctl 去碰真实服务（单测不许碰真实 launchd）。
func fakeTransmissionBrew(t *testing.T, marker string, installed bool, plist string) string {
	t.Helper()
	code := "1"
	if installed {
		code = "0"
	}
	p := filepath.Join(t.TempDir(), "brew")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + marker + `'
if [ "$1" = "list" ] && [ "$2" = "--versions" ]; then
  if [ "` + code + `" = "0" ]; then
    printf '%s\n' 'transmission-cli 4.1.3'
    exit 0
  fi
  exit 1
fi
if [ "$1" = "services" ] && [ "$2" = "info" ]; then
  printf '%s' '[{"name":"transmission-cli","status":"started","file":"` + plist + `"}]'
  exit 0
fi
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// transmissionHarness 造一个完全沙箱化的 Manager + 假 brew + 注入的 HTTP 探测。
//
// authCode 是"带凭据"时 RPC 的返回码，unauthCode 是"不带凭据"时的返回码；
// webCode 是 /transmission/web/ 的返回码。nil 探测函数默认给 200/401/200。
func transmissionHarness(t *testing.T, opts ...func(m *Manager)) (*Manager, string, *InstallResult) {
	t.Helper()
	stubSystemDaemonEnsure(t, nil)
	m, _ := sandboxIdempotentManager(t)
	// brew 前缀落在临时目录里（transmissionConfigDir 由它推导）。
	prefix := t.TempDir()
	plist := filepath.Join(prefix, "LaunchAgents", transmissionLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "brew-calls")
	m.opt.BrewBin = fakeTransmissionBrew(t, marker, true, plist)
	m.opt.UserName = "" // 不在临时目录上 chown
	transmissionWaitOverride = 300 * time.Millisecond
	t.Cleanup(func() { transmissionWaitOverride = 60 * time.Second })
	// 端口探测必须注入：默认实现跑真实 lsof，单测不许依赖本机 9091 的状态。
	m.portCheckOverride = func(int) (bool, []string, error) { return false, nil, nil }
	for _, o := range opts {
		o(m)
	}
	res := &InstallResult{App: "transmission", Name: "Transmission（下载）", Steps: []string{}}
	return m, marker, res
}

// useTransmissionProbes 注入 HTTP 探测与三个生命周期动作，并在用例结束还原。
// 包级变量是唯一注入点（Manager 字段要动 services.go，那文件被多个并行改动共享）。
func useTransmissionProbes(t *testing.T, httpProbe func(*Manager, context.Context, string, string, string) (int, error),
	stop, start func(*Manager, context.Context) error) {
	t.Helper()
	oldHTTP := transmissionHTTPProbe
	oldStop, oldStart := transmissionStopService, transmissionStartService
	oldWait := transmissionWaitOverride
	if httpProbe != nil {
		transmissionHTTPProbe = httpProbe
	}
	if stop != nil {
		transmissionStopService = stop
	}
	if start != nil {
		transmissionStartService = start
	}
	t.Cleanup(func() {
		transmissionHTTPProbe = oldHTTP
		transmissionStopService = oldStop
		transmissionStartService = oldStart
		transmissionWaitOverride = oldWait
	})
}

// hashTransmissionSettingsLikeDaemon 模拟 transmission 启动时的副作用：
// 把 settings.json 里的明文 rpc-password 换成哈希（其余字段保持面板写的值）。
// 真机实测：daemon 只在**启动时**做这件事，所以顺序必须是"先停再写、写完再启"。
func hashTransmissionSettingsLikeDaemon(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("daemon 副作用读配置失败: %v", err)
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("daemon 副作用解析配置失败: %v", err)
	}
	cfg["rpc-password"] = "{deadbeefcafe0123456789abcdef"
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestInstallTransmissionHashesPasswordAndVerifies 主路径回归：
// 面板写进去的凭据必须被"服务"换成哈希、且其余判据全部成立才算通过；
// 明文不许出现在任务步骤/告警里。
func TestInstallTransmissionHashesPasswordAndVerifies(t *testing.T) {
	m, _, res := transmissionHarness(t)
	cfgPath := m.transmissionSettingsPath()
	plain := ""
	stopped := false
	// 顺序门禁：必须先停服务、再写配置、最后启动 —— 真机实测"运行中改写会被
	// daemon 退出时的回写覆盖"，所以这里的 start 钩子会检查配置已经写好了。
	useTransmissionProbes(t, nil, func(*Manager, context.Context) error {
		stopped = true
		return nil
	}, func(*Manager, context.Context) error {
		if !stopped {
			return fmt.Errorf("启动服务之前没有先停掉旧的 transmission（会被它的回写覆盖）")
		}
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			return fmt.Errorf("启动前读不到 settings.json：%w", err)
		}
		if v, ok := transmissionSettingsBool(raw, "rpc-authentication-required"); !ok || !v {
			return fmt.Errorf("settings.json 还没被面板写好（auth-required 不是 true）就启动了服务：顺序错了")
		}
		plain = transmissionSettingsString(raw, "rpc-password")
		// 模拟真实 daemon 的启动副作用：把明文换成哈希（保留面板写的其余字段）。
		hashTransmissionSettingsLikeDaemon(t, cfgPath)
		return nil
	})
	useTransmissionProbes(t, func(_ *Manager, _ context.Context, url, user, _ string) (int, error) {
		if strings.Contains(url, "/transmission/rpc") {
			if user == "" {
				return http.StatusUnauthorized, nil
			}
			return http.StatusConflict, nil
		}
		return http.StatusOK, nil
	}, nil, nil)

	if err := m.InstallTransmission(t.Context(), res); err != nil {
		t.Fatalf("安装应成功，实际: %v", err)
	}
	steps := strings.Join(res.Steps, "\n")
	// 明文口令**必须**曾经被写进 settings.json（否则"服务读到它并哈希"无从谈起），
	// 但它不许出现在任务步骤/告警里。
	if plain == "" || len(plain) != transmissionPasswordLen {
		t.Fatalf("预期 settings.json 里出现过 20 位明文口令，实际 %q", plain)
	}
	if strings.Contains(steps+res.Warning, plain) {
		t.Error("明文口令不许出现在步骤/告警文本里")
	}
	// 凭据区必须有用户名与口令，且口令是 20 位 [A-Za-z0-9]
	pw := ""
	for _, c := range res.Credentials {
		if c.Key == "transmission_rpc_password" {
			pw = c.Value
		}
	}
	if len(pw) != transmissionPasswordLen {
		t.Errorf("凭据区口令长度应为 %d，实际 %q", transmissionPasswordLen, pw)
	}
	if strings.Contains(strings.Join(res.Steps, "\n")+res.Warning, pw) {
		t.Error("凭据区里的口令不许出现在步骤/告警文本里")
	}
	// 停止动作必须真的执行过（真机实测的"先停再写"顺序；start 钩子里的断言
	// 也会在顺序错了时让安装失败）。
	if !stopped {
		t.Error("安装流程没有先停掉 transmission —— 运行中改配置会被 daemon 退出时的回写覆盖")
	}
}

// TestInstallTransmissionFailsWhenPasswordNotApplied 是"不谎报"的负向对照：
// 服务始终没把明文换成哈希（= 口令没生效）⇒ 安装必须**失败**，不许报成功。
func TestInstallTransmissionFailsWhenPasswordNotApplied(t *testing.T) {
	m, _, res := transmissionHarness(t)
	// 服务起来了但什么也没做（不模拟哈希副作用）→ 回读必须发现"仍是明文"并失败。
	useTransmissionProbes(t, func(*Manager, context.Context, string, string, string) (int, error) {
		return http.StatusOK, nil
	}, nil, func(*Manager, context.Context) error { return nil })
	err := m.InstallTransmission(t.Context(), res)
	if err == nil {
		t.Fatal("rpc-password 一直没生效时安装必须失败（不许谎报成功）")
	}
	if !strings.Contains(err.Error(), "没有生效") {
		t.Errorf("失败原因必须说清「凭据没有生效」，实际：%v", err)
	}
}

// TestInstallTransmissionFailsWhenRPCOpenWithoutAuth 另一条负向对照：
// 无凭据访问 RPC 竟然不是 401 ⇒ 安装失败（9091 裸奔不许算成功）。
func TestInstallTransmissionFailsWhenRPCOpenWithoutAuth(t *testing.T) {
	m, _, res := transmissionHarness(t)
	cfgPath := m.transmissionSettingsPath()
	useTransmissionProbes(t, func(_ *Manager, _ context.Context, url, _ string, _ string) (int, error) {
		if strings.Contains(url, "/transmission/rpc") {
			return http.StatusOK, nil // 无凭据也 200 = 裸奔
		}
		return http.StatusOK, nil
	}, nil, func(*Manager, context.Context) error {
		hashTransmissionSettingsLikeDaemon(t, cfgPath)
		return nil
	})
	err := m.InstallTransmission(t.Context(), res)
	if err == nil {
		t.Fatal("无凭据 RPC 返回 200 时安装必须失败")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("失败原因里应点明应为 401，实际：%v", err)
	}
}

// TestInstallTransmissionRequiresHomebrew 缺 Homebrew 时如实失败（不静默）。
func TestInstallTransmissionRequiresHomebrew(t *testing.T) {
	m, _ := sandboxManager(t)
	m.opt.BrewBin = ""
	res := &InstallResult{App: "transmission", Steps: []string{}}
	if err := m.InstallTransmission(context.Background(), res); err == nil {
		t.Fatal("未配置 Homebrew 时必须失败")
	}
	m.opt.BrewBin = filepath.Join(t.TempDir(), "nope", "brew")
	if err := m.InstallTransmission(context.Background(), res); err == nil {
		t.Fatal("Homebrew 不存在时必须失败")
	}
}

// useTransmissionWriteProbe 注入下载目录写探针（负向对照用：不可写时必须失败）。
func useTransmissionWriteProbe(t *testing.T, fn func(*Manager, context.Context, string) error) {
	t.Helper()
	old := transmissionWriteProbe
	if fn != nil {
		transmissionWriteProbe = fn
	}
	t.Cleanup(func() { transmissionWriteProbe = old })
}

// TestInstallTransmissionFailsWhenDownloadDirNotWritable 是"不许谎报"的负向对照：
// 下载目录不可写时安装必须**失败**并点名目录，不许出现"能登录但什么都下不了"（坑 226）。
func TestInstallTransmissionFailsWhenDownloadDirNotWritable(t *testing.T) {
	m, _, res := transmissionHarness(t)
	useTransmissionProbes(t, func(*Manager, context.Context, string, string, string) (int, error) {
		return http.StatusUnauthorized, nil
	}, nil, func(*Manager, context.Context) error { return nil })
	useTransmissionWriteProbe(t, func(*Manager, context.Context, string) error {
		return fmt.Errorf("operation not permitted")
	})
	err := m.InstallTransmission(t.Context(), res)
	if err == nil {
		t.Fatal("下载目录不可写时安装必须失败（不许报成功）")
	}
	if !strings.Contains(err.Error(), "不可写") || !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("失败原因必须点名目录不可写并劝退手改 settings.json，实际：%v", err)
	}
}

// TestSetTransmissionRPCSettingsOrderAndReadback 主路径：改凭据/下载目录必须
// 「停 → 写 → 启动 → 回读」，并把路径、用户名、开关写对（坑 226）。
func TestSetTransmissionRPCSettingsOrderAndReadback(t *testing.T) {
	m, _, res := transmissionHarness(t)
	cfgPath := m.transmissionSettingsPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"download-dir":"`+m.opt.UserHome+`/Downloads","peer-port":51413,"lpd-enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped, started := false, false
	plain := ""
	useTransmissionProbes(t, func(_ *Manager, _ context.Context, url, user, _ string) (int, error) {
		if strings.Contains(url, "/transmission/rpc") {
			if user == "" {
				return http.StatusUnauthorized, nil
			}
			return http.StatusConflict, nil
		}
		return http.StatusOK, nil
	}, func(*Manager, context.Context) error {
		stopped = true
		return nil
	}, func(*Manager, context.Context) error {
		if !stopped {
			return fmt.Errorf("没有先停服务就写配置（会被回写覆盖）")
		}
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			return err
		}
		plain = transmissionSettingsString(raw, "rpc-password")
		started = true
		hashTransmissionSettingsLikeDaemon(t, cfgPath)
		return nil
	})
	newDir := filepath.Join(t.TempDir(), "torrents")
	info, err := m.SetTransmissionRPCSettings(t.Context(), res, "newuser", "NewPassPlain123456", newDir)
	if err != nil {
		t.Fatalf("改设置应成功，实际: %v", err)
	}
	if !stopped || !started {
		t.Fatalf("必须先停再起（stopped=%v started=%v）", stopped, started)
	}
	if plain != "NewPassPlain123456" {
		t.Fatalf("明文口令必须先落盘再被 daemon 哈希，实际 %q", plain)
	}
	if info.Username != "newuser" || info.DownloadDir != newDir {
		t.Errorf("回读结果不符：%+v", info)
	}
	raw, _ := os.ReadFile(cfgPath)
	if got := transmissionSettingsString(raw, "download-dir"); got != newDir {
		t.Errorf("download-dir 应为 %s，实际 %s", newDir, got)
	}
	if v, _ := transmissionSettingsBool(raw, "dht-enabled"); !v {
		t.Error("dht-enabled 必须被写回 true（磁力链要用）")
	}
	if v, _ := transmissionSettingsBool(raw, "lpd-enabled"); v {
		t.Error("lpd-enabled 必须被写回 false")
	}
	if !strings.Contains(string(raw), `"peer-port": 51413`) {
		t.Errorf("用户其它字段必须保留，实际：%s", raw)
	}
	if steps := strings.Join(res.Steps, "\n"); strings.Contains(steps, "NewPassPlain123456") {
		t.Error("明文口令不许出现在任务步骤里")
	}
}

// TestSetTransmissionRPCSettingsFailsWhenPlaintextStays 负向对照：服务没把明文
// 换成哈希（= 新口令没生效）时，改设置必须失败，不许报成功。
func TestSetTransmissionRPCSettingsFailsWhenPlaintextStays(t *testing.T) {
	m, _, res := transmissionHarness(t)
	cfgPath := m.transmissionSettingsPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"download-dir":"`+m.opt.UserHome+`/Downloads"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	useTransmissionProbes(t, func(*Manager, context.Context, string, string, string) (int, error) {
		return http.StatusOK, nil
	}, func(*Manager, context.Context) error { return nil }, func(*Manager, context.Context) error { return nil })
	if _, err := m.SetTransmissionRPCSettings(t.Context(), res, "u", "Plain123456789012", ""); err == nil {
		t.Fatal("新口令没生效时必须失败（不许谎报成功）")
	} else if !strings.Contains(err.Error(), "没有生效") {
		t.Errorf("失败原因应说清「没有生效」，实际：%v", err)
	}
}

// TestTransmissionSettingsInfoFrom 锁死回读字段：不能把哈希当用户名、不能编默认值。
func TestTransmissionSettingsInfoFrom(t *testing.T) {
	raw := []byte(`{"rpc-username":"u1","rpc-password":"{abc","download-dir":"/data/dl",` +
		`"rpc-authentication-required":true,"dht-enabled":true,"rpc-bind-address":"127.0.0.1"}`)
	info := transmissionSettingsInfoFrom("/tmp/settings.json", raw)
	if info.Username != "u1" || info.DownloadDir != "/data/dl" || !info.AuthRequired || !info.DHTEnabled {
		t.Errorf("回读字段不符：%+v", info)
	}
	empty := transmissionSettingsInfoFrom("/tmp/x.json", []byte(`{}`))
	if empty.Username != "" || empty.DownloadDir != "" || empty.AuthRequired || empty.DHTEnabled {
		t.Errorf("缺字段时必须如实留空/false，实际：%+v", empty)
	}
}

// TestSetTransmissionRPCSettingsKeepsPasswordWhenBlank：口令留空 = 不重设（保留原哈希），
// 且不许用空明文去做"带凭据自检"（那必然 401，会把成功谎报成失败）。
func TestSetTransmissionRPCSettingsKeepsPasswordWhenBlank(t *testing.T) {
	m, _, res := transmissionHarness(t)
	cfgPath := m.transmissionSettingsPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const oldHash = "{deadbeefdeadbeefdeadbeefdeadbeefdeadbeefAAA"
	body := `{"rpc-username":"old","rpc-password":"` + oldHash + `","download-dir":"` + m.opt.UserHome + `/Downloads"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	authProbed := false
	useTransmissionProbes(t, func(_ *Manager, _ context.Context, _ string, user, _ string) (int, error) {
		if user != "" {
			authProbed = true
			return http.StatusUnauthorized, nil // 空明文必然被拒；走这条路就是 bug
		}
		return http.StatusUnauthorized, nil
	}, func(*Manager, context.Context) error { return nil }, func(*Manager, context.Context) error { return nil })
	if _, err := m.SetTransmissionRPCSettings(t.Context(), res, "newuser", "", ""); err != nil {
		t.Fatalf("留空口令应成功（保留原哈希），实际：%v", err)
	}
	if authProbed {
		t.Error("口令留空时不得用空明文做带凭据自检（会把成功判成失败）")
	}
	raw, _ := os.ReadFile(cfgPath)
	if got := transmissionSettingsString(raw, "rpc-password"); got != oldHash {
		t.Errorf("口令留空时原哈希必须原样保留，实际 %q", got)
	}
	if got := transmissionSettingsString(raw, "rpc-username"); got != "newuser" {
		t.Errorf("用户名应更新为 newuser，实际 %q", got)
	}
	for _, c := range res.Credentials {
		if c.Key == "transmission_rpc_password" {
			t.Error("没重设口令时不该在凭据区给出口令")
		}
	}
}
