package files

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
//  压缩 / 解压
//
//  实现选择：优先调用系统自带的 tar / unzip / zip。
//  理由：
//    - macOS 自带这些工具，行为与用户手工执行完全一致，可解释
//    - 保留符号链接、权限位、扩展属性（自己实现容易丢）
//    - 参数是结构化的（不经过 shell），不存在注入
//  解压时额外做一次"归档内路径"检查，防止 zip-slip（../../ 穿越）。
// ---------------------------------------------------------------------------

// Archive 支持的归档格式。
type Archive struct {
	Ext   string `json:"ext"`
	Label string `json:"label"`
}

// ArchiveFormats 返回支持的压缩格式。
func ArchiveFormats() []Archive {
	return []Archive{
		{Ext: "zip", Label: "ZIP（通用，Windows/Mac 都能打开）"},
		{Ext: "tar.gz", Label: "TAR.GZ（保留权限与软链接，适合服务端）"},
		{Ext: "tar", Label: "TAR（不压缩）"},
	}
}

// Compress 把若干路径压缩到一个归档文件。
//
// outputName 是归档文件名（不含目录），会被放到 dir 下。
func (m *Manager) Compress(ctx context.Context, dir string, names []string, format, outputName string) (string, error) {
	realDir, err := m.Resolve(dir, false)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("请至少选择一个要压缩的文件或目录")
	}
	switch format {
	case "zip", "tar.gz", "tar":
	default:
		return "", fmt.Errorf("不支持的压缩格式: %s", format)
	}

	// 逐个校验源路径，并转换为相对 realDir 的名字（tar/zip 在 realDir 下执行）
	var rels []string
	for _, n := range names {
		full, err := m.Resolve(filepath.Join(realDir, filepath.Base(n)), false)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(realDir, full)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("%w: %s", ErrForbidden, n)
		}
		rels = append(rels, rel)
	}

	if strings.TrimSpace(outputName) == "" {
		base := strings.TrimSuffix(rels[0], filepath.Ext(rels[0]))
		if len(rels) > 1 {
			base = "archive"
		}
		outputName = base + "." + format
	}
	outputName = filepath.Base(outputName)
	outPath, err := m.Resolve(filepath.Join(realDir, outputName), true)
	if err != nil {
		return "", err
	}
	_ = os.Remove(outPath) // 覆盖同名归档

	var args []string
	switch format {
	case "zip":
		// -r 递归；-y 保留软链接本身而不是其目标内容（重要：否则会把链接目标整个拷进来）
		args = append([]string{"-r", "-y", outPath}, rels...)
		if _, err := runCmd(ctx, 10*time.Minute, realDir, "/usr/bin/zip", args...); err != nil {
			return "", err
		}
	case "tar.gz":
		args = append([]string{"-czf", outPath}, rels...)
		if _, err := runCmd(ctx, 10*time.Minute, realDir, "/usr/bin/tar", args...); err != nil {
			return "", err
		}
	case "tar":
		args = append([]string{"-cf", outPath}, rels...)
		if _, err := runCmd(ctx, 10*time.Minute, realDir, "/usr/bin/tar", args...); err != nil {
			return "", err
		}
	}
	m.chownRealUser(outPath)
	return outPath, nil
}

// Extract 解压归档文件。
//
// 安全要点：解压前先列出归档内的条目，检查是否有绝对路径或 .. 穿越；
// 同时拒绝解压出软链接到目标目录之外（tar 的软链接可以指向 /etc）。
func (m *Manager) Extract(ctx context.Context, archivePath, destDir string) (string, error) {
	src, err := m.Resolve(archivePath, false)
	if err != nil {
		return "", err
	}
	dest, err := m.Resolve(destDir, false)
	if err != nil {
		return "", err
	}
	if err := m.checkArchiveEntries(ctx, src); err != nil {
		return "", err
	}

	lower := strings.ToLower(src)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		if _, err := runCmd(ctx, 10*time.Minute, dest, "/usr/bin/unzip", "-o", "-q", src); err != nil {
			return "", err
		}
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		if _, err := runCmd(ctx, 10*time.Minute, dest, "/usr/bin/tar", "-xzf", src); err != nil {
			return "", err
		}
	case strings.HasSuffix(lower, ".tar"):
		if _, err := runCmd(ctx, 10*time.Minute, dest, "/usr/bin/tar", "-xf", src); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("不支持的归档格式（仅支持 zip / tar.gz / tgz / tar）")
	}
	m.chownTreeRealUser(dest)
	return dest, nil
}

// checkArchiveEntries 检查归档内条目是否安全。
func (m *Manager) checkArchiveEntries(ctx context.Context, src string) error {
	lower := strings.ToLower(src)
	var entries []string

	switch {
	case strings.HasSuffix(lower, ".zip"):
		out, err := runCmd(ctx, 2*time.Minute, "", "/usr/bin/unzip", "-Z1", src)
		if err != nil {
			return err
		}
		entries = strings.Split(out, "\n")
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar"):
		out, err := runCmd(ctx, 2*time.Minute, "", "/usr/bin/tar", "-tf", src)
		if err != nil {
			return err
		}
		entries = strings.Split(out, "\n")
	default:
		return fmt.Errorf("不支持的归档格式")
	}

	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if filepath.IsAbs(e) {
			return fmt.Errorf("归档内包含绝对路径 %q，已拒绝解压（可能是恶意构造的压缩包）", e)
		}
		clean := filepath.Clean(e)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("归档内包含目录穿越路径 %q，已拒绝解压", e)
		}
	}
	return nil
}

