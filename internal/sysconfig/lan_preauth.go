package sysconfig

// lan_preauth.go —— 「允许免授权访问内网段」（可选、可一键回滚）。
//
// 背景（DEVELOPMENT.md 坑 #114 / #115，两台机器真机验证）：
//   - macOS 15 的「本地网络」隐私门会拦 nginx 的局域网反代（Homebrew 的 nginx
//     是 ad-hoc 签名，标识随二进制 UUID 变，无头机器没人点弹窗 → 502）。
//     默认修法是「局域网出口收回面板」，用回环转发器绕开这道门（细粒度、无需重启）。
//   - 官方另有一扇**可编程预授权**的门：com.apple.network.local-network 域的
//     AllowedEthernetLocalNetworkAddresses / AllowedWiFiLocalNetworkAddresses
//     （CIDR 字符串数组）。写进去 + 重启后，系统"把那个网段上的每个地址都当成
//     不是本地网络地址"，于是**所有程序**都能访问该网段，与各自的 Local Network
//     授权状态无关（Apple TN3179 原文）。
//
// 这是一个**降低隐私门强度**的可选项，所以这个文件的三条纪律：
//  1. 代价必须写在返回给前端的 State 里（对**所有程序**生效，不是只对 nginx）。
//  2. 改动**只有重启后生效**；判断不了时宁可说"要重启"，绝不谎报"已生效"。
//  3. 写入后必须**读回复核**；读不回来 / 对不上就返回 error（不谎报成功）。
//
// 「系统域 + 用户域都要写」是真机 A/B 的结论（坑 #115）：只写一个域不生效。
// 两条命令都严格对齐 Apple TN3179 的官方示例（`sudo defaults write
// com.apple.network.local-network … -array …`）：
//   - 系统域：面板本身就是 root，直接跑裸域名（等价于 Apple 的 sudo 那条）；
//   - 用户域：`sudo -n -u <真实用户> defaults …` 降权到真实用户
//     （与 brew/pip 的降权路径同一套 helper）。

import (
	"context"
	"fmt"
	"net"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
)

const (
	// lanPrefDomain 是 Apple 官方预授权的偏好域（坑 #115）。
	lanPrefDomain = "com.apple.network.local-network"
	// 两个键分别是"以太网"与"Wi-Fi"的免授权网段。
	lanEthernetKey = "AllowedEthernetLocalNetworkAddresses"
	lanWiFiKey     = "AllowedWiFiLocalNetworkAddresses"

	// lanSystemPlist 是**机器级**偏好文件 —— 也是这套设置真正生效的地方。
	//
	// ⚠️ 真机教训（2026-09-17 mini，本轮 A/B 实测）：作为 root 执行
	//     defaults write com.apple.network.local-network <key> -array <cidr>
	// **写的是 root 自己的用户域**（落盘到 /var/root/Library/Preferences/…），
	// **不会**碰机器级的 /Library/Preferences/…plist。而 OS 读的是机器级那一份：
	// 实测"只留下机器级文件、删掉 root 与用户域 + 重启"之后，免授权仍然生效 ——
	// 也就是说按裸域名写的版本**根本没写对地方**，撤销也撤不干净。
	// 所以读写都必须用**显式路径**（defaults 接受不带 .plist 后缀的绝对路径）。
	lanSystemPlist = "/Library/Preferences/com.apple.network.local-network.plist"
	// lanRootUserPlist 是"root 的裸域名落到自己家目录"时的候选路径。
	lanRootUserPlist = "/var/root/Library/Preferences/com.apple.network.local-network.plist"

	defaultsBin = "/usr/bin/defaults"
	sudoBin     = "/usr/bin/sudo"
	ifconfigBin = "/sbin/ifconfig"
	sysctlBin   = "/usr/sbin/sysctl"
)

// LANPreauthWarning 是必须原样告诉用户的代价（前端按 textContent 渲染，
// 所以不要写 Markdown 的 ** —— 用户会看到字面星号）。
const LANPreauthWarning = "这会在这个网段上关掉 macOS「本地网络」隐私门"

