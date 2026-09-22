package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ---------------------------------------------------------------------------
//  版本比较
// ---------------------------------------------------------------------------

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.2.0", "0.1.9", 1},
		{"0.1.9", "0.2.0", -1},
		{"1.0.0", "0.9.9", 1},
		{"1.2", "1.2.0", 0},           // 缺省段按 0
		{"1.2.1", "1.2", 1},           //
		{"v0.2.0", "0.2.0", 0},        // 允许 v 前缀
		{"1.0.0", "1.0.0-rc1", 1},     // 正式版大于预发布
		{"1.0.0-rc1", "1.0.0", -1},    //
		{"1.0.0-rc2", "1.0.0-rc1", 1}, // 预发布之间按数值比
		// 注意：semver 规定不含点的字母数字标识符按**字典序**比较，
		// 所以 rc10 < rc9。这不是 bug，是规范如此 —— 用点分隔才会按数值比。
		{"1.0.0-rc10", "1.0.0-rc9", -1},
		{"1.0.0-rc.10", "1.0.0-rc.9", 1},
		{"1.0.0-beta", "1.0.0-rc1", -1}, // 字母 vs 数字：数字标识符优先级更低
		{"2.0.0", "10.0.0", -1},         // 数值比较而非字典序
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil {
			t.Fatalf("CompareVersions(%q,%q) 报错: %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersionsRejectsGarbage(t *testing.T) {
	// 关键：无法解析时必须报错，绝不能悄悄判定成"有更新"。
	// 否则用户会被反复提示升级到一个装不上的包。
	for _, bad := range []string{"", "abc", "1.2.3.4", "1.x.0", "1..0"} {
		if _, err := CompareVersions(bad, "1.0.0"); err == nil {
			t.Errorf("CompareVersions(%q, ...) 应当报错，却成功了", bad)
		}
		if _, err := CompareVersions("1.0.0", bad); err == nil {
			t.Errorf("CompareVersions(..., %q) 应当报错，却成功了", bad)
		}
	}
}

// ---------------------------------------------------------------------------
//  清单与验签
// ---------------------------------------------------------------------------

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func goodManifest() []byte {
	m := Manifest{
		Version: "0.2.0",
		Notes:   "测试版本",
		Assets: map[string]ManifestRef{
			"darwin_arm64": {URL: "https://example.com/zizpanel_0.2.0_darwin_arm64.tar.gz",
				SHA256: strings.Repeat("a", 64), Size: 12345},
			"darwin_amd64": {URL: "https://example.com/zizpanel_0.2.0_darwin_amd64.tar.gz",
				SHA256: strings.Repeat("b", 64)},
		},
	}
	b, _ := json.Marshal(m)
	return b
}

func TestVerifyManifestAcceptsGoodSignature(t *testing.T) {
	pub, priv := testKey(t)
	old := PubKeyHex
	PubKeyHex = hex.EncodeToString(pub)
	defer func() { PubKeyHex = old }()

	data := goodManifest()
	sig := SignManifest(priv, data)
	if err := VerifyManifest(data, sig); err != nil {
		t.Fatalf("正确签名应当通过，却失败了: %v", err)
	}
	if _, err := ParseManifest(data); err != nil {
		t.Fatalf("清单应当可解析: %v", err)
	}
}

// TestVerifyManifestRejectsTamperedManifest 是整个功能最重要的一条断言。
//
// 场景：攻击者完全控制了下载源（或做了中间人），把清单里的 tar 包地址
// 换成了自己的恶意包。他没有私钥，所以签名对不上 —— 必须被拒绝。
func TestVerifyManifestRejectsTamperedManifest(t *testing.T) {
	pub, priv := testKey(t)
	old := PubKeyHex
	PubKeyHex = hex.EncodeToString(pub)
	defer func() { PubKeyHex = old }()

	data := goodManifest()
	sig := SignManifest(priv, data)

	// 篡改：把下载地址指向攻击者的服务器
	tampered := bytes.Replace(data, []byte("example.com"), []byte("evil.test"), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("测试自身有问题：替换没有生效")
	}
	if err := VerifyManifest(tampered, sig); err == nil {
		t.Fatal("被篡改的清单竟然通过了验签 —— 这是最严重的漏洞")
	}

	// 篡改：改版本号，诱导降级/升级到任意版本
	tampered2 := bytes.Replace(data, []byte(`"0.2.0"`), []byte(`"9.9.9"`), 1)
	if err := VerifyManifest(tampered2, sig); err == nil {
		t.Fatal("被篡改版本号的清单竟然通过了验签")
	}
}

func TestVerifyManifestRejectsWrongKey(t *testing.T) {
	_, priv := testKey(t)
	otherPub, _ := testKey(t)
	old := PubKeyHex
	PubKeyHex = hex.EncodeToString(otherPub)
	defer func() { PubKeyHex = old }()

	data := goodManifest()
	sig := SignManifest(priv, data)
	if err := VerifyManifest(data, sig); err == nil {
		t.Fatal("用不配对的公钥验签竟然通过了")
	}
}

func TestVerifyManifestRejectsBadSignatureLength(t *testing.T) {
	pub, _ := testKey(t)
	old := PubKeyHex
	PubKeyHex = hex.EncodeToString(pub)
	defer func() { PubKeyHex = old }()

	if err := VerifyManifest(goodManifest(), []byte("short")); err == nil {
		t.Fatal("长度不对的签名应当被拒绝")
	}
}

// TestFetchManifestFailsClosedWithoutKey 锁定"没有公钥就拒绝网络升级"。
// 如果这里退化成"没公钥就跳过验签"，整个升级通道就变成了远程代码执行入口。
func TestFetchManifestFailsClosedWithoutKey(t *testing.T) {
	old := PubKeyHex
	PubKeyHex = ""
	defer func() { PubKeyHex = old }()

	_, _, err := FetchManifest(context.Background(), "https://example.com")
	if !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("没有公钥时应当返回 ErrNoPublicKey，实际: %v", err)
	}
	if HasPublicKey() {
		t.Fatal("HasPublicKey 在未配置公钥时不应返回 true")
	}
}

