// Package fsroot 是 mac军刀 的路径白名单闸门：所有 path/outpath 参数必须先过这里。
//
// 结论：校验始终针对 EvalSymlinks 之后的真实路径（macOS /var → /private/var，坑 A1），
// 目标不存在时只解析已存在的父目录，否则软链接可以穿越出去（坑 A2）。
package fsroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode 是访问方向：读根 / 写根。
type Mode string

const (
	Read  Mode = "read"
	Write Mode = "write"
)

// 路径里的 shell/引号类字符一律拒绝：它们从不出现在正常文件名里，出现即是攻击尝试。
// 注意：这里拒绝是**第一道**；真正的防注入是不经 shell 逐参传（见 execx）。
const forbiddenChars = ";{}`\"'$\\\n\r\t"

// Guard 持有读写白名单与敏感目录黑名单。可并发使用（只读字段）。
type Guard struct {
	readRoots  []string
	writeRoots []string
	sensitive  []sensitiveRoot
}

type sensitiveRoot struct {
	path   string
	reason string
}

// 默认敏感目录：读根默认是 $HOME，这些必须显式挡掉并给原因。
func defaultSensitive(home string) []sensitiveRoot {
	return []sensitiveRoot{
		{filepath.Join(home, "Library", "Keychains"), "钥匙串目录，含账号密钥"},
		{filepath.Join(home, ".ssh"), "SSH 私钥目录"},
		{filepath.Join(home, "Library", "Cookies"), "浏览器 Cookie，含登录态"},
		{filepath.Join(home, "Library", "Application Support", "Google", "Chrome"), "浏览器配置，含登录态"},
		{filepath.Join(home, "Library", "Safari"), "浏览器数据，含登录态"},
		{"/etc", "系统配置目录"},
		{"/var/db", "系统数据库目录"},
		{"/private/var/db", "系统数据库目录"},
		{"/Library/Keychains", "系统钥匙串目录"},
		{"/System", "系统只读目录"},
	}
}

// New 建立闸门。roots 必须是绝对路径；不存在的读根会被跳过，写根由调用方创建。
func New(readRoots, writeRoots []string) (*Guard, error) {
	g := &Guard{}
	seen := map[string]bool{}
	for _, r := range readRoots {
		p, err := normalizeRoot(r)
		if err != nil {
			return nil, fmt.Errorf("读根无效: %w", err)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		g.readRoots = append(g.readRoots, p)
	}
	for _, r := range writeRoots {
		p, err := normalizeRoot(r)
		if err != nil {
			return nil, fmt.Errorf("写根无效: %w", err)
		}
		found := false
		for _, e := range g.writeRoots {
			if e == p {
				found = true
			}
		}
		if !found {
			g.writeRoots = append(g.writeRoots, p)
		}
	}
	if len(g.readRoots) == 0 {
		return nil, errors.New("至少需要一个读根")
	}
	if len(g.writeRoots) == 0 {
		return nil, errors.New("至少需要一个写根")
	}
	home, _ := os.UserHomeDir()
	g.sensitive = defaultSensitive(home)
	g.sensitive = append(g.sensitive, sensitiveRoot{path: filepath.Join(home, ".dsh"), reason: "DSH 会话数据"})
	return g, nil
}

func normalizeRoot(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("路径为空")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("必须是绝对路径: %s", p)
	}
	if strings.ContainsAny(p, forbiddenChars) || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("路径含非法字符: %s", p)
	}
	clean := filepath.Clean(p)
	// 根目录自身按父目录解析：写根首次启动时还不存在。
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real, nil
	}
	return resolveParent(clean), nil
}

// resolveExisting 解析已存在路径；失败时返回清净化路径。
func resolveExisting(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// resolveParent 解析"父目录存在、自身可能不存在"的路径（坑 A2）。
//
// 已存在的部分一律 EvalSymlinks；软链接悬挂（目标不存在）时保留**链接目标**，
// 好让调用方的根判定把逃逸挡掉 —— 直接返回链接路径会看起来"在根内"。
func resolveParent(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir := filepath.Dir(p)
	if dir == p {
		return p
	}
	base := filepath.Base(p)
	if target, err := os.Readlink(p); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		return filepath.Join(resolveParent(target), base)
	}
	return filepath.Join(resolveParent(dir), base)
}

// Resolve 校验并规范化路径；allowMissing=true 用于输出文件（尚不存在）。
// 返回的是可直接交给 os/exec 的真实绝对路径。
func (g *Guard) Resolve(mode Mode, raw string, allowMissing bool) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", &Error{Path: raw, Reason: "路径为空"}
	}
	if strings.ContainsRune(p, 0) {
		return "", &Error{Path: raw, Reason: "路径含 NUL"}
	}
	if !filepath.IsAbs(p) {
		return "", &Error{Path: raw, Reason: "必须使用绝对路径"}
	}
	if i := strings.IndexAny(p, forbiddenChars); i >= 0 {
		return "", &Error{Path: raw, Reason: fmt.Sprintf("路径含禁止字符 %q", string(p[i]))}
	}
	clean := filepath.Clean(p)

	var real string
	if allowMissing {
		real = resolveParent(clean)
	} else {
		real = resolveExisting(clean)
	}
	// 敏感目录判定要在"文件不存在"之前：否则 ~/.ssh/id_rsa 这类路径
	// 会先报"文件不存在"，把真正的拒绝原因（敏感目录）盖掉（坑 A3）。
	if r := g.sensitiveHit(real); r != nil {
		return "", &Error{Path: p, Reason: "敏感目录禁止访问：" + r.reason, Sensitive: true}
	}
	if !allowMissing {
		if _, err := os.Stat(real); err != nil {
			if errors.Is(err, os.ErrPermission) {
				return "", &Error{Path: p, Reason: "权限不足（macOS 隐私保护可能拦住了该目录）"}
			}
			return "", &Error{Path: p, Reason: "文件不存在"}
		}
	}

	roots := g.readRoots
	if mode == Write {
		roots = g.writeRoots
	}
	for _, root := range roots {
		if within(real, root) {
			return real, nil
		}
	}
	kind := "可读根"
	if mode == Write {
		kind = "可写根"
	}
	return "", &Error{Path: p, Reason: fmt.Sprintf("不在%s内（%s）", kind, strings.Join(roots, ", "))}
}

func (g *Guard) sensitiveHit(real string) *sensitiveRoot {
	for i := range g.sensitive {
		if within(real, g.sensitive[i].path) {
			return &g.sensitive[i]
		}
	}
	return nil
}

// within 判断 p 是否等于 root 或位于 root 之下（两侧都已解析软链接）。
func within(p, root string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(os.PathSeparator))
}

// ReadRoots / WriteRoots / SensitiveRoots 给界面下单选框用。
func (g *Guard) ReadRoots() []string {
	out := make([]string, len(g.readRoots))
	copy(out, g.readRoots)
	return out
}

func (g *Guard) WriteRoots() []string {
	out := make([]string, len(g.writeRoots))
	copy(out, g.writeRoots)
	return out
}

// SensitiveRoots 返回敏感目录清单（用于帮助文案，不参与判定）。
func (g *Guard) SensitiveRoots() []string {
	out := make([]string, 0, len(g.sensitive))
	for _, s := range g.sensitive {
		out = append(out, s.path)
	}
	return out
}

// Error 是路径拒绝错误：带**真实原因**，前端直接显示。
type Error struct {
	Path      string `json:"path"`
	Reason    string `json:"reason"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Reason
	}
	return fmt.Sprintf("%s：%s", e.Reason, e.Path)
}
