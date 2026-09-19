package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  语音转文字 Web UI 的**独立服务**（`zizpanel stt-serve`）
//
//  两条入口（与「图片压缩」「语音合成」同一套做法）：
//    · 端口：这个进程只监听 127.0.0.1:<port>（默认 8892），界面与 API 都在 /；
//    · 别名：应用市场条目声明 UI.Slug=stt，面板的 internal/appproxy 把
//      http://<主机>/stt/ 反代到上面那个端口（要求先登录面板），
//      nginx 的 80 端口入口也走面板这一条（见 api_appproxy.go）。
//
//  为什么由面板二进制自己当这个服务：内嵌前端、无构建步骤、单二进制 ——
//  与仓库的既有约束一致；面板升级时界面一起升级。
//
//  引擎是 Homebrew 的 whisper.cpp（见 internal/services/stt.go），
//  转码用 ffmpeg（应用市场里的基础依赖条目）。不依赖 Python / Docker / GUI。
//
//  契约（对外真的可用，不只是界面自用）：
//    POST /v1/audio/transcriptions  OpenAI 兼容：multipart 的 file +
//                                   language / model / response_format /
//                                   prompt / translate；返回 text 与 segments
//    GET  /v1/models                装了哪些档、当前档是谁
//    GET  /healthz                  **能力**探活（模型在 + 引擎在 + 真的转一次）
//    POST /v1/models/{id}/download  下载某一档（202 + task_id，进度走 SSE）
//    POST /v1/models/{id}/delete    删除某一档（必须显式 confirm）
//    GET  /v1/tasks/{id}[/stream|/result|/cancel]
//
//  长音频：≤ STTSyncMaxSeconds（默认 60 秒，正好一片）同步返回；
//  更长的走 202 + task_id，进度是**真的**（按 60 秒切片，SSE 报 已完成/总片数）。
//  诚实标注：这个任务中心是本进程内的，**不会**出现在面板的「任务中心」列表里
//  （那是另一个进程的 Manager）——与「图片压缩」「语音合成」的说明一致。
//
//  安全边界：只绑 127.0.0.1（见 stt_install.go 的 plist）；走别名时还要过面板
//  登录；上传体有硬上限；上传文件与转写产物只存在于任务私有目录里，
//  路径不可由请求方指定（不构成任意文件读取）；模型删除只允许删**清单里那三个
//  文件名**（不允许任意路径删除）。
// ============================================================================

//go:embed assets/stt
var sttUIFS embed.FS

const (
	// sttUIPrefix 是内嵌资源在 embed.FS 里的前缀。
	sttUIPrefix = "assets/stt/"

	// sttMaxRequestBytes 是**整个 multipart 请求体**的硬上限。
	// 音频比文本大得多：3 小时的 m4a 约 100 MB，2 GiB 是宽裕的上限。
	sttMaxRequestBytes = int64(services.STTMaxUploadBytes)

	// sttMultipartMemory 是 multipart 在内存里缓冲的上限，超出部分落临时文件。
	// 32 MiB 是折中：小音频全程在内存里（快），大音频落盘（不爆内存）。
	sttMultipartMemory = int64(32 << 20)

	// sttJobTTL 是任务产物（上传的音频 + 转写结果）的保留时长（到期惰性清理）。
	sttJobTTL = 2 * time.Hour
)

// STTOptions 是独立服务的配置。
type STTOptions struct {
	// Listen 是监听地址（默认 127.0.0.1:8892）。
	Listen string
	// BrewPrefix 是 Homebrew 前缀（用来找 whisper-cli / ffmpeg）。
	BrewPrefix string
	// Root 是模型根目录（`<家目录>/stt`）。
	Root string
	// Model 是当前档位（空 = 默认档）。
	Model string
	// MirrorBase 是面板设置里的镜像基址（下载模型时遵守它）。
	MirrorBase string
	// Engine 覆盖引擎（测试注入；nil = 按上面的参数造一个）。
	Engine *services.STTEngine
	// SyncMaxSeconds <=0 用 services.STTSyncMaxSeconds。
	SyncMaxSeconds int
	// JobTTL <=0 用默认值。
	JobTTL time.Duration
	// TempRoot 是临时目录父目录（空 = os.TempDir()）。
	TempRoot string
}

// STTServer 是语音转文字独立服务。
type STTServer struct {
	opt    STTOptions
	engine *services.STTEngine
	tasks  *tasks.Manager

	mu   sync.Mutex
	jobs map[string]*sttJob
}

