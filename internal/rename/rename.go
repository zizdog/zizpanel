// Package rename 是批量改名的**唯一规则引擎**：预览（rename-plan）与执行
// （rename-apply）都调同一份实现，绝不出现"预览一套、执行另一套"。
//
// 纯函数部分（Plan / ApplyRules / ParseCN / ValidateName）不碰文件系统：
// 目录里已有哪些名字由调用方作为入参传进来，所以冲突/非法能在单测里穷举。
// Apply 是唯一的文件动作，且每条 rename 后都回读确认。
package rename

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 规则类型。
const (
	RuleReplace = "replace" // 查找→替换（replace 为空 = 删除）
	RuleInsert  = "insert"  // 前缀 / 后缀 / 第 N 个字符后
	RuleDelete  = "delete"  // 删除匹配内容
	RuleCNNum   = "cnnum"   // 中文数字 → 阿拉伯数字
	RuleEpisode = "episode" // 集数重写成 EP{num}
)

// 匹配方式（replace / delete 用）。
const (
	ModeLiteral  = "literal"  // 子串，全部替换
	ModeWildcard = "wildcard" // 整名匹配：* 任意串、? 单字符（都进捕获组）
	ModeRegex    = "regex"    // 正则，子串替换
)

// 插入位置。
const (
	PosPrefix = "prefix"
	PosSuffix = "suffix"
	PosAfter  = "after"
)

// cnnum 范围。
const (
	ScopeEpisode = "episode" // 默认：只在集数语境里替换
	ScopeAll     = "all"     // 显式选择才全名替换
)

// maxNameBytes 是文件系统对单个名字的字节上限（HFS+/APFS 同为 255）。
const maxNameBytes = 255

// ErrEmptyName：改名后主名为空（原扩展名还在，但去掉主名只剩 .mp4 这种）。
var ErrEmptyName = errors.New("改名后名称为空")

// Rule 是一条改名规则。字段按类型取用：
//
//	replace / delete：Find + Mode + Replace（delete 忽略 Replace）
//	insert：Position + At + Text
//	cnnum：Scope
//	episode：Prefix + Width
type Rule struct {
	Type     string `json:"type"`
	Find     string `json:"find,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Replace  string `json:"replace,omitempty"`
	Position string `json:"position,omitempty"`
	At       int    `json:"at,omitempty"`
	Text     string `json:"text,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Width    int    `json:"width,omitempty"`
}

// Options 是整批的开关。
type Options struct {
	// IncludeExt：规则是否作用于扩展名（默认 false，扩展名原样保留）。
	IncludeExt bool `json:"include_ext"`
	// CNNumScope：cnnum 规则的默认范围；规则自己的 Scope 优先。默认 episode。
	CNNumScope string `json:"cnnum_scope,omitempty"`
}

// 每条计划的状态。
const (
	StatusOK        = "ok"        // 可改名
	StatusUnchanged = "unchanged" // 新名与原名相同
	StatusConflict  = "conflict"  // 目标已存在，或批内两条撞同一目标
	StatusInvalid   = "invalid"   // 非法字符 / 空名 / 超长 / 规则错误
)

// Item 是计划里的一条。
type Item struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

// Result 是整批计划（也是 apply 拒绝时的逐条原因载体）。
type Result struct {
	Items     []Item `json:"items"`
	OK        int    `json:"ok"`
	Unchanged int    `json:"unchanged"`
	Conflict  int    `json:"conflict"`
	Invalid   int    `json:"invalid"`
}

// ---- 中文数字 ----

// 支持 零〇一二三四五六七八九十百千两；万/亿放进匹配类但不解析
// （解析失败 ⇒ 原样保留，绝不猜一个错的数字）。
const cnDigits = "零〇一二三四五六七八九十百千两万"
const cnUnits = "十百千"

var digitValue = map[rune]int{
	'零': 0, '〇': 0, '一': 1, '二': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '两': 2,
}

var unitValue = map[rune]int{'十': 10, '百': 100, '千': 1000}