// LANPreauthState 是「允许免授权访问内网段」的当前真实状态。
type LANPreauthState struct {
	// Supported：本机能不能读写这个偏好域（找不到 defaults 就是 false）。
	Supported bool `json:"supported"`
	// Readable：状态是否真的读到了。false 时 ReadError 说明原因，绝不猜。
	Readable bool `json:"readable"`
	// Enabled：系统域与用户域**都**已写入（缺一个都算不完整）。
	Enabled bool `json:"enabled"`
	// Partial：只有一个域被写入（不完整，多半是上一次写了一半）。
	Partial bool `json:"partial"`
	// SystemSet / UserSet：两个域各自有没有读到设置。
	SystemSet bool `json:"system_set"`
	UserSet   bool `json:"user_set"`
	// SystemCIDRs / UserCIDRs：两个域各自读到的 CIDR。
	SystemCIDRs []string `json:"system_cidrs"`
	UserCIDRs   []string `json:"user_cidrs"`
	// LegacyCIDRs：旧版本按裸域名写错位置、落在 **root 用户域**（/var/root）里的残留。
	// 它不生效，但会让"读到什么"和"实际生效什么"各说各话 —— 撤销会清掉它。
	LegacyCIDRs []string `json:"legacy_cidrs,omitempty"`
	// CIDRs：两个域的并集（给界面显示"当前放行哪些网段"）。
	CIDRs []string `json:"cidrs"`
	// DetectedCIDR：从主网卡自动推导出来的默认网段（用户没选时用）。
	DetectedCIDR string `json:"detected_cidr"`
	// RebootRequired：磁盘上的设置是在本次开机之后写的 → 还没生效。
	RebootRequired bool `json:"reboot_required"`
	// RebootNote：为什么这么判断（读不到开机时间时会说明"无法判断，按需要重启算"）。
	RebootNote string `json:"reboot_note,omitempty"`
	// Warning：固定写上"对所有程序生效"的代价。
	Warning string `json:"warning"`
	// Note：当前状态的判断依据（人话）。
	Note string `json:"note,omitempty"`
	// ReadError：读不到状态时的原因。
	ReadError string `json:"read_error,omitempty"`
}

// ---------- 可注入点（单测用假实现，绝不真的跑 defaults / 写真实偏好域） ----------

var (
	// panelUserFn 返回面板应当使用的真实用户（与安装/brew 同一套判断）。
	panelUserFn = config.PanelUser
	// userHomeFn 返回真实用户的家目录（定位用户域 plist，用于重启判断）。
	userHomeFn = func(name string) string {
		if u, err := user.Lookup(name); err == nil {
			return u.HomeDir
		}
		return ""
	}
	// isRootFn 抽出来是为了单测能造出"以 root 运行"的场景。
	isRootFn = IsRoot
	// ifconfigFn / ifconfigListFn 抽出来是为了单测用真实输出做纯解析测试。
	ifconfigFn     = func(ctx context.Context, iface string) string { return runQuiet(ctx, ifconfigBin, iface) }
	ifconfigListFn = func(ctx context.Context) string { return runQuiet(ctx, ifconfigBin, "-l") }
	// bootTimeFn 返回本次开机时间；读不到返回零值。
	bootTimeFn = func(ctx context.Context) time.Time {
		return parseBootTime(runQuiet(ctx, sysctlBin, "-n", "kern.boottime"))
	}
)

// ---------- 目标域 ----------

// lanTarget 是一个要读写的偏好域。
type lanTarget struct {
	Label string
	// Domain 是传给 defaults 的域。两个目标都用 Apple 官方的裸域名
	// `com.apple.network.local-network`（见文件头注释与 lanTargets）。
	Domain string
	// SudoUser 非空 → 用 `sudo -n -u <user>` 以该用户身份执行（用户域）。
	SudoUser string
	// PlistPaths 是该域可能落盘的 plist 路径（用于判断"是否本次开机后改过"）。
	PlistPaths []string
	// CleanupOnly 表示这个目标**只在撤销/探测时参与**，应用时不写。
	// 用于清掉旧版本按裸域名写错位置留下的 /var/root 副本。
	CleanupOnly bool
}

