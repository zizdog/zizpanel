// Package sharing —— 「文件共享（本机对外提供）」：SMB 共享 + NFS 导出。
//
// 这是「网络磁盘（挂载别人的共享）」的反向能力。所有写操作（launchctl / sharing /
// nfsd / dseditgroup）都**必须回读**：退出码 0 但回读对不上 ⇒ 如实报失败。
//
// 纪律：
//  1. 命令一律 exec.Command + 数组参数，绝不 shell 拼接；共享名/路径由调用方严格校验。
//  2. 读不到探针结论就标「未复核」（*Known=false），绝不猜"已开启/已关闭"。
//  3. /etc/exports 读改写：先备份、保留用户原有行、只增删本面板那几行、
//     写临时文件 + rename 原子替换、解析失败绝不动原文件。
package sharing

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/sysinfo"
)

// SMBDPlist 是 SMB 守护进程的 launchd plist（写死但在 Executor 里可参数化）。
const SMBDPlist = "/System/Library/LaunchDaemons/com.apple.smbd.plist"

// DefaultExportsPath 是 NFS 导出表（门禁/隔离实例注入临时文件）。
const DefaultExportsPath = "/etc/exports"

// SMBLabel 是 launchd 里的 SMB 服务标签。
const SMBLabel = "com.apple.smbd"

// AccessSMBGroup 是 macOS 控制 SMB 访问的用户组（成员才允许连 SMB）。
const AccessSMBGroup = "com.apple.access_smb"

// 可注入/可垫片的命令解析：全部先走 PATH（门禁与隔离实例用假命令垫片），
// 找不到再退回系统绝对路径（LaunchDaemon 的 PATH 可能很干净）。
var (
	lookPathFn = exec.LookPath
	fallbacks  = map[string]string{
		"launchctl":   "/bin/launchctl",
		"sharing":     "/usr/sbin/sharing",
		"nfsd":        "/sbin/nfsd",
		"pgrep":       "/usr/bin/pgrep",
		"dseditgroup": "/usr/sbin/dseditgroup",
		"ipconfig":    "/usr/sbin/ipconfig",
	}
)

// LookBin 解析一个系统命令（PATH 优先，绝对路径兜底）。导出让门禁断言用。
func LookBin(name string) string {
	if p, err := lookPathFn(name); err == nil && p != "" {
		return p
	}
	if p, ok := fallbacks[name]; ok {
		return p
	}
	return name
}

// Executor 执行共享相关的命令。零值可用。
type Executor struct {
	// Timeout 是单条命令超时（默认 20s）。
	Timeout time.Duration
	// ExportsPath 是 NFS 导出表路径（默认 /etc/exports）。
	ExportsPath string
	// SmbdPlist 是 SMB launchd plist（默认 SMBDPlist）。
	SmbdPlist string
	// User 是 NFS -mapall= 映射到的真实用户（空 = 当前用户）。
	User string
}

func (e *Executor) timeout() time.Duration {
	if e.Timeout <= 0 {
		return 20 * time.Second
	}
	return e.Timeout
}

func (e *Executor) exportsPath() string {
	if strings.TrimSpace(e.ExportsPath) != "" {
		return e.ExportsPath
	}
	return DefaultExportsPath
}

func (e *Executor) smbdPlist() string {
	if strings.TrimSpace(e.SmbdPlist) != "" {
		return e.SmbdPlist
	}
	return SMBDPlist
}

// cmdOut 是一次子进程的真实结论。
type cmdOut struct {
	Bin      string
	Args     []string
	Stdout   string
	Stderr   string
	ExitCode int // -1 = 没跑起来（找不到二进制/启动失败）
	Err      error
}

// Display 是给界面/审计看的人话命令串（只用于展示，绝不拿去 shell 执行）。
func (c cmdOut) Display() string {
	return strings.Join(append([]string{c.Bin}, c.Args...), " ")
}

// Combined 是 stdout+stderr 的合并输出（截断前的原文）。
func (c cmdOut) Combined() string {
	s := strings.TrimSpace(c.Stdout)
	if e := strings.TrimSpace(c.Stderr); e != "" {
		if s != "" {
			s += "\n"
		}
		s += e
	}
	return s
}

