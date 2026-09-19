package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/appproxy"
	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  语音转文字服务的接口契约测试（OpenAI 兼容 + /healthz + /v1/models）
//
//  纪律：**不碰真实服务、真实模型、真机家目录**（AGENTS 第三节）。
//    · 引擎的命令执行器整个被替换（Run），绝不真的 spawn whisper-cli / ffmpeg；
//    · 模型目录是 t.TempDir()，模型文件是**同尺寸稀疏文件**（判据是"大小必须与
//      上游一致"，所以必须造出正确的大小，否则测试前提就不成立）；
//    · 只走 httptest，不监听端口。
//
//  锁的是**契约**：状态码、响应头、JSON 字段名，以及"错误如实"
//  （档位没下 → 409、格式拼错 → 400、音频解不开 → 415、超时长 → 413、
//  引擎不可用 → 503 —— 而不是笼统的 500，也不是谎报的 200）。
// ============================================================================

// sttFakeBinaries 造一组"存在且可执行"的假引擎文件。
func sttFakeBinaries(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	for _, name := range []string{services.STTCLIBinName, services.STTServerBinName, "ffmpeg", "ffprobe"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// sttSparseModel 在 modelsDir 里造一个"大小正确 + ggml 魔数正确"的模型文件。
//
// 用稀疏文件（Seek 到末尾写 1 字节）：os.Stat().Size() 会如实报出上游那个
// 字节数，而磁盘占用接近 0 —— 否则每个测试都要写 466 MB。
func sttSparseModel(t *testing.T, modelsDir, id string) {
	t.Helper()
	m, err := services.FindSTTModel(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(modelsDir, m.File)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write([]byte("lmgg")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(m.Bytes-1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
}

// sttFakeEngineOptions 控制假引擎的行为。
type sttFakeEngineOptions struct {
	// Duration 是 ffprobe 报出来的音频时长（决定同步还是任务路径）。
	Duration float64
	// TranscribeText 是 whisper-cli 的 JSON 里那一段文本。
	TranscribeText string
	// Language 是 whisper 识别到的语言。
	Language string
	// CLIFails 为真时 whisper-cli 直接失败（模拟权重损坏 / 引擎跑不起来）。
	CLIFails bool
	// ProbeFails 为真时 ffprobe 报错（模拟"这个文件不是音频"）。
	ProbeFails bool
	// ProgressPcts 是 whisper-cli 打到 stderr 的进度百分比序列（真的进度行）。
	ProgressPcts []int
}

// newSTTFakeEngine 造一个"完全离线"的 STTEngine。
//
// 它把四个外部命令的行为都替换掉了，但**没有绕过任何业务代码**：
// HTTP 层 → Transcribe → 探时长 → 转码 → 切片 → transcribeChunk → 解析 JSON
// 这条链是真的在跑，只是末端不 spawn 进程。
func newSTTFakeEngine(t *testing.T, modelsDir string, opt sttFakeEngineOptions) *services.STTEngine {
	t.Helper()
	bins := sttFakeBinaries(t)
	e := services.NewSTTEngine(bins, modelsDir, services.DefaultSTTModelID())
	e.CLIBin = filepath.Join(bins, services.STTCLIBinName)
	e.ServerBin = filepath.Join(bins, services.STTServerBinName)
	e.FfmpegBin = filepath.Join(bins, "ffmpeg")
	e.FfprobeBin = filepath.Join(bins, "ffprobe")
	e.TempRoot = t.TempDir()
	e.Now = time.Now

	if opt.Duration <= 0 {
		opt.Duration = 3.5
	}
	if opt.TranscribeText == "" {
		opt.TranscribeText = "今天天气不错,测试语音转文字。"
	}
	if opt.Language == "" {
		opt.Language = "zh"
	}
	if len(opt.ProgressPcts) == 0 {
		opt.ProgressPcts = []int{25, 60, 100}
	}

	e.Run = func(ctx context.Context, timeout time.Duration, bin string, args []string,
		onStderrLine func(string)) (string, error) {

		switch filepath.Base(bin) {
		case "ffprobe":
			if opt.ProbeFails {
				return "", fmt.Errorf("ffprobe 退出码 1：Invalid data found when processing input")
			}
			return fmt.Sprintf(`{"streams":[{"sample_rate":"16000","channels":1,"codec_name":"pcm_s16le"}],`+
				`"format":{"duration":"%.3f","format_name":"wav"}}`, opt.Duration), nil

		case "ffmpeg":
			// 转码/切片的产物路径永远是最后一个参数。
			out := args[len(args)-1]
			// segment muxer 的产物是 chunk-%04d.wav 模式：造一个真分片。
			if strings.Contains(out, "%04d") {
				out = strings.Replace(out, "%04d", "0000", 1)
			}
			if err := os.WriteFile(out, []byte("RIFF....WAVEfmt "), 0o644); err != nil {
				return "", err
			}
			return "", nil

		case services.STTCLIBinName:
			if opt.CLIFails {
				return "whisper_init_from_file_with_params_no_state: failed to load model",
					fmt.Errorf("whisper-cli 退出码 1")
			}
			// 真机上 whisper-cli 会把进度打到 stderr（逐行回调就是靠这个）。
			for _, p := range opt.ProgressPcts {
				if onStderrLine != nil {
					onStderrLine(fmt.Sprintf("whisper_print_progress_callback: progress = %3d%%", p))
				}
			}
			// 找到 -of <prefix>，写 <prefix>.json（与真机行为一致）。
			prefix := ""
			for i, a := range args {
				if a == "-of" && i+1 < len(args) {
					prefix = args[i+1]
				}
			}
			if prefix == "" {
				return "", fmt.Errorf("测试前提不成立：whisper-cli 没有被传 -of")
			}
			body := fmt.Sprintf(`{"result":{"language":%q},"transcription":[
				{"timestamps":{"from":"00:00:00,000","to":"00:00:02,000"},
				 "offsets":{"from":0,"to":2000},"text":" %s "}]}`,
				opt.Language, opt.TranscribeText)
			if err := os.WriteFile(prefix+".json", []byte(body), 0o644); err != nil {
				return "", err
			}
			if onStderrLine != nil {
				onStderrLine("[00:00:00.000 --> 00:00:02.000]  " + opt.TranscribeText)
			}
			return opt.TranscribeText + "\n", nil
		}
		return "", fmt.Errorf("测试里出现了没预期的命令：%s", bin)
	}
	return e
}

// newSTTTestServer 造一个只依赖注入引擎的 STTServer（不监听端口）。
func newSTTTestServer(t *testing.T, eng *services.STTEngine, root string, syncMax int) *STTServer {
	t.Helper()
	return NewSTTServer(STTOptions{
		Listen:         "127.0.0.1:0",
		Root:           root,
		Model:          services.DefaultSTTModelID(),
		Engine:         eng,
		SyncMaxSeconds: syncMax,
		TempRoot:       t.TempDir(),
		JobTTL:         time.Minute,
	})
}

// sttMultipartBody 造一个带音频文件的 multipart 请求体。
func sttMultipartBody(t *testing.T, fields map[string]string, fileName string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if fileName != "" {
		fw, err := w.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// ---------------------------------------------------------------------------
//  ① OpenAI 兼容契约：成功路径的字段
// ---------------------------------------------------------------------------

// TestSTTTranscriptionsSyncContract 锁住短音频的同步返回（200 + 各格式正文）。
func TestSTTTranscriptionsSyncContract(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	cases := []struct {
		format      string
		wantCtype   string
		wantContain []string
	}{
		{"text", "text/plain; charset=utf-8", []string{"今天天气不错"}},
		{"json", "application/json; charset=utf-8", []string{`"text"`, "今天天气不错"}},
		{"verbose_json", "application/json; charset=utf-8", []string{`"segments"`, `"language":"zh"`, `"model":"small"`}},
		{"srt", "application/x-subrip; charset=utf-8", []string{"1\n00:00:00,000 --> 00:00:02,000"}},
		{"vtt", "text/vtt; charset=utf-8", []string{"WEBVTT", "00:00:00.000 --> 00:00:02.000"}},
		{"", "application/json; charset=utf-8", []string{`"text"`}}, // 空 = OpenAI 默认 json
	}
	for _, c := range cases {
		body, ctype := sttMultipartBody(t, map[string]string{
			"model": "small", "language": "中文", "response_format": c.format,
		}, "test.wav", []byte("RIFF....WAVEfmt "))
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("response_format=%q：期望 200，实际 %d（%s）", c.format, rec.Code, rec.Body.String())
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != c.wantCtype {
			t.Errorf("response_format=%q：Content-Type 期望 %q，实际 %q", c.format, c.wantCtype, got)
		}
		for _, want := range c.wantContain {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("response_format=%q：正文里缺少 %q\n%s", c.format, want, rec.Body.String())
			}
		}
		// X-Zizpanel-* 头是"实际用了什么"的证据（不是装饰）。
		if rec.Header().Get("X-Zizpanel-Model") != "small" {
			t.Errorf("缺少 X-Zizpanel-Model 头：%q", rec.Header().Get("X-Zizpanel-Model"))
		}
		if rec.Header().Get("X-Zizpanel-Language") != "zh" {
			t.Errorf("X-Zizpanel-Language 应为 zh，实际 %q", rec.Header().Get("X-Zizpanel-Language"))
		}
		if rec.Header().Get("X-Zizpanel-Chunks") != "1" {
			t.Errorf("X-Zizpanel-Chunks 应为 1（3.5 秒音频只切 1 片），实际 %q", rec.Header().Get("X-Zizpanel-Chunks"))
		}
		if rec.Header().Get("X-Zizpanel-Segments") != "1" {
			t.Errorf("X-Zizpanel-Segments 应为 1，实际 %q", rec.Header().Get("X-Zizpanel-Segments"))
		}
	}
}

// TestSTTTranscriptionsLanguageIsNormalized：界面写"中文"、curl 写"zh-CN"
// 都要被归一成 whisper 语言码（否则 whisper 会用自己的默认 en 去识别中文）。
func TestSTTTranscriptionsLanguageIsNormalized(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")

	var seenLang string
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	inner := eng.Run
	eng.Run = func(ctx context.Context, timeout time.Duration, bin string, args []string,
		onStderrLine func(string)) (string, error) {
		if filepath.Base(bin) == services.STTCLIBinName {
			for i, a := range args {
				if a == "-l" && i+1 < len(args) {
					seenLang = args[i+1]
				}
			}
		}
		return inner(ctx, timeout, bin, args, onStderrLine)
	}
	srv := newSTTTestServer(t, eng, root, 60)

	body, ctype := sttMultipartBody(t, map[string]string{
		"model": "small", "language": "中文", "response_format": "text",
	}, "t.wav", []byte("RIFF....WAVEfmt "))
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if seenLang != "zh" {
		t.Errorf("界面传的『中文』应被归一成 zh 传给 whisper，实际传入 %q", seenLang)
	}
}

// ---------------------------------------------------------------------------
//  ② 错误必须如实（状态码 + 原因），不许 200、不许笼统 500
// ---------------------------------------------------------------------------

// TestSTTTranscriptionsErrorCodes 是"错误如实"的核心断言。
func TestSTTTranscriptionsErrorCodes(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	post := func(fields map[string]string, fileName string, content []byte) *httptest.ResponseRecorder {
		body, ctype := sttMultipartBody(t, fields, fileName, content)
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// ① 缺少 file 字段 → 400，且说清怎么传。
	rec := post(map[string]string{"model": "small"}, "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 file 字段应给 400，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "file") {
		t.Errorf("错误信息要点名缺的是 file 字段：%s", rec.Body.String())
	}

	// ② 未知档位 → 400（请求方的问题，要能改）。
	rec = post(map[string]string{"model": "whisper-9"}, "t.wav", []byte("x"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知档位应给 400，实际 %d（%s）", rec.Code, rec.Body.String())
	}

	// ③ 档位存在但没下载 → 409（环境缺件，但请求没错），且要说清怎么办。
	rec = post(map[string]string{"model": "medium"}, "t.wav", []byte("x"))
	if rec.Code != http.StatusConflict {
		t.Errorf("档位没下载应给 409，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "download") || !strings.Contains(rec.Body.String(), "/v1/models") {
		t.Errorf("409 必须告诉用户怎么补（下载入口 / /v1/models）：%s", rec.Body.String())
	}

	// ④ response_format 拼错 → 400，且列出支持项。
	rec = post(map[string]string{"model": "small", "response_format": "docx"}, "t.wav", []byte("x"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知 response_format 应给 400，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "srt") {
		t.Errorf("错误里应列出支持的格式：%s", rec.Body.String())
	}

	// ⑤ 不是 multipart → 400。
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("非 multipart 请求应给 400，实际 %d", rec2.Code)
	}

	// ⑥ 空文件 → 400（不是 200 + 空文本）。
	rec = post(map[string]string{"model": "small"}, "empty.wav", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空音频应给 400，实际 %d（%s）", rec.Code, rec.Body.String())
	}

	// ⑦ 音频解不开（ffprobe 失败）→ 415（媒体类型问题，不是笼统 500）。
	probeFailEng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{ProbeFails: true})
	probeFailSrv := newSTTTestServer(t, probeFailEng, root, 60)
	body, ctype := sttMultipartBody(t, map[string]string{"model": "small"}, "t.wav", []byte("garbage"))
	req = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ctype)
	rec = httptest.NewRecorder()
	probeFailSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("音频解不开应给 415，实际 %d（%s）", rec.Code, rec.Body.String())
	}
}

// TestSTTTranscriptionsTooLongIs413：超时长必须**在开任务之前**给 413，
// 而不是让用户对着一个必然失败的任务等（也不许静默截断）。
func TestSTTTranscriptionsTooLongIs413(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")
	// 时长超过 services.STTMaxAudioSeconds（3 小时）。
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{
		Duration: float64(services.STTMaxAudioSeconds) + 60,
	})
	srv := newSTTTestServer(t, eng, root, 60)

	body, ctype := sttMultipartBody(t, map[string]string{"model": "small"}, "long.wav", []byte("RIFF...."))
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超时长应给 413，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "上限") {
		t.Errorf("413 要说清上限与原因：%s", rec.Body.String())
	}
	// 不许把注定失败的任务开出来。
	srv.mu.Lock()
	n := len(srv.jobs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("超时长的请求不该创建任务，实际建了 %d 个", n)
	}
}

// ---------------------------------------------------------------------------
//  ③ /healthz：必须真的探活，缺件一律不健康
// ---------------------------------------------------------------------------

// TestSTTHealthzUnhealthyWithoutModel：模型不在 → 503 + ok:false + reason。
func TestSTTHealthzUnhealthyWithoutModel(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName) // 空目录
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("模型缺失时 /healthz 必须是 503，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	var h services.STTHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("健康响应不是合法 JSON：%v", err)
	}
	if h.OK {
		t.Error("ok 必须是 false（端口在听/进程活着都不是能力证据）")
	}
	if h.Reason == "" {
		t.Error("必须给出 reason（否则用户不知道该补什么）")
	}
	if h.ModelPresent {
		t.Error("model_present 必须为 false")
	}
	if h.Service != services.STTAppID || h.MarketAppID != services.STTAppID {
		t.Errorf("自我标识不对：service=%q market_app_id=%q", h.Service, h.MarketAppID)
	}
	if len(h.Models) != len(services.STTModels) {
		t.Errorf("健康快照应带上全部档位现状，实际 %d 条", len(h.Models))
	}
}

// TestSTTHealthzHealthyOnlyWhenProbeReallyRuns：健康**必须**来自"真的跑了一次
// 极短音频转写"，而不是"文件都在"。这里两侧都验：
//   - 三件套 + 模型都在但探针失败 → 仍然不健康；
//   - 探针成功 → 200 + ok:true + probe_ok:true。
func TestSTTHealthzHealthyOnlyWhenProbeReallyRuns(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "large-v3-turbo")

	// ① 探针会失败（whisper-cli 跑不起来）。
	brokenEng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{CLIFails: true})
	brokenSrv := newSTTTestServer(t, brokenEng, root, 60)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	brokenSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("探针失败时 /healthz 必须是 503（模型在 ≠ 能转写），实际 %d（%s）",
			rec.Code, rec.Body.String())
	}
	var bad services.STTHealth
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	if bad.ModelPresent != true || bad.CLIPresent != true {
		t.Errorf("模型与引擎文件都在，必须如实报 present=true：%+v", bad)
	}
	if bad.OK {
		t.Error("OK 必须是 false")
	}
	if !strings.Contains(bad.Reason, "探针") {
		t.Errorf("reason 要说清是探针（真的转写）失败：%q", bad.Reason)
	}

	// ② 探针能过 → 200 + probe_ok。
	goodEng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	goodSrv := newSTTTestServer(t, goodEng, root, 60)
	rec = httptest.NewRecorder()
	goodSrv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("一切正常时应为 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	var good services.STTHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &good); err != nil {
		t.Fatal(err)
	}
	if !good.OK || !good.ProbeOK || !good.ModelPresent || !good.FfmpegPresent || !good.FfprobePresent {
		t.Errorf("健康快照字段不对：%+v", good)
	}
	// 第一次探针是刚跑的，不能标成缓存。
	if good.ProbeCached {
		t.Error("首次探针不该标成 probe_cached")
	}
	if good.ProbeNote == "" {
		t.Error("probe_note 要写清探针是怎么跑的（哪一档、多少秒、多少分段）")
	}
	// 服务包里还有官方的 whisper-server，要如实报出来。
	if !good.ServerBinPresent {
		t.Error("同一个包里的 whisper-server 存在时应当如实报 server_bin_present=true")
	}
}