func TestParseManifestRejectsUnknownFields(t *testing.T) {
	// 未知字段必须报错：将来清单格式升级时，旧面板要明确拒绝，
	// 而不是忽略掉它看不懂的安全约束。
	data := []byte(`{"version":"1.0.0","assets":{"darwin_arm64":{"url":"https://x/y","sha256":"` +
		strings.Repeat("a", 64) + `"}},"min_version":"0.5.0"}`)
	if _, err := ParseManifest(data); err == nil {
		t.Fatal("含未知字段的清单应当被拒绝")
	}
}

func TestParseManifestValidatesSHA(t *testing.T) {
	bad := []string{"", "xyz", strings.Repeat("a", 63), strings.Repeat("z", 64)}
	for _, s := range bad {
		data := []byte(fmt.Sprintf(`{"version":"1.0.0","assets":{"darwin_arm64":{"url":"https://x/y","sha256":%q}}}`, s))
		if _, err := ParseManifest(data); err == nil {
			t.Errorf("sha256=%q 应当被拒绝", s)
		}
	}
}

func TestManifestPickGivesHumanError(t *testing.T) {
	m := &Manifest{Version: "1.0.0", Assets: map[string]ManifestRef{
		"darwin_amd64": {URL: "https://x/y", SHA256: strings.Repeat("a", 64)},
	}}
	_, err := m.Pick("darwin", "arm64")
	if err == nil {
		t.Fatal("缺少对应架构时应当报错")
	}
	if !strings.Contains(err.Error(), "darwin_amd64") {
		t.Errorf("错误信息应当列出实际提供了哪些架构，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
//  解包安全
// ---------------------------------------------------------------------------

// makeTar 按给定条目构造 tar.gz。
func makeTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: e.typ}
		if e.typ == tar.TypeSymlink {
			// 符号链接条目的"内容"是链接目标，且 Size 必须为 0，
			// 否则 tar 会直接报 "write too long"
			hdr.Linkname = e.body
			hdr.Size = 0
		} else {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ != tar.TypeSymlink {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type tarEntry struct {
	name string
	body string
	typ  byte
}

func TestExtractBinariesHappyPath(t *testing.T) {
	p := makeTar(t, []tarEntry{
		{name: "./zizpanel", body: "panel", typ: tar.TypeReg},
		{name: "./zizpanel-helper", body: "helper", typ: tar.TypeReg},
		{name: "./install.sh", body: "installer", typ: tar.TypeReg},
		{name: "./tools/server-mode.sh", body: "tool", typ: tar.TypeReg},
	})
	dest := t.TempDir()
	got, err := ExtractBinaries(p, dest, []string{PanelBinary, HelperBinary})
	if err != nil {
		t.Fatalf("正常包应当解出成功: %v", err)
	}
	for _, n := range []string{PanelBinary, HelperBinary} {
		if _, ok := got[n]; !ok {
			t.Fatalf("缺少 %s", n)
		}
		if _, err := os.Stat(filepath.Join(dest, n)); err != nil {
			t.Fatalf("%s 没有被解出来: %v", n, err)
		}
	}
	// 只解需要的文件，不要把 install.sh 之类也摊到暂存目录里
	if _, err := os.Stat(filepath.Join(dest, "install.sh")); err == nil {
		t.Error("不应解出 install.sh")
	}
}

func TestExtractBinariesRejectsPathTraversal(t *testing.T) {
	p := makeTar(t, []tarEntry{
		{name: "../../../../tmp/evil-zizpanel", body: "pwned", typ: tar.TypeReg},
		{name: "./zizpanel", body: "panel", typ: tar.TypeReg},
		{name: "./zizpanel-helper", body: "helper", typ: tar.TypeReg},
	})
	if _, err := ExtractBinaries(p, t.TempDir(), []string{PanelBinary, HelperBinary}); err == nil {
		t.Fatal("含路径穿越的包必须被拒绝")
	}
}

func TestExtractBinariesRejectsSymlink(t *testing.T) {
	p := makeTar(t, []tarEntry{
		{name: "./zizpanel", body: "/etc/passwd", typ: tar.TypeSymlink},
		{name: "./zizpanel-helper", body: "helper", typ: tar.TypeReg},
	})
	if _, err := ExtractBinaries(p, t.TempDir(), []string{PanelBinary, HelperBinary}); err == nil {
		t.Fatal("含符号链接的包必须被拒绝")
	}
}

func TestExtractBinariesRequiresAllWanted(t *testing.T) {
	p := makeTar(t, []tarEntry{{name: "./zizpanel", body: "panel", typ: tar.TypeReg}})
	if _, err := ExtractBinaries(p, t.TempDir(), []string{PanelBinary, HelperBinary}); err == nil {
		t.Fatal("缺少提权助手的包必须被拒绝")
	}
}

// TestExtractBinariesOptionalKeepsOlderPackagesWorking：可选模块（zizvideo）是后加的 ——
// 用老发布包升级（里面没有它）不该失败；带它的包则必须把它解出来。
func TestExtractBinariesOptionalKeepsOlderPackagesWorking(t *testing.T) {
	older := makeTar(t, []tarEntry{
		{name: "./zizpanel", body: "panel", typ: tar.TypeReg},
		{name: "./zizpanel-helper", body: "helper", typ: tar.TypeReg},
	})
	dest := t.TempDir()
	got, err := ExtractBinariesOptional(older, dest,
		[]string{PanelBinary, HelperBinary}, []string{ZizvideoBinary})
	if err != nil {
		t.Fatalf("老包（没有可选模块）不该报错: %v", err)
	}
	if _, ok := got[ZizvideoBinary]; ok {
		t.Error("老包里没有 zizvideo，不该凭空多出它")
	}

	newer := makeTar(t, []tarEntry{
		{name: "./zizpanel", body: "panel", typ: tar.TypeReg},
		{name: "./zizpanel-helper", body: "helper", typ: tar.TypeReg},
		{name: "./zizvideo", body: "zizvideo", typ: tar.TypeReg},
	})
	dest2 := t.TempDir()
	got2, err := ExtractBinariesOptional(newer, dest2,
		[]string{PanelBinary, HelperBinary}, []string{ZizvideoBinary})
	if err != nil {
		t.Fatalf("带可选模块的包应当解出成功: %v", err)
	}
	if _, ok := got2[ZizvideoBinary]; !ok {
		t.Fatal("带 zizvideo 的包必须把它解出来")
	}
	if _, err := os.Stat(filepath.Join(dest2, ZizvideoBinary)); err != nil {
		t.Fatalf("zizvideo 没有被落盘: %v", err)
	}
}

// ---------------------------------------------------------------------------
//  状态与看门狗结果
// ---------------------------------------------------------------------------

func TestLoadStateAppliesWatchdogSuccess(t *testing.T) {
	w := t.TempDir()
	if err := SaveState(w, &State{
		Status: StatusRestarting, From: "0.1.0", To: "0.2.0", RunID: "run-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteResult(w, "run-1", StatusSuccess, "0.2.0", "新版已通过健康检查"); err != nil {
		t.Fatal(err)
	}
	st := LoadState(w)
	if st.Status != StatusSuccess {
		t.Fatalf("看门狗写了 success，状态应为 success，实际 %s", st.Status)
	}
	if st.To != "0.2.0" {
		t.Errorf("版本应为 0.2.0，实际 %q", st.To)
	}
	// 幂等：结果文件被消费后不应反复出现
	if _, err := os.Stat(resultPath(w)); err == nil {
		t.Error("结果文件应当已被消费删除")
	}
	st2 := LoadState(w)
	if st2.Status != StatusSuccess {
		t.Errorf("第二次读取状态发生了变化：%s", st2.Status)
	}
}

func TestLoadStateAppliesWatchdogRollback(t *testing.T) {
	w := t.TempDir()
	if err := SaveState(w, &State{
		Status: StatusRestarting, From: "0.1.0", To: "0.2.0", RunID: "run-2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteResult(w, "run-2", StatusRolledBack, "0.1.0", "新版未通过健康检查"); err != nil {
		t.Fatal(err)
	}
	st := LoadState(w)
	if st.Status != StatusRolledBack {
		t.Fatalf("应识别为已回滚，实际 %s", st.Status)
	}
	if st.To != "0.1.0" {
		t.Errorf("回滚后的版本应为 0.1.0，实际 %q", st.To)
	}
}

func TestSaveStateIsAtomicAndReloadable(t *testing.T) {
	w := t.TempDir()
	st := &State{Status: StatusApplying, From: "0.1.0", To: "0.2.0", StartedAt: time.Now()}
	if err := SaveState(w, st); err != nil {
		t.Fatal(err)
	}
	got := LoadState(w)
	if got.Status != StatusApplying || got.To != "0.2.0" {
		t.Fatalf("状态回读不一致: %+v", got)
	}
	// 不能留下临时文件
	if _, err := os.Stat(statePath(w) + ".tmp"); err == nil {
		t.Error("不应该残留 .tmp 文件")
	}
}

// ---------------------------------------------------------------------------
//  应用与回滚
// ---------------------------------------------------------------------------

// fakeBinary 写一个能被当成"二进制"执行的假程序。
// 用 bash 脚本而不是真二进制：测试要能在任何机器上跑，
// 而且我们要能精确控制它"报告什么版本""自检过不过"。
func fakeBinary(t *testing.T, dir, name, version string, selfTestOK bool) string {
	t.Helper()
	body := fmt.Sprintf(`#!/bin/bash
case "$1" in
  version)  echo '{"version":"%s","commit":"test"}' ;;
  selftest) echo '{"ok":%t}' ;;
  *) exit 0 ;;
esac
`, version, selfTestOK)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	opt := Options{
		Root:      root,
		BinDir:    filepath.Join(root, "bin"),
		WorkDir:   filepath.Join(root, "work"),
		PlistDir:  filepath.Join(root, "LaunchDaemons"),
		Label:     "cn.zizpanel.panel",
		HealthURL: "https://127.0.0.1:8443/api/v1/health",
	}
	// 模块刷新默认会碰真实 /opt 与 launchd —— 单测一律钉成假的（AGENTS 第三节）。
	// 需要验证展示的测试自己再覆盖它。
	opt.RefreshModules = func(context.Context, string) []services.ModuleRefreshResult { return nil }
	for _, d := range []string{opt.BinDir, opt.WorkDir, opt.PlistDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return opt
}

func stageDirOf(opt Options) string {
	d := filepath.Join(opt.WorkDir, "upgrade", "staging")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// TestApplyAbortsBeforeTouchingOldBinariesWhenSelfTestFails 是最关键的一条。
//
// 新二进制自检不通过时，安装目录里的旧二进制必须**一个字节都没变**。
// 如果这里出错，用户会得到一个跑不起来的面板 —— 而这本来是可以避免的。
func TestApplyAbortsBeforeTouchingOldBinariesWhenSelfTestFails(t *testing.T) {
	opt := newTestOptions(t)
	oldPanel := fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true)
	oldHelper := fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)
	before := snapshot(t, opt.BinDir)

	// 新版：能跑，但报告的版本号不对（模拟装错了包/包不完整）
	st := stageDirOf(opt)
	staged := map[string]string{
		PanelBinary:  fakeBinary(t, st, PanelBinary, "0.9.9", true),
		HelperBinary: fakeBinary(t, st, HelperBinary, "0.2.0", true),
	}

	err := Apply(context.Background(), opt, staged, "0.1.0", "0.2.0", "remote")
	if err == nil {
		t.Fatal("版本号不匹配时 Apply 应当返回错误")
	}
	if after := snapshot(t, opt.BinDir); after != before {
		t.Fatal("自检失败后旧二进制被改动了 —— 这会让面板直接不可用")
	}
	state := LoadState(opt.WorkDir)
	if state.Status != StatusFailed {
		t.Fatalf("状态应为 failed，实际 %s", state.Status)
	}
	if !strings.Contains(state.Message, "旧版本未做任何改动") {
		t.Errorf("应当明确告知旧版本未被改动，实际: %s", state.Message)
	}
	// 自检阶段就失败，绝不能留下 .bak（那会污染下一次升级的备份）
	if _, err := os.Stat(oldPanel + ".bak"); err == nil {
		t.Error("自检阶段失败不应产生 .bak 备份")
	}
	_ = oldHelper
}

// TestApplyAbortsWithoutRestartingWhenWatchdogCannotStart 锁定另一个危险情形：
// 看门狗起不来时**绝不能重启自己** —— 否则新版万一有问题就没人回滚了。
func TestApplyAbortsWithoutRestartingWhenWatchdogCannotStart(t *testing.T) {
	opt := newTestOptions(t)
	fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true)
	fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)

	st := stageDirOf(opt)
	staged := map[string]string{
		PanelBinary:  fakeBinary(t, st, PanelBinary, "0.2.0", true),
		HelperBinary: fakeBinary(t, st, HelperBinary, "0.2.0", true),
	}
	beforePanel := readFile(t, filepath.Join(opt.BinDir, PanelBinary))
	beforeHelper := readFile(t, filepath.Join(opt.BinDir, HelperBinary))

	// 只拦截 launchctl，其余命令照常真跑 —— 否则自检阶段的
	// `version --json` 拿不到输出，测试会错误地停在自检而不是看门狗那一步。
	var calls []string
	opt.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "launchctl" {
			if len(args) > 0 && args[0] == "bootstrap" {
				return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
			}
			return []byte(""), nil
		}
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}

	err := Apply(context.Background(), opt, staged, "0.1.0", "0.2.0", "remote")
	if err == nil {
		t.Fatal("看门狗加载失败时 Apply 应当返回错误")
	}
	for _, c := range calls {
		if strings.Contains(c, "kickstart") {
			t.Fatal("看门狗都没起来就重启了面板 —— 新版失败时将无人回滚")
		}
	}
	// 注意：这里比较的是"在用的两个二进制"，不是整个目录 ——
	// 回滚后 *.bak 备份文件仍然存在是正常的，不该算作失败。
	if got := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); got != beforePanel {
		t.Fatal("看门狗失败后应当恢复旧的面板二进制")
	}
	if got := readFile(t, filepath.Join(opt.BinDir, HelperBinary)); got != beforeHelper {
		t.Fatal("看门狗失败后应当恢复旧的提权助手")
	}
	state := LoadState(opt.WorkDir)
	if state.Status != StatusRolledBack {
		t.Fatalf("状态应为 rolled_back，实际 %s", state.Status)
	}
	if !strings.Contains(state.Message, "未重启") {
		t.Errorf("应当说明未重启，实际: %s", state.Message)
	}
}

// TestApplyHappyPathSwapsAndRestarts 验证正常路径：
// 二进制被换掉、看门狗被加载、最后才 kickstart。
func TestApplyHappyPathSwapsAndRestarts(t *testing.T) {
	opt := newTestOptions(t)
	oldPanelBody := readFile(t, fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true))
	fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)

	st := stageDirOf(opt)
	staged := map[string]string{
		PanelBinary:  fakeBinary(t, st, PanelBinary, "0.2.0", true),
		HelperBinary: fakeBinary(t, st, HelperBinary, "0.2.0", true),
	}

	var order []string
	opt.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			order = append(order, args[0])
			return []byte(""), nil
		}
		order = append(order, name)
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}

	if err := Apply(context.Background(), opt, staged, "0.1.0", "0.2.0", "remote"); err != nil {
		t.Fatalf("正常路径不应出错: %v", err)
	}

	// 二进制确实被换了
	if now := readFile(t, filepath.Join(opt.BinDir, PanelBinary)); now == oldPanelBody {
		t.Error("面板二进制没有被替换")
	}
	// 备份存在，且内容等于旧版本
	bak := readFile(t, filepath.Join(opt.BinDir, PanelBinary+".bak"))
	if bak != string(oldPanelBody) {
		t.Error("备份内容应当等于升级前的旧二进制")
	}
	// 看门狗脚本与 plist 都生成了
	if _, err := os.Stat(watchdogScriptPath(opt)); err != nil {
		t.Errorf("看门狗脚本未生成: %v", err)
	}
	if _, err := os.Stat(watchdogPlistPath(opt)); err != nil {
		t.Errorf("看门狗 plist 未生成: %v", err)
	}
	// 顺序必须是「先加载看门狗，后重启面板」
	bootIdx, kickIdx := indexOf(order, "bootstrap"), indexOf(order, "kickstart")
	if bootIdx < 0 || kickIdx < 0 {
		t.Fatalf("应同时出现 bootstrap 与 kickstart，实际调用：%v", order)
	}
	if bootIdx > kickIdx {
		t.Fatalf("必须先启动看门狗再重启面板，实际顺序：%v", order)
	}

	state := LoadState(opt.WorkDir)
	if state.Status != StatusRestarting {
		t.Fatalf("重启后状态应为 restarting（结论由看门狗给出），实际 %s", state.Status)
	}
}

