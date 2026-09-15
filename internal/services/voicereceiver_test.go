package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReceiverPlistHostIsConfigurable 锁住监听地址这件事。
//
// 背景（交接文档 HANDOFF-TO-PANEL-1.7B.md 第 5 条）：
// 本机这份接收端"只给网站用，127.0.0.1 就够"，而 mini 上要 0.0.0.0（网站可能在别处）。
// 这两台机器用的是同一份面板代码，所以 host 必须是**参数**而不是写死在模板里 ——
// 之前写死 0.0.0.0，等于本机也把接收端暴露到局域网。
func TestReceiverPlistHostIsConfigurable(t *testing.T) {
	p := receiverPaths{
		Dir: "/Users/u/tts/voice-receiver", Script: "/Users/u/tts/voice-receiver/receiver.py",
		Samples: "/Users/u/tts/voice-samples", Jobs: "/Users/u/tts/jobs",
		Keys:   "/Users/u/tts/voice-receiver/keys.json",
		OutLog: "/tmp/out.log", ErrLog: "/tmp/err.log",
		Plist: "/Library/LaunchDaemons/com.zizdog.voicereceiver.plist",
	}

	local := receiverPlist(p, "zizdog", "127.0.0.1", "zpa-testtoken0123456789abcdef")
	if !strings.Contains(local, "<string>127.0.0.1</string>") {
		t.Error("本机部署应能写成 127.0.0.1（只给同机的网站用，不暴露到局域网）")
	}
	if strings.Contains(local, "<string>0.0.0.0</string>") {
		t.Error("传了 127.0.0.1 就不该同时出现 0.0.0.0")
	}

	lan := receiverPlist(p, "zizdog", "0.0.0.0", "zpa-testtoken0123456789abcdef")
	if !strings.Contains(lan, "<string>0.0.0.0</string>") {
		t.Error("mini 部署仍应是 0.0.0.0")
	}

	// 脚本路径、样本/作业目录、密钥表路径都要写进去（模板改动不能碰坏这些）
	for _, want := range []string{p.Script, p.Samples, p.Jobs, p.Keys,
		"com.zizdog.voicereceiver", "--jobs-dir", "--keys-file", "--admin-token",
		"zpa-testtoken0123456789abcdef", qwenUpstream} {
		if !strings.Contains(local, want) {
			t.Errorf("plist 里缺少 %q", want)
		}
	}

	// v1.6.0：plist 里**不该**再有密钥参数 —— 它在 keys.json（热加载）。
	// 留着 --token 的后果是：改密钥要重启正在合成中的服务，而且两份真值会漂移。
	// （模板注释里会提到 --token 这个词，所以只断言参数行本身。）
	if strings.Contains(local, "<string>--token</string>") {
		t.Error("plist 里不该再写 --token 参数（密钥已迁到 keys.json）")
	}
}

// TestValidateReceiverToken 锁住共享密钥的字符集与长度。
//
// 为什么必须校验：这个值要写进 **root 拥有的 plist XML**。
// 一个 & 就能让 plist 语法坏掉、launchd 拒绝装载 —— 表现为"改完密钥接收端起不来"，
// 而且报错在 launchd 日志里，很难联想到这里。
func TestValidateReceiverToken(t *testing.T) {
	good := []string{"ttsv-0123456789abcdef0123456789abcdef", "abc12345", "A-b_c.d1"}
	for _, g := range good {
		if err := ValidateReceiverToken(g); err != nil {
			t.Errorf("%q 应当被接受，实际 %v", g, err)
		}
	}
	bad := []string{"", "short", "has space here", "amp&ersand", "lt<gt>", `quote"x`, "semi;colon"}
	for _, b := range bad {
		if err := ValidateReceiverToken(b); err == nil {
			t.Errorf("%q 不该被接受（会写进 plist XML 或长度不合适）", b)
		}
	}
	if err := ValidateReceiverToken(strings.Repeat("a", 129)); err == nil {
		t.Error("超长密钥应当被拒绝")
	}
}

