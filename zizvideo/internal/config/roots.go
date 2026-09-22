package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Roots is the live allow-root list. One atomic snapshot is the single source
// of truth at runtime, so the scanner, the API and the CLI all read the same
// view without taking a lock on the hot path.
type Roots struct {
	mu    sync.Mutex
	fpath string
	snap  atomic.Value // []string; always a fresh copy
}

// NewRoots builds a Roots view; fpath may be "" when no config file is in use.
func NewRoots(fpath string, initial []string) *Roots {
	r := &Roots{fpath: fpath}
	r.publish(cleanRoots(initial))
	return r
}

// List returns a copy of the current snapshot.
func (r *Roots) List() []string {
	v, _ := r.snap.Load().([]string)
	out := make([]string, len(v))
	copy(out, v)
	return out
}

// Path is the config file this view persists to; "" means no file.
func (r *Roots) Path() string { return r.fpath }

// FileBacked reports whether add/remove can persist at all.
func (r *Roots) FileBacked() bool { return r.fpath != "" }

// EnvOverridden reports whether ZV_MEDIA_ALLOW_ROOTS replaces the file list.
func (r *Roots) EnvOverridden() bool { return os.Getenv("ZV_MEDIA_ALLOW_ROOTS") != "" }

func (r *Roots) publish(roots []string) {
	cp := make([]string, len(roots))
	copy(cp, roots)
	r.snap.Store(cp)
}

// swap rewrites the file first, then adopts the list. Nothing changes in
// memory unless the write and the read-back both succeeded (坑 2).
func (r *Roots) swap(next []string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := cleanRoots(next)
	// 只有"相对当前列表新增"的条目要求存在：盘已拔掉的旧根不许连坐整次写入。
	known := map[string]bool{}
	for _, old := range r.List() {
		known[filepath.Clean(old)] = true
	}
	added := make([]string, 0)
	for _, item := range want {
		if !known[filepath.Clean(item)] {
			added = append(added, item)
		}
	}
	if err := SetRoots(r.fpath, want, added); err != nil {
		return nil, err
	}
	back, err := RootsFromFile(r.fpath)
	if err != nil {
		return nil, fmt.Errorf("回读配置失败: %w", err)
	}
	if !sameList(back, want) {
		return nil, fmt.Errorf("回读配置与写入不一致，未生效")
	}
	r.publish(want)
	return want, nil
}

// Add appends one root and persists it. Only this new entry must exist: a root
// that went offline (盘拔了) must never block adding another one.
func (r *Roots) Add(root string) ([]string, error) {
	if !r.FileBacked() {
		return nil, fmt.Errorf("没有配置文件，无法写入（用 --config 或 ZV_CONFIG 指定）")
	}
	if _, err := ValidateAllowRoot(root); err != nil {
		return nil, fmt.Errorf("这条允许根不可用，未保存: %s——%w", root, err)
	}
	return r.swap(append(r.List(), root))
}

// Remove drops one root and persists the rest; the last root cannot go.
func (r *Roots) Remove(root string) ([]string, error) {
	if !r.FileBacked() {
		return nil, fmt.Errorf("没有配置文件，无法写入（用 --config 或 ZV_CONFIG 指定）")
	}
	next := make([]string, 0)
	removed := false
	for _, existing := range r.List() {
		if existing == root {
			removed = true
			continue
		}
		next = append(next, existing)
	}
	if !removed {
		return nil, fmt.Errorf("该路径不在允许根里")
	}
	if len(next) == 0 {
		return nil, fmt.Errorf("至少要保留一个允许根，先添加一个可用目录再删")
	}
	return r.swap(next)
}

func cleanRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, raw := range roots {
		c := filepath.Clean(strings.TrimSpace(raw))
		if c == "" || c == "." || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RootsFromFile reads media_allow_roots out of an existing config file.
func RootsFromFile(path string) ([]string, error) {
	raw, err := readConfigMap(path)
	if err != nil {
		return nil, err
	}
	return rootsOf(raw)
}

// SetRoots atomically replaces media_allow_roots in the config file: temp file
// in the same directory (0600) + rename, so a crash never leaves half a file.
// 只有 added（相对磁盘上旧列表新增的条目）要求存在：旧根脱机时加/删别的根
// 不许整次失败，否则用户只能手改 config.json（用户 2026-09-22 报的坑）。
func SetRoots(path string, roots []string, added []string) error {
	if path == "" {
		return fmt.Errorf("没有配置文件路径")
	}
	clean := cleanRoots(roots)
	if len(clean) == 0 {
		return fmt.Errorf("media_allow_roots 不能为空")
	}
	raw, err := readConfigMap(path)
	if err != nil {
		return err
	}
	// 结构性问题照旧拒绝（相对路径/空白），但不做存在性检查。
	for _, root := range clean {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("media_allow_roots 必须是绝对路径: %s", root)
		}
	}
	// A listed root must be an existing, readable directory; kept ones may be
	// offline and are only reported as 不可用 by the status API.
	for _, root := range added {
		if _, err := ValidateAllowRoot(root); err != nil {
			return fmt.Errorf("这条允许根不可用，未保存: %s——%w", root, err)
		}
	}
	if err := replaceRoots(raw, clean); err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

func readConfigMap(path string) (map[string]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	return raw, nil
}

func rootsOf(raw map[string]json.RawMessage) ([]string, error) {
	roots := []string{}
	if v, ok := raw["media_allow_roots"]; ok {
		if err := json.Unmarshal(v, &roots); err != nil {
			return nil, fmt.Errorf("media_allow_roots 必须是字符串数组")
		}
	}
	return roots, nil
}

// replaceRoots rewrites that one key only, so every other field keeps its
// original spelling and order (坑 1：不许重排用户手写的配置).
func replaceRoots(raw map[string]json.RawMessage, roots []string) error {
	b, err := json.Marshal(roots)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, b, "", "  "); err != nil {
		return err
	}
	raw["media_allow_roots"] = json.RawMessage(indented.Bytes())
	return nil
}

func writeAtomic(path string, raw map[string]json.RawMessage) error {
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".zizvideo-config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// ResolvePath returns the comparison key for a config path: the real path when
// it exists, otherwise the cleaned spelling. Removal must stay possible for a
// root that is currently offline (磁盘没插), so this never fails.
func ResolvePath(path string) string {
	clean := filepath.Clean(strings.TrimSpace(path))
	if real, err := filepath.EvalSymlinks(clean); err == nil && filepath.IsAbs(real) {
		return real
	}
	return clean
}

// ValidateAllowRoot checks a candidate allow root and returns the canonical form
// to store: absolute, filepath.Clean-stable, and symlink-resolved so that a
// link pointing elsewhere is recorded (and later re-checked) at its real path.
func ValidateAllowRoot(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("路径含非法字符")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("必须是绝对路径")
	}
	if filepath.Clean(raw) != raw {
		return "", fmt.Errorf("路径含冗余片段，请用规范写法")
	}
	st, err := os.Stat(raw)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("路径不存在（外接盘拔了或没挂载？）")
		}
		return "", fmt.Errorf("路径不可访问（权限不足？）")
	}
	if !st.IsDir() {
		return "", fmt.Errorf("路径不是目录")
	}
	f, err := os.Open(raw)
	if err != nil {
		return "", fmt.Errorf("路径不可读")
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("路径不可读")
	}
	real, err := filepath.EvalSymlinks(raw)
	if err != nil || !filepath.IsAbs(real) {
		return "", fmt.Errorf("路径无法解析")
	}
	return real, nil
}

// EnsureDefaultMovies creates $HOME/Movies (0755) when missing, so the default
// allow root can be added with one click instead of failing with "路径不存在".
// Returns the created path ("" = nothing to do). It only ever touches that one
// system folder under $HOME — never any external volume.
func EnsureDefaultMovies() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(os.PathSeparator) {
		return "", fmt.Errorf("无法确定当前用户家目录")
	}
	dir := filepath.Join(home, "Movies")
	if st, serr := os.Stat(dir); serr == nil {
		if st.IsDir() {
			return "", nil
		}
		return "", fmt.Errorf("%s 已存在但不是目录", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
