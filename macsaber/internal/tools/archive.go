package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// 归归档格式（detectArchiveFormat 的返回值）。
const (
	arcZip = "zip"
	arcTGZ = "tgz"
	arcTar = "tar"
)

func archiveFormatOptions() []tool.Option {
	return []tool.Option{
		{Value: arcZip, Label: "ZIP（ditto，保留资源分叉）"},
		{Value: arcTGZ, Label: "TGZ（tar.gz）"},
	}
}

func extForFormat(format string) string {
	if format == arcTGZ {
		return ".tgz"
	}
	return ".zip"
}

// execTool 跑一条系统命令：超时与非 0 退出都返回真实原因（不谎报成功）。
func execTool(ctx context.Context, c *tool.Ctx, timeout time.Duration, name string, args ...string) (*execx.Result, error) {
	res := c.Exec.Run(ctx, timeout, name, args...)
	if res.TimedOut {
		return res, fmt.Errorf("%s 超过 %s 未完成，已终止", name, timeout.Round(time.Second))
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s 失败（退出码 %d）：%s", name, res.ExitCode, failureReason(res))
	}
	return res, nil
}

// ============================================================================
//  archive.list
// ============================================================================

type archiveList struct{}

func init() { Add(archiveList{}) }

func (archiveList) Meta() tool.Meta {
	_, hasTar := execx.LookPath("tar")
	_, hasUnzip := execx.LookPath("unzip")
	ok := hasTar && hasUnzip
	reason := ""
	if !ok {
		reason = "缺少系统命令：tar/unzip"
	}
	return tool.Meta{
		ID: "archive.list", Name: "查看压缩包", Category: "file", Icon: "archive",
		Summary:   "列出 zip/tar/tgz 里的条目，只读不改动。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 30,
		Params: []tool.Param{
			{Name: "path", Label: "压缩包", Type: tool.TypePath, Required: true,
				Help: "只能读允许的读根内的归档文件。"},
			{Name: "limit", Label: "列出条数", Type: tool.TypeNumber, Default: 20,
				Min: Num(1), Max: Num(200), Help: "只返回前 N 条，其余只计数。"},
		},
	}
}

func (archiveList) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("path")
	limit := in.Int("limit", 20)
	format, err := detectArchiveFormat(src)
	if err != nil {
		return nil, err
	}
	entries, err := listArchiveEntries(ctx, c, format, src)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, limit)
	for i, e := range entries {
		if i >= limit {
			break
		}
		it := map[string]any{"name": e.Name, "dir": e.Dir}
		if e.Size >= 0 {
			it["size"] = e.Size
		}
		items = append(items, it)
	}
	data := map[string]any{
		"format": format, "count": len(entries), "items": items,
		"listed": len(items), "truncated_list": len(entries) > len(items),
	}
	if format != arcZip {
		data["note"] = "tar 列表不含条目大小"
	}
	return &tool.Result{OK: true, Msg: fmt.Sprintf("共 %d 条，列出前 %d 条", len(entries), len(items)), Data: data}, nil
}

var unzipRowRe = regexp.MustCompile(`^\s*(\d+)\s+(\d{2}-\d{2}-\d{4})\s+(\d{2}:\d{2})\s+(.*\S)\s*$`)

// listArchiveEntries 用系统命令列条目：zip 走 unzip -l，tar/tgz 走 tar -tf。
func listArchiveEntries(ctx context.Context, c *tool.Ctx, format, src string) ([]arcEntry, error) {
	if format == arcZip {
		res, err := execTool(ctx, c, 30*time.Second, "unzip", "-l", src)
		if err != nil {
			return nil, err
		}
		return parseUnzipList(res.Stdout), nil
	}
	res, err := execTool(ctx, c, 60*time.Second, "tar", "-tf", src)
	if err != nil {
		return nil, err
	}
	out := []arcEntry{}
	for _, ln := range strings.Split(res.Stdout, "\n") {
		name := strings.TrimRight(ln, "\r")
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, arcEntry{Name: name, Size: -1, Dir: strings.HasSuffix(name, "/")})
	}
	return out, nil
}

func parseUnzipList(out string) []arcEntry {
	rows := []arcEntry{}
	started := false
	for _, ln := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !started {
			if strings.HasPrefix(trimmed, "----") {
				started = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "----") {
			break
		}
		m := unzipRowRe.FindStringSubmatch(ln)
		if len(m) != 5 {
			continue
		}
		size, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(m[4])
		rows = append(rows, arcEntry{Name: name, Size: size, Dir: strings.HasSuffix(name, "/")})
	}
	return rows
}

