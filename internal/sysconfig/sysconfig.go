// Package sysconfig 把「把 macOS 配成一台服务器」这件事做成面板里可点的功能。
//
// 为什么要单独一页（用户的判断）：原来的「系统监控」页和仪表盘内容高度重复
// （同样的 CPU/内存/磁盘/网络/进程），而"服务器该有的设置"反倒只能靠
// 安装时跑一次 tools/server-mode.sh。所以把那个页面改成「系统设置」，
// 由面板直接读写 macOS 的设置，并且**每项都显示当前真实状态 + 可一键执行 + 执行后复核**。
//
// 三条来自真机的纪律（都在这个包里落实）：
//
//  1. **能力必须先探测，不能假设。** 反例：MacBook Air 的 pmset 不支持
//     `autorestart`，`pmset -a autorestart 1` 会**静默返回 0 但什么都没做** ——
//     脚本却报"已开启断电自恢复"，真跳闸那天才会发现。所以这里用
//     `pmset -g cap` 判断每个键是否被支持，不支持的项**明确标为不支持**。
//  2. **执行完要复核真实值**，不能凭命令退出码。pmset / defaults 都可能静默失败。
//  3. **每次改了什么、跑的是哪条命令**都要能被用户看见（配合任务中心）。
package sysconfig

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LogFunc 接收一行进度（与 tasks.LogFunc 同签名）。
type LogFunc func(level, text string)

const (
	levelCmd  = "cmd"
	levelOut  = "out"
	levelOK   = "ok"
	levelWarn = "warn"
	levelStep = "step"
)

// hostsPath 是 hosts 文件路径。抽成变量是为了让单测能在临时文件上验证
// "加/删阻断块"的幂等性，而不是去动真的 /etc/hosts。
var hostsPath = "/etc/hosts"

// hostsMarker 用来标记"这段是我们写的"，删除时按标记整段摘掉。
const (
	hostsBegin = "# zizpanel-block-apple-updates BEGIN（面板加的：彻底阻断 macOS 系统更新）"
	hostsEnd   = "# zizpanel-block-apple-updates END"
)

// updateHosts 是 macOS 系统更新真正会走的域名（证据见 README 的坑清单）：
//   - swscan/swdist：更新目录（catalog）与分发元数据
//   - swcdn/updates.cdn-apple.com/swdownload：安装包下载
//   - mesu：移动/其它系统目录
//   - gdmf：macOS 15 起用于"设备可装哪些版本"的查询
//   - appldnld：老式下载入口
//
// 不含 xp.apple.com：那是行为遥测，与"能不能装更新"无关，
// 没必要为了让某个计数好看去动它。
var updateHosts = []string{
	"mesu.apple.com",
	"gdmf.apple.com",
	"swscan.apple.com",
	"swdist.apple.com",
	"swcdn.apple.com",
	"swdownload.apple.com",
	"updates.cdn-apple.com",
	"appldnld.apple.com",
}

// PowerItem 是一项电源/睡眠设置。
type PowerItem struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Current   string `json:"current"`
	Desired   string `json:"desired"`
	Supported bool   `json:"supported"`
	OK        bool   `json:"ok"`
	Note      string `json:"note,omitempty"`
}

// UpdateState 是系统更新的当前状态。
type UpdateState struct {
	// Blocked 表示"面板判断已经阻断"（依据：系统级偏好 + hosts 阻断标记）。
	Blocked bool `json:"blocked"`
	// Prefs 是各项偏好当前值（字符串，便于原样展示）。
	Prefs map[string]string `json:"prefs"`
	// HostsBlocked 是 hosts 里是否已有阻断段。
	HostsBlocked bool `json:"hosts_blocked"`
	// LastRecommended 是系统里挂着的"推荐更新"（清掉前会有值，如 macOS Tahoe 26.6.2）。
	LastRecommended string `json:"last_recommended,omitempty"`
	// Note 说明判断依据或为什么判断不了。
	Note string `json:"note,omitempty"`
}

// State 是这一页要展示的全部真实状态。
type State struct {
	Power        []PowerItem `json:"power"`
	Updates      UpdateState `json:"updates"`
	CrashDialog  string      `json:"crash_dialog"`
	Silenced     bool        `json:"diagnostics_silenced"`
	Spotlight    string      `json:"spotlight"`
	SpotlightOn  bool        `json:"spotlight_on"`
	SSHListening bool        `json:"ssh_listening"`
	SSHPort      int         `json:"ssh_port"`
	AutoLogin    string      `json:"auto_login"`
	Model        string      `json:"model"`
	Portable     bool        `json:"portable"`
	Tailscale    bool        `json:"tailscale"`
	IsRoot       bool        `json:"is_root"`
	// LANPreauth 是「允许免授权访问内网段」的当前状态（见 lan_preauth.go）。
	//
	// 它在 web 层填充（handler 通过可注入的探针调用 DetectLANPreauth），
	// 这样状态探测与动作可以分别注入假实现，单测绝不碰真实偏好域。
	LANPreauth LANPreauthState `json:"lan_preauth"`
	// ProtectedDirs 是 macOS 隐私保护目录（文档/下载/桌面…）的可读性。
	//
	// 为什么要探测（用户反馈）：把 ~/Documents 挂进 File Browser 容器后
	// "文件看不到"，在 Web 终端里 cd 进去 ls 直接报 Operation not permitted。
	// 那不是面板或 Docker 的 bug，而是 macOS 的 TCC：**没有「完全磁盘访问权限」
	// 的进程读不了这些目录**。面板要主动把这件事说清楚并给出授权路径，
	// 否则用户会在 Docker/权限/挂载上白折腾很久。
	ProtectedDirs []ProtectedDir `json:"protected_dirs,omitempty"`
	// Warnings 是要如实告诉用户的"做不到/有前提"的事（例如笔记本合盖必睡）。
	Warnings []string `json:"warnings,omitempty"`
}