// TestExistingReceiverHostReadsPlist 锁住"改密钥不能顺手改监听地址"。
//
// 本机是 127.0.0.1（只给同机的网站用），mini 是 0.0.0.0（网站在别的机器上）。
// 如果换密钥时读不到原地址就退回默认值，本机就会在用户毫不知情的情况下
// 被暴露到局域网 —— 这是安全语义，不是格式问题。
func TestExistingReceiverHostReadsPlist(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(nil, Options{UserName: "zizdog", UserHome: dir})
	p := receiverPaths{
		Dir: dir, Script: dir + "/receiver.py", Samples: dir + "/samples",
		Jobs: dir + "/jobs", Keys: dir + "/keys.json",
		OutLog: dir + "/o.log", ErrLog: dir + "/e.log",
		Plist: dir + "/com.zizdog.voicereceiver.plist",
	}
	write := func(host string) {
		plist := receiverPlist(p, "zizdog", host, "zpa-testtoken0123456789abcdef")
		if err := os.WriteFile(p.Plist, []byte(plist), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("127.0.0.1")
	if got := m.existingReceiverHost(p); got != "127.0.0.1" {
		t.Errorf("应从 plist 读回 127.0.0.1，实际 %q", got)
	}
	write("0.0.0.0")
	if got := m.existingReceiverHost(p); got != "0.0.0.0" {
		t.Errorf("应从 plist 读回 0.0.0.0，实际 %q", got)
	}
	// plist 不存在 / 内容异常时返回空串（调用方据此决定用默认值并给出提示）
	_ = os.Remove(p.Plist)
	if got := m.existingReceiverHost(p); got != "" {
		t.Errorf("plist 不存在时应返回空串，实际 %q", got)
	}
}

// TestQwenWarmPolicyIsSingleModel 锁住 2026-09-14 定的模型策略。
//
// 背景（usr/plugins/TtsVoice/HANDOFF-TO-PANEL-1.7B.md）：网站侧只支持自定义音色，
// 预置音色整体下线，服务端只需要 1.7B-Base-8bit 一个模型；CustomVoice 与 0.6B
// 的权重已从两台机器上删掉腾空间。
//
// 旧版这里断言的是"内存不够就只常驻一个模型"（双模型时代的守温策略）。
// 现在清单里本来就只有一个模型，那条内存门槛逻辑连同它一起删掉了 ——
// 留一条更直接的断言：清单里必须只有一个模型，且是克隆用的 Base。
func TestQwenWarmPolicyIsSingleModel(t *testing.T) {
	if len(QwenModels) != 1 {
		t.Fatalf("清单里应只有一个模型（预置音色已下线），实际 %d 个", len(QwenModels))
	}
	if !strings.Contains(qwenDefaultModel, "1.7B-Base") {
		t.Errorf("唯一的模型应是 1.7B-Base（克隆），实际 %s", qwenDefaultModel)
	}
	if QwenModels[0].Name != qwenDefaultModel {
		t.Errorf("清单里唯一的模型必须就是默认模型，实际 %s", QwenModels[0].Name)
	}
}

// TestSanitizeVoiceSource 锁住"来源标识会变成目录名"这件事。
//
// 它与 receiver.py 的 sanitize_source() 是**同一套规则的两份实现**
// （面板用它拼删除请求的路径，接收端用它拼目录名）。规则一旦分叉，
// 会出现"面板删 A、接收端理解成 B"，所以这里逐条对齐 Python 侧的用例：
// tools/test-voice-jobs-unit.py 里的 sanitize_source 断言必须是同一组期望。
func TestSanitizeVoiceSource(t *testing.T) {
	cases := []struct{ in, want string }{
		{"zizdog-cn", "zizdog-cn"},
		{"site_a", "site_a"},
		{"../../etc/passwd", "etc-passwd"}, // 路径语义字符被换成连字符
		{"", "default"},                    // 空 → default
		{"///", "default"},                 // 纯符号 → default
		{"a.wav", "a-wav"},                 // 刻意不复用 safe_name：点号不保留
		{"我的站", "default"},                 // 纯非 ASCII → default
		{"站点-a", "a"},                      // 非 ASCII 段被削掉，保留 ASCII 段
	}
	for _, c := range cases {
		if got := SanitizeVoiceSource(c.in); got != c.want {
			t.Errorf("SanitizeVoiceSource(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	long := strings.Repeat("x", 200)
	if got := SanitizeVoiceSource(long); len(got) != 64 {
		t.Errorf("来源标识要截到 64 字符，实际 %d", len(got))
	}
}

// TestExistingReceiverTokenAcceptsCustomToken 锁住"自定义密钥也要能读回来"。
//
// 曾经的实现要求密钥以 ttsv- 开头（那是面板自动生成的格式）。用户自定义的
// 密钥读回来会变成空串：换密钥时会"又生成一个"（网站立刻失联），
// 音色来源管理也无法通过接收端鉴权 —— 两种症状都不指向真正的原因。
func TestExistingReceiverTokenAcceptsCustomToken(t *testing.T) {
	dir := t.TempDir()
	plist := dir + "/com.zizdog.voicereceiver.plist"

	write := func(body string) {
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{}

	custom := `<array>
	<string>--token</string>
	<string>my-custom-key-123</string>
	<string>--host</string>
	<string>127.0.0.1</string>
</array>`
	write(custom)
	if got := m.existingReceiverToken(receiverPaths{Plist: plist}); got != "my-custom-key-123" {
		t.Errorf("自定义密钥应原样读回，实际 %q", got)
	}

	write(`<array>
	<string>--token</string>
	<string>ttsv-abc123</string>
</array>`)
	if got := m.existingReceiverToken(receiverPaths{Plist: plist}); got != "ttsv-abc123" {
		t.Errorf("面板生成的密钥仍要能读回，实际 %q", got)
	}

	// 未启用鉴权（空密钥）时不能返回一个"看起来像密钥"的东西
	write(`<array>
	<string>--token</string>
	<string></string>
</array>`)
	if got := m.existingReceiverToken(receiverPaths{Plist: plist}); got != "" {
		t.Errorf("空密钥应返回空串，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
//  v1.6.0 多密钥 / 额度
// ---------------------------------------------------------------------------

// newKeyTestManager 造一个只碰临时目录的 Manager。
//
// UserName 必须留空：SaveVoiceKeys 会按运行用户 chown，
// 而测试里那个用户不存在（也不能去动真实用户的文件归属）。
func newKeyTestManager(t *testing.T) (*Manager, receiverPaths) {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(nil, Options{UserHome: dir})
	p := m.receiverPaths()
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// receiverPaths() 里的 Plist 是**真实系统路径**（/Library/LaunchDaemons/…）。
	// 测试必须把它改到临时目录：这条断言曾经真的去写过真实 plist
	// （当时因为权限不够才没造成破坏，纯属侥幸）。
	p.Plist = filepath.Join(dir, receiverLabel+".plist")
	if err := m.SaveVoiceKeys(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Keys, dir) || !strings.HasPrefix(p.Plist, dir) {
		t.Fatalf("测试路径必须全在临时目录里：keys=%s plist=%s", p.Keys, p.Plist)
	}
	return m, p
}

func TestVoiceKeyStoreRoundTrip(t *testing.T) {
	m, p := newKeyTestManager(t)

	if keys, err := m.LoadVoiceKeys(); err != nil || keys != nil {
		t.Fatalf("密钥表不存在时应返回空表而不是错误：%v %v", keys, err)
	}

	k1, err := m.NewVoiceKey("A 站", "", 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k1.Key, receiverTokenPrefix) || len(k1.Key) != len(receiverTokenPrefix)+32 {
		t.Errorf("自动生成的密钥应为 %s<32 hex>，实际 %q", receiverTokenPrefix, k1.Key)
	}
	if k1.ID == "" || !strings.HasPrefix(k1.ID, "k-") {
		t.Errorf("密钥 id 应形如 k-<hex>，实际 %q", k1.ID)
	}

	if err := m.SaveVoiceKeys([]VoiceKey{k1}); err != nil {
		t.Fatal(err)
	}
	keys, err := m.LoadVoiceKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("应读回 1 条密钥：%v %v", keys, err)
	}
	if keys[0].Name != "A 站" || keys[0].QuotaChars != 1000 || !keys[0].Enabled {
		t.Errorf("密钥字段没有原样落盘：%+v", keys[0])
	}
	// 文件里是明文密钥，权限必须是 0600（接收端以运行用户读取）
	if fi, err := os.Stat(p.Keys); err == nil {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("密钥表权限应为 0600，实际 %o", fi.Mode().Perm())
		}
	}
}

func TestVoiceKeyGuards(t *testing.T) {
	m, _ := newKeyTestManager(t)

	if _, err := m.NewVoiceKey("x", "bad value with space", 0, true); err == nil {
		t.Error("带空格的密钥值应被拒（会写坏/难用）")
	}
	if _, err := m.AddVoiceKey("负额度", "", -5, true); err == nil {
		t.Error("负额度应被拒")
	}

	k1, err := m.AddVoiceKey("A", "", 100, true)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := m.AddVoiceKey("B", "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddVoiceKey("重值", k1.Key, 0, true); err == nil {
		t.Error("重复的密钥值应被拒（否则两把密钥共用同一份额度）")
	}

	// 停用/删除最后一条**启用**的密钥 = 一键把所有人关在门外，必须拦住
	if _, err := m.UpdateVoiceKey(k1.ID, nil, nil, boolPtr(false), nil); err != nil {
		t.Fatalf("还有 B 启用时，停用 A 应当允许：%v", err)
	}
	if _, err := m.UpdateVoiceKey(k2.ID, nil, nil, boolPtr(false), nil); err == nil {
		t.Error("停用最后一条启用的密钥应被拒")
	}
	if err := m.DeleteVoiceKey(k2.ID); err == nil {
		t.Error("删除最后一条启用的密钥应被拒")
	}

	// 恢复 A、删掉 B，然后再删 A 也不行（那就一条都不剩了）
	if _, err := m.UpdateVoiceKey(k1.ID, nil, nil, boolPtr(true), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteVoiceKey(k2.ID); err != nil {
		t.Fatalf("B 未启用，删它应当允许：%v", err)
	}
	if err := m.DeleteVoiceKey(k1.ID); err == nil {
		t.Error("删掉唯一一条启用的密钥应被拒")
	}

	if _, err := m.UpdateVoiceKey("k-nope", nil, nil, nil, nil); err == nil {
		t.Error("改一条不存在的密钥应报错")
	}
	if err := m.DeleteVoiceKey("k-nope"); err == nil {
		t.Error("删一条不存在的密钥应报错")
	}
}

func TestEnsureVoiceKeysMigratesLegacyToken(t *testing.T) {
	m, p := newKeyTestManager(t)

	legacy := "ttsv-legacytoken0123456789abcdef"
	plist := receiverPlist(p, "zizdog", "127.0.0.1", "zpa-testtoken0123456789abcdef")
	// 手工塞一个老式 --token 进去，模拟"还没迁移的机器"
	plist = strings.Replace(plist, "        <string>--keys-file</string>",
		"        <string>--token</string>\n        <string>"+legacy+"</string>\n"+
			"        <string>--keys-file</string>", 1)
	if err := os.WriteFile(p.Plist, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.existingReceiverToken(p); got != legacy {
		t.Fatalf("前置条件：应能从 plist 读回老密钥，实际 %q", got)
	}

	fresh, primary, err := m.ensureVoiceKeys(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh || primary != legacy {
		t.Errorf("迁移后应沿用老密钥（网站不能被迫改配置），实际 fresh=%v key=%q", fresh, primary)
	}
	keys, err := m.LoadVoiceKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("迁移后应有 1 条密钥：%v %v", keys, err)
	}
	if keys[0].ID != "default" || keys[0].Key != legacy || !keys[0].Enabled {
		t.Errorf("迁移出的密钥应是 default/启用且值不变：%+v", keys[0])
	}

	// 幂等：再跑一次不会多出一条，也不会改掉已有密钥
	fresh2, primary2, err := m.ensureVoiceKeys(p, "ttsv-somethingelse0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if fresh2 || primary2 != legacy {
		t.Errorf("重复调用应保持原样（不能因为安装参数就换掉线上密钥）：fresh=%v key=%q",
			fresh2, primary2)
	}
	if keys2, _ := m.LoadVoiceKeys(); len(keys2) != 1 {
		t.Errorf("重复调用不该新增密钥，实际 %d 条", len(keys2))
	}
}

func TestVoiceKeyIDValidation(t *testing.T) {
	for _, good := range []string{"k-a1b2", "default", "abc_123", "K-9"} {
		if _, err := VoiceKeyID(good); err != nil {
			t.Errorf("%q 应当接受：%v", good, err)
		}
	}
	for _, bad := range []string{"", "../x", "a b", "a/b", strings.Repeat("x", 65)} {
		if _, err := VoiceKeyID(bad); err == nil {
			t.Errorf("%q 应当被拒（会进 URL）", bad)
		}
	}
}

func boolPtr(v bool) *bool { return &v }
