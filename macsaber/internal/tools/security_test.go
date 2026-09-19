package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

// 本文件全部用 PATH shim 构造缺失/失败分支；真实命令只在确认存在时才跑。
// 断言口径：负面判定（未签名/被拒）是**结果**，不许当成工具错误。

// ---------- 纯解析：负面判定如实呈现 ----------

func TestSecKVAndFlagsParsing(t *testing.T) {
	out := "Executable=/bin/ls\nIdentifier=com.apple.ls\nCodeDirectory v=20400 size=150 flags=0x10000(runtime) hashes=1\nTeamIdentifier=not set\nAuthority=Software Signing\nAuthority=Apple Root CA\n"
	kv := kvLines(out)
	if got := firstKV(kv, "Identifier"); got != "com.apple.ls" {
		t.Errorf("Identifier = %q", got)
	}
	if len(kv["Authority"]) != 2 {
		t.Errorf("Authority 应有两行，实际 %v", kv["Authority"])
	}
	if got := firstKV(kv, "TeamIdentifier"); got != "not set" {
		t.Errorf("TeamIdentifier = %q", got)
	}
	if !flagsHas(firstKV(kv, "CodeDirectory"), 0x10000) {
		t.Error("flags=0x10000 应被识别出 runtime/公证位（坑 S3）")
	}
	if flagsHas("flags=0x2000(library-validation)", 0x10000) {
		t.Error("0x2000 不该被当成公证位")
	}
	if flagsHas("不是flags", 0x10000) {
		t.Error("解析失败时不该返回 true")
	}
}

