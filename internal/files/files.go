// Package files 实现文件管理器。
//
// 安全模型（这是本包最重要的部分）：
//
//	所有路径都必须通过 Resolve 校验：先做字符级检查，再解析符号链接，
//	最后确认结果仍落在允许的根目录内。
//
// 为什么两道检查都要：
//   - 只做字符串前缀检查：`/Users/x/www` 下的软链接 `link -> /etc`
//     会让 `/Users/x/www/link/passwd` 通过前缀检查，实际读到 /etc/passwd。
//   - 只做 realpath 检查：目标文件不存在时 realpath 失败，
//     无法判断"将要创建的路径"是否越界。
//
// 旧面板的 site_del 就是只做了 str_starts_with 前缀校验，
// 这类问题在文件管理器上后果会更严重（可以删除任意文件）。
package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manager 管理允许访问的根目录集合。
type Manager struct {
	roots []string
	// maxEditSize 是允许在线编辑的最大文件大小
	maxEditSize int64
	// userName / userHome 用于新建文件的归属修正
	userName string
	userHome string
}

// Options 是构造参数。
type Options struct {
	// Roots 是允许访问的根目录（会解析为真实路径）
	Roots []string
	// UserName / UserHome 用于把新建文件归属真实用户
	UserName string
	UserHome string
	// MaxEditSize 在线编辑大小上限（字节），默认 2MB
	MaxEditSize int64
}

// ErrForbidden 表示路径不在允许范围内。
var ErrForbidden = errors.New("路径不在允许访问的范围内")

// NewManager 创建文件管理器。
func NewManager(opt Options) *Manager {
	m := &Manager{
		maxEditSize: opt.MaxEditSize,
		userName:    opt.UserName,
		userHome:    opt.UserHome,
	}
	if m.maxEditSize <= 0 {
		m.maxEditSize = 2 << 20 // 2MB
	}
	seen := map[string]bool{}
	for _, r := range opt.Roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		// 根目录本身也要解析软链接，否则后续比较会不一致
		resolved := resolveExisting(r)
		if resolved == "" {
			continue
		}
		// 只保留实际存在的目录
		if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		m.roots = append(m.roots, resolved)
	}
	sort.Strings(m.roots)
	return m
}

// Roots 返回允许访问的根目录列表（前端用来展示"可访问范围"）。
func (m *Manager) Roots() []string {
	out := make([]string, len(m.roots))
	copy(out, m.roots)
	return out
}

// resolveExisting 解析已存在路径的软链接；失败时返回清净化后的路径。
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// resolveParent 解析"父目录存在、自身可能不存在"的路径。
//
// 用于新建文件/目录的场景：目标还不存在，无法直接 EvalSymlinks，
// 但必须把已存在的部分解析掉，否则可以借软链接穿越出去。
func resolveParent(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir := filepath.Dir(p)
	if dir == p {
		return p
	}
	return filepath.Join(resolveParent(dir), filepath.Base(p))
}

// Resolve 把用户传入的路径校验并规范化为绝对真实路径。
//
// allowMissing 为 true 时允许目标不存在（用于新建文件/目录）。
func (m *Manager) Resolve(p string, allowMissing bool) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("%w: 路径为空", ErrForbidden)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: 路径含非法字符", ErrForbidden)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: 必须使用绝对路径", ErrForbidden)
	}
	clean := filepath.Clean(p)

	var real string
	if allowMissing {
		real = resolveParent(clean)
	} else {
		real = resolveExisting(clean)
		if _, err := os.Stat(real); err != nil {
			return "", fmt.Errorf("文件不存在: %s", p)
		}
	}

	for _, root := range m.roots {
		if real == root {
			return real, nil
		}
		if strings.HasPrefix(real, root+string(os.PathSeparator)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("%w: %s（允许的根目录：%s）", ErrForbidden, p, strings.Join(m.roots, ", "))
}

// ---------- 数据结构 ----------

// Entry 是目录里的一个条目。
type Entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModeNum uint32 `json:"mode_num"`
	ModTime string `json:"mod_time"`
	Owner   string `json:"owner"`
	// Symlink 为 true 表示这是软链接；SymlinkTarget 是它指向的位置
	Symlink       bool   `json:"symlink"`
	SymlinkTarget string `json:"symlink_target"`
	// ReadOnly 表示当前用户对该项没有写权限
	ReadOnly bool `json:"read_only"`
}

// ListResult 是一次目录列举的结果。
type ListResult struct {
	Path    string   `json:"path"`
	Parent  string   `json:"parent"`
	Entries []Entry  `json:"entries"`
	Roots   []string `json:"roots"`
	// Writable 表示该目录是否可写
	Writable bool `json:"writable"`
	Total    int  `json:"total"`
	// Truncated 表示条目过多被截断
	Truncated bool `json:"truncated"`
}

