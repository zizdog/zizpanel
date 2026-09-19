package sites

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// 站点根目录策略：面板以 root 身份把 root 指令写进 nginx 配置，
// 所以必须挡住系统路径（用户 2026-09-19 要求：通用站点能力，不是"镜像站模块"）。

// RootPolicy 是根目录白名单所需的上下文（只用于给出更准确的提示）。
type RootPolicy struct {
	WWWRoot    string
	UserHome   string
	BrewPrefix string
}

// deniedRoots 是禁止作为站点根目录的系统路径（前缀按路径边界匹配）。
// 拒绝必须给出具体原因，否则用户不知道该怎么改。
var deniedRoots = []struct {
	Path   string
	Reason string
}{
	{"/System", "macOS 系统目录"},
	{"/Library", "macOS 系统资源库"},
	{"/usr", "系统程序目录"},
	{"/bin", "系统命令目录"},
	{"/sbin", "系统命令目录"},
	{"/etc", "系统配置目录"},
	{"/private/etc", "系统配置目录"},
	{"/dev", "设备文件目录"},
	{"/var/db", "系统数据库目录"},
	{"/private/var/db", "系统数据库目录"},
	{"/opt/homebrew", "Homebrew 自身目录（面板自己管理，不能当网站目录）"},
}

// ValidateRootPath 校验站点根目录，返回清理后的绝对路径。
//
// 规则是"绝对路径 + 系统路径黑名单"：家目录、/opt/<自定义>、/Volumes*、WWWRoot
// 以及其它普通目录都放行。除字面路径外还校验**软链接解析后的真实路径**，
// 防止 /Volumes/xxx -> /etc 这种绕行。
func ValidateRootPath(p string, _ RootPolicy) (string, error) {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return "", fmt.Errorf("%w: 根目录不能为空", ErrInvalid)
	}
	// 换行/制表符会破坏 nginx 配置的词法。
	if strings.ContainsAny(raw, "\x00\n\r\t") {
		return "", fmt.Errorf("%w: 根目录不能含换行或制表符", ErrInvalid)
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%w: 根目录必须是绝对路径（以 / 开头），当前是 %q", ErrInvalid, raw)
	}
	// 配置注入面：root 指令会被直接拼进 vhost。
	if strings.ContainsAny(raw, ";{}\"'$`\\") {
		return "", fmt.Errorf("%w: 根目录含 nginx 配置不允许的字符（; { } \" ' $ ` 反斜杠）", ErrInvalid)
	}
	clean := filepath.Clean(raw)
	if clean == "/" {
		return "", fmt.Errorf("%w: 拒绝把根目录设为 /：整个系统盘都会成为网站目录", ErrInvalid)
	}
	for _, cand := range []string{clean, resolveExistingPrefix(clean)} {
		if reason := dangerousRoot(cand); reason != "" {
			return "", fmt.Errorf("%w: %s", ErrInvalid, reason)
		}
	}
	return clean, nil
}

// dangerousRoot 返回该路径被拒绝的具体原因；允许时返回空串。
func dangerousRoot(p string) string {
	if p == "/" {
		return "拒绝把根目录设为 /：整个系统盘都会成为网站目录"
	}
	for _, d := range deniedRoots {
		if p == d.Path || strings.HasPrefix(p, d.Path+"/") {
			return fmt.Sprintf("拒绝 %s：%s，不能作为网站根目录", p, d.Reason)
		}
	}
	return ""
}

