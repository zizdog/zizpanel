package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  macOS 语音合成 Web UI 的**独立服务**（`zizpanel speech-serve`）
//
//  两条入口（与「图片压缩」同一套做法）：
//    · 端口：这个进程只监听 127.0.0.1:<port>（默认 8891），界面与 API 都在 /；
//    · 别名：应用市场条目声明 UI.Slug=speech，面板的 internal/appproxy 把
//      http://<主机>/speech/ 反代到上面那个端口（要求先登录面板），
//      nginx 的 80 端口入口也走面板这一条（见 api_appproxy.go）。
//
//  为什么由面板二进制自己当这个服务：内嵌前端、无构建步骤、单二进制 ——
//  与仓库的既有约束一致；面板升级时界面一起升级。
//
//  引擎是系统自带的 /usr/bin/say（见 internal/services/macspeech.go），
//  所以这个服务**不需要任何外部依赖**、完全离线可用。
//
//  长文本：≤ SyncMaxChars（默认 600 字）同步返回音频（秒级）；
//  更长的走 202 + task_id，进度是**真的**（按句分段、逐段合成，SSE 报 已完成/总段数）。
//  诚实标注：这个任务中心是本进程内的，**不会**出现在面板的「任务中心」列表里
//  （那是另一个进程的 Manager）——与「图片压缩」的说明一致。
//
//  安全边界：只绑 127.0.0.1（见 macspeech_install.go 的 plist）；走别名时还要
//  过面板登录；请求体有硬上限；临时音频只存在于任务私有目录里，
//  路径不可由请求方指定（不构成任意文件读取）。
// ============================================================================

//go:embed assets/speech
var speechUIFS embed.FS

const (
	// speechUIPrefix 是内嵌资源在 embed.FS 里的前缀。
	speechUIPrefix = "assets/speech/"

	// speechDefaultSyncMaxChars：不超过它就同步返回音频。
	//
	// 实测 say 合成 184 字（约 39 秒音频）只要 0.7 秒，所以 600 字的同步请求
	// 也在 1 秒级；800 字以上开始有"用户要等"的感觉，改走任务 + 进度。
	speechDefaultSyncMaxChars = 600

	// speechMaxBodyBytes 是请求体上限（20000 字 + JSON 转义，1 MiB 足够）。
	speechMaxBodyBytes = 1 << 20

	// speechJobTTL 是任务音频的保留时长（到期惰性清理）。
	speechJobTTL = 2 * time.Hour
)

// SpeechOptions 是独立服务的配置。
type SpeechOptions struct {
	// Listen 是监听地址（默认 127.0.0.1:8891）。
	Listen string
	// Engine 覆盖引擎（测试注入；nil = services.NewSpeechEngine()）。
	Engine *services.SpeechEngine
	// SyncMaxChars <=0 用默认值。
	SyncMaxChars int
	// JobTTL <=0 用默认值。
	JobTTL time.Duration
	// TempRoot 是临时目录父目录（空 = os.TempDir()；引擎自己也有一份，以它为准）。
	TempRoot string
}

// SpeechServer 是语音合成独立服务。
type SpeechServer struct {
	opt    SpeechOptions
	engine *services.SpeechEngine
	tasks  *tasks.Manager

	mu   sync.Mutex
	jobs map[string]*speechJob
}

// speechJob 保存一个长文本任务的产物（音频文件 + 它的临时目录）。
type speechJob struct {
	id      string
	created time.Time
	result  *services.SynthResult
}

// NewSpeechServer 造一个服务实例。
func NewSpeechServer(opt SpeechOptions) *SpeechServer {
	if strings.TrimSpace(opt.Listen) == "" {
		opt.Listen = fmt.Sprintf("127.0.0.1:%d", services.MacSpeechPort)
	}
	if opt.SyncMaxChars <= 0 {
		opt.SyncMaxChars = speechDefaultSyncMaxChars
	}
	if opt.JobTTL <= 0 {
		opt.JobTTL = speechJobTTL
	}
	eng := opt.Engine
	if eng == nil {
		eng = services.NewSpeechEngine()
	}
	if strings.TrimSpace(eng.TempRoot) == "" && strings.TrimSpace(opt.TempRoot) != "" {
		eng.TempRoot = opt.TempRoot
	}
	return &SpeechServer{
		opt:    opt,
		engine: eng,
		tasks:  tasks.NewManager(),
		jobs:   map[string]*speechJob{},
	}
}

// Engine 暴露引擎给子命令（启动时打印一次真实状态）。
func (s *SpeechServer) Engine() *services.SpeechEngine { return s.engine }

