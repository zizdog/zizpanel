package upgrade

import (
	"fmt"
	"time"
)

// ============================================================================
//  升级过程的**结构化进度**
//
//  为什么要有这个文件（用户报的问题）：
//
//	「从 zizdog.com 下载升级包要很久，页面上没有明显的升级进度，
//	 用户不知道发生了什么就去刷新重试。」
//
//  在此之前，State 里只有一句人读的 `message`（"已下载 12.3 MB / 24.4 MB（50%）"），
//  没有阶段 id、没有字节数、没有速度、没有日志 —— 前端画不出进度条，
//  也没法在用户刷新页面后看出"到底走到哪一步了"。
//
//  两条硬约束写在实现里：
//   1. **进度必须是真实的** —— 字节数只来自下载回调实际写入的字节，
//      阶段只来自真实发生的过程切换。这里没有任何定时器在"编造"百分比。
//   2. **必须落盘** —— 升级要重启面板，用户也随时可能刷新；只放内存等于没有。
//      但下载回调是每 256KB 一次，每次都写盘会把下载本身拖慢，所以要节流。
// ============================================================================

// 升级阶段 id。前端按它决定画"确定进度条"还是"不确定进度条"，
// 中文名一律由 StageLabel 给出 —— 阶段文案不能散落在前端硬编码。
const (
	StageManifest = "manifest" // 获取发布清单
	StageDownload = "download" // 下载安装包
	StageVerify   = "verify"   // 校验 SHA-256
	StageExtract  = "extract"  // 解包
	StageSmoke    = "smoke"    // 试运行新二进制
	StageApply    = "apply"    // 替换二进制
	StageModules  = "modules"  // 刷新已安装的面板托管模块
	StageRestart  = "restart"  // 重启并等待看门狗结论
	StageDone     = "done"     // 完成
	StageFailed   = "failed"   // 失败
)

// maxLogLines 是 State 里保留的日志行数上限。
//
// 必须有上限：State 每次轮询都要序列化并传输，日志无限增长会让
// status 接口越来越慢。200 行足够覆盖一次升级的全过程。
const maxLogLines = 200

// defaultSaveInterval 是进度写盘的节流间隔（下载期间每 256KB 回调一次，
// 不节流等于给下载加了几百次 fsync）。
const defaultSaveInterval = 700 * time.Millisecond

// stageLabels 是阶段 id → 用户可读的中文名。
var stageLabels = map[string]string{
	StageManifest: "获取发布清单",
	StageDownload: "下载安装包",
	StageVerify:   "校验安装包",
	StageExtract:  "解包",
	StageSmoke:    "试运行",
	StageApply:    "替换程序",
	StageModules:  "刷新模块",
	StageRestart:  "重启并验证",
	StageDone:     "完成",
	StageFailed:   "失败",
}

// StageLabel 返回阶段的中文名；未知阶段原样返回（宁可显示一个 id，
// 也不要显示空白 —— 空白会被当成"没在升级"）。
func StageLabel(stage string) string {
	if s, ok := stageLabels[stage]; ok {
		return s
	}
	return stage
}

// LogLine 是一条升级日志。
type LogLine struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"` // info / ok / warn / error
	Text  string    `json:"text"`
}