// ProtectedDir 是一个受 macOS 隐私保护的目录及其可读性。
type ProtectedDir struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Err      string `json:"err,omitempty"`
}

// powerDesired 是"服务器应该长什么样"。取值与 tools/server-mode.sh 保持一致
// （那个脚本在真机上跑过很久），唯有 displaysleep 允许息屏：显示器睡了不影响服务。
var powerDesired = []struct {
	Key   string
	Value string
	Label string
	Note  string
}{
	{"sleep", "0", "系统睡眠", "服务器睡眠＝服务离线，必须关"},
	{"disksleep", "0", "硬盘睡眠", "硬盘睡了之后首次访问要等唤醒"},
	{"displaysleep", "10", "显示器睡眠", "只息屏，不影响服务（省电）"},
	{"womp", "1", "网络唤醒", "远程可以叫醒这台机器"},
	{"powernap", "0", "Power Nap", "关闭后台偷偷唤醒做事"},
	{"autorestart", "1", "断电自恢复", "跳闸恢复供电后自动开机（部分机型不支持）"},
}

// ---------- 命令执行 ----------

type runner struct {
	log LogFunc
}

// runCommand 是"执行一条命令并取回合并输出"的唯一入口。
//
// 抽成包级变量有两个原因：
//  1. 本包新增的「内网段预授权」（lan_preauth.go）要写 macOS 偏好域，
//     单测必须能断言**拼出来的 argv**（含用户域的 sudo -u 真实用户），
//     而绝不能真的执行 defaults —— 那会改到真实偏好。
//  2. 执行路径集中在一处，不会出现"另一条自己拼 exec.Command 的旁路"。
//
// 生产实现就是 exec.CommandContext(...).CombinedOutput()，行为与改造前一致。
var runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// run 执行一条命令，把命令本身与输出都写进日志（用户要看到"到底跑了什么"）。
func (r *runner) run(ctx context.Context, name string, args ...string) (string, error) {
	line := name
	if len(args) > 0 {
		line += " " + strings.Join(args, " ")
	}
	if r.log != nil {
		r.log(levelCmd, "$ "+line)
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := runCommand(cctx, name, args...)
	text := strings.TrimSpace(string(out))
	if text != "" && r.log != nil {
		for _, l := range strings.Split(text, "\n") {
			r.log(levelOut, l)
		}
	}
	if err != nil {
		return text, fmt.Errorf("%s 失败：%s", line, firstLine(text, err))
	}
	return text, nil
}

func firstLine(text string, err error) string {
	if t := strings.TrimSpace(text); t != "" {
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			return strings.TrimSpace(t[:i])
		}
		return t
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// ---------- 探测 ----------

// Probe 读取当前真实状态。它只读，不做任何修改。
func Probe(ctx context.Context) State {
	st := State{IsRoot: os.Geteuid() == 0}
	st.Model = strings.TrimSpace(runQuiet(ctx, "/usr/sbin/sysctl", "-n", "hw.model"))
	st.Portable = isPortable(ctx, st.Model)
	st.Power = probePower(ctx)
	st.Updates = probeUpdates(ctx)
	st.CrashDialog, st.Silenced = probeDiagnostics(ctx)
	st.Spotlight, st.SpotlightOn = probeSpotlight(ctx)
	st.SSHPort = 22
	st.SSHListening = portListening(ctx, st.SSHPort)
	st.AutoLogin = probeAutoLogin(ctx)
	st.Tailscale = dirExists("/Applications/Tailscale.app")

	if st.Portable {
		// 这里给的是纯文本（前端按 textContent 渲染），不要写 Markdown 的 ** ——
		// 它不会被解析，用户会看到一对字面星号。
		st.Warnings = append(st.Warnings,
			"本机是笔记本（"+st.Model+"）：合盖仍会休眠。要合盖当服务器，需外接电源 + 显示器 + 键鼠。")
	}
	if !st.IsRoot {
		st.Warnings = append(st.Warnings, "面板当前不是以 root 运行：电源/更新/hosts 这些设置改不动。")
	}
	st.ProtectedDirs = probeProtectedDirs()
	for _, d := range st.ProtectedDirs {
		if !d.Readable {
			st.Warnings = append(st.Warnings,
				"面板读不到"+d.Name+"（"+d.Path+"）："+d.Err+
					"。这是 macOS 的隐私保护（TCC）—— 需要到「系统设置 → 隐私与安全性 → 完全磁盘访问权限」里"+
					"把 /opt/zizpanel/bin/zizpanel（以及运行 Docker 的 Colima / lima）加进去，"+
					"否则 Web 终端与容器里都看不到这些目录里的文件。")
			break // 同因同果，说一次就够
		}
	}
	return st
}

// probeProtectedDirs 逐个试读 macOS 的隐私保护目录。
//
// 用 os.ReadDir 而不是 os.Stat：TCC 拦的是"列目录/读内容"，
// stat 往往仍然成功（能看到目录存在、却读不了里面的东西）——
// 只 stat 会得出"一切正常"的错误结论。
func probeProtectedDirs() []ProtectedDir {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	names := [][2]string{
		{"文档", "Documents"},
		{"下载", "Downloads"},
		{"桌面", "Desktop"},
		{"音乐", "Music"},
		{"影片", "Movies"},
	}
	out := make([]ProtectedDir, 0, len(names))
	for _, n := range names {
		p := filepath.Join(home, n[1])
		d := ProtectedDir{Name: n[0], Path: p}
		if _, err := os.Stat(p); err != nil {
			continue // 目录不存在（用户没建），不算问题
		}
		if _, err := os.ReadDir(p); err != nil {
			d.Err = err.Error()
		} else {
			d.Readable = true
		}
		out = append(out, d)
	}
	return out
}

func probePower(ctx context.Context) []PowerItem {
	caps := parsePmsetCaps(runQuiet(ctx, "/usr/bin/pmset", "-g", "cap"))
	current := parsePmsetCurrent(runQuiet(ctx, "/usr/bin/pmset", "-g"))
	items := make([]PowerItem, 0, len(powerDesired))
	for _, d := range powerDesired {
		it := PowerItem{Key: d.Key, Label: d.Label, Desired: d.Value, Note: d.Note}
		it.Supported = caps[d.Key]
		if v, ok := current[d.Key]; ok {
			it.Current = v
			it.OK = v == d.Value
		} else {
			it.Current = "—"
		}
		if !it.Supported && it.Current == "—" {
			it.Note = "本机 pmset 不支持这个键（执行它只会静默无效，所以这里不给点）"
		}
		items = append(items, it)
	}
	return items
}

// parsePmsetCaps 解析 `pmset -g cap`，返回本机支持哪些键。
//
// 真机输出是**多行、每行前有一个空格**（不是空格分隔的一行）：
//
//	Capabilities for AC Power:
//	 displaysleep
//	 disksleep
//	 sleep
//	 womp
//	 autorestart
//	 ...
//
// 第一版写成 `strings.Contains(" "+out+" ", " "+key+" ")`，于是除了首尾那两个键
// 之外**全都匹配不上** —— 界面上会把每一项都标成"本机不支持"，
// 用户因此永远改不了电源设置。这是真机（Mac mini）上验证时抓到的：
// `pmset -g cap` 明明列着 autorestart，页面却说"本机不支持"。
// 所以这里按字段切分，而不是拿整段做子串匹配。
func parsePmsetCaps(out string) map[string]bool {
	caps := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ":") {
			continue // 跳过空行与 "Capabilities for AC Power:" 这类表头
		}
		// 正常情况下每行就是一个键；用 Fields 兜底，防止未来格式变化。
		for _, f := range strings.Fields(line) {
			caps[f] = true
		}
	}
	return caps
}

