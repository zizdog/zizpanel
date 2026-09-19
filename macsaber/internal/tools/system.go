package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// ============================================================================
//  sys.network_quality
// ============================================================================

type sysNetworkQuality struct{}

func init() { Add(sysNetworkQuality{}) }

func (sysNetworkQuality) Meta() tool.Meta {
	_, ok := execx.LookPath("networkQuality")
	reason := ""
	if !ok {
		reason = "缺少系统命令 networkQuality（macOS 12+）"
	}
	return tool.Meta{
		ID: "sys.network_quality", Name: "网络质量测量", Category: "sys", Icon: "gauge",
		Summary: "原生测速：下行/上行、延迟、响应度，约 1 分钟。",
		Async:   true, Available: ok, UnavailableReason: reason, TimeoutSeconds: 300,
	}
}

// nqResult 是 networkQuality -c 的 JSON 字段（不同系统版本给的键不一样，按需取）。
type nqResult struct {
	DLThroughput     float64 `json:"dl_throughput"`
	ULThroughput     float64 `json:"ul_throughput"`
	DLResponsiveness float64 `json:"dl_responsiveness"`
	ULResponsiveness float64 `json:"ul_responsiveness"`
	Responsiveness   float64 `json:"responsiveness"`
	BaseRTT          float64 `json:"base_rtt"`
	IdleLatency      float64 `json:"idle_latency"`
	DLLatency        float64 `json:"dl_latency"`
	ULLatency        float64 `json:"ul_latency"`
	InterfaceName    string  `json:"interface_name"`
	TestEndpoint     string  `json:"test_endpoint"`
	OSVersion        string  `json:"os_version"`
	StartDate        string  `json:"start_date"`
	EndDate          string  `json:"end_date"`
}

func (sysNetworkQuality) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	c.Logf("开始测量，会占用带宽约 1 分钟")
	c.Progress(5, "测量中")
	res := c.Exec.Run(ctx, 4*time.Minute, "networkQuality", "-c")
	if res.TimedOut {
		return nil, fmt.Errorf("networkQuality 超过 4 分钟未完成，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("networkQuality 失败（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}
	nq, err := parseNetworkQuality(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("解析测量结果失败：%v", err)
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("下行 %.1f Mbps / 上行 %.1f Mbps", nq["download_mbps"], nq["upload_mbps"]),
		Data: nq,
	}, nil
}

func parseNetworkQuality(out string) (map[string]any, error) {
	var raw nqResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return nil, err
	}
	if raw.DLThroughput == 0 && raw.ULThroughput == 0 {
		return nil, fmt.Errorf("没测到吞吐（下行/上行都是 0）")
	}
	data := map[string]any{
		"download_mbps":      round1(raw.DLThroughput / 1e6),
		"upload_mbps":        round1(raw.ULThroughput / 1e6),
		"responsiveness_rpm": round1(raw.Responsiveness),
		"interface":          raw.InterfaceName,
		"endpoint":           raw.TestEndpoint,
		"os_version":         raw.OSVersion,
	}
	if raw.BaseRTT > 0 {
		data["latency_ms"] = round1(raw.BaseRTT)
		data["latency_source"] = "base_rtt"
	} else if raw.IdleLatency > 0 {
		data["latency_ms"] = round1(raw.IdleLatency)
		data["latency_source"] = "idle_latency"
	}
	if raw.DLResponsiveness > 0 {
		data["download_responsiveness_rpm"] = round1(raw.DLResponsiveness)
	}
	if raw.ULResponsiveness > 0 {
		data["upload_responsiveness_rpm"] = round1(raw.ULResponsiveness)
	}
	if raw.DLLatency > 0 {
		data["download_latency_ms"] = round1(raw.DLLatency)
	}
	if raw.ULLatency > 0 {
		data["upload_latency_ms"] = round1(raw.ULLatency)
	}
	if d := durationBetween(raw.StartDate, raw.EndDate); d > 0 {
		data["duration_seconds"] = round1(d)
	}
	return data, nil
}

