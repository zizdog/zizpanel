package permissions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 判据层 ----------

func TestConsoleOKRejectsNobodyAtScreen(t *testing.T) {
	for _, u := range []string{"", "root", "loginwindow", "  "} {
		if ConsoleOK(u) {
			t.Errorf("控制台用户 %q 表示没人在屏幕前，必须判 false", u)
		}
	}
	if !ConsoleOK("zizdog") {
		t.Error("真实登录用户必须判 true")
	}
}

// TestProtectedDirCandidatesOnlyJoinPaths：GET 判据只拼路径、**不 stat**（坑 191）。
func TestProtectedDirCandidatesOnlyJoinPaths(t *testing.T) {
	got := ProtectedDirCandidates("/Users/zizdog")
	want := []string{"/Users/zizdog/Desktop", "/Users/zizdog/Documents", "/Users/zizdog/Downloads"}
	if len(got) != len(want) {
		t.Fatalf("受保护目录候选应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("候选[%d] 应为 %q，实际 %q", i, want[i], got[i])
		}
	}
	if ProtectedDirCandidates("") != nil {
		t.Error("拿不到家目录时必须返回 nil（不猜路径）")
	}
}

func TestReadOneReportsReadableAndMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := ReadOne(dir); !r.Readable || r.Denied {
		t.Errorf("普通目录应判可读：%+v", r)
	}
	missing := ReadOne(filepath.Join(dir, "nope"))
	if missing.Readable || missing.Denied || missing.Reason == "" {
		t.Errorf("不存在的目标应如实报「未读到」且不算权限拒绝：%+v", missing)
	}
}

func TestHistoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions-history.json")
	h := NewHistory(path)
	if _, ok := h.Last(ItemFullDisk); ok {
		t.Fatal("空历史不该有记录")
	}
	if err := h.Record(Entry{ID: ItemFullDisk, Status: StatusGranted, Result: "1/1 已可访问"}); err != nil {
		t.Fatal(err)
	}
	again := NewHistory(path)
	e, ok := again.Last(ItemFullDisk)
	if !ok || e.Status != StatusGranted || e.At.IsZero() {
		t.Fatalf("历史必须落盘可回读（含时间戳），实际 %+v ok=%v", e, ok)
	}
}

// ---------- 外部应用（注册表驱动；zizvideo 只是其中一条） ----------

// TestZizvideoRegistryEntryIsDisabled：zizvideo 已回仓为面板模块、由面板托管并与面板共用
// 文件权限 ⇒ 条目必须关闭（机制与代码全留，只是不再显示误导入口）。
func TestZizvideoRegistryEntryIsDisabled(t *testing.T) {
	spec, ok := LookupExternalApp(ItemZizvideo)
	if !ok {
		t.Fatal("zizvideo 必须仍在注册表里（机制与代码全留）")
	}
	if spec.Enabled {
		t.Error("zizvideo 已改为面板托管、与面板共用权限，条目必须 Enabled=false")
	}
	if !strings.Contains(spec.DisabledNote, "面板托管") || !strings.Contains(spec.DisabledNote, "共用权限") {
		t.Errorf("关闭原因要说清为何无需单独授权，实际 %q", spec.DisabledNote)
	}
	for _, a := range EnabledRegistry() {
		if a.ID == ItemZizvideo {
			t.Error("关闭的条目不能出现在启用列表里（前端因此不渲染）")
		}
	}
}

// fakeExternalApp 是门禁用的假外部应用：验证"加一条就走通整条授权链路"，不依赖真实软件。
func fakeExternalApp() ExternalApp {
	return ExternalApp{
		ID: "fakeapp", Name: "假外部应用", Title: "假外部应用", Why: "验证外部应用授权入口",
		Enabled: true, InstallPath: "/opt/fakeapp/bin/fakeapp",
		Label: "cn.fakeapp.serve", Port: 7799, SigningID: "cn.fakeapp.serve",
		CheckVerb: "check-access", DataSubdir: "Library/Application Support/fakeapp",
		HealthPath: "healthz",
		RootsArgs:  func(cfg string) []string { return []string{"roots", "list", "--config", cfg} },
	}
}

