package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// 本文件只用系统自带 CLI（afinfo/mdls/afconvert/say），不碰原生框架桥。

// redactHome 把结果里的读/写根前缀换成 …：错误信息不该回显完整家目录路径。
func redactHome(c *tool.Ctx, s string) string {
	if s == "" {
		return s
	}
	if c != nil && c.Guard != nil {
		for _, r := range append(c.Guard.ReadRoots(), c.Guard.WriteRoots()...) {
			if len(r) > 1 {
				s = strings.ReplaceAll(s, r, "…")
			}
		}
	}
	if h, err := os.UserHomeDir(); err == nil && len(h) > 1 {
		s = strings.ReplaceAll(s, h, "…")
	}
	return s
}

// cmdFailure 汇总真实 stderr/stdout 关键行，并抹掉家目录前缀。
func cmdFailure(c *tool.Ctx, verb string, res *execx.Result) error {
	if res.TimedOut {
		return fmt.Errorf("%s 超时被终止", verb)
	}
	return fmt.Errorf("%s（退出码 %d）：%s", verb, res.ExitCode, redactHome(c, failureReason(res)))
}

// ============================================================================
//  media.probe —— 音频/视频信息（同步，只读）
// ============================================================================

type mediaProbe struct{}

func init() { Add(mediaProbe{}) }

func (mediaProbe) Meta() tool.Meta {
	_, ok := execx.LookPath("afinfo")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/afinfo"
	}
	return tool.Meta{
		ID: "media.probe", Name: "媒体信息", Category: "media", Icon: "waveform",
		Summary: "读取音频/视频的时长、格式、采样率与分辨率。",
		Async:   false, TimeoutSeconds: 60,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "媒体文件", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Movies/a.mov", Help: "只能读允许的读根内的文件。"},
		},
	}
}

func (mediaProbe) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	st, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败：%s", redactHome(c, err.Error()))
	}
	data := map[string]any{"path": src, "name": filepath.Base(src), "size": st.Size()}
	var notes []string

	info := c.Exec.Run(ctx, 30*time.Second, "afinfo", src)
	if info.ExitCode == 0 {
		for k, v := range parseAfInfo(info.Stdout) {
			data[k] = v
		}
	} else {
		notes = append(notes, "afinfo 读不出音轨："+redactHome(c, firstLine(info.Output())))
	}

	// mdls 只在 Spotlight 已索引时才有媒体属性（/tmp 常为 null，坑 M1）。
	if _, ok := execx.LookPath("mdls"); ok {
		m := c.Exec.Run(ctx, 20*time.Second, "mdls", src)
		if m.ExitCode == 0 {
			notes = append(notes, fillFromMdls(data, parseMdls(m.Stdout))...)
		} else {
			notes = append(notes, "mdls 失败："+redactHome(c, firstLine(m.Output())))
		}
	} else {
		notes = append(notes, "系统缺少 mdls，视频参数取不到")
	}

	if !mediaUseful(data) {
		msg := "afinfo 与 mdls 都没读到媒体参数"
		if len(notes) > 0 {
			msg += "：" + notes[0]
		}
		return nil, fmt.Errorf("%s", msg)
	}
	if v, ok := data["duration_seconds"].(float64); ok {
		data["duration_human"] = humanDuration(v)
	}
	if len(notes) > 0 {
		data["notes"] = notes
	}
	return &tool.Result{OK: true, Msg: mediaSummary(data), Data: data}, nil
}

func mediaUseful(d map[string]any) bool {
	for _, k := range []string{"duration_seconds", "sample_rate_hz", "channels", "bit_rate_bps", "video", "data_format"} {
		if _, ok := d[k]; ok {
			return true
		}
	}
	return false
}

// fillFromMdls 只补 afinfo 给不出的字段；null 一律当"没有"，不猜默认值。
func fillFromMdls(d map[string]any, md map[string]string) []string {
	var notes []string
	setNum := func(key, attr string) {
		if _, exists := d[key]; exists {
			return
		}
		if v := md[attr]; v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				d[key] = f
			}
		}
	}
	setNum("duration_seconds", "kMDItemDurationSeconds")
	setNum("bit_rate_bps", "kMDItemAudioBitRate")
	setNum("sample_rate_hz", "kMDItemAudioSampleRate")
	setNum("channels", "kMDItemAudioChannelCount")
	if v := md["kMDItemKind"]; v != "" {
		d["kind"] = v
	}
	if v := md["kMDItemCodecs"]; v != "" {
		d["codecs"] = strings.Split(v, ", ")
	}
	w, _ := strconv.Atoi(md["kMDItemPixelWidth"])
	h, _ := strconv.Atoi(md["kMDItemPixelHeight"])
	if w > 0 && h > 0 {
		d["video"] = map[string]any{"width": w, "height": h}
	} else if strings.Contains(md["kMDItemKind"], "影片") || strings.Contains(md["kMDItemKind"], "Video") {
		notes = append(notes, "Spotlight 未给出视频分辨率（可能未索引）")
	}
	return notes
}