// ============================================================================
//  archive.create
// ============================================================================

type archiveCreate struct{}

func init() { Add(archiveCreate{}) }

func (archiveCreate) Meta() tool.Meta {
	_, hasDitto := execx.LookPath("ditto")
	_, hasTar := execx.LookPath("tar")
	ok := hasDitto && hasTar
	reason := ""
	if !ok {
		reason = "缺少系统命令：ditto/tar"
	}
	return tool.Meta{
		ID: "archive.create", Name: "新建压缩包", Category: "file", Icon: "archive",
		Summary: "把一个文件或目录打包成 zip 或 tgz。",
		Async:   true, Available: ok, UnavailableReason: reason, TimeoutSeconds: 1800,
		Params: []tool.Param{
			{Name: "input", Label: "文件或目录", Type: tool.TypePath, Required: true,
				Help: "只能打包允许的读根内的内容。"},
			{Name: "format", Label: "格式", Type: tool.TypeSelect, Required: true,
				Default: arcZip, Options: archiveFormatOptions(),
				Help: "zip 用 ditto，tgz 用 tar。"},
			{Name: "outpath", Label: "输出压缩包", Type: tool.TypeOutPath, Required: true,
				AllowMissing: true, Placeholder: "/Users/你/MacSaberFiles/out.zip",
				Help: "写到允许的可写根内；没写后缀会自动补。"},
			{Name: "overwrite", Label: "覆盖同名文件", Type: tool.TypeBool, Default: false,
				Help: "默认拒绝覆盖，已存在就报错。"},
		},
	}
}

func (archiveCreate) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	format := in.Str("format")
	dst := in.Path("outpath")
	if st, err := os.Lstat(dst); err == nil && st.IsDir() {
		return nil, fmt.Errorf("输出路径是目录，请给出压缩包文件名")
	}
	if filepath.Ext(dst) == "" {
		dst += extForFormat(format)
	}
	if _, err := os.Lstat(dst); err == nil && !in.Bool("overwrite", false) {
		return nil, fmt.Errorf("目标已存在，换一个输出名或勾选覆盖")
	}
	_, preErr := os.Lstat(dst)
	cleanup := func() {
		// 只删本次新建的残留：用户原有文件即使覆盖失败也不替他删。
		if preErr != nil {
			_ = os.Remove(dst)
		}
	}
	base := filepath.Base(src)
	if base == "/" || base == "." || base == string(os.PathSeparator) {
		return nil, errors.New("不能把文件系统根目录打包")
	}

	c.Logf("打包 %s → %s（%s）", base, filepath.Base(dst), format)
	c.Progress(10, "开始打包")
	var err error
	if format == arcTGZ {
		// tar 侧用 -C 父目录 + 条目名，避免把绝对路径写进归档。
		_, err = execTool(ctx, c, 30*time.Minute, "tar", "-czf", dst, "-C", filepath.Dir(src), base)
	} else {
		_, err = execTool(ctx, c, 30*time.Minute, "ditto",
			"-c", "-k", "--sequesterRsrc", "--keepParent", src, dst)
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	st, statErr := os.Stat(dst)
	if statErr != nil || st.Size() == 0 {
		// ditto/tar 可能"退出码 0 却没产出"，必须如实报错（坑 F1）。
		cleanup()
		return nil, fmt.Errorf("命令报告成功但没有产出压缩包")
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已写出 %s（%s）", filepath.Base(dst), humanSize(st.Size())),
		Data: map[string]any{
			"output": dst, "format": format, "size": st.Size(), "source": src,
		},
		Files: []tool.File{{Name: filepath.Base(dst), Path: dst, Size: st.Size()}},
	}, nil
}

// ============================================================================
//  archive.extract
// ============================================================================

type archiveExtract struct{}

func init() { Add(archiveExtract{}) }

func (archiveExtract) Meta() tool.Meta {
	_, hasDitto := execx.LookPath("ditto")
	_, hasTar := execx.LookPath("tar")
	ok := hasDitto && hasTar
	reason := ""
	if !ok {
		reason = "缺少系统命令：ditto/tar"
	}
	return tool.Meta{
		ID: "archive.extract", Name: "解压到新目录", Category: "file", Icon: "archive",
		Summary: "解压 zip/tar/tgz，先查越界条目再落盘。",
		Async:   true, Available: ok, UnavailableReason: reason, TimeoutSeconds: 1800,
		Params: []tool.Param{
			{Name: "input", Label: "压缩包", Type: tool.TypePath, Required: true,
				Help: "只能读允许的读根内的归档文件。"},
			{Name: "outpath", Label: "解压到", Type: tool.TypeOutPath, Required: true,
				AllowMissing: true, Placeholder: "/Users/你/MacSaberFiles/newdir",
				Help: "写根内的新建子目录；已存在且非空会被拒。"},
		},
	}
}