// fakeAppEnv 造一个"装了 spec"的假世界：所有外部命令都是假的。
func fakeAppEnv(t *testing.T, spec ExternalApp, healthOK bool, execPath string) Env {
	t.Helper()
	cfg := filepath.Join("/Users/zizdog", filepath.FromSlash(spec.DataSubdir), "config.json")
	return Env{
		HTTPGet: func(context.Context, string) ([]byte, error) {
			if !healthOK {
				return nil, errors.New("connection refused")
			}
			return []byte("ok"), nil
		},
		Run: func(_ context.Context, name string, args ...string) CmdResult {
			key := name + " " + strings.Join(args, " ")
			switch {
			case strings.HasPrefix(key, "/usr/sbin/lsof -nP -ti"):
				return CmdResult{Stdout: "26101\n"}
			case key == "/bin/ps -p 26101 -o user=":
				return CmdResult{Stdout: "zizdog\n"}
			case strings.HasPrefix(key, "/usr/sbin/lsof -p 26101"):
				return CmdResult{Stdout: "p26101\nftxt\nn" + execPath + "\nftxt\nn/usr/lib/dyld\n"}
			case key == "/bin/ps -p 26101 -o args=":
				return CmdResult{Stdout: execPath + " --config " + cfg + "\n"}
			case key == execPath+" --version":
				return CmdResult{Stdout: spec.ID + " 1.2.3\n"}
			case key == execPath+" roots list --config "+cfg:
				return CmdResult{Stdout: `{"action":"list","ok":true,"roots":["/Volumes/ZPMirror/video"]}` + "\n"}
			}
			return CmdResult{Code: 1}
		},
		UserHome: func(user string) (string, error) { return "/Users/" + user, nil },
	}
}

func writeFakeBinary(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-external-app")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDetectAppParsesHealthPortBinaryVersionRoots：假应用的门禁 —— 加一条就自动获得探测能力。
func TestDetectAppParsesHealthPortBinaryVersionRoots(t *testing.T) {
	spec := fakeExternalApp()
	bin := writeFakeBinary(t, "#!/bin/sh\nexit 0\n")
	d := DetectApp(context.Background(), fakeAppEnv(t, spec, true, bin), spec)

	if !d.Installed {
		t.Fatalf("回环健康通 + 反查到可执行文件时必须判已安装，实际：%s", d.Reason)
	}
	if d.ExecPath != bin {
		t.Errorf("可执行文件应来自端口反查，实际 %q", d.ExecPath)
	}
	if d.Owner != "zizdog" {
		t.Errorf("属主应为 zizdog，实际 %q", d.Owner)
	}
	if d.Version != "1.2.3" {
		t.Errorf("版本应解析成 1.2.3，实际 %q", d.Version)
	}
	if len(d.Roots) != 1 || d.Roots[0] != "/Volumes/ZPMirror/video" {
		t.Errorf("允许根应来自该实例的 config，实际 %v", d.Roots)
	}
	if d.SigningID != spec.SigningID || d.Port != spec.Port {
		t.Errorf("签名身份/端口应取自注册表条目，实际 %q / %d", d.SigningID, d.Port)
	}
}

// TestDetectZizvideoEntryStillWorks：zizvideo 条目即使关闭，冻结契约仍可探测
// （回仓后它还是 /opt/zizvideo/bin/zizvideo、label cn.zizvideo.serve、端口 7766）。
func TestDetectZizvideoEntryStillWorks(t *testing.T) {
	spec := ZizvideoApp()
	bin := writeFakeBinary(t, "#!/bin/sh\nexit 0\n")
	d := DetectApp(context.Background(), fakeAppEnv(t, spec, true, bin), spec)
	if !d.Installed {
		t.Fatalf("zizvideo 条目必须仍可探测，实际：%s", d.Reason)
	}
	if d.SigningID != ZizvideoSigningID || d.Port != ZizvideoPort {
		t.Errorf("zizvideo 冻结契约不得漂移：signing=%q port=%d", d.SigningID, d.Port)
	}
}

// TestDetectAppNotInstalled：探测失败/反查不到可执行文件都算**未安装**（前端因此不渲染）。
func TestDetectAppNotInstalled(t *testing.T) {
	spec := fakeExternalApp()
	bin := writeFakeBinary(t, "#!/bin/sh\nexit 0\n")

	env := fakeAppEnv(t, spec, false, bin)
	if d := DetectApp(context.Background(), env, spec); d.Installed {
		t.Errorf("健康探测失败必须判未安装，实际 %+v", d)
	}

	env2 := fakeAppEnv(t, spec, true, bin)
	env2.Run = func(_ context.Context, name string, args ...string) CmdResult {
		key := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(key, "/usr/sbin/lsof -nP -ti"):
			return CmdResult{} // 没有监听进程
		case strings.HasPrefix(key, "/usr/sbin/lsof -p"):
			return CmdResult{Stdout: "p1\nftxt\nn/usr/lib/dyld\n"}
		}
		return CmdResult{Code: 1}
	}
	if d := DetectApp(context.Background(), env2, spec); d.Installed {
		t.Errorf("反查不到可执行文件必须判未安装，实际 %+v", d)
	}
}

