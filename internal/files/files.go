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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
			// 权限拒绝必须**保留真实 errno 与路径**，不能一律谎报"文件不存在"：
			// 外接卷被 macOS 隐私保护（TCC）拦住时 stat 就会 EPERM，而 web 层要靠
			// errno + 路径前缀把它映射成"用自定义挂载点绕过"的可操作指引
			// （见 internal/web/api_files.go 的 volumeTCCPath）。
			if errors.Is(err, fs.ErrPermission) {
				return "", err
			}
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
	// Sensitive 表示这一项落在"敏感目录"里（如面板数据目录：SQLite 库、
	// 凭据文件）。面板仍允许读写它，但界面要打标记并在覆盖/删除前二次确认。
	Sensitive bool `json:"sensitive,omitempty"`
}

// ListResult 是一次目录列举的结果。
type ListResult struct {
	Path    string   `json:"path"`
	Parent  string   `json:"parent"`
	Entries []Entry  `json:"entries"`
	Roots   []string `json:"roots"`
	// RootKinds 给每个根目录标一个用途（www/home/panel/homebrew/volume/other），
	// 供前端「位置」下拉分组。值与 Roots 里的路径一一对应。
	RootKinds map[string]string `json:"root_kinds,omitempty"`
	// SensitiveRoots 是需要特别当心的目录（面板数据目录：库与凭据）。
	SensitiveRoots []string `json:"sensitive_roots,omitempty"`
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

// MoveStrategy 描述一次移动**实际**采用的方式，前端据此如实告诉用户
// "同卷重命名"还是"跨卷复制后删除"（两者的语义/风险不同，不能合成一句"已移动"）。
type MoveStrategy string

const (
	// MoveStrategyNone：源与目标相同，什么都没做。
	MoveStrategyNone MoveStrategy = "none"
	// MoveStrategyRename：同卷 os.Rename（原子、瞬时，不复制数据）。
	MoveStrategyRename MoveStrategy = "rename"
	// MoveStrategyCopyDelete：跨卷回退 —— 复制整棵树到目标，成功后删除源。
	MoveStrategyCopyDelete MoveStrategy = "copy+delete"
)

// 冲突处理策略（与上传的 on_conflict 语义对齐）。
const (
	// MoveConflictRename 目标已存在时自动改名（-1/-2…），两个都保留。
	MoveConflictRename = "rename"
	// MoveConflictOverwrite 覆盖目标（先删目标再移入）。破坏性，必须由用户显式选择。
	MoveConflictOverwrite = "overwrite"
	// MoveConflictSkip 目标已存在时跳过这一项。
	MoveConflictSkip = "skip"
)

// MoveResult 是一次移动的结果。
type MoveResult struct {
	From string       `json:"from"`
	To   string       `json:"to"`
	Way  MoveStrategy `json:"way"`
	// Skipped 为 true 表示按用户选择跳过了（目标已存在）。
	Skipped bool `json:"skipped,omitempty"`
	// Overwritten 为 true 表示确实删掉了目标处的原有内容再移入。
	Overwritten bool `json:"overwritten,omitempty"`
}

// renameFunc 是 os.Rename 的注入点。
//
// 变量而不是直接调 os.Rename：跨卷回退（EXDEV → 复制后删除）是移动语义里最容易
// 写错的一条分支，而单测环境里所有临时目录都在同一卷上，根本触发不到 EXDEV。
// 测试注入一个"永远返回 EXDEV"的实现就能真正跑到那条分支。
var renameFunc = os.Rename

// Move 移动文件或目录（剪切粘贴用它）。
//
// 语义（按顺序）：
//  1. 源与目标相同 → 直接返回 "none"；
//  2. 目标已存在 → 按 onConflict 处理：skip 跳过 / overwrite 先删目标再移入 /
//     其它（默认）自动改名保留两者。**绝不静默覆盖**。
//  3. 先试 os.Rename（同卷：原子且不复制数据）；
//  4. Rename 报 EXDEV（跨卷）才回退到 copy + delete，并在结果里如实标记方式。
//
// 失败时的清理：跨卷复制阶段失败会把可能写了一半的目标删掉，避免留下一个
// "看起来成功、其实残缺"的副本；删除源失败时明确报出"已复制但源没删掉"。
func (m *Manager) Move(from, to, onConflict string) (*MoveResult, error) {
	src, err := m.Resolve(from, false)
	if err != nil {
		return nil, err
	}
	dst, err := m.Resolve(to, true)
	if err != nil {
		return nil, err
	}
	if src == dst {
		return &MoveResult{From: src, To: dst, Way: MoveStrategyNone}, nil
	}
	// 不允许把目录移进它自己的子目录（会失败并可能损坏数据）
	if strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		return nil, fmt.Errorf("不能把目录移动到它自己的子目录中")
	}

	res := &MoveResult{From: src, To: dst}
	if _, statErr := os.Lstat(dst); statErr == nil {
		switch strings.ToLower(strings.TrimSpace(onConflict)) {
		case MoveConflictSkip:
			res.Skipped = true
			res.Way = MoveStrategyNone
			return res, nil
		case MoveConflictOverwrite:
			// 覆盖 = 删掉目标原有内容。绝不能把白名单根目录本身删掉。
			for _, root := range m.roots {
				if dst == root {
					return nil, fmt.Errorf("不允许覆盖根目录: %s", root)
				}
			}
			if err := os.RemoveAll(dst); err != nil {
				return nil, fmt.Errorf("覆盖目标失败: %w", err)
			}
			res.Overwritten = true
		default:
			res.To = uniquePath(dst)
			dst = res.To
		}
	}

	if err := renameFunc(src, dst); err == nil {
		res.Way = MoveStrategyRename
		m.chownRealUser(dst)
		return res, nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return nil, fmt.Errorf("移动失败: %w", err)
	}

	// 跨卷：Rename 无法跨设备，回退为复制后删除。
	if err := copyPath(src, dst); err != nil {
		_ = os.RemoveAll(dst) // 清掉写了一半的目标，别留下残缺副本
		return nil, fmt.Errorf("跨卷移动失败（复制阶段）: %w", err)
	}
	if err := os.RemoveAll(src); err != nil {
		return nil, fmt.Errorf("跨卷移动：内容已复制到 %s，但删除源 %s 失败（源仍在，请手动清理）: %w",
			dst, src, err)
	}
	res.Way = MoveStrategyCopyDelete
	m.chownRealUser(dst)
	return res, nil
}

