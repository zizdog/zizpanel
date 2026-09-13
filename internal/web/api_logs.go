package web

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/logs"
)

// ============================================================================
//  日志中心
//
//  与其它模块相比，这里有一个特殊约束：日志里可能含敏感信息
//  （访问日志的 URL 参数、错误日志的堆栈、配置泄露等）。
//  因此：
//    - 日志 key 由服务端映射到真实路径，前端不接触绝对路径做拼接
//    - 清空需要显式确认（并自动保留尾部备份）
//    - 删除只允许面板自己产生的日志
// ============================================================================

// logCatalog 惰性构造日志目录。
func (s *Server) logCatalog() *logs.Catalog {
	s.logOnce.Do(func() {
		// 纳管服务的日志也纳入进来：它们的路径来自服务注册表
		var svcLogs []logs.Entry
		if list, err := s.serviceRepo.List(context.Background()); err == nil {
			for _, svc := range list {
				if svc.LogPath == "" {
					continue
				}
				svcLogs = append(svcLogs, logs.Entry{
					Key:         "service:" + svc.Name,
					Name:        svc.DisplayName + " · 运行日志",
					Category:    "service",
					Path:        expandHomePath(svc.LogPath, s.Cfg.UserHome),
					Description: fmt.Sprintf("纳管服务 %s 的输出日志", svc.Name),
				})
			}
		}
		s.logCat = logs.NewCatalog(logs.Options{
			SiteLogDir:  filepath.Join(s.Cfg.WWWRoot, "_logs"),
			PanelLogDir: s.Cfg.LogDir,
			CronLogDir:  filepath.Join(s.Cfg.LogDir, "cron"),
			BrewPrefix:  s.Cfg.BrewPrefix,
			ServiceLogs: svcLogs,
		})
	})
	return s.logCat
}

func expandHomePath(p, home string) string {
	if strings.HasPrefix(p, "~/") && home != "" {
		return filepath.Join(home, p[2:])
	}
	return p
}

// handleLogList 列出所有日志。
func (s *Server) handleLogList(w http.ResponseWriter, r *http.Request) {
	cat := s.logCatalog()
	ok(w, map[string]any{
		"list":       cat.List(),
		"categories": logs.Categories(),
		"stats":      cat.Stats(),
	})
}

// handleLogRead 读取日志内容。
func (s *Server) handleLogRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opt := logs.ReadOptions{
		Filter:  q.Get("filter"),
		Level:   q.Get("level"),
		IsRegex: q.Get("regex") == "1",
	}
	if v := q.Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opt.Lines = n
		}
	}
	cat := s.logCatalog()
	res, err := cat.Read(q.Get("key"), opt)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, res)
}

// handleLogStream 用 SSE 实时尾随日志。
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	flusher, okf := w.(http.Flusher)
	if !okf {
		fail(w, http.StatusInternalServerError, "当前服务不支持流式响应")
		return
	}
	tail := 50
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			tail = n
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cat := s.logCatalog()
	ch, err := cat.Follow(ctx, key, tail)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: open\ndata: {}\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case chunk, alive := <-ch:
			if !alive {
				fmt.Fprint(w, "event: close\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			if chunk == "" {
				continue
			}
			// 按行拆分，符合 SSE 规范（避免单行超长）
			for _, line := range strings.Split(strings.TrimRight(chunk, "\n"), "\n") {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			fmt.Fprint(w, "\n")
			flusher.Flush()
		}
	}
}

// handleLogTruncate 清空日志（保留尾部备份）。
func (s *Server) handleLogTruncate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	cat := s.logCatalog()
	msg, err := cat.Truncate(req.Key)
	if err != nil {
		s.audit(r, "log_truncate", req.Key, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "log_truncate", req.Key, msg, true, "")
	ok(w, map[string]any{"msg": msg})
}

// handleLogDelete 删除面板自身的日志文件。
func (s *Server) handleLogDelete(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	cat := s.logCatalog()
	if err := cat.Delete(key); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "log_delete", key, "删除日志文件", true, "")
	ok(w, map[string]any{"msg": "日志已删除"})
}

// handleLogDownload 下载日志文件（便于发给他人排查）。
func (s *Server) handleLogDownload(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	cat := s.logCatalog()
	res, err := cat.Read(key, logs.ReadOptions{Lines: 5000, MaxBytes: 32 << 20})
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	name := "log.txt"
	if i := strings.LastIndex(key, ":"); i >= 0 {
		name = key[i+1:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+asciiFallback(name)+`"`)
	_, _ = w.Write([]byte(strings.Join(res.Lines, "\n")))
	s.audit(r, "log_download", key, fmt.Sprintf("下载 %d 行", len(res.Lines)), true, "")
}
