package priv

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ============================================================================
//  Nginx 性能参数（宝塔式表单的后端）
//
//  用户 2026-09-18 的要求（附了一张宝塔「Nginx 管理 → 性能调整」的截图）：
//    "php 和 nginx 的『编辑配置文件』都是灰色的！用户没法更改文件大小限制的问题！
//     并且，这些常用更改应该同时做成功能！而不应该是让用户只能编辑配置原文件。参考宝塔的样子。"
//
//  宝塔的做法是**直接改 nginx.conf 里对应的指令**（表单 ↔ 指令一一对应），
//  本文件照做，但加了三道保险（这个项目的铁律）：
//    ① 改写是**幂等**的：同一个值写两遍，文件一字不变（有单测）；
//    ② 改写前备份、改写后 `nginx -t`，不通过就**原样回滚**（绝不留下一个起不来的 nginx）；
//    ③ 保存后**回读生效值**（解析 nginx.conf + `nginx -T` 的真实加载结果）——
//       "保存成功"不等于"已生效"，被别处配置覆盖时必须如实说出来。
//
//  为什么不能把参数写进面板自己的 conf.d 片段：实测（2026-09-18）
//  `gzip` 这类指令在同一个上下文里出现两次会让 nginx 直接拒绝启动
//  （`"gzip" directive is duplicate`）。所以只能就地改 nginx.conf 里那一行。
// ============================================================================

// NginxTuning 是一组 nginx 性能参数（**界面单位**：大小按 MB/KB、时间按秒）。
//
// 字段用指针以外的方式表达"未设置"：这里所有字段都必须有值 ——
// 表单上每一项都有当前值（读不到就用 nginx 出厂默认），不存在"留空"。
type NginxTuning struct {
	WorkerProcesses           string `json:"worker_processes"`              // auto 或 1-64
	WorkerConnections         int    `json:"worker_connections"`            // events
	KeepaliveTimeout          int    `json:"keepalive_timeout"`             // 秒
	Gzip                      bool   `json:"gzip"`                          // on/off
	GzipMinLengthKB           int    `json:"gzip_min_length_kb"`            // KB
	GzipCompLevel             int    `json:"gzip_comp_level"`               // 1-9
	ClientMaxBodySizeMB       int    `json:"client_max_body_size_mb"`       // MB（宝塔界面的单位就是 MB）
	ServerNamesHashBucketSize int    `json:"server_names_hash_bucket_size"` // 32-1024
	ClientHeaderBufferSizeKB  int    `json:"client_header_buffer_size_kb"`  // KB
	ClientBodyBufferSizeKB    int    `json:"client_body_buffer_size_kb"`    // KB
}

// nginxTuningSpec 描述一个参数：写在哪、怎么写、怎么校验、默认值多少。
type nginxTuningSpec struct {
	Key       string // JSON 字段名（界面用的名字）
	Directive string // nginx 指令名
	Context   string // main / events / http
	// Format 把界面值写成指令值。
	Format func(v NginxTuning) string
	// Parse 把指令值解析回界面值；ok=false 表示这个值面板不认识（保持原样、不覆盖）。
	Parse func(raw string) (any, bool)
	Def   any
}

func parseIntDefault(raw string, def int) (any, bool) {
	f := strings.Fields(strings.TrimSpace(raw))
	if len(f) == 0 {
		return nil, false
	}
	n, err := strconv.Atoi(f[0])
	if err != nil {
		return nil, false
	}
	return n, true
}

