package upgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Status 是升级流程的状态。
//
// 之所以要有这么多中间状态，而不是简单的成功/失败：
// 升级会**重启面板**，用户刷新页面时必须能知道"进行到哪一步了"，
// 否则界面只能显示一句"升级中…"，出问题时无从判断卡在哪里。
type Status string

const (
	StatusIdle        Status = "idle"
	StatusChecking    Status = "checking"
	StatusDownloading Status = "downloading"
	StatusStaged      Status = "staged"      // 已下载校验完毕，等待应用
	StatusApplying    Status = "applying"    // 正在替换二进制
	StatusRestarting  Status = "restarting"  // 已重启，等待看门狗判定
	StatusSuccess     Status = "success"     // 新版已确认可用
	StatusRolledBack  Status = "rolled_back" // 新版失败，已回滚到旧版
	StatusFailed      Status = "failed"      // 未到替换阶段就失败（旧版没动）
)

// IsTerminal 报告该状态是否已经结束（前端据此停止轮询）。
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSuccess, StatusRolledBack, StatusFailed, StatusIdle, StatusStaged:
		return true
	}
	return false
}

// Step 是升级过程里的一条流水记录。
type Step struct {
	At      time.Time `json:"at"`
	Stage   string    `json:"stage"`
	Message string    `json:"message"`
	OK      bool      `json:"ok"`
}

// State 是持久化的升级状态。
//
// **必须落盘而不是放内存**：升级的最后一步是重启面板，
// 内存在那一刻就没了。新版启动后要能读到"上次升级进行到哪一步"，
// 才能如实告诉用户结果；否则界面会永远停在"升级中"。
type State struct {
	Status Status `json:"status"`
	// RunID 标识"这一次升级尝试"。
	//
	// 为什么必须有它：看门狗的结论写在磁盘上，而结论被读走之前会一直留着。
	// 于是会出现这种真实故障 ——
	//   第一次升级成功 → result.txt 留在磁盘上（用户没打开过设置页，没被消费）
	//   → 用户后来准备了新版本 → 点"立即升级"时读到的是**上一次**的 success
	//   → 状态被改写成 success，于是拒绝执行，提示"还没有已准备好的升级包"。
	// 把结论与具体的 RunID 绑定，才能确认"这条结论是这一次的"。
	RunID      string    `json:"run_id,omitempty"`
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	Source     string    `json:"source,omitempty"` // remote / upload
	Stage      string    `json:"stage,omitempty"`
	Message    string    `json:"message,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Steps      []Step    `json:"steps,omitempty"`
}

// statePath 返回状态文件路径。
func statePath(workDir string) string {
	return filepath.Join(workDir, "upgrade", "state.json")
}

// resultPath 返回看门狗写下的最终结果文件路径。
//
// 单独一个纯文本文件，而不是复用 state.json：
// 看门狗是 shell 脚本，在"新版可能已经崩溃"的环境里做判断，
// 让它去拼 JSON 是给自己找麻烦。纯文本一行，怎么都不会写错。
func resultPath(workDir string) string {
	return filepath.Join(workDir, "upgrade", "result.txt")
}

// LoadState 读取状态；文件不存在时返回一个 idle 状态而不是错误。
//
// 注意这个"读"函数会**写**一次：消费看门狗的结论后会把它持久化。
// 这是刻意的 —— 结论必须先落到 state.json 再删 result.txt，
// 否则会出现"这一次读到了 success，下一次又变回 restarting"的鬼状态：
// 状态文件里始终是旧的中间态，而唯一的权威证据已经被删掉了。
func LoadState(workDir string) *State {
	st := &State{Status: StatusIdle}
	if b, err := os.ReadFile(statePath(workDir)); err == nil {
		if err := json.Unmarshal(b, st); err != nil {
			st = &State{Status: StatusIdle, Message: "状态文件损坏，已重置"}
		}
	}
	// 状态文件缺失（首次升级、被清理、升级中途被删）时也要看结论：
	// 看门狗写下的结果比状态文件更权威。
	if applyResult(workDir, st) {
		_ = SaveState(workDir, st)
	}
	return st
}

// SaveState 原子写入状态。
func SaveState(workDir string, st *State) error {
	p := statePath(workDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// applyResult 把看门狗写下的结果合并进状态。
//
// 幂等：合并后会把结果文件删掉，避免同一个结果被反复应用
// （否则用户点了"我知道了"，刷新一次又冒出来）。
//
// 返回是否真的合并了内容，调用方据此决定要不要持久化。
func applyResult(workDir string, st *State) bool {
	b, err := os.ReadFile(resultPath(workDir))
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		return false
	}
	fields := strings.Fields(line)
	// 格式：<success|rolled_back> <version> <unix秒> <runID> [说明...]
	if len(fields) < 4 {
		return false
	}
	// 只接受属于**当前这次升级**的结论。
	// 没有 RunID 的旧格式结论、或属于别的尝试的结论，一律忽略。
	if st.RunID == "" || fields[3] != st.RunID {
		return false
	}
	// 再来一道保险：已经开始新尝试（staged/applying）时，
	// 绝不让一条迟到的旧结论把状态改回去。
	if st.Status != StatusRestarting && st.Status != StatusApplying {
		return false
	}
	switch fields[0] {
	case string(StatusSuccess):
		st.Status = StatusSuccess
		st.To = fields[1]
		st.Message = "升级成功，新版已通过健康检查"
		st.Error = ""
	case string(StatusRolledBack):
		st.Status = StatusRolledBack
		// 回滚之后"当前版本"就是被还原的那个版本。
		// 这里必须更新 To，否则界面会显示一个根本没在运行的版本号。
		st.To = fields[1]
		st.Message = "新版启动失败，已自动回滚到 " + fields[1]
		if len(fields) > 4 {
			st.Error = strings.Join(fields[4:], " ")
		} else {
			st.Error = "新版未能在规定时间内通过健康检查"
		}
	default:
		return false
	}
	st.Stage = ""
	st.FinishedAt = parseUnix(fields[2])
	_ = os.Remove(resultPath(workDir))
	return true
}

func parseUnix(s string) time.Time {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return time.Time{}
		}
		n = n*10 + int64(r-'0')
	}
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// WriteResult 由看门狗脚本调用（也用于测试），写下一行结果。
func WriteResult(workDir string, runID string, status Status, version string, note string) error {
	p := resultPath(workDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// 字段顺序：status version unix runID note...
	// runID 放第 4 位之后说明文字可以带空格，解析时从第 5 位开始拼。
	line := fmt.Sprintf("%s %s %d %s %s\n", status, version, time.Now().Unix(), runID, note)
	return os.WriteFile(p, []byte(line), 0o644)
}

// ClearResult 删掉遗留的看门狗结论。
//
// 每次开始新的升级尝试前都要清一次：结论文件是"上一次"的，
// 留着会让新尝试读到旧结论（这个 bug 在真机上真的发生过）。
func ClearResult(workDir string) {
	_ = os.Remove(resultPath(workDir))
}
