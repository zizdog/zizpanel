package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  任务中心（SPEC-任务中心.md）
//
//  安装/卸载这类动辄几分钟到十几分钟的操作，过去是**同步 HTTP 请求**：
//  brew / docker 的输出被 CombinedOutput() 攒着，用户在整段时间里只能看到
//  "请等待"，而且任务挂在 r.Context() 上 —— 一刷新就把子进程杀了。
//
//  现在：POST 立刻返回 task_id（202），真正的活在后台 goroutine 里跑，
//  进度通过 SSE 推给任意数量/任意时刻加入的观察者。
// ============================================================================

// auditInfo 是审计所需的请求信息，在请求还活着时抓下来。
//
// 为什么需要：s.audit(r, ...) 用的是 r.Context()，而任务结束时请求早就返回了
// （context canceled），那时候再拿 r 去写审计会静默失败。
type auditInfo struct {
	actor string
	ip    string
}

// captureAudit 抓取审计信息（必须在请求处理期间调用）。
func (s *Server) captureAudit(r *http.Request) auditInfo {
	actor := ""
	if u := userFrom(r.Context()); u != nil {
		actor = u.Username
	}
	return auditInfo{actor: actor, ip: s.clientIP(r)}
}

// auditAs 用抓好的信息写审计（可在后台 goroutine 里安全调用）。
func (s *Server) auditAs(info auditInfo, action, target, detail string, success bool, msg string) {
	okInt := 0
	if success {
		okInt = 1
	}
	// 用独立的 context：这个写入发生在 HTTP 请求结束之后。
	if _, err := s.Store.DB().ExecContext(context.Background(),
		`INSERT INTO audit_logs(actor,ip,action,target,detail,ok,message) VALUES(?,?,?,?,?,?,?)`,
		info.actor, info.ip, action, target, detail, okInt, msg); err != nil {
		s.Log.Warn("写审计日志失败: %v", err)
	}
}

// taskRunner 是任务体：拿 ctx 与进度接收器，返回业务结果。
type taskRunner func(ctx context.Context, log tasks.LogFunc) (any, error)

// launchTask 把一个长任务交给任务中心，立刻返回 task_id。
//
// 同一个 target 上已有运行中的任务时返回 409：brew 自己有全局锁，
// 重复点两下"安装"只会让后一个任务卡住并最终失败，不如当场说清楚。
func (s *Server) launchTask(w http.ResponseWriter, r *http.Request,
	kind, target, title, auditAction string, run taskRunner) {

	if t := s.Tasks.RunningFor(target); t != nil {
		fail(w, http.StatusConflict, "「"+t.Meta().Title+"」正在进行中，请等它结束或在任务中心里中断")
		return
	}

	info := s.captureAudit(r)
	t := s.Tasks.Start(kind, target, title, func(ctx context.Context, log tasks.LogFunc) (any, error) {
		// 业务代码不依赖 tasks 包，进度通过 ctx 传进 services 层
		ctx = services.WithProgress(ctx, log)
		res, err := run(ctx, log)
		if err != nil {
			s.auditAs(info, auditAction, target, "失败: "+err.Error(), false, "")
		} else {
			s.auditAs(info, auditAction, target, summarizeResult(res), true, "")
		}
		return res, err
	})
	s.auditAs(info, "task_start", target, title+"（任务 "+t.ID()+"）", true, "")

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":   true,
		"data": map[string]any{"task_id": t.ID(), "title": title},
	})
}

// summarizeResult 把业务结果压成一行审计描述。
// 与改造前的 strings.Join(res.Steps, " | ") 保持一致，历史审计的形态不变。
func summarizeResult(res any) string {
	switch v := res.(type) {
	case *services.InstallResult:
		if v == nil {
			return "完成"
		}
		if len(v.Steps) == 0 {
			return "完成"
		}
		return strings.Join(v.Steps, " | ")
	case *services.Service:
		if v == nil {
			return "完成"
		}
		return "已纳管/登记服务 " + v.DisplayName
	default:
		return "完成"
	}
}

// ---------- 任务查询 ----------

// handleTasksList 返回最近任务（新的在前）。
func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	list := s.Tasks.List()
	ok(w, map[string]any{
		"tasks":   list,
		"running": s.Tasks.RunningCount(),
	})
}

// handleTaskGet 返回任务元信息 + 日志（用于"关掉窗口再打开"）。
//
// 支持 after=<seq> 增量拉取：进度窗先拉一次全量，之后靠 SSE 续。
func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	t := s.Tasks.Get(r.PathValue("id"))
	if t == nil {
		fail(w, http.StatusNotFound, "任务不存在（面板重启后不再保留历史任务）")
		return
	}
	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			after = n
		}
	}
	limit := 800
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 4000 {
			limit = n
		}
	}
	lines, next, hasMore, oldest := t.Snapshot(after, limit)
	ok(w, map[string]any{
		"task":       t.Meta(),
		"lines":      lines,
		"next_after": next,
		"has_more":   hasMore,
		// oldest_seq 如实告诉前端：更早的行已经滚出缓冲，日志不是完整的
		"oldest_seq": oldest,
		"done":       t.Status() != tasks.StatusRunning,
	})
}

