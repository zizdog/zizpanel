package services

// ============================================================================
//  Syncthing 安装器的回归测试
//
//  三条不能动的约定：
//    1. config.xml 补丁只改 <address> / sendBasicAuthPrompt / <user> /
//       <password> 四处，其余元素**逐字节保留**（绝不整份 XML 重写），
//       且对"已经配置好"的配置幂等（不重置凭据）；
//    2. 凭据不可用时**拒绝**把界面开到局域网（哪怕只改地址也不行）；
//    3. 面板生成的口令只进 InstallResult.Credentials，绝不进步骤 / 错误 /
//       日志文本。
//
//  单测不许碰真实 brew / Syncthing / 家目录：brew 用临时目录里的假脚本，
//  两段轮询的 HTTP 探针与超时都用包级注入点替换（见 syncthing.go 的说明）。
//  这些测试**不能并行**（它们改包级变量），与 ready_test.go 同一约定。
// ============================================================================

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// syncthingTestConfig 是一份**贴近真实默认值**的 config.xml：
//   - <gui> 默认只绑 127.0.0.1:8384、sendBasicAuthPrompt="false"、
//     <user>/<password> 为空（真机实测的默认形态）；
//   - 刻意带一个 <ldap><address> —— Syncthing 的配置里确实有两个
//     <address>，补丁必须只动 <gui> 里的那一个；
//   - 带注释与紧凑/带属性的写法，用来证明"没被整份重写"。
const syncthingTestConfig = `<?xml version="1.0" encoding="UTF-8"?>
<configuration version="37">
    <!-- 面板补丁必须逐字节保留这些元素 -->
    <folder id="default" label="Default Folder" path="/Users/x/Sync" type="sendreceive" rescanIntervalS="3600">
        <device id="AAAA" introducedBy=""></device>
    </folder>
    <device id="AAAA" name="mac" compression="metadata" introducer="false"></device>
    <gui enabled="true" tls="false" debugging="false" sendBasicAuthPrompt="false">
        <address>127.0.0.1:8384</address>
        <apikey>abcdef0123456789</apikey>
        <theme>default</theme>
        <user></user>
        <password></password>
    </gui>
    <ldap>
        <address></address>
        <bindDN></bindDN>
    </ldap>
    <options>
        <listenAddress>default</listenAddress>
    </options>
</configuration>
`

// syncthingAlreadyConfigured 复刻"用户自己配置过界面凭据"的配置。
const syncthingAlreadyConfigured = `<configuration version="37">
    <gui enabled="true" tls="false" sendBasicAuthPrompt="true">
        <address>0.0.0.0:8384</address>
        <user>alice</user>
        <password>$2a$10$abcdefghijklmnopqrstuvABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789</password>
    </gui>
</configuration>
`

// ---------- 小工具（刻意不依赖别的测试文件里的同名助手） ----------