// List 列举目录内容。
func (m *Manager) List(p string, showHidden bool) (*ListResult, error) {
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

	dir, err := os.Open(real)
	if err != nil {
		return nil, fmt.Errorf("打开目录失败: %w", err)
	}
	defer func() { _ = dir.Close() }()

	names, err := dir.Readdirnames(-1)
	if err != nil && err != io.EOF {
		return nil, err
	}

	res := &ListResult{
		Path:     real,
		Entries:  []Entry{},
		Roots:    m.Roots(),
		Writable: isWritable(real),
	}
	// 父目录：到根目录时不再往上
	if parent := filepath.Dir(real); parent != real && m.withinRoots(parent) {
		res.Parent = parent
	}

	const maxEntries = 3000
	for _, name := range names {
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		res.Total++
		if len(res.Entries) >= maxEntries {
			res.Truncated = true
			continue
		}
		full := filepath.Join(real, name)
		e := Entry{Name: name, Path: full}
		// 用 Lstat：符号链接本身的信息（不跟随），避免链接指向外部时误报
		li, err := os.Lstat(full)
		if err != nil {
			continue
		}
		e.Mode = li.Mode().String()
		e.ModeNum = uint32(li.Mode().Perm())
		e.ModTime = li.ModTime().Format("2006-01-02 15:04:05")
		e.Owner = ownerName(li)
		if li.Mode()&os.ModeSymlink != 0 {
			e.Symlink = true
			if target, err := os.Readlink(full); err == nil {
				e.SymlinkTarget = target
			}
			// 跟随链接判断是否为目录，方便前端展示
			if si, err := os.Stat(full); err == nil {
				e.IsDir = si.IsDir()
				e.Size = si.Size()
				e.ReadOnly = !isWritable(filepath.Dir(full))
			} else {
				e.ReadOnly = true
			}
		} else {
			e.IsDir = li.IsDir()
			e.Size = li.Size()
			e.ReadOnly = !isWritable(filepath.Dir(full))
		}
		res.Entries = append(res.Entries, e)
	}

	// 排序：目录在前，然后按名称自然序
	sort.SliceStable(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i], res.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return res, nil
}

// withinRoots 判断路径是否在允许的根目录内（不做存在性要求）。
func (m *Manager) withinRoots(p string) bool {
	p = resolveParent(p)
	for _, root := range m.roots {
		if p == root || strings.HasPrefix(p, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// ---------- 读 / 写 ----------

// ReadResult 是读取文件的结果。
type ReadResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Binary    bool   `json:"binary"`
	TooLarge  bool   `json:"too_large"`
	Encoding  string `json:"encoding"`
	Truncated bool   `json:"truncated"`
}

// Read 读取文本文件内容（用于在线编辑）。
//
// 二进制文件不返回内容：既避免前端把乱码写回破坏文件，
// 也避免把大文件塞进 JSON 响应。
func (m *Manager) Read(p string) (*ReadResult, error) {
	real, err := m.Resolve(p, false)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("这是目录，不能作为文件读取")
	}
	res := &ReadResult{Path: real, Size: st.Size()}
	if st.Size() > m.maxEditSize {
		res.TooLarge = true
		return res, nil
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return nil, fmt.Errorf("读取失败: %w", err)
	}
	if isBinary(b) {
		res.Binary = true
		return res, nil
	}
	res.Content = string(b)
	res.Encoding = "utf-8"
	return res, nil
}

// isBinary 用"是否含 NUL 字节"判断二进制文件。
//
// 这是 git 等工具的通用做法：文本文件几乎不会出现 NUL，
// 而二进制文件几乎必然包含。
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// Write 写入文件内容。
//
// 先写临时文件再 rename：避免保存过程中断导致文件被截断
// （正在编辑配置文件时断电，留一个半截文件会让服务起不来）。
func (m *Manager) Write(p, content string, createIfMissing bool) error {
	allowMissing := createIfMissing
	real, err := m.Resolve(p, allowMissing)
	if err != nil {
		return err
	}
	if st, err := os.Stat(real); err == nil && st.IsDir() {
		return fmt.Errorf("这是目录，不能写入")
	}
	if !createIfMissing {
		if _, err := os.Stat(real); err != nil {
			return fmt.Errorf("文件不存在: %s", p)
		}
	}

	dir := filepath.Dir(real)
	tmp, err := os.CreateTemp(dir, ".zp-edit-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败（目录可能不可写）: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // rename 成功后这里不会删掉目标文件
	}()

	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// 保留原文件权限（默认 0644）
	mode := os.FileMode(0o644)
	if st, err := os.Stat(real); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, real); err != nil {
		return fmt.Errorf("替换文件失败: %w", err)
	}
	m.chownRealUser(real)
	return nil
}

// ---------- 增删改 ----------

