package web

import (
	"archive/zip"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/imgopt"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  图片压缩 Web UI 的**独立服务**（`zizpanel imgcompress-serve`）
//
//  为什么要有它（用户原话："了（子）代理处理：图片压缩（libvips）
//  安装完没有反应！这个问题解决一下。给它加一个 webui 通过端口和别名调用！"）：
//    · 端口：这个进程只监听 127.0.0.1:<port>（默认 8890），界面与 API 都在 `/`；
//    · 别名：应用市场条目声明 UI.Slug=imgcompress，面板的 internal/appproxy
//      把 http://<主机>/imgcompress/ 反代到上面那个端口（要求先登录面板），
//      nginx 的 80 端口入口也是走面板这一条（见 api_appproxy.go）。
//
//  为什么由面板二进制自己当这个服务（而不是再造一个小程序）：
//    · 内嵌前端、无构建步骤、单二进制 —— 与仓库的既有约束一致；
//    · 面板升级时界面一起升级，不会出现"面板换了、应用还是旧的"。
//
//  进度：长任务走**与面板任务中心同一套契约**（202 + task_id + SSE 事件流）。
//  为什么不是复用面板那个 Manager 实例：面板的任务中心是**进程内**的
//  （internal/tasks.Manager 只有内存态），而这是一个独立的 launchd 进程 ——
//  跨进程共享同一个 Manager 在本项目里不存在。所以这里起一个自己的
//  tasks.Manager，事件形状/断点续传语义与面板完全一致，前端代码可以照搬。
//  诚实标注：它的任务**不会**出现在面板的「任务中心」列表里（那是另一个进程）。
//
//  安全边界：
//    · 只绑 127.0.0.1（见 imgcompress.go 的 plist）；走别名时还要过面板登录；
//    · 上传只接受白名单图片扩展名、单文件与单次总量都有上限；
//    · 每个任务一个私有临时目录，输出文件只能通过任务 id + 索引取，路径不可由
//      请求方指定（不构成任意文件读取）。
// ============================================================================

//go:embed assets/imgcompress
var imgCompressUIFS embed.FS

const (
	// imgCompressUIPrefix 是内嵌资源在 embed.FS 里的前缀。
	imgCompressUIPrefix = "assets/imgcompress/"

	// 默认上限。刻意都是"有明确数字、报错时原样告诉用户"的硬限制，
	// 不做静默截断（静默截断会让用户以为全压完了）。
	imgCompressDefaultMaxFileBytes  = 64 << 20
	imgCompressDefaultMaxTotalBytes = 256 << 20
	imgCompressDefaultMaxFiles      = 100
	// imgCompressDefaultSyncMaxBytes：单文件且不超过它时同步返回（"秒级单文件"）。
	imgCompressDefaultSyncMaxBytes = 4 << 20
	// imgCompressDefaultJobTTL 是任务输出的保留时长（到期自动清临时目录）。
	imgCompressDefaultJobTTL = 2 * time.Hour
)

// ImgCompressEngine 暴露引擎探测给子命令（启动时打印一次真实状态）。
func ImgCompressEngine(brewPrefix string) imgopt.Engine {
	return imgopt.DetectEngine(brewPrefix)
}

// ImgCompressOptions 是独立服务的配置。
type ImgCompressOptions struct {
	// Listen 是监听地址（默认 127.0.0.1:8890）。
	Listen string
	// BrewPrefix 用于探测 `vips`（空则退回 PATH）。
	BrewPrefix string
	// MaxFileBytes / MaxTotalBytes / MaxFiles 是上传上限（<=0 用默认值）。
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxFiles      int
	// SyncMaxBytes：单文件且总量不超过它时同步返回结果（<=0 用默认值）。
	SyncMaxBytes int64
	// JobTTL 是任务输出保留时长（<=0 用默认值）。
	JobTTL time.Duration
	// TempRoot 是临时目录的父目录（空 = os.TempDir()）。
	TempRoot string
	// DetectEngine 覆盖引擎探测（测试注入；nil = imgopt.DetectEngine）。
	DetectEngine func(brewPrefix string) imgopt.Engine
}

// ImgCompressServer 是图片压缩独立服务。
type ImgCompressServer struct {
	opt   ImgCompressOptions
	tasks *tasks.Manager

	mu   sync.Mutex
	jobs map[string]*imgCompressJob
}

type imgCompressJob struct {
	id      string
	dir     string
	created time.Time

	mu      sync.Mutex
	outputs map[int]imgCompressOutput
	result  *imgCompressResult
}

type imgCompressOutput struct {
	Path string
	Name string
}

// imgCompressItem 是单个文件的压缩结果（JSON 契约，前端直接渲染）。
type imgCompressItem struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Dst     string `json:"dst,omitempty"`
	Before  int64  `json:"before"`
	After   int64  `json:"after"`
	Saved   int64  `json:"saved"`
	Skipped bool   `json:"skipped"`
	Error   string `json:"error,omitempty"`
}

