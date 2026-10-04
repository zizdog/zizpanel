package videoopt

// decode_env_gate_test.go —— 「默认软件解码」与「环境提示」的唯一门禁。
//
// 为什么默认**不加** -hwaccel（2026-10-04 本机 MacBook Air M4 / macOS 15.6.1 /
// ffmpeg 9.0.1_1 —— 与面板市场安装的 ffmpeg 同一版本，实测 4K→852x480 libx264 800k）：
//
//	源              解码方式                墙钟     speed   ffmpeg CPU
//	4K H.264        软件（现状）            1.10s    17.4x   670%
//	4K H.264        -hwaccel videotoolbox   3.25s     4.9x   160%
//	4K HEVC         软件（现状）            1.66s     9.3x   840%
//	4K HEVC         -hwaccel videotoolbox   2.69s     5.7x   180%
//	4K HEVC 10bit   软件                    1.22s     8.4x   800%
//	4K HEVC 10bit   -hwaccel videotoolbox   1.73s     5.9x    43%
//	只解码 4K H.264：软件 0.52s（32x） vs VT 4.29s（3.5x）
//	硬编路径也一样：软件解码+hevc_videotoolbox 1.53s vs VT 解码 3.70s
//
// 结论：硬解帧要 hwdownload 回内存，x264 的帧级并行也用不上 ⇒ 加 -hwaccel 会让
// 用户更慢（用户报障的 mini 与开发机同为 M4）。零拷贝（-hwaccel_output_format
// videotoolbox + scale_vt）本机 ffmpeg 直接报 -22 不可用。
//
// 本门禁把"默认软件解码"钉死：变异测试 = 给 TranscodeArgs 加一行 -hwaccel ⇒ 立刻变红。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// TestDecodePolicyGate 断言默认命令里没有 -hwaccel，且既有档位参数逐字节没变。
func TestDecodePolicyGate(t *testing.T) {
	t.Run("① 默认 argv 逐字节固定（档位参数一个都没变）", func(t *testing.T) {
		req := TranscodeRequest{
			Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
			VideoKbps: 800, AudioKbps: 96, DurationSec: 5,
			Encoder: EncoderCPU, Mode: ModeBitrate,
		}
		got := strings.Join(TranscodeArgs(req, 0), " ")
		want := "-hide_banner -nostdin -y -i in.mp4 -c:v libx264 -preset veryfast -profile:v main " +
			"-pix_fmt yuv420p -b:v 800k -maxrate 800k -bufsize 1600k -vf scale=854:480 " +
			"-c:a aac -b:a 96k -movflags +faststart -f mp4 -progress pipe:1 -nostats " +
			"-loglevel error out.mp4"
		if got != want {
			t.Fatalf("默认转码 argv 变了（档位语义必须逐字节不变）\n got=%s\nwant=%s", got, want)
		}
	})

	t.Run("② 任何编码器/模式/遍数都不许出现 -hwaccel（实测硬解慢 2~8 倍）", func(t *testing.T) {
		base := TranscodeRequest{
			Src: "in.mp4", Dst: "out.mp4", Width: 854, Height: 480,
			VideoKbps: 800, AudioKbps: 96, DurationSec: 5,
		}
		reqs := []TranscodeRequest{
			withOpts(base, EncoderCPU, ModeBitrate, 0, false),
			withOpts(base, EncoderCPU, ModeQuality, DefaultCRF, false),
			withOpts(base, EncoderHardware, ModeBitrate, 0, false),
			withOpts(base, EncoderHardware, ModeQuality, DefaultVTQuality, false),
			withOpts(base, EncoderCPU, ModeBitrate, 0, true),
		}
		for _, req := range reqs {
			for _, pass := range []int{0, 1, 2} {
				args := TranscodeArgs(req, pass)
				for _, a := range args {
					if a == "-hwaccel" || a == "-hwaccel_output_format" {
						t.Fatalf("%s/%s pass=%d 出现了硬解开关 %s（实测更慢）\nargv=%v",
							req.Encoder, req.Mode, pass, a, args)
					}
				}
				// 负向对照：argv 本身不能是空的（"删干净"不算通过）。
				if !argsHave(args, "-i", "") || !argsHave(args, "-c:v", "") {
					t.Fatalf("argv 缺 -i/-c:v：%v", args)
				}
			}
		}
		// 档位参数仍在：-crf 26 / -q:v 45 / 缩放。
		if a := TranscodeArgs(withOpts(base, EncoderCPU, ModeQuality, DefaultCRF, false), 0); !argsHave(a, "-crf", "26") {
			t.Errorf("CPU 质量优先的 -crf 丢了：%v", a)
		}
		if a := TranscodeArgs(withOpts(base, EncoderHardware, ModeQuality, DefaultVTQuality, false), 0); !argsHave(a, "-q:v", "45") {
			t.Errorf("硬件质量优先的 -q:v 丢了：%v", a)
		}
		if a := TranscodeArgs(base, 0); !argsHave(a, "-vf", "scale=854:480") {
			t.Errorf("缩放参数丢了：%v", a)
		}
	})
}