// TestApplyInstallsBundledZizvideoAndReadsItBack：随包模块（zizvideo）必须真的落到
// <BinDir>/zizvideo 且 `--version` 能跑；缺了/跑不起来要红。
//
// 为什么必须锁：make release 早就在包里放了 ./zizvideo，但升级只 swap 面板与助手 ——
// 结果升级到新版的机器上，市场里点安装 zizvideo 会找不到源文件（判据贴运行体，坑 161 同类）。
func TestApplyInstallsBundledZizvideoAndReadsItBack(t *testing.T) {
	opt := newTestOptions(t)
	fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true)
	fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)

	st := stageDirOf(opt)
	staged := map[string]string{
		PanelBinary:  fakeBinary(t, st, PanelBinary, "0.2.0", true),
		HelperBinary: fakeBinary(t, st, HelperBinary, "0.2.0", true),
	}
	// 真的会打印版本、且带执行位的"模块二进制"。
	mod := filepath.Join(st, ZizvideoBinary)
	if err := os.WriteFile(mod, []byte("#!/bin/bash\necho 'zizvideo 0.1.0-mvp'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged[ZizvideoBinary] = mod

	opt.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte(""), nil
		}
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	if err := Apply(context.Background(), opt, staged, "0.1.0", "0.2.0", "remote"); err != nil {
		t.Fatalf("带随包模块的正常升级不应出错: %v", err)
	}

	dst := filepath.Join(opt.BinDir, ZizvideoBinary)
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("升级后 %s 必须存在（随包模块没有自动就位）: %v", dst, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s 缺可执行位：%v", dst, info.Mode().Perm())
	}
	out, err := exec.Command(dst, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version 跑不起来: %v（%s）", dst, err, out)
	}
	if !strings.Contains(string(out), "zizvideo") {
		t.Errorf("--version 输出不对：%q", out)
	}
}