// Mkdir 新建目录。
func (m *Manager) Mkdir(p string) error {
	real, err := m.Resolve(p, true)
	if err != nil {
		return err
	}
	if _, err := os.Stat(real); err == nil {
		return fmt.Errorf("已存在同名文件或目录")
	}
	if err := os.MkdirAll(real, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	m.chownRealUser(real)
	return nil
}

// Touch 新建空文件。
func (m *Manager) Touch(p string) error {
	real, err := m.Resolve(p, true)
	if err != nil {
		return err
	}
	if _, err := os.Stat(real); err == nil {
		return fmt.Errorf("已存在同名文件")
	}
	f, err := os.OpenFile(real, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_ = f.Close()
	m.chownRealUser(real)
	return nil
}

// Delete 删除文件或目录。
//
// 删除目录必须显式传 recursive=true —— 界面上也应该二次确认。
func (m *Manager) Delete(p string, recursive bool) error {
	real, err := m.Resolve(p, false)
	if err != nil {
		return err
	}
	// 禁止删除根目录本身（否则会把整个站点目录抹掉）
	for _, root := range m.roots {
		if real == root {
			return fmt.Errorf("不允许删除根目录: %s", root)
		}
	}
	st, err := os.Lstat(real)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if !recursive {
			// 非递归时只允许删空目录，避免误删整棵树
			entries, err := os.ReadDir(real)
			if err != nil {
				return err
			}
			if len(entries) > 0 {
				return fmt.Errorf("目录非空，需要确认递归删除")
			}
			return os.Remove(real)
		}
		return os.RemoveAll(real)
	}
	return os.Remove(real)
}

// Rename 重命名或移动。
func (m *Manager) Rename(from, to string) error {
	src, err := m.Resolve(from, false)
	if err != nil {
		return err
	}
	dst, err := m.Resolve(to, true)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("目标已存在: %s", to)
	}
	// 不允许把一个目录移进它自己的子目录（会失败并可能损坏数据）
	if strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		return fmt.Errorf("不能把目录移动到它自己的子目录中")
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("重命名失败: %w", err)
	}
	return nil
}

// Copy 复制文件或目录。
func (m *Manager) Copy(from, to string) error {
	src, err := m.Resolve(from, false)
	if err != nil {
		return err
	}
	dst, err := m.Resolve(to, true)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("目标已存在: %s", to)
	}
	if strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		return fmt.Errorf("不能把目录复制到它自己的子目录中")
	}
	return copyPath(src, dst)
}

func copyPath(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		// 复制软链接本身，而不是它指向的内容（避免意外复制到根目录之外）
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if st.IsDir() {
		if err := os.MkdirAll(dst, st.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// Chmod 修改权限。
func (m *Manager) Chmod(p string, mode os.FileMode) error {
	real, err := m.Resolve(p, false)
	if err != nil {
		return err
	}
	return os.Chmod(real, mode)
}

// ---------- 上传 / 下载 ----------

// SaveUpload 保存上传的文件到指定目录。
//
// 文件名会被清洗：只取 basename，并拒绝空名与 . / .. ，
// 防止上传时用 ../ 覆盖上层文件。
func (m *Manager) SaveUpload(dir, filename string, r io.Reader) (string, int64, error) {
	realDir, err := m.Resolve(dir, false)
	if err != nil {
		return "", 0, err
	}
	name := filepath.Base(filepath.FromSlash(filename))
	if name == "" || name == "." || name == ".." || name == string(os.PathSeparator) {
		return "", 0, fmt.Errorf("非法文件名")
	}
	dst := filepath.Join(realDir, name)
	// 再走一次校验（realDir 内可能有同名软链接指向外部）
	final, err := m.Resolve(dst, true)
	if err != nil {
		return "", 0, err
	}

	// 同名文件自动加序号，避免静默覆盖
	final = uniquePath(final)

	f, err := os.OpenFile(final, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, fmt.Errorf("创建文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(f, r)
	if err != nil {
		_ = os.Remove(final)
		return "", 0, fmt.Errorf("写入失败: %w", err)
	}
	m.chownRealUser(final)
	return final, n, nil
}

// uniquePath 在同名文件存在时追加 -1 / -2 后缀。
func uniquePath(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p
	}
	dir, name := filepath.Split(p)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, os.Getpid(), ext))
}

// OpenForRead 打开文件用于下载。
func (m *Manager) OpenForRead(p string) (*os.File, os.FileInfo, error) {
	real, err := m.Resolve(p, false)
	if err != nil {
		return nil, nil, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, nil, err
	}
	if st.IsDir() {
		return nil, nil, fmt.Errorf("这是目录，无法直接下载。请先压缩为归档文件")
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, nil, err
	}
	return f, st, nil
}

// ---------- 小工具 ----------

func isWritable(p string) bool {
	// 用 Access 语义判断：尝试以只写方式打开目录会失败，
	// 这里用 unix.Access 的替代 —— 通过 os.Stat 的权限位 + 当前 uid 推断过于复杂，
	// 直接尝试创建一个临时文件最可靠，但代价高。
	// 折中：检查是否可写位存在（root 下始终可写，这是可接受的近似）。
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	if os.Geteuid() == 0 {
		return true
	}
	return st.Mode().Perm()&0o200 != 0
}

func ownerName(fi os.FileInfo) string {
	return fileOwner(fi)
}

// FormatSize 把字节数格式化为人类可读（前端也用得上）。
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ModTimeOf 返回文件修改时间（用于前端展示）。
func ModTimeOf(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
