package permissions

// fulldiskprobe.go —— **实测**"面板到底有没有完全磁盘访问权限"。
//
// 为什么不能只看申请记录（用户 2026-10-06 报障）：面板重装后
// `permissions-history.json` 是空的，于是仪表盘一直喊「缺可移除宗卷授权」，
// 可用户其实早就在系统设置里把面板加进了完全磁盘访问权限 —— **记录 ≠ 事实**。
// 记录只在"全新安装、还没授过"时才有参考价值，作为兜底保留。
//
// 判据：真的去读一个**只有 FDA 读得到**的文件（系统 TCC 数据库）。它是 SIP + TCC
// 双重保护的，没有 FDA 时连 root 都读不到，而且 **FDA 这个类别系统不弹窗**（静默拒绝），
// 所以放在 GET 里做探测不会给用户弹任何东西（与"读桌面/文稿/下载"那类会弹窗的动作不同）。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// FullDiskProbe 是一次实测的结论。
type FullDiskProbe struct {
	// Granted = 真的读到了受系统保护的文件（不是靠记录推断的）。
	Granted bool
	// Known = 这次探测拿到了明确结论；false 表示目标都不在/错误类型认不出（绝不猜）。
	Known  bool
	Detail string
}

// fullDiskProbeTargets 返回按顺序尝试的探测目标。
//
//   - 系统 TCC 库：只有 FDA 读得到（首选，与用户是谁无关）；
//   - 用户 TCC 库：备选（某些机器上前者不可读/不存在）。
func fullDiskProbeTargets(home string) []string {
	out := []string{filepath.Join("/Library/Application Support/com.apple.TCC", "TCC.db")}
	if h := strings.TrimSpace(home); h != "" {
		out = append(out, filepath.Join(h, "Library/Application Support/com.apple.TCC", "TCC.db"))
	}
	return out
}

// fullDiskReadFn 是实际去读的动作（注入点：单测要能造出"读得到/被拒/文件不存在"
// 三种世界，而绝不真去碰系统 TCC 库）。
var fullDiskReadFn = func(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	return f.Close()
}

// ProbeFullDiskAccess 实测一次完全磁盘访问权限。
func ProbeFullDiskAccess(home string) FullDiskProbe {
	tried := 0
	denied := 0
	for _, p := range fullDiskProbeTargets(home) {
		err := fullDiskReadFn(p)
		switch {
		case err == nil:
			return FullDiskProbe{Granted: true, Known: true,
				Detail: "实测可读受系统保护的文件（依据是真实权限，不是申请记录）"}
		case errors.Is(err, fs.ErrNotExist):
			continue // 这个目标不存在，换下一个
		case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
			tried++
			denied++
		default:
			// 认不出的错误（比如路径不可达）不下结论，换下一个目标。
			continue
		}
	}
	if tried > 0 && tried == denied {
		return FullDiskProbe{Known: true, Detail: "实测读不到受系统保护的文件：系统里还没有这一项授权"}
	}
	return FullDiskProbe{Detail: "无法实测（探测目标不可用）：不下结论"}
}