func (archiveExtract) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("input")
	target := in.Path("outpath")
	format, err := detectArchiveFormat(src)
	if err != nil {
		return nil, err
	}
	// 先列表后解压：条目在落盘前逐条校验，出现越界/链接即整体拒绝。
	entries, err := validateArchive(format, src)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("归档里没有任何条目")
	}
	if err := prepareNewDir(target); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(target, ".msx-")
	if err != nil {
		return nil, fmt.Errorf("创建临时解压目录失败：%v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	c.Logf("校验通过 %d 条，开始解压 %s", len(entries), filepath.Base(src))
	c.Progress(30, "解压")
	var runErr error
	if format == arcZip {
		_, runErr = execTool(ctx, c, 30*time.Minute, "ditto", "-x", "-k", src, tmp)
	} else {
		_, runErr = execTool(ctx, c, 30*time.Minute, "tar", "-xf", src, "-C", tmp)
	}
	if runErr != nil {
		return nil, runErr
	}
	files, err := verifyExtractedTree(tmp)
	if err != nil {
		return nil, err
	}
	if files == 0 {
		return nil, errors.New("解压完成但没有任何文件产出")
	}
	names, err := moveUp(tmp, target)
	if err != nil {
		return nil, err
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("已解压 %d 个文件到 %s", files, filepath.Base(target)),
		Data: map[string]any{
			"outdir": target, "format": format, "entries": len(entries),
			"files": files, "top_level": names,
		},
	}, nil
}

// ============================================================================
//  归档条目校验（zip-slip / 符号链接门禁）
// ============================================================================

type arcEntry struct {
	Name    string
	Size    int64
	Dir     bool
	Symlink bool
	Special bool
}

func detectArchiveFormat(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("打不开归档：%v", err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case len(head) >= 4 && head[0] == 'P' && head[1] == 'K' &&
		(head[2] == 3 || head[2] == 5 || head[2] == 7):
		return arcZip, nil
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return arcTGZ, nil
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return arcTar, nil
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".zip":
		return arcZip, nil
	case ".tgz", ".gz":
		return arcTGZ, nil
	case ".tar":
		return arcTar, nil
	}
	return "", errors.New("认不出归档格式（只支持 zip/tar/tgz）")
}

// validateArchive 用 Go 标准库精确读取条目名（不解析命令行输出），逐条过门禁。
func validateArchive(format, src string) ([]arcEntry, error) {
	var (
		entries []arcEntry
		err     error
	)
	if format == arcZip {
		entries, err = zipEntries(src)
	} else {
		entries, err = tarEntries(src, format)
	}
	if err != nil {
		return nil, fmt.Errorf("读取归档目录失败：%v", err)
	}
	for _, e := range entries {
		if reason := unsafeEntryName(e.Name); reason != "" {
			return nil, fmt.Errorf("归档含不安全条目 %q（%s），已整体拒绝", e.Name, reason)
		}
		if e.Symlink {
			return nil, fmt.Errorf("归档含符号链接条目 %q，已整体拒绝", e.Name)
		}
		if e.Special {
			return nil, fmt.Errorf("归档含设备/管道条目 %q，已整体拒绝", e.Name)
		}
	}
	return entries, nil
}

// unsafeEntryName 返回拒绝原因；空串表示安全。判断只做在路径分量上（不拼 shell）。
func unsafeEntryName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "条目名为空"
	}
	if strings.ContainsRune(name, 0) {
		return "含 NUL"
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "绝对路径"
	}
	// 只挡 Windows 盘符（C:/…）；冒号本身在 macOS 文件名里合法。
	if len(name) >= 3 && name[1] == ':' && isASCIILetter(name[0]) && (name[2] == '/' || name[2] == '\\') {
		return "盘符路径"
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "含 .. 回溯"
		}
	}
	return ""
}

func isASCIILetter(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func zipEntries(src string) ([]arcEntry, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out := make([]arcEntry, 0, len(zr.File))
	for _, f := range zr.File {
		mode := f.Mode()
		out = append(out, arcEntry{
			Name:    f.Name,
			Size:    int64(f.UncompressedSize64),
			Dir:     f.FileInfo().IsDir(),
			Symlink: mode&os.ModeSymlink != 0,
			Special: mode&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice) != 0,
		})
	}
	return out, nil
}