// lanTargets 返回要同时写入的两个域（顺序固定：系统域在前）。
//
// 命令形状严格对齐 Apple TN3179 的官方示例：
//
//	sudo defaults write com.apple.network.local-network <key> -array <cidr>
//
// 面板本身以 root 运行，所以"系统域"直接跑裸域名（等价于 Apple 的 sudo 那条）；
// "用户域"用 `sudo -n -u <真实用户>` 降权，把同一份设置写进真实用户的域 ——
// 真机 A/B 的结论是两个域都要写（坑 #115）。
func lanTargets() []lanTarget {
	// 机器级：**必须用显式路径**（裸域名在 root 下会落到 /var/root，写不到这里）。
	plain := strings.TrimSuffix(lanSystemPlist, ".plist")
	targets := []lanTarget{{
		Label:      "系统域",
		Domain:     plain,
		PlistPaths: []string{lanSystemPlist},
	}}
	user := strings.TrimSpace(panelUserFn())
	ut := lanTarget{Label: "用户域", Domain: lanPrefDomain}
	// 面板以 root 运行时，用户域必须降权到真实用户写 —— 否则写进 root 的家目录，
	// 对登录用户不生效（真机 A/B 的结论）。
	if isRootFn() && user != "" && user != "root" {
		ut.SudoUser = user
	}
	if user != "" && user != "root" {
		if home := userHomeFn(user); home != "" {
			ut.PlistPaths = []string{filepath.Join(home, "Library", "Preferences", lanPrefDomain+".plist")}
		}
	}
	targets = append(targets, ut)
	// 历史遗留：旧版本按裸域名写、落到 root 用户域的那一份。它不生效，但留着会让人
	// （和面板自己）误判状态，所以撤销时一并清掉；应用时**不**再写它。
	legacy := lanTarget{Label: "root 用户域（历史遗留）", Domain: lanPrefDomain, CleanupOnly: true,
		PlistPaths: []string{lanRootUserPlist}}
	targets = append(targets, legacy)
	return targets
}

// lanCommand 拼出一次 defaults 调用的完整 argv（程序名 + 参数）。
//
// action ∈ read | write | delete。用户域会包一层
// `sudo -n -u <真实用户> /usr/bin/defaults …`（与 install.go 的 brewRun 同形）。
// **只拼参数，不经过 shell**；用户输入只作为数组元素出现。
func lanCommand(t lanTarget, action, key string, cidrs []string) (string, []string) {
	var args []string
	switch action {
	case "read":
		args = []string{"read", t.Domain, key}
	case "delete":
		args = []string{"delete", t.Domain, key}
	case "write":
		args = []string{"write", t.Domain, key, "-array"}
		args = append(args, cidrs...)
	}
	if t.SudoUser != "" {
		full := append([]string{"-n", "-u", t.SudoUser, defaultsBin}, args...)
		return sudoBin, full
	}
	return defaultsBin, args
}

// ---------- 纯解析（全部可用真机输出做单测） ----------

