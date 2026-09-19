package services

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Homebrew「目录缺失」（stderr 里 `dir_initialize` + 前缀下路径）这一类：
// 如实诊断 → 给出具体修复命令与归属 → **用户点击才执行**的补建，做完回读复核。
// 身份/环境与市场探测同一条路（见 InstalledFormulaVersionsErr）；坑 186。

// BrewDirDiagnosis 是一次「brew 目录缺失」判定。
type BrewDirDiagnosis struct {
	// Raw 是判定所依据的 stderr（调用方给的原样文本）。
	Raw string
	// Prefix 是缺失目录所属的 Homebrew 前缀（如 /opt/homebrew）。
	Prefix string
	// MissingDir 是 stderr 里点名的那个不存在的目录（绝对路径）。
	MissingDir string
	// MissingRel 是相对前缀的写法（如 Caskroom）。
	MissingRel string
}

// BrewDirOwner 描述 brew 前缀的**真实属主**——补建出来的目录必须交还给它。
type BrewDirOwner struct {
	// Known=false 表示读不到属主。这时**绝不猜**，也绝不用 root 冒充。
	Known bool
	Name  string
	Group string
	UID   int
	GID   int
}

// Label 是给用户看的归属描述。
func (o BrewDirOwner) Label() string {
	if !o.Known {
		return "（读不到真实属主）"
	}
	switch {
	case o.Name != "" && o.Group != "":
		return o.Name + ":" + o.Group
	case o.Name != "":
		return o.Name
	default:
		return fmt.Sprintf("uid=%d gid=%d", o.UID, o.GID)
	}
}

// brewStandardDirs 是 brew 自己会创建/使用的顶层目录（只补空目录，不写文件）。
var brewStandardDirs = []string{
	"Caskroom",
	"Cellar",
	"opt",
	"var",
	"etc",
	"include",
	"lib",
	"sbin",
	"share",
	"Frameworks",
}

// brewRepairDirs 是补建时要检查的目录（顶层 + brew 内部要用的嵌套目录）。
var brewRepairDirs = append(append([]string(nil), brewStandardDirs...),
	filepath.Join("var", "homebrew", "locks"),
	filepath.Join("var", "homebrew", "tmp"),
)

// defaultBrewPrefixes 从 brew 二进制候选位置反推前缀（Apple Silicon 优先）。
func defaultBrewPrefixes() []string {
	return prefixesFromBins(brewBinCandidates()...)
}

// brewPrefixes 返回可能用到的 Homebrew 前缀；配置里的排第一（判定必须贴着真正执行的那个 brew）。
func (m *Manager) brewPrefixes() []string {
	bins := []string{}
	if m != nil && m.opt.BrewBin != "" {
		bins = append(bins, m.opt.BrewBin)
	}
	bins = append(bins, brewBinCandidates()...)
	return prefixesFromBins(bins...)
}