// TestApplyRollsBackWhenBundledModuleCannotRun：包里带了模块但装完跑不起来（损坏）⇒
// 必须回滚并在状态里如实说明，不许静默继续。
func TestApplyRollsBackWhenBundledModuleCannotRun(t *testing.T) {
	opt := newTestOptions(t)
	fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true)
	fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)

	st := stageDirOf(opt)
	staged := map[string]string{
		PanelBinary:  fakeBinary(t, st, PanelBinary, "0.2.0", true),
		HelperBinary: fakeBinary(t, st, HelperBinary, "0.2.0", true),
	}
	// 自检阶段就用 `--version` 拦下它（无输出），所以这里造一个"哑"模块。
	mod := filepath.Join(st, ZizvideoBinary)
	if err := os.WriteFile(mod, []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged[ZizvideoBinary] = mod

	opt.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte(""), nil
		}
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	err := Apply(context.Background(), opt, staged, "0.1.0", "0.2.0", "remote")
	if err == nil {
		t.Fatal("随包模块自检/回读失败时必须返回错误")
	}
	state := LoadState(opt.WorkDir)
	if state.Status != StatusFailed {
		t.Fatalf("自检阶段失败的状态应为 failed，实际 %s", state.Status)
	}
	if _, statErr := os.Stat(filepath.Join(opt.BinDir, ZizvideoBinary)); statErr == nil {
		t.Error("自检失败时不该把模块放进安装目录")
	}
}

