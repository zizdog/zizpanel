package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

// ---------------------------------------------------------------------------
//  v1.7.1 内置默认音色
// ---------------------------------------------------------------------------

func TestBuiltinDefaultVoiceProvisioned(t *testing.T) {
	m, p := newKeyTestManager(t)
	var result InstallResult
	ctx := context.Background()

	if err := m.provisionDefaultVoice(ctx, &result, p); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(p.Samples, defaultVoiceSource, REF_FILENAME)
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("应写入 default 音色：%v", err)
	}
	if len(got) != len(defaultVoiceWav) {
		t.Errorf("写入的字节数不对：%d != %d", len(got), len(defaultVoiceWav))
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != builtinVoiceSHA() {
		t.Error("写入的音色与内嵌样本 sha256 不一致")
	}
	// 参考文字（接收端读它）
	txt, err := os.ReadFile(filepath.Join(p.Samples, defaultVoiceSource, REF_TEXT_FILENAME))
	if err != nil {
		t.Fatalf("应写入 ref.txt：%v", err)
	}
	if strings.TrimSpace(string(txt)) != defaultVoiceRefText {
		t.Errorf("ref.txt 内容不对：%q", strings.TrimSpace(string(txt)))
	}
	// 参考文字必须与音频对得上（用户给的那句话，逐字）
	if !strings.Contains(defaultVoiceRefText, "我一直都在这里陪着你") {
		t.Error("内置参考文字被改坏了")
	}
	// 幂等
	var again InstallResult
	if err := m.provisionDefaultVoice(ctx, &again, p); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(target)
	if string(got2) != string(got) {
		t.Error("重复部署不该改动内容")
	}
}

