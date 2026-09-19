package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// sysOverview 是**只读同步工具**打样：sysctl / vm_stat / df -h 的诚实解析。
// 任何一条命令失败都会如实列出（不谎报"系统正常"）。
type sysOverview struct{}

func init() { Add(sysOverview{}) }

func (sysOverview) Meta() tool.Meta {
	_, hasSysctl := execx.LookPath("sysctl")
	_, hasVM := execx.LookPath("vm_stat")
	_, hasDF := execx.LookPath("df")
	ok := hasSysctl && hasVM && hasDF
	reason := ""
	if !ok {
		reason = "缺少系统命令：sysctl/vm_stat/df"
	}
	return tool.Meta{
		ID: "sys.overview", Name: "系统概览", Category: "sys", Icon: "cpu",
		Summary:   "读硬件型号、CPU、内存、负载与磁盘用量。",
		Available: ok, UnavailableReason: reason,
		TimeoutSeconds: 40,
		Params: []tool.Param{
			{Name: "note", Label: "备注", Type: tool.TypeText,
				Placeholder: "可留空，只写进审计日志",
				Help:        "不参与采集，仅方便记录这次查看的原因。"},
		},
	}
}

func (sysOverview) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	data := map[string]any{}
	problems := []string{}

	sysctlOut, err := readCmd(ctx, c, "sysctl", "-n",
		"machdep.cpu.brand_string", "hw.model", "hw.ncpu", "hw.memsize",
		"hw.logicalcpu", "hw.physicalcpu", "kern.osproductversion", "kern.osrelease")
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		data["hardware"] = parseSysctl(sysctlOut)
	}

	loadOut, err := readCmd(ctx, c, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		data["load"] = parseLoadAvg(loadOut)
	}

	vmOut, err := readCmd(ctx, c, "vm_stat")
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		data["memory"] = parseVMStat(vmOut)
	}

	dfOut, err := readCmd(ctx, c, "df", "-h")
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		data["disks"] = parseDF(dfOut)
	}

	data["paths"] = map[string]any{
		"read_roots":  c.Guard.ReadRoots(),
		"write_roots": c.Guard.WriteRoots(),
	}
	if len(problems) > 0 {
		data["unavailable"] = problems
	}
	msg := fmt.Sprintf("已采集 %d 项；%d 项不可用", len(data)-1, len(problems))
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// readCmd 跑一条只读命令，失败时返回真实原因（不返回空数据装成功）。
func readCmd(ctx context.Context, c *tool.Ctx, name string, args ...string) (string, error) {
	res := c.Exec.Run(ctx, 8*time.Second, name, args...)
	if res.TimedOut {
		return "", fmt.Errorf("%s 超时被终止", name)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s 失败（退出码 %d）：%s", name, res.ExitCode, firstLine(res.Output()))
	}
	return res.Stdout, nil
}

func parseSysctl(out string) map[string]any {
	keys := []string{"machdep.cpu.brand_string", "hw.model", "hw.ncpu", "hw.memsize",
		"hw.logicalcpu", "hw.physicalcpu", "kern.osproductversion", "kern.osrelease"}
	vals := strings.Split(strings.TrimRight(out, "\n"), "\n")
	m := map[string]any{"keys": keys}
	for i, k := range keys {
		if i < len(vals) {
			v := strings.TrimSpace(vals[i])
			if v == "" {
				continue
			}
			if k == "hw.memsize" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					m[k] = n
					m["memory_human"] = humanSize(n)
					continue
				}
			}
			if n, err := strconv.Atoi(v); err == nil && (k == "hw.ncpu" || k == "hw.logicalcpu" || k == "hw.physicalcpu") {
				m[k] = n
				continue
			}
			m[k] = v
		}
	}
	return m
}

var loadRe = regexp.MustCompile(`\{([^}]*)\}`)

func parseLoadAvg(out string) map[string]any {
	m := map[string]any{}
	if m2 := loadRe.FindStringSubmatch(out); len(m2) == 2 {
		parts := strings.Fields(strings.TrimSpace(m2[1]))
		names := []string{"1m", "5m", "15m"}
		for i, n := range names {
			if i < len(parts) {
				if f, err := strconv.ParseFloat(parts[i], 64); err == nil {
					m[n] = f
				}
			}
		}
	}
	return m
}

var vmLineRe = regexp.MustCompile(`^"?([^":]+)"?:\s+(\d+)\.?$`)

func parseVMStat(out string) map[string]any {
	res := map[string]any{}
	var pageSize int64
	pages := map[string]int64{}
	for _, ln := range strings.Split(out, "\n") {
		if ps := strings.Contains(ln, "page size of"); ps {
			fields := strings.Fields(ln)
			for i, f := range fields {
				if f == "of" && i+1 < len(fields) {
					if n, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
						pageSize = n
					}
				}
			}
		}
		if m := vmLineRe.FindStringSubmatch(strings.TrimSpace(ln)); len(m) == 3 {
			if n, err := strconv.ParseInt(m[2], 10, 64); err == nil {
				pages[strings.TrimSpace(m[1])] = n
			}
		}
	}
	if pageSize == 0 {
		pageSize = 4096
	}
	res["page_size"] = pageSize
	mul := func(k string) int64 { return pages[k] * pageSize }
	res["free_bytes"] = mul("Pages free")
	res["active_bytes"] = mul("Pages active")
	res["inactive_bytes"] = mul("Pages inactive")
	res["wired_bytes"] = mul("Pages wired down")
	res["compressed_bytes"] = mul("Pages occupied by compressor")
	res["free_human"] = humanSize(mul("Pages free"))
	res["active_human"] = humanSize(mul("Pages active"))
	res["wired_human"] = humanSize(mul("Pages wired down"))
	res["compressed_human"] = humanSize(mul("Pages occupied by compressor"))
	res["speculative_human"] = humanSize(mul("Pages speculative"))
	return res
}

func parseDF(out string) []map[string]any {
	rows := []map[string]any{}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, ln := range lines {
		if i == 0 {
			continue // 表头
		}
		f := strings.Fields(ln)
		if len(f) < 6 {
			continue
		}
		mount := strings.Join(f[8:], " ")
		rows = append(rows, map[string]any{
			"filesystem": f[0], "size": f[1], "used": f[2],
			"avail": f[3], "capacity": f[4], "mount": mount,
		})
	}
	return rows
}