// TestApplyRecordsModuleRefreshOutcomes：升级结果里必须如实呈现"哪个模块刷新了 /
// 哪个跳过了 / 哪个失败并回滚了"（坑 216）。前端读的就是 state.logs 与 state.steps，
// 所以这里断言的也是这两条通道 —— 不需要改 JS。
func TestApplyRecordsModuleRefreshOutcomes(t *testing.T) {
	opt := newTestOptions(t)
	fakeBinary(t, opt.BinDir, PanelBinary, "0.1.0", true)
	fakeBinary(t, opt.BinDir, HelperBinary, "0.1.0", true)
	st := stageDirOf(opt)
	staged := map[string]string{
		PanelBinary:  fakeBinary(t, st, PanelBinary, "0.2.0", true),
		HelperBinary: fakeBinary(t, st, HelperBinary, "0.2.0", true),
	}
	opt.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte(""), nil
		}
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	opt.RefreshModules = func(context.Context, string) []services.ModuleRefreshResult {
		return []services.ModuleRefreshResult{
			{Name: "zizvideo", Status: services.ModuleRefreshed,
				Reason:  "已替换并重启，自检：zizvideo 0.2.0",
				FromSHA: strings.Repeat("a", 64), ToSHA: strings.Repeat("b", 64)},
			{Name: "widget", Status: services.ModuleRolledBack,
				Reason:  "自检未通过：exit status 1；已回滚旧二进制",
				FromSHA: strings.Repeat("c", 64), ToSHA: strings.Repeat("d", 64)},
			{Name: "gizmo", Status: services.ModuleSkipped, Reason: "未安装 gizmo"},
		}
	}

	if err := Apply(context.Background(), opt, staged, "0.1.0", "0.2.0", "remote"); err != nil {
		t.Fatalf("模块刷新结果不该让面板升级失败: %v", err)
	}

	state := LoadState(opt.WorkDir)
	byName := map[string]ModuleRefresh{}
	for _, m := range state.Modules {
		byName[m.Name] = m
	}
	if len(byName) != 3 {
		t.Fatalf("应记录 3 个模块结果，实际 %+v", state.Modules)
	}
	if m := byName["zizvideo"]; m.Status != services.ModuleRefreshed ||
		m.FromSHA != strings.Repeat("a", 12) || m.ToSHA != strings.Repeat("b", 12) {
		t.Errorf("zizvideo 结果不对: %+v", m)
	}
	if m := byName["widget"]; m.Status != services.ModuleRolledBack {
		t.Errorf("widget 应为 rolled-back: %+v", m)
	}
	if m := byName["gizmo"]; m.Status != services.ModuleSkipped {
		t.Errorf("gizmo 应为 skipped: %+v", m)
	}

	// 用户可见通道（前端渲染 state.logs / state.steps）。
	var logged strings.Builder
	for _, l := range state.Logs {
		logged.WriteString(l.Text + "\n")
	}
	for _, want := range []string{"已刷新并重启", "已回滚旧二进制", "已跳过"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("升级日志应包含 %q，实际：\n%s", want, logged.String())
		}
	}
	var stepped bool
	for _, s := range state.Steps {
		if s.Stage == "刷新模块 zizvideo" && s.OK {
			stepped = true
		}
		if s.Stage == "刷新模块 widget" && s.OK {
			t.Errorf("回滚的模块不该记成成功：%+v", s)
		}
	}
	if !stepped {
		t.Errorf("应有一条成功的模块刷新步骤，实际：%+v", state.Steps)
	}
}