func tarEntries(src, format string) ([]arcEntry, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if format == arcTGZ {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	tr := tar.NewReader(r)
	out := []arcEntry{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		e := arcEntry{Name: hdr.Name, Size: hdr.Size}
		switch hdr.Typeflag {
		case tar.TypeDir:
			e.Dir = true
		case tar.TypeSymlink, tar.TypeLink:
			e.Symlink = true
		case tar.TypeReg, tar.TypeRegA:
		default:
			e.Special = true
		}
		out = append(out, e)
	}
	return out, nil
}

// prepareNewDir 只接受不存在或空目录的目标，避免把解压结果混进用户已有目录。
func prepareNewDir(target string) error {
	st, err := os.Lstat(target)
	if err == nil {
		if !st.IsDir() {
			return fmt.Errorf("目标已存在且不是目录：%s", filepath.Base(target))
		}
		ents, rerr := os.ReadDir(target)
		if rerr != nil {
			return fmt.Errorf("读取目标目录失败：%v", rerr)
		}
		if len(ents) > 0 {
			return errors.New("目标目录已存在且非空，请换一个新建目录")
		}
		return nil
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("创建目标目录失败：%v", err)
	}
	return nil
}

// verifyExtractedTree 复核落盘结果：不越界、不含符号链接，并统计文件数。
func verifyExtractedTree(root string) (int, error) {
	files := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("解压产物逃出目标目录：%s", d.Name())
		}
		info, ierr := os.Lstat(p)
		if ierr != nil {
			return ierr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("解压产物含符号链接：%s", rel)
		}
		if !info.IsDir() {
			files++
		}
		return nil
	})
	return files, err
}

// moveUp 把临时目录里的顶层条目挪到目标目录，并复核每个落点仍在目标内。
func moveUp(tmp, target string) ([]string, error) {
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return nil, fmt.Errorf("读取解压产物失败：%v", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		dst := filepath.Join(target, e.Name())
		rel, rerr := filepath.Rel(target, dst)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("解压产物落点越界：%s", e.Name())
		}
		if err := os.Rename(filepath.Join(tmp, e.Name()), dst); err != nil {
			return nil, fmt.Errorf("移动解压产物失败：%v", err)
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// ============================================================================
//  xattr.list
// ============================================================================

type xattrList struct{}

func init() { Add(xattrList{}) }

func (xattrList) Meta() tool.Meta {
	_, ok := execx.LookPath("xattr")
	reason := ""
	if !ok {
		reason = "缺少系统命令 xattr"
	}
	return tool.Meta{
		ID: "xattr.list", Name: "扩展属性", Category: "file", Icon: "tag",
		Summary:   "查看文件的扩展属性与隔离标记，只读。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 20,
		Params: []tool.Param{
			{Name: "path", Label: "文件", Type: tool.TypePath, Required: true,
				Help: "只能读允许的读根内的文件。"},
		},
	}
}

func (xattrList) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("path")
	res, err := execTool(ctx, c, 15*time.Second, "xattr", "-l", src)
	if err != nil {
		return nil, err
	}
	attrs, quarantined := parseXattrList(res.Stdout)
	data := map[string]any{
		"path": src, "count": len(attrs), "attributes": attrs, "truncated": res.TruncOut,
	}
	if quarantined != nil {
		data["quarantine"] = quarantined
	}
	msg := "没有扩展属性"
	if len(attrs) > 0 {
		msg = fmt.Sprintf("共 %d 个扩展属性", len(attrs))
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

func parseXattrList(out string) ([]map[string]any, map[string]any) {
	attrs := []map[string]any{}
	var quarantine map[string]any
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		i := strings.Index(ln, ": ")
		name, val := ln, ""
		if i >= 0 {
			name, val = ln[:i], ln[i+2:]
		} else if j := strings.IndexByte(ln, ':'); j >= 0 {
			name, val = ln[:j], ln[j+1:]
		}
		truncated := false
		if len(val) > 300 {
			val = val[:300]
			truncated = true
		}
		it := map[string]any{"name": strings.TrimSpace(name), "value": val}
		if truncated {
			it["truncated"] = true
		}
		attrs = append(attrs, it)
		if strings.TrimSpace(name) == "com.apple.quarantine" && quarantine == nil {
			quarantine = parseQuarantine(val)
		}
	}
	return attrs, quarantine
}

// quarantine 值形如 0081;0;Safari;UUID，第 3 段是写入程序名。
func parseQuarantine(val string) map[string]any {
	parts := strings.Split(val, ";")
	out := map[string]any{"raw": val, "present": true}
	if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
		out["flags"] = strings.TrimSpace(parts[0])
	}
	if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
		out["agent"] = strings.TrimSpace(parts[2])
	}
	return out
}
