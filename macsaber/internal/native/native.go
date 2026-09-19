// Package native 是原生框架桥：PDFKit / Vision 只能经 Apple 脚本桥调用。
//
// 结论：本机 /usr/bin/python3 与 brew python3 都没有 PyObjC，实测可用的是
// `osascript -l JavaScript`（JXA ObjC.import）；载荷走临时文件、只传路径，
// 绝不 sh -c 拼串（坑 B1），也避开 osascript 把非 ASCII 参数变乱码（坑 N3）。
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
)

// Bridge 是原生桥后端：python3+PyObjC 优先，退路是 JXA。
const (
	BridgePyObjC = "pyobjc"
	BridgeJXA    = "jxa"
)

// ProbeTTL 是探测结论缓存时长。
const probeTTL = 10 * time.Minute

// Probe 是原生桥的真实探测结论。
type Probe struct {
	Available bool
	Reason    string
	Backend   string
}

// Script 描述一段原生脚本怎么跑：命令 + 前置参数 + 常量正文。
type Script struct {
	Name    string
	Cmd     string
	PreArgs []string
	Body    string
	Timeout time.Duration
}

// ---------- 结果类型 ----------

// OCRLine 是识别到的一行文字。
type OCRLine struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
}

// OCRResult 是 ocr.image 的原生返回。
type OCRResult struct {
	Lines []OCRLine `json:"lines"`
	Text  string    `json:"text"`
	Note  string    `json:"note,omitempty"`
}

// Barcode 是一条条码/二维码。
type Barcode struct {
	Symbology string `json:"symbology"`
	Payload   string `json:"payload"`
}

// BarcodeResult 是 ocr.qrcode 的原生返回。
type BarcodeResult struct {
	Items []Barcode `json:"items"`
}

// Corners 是文档四角（归一化坐标，Vision 原点在左下）。
type Corners struct {
	TopLeft     [2]float64 `json:"top_left"`
	TopRight    [2]float64 `json:"top_right"`
	BottomLeft  [2]float64 `json:"bottom_left"`
	BottomRight [2]float64 `json:"bottom_right"`
}

// DocumentResult 是 ocr.deskew 的原生返回。
type DocumentResult struct {
	Found   bool     `json:"found"`
	Corners *Corners `json:"corners,omitempty"`
}