func TestSecUnsignedDetection(t *testing.T) {
	cases := []struct {
		name string
		res  *execx.Result
		want bool
	}{
		{"未签名报错文本", &execx.Result{ExitCode: 1, Stderr: "/tmp/a: code object is not signed at all\n"}, true},
		{"正常签名", &execx.Result{ExitCode: 0, Stdout: "Identifier=com.apple.x\n"}, false},
		{"其它失败不算未签名", &execx.Result{ExitCode: 1, Stderr: "some other error"}, false},
	}
	for _, tc := range cases {
		if got := isUnsignedCodesign(tc.res); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestSecSpctlVerdict(t *testing.T) {
	cases := []struct {
		name string
		res  *execx.Result
		want string
	}{
		{"接受", &execx.Result{ExitCode: 0, Stdout: "/x: accepted\nsource=Apple System\n"}, "accepted"},
		{"拒绝(退出码3)", &execx.Result{ExitCode: 3, Stderr: "/x: rejected\nsource=no usable signature\n"}, "rejected"},
		{"拒绝(文本)", &execx.Result{ExitCode: 3, Stderr: "a: rejected\n"}, "rejected"},
		{"无法判定", &execx.Result{ExitCode: 9, Stderr: "unexpected"}, "unknown"},
	}
	for _, tc := range cases {
		if got := spctlVerdict(tc.res); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestSecIdentityParsing(t *testing.T) {
	blob := "  1) 42C3674D300BCB01D984F807868A22459050D1BF \"ZizPanel Release\"\n     1 valid identities found\n"
	set := identitySet(blob)
	if !set["42C3674D300BCB01D984F807868A22459050D1BF"] {
		t.Fatalf("应解析出身份指纹，实际 %v", set)
	}
	if len(set) != 1 {
		t.Errorf("汇总行不该被当成身份，实际 %v", set)
	}
	if got := len(identitySet("     0 valid identities found\n")); got != 0 {
		t.Errorf("0 个身份应返回空集，实际 %d", got)
	}
}

// ---------- 只读契约：不得出现任何私钥/写操作口径 ----------

func TestSecSourcesAreReadOnly(t *testing.T) {
	src := readFileForTest(t, "security.go")
	forbidden := []string{
		"import-certificate", "add-certificates", "delete-certificate",
		"security add", "security delete", "security import",
		"csrutil enable", "csrutil disable", "spctl --add", "spctl --enable",
		"installer -", "pkgutil --expand", "codesign -s", "codesign --sign",
	}
	for _, f := range forbidden {
		if strings.Contains(src, f) {
			t.Errorf("只读工具里出现了写操作 %q", f)
		}
	}
}

func readFileForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return string(b)
}

// runTaskErr / runTaskOK 复用 automation_test.go 的脚手架；security 侧只用到快照。
func secSnap(t *testing.T, b *bench, id string, body map[string]any) tasks.Snapshot {
	t.Helper()
	_, snap := b.run(id, body)
	return snap
}

func secData(t *testing.T, snap tasks.Snapshot) map[string]any {
	t.Helper()
	if snap.Status != tasks.Succeeded {
		t.Fatalf("任务应成功，实际 %s（%s）", snap.Status, snap.Error)
	}
	res, ok := snap.Result.(*tool.Result)
	if !ok || res == nil {
		t.Fatalf("任务结果类型不符：%#v", snap.Result)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("结果 data 类型不符：%#v", res.Data)
	}
	return data
}

// ---------- 元数据：分类/可用性/只读无 danger ----------
func TestSecToolsMeta(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAll(reg)
	wantIDs := []string{"sec.verify", "sec.certificates", "sec.pkg_info", "sec.gatekeeper_status"}
	for _, id := range wantIDs {
		m := metaOf(t, reg, id)
		if m.Category != "sec" {
			t.Errorf("%s: 分类应为 sec，实际 %s", id, m.Category)
		}
		if m.Danger {
			t.Errorf("%s: 只读工具不该标 danger", id)
		}
		if m.Async != true {
			t.Errorf("%s: 都走任务中心（子进程类），应 async=true", id)
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s: summary 超过 40 字：%q", id, m.Summary)
		}
	}
	if m := metaOf(t, reg, "sec.verify"); !m.Available {
		if m.UnavailableReason == "" {
			t.Error("不可用必须给真实原因")
		}
	}
}

// ---------- 缺命令 → 不可用且原因点出命令名 ----------

func TestSecAvailabilityUnderEmptyPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	reg := tool.NewRegistry()
	RegisterAll(reg)
	for _, tc := range []struct{ id, bin string }{
		{"sec.verify", "codesign"},
		{"sec.certificates", "security"},
		{"sec.pkg_info", "pkgutil"},
		{"sec.gatekeeper_status", "spctl"},
	} {
		m := metaOf(t, reg, tc.id)
		if m.Available {
			t.Errorf("%s: 空 PATH 下不该可用", tc.id)
		}
		if !strings.Contains(m.UnavailableReason, tc.bin) {
			t.Errorf("%s: 原因应点出 %s，实际 %q", tc.id, tc.bin, m.UnavailableReason)
		}
	}
}

// ---------- sec.verify：真机跑签名 App 与未签名文件 ----------

func TestSecVerifyRealSignedAndUnsigned(t *testing.T) {
	if _, ok := execx.LookPath("codesign"); !ok {
		t.Skip("本机没有 codesign")
	}
	if _, ok := execx.LookPath("spctl"); !ok {
		t.Skip("本机没有 spctl")
	}
	// 必须挑**不是软链接**的 App：Safari 之类软链接到 /System，会被敏感目录闸门挡住（坑 A3）。
	signed := signedAppOutsideSystem(t)
	if signed == "" {
		t.Skip("本机 /Applications 下没有直接的签名 App")
	}
	b := newBenchWithRoots(t, []string{"/Applications"})
	snap := secSnap(t, b, "sec.verify", map[string]any{"path": signed})
	data := secData(t, snap)
	if data["signed"] != true {
		t.Fatalf("系统 App 应判定为已签名：%v", data)
	}
	if s, _ := data["signer"].(string); s == "" || strings.Contains(s, "未知") {
		t.Errorf("应给出签名者，实际 %v", data["signer"])
	}
	gk, ok := data["gatekeeper"].(map[string]any)
	if !ok {
		t.Fatal("应给出 gatekeeper 判定")
	}
	if v, _ := gk["verdict"].(string); v != "accepted" && v != "rejected" && v != "unknown" {
		t.Errorf("判定词非法：%v", gk["verdict"])
	}

	// 未签名文件：必须**正常返回**并如实说没签名，不是工具失败。
	plain := filepath.Join(t.TempDir(), "unsigned.txt")
	if err := os.WriteFile(plain, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	b2 := newBenchWithRoots(t, []string{filepath.Dir(plain)})
	snap2 := secSnap(t, b2, "sec.verify", map[string]any{"path": plain})
	d2 := secData(t, snap2)
	res2 := snap2.Result.(*tool.Result)
	if d2["signed"] != false {
		t.Fatalf("未签名文件应判定为未签名：%v", d2)
	}
	gk2 := d2["gatekeeper"].(map[string]any)
	if v, _ := gk2["verdict"].(string); v != "rejected" {
		t.Errorf("未签名文件 Gatekeeper 应 rejected，实际 %v", gk2["verdict"])
	}
	if res2.Msg == "" || !strings.Contains(res2.Msg, "没有代码签名") {
		t.Errorf("消息应如实说没签名，实际 %q", res2.Msg)
	}
}

// signedAppOutsideSystem 找一个真实路径不在 /System 下的签名 App。
func signedAppOutsideSystem(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir("/Applications")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".app") {
			continue
		}
		p := filepath.Join("/Applications", e.Name())
		if _, err := filepath.EvalSymlinks(p); err != nil {
			continue
		}
		real, _ := filepath.EvalSymlinks(p)
		if strings.HasPrefix(real, "/System") {
			continue
		}
		if res := execx.New().Run(context.Background(), 10*time.Second, "codesign", "-dv", p); res.ExitCode == 0 {
			return p
		}
	}
	return ""
}

// ---------- sec.verify：codesign 失败（非"未签名"）→ 报错带 stderr ----------

func TestSecVerifyCodesignHardFailure(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, "codesign", "#!/bin/sh\necho 'x: 磁盘读取错误' >&2\nexit 8\n")
	writeShim(t, dir, "spctl", "#!/bin/sh\nexit 3\n")
	t.Setenv("PATH", dir)
	b := newBench(t)
	f := filepath.Join(b.home, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap := b.run("sec.verify", map[string]any{"path": f})
	if snap.Status != tasks.Failed {
		t.Fatalf("codesign 硬失败任务应失败，实际 %s", snap.Status)
	}
	if !strings.Contains(snap.Error, "磁盘读取错误") {
		t.Errorf("错误应带真实 stderr 关键行，实际 %q", snap.Error)
	}
	if strings.Contains(snap.Error, b.home) {
		t.Errorf("错误不该回显完整家目录，实际 %q", snap.Error)
	}
}

// ---------- sec.certificates：真实钥匙串（只摘要） ----------

func TestSecCertificatesReal(t *testing.T) {
	if _, ok := execx.LookPath("security"); !ok {
		t.Skip("本机没有 security")
	}
	b := newBench(t)
	snap := secSnap(t, b, "sec.certificates", map[string]any{"limit": 5})
	data := secData(t, snap)
	if _, ok := data["total"]; !ok {
		t.Fatal("应报告证书总数（空钥匙串也要如实）")
	}
	list, _ := data["certificates"].([]certInfo)
	if len(list) > 5 {
		t.Fatalf("limit=5 时最多返回 5 张，实际 %d", len(list))
	}
	// 绝不回显私钥：结果里不该出现任何私钥材料。
	raw := snap.Result.(*tool.Result).Msg + fmt.Sprint(data)
	for _, bad := range []string{"BEGIN PRIVATE KEY", "BEGIN RSA PRIVATE KEY", "PRIVATE KEY-----"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("结果里出现了私钥材料：%s", bad)
		}
	}
}