type imgCompressResult struct {
	Total       int               `json:"total"`
	Done        int               `json:"done"`
	Skipped     int               `json:"skipped"`
	Failed      int               `json:"failed"`
	BeforeBytes int64             `json:"before_bytes"`
	AfterBytes  int64             `json:"after_bytes"`
	SavedBytes  int64             `json:"saved_bytes"`
	Format      string            `json:"format"`
	Quality     int               `json:"quality"`
	MaxEdge     int               `json:"max_edge"`
	Strip       bool              `json:"strip_metadata"`
	Items       []imgCompressItem `json:"items"`
}

// NewImgCompressServer 构造独立服务（不监听，调用方自己起 http.Server）。
func NewImgCompressServer(opt ImgCompressOptions) *ImgCompressServer {
	if strings.TrimSpace(opt.Listen) == "" {
		opt.Listen = fmt.Sprintf("127.0.0.1:%d", services.ImgCompressPort)
	}
	if opt.MaxFileBytes <= 0 {
		opt.MaxFileBytes = imgCompressDefaultMaxFileBytes
	}
	if opt.MaxTotalBytes <= 0 {
		opt.MaxTotalBytes = imgCompressDefaultMaxTotalBytes
	}
	if opt.MaxFiles <= 0 {
		opt.MaxFiles = imgCompressDefaultMaxFiles
	}
	if opt.SyncMaxBytes <= 0 {
		opt.SyncMaxBytes = imgCompressDefaultSyncMaxBytes
	}
	if opt.JobTTL <= 0 {
		opt.JobTTL = imgCompressDefaultJobTTL
	}
	if strings.TrimSpace(opt.TempRoot) == "" {
		opt.TempRoot = os.TempDir()
	}
	if opt.DetectEngine == nil {
		opt.DetectEngine = imgopt.DetectEngine
	}
	return &ImgCompressServer{
		opt:   opt,
		tasks: tasks.NewManager(),
		jobs:  map[string]*imgCompressJob{},
	}
}

// Listen 返回监听地址。
func (s *ImgCompressServer) Listen() string { return s.opt.Listen }

// Handler 返回这个服务的 HTTP 路由。
func (s *ImgCompressServer) Handler() http.Handler {
	mux := http.NewServeMux()
	// 精确根路径：直接端口访问是 /；经面板别名时 appproxy 已把 /imgcompress 前缀剥掉。
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /app.js", s.handleAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /app.css", s.handleAsset("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/engine", s.handleEngine)
	mux.HandleFunc("POST /api/v1/compress", s.handleCompress)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.handleTaskGet)
	mux.HandleFunc("GET /api/v1/tasks/{id}/stream", s.handleTaskStream)
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.handleTaskCancel)
	mux.HandleFunc("GET /api/v1/tasks/{id}/files/{idx}", s.handleTaskFile)
	mux.HandleFunc("GET /api/v1/tasks/{id}/zip", s.handleTaskZip)
	return mux
}

// ---------------------------------------------------------------------------
//  静态资源
// ---------------------------------------------------------------------------