// parsePmsetCurrent 解析 `pmset -g` 的 "Currently in use:" 段。
// 值后面可能跟括号注释（如 `sleep 1 (sleep prevented by ...)`），只取第一个字段。
func parsePmsetCurrent(out string) map[string]string {
	res := map[string]string{}
	inUse := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Currently in use") {
			inUse = true
			continue
		}
		if !inUse || trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		// 只收我们关心的那些键，避免把 hibernatefile 之类塞进来
		for _, d := range powerDesired {
			if d.Key == key {
				res[key] = fields[1]
			}
		}
	}
	return res
}

func probeUpdates(ctx context.Context) UpdateState {
	st := UpdateState{Prefs: map[string]string{}}
	keys := []string{
		"AutomaticCheckEnabled", "AutomaticDownload", "AutomaticallyInstallMacOSUpdates",
		"CriticalUpdateInstall", "ConfigDataInstall", "AutomaticRestartAfterUpdate",
	}
	for _, k := range keys {
		v := strings.TrimSpace(runQuiet(ctx, "/usr/bin/defaults", "read",
			"/Library/Preferences/com.apple.SoftwareUpdate.plist", k))
		if v == "" {
			v = "（未设置）"
		}
		st.Prefs[k] = v
	}
	commerce := strings.TrimSpace(runQuiet(ctx, "/usr/bin/defaults", "read",
		"/Library/Preferences/com.apple.commerce.plist", "AutoUpdate"))
	st.Prefs["AppStoreAutoUpdate"] = orDefault(commerce, "（未设置）")

	// 判断依据：几个"会真的动手装"的键必须为 0；自动检查为 0 更彻底，
	// 但 macOS 有时会把这个键删掉（守护进程重写 plist）——那时靠 hosts 阻断兜底，
	// 所以这里只要"安装类"的键为 0 且 hosts 有阻断段，就认为已经阻断。
	installOff := st.Prefs["AutomaticDownload"] == "0" &&
		st.Prefs["AutomaticallyInstallMacOSUpdates"] == "0" &&
		st.Prefs["CriticalUpdateInstall"] == "0" &&
		st.Prefs["ConfigDataInstall"] == "0"
	st.HostsBlocked = hostsHasBlock()
	st.Blocked = installOff && st.HostsBlocked

	if rec := probeRecommended(ctx); rec != "" {
		st.LastRecommended = rec
	}
	switch {
	case st.Blocked && st.LastRecommended == "":
		st.Note = "已阻断：安装类偏好全为 0，且 hosts 里挡着更新目录域名。"
	case st.Blocked:
		st.Note = "已阻断（但系统里还挂着一条推荐更新：" + st.LastRecommended + "，建议再执行一次阻断开关以清掉它）。"
	case !st.HostsBlocked:
		st.Note = "未阻断：hosts 里没有阻断段（面板会把更新目录域名解析到 0.0.0.0）。"
	default:
		st.Note = "未阻断：还有安装类偏好为开。"
	}
	return st
}