// tuningSpecs 是参数表（顺序即界面顺序，与宝塔那张表一致）。
var tuningSpecs = []nginxTuningSpec{
	{
		Key: "worker_processes", Directive: "worker_processes", Context: "main",
		Format: func(v NginxTuning) string { return v.WorkerProcesses },
		Parse: func(raw string) (any, bool) {
			s := strings.TrimSpace(raw)
			if s == "" {
				return nil, false
			}
			return s, true
		},
		Def: "auto",
	},
	{
		Key: "worker_connections", Directive: "worker_connections", Context: "events",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.WorkerConnections) },
		Parse:  func(raw string) (any, bool) { return parseIntDefault(raw, 1024) },
		Def:    1024,
	},
	{
		Key: "keepalive_timeout", Directive: "keepalive_timeout", Context: "http",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.KeepaliveTimeout) },
		Parse:  func(raw string) (any, bool) { return parseIntDefault(raw, 65) },
		Def:    65,
	},
	{
		Key: "gzip", Directive: "gzip", Context: "http",
		Format: func(v NginxTuning) string {
			if v.Gzip {
				return "on"
			}
			return "off"
		},
		Parse: func(raw string) (any, bool) {
			switch strings.TrimSpace(raw) {
			case "on":
				return true, true
			case "off":
				return false, true
			}
			return nil, false
		},
		Def: true,
	},
	{
		Key: "gzip_min_length_kb", Directive: "gzip_min_length", Context: "http",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.GzipMinLengthKB) + "k" },
		Parse: func(raw string) (any, bool) {
			f := strings.Fields(strings.TrimSpace(raw))
			if len(f) == 0 {
				return nil, false
			}
			s := strings.ToLower(f[0])
			mult := 1
			switch {
			case strings.HasSuffix(s, "k"):
				s = strings.TrimSuffix(s, "k")
			case strings.HasSuffix(s, "m"):
				s = strings.TrimSuffix(s, "m")
				mult = 1024
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				return nil, false
			}
			return n * mult, true
		},
		Def: 1,
	},
	{
		Key: "gzip_comp_level", Directive: "gzip_comp_level", Context: "http",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.GzipCompLevel) },
		Parse:  func(raw string) (any, bool) { return parseIntDefault(raw, 1) },
		Def:    1,
	},
	{
		Key: "client_max_body_size_mb", Directive: "client_max_body_size", Context: "http",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.ClientMaxBodySizeMB) + "m" },
		Parse: func(raw string) (any, bool) {
			f := strings.Fields(strings.TrimSpace(raw))
			if len(f) == 0 {
				return nil, false
			}
			s := strings.ToLower(f[0])
			mult := 1024 // 缺单位时 nginx 是字节；这里按"能不能放进 MB 表单"处理
			switch {
			case strings.HasSuffix(s, "m"):
				s = strings.TrimSuffix(s, "m")
			case strings.HasSuffix(s, "k"):
				s = strings.TrimSuffix(s, "k")
				mult = 1
			case strings.HasSuffix(s, "g"):
				s = strings.TrimSuffix(s, "g")
				mult = 1024 * 1024
			default:
				mult = 0 // 纯字节：太小，按未知处理，保留原样不覆盖
			}
			n, err := strconv.Atoi(s)
			if err != nil || mult == 0 {
				return nil, false
			}
			kb := n * mult
			mb := kb / 1024
			if mb < 1 {
				mb = 1
			}
			return mb, true
		},
		Def: 1,
	},
	{
		Key: "server_names_hash_bucket_size", Directive: "server_names_hash_bucket_size", Context: "http",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.ServerNamesHashBucketSize) },
		Parse:  func(raw string) (any, bool) { return parseIntDefault(raw, 64) },
		Def:    64,
	},
	{
		Key: "client_header_buffer_size_kb", Directive: "client_header_buffer_size", Context: "http",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.ClientHeaderBufferSizeKB) + "k" },
		Parse: func(raw string) (any, bool) {
			f := strings.Fields(strings.TrimSpace(raw))
			if len(f) == 0 {
				return nil, false
			}
			s := strings.ToLower(f[0])
			if !strings.HasSuffix(s, "k") {
				return nil, false
			}
			n, err := strconv.Atoi(strings.TrimSuffix(s, "k"))
			if err != nil {
				return nil, false
			}
			return n, true
		},
		Def: 1,
	},
	{
		Key: "client_body_buffer_size_kb", Directive: "client_body_buffer_size", Context: "http",
		Format: func(v NginxTuning) string { return strconv.Itoa(v.ClientBodyBufferSizeKB) + "k" },
		Parse: func(raw string) (any, bool) {
			f := strings.Fields(strings.TrimSpace(raw))
			if len(f) == 0 {
				return nil, false
			}
			s := strings.ToLower(f[0])
			if !strings.HasSuffix(s, "k") {
				return nil, false
			}
			n, err := strconv.Atoi(strings.TrimSuffix(s, "k"))
			if err != nil {
				return nil, false
			}
			return n, true
		},
		Def: 8,
	},
}