func (s *ImgCompressServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := imgCompressUIFS.ReadFile(imgCompressUIPrefix + "index.html")
	if err != nil {
		http.Error(w, "界面资源缺失（这是面板打包问题，请反馈）", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (s *ImgCompressServer) handleAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := imgCompressUIFS.ReadFile(imgCompressUIPrefix + name)
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
func (s *ImgCompressServer) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
//  健康 / 引擎
// ---------------------------------------------------------------------------

// handleHealthz 是**能力**健康检查，不是"进程活着"检查。
//
// 判定贴着能力本身（AGENTS 第三节）：`vips` 不在或跑不起来时返回 503 +
// ok:false，这样面板的服务健康检查会如实标红 —— 绝不给一个"服务在跑、
// 但每次压缩都失败"的绿灯。
func (s *ImgCompressServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	eng := s.opt.DetectEngine(s.opt.BrewPrefix)
	if !eng.Available() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":            false,
			"reason":        eng.Reason,
			"market_app_id": imgCompressAppID,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"engine":  eng.Version,
		"bin":     eng.Bin,
		"service": "imgcompress",
	})
}

func (s *ImgCompressServer) handleEngine(w http.ResponseWriter, r *http.Request) {
	eng := s.opt.DetectEngine(s.opt.BrewPrefix)
	formats := make([]string, 0, len(imgopt.Formats))
	for _, f := range imgopt.Formats {
		formats = append(formats, string(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available":        eng.Available(),
		"bin":              eng.Bin,
		"version":          eng.Version,
		"reason":           eng.Reason,
		"formats":          formats,
		"default_quality":  imgopt.DefaultQuality,
		"max_edge_choices": []int{0, 1280, 1920, 2560, 3840},
		"market_app_id":    imgCompressAppID,
		"max_file_bytes":   s.opt.MaxFileBytes,
		"max_total_bytes":  s.opt.MaxTotalBytes,
		"max_files":        s.opt.MaxFiles,
	})
}

// ---------------------------------------------------------------------------
//  压缩
// ---------------------------------------------------------------------------

// handleCompress 接收上传并压缩。
//
// 两种返回：
//   - 单文件且不大 → 同步 200，直接带回结果（"秒级单文件可同步返回"）；
//   - 多文件 / 大文件 → 202 + task_id，进度走 SSE（见 handleTaskStream）。
func (s *ImgCompressServer) handleCompress(w http.ResponseWriter, r *http.Request) {
	// 先解除服务端 30 秒读超时对这条路由的限制。
	//
	// 为什么必须有（门禁 TestEveryMultipartRouteClearsReadDeadline 会遍历全部
	// multipart 路由）：面板全局 http.Server 设了 ReadTimeout=30s，它是"从连接
	// 建立到读完整个请求体"的**绝对**截止时间 —— 一张几十 MB 的图在慢网络下
	// 传不完就被掐断，浏览器只看到"网络错误"，用户看到的就是"点了没反应"。
	// 上传体全部在开任务之前读完，所以这一步同时覆盖了任务中心那条路径。
	if err := allowLongUpload(w, r); err != nil && !errors.Is(err, http.ErrNotSupported) {
		fmt.Fprintf(os.Stderr, "imgcompress: 延长上传读超时失败（超过 30 秒的上传可能被中断）: %v\n", err)
	}

	eng := s.opt.DetectEngine(s.opt.BrewPrefix)
	if !eng.Available() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok":  false,
			"msg": "图片压缩引擎不可用：" + eng.Reason,
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.opt.MaxTotalBytes+(8<<20))
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok": false, "msg": fmt.Sprintf("一次上传的总大小超过上限 %s，请分批上传",
					humanBytes(s.opt.MaxTotalBytes)),
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "msg": "上传内容解析失败：" + err.Error(),
		})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	opt, err := imgCompressOptionsFromForm(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": err.Error()})
		return
	}

	fileHeaders := r.MultipartForm.File["files"]
	if len(fileHeaders) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "msg": "没有收到任何文件（表单字段名必须是 files）",
		})
		return
	}
	if len(fileHeaders) > s.opt.MaxFiles {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"ok": false, "msg": fmt.Sprintf("一次最多 %d 个文件，这次收到 %d 个，请分批上传",
				s.opt.MaxFiles, len(fileHeaders)),
		})
		return
	}

	var total int64
	var badExt []string
	for _, fh := range fileHeaders {
		if fh.Size > s.opt.MaxFileBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok": false, "msg": fmt.Sprintf("「%s」大小 %s，超过单文件上限 %s",
					fh.Filename, humanBytes(fh.Size), humanBytes(s.opt.MaxFileBytes)),
			})
			return
		}
		total += fh.Size
		if !imgopt.IsImagePath(fh.Filename) {
			badExt = append(badExt, fh.Filename)
		}
	}
	if total > s.opt.MaxTotalBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"ok": false, "msg": fmt.Sprintf("这次上传共 %s，超过单次上限 %s，请分批上传",
				humanBytes(total), humanBytes(s.opt.MaxTotalBytes)),
		})
		return
	}
	if len(badExt) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "msg": "这些文件不是支持的图片格式（jpg / jpeg / png / webp / tif / gif；" +
				"heic / heif 由 macOS 系统解码器兜底；avif / bmp 取决于这台机器上的 vips 是否带对应模块）：" +
				strings.Join(badExt, "、"),
		})
		return
	}

	// 一次请求一个私有目录：输入与输出都在里面，任务 id 之外的路径不可猜。
	// 先清掉过期任务的目录（惰性清理，不额外起 goroutine）。
	s.cleanupExpired()
	jobDir, err := os.MkdirTemp(s.opt.TempRoot, "zp-imgcompress-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "msg": "创建临时目录失败：" + err.Error(),
		})
		return
	}
	inDir := filepath.Join(jobDir, "in")
	if err := os.MkdirAll(inDir, 0o755); err != nil {
		_ = os.RemoveAll(jobDir)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "msg": "创建临时目录失败：" + err.Error(),
		})
		return
	}

	targets := make([]string, 0, len(fileHeaders))
	for i, fh := range fileHeaders {
		base := safeBaseName(fh.Filename)
		if base == "" {
			_ = os.RemoveAll(jobDir)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "msg": fmt.Sprintf("第 %d 个文件的文件名不可用", i+1),
			})
			return
		}
		dst := filepath.Join(inDir, fmt.Sprintf("%03d-%s", i+1, base))
		if err := copyUpload(fh, dst); err != nil {
			_ = os.RemoveAll(jobDir)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "msg": "保存上传文件失败：" + err.Error(),
			})
			return
		}
		targets = append(targets, dst)
	}

	job := &imgCompressJob{
		dir:     jobDir,
		created: time.Now(),
		outputs: map[int]imgCompressOutput{},
	}
	title := fmt.Sprintf("压缩 %d 张图片（%s）", len(targets), humanBytes(total))
	t := s.tasks.Start("image_compress", jobDir, title, func(ctx context.Context, log tasks.LogFunc) (any, error) {
		return s.runCompressJob(ctx, job, eng, targets, opt, log)
	})
	s.mu.Lock()
	job.id = t.ID()
	s.jobs[t.ID()] = job
	s.mu.Unlock()

	// 秒级单文件：等它跑完直接给结果（前端无需订阅 SSE）。
	if len(targets) == 1 && total <= s.opt.SyncMaxBytes {
		select {
		case <-t.Done():
		case <-r.Context().Done():
			// 客户端走了，任务仍在跑（已经交给任务管理器），下次可从任务接口取回。
		}
		meta := t.Meta()
		if meta.Status == tasks.StatusFailed {
			// 同步路径没有任务窗口可看日志，必须把**第一张的真实失败原因**直接带回来，
			// 否则用户看到的只是"全部失败了，原因见日志"而根本没有日志可看。
			msg := firstNonEmptyStr(meta.Error, "压缩失败")
			if r, ok := meta.Result.(*imgCompressResult); ok {
				for _, it := range r.Items {
					if strings.TrimSpace(it.Error) != "" {
						msg += "：" + it.Error
						break
					}
				}
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "msg": msg,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true,
			"data": map[string]any{
				"task_id": t.ID(),
				"sync":    true,
				"result":  meta.Result,
			},
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true,
		"data": map[string]any{
			"task_id": t.ID(),
			"sync":    false,
			"total":   len(targets),
			"title":   title,
		},
	})
}