// TestWatchdogScriptHasSafetyRequisites 检查生成脚本的关键要素。
// 这些字符串一旦丢失，看门狗就会"看起来在跑但不干活"，
// 而那正是升级失败时唯一的救命绳。
func TestWatchdogScriptHasSafetyRequisites(t *testing.T) {
	opt := newTestOptions(t)
	s := watchdogScript(opt, "test-run", "0.1.0", "0.2.0")

	must := []string{
		"0.2.0",                                // 期望的新版本
		"0.1.0",                                // 回滚目标
		opt.BinDir,                             // 操作目录
		"$name.bak",                            // 从备份还原（循环变量拼接）
		"for name in zizpanel zizpanel-helper", // 两个必需二进制都要还原
		"zizvideo.bak",                         // 可选模块（随包分发）也要还原/清理
		"launchctl kickstart",                  // 回滚后重启
		"rolled_back",                          // 失败结论
		"success",                              // 成功结论
		"launchctl bootout",                    // 自清理
		resultPath(opt.WorkDir),                // 结果落盘
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("看门狗脚本缺少关键内容 %q", m)
		}
	}
	// 脚本必须是合法 bash
	if out, err := runBash(t, s); err != nil {
		t.Errorf("看门狗脚本语法错误: %v\n%s", err, out)
	}
}