// ---------- sec.certificates：空钥匙串不算失败 ----------

func TestSecCertificatesEmptyKeystore(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, "security", "#!/bin/sh\nif [ \"$1\" = find-certificate ]; then exit 44; fi\nexit 0\n")
	t.Setenv("PATH", dir)
	b := newBench(t)
	snap := secSnap(t, b, "sec.certificates", nil)
	data := secData(t, snap)
	if data["total"] != 0 {
		t.Fatalf("空钥匙串 total 应为 0，实际 %v", data["total"])
	}
	if list, _ := data["certificates"].([]certInfo); len(list) != 0 {
		t.Errorf("空钥匙串不该有证书，实际 %v", list)
	}
}

// ---------- sec.gatekeeper_status：空 PATH 下命令失败要如实列出 ----------

func TestSecGatekeeperStatusReal(t *testing.T) {
	if _, ok := execx.LookPath("spctl"); !ok {
		t.Skip("本机没有 spctl")
	}
	b := newBench(t)
	snap := secSnap(t, b, "sec.gatekeeper_status", nil)
	data := secData(t, snap)
	if _, ok := data["gatekeeper"]; !ok {
		t.Error("应报告 Gatekeeper 状态")
	}
	if _, ok := data["sip"]; !ok {
		t.Error("应报告 SIP 状态")
	}
	if _, ok := data["developer_tools"]; !ok {
		t.Error("开发工具探测结果要如实给出（空也要给）")
	}
}

// ---------- sec.pkg_info：非 pkg 文件 → 失败原因如实 ----------

func TestSecPkgInfoNotAPackage(t *testing.T) {
	if _, ok := execx.LookPath("pkgutil"); !ok {
		t.Skip("本机没有 pkgutil")
	}
	b := newBench(t)
	f := filepath.Join(b.home, "not.pkg")
	if err := os.WriteFile(f, []byte("not a pkg"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := secSnap(t, b, "sec.pkg_info", map[string]any{"path": f})
	data := secData(t, snap)
	if probs, ok := data["unavailable"]; !ok {
		t.Fatalf("非 pkg 文件应如实列出不可用项，实际 %v", data)
	} else if len(probs.([]string)) == 0 {
		t.Error("不可用项不该为空")
	}
}

// ---------- 脚手架 ----------

// newBenchWithRoots 在标准 bench 基础上追加读根（真机验证系统 App 的签名要用）。
func newBenchWithRoots(t *testing.T, extraReadRoots []string) *bench {
	t.Helper()
	b := newBench(t)
	g, err := fsroot.New(append([]string{b.home}, extraReadRoots...), []string{b.write})
	if err != nil {
		t.Fatal(err)
	}
	b.ctx.Guard = g
	b.runner.Ctx = b.ctx
	return b
}

// writeShim 写一个可执行假命令（构造缺失/失败分支用，绝不依赖真实命令）。
func writeShim(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