func durationBetween(start, end string) float64 {
	const layout = "2006-01-02 15:04:05.000"
	s, err1 := time.Parse(layout, strings.TrimSpace(start))
	e, err2 := time.Parse(layout, strings.TrimSpace(end))
	if err1 != nil || err2 != nil {
		return 0
	}
	return e.Sub(s).Seconds()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// ============================================================================
//  sys.dns
// ============================================================================

type sysDNS struct{}

func init() { Add(sysDNS{}) }

func (sysDNS) Meta() tool.Meta {
	_, ok := execx.LookPath("scutil")
	reason := ""
	if !ok {
		reason = "缺少系统命令 scutil"
	}
	return tool.Meta{
		ID: "sys.dns", Name: "DNS 解析器", Category: "sys", Icon: "network",
		Summary:   "读当前 resolver、nameserver 与搜索域，只读。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 20,
	}
}

func (sysDNS) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	res, err := execTool(ctx, c, 15*time.Second, "scutil", "--dns")
	if err != nil {
		return nil, err
	}
	data := parseDNS(res.Stdout)
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("%d 个解析器，%d 个搜索域", data["resolver_count"], len(data["search_domains"].([]string))),
		Data: data,
	}, nil
}

var (
	dnsResolverRe = regexp.MustCompile(`^resolver #(\d+)\s*$`)
	dnsSearchRe   = regexp.MustCompile(`^search domain\[(\d+)\]\s*:\s*(.+)$`)
	dnsNSRe       = regexp.MustCompile(`^nameserver\[(\d+)\]\s*:\s*(.+)$`)
	dnsKVRe       = regexp.MustCompile(`^([a-z_]+)\s*:\s*(.+)$`)
)

func parseDNS(out string) map[string]any {
	resolvers := []map[string]any{}
	searchDomains := []string{}
	nameservers := []string{}
	seenDomain := map[string]bool{}
	seenNS := map[string]bool{}
	scoped := false
	var cur map[string]any

	flush := func() {
		if cur == nil {
			return
		}
		if ds, ok := cur["search_domains"].([]string); ok {
			for _, d := range ds {
				if !seenDomain[d] {
					seenDomain[d] = true
					searchDomains = append(searchDomains, d)
				}
			}
		}
		if ns, ok := cur["nameservers"].([]string); ok {
			for _, n := range ns {
				if !seenNS[n] {
					seenNS[n] = true
					nameservers = append(nameservers, n)
				}
			}
		}
		resolvers = append(resolvers, cur)
		cur = nil
	}

	for _, raw := range strings.Split(out, "\n") {
		ln := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(ln, "DNS configuration") {
			flush()
			scoped = strings.Contains(ln, "scoped")
			continue
		}
		trimmed := strings.TrimSpace(ln)
		if m := dnsResolverRe.FindStringSubmatch(trimmed); len(m) == 2 {
			flush()
			n, _ := strconv.Atoi(m[1])
			cur = map[string]any{"index": n, "scoped": scoped}
			continue
		}
		if cur == nil {
			continue
		}
		if m := dnsSearchRe.FindStringSubmatch(trimmed); len(m) == 3 {
			ds, _ := cur["search_domains"].([]string)
			cur["search_domains"] = append(ds, strings.TrimSpace(m[2]))
			continue
		}
		if m := dnsNSRe.FindStringSubmatch(trimmed); len(m) == 3 {
			ns, _ := cur["nameservers"].([]string)
			cur["nameservers"] = append(ns, strings.TrimSpace(m[2]))
			continue
		}
		if m := dnsKVRe.FindStringSubmatch(trimmed); len(m) == 3 {
			cur[m[1]] = strings.TrimSpace(m[2])
		}
	}
	flush()

	scopedCount := 0
	for _, r := range resolvers {
		if r["scoped"] == true {
			scopedCount++
		}
	}
	return map[string]any{
		"resolver_count": len(resolvers), "scoped_count": scopedCount,
		"search_domains": searchDomains, "nameservers": nameservers,
		"resolvers": resolvers,
	}
}

// ============================================================================
//  sys.processes
// ============================================================================

type sysProcesses struct{}

func init() { Add(sysProcesses{}) }