// copyUpload 把上传流落盘（0600：里面可能是用户的私人照片）。
func copyUpload(fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, src)
	return err
}

// safeBaseName 取出可安全落盘的文件名（去掉任何路径成分与怪异字符）。
func safeBaseName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." || base == "/" {
		return ""
	}
	// 保证扩展名判定不被前缀破坏：只保留字母数字与 . _ - 空格（中文名也允许）。
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-' || r == ' ':
			return r
		case r > 127:
			return r
		}
		return '_'
	}, base)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return ""
	}
	return cleaned
}

// imgCompressOptionsFromForm 读表单里的压缩选项（沿用 internal/imgopt 的语义）。
func imgCompressOptionsFromForm(r *http.Request) (imgopt.Options, error) {
	opt := imgopt.Options{Overwrite: false}
	if v := strings.TrimSpace(r.FormValue("quality")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return opt, fmt.Errorf("质量必须是 1-100 的整数，收到 %q", v)
		}
		opt.Quality = n
	}
	if v := strings.TrimSpace(r.FormValue("max_edge")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return opt, fmt.Errorf("最长边必须是整数（0 = 不缩放），收到 %q", v)
		}
		opt.MaxEdge = n
	}
	if v := strings.TrimSpace(r.FormValue("format")); v != "" {
		opt.Format = imgopt.Format(v)
	}
	switch strings.ToLower(strings.TrimSpace(r.FormValue("strip_metadata"))) {
	case "1", "true", "on", "yes":
		opt.StripMetadata = true
	}
	return opt.Normalize()
}