func TestWatchdogPlistIsWellFormed(t *testing.T) {
	opt := newTestOptions(t)
	p := watchdogPlist(opt, "/tmp/watchdog.sh")
	for _, m := range []string{WatchdogLabel, "<key>RunAtLoad</key>", "/bin/bash", "/tmp/watchdog.sh"} {
		if !strings.Contains(p, m) {
			t.Errorf("plist 缺少 %q", m)
		}
	}
	// KeepAlive 必须为 false：看门狗跑一次就结束，
	// 如果被反复拉起，回滚逻辑会被重复执行。
	if !strings.Contains(p, "<key>KeepAlive</key>\n    <false/>") {
		t.Error("看门狗不应设置 KeepAlive")
	}
}

// ---------------------------------------------------------------------------
//  辅助
// ---------------------------------------------------------------------------

func snapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		h := sha256.Sum256([]byte(readFile(t, filepath.Join(dir, e.Name()))))
		fmt.Fprintf(&b, "%s=%s\n", e.Name(), hex.EncodeToString(h[:]))
	}
	return b.String()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

func runBash(t *testing.T, script string) (string, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := execCommand("bash", "-n", p)
	return string(out), err
}

// execCommand 是 os/exec 的薄封装，避免测试文件里散落 import。
func execCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// TestDownloadTarballReportsProgress 锁住"下载进度回调"这条新增能力。
//
// 为什么值得测：用户明确要求"升级过程要有详细的内容展示"，
// 而进度回调是那条要求唯一的数据来源。回调漏调/重复计数都会让界面上的
// 百分比说谎（显示"下完了"或一直停在 0%），而升级界面恰恰是用户最紧张的地方。
func TestDownloadTarballReportsProgress(t *testing.T) {
	payload := bytes.Repeat([]byte("zizpanel"), 4096) // 32 KiB
	sum := sha256.Sum256(payload)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	var (
		lastWritten int64
		lastTotal   int64
		calls       int
	)
	dest := filepath.Join(t.TempDir(), "pkg.tar.gz")
	got, err := DownloadTarballWithProgress(context.Background(), ts.URL, dest,
		hex.EncodeToString(sum[:]), func(written, total int64) {
			calls++
			if written < lastWritten {
				t.Errorf("进度回调的字节数回退了：%d → %d", lastWritten, written)
			}
			lastWritten, lastTotal = written, total
		})
	if err != nil {
		t.Fatalf("下载失败：%v", err)
	}
	if got != dest {
		t.Errorf("返回路径应为 %s，实际 %s", dest, got)
	}
	if calls == 0 {
		t.Fatal("一次进度回调都没有 —— 界面上会永远停在「正在下载」")
	}
	if lastWritten != int64(len(payload)) {
		t.Errorf("最后一次回调应报告完整字节数 %d，实际 %d", len(payload), lastWritten)
	}
	if lastTotal != int64(len(payload)) {
		t.Errorf("total 应为服务端声明的大小 %d，实际 %d", len(payload), lastTotal)
	}
	// 没有回调（nil）时也必须照常工作：DownloadTarball 是它的包装。
	dest2 := filepath.Join(t.TempDir(), "pkg2.tar.gz")
	if _, err := DownloadTarball(context.Background(), ts.URL, dest2, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("不带进度回调的下载也必须成功：%v", err)
	}
}

// TestDownloadTarballWithProgressRejectsBadChecksum：带进度的路径同样要做校验，
// 不能因为"边下边报进度"就把校验漏掉（漏掉等于允许装一个坏包）。
func TestDownloadTarballWithProgressRejectsBadChecksum(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not the expected bytes"))
	}))
	defer ts.Close()
	dest := filepath.Join(t.TempDir(), "pkg.tar.gz")
	_, err := DownloadTarballWithProgress(context.Background(), ts.URL, dest,
		strings.Repeat("ab", 32), func(int64, int64) {})
	if err == nil {
		t.Fatal("校验和不匹配时必须报错")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("校验失败时不该留下目标文件（升级会拿它去替换面板）")
	}
}