// handleTaskCancel 中断任务。
func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.Tasks.Cancel(id)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "task_cancel", t.Meta().Target, "中断任务 "+t.Meta().Title, true, "")
	ok(w, map[string]any{"task": t.Meta()})
}

// handleTaskStream 是任务的 SSE 进度流。
//
// 断点续传：EventSource 重连时会自动带上 Last-Event-ID 头，这里据此从该 seq
// 之后续发；订阅被关闭（消费者跟不上）时前端也会重连，所以"丢行"不会变成
// "卡住安装"。事件形状见 SPEC-任务中心.md。
func (s *Server) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	flusher, okf := w.(http.Flusher)
	if !okf {
		fail(w, http.StatusInternalServerError, "当前服务不支持流式响应")
		return
	}
	t := s.Tasks.Get(r.PathValue("id"))
	if t == nil {
		fail(w, http.StatusNotFound, "任务不存在（面板重启后不再保留历史任务）")
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

	// 顺序很重要：**先订阅、再取快照**。反过来的话，快照与订阅之间产生的行
	// 会丢。重复的行由下面的 cursor 去重（seq <= cursor 的直接跳过）。
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

	if !send("meta", map[string]any{
		"task":       t.Meta(),
		"oldest_seq": oldest,
	}, cursor) {
		return
	}
	if len(lines) > 0 {
		if !send("lines", map[string]any{"lines": lines}, cursor) {
			return
		}
	}

	// 小批量聚合：brew/docker 的输出可能每秒几十行，一行一个事件太碎；
	// 但也不能攒太久（否则"实时"就没意义了）。100 行或 80ms 先到者发。
	const (
		batchMax  = 100
		batchWait = 80 * time.Millisecond
	)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	flushTimer := time.NewTimer(batchWait)
	if !flushTimer.Stop() {
		<-flushTimer.C
	}
	defer flushTimer.Stop()

	batch := make([]tasks.Line, 0, batchMax)
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		if !send("lines", map[string]any{"lines": batch}, cursor) {
			return false
		}
		batch = batch[:0]
		return true
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case line, open := <-ch:
			if !open {
				// 订阅被服务端关闭：消费者跟不上（通道满）。直接断开，
				// 让 EventSource 带着 Last-Event-ID 重连，从快照补齐。
				return
			}
			if line.Seq <= cursor {
				continue // 订阅早于快照造成的小重复
			}
			cursor = line.Seq
			batch = append(batch, line)
			if len(batch) >= batchMax {
				if !flush() {
					return
				}
				if !flushTimer.Stop() {
					select {
					case <-flushTimer.C:
					default:
					}
				}
				flushTimer.Reset(batchWait)
			} else if len(batch) == 1 {
				flushTimer.Reset(batchWait)
			}

		case <-flushTimer.C:
			if !flush() {
				return
			}

		case <-t.Done():
			// 任务结束：把通道里剩下的行取干净（不能让最后几行丢掉），
			// 再发一次状态，然后正常关闭。
			for {
				select {
				case line, open := <-ch:
					if !open {
						goto drainDone
					}
					if line.Seq > cursor {
						cursor = line.Seq
						batch = append(batch, line)
					}
				default:
					goto drainDone
				}
			}
		drainDone:
			if !flush() {
				return
			}
			send("status", t.Meta(), cursor)
			return

		case <-ticker.C:
			// SSE 注释心跳：中间有代理时防止长连接被判定为空闲而掐断
			if _, err := w.Write([]byte(": hb\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSE 写一个 SSE 事件。id 用于前端的 Last-Event-ID 续传。
func writeSSE(w http.ResponseWriter, event string, data any, id int64) bool {
	if id > 0 {
		if _, err := w.Write([]byte("id: " + strconv.FormatInt(id, 10) + "\n")); err != nil {
			return false
		}
	}
	if _, err := w.Write([]byte("event: " + event + "\n")); err != nil {
		return false
	}
	b, err := jsonMarshalNoEscape(data)
	if err != nil {
		return false
	}
	if _, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n')); err != nil {
		return false
	}
	return true
}

// jsonMarshalNoEscape 序列化 JSON 但**不转义 HTML 字符**。
//
// 任务日志里全是 URL 与命令（`<`、`>`、`&`），默认转义成 \u003c 之后
// 人看着难受、抓包排障也不方便；这些数据只经 JSON.parse 进 DOM 文本节点，
// 不参与 HTML 解析，所以不转义是安全的（与 writeJSON 的口径一致）。
func jsonMarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