func (sysProcesses) Meta() tool.Meta {
	_, ok := execx.LookPath("ps")
	reason := ""
	if !ok {
		reason = "缺少系统命令 ps"
	}
	return tool.Meta{
		ID: "sys.processes", Name: "进程占用", Category: "sys", Icon: "list",
		Summary:   "按 CPU 或内存列出占用最高的进程，只读。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 20,
		Params: []tool.Param{
			{Name: "sort", Label: "排序", Type: tool.TypeSelect, Required: true, Default: "cpu",
				Options: []tool.Option{
					{Value: "cpu", Label: "按 CPU"},
					{Value: "mem", Label: "按内存"},
				}, Help: "用 ps 的 -r/-m 排序，不用 top。"},
			{Name: "count", Label: "条数", Type: tool.TypeNumber, Default: 15,
				Min: Num(1), Max: Num(50), Help: "最多列出 N 条。"},
		},
	}
}

var psLineRe = regexp.MustCompile(`^\s*(\d+)\s+(\S+)\s+([\d.]+)\s+([\d.]+)\s+(\d+)\s+(.+?)\s*$`)

func (sysProcesses) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	sortBy := in.Str("sort")
	count := in.Int("count", 15)
	flag := "-r"
	if sortBy == "mem" {
		flag = "-m"
	}
	res, err := execTool(ctx, c, 15*time.Second, "ps",
		"-axo", "pid=,user=,pcpu=,pmem=,rss=,comm=", flag)
	if err != nil {
		return nil, err
	}
	rows := parsePS(res.Stdout, count)
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("按%s列出 %d 个进程", map[string]string{"cpu": "CPU", "mem": "内存"}[sortBy], len(rows)),
		Data: map[string]any{"sort": sortBy, "count": len(rows), "processes": rows},
	}, nil
}

func parsePS(out string, limit int) []map[string]any {
	rows := []map[string]any{}
	for _, ln := range strings.Split(out, "\n") {
		if len(rows) >= limit {
			break
		}
		m := psLineRe.FindStringSubmatch(strings.TrimRight(ln, "\r"))
		if len(m) != 7 {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		cpu, _ := strconv.ParseFloat(m[3], 64)
		mem, _ := strconv.ParseFloat(m[4], 64)
		rssKB, _ := strconv.ParseInt(m[5], 10, 64)
		rows = append(rows, map[string]any{
			"pid": pid, "user": m[2], "cpu_percent": cpu, "mem_percent": mem,
			"rss_bytes": rssKB * 1024, "rss_human": humanSize(rssKB * 1024),
			"command": m[6],
		})
	}
	return rows
}

// ============================================================================
//  sys.launchd
// ============================================================================

type sysLaunchd struct{}

func init() { Add(sysLaunchd{}) }

func (sysLaunchd) Meta() tool.Meta {
	_, ok := execx.LookPath("launchctl")
	reason := ""
	if !ok {
		reason = "缺少系统命令 launchctl"
	}
	return tool.Meta{
		ID: "sys.launchd", Name: "launchd 服务", Category: "sys", Icon: "list",
		Summary:   "只读列出 launchd 服务状态，可按关键词过滤。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 25,
		Params: []tool.Param{
			{Name: "filter", Label: "关键词", Type: tool.TypeText,
				Placeholder: "留空列出全部", Help: "按服务标签模糊匹配。"},
			{Name: "limit", Label: "条数", Type: tool.TypeNumber, Default: 50,
				Min: Num(1), Max: Num(200), Help: "最多列出 N 条。"},
		},
	}
}

func (sysLaunchd) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	res, err := execTool(ctx, c, 20*time.Second, "launchctl", "list")
	if err != nil {
		return nil, err
	}
	items, running, total := parseLaunchctl(res.Stdout)
	filter := strings.ToLower(strings.TrimSpace(in.Str("filter")))
	limit := in.Int("limit", 50)
	matched := items
	if filter != "" {
		matched = make([]map[string]any, 0, len(items))
		for _, it := range items {
			if strings.Contains(strings.ToLower(fmt.Sprint(it["label"])), filter) {
				matched = append(matched, it)
			}
		}
	}
	out := matched
	if len(out) > limit {
		out = out[:limit]
	}
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("共 %d 项，运行中 %d，匹配 %d", total, running, len(matched)),
		Data: map[string]any{
			"total": total, "running": running, "matched": len(matched),
			"returned": len(out), "items": out,
		},
	}, nil
}