// Progress 是升级过程的结构化进度。
//
// 所有字段都只增不改语义，且带 omitempty —— 老前端读不到新字段不会报错，
// 新前端遇到老状态（没有 progress）也只是不画进度条，而不是崩掉。
type Progress struct {
	// Stage 是阶段 id（见 StageManifest…），StageLabel 是它的中文名。
	Stage      string `json:"stage"`
	StageLabel string `json:"stage_label"`
	// DownloadedBytes / TotalBytes 是**真实**字节数。
	// TotalBytes = 0 表示"真的不知道总大小"（既没有清单 size 也没有 Content-Length），
	// 此时 Percent = -1，前端应画不确定进度条而不是编一个假百分比。
	DownloadedBytes int64   `json:"downloaded_bytes"`
	TotalBytes      int64   `json:"total_bytes"`
	Percent         float64 `json:"percent"` // 0..100；-1 = 未知
	// BytesPerSecond 是**实测平均速度**（真实字节 ÷ 真实耗时），不是估算出来的。
	BytesPerSecond float64   `json:"bytes_per_second"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// StateWriter 负责把一次升级的真实进展累积进 State 并落盘。
//
// 它是升级流程唯一的"写状态"入口：阶段切换、真实字节进度、日志行都经过它，
// 这样节流、日志上限、百分比计算只有一份实现，不会同一件事写出两套口径。
type StateWriter struct {
	workDir string
	st      *State

	// now 可注入，便于单测在"没有真实等待"的情况下验证速度计算。
	now func() time.Time
	// saveInterval 是节流间隔；<=0 表示每次调用都立即落盘（单测用）。
	saveInterval time.Duration

	lastSave time.Time
	speedAt  time.Time // 下载速度的计时起点（第一次收到真实字节时设置）
}

// NewStateWriter 为一次升级创建写入器。
//
// st 为 nil 时会新建一个 idle 状态；Phase 缺省字段在这里补齐，
// 调用方不需要（也不应该）自己拼 Progress。
func NewStateWriter(workDir string, st *State) *StateWriter {
	if st == nil {
		st = &State{Status: StatusIdle}
	}
	now := time.Now()
	if st.Progress == nil {
		// 阶段留空，由调用方紧接着用 Stage() 设置 —— 这样第一句"▶ 获取发布清单"
		// 也会进日志（如果这里预置成 manifest，Stage(manifest) 会被当成重复而静默）。
		st.Progress = &Progress{Percent: -1}
	}
	if st.Progress.StartedAt.IsZero() {
		st.Progress.StartedAt = now
	}
	st.Progress.StageLabel = StageLabel(st.Progress.Stage)
	st.Progress.UpdatedAt = now
	return &StateWriter{
		workDir:      workDir,
		st:           st,
		now:          time.Now,
		saveInterval: defaultSaveInterval,
	}
}

// State 返回底层状态指针（调用方只读或做兼容字段的赋值）。
func (w *StateWriter) State() *State { return w.st }

// Log 追加一条日志并落盘。
//
// 日志行直接进 State（而不是只写进程日志）：用户看的是网页，
// 面板重启后进程日志也未必还在，而 state.json 会一直在。
func (w *StateWriter) Log(level, text string) {
	if text == "" {
		return
	}
	w.st.Logs = append(w.st.Logs, LogLine{At: w.now(), Level: level, Text: text})
	if len(w.st.Logs) > maxLogLines {
		// 只保留最近 maxLogLines 行：留头不留尾会把"最新发生了什么"丢掉，
		// 而那恰恰是用户最需要看到的。
		w.st.Logs = append([]LogLine(nil), w.st.Logs[len(w.st.Logs)-maxLogLines:]...)
	}
	w.Save()
}

// Logf 是 Log 的格式化版本。
func (w *StateWriter) Logf(level, format string, args ...any) {
	w.Log(level, fmt.Sprintf(format, args...))
}

// Stage 切换阶段，并自动在日志里留一行"▶ 阶段名"。
//
// 自动记日志是刻意的：用户要看到的本来就是"逐行的阶段 + 日志"，
// 让每个调用点自己再补一句只会漏写（漏掉的那一步在日志里就是空白）。
func (w *StateWriter) Stage(stage string) {
	if w.st.Progress == nil {
		w.st.Progress = &Progress{Percent: -1}
	}
	if w.st.Progress.Stage == stage && !w.st.Progress.StartedAt.IsZero() {
		// 同一阶段重复设置（例如下载回落到第二个候选源）不重复打日志。
		w.st.Progress.StageLabel = StageLabel(stage)
		w.st.Progress.UpdatedAt = w.now()
		w.Save()
		return
	}
	w.st.Progress.Stage = stage
	w.st.Progress.StageLabel = StageLabel(stage)
	w.st.Progress.UpdatedAt = w.now()
	w.Log("info", "▶ "+StageLabel(stage))
}

// StartDownload 记录"下载开始"，并把清单里已知的总字节写进进度。
//
// total<=0 时明确记成"未知"（Percent=-1），绝不猜一个数字 ——
// 猜出来的百分比会让用户以为"快下完了"，比不显示更糟。
func (w *StateWriter) StartDownload(total int64) {
	if w.st.Progress == nil {
		w.st.Progress = &Progress{}
	}
	w.st.Progress.DownloadedBytes = 0
	w.st.Progress.TotalBytes = total
	w.st.Progress.Percent = percentOf(0, total)
	w.st.Progress.BytesPerSecond = 0
	w.speedAt = time.Time{}
	w.st.Progress.UpdatedAt = w.now()
}

// SetDownloaded 记录**真实**已下载字节数（来自下载回调的写入计数）。
//
// callbackTotal 只在清单没有 size 时才参考：清单里的 size 是权威值，
// 而 Content-Length 可能缺失或被中间层改写。
func (w *StateWriter) SetDownloaded(written, callbackTotal int64) {
	if w.st.Progress == nil {
		w.st.Progress = &Progress{}
	}
	p := w.st.Progress
	if p.TotalBytes <= 0 {
		if callbackTotal > 0 {
			p.TotalBytes = callbackTotal
		}
	}
	p.DownloadedBytes = written
	p.Percent = percentOf(written, p.TotalBytes)

	now := w.now()
	if w.speedAt.IsZero() {
		w.speedAt = now
	}
	// 平均速度 = 真实字节 ÷ 真实耗时。用平均值而不是瞬时差值：
	// 瞬时速度在限速源上抖动很大，用户会以为"卡住了又好了"。
	if elapsed := now.Sub(w.speedAt).Seconds(); elapsed >= 0.5 {
		p.BytesPerSecond = float64(written) / elapsed
	}
	p.UpdatedAt = now
	w.maybeSave()
}

// Fail 把阶段切成"失败"并记一条 error 日志。
//
// 失败原因同时写进 st.Error（既有的、前端已在显示的字段），
// 保证老前端也能如实看到原因。
func (w *StateWriter) Fail(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	w.st.Error = msg
	if msg != "" {
		w.Log("error", "✗ 升级失败："+msg)
	}
	if w.st.Progress != nil {
		w.st.Progress.Stage = StageFailed
		w.st.Progress.StageLabel = StageLabel(StageFailed)
		w.st.Progress.UpdatedAt = w.now()
	}
	w.Save()
}

// Save 立即落盘（跳过节流）。
func (w *StateWriter) Save() {
	w.lastSave = w.now()
	_ = SaveState(w.workDir, w.st)
}

// maybeSave 按节流间隔落盘，用于高频的下载进度回调。
func (w *StateWriter) maybeSave() {
	if w.saveInterval <= 0 {
		w.Save()
		return
	}
	now := w.now()
	if !w.lastSave.IsZero() && now.Sub(w.lastSave) < w.saveInterval {
		return
	}
	w.lastSave = now
	_ = SaveState(w.workDir, w.st)
}

// percentOf 计算百分比；总大小未知时返回 -1（"不知道"，不是 0%）。
func percentOf(done, total int64) float64 {
	if total <= 0 {
		return -1
	}
	p := float64(done) * 100 / float64(total)
	if p > 100 {
		// 服务端声明的大小偏小时不显示 >100%，但也不截断已下载字节数
		// —— 字节数是真实证据，百分比只是它的展示形式。
		p = 100
	}
	if p < 0 {
		p = 0
	}
	return p
}