// Chmod 修改权限。
func (m *Manager) Chmod(p string, mode os.FileMode) error {
	real, err := m.Resolve(p, false)
	if err != nil {
		return err
	}
	return os.Chmod(real, mode)
}

// DefaultDir 返回"没有指定路径时应该打开哪个目录"。
//
// 为什么不直接用 Roots()[0]：roots 在 NewManager 里是**按字母排序**的，
// 而文件管理器还会把"面板安装的应用的配置文件所在目录"加进白名单。
// 真机上实测：`/opt/homebrew/etc` 排在网站根目录前面 —— 于是用户点开
// 「文件管理」看到的是 Homebrew 的配置目录。他正要"把网站文件传上去"，
// 一不小心就把整站传进了 Homebrew 的配置目录（2026-09-18 复现，
// 这也正是 handleFileList 那句注释「默认打开网站根目录」本来想做的事）。
//
// 优先返回 preferred（网站根目录）；它不在白名单里（没建/不存在）时才退回第一个根。
func (m *Manager) DefaultDir(preferred string) string {
	if preferred != "" {
		if real, err := m.Resolve(preferred, false); err == nil {
			return real
		}
	}
	if len(m.roots) == 0 {
		return ""
	}
	return m.roots[0]
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

// SaveUploadAs 按**相对路径**保存上传的文件，用于「上传文件夹」时重建目录树。
//
// 与 SaveUpload 的区别（都是刻意的）：
//   - 相对路径的每一段都会被 CleanRelPath 校验（拒绝 ..、绝对路径、反斜杠、
//     盘符前缀），再把整条路径交给 Resolve 做软链接解析 + 白名单前缀检查；
//   - overwrite 为 true 时覆盖同名文件（浏览器上传整个站点时，把 index.php
//     存成 index-1.php 会直接让站点跑不起来）；为 false 时沿用 SaveUpload 的
//     "加序号不覆盖" 行为。
//
// 返回值里的 overwritten 表示这次确实覆盖了一个已存在的文件，前端要如实告诉
// 用户（"传上去了"和"把原来的覆盖了"是两件事）。
func (m *Manager) SaveUploadAs(dir, relPath string, r io.Reader, overwrite bool) (path string, n int64, overwritten bool, err error) {
	realDir, err := m.Resolve(dir, false)
	if err != nil {
		return "", 0, false, err
	}
	rel, err := CleanRelPath(relPath)
	if err != nil {
		return "", 0, false, err
	}
	dst := filepath.Join(realDir, filepath.FromSlash(rel))

	// 关键一步：目标（多半）还不存在，必须让 Resolve 去解析**已存在的祖先**
	// 里的软链接。否则 <根>/link -> /etc 这种目录能借相对路径穿越出去 ——
	// 纯字符串前缀检查看不出来（见本包开头的安全模型）。
	final, err := m.Resolve(dst, true)
	if err != nil {
		return "", 0, false, err
	}
	// 双保险：Resolve 已经做过前缀检查，这里再确认一次结果落在目标目录之内，
	// 免得将来有人改动 Resolve 时悄悄放宽。
	if final == realDir || !strings.HasPrefix(final, realDir+string(os.PathSeparator)) {
		return "", 0, false, fmt.Errorf("%w: %s", ErrForbidden, relPath)
	}
	if err := m.mkdirAllOwned(realDir, filepath.Dir(final)); err != nil {
		return "", 0, false, err
	}

	if overwrite {
		if _, statErr := os.Lstat(final); statErr == nil {
			overwritten = true
		}
	} else {
		final = uniquePath(final)
	}
	// MkdirAll 之后父目录真实存在了，再解析一次：这次连**文件本身**是软链接的
	// 情况也一起挡下（覆盖一个指向外部的软链接 = 往外部写）。
	final, err = m.Resolve(final, true)
	if err != nil {
		return "", 0, false, err
	}
	if final == realDir || !strings.HasPrefix(final, realDir+string(os.PathSeparator)) {
		return "", 0, false, fmt.Errorf("%w: %s", ErrForbidden, relPath)
	}

	f, err := os.OpenFile(final, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, false, fmt.Errorf("创建文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	n, err = io.Copy(f, r)
	if err != nil {
		_ = os.Remove(final)
		return "", 0, false, fmt.Errorf("写入失败: %w", err)
	}
	m.chownRealUser(final)
	return final, n, overwritten, nil
}

// mkdirAllOwned 在 base 之下逐段创建 dir，并把**新建的**目录归属给真实用户。
//
// 为什么不用 os.MkdirAll：面板以 root 运行，MkdirAll 建出来的目录归 root，
// 而网站进程（nginx / php-fpm 以真实用户跑）要能在里面写缓存/上传 —— 归属必须
// 在创建的那一刻就交还，不能"下次再说"（见 AGENTS.md 第三节：坑 156/163）。
// 逐段做还有个好处：只在真正新建的那一层 chown，重复上传同一棵目录树不会
// 对每个文件都重跑一遍 chown。
func (m *Manager) mkdirAllOwned(base, dir string) error {
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	if rel == "." {
		return nil // base 本身由调用方保证存在
	}
	cur := base
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("创建目录失败: 非法的相对目录 %q", rel)
		}
		cur = filepath.Join(cur, seg)
		if st, statErr := os.Stat(cur); statErr == nil {
			if !st.IsDir() {
				return fmt.Errorf("创建目录失败: %s 已被同名文件占用", cur)
			}
			continue
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return fmt.Errorf("创建目录失败: %w", err)
		}
		m.chownRealUser(cur)
	}
	return nil
}

// ErrBadRelPath 表示上传时携带的相对路径不合法（疑似路径穿越）。
var ErrBadRelPath = errors.New("非法的相对路径")

// CleanRelPath 校验并净化上传时携带的相对路径（如 webkitRelativePath）。
//
// 「上传文件夹」要按客户端给的相对路径重建目录树，这条路径是**用户可控输入**，
// 因此这里的每一条拒绝规则都对应一种穿越手法：
//
// 空路径 / 含 NUL：截断类攻击，以及无意义的输入。
// 以斜杠开头：绝对路径，直接无视目标目录。
// 含反斜杠：Windows 分隔符。macOS 上反斜杠是合法文件名字符，但后端不该按平台
// 差异给出两种语义（同一份请求在 Windows 浏览器上会变成 ..\..\ 穿越），一律拒绝。
// 形如 "C:" 的盘符前缀：同上。
// 任何一段是空 / 点 / 点点：空段与相对段。点点段就是最典型的穿越，而且
// filepath.Join 会**静默吃掉**它，不在这里挡住就没人挡了。
//
// 返回用 "/" 分隔的净化路径（至少一段）。
func CleanRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: 路径为空", ErrBadRelPath)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: 含 NUL 字符", ErrBadRelPath)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: 不允许绝对路径 %q", ErrBadRelPath, p)
	}
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("%w: 不允许反斜杠分隔符 %q", ErrBadRelPath, p)
	}
	if len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("%w: 不允许盘符前缀 %q", ErrBadRelPath, p)
	}
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "":
			return "", fmt.Errorf("%w: 含空的路径段 %q", ErrBadRelPath, p)
		case ".", "..":
			return "", fmt.Errorf("%w: 含相对路径段 %q（疑似目录穿越）", ErrBadRelPath, p)
		}
		out = append(out, s)
	}
	return strings.Join(out, "/"), nil
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
