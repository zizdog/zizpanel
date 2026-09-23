package plugins

// patch.go —— 声明式配置补丁（表的"能改一行配置"能力）。
//
// 为什么需要它（2026-09-24 的实测结论）：表原先只能表达"安装后无需任何定制"的应用。
// grafana 默认 3000（与 gitea 撞端口）、code-server / meilisearch 默认只绑回环、
// couchdb 3.x 必须先有 admin 口令才肯启动 —— 这些"只差一行配置"的应用因此上不了架，
// 而每个都写一个面板内建补丁就等于退回"一个一个造轮子"。
//
// 边界（安全，别松）：补丁只能**改键值**，不能插入任意命令、不能写多行值
// （值里出现换行等于能注入任意配置行，校验阶段直接拒绝）。
//
// 为什么在**文本层面**改而不是"解析成结构再序列化"：这些配置文件的注释、键顺序、
// include 指令对应用都有意义（grafana.ini 整篇都是注释掉的默认值），
// round-trip 会把它们吃掉 —— 那等于面板偷偷重写了用户的配置。

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// patchKeyRe 限制补丁能改的键名形状（避免把奇怪的字符串当键写进配置）。
var patchKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// 补丁格式。kv = 通用 `key = value`（多数 *nix 配置），ini = 带 [section] 的 kv，
// yaml = `key: value`。
const (
	PatchKV   = "kv"
	PatchINI  = "ini"
	PatchYAML = "yaml"
)

// Patch 描述"把配置文件里这几个键改成这些值"。
type Patch struct {
	Format  string            `json:"format,omitempty"` // kv（默认）/ ini / yaml
	Section string            `json:"section,omitempty"`
	Set     map[string]string `json:"set,omitempty"`
	// IfMissing：配置文件还不存在时怎么办。skip（默认）= 不动，
	// create = 建一个只含这些键的最小文件。
	//
	// 为什么默认 skip：不少应用是**首次启动才生成**配置（netdata.conf、code-server
	// 的 config.yaml），抢先写一个"最小配置"会把应用的默认值全抹掉。
	IfMissing string `json:"if_missing,omitempty"`
	// Secrets 是"这些键的值由面板随机生成、只展示一次"（例如数据库管理员口令）。
	//
	// 关键语义：**已经存在且非空的值一律复用**，绝不轮换 —— 重装一次就把口令换掉，
	// 等于把用户已经配好的应用弄坏。只有真的生成了新值，才会出现在安装结果的凭据区。
	Secrets []string `json:"secrets,omitempty"`
}

// NormalizeFormat 返回规范化的格式（空 = kv）。
func NormalizeFormat(f string) string {
	if strings.TrimSpace(f) == "" {
		return PatchKV
	}
	return strings.ToLower(strings.TrimSpace(f))
}

// Keys 返回排序后的键（给日志/计划文本用，保证输出稳定）。
func (p Patch) Keys() []string {
	out := make([]string, 0, len(p.Set))
	for k := range p.Set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CurrentValue 取文件里某个键现在的值（不存在返回空串 + false）。
//
// 用途：随机密钥必须**先看有没有**再决定生不生成（重装不许轮换口令）。
func CurrentValue(content string, p Patch, key string) (string, bool) {
	format := NormalizeFormat(p.Format)
	section := ""
	for _, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if format == PatchINI && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			continue
		}
		if format == PatchINI && p.Section != "" && section != p.Section {
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		k, v, _, _, ok := parseLine(raw, format)
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

// ApplyPatch 在文本层面应用补丁，返回新内容与"是否真的改了"。
//
// 规则：
//   - 已存在的键 → **整行替换**（保留原缩进与分隔符写法）；
//   - 不存在的键 → 追加（ini 且有 section 时追加到该 section 末尾）；
//   - 注释行、其它键、include 指令：一字不动；
//   - 幂等：值已经一样时返回 changed=false，不重写文件。
func ApplyPatch(content string, p Patch) (string, bool) {
	format := NormalizeFormat(p.Format)
	sep := " = "
	if format == PatchYAML {
		sep = ": "
	}
	// 行尾统一：按 \n 切，保留每行原始内容（含 \r 时在匹配阶段容忍）
	lines := strings.Split(content, "\n")
	// 去掉末尾空行带来的伪行（最后一个 \n 之后是空串），最后再补回来。
	trailing := false
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
		trailing = true
	}

	changed := false
	remaining := map[string]string{}
	for k, v := range p.Set {
		remaining[k] = v
	}

	section := ""
	for i, raw := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if format == PatchINI && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			continue
		}
		if format == PatchINI && p.Section != "" && section != p.Section {
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		key, cur, prefix, comment, ok := parseLine(raw, format)
		if !ok {
			continue
		}
		want, has := p.Set[key]
		if !has {
			continue
		}
		if strings.TrimSpace(cur) == strings.TrimSpace(want) {
			delete(remaining, key) // 已经是目标值：幂等，不重写这一行
			continue
		}
		// 只换值：**前缀（缩进 + 键 + 分隔符写法）与行尾注释原样保留**。
		lines[i] = prefix + want + comment
		delete(remaining, key)
		changed = true
	}

	if len(remaining) > 0 {
		keys := make([]string, 0, len(remaining))
		for k := range remaining {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if format == PatchINI && p.Section != "" {
			// 追加到目标 section 末尾；section 不存在就新开一个。
			end := -1
			inTarget := false
			for i, raw := range lines {
				t := strings.TrimSpace(raw)
				if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
					if inTarget {
						break
					}
					inTarget = strings.TrimSpace(t[1:len(t)-1]) == p.Section
				}
				if inTarget {
					end = i
				}
			}
			add := make([]string, 0, len(keys))
			if end < 0 {
				lines = append(lines, "", "["+p.Section+"]")
				end = len(lines) - 1
			}
			for _, k := range keys {
				add = append(add, k+sep+remaining[k])
			}
			rest := append([]string{}, lines[end+1:]...)
			lines = append(append(lines[:end+1], add...), rest...)
		} else {
			for _, k := range keys {
				lines = append(lines, k+sep+remaining[k])
			}
		}
		changed = true
	}

	out := strings.Join(lines, "\n")
	if trailing || changed {
		out += "\n"
	}
	return out, changed
}