// probeRecommended 读取系统里挂着的"推荐更新"（清掉前会有值）。
func probeRecommended(ctx context.Context) string {
	out := runQuiet(ctx, "/usr/bin/defaults", "read",
		"/Library/Preferences/com.apple.SoftwareUpdate.plist", "RecommendedUpdates")
	if out == "" || !strings.Contains(out, "Display Name") {
		return ""
	}
	// 形如： "Display Name" = "macOS Tahoe 26.6.2";
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Display Name") {
			if i := strings.Index(line, "="); i > 0 {
				return strings.Trim(strings.TrimSpace(line[i+1:]), `";`)
			}
		}
	}
	return "有推荐更新"
}

func probeDiagnostics(ctx context.Context) (string, bool) {
	v := strings.TrimSpace(runQuiet(ctx, "/usr/bin/defaults", "read",
		"/Library/Preferences/com.apple.CrashReporter", "DialogType"))
	autosubmit := strings.TrimSpace(runQuiet(ctx, "/usr/bin/defaults", "read",
		"/Library/Application Support/CrashReporter/DiagnosticMessagesHistory.plist", "AutoSubmit"))
	return orDefault(v, "（未设置）"), v == "none" && autosubmit == "0"
}

func probeSpotlight(ctx context.Context) (string, bool) {
	out := strings.TrimSpace(runQuiet(ctx, "/usr/bin/mdutil", "-s", "/"))
	if out == "" {
		return "未知（mdutil 没返回）", false
	}
	// 形如 "/:\n\tIndexing enabled." —— 直接把整段给界面看太丑也难懂，翻成人话。
	switch {
	case strings.Contains(out, "Indexing enabled"):
		return "已开启（索引中）", true
	case strings.Contains(out, "Indexing disabled"):
		return "已关闭", false
	default:
		return firstLine(out, nil), false
	}
}

func probeAutoLogin(ctx context.Context) string {
	v := strings.TrimSpace(runQuiet(ctx, "/usr/bin/defaults", "read",
		"/Library/Preferences/com.apple.loginwindow", "autoLoginUser"))
	if v == "" {
		return ""
	}
	return v
}

func isPortable(ctx context.Context, model string) bool {
	// 有电池就是笔记本：这是最可靠的判据（MacBook* 同理，但名字不能当唯一依据）
	out := runQuiet(ctx, "/usr/bin/pmset", "-g", "batt")
	if strings.Contains(out, "InternalBattery") {
		return true
	}
	return strings.HasPrefix(model, "MacBook")
}

// lsofListenArgs 拼出"看某个端口有没有人在听"的 lsof 参数。
//
// 必须是**三个独立参数**。第一版写的是
// `fmt.Sprintf("-nP -iTCP:%d -sTCP:LISTEN", port)` —— 整个字符串作为**一个**
// argv 传给 lsof，lsof 把它当成一个不认识的选项，随即报错退出、没有输出，
// 于是 `portListening` 永远返回 false。真机上的表现极其误导：
// mini 的 SSH 明明开着（`launchctl` 的 launchd 正在 `*:22` 上听，用户就正连着），
// 「系统设置」页却写着"未监听（22 端口没人听）"。抽成函数是为了让单测锁死参数形状。
func lsofListenArgs(port int) []string {
	return []string{"-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN"}
}

func portListening(ctx context.Context, port int) bool {
	out := runQuiet(ctx, "/usr/sbin/lsof", lsofListenArgs(port)...)
	return strings.TrimSpace(out) != ""
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// runQuiet 执行命令只要输出（不给用户看），用于探测。
func runQuiet(ctx context.Context, name string, args ...string) string {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cctx, name, args...).Output()
	return string(out)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ---------- hosts 阻断 ----------

func hostsHasBlock() bool {
	b, err := os.ReadFile(hostsPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), hostsBegin)
}

// applyHostsBlock 幂等地把阻断段写进 hosts；已经有就跳过。
func applyHostsBlock(ctx context.Context, r *runner, extraLines ...string) error {
	if hostsHasBlock() {
		if r.log != nil {
			r.log(levelStep, "hosts 里已有阻断段，跳过")
		}
		return nil
	}
	var sb strings.Builder
	sb.WriteString("\n" + hostsBegin + "\n")
	for _, h := range updateHosts {
		sb.WriteString("0.0.0.0 " + h + "\n")
	}
	for _, l := range extraLines {
		if strings.TrimSpace(l) != "" {
			sb.WriteString(l + "\n")
		}
	}
	sb.WriteString(hostsEnd + "\n")
	// 用追加写 + 立刻读回校验（不靠退出码）
	f, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("写入 hosts 失败：%w", err)
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入 hosts 失败：%w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("写入 hosts 失败：%w", err)
	}
	if !hostsHasBlock() {
		return fmt.Errorf("hosts 写入后复核失败（没读到阻断段）")
	}
	if r.log != nil {
		r.log(levelOK, fmt.Sprintf("已把 %d 个更新域名解析到 0.0.0.0", len(updateHosts)))
	}
	// 刷新 DNS 缓存，让阻断立刻生效
	_, _ = r.run(ctx, "/usr/bin/dscacheutil", "-flushcache")
	return nil
}