// prefixesFromBins 把 brew 二进制路径投影成前缀，按出现顺序去重。
func prefixesFromBins(bins ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, bin := range bins {
		bin = strings.TrimSpace(bin)
		if bin == "" {
			continue
		}
		p := filepath.Dir(filepath.Dir(bin))
		if p == "" || p == "/" || p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// DiagnoseBrewDirError 判断 stderr 是否属于「目录缺失」：No such file or directory
// + 落在 Homebrew 前缀下的路径 +（`dir_initialize` 或缺失的正是 brew 目录）。
func DiagnoseBrewDirError(stderr string) (BrewDirDiagnosis, bool) {
	return diagnoseBrewDirError(stderr, defaultBrewPrefixes())
}

func diagnoseBrewDirError(stderr string, prefixes []string) (BrewDirDiagnosis, bool) {
	raw := strings.TrimSpace(stderr)
	if raw == "" {
		return BrewDirDiagnosis{}, false
	}
	if !strings.Contains(strings.ToLower(raw), "no such file or directory") {
		return BrewDirDiagnosis{}, false
	}
	dir, prefix := missingDirFromBrewError(raw, prefixes)
	if dir == "" {
		return BrewDirDiagnosis{}, false
	}
	rel := strings.Trim(strings.TrimPrefix(dir, prefix), "/")
	if rel == "" {
		// 前缀整个不存在：那是"Homebrew 没装好"，不是"目录缺失"，
		// 不能在这里被修复冒充（装 brew 走「基础环境」那条路）。
		return BrewDirDiagnosis{}, false
	}
	if !strings.Contains(raw, "dir_initialize") && !isBrewStandardDir(rel) {
		return BrewDirDiagnosis{}, false
	}
	return BrewDirDiagnosis{Raw: raw, Prefix: prefix, MissingDir: dir, MissingRel: rel}, true
}

// missingDirFromBrewError 按行找缺失目录（全文取第一个会命中 PATH 警告里的无关路径）。
// 顺序：dir_initialize 那一行 → "路径不存在"那一行 → 全文（仍需 dir_initialize）。
func missingDirFromBrewError(raw string, prefixes []string) (string, string) {
	lines := strings.Split(raw, "\n")
	for _, ln := range lines {
		if idx := strings.Index(ln, "dir_initialize"); idx >= 0 {
			// 先看标记之后那段：面板自己会把命令路径写在前头，而这条消息会被再次解析。
			if d, p := firstBrewPrefixPath(ln[idx+len("dir_initialize"):], prefixes); d != "" {
				return d, p
			}
			if d, p := firstBrewPrefixPath(ln, prefixes); d != "" {
				return d, p
			}
		}
	}
	for _, ln := range lines {
		if strings.Contains(strings.ToLower(ln), "no such file or directory") {
			if d, p := firstBrewPrefixPath(ln, prefixes); d != "" {
				return d, p
			}
		}
	}
	if strings.Contains(raw, "dir_initialize") {
		return firstBrewPrefixPath(raw, prefixes)
	}
	return "", ""
}

// firstBrewPrefixPath 找出文本里第一个落在某个前缀下的路径（前缀须是路径边界）。
func firstBrewPrefixPath(text string, prefixes []string) (string, string) {
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		from := 0
		for {
			idx := strings.Index(text[from:], p)
			if idx < 0 {
				break
			}
			idx += from
			end := idx + len(p)
			if end < len(text) && text[end] != '/' && !isPathDelimiter(text[end]) {
				from = idx + 1 // 只是更长的路径前缀（如 /opt/homebrewX），继续找
				continue
			}
			j := end
			for j < len(text) && !isPathDelimiter(text[j]) {
				j++
			}
			full := strings.TrimRight(text[idx:j], "/")
			if full == "" {
				full = p
			}
			return full, p
		}
	}
	return "", ""
}

// isPathDelimiter 判断"路径到此结束"的分隔符；非 ASCII 一律算（后面常紧跟中文）。
func isPathDelimiter(b byte) bool {
	if b >= 0x80 {
		return true
	}
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', ')', ']', '}', '>', '<', ',', ';', ':', '=', '|', '*', '?', '!':
		return true
	}
	return false
}

// isBrewStandardDir 判断 rel 的顶层目录是不是 brew 的标准目录。
func isBrewStandardDir(rel string) bool {
	head := rel
	if i := strings.IndexByte(head, '/'); i >= 0 {
		head = head[:i]
	}
	for _, d := range brewStandardDirs {
		if head == d {
			return true
		}
	}
	return false
}

// BrewDirInstallCommand 返回补建一个目录的**具体命令**（用户可以直接复制）。
func BrewDirInstallCommand(dir string, own BrewDirOwner) string {
	if !own.Known || own.Name == "" {
		return "sudo install -d " + dir
	}
	if own.Group == "" {
		return fmt.Sprintf("sudo install -d -o %s %s", own.Name, dir)
	}
	return fmt.Sprintf("sudo install -d -o %s -g %s %s", own.Name, own.Group, dir)
}

// FormatBrewDirAdvice 生成用户可见建议：缺哪个目录、用什么命令补、归属交给谁。
// 第一句必须短（应用市场的"原因："在 apps.js 里截断到 140 字符）。
func FormatBrewDirAdvice(d BrewDirDiagnosis, own BrewDirOwner) string {
	var b strings.Builder
	if own.Known && own.Name != "" {
		fmt.Fprintf(&b, "缺目录 %s：%s（或在面板「mac设置 → Homebrew 目录」点「补建缺失目录」）",
			d.MissingDir, BrewDirInstallCommand(d.MissingDir, own))
	} else {
		fmt.Fprintf(&b, "缺目录 %s：在面板「mac设置 → Homebrew 目录」点「补建缺失目录」，或 sudo install -d %s，"+
			"归属用 `ls -ld %s` 看真实属主", d.MissingDir, d.MissingDir, d.Prefix)
	}
	b.WriteString("\n")
	if own.Known {
		fmt.Fprintf(&b, "归属必须交给 brew 前缀的真实属主 %s（用 root 建完不 chown 回去，以该用户跑的 brew 仍写不进去）。\n", own.Label())
	} else {
		fmt.Fprintf(&b, "归属必须交给 brew 前缀 %s 的真实属主，别留在 root 名下。\n", d.Prefix)
	}
	b.WriteString("这只是「未能复核」，不代表软件没装；面板不会自动改系统，点按钮后会补建并重新跑 `brew list --versions` 复核。")
	return b.String()
}

