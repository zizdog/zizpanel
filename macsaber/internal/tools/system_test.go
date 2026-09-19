package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
)

func TestSystemToolsRegistered(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAll(reg)
	want := []struct {
		id    string
		async bool
	}{
		{"sys.network_quality", true},
		{"sys.dns", false},
		{"sys.processes", false},
		{"sys.launchd", false},
		{"sys.logs", true},
		{"sys.power", false},
	}
	for _, w := range want {
		m := metaOf(t, reg, w.id)
		if m.Category != "sys" {
			t.Errorf("%s 分类应为 sys，实际 %s", w.id, m.Category)
		}
		if m.Async != w.async {
			t.Errorf("%s async 应为 %v", w.id, w.async)
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s summary 超过 40 字：%q", w.id, m.Summary)
		}
	}
}

func TestSystemUnavailableWhenCommandsMissing(t *testing.T) {
	pathWithShims(t, nil)
	reg := tool.NewRegistry()
	RegisterAll(reg)
	bins := map[string]string{
		"sys.network_quality": "networkQuality", "sys.dns": "scutil",
		"sys.processes": "ps", "sys.launchd": "launchctl",
		"sys.logs": "log", "sys.power": "pmset",
	}
	for id, bin := range bins {
		m := metaOf(t, reg, id)
		if m.Available {
			t.Errorf("%s 在无命令环境应不可用", id)
		}
		if !strings.Contains(m.UnavailableReason, bin) {
			t.Errorf("%s 原因应点出 %s，实际 %q", id, bin, m.UnavailableReason)
		}
	}
}

// ---------- sys.dns ----------

const dnsFixture = `DNS configuration

resolver #1
  search domain[0] : lan
  nameserver[0] : 192.168.1.1
  nameserver[1] : fd58:cbf6:7fb8::1
  if_index : 11 (en0)
  flags    : Request A records, Request AAAA records
  reach    : 0x00020002 (Reachable,Directly Reachable Address)

resolver #2
  domain   : local
  options  : mdns
  timeout  : 5
  order    : 300000

DNS configuration (for scoped queries)

resolver #1
  search domain[0] : lan
  nameserver[0] : 192.168.1.1
  if_index : 11 (en0)
  flags    : Scoped, Request A records
`

func TestParseDNS(t *testing.T) {
	d := parseDNS(dnsFixture)
	if d["resolver_count"].(int) != 3 {
		t.Fatalf("应解析 3 个 resolver，实际 %v", d["resolver_count"])
	}
	if d["scoped_count"].(int) != 1 {
		t.Errorf("应有 1 个 scoped resolver，实际 %v", d["scoped_count"])
	}
	if got := strings.Join(d["nameservers"].([]string), ","); got != "192.168.1.1,fd58:cbf6:7fb8::1" {
		t.Errorf("nameserver 集合错: %s", got)
	}
	if got := strings.Join(d["search_domains"].([]string), ","); got != "lan" {
		t.Errorf("搜索域应去重为 lan，实际 %s", got)
	}
	r1 := d["resolvers"].([]map[string]any)[0]
	if !strings.Contains(fmt.Sprint(r1["flags"]), "Request A records") {
		t.Errorf("resolver 1 flags 解析错: %v", r1)
	}
}

// ---------- sys.processes ----------

const psFixture = ` 4461 zizdog            83.0  0.5  91536 /var/folders/x/web.test
 3790 zizdog            42.0 10.5 1767296 /System/Library/WebContent
 1234 root              0.1  0.0  1024 /usr/sbin/something with spaces
`

func TestParsePS(t *testing.T) {
	rows := parsePS(psFixture, 2)
	if len(rows) != 2 {
		t.Fatalf("limit 2 应只返回 2 行，实际 %d", len(rows))
	}
	if rows[0]["pid"].(int) != 4461 || rows[0]["cpu_percent"].(float64) != 83.0 {
		t.Errorf("首行解析错: %+v", rows[0])
	}
	if rows[0]["rss_bytes"].(int64) != 91536*1024 {
		t.Errorf("RSS 应换算成字节: %v", rows[0]["rss_bytes"])
	}
	all := parsePS(psFixture, 50)
	if !strings.Contains(all[2]["command"].(string), "something with spaces") {
		t.Errorf("带空格的命令名应完整保留: %v", all[2]["command"])
	}
}

// ---------- sys.launchd ----------

const launchctlFixture = "PID\tStatus\tLabel\n" +
	"-\t0\tcom.apple.SafariHistoryServiceAgent\n" +
	"1242\t-9\tcom.apple.progressd\n" +
	"608\t0\tcom.apple.Finder\n"

