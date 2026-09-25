package files

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 目录递归大小的预算（用户要的是宝塔那种"点一下才统计"）。
//
// 为什么必须有界：一个超大目录（几十万条目）或刚挂上的慢盘，不能让 HTTP
// 请求挂死。超预算就返回**已统计的部分**并标 Truncated —— 如实说"没统计完"，
// 绝不假装是全部。
//
// 8s：本机 SSD 上足够扫完几十万条目，又短于浏览器/代理的常见超时；
// 20 万条：条目数上限，防退化成分钟级扫描（预算之外的第二道闸）。
const (
	DirSizeDefaultBudget     = 8 * time.Second
	DirSizeDefaultMaxEntries = 200000
)

// DirSizeLimits 是一次统计的预算（零值表示用默认值）。
//
// 它做成参数而不是写死的常量，是为了留一个**测试注入点**：门禁把预算调到
// 极小，验证"超预算必定返回 truncated=true"这条语义（见 internal/web 的门禁）。
type DirSizeLimits struct {
	Budget     time.Duration
	MaxEntries int64
}

func (l DirSizeLimits) withDefaults() DirSizeLimits {
	if l.Budget <= 0 {
		l.Budget = DirSizeDefaultBudget
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DirSizeDefaultMaxEntries
	}
	return l
}

// DirSizeResult 是一次递归大小统计的结果。
type DirSizeResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Files int64  `json:"files"`
	// Dirs 是子孙目录数（不含起点目录本身）
	Dirs int64 `json:"dirs"`
	// Skipped 是**没读到的条目**（ReadDir/Lstat 失败，或设备/FIFO 这类特殊文件）：
	// 跳过但如实计数，绝不静默当 0
	Skipped int64 `json:"skipped"`
	// Symlinks 是刻意**没跟随**的符号链接数（Lstat）：不跟随才不会借链接跳出
	// 白名单，也不会绕进环
	Symlinks int64 `json:"symlinks"`
	// Truncated 表示没统计完（超预算）；Reason 是一句人话原因
	Truncated bool   `json:"truncated"`
	Reason    string `json:"reason,omitempty"`
	Ms        int64  `json:"ms"`
}

// DirSize 统计目录的递归大小。
//
// 语义：
//   - 路径先过 Resolve（与列表/其它文件操作同一套白名单校验，越界返回 ErrForbidden）；
//   - 必须是目录（否则报错，web 层映射成 400）；
//   - 不跟随符号链接（Lstat）：既不跳白名单、也不会绕环；
//   - 有界：超预算/超条目上限就返回已统计的部分，Truncated=true 并给 Reason；
//   - 读不到的条目计入 Skipped，绝不静默当 0。
func (m *Manager) DirSize(p string, lim DirSizeLimits) (*DirSizeResult, error) {
	real, err := m.Resolve(p, false)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("不是目录: %s", p)
	}
	lim = lim.withDefaults()
	start := time.Now()
	deadline := start.Add(lim.Budget)
	res := &DirSizeResult{Path: real}

	// 迭代遍历（显式栈）：递归实现在极深目录树上会爆栈。
	stack := []string{real}
	var seen int64
	for len(stack) > 0 {
		if time.Now().After(deadline) {
			res.Truncated = true
			res.Reason = fmt.Sprintf("超过 %.1fs 预算，只统计了一部分", lim.Budget.Seconds())
			break
		}
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		items, err := os.ReadDir(dir)
		if err != nil {
			res.Skipped++
			continue
		}
		for _, it := range items {
			seen++
			if seen > lim.MaxEntries {
				res.Truncated = true
				res.Reason = fmt.Sprintf("超过 %d 条上限，只统计了一部分", lim.MaxEntries)
				break
			}
			if time.Now().After(deadline) {
				res.Truncated = true
				res.Reason = fmt.Sprintf("超过 %.1fs 预算，只统计了一部分", lim.Budget.Seconds())
				break
			}
			full := filepath.Join(dir, it.Name())
			li, err := os.Lstat(full)
			if err != nil {
				res.Skipped++
				continue
			}
			switch {
			case li.Mode()&os.ModeSymlink != 0:
				res.Symlinks++
			case li.IsDir():
				res.Dirs++
				stack = append(stack, full)
			case li.Mode().IsRegular():
				res.Bytes += li.Size()
				res.Files++
			default:
				// 设备/FIFO/套接字没有有意义的大小（读它还可能阻塞），按 0 计但计数
				res.Skipped++
			}
		}
		if res.Truncated {
			break
		}
	}
	res.Ms = time.Since(start).Milliseconds()
	return res, nil
}