// ============================================================================
//  探测与修复
// ============================================================================

// BrewRepairPlan 是"补建缺失 brew 目录"的探测结果（只描述，不执行）。
type BrewRepairPlan struct {
	Prefix  string
	Owner   BrewDirOwner
	Missing []string
	Present []string
	// Conflicts 是"存在但不是目录"的路径：面板**不会**删/改它，如实列出来让用户处理。
	Conflicts []string
	// Skipped 是点名了但**不属于当前前缀**的目录（不做，也不假装做了）。
	Skipped []string
}

// Text 把计划渲染成任务日志用的多行文本（不执行任何动作）。
func (p BrewRepairPlan) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Homebrew 前缀：%s（真实属主 %s）\n", p.Prefix, p.Owner.Label())
	if len(p.Missing) == 0 {
		b.WriteString("缺失目录：无\n")
	} else {
		fmt.Fprintf(&b, "缺失目录：%s\n", strings.Join(p.Missing, "、"))
	}
	if len(p.Present) > 0 {
		fmt.Fprintf(&b, "已存在：%s\n", strings.Join(p.Present, "、"))
	}
	if len(p.Conflicts) > 0 {
		fmt.Fprintf(&b, "存在但不是目录（面板不会动它）：%s\n", strings.Join(p.Conflicts, "、"))
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintf(&b, "不属于该前缀、已跳过：%s\n", strings.Join(p.Skipped, "、"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// BrewDirOwnerOf 读一个目录的真实属主（uid/gid + 名字）。读不到时 Known=false。
func BrewDirOwnerOf(path string) BrewDirOwner {
	var own BrewDirOwner
	st, err := os.Stat(path)
	if err != nil {
		return own
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return own
	}
	own.Known = true
	own.UID, own.GID = int(sys.Uid), int(sys.Gid)
	if u, err := user.LookupId(strconv.Itoa(own.UID)); err == nil {
		own.Name = u.Username
	}
	if g, err := user.LookupGroupId(strconv.Itoa(own.GID)); err == nil {
		own.Group = g.Name
	}
	return own
}

// resolveBrewPrefix 找真实前缀：按配置的 brew 反推，再让 `brew --prefix` 校正。
// 不回落到候选位置（没验证过的前缀不能拿去建目录）；配置不对就明确报错。
func (m *Manager) resolveBrewPrefix(ctx context.Context) (string, error) {
	derived := ""
	if m.opt.BrewBin != "" {
		if st, err := os.Stat(m.opt.BrewBin); err == nil && !st.IsDir() {
			derived = filepath.Dir(filepath.Dir(m.opt.BrewBin))
		}
	}
	if derived == "" {
		return "", fmt.Errorf("找不到 Homebrew 二进制（配置里的 %q 不存在）：请先在「基础环境」里安装 Homebrew",
			m.opt.BrewBin)
	}
	if out, err := m.runBrewReadOnly(ctx, 15*time.Second, "--prefix"); err == nil {
		if p := lastPathLine(out); p != "" {
			if st, e := os.Stat(p); e == nil && st.IsDir() {
				return p, nil
			}
		}
	}
	if st, err := os.Stat(derived); err == nil && st.IsDir() {
		return derived, nil
	}
	return "", fmt.Errorf("Homebrew 前缀 %s 不存在：这台机器上可能还没装好 Homebrew（请先在「基础环境」里装）", derived)
}

// lastPathLine 取输出的最后一行非空内容（`brew --prefix` 只回一行路径）。
func lastPathLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		p := strings.TrimSpace(lines[i])
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "/") {
			return strings.TrimRight(p, "/")
		}
		return ""
	}
	return ""
}

// runBrewReadOnly 以与市场探测相同的身份跑只读 brew（root 时降权 + HOME）。
func (m *Manager) runBrewReadOnly(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	src := brewInstallSource{Name: "只读探测", Env: append([]string(nil), brewCommonEnv...)}
	return streamCmd(ctx, m.brewCommand(ctx, src, args...))
}