func mediaSummary(d map[string]any) string {
	parts := []string{}
	if v, ok := d["container"].(string); ok && v != "" {
		parts = append(parts, v)
	} else if v, ok := d["kind"].(string); ok && v != "" {
		parts = append(parts, v)
	}
	if v, ok := d["duration_human"].(string); ok {
		parts = append(parts, v)
	}
	if v, ok := d["sample_rate_hz"].(float64); ok && v > 0 {
		parts = append(parts, strconv.Itoa(int(v))+" Hz")
	}
	if v, ok := d["channels"].(float64); ok && v > 0 {
		parts = append(parts, channelName(int(v)))
	}
	if v, ok := d["video"].(map[string]any); ok {
		parts = append(parts, fmt.Sprintf("%vx%v", v["width"], v["height"]))
	}
	if len(parts) == 0 {
		return "已读取媒体信息"
	}
	return strings.Join(parts, " · ")
}

func channelName(n int) string {
	switch n {
	case 1:
		return "单声道"
	case 2:
		return "双声道"
	}
	return fmt.Sprintf("%d 声道", n)
}

func humanDuration(sec float64) string {
	if sec < 60 {
		return fmt.Sprintf("%.1f 秒", sec)
	}
	m := int(sec) / 60
	s := int(sec) % 60
	if m < 60 {
		return fmt.Sprintf("%d 分 %d 秒", m, s)
	}
	return fmt.Sprintf("%d 时 %d 分", m/60, m%60)
}

// parseAfInfo 解析 afinfo 的关键行；多轨只取第一条音轨。
func parseAfInfo(out string) map[string]any {
	d := map[string]any{}
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "File type ID:"):
			d["container"] = strings.TrimSpace(strings.TrimPrefix(t, "File type ID:"))
		case strings.HasPrefix(t, "Num Tracks:"):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(t, "Num Tracks:"))); err == nil {
				d["tracks"] = n
			}
		case strings.HasPrefix(t, "Data format:"):
			if _, done := d["sample_rate_hz"]; done {
				continue
			}
			ch, rate, format := parseDataFormat(t)
			if ch > 0 {
				d["channels"] = ch
			}
			if rate > 0 {
				d["sample_rate_hz"] = rate
			}
			if format != "" {
				d["data_format"] = format
			}
		case strings.HasPrefix(t, "estimated duration:"):
			if f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(t, "estimated duration:")), " sec"), 64); err == nil {
				d["duration_seconds"] = f
			}
		case strings.HasPrefix(t, "bit rate:"):
			if f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "bit rate:"), "bits per second")), 64); err == nil {
				d["bit_rate_bps"] = f
			}
		case strings.HasPrefix(t, "audio bytes:"):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(t, "audio bytes:"))); err == nil {
				d["audio_bytes"] = n
			}
		case strings.HasPrefix(t, "source bit depth:"):
			d["source_bit_depth"] = strings.TrimSpace(strings.TrimPrefix(t, "source bit depth:"))
		}
	}
	return d
}

// parseDataFormat 拆 "1 ch,  22050 Hz, lpcm (0x…) 16-bit …"。
func parseDataFormat(line string) (channels int, rate float64, format string) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "Data format:"))
	if i := strings.Index(rest, " ch"); i > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(rest[:i])); err == nil {
			channels = n
		}
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest[i+3:]), ","))
	}
	if i := strings.Index(rest, " Hz"); i > 0 {
		if f, err := strconv.ParseFloat(strings.TrimSpace(rest[:i]), 64); err == nil {
			rate = f
		}
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest[i+3:]), ","))
	}
	format = strings.TrimSpace(rest)
	if i := strings.Index(format, "(0x"); i > 0 {
		format = strings.TrimSpace(format[:i])
	}
	return
}

// parseMdls 解析 mdls 的 key = value，含多行数组；(null) 视为没有。
func parseMdls(out string) map[string]string {
	res := map[string]string{}
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		if val == "(" {
			var parts []string
			for i++; i < len(lines); i++ {
				t := strings.TrimSpace(lines[i])
				if t == ")" {
					break
				}
				t = strings.Trim(strings.TrimSuffix(t, ","), "\"")
				if t != "" {
					parts = append(parts, t)
				}
			}
			if len(parts) > 0 {
				res[key] = strings.Join(parts, ", ")
			}
			continue
		}
		if val == "(null)" || val == "" {
			continue
		}
		res[key] = strings.Trim(val, "\"")
	}
	return res
}