func TestBuiltinDefaultVoiceDoesNotClobberUserSample(t *testing.T) {
	m, p := newKeyTestManager(t)
	ctx := context.Background()

	// 用户自己上传过 default 样本：没有我们的标记
	dir := filepath.Join(p.Samples, defaultVoiceSource)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := []byte("用户自己的样本，不是内置的")
	target := filepath.Join(dir, REF_FILENAME)
	if err := os.WriteFile(target, mine, 0o644); err != nil {
		t.Fatal(err)
	}

	var result InstallResult
	if err := m.provisionDefaultVoice(ctx, &result, p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(mine) {
		t.Error("用户自己上传的 default 样本被内置音色覆盖了 —— 这是最不能犯的错")
	}
	if !strings.Contains(strings.Join(result.Steps, " "), "保留") {
		t.Errorf("应当明确告知保留了用户的样本，实际步骤：%v", result.Steps)
	}
}

func TestBuiltinDefaultVoiceUpdatesWhenBuiltinChanges(t *testing.T) {
	m, p := newKeyTestManager(t)
	ctx := context.Background()

	// 模拟"上一版内置音色"：有标记但 sha 不同
	dir := filepath.Join(p.Samples, defaultVoiceSource)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, REF_FILENAME)
	if err := os.WriteFile(target, []byte("旧的内置样本"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := builtinVoiceInfo{Source: defaultVoiceSource, SHA256: "deadbeef", Size: 1,
		RefText: "旧的参考文字", Updated: 1}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, builtinVoiceMarker), b, 0o644); err != nil {
		t.Fatal(err)
	}

	var result InstallResult
	if err := m.provisionDefaultVoice(ctx, &result, p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != builtinVoiceSHA() {
		t.Error("内置音色升级后应被替换成新版本")
	}
}

func TestBuiltinVoiceAssetIsUsableWav(t *testing.T) {
	// 内置样本必须是接收端直接能用的格式：WAV / 24kHz / 单声道 / 16bit。
	// 不是的话安装时得转码，而接收端所在的机器不一定有 ffmpeg。
	if len(defaultVoiceWav) < 44 || string(defaultVoiceWav[0:4]) != "RIFF" ||
		string(defaultVoiceWav[8:12]) != "WAVE" {
		t.Fatal("内置音色不是合法的 WAV")
	}
	if string(defaultVoiceWav[12:16]) != "fmt " {
		t.Fatal("内置音色缺少 fmt 块")
	}
	channels := int(defaultVoiceWav[22]) | int(defaultVoiceWav[23])<<8
	rate := int(defaultVoiceWav[24]) | int(defaultVoiceWav[25])<<8 |
		int(defaultVoiceWav[26])<<16 | int(defaultVoiceWav[27])<<24
	bits := int(defaultVoiceWav[34]) | int(defaultVoiceWav[35])<<8
	if channels != 1 || rate != 24000 || bits != 16 {
		t.Errorf("内置音色应为 24kHz/单声道/16bit，实际 %dHz/%dch/%dbit", rate, channels, bits)
	}
	if len(defaultVoiceWav) < 48000 {
		t.Error("内置音色太短（不足 1 秒），接收端会拒收")
	}
}

// TestBrewEnvInjectsChinaMirrors 锁住"面板跑 brew 时必须注入国内镜像"。
//
// 背景：install.sh 只把镜像写进用户 shell 的 rc，而面板是 LaunchDaemon（root），
// 读不到那个 rc；sudo 又会清空环境。结果面板装 nginx/PHP/MySQL 时 brew 走官方源，
// 国内无代理基本不通，界面表现是"点了安装长时间没进度"，看着像面板卡死。
func TestBrewEnvInjectsChinaMirrors(t *testing.T) {
	// ⚠️ 必须清掉**运行环境里**可能存在的 HOMEBREW_API_DOMAIN / HOMEBREW_BOTTLE_DOMAIN。
	// 开发机/CI 上常为了加速而导出这两个变量（本机实测就导出的是阿里云），
	// 而 brewEnv 的契约是"用户设过的以用户为准" —— 不清掉的话，
	// 这个测试断言的就成了"运行环境的值"，而不是被测代码的行为。
	t.Setenv("HOMEBREW_API_DOMAIN", "")
	t.Setenv("HOMEBREW_BOTTLE_DOMAIN", "")

	// 注入假探测：不许打真实网络（单测纪律）。这里模拟"自建镜像可用"。
	m := &Manager{mirrorProbeOverride: func(ctx context.Context, probeFormula string) (string, string) {
		return "https://mirror.example.com:8888/brew/api", "https://mirror.example.com:8888/brew"
	}}
	env := m.brewEnv(context.Background(), "php@8.3")
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"HOMEBREW_API_DOMAIN=https://mirror.example.com:8888/brew/api",
		"HOMEBREW_BOTTLE_DOMAIN=https://mirror.example.com:8888/brew",
		"HOMEBREW_NO_AUTO_UPDATE=1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("brewEnv 缺少 %s：\n%s", want, joined)
		}
	}
	// 用户已经设过的以用户为准（不覆盖）
	t.Setenv("HOMEBREW_API_DOMAIN", "https://example.com/api")
	if got := strings.Join(m.brewEnv(context.Background(), "php@8.3"), "\n"); !strings.Contains(got, "https://example.com/api") {
		t.Error("用户自己设的 HOMEBREW_API_DOMAIN 应被尊重，不能被默认值覆盖")
	}
}

// TestBrewEnvPrefersSelfHostedMirror 锁住用户的要求：
// **LNMP 的包也要走自建镜像站，并且优先调用**。
//
// 自建镜像不可用时必须能落到公共镜像 —— 否则镜像站一挂，LNMP 就装不上。
func TestBrewEnvPrefersSelfHostedMirror(t *testing.T) {
	// 同上：先清掉运行环境里的同名变量，否则断言的是环境而不是代码。
	t.Setenv("HOMEBREW_API_DOMAIN", "")
	t.Setenv("HOMEBREW_BOTTLE_DOMAIN", "")

	// 自建镜像可用 → 用它（api 与瓶都指向它）
	ok := &Manager{mirrorProbeOverride: func(ctx context.Context, f string) (string, string) {
		return "https://mirror.zizdog.com:8888/brew/api", "https://mirror.zizdog.com:8888/brew"
	}}
	got := strings.Join(ok.brewEnv(context.Background(), "nginx"), "\n")
	if !strings.Contains(got, "HOMEBREW_BOTTLE_DOMAIN=https://mirror.zizdog.com:8888/brew") {
		t.Errorf("自建镜像可用时应优先用它做瓶域：\n%s", got)
	}

	// 自建镜像不可用 → 落到公共镜像（这里由注入模拟），不能留空导致走官方
	fallback := &Manager{mirrorProbeOverride: func(ctx context.Context, f string) (string, string) {
		return "https://mirrors.ustc.edu.cn/homebrew-bottles/api",
			"https://mirrors.ustc.edu.cn/homebrew-bottles"
	}}
	got2 := strings.Join(fallback.brewEnv(context.Background(), "nginx"), "\n")
	if !strings.Contains(got2, "HOMEBREW_API_DOMAIN=https://mirrors.ustc.edu.cn/homebrew-bottles/api") {
		t.Errorf("自建镜像不可用时该落到公共镜像：\n%s", got2)
	}
	if !strings.Contains(got2, "HOMEBREW_BOTTLE_DOMAIN=https://mirrors.ustc.edu.cn/homebrew-bottles") {
		t.Errorf("公共镜像的瓶域也要设上（实测中科大 5.1MB/s）：\n%s", got2)
	}
}