// runCompressJob 是任务体：并发 2（libvips 自己就是多线程的，再叠高并发只会抢内存）。
func (s *ImgCompressServer) runCompressJob(ctx context.Context, job *imgCompressJob,
	eng imgopt.Engine, targets []string, opt imgopt.Options, log tasks.LogFunc) (any, error) {

	res := &imgCompressResult{
		Total: len(targets), Format: string(opt.Format), Quality: opt.Quality,
		MaxEdge: opt.MaxEdge, Strip: opt.StripMetadata,
		Items: make([]imgCompressItem, 0, len(targets)),
	}
	stripText := "保留"
	if opt.StripMetadata {
		stripText = "移除"
	}
	log(tasks.LevelStep, fmt.Sprintf("共 %d 个文件：质量 %d，格式 %s，最长边 %s，元数据 %s",
		len(targets), opt.Quality, opt.Format, maxEdgeText(opt.MaxEdge), stripText))

	type jobItem struct {
		idx  int
		path string
	}
	type outcome struct {
		idx int
		res imgopt.Result
		err error
	}
	jobs := make(chan jobItem)
	outs := make(chan outcome)
	workers := 2
	if len(targets) < workers {
		workers = len(targets)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				rr, err := eng.CompressFile(ctx, j.path, opt)
				outs <- outcome{idx: j.idx, res: rr, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i, p := range targets {
			select {
			case <-ctx.Done():
				return
			case jobs <- jobItem{idx: i, path: p}:
			}
		}
	}()
	go func() { wg.Wait(); close(outs) }()

	items := make([]imgCompressItem, len(targets))
	for oc := range outs {
		// 去掉落盘前缀（003-xxx.jpg → xxx.jpg），错误/日志里显示用户认识的名字。
		display := stripUploadPrefix(filepath.Base(targets[oc.idx]))
		it := imgCompressItem{Index: oc.idx, Name: display, Before: oc.res.Before, After: oc.res.After}
		switch {
		case oc.err != nil:
			it.Error = oc.err.Error()
			res.Failed++
			log(tasks.LevelErr, "✗ "+display+"："+oc.err.Error())
		case oc.res.Skipped:
			it.Skipped = true
			res.Skipped++
			res.BeforeBytes += oc.res.Before
			log(tasks.LevelWarn, "↷ "+display+"：压完反而更大（"+
				humanBytes(oc.res.Before)+" → "+humanBytes(oc.res.After)+"），已保留原文件")
		default:
			it.Saved = oc.res.Saved()
			it.Dst = stripUploadPrefix(filepath.Base(oc.res.Dst))
			res.Done++
			res.BeforeBytes += oc.res.Before
			res.AfterBytes += oc.res.After
			res.SavedBytes += oc.res.Saved()
			job.mu.Lock()
			job.outputs[oc.idx] = imgCompressOutput{Path: oc.res.Dst, Name: it.Dst}
			job.mu.Unlock()
			log(tasks.LevelOK, "✓ "+display+"："+humanBytes(oc.res.Before)+" → "+
				humanBytes(oc.res.After)+"（省 "+humanBytes(oc.res.Saved())+"）")
		}
		items[oc.idx] = it
	}
	res.Items = items
	job.mu.Lock()
	job.result = res
	job.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("任务被取消（已完成 %d/%d）", res.Done+res.Failed+res.Skipped, res.Total)
	}
	log(tasks.LevelStep, fmt.Sprintf("完成：成功 %d，跳过 %d（压完更大），失败 %d；共省 %s",
		res.Done, res.Skipped, res.Failed, humanBytes(res.SavedBytes)))
	if res.Failed == res.Total && res.Total > 0 {
		return res, fmt.Errorf("全部 %d 张都失败了，第一张的原因见日志", res.Failed)
	}
	return res, nil
}

// stripUploadPrefix 去掉落盘时加的 `%03d-` 前缀。
func stripUploadPrefix(name string) string {
	if len(name) > 4 && name[3] == '-' {
		return name[4:]
	}
	return name
}

func maxEdgeText(n int) string {
	if n <= 0 {
		return "不缩放"
	}
	return strconv.Itoa(n) + "px"
}

// ---------------------------------------------------------------------------
//  任务查询 / 进度 / 下载
// ---------------------------------------------------------------------------

