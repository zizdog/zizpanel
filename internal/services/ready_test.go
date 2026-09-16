package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  「就绪判定」一族缺陷的回归测试
//
//  锁的是用户最在意的那条约束：**面板不许报成功而其实没起来**。
//  2026-09-17 审计发现三处「就绪检查失败只写 Warning 然后 return nil」。
//
//  全部离线：端口 / HTTP / launchd 三个探针都通过 ready.go 里的注入点替换，
//  单测不真等 20~90 秒、不真开端口、不碰真实 launchd（AGENTS.md 第三节）。
//
//  注意：这几个注入点是**包级变量**，所以本文件里的测试都不并行
//  （没有 t.Parallel），并且用 t.Cleanup 恢复原值。
// ============================================================================

func newReadyTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
}

func readyStepsContain(res *InstallResult, want string) bool {
	for _, s := range res.Steps {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func stubReadyWaitPort(t *testing.T, fn func(context.Context, int, time.Duration) bool) {
	t.Helper()
	old := readyWaitPort
	readyWaitPort = fn
	t.Cleanup(func() { readyWaitPort = old })
}

func stubReadyWaitJSONBool(t *testing.T, fn func(context.Context, string, string, bool, time.Duration) bool) {
	t.Helper()
	old := readyWaitJSONBool
	readyWaitJSONBool = fn
	t.Cleanup(func() { readyWaitJSONBool = old })
}

func stubReadyLaunchRunning(t *testing.T, fn func(string) (bool, string)) {
	t.Helper()
	old := readyLaunchRunning
	readyLaunchRunning = fn
	t.Cleanup(func() { readyLaunchRunning = old })
}

func stubReadyVerifyReceiverKey(t *testing.T, fn func(context.Context, *Manager, string) (bool, string)) {
	t.Helper()
	old := readyVerifyReceiverKey
	readyVerifyReceiverKey = fn
	t.Cleanup(func() { readyVerifyReceiverKey = old })
}

// ---------------------------------------------------------------------------
//  assertReady 本身：成功 / 超时 / 没有日志
// ---------------------------------------------------------------------------

func TestAssertReadySuccessWritesStep(t *testing.T) {
	res := &InstallResult{}
	err := assertReady(context.Background(), readySpec{
		What:    "示例服务",
		Expect:  "端口 1234 开始监听",
		Timeout: time.Second,
		Probe: func(context.Context) readyVerdict {
			return readyVerdict{OK: true, Actual: "已就绪，监听 1234 端口"}
		},
		Result: res,
	})
	if err != nil {
		t.Fatalf("探针报就绪时不该失败：%v", err)
	}
	if res.Warning != "" {
		t.Fatalf("成功了却写了 Warning：%q", res.Warning)
	}
	if !readyStepsContain(res, "示例服务已就绪，监听 1234 端口") {
		t.Fatalf("成功时要在任务日志里明确写出就绪，实际：%v", res.Steps)
	}
}

// 默认必须是 error —— 这就是「面板报成功、其实没起来」的直接防线。
func TestAssertReadyTimeoutIsErrorByDefault(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "svc.err.log")
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "line-%d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	res := &InstallResult{}
	err := assertReady(context.Background(), readySpec{
		What:    "示例服务",
		Expect:  "端口 1234 开始监听",
		Timeout: 2 * time.Second,
		Probe: func(context.Context) readyVerdict {
			return readyVerdict{Actual: "连 127.0.0.1:1234 一直失败"}
		},
		LogPath: logPath,
		State:   "服务已登记进服务管理",
		Missing: "但端口没有监听",
		Remedy:  "按日志修好后重新部署",
		Result:  res,
	})
	if err == nil {
		t.Fatal("没就绪时必须返回错误；返回 nil 就是谎报成功")
	}
	msg := err.Error()
	// 期望什么 / 实际什么 / 等了多久 / 已完成 / 还差什么 / 怎么办 / 日志尾部
	for _, want := range []string{
		"期望", "端口 1234 开始监听",
		"实际", "连 127.0.0.1:1234 一直失败",
		"已等待 2s",
		"已登记进服务管理",
		"但端口没有监听",
		"重新部署",
		logPath,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q，实际：%s", want, msg)
		}
	}
	// 日志尾部：保留最后 20 行，够定位报错；不该把整份日志倒进错误里。
	if !strings.Contains(msg, "line-39") {
		t.Errorf("错误信息应包含日志最后一行，实际：%s", msg)
	}
	if strings.Contains(msg, "line-0\n") || strings.Contains(msg, "line-19") {
		t.Errorf("错误信息只该带日志尾部（最后 20 行），实际：%s", msg)
	}
	// 失败也要在结果里留下痕迹（有些界面只看 Warning）。
	if !strings.Contains(res.Warning, "未就绪") {
		t.Errorf("失败时必须写 Warning，实际：%q", res.Warning)
	}
	if !readyStepsContain(res, "错误：") {
		t.Errorf("失败时任务日志要写「错误：」，实际：%v", res.Steps)
	}
}