// run 以数组参数执行一个命令，绝不经过 shell。
func (e *Executor) run(ctx context.Context, name string, args ...string) cmdOut {
	bin := LookBin(name)
	cctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	out := cmdOut{Bin: bin, Args: args, ExitCode: -1}
	cmd := exec.CommandContext(cctx, bin, args...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	out.Stdout, out.Stderr = so.String(), se.String()
	if err == nil {
		out.ExitCode = 0
		return out
	}
	out.Err = err
	if ee, ok := err.(*exec.ExitError); ok {
		out.ExitCode = ee.ExitCode()
	}
	return out
}

// tail 截断过长输出（界面折叠用）。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ---------- 服务状态（只读，任何时刻可调） ----------

// ServiceState 是一个共享服务的**真实**状态。
// *Known=false 表示探针读不到结论 —— 界面必须显示「未复核」，不许当成已停止/已开启。
type ServiceState struct {
	// Enabled：SMB = launchd 里服务已加载（launchctl print 成功）；NFS = nfsd service is enabled。
	Enabled      bool `json:"enabled"`
	EnabledKnown bool `json:"enabled_known"`
	// Running：守护进程现在真的在跑（pgrep / nfsd status）。
	Running      bool `json:"running"`
	RunningKnown bool `json:"running_known"`
	// Error 是探针失败的原因（Known=false 时）。
	Error string `json:"error,omitempty"`
	// Raw 是探针原样输出（截断）。
	Raw string `json:"raw,omitempty"`
}

// Verified 表示两个探针都拿到了确定结论。
func (s ServiceState) Verified() bool { return s.EnabledKnown && s.RunningKnown }

// SMBStatus 读一次 SMB 状态（只读）。
func (e *Executor) SMBStatus(ctx context.Context) ServiceState {
	st := ServiceState{}
	p := e.run(ctx, "launchctl", "print", "system/"+SMBLabel)
	raw := []string{}
	if c := p.Combined(); c != "" {
		raw = append(raw, "launchctl print: "+c)
	}
	switch {
	case p.Err == nil:
		st.Enabled, st.EnabledKnown = true, true
	case strings.Contains(p.Combined(), "Could not find service"):
		// 真机实测：未加载时退出码 113 + 这句。
		st.Enabled, st.EnabledKnown = false, true
	default:
		st.Error = "launchctl print 读不到结论：" + tail(p.Combined(), 200)
	}
	g := e.run(ctx, "pgrep", "-x", "smbd")
	switch {
	case g.Err == nil && strings.TrimSpace(g.Stdout) != "":
		st.Running, st.RunningKnown = true, true
	case g.ExitCode == 1:
		st.Running, st.RunningKnown = false, true
	default:
		if st.Error == "" {
			st.Error = "pgrep smbd 读不到结论：" + tail(g.Combined(), 200)
		}
	}
	if c := g.Combined(); c != "" {
		raw = append(raw, "pgrep -x smbd: "+c)
	}
	st.Raw = tail(strings.Join(raw, "\n"), 800)
	return st
}

// NFSStatus 读一次 NFS 状态（只读）。
func (e *Executor) NFSStatus(ctx context.Context) ServiceState {
	st := ServiceState{}
	n := e.run(ctx, "nfsd", "status")
	low := strings.ToLower(n.Combined())
	raw := []string{}
	if c := n.Combined(); c != "" {
		raw = append(raw, "nfsd status: "+c)
	}
	// 真机输出："nfsd service is enabled" / "nfsd is not running"；未运行时退出码是 1，
	// 所以**只认输出文本**，不看退出码。
	switch {
	case strings.Contains(low, "service is enabled"):
		st.Enabled, st.EnabledKnown = true, true
	case strings.Contains(low, "service is disabled"):
		st.Enabled, st.EnabledKnown = false, true
	}
	switch {
	case strings.Contains(low, "is not running"):
		st.Running, st.RunningKnown = false, true
	case strings.Contains(low, "is running"):
		st.Running, st.RunningKnown = true, true
	}
	if n.Err != nil && !st.EnabledKnown && !st.RunningKnown {
		st.Error = "nfsd status 读不到结论：" + tail(n.Combined(), 200)
	}
	// pgrep 作为独立佐证（nfsd status 文本变了也能兜住）。
	g := e.run(ctx, "pgrep", "-x", "nfsd")
	if g.Err == nil && strings.TrimSpace(g.Stdout) != "" {
		st.Running, st.RunningKnown = true, true
	}
	if c := g.Combined(); c != "" {
		raw = append(raw, "pgrep -x nfsd: "+c)
	}
	st.Raw = tail(strings.Join(raw, "\n"), 800)
	return st
}

// ---------- 服务开关（写，必须回读） ----------

// Result 是一次写操作的完整结论（可直接进审计/响应）。
type Result struct {
	Action   string        `json:"action"`
	Command  string        `json:"command,omitempty"`
	Commands []string      `json:"commands,omitempty"`
	Output   string        `json:"output,omitempty"`
	ExitCode int           `json:"exit_code"`
	Verified bool          `json:"verified"`
	OK       bool          `json:"ok"`
	State    *ServiceState `json:"state,omitempty"`
	Error    string        `json:"error,omitempty"`
	Note     string        `json:"note,omitempty"`
	// Missing 表示"要删的东西本来就不存在"（web 层据此回 404，而不是 502）。
	Missing bool `json:"missing,omitempty"`
}

func (e *Executor) execSeq(ctx context.Context, res *Result, name string, args ...string) cmdOut {
	c := e.run(ctx, name, args...)
	res.Commands = append(res.Commands, c.Display())
	if res.Output == "" {
		res.Output = c.Combined()
	} else if c.Combined() != "" {
		res.Output += "\n" + c.Combined()
	}
	res.ExitCode = c.ExitCode
	return c
}

// SMBAction 开启/关闭 SMB 共享（enable=true 开启）。成功只认回读。
func (e *Executor) SMBAction(ctx context.Context, enable bool) Result {
	res := Result{Action: "smb_enable"}
	if !enable {
		res.Action = "smb_disable"
	}
	var last cmdOut
	if enable {
		e.execSeq(ctx, &res, "launchctl", "enable", "system/"+SMBLabel)
		last = e.execSeq(ctx, &res, "launchctl", "bootstrap", "system", e.smbdPlist())
	} else {
		e.execSeq(ctx, &res, "launchctl", "bootout", "system", e.smbdPlist())
		last = e.execSeq(ctx, &res, "launchctl", "disable", "system/"+SMBLabel)
	}
	res.Command = strings.Join(res.Commands, " && ")
	// 回读：退出码 0 不算数，launchctl print 说加载了才算。
	st := e.SMBStatus(ctx)
	res.State, res.Verified = &st, st.Verified()
	if enable {
		if st.EnabledKnown && st.Enabled {
			res.OK = true
			return res
		}
		res.Error = "命令退出码 0，但回读显示 SMB 服务仍未加载（退出码不算数）"
		if !st.EnabledKnown {
			res.Error = "命令已执行，但回读不到 SMB 服务状态（未复核，不敢报成功）"
		}
		if st.Error != "" {
			res.Error += "：" + st.Error
		}
	} else {
		if st.EnabledKnown && !st.Enabled && st.RunningKnown && !st.Running {
			res.OK = true
			return res
		}
		res.Error = "命令退出码 0，但回读显示 SMB 服务仍在运行"
		if !st.EnabledKnown || !st.RunningKnown {
			res.Error = "命令已执行，但回读不到 SMB 服务状态（未复核，不敢报成功）"
		}
		if st.Error != "" {
			res.Error += "：" + st.Error
		}
	}
	if last.Err != nil && last.Combined() != "" {
		res.Error += "；命令输出：" + tail(last.Combined(), 300)
	}
	return res
}

// NFSAction 开启/关闭 NFS 导出（enable=true 开启）。成功只认回读。
func (e *Executor) NFSAction(ctx context.Context, enable bool) Result {
	res := Result{Action: "nfs_enable"}
	if !enable {
		res.Action = "nfs_disable"
	}
	if enable {
		e.execSeq(ctx, &res, "nfsd", "enable")
		e.execSeq(ctx, &res, "nfsd", "start")
	} else {
		e.execSeq(ctx, &res, "nfsd", "stop")
		e.execSeq(ctx, &res, "nfsd", "disable")
	}
	res.Command = strings.Join(res.Commands, " && ")
	st := e.NFSStatus(ctx)
	res.State, res.Verified = &st, st.Verified()
	if !st.RunningKnown {
		res.Error = "命令已执行，但回读不到 nfsd 是否在跑（未复核，不敢报成功）"
		if st.Error != "" {
			res.Error += "：" + st.Error
		}
		return res
	}
	if enable && st.Running {
		res.OK = true
		return res
	}
	if !enable && !st.Running {
		res.OK = true
		return res
	}
	if enable {
		res.Error = "命令退出码 0，但回读显示 nfsd 仍未在跑"
	} else {
		res.Error = "命令退出码 0，但回读显示 nfsd 仍在跑"
	}
	return res
}

// LocalIPv4Probe 是"本机局域网地址"的探测实现（门禁注入假地址用；生产走 sysinfo.LANIPv4）。
var LocalIPv4Probe = sysinfo.LANIPv4

// LocalIPv4 是本机局域网地址（只读探测；读不到回空串，调用方如实标"地址未知"）。
//
// 实现放在 sysinfo.LANIPv4：那里**不写死 en0**（Mac mini 的以太网是 en0、Wi-Fi 是 en1，
// 拔了网线只看 en0 就会"读不到本机 IP"，用户 2026-10-06 报障）。
func LocalIPv4(ctx context.Context) string {
	return LocalIPv4Probe(ctx)
}

// URL 构造客户端挂载地址建议：smb://<ip>/<共享名>；nfs://<ip><导出路径>。
func URL(kind, ip, name string, readOnly bool) string {
	if ip == "" {
		return ""
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "nfs" {
		p := name
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return "nfs://" + ip + p
	}
	return "smb://" + ip + "/" + strings.TrimPrefix(name, "/")
}
