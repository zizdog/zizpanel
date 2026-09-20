package api

import (
	"net/http"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/zizdog/zizvideo/internal/domain"
)

// minFreeBytes is the documented readiness floor for the data volume.
const minFreeBytes = 100 << 20

// HandleHealthz checks the process only.
func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// HandleReadyz reports real readiness: DB, ffmpeg/ffprobe, allow roots, disk.
// Missing tools make this endpoint NOT ready — never a fake 200.
func (s *Server) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]any{}
	ok := true

	dbErr := s.DB.Ping()
	checks["db"] = checkResult(dbErr == nil, errText(dbErr))
	if dbErr != nil {
		ok = false
	}
	if _, err := s.DB.SchemaVersion(); err != nil {
		checks["db"] = checkResult(false, "schema unreadable")
		ok = false
	}

	caps := s.Capabilities()
	checks["ffmpeg"] = map[string]any{"ok": caps.FFmpegOK, "path": caps.FFmpegPath,
		"version": caps.FFmpegVersion, "error": toolError(caps.FFmpegOK, caps.Error)}
	checks["ffprobe"] = map[string]any{"ok": caps.FFprobeOK, "path": caps.FFprobePath,
		"version": caps.FFprobeVersion, "error": toolError(caps.FFprobeOK, caps.Error)}
	if !caps.FFmpegOK || !caps.FFprobeOK {
		ok = false
	}

	roots := map[string]any{}
	for _, root := range s.Roots.List() {
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			roots[root] = "不存在"
			ok = false
			continue
		}
		roots[root] = "ok"
	}
	checks["media_roots"] = roots

	free, total, err := diskUsage(s.Cfg.DataDir)
	if err != nil {
		checks["disk"] = map[string]any{"ok": false, "error": err.Error()}
		ok = false
	} else {
		checks["disk"] = map[string]any{"ok": free >= minFreeBytes,
			"free_bytes": free, "total_bytes": total, "data_dir": s.Cfg.DataDir}
		if free < minFreeBytes {
			ok = false
		}
	}

	status := http.StatusOK
	label := "ready"
	if !ok {
		status = http.StatusServiceUnavailable
		label = "not_ready"
	}
	writeJSON(w, status, envelope{Data: map[string]any{"status": label, "checks": checks}})
}

func toolError(ok bool, msg string) string {
	if ok {
		return ""
	}
	return msg
}

func checkResult(ok bool, reason string) map[string]any {
	m := map[string]any{"ok": ok}
	if !ok && reason != "" {
		m["reason"] = reason
	}
	return m
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func diskUsage(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

// HandleSystemInfo exposes versions, capabilities, disk and counters to admins.
func (s *Server) HandleSystemInfo(w http.ResponseWriter, r *http.Request) {
	caps := s.Capabilities()
	free, total, derr := diskUsage(s.Cfg.DataDir)
	libs, _ := s.DB.ListLibraries()
	mediaCount, _ := s.DB.CountMedia("")
	failedCount, _ := s.DB.CountFailedMedia("")
	disk := map[string]any{"data_dir": s.Cfg.DataDir, "free_bytes": free, "total_bytes": total}
	if derr != nil {
		disk["error"] = derr.Error()
	}
	respond(w, http.StatusOK, map[string]any{
		"version":    Version,
		"go_version": runtime.Version(),
		"uptime_s":   int64(time.Since(s.StartedAt).Seconds()),
		"listen":     s.Cfg.Listen,
		"data_dir":   s.Cfg.DataDir,
		"ffmpeg": map[string]any{"path": caps.FFmpegPath, "version": caps.FFmpegVersion,
			"ok": caps.FFmpegOK, "videotoolbox_h264": caps.VideoToolboxH264},
		"ffprobe": map[string]any{"path": caps.FFprobePath, "version": caps.FFprobeVersion,
			"ok": caps.FFprobeOK},
		"tools_error":    caps.Error,
		"media_roots":    s.Roots.List(),
		"scan_workers":   s.Cfg.ScanWorkers,
		"transcode":      map[string]any{"enabled": false, "reason": "Phase2"},
		"disk":           disk,
		"counts":         map[string]any{"libraries": len(libs), "media": mediaCount, "failed": failedCount},
		"schema_version": schemaVersionOrZero(s),
	}, nil)
}

func schemaVersionOrZero(s *Server) int {
	v, err := s.DB.SchemaVersion()
	if err != nil {
		return 0
	}
	return v
}

// HandleAuditList returns recent write operations.
func (s *Server) HandleAuditList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListAudit(queryInt(r, "limit", 100, 1, 500))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"list": rows}, nil)
}

var _ = domain.ErrInternal