func parseLaunchctl(out string) ([]map[string]any, int, int) {
	items := []map[string]any{}
	running := 0
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if i == 0 && strings.HasPrefix(ln, "PID") {
			continue
		}
		f := strings.Split(ln, "\t")
		if len(f) < 3 {
			f = strings.Fields(ln)
		}
		if len(f) < 3 {
			continue
		}
		item := map[string]any{"label": strings.TrimSpace(f[2]), "running": false}
		pidField := strings.TrimSpace(f[0])
		if pidField != "-" && pidField != "" {
			if pid, err := strconv.Atoi(pidField); err == nil {
				item["pid"] = pid
				item["running"] = true
				running++
			}
		}
		st := strings.TrimSpace(f[1])
		item["status"] = st
		if st != "-" && st != "" {
			if code, err := strconv.Atoi(st); err == nil {
				item["last_exit"] = code
			}
		}
		items = append(items, item)
	}
	return items, running, len(items)
}

// ============================================================================
//  sys.logs
// ============================================================================

type sysLogs struct{}

func init() { Add(sysLogs{}) }

// logPresetOrder 与 logPresetLabels 决定下拉顺序；predicate 只来自常量（防注入）。
var logPresetOrder = []string{"errors", "faults", "kernel", "network", "nginx", "self"}

var logPresetLabels = map[string]string{
	"errors": "错误级别", "faults": "故障级别", "kernel": "内核",
	"network": "网络子系统", "nginx": "nginx", "self": "本工具自身进程",
}

var logPresetPredicates = map[string]string{
	"errors": `messageType == error`, "faults": `messageType == fault`,
	"kernel": `process == "kernel"`, "network": `subsystem == "com.apple.network"`,
	"nginx": `process == "nginx"`,
}

// logPredicate 只认预设；self 用运行体自己的进程名（不是用户输入）。
func logPredicate(preset string) (string, bool) {
	if preset == "self" {
		return fmt.Sprintf("process == %q", selfProcessName()), true
	}
	p, ok := logPresetPredicates[preset]
	return p, ok
}

func selfProcessName() string {
	exe, err := os.Executable()
	if err != nil {
		return "macsaber"
	}
	var b strings.Builder
	for _, r := range filepath.Base(exe) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "macsaber"
	}
	return b.String()
}

func logPresetOptions() []tool.Option {
	out := make([]tool.Option, 0, len(logPresetOrder))
	for _, k := range logPresetOrder {
		out = append(out, tool.Option{Value: k, Label: logPresetLabels[k]})
	}
	return out
}

func (sysLogs) Meta() tool.Meta {
	_, ok := execx.LookPath("log")
	reason := ""
	if !ok {
		reason = "缺少系统命令 log"
	}
	return tool.Meta{
		ID: "sys.logs", Name: "统一日志", Category: "sys", Icon: "terminal",
		Summary: "按预设条件读最近日志，条件不可自定义。",
		Async:   true, Available: ok, UnavailableReason: reason, TimeoutSeconds: 180,
		Params: []tool.Param{
			{Name: "preset", Label: "条件", Type: tool.TypeSelect, Required: true, Default: "errors",
				Options: logPresetOptions(), Help: "只支持这几个预设，防注入。"},
			{Name: "window", Label: "时间窗口", Type: tool.TypeSelect, Required: true, Default: "5m",
				Options: []tool.Option{
					{Value: "5m", Label: "最近 5 分钟"},
					{Value: "30m", Label: "最近 30 分钟"},
					{Value: "2h", Label: "最近 2 小时"},
				}, Help: "窗口越大越慢。"},
			{Name: "limit", Label: "返回行数", Type: tool.TypeNumber, Default: 50,
				Min: Num(1), Max: Num(200), Help: "返回最早的 N 行。"},
		},
	}
}