// Listen 返回监听地址。
func (s *SpeechServer) Listen() string { return s.opt.Listen }

// Handler 返回这个服务的 HTTP 路由。
func (s *SpeechServer) Handler() http.Handler {
	mux := http.NewServeMux()
	// 精确根路径：直接端口访问是 /；经面板别名时 appproxy 已把 /speech 前缀剥掉。
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /app.js", s.handleAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /app.css", s.handleAsset("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/voices", s.handleVoices)
	mux.HandleFunc("POST /v1/audio/speech", s.handleSpeech)
	mux.HandleFunc("GET /v1/tasks/{id}", s.handleTaskGet)
	mux.HandleFunc("GET /v1/tasks/{id}/stream", s.handleTaskStream)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.handleTaskCancel)
	mux.HandleFunc("GET /v1/tasks/{id}/audio", s.handleTaskAudio)
	return mux
}

// ---------------------------------------------------------------------------
//  静态资源
// ---------------------------------------------------------------------------

func (s *SpeechServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := speechUIFS.ReadFile(speechUIPrefix + "index.html")
	if err != nil {
		http.Error(w, "界面资源缺失（这是面板打包问题，请反馈）", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (s *SpeechServer) handleAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := speechUIFS.ReadFile(speechUIPrefix + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	}
}

// handleFavicon 返回 204：空图标不会在日志里刷 404（页面本身不依赖它）。
func (s *SpeechServer) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
//  健康 / 音色
// ---------------------------------------------------------------------------

// handleHealthz 是**能力**健康检查，不是"进程活着"检查。
//
// 判定贴着能力本身（AGENTS 第三节）：`say -v '?'` 答不出来就返回 503 +
// ok:false + reason，这样面板的服务健康检查会如实标红 —— 绝不给一个
// "服务在跑、但每次合成都失败"的绿灯。
func (s *SpeechServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h := s.engine.Health(r.Context())
	if !h.OK {
		writeJSON(w, http.StatusServiceUnavailable, h)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// handleVoices 返回音色清单（来自 `say -v '?'`）。
//
// 形状照 OpenAI 的 list 接口（object/data），字段就是 SpeechVoice 的契约；
// 额外给 count / chinese_count，省得界面自己数。
func (s *SpeechServer) handleVoices(w http.ResponseWriter, r *http.Request) {
	voices, err := s.engine.Voices(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "engine_unavailable"},
		})
		return
	}
	chinese := 0
	for _, v := range voices {
		if v.Chinese {
			chinese++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":         "list",
		"data":           voices,
		"count":          len(voices),
		"chinese_count":  chinese,
		"default":        strings.Join(services.PreferredChineseVoiceNames(), ", "),
		"source":         "say -v '?'",
		"market_app_id":  services.MacSpeechAppID,
		"say_bin":        s.engine.SayBin,
		"sample_rate":    services.MacSpeechSampleRate,
		"max_chars":      s.engine.MaxChars,
		"chunk_chars":    s.engine.ChunkChars,
		"sync_max_chars": s.opt.SyncMaxChars,
	})
}

// ---------------------------------------------------------------------------
//  合成
// ---------------------------------------------------------------------------

// speechRequestBody 是 POST /v1/audio/speech 的请求体（OpenAI 兼容 + 面板扩展）。
type speechRequestBody struct {
	// Input 要合成的文本（OpenAI 字段名）。
	Input string `json:"input"`
	// Voice 音色名（来自 /v1/voices；空 = 按文本自动挑中文音色）。
	Voice string `json:"voice"`
	// Speed 语速倍率（1.0 = 正常，范围 0.25~4.0；会换算成 `say -r` 并夹取）。
	Speed float64 `json:"speed"`
	// Format 输出格式（aiff / wav / m4a / mp3）；面板扩展字段。
	Format string `json:"format"`
	// ResponseFormat 是 OpenAI 的字段名，与 Format 等价（Format 优先）。
	ResponseFormat string `json:"response_format"`
	// Model 只为 OpenAI SDK 兼容而接受，**不参与任何判断**（面板只有一个引擎）。
	Model string `json:"model"`
}

// handleSpeech 合成语音。
//
// 两种返回：
//   - 短文本（≤ SyncMaxChars）：200 + 音频字节本体；
//   - 长文本：202 + task_id（进度走 SSE，音频在任务完成后取）。
//
// 参数校验**在开任务之前**：空文本、未知音色、不可用格式一律当场 4xx/503，
// 不让用户对着一个注定失败的任务干等。
func (s *SpeechServer) handleSpeech(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, speechMaxBodyBytes+1))
	if err != nil {
		writeSpeechError(w, fmt.Errorf("读取请求体失败: %w", err))
		return
	}
	if len(body) > speechMaxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, apiErrorResponse(fmt.Sprintf(
			"请求体超过 %d 字节上限（文本上限 %d 字）", speechMaxBodyBytes, services.MacSpeechMaxChars)))
		return
	}
	var req speechRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiErrorResponse("请求体不是合法 JSON："+err.Error()))
		return
	}
	opt, perr := s.prepareSynth(r.Context(), req)
	if perr != nil {
		writeSpeechError(w, perr)
		return
	}

	chars := len([]rune(strings.TrimSpace(opt.Text)))
	if chars <= s.opt.SyncMaxChars {
		// 秒级路径：同步跑完直接给音频（客户端拿到的就是最终结果）。
		res, err := s.engine.Synthesize(r.Context(), opt)
		if err != nil {
			writeSpeechError(w, err)
			return
		}
		defer res.Cleanup()
		writeSpeechAudio(w, res, "speech")
		return
	}

	// 长文本路径：交给本进程的任务中心，立刻返回 202 + task_id。
	// 进度是真的 —— Synthesize 每合成完一段就回报一次 done/total。
	//
	// 用 StartWithTask 而不是 Start：任务体里要用 t.ID() 注册产物目录，
	// 而 `t := s.tasks.Start(..., func(){ 用 t })` 里的 t 在函数字面量里
	// **还没进入作用域**（编译期就过不去）—— tasks 包为此专门提供了它。
	s.cleanupExpired()
	segments := services.SplitSpeechText(opt.Text, s.engine.ChunkChars)
	t := s.tasks.StartWithTask("speech_synth", "speech",
		fmt.Sprintf("语音合成 %d 字（%d 段）", chars, len(segments)),
		func(ctx context.Context, task *tasks.Task) (any, error) {
			log := task.LogFunc()
			opt.Progress = func(done, total int, note string) {
				log(tasks.LevelStep, fmt.Sprintf("%s（%d 字，格式 %s）", note, chars, opt.Format))
			}
			log(tasks.LevelStep, fmt.Sprintf("开始合成：%d 字，音色 %s，格式 %s",
				chars, firstNonEmptyStr(opt.Voice, "（自动挑选）"), opt.Format))
			res, serr := s.engine.Synthesize(ctx, opt)
			if serr != nil {
				return nil, serr
			}
			log(tasks.LevelOK, fmt.Sprintf("合成完成：%d 段拼接，输出 %d 字节（%s）",
				res.Segments, res.Bytes, res.Format))
			s.mu.Lock()
			s.jobs[task.ID()] = &speechJob{id: task.ID(), created: time.Now(), result: res}
			s.mu.Unlock()
			return map[string]any{
				"format": string(res.Format), "bytes": res.Bytes, "chars": res.Chars,
				"segments": res.Segments, "voice": res.Voice, "lang": res.Lang,
				"rate": res.Rate, "effective_speed": res.Speed,
				"audio_url": "/v1/tasks/" + task.ID() + "/audio",
			}, nil
		})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true,
		"data": map[string]any{
			"task_id":        t.ID(),
			"sync":           false,
			"chars":          chars,
			"segments":       len(segments),
			"format":         string(opt.Format),
			"stream_url":     "/v1/tasks/" + t.ID() + "/stream",
			"audio_url":      "/v1/tasks/" + t.ID() + "/audio",
			"sync_max_chars": s.opt.SyncMaxChars,
			"note": "文本超过同步上限（" + strconv.Itoa(s.opt.SyncMaxChars) +
				" 字），已转入任务：进度见 stream_url，音频在任务完成后从 audio_url 取。",
		},
	})
}

