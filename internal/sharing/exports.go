package sharing

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// Export 是 /etc/exports 里的一条导出（我们只解析行，不改语义）。
type Export struct {
	Path     string   `json:"path"`
	ReadOnly bool     `json:"read_only"`
	Options  []string `json:"options"`
	// Managed 表示这一行是本面板生成的形态（路径 + 我们那组选项）。
	Managed bool   `json:"managed"`
	Line    string `json:"line"`
}

// exportLine 是一行原文及其解析结果。
type exportLine struct {
	Raw     string
	Comment bool // 空行或 # 注释
	Path    string
	Options []string
}

func (e *Executor) mapUser() string {
	if u := strings.TrimSpace(e.User); u != "" {
		return u
	}
	if cu, err := user.Current(); err == nil && cu.Username != "" {
		return cu.Username
	}
	return "root"
}

// canonicalOptions 是本面板写出的导出选项。macOS `man exports` 语义：
//   - `-ro` 只读（默认读写）；
//   - `-mapall=<用户>` 把所有客户端 uid（含 root）映射成本机真实用户，
//     否则文件会以 nobody/-2 落盘，用户在自己的共享里写不进去。
func canonicalOptions(readOnly bool, mapUser string) []string {
	opts := []string{}
	if readOnly {
		opts = append(opts, "-ro")
	}
	return append(opts, "-mapall="+mapUser)
}

func sameOptions(a, b []string) bool {
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

// isManagedLine 判断一行是不是本面板生成的形态（读写/只读两种都算）。
func isManagedLine(ln exportLine, path, mapUser string) bool {
	if ln.Comment || ln.Path != path {
		return false
	}
	return sameOptions(ln.Options, canonicalOptions(false, mapUser)) ||
		sameOptions(ln.Options, canonicalOptions(true, mapUser))
}

// parseExportLines 按行解析。解析不了就报错 —— 调用方**绝不**在报错时改文件。
//
// 判为解析失败的形态：非注释行的第一个字段不是绝对路径、含引号（带引号的路径我们
// 不重写，怕改坏）、含控制字符。
func parseExportLines(text, mapUser string) ([]exportLine, error) {
	var out []exportLine
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // 尾部换行不算一行
	}
	for i, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			out = append(out, exportLine{Raw: raw, Comment: true})
			continue
		}
		if strings.ContainsAny(raw, "\r\x00") {
			return nil, errors.New("第 " + itoa(i+1) + " 行含控制字符")
		}
		if strings.ContainsAny(raw, `"'`) {
			return nil, errors.New("第 " + itoa(i+1) + " 行含引号，本面板不重写带引号的导出行")
		}
		fields := strings.Fields(t)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
			return nil, errors.New("第 " + itoa(i+1) + " 行不是绝对路径：" + tail(t, 80))
		}
		out = append(out, exportLine{Raw: raw, Path: fields[0], Options: fields[1:]})
	}
	return out, nil
}