func (sysLogs) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	preset := in.Str("preset")
	pred, ok := logPredicate(preset)
	if !ok {
		return nil, fmt.Errorf("不支持的日志预设：%s", preset)
	}
	window := in.Str("window")
	limit := in.Int("limit", 50)
	c.Logf("查询最近 %s 的日志（%s）", window, logPresetLabels[preset])
	c.Progress(10, "查询日志")
	res := c.Exec.Run(ctx, 90*time.Second, "log", "show", "--last", window, "--predicate", pred, "--style", "compact")
	if res.TimedOut {
		return nil, fmt.Errorf("log show 超过 90 秒未返回，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("log show 失败（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}
	lines := splitOutputLines(res.Stdout)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "Timestamp") {
		lines = lines[1:]
	}
	shown := lines
	if len(shown) > limit {
		shown = shown[:limit]
	}
	msg := fmt.Sprintf("命中 %d 行，返回前 %d 行", len(lines), len(shown))
	if len(lines) == 0 {
		// 进程名/子系统没日志是正常结果，不当失败。
		msg = "该窗口内没有匹配日志"
	}
	c.Progress(100, "完成")
	return &tool.Result{
		OK: true, Msg: msg,
		Data: map[string]any{
			"preset": preset, "predicate": pred, "window": window,
			"matched_lines": len(lines), "returned": len(shown),
			"lines": shown, "truncated": res.TruncOut,
		},
	}, nil
}

// ============================================================================
//  sys.power
// ============================================================================

type sysPower struct{}

func init() { Add(sysPower{}) }

func (sysPower) Meta() tool.Meta {
	_, ok := execx.LookPath("pmset")
	reason := ""
	if !ok {
		reason = "缺少系统命令 pmset"
	}
	return tool.Meta{
		ID: "sys.power", Name: "电源与睡眠", Category: "sys", Icon: "power",
		Summary:   "读当前电源来源与睡眠设置，只读。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 20,
	}
}

func (sysPower) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	res, err := execTool(ctx, c, 15*time.Second, "pmset", "-g")
	if err != nil {
		return nil, err
	}
	system, current := parsePMSet(res.Stdout)
	data := map[string]any{"system_settings": system, "settings": current}
	problems := []string{}
	batt, berr := execTool(ctx, c, 15*time.Second, "pmset", "-g", "batt")
	if berr != nil {
		problems = append(problems, berr.Error())
	} else {
		source, battery := parsePMSetBatt(batt.Stdout)
		if source != "" {
			data["power_source"] = source
		}
		if battery != nil {
			data["battery"] = battery
		}
	}
	if len(problems) > 0 {
		data["unavailable"] = problems
	}
	msg := "已读电源设置"
	if s, ok := data["power_source"].(string); ok && s != "" {
		msg = "当前电源：" + s
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// parsePMSet 解析 "System-wide power settings:" / "Currently in use:" 两段。
func parsePMSet(out string) (map[string]string, map[string]string) {
	system := map[string]string{}
	current := map[string]string{}
	target := system
	for _, raw := range strings.Split(out, "\n") {
		ln := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, " ") && strings.HasSuffix(trimmed, ":") {
			if strings.HasPrefix(trimmed, "Currently in use") {
				target = current
			} else {
				target = system
			}
			continue
		}
		i := strings.LastIndexFunc(trimmed, unicode.IsSpace)
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:i])
		val := strings.TrimSpace(trimmed[i+1:])
		if key != "" {
			target[key] = val
		}
	}
	return system, current
}

var (
	battSourceRe = regexp.MustCompile(`Now drawing from '([^']+)'`)
	battPctRe    = regexp.MustCompile(`(\d+)%`)
	battLeftRe   = regexp.MustCompile(`([0-9]+:[0-9]+) remaining`)
)

func parsePMSetBatt(out string) (string, map[string]any) {
	source := ""
	if m := battSourceRe.FindStringSubmatch(out); len(m) == 2 {
		source = m[1]
	}
	var battery map[string]any
	for _, raw := range strings.Split(out, "\n") {
		ln := strings.TrimRight(raw, "\r")
		if !strings.Contains(ln, "%") {
			continue
		}
		battery = map[string]any{"raw": strings.TrimSpace(ln)}
		if m := battPctRe.FindStringSubmatch(ln); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				battery["percent"] = n
			}
		}
		if i := strings.Index(ln, "%"); i >= 0 {
			rest := strings.TrimSpace(ln[i+1:])
			rest = strings.TrimLeft(rest, "; ")
			if j := strings.IndexByte(rest, ';'); j > 0 {
				battery["state"] = strings.TrimSpace(rest[:j])
			}
		}
		if m := battLeftRe.FindStringSubmatch(ln); len(m) == 2 {
			battery["remaining"] = m[1]
		}
		battery["present"] = strings.Contains(ln, "present: true")
		break
	}
	return source, battery
}