// parseLine 把一行拆成「键 / 值 / 前缀（缩进+键+分隔符）/ 行尾注释」。
//
// 为什么拆得这么细：替换时**只换值**，缩进、`=` 与 `:` 的写法、行尾注释都要原样留下 ——
// 面板去改用户的配置文件，重排它的格式是不能接受的。
func parseLine(raw, format string) (key, val, prefix, comment string, ok bool) {
	indent := raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]
	body := strings.TrimRight(raw[len(indent):], "\r")
	sep := "="
	if format == PatchYAML {
		sep = ":"
	}
	i := strings.Index(body, sep)
	if i <= 0 {
		return "", "", "", "", false
	}
	key = strings.TrimSpace(body[:i])
	if key == "" {
		return "", "", "", "", false
	}
	after := body[i+1:]
	for _, mark := range []string{" #", "\t#", " ;", "\t;"} {
		if j := strings.Index(after, mark); j >= 0 {
			comment = after[j:]
			after = after[:j]
			break
		}
	}
	spaces := after[:len(after)-len(strings.TrimLeft(after, " \t"))]
	prefix = indent + body[:i+1] + spaces
	return key, strings.TrimSpace(after), prefix, comment, true
}

// ValidatePatch 校验补丁本身（与 Spec.Validate 分开，方便单独测）。
func ValidatePatch(p *Patch) []string {
	if p == nil || len(p.Set) == 0 {
		return nil
	}
	var errs []string
	switch NormalizeFormat(p.Format) {
	case PatchKV, PatchINI, PatchYAML:
	default:
		errs = append(errs, fmt.Sprintf("config.format 只能是 kv / ini / yaml，实际 %q", p.Format))
	}
	if p.Section != "" && NormalizeFormat(p.Format) != PatchINI {
		errs = append(errs, "config.section 只在 format=ini 时有意义（写别的格式会被静默忽略，所以直接报错）")
	}
	switch p.IfMissing {
	case "", "skip", "create":
	default:
		errs = append(errs, fmt.Sprintf("config.if_missing 只能是 skip / create，实际 %q", p.IfMissing))
	}
	for _, k := range p.Secrets {
		if !patchKeyRe.MatchString(k) {
			errs = append(errs, fmt.Sprintf("config.secrets 的键 %q 形状不对（只允许字母数字与 _ . -）", k))
		}
		if _, dup := p.Set[k]; dup {
			errs = append(errs, fmt.Sprintf("config.secrets 里的 %q 与同一处 config.set 撞了同一个键（两者都要写它）", k))
		}
	}
	for k, v := range p.Set {
		if !patchKeyRe.MatchString(k) {
			errs = append(errs, fmt.Sprintf("config.set 的键 %q 形状不对（只允许字母数字与 _ . -）", k))
		}
		// 值里出现换行 = 能往配置里注入任意行，必须拒绝。
		if strings.ContainsAny(v, "\n\r") {
			errs = append(errs, fmt.Sprintf("config.set 的 %s 值里不能有换行（否则等于允许注入任意配置行）", k))
		}
	}
	return errs
}

// PatchText 生成"这次会改什么"的人话（计划文本与安装日志共用）。
func PatchText(p *Patch) string {
	if p == nil || len(p.Set) == 0 {
		return ""
	}
	parts := make([]string, 0, len(p.Set))
	for _, k := range p.Keys() {
		where := ""
		if p.Section != "" {
			where = "[" + p.Section + "] "
		}
		parts = append(parts, fmt.Sprintf("%s%s=%s", where, k, p.Set[k]))
	}
	extra := ""
	if p.IfMissing == "create" {
		extra = "；文件不存在时新建"
	}
	return strings.Join(parts, "、") + extra
}