// ---------------------------------------------------------------------------
//  ④ /v1/models
// ---------------------------------------------------------------------------

// TestSTTModelsListContract：装了哪些档、当前档是谁，逐字段锁住。
func TestSTTModelsListContract(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")
	sttSparseModel(t, modelsDir, "large-v3-turbo")
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID        string `json:"id"`
			Object    string `json:"object"`
			File      string `json:"file"`
			Bytes     int64  `json:"bytes"`
			SizeHuman string `json:"size_human"`
			Installed bool   `json:"installed"`
			Current   bool   `json:"current"`
			Missing   bool   `json:"missing"`
			RAMMB     int    `json:"ram_mb"`
			RAMNote   string `json:"ram_note"`
			Note      string `json:"note"`
		} `json:"data"`
		Current         string `json:"current"`
		Ready           int    `json:"ready"`
		Total           int    `json:"total"`
		SyncMaxSeconds  int    `json:"sync_max_seconds"`
		MaxAudioSeconds int    `json:"max_audio_seconds"`
		ChunkSeconds    int    `json:"chunk_seconds"`
		Service         string `json:"service"`
		MarketAppID     string `json:"market_app_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, rec.Body.String())
	}
	if body.Object != "list" {
		t.Errorf("object 应为 list（OpenAI 兼容），实际 %q", body.Object)
	}
	if len(body.Data) != len(services.STTModels) {
		t.Fatalf("应列出全部 %d 档，实际 %d", len(services.STTModels), len(body.Data))
	}
	if body.Ready != 2 || body.Current != "large-v3-turbo" {
		t.Errorf("ready/current 不对：ready=%d current=%q", body.Ready, body.Current)
	}
	byID := map[string]int{}
	for i, m := range body.Data {
		byID[m.ID] = i
		if m.Object != "model" {
			t.Errorf("%s：object 应为 model", m.ID)
		}
		if m.Bytes <= 0 || m.SizeHuman == "" {
			t.Errorf("%s：必须给出体积（用户要求显示磁盘占用），得到 %d / %q", m.ID, m.Bytes, m.SizeHuman)
		}
		// 用户要求"显示各自磁盘占用与内存建议"—— 内存要么给数字、要么给说明。
		if m.RAMMB == 0 && m.RAMNote == "" {
			t.Errorf("%s：必须给内存建议（ram_mb 或 ram_note）", m.ID)
		}
		if m.Note == "" {
			t.Errorf("%s：必须有取舍说明", m.ID)
		}
		if m.Current != (m.ID == "large-v3-turbo") {
			t.Errorf("%s：current 标记不对", m.ID)
		}
	}
	if !body.Data[byID["small"]].Installed || body.Data[byID["small"]].Missing {
		t.Error("small 已下载：installed=true / missing=false")
	}
	if body.Data[byID["medium"]].Installed || !body.Data[byID["medium"]].Missing {
		t.Error("medium 未下载：installed=false / missing=true")
	}
	if body.SyncMaxSeconds <= 0 || body.ChunkSeconds <= 0 || body.MaxAudioSeconds <= 0 {
		t.Errorf("必须给出同步上限/切片长度/总上限：%+v", body)
	}
	if body.Service != services.STTAppID || body.MarketAppID != services.STTAppID {
		t.Errorf("自我标识不对：%+v", body)
	}
}

// TestSTTModelDeleteRequiresConfirm：删模型不可逆，必须显式 confirm；
// 删掉当前档要能正确回落；一档不剩时 /healthz 必须如实不健康。
//
// 三个档位都造出来，是为了让"删当前档后回落到另一个已装档"这条真的被走到 ——
// 只造两个档时那条路径会被"没有别的已装档"掩盖过去。
func TestSTTModelDeleteRequiresConfirm(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	for _, id := range []string{"small", "large-v3-turbo", "medium"} {
		sttSparseModel(t, modelsDir, id)
	}
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	del := func(id, body string) (*httptest.ResponseRecorder, struct {
		OK         bool   `json:"ok"`
		Deleted    string `json:"deleted"`
		FreedBytes int64  `json:"freed_bytes"`
		Current    string `json:"current"`
	}) {
		var out struct {
			OK         bool   `json:"ok"`
			Deleted    string `json:"deleted"`
			FreedBytes int64  `json:"freed_bytes"`
			Current    string `json:"current"`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/models/"+id+"/delete", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}

	// ① 没有 confirm → 400，且文件**必须**还在（不可逆操作不许被一个手滑请求删掉）。
	rec, _ := del("medium", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没有 confirm 时应给 400，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !services.STTModelFileExists(modelsDir, "medium") {
		t.Fatal("没有 confirm 时**绝不能**删掉模型文件")
	}

	// ② 带 confirm → 200，文件真没了，释放字节数如实回报，当前档（默认 large-v3-turbo）不变。
	rec, body := del("medium", `{"confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("带 confirm 时应给 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	want, _ := services.FindSTTModel("medium")
	if !body.OK || body.Deleted != "medium" || body.FreedBytes != want.Bytes {
		t.Errorf("删除结果不对：%+v（期望释放 %d 字节）", body, want.Bytes)
	}
	if services.STTModelFileExists(modelsDir, "medium") {
		t.Error("确认后 medium 的权重应当被删除")
	}
	if body.Current != "large-v3-turbo" {
		t.Errorf("删非当前档时当前档不该变，实际 %q", body.Current)
	}

	// ③ 把当前档切成 large-v3-turbo，再删它 → 必须回落到仍然装着的 small。
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/models/large-v3-turbo/select", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("切换当前档失败：%d（%s）", rec.Code, rec.Body.String())
	}
	rec, body = del("large-v3-turbo", `{"confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除当前档应给 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if body.Current != "small" {
		t.Errorf("删掉当前档 large-v3-turbo 后应回落到仍然装着的 small，实际 %q", body.Current)
	}
	if !services.STTModelFileExists(modelsDir, "small") {
		t.Error("删 large-v3-turbo 不该动到 small 的权重")
	}

	// ④ 一档都不剩 → 当前档仍必须是一个**合法档位名**（服务不能因此起不来），
	//    而 /healthz 必须如实报不健康（不是绿灯）。
	rec, body = del("small", `{"confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除最后一档应给 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if _, err := services.FindSTTModel(body.Current); err != nil {
		t.Errorf("一档不剩时当前档仍必须是合法档位名，实际 %q", body.Current)
	}
	hrec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(hrec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if hrec.Code != http.StatusServiceUnavailable {
		t.Errorf("一档都没装时 /healthz 必须是 503，实际 %d", hrec.Code)
	}

	// ⑤ 删一个已经不存在的档位 → 404（说清"无需删除"），不假装成功。
	rec, _ = del("large-v3-turbo", `{"confirm":true}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("删除未下载的档位应给 404，实际 %d（%s）", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
//  ⑤ 长音频：202 + task_id + 真实进度（SSE）
// ---------------------------------------------------------------------------

// TestSTTLongAudioGoesToTaskWithRealProgress 是"长音频要有真实进度"的端到端断言。
//
// 判据：
//   - 超过同步上限 → 202 + task_id（不是同步等待）；
//   - 进度日志里出现**真的**分片计数（已完成 N/M 片），而不是假转圈；
//   - 结果接口按当初请求的 response_format 渲染。
func TestSTTLongAudioGoesToTaskWithRealProgress(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")
	// 3 分钟音频 = 4 片（每片 60 秒）。
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{Duration: 200})
	srv := newSTTTestServer(t, eng, root, 60)

	body, ctype := sttMultipartBody(t, map[string]string{
		"model": "small", "language": "zh", "response_format": "srt",
	}, "long.wav", []byte("RIFF....WAVEfmt "))
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("长音频应给 202，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	var accepted struct {
		OK   bool `json:"ok"`
		Data struct {
			TaskID      string  `json:"task_id"`
			Sync        bool    `json:"sync"`
			DurationSec float64 `json:"duration_seconds"`
			Chunks      int     `json:"chunks"`
			StreamURL   string  `json:"stream_url"`
			ResultURL   string  `json:"result_url"`
			RespFormat  string  `json:"response_format"`
			SyncMaxSec  int     `json:"sync_max_seconds"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("202 响应不是合法 JSON：%v（%s）", err, rec.Body.String())
	}
	if !accepted.OK || accepted.Data.TaskID == "" {
		t.Fatalf("202 必须带 task_id：%s", rec.Body.String())
	}
	if accepted.Data.Sync {
		t.Error("长音频的 sync 必须是 false")
	}
	if accepted.Data.Chunks < 2 {
		t.Errorf("200 秒音频按 60 秒切片应至少 2 片，实际 %d", accepted.Data.Chunks)
	}
	if accepted.Data.RespFormat != "srt" {
		t.Errorf("202 应回报请求的 response_format，实际 %q", accepted.Data.RespFormat)
	}
	taskID := accepted.Data.TaskID

	// 等任务结束（任务体是我们注入的假引擎，毫秒级）。
	deadline := time.Now().Add(15 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		r2 := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID, nil)
		w2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w2, r2)
		var got struct {
			Data struct {
				Task struct {
					Status string `json:"status"`
				} `json:"task"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w2.Body.Bytes(), &got)
		status = got.Data.Task.Status
		if status != "" && status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "succeeded" && status != "success" && status != "done" {
		// 不同版本用的字面量可能不同；直接看结果接口更稳。
		t.Logf("任务状态字面量：%q（继续核对结果接口）", status)
	}

	// 结果接口必须按当初请求的格式（srt）返回正文。
	r3 := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/result", nil)
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, r3)
	if w3.Code != http.StatusOK {
		t.Fatalf("任务结果接口应给 200，实际 %d（%s）", w3.Code, w3.Body.String())
	}
	if !strings.Contains(w3.Body.String(), "-->") {
		t.Errorf("按 srt 请求的结果应是字幕（含 -->），实际：%s", w3.Body.String())
	}
	// SSE 进度流里必须有**真的**分片计数（已完成 N/M 片）。
	r4 := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/stream", nil)
	w4 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w4, r4)
	if w4.Code != http.StatusOK {
		t.Fatalf("SSE 流应给 200，实际 %d", w4.Code)
	}
	if ct := w4.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("SSE 的 Content-Type 不对：%q", ct)
	}
	stream := w4.Body.String()
	if !strings.Contains(stream, "已完成") {
		t.Errorf("进度流里必须有真实的『已完成 N/M 片』：%s", stream)
	}
	// meta / lines / done 三种事件都要有（与面板任务中心同一套协议）。
	for _, ev := range []string{"event: meta", "event: lines", "event: done"} {
		if !strings.Contains(stream, ev) {
			t.Errorf("SSE 缺少 %q 事件：%s", ev, stream)
		}
	}
}

// TestSTTModelDownloadGoesToTask：下载也是长任务（202 + task_id），
// 不许同步等待（那会让用户关掉窗口就找不回进度）。
func TestSTTModelDownloadGoesToTask(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	// 已下载的档位：直接返回 already，不该建任务。
	req := httptest.NewRequest(http.MethodPost, "/v1/models/small/download", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "already") {
		t.Errorf("已下载的档位应给 200 + already=true，实际 %d（%s）", rec.Code, rec.Body.String())
	}

	// 未知档位 → 400。
	req = httptest.NewRequest(http.MethodPost, "/v1/models/nope/download", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知档位下载应给 400，实际 %d", rec.Code)
	}

	// 未下载的档位 → 202 + task_id（这里**不**等它真的下完：那要联网，
	// 单测不许联网。只断言"转成了任务"这个契约）。
	req = httptest.NewRequest(http.MethodPost, "/v1/models/medium/download", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("未下载档位应给 202，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "task_id") {
		t.Errorf("202 必须带 task_id：%s", rec.Body.String())
	}
}

// TestSTTSelectModelRequiresInstalled：不能把当前档切成没下载的档。
func TestSTTSelectModelRequiresInstalled(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "small")
	sttSparseModel(t, modelsDir, "large-v3-turbo")
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	// 已下载 → 200，且真的写进了 current_model（独立进程靠这个文件知道当前档）。
	req := httptest.NewRequest(http.MethodPost, "/v1/models/large-v3-turbo/select", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("切换已装档位应给 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(filepath.Join(root, services.STTCurrentModelFile))
	if err != nil {
		t.Fatalf("必须把当前档写进 current_model（stt-serve 是独立进程）：%v", err)
	}
	if strings.TrimSpace(string(b)) != "large-v3-turbo" {
		t.Errorf("current_model 内容不对：%q", strings.TrimSpace(string(b)))
	}

	// 未下载 → 409，且当前档不变。
	req = httptest.NewRequest(http.MethodPost, "/v1/models/medium/select", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("切换未下载档位应给 409，实际 %d（%s）", rec.Code, rec.Body.String())
	}
}

// TestSTTRoutesAreRegistered：路由表齐全（少一条就是"界面/调用方 404"）。
func TestSTTRoutesAreRegistered(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "large-v3-turbo")
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)
	h := srv.Handler()

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/app.js", http.StatusOK},
		{http.MethodGet, "/app.css", http.StatusOK},
		{http.MethodGet, "/favicon.ico", http.StatusNoContent},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/v1/models", http.StatusOK},
		{http.MethodGet, "/v1/tasks/does-not-exist", http.StatusNotFound},
		{http.MethodGet, "/v1/tasks/x/stream", http.StatusNotFound},
		{http.MethodGet, "/v1/tasks/x/result", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s：期望 %d，实际 %d", c.method, c.path, c.want, rec.Code)
		}
	}

	// 界面必须是相对路径引用资源（否则经 /stt/ 别名打开时白屏）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	html := rec.Body.String()
	if !strings.Contains(html, `href="./app.css"`) || !strings.Contains(html, `src="./app.js"`) {
		t.Error("index.html 必须用相对路径引用 ./app.css 与 ./app.js（别名下绝对路径会打到站点根）")
	}
	// app.js 里的接口调用也必须用相对路径。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	js := rec.Body.String()
	for _, bad := range []string{"fetch('/v1/", "fetch(\"/v1/", "EventSource('/v1/"} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js 里出现了绝对路径调用 %q（经别名打开会 404）", bad)
		}
	}
	// 界面必须真的调了 OpenAI 契约端点。
	if !strings.Contains(js, "v1/audio/transcriptions") {
		t.Error("app.js 必须调用 /v1/audio/transcriptions")
	}
}

// ---------------------------------------------------------------------------
//  ⑥ 端口 + 别名：走面板既有应用界面代理，两条路都能用
// ---------------------------------------------------------------------------

// TestSTTAliasPathWorksThroughAppProxy 是"别名入口"的**真实**端到端断言。
//
// 面板的 /stt/ 不是单独写的一套转发，而是复用 internal/appproxy ——
// 所以这里就用**同一个 Handler** 把请求接到真实的 STTServer 上，逐条验证：
//
//	· GET  /stt/healthz            → 前缀被剥掉、上游收到 /healthz；
//	· GET  /stt/                   → 界面能打开；
//	· GET  /stt/app.js             → 相对路径资源在别名下也取得到（否则白屏）；
//	· POST /stt/v1/audio/transcriptions（multipart）→ 请求体真的流过去了，
//	  且返回的是同一份 OpenAI 契约。
//
// 上游用的是**本测试自己的 httptest 服务器**（不是真机端口），所以这条测试
// 不依赖任何外部进程 —— 但走的确实是生产那条代理代码。
func TestSTTAliasPathWorksThroughAppProxy(t *testing.T) {
	root := t.TempDir()
	modelsDir := filepath.Join(root, services.STTModelDirName)
	sttSparseModel(t, modelsDir, "large-v3-turbo")
	eng := newSTTFakeEngine(t, modelsDir, sttFakeEngineOptions{})
	srv := newSTTTestServer(t, eng, root, 60)

	// 上游 = 真实的 STTServer。
	upstream := httptest.NewServer(srv.Handler())
	defer upstream.Close()

	// 目录条目必须是"有界面 + 有端口"的，否则 appproxy.Slugs() 根本不会带上它。
	app, ok := services.FindApp(services.STTAppID)
	if !ok {
		t.Fatal("目录里没有 stt")
	}
	if app.UI == nil || app.UI.Slug != services.STTSlug {
		t.Fatalf("目录条目必须声明 UI.Slug=%s（否则不会有 /stt/ 入口）", services.STTSlug)
	}
	if app.UI.SelfConf {
		t.Error("stt 的界面由面板自己的二进制提供，不该标 SelfConf")
	}

	// appproxy.Slugs() 必须包含它（registerAppProxy 就是遍历这个列表挂路由的）。
	found := false
	for _, a := range appproxy.Slugs() {
		if a.ID == services.STTAppID {
			found = true
			if a.UI.Slug != services.STTSlug {
				t.Errorf("Slugs() 里的 slug 不对：%q", a.UI.Slug)
			}
			if a.WebPort() != services.STTPort {
				t.Errorf("Slugs() 里的端口不对：%d（期望 %d）", a.WebPort(), services.STTPort)
			}
		}
	}
	if !found {
		t.Fatal("appproxy.Slugs() 里没有 stt —— 面板不会给它挂 /stt/ 别名")
	}

	// 用**同一个** appproxy Handler，只把上游端口换成 httptest 的端口。
	upURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := strings.Cut(upURL.Host, ":")
	appForProxy := app
	appForProxy.Port = atoiOr(t, portStr)
	if appForProxy.UIPort != 0 {
		appForProxy.UIPort = appForProxy.Port
	}

	mux := http.NewServeMux()
	h := appproxy.Handler(appForProxy)
	mux.Handle("/"+services.STTSlug+"/", h)
	alias := httptest.NewServer(mux)
	defer alias.Close()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// ① 健康检查经别名可达（前缀被剥掉）。
	if rec := get("/stt/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /stt/healthz 期望 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	// ② 界面本体。
	if rec := get("/stt/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "语音转文字") {
		t.Errorf("GET /stt/ 期望 200 + 界面 HTML，实际 %d", rec.Code)
	}
	// ③ 相对路径资源：这一条是"别名下会不会白屏"的判据。
	for _, asset := range []string{"/stt/app.js", "/stt/app.css"} {
		if rec := get(asset); rec.Code != http.StatusOK {
			t.Errorf("GET %s 期望 200（别名下相对资源必须取得到），实际 %d", asset, rec.Code)
		}
	}
	// ④ 档位清单。
	if rec := get("/stt/v1/models"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "small") {
		t.Errorf("GET /stt/v1/models 期望 200 + small，实际 %d", rec.Code)
	}

	// ⑤ POST multipart 经别名：证明请求体真的流过去了（大上传就是这条路）。
	body, ctype := sttMultipartBody(t, map[string]string{
		"model": "large-v3-turbo", "language": "zh", "response_format": "text",
	}, "t.wav", []byte("RIFF....WAVEfmt "))
	req := httptest.NewRequest(http.MethodPost, "/stt/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /stt/v1/audio/transcriptions 期望 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "今天天气不错") {
		t.Errorf("经别名调用必须拿到同一份契约结果：%s", rec.Body.String())
	}

	// ⑥ nginx 那段 location 也必须会包含 /stt/（80 端口入口走它）。
	block := appProxyBlock(appProxyEntries(), "127.0.0.1:8443")
	// 生成的形状是 `location ^~ /stt/`（^~ 表示前缀匹配优先，见 appProxyBlock）。
	if !strings.Contains(block, "location ^~ /"+services.STTSlug+"/") {
		t.Errorf("nginx 应用代理块里没有 /%s/ —— http://<主机>/%s/ 会 404\n%s",
			services.STTSlug, services.STTSlug, block)
	}
	if !strings.Contains(block, "127.0.0.1:"+strconv.Itoa(services.STTPort)) {
		t.Errorf("nginx 应用代理块里没有 %d 端口的注释：\n%s", services.STTPort, block)
	}
}

// atoiOr 解析端口（解析不出来直接失败 —— 测试前提不成立比静默用 0 好）。
func atoiOr(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("端口 %q 解析失败：%v", s, err)
	}
	return n
}