func (s *ImgCompressServer) job(id string) *imgCompressJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *ImgCompressServer) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	t := s.tasks.Get(r.PathValue("id"))
	if t == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "任务不存在（服务重启后不再保留历史任务）",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"data": map[string]any{
			"task": t.Meta(),
			"done": t.Status() != tasks.StatusRunning,
		},
	})
}

func (s *ImgCompressServer) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	t, err := s.tasks.Cancel(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": t.Meta()})
}

// handleTaskStream 是任务的 SSE 进度流，契约与面板任务中心一致
// （事件 meta / lines / done，支持 Last-Event-ID 断点续传）。
func (s *ImgCompressServer) handleTaskStream(w http.ResponseWriter, r *http.Request) {
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
	// 任务已经结束：把缓冲里剩下的行补齐，再补一条 done。
	if t.Status() != tasks.StatusRunning {
		s.flushRemaining(t, send, &cursor)
		send("done", map[string]any{"task": t.Meta()}, cursor)
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
			send("done", map[string]any{"task": t.Meta()}, cursor)
			return
		}
	}
}

// flushRemaining 把任务缓冲里 cursor 之后的日志行补齐（任务刚好结束时用）。
func (s *ImgCompressServer) flushRemaining(t *tasks.Task, send func(string, any, int64) bool, cursor *int64) {
	rest, next, _, _ := t.Snapshot(*cursor, 4000)
	if len(rest) == 0 {
		return
	}
	*cursor = next
	send("lines", map[string]any{"lines": rest}, *cursor)
}

func (s *ImgCompressServer) handleTaskFile(w http.ResponseWriter, r *http.Request) {
	job := s.job(r.PathValue("id"))
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "任务不存在或输出已清理",
		})
		return
	}
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "文件索引不合法"})
		return
	}
	job.mu.Lock()
	out, ok := job.outputs[idx]
	job.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "这个文件没有可下载的输出（压缩失败或被跳过：压完反而更大）",
		})
		return
	}
	if _, err := os.Stat(out.Path); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "输出文件已不存在（临时目录被清理了）",
		})
		return
	}
	w.Header().Set("Content-Disposition", contentDisposition(out.Name))
	http.ServeFile(w, r, out.Path)
}

func (s *ImgCompressServer) handleTaskZip(w http.ResponseWriter, r *http.Request) {
	job := s.job(r.PathValue("id"))
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "任务不存在或输出已清理",
		})
		return
	}
	job.mu.Lock()
	outs := make([]imgCompressOutput, 0, len(job.outputs))
	for _, o := range job.outputs {
		outs = append(outs, o)
	}
	job.mu.Unlock()
	if len(outs) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "这个任务没有任何可打包的输出",
		})
		return
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].Name < outs[j].Name })
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("imgcompress-"+job.id+".zip"))
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()
	seen := map[string]int{}
	for _, o := range outs {
		f, err := os.Open(o.Path)
		if err != nil {
			continue
		}
		name := o.Name
		if n := seen[name]; n > 0 {
			name = fmt.Sprintf("%d-%s", n+1, name)
		}
		seen[o.Name]++
		w2, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err == nil {
			_, _ = io.Copy(w2, f)
		}
		_ = f.Close()
	}
}

// cleanupExpired 惰性清理过期任务的临时目录与内存记录。
//
// 判据是 TempRoot 下的目录名 + 修改时间：正在跑的任务目录 mtime 一直在变，
// 不会被误删；长任务因此安全。
func (s *ImgCompressServer) cleanupExpired() {
	now := time.Now()
	s.mu.Lock()
	for id, job := range s.jobs {
		if now.Sub(job.created) > s.opt.JobTTL {
			delete(s.jobs, id)
			// 目录统一由下面的扫描处理（按 mtime，避免删掉正在写的目录）。
		}
	}
	s.mu.Unlock()
	entries, err := os.ReadDir(s.opt.TempRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "zp-imgcompress-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > s.opt.JobTTL {
			_ = os.RemoveAll(filepath.Join(s.opt.TempRoot, e.Name()))
		}
	}
}

// ---------------------------------------------------------------------------
//  小工具
// ---------------------------------------------------------------------------

// contentDisposition 生成同时给 ASCII 回退与 UTF-8 文件名的响应头，
// 中文文件名（用户的照片常常是中文名）才不会在下载时变成乱码或坏头。
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' || r == '/' {
			return '_'
		}
		return r
	}, name)
	if strings.TrimSpace(ascii) == "" {
		ascii = "file"
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", ascii, url.PathEscape(name))
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
