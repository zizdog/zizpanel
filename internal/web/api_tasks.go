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

// marketAffectingTask 判断一类任务结束后要不要让市场缓存失效。
//
// 判据是"这个动作可能改变 Homebrew 里装了/卸了什么"（那正是市场缓存里唯一会过期的
// 证据），而不是任务名好看：
//   - install     —— 应用市场安装 / 一键 LNMP / 基础环境 / Docker 运行时都在这一类；
//   - uninstall   —— 应用市场卸载；
//   - site-install—— 一键建站会按需补齐站点赖以运行的组件。
//
// 刻意**不**做成"所有任务都失效"：每次失效都会让下一次打开市场走一次同步
// brew 探测（约 1.5 秒）。文件压缩、数据库导入、容器创建、部署 compose 这些任务
// 都不改 Homebrew 的包集合（它们的"装了没有"来自服务记录，而服务记录不进缓存），
// 让它们背这份等待没有意义。
func marketAffectingTask(kind string) bool {
	switch kind {
	case "install", "uninstall", "site-install":
		return true
	}
	return false
}

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
	// 用 StartWithTask：安装流程要能"限时询问用户输入"（如 MySQL root 口令），
	// 而输入通道就是任务对象本身（*tasks.Task 满足 services.InputProvider）。
	t := s.Tasks.StartWithTask(kind, target, title, func(ctx context.Context, t *tasks.Task) (any, error) {
		// 业务代码不依赖任务中心的内部结构，进度与"限时询问"都通过 ctx 传进
		// services 层。
		ctx = services.WithProgress(ctx, t.LogFunc())
		ctx = services.WithInput(ctx, t)
		res, err := run(ctx, t.LogFunc())
		if err != nil {
			s.auditAs(info, auditAction, target, "失败: "+err.Error(), false, "")
		} else {
			s.auditAs(info, auditAction, target, summarizeResult(res), true, "")
		}
		// 任务收尾顺手核对一次「有 nginx、但 80 端口上还没有默认站点」+ 环境自愈。
		//
		// 为什么放在这个**中心位置**：nginx/PHP 可能是任务跑起来之后才装上的
		//（一键 LNMP / 应用市场单装 nginx / 重装 nginx），而面板启动时那一次检查
		// 早就过去了。放在每个安装路径里逐个加钩子，漏一个就是又一轮
		// "装完没有默认网站"（用户报障）。失败的任务也照查：
		// LNMP 可能装好了 nginx、只是后面的 MySQL 步骤超时。
		//
		// 为什么要**后台**跑（第一版写成同步，立刻踩到）：任务中心的"结束"以这个
		// 闭包返回为准，而自愈可能因为提权助手/探测而花上几秒到几十秒 ——
		// 同步做会让任务看起来一直没结束，期间同一目标再点一次就是 409
		//（单测 TestInstallLNMPEmptyBodyStillAccepted 当场变红，真机上就是
		// "明明装完了，再点一次说它还在跑"）。自愈本来就是幂等的后台维护，
		// 不该占着任务的生命周期。
		// 安装/卸载结束必须让市场缓存失效 —— 否则这条任务改变了机器上装了什么，
		// 而「已安装」是 5 分钟 TTL 的缓存结论：用户装完刷新页面，卡片仍停在
		// 「安装」、也不进「已安装」（用户报障的根因之一，
		// 见 Server.InvalidateMarketCache）。
		if marketAffectingTask(kind) {
			s.InvalidateMarketCache()
		}
		s.kickEnvHeal()
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
		// 用户可见的任务标题：不要出现"纳管"这种内部词（用户 2026-09-19 要求
		// 弱化纳管概念——那是面板要做的事，不是用户要理解的词）。
		return "登记服务 " + v.DisplayName
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

// handleTaskInput 接收用户在任务里提交的输入（目前只有 MySQL root 口令）。
//
// 安全约定（硬要求）：
//   - 值只交给正在等它的那个任务，**不回显、不写审计、不进日志**；
//   - 审计只记录"提交了哪个 key"，因为审计是长期保存且广可见的；
//   - 只有确实在等这个 key 的任务才收，晚到的/多余的提交明确报错（见 SubmitInput）。
func (s *Server) handleTaskInput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		fail(w, http.StatusBadRequest, "缺少 key（要回答的是哪一个输入）")
		return
	}
	t := s.Tasks.Get(r.PathValue("id"))
	if t == nil {
		fail(w, http.StatusNotFound, "任务不存在（面板重启后不再保留历史任务）")
		return
	}
	if err := t.SubmitInput(key, req.Value); err != nil {
		// 409：任务在，但此刻不接受这个输入。如实说明原因，绝不静默吞掉 ——
		// 用户会以为"我填了"，而任务其实已经用默认值继续了。
		fail(w, http.StatusConflict, err.Error())
		return
	}
	s.audit(r, "task_input", t.Meta().Target, "提交任务输入 "+key, true, "")
	// 响应里不含任何值：不回显是这一段的硬要求。
	ok(w, map[string]any{"accepted": true, "key": key})
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
	// 状态订阅（等输入的开始/落定）与日志是两条通道：日志有序号可续传，
	// 状态没有序号，混在一起会让 Last-Event-ID 的语义变含糊。
	stateID, stateCh := t.SubscribeState()
	defer t.UnsubscribeState(stateID)

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

		case _, open := <-stateCh:
			if !open {
				return
			}
			// 先把攒着的日志发出去：输入请求与它前面那句提示文案（Level=Input）
			// 的顺序不能颠倒，否则前端会先看到输入框、再看到"请在 60 秒内…"。
			if !flush() {
				return
			}
			// input_required 为 null = 这次等待已经落定（提交/超时/任务被中断），
			// 前端据此收起输入框；input_result 说明是哪种结局。
			if !send("input_required", map[string]any{
				"input_required": t.PendingInput(),
				"input_result":   t.InputResult(),
			}, cursor) {
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