func TestParseLaunchctl(t *testing.T) {
	items, running, total := parseLaunchctl(launchctlFixture)
	if total != 3 || running != 2 {
		t.Fatalf("应 3 项、2 个运行中，实际 total=%d running=%d", total, running)
	}
	if items[0]["running"].(bool) {
		t.Error("PID 为 - 的项不该算运行中")
	}
	if items[1]["last_exit"].(int) != -9 {
		t.Errorf("退出码解析错: %v", items[1])
	}
	if items[0]["label"].(string) != "com.apple.SafariHistoryServiceAgent" {
		t.Errorf("label 解析错: %v", items[0])
	}
}

func TestLaunchdFilterAndLimit(t *testing.T) {
	pathWithShims(t, map[string]string{
		"launchctl": printfShim(splitOutputLines(launchctlFixture)...),
	})
	b := newBench(t)
	res, _ := b.run("sys.launchd", map[string]any{"filter": "finder", "limit": 5})
	d := res.Data.(map[string]any)
	if d["matched"].(int) != 1 || d["returned"].(int) != 1 {
		t.Fatalf("关键词过滤错: %v", d)
	}
	if d["total"].(int) != 3 {
		t.Errorf("total 应为 3，实际 %v", d["total"])
	}
}

// ---------- sys.power ----------

const pmsetFixture = `System-wide power settings:
 SleepDisabled		0
Currently in use:
 standby              1
 Sleep On Power Button 1
 hibernatefile        /var/vm/sleepimage
 displaysleep         10
 lowpowermode         1
`

const pmsetBattFixture = "Now drawing from 'AC Power'\n" +
	" -InternalBattery-0 (id=1234567)\t100%; charged; 0:00 remaining present: true\n"

func TestParsePMSet(t *testing.T) {
	system, current := parsePMSet(pmsetFixture)
	if system["SleepDisabled"] != "0" {
		t.Errorf("全局设置解析错: %v", system)
	}
	if current["displaysleep"] != "10" {
		t.Errorf("当前设置解析错: %v", current)
	}
	if current["Sleep On Power Button"] != "1" {
		t.Errorf("带空格的键解析错: %v", current)
	}
	if current["hibernatefile"] != "/var/vm/sleepimage" {
		t.Errorf("路径值解析错: %v", current)
	}
}

func TestParsePMSetBatt(t *testing.T) {
	source, battery := parsePMSetBatt(pmsetBattFixture)
	if source != "AC Power" {
		t.Errorf("电源来源解析错: %q", source)
	}
	if battery == nil || battery["percent"].(int) != 100 || battery["state"] != "charged" {
		t.Fatalf("电池解析错: %+v", battery)
	}
	if battery["present"] != true || battery["remaining"] != "0:00" {
		t.Errorf("电池细节解析错: %+v", battery)
	}
}

// ---------- sys.network_quality ----------

const nqFixture = `{"base_rtt":103.61135864257812,"dl_throughput":85659408,"ul_throughput":26010358,
"responsiveness":89.78199768066406,"interface_name":"lo0","test_endpoint":"hkhkg1-edge-fx-003.aaplimg.com",
"os_version":"Version 15.6.1 (Build 24G90)","start_date":"2026-09-20 01:45:37.162",
"end_date":"2026-09-20 01:46:22.458"}`

func TestParseNetworkQuality(t *testing.T) {
	d, err := parseNetworkQuality(nqFixture)
	if err != nil {
		t.Fatal(err)
	}
	if d["download_mbps"].(float64) != 85.7 || d["upload_mbps"].(float64) != 26.0 {
		t.Errorf("Mbps 换算错: %v", d)
	}
	if d["latency_ms"].(float64) != 103.6 || d["latency_source"] != "base_rtt" {
		t.Errorf("延迟解析错: %v", d)
	}
	if d["duration_seconds"].(float64) != 45.3 {
		t.Errorf("时长解析错: %v", d["duration_seconds"])
	}
	// 旧版本只给 idle_latency 时也能读到延迟。
	d2, err := parseNetworkQuality(`{"idle_latency":12.34,"dl_throughput":1000000,"ul_throughput":500000}`)
	if err != nil {
		t.Fatal(err)
	}
	if d2["latency_ms"].(float64) != 12.3 || d2["latency_source"] != "idle_latency" {
		t.Errorf("回退延迟解析错: %v", d2)
	}
	// 空结果必须报错，不许谎报成功。
	if _, err := parseNetworkQuality(`{"dl_throughput":0,"ul_throughput":0}`); err == nil {
		t.Error("吞吐全 0 应报错")
	}
	if _, err := parseNetworkQuality("not json"); err == nil {
		t.Error("非 JSON 应报错")
	}
}

// ---------- sys.logs ----------