// sttJob 保存一个任务的产物（上传的音频 + 转写结果 + 它的临时目录）。
type sttJob struct {
	id      string
	created time.Time
	dir     string
	// result 在转写任务成功后才有。
	result  *services.STTResult
	format  services.STTFormat
	audioIn string
}

// NewSTTServer 造一个服务实例。
func NewSTTServer(opt STTOptions) *STTServer {
	if strings.TrimSpace(opt.Listen) == "" {
		opt.Listen = fmt.Sprintf("127.0.0.1:%d", services.STTPort)
	}
	if opt.SyncMaxSeconds <= 0 {
		opt.SyncMaxSeconds = services.STTSyncMaxSeconds
	}
	if opt.JobTTL <= 0 {
		opt.JobTTL = sttJobTTL
	}
	if strings.TrimSpace(opt.TempRoot) == "" {
		opt.TempRoot = os.TempDir()
	}
	eng := opt.Engine
	if eng == nil {
		eng = services.NewSTTEngine(opt.BrewPrefix, sttModelsDir(opt.Root), sttCurrentModelID(opt))
	}
	return &STTServer{
		opt:    opt,
		engine: eng,
		tasks:  tasks.NewManager(),
		jobs:   map[string]*sttJob{},
	}
}

// sttModelsDir 由模型根目录算出权重目录（与 services 侧同一套约定）。
func sttModelsDir(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	return filepath.Join(root, services.STTModelDirName)
}

// sttCurrentModelID 解析当前档位（非法/缺失 = 默认档）。
func sttCurrentModelID(opt STTOptions) string {
	if m, err := services.FindSTTModel(opt.Model); err == nil {
		return m.ID
	}
	if root := strings.TrimSpace(opt.Root); root != "" {
		if b, err := os.ReadFile(filepath.Join(root, services.STTCurrentModelFile)); err == nil {
			if m, ferr := services.FindSTTModel(string(b)); ferr == nil {
				return m.ID
			}
		}
	}
	return services.DefaultSTTModelID()
}

// Engine 暴露引擎给子命令（启动时打印一次真实状态）。
func (s *STTServer) Engine() *services.STTEngine { return s.engine }

// Listen 返回监听地址。
func (s *STTServer) Listen() string { return s.opt.Listen }

// Handler 返回这个服务的 HTTP 路由。
func (s *STTServer) Handler() http.Handler {
	mux := http.NewServeMux()
	// 精确根路径：直接端口访问是 /；经面板别名时 appproxy 已把 /stt 前缀剥掉。
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /app.js", s.handleAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /app.css", s.handleAsset("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/models/{id}/download", s.handleModelDownload)
	mux.HandleFunc("POST /v1/models/{id}/delete", s.handleModelDelete)
	mux.HandleFunc("POST /v1/models/{id}/select", s.handleModelSelect)
	mux.HandleFunc("POST /v1/audio/transcriptions", s.handleTranscriptions)
	mux.HandleFunc("GET /v1/tasks/{id}", s.handleTaskGet)
	mux.HandleFunc("GET /v1/tasks/{id}/stream", s.handleTaskStream)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.handleTaskCancel)
	mux.HandleFunc("GET /v1/tasks/{id}/result", s.handleTaskResult)
	return mux
}

// ---------------------------------------------------------------------------
//  静态资源
// ---------------------------------------------------------------------------

