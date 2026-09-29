package mediaclean

// probe.go —— ffprobe 的只读查询（内嵌封面 / 图片附件 / 广告标签 / 时长）。
//
// 为什么不复用 internal/videoopt 的 Runner：那个接口回答的是"这个视频能不能压、
// 压到多少码率"（MediaInfo），拿不到 attached_pic、MKV 附件与容器标签；把这几件事
// 塞进同一个接口只会让两边都变形。命令定位那一步仍然复用面板既有的
// services.LocateCommand（见 web 层），所以"这台机器装没装 ffmpeg"只有一份判据。
//
// 封面有两种形态（都要清）：
//   · MP4/MOV 的 attached_pic 视频流（disposition=attached_pic）；
//   · **MKV 的图片附件**（attachment / codec_type=video 的 cover.jpg）——
//     MKV 没有 attached_pic 概念，只查 attached_pic 会把内嵌封面整张漏掉。

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

// attachedPics 返回 attached_pic（内嵌封面/广告图）的视频流号。
//
// 命令按用户点名的那一条写死：csv 每行是 "index,attached_pic"。
// 读不出来 ⇒ 返回错误（绝不当作"没有封面"，那会静默跳过该清的广告）。
func (e *Engine) attachedPics(ctx context.Context, path string) ([]int, error) {
	out, err := e.runProbe(ctx, "-v", "error", "-select_streams", "v",
		"-show_entries", "stream=index:stream_disposition=attached_pic",
		"-of", "csv=p=0", path)
	if err != nil {
		return nil, err
	}
	var idxs []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		idx, ierr := strconv.Atoi(strings.TrimSpace(parts[0]))
		if ierr != nil {
			continue
		}
		if strings.TrimSpace(parts[len(parts)-1]) == "1" {
			idxs = append(idxs, idx)
		}
	}
	sort.Ints(idxs)
	return idxs, nil
}

// imageAttachmentExts 是会被当成内嵌封面/广告图的图片扩展名。
var imageAttachmentExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true, ".bmp": true,
}

// fontAttachmentExts 是字体附件：**必须保留**（丢了会毁掉 ASS 字幕的字体）。
var fontAttachmentExts = map[string]bool{
	".ttf": true, ".otf": true, ".woff": true, ".woff2": true, ".ttc": true,
}

// coverBaseNames 是常见封面/广告图的文件名主干（小写、不含扩展名）。
var coverBaseNames = map[string]bool{
	"cover": true, "poster": true, "folder": true, "promo": true, "广告": true,
}

// attachmentStream 是 ffprobe 的一条流（附件查询只取用得到的字段）。
type attachmentStream struct {
	Index     int    `json:"index"`
	CodecType string `json:"codec_type"`
	Tags      struct {
		Filename string `json:"filename"`
		Mimetype string `json:"mimetype"`
	} `json:"tags"`
}

// imageAttachments 返回 MKV 图片附件（内嵌封面/广告图）的流号。
//
// 判据（用户点名）：mimetype 以 image/ 开头，或文件名是 cover/poster/folder/promo/广告
// 且扩展名是图片；attachment 流只要是图片扩展名也算。**字体附件一律排除**。
func (e *Engine) imageAttachments(ctx context.Context, path string) ([]int, error) {
	out, err := e.runProbe(ctx, "-v", "error",
		"-show_entries", "stream=index,codec_type:stream_tags=filename,mimetype",
		"-of", "json", path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Streams []attachmentStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("ffprobe 附件输出不是合法 JSON: %w", err)
	}
	var idxs []int
	for _, s := range doc.Streams {
		if isImageAttachment(s.CodecType, s.Tags.Filename, s.Tags.Mimetype) {
			idxs = append(idxs, s.Index)
		}
	}
	sort.Ints(idxs)
	return idxs, nil
}

// isImageAttachment 判断一条流是不是"该去掉的图片附件"（字体永远返回 false）。
func isImageAttachment(codecType, filename, mimetype string) bool {
	mt := strings.ToLower(strings.TrimSpace(mimetype))
	fn := strings.ToLower(strings.TrimSpace(filename))
	ext := filepath.Ext(fn)
	if strings.Contains(mt, "font") || fontAttachmentExts[ext] {
		return false
	}
	if strings.HasPrefix(mt, "image/") {
		return true
	}
	if !imageAttachmentExts[ext] {
		return false
	}
	// 有图片扩展名：attachment 流本身就是图片；有些容器 codec_type 给 video，
	// 那种靠文件名主干（cover/poster/…）认。
	if strings.EqualFold(strings.TrimSpace(codecType), "attachment") {
		return true
	}
	return coverBaseNames[strings.TrimSuffix(fn, ext)]
}

// junkTags 返回容器里非空的 title/comment/description 标签名（升序，供日志说明）。
func (e *Engine) junkTags(ctx context.Context, path string) ([]string, error) {
	out, err := e.runProbe(ctx, "-v", "error",
		"-show_entries", "format_tags=title,comment,description", "-of", "json", path)
	if err != nil {
		return nil, err
	}
	// 标签名大小写随容器（mp4 可能给 TITLE），所以按小写比较。
	var doc struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("ffprobe 标签输出不是合法 JSON: %w", err)
	}
	var hit []string
	for k, v := range doc.Format.Tags {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "title", "comment", "description":
			if strings.TrimSpace(v) != "" {
				hit = append(hit, strings.ToLower(strings.TrimSpace(k)))
			}
		}
	}
	sort.Strings(hit)
	return hit, nil
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