// prepareSynth 校验请求并把它变成引擎的合成参数。
func (s *SpeechServer) prepareSynth(ctx context.Context, req speechRequestBody) (services.SynthOptions, error) {
	text := strings.TrimSpace(req.Input)
	if text == "" {
		return services.SynthOptions{}, services.ErrSpeechEmptyText
	}
	if n := len([]rune(text)); n > services.MacSpeechMaxChars {
		return services.SynthOptions{}, fmt.Errorf("%w（当前 %d 字，上限 %d 字）",
			services.ErrSpeechTextTooLong, n, services.MacSpeechMaxChars)
	}
	formatName := req.Format
	if strings.TrimSpace(formatName) == "" {
		formatName = req.ResponseFormat
	}
	format, err := services.ParseSpeechFormat(formatName)
	if err != nil {
		return services.SynthOptions{}, err
	}
	if ok, reason := s.engine.FormatAvailable(format); !ok {
		return services.SynthOptions{}, fmt.Errorf("%w：%s", services.ErrSpeechUnsupportedFormat, reason)
	}
	// 音色校验必须在这里做：`say -v 不存在的音色` 返回退出码 0 却不产出文件
	// （静默成功），不校验就会把"成功"报给用户、然后给一个 0 字节音频。
	if v := strings.TrimSpace(req.Voice); v != "" {
		voices, verr := s.engine.Voices(ctx)
		if verr != nil {
			return services.SynthOptions{}, verr
		}
		if _, ok := services.LookupSpeechVoice(voices, v); !ok {
			return services.SynthOptions{}, fmt.Errorf("%w：%q（这台机器上有 %d 个音色，见 GET /v1/voices）",
				services.ErrSpeechUnknownVoice, v, len(voices))
		}
	}
	if req.Speed < 0 || req.Speed > 4 {
		// OpenAI 的 speed 范围就是 0.25~4.0。面板把它换算成 `say -r` 的词/分钟
		// 并夹取到 80~400（超出范围时 say 的行为不可预期），**实际使用**的值
		// 通过 X-Zizpanel-Effective-Speed 头如实回给调用方。
		return services.SynthOptions{}, fmt.Errorf("%w：speed=%v（允许 0.25~4.0；0 或省略 = 默认 1.0。"+
			"面板会换算成 say 的词/分钟并夹取到 %d~%d，实际值见 X-Zizpanel-Effective-Speed）",
			services.ErrSpeechInvalidRequest,
			req.Speed, services.MacSpeechMinRate, services.MacSpeechMaxRate)
	}
	return services.SynthOptions{
		Text:   text,
		Voice:  strings.TrimSpace(req.Voice),
		Speed:  req.Speed,
		Format: format,
	}, nil
}

