package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizvideo/internal/domain"
)

// recordingRunner captures the exact argv handed to the process.
type recordingRunner struct {
	name string
	args []string
	out  string
	err  error
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	r.name, r.args = name, args
	if r.err != nil {
		return nil, []byte(r.out), r.err
	}
	return []byte(r.out), nil, nil
}

const goodJSON = `{"streams":[
 {"codec_type":"video","codec_name":"h264","width":1080,"height":1920,"r_frame_rate":"30000/1001","duration":"12.5"},
 {"codec_type":"audio","codec_name":"aac"}],
 "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"12.520000","bit_rate":"2500000"}}`

func TestProbeMapsStreamsAndFormat(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{out: goodJSON}
	got, err := Probe(context.Background(), r, "/opt/homebrew/bin/ffprobe", src, 5*time.Second)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if r.name != "/opt/homebrew/bin/ffprobe" {
		t.Fatalf("可执行文件 = %q", r.name)
	}
	want := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", src}
	if len(r.args) != len(want) {
		t.Fatalf("参数 = %q", r.args)
	}
	for i := range want {
		if r.args[i] != want[i] {
			t.Fatalf("参数[%d] = %q, 期望 %q", i, r.args[i], want[i])
		}
	}
	if got.VideoCodec != "h264" || got.AudioCodec != "aac" {
		t.Fatalf("编码 = %+v", got)
	}
	if got.Width != 1080 || got.Height != 1920 {
		t.Fatalf("分辨率 = %dx%d", got.Width, got.Height)
	}
	if got.DurationMS != 12520 || got.Bitrate != 2500000 {
		t.Fatalf("时长/码率 = %d/%d", got.DurationMS, got.Bitrate)
	}
	if got.FPS < 29.9 || got.FPS > 30.1 {
		t.Fatalf("fps = %v", got.FPS)
	}
}

// TestProbeNeverInvolvesAShell locks the injection rule at the argv level.
func TestProbeNeverInvolvesAShell(t *testing.T) {
	dir := t.TempDir()
	hostile := filepath.Join(dir, "; rm -rf ~")
	if err := os.WriteFile(hostile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{out: goodJSON}
	if _, err := Probe(context.Background(), r, "/usr/bin/ffprobe", hostile, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(r.name, "sh") || r.name == "bash" {
		t.Fatalf("必须直接执行 ffprobe, 实际 %q", r.name)
	}
	for _, a := range r.args {
		if a == "-c" {
			t.Fatal("参数列表里不允许出现 -c")
		}
	}
	if last := r.args[len(r.args)-1]; last != hostile {
		t.Fatalf("路径参数 = %q, 期望原样 %q", last, hostile)
	}
}

func TestClassifyProbeFailures(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		err   error
		out   string
		want  string
		ctxTo time.Duration
	}{
		{"invalid format", errors.New("exit 1"), "Invalid data found when processing input", domain.ClassInvalidFormat, 0},
		{"not found", errors.New("exit 1"), "No such file or directory", domain.ClassNotFound, 0},
		{"unreadable", errors.New("exit 1"), "Permission denied", domain.ClassUnreadable, 0},
		{"timeout", errors.New("signal: killed"), "whatever", domain.ClassProbeTimeout, time.Nanosecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingRunner{err: tc.err, out: tc.out}
			ctx := context.Background()
			if tc.ctxTo > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTo)
				defer cancel()
				time.Sleep(2 * time.Millisecond)
			}
			_, err := Probe(ctx, r, "/usr/bin/ffprobe", src, time.Second)
			var pe *ProbeError
			if !errors.As(err, &pe) {
				t.Fatalf("错误类型 = %T (%v)", err, err)
			}
			if pe.Class != tc.want {
				t.Fatalf("分类 = %q, 期望 %q", pe.Class, tc.want)
			}
		})
	}
}

func TestProbeReportsMissingSourceAsNotFound(t *testing.T) {
	r := &recordingRunner{out: goodJSON}
	_, err := Probe(context.Background(), r, "/usr/bin/ffprobe", "/nope/gone.mp4", time.Second)
	var pe *ProbeError
	if !errors.As(err, &pe) || pe.Class != domain.ClassNotFound {
		t.Fatalf("分类 = %v, 期望 NOT_FOUND", err)
	}
}

func TestExtractCoverArgs(t *testing.T) {
	r := &recordingRunner{}
	err := ExtractCover(context.Background(), r, "/usr/bin/ffmpeg", "/in/clip.mp4",
		"/out/med_1.jpg", 0.75, 3, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.name != "/usr/bin/ffmpeg" {
		t.Fatalf("可执行文件 = %q", r.name)
	}
	joined := strings.Join(r.args, " ")
	for _, want := range []string{"-ss 0.750", "-i /in/clip.mp4", "-frames:v 1", "-q:v 3", "/out/med_1.jpg"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("参数缺少 %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "sh -c") {
		t.Fatal("不允许 shell 调用")
	}
}

func TestCoverTimeBounds(t *testing.T) {
	cases := []struct {
		ms   int64
		want float64
	}{
		{0, 0.5},
		{2000, 0.5},
		{12000, 0.6},
		{60000, 1.0},
	}
	for _, c := range cases {
		if got := CoverTime(c.ms); got != c.want {
			t.Fatalf("CoverTime(%d) = %v, 期望 %v", c.ms, got, c.want)
		}
	}
}

func TestDetectReportsMissingTools(t *testing.T) {
	caps := Detect(context.Background(), ExecRunner{}, "/nonexistent/ffmpeg", "/nonexistent/ffprobe")
	if caps.FFmpegOK || caps.FFprobeOK {
		t.Fatal("不存在的工具必须报 false")
	}
	if caps.Error == "" {
		t.Fatal("必须给出原因")
	}
}

func TestDetectReadsVersionAndEncoders(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ffmpeg")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-version\" ]; then echo 'ffmpeg version 9.0.1 Copyright'; exit 0; fi\n" +
		"echo ' V....D h264_videotoolbox fake'; exit 0\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	caps := Detect(context.Background(), ExecRunner{}, stub, stub)
	if !caps.FFmpegOK || caps.FFmpegVersion != "9.0.1" {
		t.Fatalf("版本探测 = %+v", caps)
	}
	if !caps.VideoToolboxH264 {
		t.Fatal("未识别 h264_videotoolbox")
	}
}