func (s *STTServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := sttUIFS.ReadFile(sttUIPrefix + "index.html")
	if err != nil {
		http.Error(w, "界面资源缺失（这是面板打包问题，请反馈）", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (s *STTServer) handleAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := sttUIFS.ReadFile(sttUIPrefix + name)
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
func (s *STTServer) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
//  健康 / 档位
// ---------------------------------------------------------------------------

// handleHealthz 是**能力**健康检查，不是"进程活着"检查。
//
// 判定贴着能力本身（AGENTS 第三节）：模型文件在 + 引擎在 + **真的跑一次极短
// 音频转写**。任一条不成立就返回 503 + ok:false + reason，这样面板的服务健康
// 检查会如实标红 —— 绝不给一个"服务在跑、但每次转写都失败"的绿灯。
func (s *STTServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h := s.engine.Health(r.Context())
	if !h.OK {
		writeJSON(w, http.StatusServiceUnavailable, h)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// handleModels 返回档位清单（装了哪些、当前是哪一档）。
//
// 形状照 OpenAI 的 list 接口（object/data），但每一项都带上面板扩展的
// installed / size_bytes / path —— 界面据此禁用或启用"下载"按钮，
// 调用方也据此知道"现在到底能转哪一档"。
func (s *STTServer) handleModels(w http.ResponseWriter, r *http.Request) {
	state := s.modelsState()
	data := make([]map[string]any, 0, len(state.Models))
	for _, m := range state.Models {
		item := map[string]any{
			"id": m.ID, "object": "model",
			"name": m.Name, "file": m.File,
			"bytes": m.Bytes, "size_human": humanSize(m.Bytes),
			"installed": m.Installed, "missing": m.Missing,
			"current": m.Current, "default": m.Default,
			"path": m.Path, "note": m.Note,
		}
		if m.RAMMB > 0 {
			item["ram_mb"] = m.RAMMB
		}
		if m.RAMNote != "" {
			item["ram_note"] = m.RAMNote
		}
		if m.Reason != "" {
			item["reason"] = m.Reason
		}
		if m.SizeBytes > 0 {
			item["size_bytes"] = m.SizeBytes
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  "list",
		"data":    data,
		"current": state.Current,
		"ready":   state.Ready,
		"total":   state.Total,
		// 面板扩展：把"怎么用"的元信息一次给全，省得调用方去猜。
		"languages":         services.STTLanguageChoices,
		"formats":           services.STTFormats,
		"service":           services.STTAppID,
		"market_app_id":     services.STTAppID,
		"sync_max_seconds":  s.opt.SyncMaxSeconds,
		"max_audio_seconds": services.STTMaxAudioSeconds,
		"chunk_seconds":     services.STTChunkSeconds,
		"models_dir":        sttModelsDir(s.opt.Root),
	})
}

// modelsState 组装档位现状（当前档取自引擎，保证与 /healthz 一致）。
func (s *STTServer) modelsState() services.STTModelsState {
	return services.STTModelsStateFor(sttModelsDir(s.opt.Root), s.engine.CurrentModel)
}

// handleModelSelect 切换当前档位（只允许切成**已经下好的**档）。
func (s *STTServer) handleModelSelect(w http.ResponseWriter, r *http.Request) {
	m, err := services.FindSTTModel(r.PathValue("id"))
	if err != nil {
		writeSTTError(w, err)
		return
	}
	if !services.STTModelFileExists(sttModelsDir(s.opt.Root), m.ID) {
		writeJSON(w, http.StatusConflict, sttErrorResponse(fmt.Sprintf(
			"档位 %s 的模型还没下载，不能切成当前档；请先 POST /v1/models/%s/download", m.ID, m.ID)))
		return
	}
	if err := services.WriteSTTCurrentModel(s.opt.Root, m.ID); err != nil {
		writeSTTError(w, err)
		return
	}
	s.engine.CurrentModel = m.ID
	s.engine.InvalidateHealthProbe()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "current": m.ID})
}

// handleModelDelete 删除某一档的权重文件。
//
// 必须显式 `{"confirm": true}`：删模型是不可逆的（要重新下几百 MB ~ 1.5 GB），
// 不能让一个手滑的请求或一个爬虫把它删掉。界面会二次确认后再带上这个字段。
func (s *STTServer) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	m, err := services.FindSTTModel(r.PathValue("id"))
	if err != nil {
		writeSTTError(w, err)
		return
	}
	var body struct {
		Confirm bool `json:"confirm"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if !body.Confirm {
		writeJSON(w, http.StatusBadRequest, sttErrorResponse(
			"删除模型必须先确认（请求体 {\"confirm\":true}）—— 删掉要重新下载，不可逆"))
		return
	}
	path := filepath.Join(sttModelsDir(s.opt.Root), m.File)
	st, serr := os.Stat(path)
	if serr != nil {
		writeJSON(w, http.StatusNotFound, sttErrorResponse("档位 "+m.ID+" 的模型文件不在（"+path+"），无需删除"))
		return
	}
	if rmErr := os.Remove(path); rmErr != nil {
		writeSTTError(w, fmt.Errorf("删除模型 %s 失败：%w", path, rmErr))
		return
	}
	// 删掉的正好是当前档时，把当前档回落到默认档（否则 /healthz 就一直红着，
	// 而用户只是"清理了一下磁盘"）。
	current := s.engine.CurrentModel
	if strings.EqualFold(current, m.ID) {
		fallback := services.DefaultSTTModelID()
		if !services.STTModelFileExists(sttModelsDir(s.opt.Root), fallback) {
			for _, st := range s.modelsState().Models {
				if st.Installed {
					fallback = st.ID
					break
				}
			}
		}
		_ = services.WriteSTTCurrentModel(s.opt.Root, fallback)
		s.engine.CurrentModel = fallback
	}
	s.engine.InvalidateHealthProbe()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "deleted": m.ID, "path": path, "freed_bytes": st.Size(),
		"current": s.engine.CurrentModel,
	})
}

// handleModelDownload 下载某一档（202 + task_id，进度走 SSE）。
//
// 下载是**长任务**（medium 档 1.43 GiB / 实测 783 KB/s ≈ 32 分钟），
// 所以必须走任务中心：同步返回会让用户只能对着转圈，关掉窗口就找不回进度，
// 而且任务挂在 r.Context() 上 —— 用户一刷新就把下载杀了（AGENTS 第三节）。
func (s *STTServer) handleModelDownload(w http.ResponseWriter, r *http.Request) {
	model, err := services.FindSTTModel(r.PathValue("id"))
	if err != nil {
		writeSTTError(w, err)
		return
	}
	if services.STTModelFileExists(sttModelsDir(s.opt.Root), model.ID) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "already": true, "model": model.ID,
			"msg": "档位 " + model.ID + " 的模型已经在了，无需下载",
		})
		return
	}
	s.cleanupExpired()
	srcs := services.STTModelSourcesForServe(r.Context(), s.opt.MirrorBase, model)
	dst := filepath.Join(sttModelsDir(s.opt.Root), model.File)
	label := fmt.Sprintf("下载语音模型 %s（%s）", model.ID, humanSize(model.Bytes))
	t := s.tasks.StartWithTask("stt_model_download", "stt", label,
		func(ctx context.Context, task *tasks.Task) (any, error) {
			log := task.LogFunc()
			log(tasks.LevelStep, fmt.Sprintf("档位 %s：%s → %s，共 %d 个候选来源（镜像优先，逐个回落）",
				model.ID, humanSize(model.Bytes), dst, len(srcs)))
			dctx, cancel := context.WithTimeout(ctx, services.STTModelDownloadTimeout)
			defer cancel()
			res, derr := services.DownloadSTTModelTo(dctx, srcs, dst, model, services.STTDefaultFetch,
				func(note string) { log(tasks.LevelStep, note) })
			if derr != nil {
				return nil, derr
			}
			log(tasks.LevelOK, fmt.Sprintf("模型就绪：%s（来源 %s，用时 %.0f 秒）",
				res.Path, res.Source, res.Elapsed.Seconds()))
			s.engine.InvalidateHealthProbe()
			// 原本一档都没有时，下完这一档就把它切成当前档 ——
			// 否则用户下完了却发现"还是不能用"（当前档仍是那个没下的）。
			if !services.STTModelFileExists(sttModelsDir(s.opt.Root), s.engine.CurrentModel) {
				if werr := services.WriteSTTCurrentModel(s.opt.Root, model.ID); werr == nil {
					s.engine.CurrentModel = model.ID
					log(tasks.LevelStep, "已把当前档切换为 "+model.ID)
				}
			}
			return map[string]any{
				"model": model.ID, "path": res.Path, "source": res.Source,
				"bytes": res.Bytes, "elapsed_ms": res.Elapsed.Milliseconds(),
			}, nil
		})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true,
		"data": map[string]any{
			"task_id": t.ID(), "model": model.ID, "bytes": model.Bytes,
			"stream_url": "/v1/tasks/" + t.ID() + "/stream",
			"result_url": "/v1/tasks/" + t.ID() + "/result",
			"note":       "模型下载已转入任务：进度见 stream_url（下载进度是真的，来自字节数）。",
		},
	})
}

// ---------------------------------------------------------------------------
//  转写
// ---------------------------------------------------------------------------

// handleTranscriptions 是 OpenAI 兼容的转写入口（multipart/form-data）。
//
// 两种返回：
//   - 短音频（≤ SyncMaxSeconds）：200 + 请求要的 response_format 正文；
//   - 长音频：202 + task_id（进度走 SSE，结果从 /result 取）。
//
// 参数校验**在开任务之前**：未知档位 / 档位没装 / response_format 拼错
// 一律当场 4xx，不让用户对着一个注定失败的任务干等。
func (s *STTServer) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	// 上传体是音频，可能几十上百 MB，而面板给整个进程设了 ReadTimeout=30s
	// （"从连接建立到读完整个请求体"的绝对截止时间）—— 不推后读截止时间，
	// 慢网络下的大文件会在 30 秒处被掐断，浏览器只看到网络错误。
	// 这条与 handleFileUpload 是同一道保护（门禁
	// TestEveryMultipartRouteClearsReadDeadline 会抓漏）。
	// 失败不拦请求（理由见 api_upload_guard.go：http.ErrNotSupported 只说明
	// 这个 ResponseWriter 不支持设置截止时间，不是错误）。
	_ = allowLongUpload(w, r)
	// 请求体硬上限：超过就 413，绝不静默截断音频（截断 = 用户以为整段都转完了）。
	r.Body = http.MaxBytesReader(w, r.Body, sttMaxRequestBytes)
	if err := r.ParseMultipartForm(sttMultipartMemory); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, sttErrorResponse(fmt.Sprintf(
				"上传体超过上限 %s（%s）", humanSize(sttMaxRequestBytes), humanSize(int64(services.STTMaxUploadBytes)))))
			return
		}
		writeJSON(w, http.StatusBadRequest, sttErrorResponse(
			"请求体不是合法的 multipart/form-data："+err.Error()))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, fh, ferr := r.FormFile("file")
	if ferr != nil {
		writeJSON(w, http.StatusBadRequest, sttErrorResponse(
			"缺少 file 字段（OpenAI 兼容格式：multipart/form-data 里的 file 就是要转写的音频）"))
		return
	}
	defer func() { _ = file.Close() }()

	// 先把上传落到任务私有目录：请求返回后 r.Body 就没了，
	// 长音频的转写是在后台任务里跑的。路径由面板生成，请求方指定不了。
	workDir, derr := os.MkdirTemp(s.opt.TempRoot, "zp-stt-up-")
	if derr != nil {
		writeSTTError(w, fmt.Errorf("创建临时目录失败：%w", derr))
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(workDir)
		}
	}()
	audioPath, serr := s.saveUpload(file, fh, workDir)
	if serr != nil {
		writeSTTError(w, serr)
		return
	}

	// 这里之后才做"要开任务吗"的判断 —— 时长必须真的探一次。
	opt, format, perr := s.prepareTranscribe(r, audioPath)
	if perr != nil {
		writeSTTError(w, perr)
		return
	}
	info, ierr := s.engine.ProbeAudio(r.Context(), audioPath)
	if ierr != nil {
		// 探不出时长就**不猜**：ffprobe 都读不出来的文件，引擎也读不出来。
		writeSTTError(w, ierr)
		return
	}
	opt.KnownDuration = info.Seconds

	// 时长上限**在开任务之前**判：超过上限的音频注定失败，先拒掉能给出
	// 精确的 413 + 原因，而不是让用户对着一个必然失败的任务干等，
	// 也不是等任务失败后从日志字符串里猜状态码。
	if info.Seconds > float64(services.STTMaxAudioSeconds) {
		writeSTTError(w, fmt.Errorf("%w：音频 %.1f 秒，上限 %d 秒（%.1f 小时）。"+
			"请先自行剪短再上传（面板不静默截断 —— 截断会让你以为整段都转完了）",
			services.ErrSTTAudioTooLong, info.Seconds,
			services.STTMaxAudioSeconds, float64(services.STTMaxAudioSeconds)/3600))
		return
	}

	if info.Seconds <= float64(s.opt.SyncMaxSeconds) {
		// 秒级路径：同步跑完直接给正文（客户端拿到的就是最终结果）。
		res, terr := s.engine.Transcribe(r.Context(), opt)
		if terr != nil {
			writeSTTError(w, terr)
			return
		}
		s.writeTranscript(w, res, format, http.StatusOK)
		return
	}

	// 长音频路径：交给本进程的任务中心，立刻返回 202 + task_id。
	// 进度是真的 —— Transcribe 每转完一片就回报一次 done/total。
	s.cleanupExpired()
	keep = true
	chunks := int(info.Seconds/float64(services.STTChunkSeconds)) + 1
	t := s.tasks.StartWithTask("stt_transcribe", "stt",
		fmt.Sprintf("语音转文字 %.1f 秒（%d 片，档位 %s）", info.Seconds, chunks, opt.ModelID),
		func(ctx context.Context, task *tasks.Task) (any, error) {
			log := task.LogFunc()
			opt.Progress = func(done, total int, note string) {
				log(tasks.LevelStep, fmt.Sprintf("%s（总时长 %.1f 秒，格式 %s）", note, info.Seconds, format))
			}
			log(tasks.LevelStep, fmt.Sprintf("开始转写：%.1f 秒音频，档位 %s，语言 %s",
				info.Seconds, opt.ModelID, firstNonEmptyStr(opt.Language, "auto")))
			res, rerr := s.engine.Transcribe(ctx, opt)
			if rerr != nil {
				return nil, rerr
			}
			log(tasks.LevelOK, fmt.Sprintf("转写完成：%d 片、%d 个分段、%d 字（耗时 %.1f 秒）",
				res.Chunks, len(res.Segments), len([]rune(res.Text)), float64(res.ElapsedMS)/1000))
			s.mu.Lock()
			s.jobs[task.ID()] = &sttJob{
				id: task.ID(), created: time.Now(), dir: workDir,
				result: res, format: format, audioIn: audioPath,
			}
			s.mu.Unlock()
			return map[string]any{
				"model": res.ModelID, "language": res.Language,
				"duration_ms": res.DurationMS, "chunks": res.Chunks,
				"segments": len(res.Segments), "chars": len([]rune(res.Text)),
				"elapsed_ms": res.ElapsedMS,
				"result_url": "/v1/tasks/" + task.ID() + "/result",
			}, nil
		})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true,
		"data": map[string]any{
			"task_id": t.ID(), "sync": false,
			"duration_seconds": info.Seconds, "chunks": chunks,
			"model": opt.ModelID, "response_format": string(format),
			"stream_url":       "/v1/tasks/" + t.ID() + "/stream",
			"result_url":       "/v1/tasks/" + t.ID() + "/result",
			"sync_max_seconds": s.opt.SyncMaxSeconds,
			"note": "音频超过同步上限（" + strconv.Itoa(s.opt.SyncMaxSeconds) +
				" 秒），已转入任务：进度见 stream_url（按 " + strconv.Itoa(services.STTChunkSeconds) +
				" 秒切片，进度是真的），结果在任务完成后从 result_url 取。",
		},
	})
}

// saveUpload 把上传的文件写到任务私有目录。
//
// 返回的路径**完全由面板拼**（只用请求里的文件扩展名，且做了白名单清洗），
// 请求方**无法**指定落盘路径 —— 不构成任意文件写入。
func (s *STTServer) saveUpload(file multipart.File, fh *multipart.FileHeader, dir string) (string, error) {
	name := "upload"
	if fh != nil {
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		if sttAllowedExt(ext) {
			name += ext
		}
	}
	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("写入上传文件失败：%w", err)
	}
	defer func() { _ = out.Close() }()
	n, err := io.Copy(out, io.LimitReader(file, sttMaxRequestBytes+1))
	if err != nil {
		return "", fmt.Errorf("接收上传文件失败：%w", err)
	}
	if n == 0 {
		return "", services.ErrSTTAudioEmpty
	}
	if n > sttMaxRequestBytes {
		return "", fmt.Errorf("上传体超过上限 %s", humanSize(sttMaxRequestBytes))
	}
	return dst, nil
}

// sttAudioExts 是允许保留的扩展名（只是为了给 ffmpeg 一个好提示，
// 内容格式仍由 ffprobe 判定 —— 扩展名骗人时以内容为准）。
var sttAudioExts = map[string]bool{
	".wav": true, ".mp3": true, ".m4a": true, ".mp4": true, ".aac": true,
	".flac": true, ".ogg": true, ".oga": true, ".opus": true, ".webm": true,
	".aiff": true, ".aif": true, ".aifc": true, ".caf": true, ".wma": true,
	".mov": true, ".mkv": true, ".amr": true, ".3gp": true,
}

func sttAllowedExt(ext string) bool { return sttAudioExts[ext] }

// prepareTranscribe 校验请求并把它变成引擎的转写参数 + 导出格式。
//
// 导出格式与引擎参数**分开返回**：引擎只产出分段（一份数据），
// json/text/srt/vtt 是同一条数据的四种渲染 —— 这样"文本里有、字幕里没有"
// 这类不一致不可能发生。
func (s *STTServer) prepareTranscribe(r *http.Request, audioPath string) (services.STTOptions, services.STTFormat, error) {
	model, err := services.FindSTTModel(r.FormValue("model"))
	if err != nil {
		return services.STTOptions{}, "", err
	}
	if !services.STTModelFileExists(sttModelsDir(s.opt.Root), model.ID) {
		return services.STTOptions{}, "", fmt.Errorf("%w：档位 %s（%s）没下载。"+
			"到网页界面点「下载该档位」，或 POST /v1/models/%s/download；GET /v1/models 看哪一档已就绪",
			services.ErrSTTModelMissing, model.ID, humanSize(model.Bytes), model.ID)
	}
	format, ferr := services.ParseSTTFormat(r.FormValue("response_format"))
	if ferr != nil {
		return services.STTOptions{}, "", ferr
	}
	translate := false
	switch strings.ToLower(strings.TrimSpace(r.FormValue("translate"))) {
	case "1", "true", "yes", "on":
		translate = true
	}
	lang := services.NormalizeSTTLanguage(r.FormValue("language"))
	return services.STTOptions{
		AudioPath: audioPath,
		ModelID:   model.ID,
		Language:  lang,
		Translate: translate,
		Prompt:    r.FormValue("prompt"),
	}, format, nil
}

// writeTranscript 按请求的格式把结果写回。
//
// 顺带带上 X-Zizpanel-* 头：**实际使用**的档位、识别到的语言、切片数、耗时 ——
// 这些不是装饰，请求方要能核对"我传的档位/语言"与"实际用的"是不是一回事。
func (s *STTServer) writeTranscript(w http.ResponseWriter, res *services.STTResult, format services.STTFormat, code int) {
	body, err := services.FormatTranscript(res, format)
	if err != nil {
		writeSTTError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", format.ContentType())
	h.Set("Cache-Control", "no-store")
	h.Set("X-Zizpanel-Model", res.ModelID)
	h.Set("X-Zizpanel-Model-File", res.ModelFile)
	h.Set("X-Zizpanel-Language", res.Language)
	h.Set("X-Zizpanel-Chunks", strconv.Itoa(res.Chunks))
	h.Set("X-Zizpanel-Duration-Ms", strconv.Itoa(res.DurationMS))
	h.Set("X-Zizpanel-Elapsed-Ms", strconv.Itoa(res.ElapsedMS))
	h.Set("X-Zizpanel-Segments", strconv.Itoa(len(res.Segments)))
	// text / srt / vtt 是纯文本，给下载名会方便用户；json 保持 inline。
	if format != services.STTFormatJSON && format != services.STTFormatVerboseJSON {
		h.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", "transcript."+string(format)))
	}
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// ---------------------------------------------------------------------------
//  任务
// ---------------------------------------------------------------------------

func (s *STTServer) handleTaskGet(w http.ResponseWriter, r *http.Request) {
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
			"task":       t.Meta(),
			"done":       t.Status() != tasks.StatusRunning,
			"result_url": "/v1/tasks/" + id + "/result",
		},
	})
}

func (s *STTServer) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
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
// 与 imgcompress / speech 的实现同形：这里不发明第二套协议。
func (s *STTServer) handleTaskStream(w http.ResponseWriter, r *http.Request) {
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
		send("done", map[string]any{"task": t.Meta(), "result_url": "/v1/tasks/" + t.ID() + "/result"}, cursor)
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// 心跳：防止中间的反代把空闲连接掐掉（模型下载可能十几分钟没新行）。
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
			send("done", map[string]any{"task": t.Meta(), "result_url": "/v1/tasks/" + t.ID() + "/result"}, cursor)
			return
		}
	}
}

// flushRemaining 把任务缓冲里 cursor 之后的日志行补齐（任务刚好结束时用）。
func (s *STTServer) flushRemaining(t *tasks.Task, send func(string, any, int64) bool, cursor *int64) {
	rest, next, _, _ := t.Snapshot(*cursor, 4000)
	if len(rest) == 0 {
		return
	}
	*cursor = next
	send("lines", map[string]any{"lines": rest}, *cursor)
}

// handleTaskResult 取任务的最终结果。
//
//   - 模型下载：返回下载结论（路径/来源/字节数）；
//   - 语音转写：按**当初请求的 response_format** 渲染（text 就是纯文本、
//     srt 就是字幕文件），所以长音频与短音频拿到的正文格式完全一致。
//
// 任务失败时如实回报失败原因（不返回空正文冒充成功）。
func (s *STTServer) handleTaskResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t := s.tasks.Get(id)
	if t == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "任务不存在"})
		return
	}
	meta := t.Meta()
	if meta.Status == tasks.StatusRunning {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "msg": "任务还在跑（进度见 /v1/tasks/" + id + "/stream）",
		})
		return
	}
	if meta.Status == tasks.StatusFailed {
		// 说明：任务体里抛出的错误到这里只剩一个字符串（tasks.Meta 不带错误类别），
		// 所以**请求方那一类的错误都已经在开任务之前被拦掉了**（档位没下 → 409、
		// 格式拼错 → 400、超时长 → 413、音频解不开 → 415）。能走到这里的都是
		// 引擎/环境在后台跑的时候出的问题 —— 那确实是服务端的失败，如实给 503。
		writeJSON(w, http.StatusServiceUnavailable, sttErrorResponse(
			firstNonEmptyStr(meta.Error, "任务失败（任务日志里有原因）")))
		return
	}
	if meta.Kind == "stt_model_download" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "kind": meta.Kind, "result": meta.Result,
			"models": s.modelsState(),
		})
		return
	}
	job := s.job(id)
	if job == nil || job.result == nil {
		writeJSON(w, http.StatusGone, map[string]any{
			"ok": false, "msg": "结果已过期（保留 " + s.opt.JobTTL.String() + "），请重新转写",
		})
		return
	}
	// 想让调用方拿到 JSON 元信息时给 ?meta=1（界面用它显示语言/切片数）；
	// 默认按请求时选的格式返回正文（OpenAI 的语义）。
	if r.URL.Query().Get("meta") == "1" {
		body, _ := services.FormatTranscript(job.result, services.STTFormatVerboseJSON)
		var parsed any
		_ = json.Unmarshal([]byte(body), &parsed)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "kind": meta.Kind, "result": parsed,
			"response_format": string(job.format),
		})
		return
	}
	s.writeTranscript(w, job.result, job.format, http.StatusOK)
}

func (s *STTServer) job(id string) *sttJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

// cleanupExpired 惰性清理过期任务产物（不额外起 goroutine）。
func (s *STTServer) cleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, j := range s.jobs {
		if now.Sub(j.created) < s.opt.JobTTL {
			continue
		}
		if j.dir != "" {
			_ = os.RemoveAll(j.dir)
		}
		delete(s.jobs, id)
	}
}

// ---------------------------------------------------------------------------
//  错误
// ---------------------------------------------------------------------------

// sttErrorResponse 是 OpenAI 风格的错误体（SDK 能直接读 message）。
func sttErrorResponse(msg string) map[string]any {
	return map[string]any{
		"ok": false,
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
		},
		"msg": msg,
	}
}

// writeSTTError 把引擎/校验错误如实映射成 HTTP 状态码。
//
// 映射原则（AGENTS 第三节"读不到就拒绝，绝不猜"）：
//
//	· 请求方的问题（空音频/档位名错/格式错）      → 400，让人能改；
//	· 音频本身的问题（解不开/没有音轨）           → 415，是**媒体类型**的问题；
//	· 音频超限                                    → 413；
//	· 档位存在但没下模型                          → 409，环境缺件但请求没错；
//	· 引擎/ffmpeg 不可用                          → 503，环境问题不是请求问题；
//	· 其它                                        → 500，并把原始错误带出去。
func writeSTTError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, services.ErrSTTAudioEmpty),
		errors.Is(err, services.ErrSTTModelUnknown),
		errors.Is(err, services.ErrSTTInvalidRequest),
		errors.Is(err, services.ErrSTTExportUnsupported):
		code = http.StatusBadRequest
	case errors.Is(err, services.ErrSTTAudioUnsupported):
		code = http.StatusUnsupportedMediaType
	case errors.Is(err, services.ErrSTTAudioTooLong):
		code = http.StatusRequestEntityTooLarge
	case errors.Is(err, services.ErrSTTModelMissing):
		code = http.StatusConflict
	case errors.Is(err, services.ErrSTTEngineUnavailable),
		errors.Is(err, services.ErrSTTFfmpegMissing):
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, sttErrorResponse(err.Error()))
}

// humanSize 是界面/日志里的可读体积（与 services 里的 humanBytes 同一口径，
// 但 web 包不能反向依赖 services 的内部函数，所以这里保留一份薄的）。
func humanSize(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
