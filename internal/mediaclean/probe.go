package mediaclean

// probe.go —— ffprobe 的只读查询（全局标签 / 轨道标题 / 附件 / 视频流 / 时长）。
//
// 为什么不复用 internal/videoopt 的 Runner：那个接口回答的是"这个视频能不能压、
// 压到多少码率"（MediaInfo），拿不到 attached_pic、MKV 附件与容器标签；把这几件事
// 塞进同一个接口只会让两边都变形。命令定位那一步仍然复用面板既有的
// services.LocateCommand（见 web 层），所以"这台机器装没装 ffmpeg"只有一份判据。
//
// 一次 ffprobe 拿全"彻底清理"需要的依据（见 inspect）：全局标签、每条流的
// codec_type/attached_pic/title/filename/mimetype。判据只有一处（planClean），
// 命令构造只有一处（CopyArgs），改判据不会漏掉 argv。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrEngineMissing 是"这台机器没有 ffmpeg/ffprobe"的哨兵错误。
var ErrEngineMissing = fmt.Errorf("缺少 ffmpeg / ffprobe")

// Engine 是一次任务用的 ffmpeg/ffprobe 位置（由 web 层用 services.LocateCommand 填好）。
type Engine struct {
	Ffmpeg  string
	Ffprobe string
}

// Available 判断两个命令是否都定位到了；读不到时给出可照做的下一步。
func (e *Engine) Available() error {
	if e == nil || strings.TrimSpace(e.Ffmpeg) == "" || strings.TrimSpace(e.Ffprobe) == "" {
		return fmt.Errorf("%w：这台机器没装 ffmpeg（或 ffprobe）。请到「应用市场 → FFmpeg（音视频工具）」安装后重试", ErrEngineMissing)
	}
	return nil
}

// probeTimeout 是单次 ffprobe 的上限（读不出就如实失败，绝不挂住整个任务）。
const probeTimeout = 60 * time.Second

// runProbe 跑一次 ffprobe 并返回 stdout（失败时带上它自己的 stderr）。
func (e *Engine) runProbe(ctx context.Context, args ...string) ([]byte, error) {
	if e == nil || strings.TrimSpace(e.Ffprobe) == "" {
		return nil, ErrEngineMissing
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.Ffprobe, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &errBuf, n: 2048}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffprobe 读取失败: %s", oneLine(msg))
	}
	return out, nil
}

// attachedPic / 附件 / 轨道标题 / 全局标签都由下面这一次 ffprobe 拿全。
//
// 判据（用户点名"能去多少去多少"）：
//   · attached_pic 视频流全删；
//   · 非 attached_pic 的视频流只保留第一条，其余全删（内嵌推广视频属于这类）；
//   · 附件只保留字体（mimetype 含 font，或扩展名 ttf/otf/woff/woff2/ttc），其余全删；
//   · 任何非空的全局标签都要清（-map_metadata -1）；
//   · 任何轨道标题都要清（-metadata:s:v/a/s title=）。
//
// 例外：muxer 自己生成的 ENCODER 标签不算"可清理项"（ffmpeg 每次都会重新写回，
// 算进去的话任何文件第二次跑都不会命中"本来就是干净的"，跳过分支等于死代码）。
//
// ⚠️ -map_metadata -1 连**轨道级**元数据一起丢：language 与 MKV 附件的 filename
// 都会没掉（matroska 缺 filename 直接 mux 失败），所以 inspect 必须把它们读出来，
// 由 CopyArgs 逐项还原（章节/语言/字体附件一个都不能丢，见 planClean）。

// StreamInfo 是一条流的只读结论。
type StreamInfo struct {
	Index       int
	CodecType   string
	AttachedPic bool
	Title       string
	Filename    string
	Mimetype    string
	// Language 是轨道语言（-map_metadata -1 会连它一起丢，必须显式还原）。
	Language string
}

// MediaInfo 是一次 ffprobe 得到的全部判据依据。
type MediaInfo struct {
	// GlobalTags 是容器级非空标签名（小写、升序；encoder 已排除）。
	GlobalTags []string
	Streams    []StreamInfo
}

// inspect 跑一次 ffprobe 读出所有可清理项的判据。读不出来 ⇒ 返回错误。
func (e *Engine) inspect(ctx context.Context, path string) (*MediaInfo, error) {
	out, err := e.runProbe(ctx, "-v", "error",
		"-show_entries",
		"stream=index,codec_type:stream_tags=title,filename,mimetype,language:stream_disposition=attached_pic:format_tags",
		"-of", "json", path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Index       int    `json:"index"`
			CodecType   string `json:"codec_type"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
			Tags struct {
				Title    string `json:"title"`
				Filename string `json:"filename"`
				Mimetype string `json:"mimetype"`
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("ffprobe 输出不是合法 JSON: %w", err)
	}
	info := &MediaInfo{}
	for k, v := range doc.Format.Tags {
		name := strings.ToLower(strings.TrimSpace(k))
		// ENCODER 是 muxer 自加的，不算可清理项（见文件头注释）。
		if name == "" || name == "encoder" || strings.TrimSpace(v) == "" {
			continue
		}
		info.GlobalTags = append(info.GlobalTags, name)
	}
	sort.Strings(info.GlobalTags)
	for _, s := range doc.Streams {
		info.Streams = append(info.Streams, StreamInfo{
			Index:       s.Index,
			CodecType:   strings.ToLower(strings.TrimSpace(s.CodecType)),
			AttachedPic: s.Disposition.AttachedPic == 1,
			Title:       s.Tags.Title,
			Filename:    s.Tags.Filename,
			Mimetype:    s.Tags.Mimetype,
			Language:    s.Tags.Language,
		})
	}
	sort.Slice(info.Streams, func(i, j int) bool { return info.Streams[i].Index < info.Streams[j].Index })
	return info, nil
}

// fontAttachmentExts 是字体附件：**必须保留**（丢了会毁掉 ASS 字幕的字体）。
var fontAttachmentExts = map[string]bool{
	".ttf": true, ".otf": true, ".woff": true, ".woff2": true, ".ttc": true,
}

// isFontAttachment 判断一条附件是不是字体（mimetype 含 font，或字体扩展名）。
func isFontAttachment(mimetype, filename string) bool {
	if strings.Contains(strings.ToLower(mimetype), "font") {
		return true
	}
	return fontAttachmentExts[filepath.Ext(strings.ToLower(filename))]
}

// duration 读一个文件的时长（秒）。读不到 ⇒ 返回错误，调用方据此拒绝替换。
func (e *Engine) duration(ctx context.Context, path string) (float64, error) {
	out, err := e.runProbe(ctx, "-v", "error",
		"-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(string(out))
	f, perr := strconv.ParseFloat(raw, 64)
	if perr != nil || f <= 0 {
		return 0, fmt.Errorf("读不到时长（ffprobe 输出 %q）", raw)
	}
	return f, nil
}

// limitedWriter 只保留前 n 个字节（外部命令的 stderr 可能很长）。
type limitedWriter struct {
	w interface{ Write([]byte) (int, error) }
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	b := p
	if len(b) > l.n {
		b = b[:l.n]
	}
	l.n -= len(b)
	if l.w != nil {
		_, _ = l.w.Write(b)
	}
	return len(p), nil
}

// oneLine 把多行错误压成一行（任务日志一行一条）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "\n")
	for _, p := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(p); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}