// PDFPageInfo 是一页的尺寸（点）。
type PDFPageInfo struct {
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// PDFInfoResult 是 pdf.info 的原生返回。
type PDFInfoResult struct {
	Pages     int            `json:"pages"`
	Encrypted bool           `json:"encrypted"`
	Locked    bool           `json:"locked"`
	Title     string         `json:"title"`
	Author    string         `json:"author"`
	Subject   string         `json:"subject"`
	Creator   string         `json:"creator"`
	Producer  string         `json:"producer"`
	Created   string         `json:"created"`
	Modified  string         `json:"modified"`
	PageSizes []PDFPageInfo  `json:"page_sizes"`
	Notes     []string       `json:"notes,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// PDFTextPage 是一页的文本。
type PDFTextPage struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// PDFTextResult 是 pdf.text 的原生返回。
type PDFTextResult struct {
	Pages []PDFTextPage `json:"pages"`
	Text  string        `json:"text"`
	Note  string        `json:"note,omitempty"`
}

// ---------- 载荷 ----------

type ocrPayload struct {
	Op    string   `json:"op"`
	Input string   `json:"input"`
	Langs []string `json:"langs,omitempty"`
}

type documentPayload struct {
	Op    string `json:"op"`
	Input string `json:"input"`
}

type pdfMergePayload struct {
	Op     string   `json:"op"`
	Inputs []string `json:"inputs"`
	Output string   `json:"output"`
}

type pdfOnePayload struct {
	Op     string `json:"op"`
	Input  string `json:"input"`
	Output string `json:"output,omitempty"`
}

type pdfTextPayload struct {
	Op      string `json:"op"`
	Input   string `json:"input"`
	MaxChar int    `json:"max_chars,omitempty"`
}

type pdfEncryptPayload struct {
	Op       string `json:"op"`
	Input    string `json:"input"`
	Output   string `json:"output"`
	Password string `json:"password"`
	Owner    string `json:"owner,omitempty"`
}

// ---------- 探测 ----------

// Ctx 是原生桥调用上下文。
type Ctx struct {
	Exec    *execx.Execer
	Probes  *execx.ProbeCache
	TempDir string
}

// NewCtx 造一个原生桥上下文。
func NewCtx(exec *execx.Execer, probes *execx.ProbeCache, tempDir string) *Ctx {
	return &Ctx{Exec: exec, Probes: probes, TempDir: tempDir}
}

// Probe 跑（或取缓存）原生桥探测。
func (c *Ctx) Probe(ctx context.Context) Probe {
	if c == nil {
		return Probe{Reason: "原生桥上下文缺失"}
	}
	p, err := RunProbe(ctx, c.Exec, c.Probes, c.TempDir)
	if err != nil {
		return Probe{Reason: err.Error()}
	}
	return p
}

const cacheKey = "native.frameworks"
const backendKey = "native.frameworks.backend"

// RunProbe 逐条给出真实原因：python3 不存在 / PyObjC 缺模块 / JXA 不可用。
func RunProbe(ctx context.Context, ex *execx.Execer, cache *execx.ProbeCache, tmpDir string) (Probe, error) {
	if ex == nil {
		return Probe{}, errors.New("原生桥缺少执行器")
	}
	if cache != nil {
		if ok, reason, hit := cache.Get(cacheKey); hit {
			p := Probe{Available: ok, Reason: reason}
			if ok {
				if _, b, hit2 := cache.Get(backendKey); hit2 {
					p.Backend, p.Reason = b, ""
				}
			}
			return p, nil
		}
	}
	pyReason := ""
	if _, ok := execx.LookPath("python3"); !ok {
		pyReason = "找不到命令 python3"
	} else if ok, reason := runPyObjCProbe(ctx, ex); ok {
		return storeProbe(cache, Probe{Available: true, Backend: BridgePyObjC}), nil
	} else {
		pyReason = reason
	}
	jxaReason := ""
	if _, ok := execx.LookPath("osascript"); !ok {
		jxaReason = "找不到命令 osascript"
	} else if ok, reason := runJXAProbe(ctx, ex, tmpDir); ok {
		return storeProbe(cache, Probe{Available: true, Backend: BridgeJXA}), nil
	} else {
		jxaReason = reason
	}
	reason := fmt.Sprintf("python3 后端不可用（%s）；JXA 后端不可用（%s）", pyReason, jxaReason)
	return storeProbe(cache, Probe{Available: false, Reason: reason}), nil
}

func storeProbe(cache *execx.ProbeCache, p Probe) Probe {
	if cache != nil {
		reason := p.Reason
		if p.Available {
			reason = ""
		}
		cache.Put(cacheKey, p.Available, reason, probeTTL)
		cache.Put(backendKey, true, p.Backend, probeTTL)
	}
	return p
}

// runPyObjCProbe 让 python3 自己交代缺哪个模块（不猜）。
func runPyObjCProbe(ctx context.Context, ex *execx.Execer) (bool, string) {
	py, _ := execx.LookPath("python3")
	res := ex.Run(ctx, 20*time.Second, py, "-c", pyProbeCode)
	if res.ExitCode == 0 {
		return true, ""
	}
	if res.TimedOut {
		return false, "探测超时被终止"
	}
	msg := strings.TrimSpace(res.Output())
	if strings.Contains(msg, "CommandLineTools") || strings.Contains(msg, "xcrun") {
		return false, "需要先安装命令行开发者工具"
	}
	if m := missingModule(msg); m != "" {
		return false, "PyObjC 模块缺失：" + m
	}
	return false, "PyObjC 不可用：" + shortLine(msg)
}

// pyProbeCode 只 import 框架，不碰任何用户数据。
const pyProbeCode = `import sys
mods = ["Quartz", "Vision", "PDFKit"]
missing = []
for m in mods:
    try:
        __import__(m)
    except Exception:
        missing.append(m)
if missing:
    sys.stderr.write("MISSING " + ",".join(missing) + "\n")
    sys.exit(2)
sys.stdout.write("OK\n")`

// probeScript 逐项验证框架类真的存在；缺哪个报哪个。
const probeScript = `function run(argv){
  ObjC.import('Foundation');
  var missing = [];
  try { ObjC.import('PDFKit'); } catch (e) { missing.push('PDFKit'); }
  try { ObjC.import('Vision'); } catch (e) { missing.push('Vision'); }
  if (missing.length === 0) {
    try {
      if (typeof $.PDFDocument !== 'function') missing.push('PDFKit(PDFDocument)');
      if (typeof $.VNRecognizeTextRequest !== 'function') missing.push('Vision(VNRecognizeTextRequest)');
    } catch (e) { missing.push('类查找失败'); }
  }
  if (missing.length > 0) { console.log('MISSING ' + missing.join(',')); $.exit(2); }
  console.log('OK');
}`

func missingModule(msg string) string {
	for _, ln := range strings.Split(msg, "\n") {
		if i := strings.Index(ln, "MISSING "); i >= 0 {
			return strings.TrimSpace(ln[i+len("MISSING "):])
		}
	}
	return ""
}

func runJXAProbe(ctx context.Context, ex *execx.Execer, tmpDir string) (bool, string) {
	path, cleanup, err := writeTempScript(tmpDir, probeScript)
	if err != nil {
		return false, "写临时脚本失败：" + err.Error()
	}
	defer cleanup()
	res := ex.Run(ctx, 20*time.Second, "osascript", "-l", "JavaScript", path)
	if res.ExitCode == 0 {
		return true, ""
	}
	if res.TimedOut {
		return false, "探测超时被终止"
	}
	msg := strings.TrimSpace(res.Output())
	if i := strings.Index(msg, "MISSING "); i >= 0 {
		return false, "JXA 框架缺失：" + strings.TrimSpace(msg[i+len("MISSING "):])
	}
	return false, "JXA 执行失败：" + shortLine(msg)
}

// ---------- 调用 ----------

// maxPayloadBytes 是 argv 载荷上限；超过直接拒绝，不静默截断。
const maxPayloadBytes = 8 << 20

// EncodePayload 把载荷编成 JSON（JXA 侧 JSON.parse 读第一个参数）。
func EncodePayload(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if len(b) > maxPayloadBytes {
		return "", fmt.Errorf("参数过大（%d 字节，上限 %d）", len(b), maxPayloadBytes)
	}
	return string(b), nil
}

// Call 用给定的桥跑一段脚本；载荷走 argv，脚本正文是常量。
func Call(ctx context.Context, c *Ctx, p Probe, sc Script, payload any) (string, error) {
	if c == nil || c.Exec == nil {
		return "", errors.New("原生桥缺少执行器")
	}
	if !p.Available {
		reason := p.Reason
		if reason == "" {
			reason = "原生框架不可用"
		}
		return "", errors.New(SanitizeMessage(reason))
	}
	arg, err := EncodePayload(payload)
	if err != nil {
		return "", err
	}
	if sc.Cmd == "" {
		return "", errors.New("原生桥脚本缺少命令")
	}
	// 载荷写文件、argv 只传路径：osascript 的 argv 会把非 ASCII 变成乱码（坑 N3）。
	payPath, payCleanup, perr := writeTempPayload(c.TempDir, arg)
	if perr != nil {
		return "", fmt.Errorf("写临时载荷失败：%w", perr)
	}
	defer payCleanup()
	timeout := sc.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	args := append([]string{}, sc.PreArgs...)
	if sc.Body != "" {
		path, cleanup, werr := writeTempScript(c.TempDir, sc.Body)
		if werr != nil {
			return "", fmt.Errorf("写临时脚本失败：%w", werr)
		}
		defer cleanup()
		args = append(args, path)
	}
	args = append(args, payPath)
	res := c.Exec.Run(ctx, timeout, sc.Cmd, args...)
	if res.TimedOut {
		return "", fmt.Errorf("%s 超过 %s 未完成，已终止", sc.Name, timeout.Round(time.Second))
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s 失败（退出码 %d）：%s", sc.Name, res.ExitCode, shortLine(res.Output()))
	}
	// JXA 在 run(argv) 里把 console.log 写到 stderr（坑 N2），两边都要看。
	if out := res.Output(); strings.TrimSpace(out) != "" {
		return out, nil
	}
	return res.Stdout, nil
}

// callJSON 跑脚本并把输出里的 JSON 行解成 out（脚本保证结果里有 error 或 data）。
func callJSON(ctx context.Context, c *Ctx, p Probe, sc Script, payload, out any) error {
	raw, err := Call(ctx, c, p, sc, payload)
	if err != nil {
		return err
	}
	line := lastJSONLine(raw)
	if line == "" {
		return fmt.Errorf("%s 没有返回结果", sc.Name)
	}
	if err := json.Unmarshal([]byte(line), out); err != nil {
		return fmt.Errorf("%s 返回无法解析：%s", sc.Name, shortLine(line))
	}
	// 脚本侧把失败原因放在 error 字段里，必须原样抛出来。
	var probe struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(line), &probe)
	if strings.TrimSpace(probe.Error) != "" {
		return errors.New(SanitizeMessage(probe.Error))
	}
	return nil
}

// lastJSONLine 取最后一行非空输出（osascript 偶尔在前面打警告）。
func lastJSONLine(stdout string) string {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			return s
		}
	}
	return ""
}

// writeTempScript 把常量脚本落到临时文件（0600），用完即删。
func writeTempScript(dir, body string) (string, func(), error) {
	return writeTempFile(dir, "macsaber-native-*", body)
}

func writeTempFile(dir, pattern, body string) (string, func(), error) {
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// writeTempPayload 把 UTF-8 载荷落到临时文件（0600），argv 只传路径。
func writeTempPayload(dir, body string) (string, func(), error) {
	return writeTempFile(dir, "macsaber-payload-*.json", body)
}

// shortLine 取首行并截断；报错要带真实原因但不回显完整家目录。
func shortLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "无输出"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return SanitizeMessage(s)
}

// SanitizeMessage 把消息里的家目录缩成 ~，避免回显完整路径。
func SanitizeMessage(s string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return s
	}
	return strings.ReplaceAll(s, home, "~")
}

// capabilityScript 检查某个框架类在旧系统上到底有没有（如实回答，不猜）。
const capabilityScript = `function run(argv){
  ObjC.import('Foundation');
  var P = null;
  try {
    var raw = $.NSString.stringWithContentsOfFileEncodingError($(argv[0]), 4, $());
    P = raw.isNil() ? null : JSON.parse(ObjC.unwrap(raw));
  } catch (e) { P = null; }
  function end(o){ console.log(JSON.stringify(o)); }
  if (!P) { return end({available: false, reason: '缺少参数'}); }
  var fw = P.framework || '', cls = P.class || '';
  if (fw) { try { ObjC.import(fw); } catch (e) { return end({available: false, reason: '系统没有 ' + fw + ' 框架'}); } }
  try {
    if (typeof $[cls] !== 'function') { return end({available: false, reason: '系统不支持 ' + cls + '（需要更新的 macOS）'}); }
  } catch (e) { return end({available: false, reason: '类查找失败：' + e}); }
  end({available: true});
}`

// CapabilityResult 是一项原生能力的探测结论。
type CapabilityResult struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Capability 探测某个框架类是否真的存在（如 VNDetectDocumentSegmentationRequest）。
func (c *Ctx) Capability(ctx context.Context, p Probe, key, framework, class string) CapabilityResult {
	if !p.Available {
		return CapabilityResult{Reason: p.Reason}
	}
	if c.Probes != nil {
		if ok, reason, hit := c.Probes.Get(key); hit {
			return CapabilityResult{Available: ok, Reason: reason}
		}
	}
	sc := Script{Name: class + " 探测", Cmd: "osascript", PreArgs: []string{"-l", "JavaScript"},
		Body: capabilityScript, Timeout: 30 * time.Second}
	out := CapabilityResult{}
	if err := callJSON(ctx, c, p, sc, map[string]string{"framework": framework, "class": class}, &out); err != nil {
		out = CapabilityResult{Reason: err.Error()}
	}
	if c.Probes != nil {
		reason := out.Reason
		if out.Available {
			reason = ""
		}
		c.Probes.Put(key, out.Available, reason, probeTTL)
	}
	return out
}

// ---------- 桥方法 ----------

func (c *Ctx) script(p Probe, pick func(visionScripts, pdfScripts) Script) (Script, error) {
	v, pm, err := scripts(p.Backend, c.TempDir)
	if err != nil {
		return Script{}, err
	}
	return pick(v, pm), nil
}

// OCRImage 识别图片文字（中文优先）。
func (c *Ctx) OCRImage(ctx context.Context, p Probe, input string, langs []string) (*OCRResult, error) {
	sc, err := c.script(p, func(v visionScripts, _ pdfScripts) Script { return v.ocr })
	if err != nil {
		return nil, err
	}
	out := &OCRResult{}
	if err := callJSON(ctx, c, p, sc, ocrPayload{Op: "ocr", Input: input, Langs: langs}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Barcodes 识别二维码/条码。
func (c *Ctx) Barcodes(ctx context.Context, p Probe, input string) (*BarcodeResult, error) {
	sc, err := c.script(p, func(v visionScripts, _ pdfScripts) Script { return v.barcode })
	if err != nil {
		return nil, err
	}
	out := &BarcodeResult{}
	if err := callJSON(ctx, c, p, sc, ocrPayload{Op: "barcode", Input: input}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// DetectDocument 找文档四角（Vision 文档分割）。
func (c *Ctx) DetectDocument(ctx context.Context, p Probe, input string) (*DocumentResult, error) {
	sc, err := c.script(p, func(v visionScripts, _ pdfScripts) Script { return v.document })
	if err != nil {
		return nil, err
	}
	out := &DocumentResult{}
	if err := callJSON(ctx, c, p, sc, documentPayload{Op: "document", Input: input}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// MergePDF 按顺序合并 PDF。
func (c *Ctx) MergePDF(ctx context.Context, p Probe, inputs []string, output string) error {
	sc, err := c.script(p, func(_ visionScripts, m pdfScripts) Script { return m.merge })
	if err != nil {
		return err
	}
	return callJSON(ctx, c, p, sc, pdfMergePayload{Op: "merge", Inputs: inputs, Output: output}, &struct{}{})
}

// SplitPDF 每页导出成一个 PDF，返回产物路径。
func (c *Ctx) SplitPDF(ctx context.Context, p Probe, input, outDir string) ([]string, error) {
	sc, err := c.script(p, func(_ visionScripts, m pdfScripts) Script { return m.split })
	if err != nil {
		return nil, err
	}
	out := struct {
		Files []string `json:"files"`
	}{}
	if err := callJSON(ctx, c, p, sc, pdfOnePayload{Op: "split", Input: input, Output: outDir}, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// PDFText 提取文本；maxChars 为 0 表示全文。
func (c *Ctx) PDFText(ctx context.Context, p Probe, input string, maxChars int) (*PDFTextResult, error) {
	sc, err := c.script(p, func(_ visionScripts, m pdfScripts) Script { return m.text })
	if err != nil {
		return nil, err
	}
	out := &PDFTextResult{}
	if err := callJSON(ctx, c, p, sc, pdfTextPayload{Op: "text", Input: input, MaxChar: maxChars}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// PDFInfo 读页数、尺寸、加密状态与元数据。
func (c *Ctx) PDFInfo(ctx context.Context, p Probe, input string) (*PDFInfoResult, error) {
	sc, err := c.script(p, func(_ visionScripts, m pdfScripts) Script { return m.info })
	if err != nil {
		return nil, err
	}
	out := &PDFInfoResult{}
	if err := callJSON(ctx, c, p, sc, pdfOnePayload{Op: "info", Input: input}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// EncryptPDF 生成带打开口令的新 PDF（不动源文件）。
func (c *Ctx) EncryptPDF(ctx context.Context, p Probe, input, output, password, owner string) error {
	sc, err := c.script(p, func(_ visionScripts, m pdfScripts) Script { return m.encrypt })
	if err != nil {
		return err
	}
	payload := pdfEncryptPayload{Op: "encrypt", Input: input, Output: output, Password: password, Owner: owner}
	return callJSON(ctx, c, p, sc, payload, &struct{}{})
}

// ---------- 脚本装配 ----------

type visionScripts struct {
	ocr      Script
	barcode  Script
	document Script
}

type pdfScripts struct {
	merge   Script
	split   Script
	text    Script
	info    Script
	encrypt Script
}

// scripts 按后端给出脚本：JXA 用常量正文文件，PyObjC 用 -c 常量正文。
func scripts(bridge, tmpDir string) (visionScripts, pdfScripts, error) {
	switch bridge {
	case BridgeJXA:
		mk := func(name, body string) Script {
			return Script{Name: name, Cmd: "osascript", PreArgs: []string{"-l", "JavaScript"}, Body: body}
		}
		return visionScripts{
			ocr:      mk("Vision 文字识别", visionOCRScript),
			barcode:  mk("Vision 条码识别", visionBarcodeScript),
			document: mk("Vision 文档校正", visionDocumentScript),
		}, pdfScripts{
			merge:   mk("PDFKit 合并", pdfMergeScript),
			split:   mk("PDFKit 拆分", pdfSplitScript),
			text:    mk("PDFKit 提取文本", pdfTextScript),
			info:    mk("PDFKit 读取信息", pdfInfoScript),
			encrypt: mk("PDFKit 加密", pdfEncryptScript),
		}, nil
	case BridgePyObjC:
		mk := func(name, body string) Script {
			return Script{Name: name, Cmd: "python3", PreArgs: []string{"-c", body}}
		}
		return visionScripts{
			ocr:      mk("Vision 文字识别", pyOCRScript),
			barcode:  mk("Vision 条码识别", pyBarcodeScript),
			document: mk("Vision 文档校正", pyDocumentScript),
		}, pdfScripts{
			merge:   mk("PDFKit 合并", pyMergeScript),
			split:   mk("PDFKit 拆分", pySplitScript),
			text:    mk("PDFKit 提取文本", pyTextScript),
			info:    mk("PDFKit 读取信息", pyInfoScript),
			encrypt: mk("PDFKit 加密", pyEncryptScript),
		}, nil
	}
	return visionScripts{}, pdfScripts{}, fmt.Errorf("未知原生桥后端 %q", bridge)
}

// ---------- JXA 脚本（常量） ----------
//
// 注意：JXA 脚本文件里只有 run 函数体在作用域内，顶层 function 在 run 里取不到，
// 所以每段脚本必须自包含（坑 N1：常量拼接的辅助函数会被 JXA 丢掉）。

const jxaPreamble = `function run(argv){
  ObjC.import('Foundation');
  var P = null;
  try {
    var raw = $.NSString.stringWithContentsOfFileEncodingError($(argv[0]), 4, $());
    P = raw.isNil() ? null : JSON.parse(ObjC.unwrap(raw));
  } catch (e) { P = null; }
  function end(o){ console.log(JSON.stringify(o)); }
  function fail(m){ end({error: String(m)}); }
  if (!P) { return fail('缺少参数'); }
  function cgImage(path){
    ObjC.import('AppKit');
    var reps = $.NSBitmapImageRep.imageRepsWithContentsOfFile($(path));
    if (reps.isNil() || reps.count === 0) { return null; }
    return reps.objectAtIndex(0).CGImage;
  }`

const jxaPDFHelpers = `
  ObjC.import('PDFKit');
  function openPDF(path){
    var d = $.PDFDocument.alloc.initWithURL($.NSURL.fileURLWithPath($(path)));
    if (d.isNil()) { return null; }
    return d;
  }
  function attrsOf(doc){
    var out = {};
    var a = doc.documentAttributes;
    if (a.isNil()) { return out; }
    var keys = ['Title','Author','Subject','Creator','Producer','CreationDate','ModDate'];
    for (var i = 0; i < keys.length; i++) {
      var v = a.objectForKey(keys[i]);
      if (v && !v.isNil()) { out[keys[i]] = String(ObjC.unwrap(v)); }
    }
    return out;
  }`

const visionOCRScript = jxaPreamble + `
  ObjC.import('Vision');
  var cg = cgImage(P.input);
  if (!cg) { return fail('无法读取图片，可能不是图片文件'); }
  var req = $.VNRecognizeTextRequest.alloc.init;
  // 必须用 accurate（0）：fast（1）会把中文识别成乱码（坑 N4）。
  req.recognitionLevel = 0;
  req.usesLanguageCorrection = true;
  if (P.langs && P.langs.length) { req.recognitionLanguages = $(P.langs); }
  var h = $.VNImageRequestHandler.alloc.initWithCGImageOptions(cg, $());
  if (!h.performRequestsError($([req]), $())) { return fail('Vision 识别失败'); }
  var rs = req.results, lines = [], all = [];
  for (var i = 0; i < rs.count; i++) {
    var c = rs.objectAtIndex(i).topCandidates(1);
    if (c.count === 0) { continue; }
    var s = ObjC.unwrap(c.objectAtIndex(0).string);
    lines.push({text: s, confidence: Number(c.objectAtIndex(0).confidence)});
    all.push(s);
  }
  end({lines: lines, text: all.join('\n')});
}`

const visionBarcodeScript = jxaPreamble + `
  ObjC.import('Vision');
  var cg = cgImage(P.input);
  if (!cg) { return fail('无法读取图片，可能不是图片文件'); }
  var req = $.VNDetectBarcodesRequest.alloc.init;
  var h = $.VNImageRequestHandler.alloc.initWithCGImageOptions(cg, $());
  if (!h.performRequestsError($([req]), $())) { return fail('条码识别失败'); }
  var rs = req.results, items = [];
  for (var i = 0; i < rs.count; i++) {
    var r = rs.objectAtIndex(i);
    var pay = r.payloadStringValue;
    items.push({symbology: String(ObjC.unwrap(r.symbology)), payload: pay.isNil() ? '' : String(ObjC.unwrap(pay))});
  }
  end({items: items});
}`

const visionDocumentScript = jxaPreamble + `
  ObjC.import('Vision');
  var cg = cgImage(P.input);
  if (!cg) { return fail('无法读取图片，可能不是图片文件'); }
  var req = $.VNDetectDocumentSegmentationRequest.alloc.init;
  var h = $.VNImageRequestHandler.alloc.initWithCGImageOptions(cg, $());
  if (!h.performRequestsError($([req]), $())) { return fail('文档检测失败'); }
  if (req.results.isNil() || req.results.count === 0) { return end({found: false}); }
  var o = req.results.objectAtIndex(0);
  end({found: true, corners: {
    top_left: [o.topLeft.x, o.topLeft.y], top_right: [o.topRight.x, o.topRight.y],
    bottom_left: [o.bottomLeft.x, o.bottomLeft.y], bottom_right: [o.bottomRight.x, o.bottomRight.y]}});
}`

const pdfMergeScript = jxaPreamble + jxaPDFHelpers + `
  if (!P.inputs || !P.inputs.length) { return fail('没有输入 PDF'); }
  var out = $.PDFDocument.alloc.init;
  for (var i = 0; i < P.inputs.length; i++) {
    var d = openPDF(P.inputs[i]);
    if (!d) { return fail('打不开第 ' + (i+1) + ' 个 PDF（加密或损坏？）'); }
    if (d.isLocked) { return fail('第 ' + (i+1) + ' 个 PDF 有打开口令，无法合并'); }
    for (var k = 0; k < d.pageCount; k++) { out.insertPageAtIndex(d.pageAtIndex(k), out.pageCount); }
  }
  if (out.pageCount === 0) { return fail('合并后没有页面'); }
  if (!out.writeToFile($(P.output))) { return fail('写不出合并结果'); }
  end({ok: true, pages: Number(out.pageCount)});
}`

const pdfSplitScript = jxaPreamble + jxaPDFHelpers + `
  var d = openPDF(P.input);
  if (!d) { return fail('打不开 PDF（加密或损坏？）'); }
  if (d.isLocked) { return fail('PDF 有打开口令，无法拆分'); }
  var files = [];
  for (var i = 0; i < d.pageCount; i++) {
    var path = P.output + '/page-' + (i+1) + '.pdf';
    var data = d.pageAtIndex(i).dataRepresentation;
    if (data.isNil() || !data.writeToFileAtomically($(path), true)) { return fail('写不出第 ' + (i+1) + ' 页'); }
    files.push(path);
  }
  end({ok: true, files: files});
}`

const pdfTextScript = jxaPreamble + jxaPDFHelpers + `
  var d = openPDF(P.input);
  if (!d) { return fail('打不开 PDF（加密或损坏？）'); }
  if (d.isLocked) { return fail('PDF 有打开口令，无法提取文本'); }
  var pages = [], parts = [];
  for (var i = 0; i < d.pageCount; i++) {
    var s = d.pageAtIndex(i).string;
    var t = s.isNil() ? '' : String(ObjC.unwrap(s));
    pages.push({index: i, text: t});
    parts.push(t);
  }
  var all = parts.join('\n');
  end({pages: pages, text: all, note: all.length === 0 ? '这份 PDF 没有文本层（可能是扫描件）' : ''});
}`

const pdfInfoScript = jxaPreamble + jxaPDFHelpers + `
  var d = openPDF(P.input);
  if (!d) { return fail('打不开 PDF（损坏或不是 PDF？）'); }
  var a = attrsOf(d), sizes = [], notes = [];
  if (d.isLocked) { notes.push('文档有打开口令，页数/尺寸/元数据均无法读取'); }
  else {
    for (var i = 0; i < d.pageCount; i++) {
      var b = d.pageAtIndex(i).boundsForBox(0);
      sizes.push({width: b.size.width, height: b.size.height});
    }
  }
  end({pages: Number(d.pageCount), encrypted: d.isEncrypted, locked: d.isLocked,
    title: a.Title || '', author: a.Author || '', subject: a.Subject || '',
    creator: a.Creator || '', producer: a.Producer || '',
    created: a.CreationDate || '', modified: a.ModDate || '',
    page_sizes: sizes, notes: notes});
}`

const pdfEncryptScript = jxaPreamble + jxaPDFHelpers + `
  if (!P.password) { return fail('缺少打开口令'); }
  var d = openPDF(P.input);
  if (!d) { return fail('打不开 PDF（损坏或不是 PDF？）'); }
  if (d.isLocked) { return fail('PDF 已有打开口令，请先解密再加密'); }
  var keys = $(['PDFDocumentUserPasswordOption', 'PDFDocumentOwnerPasswordOption']);
  var vals = $([P.password, P.owner || P.password]);
  var opts = $.NSDictionary.dictionaryWithObjectsForKeys(vals, keys);
  if (!d.writeToFileWithOptions($(P.output), opts)) { return fail('写不出加密结果'); }
  end({ok: true});
}`

// ---------- PyObjC 脚本（常量，本机无 PyObjC 未实测） ----------

const pyHead = `import json, sys
try:
    import Quartz, Vision, PDFKit
    from Foundation import NSURL, NSDictionary, NSData