// PlanBrewDirRepair 探测前缀下缺哪些 brew 目录（只读）。extraDirs 通常来自上次失败时
// stderr 点名的目录；不属于当前前缀的一律进 Skipped。
func (m *Manager) PlanBrewDirRepair(ctx context.Context, extraDirs ...string) (BrewRepairPlan, error) {
	prefix, err := m.resolveBrewPrefix(ctx)
	if err != nil {
		return BrewRepairPlan{}, err
	}
	plan := BrewRepairPlan{Prefix: prefix, Owner: BrewDirOwnerOf(prefix)}

	cands := append([]string(nil), brewRepairDirs...)
	cands = append(cands, extraDirs...)
	seen := map[string]bool{}
	for _, d := range cands {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		full := d
		if !filepath.IsAbs(full) {
			full = filepath.Join(prefix, full)
		}
		full = filepath.Clean(full)
		if seen[full] {
			continue
		}
		seen[full] = true
		if full == prefix || !strings.HasPrefix(full, prefix+string(filepath.Separator)) {
			plan.Skipped = append(plan.Skipped, full)
			continue
		}
		if st, serr := os.Stat(full); serr == nil {
			if st.IsDir() {
				plan.Present = append(plan.Present, full)
			} else {
				plan.Conflicts = append(plan.Conflicts, full)
			}
			continue
		}
		plan.Missing = append(plan.Missing, full)
	}
	return plan, nil
}

// RepairBrewDirs 补建缺失目录 + 交还归属，最后回读复核。返回 nil 的唯一条件是
// 重新跑一次真实的 `brew list --versions` 成功；读不到就报"未复核通过"。
func (m *Manager) RepairBrewDirs(ctx context.Context, result *InstallResult, extraDirs ...string) error {
	if result == nil {
		result = &InstallResult{Steps: []string{}}
	}
	result.step(ctx, "探测 Homebrew 目录（只读；面板不会自动改系统）")

	plan, err := m.PlanBrewDirRepair(ctx, extraDirs...)
	if err != nil {
		return err
	}
	for _, ln := range strings.Split(plan.Text(), "\n") {
		result.step(ctx, ln)
	}

	if len(plan.Conflicts) > 0 {
		result.step(ctx, "⚠️ 上面那些「存在但不是目录」的路径面板不会删改，请先人工确认它们是不是被别的东西占了")
	}

	for _, dir := range plan.Missing {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("补建 %s 失败：%v", dir, err)
		}
		if err := chownBrewDir(dir, plan.Owner); err != nil {
			return fmt.Errorf("已建好 %s，但把归属交还 %s 失败：%v", dir, plan.Owner.Label(), err)
		}
		result.step(ctx, "已补建 "+dir+"（归属 "+plan.Owner.Label()+"）")
	}

	// 回读（不谎报成功的落点）：目录存在 ≠ brew 能跑，读不到就如实说未复核。
	result.step(ctx, "回读复核：重新执行 `brew list --versions`")
	vers, rerr := m.InstalledFormulaVersionsErr(ctx)
	if rerr != nil {
		result.step(ctx, "⚠️ 目录已处理完，但重新复核**仍未通过**："+rerr.Error())
		return fmt.Errorf("已补建目录，但重新执行 `brew list --versions` 仍未通过（未复核通过）：%w", rerr)
	}
	result.step(ctx, fmt.Sprintf("复核通过：`brew list --versions` 已能正常返回（%d 个已装包）", len(vers)))
	return nil
}

// chownBrewDir 交还归属并 stat 回读确认（退出码在本项目谎报过，见 AGENTS 第三节）。
func chownBrewDir(dir string, own BrewDirOwner) error {
	if !own.Known {
		return fmt.Errorf("读不到 brew 前缀的真实属主，拒绝用 root 归属糊弄过去（以用户身份跑的 brew 仍会写不进去）")
	}
	if st, err := os.Stat(dir); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == own.UID && int(sys.Gid) == own.GID {
			return nil // 极常见：目录本来就是那个用户建的
		}
	}
	if err := os.Chown(dir, own.UID, own.GID); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("chown 后读不回 %s：%v", dir, err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != own.UID || int(sys.Gid) != own.GID {
		return fmt.Errorf("chown 后回读 %s 的归属仍不是 %s", dir, own.Label())
	}
	return nil
}