// TestCheckAppAccessUsesSudoAndRealscript：注入一个真的 check-access 脚本，
// 断言用的是 `sudo -n -u <真实用户> <可执行文件> check-access <目录>`（动词来自注册表）。
func TestCheckAppAccessUsesSudoAndRealscript(t *testing.T) {
	spec := fakeExternalApp()
	bin := writeFakeBinary(t, "#!/bin/sh\necho '{\"path\":\"/x\",\"readable\":true,\"reason\":\"\"}'\n")
	var gotName string
	var gotArgs []string
	env := Env{Run: func(ctx context.Context, name string, args ...string) CmdResult {
		gotName, gotArgs = name, append([]string(nil), args...)
		if name != "/usr/bin/sudo" {
			return CmdResult{Code: -1, Err: errors.New("必须经 sudo 以真实用户身份运行")}
		}
		// 去掉 sudo 前缀，真的执行那个脚本（测试机不需要 sudo）。
		rest := args[3:]
		out, err := exec.CommandContext(ctx, rest[0], rest[1:]...).Output()
		if err != nil {
			return CmdResult{Code: -1, Err: err}
		}
		return CmdResult{Stdout: string(out)}
	}}

	res, err := CheckAppAccess(context.Background(), env, "zizdog", bin, spec.CheckVerb, "/Volumes/ZPMirror/video")
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "/usr/bin/sudo" || len(gotArgs) < 6 ||
		gotArgs[0] != "-n" || gotArgs[1] != "-u" || gotArgs[2] != "zizdog" ||
		gotArgs[3] != bin || gotArgs[4] != spec.CheckVerb || gotArgs[5] != "/Volumes/ZPMirror/video" {
		t.Fatalf("必须以 `sudo -n -u zizdog <bin> %s <path>` 调用，实际 %s %v", spec.CheckVerb, gotName, gotArgs)
	}
	if !res.Supported || !res.Readable {
		t.Errorf("脚本回 readable:true 时必须如实报可读，实际 %+v", res)
	}
}

func TestCheckAppAccessReadsUnreadable(t *testing.T) {
	bin := writeFakeBinary(t, "#!/bin/sh\necho '{\"path\":\"/x\",\"readable\":false,\"reason\":\"operation not permitted\"}'\nexit 1\n")
	env := Env{Run: func(ctx context.Context, _ string, args ...string) CmdResult {
		rest := args[3:]
		out, _ := exec.CommandContext(ctx, rest[0], rest[1:]...).Output()
		return CmdResult{Stdout: string(out), Code: 1}
	}}
	res, err := CheckAppAccess(context.Background(), env, "zizdog", bin, "check-access", "/Volumes/x")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Supported || res.Readable {
		t.Errorf("readable:false 必须如实报不可读，实际 %+v", res)
	}
	if res.Reason == "" {
		t.Error("不可读时要带上该应用给的原因")
	}
}

// TestCheckAppAccessUnsupportedIsHonest：旧版本没有这个动词（无 JSON）⇒ 报"不支持自检"，
// **不退化**去读，也不谎报成功。
func TestCheckAppAccessUnsupportedIsHonest(t *testing.T) {
	bin := writeFakeBinary(t, "#!/bin/sh\necho 'flag provided but not defined' >&2\nexit 2\n")
	env := Env{Run: func(ctx context.Context, _ string, args ...string) CmdResult {
		rest := args[3:]
		cmd := exec.CommandContext(ctx, rest[0], rest[1:]...)
		out, _ := cmd.Output()
		return CmdResult{Stdout: string(out), Code: 2}
	}}
	res, err := CheckAppAccess(context.Background(), env, "zizdog", bin, "check-access", "/Volumes/x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Supported {
		t.Fatal("没有 JSON 应答时必须判「该版本不支持自检」")
	}
	if !strings.Contains(res.Reason, "不支持自检") || !strings.Contains(res.Reason, "升级") {
		t.Errorf("不支持时必须如实说「不支持自检，请升级」，实际 %q", res.Reason)
	}
}

// TestCheckAppAccessSudoFailureIsNotUnsupported：sudo 本身失败要与"版本不支持"分开报。
func TestCheckAppAccessSudoFailureIsNotUnsupported(t *testing.T) {
	bin := writeFakeBinary(t, "#!/bin/sh\nexit 1\n")
	env := Env{Run: func(context.Context, string, ...string) CmdResult {
		return CmdResult{Stderr: "sudo: a password is required\n", Code: 1}
	}}
	_, err := CheckAppAccess(context.Background(), env, "zizdog", bin, "check-access", "/Volumes/x")
	if err == nil {
		t.Fatal("sudo -n 失败必须如实报错，不能当成「版本不支持」")
	}
	if !strings.Contains(err.Error(), "zizdog") {
		t.Errorf("报错要点名以谁的身份失败，实际：%v", err)
	}
}