// DefaultTuning 返回 nginx 出厂值（读不到任何配置时的兜底，界面上要先有值）。
func DefaultTuning() NginxTuning {
	v := NginxTuning{}
	for _, sp := range tuningSpecs {
		switch sp.Key {
		case "worker_processes":
			v.WorkerProcesses = "auto"
		case "worker_connections":
			v.WorkerConnections = 1024
		case "keepalive_timeout":
			v.KeepaliveTimeout = 65
		case "gzip":
			v.Gzip = true
		case "gzip_min_length_kb":
			v.GzipMinLengthKB = 1
		case "gzip_comp_level":
			v.GzipCompLevel = 1
		case "client_max_body_size_mb":
			v.ClientMaxBodySizeMB = 1
		case "server_names_hash_bucket_size":
			v.ServerNamesHashBucketSize = 64
		case "client_header_buffer_size_kb":
			v.ClientHeaderBufferSizeKB = 1
		case "client_body_buffer_size_kb":
			v.ClientBodyBufferSizeKB = 8
		}
	}
	return v
}

// ValidateTuning 校验界面传来的值（返回人话错误）。
func ValidateTuning(v NginxTuning) error {
	wp := strings.TrimSpace(v.WorkerProcesses)
	if wp == "" {
		return fmt.Errorf("缺少 worker_processes —— 参数必须**整份**提交（表单里有它，值为 auto 或 1-64 的数字）；" +
			"面板不做「只改某一项」的部分更新，免得把没提交的项写成空值")
	}
	if wp != "auto" {
		n, err := strconv.Atoi(wp)
		if err != nil || n < 1 || n > 64 {
			return fmt.Errorf("worker_processes 只能是 auto 或 1-64 的数字，收到 %q", wp)
		}
	}
	check := func(name string, val, min, max int) error {
		if val < min || val > max {
			return fmt.Errorf("%s 应在 %d-%d 之间，收到 %d", name, min, max, val)
		}
		return nil
	}
	if err := check("worker_connections", v.WorkerConnections, 128, 65535); err != nil {
		return err
	}
	if err := check("keepalive_timeout（秒）", v.KeepaliveTimeout, 1, 3600); err != nil {
		return err
	}
	if err := check("gzip_min_length（KB）", v.GzipMinLengthKB, 0, 1024*1024); err != nil {
		return err
	}
	if err := check("gzip_comp_level", v.GzipCompLevel, 1, 9); err != nil {
		return err
	}
	// 上限 102400 MB = 100 GB：够大但不至于让用户填出一个 nginx 不认的值。
	if err := check("client_max_body_size（MB）", v.ClientMaxBodySizeMB, 1, 102400); err != nil {
		return err
	}
	if err := check("server_names_hash_bucket_size", v.ServerNamesHashBucketSize, 32, 1024); err != nil {
		return err
	}
	if err := check("client_header_buffer_size（KB）", v.ClientHeaderBufferSizeKB, 1, 1024); err != nil {
		return err
	}
	if err := check("client_body_buffer_size（KB）", v.ClientBodyBufferSizeKB, 8, 4096); err != nil {
		return err
	}
	return nil
}

// ---------- 解析与改写（纯函数，便于单测）----------

type confLine struct {
	text string
	// context 是这个行**所在**的上下文（main/events/http/其它块名）
	context string
	// depth 是进入本行时的花括号深度（0 = 顶层）
	depth int
}

// splitConfLines 逐行标注上下文。
//
// 只需要处理 nginx.conf 这种形态：一行一个指令、块用 `{`/`}`（可能同行）。
// 注释行与引号里的花括号都要忽略 —— 否则一份带注释的配置会把上下文解析歪。
func splitConfLines(content string) []confLine {
	lines := strings.Split(content, "\n")
	out := make([]confLine, 0, len(lines))
	stack := []string{"main"}
	depth := 0
	for _, ln := range lines {
		ctx := stack[len(stack)-1]
		out = append(out, confLine{text: ln, context: ctx, depth: depth})
		// 统计本行的花括号（忽略 # 之后的内容与引号内的内容）
		code := stripCommentAndStrings(ln)
		for _, ch := range code {
			switch ch {
			case '{':
				name := blockName(ln)
				stack = append(stack, name)
				depth++
			case '}':
				if len(stack) > 1 {
					stack = stack[:len(stack)-1]
					depth--
				}
			}
		}
	}
	return out
}