// 「没有日志」本身就是信息：不能因为读不到日志就跳过报错，
// 也不能编一句含糊的「请查看日志」糊过去。
func TestAssertReadyWithoutLogIsStillReadable(t *testing.T) {
	emptyLog := filepath.Join(t.TempDir(), "empty.err.log")
	if err := os.WriteFile(emptyLog, []byte("\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		logPath string
	}{
		{"文件不存在", filepath.Join(t.TempDir(), "missing.err.log")},
		{"文件存在但为空", emptyLog},
		{"调用方没给路径", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertReady(context.Background(), readySpec{
				What:    "示例服务",
				Expect:  "端口 1234 开始监听",
				Timeout: time.Second,
				Probe:   func(context.Context) readyVerdict { return readyVerdict{} },
				LogPath: c.logPath,
			})
			if err == nil {
				t.Fatal("没有日志也必须如实失败，不能因为读不到日志就放行")
			}
			msg := err.Error()
			if !strings.Contains(msg, "没有日志") {
				t.Errorf("没有日志时要**明确写出来**，实际：%s", msg)
			}
			// 可读性：期望/实际/超时一个都不能少。
			for _, want := range []string{"示例服务未就绪", "期望", "实际", "已等待 1s"} {
				if !strings.Contains(msg, want) {
					t.Errorf("错误信息应包含 %q，实际：%s", want, msg)
				}
			}
			// 空路径时不该编造一个空文件名。
			if c.logPath == "" && strings.Contains(msg, "服务日志 ：") {
				t.Errorf("没有提供日志路径时不该拼出空的日志路径，实际：%s", msg)
			}
		})
	}
}

// 显式降级必须给出理由：没有理由 = 静默吞掉失败，直接报错。
func TestAssertReadyDegradeRequiresReason(t *testing.T) {
	err := assertReady(context.Background(), readySpec{
		What:    "示例服务",
		Expect:  "端口 1234 开始监听",
		Timeout: time.Second,
		Probe:   func(context.Context) readyVerdict { return readyVerdict{} },
		Degrade: true,
	})
	if err == nil {
		t.Fatal("Degrade=true 但没写理由时必须报错（否则就是静默降级）")
	}
	if !strings.Contains(err.Error(), "DegradeReason") {
		t.Errorf("错误信息要点明缺什么，实际：%v", err)
	}
}

func TestAssertReadyDegradeWritesWarningAndReturnsNil(t *testing.T) {
	res := &InstallResult{}
	err := assertReady(context.Background(), readySpec{
		What:          "示例服务",
		Expect:        "端口 1234 开始监听",
		Timeout:       time.Second,
		Probe:         func(context.Context) readyVerdict { return readyVerdict{Actual: "端口没监听"} },
		Degrade:       true,
		DegradeReason: "这个路径本来就允许稍后由用户手动启动",
		Result:        res,
	})
	if err != nil {
		t.Fatalf("显式降级时不该返回 error：%v", err)
	}
	if !strings.Contains(res.Warning, "未就绪") ||
		!strings.Contains(res.Warning, "可接受的降级") ||
		!strings.Contains(res.Warning, "手动启动") {
		t.Fatalf("降级必须写清「为什么可接受」，实际 Warning：%q", res.Warning)
	}
	if !readyStepsContain(res, "警告：") {
		t.Fatalf("降级要在任务日志里留痕，实际：%v", res.Steps)
	}
}