except Exception as e:
    print(json.dumps({"error": "PyObjC 不可用: %s" % e})); sys.exit(0)
p = json.loads(sys.argv[1])
def emit(o): print(json.dumps(o, ensure_ascii=False))
def fail(m): print(json.dumps({"error": m}, ensure_ascii=False)); sys.exit(0)
def cgimage(path):
    src = Quartz.CGImageSourceCreateWithURL(NSURL.fileURLWithPath_(path), None)
    if src is None: return None
    return Quartz.CGImageSourceCreateImageAtIndex(src, 0, None)
def tl_rect(r):
    h = r.size.height
    return Quartz.CGRectMake(r.origin.x, r.origin.y, r.size.width, h)
`

const pyOCRScript = pyHead + `
cg = cgimage(p["input"])
if cg is None: fail("无法读取图片，可能不是图片文件")
req = Vision.VNRecognizeTextRequest.alloc().init()
# accurate（0）：fast 会把中文识别成乱码（坑 N4）。
req.setRecognitionLevel_(0)
req.setUsesLanguageCorrection_(True)
if p.get("langs"): req.setRecognitionLanguages_(p["langs"])
h = Vision.VNImageRequestHandler.alloc().initWithCGImage_options_(cg, None)
ok, err = h.performRequests_error_([req], None)
if not ok: fail("Vision 识别失败: %s" % err)
lines = []
for o in (req.results() or []):
    c = o.topCandidates_(1)
    if c: lines.append({"text": c[0].string(), "confidence": float(c[0].confidence())})