// writeSpeechAudio 把合成结果作为音频本体写回（并带上"实际用了什么"的头）。
//
// 这些 X-Zizpanel-* 头不是装饰：speed 会被换算并夹取，"实际用的语速/音色"
// 必须让调用方能核对，否则"我传了 speed=4 却听起来是 2.3"就无从发现。
func writeSpeechAudio(w http.ResponseWriter, res *services.SynthResult, name string) {
	f, err := os.Open(res.Path)
	if err != nil {
		writeSpeechError(w, fmt.Errorf("打开合成结果失败: %w", err))
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", res.ContentType)
	h.Set("Content-Length", strconv.FormatInt(res.Bytes, 10))
	h.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", name+"."+res.Format.Ext()))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Zizpanel-Format", string(res.Format))
	h.Set("X-Zizpanel-Voice", res.Voice)
	h.Set("X-Zizpanel-Lang", res.Lang)
	h.Set("X-Zizpanel-Rate", strconv.Itoa(res.Rate))
	h.Set("X-Zizpanel-Effective-Speed", strconv.FormatFloat(res.Speed, 'f', 3, 64))
	h.Set("X-Zizpanel-Chars", strconv.Itoa(res.Chars))
	h.Set("X-Zizpanel-Segments", strconv.Itoa(res.Segments))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// ---------------------------------------------------------------------------
//  任务
// ---------------------------------------------------------------------------

func (s *SpeechServer) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t := s.tasks.Get(id)
	if t == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "任务不存在（服务重启后不再保留历史任务）",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"data": map[string]any{
			"task":      t.Meta(),
			"done":      t.Status() != tasks.StatusRunning,
			"audio_url": "/v1/tasks/" + id + "/audio",
		},
	})
}

func (s *SpeechServer) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	t, err := s.tasks.Cancel(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": t.Meta()})
}