// resolveExistingPrefix 解析路径中**已存在**的最长前缀的软链接，再把剩余部分接回去。
//
// 为什么不能直接用 EvalSymlinks：目录还不存在（面板将要创建）时它会直接失败，
// 那样软链接绕行就检不出来。
func resolveExistingPrefix(p string) string {
	cur := filepath.Clean(p)
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if len(tail) == 0 {
				return resolved
			}
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

// RunRoot 在基础根目录上叠加伪静态模板要求的入口子目录（如 Laravel 的 public）。
// 基础目录已经指向该子目录时不重复叠加。
func RunRoot(base, rewrite string) string {
	p, ok := RewritePresetByName(rewrite)
	if !ok || p.PublicDir == "" {
		return base
	}
	if filepath.Base(filepath.Clean(base)) == p.PublicDir {
		return base
	}
	return filepath.Join(base, p.PublicDir)
}

// ValidateListenPort 校验站点监听端口：1-65535，0 与负数无效。
// 保留端口与冲突由调用方用真实来源判定（面板端口/PHP-FPM/站点/反代）。
func ValidateListenPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: 监听端口必须在 1-65535 之间（0 与负数无效），当前 %d", ErrInvalid, port)
	}
	return nil
}

// EffectiveListenPort 返回站点实际写进 nginx 的监听端口。
// 未设置/越界一律按 80 —— 老站点没有这个字段，行为与加它之前逐字一致。
func (s *Site) EffectiveListenPort() int {
	if s.ListenPort < 1 || s.ListenPort > 65535 {
		return 80
	}
	return s.ListenPort
}

// nginxQuote 给含空白的路径加引号。外接盘名常带空格（"My Passport"），
// 不加引号 nginx 会按空白切词，直接 nginx -t 失败。
func nginxQuote(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

// FindRootDirective 从 vhost 文本里读出第一条 `root` 指令的值（去掉引号）。
//
// 用于"改完根目录回读生效值"：按指令词 + 分号扫描（兼容同行多指令的写法），
// 跳过注释行与 root_path 这类前缀相同的标识符；读不到返回空串。
func FindRootDirective(conf string) string {
	for i := 0; i+len("root") <= len(conf); i++ {
		if conf[i:i+len("root")] != "root" {
			continue
		}
		if !isDirectiveBoundary(conf, i) {
			continue
		}
		lineStart := strings.LastIndexByte(conf[:i], '\n') + 1
		if strings.HasPrefix(strings.TrimSpace(conf[lineStart:i]), "#") {
			continue
		}
		rest := conf[i+len("root"):]
		if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue // root_path 之类
		}
		rest = strings.TrimSpace(rest)
		if j := strings.IndexByte(rest, ';'); j >= 0 {
			rest = rest[:j]
		}
		return strings.Trim(strings.TrimSpace(rest), `"`)
	}
	return ""
}

// isDirectiveBoundary 判断 conf[i] 是否为 nginx 指令词的起始边界。
func isDirectiveBoundary(conf string, i int) bool {
	if i == 0 {
		return true
	}
	switch conf[i-1] {
	case ' ', '\t', '\n', '\r', '{', '}', ';':
		return true
	}
	return false
}

// DirOwnerUID 回读目录归属（uid/gid）。面板以 root 创建目录后必须交还真实用户，
// 这里就是"做完 stat 回读"的判据。
func DirOwnerUID(path string) (int, int, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("无法读取 %s 的归属信息", path)
	}
	return int(sys.Uid), int(sys.Gid), nil
}

// DirReadableBy 判断目录对指定 uid/gid 是否有 r-x（nginx 要能列目录/读文件）。
// 只看权限位，不查 ACL —— 查不到就按权限位如实判断。
func DirReadableBy(path string, uid, gid int) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("读取目录 %s 失败: %w", path, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s 不是目录", path)
	}
	mode := st.Mode().Perm()
	bits := mode & 7
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		switch {
		case int(sys.Uid) == uid:
			bits = (mode >> 6) & 7
		case int(sys.Gid) == gid:
			bits = (mode >> 3) & 7
		}
	}
	if bits&5 != 5 { // r=4, x=1
		return fmt.Errorf("%s 对运行站点用户不可读（权限 %03o，需要 r-x）：nginx 会返回 403，"+
			"请改成 chmod 755 或把目录交给该用户", path, mode)
	}
	return nil
}
