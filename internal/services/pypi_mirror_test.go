package services

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// pipMirrorArgs 是"NAS 优先 + 回落"在 pip 上的唯一入口。
// 这几条测试锁死三件事：探通走 NAS、探不通回落清华、离线模式不许回落。
// 全部用 mirrorFileProbeOverride 注入，单测不碰真实镜像站（AGENTS.md 第三节）。

func TestPipMirrorArgsUsesNASWhenReachable(t *testing.T) {
	var probed string
	m := &Manager{
		opt: Options{MirrorBase: "https://mirror.example.com:8888"},
		mirrorFileProbeOverride: func(_ context.Context, url string) (int64, error) {
			probed = url
			return 73764, nil
		},
	}
	args, err := m.pipMirrorArgs(context.Background(), nil)
	if err != nil {
		t.Fatalf("镜像探通时不应报错：%v", err)
	}
	if want := "https://mirror.example.com:8888/pypi/simple/pip/"; probed != want {
		t.Errorf("探针地址应为 %s，实际 %s", want, probed)
	}
	got := strings.Join(args, " ")
	wantIdx := "-i https://mirror.example.com:8888/pypi/simple/"
	if !strings.Contains(got, wantIdx) {
		t.Errorf("应优先用 NAS 索引 %q，实际参数：%s", wantIdx, got)
	}
	// NAS 的按需缓存是整份落盘后才回字节，必须放宽 pip 的 15 秒默认读超时。
	for _, want := range []string{"--timeout 180", "--retries 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("NAS 索引模式下参数应包含 %q，实际：%s", want, got)
		}
	}
	// https 基址不需要 --trusted-host。
	if strings.Contains(got, "--trusted-host") {
		t.Errorf("https 基址不该带 --trusted-host：%s", got)
	}
}

func TestPipMirrorArgsAddsTrustedHostForHTTPMirror(t *testing.T) {
	m := &Manager{
		opt: Options{MirrorBase: "http://192.168.1.8:8090"},
		mirrorFileProbeOverride: func(context.Context, string) (int64, error) {
			return 1, nil
		},
	}
	args, err := m.pipMirrorArgs(context.Background(), nil)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	got := strings.Join(args, " ")
	// pip 默认拒绝 http 索引，必须显式信任主机（不带端口，实测这样就能通过）。
	if !strings.Contains(got, "--trusted-host 192.168.1.8") {
		t.Errorf("http 基址必须带 --trusted-host <host>，实际：%s", got)
	}
}

func TestPipMirrorArgsFallsBackWhenMirrorMissing(t *testing.T) {
	m := &Manager{
		opt: Options{MirrorBase: "https://mirror.example.com:8888"},
		mirrorFileProbeOverride: func(context.Context, string) (int64, error) {
			return -1, errors.New("镜像站访问不了")
		},
	}
	args, err := m.pipMirrorArgs(context.Background(), nil)
	if err != nil {
		t.Fatalf("非离线模式下镜像不可用应回落，不应报错：%v", err)
	}
	if want := []string{"-i", pipIndexFallback}; strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("应回落清华源 %v，实际 %v", want, args)
	}
	// 回落路径是 pip 直连（流式下载），不要带 NAS 专属的放宽超时。
	if strings.Join(args, " ") != "-i "+pipIndexFallback {
		t.Errorf("回落参数应只有索引，实际：%v", args)
	}
}

func TestPipMirrorArgsOfflineMissingFails(t *testing.T) {
	m := &Manager{
		opt: Options{OfflineOnly: true, MirrorBase: "https://mirror.example.com:8888"},
		mirrorFileProbeOverride: func(context.Context, string) (int64, error) {
			return -1, errors.New("镜像站访问不了")
		},
	}
	args, err := m.pipMirrorArgs(context.Background(), nil)
	if err == nil {
		t.Fatal("离线模式下镜像缺件必须明确失败，不许回落到公网")
	}
	if args != nil {
		t.Errorf("失败时不应返回任何 pip 参数：%v", args)
	}
	for _, want := range []string{"离线模式", "禁止回落外网", "PyPI 索引"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际：%s", want, err.Error())
		}
	}
}

func TestPipMirrorArgsNoMirrorConfigured(t *testing.T) {
	m := &Manager{}
	args, err := m.pipMirrorArgs(context.Background(), nil)
	if err != nil {
		t.Fatalf("未配镜像基址不应报错：%v", err)
	}
	if want := "-i " + pipIndexFallback; strings.Join(args, " ") != want {
		t.Errorf("未启用镜像时应回落清华源 %q，实际 %v", want, args)
	}
}

// TestPythonPipInstallersGoThroughMirror 是一条防回归的源码断言：
// iopaint 与 qwen3tts 的 pip 调用必须走 pipMirrorArgs，不许再把索引写死成
// `-i qwenPipMirror` —— 写死就等于"镜像优先"在这两个应用上完全没接线。
// 与 homebrew_clt_mirror_test.go 里那类源码断言是同一个思路。
func TestPythonPipInstallersGoThroughMirror(t *testing.T) {
	for _, f := range []string{"iopaint.go", "qwentts.go"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", f, err)
		}
		src := string(raw)
		if !strings.Contains(src, "m.pipMirrorArgs(ctx, result)") {
			t.Errorf("%s 的 pip 安装没有走 pipMirrorArgs（镜像未接线）", f)
		}
		if strings.Contains(src, `"-i", qwenPipMirror`) {
			t.Errorf("%s 里仍有写死清华源的 pip 调用，绕过了 NAS 镜像", f)
		}
	}
}