// TestFirstFormulaOf 锁住"从 brew 参数里认出探针包名"。
//
// 这个解析只服务于"用哪个镜像"的探测：探错了包名最多是探测失败（回落到第一家），
// 不会影响真正安装的参数 —— 所以它必须足够保守，宁可返回空。
func TestFirstFormulaOf(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"install", "php@8.3"}, "php@8.3"},
		{[]string{"install", "--formula", "nginx"}, "nginx"}, // -开头 的跳过
		{[]string{"reinstall", "mysql@8.4"}, "mysql@8.4"},
		{[]string{"services", "start", "nginx"}, "nginx"},
		{[]string{"install"}, ""},   // 没有包名
		{[]string{"--version"}, ""}, // 只有选项
	}
	for _, c := range cases {
		if got := firstFormulaOf(c.args); got != c.want {
			t.Errorf("firstFormulaOf(%v) = %q，期望 %q", c.args, got, c.want)
		}
	}
}

// TestBrewMirrorWorksProbe 锁住镜像探测本身。
//
// 为什么值得单独测：这个探测决定"用哪家镜像"，而选错的后果很隐蔽 ——
// 镜像清单旧了会让 brew 取不到瓶文件、静默回落到 ghcr.io（91KB/s），
// 表现成"一键装 LNMP 特别慢/像卡死"。这里钉住"能拿到清单才算可用"。
func TestBrewMirrorWorksProbe(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/formula/nginx.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"nginx"}`))
	})
	// php@8.3 故意不给 → 模拟"清单里没有这个包"的镜像
	ts := httptest.NewServer(mux)
	defer ts.Close()

	if !brewMirrorWorks(context.Background(), ts.URL, "nginx") {
		t.Error("能从镜像 API 拿到清单时应判定为可用")
	}
	if brewMirrorWorks(context.Background(), ts.URL, "php@8.3") {
		t.Error("镜像 API 没有这个 formula（404）时不该判定为可用")
	}
	if brewMirrorWorks(context.Background(), "", "nginx") {
		t.Error("基址为空时不该判定为可用")
	}
	// 不可达的地址：必须如实返回 false，而不是挂住
	if brewMirrorWorks(context.Background(), "http://127.0.0.1:1", "nginx") {
		t.Error("不可达的镜像不该判定为可用")
	}
}

// TestBrewMirrorSupportsOCI 锁住"只有真能取到瓶的镜像才配当瓶域"。
//
// 背景（2026-09-16 真机教训）：这里原先硬编码了一个**编造的** sha256 去 HEAD，
// 结果永远 404 → 镜像分支永不成立 → brew 静默回落 ghcr.io（用户"装了 21 分钟"）。
// 现在改成：先读 <base>/api/formula/<f>.json 拿真实版本/标签/rebuild，再按 brew 实际
// 会用的两种瓶路径去探测。这条测试同时锁住"必须有清单""瓶必须真的能取到（且非空）"。
//
// 2026-09-18 补：判据从"状态码 200/206"升级为"200/206 **且真的有字节**"，
// 并且文件名要按 brew 7 的规则拼（`@`→%40、rebuild>0 时带 `.bottle.<N>`）。
func TestBrewMirrorSupportsOCI(t *testing.T) {
	manifest := `{"versions":{"stable":"1.31.5"},
		"bottle":{"stable":{"files":{"arm64_sequoia":{"sha256":"deadbeefcafe"}}}}}`

	// 1) 清单在 + 瓶可下（206）→ 可用
	ok := http.NewServeMux()
	ok.HandleFunc("/api/formula/nginx.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifest))
	})
	ok.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".bottle.tar.gz") || strings.Contains(r.URL.Path, "/blobs/sha256:") {
			// 必须真给一个字节：2026-09-18 起探测判据是"200/206 **且有内容**"，
			// 因为真机实测中科大 IPv4 侧对所有瓶回 `200 + 0 字节`，
			// 只看状态码会把坏源判成可用（正是 python@3.11 事故的表象）。
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("B"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(ok)
	defer ts.Close()
	if !brewMirrorSupportsOCI(context.Background(), ts.URL) {
		t.Error("清单在且瓶能取到时，应判定为可当瓶域")
	}

	// 2) 只有瓶、没有清单（旧实现会误判为可用）→ 不可用
	noManifest := http.NewServeMux()
	noManifest.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/sha256:") {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("B"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts2 := httptest.NewServer(noManifest)
	defer ts2.Close()
	if brewMirrorSupportsOCI(context.Background(), ts2.URL) {
		t.Error("拿不到清单时不该判定为可用（探测要靠清单里的真实 sha，不能拍固定 URL）")
	}

	// 3) 清单在但瓶取不到（404）→ 不可用（正是阿里云那种"有清单没 /v2"）
	noBottle := http.NewServeMux()
	noBottle.HandleFunc("/api/formula/nginx.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifest))
	})
	noBottle.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ts3 := httptest.NewServer(noBottle)
	defer ts3.Close()
	if brewMirrorSupportsOCI(context.Background(), ts3.URL) {
		t.Error("清单在但瓶 404 时不该判定为可用")
	}

	if brewMirrorSupportsOCI(context.Background(), "") {
		t.Error("基址为空时不该判定为可用")
	}
}

// TestParseCLTLabel 锁住"从 softwareupdate -l 输出里认出命令行工具条目"。
//
// 这段解析最容易写错又最难复现：真机上输出是英文/本地化混排，
// 而它决定"无界面静默安装"这条路能不能走通（走不通就要用户点弹窗）。
func TestParseCLTLabel(t *testing.T) {
	cases := []struct{ name, out, want string }{
		{"带 Label 前缀",
			"Software Update Tool\n\nFinding available software\n* Label: Command Line Tools for Xcode-16.2\n",
			"Command Line Tools for Xcode-16.2"},
		{"没有 Label 前缀",
			"* Command Line Tools for Xcode-15.4\n",
			"Command Line Tools for Xcode-15.4"},
		{"Title 形式（另一种版本）",
			"   Title: Command Line Tools for Xcode 16.1, Version: 16.1, Size: 700000KiB\n",
			"Command Line Tools for Xcode 16.1"},
		{"只有普通更新：认不出来",
			"* macOS Sonoma 14.5-23F79\n* Safari 17.5\n",
			""},
		{"空输出", "", ""},
	}
	for _, c := range cases {
		if got := parseCLTLabel(c.out); got != c.want {
			t.Errorf("%s：parseCLTLabel = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestBrewInstallScriptCandidatesMirrorFirst 锁住"安装脚本**镜像优先、官方兜底**"。
//
// 2026-09-18 用户真机反馈后反转的：原测试锁的是"官方优先"（本函数名原为
// PrefersOfficial），而 raw.githubusercontent.com 在国内会长时间无响应，
// 用户看到的是"下载 Homebrew 安装脚本"那一步静默卡很久 —— 默认就该走国内镜像。
func TestBrewInstallScriptCandidatesMirrorFirst(t *testing.T) {
	got := brewInstallScriptCandidates()
	if len(got) < 2 {
		t.Fatalf("应当有镜像 + 官方兜底，实际 %v", got)
	}
	if got[0] != brewInstallScriptMirror {
		t.Errorf("第一个应为国内镜像 %s，实际 %s", brewInstallScriptMirror, got[0])
	}
	if got[len(got)-1] != brewInstallScriptURL {
		t.Errorf("最后一个应为官方兜底 %s，实际 %s", brewInstallScriptURL, got[len(got)-1])
	}
	// 中间那些是"加速前缀 + 官方路径"
	for _, u := range got[1 : len(got)-1] {
		if !strings.Contains(u, "raw.githubusercontent.com") {
			t.Errorf("加速镜像应当是加速前缀 + 官方路径，实际 %s", u)
		}
	}
}

// TestFreshMacNeedsCLTBeforeTTS 锁住"装 TTS 之前必须先确保命令行开发者工具"。
//
// 真机（抹机后的 mini）实测：全新 macOS 上 /usr/bin/python3 只是占位程序，
// 跑它只会打印 "xcode-select: note: No developer tools were found, requesting install."
// 并弹出图形对话框。而 Qwen3 TTS（python venv + pip）与音色接收端（/usr/bin/python3）
// 都要真 Python —— 不先装 CLT，它们会以"看不懂的方式"失败。
func TestFreshMacNeedsCLTBeforeTTS(t *testing.T) {
	// 接收端只要真 python3（/usr/bin/python3）→ 确保 CLT 即可
	b, err := os.ReadFile("voicereceiver.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "m.EnsureCLT(ctx, result)") {
		t.Error("voicereceiver.go 必须在安装前调用 EnsureCLT（接收端脚本靠 /usr/bin/python3）")
	}
	// Qwen 还要 brew 里的 python@3.11 → 需要 EnsureHomebrew（它内部会先装 CLT）
	b, err = os.ReadFile("qwentts.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "m.EnsureHomebrew(ctx, result)") {
		t.Error("qwentts.go 必须先 EnsureHomebrew（CLT → brew → python@3.11 一整条链）")
	}
	// EnsureHomebrew 也要走同一条路（不能各写一份 CLT 检查）
	b, err = os.ReadFile("homebrew.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "m.installCLT(") != 1 {
		t.Error("installCLT 只应被 EnsureCLT 调用一次，避免两套判定漂移")
	}
}

// TestForceWavFormatFallback 锁住"上游没装 ffmpeg 时不能靠 mp3 路径"。
//
// 真机（抹机后的 mini）实测：上游 mlx-audio 在**没带 response_format** 时
// 走 ffmpeg 编码路径，而没装 ffmpeg 时返回"HTTP 200 + 断连"，
// 客户端只看到 `IncompleteRead(0 bytes read)` —— 完全推不到"缺 ffmpeg"。
// 同一个请求带上 response_format=wav 就 200 且给出合法 wav（183KB）。
// 所以接收端必须兜底：没写、或写了 mp3，都改成 wav；
// 写了其它格式（客户端有明确意图）则保持原样。
//
// 这个测试直接跑 receiver.py 里的纯函数（用 python3 子进程），
// 不启服务、不碰上游。
func TestForceWavFormatFallback(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("没有 python3")
	}
	prog := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("rec", "voice-receiver.py")
m = importlib.util.module_from_spec(spec)
# 只加载模块级定义，不启动服务（文件末尾才有 __main__ 分支）
spec.loader.exec_module(m)
cases = [
    ({"model": "x", "input": "你好"}, "wav"),
    ({"model": "x", "input": "你好", "response_format": "mp3"}, "wav"),
    ({"model": "x", "input": "你好", "response_format": "wav"}, "wav"),
    ({"model": "x", "input": "你好", "response_format": "flac"}, "flac"),
]
bad = 0
for body, want in cases:
    raw = json.dumps(body).encode()
    out, changed = m.force_wav_format(raw)
    got = json.loads(out.decode()).get("response_format")
    if got != want:
        print("FAIL want=%s got=%s" % (want, got)); bad += 1
# 非法 JSON 必须原样返回，不能抛异常把转发路径打断
raw = b"not-json"
out, changed = m.force_wav_format(raw)
if out != raw or changed:
    print("FAIL 非法 JSON 应当原样返回"); bad += 1
print("BAD=%d" % bad)
sys.exit(1 if bad else 0)
`
	cmd := exec.Command(py, "-c", prog)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("force_wav_format 行为不符合预期：%v\n%s", err, out)
	}
}
