// Package ffmpeg probes media and extracts covers. Every call is an argument
// list passed to exec.CommandContext — user-supplied names never reach a shell.
package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/govideo/internal/domain"
)

// Runner executes an external program and returns its raw output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner is the production runner. There is no shell involved.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return []byte(out.String()), []byte(errb.String()), err
}

// ProbeError carries the classified failure reason for a single file.
type ProbeError struct {
	Class string
	Err   error
}

func (e *ProbeError) Error() string { return e.Class + ": " + e.Err.Error() }
func (e *ProbeError) Unwrap() error { return e.Err }

// ProbeResult is the subset of ffprobe output the MVP stores.
type ProbeResult struct {
	Container  string
	VideoCodec string
	AudioCodec string
	Width      int
	Height     int
	DurationMS int64
	Bitrate    int64
	FPS        float64
}

// Probe runs ffprobe in JSON mode and maps the output.
func Probe(ctx context.Context, r Runner, ffprobeBin, path string, timeout time.Duration) (*ProbeResult, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ProbeError{Class: domain.ClassNotFound, Err: err}
		}
		return nil, &ProbeError{Class: domain.ClassUnreadable, Err: err}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err := r.Run(cctx, ffprobeBin,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	if err != nil {
		return nil, classify(cctx, err, stderr, ffprobeBin)
	}
	var parsed probeJSON
	if err := json.Unmarshal(stdout, &parsed); err != nil {
		return nil, &ProbeError{Class: domain.ClassInvalidFormat, Err: err}
	}
	if len(parsed.Streams) == 0 {
		return nil, &ProbeError{Class: domain.ClassInvalidFormat,
			Err: errors.New("no streams")}
	}
	res := &ProbeResult{Container: parsed.Format.FormatName}
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if res.VideoCodec != "" {
				continue
			}
			res.VideoCodec = s.CodecName
			res.Width, res.Height = s.Width, s.Height
			res.FPS = parseRatio(s.RFrameRate)
		case "audio":
			if res.AudioCodec == "" {
				res.AudioCodec = s.CodecName
			}
		}
	}
	if res.VideoCodec == "" {
		return nil, &ProbeError{Class: domain.ClassInvalidFormat, Err: errors.New("no video stream")}
	}
	res.DurationMS = secondsToMS(parsed.Format.Duration)
	if res.DurationMS == 0 {
		for _, s := range parsed.Streams {
			if s.CodecType == "video" && s.Duration != "" {
				res.DurationMS = secondsToMS(s.Duration)
				break
			}
		}
	}
	res.Bitrate, _ = strconv.ParseInt(parsed.Format.BitRate, 10, 64)
	return res, nil
}

// classify maps a runner failure onto one of the four documented classes.
func classify(ctx context.Context, err error, stderr []byte, bin string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &ProbeError{Class: domain.ClassProbeTimeout, Err: err}
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return &ProbeError{Class: domain.ClassInvalidFormat,
			Err: fmt.Errorf("探测程序不可用 %s: %w", bin, err)}
	}
	msg := strings.ToLower(string(stderr))
	switch {
	case strings.Contains(msg, "no such file or directory"):
		return &ProbeError{Class: domain.ClassNotFound, Err: errors.New("file vanished")}
	case strings.Contains(msg, "permission denied"), strings.Contains(msg, "operation not permitted"):
		return &ProbeError{Class: domain.ClassUnreadable, Err: errors.New("permission denied")}
	default:
		return &ProbeError{Class: domain.ClassInvalidFormat, Err: errors.New("probe failed")}
	}
}

// ExtractCover writes a single JPEG frame at the given offset.
func ExtractCover(ctx context.Context, r Runner, ffmpegBin, src, dst string, atSeconds float64, quality int, timeout time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if quality < 2 || quality > 31 {
		quality = 3
	}
	_, stderr, err := r.Run(cctx, ffmpegBin,
		"-nostdin", "-y",
		"-ss", strconv.FormatFloat(atSeconds, 'f', 3, 64),
		"-i", src,
		"-frames:v", "1",
		"-q:v", strconv.Itoa(quality),
		"-f", "image2",
		dst)
	if err != nil {
		return classify(cctx, err, stderr, ffmpegBin)
	}
	return nil
}

// CoverTime picks the documented offset: min(1.0, max(0.05*duration, 0.5)).
func CoverTime(durationMS int64) float64 {
	t := 0.05 * float64(durationMS) / 1000.0
	if t < 0.5 {
		t = 0.5
	}
	if t > 1.0 {
		t = 1.0
	}
	return t
}

// Capabilities is the startup probe result used by /readyz and system info.
type Capabilities struct {
	FFmpegPath       string
	FFmpegVersion    string
	FFmpegOK         bool
	FFprobePath      string
	FFprobeVersion   string
	FFprobeOK        bool
	VideoToolboxH264 bool
	Error            string
}

// Detect versions and the h264_videotoolbox encoder. A missing binary is
// reported, never assumed.
func Detect(ctx context.Context, r Runner, ffmpegBin, ffprobeBin string) Capabilities {
	cap := Capabilities{FFmpegPath: ffmpegBin, FFprobePath: ffprobeBin}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if out, _, err := r.Run(cctx, ffmpegBin, "-version"); err == nil {
		cap.FFmpegVersion = firstVersionLine(string(out))
		cap.FFmpegOK = true
	} else {
		cap.Error = "ffmpeg 不可用: " + err.Error()
	}
	if out, _, err := r.Run(cctx, ffprobeBin, "-version"); err == nil {
		cap.FFprobeVersion = firstVersionLine(string(out))
		cap.FFprobeOK = true
	} else if cap.Error == "" {
		cap.Error = "ffprobe 不可用: " + err.Error()
	}
	if cap.FFmpegOK {
		if out, _, err := r.Run(cctx, ffmpegBin, "-hide_banner", "-encoders"); err == nil {
			cap.VideoToolboxH264 = strings.Contains(string(out), "h264_videotoolbox")
		}
	}
	return cap
}

func firstVersionLine(out string) string {
	line := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		line = out[:i]
	}
	f := strings.Fields(line)
	for i, tok := range f {
		if tok == "version" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return strings.TrimSpace(line)
}

func secondsToMS(s string) int64 {
	if s == "" || s == "N/A" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0
	}
	return int64(v * 1000)
}

func parseRatio(s string) float64 {
	if s == "" || s == "0/0" {
		return 0
	}
	if i := strings.IndexByte(s, '/'); i > 0 {
		num, err1 := strconv.ParseFloat(s[:i], 64)
		den, err2 := strconv.ParseFloat(s[i+1:], 64)
		if err1 == nil && err2 == nil && den != 0 {
			return num / den
		}
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

type probeJSON struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
		Duration   string `json:"duration"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
}