// stripCommentAndStrings 去掉 # 注释与引号里的内容（只看结构，不改原行）。
func stripCommentAndStrings(line string) string {
	var b strings.Builder
	inS, inD := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inS:
			if c == '\'' {
				inS = false
			}
		case inD:
			if c == '"' {
				inD = false
			}
		case c == '#':
			return b.String()
		case c == '\'':
			inS = true
		case c == '"':
			inD = true
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// blockName 从 `xxx {` 这种行里取块名（用于判断 events / http）。
func blockName(line string) string {
	code := stripCommentAndStrings(line)
	idx := strings.Index(code, "{")
	if idx < 0 {
		return ""
	}
	head := strings.TrimSpace(code[:idx])
	if head == "" {
		return ""
	}
	f := strings.Fields(head)
	return f[len(f)-1]
}

// directiveOf 返回这一行的指令名（非指令行返回空：注释、纯 `{`/`}`、空行）。
func directiveOf(line string) string {
	code := stripCommentAndStrings(line)
	trimmed := strings.TrimSpace(code)
	if trimmed == "" || trimmed == "{" || trimmed == "}" || strings.HasPrefix(trimmed, "{") {
		return ""
	}
	f := strings.Fields(trimmed)
	if len(f) == 0 {
		return ""
	}
	name := f[0]
	if strings.ContainsAny(name, "{}") {
		return ""
	}
	return name
}

// NginxTuningValues 从配置内容里读出各项参数（缺失的项用出厂默认）。
//
// 返回 (值, 每项是否真的在文件里找到了)。第二项用于界面如实标注
// "这一项 nginx.conf 里没有，显示的是出厂默认值"。
func NginxTuningValues(content string) (NginxTuning, map[string]bool) {
	vals := DefaultTuning()
	found := map[string]bool{}
	lines := splitConfLines(content)
	for _, sp := range tuningSpecs {
		for _, ln := range lines {
			if ln.context != sp.Context {
				continue
			}
			if directiveOf(ln.text) != sp.Directive {
				continue
			}
			raw := directiveValue(ln.text, sp.Directive)
			got, ok := sp.Parse(raw)
			if !ok {
				continue
			}
			// 同一个上下文里出现多次时**以最后一次为准**（nginx 也是这个语义）。
			applyTuningValue(&vals, sp.Key, got)
			found[sp.Key] = true
		}
	}
	return vals, found
}

// directiveValue 取 `directive value;` 里的 value（去掉指令名与结尾分号）。
func directiveValue(line, directive string) string {
	code := stripCommentAndStrings(line)
	code = strings.TrimSpace(code)
	code = strings.TrimSuffix(code, ";")
	code = strings.TrimSpace(strings.TrimPrefix(code, directive))
	return strings.TrimSpace(code)
}

// applyTuningValue 把一个解析出来的值写进结构体。
func applyTuningValue(v *NginxTuning, key string, val any) {
	switch key {
	case "worker_processes":
		if s, ok := val.(string); ok {
			v.WorkerProcesses = s
		}
	case "worker_connections":
		if n, ok := val.(int); ok {
			v.WorkerConnections = n
		}
	case "keepalive_timeout":
		if n, ok := val.(int); ok {
			v.KeepaliveTimeout = n
		}
	case "gzip":
		if b, ok := val.(bool); ok {
			v.Gzip = b
		}
	case "gzip_min_length_kb":
		if n, ok := val.(int); ok {
			v.GzipMinLengthKB = n
		}
	case "gzip_comp_level":
		if n, ok := val.(int); ok {
			v.GzipCompLevel = n
		}
	case "client_max_body_size_mb":
		if n, ok := val.(int); ok {
			v.ClientMaxBodySizeMB = n
		}
	case "server_names_hash_bucket_size":
		if n, ok := val.(int); ok {
			v.ServerNamesHashBucketSize = n
		}
	case "client_header_buffer_size_kb":
		if n, ok := val.(int); ok {
			v.ClientHeaderBufferSizeKB = n
		}
	case "client_body_buffer_size_kb":
		if n, ok := val.(int); ok {
			v.ClientBodyBufferSizeKB = n
		}
	}
}

// ApplyNginxTuning 把参数写进配置内容（**幂等**：同一个值写两遍结果一致）。
//
// 规则：
//   - 目标上下文里已有该指令 → 就地替换（保留原有缩进）；
//   - 同一上下文里出现多次 → 第一处替换成新值，其余注释掉并写明原因
//     （否则 nginx 会以 `directive is duplicate` 拒绝启动 —— 实测过）；
//   - 没有该指令 → 在该块的末尾（`}` 之前）按块内缩进插入。
func ApplyNginxTuning(content string, v NginxTuning) (string, error) {
	lines := splitConfLines(content)
	raw := make([]string, len(lines))
	for i, ln := range lines {
		raw[i] = ln.text
	}
	for _, sp := range tuningSpecs {
		// 找到目标块的结束行（该上下文里深度最浅的 `}`）
		insertAt := -1
		for i, ln := range lines {
			if ln.context != sp.Context {
				continue
			}
			if strings.TrimSpace(stripCommentAndStrings(ln.text)) == "}" && ln.depth == contextDepth(sp.Context) {
				insertAt = i
				break
			}
		}
		// 找到已有指令行
		targets := []int{}
		for i, ln := range lines {
			if ln.context != sp.Context {
				continue
			}
			if directiveOf(ln.text) == sp.Directive {
				targets = append(targets, i)
			}
		}
		value := sp.Format(v)
		newLine := func(indent string) string {
			return indent + sp.Directive + " " + value + ";"
		}
		switch {
		case len(targets) > 0:
			// 用第一处的缩进
			indent := raw[targets[0]][:len(raw[targets[0]])-len(strings.TrimLeft(raw[targets[0]], " \t"))]
			raw[targets[0]] = newLine(indent)
			for _, idx := range targets[1:] {
				// 重复指令会让 nginx 拒绝启动：整行注释掉并说明（保留原始内容便于用户回查）
				dupIndent := raw[idx][:len(raw[idx])-len(strings.TrimLeft(raw[idx], " \t"))]
				raw[idx] = dupIndent + "# ZizPanel：这一条是重复指令，已注释" +
					"（同一上下文里出现两次会让 nginx 报 duplicate 并拒绝启动）。原内容：" +
					strings.TrimSpace(raw[idx])
			}
		case insertAt >= 0:
			// 按块内已有指令的缩进插入；没有则按上下文给 4 空格缩进
			indent := "    "
			for i, ln := range lines {
				if ln.context != sp.Context || i == insertAt {
					continue
				}
				if directiveOf(ln.text) != "" {
					indent = raw[i][:len(raw[i])-len(strings.TrimLeft(raw[i], " \t"))]
					break
				}
			}
			raw = append(raw[:insertAt], append([]string{newLine(indent)}, raw[insertAt:]...)...)
			// 插入后行号变化：后面的 spec 会重新 split，这里直接重建 lines 以免索引错位
			lines = splitConfLines(strings.Join(raw, "\n"))
			continue
		default:
			// 目标上下文里既没有该指令、也找不到它的收尾 `}`（常见于
			// `events { worker_connections 1024; }` 这种**单行块**）。
			// 这种结构下面板**不猜、不改**：宁可如实报错，也不要把值写进错误的上下文
			// （写错会让 nginx 拒绝启动）。
			where := ""
			for _, ln := range lines {
				if directiveOf(ln.text) == sp.Directive {
					where = "（它在 " + ln.context + " 上下文里，或与块写在同一行）"
					break
				}
			}
			return "", fmt.Errorf("找不到 %s 在 %s 上下文里的位置%s —— "+
				"这份 nginx.conf 的结构面板不认（例如把块写在一行里），已放弃改写。"+
				"可以先把这一段展开成多行，或在「配置文件」里手工修改",
				sp.Directive, sp.Context, where)
		}
		lines = splitConfLines(strings.Join(raw, "\n"))
	}
	return strings.Join(raw, "\n"), nil
}

// contextDepth 返回某个上下文所在的花括号深度（main=0，events/http=1）。
func contextDepth(ctx string) int {
	switch ctx {
	case "main":
		return 0
	case "events", "http":
		return 1
	}
	return 0
}

// ---------- 生产路径：读 / 写 / 回读 ----------

// NginxTuningReadResult 是 GET 的返回。
type NginxTuningReadResult struct {
	File    string      `json:"file"`
	Version string      `json:"version"`
	Values  NginxTuning `json:"values"`
	// Found 标明每一项是"配置里真的写着"还是"用出厂默认值"。
	Found map[string]bool `json:"found"`
	// Effective 是 `nginx -T`（真实加载结果）里的值；读不到就是空。
	Effective map[string]string `json:"effective,omitempty"`
	// Conflicts 非空表示"我们写的值被别处的配置覆盖了"（如实报告，不假装生效）。
	Conflicts []string `json:"conflicts,omitempty"`
	// TestOutput 是 `nginx -t` 的输出（用户在界面上能直接看到配置是否合法）。
	TestOutput string `json:"test_output,omitempty"`
	Error      string `json:"error,omitempty"`
}

// NginxTuningRead 读取当前参数（解析 nginx.conf + 用 `nginx -T` 复核生效值）。
func NginxTuningRead() NginxTuningReadResult {
	path := NginxConf()
	res := NginxTuningReadResult{File: path}
	b, err := os.ReadFile(path)
	if err != nil {
		res.Error = "读取 nginx.conf 失败：" + err.Error()
		res.Values = DefaultTuning()
		res.Found = map[string]bool{}
		return res
	}
	vals, found := NginxTuningValues(string(b))
	res.Values, res.Found = vals, found
	res.Version = nginxVersion()
	if out, terr := NginxTest(); terr != nil {
		res.TestOutput = out
	} else {
		res.TestOutput = strings.TrimSpace(out)
	}
	// 生效值：`nginx -T` 打印的是**真实加载**的全部配置（含 include）。
	if dump, derr := nginxDumpConf(); derr == nil {
		effVals, _ := NginxTuningValues(dump)
		res.Effective = map[string]string{}
		for _, sp := range tuningSpecs {
			res.Effective[sp.Key] = tuningEffectiveString(effVals, sp.Key)
			// 冲突判定：真实加载的值与我们解析 nginx.conf 得到的值不一致
			if tuningEffectiveString(effVals, sp.Key) != tuningEffectiveString(vals, sp.Key) {
				res.Conflicts = append(res.Conflicts, sp.Directive+" 的真实生效值（"+
					tuningEffectiveString(effVals, sp.Key)+"）与 nginx.conf 里的写法（"+
					tuningEffectiveString(vals, sp.Key)+"）不一致 —— 可能被 conf.d 或 include 的其它文件覆盖")
			}
		}
	}
	return res
}

// tuningEffectiveString 把一个参数值格式化成稳定的字符串（用于比较与展示）。
func tuningEffectiveString(v NginxTuning, key string) string {
	for _, sp := range tuningSpecs {
		if sp.Key == key {
			return sp.Format(v)
		}
	}
	return ""
}

// NginxTuningWrite 把参数写进 nginx.conf：备份 → 改写 → `nginx -t` → 失败回滚 → reload → 回读。
func NginxTuningWrite(v NginxTuning) (NginxTuningReadResult, error) {
	if err := ValidateTuning(v); err != nil {
		return NginxTuningReadResult{}, err
	}
	path := NginxConf()
	before, err := os.ReadFile(path)
	if err != nil {
		return NginxTuningReadResult{}, fmt.Errorf("读取 nginx.conf 失败: %w", err)
	}
	next, err := ApplyNginxTuning(string(before), v)
	if err != nil {
		return NginxTuningReadResult{}, err
	}
	if next == string(before) {
		// 幂等：内容没变就不写盘、不 reload（用户点两次保存不该动 nginx）。
		res := NginxTuningRead()
		res.Error = ""
		return res, nil
	}
	// 备份（用户能自己回滚；失败不阻断 —— 真正的保险是"改写后 nginx -t 不过就回滚内存里的副本"）
	if err := os.WriteFile(NginxConfBackupPath(), before, 0o644); err != nil {
		return NginxTuningReadResult{}, fmt.Errorf("备份 nginx.conf 失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return NginxTuningReadResult{}, fmt.Errorf("写入 nginx.conf 失败: %w", err)
	}
	if out, terr := NginxTest(); terr != nil {
		// 回滚：绝不留下一个起不来的 nginx
		_ = os.WriteFile(path, before, 0o644)
		return NginxTuningReadResult{}, fmt.Errorf("新配置没通过 nginx -t，已回滚：\n%s", out)
	}
	if err := NginxReload(); err != nil {
		_ = os.WriteFile(path, before, 0o644)
		_ = NginxReload()
		return NginxTuningReadResult{}, fmt.Errorf("配置已写入但重载失败，已回滚：%w", err)
	}
	return NginxTuningRead(), nil
}

// nginxVersion 取 `nginx -v`（输出在 stderr，形如 `nginx version: nginx/1.31.5`）。
func nginxVersion() string {
	r := run(NginxBin(), "-v")
	return strings.TrimSpace(r.combined())
}

// nginxDumpConf 取 `nginx -T`（把真实加载的配置全部打印出来）。
func nginxDumpConf() (string, error) {
	r := run(NginxBin(), "-c", NginxConf(), "-T")
	if r.err != nil {
		return "", fmt.Errorf("nginx -T 失败: %s", r.combined())
	}
	return r.combined(), nil
}

// TuningJSON 是 helper 与面板之间传参用的编解码（结构化，不用 shell 字符串）。
func TuningEncode(v NginxTuning) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// TuningDecode 解析面板传来的 JSON。
func TuningDecode(s string) (NginxTuning, error) {
	var v NginxTuning
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return v, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	return v, nil
}
