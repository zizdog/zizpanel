package services

import (
	"os"
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
		OutLog: "/tmp/out.log", ErrLog: "/tmp/err.log",
		Plist: "/Library/LaunchDaemons/com.zizdog.voicereceiver.plist",
	}

	local := receiverPlist(p, "zizdog", "ttsv-abc", "127.0.0.1")
	if !strings.Contains(local, "<string>127.0.0.1</string>") {
		t.Error("本机部署应能写成 127.0.0.1（只给同机的网站用，不暴露到局域网）")
	}
	if strings.Contains(local, "<string>0.0.0.0</string>") {
		t.Error("传了 127.0.0.1 就不该同时出现 0.0.0.0")
	}

	lan := receiverPlist(p, "zizdog", "ttsv-abc", "0.0.0.0")
	if !strings.Contains(lan, "<string>0.0.0.0</string>") {
		t.Error("mini 部署仍应是 0.0.0.0")
	}

	// 密钥、脚本路径、作业目录都要照旧写进去（模板改动不能碰坏这些）
	for _, want := range []string{"ttsv-abc", p.Script, p.Samples, p.Jobs,
		"com.zizdog.voicereceiver", "--jobs-dir", qwenUpstream} {
		if !strings.Contains(local, want) {
			t.Errorf("plist 里缺少 %q", want)
		}
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
		Jobs: dir + "/jobs", OutLog: dir + "/o.log", ErrLog: dir + "/e.log",
		Plist: dir + "/com.zizdog.voicereceiver.plist",
	}
	write := func(host string) {
		plist := receiverPlist(p, "zizdog", "ttsv-abc12345", host)
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

// TestQwenWarmPolicyRespectsMemory 锁住"内存不够就只常驻一个模型"。
//
// 背景：0.6B 时代两个模型都常驻约 4GB；换成 1.7B 后两个要 10GB 上下。
// 本机与 mini 都是 16GB，再叠加 Docker/MySQL/Ollama，全常驻会把机器拖进换页
// （实测本机 swap 曾用到 7GB/8GB）。交接文档的口径也是"吃紧就让它冷加载"。
func TestQwenWarmPolicyRespectsMemory(t *testing.T) {
	// 这两台真机都是 16GB → 不该尝试同时常驻两个 1.7B 模型
	gb := memoryGB()
	if gb > 0 && gb < qwenWarmAllMemGB {
		m := NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
		if m.canWarmAllQwenModels() {
			t.Errorf("本机 %dGB < %dGB，不该同时常驻两个模型（会把机器拖进换页）",
				gb, qwenWarmAllMemGB)
		}
	} else {
		t.Skipf("本机内存 %dGB，不适用于这条断言", gb)
	}
	// 门槛本身是常量，防止有人顺手调小到 16
	if qwenWarmAllMemGB < 20 {
		t.Errorf("门槛 %dGB 太低：两个 1.7B 常驻就要 ~10GB，留不出系统余量", qwenWarmAllMemGB)
	}
	// 默认模型必须是克隆用的 Base（网站当前的主用法）
	if !strings.Contains(qwenDefaultModel, "Base") {
		t.Errorf("默认模型应仍是 Base（克隆是主用法），实际 %s", qwenDefaultModel)
	}
}

// TestQwenWarmTargetsFallBackWithoutMemoryInfo：取不到内存信息时必须保守。
func TestQwenWarmTargetsFallBackWithoutMemoryInfo(t *testing.T) {
	m := NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
	// memoryGB() 依赖真机 sysctl，这里只断言"不 panic 且返回布尔"；
	// 真正的保守分支在 canWarmAllQwenModels 里（gb<=0 → false）。
	_ = m.canWarmAllQwenModels()
}