// ParseLANCIDRs 解析用户输入的网段列表（逗号/分号/空白/换行分隔），
// 校验每个都是合法 CIDR，并归一到网络地址（192.0.2.5/24 → 192.0.2.0/24）。
func ParseLANCIDRs(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	seen := map[string]bool{}
	out := []string{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(f)
		if err != nil {
			return nil, fmt.Errorf("网段 %q 不是合法的 CIDR（形如 192.0.2.0/24）：%v", f, err)
		}
		v := ipnet.String()
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

// ParseIPv4CIDR 从 `ifconfig <iface>` 的输出里取 IPv4 + netmask，拼成 CIDR。
//
// 真机输出形如：
//
//	inet 192.0.2.179 netmask 0xffffff00 broadcast 192.0.2.255
//
// 注意要跳过 inet6 行；netmask 可能是十六进制（Apple 默认）或点分十进制。
func ParseIPv4CIDR(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		// 只要 `inet ` 这一段；`inet6` 会因为 f[0] != "inet" 被跳过。
		if len(f) < 4 || f[0] != "inet" {
			continue
		}
		ip := net.ParseIP(f[1])
		if ip == nil || ip.To4() == nil {
			continue
		}
		var mask net.IPMask
		for i := 2; i+1 < len(f); i++ {
			if f[i] == "netmask" {
				mask = parseNetmask(f[i+1])
				break
			}
		}
		if mask == nil {
			continue
		}
		return (&net.IPNet{IP: ip.Mask(mask), Mask: mask}).String(), true
	}
	return "", false
}

// parseNetmask 解析 ifconfig 的 netmask（0xffffff00 或 255.255.255.0）。
func parseNetmask(s string) net.IPMask {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		n, err := strconv.ParseUint(s[2:], 16, 32)
		if err != nil {
			return nil
		}
		return net.IPv4Mask(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	if m := ip.To4(); m != nil {
		return net.IPv4Mask(m[0], m[1], m[2], m[3])
	}
	return nil
}

// parseDefaultsArray 解析 `defaults read <domain> <key>` 的数组输出：
//
//	(
//	    "192.0.2.0/24",
//	    "198.51.100.0/24"
//	)
//
// 只接受数组形态；其它形态返回错误（不猜、不硬塞）。
func parseDefaultsArray(out string) ([]string, error) {
	text := strings.TrimSpace(out)
	if text == "" {
		return nil, fmt.Errorf("输出为空")
	}
	if !strings.HasPrefix(text, "(") || !strings.HasSuffix(text, ")") {
		return nil, fmt.Errorf("不是数组格式：%s", firstLine(text, nil))
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "("), ")"))
	if body == "" {
		return []string{}, nil
	}
	vals := []string{}
	for _, line := range strings.Split(body, "\n") {
		v := strings.TrimSpace(line)
		v = strings.TrimSuffix(v, ",")
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if v == "" {
			continue
		}
		vals = append(vals, v)
	}
	return vals, nil
}