// ============================================================================
//  media.audio_convert —— afconvert 转码（异步）
// ============================================================================

type mediaAudioConvert struct{}

func init() { Add(mediaAudioConvert{}) }

// audioTargets 是容器 → (afconvert -f, 默认 -d)。采样率是 -d 的 @ 后缀。
var audioTargets = map[string][2]string{
	"wav":  {"WAVE", "LEI16"},
	"aiff": {"AIFF", "BEI16"},
	"m4a":  {"m4af", "aac"},
	"caf":  {"caff", "LEI16"},
}

func (mediaAudioConvert) Meta() tool.Meta {
	_, ok := execx.LookPath("afconvert")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/afconvert"
	}
	return tool.Meta{
		ID: "media.audio_convert", Name: "音频转码", Category: "media", Icon: "waveform",
		Summary: "系统 afconvert 转 wav/aiff/m4a/caf，可改采样率。",
		Async:   true, TimeoutSeconds: 600,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "input", Label: "输入音频", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Music/a.wav", Help: "只能读允许的读根内的文件。"},
			{Name: "format", Label: "目标格式", Type: tool.TypeSelect, Required: true, Default: "m4a",
				Options: []tool.Option{
					{Value: "m4a", Label: "M4A（AAC 压缩）"},
					{Value: "wav", Label: "WAV（无损 PCM）"},
					{Value: "aiff", Label: "AIFF（无损 PCM）"},
					{Value: "caf", Label: "CAF（Core Audio）"},
				}},
			{Name: "sample_rate", Label: "目标采样率", Type: tool.TypeSelect, Default: "keep",
				Options: []tool.Option{
					{Value: "keep", Label: "保持原样"},
					{Value: "8000", Label: "8000 Hz"},
					{Value: "16000", Label: "16000 Hz"},
					{Value: "22050", Label: "22050 Hz"},
					{Value: "44100", Label: "44100 Hz"},
					{Value: "48000", Label: "48000 Hz"},
				}},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

// Probe：afconvert -hf 列出可用容器（输出走 stderr 且退出码为 2，坑 M2）。
func (mediaAudioConvert) Probe(ctx context.Context, c *tool.Ctx) (bool, string) {
	const key = "media.audio_convert:afconvert-hf"
	if ok, reason, hit := c.Probes.Get(key); hit {
		return ok, reason
	}
	res := c.Exec.Run(ctx, 20*time.Second, "afconvert", "-hf")
	list := res.Stdout + res.Stderr
	var missing []string
	for _, token := range []string{"'WAVE'", "'AIFF'", "'m4af'", "'caff'"} {
		if !strings.Contains(list, token) {
			missing = append(missing, token)
		}
	}
	if len(missing) > 0 {
		reason := "afconvert 不支持格式：" + strings.Join(missing, " ")
		if strings.TrimSpace(list) == "" {
			reason = "afconvert -hf 没有输出：" + redactHome(c, firstLine(res.Output()))
		}
		c.Probes.Put(key, false, reason, 0)
		return false, reason
	}
	c.Probes.Put(key, true, "", 0)
	return true, ""
}

func (m mediaAudioConvert) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	format := strings.ToLower(in.Str("format"))
	target, ok := audioTargets[format]
	if !ok {
		return nil, fmt.Errorf("不支持的目标格式：%s", format)
	}
	dst, err := resolveOutput(c, in.Path("output"), src, format)
	if err != nil {
		return nil, err
	}
	if sameFile(src, dst) {
		return nil, fmt.Errorf("输出文件与输入文件相同，请换一个输出路径")
	}
	dataFormat := target[1]
	if sr := in.Str("sample_rate"); sr != "" && sr != "keep" {
		dataFormat += "@" + sr
	}
	c.Logf("开始转码：%s → %s（%s）", filepath.Base(src), filepath.Base(dst), format)
	c.Progress(10, "调用 afconvert")
	res := c.Exec.Run(ctx, 10*time.Minute, "afconvert", "-f", target[0], "-d", dataFormat, src, "-o", dst)
	if res.ExitCode != 0 {
		return nil, cmdFailure(c, "afconvert 转码失败", res)
	}
	st, err := os.Stat(dst)
	if err != nil {
		// 退出码 0 但没产出：必须如实报错（坑 F1）。
		return nil, fmt.Errorf("afconvert 报告成功但没有产出文件：%s", filepath.Base(dst))
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("afconvert 产出了 0 字节文件：%s", filepath.Base(dst))
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK:  true,
		Msg: fmt.Sprintf("已转码为 %s（%s）", strings.ToUpper(format), humanSize(st.Size())),
		Data: map[string]any{
			"input": src, "output": dst, "format": format,
			"sample_rate": in.Str("sample_rate"), "size": st.Size(),
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ============================================================================
//  media.tts —— 系统 say 中文语音合成（异步）
// ============================================================================

type mediaTTS struct{}

func init() { Add(mediaTTS{}) }

func (mediaTTS) Meta() tool.Meta {
	_, ok := execx.LookPath("say")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/say"
	}
	return tool.Meta{
		ID: "media.tts", Name: "文字转语音", Category: "media", Icon: "speaker",
		Summary: "系统 say 朗读中文并导出音频，可再转 m4a。",
		Async:   true, TimeoutSeconds: 600,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "text", Label: "要朗读的文本", Type: tool.TypeTextarea, Required: true, Multiline: true,
				Placeholder: "你好，这是 mac军刀 的语音测试。", Help: "长文本经临时文件传给 say，不进命令行。"},
			{Name: "voice", Label: "音色", Type: tool.TypeSelect, Default: "auto",
				Probe:   "say-voices",
				Options: voiceOptions(),
				Help:    "点执行会用 say -v '?' 列真实音色并缓存。"},
			{Name: "format", Label: "输出格式", Type: tool.TypeSelect, Default: "aiff",
				Options: []tool.Option{
					{Value: "aiff", Label: "AIFF（say 原生）"},
					{Value: "m4a", Label: "M4A（转 AAC）"},
				}},
			{Name: "output", Label: "输出文件", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

type ttsVoice struct {
	name   string
	locale string
	label  string
}

var ttsVoiceCache struct {
	mu     sync.Mutex
	loaded bool
	voices []ttsVoice
}

// voiceOptions 列表阶段只读缓存：昂贵探测一律留给 Probe（坑 E1）。
func voiceOptions() []tool.Option {
	opts := []tool.Option{{Value: "auto", Label: "自动（优先中文）"}}
	for _, v := range cachedVoices() {
		opts = append(opts, tool.Option{Value: v.name, Label: v.label})
	}
	return opts
}

func cachedVoices() []ttsVoice {
	ttsVoiceCache.mu.Lock()
	defer ttsVoiceCache.mu.Unlock()
	out := make([]ttsVoice, len(ttsVoiceCache.voices))
	copy(out, ttsVoiceCache.voices)
	return out
}

func setVoices(list []ttsVoice) {
	ttsVoiceCache.mu.Lock()
	ttsVoiceCache.voices = list
	ttsVoiceCache.loaded = true
	ttsVoiceCache.mu.Unlock()
}

// Probe：真实列出本机音色并缓存，不写死猜（坑 M3）。
func (mediaTTS) Probe(ctx context.Context, c *tool.Ctx) (bool, string) {
	const key = "media.tts:voices"
	if ok, reason, hit := c.Probes.Get(key); hit {
		return ok, reason
	}
	res := c.Exec.Run(ctx, 20*time.Second, "say", "-v", "?")
	if res.ExitCode != 0 {
		reason := "say -v '?' 失败：" + redactHome(c, failureReason(res))
		c.Probes.Put(key, false, reason, 0)
		return false, reason
	}
	list := parseSayVoices(res.Stdout)
	if len(list) == 0 {
		reason := "say -v '?' 没列出任何音色"
		c.Probes.Put(key, false, reason, 0)
		return false, reason
	}
	setVoices(list)
	c.Probes.Put(key, true, "", 0)
	return true, ""
}

// parseSayVoices 解析 "名称  区域  # 示例"；名称可含空格与非 ASCII。
func parseSayVoices(out string) []ttsVoice {
	var list []ttsVoice
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		left := line
		if i := strings.IndexByte(line, '#'); i >= 0 {
			left = line[:i]
		}
		left = strings.TrimSpace(left)
		if left == "" {
			continue
		}
		fields := strings.Fields(left)
		name, locale := left, ""
		if last := fields[len(fields)-1]; isLocaleCode(last) {
			locale = last
			name = strings.TrimSpace(strings.TrimSuffix(left, last))
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		label := name
		if locale != "" {
			label = name + "（" + locale + "）"
		}
		list = append(list, ttsVoice{name: name, locale: locale, label: label})
	}
	return list
}

func isLocaleCode(s string) bool {
	if len(s) < 5 || len(s) > 12 || !strings.ContainsAny(s, "_-") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// pickAutoVoice 优先 zh_CN，其次任意 zh；都没有就用系统默认嗓音。
func pickAutoVoice() (string, string) {
	list := cachedVoices()
	for _, want := range []string{"zh_CN", "zh"} {
		for _, v := range list {
			if strings.HasPrefix(v.locale, want) {
				return v.name, "自动选中文音色 " + v.name
			}
		}
	}
	return "", "本机没列出中文音色，用系统默认嗓音"
}

func (mediaTTS) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	text := in.Str("text")
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("朗读文本不能为空")
	}
	format := strings.ToLower(in.Str("format"))
	if format == "" {
		format = "aiff"
	}
	if format != "aiff" && format != "m4a" {
		return nil, fmt.Errorf("不支持的输出格式：%s", format)
	}
	dst, err := resolveOutput(c, in.Path("output"), "tts", format)
	if err != nil {
		return nil, err
	}
	if format == "m4a" {
		if _, ok := execx.LookPath("afconvert"); !ok {
			return nil, fmt.Errorf("导出 m4a 需要 /usr/bin/afconvert，本机没有")
		}
	}

	voice := in.Str("voice")
	voiceNote := ""
	if voice == "" || voice == "auto" {
		voice, voiceNote = pickAutoVoice()
	}

	// 文本只走临时文件，不拼命令行（坑 B1）。
	tmpDir, err := os.MkdirTemp(c.TempDir, "ms-tts-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败：%s", redactHome(c, err.Error()))
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	txtPath := filepath.Join(tmpDir, "text.txt")
	if err := os.WriteFile(txtPath, []byte(text), 0o600); err != nil {
		return nil, fmt.Errorf("写临时文本失败：%s", redactHome(c, err.Error()))
	}

	target := dst
	if format == "m4a" {
		target = filepath.Join(tmpDir, "raw.aiff")
	}
	args := []string{}
	if voice != "" {
		args = append(args, "-v", voice)
	}
	args = append(args, "-f", txtPath, "-o", target)
	c.Logf("调用 say 合成 %d 字%s", len([]rune(text)), voiceNote)
	c.Progress(20, "调用 say")
	res := c.Exec.Run(ctx, 10*time.Minute, "say", args...)
	if res.ExitCode != 0 {
		return nil, cmdFailure(c, "say 合成失败", res)
	}
	if st, err := os.Stat(target); err != nil || st.Size() == 0 {
		return nil, fmt.Errorf("say 报告成功但没有产出音频")
	}

	if format == "m4a" {
		c.Progress(70, "转 m4a")
		conv := c.Exec.Run(ctx, 10*time.Minute, "afconvert", "-f", "m4af", "-d", "aac", target, "-o", dst)
		if conv.ExitCode != 0 {
			return nil, cmdFailure(c, "afconvert 转 m4a 失败", conv)
		}
	}
	st, err := os.Stat(dst)
	if err != nil || st.Size() == 0 {
		return nil, fmt.Errorf("没有产出音频文件")
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK:  true,
		Msg: fmt.Sprintf("已生成 %s（%s）", filepath.Base(dst), humanSize(st.Size())),
		Data: map[string]any{
			"output": dst, "format": format, "chars": len([]rune(text)),
			"voice": voice, "voice_note": voiceNote, "size": st.Size(),
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ============================================================================
//  media.extract_audio —— 系统 CLI 做不到，如实标不可用
// ============================================================================

type mediaExtractAudio struct{}

func init() { Add(mediaExtractAudio{}) }

// extractAudioReason 是实机验证过的事实：afconvert 打不开视频容器。
const extractAudioReason = "系统自带 CLI 不支持从视频容器抽音轨，需要 ffmpeg（Phase2）"

func (mediaExtractAudio) Meta() tool.Meta {
	return tool.Meta{
		ID: "media.extract_audio", Name: "抽取视频音轨", Category: "media", Icon: "waveform",
		Summary:   "从视频容器抽音轨（系统 CLI 做不到，待 Phase2）。",
		Async:     true,
		Available: false, UnavailableReason: extractAudioReason,
		Params: []tool.Param{
			{Name: "input", Label: "视频文件", Type: tool.TypePath,
				Placeholder: "/Users/你/Movies/a.mp4", Help: "只能读允许的读根内的文件。"},
			{Name: "output", Label: "输出音频", Type: tool.TypeOutPath, AllowMissing: true,
				Placeholder: "留空写到可写根，自动命名", Help: "只能写到允许的可写根内。"},
		},
	}
}

func (mediaExtractAudio) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	// Runner 会在执行前用 unavailable_reason 拒绝，这里只兜底。
	return nil, fmt.Errorf("%s", extractAudioReason)
}