emit({"lines": lines, "text": "\n".join(x["text"] for x in lines)})
`

const pyBarcodeScript = pyHead + `
cg = cgimage(p["input"])
if cg is None: fail("无法读取图片，可能不是图片文件")
req = Vision.VNDetectBarcodesRequest.alloc().init()
h = Vision.VNImageRequestHandler.alloc().initWithCGImage_options_(cg, None)
ok, err = h.performRequests_error_([req], None)
if not ok: fail("条码识别失败: %s" % err)
items = []
for r in (req.results() or []):
    items.append({"symbology": str(r.symbology()), "payload": r.payloadStringValue() or ""})
emit({"items": items})
`

const pyDocumentScript = pyHead + `
cg = cgimage(p["input"])
if cg is None: fail("无法读取图片，可能不是图片文件")
req = Vision.VNDetectDocumentSegmentationRequest.alloc().init()
h = Vision.VNImageRequestHandler.alloc().initWithCGImage_options_(cg, None)
ok, err = h.performRequests_error_([req], None)
if not ok: fail("文档检测失败: %s" % err)
rs = req.results() or []
if not rs: emit({"found": False})
else:
    o = rs[0]
    emit({"found": True, "corners": {
        "top_left": [o.topLeft().x, o.topLeft().y], "top_right": [o.topRight().x, o.topRight().y],
        "bottom_left": [o.bottomLeft().x, o.bottomLeft().y], "bottom_right": [o.bottomRight().x, o.bottomRight().y]}})