func syncthingAlnumOnly(s string) bool {
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

func syncthingCredValue(res *InstallResult, key string) string {
	for _, c := range res.Credentials {
		if c.Key == key {
			return c.Value
		}
	}
	return ""
}

// ---------- 纯函数：补丁 ----------

// TestPatchSyncthingGUIConfigShape 把补丁的"改什么、不改什么"钉死。
func TestPatchSyncthingGUIConfigShape(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("S3cretPass0123456789a"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := patchSyncthingGUI(syncthingTestConfig, string(hash))
	if err != nil {
		t.Fatalf("补丁不该失败: %v", err)
	}
	if !changed {
		t.Fatal("默认配置只绑回环，补丁必须报告发生了改动")
	}

	// 最强断言：只做那四处定点替换，其余逐字节相同。
	want := strings.NewReplacer(
		"<address>127.0.0.1:8384</address>", "<address>"+syncthingLANAddress+"</address>",
		`sendBasicAuthPrompt="false"`, `sendBasicAuthPrompt="true"`,
		"<user></user>", "<user>"+syncthingGUIUser+"</user>",
		"<password></password>", "<password>"+string(hash)+"</password>",
	).Replace(syncthingTestConfig)
	if out != want {
		t.Errorf("补丁改动了不该改动的内容（必须逐字节保留其它元素）\n实际：\n%s\n期望：\n%s", out, want)
	}

	// 关键字段逐条复述一遍（上面那条一旦写错会一起错，这里给更可读的失败信息）
	for _, needle := range []string{
		"<address>" + syncthingLANAddress + "</address>",
		`sendBasicAuthPrompt="true"`,
		"<user>" + syncthingGUIUser + "</user>",
		"<password>" + string(hash) + "</password>",
		"<!-- 面板补丁必须逐字节保留这些元素 -->",
		"<apikey>abcdef0123456789</apikey>",
		"<address></address>", // <ldap> 里的空地址不许被改成 0.0.0.0
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("补丁结果里缺少 %q：\n%s", needle, out)
		}
	}
	// 哈希是 Syncthing v2 认得的 bcrypt
	if !syncthingBcryptRe.MatchString(string(hash)) {
		t.Errorf("生成的哈希不符合 Syncthing v2 的 bcrypt 形态：%s", hash)
	}
}

// TestPatchSyncthingGUIIsIdempotent：补丁打两次与打一次结果相同，
// 且第二次报告"没有改动"（重跑安装因此不会重置口令）。
func TestPatchSyncthingGUIIsIdempotent(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("S3cretPass0123456789a"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	first, changed, err := patchSyncthingGUI(syncthingTestConfig, string(hash))
	if err != nil || !changed {
		t.Fatalf("第一次补丁应成功且报告改动（changed=%v err=%v）", changed, err)
	}
	second, changed2, err := patchSyncthingGUI(first, string(hash))
	if err != nil {
		t.Fatalf("第二次不该失败: %v", err)
	}
	if changed2 {
		t.Error("已经是 0.0.0.0 + 有用户名，第二次不该再改（否则每次重装都churn）")
	}
	if second != first {
		t.Errorf("第二次必须原样返回\n第一次：\n%s\n第二次：\n%s", first, second)
	}
}

// TestPatchSyncthingGUIKeepsUserConfiguredCredentials：用户自己配好的
// "用户名 + 对外监听"必须原样保留（面板不重置）。
func TestPatchSyncthingGUIKeepsUserConfiguredCredentials(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("PanelPass0123456789ab"), bcrypt.DefaultCost)
	out, changed, err := patchSyncthingGUI(syncthingAlreadyConfigured, string(hash))
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if changed {
		t.Error("已有用户名 + 非回环地址时不该改动配置")
	}
	if out != syncthingAlreadyConfigured {
		t.Errorf("必须原样返回：\n%s", out)
	}
	if !strings.Contains(out, "<user>alice</user>") {
		t.Error("用户自己设置的用户名被冲掉了")
	}
	if strings.Contains(out, "PanelPass") || strings.Contains(out, string(hash)) {
		t.Error("面板把你自己的配置改掉了")
	}
}

// TestPatchSyncthingGUIRefusesUnauthenticatedLANExposure 是这个安装器
// **最重要**的一条安全约束：凭据不可用时宁可不动，也不许把界面开到局域网。
func TestPatchSyncthingGUIRefusesUnauthenticatedLANExposure(t *testing.T) {
	cases := []struct {
		name string
		hash string
	}{
		{"空口令哈希", ""},
		{"明文口令（Syncthing v2 不接受）", "S3cretPass0123456789a"},
		{"过短的哈希", "$2a$10$short"},
		{"前缀不对的哈希", "$1a$10$abcdefghijklmnopqrstuvABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed, err := patchSyncthingGUI(syncthingTestConfig, c.hash)
			if err == nil {
				t.Fatal("凭据不可用必须返回错误（否则界面会暴露却无人能认证）")
			}
			if changed {
				t.Error("拒绝时不能报告改动")
			}
			if out != syncthingTestConfig {
				t.Errorf("拒绝时必须原样返回配置（万一被误写也不能是外网监听）：\n%s", out)
			}
			if strings.Contains(out, syncthingLANAddress) {
				t.Errorf("拒绝的结果里绝不能出现 %s：\n%s", syncthingLANAddress, out)
			}
			if !strings.Contains(out, "<address>127.0.0.1:8384</address>") {
				t.Error("回环地址必须原样保留")
			}
		})
	}

	t.Run("没有 gui 段落", func(t *testing.T) {
		hash, _ := bcrypt.GenerateFromPassword([]byte("S3cretPass0123456789a"), bcrypt.DefaultCost)
		cfg := `<configuration version="37"><options><listenAddress>default</listenAddress></options></configuration>`
		out, changed, err := patchSyncthingGUI(cfg, string(hash))
		if err == nil {
			t.Fatal("找不到 <gui> 时必须报错，而不是当成改好了")
		}
		if changed || out != cfg {
			t.Error("拒绝时必须原样返回配置")
		}
	})

	t.Run("gui 里没有 address", func(t *testing.T) {
		hash, _ := bcrypt.GenerateFromPassword([]byte("S3cretPass0123456789a"), bcrypt.DefaultCost)
		cfg := `<configuration version="37"><gui enabled="true"><apikey>x</apikey></gui></configuration>`
		out, changed, err := patchSyncthingGUI(cfg, string(hash))
		if err == nil {
			t.Fatal("找不到 <address> 时必须报错（无法确认监听地址）")
		}
		if changed || out != cfg {
			t.Error("拒绝时必须原样返回配置")
		}
	})
}