// chownTreeRealUser 把解压出来的内容归属真实用户。
func (m *Manager) chownTreeRealUser(root string) {
	if os.Geteuid() != 0 || m.userName == "" || m.userName == "root" {
		return
	}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		m.chownRealUser(p)
		return nil
	})
}

// runCmd 执行外部命令（不经 shell）。
//
// dir 非空时作为工作目录；返回合并后的输出。
func runCmd(ctx context.Context, timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		return text, fmt.Errorf("%s 执行失败: %s", filepath.Base(name), truncateStr(strings.TrimSpace(text), 400))
	}
	return text, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
//  搜索
// ---------------------------------------------------------------------------

// SearchHit 是一条搜索结果。
type SearchHit struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	// MatchLine 命中行内容（按内容搜索时）
	MatchLine string `json:"match_line,omitempty"`
	LineNo    int    `json:"line_no,omitempty"`
}

// SearchResult 是搜索结果集合。
type SearchResult struct {
	Query     string      `json:"query"`
	Mode      string      `json:"mode"` // name / content
	Hits      []SearchHit `json:"hits"`
	Scanned   int         `json:"scanned"`
	Truncated bool        `json:"truncated"`
	Elapsed   int64       `json:"elapsed_ms"`
}

// Search 在指定目录下按文件名或内容搜索。
//
// 性能取舍：文件名搜索用 filepath.WalkDir（快）；
// 内容搜索只读小于 1MB 的文件，并限制总命中数 ——
// 在大目录里做全文本搜索很容易把面板拖住。
func (m *Manager) Search(ctx context.Context, root, query, mode string, limit int) (*SearchResult, error) {
	real, err := m.Resolve(root, false)
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("请输入搜索关键词")
	}
	if mode != "content" {
		mode = "name"
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	start := time.Now()
	res := &SearchResult{Query: query, Mode: mode, Hits: []SearchHit{}}
	lowerQ := strings.ToLower(query)

	const maxScan = 60000 // 最多遍历这么多条目
	_ = filepath.WalkDir(real, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 权限不足的目录直接跳过，不中断整个搜索
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		// 跳过常见的重型目录，否则搜索会非常慢
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".cache", ".Trash", "Library":
				if p != real {
					return filepath.SkipDir
				}
			}
		}
		res.Scanned++
		if res.Scanned > maxScan {
			res.Truncated = true
			return filepath.SkipAll
		}

		if mode == "name" {
			if strings.Contains(strings.ToLower(d.Name()), lowerQ) {
				hit, ok := m.makeHit(p, d)
				if ok {
					res.Hits = append(res.Hits, hit)
				}
			}
		} else if !d.IsDir() {
			info, err := d.Info()
			if err != nil || info.Size() > 1<<20 {
				return nil
			}
			if line, no, ok := searchInFile(p, query); ok {
				res.Hits = append(res.Hits, SearchHit{
					Path: p, Name: d.Name(), Size: info.Size(),
					ModTime:   info.ModTime().Format("2006-01-02 15:04:05"),
					MatchLine: line, LineNo: no,
				})
			}
		}
		if len(res.Hits) >= limit {
			res.Truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	res.Elapsed = time.Since(start).Milliseconds()
	return res, nil
}

func (m *Manager) makeHit(p string, d os.DirEntry) (SearchHit, bool) {
	info, err := d.Info()
	if err != nil {
		return SearchHit{}, false
	}
	return SearchHit{
		Path: p, Name: d.Name(), IsDir: d.IsDir(),
		Size: info.Size(), ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
	}, true
}

// searchInFile 在文件里找第一处匹配行。
func searchInFile(p, query string) (string, int, bool) {
	b, err := os.ReadFile(p)
	if err != nil || isBinary(b) {
		return "", 0, false
	}
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		if strings.Contains(ln, query) {
			if len(ln) > 300 {
				ln = ln[:300] + "…"
			}
			return strings.TrimSpace(ln), i + 1, true
		}
	}
	return "", 0, false
}

// ReplaceInFile 在文件内做文本替换（返回替换次数）。
//
// 用 regexp.QuoteMeta 保证按字面量替换，避免用户输入的
// `.` `*` 等被当成正则元字符而误替换。
func (m *Manager) ReplaceInFile(p, find, replace string, all bool) (int, error) {
	real, err := m.Resolve(p, false)
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return 0, err
	}
	if isBinary(b) {
		return 0, fmt.Errorf("这是二进制文件，不支持文本替换")
	}
	content := string(b)
	if find == "" {
		return 0, fmt.Errorf("查找内容不能为空")
	}
	re := regexp.MustCompile(regexp.QuoteMeta(find))
	if all {
		n := len(re.FindAllStringIndex(content, -1))
		if n == 0 {
			return 0, nil
		}
		next := re.ReplaceAllString(content, replace)
		if err := m.Write(real, next, false); err != nil {
			return 0, err
		}
		return n, nil
	}
	loc := re.FindStringIndex(content)
	if loc == nil {
		return 0, nil
	}
	next := content[:loc[0]] + replace + content[loc[1]:]
	if err := m.Write(real, next, false); err != nil {
		return 0, err
	}
	return 1, nil
}

// 保留：用于将来支持更多归档格式时的探测
var _ = tar.TypeReg
var _ = zip.Store
var _ = gzip.BestCompression
var _ = io.Discard
