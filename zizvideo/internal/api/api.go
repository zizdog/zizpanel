// Package api implements the REST handlers and middleware for /api/v1.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizvideo/internal/auth"
	"github.com/zizdog/zizvideo/internal/autoscan"
	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/ffmpeg"
	"github.com/zizdog/zizvideo/internal/storage"
	"github.com/zizdog/zizvideo/internal/task"
)

// Version is the reported build version; overridable with -ldflags.
var Version = "0.1.0-mvp"

// Server holds every dependency the handlers need.
type Server struct {
	Cfg    *config.Config
	DB     *storage.DB
	Auth   *auth.Manager
	Tasks  *task.Manager
	Roots  *config.Roots
	Runner ffmpeg.Runner
	Log    *slog.Logger

	// Auto 负责定时/事件驱动的增量扫描；StartAutoScan 之前不运行。
	Auto *autoscan.Scheduler

	// Uploads 是三步上传的进程内会话（start/PUT/finish）。
	Uploads *uploadStore

	StartedAt time.Time

	mu   sync.RWMutex
	caps ffmpeg.Capabilities
}

// NewServer wires a Server and probes ffmpeg capabilities once at startup.
// Roots is the single source of truth for media allow roots.
func NewServer(cfg *config.Config, db *storage.DB, a *auth.Manager, t *task.Manager,
	roots *config.Roots, r ffmpeg.Runner, log *slog.Logger) *Server {
	s := &Server{Cfg: cfg, DB: db, Auth: a, Tasks: t, Roots: roots, Runner: r, Log: log,
		Uploads: newUploadStore(), StartedAt: time.Now()}
	// 扫描成功结束后自动补齐识别（不改变 task.Manager 对 api 的依赖方向）。
	if t != nil {
		t.SetAfterScan(s.OnScanFinished)
	}
	// 自动扫描复用 task.Manager.StartScan，扫描结果与后置识别都留在任务中心。
	s.Auto = autoscan.New(db, t, log)
	s.RefreshCapabilities(context.Background())
	return s
}

// StartAutoScan 启动后先扫一轮（覆盖停机期间的变动），再按设置定时/事件触发。
func (s *Server) StartAutoScan(ctx context.Context) {
	if s.Auto != nil {
		s.Auto.Start(ctx)
	}
}

// StopAutoScan stops the timer and the event watcher.
func (s *Server) StopAutoScan() {
	if s.Auto != nil {
		s.Auto.Stop()
	}
}

// AutoScanChanged asks the scheduler to re-read settings and rebuild watches.
func (s *Server) AutoScanChanged() {
	if s.Auto != nil {
		s.Auto.Reload()
	}
}

// RefreshCapabilities re-detects ffmpeg/ffprobe availability.
func (s *Server) RefreshCapabilities(ctx context.Context) {
	caps := ffmpeg.Detect(ctx, s.Runner, s.Cfg.FFmpegBin, s.Cfg.FFprobeBin)
	s.mu.Lock()
	s.caps = caps
	s.mu.Unlock()
	if !caps.FFmpegOK || !caps.FFprobeOK {
		s.Log.Error("外部工具不可用", "ffmpeg_ok", caps.FFmpegOK,
			"ffprobe_ok", caps.FFprobeOK, "error", caps.Error)
	}
}

// Capabilities returns the last probe result.
func (s *Server) Capabilities() ffmpeg.Capabilities {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.caps
}

// ============================================================================
//  Response envelope
// ============================================================================

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	Data  any        `json:"data"`
	Meta  any        `json:"meta"`
	Error *errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

// respond writes a success envelope.
func respond(w http.ResponseWriter, status int, data any, meta any) {
	if meta == nil {
		meta = map[string]any{}
	}
	writeJSON(w, status, envelope{Data: data, Meta: meta})
}

// fail maps a domain error to its HTTP status and writes the error envelope.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var de *domain.Error
	if !errors.As(err, &de) {
		s.Log.Error("请求处理失败", "request_id", RequestID(r.Context()), "error", err.Error())
		de = domain.ErrInternal
	}
	writeJSON(w, de.Status, envelope{
		Meta:  map[string]any{"request_id": RequestID(r.Context())},
		Error: &errorBody{Code: de.Code, Message: de.Message},
	})
}

// decodeJSON reads a size-limited JSON body into dst.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.ErrBadRequest
	}
	return nil
}

func queryInt(r *http.Request, key string, def, minV, maxV int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < minV {
		return minV
	}
	if n > maxV {
		return maxV
	}
	return n
}

// ============================================================================
//  Context helpers
// ============================================================================

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxUser
	ctxClientIP
	ctxScope
)

// RequestID returns the per-request correlation id.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// UserFrom returns the authenticated user, or nil.
func UserFrom(ctx context.Context) *domain.User {
	v, _ := ctx.Value(ctxUser).(*domain.User)
	return v
}

func withUser(ctx context.Context, u *domain.User) context.Context {
	return context.WithValue(ctx, ctxUser, u)
}

func clientIPFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxClientIP).(string)
	return v
}

func withClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ctxClientIP, ip)
}

var _ = runtime.NumCPU