// TestSyncthingAddressIsLoopback 把"哪些地址算对外"钉死。
//
// ":8384" 与 "" 是最容易误判的两个：前者 SplitHostPort 得到空 host，
// 含义是**监听所有网卡**；后者确认不了。两者都必须判成"不算回环"。
func TestSyncthingAddressIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8384", true},
		{"localhost:8384", true},
		{"[::1]:8384", true},
		{"0.0.0.0:8384", false},
		{":8384", false},
		{"", false},
		{"192.168.1.4:8384", false},
		{"syncthing.local:8384", false},
	}
	for _, c := range cases {
		if got := syncthingAddressIsLoopback(c.addr); got != c.want {
			t.Errorf("syncthingAddressIsLoopback(%q) = %v，期望 %v", c.addr, got, c.want)
		}
	}
}

// TestGenerateSyncthingSecret 锁住"只出 [A-Za-z0-9] 且长度正确"。
func TestGenerateSyncthingSecret(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		s, err := generateSyncthingSecret(syncthingPasswordLen)
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if len(s) != syncthingPasswordLen {
			t.Fatalf("长度应为 %d，实际 %d（%q）", syncthingPasswordLen, len(s), s)
		}
		if !syncthingAlnumOnly(s) {
			t.Fatalf("只允许 [A-Za-z0-9]，实际 %q", s)
		}
		if seen[s] {
			t.Fatalf("两次生成出现了相同的随机串：%q", s)
		}
		seen[s] = true
	}
	if _, err := generateSyncthingSecret(0); err == nil {
		t.Error("长度 0 应报错")
	}
}

// ---------- 安装流程（假 brew + 注入探针） ----------

// syncthingRecorder 记录两段轮询的探针结果与调用情况。
type syncthingRecorder struct {
	healthCode  int
	authCode    int
	healthCalls int
	authCalls   int
	lastUser    string
	lastPass    string
}

func (r *syncthingRecorder) health(context.Context) (int, error) {
	r.healthCalls++
	return r.healthCode, nil
}

func (r *syncthingRecorder) auth(_ context.Context, user, password string) (int, error) {
	r.authCalls++
	r.lastUser, r.lastPass = user, password
	return r.authCode, nil
}

// useSyncthingProbes 注入探针并把两段轮询的超时压到毫秒级。
//
// 必须压：真机实现等 60 秒，"超时=如实失败"这条约束不能靠人工等一分钟来验证。
func useSyncthingProbes(t *testing.T, rec *syncthingRecorder) {
	t.Helper()
	oldH, oldA := syncthingHealthProbe, syncthingAuthProbe
	oldReady, oldAuth, oldPoll := syncthingReadyTimeout, syncthingAuthTimeout, syncthingPollInterval
	syncthingHealthProbe = rec.health
	syncthingAuthProbe = rec.auth
	syncthingReadyTimeout = 80 * time.Millisecond
	syncthingAuthTimeout = 80 * time.Millisecond
	syncthingPollInterval = time.Millisecond
	t.Cleanup(func() {
		syncthingHealthProbe, syncthingAuthProbe = oldH, oldA
		syncthingReadyTimeout, syncthingAuthTimeout, syncthingPollInterval = oldReady, oldAuth, oldPoll
	})
}