// ParseCN 解析中文数字。支持 十/二十/二十一/一百/一百零二/一千零二十/三百二十；
// 纯数字串（二〇二）按逐位读法；含不支持的字符（万/亿…）返回 false。
func ParseCN(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	section, number := 0, 0
	hasDigit, hasUnit, pureDigits := false, false, true
	for _, r := range s {
		if d, ok := digitValue[r]; ok {
			number = d
			hasDigit = true
			continue
		}
		if u, ok := unitValue[r]; ok {
			hasUnit = true
			pureDigits = false
			// "十二" = 12、"十" = 10：单位前没数字时按 1 算。
			if number == 0 {
				number = 1
			}
			section += number * u
			number = 0
			continue
		}
		return 0, false
	}
	if !hasDigit && !hasUnit {
		return 0, false
	}
	// 没有单位的纯数字串按逐位读（二〇二 → 202）；单字仍走位值。
	if pureDigits && utf8.RuneCountInString(s) > 1 {
		v := 0
		for _, r := range s {
			v = v*10 + digitValue[r]
		}
		return v, true
	}
	return section + number, true
}

// 集数语境：第X集/话/期/回/部/季/卷/章、EPx、Ex。
var (
	reCNRun  = regexp.MustCompile(`[` + cnDigits + `]+`)
	numPat   = `(?:[` + cnDigits + `]+|[0-9]+)`
	reEpCN   = regexp.MustCompile(`第([` + cnDigits + `]+)(集|话|話|期|回|部|季|卷|章)`)
	reEpEn   = regexp.MustCompile(`(?i)E(P)?([` + cnDigits + `]+)`)
	reTokNum = regexp.MustCompile(`第(` + numPat + `)(集|话|話|期|回|部|季|卷|章)`)
	reTokEn  = regexp.MustCompile(`(?i)E(P)?(` + numPat + `)`)
	reTokBar = regexp.MustCompile(`(` + numPat + `)(集|话|話|期|回|部|季|卷|章)`)
)

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isASCIIDigit(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseNumToken 解析一个集数片段（阿拉伯或中文数字）。
func ParseNumToken(tok string) (int, bool) {
	if isASCIIDigit(tok) {
		n, err := strconv.Atoi(tok)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return ParseCN(tok)
}

// cnNumInEpisode 只把集数语境里的中文数字换成阿拉伯数字。
func cnNumInEpisode(s string) string {
	s = reEpCN.ReplaceAllStringFunc(s, func(m string) string {
		sub := reEpCN.FindStringSubmatch(m)
		if n, ok := ParseCN(sub[1]); ok {
			return "第" + strconv.Itoa(n) + sub[2]
		}
		return m
	})
	idx := reEpEn.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		start, end := m[0], m[1]
		// "SOMEE二十" 这种英文词尾的 E 不算集数；"S02E二十" 前一位是数字，要算。
		if start > 0 && isASCIILetter(s[start-1]) {
			continue
		}
		n, ok := ParseCN(s[m[4]:m[5]])
		if !ok {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(s[start:m[4]]) // "E" / "EP" 原样
		b.WriteString(strconv.Itoa(n))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// applyCNNum 按范围替换中文数字。
func applyCNNum(s, scope string) (string, error) {
	switch normalizeScope(scope) {
	case ScopeAll:
		return reCNRun.ReplaceAllStringFunc(s, func(m string) string {
			if n, ok := ParseCN(m); ok {
				return strconv.Itoa(n)
			}
			return m
		}), nil
	case ScopeEpisode:
		return cnNumInEpisode(s), nil
	default:
		return "", fmt.Errorf("未知中文数字范围 %q", scope)
	}
}

// applyEpisode 找到**第一个**集数片段并整体换成 Prefix+数字（可补零）。
// 只换片段、保留前后文，所以 "[组名] 第三十集" → "[组名] EP30"。
func applyEpisode(s string, r Rule) (string, error) {
	prefix := r.Prefix
	if prefix == "" {
		prefix = "EP"
	}
	width := r.Width
	if width < 0 {
		width = 0
	}
	if width > 3 {
		width = 3
	}

	type hit struct {
		start, end int
		numStr     string
	}
	best := hit{start: -1}
	keep := func(h hit) {
		if h.start < 0 {
			return
		}
		if best.start < 0 || h.start < best.start || (h.start == best.start && h.end > best.end) {
			best = h
		}
	}
	if m := reTokNum.FindStringSubmatchIndex(s); m != nil {
		keep(hit{start: m[0], end: m[1], numStr: s[m[2]:m[3]]})
	}
	for _, m := range reTokEn.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > 0 && isASCIILetter(s[m[0]-1]) {
			continue // 英文单词里的 E 不算
		}
		keep(hit{start: m[0], end: m[1], numStr: s[m[4]:m[5]]})
	}
	for _, m := range reTokBar.FindAllStringSubmatchIndex(s, -1) {
		// "第X集" 已由 reTokNum 整体覆盖（起点更早，keep 会选中它）；
		// 这里只补 "20集/二十话" 这种裸写法。
		if m[0] > 0 && isASCIILetter(s[m[0]-1]) {
			continue
		}
		keep(hit{start: m[0], end: m[1], numStr: s[m[2]:m[3]]})
	}
	if best.start < 0 {
		return s, nil
	}
	n, ok := ParseNumToken(best.numStr)
	if !ok {
		return s, nil
	}
	num := strconv.Itoa(n)
	if width >= 2 {
		num = fmt.Sprintf("%0*d", width, n)
	}
	return s[:best.start] + prefix + num + s[best.end:], nil
}

// ---- 替换 / 插入 ----

// wildcardRegexp 把通配符编译成**整名锚定**的正则：* → (.*)、? → (.)，
// 所以 $1 就是第一个通配符捕获的内容。
func wildcardRegexp(pat string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pat {
		switch r {
		case '*':
			b.WriteString("(.*)")
		case '?':
			b.WriteString("(.)")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("通配符无效：%v", err)
	}
	return re, nil
}

func normalizeMode(m string) string {
	if m == "" {
		return ModeLiteral
	}
	return m
}

func normalizeScope(s string) string {
	if s == "" {
		return ScopeEpisode
	}
	return s
}

func applyReplace(s string, r Rule, repl string) (string, error) {
	if r.Find == "" {
		return "", errors.New("查找内容为空")
	}
	switch normalizeMode(r.Mode) {
	case ModeLiteral:
		return strings.ReplaceAll(s, r.Find, repl), nil
	case ModeWildcard:
		re, err := wildcardRegexp(r.Find)
		if err != nil {
			return "", err
		}
		return re.ReplaceAllString(s, repl), nil
	case ModeRegex:
		re, err := regexp.Compile(r.Find)
		if err != nil {
			return "", fmt.Errorf("正则无效：%v", err)
		}
		return re.ReplaceAllString(s, repl), nil
	default:
		return "", fmt.Errorf("未知匹配方式 %q", r.Mode)
	}
}

func applyInsert(s string, r Rule) (string, error) {
	switch r.Position {
	case "", PosPrefix:
		return r.Text + s, nil
	case PosSuffix:
		return s + r.Text, nil
	case PosAfter:
		rs := []rune(s)
		n := r.At
		if n < 0 {
			n = 0
		}
		if n > len(rs) {
			n = len(rs)
		}
		return string(rs[:n]) + r.Text + string(rs[n:]), nil
	default:
		return "", fmt.Errorf("未知插入位置 %q", r.Position)
	}
}

func applyRule(s string, r Rule, defScope string) (string, error) {
	switch r.Type {
	case RuleReplace:
		return applyReplace(s, r, r.Replace)
	case RuleDelete:
		return applyReplace(s, r, "")
	case RuleInsert:
		return applyInsert(s, r)
	case RuleCNNum:
		// 规则自己的 scope 优先；否则用整批的默认（再否则 episode）。
		scope := r.Scope
		if scope == "" {
			scope = defScope
		}
		return applyCNNum(s, scope)
	case RuleEpisode:
		return applyEpisode(s, r)
	default:
		return "", fmt.Errorf("未知规则类型 %q", r.Type)
	}
}

// splitExt 拆主名与扩展名。点开头的隐藏文件不算扩展名（".bashrc" 整名当主名）。
func splitExt(name string) (main, ext string) {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return name, ""
	}
	return name[:i], name[i:]
}

// ApplyRules 把规则按顺序作用在一个名字上；IncludeExt=false 时扩展名原样保留。
func ApplyRules(name string, rules []Rule, opts Options) (string, error) {
	main, ext := name, ""
	if !opts.IncludeExt {
		main, ext = splitExt(name)
	}
	defScope := opts.CNNumScope
	for i, r := range rules {
		out, err := applyRule(main, r, defScope)
		if err != nil {
			return "", fmt.Errorf("第 %d 条规则：%w", i+1, err)
		}
		main = out
	}
	if ext != "" && strings.TrimSpace(main) == "" {
		return "", ErrEmptyName
	}
	return main + ext, nil
}

// ValidateName 校验改后的名字（纯函数，可与文件系统无关地测）。
func ValidateName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "名称为空"
	}
	if strings.Contains(name, "/") {
		return "名称不能包含 /"
	}
	if strings.ContainsRune(name, 0) {
		return "名称包含非法字符"
	}
	if name == "." || name == ".." {
		return "名称不能是 . 或 .."
	}
	if len([]byte(name)) > maxNameBytes {
		return fmt.Sprintf("名称超过 %d 字节", maxNameBytes)
	}
	return ""
}

// Plan 计算整批改名计划。
//
// existing 是"目标目录里现在已有哪些名字"（调用方从磁盘读；纯函数测试里直接传）。
// 冲突判据：目标已在目录里且**不会因为本批次腾空**，或批内两条撞同一目标。
// A↔B 互换/成环不算冲突 —— 那些源名会被腾空，由 Apply 用临时名过渡。
func Plan(names, existing []string, rules []Rule, opts Options) Result {
	type row struct {
		name, newName string
		invalid       string
	}
	rows := make([]row, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		r := row{name: name, newName: name}
		switch {
		case strings.TrimSpace(name) == "":
			r.invalid = "原名称为空"
		case strings.ContainsAny(name, "/\x00"):
			r.invalid = "原名称含非法字符"
		case name == "." || name == "..":
			r.invalid = "原名称非法"
		case seen[name]:
			r.invalid = "批内重复选中"
		default:
			seen[name] = true
			out, err := ApplyRules(name, rules, opts)
			if err != nil {
				r.invalid = err.Error()
			} else if reason := ValidateName(out); reason != "" {
				r.invalid = reason
			} else {
				r.newName = out
			}
		}
		rows = append(rows, r)
	}

	// 会被腾空的名字（只有有效且确实改名的行才腾空）。
	vacated := map[string]bool{}
	for _, r := range rows {
		if r.invalid == "" && r.newName != r.name {
			vacated[r.name] = true
		}
	}
	inDir := map[string]bool{}
	for _, n := range existing {
		inDir[n] = true
	}
	// 批内目标计数（只统计有效行）。
	targets := map[string]int{}
	for _, r := range rows {
		if r.invalid == "" {
			targets[r.newName]++
		}
	}

	res := Result{Items: make([]Item, 0, len(rows))}
	for _, r := range rows {
		it := Item{Name: r.name, NewName: r.newName}
		switch {
		case r.invalid != "":
			it.Status = StatusInvalid
			it.Reason = r.invalid
			res.Invalid++
		case r.newName == r.name:
			it.Status = StatusUnchanged
			res.Unchanged++
		case targets[r.newName] > 1:
			it.Status = StatusConflict
			it.Reason = "批内多行改成同一个名字"
			res.Conflict++
		case inDir[r.newName] && !vacated[r.newName]:
			it.Status = StatusConflict
			it.Reason = "目标已存在"
			res.Conflict++
		default:
			it.Status = StatusOK
			res.OK++
		}
		res.Items = append(res.Items, it)
	}
	return res
}

// Fingerprint 是计划的稳定指纹：目录内容或规则变了 ⇒ 指纹变 ⇒ apply 拒绝
// 一份过期计划，而不是照着旧计划改现在的文件。
func (r Result) Fingerprint() string {
	h := sha256.New()
	for _, it := range r.Items {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", it.Name, it.NewName, it.Status, it.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ---- 执行 ----

// 每条执行结果的状态。
const (
	ApplyOK      = "ok"      // 成功
	ApplySkipped = "skipped" // 跳过（名称未变）
	ApplyFailed  = "failed"  // 失败
)

// ApplyItem 是执行结果里的一条。
type ApplyItem struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

// ApplyResult 是整批执行结果。
type ApplyResult struct {
	Items     []ApplyItem `json:"items"`
	Succeeded int         `json:"succeeded"`
	Skipped   int         `json:"skipped"`
	Failed    int         `json:"failed"`
	Summary   string      `json:"summary"`
}

// Apply 在 dir 内执行计划。计划里只要有 conflict/invalid 就直接拒绝 ——
// 调用方必须已经把它们挡在 400，这里再兜一层，绝不改一半。
//
// 每个 rename 都回读确认（新名真的存在、旧名真的没了）；A↔B 互换/成环时
// 先把所有要动的文件改成唯一临时名，再改成目标名 —— 不用临时名会互相覆盖。
func Apply(dir string, plan Result) (ApplyResult, error) {
	for _, it := range plan.Items {
		if it.Status == StatusConflict || it.Status == StatusInvalid {
			return ApplyResult{}, fmt.Errorf("计划里还有 %s：%s（整批拒绝）", it.Status, it.Name)
		}
	}

	var out ApplyResult
	// 名称未变的先如实记跳过（计数统一由最后那轮做，别在这里加，否则跳过会被算两次）。
	moving := make([]Item, 0, len(plan.Items))
	for _, it := range plan.Items {
		if it.Status == StatusUnchanged {
			out.Items = append(out.Items, ApplyItem{Name: it.Name, NewName: it.NewName, Status: ApplySkipped, Reason: "名称未变"})
			continue
		}
		moving = append(moving, it)
	}

	srcSet := map[string]bool{}
	for _, it := range moving {
		srcSet[it.Name] = true
	}
	needTemp := false
	for _, it := range moving {
		if srcSet[it.NewName] {
			needTemp = true // 目标是批次里另一个源名 ⇒ 互换/成环
		}
	}

	if needTemp {
		out.Items = append(out.Items, applyWithTemp(dir, moving)...)
	} else {
		for _, it := range moving {
			out.Items = append(out.Items, applyDirect(dir, it))
		}
	}
	for _, it := range out.Items {
		switch it.Status {
		case ApplyOK:
			out.Succeeded++
		case ApplySkipped:
			out.Skipped++
		case ApplyFailed:
			out.Failed++
		}
	}
	out.Summary = fmt.Sprintf("成功 %d 项，跳过 %d 项，失败 %d 项", out.Succeeded, out.Skipped, out.Failed)
	return out, nil
}

// verifyMoved 回读确认：dst 存在、src 不存在。
func verifyMoved(src, dst string) error {
	if _, err := os.Lstat(dst); err != nil {
		return fmt.Errorf("新名回读失败：%v", err)
	}
	if _, err := os.Lstat(src); err == nil {
		return errors.New("原名回读仍在")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("原名回读异常：%v", err)
	}
	return nil
}

// targetTaken 执行前再查一次目标（os.Rename 在 Unix 上会静默覆盖，绝不能靠它兜底）。
func targetTaken(dst string) bool {
	_, err := os.Lstat(dst)
	return err == nil
}

func applyDirect(dir string, it Item) ApplyItem {
	res := ApplyItem{Name: it.Name, NewName: it.NewName}
	src := filepath.Join(dir, it.Name)
	dst := filepath.Join(dir, it.NewName)
	if targetTaken(dst) {
		res.Status = ApplyFailed
		res.Reason = "目标已存在（执行时出现）"
		return res
	}
	if err := os.Rename(src, dst); err != nil {
		res.Status = ApplyFailed
		res.Reason = "重命名失败：" + err.Error()
		return res
	}
	if err := verifyMoved(src, dst); err != nil {
		res.Status = ApplyFailed
		res.Reason = "已改名但回读异常：" + err.Error()
		return res
	}
	res.Status = ApplyOK
	return res
}

// applyWithTemp 是互换/成环的安全路径：先全部改成唯一临时名，再改成目标名。
// 任一步失败就尽力把已改的还原，并如实逐条报失败原因。
func applyWithTemp(dir string, moving []Item) []ApplyItem {
	type slot struct {
		it  Item
		tmp string
	}
	slots := make([]slot, len(moving))
	results := make([]ApplyItem, len(moving))

	// 阶段 1：源 → 唯一临时名。
	for i, it := range moving {
		results[i] = ApplyItem{Name: it.Name, NewName: it.NewName}
		tmp := fmt.Sprintf(".zp-rename-%d-%d-%d.tmp", os.Getpid(), time.Now().UnixNano(), i)
		tmpPath := filepath.Join(dir, tmp)
		if targetTaken(tmpPath) {
			results[i].Status = ApplyFailed
			results[i].Reason = "临时名被占用：" + tmp
			continue
		}
		src := filepath.Join(dir, it.Name)
		if err := os.Rename(src, tmpPath); err != nil {
			results[i].Status = ApplyFailed
			results[i].Reason = "改临时名失败：" + err.Error()
			continue
		}
		if err := verifyMoved(src, tmpPath); err != nil {
			results[i].Status = ApplyFailed
			results[i].Reason = "临时名回读异常：" + err.Error()
			continue
		}
		results[i].Status = ApplyOK // 暂记，阶段 2 再定
		slots[i] = slot{it: it, tmp: tmpPath}
	}

	// 阶段 1 有失败：把已经挪到临时名的还原回原名，整批不做目标改名。
	phase1Broken := false
	for i := range moving {
		if results[i].Status == ApplyFailed {
			phase1Broken = true
			break
		}
	}
	if phase1Broken {
		for i := range moving {
			if slots[i].tmp == "" {
				continue
			}
			src := filepath.Join(dir, moving[i].Name)
			if err := os.Rename(slots[i].tmp, src); err != nil {
				results[i].Status = ApplyFailed
				results[i].Reason = "已还原失败，文件在临时名 " + filepath.Base(slots[i].tmp)
				continue
			}
			results[i].Status = ApplyFailed
			results[i].Reason = "整批中止，已还原原名"
		}
		return results
	}

	// 阶段 2：临时名 → 目标名。
	for i := range moving {
		if slots[i].tmp == "" {
			continue
		}
		dst := filepath.Join(dir, moving[i].NewName)
		if targetTaken(dst) {
			// 目标在这期间被别的东西占了：还原这一条。
			src := filepath.Join(dir, moving[i].Name)
			if err := os.Rename(slots[i].tmp, src); err != nil {
				results[i].Status = ApplyFailed
				results[i].Reason = "目标已存在且还原失败，文件在临时名 " + filepath.Base(slots[i].tmp)
				continue
			}
			results[i].Status = ApplyFailed
			results[i].Reason = "目标已存在（执行时出现），已还原原名"
			continue
		}
		if err := os.Rename(slots[i].tmp, dst); err != nil {
			src := filepath.Join(dir, moving[i].Name)
			if rerr := os.Rename(slots[i].tmp, src); rerr != nil {
				results[i].Status = ApplyFailed
				results[i].Reason = "改目标失败且还原失败，文件在临时名 " + filepath.Base(slots[i].tmp)
				continue
			}
			results[i].Status = ApplyFailed
			results[i].Reason = "改目标失败：" + err.Error() + "（已还原原名）"
			continue
		}
		if err := verifyMoved(slots[i].tmp, dst); err != nil {
			results[i].Status = ApplyFailed
			results[i].Reason = "已改名但回读异常：" + err.Error()
			continue
		}
		results[i].Status = ApplyOK
		results[i].Reason = ""
	}
	return results
}