// TestEnvNotesGate 断言环境提示（别的 ffmpeg / swap / 负载）只在真异常时出现。
func TestEnvNotesGate(t *testing.T) {
	t.Run("① 纯函数：三个信号各自触发；用户报障那组数字必须三条全中", func(t *testing.T) {
		cases := []struct {
			name                     string
			peers, swapUsed, swapTot int
			load                     float64
			cores                    int
			want                     int
		}{
			{"全正常", 0, 0, 0, 2.0, 10, 0},
			{"负载未超核数（等于也不算）", 0, 0, 0, 10.0, 10, 0},
			{"swap 占了一半但不到 1G 不提示", 0, 512, 4096, 2.0, 10, 0},
			{"有别的 ffmpeg", 2, 0, 0, 2.0, 10, 1},
			{"swap 过半且超 1G", 0, 2944, 4096, 2.0, 10, 1},
			{"负载超核数", 0, 0, 0, 15.75, 10, 1},
			{"三条同时", 1, 2944, 4096, 15.75, 10, 3},
		}
		for _, c := range cases {
			got := envNotesFrom(c.peers, c.swapUsed, c.swapTot, c.load, c.cores)
			if len(got) != c.want {
				t.Errorf("%s：应有 %d 条，实际 %d 条（%v）", c.name, c.want, len(got), got)
			}
		}
		joined := strings.Join(envNotesFrom(1, 2944, 4096, 15.75, 10), " | ")
		for _, want := range []string{"ffmpeg", "swap", "负载"} {
			if !strings.Contains(joined, want) {
				t.Errorf("用户报障场景（CPU 56.6%%、负载 15.75、swap 2.94G）必须提示 %q，实际：%s", want, joined)
			}
		}
	})

	t.Run("② 任务里：开始时说一次、每 20 个文件复查、同一条不重复、nil 不检测", func(t *testing.T) {
		dir := t.TempDir()
		outDir := filepath.Join(dir, "output")
		rows := make([]Plan, 25)
		for i := range rows {
			name := fmt.Sprintf("f%02d.mp4", i)
			rows[i] = Plan{
				Name: name, Path: filepath.Join(dir, name),
				OutPath:   filepath.Join(outDir, name+".480p.mp4"),
				VideoKbps: 800, AudioKbps: 96, TargetWidth: 854, TargetHeight: 480,
				SourceBytes: 4096, Encoder: EncoderCPU, Mode: ModeBitrate,
			}
		}
		runner := &gateRunner{outBytes: 100}

		var envLines []string
		calls := 0
		hooks := Hooks{
			Log: func(level, msg string) {
				if strings.HasPrefix(msg, "⚠ ") {
					envLines = append(envLines, level+"|"+msg)
				}
			},
			EnvNotes: func() []string {
				calls++
				if calls == 1 {
					// 任务开始时：两个信号（第二条要在复查时被去重）。
					return []string{"检测到 1 个其它 ffmpeg 进程，可能在抢 CPU", "系统负载 15.8 超过 10 核"}
				}
				// 中途（第 21 个文件前）复查：多出一个新信号。
				return []string{"系统负载 15.8 超过 10 核", "检测到 2 个其它 ffmpeg 进程，可能在抢 CPU"}
			},
		}
		res, err := RunPlan(context.Background(), outDir, rows, runner, hooks)
		if err != nil || res.Done != len(rows) {
			t.Fatalf("任务没跑完：err=%v done=%d/%d", err, res.Done, len(rows))
		}
		if calls != 2 {
			t.Fatalf("25 个文件应复查 2 次（第 1 个之前 + 第 21 个之前），实际 %d 次", calls)
		}
		if len(envLines) != 3 {
			t.Fatalf("应有 3 条环境提示（开始 2 条 + 中途新增 1 条），实际 %d 条：%v", len(envLines), envLines)
		}
		joined := strings.Join(envLines, "\n")
		if n := strings.Count(joined, "系统负载"); n != 1 {
			t.Errorf("同一条（系统负载）只许说一次，实际 %d 次", n)
		}
		if n := strings.Count(joined, "其它 ffmpeg"); n != 2 {
			t.Errorf("ffmpeg 数从 1 变 2 ⇒ 要说两次（开始一次、中途一次），实际 %d 次", n)
		}
		for _, l := range envLines {
			if !strings.HasPrefix(l, tasks.LevelWarn+"|⚠ ") {
				t.Errorf("环境提示必须是 warn + ⚠ 前缀，实际 %q", l)
			}
		}

		// 负向对照：nil = 不检测（单测默认），一条都不许有。
		envLines = nil
		if _, err := RunPlan(context.Background(), outDir, rows, runner, Hooks{Log: hooks.Log}); err != nil {
			t.Fatal(err)
		}
		if len(envLines) != 0 {
			t.Errorf("EnvNotes=nil 时不该有任何环境提示，实际 %v", envLines)
		}
	})
}

// withOpts 复制一份请求并套上选项（与 web 门禁同一口径，避免每个 case 重复写字段）。
func withOpts(req TranscodeRequest, enc, mode string, quality int, twoPass bool) TranscodeRequest {
	req.Encoder, req.Mode, req.Quality, req.TwoPass = enc, mode, quality, twoPass
	return req
}

// argsHave 断言 argv 里出现某个 flag（value=="" 只看 flag）。
func argsHave(args []string, flag, value string) bool {
	for i, a := range args {
		if a != flag {
			continue
		}
		if value == "" || (i+1 < len(args) && args[i+1] == value) {
			return true
		}
	}
	return false
}