// handleTaskStream 是任务的 SSE 进度流，契约与面板任务中心一致
// （事件 meta / lines / done，支持 Last-Event-ID 断点续传）。
//
// 与 imgcompress 的实现同形：这里不发明第二套协议。
func (s *SpeechServer) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	flusher, okf := w.(http.Flusher)
	if !okf {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "msg": "当前服务不支持流式响应",
		})
		return
	}
	t := s.tasks.Get(r.PathValue("id"))
	if t == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "任务不存在（服务重启后不再保留历史任务）",
		})
		return
	}
	after := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			after = n
		}
	} else if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			after = n
		}
	}

	// 先订阅、再取快照（反了会丢两者之间产生的行）；重复由 cursor 去重。
	subID, ch := t.Subscribe()
	defer t.Unsubscribe(subID)
	lines, cursor, _, oldest := t.Snapshot(after, 3000)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, data any, id int64) bool {
		if !writeSSE(w, event, data, id) {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send("meta", map[string]any{"task": t.Meta(), "oldest_seq": oldest}, cursor) {
		return
	}
	if len(lines) > 0 {
		if !send("lines", map[string]any{"lines": lines}, cursor) {
			return
		}
	}
	if t.Status() != tasks.StatusRunning {
		s.flushRemaining(t, send, &cursor)
		send("done", map[string]any{"task": t.Meta(), "audio_url": "/v1/tasks/" + t.ID() + "/audio"}, cursor)
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// 心跳：防止中间的反代把空闲连接掐掉。
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case line, open := <-ch:
			if !open {
				return // 消费者跟不上，让 EventSource 带 Last-Event-ID 重连补齐
			}
			if line.Seq <= cursor {
				continue
			}
			cursor = line.Seq
			if !send("lines", map[string]any{"lines": []tasks.Line{line}}, cursor) {
				return
			}
		case <-t.Done():
			s.flushRemaining(t, send, &cursor)
			send("done", map[string]any{"task": t.Meta(), "audio_url": "/v1/tasks/" + t.ID() + "/audio"}, cursor)
			return
		}
	}
}

// flushRemaining 把任务缓冲里 cursor 之后的日志行补齐（任务刚好结束时用）。
func (s *SpeechServer) flushRemaining(t *tasks.Task, send func(string, any, int64) bool, cursor *int64) {
	rest, next, _, _ := t.Snapshot(*cursor, 4000)
	if len(rest) == 0 {
		return
	}
	*cursor = next
	send("lines", map[string]any{"lines": rest}, *cursor)
}

// handleTaskAudio 取任务产出的音频。
//
// 只在任务**成功**后可用；任务失败时如实回报失败原因（不返回空音频）。
func (s *SpeechServer) handleTaskAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t := s.tasks.Get(id)
	if t == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "任务不存在"})
		return
	}
	meta := t.Meta()
	if meta.Status == tasks.StatusRunning {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "msg": "任务还在跑，音频还没有生成（进度见 /v1/tasks/" + id + "/stream）",
		})
		return
	}
	if meta.Status == tasks.StatusFailed {
		writeJSON(w, http.StatusInternalServerError, apiErrorResponse(
			firstNonEmptyStr(meta.Error, "合成失败（任务日志里有原因）")))
		return
	}
	job := s.job(id)
	if job == nil || job.result == nil {
		writeJSON(w, http.StatusGone, map[string]any{
			"ok": false, "msg": "音频已过期（保留 " + s.opt.JobTTL.String() + "），请重新合成",
		})
		return
	}
	writeSpeechAudio(w, job.result, "speech")
}

func (s *SpeechServer) job(id string) *speechJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

// cleanupExpired 惰性清理过期任务产物（不额外起 goroutine）。
func (s *SpeechServer) cleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, j := range s.jobs {
		if now.Sub(j.created) < s.opt.JobTTL {
			continue
		}
		if j.result != nil {
			j.result.Cleanup()
		}
		delete(s.jobs, id)
	}
}

// ---------------------------------------------------------------------------
//  错误
// ---------------------------------------------------------------------------

// apiErrorResponse 是 OpenAI 风格的错误体（SDK 能直接读 message）。
func apiErrorResponse(msg string) map[string]any {
	return map[string]any{
		"ok": false,
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
		},
		"msg": msg,
	}
}

// writeSpeechError 把引擎/校验错误如实映射成 HTTP 状态码。
//
// 映射原则（AGENTS 第三节"读不到就拒绝，绝不猜"）：
//
//	· 请求方的问题（空文本/超长/未知音色/格式不可用）→ 400，让人能改；
//	· 引擎不可用（say 不在、没有音色）→ 503，这是环境问题不是请求问题；
//	· 其它 → 500，并把原始错误带出去（绝不吞掉）。
func writeSpeechError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, services.ErrSpeechEmptyText),
		errors.Is(err, services.ErrSpeechTextTooLong),
		errors.Is(err, services.ErrSpeechUnknownVoice),
		errors.Is(err, services.ErrSpeechUnsupportedFormat),
		errors.Is(err, services.ErrSpeechInvalidRequest):
		code = http.StatusBadRequest
	case errors.Is(err, services.ErrSpeechEngineUnavailable):
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, apiErrorResponse(err.Error()))
}