func TestLogPredicatePresets(t *testing.T) {
	for _, k := range logPresetOrder {
		p, ok := logPredicate(k)
		if !ok || strings.TrimSpace(p) == "" {
			t.Errorf("预设 %s 没有谓词", k)
		}
		if logPresetLabels[k] == "" {
			t.Errorf("预设 %s 没有中文标签", k)
		}
	}
	if p, _ := logPredicate("errors"); p != `messageType == error` {
		t.Errorf("错误级别谓词应固定: %q", p)
	}
	self, ok := logPredicate("self")
	if !ok || !strings.HasPrefix(self, `process == "`) {
		t.Errorf("自身进程谓词应只含进程名: %q", self)
	}
	if _, ok := logPredicate("user-supplied"); ok {
		t.Error("非预设条件必须被拒（防注入）")
	}
}

func TestLogsRejectsUnknownPreset(t *testing.T) {
	b := newBench(t)
	code, err := b.runErr("sys.logs", map[string]any{"preset": "rm -rf /", "window": "5m"})
	if err == nil || code != 400 {
		t.Fatalf("非预设条件应 400，实际 code=%d err=%v", code, err)
	}
}

func TestLogsShimParsesLinesAndArgs(t *testing.T) {
	b := newBench(t)
	capture := filepath.Join(t.TempDir(), "args.txt")
	body := "printf '%s\\n' \"$@\" > " + capture + "\n" +
		printfShim(
			"Timestamp               Ty Process[PID:TID]",
			"2026-09-20 01:46:23.562 E  kernel[0:ab] boom one",
			"2026-09-20 01:46:24.562 E  kernel[0:ab] boom two",
		)
	pathWithShims(t, map[string]string{"log": body})
	snap := b.runTask("sys.logs", map[string]any{"preset": "errors", "window": "5m", "limit": 1})
	if snap.Status != tasks.Succeeded {
		t.Fatalf("应成功: %s", snap.Error)
	}
	data := b.resultData(t, snap)
	if data["matched_lines"].(int) != 2 || data["returned"].(int) != 1 {
		t.Fatalf("行数统计错: %v", data)
	}
	lines := data["lines"].([]string)
	if len(lines) != 1 || !strings.Contains(lines[0], "boom one") {
		t.Fatalf("应返回最早的一行且去掉表头: %v", lines)
	}
	blob, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(splitOutputLines(string(blob)), " ")
	if got != "show --last 5m --predicate messageType == error --style compact" {
		t.Fatalf("log 参数错: %q", got)
	}
}

func TestLogsEmptyIsNotFailure(t *testing.T) {
	pathWithShims(t, map[string]string{
		"log": printfShim("Timestamp               Ty Process[PID:TID]"),
	})
	b := newBench(t)
	snap := b.runTask("sys.logs", map[string]any{"preset": "nginx", "window": "5m"})
	if snap.Status != tasks.Succeeded {
		t.Fatalf("空结果是正常情况，不该失败: %s", snap.Error)
	}
	res := snap.Result.(*tool.Result)
	if res.Data.(map[string]any)["matched_lines"].(int) != 0 || !strings.Contains(res.Msg, "没有匹配日志") {
		t.Fatalf("空结果要如实说明: %+v", res)
	}
}

func TestLogsFailureSurfacesStderr(t *testing.T) {
	pathWithShims(t, map[string]string{"log": "echo 'log: bad predicate' >&2\nexit 1"})
	b := newBench(t)
	snap := b.runTask("sys.logs", map[string]any{"preset": "errors", "window": "5m"})
	if snap.Status != tasks.Failed || !strings.Contains(snap.Error, "bad predicate") {
		t.Fatalf("失败要带真实 stderr，实际 %s / %q", snap.Status, snap.Error)
	}
}

// ---------- 真机只读冒烟 ----------

func TestSystemReadOnlySmoke(t *testing.T) {
	requireCmds(t, "scutil", "ps", "launchctl", "pmset", "log")
	b := newBench(t)

	res, _ := b.run("sys.dns", nil)
	if res.Data.(map[string]any)["resolver_count"].(int) == 0 {
		t.Error("真机 scutil --dns 至少该有一个 resolver")
	}

	res2, _ := b.run("sys.processes", map[string]any{"sort": "mem", "count": 5})
	rows := res2.Data.(map[string]any)["processes"].([]map[string]any)
	if len(rows) == 0 || len(rows) > 5 {
		t.Fatalf("进程条数不对: %d", len(rows))
	}

	res3, _ := b.run("sys.power", nil)
	if len(res3.Data.(map[string]any)["settings"].(map[string]string)) == 0 {
		t.Error("真机 pmset -g 该读到当前设置")
	}

	res4, _ := b.run("sys.launchd", map[string]any{"filter": "com.apple", "limit": 5})
	if res4.Data.(map[string]any)["total"].(int) == 0 {
		t.Error("真机 launchctl list 不该为空")
	}

	snap := b.runTask("sys.logs", map[string]any{"preset": "errors", "window": "5m", "limit": 3})
	if snap.Status != tasks.Succeeded {
		t.Fatalf("真机 log show 应成功: %s", snap.Error)
	}
}
