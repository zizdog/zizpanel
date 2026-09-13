package files

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// 平台相关的小工具。单独成文件是因为这些实现依赖 Unix 的 syscall 结构体，
// 与文件管理的主逻辑（路径校验、增删改查）性质不同。

// fileOwner 返回文件属主的 "用户:组" 字符串。
func fileOwner(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	gid := strconv.FormatUint(uint64(st.Gid), 10)
	uname, gname := uid, gid
	if u, err := user.LookupId(uid); err == nil {
		uname = u.Username
	}
	if g, err := user.LookupGroupId(gid); err == nil {
		gname = g.Name
	}
	return uname + ":" + gname
}

// chownRealUser 把新建的文件归属给真实用户。
//
// 面板以 root 运行，新建的文件默认属于 root，
// 结果用户在 Finder 里无法编辑，也会让 git 等工具报权限问题。
func (m *Manager) chownRealUser(p string) {
	if os.Geteuid() != 0 || m.userName == "" || m.userName == "root" {
		return
	}
	u, err := user.Lookup(m.userName)
	if err != nil {
		return
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return
	}
	_ = os.Chown(p, uid, gid)
}

// StatUint32 把 os.FileMode 的权限位转成八进制整数（前端编辑权限用）。
func StatUint32(mode os.FileMode) uint32 { return uint32(mode.Perm()) }

// ParseMode 把 "755" 这样的八进制字符串解析为 FileMode。
//
// 严格校验：只接受 3~4 位八进制数字，
// 避免把用户输入直接当权限位（例如 "9999" 这类非法值）。
func ParseMode(s string) (os.FileMode, error) {
	s = trimSpace(s)
	if len(s) < 3 || len(s) > 4 {
		return 0, fmt.Errorf("权限必须是 3 或 4 位八进制数字，例如 644 / 755")
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("权限格式不正确: %s", s)
	}
	if n > 0o7777 {
		return 0, fmt.Errorf("权限值超出范围: %s", s)
	}
	return os.FileMode(n), nil
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