// removeHostsBlock 按标记整段摘掉（恢复更新时用）。
func removeHostsBlock(ctx context.Context, r *runner) error {
	b, err := os.ReadFile(hostsPath)
	if err != nil {
		return fmt.Errorf("读取 hosts 失败：%w", err)
	}
	text := string(b)
	start := strings.Index(text, hostsBegin)
	if start < 0 {
		if r.log != nil {
			r.log(levelStep, "hosts 里没有阻断段，跳过")
		}
		return nil
	}
	end := strings.Index(text[start:], hostsEnd)
	if end < 0 {
		return fmt.Errorf("hosts 阻断段不完整（找不到结束标记），没有改动")
	}
	end = start + end + len(hostsEnd)
	// 连同前面的空行一起去掉
	trimmed := strings.TrimRight(text[:start], "\n")
	rest := strings.TrimLeft(text[end:], "\n")
	out := trimmed + "\n"
	if rest != "" {
		out += rest
	}
	if err := os.WriteFile(hostsPath, []byte(out), 0o644); err != nil {
		return fmt.Errorf("写回 hosts 失败：%w", err)
	}
	if hostsHasBlock() {
		return fmt.Errorf("hosts 移除后复核失败（阻断段还在）")
	}
	if r.log != nil {
		r.log(levelOK, "已移除 hosts 阻断段")
	}
	_, _ = r.run(ctx, "/usr/bin/dscacheutil", "-flushcache")
	return nil
}

// ---------- 动作 ----------

// ApplyServerPower 关闭睡眠等（服务器必备）。不支持的能力会被明确跳过并如实记录。
func ApplyServerPower(ctx context.Context, log LogFunc) error {
	r := &runner{log: log}
	if !IsRoot() {
		return fmt.Errorf("需要以 root 运行（面板服务默认就是 root）")
	}
	items := probePower(ctx)
	applied, skipped := 0, 0
	for _, it := range items {
		if !it.Supported {
			if log != nil {
				log(levelWarn, fmt.Sprintf("跳过「%s」：本机 pmset 不支持 %s（执行它只会静默无效）",
					it.Label, it.Key))
			}
			skipped++
			continue
		}
		if _, err := r.run(ctx, "/usr/bin/pmset", "-a", it.Key, it.Desired); err != nil {
			return err
		}
		applied++
	}
	// 复核：重新读一遍，把实际值报出来（命令退出码不算证据）
	after := parsePmsetCurrent(runQuiet(ctx, "/usr/bin/pmset", "-g"))
	if log != nil {
		log(levelStep, "复核实际生效值：")
	}
	for _, it := range items {
		v := after[it.Key]
		if v == "" {
			continue
		}
		if v == it.Desired {
			if log != nil {
				log(levelOK, fmt.Sprintf("  %s = %s ✓", it.Key, v))
			}
		} else if log != nil {
			log(levelWarn, fmt.Sprintf("  %s = %s（期望 %s，本机没接受这个值）", it.Key, v, it.Desired))
		}
	}
	if log != nil {
		log(levelOK, fmt.Sprintf("电源设置完成：生效 %d 项，跳过 %d 项（不支持）", applied, skipped))
	}
	return nil
}

// BlockUpdates 彻底阻断系统更新。
//
// 这是把用户机器上手工验证过的那套固化成按钮：偏好全关 + 清掉挂着的推荐更新 +
// hosts 阻断目录域名 + 抑制"有更新"的提示记账。**不靠 launchctl disable** ——
// 实测那两个系统守护进程受 SIP 保护，disable 标记重启后会被清掉、进程照跑，
// 真正有效的是 hosts 阻断（`softwareupdate --list` 会直接报"错误的URL"）。
func BlockUpdates(ctx context.Context, log LogFunc) error {
	r := &runner{log: log}
	if !IsRoot() {
		return fmt.Errorf("需要以 root 运行")
	}
	if log != nil {
		log(levelStep, "1/5 关闭自动检查/下载/安装（含安全数据，这是\"彻底\"的代价）")
	}
	pref := "/Library/Preferences/com.apple.SoftwareUpdate.plist"
	for _, kv := range [][2]string{
		{"AutomaticCheckEnabled", "false"},
		{"AutomaticDownload", "false"},
		{"AutomaticallyInstallMacOSUpdates", "false"},
		{"CriticalUpdateInstall", "false"},
		{"ConfigDataInstall", "false"},
		{"AutomaticRestartAfterUpdate", "false"},
	} {
		if _, err := r.run(ctx, "/usr/bin/defaults", "write", pref, kv[0], "-bool", kv[1]); err != nil {
			return err
		}
	}
	if _, err := r.run(ctx, "/usr/bin/defaults", "write",
		"/Library/Preferences/com.apple.commerce.plist", "AutoUpdate", "-bool", "false"); err != nil {
		return err
	}

	if log != nil {
		log(levelStep, "2/5 清掉系统里挂着的推荐更新（否则偏好里一直显示\"待更新\"）")
	}
	for _, k := range []string{"RecommendedUpdates", "LastRecommendedMajorOSBundleIdentifier", "FirstOfferDateDictionary"} {
		// 不存在时 defaults delete 会报错，这里不算失败
		_, _ = r.run(ctx, "/usr/bin/defaults", "delete", pref, k)
	}
	_, _ = r.run(ctx, "/usr/bin/defaults", "write", pref, "LastRecommendedUpdatesAvailable", "-int", "0")

	if log != nil {
		log(levelStep, "3/5 抑制\"有更新\"的提示（用户级记账清零 + 通知日期推远）")
	}
	for _, kv := range [][2]string{
		{"AvailableUpdatesNotificationCountKey", "0"},
		{"AvailableUpdatesNotificationProductKey", ""},
		{"MajorOSUserNotificationDate", "2035-01-01 00:00:00 +0000"},
		{"UserNotificationDate", "2035-01-01 00:00:00 +0000"},
	} {
		args := []string{"write", "com.apple.SoftwareUpdate", kv[0]}
		if kv[0] == "AvailableUpdatesNotificationCountKey" {
			args = append(args, "-int", kv[1])
		} else if strings.Contains(kv[0], "Date") {
			args = append(args, "-date", kv[1])
		} else {
			args = append(args, "-string", kv[1])
		}
		_, _ = r.run(ctx, "/usr/bin/defaults", args...)
	}

	if log != nil {
		log(levelStep, "4/5 把更新目录域名指向 0.0.0.0（真正兜底的一层）")
	}
	// 把改动前的偏好原值记进 hosts 阻断块里。
	//
	// 为什么要记：回滚时如果一律写成 true，就会**改掉用户原本的选择** ——
	// 真机上"恢复更新"把 6 个偏好全写成了 true，而这些值在阻断之前未必都是 true。
	// 记在 hosts 块里（而不是另建一个状态文件）有两个好处：
	// 删块时它自动一起消失，不会留下孤儿状态；人肉排查时也能一眼看到原值。
	if err := applyHostsBlock(ctx, r, prevPrefsComment(ctx)); err != nil {
		return err
	}

	if log != nil {
		log(levelStep, "5/5 复核")
	}
	st := probeUpdates(ctx)
	if !st.HostsBlocked {
		return fmt.Errorf("复核失败：hosts 阻断没生效")
	}
	if log != nil {
		for k, v := range sortedPrefs(st.Prefs) {
			log(levelOut, fmt.Sprintf("  %s = %s", k, v))
		}
		log(levelOK, "系统更新已阻断（更新目录域名不可达；`softwareupdate --list` 会报\"错误的URL\"）")
	}
	return nil
}