// readExports 读导出表；不存在按空文件处理（existed=false）。
func (e *Executor) readExports() (raw []byte, existed bool, err error) {
	b, err := os.ReadFile(e.exportsPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

// Exports 列出当前导出项。known=false 表示读不到/解析不了（界面标「未复核」）。
func (e *Executor) Exports(ctx context.Context) (list []Export, known bool, errMsg string) {
	raw, _, err := e.readExports()
	if err != nil {
		return nil, false, "读 " + e.exportsPath() + " 失败：" + err.Error()
	}
	lines, perr := parseExportLines(string(raw), e.mapUser())
	if perr != nil {
		return nil, false, e.exportsPath() + " 解析失败（原文件未动）：" + perr.Error()
	}
	out := []Export{}
	for _, ln := range lines {
		if ln.Comment {
			continue
		}
		out = append(out, Export{
			Path: ln.Path, ReadOnly: hasOpt(ln.Options, "-ro"), Options: ln.Options,
			Managed: isManagedLine(ln, ln.Path, e.mapUser()), Line: ln.Raw,
		})
	}
	return out, true, ""
}

func hasOpt(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}

// backupExports 把原文备到 <exports>.zp-bak-<时间戳>，返回备份路径。
func (e *Executor) backupExports(raw []byte) (string, error) {
	for i := 0; i < 5; i++ {
		p := e.exportsPath() + ".zp-bak-" + time.Now().Format("20060102-150405.000000000")
		if i > 0 {
			p += "-" + itoa(i)
		}
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			if werr := os.WriteFile(p, raw, 0o644); werr != nil {
				return "", werr
			}
			return p, nil
		}
	}
	return "", errors.New("备份文件名一直撞车")
}

// atomicWrite 写同目录临时文件再 rename（原子替换；失败不碰原文件）。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".zp-exports-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // rename 成功后这里删的是不存在的名字
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// renderExports 生成新文件内容：保留所有原有行（顺序不变），
// 只删掉"目标路径 + 本面板形态"的那一行，再把新行追加到末尾。
func renderExports(lines []exportLine, dropPath string, add *string, mapUser string) string {
	keep := make([]string, 0, len(lines)+1)
	for _, ln := range lines {
		if dropPath != "" && isManagedLine(ln, dropPath, mapUser) {
			continue
		}
		keep = append(keep, ln.Raw)
	}
	if add != nil {
		keep = append(keep, *add)
	}
	return strings.Join(keep, "\n") + "\n"
}

func canonicalLine(path string, readOnly bool, mapUser string) string {
	return strings.Join(append([]string{path}, canonicalOptions(readOnly, mapUser)...), " ")
}

// restore 回到写之前的字节（best-effort；失败也不掩盖原始错误）。
func (e *Executor) restore(raw []byte, existed bool, backup string) {
	if existed {
		if backup != "" {
			if b, err := os.ReadFile(backup); err == nil {
				_ = atomicWrite(e.exportsPath(), b)
				return
			}
		}
		_ = atomicWrite(e.exportsPath(), raw)
		return
	}
	_ = os.Remove(e.exportsPath()) // 原来没有这个文件 ⇒ 回到没有
}

// confirmExports 回读文件，确认目标行是否存在（add=true 要存在，add=false 要不存在）。
func (e *Executor) confirmExports(path string, readOnly bool, add bool) (bool, string) {
	raw, _, err := e.readExports()
	if err != nil {
		return false, "回读 " + e.exportsPath() + " 失败：" + err.Error()
	}
	lines, perr := parseExportLines(string(raw), e.mapUser())
	if perr != nil {
		return false, "回读解析失败：" + perr.Error()
	}
	found := false
	for _, ln := range lines {
		if ln.Comment || ln.Path != path {
			continue
		}
		if sameOptions(ln.Options, canonicalOptions(readOnly, e.mapUser())) {
			found = true
		}
	}
	if found != add {
		if add {
			return false, "回读 " + e.exportsPath() + " 里没有刚写入的导出行（退出码不算数）"
		}
		return false, "回读 " + e.exportsPath() + " 里那一行还在（退出码不算数）"
	}
	return true, ""
}

// confirmExportAbsent 回读文件，确认某路径下**没有**本面板形态的导出行。
func (e *Executor) confirmExportAbsent(path string) (bool, string) {
	raw, _, err := e.readExports()
	if err != nil {
		return false, "回读 " + e.exportsPath() + " 失败：" + err.Error()
	}
	lines, perr := parseExportLines(string(raw), e.mapUser())
	if perr != nil {
		return false, "回读解析失败：" + perr.Error()
	}
	for _, ln := range lines {
		if isManagedLine(ln, path, e.mapUser()) {
			return false, "回读 " + e.exportsPath() + " 里那一行还在（退出码不算数）"
		}
	}
	return true, ""
}

// AddExport 增/改一条 NFS 导出：备份 → 原子替换 → nfsd update → 回读确认。
func (e *Executor) AddExport(ctx context.Context, path string, readOnly bool) Result {
	res := Result{Action: "export_add"}
	raw, existed, err := e.readExports()
	if err != nil {
		res.Error = "读 " + e.exportsPath() + " 失败，未改动：" + err.Error()
		return res
	}
	mapUser := e.mapUser()
	lines, perr := parseExportLines(string(raw), mapUser)
	if perr != nil {
		res.Error = e.exportsPath() + " 解析失败，未改动原文件：" + perr.Error()
		return res
	}
	backup := ""
	if existed {
		backup, err = e.backupExports(raw)
		if err != nil {
			res.Error = "备份失败，未改动原文件：" + err.Error()
			return res
		}
	}
	line := canonicalLine(path, readOnly, mapUser)
	content := renderExports(lines, path, &line, mapUser)
	if err := atomicWrite(e.exportsPath(), []byte(content)); err != nil {
		res.Error = "写 " + e.exportsPath() + " 失败，未改动原文件：" + err.Error()
		return res
	}
	upd := e.execSeq(ctx, &res, "nfsd", "update")
	res.Command = strings.Join(res.Commands, " && ")
	if ok, msg := e.confirmExports(path, readOnly, true); !ok {
		e.restore(raw, existed, backup)
		res.Error = msg + "（已回滚到写入前）"
		return res
	}
	res.Verified, res.OK = true, true
	if backup != "" {
		res.Note = "原文件已备份到 " + backup
	}
	if upd.Err != nil {
		res.Note = strings.TrimSpace(res.Note + "；nfsd update 未成功（" + tail(upd.Combined(), 120) + "），导出表已写入，服务下次读取时生效")
	}
	return res
}

// RemoveExport 删掉本面板为某路径写的那一行：备份 → 原子替换 → nfsd update → 回读。
func (e *Executor) RemoveExport(ctx context.Context, path string) Result {
	res := Result{Action: "export_remove"}
	raw, existed, err := e.readExports()
	if err != nil {
		res.Error = "读 " + e.exportsPath() + " 失败，未改动：" + err.Error()
		return res
	}
	mapUser := e.mapUser()
	lines, perr := parseExportLines(string(raw), mapUser)
	if perr != nil {
		res.Error = e.exportsPath() + " 解析失败，未改动原文件：" + perr.Error()
		return res
	}
	found := false
	for _, ln := range lines {
		if isManagedLine(ln, path, mapUser) {
			found = true
		}
	}
	if !found {
		res.Missing = true
		res.Error = "没有找到本面板导出的这一行（" + path + "），未改动原文件"
		return res
	}
	backup, err := e.backupExports(raw)
	if err != nil {
		res.Error = "备份失败，未改动原文件：" + err.Error()
		return res
	}
	content := renderExports(lines, path, nil, mapUser)
	if err := atomicWrite(e.exportsPath(), []byte(content)); err != nil {
		res.Error = "写 " + e.exportsPath() + " 失败，未改动原文件：" + err.Error()
		return res
	}
	upd := e.execSeq(ctx, &res, "nfsd", "update")
	res.Command = strings.Join(res.Commands, " && ")
	if ok, msg := e.confirmExportAbsent(path); !ok {
		e.restore(raw, existed, backup)
		res.Error = msg + "（已回滚到写入前）"
		return res
	}
	res.Verified, res.OK = true, true
	if backup != "" {
		res.Note = "原文件已备份到 " + backup
	}
	if upd.Err != nil {
		res.Note = strings.TrimSpace(res.Note + "；nfsd update 未成功（" + tail(upd.Combined(), 120) + "）")
	}
	return res
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