// writeSyncthingFakeBrew 写一个只记录调用、按需报告"包已装/未装"的假 brew。
//
// services info 返回**沙箱里真实存在**的 plist 路径：这样 brewServiceInfo 与
// AdoptCandidate 都不会退到 `launchctl print` 去碰真实 launchd。
func writeSyncthingFakeBrew(t *testing.T, marker string, installed bool, plist string) string {
	t.Helper()
	code := "1"
	if installed {
		code = "0"
	}
	p := filepath.Join(t.TempDir(), "brew")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + marker + `'
if [ "$1" = "list" ] && [ "$2" = "--versions" ] && [ "$3" = "syncthing" ]; then
  exit ` + code + `
fi
if [ "$1" = "services" ] && [ "$2" = "info" ]; then
  printf '%s' '[{"name":"syncthing","status":"started","file":"` + plist + `"}]'
fi
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newSyncthingHarness(t *testing.T, installed bool, healthCode, authCode int) (*Manager, *syncthingRecorder, string) {
	t.Helper()
	m, _ := sandboxIdempotentManager(t)
	// 让 brewServiceInfo 拿到沙箱里的 plist，AdoptCandidate 因此短路，
	// 不会去跑真实 launchctl（单测不许碰真实服务）。
	plist := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", syncthingLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "brew-calls")
	m.opt.BrewBin = writeSyncthingFakeBrew(t, marker, installed, plist)

	rec := &syncthingRecorder{healthCode: healthCode, authCode: authCode}
	useSyncthingProbes(t, rec)
	return m, rec, marker
}

// writeSyncthingConfig 在沙箱家目录里写出配置（模拟 Syncthing 首次启动的结果）。
func writeSyncthingConfig(t *testing.T, m *Manager, content string) string {
	t.Helper()
	path := m.syncthingConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInstallSyncthingMainPathKeepsSecretsOutOfSteps 是主路径回归：
// 装好 → 配置被改成对外监听 + bcrypt 凭据 → 用新凭据自检 204 → 登记服务；
// 同时锁住"口令只出现在一次性凭据区块里"。
func TestInstallSyncthingMainPathKeepsSecretsOutOfSteps(t *testing.T) {
	m, rec, marker := newSyncthingHarness(t, true, 200, 204)
	ctx := context.Background()
	cfgPath := writeSyncthingConfig(t, m, syncthingTestConfig)

	res := &InstallResult{App: "syncthing", Name: syncthingDisplayName, Steps: []string{}}
	if err := m.InstallSyncthing(ctx, res); err != nil {
		t.Fatalf("安装应成功，实际: %v", err)
	}

	// 凭据区块：用户名 + 20 位 [A-Za-z0-9] 口令
	pw := syncthingCredValue(res, "syncthing_gui_password")
	if len(pw) != syncthingPasswordLen || !syncthingAlnumOnly(pw) {
		t.Fatalf("口令形状不对：%q", pw)
	}
	if got := syncthingCredValue(res, "syncthing_gui_user"); got != syncthingGUIUser {
		t.Errorf("凭据区块应给出用户名 %q，实际 %q", syncthingGUIUser, got)
	}

	// 落盘的 config.xml：对外监听 + 口令哈希与凭据一致 + 权限 0600
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "<address>"+syncthingLANAddress+"</address>") {
		t.Errorf("配置应改成对外监听：\n%s", raw)
	}
	hash := syncthingElementText(string(raw), syncthingPasswordRe)
	if hash == "" {
		t.Fatalf("配置里应写入口令哈希：\n%s", raw)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil {
		t.Error("config.xml 里的哈希与凭据区块给出的口令对不上（用户会登不上）")
	}
	if got := syncthingElementText(string(raw), syncthingUserRe); got != syncthingGUIUser {
		t.Errorf("配置里的用户名应为 %q，实际 %q", syncthingGUIUser, got)
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("配置权限必须 0600（里面有口令哈希），实际 %o", perm)
	}
	// ldap 的地址没被误改
	if !strings.Contains(string(raw), "<address></address>") {
		t.Error("<ldap> 里的 address 被误改了（补丁必须只动 <gui> 里的那个）")
	}

	// **口令与哈希都不许出现在任务步骤 / 结果文案 / 告警里**
	texts := strings.Join(res.Steps, "\n") + "\n" + res.Message + "\n" + res.Warning
	if strings.Contains(texts, pw) {
		t.Errorf("口令出现在了步骤/文案里（会被长期保存与转发）：\n%s", texts)
	}
	if strings.Contains(texts, hash) {
		t.Errorf("口令哈希出现在了步骤/文案里：\n%s", texts)
	}

	// 自检用的是新凭据，且成功必须留下 204 的记录
	if rec.lastUser != syncthingGUIUser || rec.lastPass != pw {
		t.Errorf("自检应使用新凭据，实际 %q/%q", rec.lastUser, rec.lastPass)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "204") {
		t.Errorf("成功时应在步骤里写明 204 自检通过：\n%s", joined)
	}
	if !strings.Contains(res.Address, ":8384") {
		t.Errorf("结果里应给出访问地址，实际 %q", res.Address)
	}
	if !strings.Contains(res.Message, "已安装") {
		t.Errorf("成功时应如实说明已安装，实际 %q", res.Message)
	}

	// 包已装 → 不重复 brew install；必须启动服务
	calls, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("假 brew 没被调用: %v", err)
	}
	if strings.Contains(string(calls), "install syncthing") {
		t.Errorf("包已装时不该再 brew install：\n%s", calls)
	}
	if !strings.Contains(string(calls), "services start syncthing") {
		t.Errorf("必须执行 brew services start syncthing，实际调用：\n%s", calls)
	}

	// 服务已登记进「服务管理」
	list, lerr := m.repo.List(ctx)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(list) != 1 || list[0].LaunchLabel != syncthingLabel {
		t.Fatalf("应登记一条 %s 的服务记录，实际 %+v", syncthingLabel, list)
	}
}

// TestInstallSyncthingInstallsMissingFormula 锁住"缺包时才 brew install"。
func TestInstallSyncthingInstallsMissingFormula(t *testing.T) {
	m, _, marker := newSyncthingHarness(t, false, 200, 204)
	writeSyncthingConfig(t, m, syncthingTestConfig)
	res := &InstallResult{App: "syncthing", Steps: []string{}}
	if err := m.InstallSyncthing(context.Background(), res); err != nil {
		t.Fatalf("安装应成功，实际: %v", err)
	}
	calls, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"install syncthing", "services start syncthing"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("缺包时应执行 %q，实际调用：\n%s", want, calls)
		}
	}
}

// TestInstallSyncthingRerunDoesNotResetCredentials 锁住重复安装的幂等语义：
// 第二次不改配置、不重置口令、不产生凭据区块，而且**不再打验收探针**
// （面板手里没有用户自己配的那个口令）。
func TestInstallSyncthingRerunDoesNotResetCredentials(t *testing.T) {
	m, rec, _ := newSyncthingHarness(t, true, 200, 204)
	ctx := context.Background()
	cfgPath := writeSyncthingConfig(t, m, syncthingTestConfig)

	res1 := &InstallResult{App: "syncthing", Steps: []string{}}
	if err := m.InstallSyncthing(ctx, res1); err != nil {
		t.Fatalf("第一次安装应成功: %v", err)
	}
	pw1 := syncthingCredValue(res1, "syncthing_gui_password")
	if pw1 == "" {
		t.Fatal("第一次应给出凭据")
	}
	after1, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if rec.authCalls != 1 {
		t.Fatalf("第一次应打一次验收探针，实际 %d", rec.authCalls)
	}

	// 第二次：故意让探针拒绝 —— 已经配置好时面板**不该**再用任何凭据打它。
	rec.authCode = 403
	res2 := &InstallResult{App: "syncthing", Steps: []string{}}
	if err := m.InstallSyncthing(ctx, res2); err != nil {
		t.Fatalf("重复安装必须安全成功（不重置、不失败），实际: %v", err)
	}
	after2, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after2) != string(after1) {
		t.Errorf("重复安装改动了配置（口令会被重置）\n之前：\n%s\n之后：\n%s", after1, after2)
	}
	if got := syncthingCredValue(res2, "syncthing_gui_password"); got != "" {
		t.Errorf("保留现有凭据时不该上报一个并不生效的新口令，实际 %q", got)
	}
	if rec.authCalls != 1 {
		t.Errorf("已经配置好时不该再打验收探针（面板没有那个口令），实际调用 %d 次", rec.authCalls)
	}
	joined := strings.Join(res2.Steps, "\n")
	if !strings.Contains(joined, "不重置") {
		t.Errorf("应如实说明凭据被保留、面板不重置：\n%s", joined)
	}
	if strings.Contains(joined, pw1) {
		t.Errorf("步骤里出现了口令：\n%s", joined)
	}
	// 重复安装也不该产生重复的服务记录
	list, _ := m.repo.List(ctx)
	if len(list) != 1 {
		t.Errorf("重复安装不该产生重复记录，实际 %d 条", len(list))
	}
}

// TestInstallSyncthingAuthFailureIsRealError：改了配置但自检不过 → 真错误，
// 绝不写"已就绪"，但凭据仍要到达用户（那是口令唯一的记录）。
func TestInstallSyncthingAuthFailureIsRealError(t *testing.T) {
	m, rec, _ := newSyncthingHarness(t, true, 200, 403)
	writeSyncthingConfig(t, m, syncthingTestConfig)
	logPath := m.syncthingOwnLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("reloading config\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := &InstallResult{App: "syncthing", Steps: []string{}}
	err := m.InstallSyncthing(context.Background(), res)
	if err == nil {
		t.Fatal("自检不是 204 时必须返回错误，不许谎报成功")
	}
	if !strings.Contains(err.Error(), "204") {
		t.Errorf("错误里应写清期望 204，实际：%v", err)
	}
	if !strings.Contains(err.Error(), logPath) {
		t.Errorf("错误里应给出日志路径 %s，实际：%v", logPath, err)
	}
	if strings.Contains(res.Message, "已安装") {
		t.Errorf("失败时绝不能写「已安装」：%q", res.Message)
	}
	pw := syncthingCredValue(res, "syncthing_gui_password")
	if pw == "" {
		t.Fatal("配置已经改动，口令必须通过一次性凭据区块到达用户")
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("错误信息里不许出现口令：%v", err)
	}
	if rec.authCalls == 0 {
		t.Error("必须真的打过验收探针（不能只看文件写没写）")
	}
}

// TestInstallSyncthingFirstRunFailureIsHonest：首次启动就没起来 → 真错误，
// 带日志尾部与"踢一下"的做法；没有动过配置就不该有凭据区块。
func TestInstallSyncthingFirstRunFailureIsHonest(t *testing.T) {
	m, _, _ := newSyncthingHarness(t, true, 503, 204)
	logPath := m.syncthingOwnLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("FATAL: failed to open config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 刻意不写 config.xml（复刻"进程没起来/还没生成配置"）。
	res := &InstallResult{App: "syncthing", Steps: []string{}}
	err := m.InstallSyncthing(context.Background(), res)
	if err == nil {
		t.Fatal("首次启动没就绪必须返回错误")
	}
	for _, needle := range []string{"rest/noauth/health", logPath, "FATAL", "--no-browser --no-restart"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("错误信息里应包含 %q，实际：%v", needle, err)
		}
	}
	if strings.Contains(res.Message, "已安装") {
		t.Errorf("失败时绝不能写「已安装」：%q", res.Message)
	}
	if pw := syncthingCredValue(res, "syncthing_gui_password"); pw != "" {
		t.Errorf("没有改动过配置就不该上报凭据，实际 %q", pw)
	}
}

// TestInstallSyncthingRequiresHomebrew：没有 Homebrew 必须立刻失败并点名。
func TestInstallSyncthingRequiresHomebrew(t *testing.T) {
	m, _, marker := newSyncthingHarness(t, true, 200, 204)
	m.opt.BrewBin = ""
	res := &InstallResult{App: "syncthing", Steps: []string{}}
	err := m.InstallSyncthing(context.Background(), res)
	if err == nil || !strings.Contains(err.Error(), "Homebrew") {
		t.Fatalf("缺 Homebrew 应报错并点名，实际 %v", err)
	}
	if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
		t.Errorf("没有 Homebrew 时不该执行任何命令，实际有调用记录：%s", marker)
	}
}