// RestoreUpdates 恢复系统更新（回滚）。
func RestoreUpdates(ctx context.Context, log LogFunc) error {
	r := &runner{log: log}
	if !IsRoot() {
		return fmt.Errorf("需要以 root 运行")
	}
	pref := "/Library/Preferences/com.apple.SoftwareUpdate.plist"
	// 优先按阻断时记下的**原值**恢复；没有记录（老版本阻断的）才退回"按系统默认打开"。
	prev := readPrevPrefs()
	if log != nil {
		if len(prev) > 0 {
			log(levelStep, fmt.Sprintf("按阻断前记录的原值恢复 %d 个开关", len(prev)))
		} else {
			log(levelStep, "没有找到阻断前的原值记录，按系统默认打开这 5 个开关")
		}
	}
	if len(prev) == 0 {
		prev = map[string]string{
			"AutomaticCheckEnabled":            "1",
			"AutomaticDownload":                "1",
			"AutomaticallyInstallMacOSUpdates": "1",
			"CriticalUpdateInstall":            "1",
			"ConfigDataInstall":                "1",
		}
	}
	for k, v := range sortedPrefs(prev) {
		want := "false"
		if v == "1" || strings.EqualFold(v, "true") {
			want = "true"
		}
		if _, err := r.run(ctx, "/usr/bin/defaults", "write", pref, k, "-bool", want); err != nil {
			return err
		}
	}
	if err := removeHostsBlock(ctx, r); err != nil {
		return err
	}
	if log != nil {
		log(levelOK, "已恢复系统更新（hosts 阻断段已摘除）")
	}
	return nil
}

// SilenceDiagnostics 关掉崩溃报告弹窗与诊断上报（"提示都不要提示"的一部分）。
func SilenceDiagnostics(ctx context.Context, log LogFunc) error {
	r := &runner{log: log}
	if !IsRoot() {
		return fmt.Errorf("需要以 root 运行")
	}
	if _, err := r.run(ctx, "/usr/bin/defaults", "write",
		"/Library/Preferences/com.apple.CrashReporter", "DialogType", "-string", "none"); err != nil {
		return err
	}
	plist := "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory.plist"
	_, _ = r.run(ctx, "/usr/bin/defaults", "write", plist, "AutoSubmit", "-bool", "false")
	_, _ = r.run(ctx, "/usr/bin/defaults", "write", plist, "SeedExpirationDays", "-int", "0")
	// 复核
	got := strings.TrimSpace(runQuiet(ctx, "/usr/bin/defaults", "read",
		"/Library/Preferences/com.apple.CrashReporter", "DialogType"))
	if got != "none" {
		return fmt.Errorf("复核失败：DialogType 仍是 %q", got)
	}
	if log != nil {
		log(levelOK, "崩溃报告弹窗已关闭（DialogType=none）")
	}
	return nil
}

// SetSpotlight 打开/关闭 Spotlight 索引。服务器上关掉可以少一堆后台磁盘活动。
func SetSpotlight(ctx context.Context, enabled bool, log LogFunc) error {
	r := &runner{log: log}
	if !IsRoot() {
		return fmt.Errorf("需要以 root 运行")
	}
	flag := "off"
	if enabled {
		flag = "on"
	}
	if _, err := r.run(ctx, "/usr/bin/mdutil", "-a", "-i", flag); err != nil {
		return err
	}
	s, on := probeSpotlight(ctx)
	if on != enabled {
		return fmt.Errorf("复核失败：Spotlight 当前状态是 %q", s)
	}
	if log != nil {
		log(levelOK, "Spotlight 索引已"+(map[bool]string{true: "开启", false: "关闭"}[enabled])+"（"+s+"）")
	}
	return nil
}

