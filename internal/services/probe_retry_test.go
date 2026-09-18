package services

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestEngineProbeRetriesOnceAfterFailure 锁住"冷启动一次失败不算失败"。
//
// 2026-09-18 真机事故：刚 brew install 完的 whisper-cli 首次执行被系统
// 完整性校验拖过 60 秒，复核失败 → 任务报"引擎装好了但跑不起来"，
// 用户以为装失败。首次执行的额外开销是一次性的，所以第二次必须能救回来。
func TestEngineProbeRetriesOnceAfterFailure(t *testing.T) {
	var calls int
	out, err := RunEngineProbeWithRetry(context.Background(), nil,
		func(_ context.Context, _ time.Duration, _ string, _ ...string) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("命令超时（1m0s）：whisper-cli")
			}
			return "usage: whisper-cli --model --language", nil
		}, "/opt/homebrew/bin/whisper-cli", "--help")
	if err != nil {
		t.Fatalf("第二次成功时不该报错，得到：%v", err)
	}
	if calls != 2 {
		t.Fatalf("首次失败后应当**再试一次**（共 2 次调用），实际 %d 次", calls)
	}
	if !strings.Contains(out, "--model") {
		t.Fatalf("应当返回第二次的输出，得到 %q", out)
	}
}

// TestEngineProbeReportsBothAttempts：两次都失败时，错误里必须带上两次的原因 ——
// 否则用户只看到"超时"，无法判断是冷启动还是真坏。
func TestEngineProbeReportsBothAttempts(t *testing.T) {
	var calls int
	_, err := RunEngineProbeWithRetry(context.Background(), nil,
		func(_ context.Context, _ time.Duration, _ string, _ ...string) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("命令超时（3m0s）：whisper-cli")
			}
			return "", errors.New("dyld: Library not loaded")
		}, "/opt/homebrew/bin/whisper-cli", "--help")
	if err == nil {
		t.Fatal("两次都失败时必须返回错误（绝不谎报成功）")
	}
	if calls != 2 {
		t.Fatalf("应当恰好尝试 2 次，实际 %d 次", calls)
	}
	for _, want := range []string{"命令超时", "dyld"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应当带上两次尝试的原因，缺 %q：%v", want, err)
		}
	}
}

// TestEngineProbeDoesNotRetryAfterCancel：任务被取消时不该再占用时间重试。
func TestEngineProbeDoesNotRetryAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int
	if _, err := RunEngineProbeWithRetry(ctx, nil,
		func(_ context.Context, _ time.Duration, _ string, _ ...string) (string, error) {
			calls++
			return "", errors.New("命令超时")
		}, "whisper-cli", "--help"); err == nil {
		t.Fatal("取消后仍应返回错误")
	}
	if calls != 1 {
		t.Fatalf("上下文已取消时不该重试（应为 1 次调用），实际 %d 次", calls)
	}
}

// TestEngineProbeTimeoutIsLongEnough：60 秒就是那次事故的元凶，
// 这里用门禁锁死"不许再缩回一分钟级"。
func TestEngineProbeTimeoutIsLongEnough(t *testing.T) {
	if EngineProbeTimeout < 120*time.Second {
		t.Fatalf("装完复核的单次超时是 %s，太短了：新装的二进制首次执行"+
			"（系统完整性校验 + Metal 内核首次编译）会被误判成「跑不起来」（2026-09-18 真机事故）",
			EngineProbeTimeout)
	}
}

// TestEngineProbeRunsAfterServiceRegistration 用**源码顺序**锁住这一类事故：
// 贵的 spawn 复核必须排在"注册网页界面服务"**之后**。
//
// 为什么必须有这条门禁：复核排在前面时，它一失败（哪怕只是冷启动超时），
// 后面的模型下载与界面服务注册就都不会执行 —— 用户点「打开」得到 502，
// 而引擎其实好好的。判据越贵，越要放在不挡用户路的位置。
func TestEngineProbeRunsAfterServiceRegistration(t *testing.T) {
	cases := []struct {
		file     string
		probe    string
		register string
		why      string
	}{
		{
			file:     "stt_install.go",
			probe:    "m.sttVerifyCLI(ctx, eng, result)",
			register: "m.installSTTService(ctx, app, result)",
			why:      "语音转文字（whisper-cli --help）",
		},
		{
			file:     "imgcompress.go",
			probe:    "m.imgCompressVersion(ctx, bin)",
			register: "m.installImgCompressService(ctx, app, result)",
			why:      "图片压缩（vips --version）",
		},
	}
	for _, c := range cases {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("读不到 %s：%v", c.file, err)
		}
		src := string(b)
		// 只看真正的调用（不是定义/注释）：定义行的前缀是 "func "。
		pi := strings.Index(src, c.probe)
		ri := strings.Index(src, c.register)
		if pi < 0 {
			t.Fatalf("%s 里找不到复核调用 %q（改了函数名？门禁要同步更新）", c.file, c.probe)
		}
		if ri < 0 {
			t.Fatalf("%s 里找不到注册服务调用 %q（改了函数名？门禁要同步更新）", c.file, c.register)
		}
		if pi < ri {
			t.Errorf("%s：%s 的复核调用排在了注册网页界面服务**之前**。\n"+
				"复核失败（哪怕只是首次执行被系统校验拖慢）会让界面服务根本不注册，"+
				"用户点「打开」就得到 502 —— 2026-09-18 真机事故就是这么发生的。\n"+
				"把复核挪到注册之后（或让它失败不阻断注册）。", c.file, c.why)
		}
	}
}
