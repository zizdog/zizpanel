// Package permissions 是「权限」页的判据层：只做**不碰受保护路径**的状态判定。
//
// 铁律（坑 191）：列表/首屏绝不能读一次受保护路径 —— 那会替用户把授权记成
// denial。真正会弹窗的读只发生在用户显式申请之后，由 web 层驱动。
package permissions

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// 状态（GET 下发的机器可读值）。
const (
	StatusUnknown      = "unknown"
	StatusGranted      = "granted"
	StatusDenied       = "denied"
	StatusNeedsConsole = "needs_console"
)

// Item 是「权限」页的一项。外部应用专有字段只在装了它的机器上非空。
type Item struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Why           string   `json:"why"`
	Status        string   `json:"status"`
	StatusHint    string   `json:"status_hint,omitempty"`
	ConsoleUser   string   `json:"console_user"`
	IsRemote      bool     `json:"is_remote"`
	CanApply      bool     `json:"can_apply"`
	LastCheckedAt string   `json:"last_checked_at,omitempty"`
	LastResult    string   `json:"last_result,omitempty"`
	Targets       []string `json:"targets,omitempty"`
	ManualPath    string   `json:"manual_path,omitempty"`
	// AcceptsPath 为 true 时前端显示"要申请的目录"选择/输入（外部应用用）。
	AcceptsPath bool `json:"accepts_path,omitempty"`

	Version   string   `json:"version,omitempty"`
	ExecPath  string   `json:"exec_path,omitempty"`
	SigningID string   `json:"signing_id,omitempty"`
	Port      int      `json:"port,omitempty"`
	Roots     []string `json:"roots,omitempty"`
}

// ConsoleOK 判定"屏幕上有人在、能点系统弹窗"。
//
// root 与 loginwindow 都表示没人在（登录窗口 / 无 GUI），与磁盘页同一套判据。
func ConsoleOK(consoleUser string) bool {
	switch strings.TrimSpace(consoleUser) {
	case "", "root", "loginwindow":
		return false
	}
	return true
}

// ProtectedDirCandidates 返回完全磁盘访问会拦到的受保护目录。
//
// 只拼路径、**不 stat**：GET 一个字节都不许读（坑 191）。
func ProtectedDirCandidates(home string) []string {
	home = strings.TrimSpace(home)
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Documents"),
		filepath.Join(home, "Downloads"),
	}
}

// PathResult 是对一个目标路径"读一次"的真实结果。
type PathResult struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Denied   bool   `json:"denied"`
	Reason   string `json:"reason,omitempty"`
}

// Probe 是唯一会触发系统弹窗的读动作；web 层注入它，GET 路径永不调用。
type Probe func(ctx context.Context, targets []string) []PathResult

// ReadProtected 是默认读动作：目录列一次目录，再真的打开一个文件证明内容可读。
func ReadProtected(ctx context.Context, targets []string) []PathResult {
	out := make([]PathResult, 0, len(targets))
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		out = append(out, ReadOne(t))
	}
	return out
}

// ReadOne 读一个目标；权限拒绝单独标记（判据不靠文案）。
func ReadOne(path string) PathResult {
	res := PathResult{Path: path}
	entries, err := os.ReadDir(path)
	if err == nil {
		res.Readable = true
		// 目录读通只证明"能列目录"；再打开一个文件，才算内容真的可读。
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			f, oerr := os.Open(filepath.Join(path, e.Name()))
			if oerr != nil {
				res.Readable = false
				res.Denied = permissionDenied(oerr)
				res.Reason = oerr.Error()
				return res
			}
			buf := make([]byte, 1)
			_, rerr := f.Read(buf)
			_ = f.Close()
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				res.Readable = false
				res.Denied = permissionDenied(rerr)
				res.Reason = rerr.Error()
			}
			return res
		}
		return res
	}
	res.Denied = permissionDenied(err)
	res.Reason = err.Error()
	return res
}

// permissionDenied 判 TCC 拒绝：macOS 报 EPERM（operation not permitted），有时 EACCES。
func permissionDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}