// EnableSSH 打开远程登录。
//
// 走 launchctl enable + bootstrap（不需要 GUI、不需要完全磁盘访问），
// 失败再退回 systemsetup；最后**以 22 端口是否在监听**为准 —— 这两个命令
// 都出现过"报告成功但其实没开"的情况。
func EnableSSH(ctx context.Context, log LogFunc) error {
	r := &runner{log: log}
	if !IsRoot() {
		return fmt.Errorf("需要以 root 运行")
	}
	if portListening(ctx, 22) {
		if log != nil {
			log(levelStep, "SSH 已经在监听 22 端口，跳过")
		}
		return nil
	}
	launchctlOK := true
	if _, err := r.run(ctx, "/bin/launchctl", "enable", "system/com.openssh.sshd"); err != nil {
		launchctlOK = false
	}
	if _, err := r.run(ctx, "/bin/launchctl", "bootstrap", "system",
		"/System/Library/LaunchDaemons/ssh.plist"); err != nil {
		launchctlOK = false
	}
	// 等端口真的起来
	for i := 0; i < 20; i++ {
		if portListening(ctx, 22) {
			if log != nil {
				log(levelOK, "远程登录（SSH）已开启，22 端口在监听")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if log != nil {
		log(levelWarn, "launchd 路径没能让 22 端口起来，改试 systemsetup")
	}
	_, _ = r.run(ctx, "/usr/sbin/systemsetup", "-setremotelogin", "on")
	for i := 0; i < 20; i++ {
		if portListening(ctx, 22) {
			if log != nil {
				log(levelOK, "远程登录（SSH）已开启（走 systemsetup 路径）")
			}
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = launchctlOK
	return fmt.Errorf("两条路径都没能让 SSH 监听 22 端口：请在「系统设置 → 通用 → 共享」手工打开远程登录")
}

// DisableAutoLogin 关掉自动登录（无人值守服务器的推荐做法）。
//
// 说明：面板自身以 LaunchDaemon 运行，不需要有人登录；但**用户级**服务
// （例如 brew services 装在 ~/Library/LaunchAgents 的东西）只在登录后才起。
// 所以这是个取舍，页面上要写清楚，不能偷偷替用户决定。
func DisableAutoLogin(ctx context.Context, log LogFunc) error {
	r := &runner{log: log}
	if !IsRoot() {
		return fmt.Errorf("需要以 root 运行")
	}
	_, _ = r.run(ctx, "/usr/bin/defaults", "delete",
		"/Library/Preferences/com.apple.loginwindow", "autoLoginUser")
	if _, err := os.Stat("/etc/kcpassword"); err == nil {
		if err := os.Remove("/etc/kcpassword"); err != nil {
			return fmt.Errorf("删除 /etc/kcpassword 失败：%w", err)
		}
		if log != nil {
			log(levelOK, "已删除 /etc/kcpassword")
		}
	}
	if got := probeAutoLogin(ctx); got != "" {
		return fmt.Errorf("复核失败：autoLoginUser 仍是 %q", got)
	}
	if log != nil {
		log(levelOK, "自动登录已关闭（注意：用户级 LaunchAgent 将只在有人登录后才启动）")
	}
	return nil
}

// ApplyServerMode 一键把整台机器配成服务器：电源 + 更新阻断 + 静默诊断 + 关索引 + 开 SSH。
func ApplyServerMode(ctx context.Context, log LogFunc) error {
	steps := []struct {
		name string
		fn   func(context.Context, LogFunc) error
	}{
		{"关闭睡眠与节能策略", ApplyServerPower},
		{"彻底阻止系统更新", BlockUpdates},
		{"关闭崩溃报告弹窗", SilenceDiagnostics},
		{"关闭 Spotlight 索引", func(c context.Context, l LogFunc) error { return SetSpotlight(c, false, l) }},
		{"开启远程登录（SSH）", EnableSSH},
	}
	for i, s := range steps {
		if log != nil {
			log(levelStep, fmt.Sprintf("【%d/%d】%s", i+1, len(steps), s.name))
		}
		if err := s.fn(ctx, log); err != nil {
			return fmt.Errorf("%s：%w", s.name, err)
		}
	}
	if log != nil {
		log(levelOK, "服务器模式已应用（合盖休眠、断电自恢复等硬件限制见页面说明）")
	}
	return nil
}

// IsRoot 判断当前进程是否 root。
func IsRoot() bool { return os.Geteuid() == 0 }

// sortedPrefs 让偏好输出顺序稳定（便于比对与测试）。
func sortedPrefs(m map[string]string) map[string]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}

// softwareupdateBin 找到 softwareupdate 的绝对路径。
//
// 真机上它**不在 /usr/bin**，而在 **/usr/sbin**（两台 Mac 都实测确认）。
// 早期这里写死了 /usr/bin/softwareupdate，于是"验证阻断"这个动作在任何机器上
// 都必然失败，而且报的是一句极具误导性的话："仍能查到更新（阻断可能没生效）"
// —— 实际原因只是命令路径写错、根本没跑起来。
//
// 按候选路径逐个试，最后退回 PATH 查找；都找不到就返回空串，
// 由调用方给出**准确**的错误（"找不到 softwareupdate 命令"而不是"阻断没生效"）。
func softwareupdateBin() string {
	for _, p := range []string{"/usr/sbin/softwareupdate", "/usr/bin/softwareupdate"} {
		if st, err := statFn(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := lookPathFn("softwareupdate"); err == nil {
		return p
	}
	return ""
}

// statFn / lookPathFn 抽成变量只为可测：单测要把"这台机器上没有这个命令"
// 这种情况造出来，而真机上它一直存在（路径写错才会"找不到"）。
var (
	statFn     = os.Stat
	lookPathFn = exec.LookPath
)

// VerifyUpdatesBlocked 真的去问一次系统"有没有更新"（慢，约 10-30 秒），
// 用来向用户证明阻断有效，而不是只看偏好值。
func VerifyUpdatesBlocked(ctx context.Context, log LogFunc) error {
	bin := softwareupdateBin()
	if bin == "" {
		return fmt.Errorf("找不到 softwareupdate 命令（试过 /usr/sbin、/usr/bin 与 PATH）—— " +
			"这台机器无法用「问系统」的方式来验证，请改用偏好与 hosts 状态判断")
	}
	r := &runner{log: log}
	cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	out, err := r.run(cctx, bin, "--list")
	if verdict := judgeSoftwareupdate(out, err); verdict != nil {
		return verdict
	}
	if log != nil {
		log(levelOK, "已验证：更新目录不可达 → 系统查不到任何更新")
	}
	return nil
}

// judgeSoftwareupdate 是"验证阻断"的判定逻辑（纯函数，便于用真机输出做单测）。
//
// 为什么单独抽出来：这条判定的三种结论（已阻断 / 没阻断 / 无法判断）
// 直接决定用户下一步怎么做，判错一次就会把人带去修一个不存在的问题。
//
// 踩过的坑（2026-09-14 真机）：`softwareupdate --list` 在 hosts 阻断生效时
// 会打印 "错误的URL"，但**退出码仍是 0**。早期判定把"看到错误文本"绑在
// `err != nil` 上，于是"阻断明明有效"被判成了"仍能查到更新（阻断可能没生效）"
// —— 比不报更糟：用户会去反复重做阻断。
//
// 所以判定顺序改成：
//  1. 输出里有更新目录不可达的特征 → 已阻断（**不看退出码**）；
//  2. 输出里有 "no new software" → 没有可用更新（也是"阻断有效"的一种表现）；
//  3. 命令本身失败（但输出不像上面两种）→ 如实说"无法据此判断"，而不是咬定没阻断；
//  4. 其余（真的列出了更新）→ 没阻断。
func judgeSoftwareupdate(out string, runErr error) error {
	lower := strings.ToLower(out)
	// 只认"更新目录不可达"这类明确特征，不用宽泛的 "错误"/"could not"
	// —— 否则网络断了、证书坏了都会被说成"阻断有效"。
	unreachable := []string{
		"错误的url", "wrong url", "could not connect", "can't connect",
		"无法连接", "could not resolve", "swscan", "swdist",
	}
	for _, needle := range unreachable {
		if strings.Contains(lower, needle) {
			return nil
		}
	}
	if strings.Contains(lower, "no new software") {
		return nil
	}
	if runErr != nil {
		return fmt.Errorf("softwareupdate 执行失败，无法据此判断阻断是否生效（这不是「阻断失效」）：%s",
			firstLine(out, runErr))
	}
	// 真的列出了更新：把第一条给用户看，便于判断
	return fmt.Errorf("仍能查到更新（阻断可能没生效）：%s", firstLine(out, nil))
}

// ---------- 阻断前的偏好原值（回滚保真） ----------

// prevPrefsTag 是写在 hosts 阻断块里的一行注释：记录改动前的偏好原值。
//
// 用注释而不是单独的 JSON 文件：删块时它自动一起消失，不会留下孤儿状态，
// 人肉排查 nginx/hosts 时也能直接看到"阻断前是什么值"。
const prevPrefsTag = "# zizpanel-prev-prefs:"

// trackedPrefs 是阻断会改动的偏好（回滚要按原值恢复的就是这几个）。
var trackedPrefs = []string{
	"AutomaticCheckEnabled", "AutomaticDownload", "AutomaticallyInstallMacOSUpdates",
	"CriticalUpdateInstall", "ConfigDataInstall",
}

// prevPrefsComment 读当前值并拼成一行注释；读不到就返回空（不写这行）。
func prevPrefsComment(ctx context.Context) string {
	m := map[string]string{}
	for _, k := range trackedPrefs {
		v := strings.TrimSpace(runQuiet(ctx, "/usr/bin/defaults", "read",
			"/Library/Preferences/com.apple.SoftwareUpdate.plist", k))
		if v == "" {
			v = "unset" // 键不存在：回滚时不该凭空写一个值进去
		}
		m[k] = v
	}
	return formatPrevPrefs(m)
}

// formatPrevPrefs 把原值拼成一行：`# zizpanel-prev-prefs: A=1 B=0`
func formatPrevPrefs(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	// 注意：sortedPrefs 返回的是 map，range 出来的是 (key, value) ——
	// 早期这里写成了 `for _, k := range keys`，于是 k 拿到的是**值**，
	// 拼出来的是 "=0" 这种没有键名的垃圾（单测当场抓到）。
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+m[k])
	}
	return prevPrefsTag + " " + strings.Join(parts, " ")
}

// parsePrevPrefs 从 hosts 内容里解析出原值（没有记录就返回空 map）。
func parsePrevPrefs(hosts string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(hosts, "\n") {
		i := strings.Index(line, prevPrefsTag)
		if i < 0 {
			continue
		}
		for _, kv := range strings.Fields(strings.TrimSpace(line[i+len(prevPrefsTag):])) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				continue
			}
			out[k] = v
		}
	}
	return out
}

// readPrevPrefs 读当前 hosts 里的原值记录。
func readPrevPrefs() map[string]string {
	b, err := os.ReadFile(hostsPath)
	if err != nil {
		return nil
	}
	return parsePrevPrefs(string(b))
}