// 没有探针就没有判定依据 —— 绝不能当成「通过」。
func TestAssertReadyMissingProbeIsError(t *testing.T) {
	err := assertReady(context.Background(), readySpec{
		What:   "示例服务",
		Expect: "端口 1234 开始监听",
	})
	if err == nil {
		t.Fatal("缺少探针时必须报错，不能默认通过")
	}
}

func TestReadyTailLinesKeepsTail(t *testing.T) {
	in := "a\nb\nc\nd\n"
	if got := readyTailLines(in, 2); got != "c\nd" {
		t.Errorf("应取最后 2 行并保留换行，实际 %q", got)
	}
	if got := readyTailLines("", 5); got != "" {
		t.Errorf("空输入应返回空，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
//  Qwen3 TTS：90 秒没监听 = 失败（原来是 Warning + return nil）
// ---------------------------------------------------------------------------

func TestWaitQwenReadyTimeoutIsError(t *testing.T) {
	m := newReadyTestManager(t)
	p := m.qwenPaths()
	var gotPort int
	var gotTimeout time.Duration
	stubReadyWaitPort(t, func(_ context.Context, port int, timeout time.Duration) bool {
		gotPort, gotTimeout = port, timeout
		return false
	})
	res := &InstallResult{}

	err := m.waitQwenReady(context.Background(), p, true, res)
	if err == nil {
		t.Fatal("8880 没监听时必须返回错误；返回 nil 就是谎报成功（TtsVoice 会全站失联）")
	}
	if gotPort != qwenPort || gotTimeout != qwenReadyTimeout {
		t.Fatalf("等待参数不对：port=%d timeout=%v", gotPort, gotTimeout)
	}
	for _, want := range []string{"8880", "未就绪", "已登记进服务管理", p.ErrLog, "没有日志"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}

func TestWaitQwenReadySuccess(t *testing.T) {
	m := newReadyTestManager(t)
	stubReadyWaitPort(t, func(context.Context, int, time.Duration) bool { return true })
	res := &InstallResult{}

	if err := m.waitQwenReady(context.Background(), m.qwenPaths(), true, res); err != nil {
		t.Fatalf("端口起来了不该失败：%v", err)
	}
	if !readyStepsContain(res, "已就绪") {
		t.Fatalf("就绪时必须写一行日志，实际：%v", res.Steps)
	}
}

// 登记失败时，错误信息不许谎称「已登记」。
func TestWaitQwenReadyDoesNotClaimRegisteredWhenItFailed(t *testing.T) {
	m := newReadyTestManager(t)
	stubReadyWaitPort(t, func(context.Context, int, time.Duration) bool { return false })
	err := m.waitQwenReady(context.Background(), m.qwenPaths(), false, &InstallResult{})
	if err == nil {
		t.Fatal("没就绪时必须失败")
	}
	if strings.Contains(err.Error(), "服务也已登记进服务管理") {
		t.Errorf("登记失败时不能在错误里谎称已登记：%v", err)
	}
	if !strings.Contains(err.Error(), "登记进服务管理失败") {
		t.Errorf("登记失败要如实写出来：%v", err)
	}
}

// ---------------------------------------------------------------------------
//  release 二进制（frpc / ddns-go / orbien-client）：60 秒没监听 = 失败
// ---------------------------------------------------------------------------

func TestWaitReleaseBinaryReadyPortTimeoutIsError(t *testing.T) {
	m := newReadyTestManager(t)
	spec := releaseBinaryApps["ddns-go"]
	p := m.binaryReleasePaths(spec)
	var gotPort int
	var gotTimeout time.Duration
	stubReadyWaitPort(t, func(_ context.Context, port int, timeout time.Duration) bool {
		gotPort, gotTimeout = port, timeout
		return false
	})
	res := &InstallResult{}

	err := m.waitReleaseBinaryReady(context.Background(), spec, p, true, res)
	if err == nil {
		t.Fatal("端口没监听时必须返回错误；返回 nil 就是「任务完成 ✅ 但用不了」")
	}
	if gotPort != spec.Port || gotTimeout != releaseBinaryReadyTimeout {
		t.Fatalf("等待参数不对：port=%d timeout=%v", gotPort, gotTimeout)
	}
	for _, want := range []string{fmt.Sprint(spec.Port), "未就绪", p.ErrLog, "没有日志"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}

// orbien 客户端不监听任何端口：判据是 launchd 有没有真的把进程拉起来，
// 而不是 waitPort(0)（那必然超时，每次安装都会误报一次）。
func TestWaitReleaseBinaryReadyNoPortUsesLaunchd(t *testing.T) {
	m := newReadyTestManager(t)
	spec := releaseBinaryApps["orbien-client"]
	if spec.Port != 0 {
		t.Fatalf("这个测试的前提是 orbien-client 没有端口，实际 Port=%d", spec.Port)
	}
	p := m.binaryReleasePaths(spec)
	stubReadyWaitPort(t, func(context.Context, int, time.Duration) bool {
		t.Fatal("没有监听端口的应用不该去等端口（Port=0 必然超时）")
		return false
	})
	stubReadyLaunchRunning(t, func(string) (bool, string) { return true, "launchd 已拉起该作业（pid 123）" })
	res := &InstallResult{}

	if err := m.waitReleaseBinaryReady(context.Background(), spec, p, true, res); err != nil {
		t.Fatalf("launchd 已拉起时不该失败：%v", err)
	}
	if !readyStepsContain(res, "已就绪") {
		t.Fatalf("就绪时必须写一行日志，实际：%v", res.Steps)
	}
}

func TestWaitLaunchdRunningTimesOut(t *testing.T) {
	stubReadyLaunchRunning(t, func(string) (bool, string) {
		return false, "launchd 已加载该作业，但进程没有在运行"
	})
	v := waitLaunchdRunning(context.Background(), "com.zizdog.orbien-client", 50*time.Millisecond)
	if v.OK {
		t.Fatal("进程没在跑时不能判成就绪")
	}
	if !strings.Contains(v.Actual, "没有在运行") {
		t.Errorf("要如实带出 launchd 的实际状态，实际：%q", v.Actual)
	}
}

// ---------------------------------------------------------------------------
//  音色接收端：/voice/health 20 秒没返回期望值 = 失败（原来是 Warning + return nil）
// ---------------------------------------------------------------------------

func TestWaitReceiverReadyTimeoutIsError(t *testing.T) {
	m := newReadyTestManager(t)
	p := m.receiverPaths()
	var gotURL, gotField string
	var gotWant bool
	var gotTimeout time.Duration
	stubReadyWaitJSONBool(t, func(_ context.Context, url, field string, want bool, timeout time.Duration) bool {
		gotURL, gotField, gotWant, gotTimeout = url, field, want, timeout
		return false
	})
	res := &InstallResult{}

	err := m.waitReceiverReady(context.Background(), p, true, true, res)
	if err == nil {
		t.Fatal("健康检查没返回期望值时必须返回错误；返回 nil 就是谎报成功")
	}
	if gotField != "auth" || !gotWant || gotTimeout != receiverReadyTimeout {
		t.Fatalf("等待参数不对：field=%q want=%v timeout=%v", gotField, gotWant, gotTimeout)
	}
	if !strings.Contains(gotURL, "/voice/health") {
		t.Fatalf("探测地址不对：%s", gotURL)
	}
	// 失败信息必须让用户知道「服务其实已经登记了，去哪看」。
	for _, want := range []string{"/voice/health", `"auth": true`, "已登记进服务管理", p.ErrLog} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}

func TestWaitReceiverReadySuccess(t *testing.T) {
	m := newReadyTestManager(t)
	stubReadyWaitJSONBool(t, func(context.Context, string, string, bool, time.Duration) bool { return true })
	res := &InstallResult{}
	if err := m.waitReceiverReady(context.Background(), m.receiverPaths(), false, true, res); err != nil {
		t.Fatalf("健康检查返回期望值了不该失败：%v", err)
	}
	if !readyStepsContain(res, "已就绪") {
		t.Fatalf("就绪时必须写一行日志，实际：%v", res.Steps)
	}
}

// 「用密钥真打一次」失败原来也只写 Warning —— 那等于交付一个网站每次都被拒的接收端。
func TestAssertReceiverKeyUsableFailureIsError(t *testing.T) {
	m := newReadyTestManager(t)
	p := m.receiverPaths()
	stubReadyVerifyReceiverKey(t, func(context.Context, *Manager, string) (bool, string) {
		return false, "带正确密钥返回 403"
	})
	res := &InstallResult{}

	err := m.assertReceiverKeyUsable(context.Background(), p, "ttsv-secret", true, res)
	if err == nil {
		t.Fatal("密钥打不通 /jobs 时必须返回错误，不能只写 Warning")
	}
	for _, want := range []string{"带正确密钥返回 403", "鉴权", "调用密钥", p.ErrLog} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}

func TestAssertReceiverKeyUsableSuccess(t *testing.T) {
	m := newReadyTestManager(t)
	stubReadyVerifyReceiverKey(t, func(context.Context, *Manager, string) (bool, string) {
		return true, ""
	})
	res := &InstallResult{}
	if err := m.assertReceiverKeyUsable(context.Background(), m.receiverPaths(), "ttsv-secret", true, res); err != nil {
		t.Fatalf("密钥可用时不该失败：%v", err)
	}
	if !readyStepsContain(res, "用密钥实测 /jobs（200）") {
		t.Fatalf("成功时要写清真的打了一次 /jobs，实际：%v", res.Steps)
	}
}

// ---------------------------------------------------------------------------
//  Qwen 加鉴权时的对外入口（反代 / 音色接收端）：失败 = 整单失败
// ---------------------------------------------------------------------------

// 同族第 4 处：加鉴权时 Qwen 只监听 127.0.0.1，反代没起来网站就**一定**连不上。
// 原来这里只写一条 Warning 就 return nil —— 任务报「✅ 完成」，用户对着绿灯排查。
func TestFinishQwenAuthEntryProxyFailureIsError(t *testing.T) {
	m := newReadyTestManager(t)
	res := &InstallResult{}
	deployErr := errors.New("接收端 /voice/health 20s 内没有返回 auth:true")
	deploy := func(context.Context, *InstallResult, ReceiverOptions) error {
		return deployErr
	}

	err := m.finishQwenAuthEntry(context.Background(), res, "ttsv-secret", deploy)
	if err == nil {
		t.Fatal("反代没起来时必须让整个部署失败；返回 nil 就是谎报成功（网站连不上却报成功）")
	}
	// 必须说清：Qwen 本身成了、差的是对外入口、所以网站连不上、能怎么办。
	for _, want := range []string{
		"Qwen3 TTS 本身已就绪",
		"只监听 127.0.0.1",
		"网站（TtsVoice）现在连不上 8880",
		"已中止 Qwen3 TTS 部署",
		m.receiverPaths().ErrLog,
		"重试",
		"不启用鉴权",
		deployErr.Error(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
	// 失败也要在结果里留痕：不能只有任务中心知道。
	if !strings.Contains(res.Warning, "已中止") {
		t.Errorf("失败时必须写 Warning，实际：%q", res.Warning)
	}
	if !readyStepsContain(res, "错误：") {
		t.Errorf("失败时任务日志要写「错误：」，实际：%v", res.Steps)
	}
}

func TestFinishQwenAuthEntryProxySuccess(t *testing.T) {
	m := newReadyTestManager(t)
	res := &InstallResult{}
	called := false
	deploy := func(_ context.Context, _ *InstallResult, opt ReceiverOptions) error {
		called = true
		if opt.Token != "ttsv-secret" {
			t.Errorf("密钥应原样传给接收端，实际 %q", opt.Token)
		}
		return nil
	}

	if err := m.finishQwenAuthEntry(context.Background(), res, "ttsv-secret", deploy); err != nil {
		t.Fatalf("反代部署成功时不该失败：%v", err)
	}
	if !called {
		t.Fatal("必须真的调用部署动作（不能因为它是可选步骤就跳过）")
	}
	if res.Warning != "" {
		t.Errorf("成功时不该写 Warning，实际：%q", res.Warning)
	}
}