// parseBootTime 解析 `sysctl -n kern.boottime`（形如
// `{ sec = 1789397632, usec = 424305 } Mon Sep 14 22:53:52 2026`）。
func parseBootTime(out string) time.Time {
	i := strings.Index(out, "sec =")
	if i < 0 {
		return time.Time{}
	}
	rest := strings.TrimSpace(out[i+len("sec ="):])
	if j := strings.IndexAny(rest, ",}"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// ---------- 网段自动推导 ----------

// virtualIfacePrefixes 是不该用来推导"本机局域网段"的接口前缀
// （回环、隧道、AirDrop/AWDL、桥接等）。
var virtualIfacePrefixes = []string{"lo", "gif", "stf", "anpi", "utun", "awdl", "llw", "bridge", "ap", "vmenet", "p2p"}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// interfaceCandidates 从 `ifconfig -l` 的输出里排出探测顺序：物理 en* 在前。
func interfaceCandidates(listOut string) []string {
	var en, other []string
	for _, n := range strings.Fields(listOut) {
		if n == "" || isVirtualIface(n) {
			continue
		}
		if strings.HasPrefix(n, "en") {
			en = append(en, n)
		} else {
			other = append(other, n)
		}
	}
	sort.Strings(en)
	sort.Strings(other)
	out := append(en, other...)
	if len(out) == 0 {
		// ifconfig -l 没给出东西时也要能工作（老系统/异常输出）——
		// 这是候选顺序，不是把 192.0.2.0/24 写死。
		return []string{"en0", "en1"}
	}
	return out
}

// DetectPrimaryCIDR 从主网卡的 IPv4 + netmask 推导默认网段。
// 推不出来返回空串（界面会让用户自己填，而不是硬塞 192.0.2.0/24）。
func DetectPrimaryCIDR(ctx context.Context) string {
	for _, name := range interfaceCandidates(ifconfigListFn(ctx)) {
		if cidr, ok := ParseIPv4CIDR(ifconfigFn(ctx, name)); ok {
			return cidr
		}
	}
	return ""
}

// ---------- 执行 ----------

// defaultsSupported 判断本机有没有 defaults 命令。
func defaultsSupported() bool {
	st, err := statFn(defaultsBin)
	return err == nil && !st.IsDir()
}

// lanReadKey 读一个键。返回值 set=false 表示"键不存在"（未设置），不是错误。
func lanReadKey(ctx context.Context, t lanTarget, key string) (vals []string, set bool, err error) {
	name, args := lanCommand(t, "read", key, nil)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, runErr := runCommand(cctx, name, args...)
	if runErr != nil {
		if lanPrefMissing(out) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取%s %s 失败：%s", t.Label, key, firstLine(out, runErr))
	}
	vals, perr := parseDefaultsArray(out)
	if perr != nil {
		return nil, false, fmt.Errorf("解析%s %s 失败：%v", t.Label, key, perr)
	}
	return vals, true, nil
}

// lanPrefMissing 判断 defaults 的输出/错误是不是"本来就没有这个键/域"。
// 真机上它长这样（退出码 1，stderr 里还带一行系统日志前缀）：
//
//	The domain/default pair of (com.apple..., Allowed...) does not exist
func lanPrefMissing(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "does not exist") || strings.Contains(low, "not found")
}

// lanRun 执行一次写/删；delete 遇到"本来就不存在"视为成功（幂等）。
func lanRun(ctx context.Context, r *runner, t lanTarget, action, key string, cidrs []string) error {
	name, args := lanCommand(t, action, key, cidrs)
	text, err := r.run(ctx, name, args...)
	if err != nil {
		if action == "delete" && lanPrefMissing(text) {
			return nil
		}
		return fmt.Errorf("%s %s %s 失败：%w", t.Label, action, key, err)
	}
	return nil
}

// lanRebootPending 判断磁盘上的预授权是否"本次开机之后才改的"。
//
// 依据是相关 plist 的 mtime 是否晚于 kern.boottime —— 这是唯一能从外部
// 观察"改动有没有进入本次运行的系统"的真实信号。
// 判断不了时返回 true：宁可让用户多重启一次，也不能在没重启时谎称已生效。
func lanRebootPending(ctx context.Context, targets []lanTarget) (bool, string) {
	var newest time.Time
	found := false
	for _, t := range targets {
		for _, p := range t.PlistPaths {
			fi, err := statFn(p)
			if err != nil {
				continue
			}
			found = true
			if fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
	}
	if !found {
		// 两个 plist 都不存在 → 从来没设置过，没有"待生效的改动"。
		return false, ""
	}
	boot := bootTimeFn(ctx)
	if boot.IsZero() {
		return true, "读不到本次开机时间（kern.boottime），无法判断改动是否已生效 —— 一律以重启后为准。"
	}
	if newest.After(boot) {
		return true, "偏好文件是在本次开机之后被改写的，所以要重启后才会生效。"
	}
	return false, ""
}

// DetectLANPreauth 只读地探测当前真实状态。只读，不修改任何东西。
func DetectLANPreauth(ctx context.Context) LANPreauthState {
	st := LANPreauthState{Supported: defaultsSupported(), Warning: LANPreauthWarning}
	st.DetectedCIDR = DetectPrimaryCIDR(ctx)
	if !st.Supported {
		st.Note = "本机找不到 " + defaultsBin + "，无法读写这个偏好域。"
		return st
	}

	targets := lanTargets()
	st.Readable = true
	for _, t := range targets {
		var got []string
		for _, key := range []string{lanEthernetKey, lanWiFiKey} {
			vals, set, err := lanReadKey(ctx, t, key)
			if err != nil {
				// 读不到就如实说读不到，绝不猜一个状态给用户。
				st.Readable = false
				st.ReadError = err.Error()
				st.Note = "状态读取失败：" + err.Error()
				return st
			}
			if set {
				got = append(got, vals...)
			}
		}
		got = dedupeSorted(got)
		switch {
		case t.Label == "系统域":
			st.SystemCIDRs = got
			st.SystemSet = len(got) > 0
		case t.CleanupOnly:
			if len(got) > 0 {
				st.LegacyCIDRs = got
			}
		default:
			st.UserCIDRs = got
			st.UserSet = len(got) > 0
		}
	}
	st.Enabled = st.SystemSet && st.UserSet
	st.Partial = (st.SystemSet || st.UserSet) && !st.Enabled
	st.CIDRs = dedupeSorted(append(append([]string{}, st.SystemCIDRs...), st.UserCIDRs...))
	st.RebootRequired, st.RebootNote = lanRebootPending(ctx, targets)

	switch {
	case st.Enabled:
		st.Note = "系统域与用户域都已写入（" + strings.Join(st.CIDRs, ", ") + "）。"
	case st.Partial:
		st.Note = "只有一个域读到了设置（不完整）—— 建议重新「应用」，或点「撤销」清干净。"
	default:
		st.Note = "未设置：这个网段仍然受 macOS「本地网络」隐私门限制。"
	}
	if len(st.LegacyCIDRs) > 0 {
		// 旧版本按裸域名写过 root 用户域：它不影响生效状态，但必须告诉用户"这里有残留"，
		// 而且撤销会顺手清掉（否则"读到的状态"和"真正生效的状态"会各说各话）。
		st.Note += "（检测到 root 用户域里有旧版本写入的残留 " + strings.Join(st.LegacyCIDRs, ", ") +
			"，它不生效；点「撤销」会一并清掉）"
	}
	return st
}

// ApplyLANPreauth 写入预授权：两个键 × 两个域，写完后读回复核。
//
// 任何一步失败、或复核对不上，都返回**真实错误** —— 这个功能会削弱隐私门，
// 谎报成功比不工作更糟。
func ApplyLANPreauth(ctx context.Context, rawCIDRs string, log LogFunc) error {
	if !isRootFn() {
		return fmt.Errorf("需要以 root 运行（面板服务默认就是 root）")
	}
	cidrs, err := ParseLANCIDRs(rawCIDRs)
	if err != nil {
		return err
	}
	if len(cidrs) == 0 {
		return fmt.Errorf("至少填一个网段（CIDR），例如 192.0.2.0/24")
	}
	if !defaultsSupported() {
		return fmt.Errorf("本机找不到 %s，无法写入这个偏好域", defaultsBin)
	}
	r := &runner{log: log}
	targets := lanTargets()
	for _, t := range targets {
		if t.CleanupOnly {
			continue // 只写真正生效的两个位置；遗留位置由撤销负责清理
		}
		for _, key := range []string{lanEthernetKey, lanWiFiKey} {
			if err := lanRun(ctx, r, t, "write", key, cidrs); err != nil {
				return err
			}
		}
	}
	// 复核：重新读一遍真实值（不看退出码）。
	st := DetectLANPreauth(ctx)
	if !st.Readable {
		return fmt.Errorf("复核失败：写完读不回来（%s）", st.ReadError)
	}
	if !st.SystemSet || !st.UserSet {
		return fmt.Errorf("复核失败：系统域=%v、用户域=%v，设置没有同时落到两个域",
			st.SystemSet, st.UserSet)
	}
	for _, want := range cidrs {
		if !containsStr(st.CIDRs, want) {
			return fmt.Errorf("复核失败：写完之后读不到网段 %s（实际读到 %v）", want, st.CIDRs)
		}
	}
	if log != nil {
		log(levelOK, "已写入系统域与用户域（"+strings.Join(cidrs, ", ")+"）；重启后生效")
	}
	return nil
}

// RollbackLANPreauth 一键撤销：两个键 × 两个域都 delete，然后复核确实没了。
func RollbackLANPreauth(ctx context.Context, log LogFunc) error {
	if !isRootFn() {
		return fmt.Errorf("需要以 root 运行")
	}
	if !defaultsSupported() {
		return fmt.Errorf("本机找不到 %s，无法修改这个偏好域", defaultsBin)
	}
	r := &runner{log: log}
	targets := lanTargets()
	for _, t := range targets {
		for _, key := range []string{lanEthernetKey, lanWiFiKey} {
			if err := lanRun(ctx, r, t, "delete", key, nil); err != nil {
				return err
			}
		}
	}
	st := DetectLANPreauth(ctx)
	if !st.Readable {
		return fmt.Errorf("复核失败：删完读不回来（%s）", st.ReadError)
	}
	if st.SystemSet || st.UserSet || len(st.LegacyCIDRs) > 0 {
		return fmt.Errorf("复核失败：仍有预授权没删掉（系统域=%v、用户域=%v、遗留=%v）",
			st.SystemSet, st.UserSet, st.LegacyCIDRs)
	}
	if log != nil {
		log(levelOK, "已删除系统域与用户域的预授权；重启后该网段不再豁免（已单独登记/授权过的程序不受影响）")
	}
	return nil
}

// ---------- 小工具 ----------

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