`

const pyPDFHead = `import json, sys
try:
    import Quartz, PDFKit
    from Foundation import NSURL, NSDictionary
except Exception as e:
    print(json.dumps({"error": "PyObjC 不可用: %s" % e})); sys.exit(0)
p = json.loads(sys.argv[1])
def emit(o): print(json.dumps(o, ensure_ascii=False))
def fail(m): print(json.dumps({"error": m}, ensure_ascii=False)); sys.exit(0)
def openpdf(path):
    d = PDFKit.PDFDocument.alloc().initWithURL_(NSURL.fileURLWithPath_(path))
    return d
`

const pyMergeScript = pyPDFHead + `
out = PDFKit.PDFDocument.alloc().init()
for i, path in enumerate(p["inputs"]):
    d = openpdf(path)
    if d is None: fail("打不开第 %d 个 PDF（加密或损坏？）" % (i+1))
    if d.isLocked(): fail("第 %d 个 PDF 有打开口令，无法合并" % (i+1))
    for k in range(d.pageCount()):
        out.insertPageAtIndex_atIndex_(d.pageAtIndex_(k), out.pageCount())
if out.pageCount() == 0: fail("合并后没有页面")
if not out.writeToFile_(p["output"]): fail("写不出合并结果")
emit({"ok": True, "pages": int(out.pageCount()))
`

const pySplitScript = pyPDFHead + `
d = openpdf(p["input"])
if d is None: fail("打不开 PDF（加密或损坏？）")
if d.isLocked(): fail("PDF 有打开口令，无法拆分")
files = []
for i in range(d.pageCount()):
    path = p["output"] + "/page-%d.pdf" % (i+1)
    data = d.pageAtIndex_(i).dataRepresentation()
    if data is None or not data.writeToFile_atomically_(path, True): fail("写不出第 %d 页" % (i+1))
    files.append(path)
emit({"ok": True, "files": files})
`

const pyTextScript = pyPDFHead + `
d = openpdf(p["input"])
if d is None: fail("打不开 PDF（加密或损坏？）")
if d.isLocked(): fail("PDF 有打开口令，无法提取文本")
pages, parts = [], []
for i in range(d.pageCount()):
    t = d.pageAtIndex_(i).string() or ""
    pages.append({"index": i, "text": t}); parts.append(t)
all_text = "\n".join(parts)
emit({"pages": pages, "text": all_text,
      "note": "" if all_text else "这份 PDF 没有文本层（可能是扫描件）"})
`

const pyInfoScript = pyPDFHead + `
d = openpdf(p["input"])
if d is None: fail("打不开 PDF（损坏或不是 PDF？）")
a = d.documentAttributes() or {}
def attr(k):
    v = a.get(k)
    return "" if v is None else str(v)
sizes, notes = [], []
if d.isLocked(): notes.append("文档有打开口令，页数/尺寸/元数据均无法读取")
else:
    for i in range(d.pageCount()):
        r = d.pageAtIndex_(i).boundsForBox_(0)
        sizes.append({"width": r.size.width, "height": r.size.height})
emit({"pages": d.pageCount(), "encrypted": bool(d.isEncrypted()), "locked": bool(d.isLocked()),
      "title": attr("Title"), "author": attr("Author"), "subject": attr("Subject"),
      "creator": attr("Creator"), "producer": attr("Producer"),
      "created": attr("CreationDate"), "modified": attr("ModDate"),
      "page_sizes": sizes, "notes": notes})
`

const pyEncryptScript = pyPDFHead + `
if not p.get("password"): fail("缺少打开口令")
d = openpdf(p["input"])
if d is None: fail("打不开 PDF（损坏或不是 PDF？）")
if d.isLocked(): fail("PDF 已有打开口令，请先解密再加密")
opts = NSDictionary.dictionaryWithObjects_forKeys_(
    [p["password"], p.get("owner") or p["password"]],
    ["PDFDocumentUserPasswordOption", "PDFDocumentOwnerPasswordOption"])
if not d.writeToFile_withOptions_(p["output"], opts): fail("写不出加密结果")
emit({"ok": True})
`

// ---------- 文档校正（Go 侧纯计算，避免再依赖 CoreImage 桥） ----------

// WarpDocument 按四角把文档裁正；corners 是归一化坐标（原点左下）。
func WarpDocument(src image.Image, cs Corners) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	// 归一化（原点左下）→ 像素（原点左上）。
	pt := func(c [2]float64) [2]float64 {
		return [2]float64{c[0] * float64(w), (1 - c[1]) * float64(h)}
	}
	tl, tr, bl, br := pt(cs.TopLeft), pt(cs.TopRight), pt(cs.BottomLeft), pt(cs.BottomRight)
	outW, outH := quadSize(tl, tr, bl, br)
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	for y := 0; y < outH; y++ {
		v := float64(y) / math.Max(1, float64(outH-1))
		for x := 0; x < outW; x++ {
			u := float64(x) / math.Max(1, float64(outW-1))
			top := lerp2(tl, tr, u)
			bot := lerp2(bl, br, u)
			p := lerp2(top, bot, v)
			dst.Set(x, y, bilinear(src, p[0], p[1]))
		}
	}
	return dst
}

// quadSize 取对边长度的最大值作为输出尺寸。
func quadSize(tl, tr, bl, br [2]float64) (int, int) {
	top := dist(tl, tr)
	bot := dist(bl, br)
	left := dist(tl, bl)
	right := dist(tr, br)
	w := int(math.Round(math.Max(top, bot)))
	h := int(math.Round(math.Max(left, right)))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

func dist(a, b [2]float64) float64 {
	return math.Hypot(a[0]-b[0], a[1]-b[1])
}

func lerp2(a, b [2]float64, t float64) [2]float64 {
	return [2]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t}
}

// bilinear 双线性取样，越界取最近边缘。
func bilinear(src image.Image, x, y float64) color.RGBA {
	b := src.Bounds()
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)
	c := func(px, py int) (r, g, bl, a uint32) {
		if px < b.Min.X {
			px = b.Min.X
		}
		if py < b.Min.Y {
			py = b.Min.Y
		}
		if px > b.Max.X-1 {
			px = b.Max.X - 1
		}
		if py > b.Max.Y-1 {
			py = b.Max.Y - 1
		}
		r16, g16, b16, a16 := src.At(px, py).RGBA()
		return r16, g16, b16, a16
	}
	r00, g00, b00, a00 := c(x0, y0)
	r10, g10, b10, a10 := c(x0+1, y0)
	r01, g01, b01, a01 := c(x0, y0+1)
	r11, g11, b11, a11 := c(x0+1, y0+1)
	mix := func(v00, v10, v01, v11 uint32) uint8 {
		top := float64(v00)*(1-fx) + float64(v10)*fx
		bot := float64(v01)*(1-fx) + float64(v11)*fx
		return uint8(math.Round((top*(1-fy) + bot*fy) / 257))
	}
	return color.RGBA{
		R: mix(r00, r10, r01, r11), G: mix(g00, g10, g01, g11),
		B: mix(b00, b10, b01, b11), A: mix(a00, a10, a01, a11),
	}
}

// EncodePNG 把图像写成 PNG（0600）。
func EncodePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	return enc.Encode(f, img)
}
