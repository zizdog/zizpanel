// Package smb —— 「把 NAS 的网络共享挂到本机」这一件事的后端（SMB 与 NFS）。
//
// NFS 无凭据：直接跑 mount_nfs（不用 pty、不喂任何口令），其余（降权、回读、卸载）与 SMB 同源。
//
// 口令通道（2026-09-29 逐条核实，依据 Apple 开源 mount_smbfs/SMBClient 源码）：
//   - 写进命令行 URL（`//user:pass@host/share`）会进 argv，`ps` 里能看见 —— **不用**；
//   - `-o password=` 被 mount_smbfs 拒绝（本机实测：`-o password: option not supported`）；
//   - nsmb.conf 的官方关键字表里**没有** password（man 5 nsmb.conf，AppKit 版同样没有）；
//   - 框架只在 TTY 上读口令：`lib/smbclient/server.c` 的 SMBPasswordPrompt 用
//     `readpassphrase(prompt, buf, size, RPP_REQUIRE_TTY)` —— RPP_REQUIRE_TTY 让
//     「没有 tty 就退回 stdin」这条 fallback **失效**，管道喂 stdin 无效；`-N` 直接不提示。
//
// 于是只剩一条安全通道：**给子进程一个真 TTY**（/usr/bin/script 分配 pty），
// 看到 "Password for …" 提示后再把口令写进那个 pty。口令不进 argv、不进环境变量、
// 不落盘；只存在于本进程内存与那个临时 pty 里，用完即弃。
package smb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
//  配置
// ---------------------------------------------------------------------------

// Mount 是一条网络磁盘配置。口令**只**留在服务端存储里（settings KV），
// 任何 HTTP 响应都不带它（对外只回 password_set）。
type Mount struct {
	ID   string `json:"id"`
	Name string `json:"name"` // 挂载点目录名，也是界面上的名字
	// Kind 是网络盘类型；空 = smb（老配置没有这个字段，必须继续当 SMB 用）。
	Kind     string `json:"kind,omitempty"`
	Host     string `json:"host"`
	Share    string `json:"share"`
	User     string `json:"user"`
	Domain   string `json:"domain,omitempty"`
	Password string `json:"password,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// GuestUser 是匿名/来宾访问的写法：`//guest:@host/share`（mount_smbfs 认这个形式）。
const GuestUser = "guest"

// 网络盘类型：SMB 有账号/口令；NFS 无凭据，Share 存导出路径。
const (
	KindSMB = "smb"
	KindNFS = "nfs"
)

// maxNFSExportLen 是导出路径的长度上限（字符）。
const maxNFSExportLen = 1024

// KindOrDefault 返回归一化后的类型：空/未知都当 SMB（Normalize 拒绝未知值）。
func (m Mount) KindOrDefault() string {
	if strings.ToLower(strings.TrimSpace(m.Kind)) == KindNFS {
		return KindNFS
	}
	return KindSMB
}

// NewID 生成一条配置的稳定 id（只用于面板内部与 URL，不参与命令行）。
func NewID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "smb-" + fmt.Sprint(time.Now().UnixNano())
	}
	return "smb-" + hex.EncodeToString(b)
}

// URL 拼 mount_smbfs 的目标（**不含口令**）。share 逐个字节百分号转义：
// 实测 `//user@host/中文` 与带空格的共享名会被判 "URL parsing failed"，
// 而 `%E5%AA%92%E4%BD%93` 能正常解析（2026-09-29 本机实测）。
func (m Mount) URL() string {
	return m.authPrefix() + m.Host + "/" + percentEncode(m.Share)
}

// authPrefix 返回 `//[域;]用户@` 这一段（没有用户就是 `//`）。
// shareName 无关，因此"列出共享"（smbutil view）与挂载共用同一份构造。
func (m Mount) authPrefix() string {
	user := m.User
	if user == GuestUser {
		user += ":" // //guest:@host/share = 来宾访问
	}
	if user == "" {
		return "//"
	}
	if d := strings.TrimSpace(m.Domain); d != "" {
		return "//" + d + ";" + user + "@"
	}
	return "//" + user + "@"
}

// percentEncode 只保留 RFC3986 的非保留字符，其余按字节转义。
func percentEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// PathComponent 是挂载点目录名 / 配置名的白名单：中英文、数字与 - _ .（≤40 字符）。
// 刻意不放行空格：挂载表是文本，空格会让解析出歧义（宁可让用户换个名字）。
func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("名字不能为空")
	}
	if n := len([]rune(name)); n > 40 {
		return fmt.Errorf("名字过长（%d 个字符，上限 40）", n)
	}
	for _, r := range name {
		switch {
		case r == '-' || r == '_' || r == '.':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
		case unicode.Is(unicode.Han, r):
		default:
			return fmt.Errorf("名字里有不允许的字符 %q（只允许中英文、数字与 - _ .）", string(r))
		}
	}
	if strings.Trim(name, ".") == "" {
		return errors.New("名字不能只由点组成")
	}
	return nil
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._:-]*[A-Za-z0-9.])?$`)
var userRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Source 返回这条网络盘在挂载表里的源：SMB = URL()，NFS = host:/export。
func (m Mount) Source() string {
	if m.KindOrDefault() == KindNFS {
		return m.Host + ":" + m.Share
	}
	return m.URL()
}

// Normalize 去空白并做保守校验。返回的 Mount 已可直接用。
func Normalize(m *Mount) error {
	m.Name = strings.TrimSpace(m.Name)
	m.Host = strings.TrimSpace(m.Host)
	m.Share = strings.TrimSpace(m.Share)
	m.User = strings.TrimSpace(m.User)
	m.Domain = strings.TrimSpace(m.Domain)
	m.Kind = strings.ToLower(strings.TrimSpace(m.Kind))
	switch m.Kind {
	case "":
		m.Kind = KindSMB // 老配置没有 kind
	case KindSMB, KindNFS:
	default:
		return errors.New("网络盘类型不支持：" + m.Kind + "（只支持 smb / nfs）")
	}

	if m.ID == "" {
		m.ID = NewID()
	}
	if err := validateName(m.Name); err != nil {
		return err
	}
	if m.Host == "" {
		return errors.New("NAS 地址不能为空（IP 或主机名）")
	}
	if len(m.Host) > 253 || !hostRe.MatchString(m.Host) {
		return errors.New("NAS 地址不合法：" + m.Host + "（只允许字母数字与 . - : _）")
	}
	if m.Kind == KindNFS {
		return normalizeNFS(m)
	}
	// ---- 以下 SMB 分支与加 NFS 之前逐字一致（老配置行为不许变） ----
	if m.Share == "" {
		return errors.New("共享名不能为空")
	}
	if len([]rune(m.Share)) > 128 {
		return errors.New("共享名过长（上限 128 字符）")
	}
	for _, r := range m.Share {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\@;%`, r) {
			return fmt.Errorf("共享名里有不允许的字符 %q", string(r))
		}
	}
	if m.User == "" {
		return fmt.Errorf("用户名不能为空（公开共享请填 %s）", GuestUser)
	}
	if !userRe.MatchString(m.User) {
		return errors.New("用户名不合法：" + m.User + "（只允许字母数字与 . _ -）")
	}
	if m.Domain != "" && !userRe.MatchString(m.Domain) {
		return errors.New("域不合法：" + m.Domain + "（只允许字母数字与 . _ -）")
	}
	// 非来宾必须有口令：留着空口令去挂只会得到一句"认证失败"，不如当场说清。
	if m.User != GuestUser && m.Password == "" {
		return fmt.Errorf("请填口令（公开共享请把用户名填 %s）", GuestUser)
	}
	if m.User == GuestUser {
		m.Password = ""
	}
	if strings.ContainsAny(m.Password, "\r\n") {
		return errors.New("口令里不能有换行")
	}
	return nil
}

// normalizeNFS 校验 NFS 导出路径，并清空凭据（NFS 没有账号/口令）。
func normalizeNFS(m *Mount) error {
	m.User, m.Domain, m.Password = "", "", ""
	if m.Share == "" {
		return errors.New("导出路径不能为空（例如 /volume1/media）")
	}
	if len([]rune(m.Share)) > maxNFSExportLen {
		return fmt.Errorf("导出路径过长（上限 %d 字符）", maxNFSExportLen)
	}
	if !strings.HasPrefix(m.Share, "/") {
		return errors.New("导出路径必须以 / 开头（例如 /volume1/media）")
	}
	if strings.Contains(m.Share, "..") {
		return errors.New("导出路径不能包含 ..")
	}
	for _, r := range m.Share {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("导出路径里有空白或控制字符 %q", string(r))
		}
	}
	return nil
}

// MountPoint 返回这条配置的挂载点：<安装根>/mnt/<名字>。
func MountPoint(base, name string) string { return filepath.Join(base, strings.TrimSpace(name)) }

// ---------------------------------------------------------------------------
//  执行（挂载 / 卸载 / 挂载表回读）
// ---------------------------------------------------------------------------

// 可注入/可垫片的命令解析：全部先走 PATH（门禁与隔离实例用假命令垫片），
// 找不到再退回系统绝对路径（LaunchDaemon 的 PATH 可能很干净）。
var (
	lookPathFn = exec.LookPath
	fallbacks  = map[string]string{
		"script":      "/usr/bin/script",
		"mount_smbfs": "/sbin/mount_smbfs",
		"mount_nfs":   "/sbin/mount_nfs",
		"smbutil":     "/usr/bin/smbutil",
		"mount":       "/sbin/mount",
		"umount":      "/sbin/umount",
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

// Entry 是挂载表里的一行（来自 `mount` 命令的文本输出）。
type Entry struct {
	Source     string
	MountPoint string
	FSType     string
	Options    string
}

var mountLineRe = regexp.MustCompile(`^(.+) on (.+) \(([^()]*)\)$`)

// MountTable 读一次真实挂载表。用 `mount` 命令而不是 getfsstat 系统调用：
// 命令可以被 PATH 垫片替换，门禁与隔离实例才能在不 root、不连真 NAS 的前提下
// 验完"回读"这条路（数据来源与系统调用是同一份内核状态）。
func MountTable(ctx context.Context) ([]Entry, error) {
	bin := LookBin("mount")
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin).Output()
	if err != nil {
		return nil, fmt.Errorf("读取挂载表失败（%s）：%w", bin, err)
	}
	return parseMountTable(string(out)), nil
}

func parseMountTable(text string) []Entry {
	var out []Entry
	for _, ln := range strings.Split(text, "\n") {
		m := mountLineRe.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		opts := m[3]
		fs := strings.TrimSpace(strings.SplitN(opts, ",", 2)[0])
		out = append(out, Entry{Source: m[1], MountPoint: m[2], FSType: fs, Options: opts})
	}
	return out
}

// Canon 把路径解析成真实路径（/tmp → /private/tmp）。挂载表里存的是真实路径，
// 拿用户给的路径直接比会永远比不上。
func Canon(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// EntryAt 在挂载表里找某个挂载点。
func EntryAt(entries []Entry, mountPoint string) (Entry, bool) {
	want := Canon(mountPoint)
	for _, e := range entries {
		if Canon(e.MountPoint) == want {
			return e, true
		}
	}
	return Entry{}, false
}

// Result 是一次挂载/卸载动作的完整结论（口令已 scrub，可直接进日志/响应）。
type Result struct {
	Action     string   `json:"action"`
	Command    string   `json:"command"`
	Output     string   `json:"output,omitempty"`
	ExitCode   int      `json:"exit_code"`
	Verified   bool     `json:"verified"`
	Mounted    bool     `json:"mounted"`
	MountPoint string   `json:"mount_point"`
	FSType     string   `json:"fs_type,omitempty"`
	Source     string   `json:"source,omitempty"`
	Entries    []string `json:"entries,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Error      string   `json:"error,omitempty"`
	Remedy     string   `json:"remedy,omitempty"`
	Note       string   `json:"note,omitempty"`
	// RunAs 是这次挂载实际降权到的本地用户（空 = 当前身份）。
	RunAs string `json:"run_as,omitempty"`
}

// Executor 执行挂载/卸载。零值可用；单测只改 Timeout/RunAs。
type Executor struct {
	Timeout time.Duration // 单次挂载总超时（默认 90s）
	// RunAs 是"以哪个本地用户身份挂载"（空 = 当前身份）。面板以 root 运行时**必须**给：
	// 挂载归执行身份所有，root 挂的 smbfs 别的用户连目录都进不去（2026-09-29 真机实测：
	// 面板 root 挂上后 zizdog `ls` 直接 Permission denied ⇒ Jellyfin 会是空库）。
	RunAs string
}

// RunAsUser 是一次挂载要降权到的本地用户。
type RunAsUser struct {
	Name string
	UID  uint32
	GID  uint32
	Home string
}

// runAsLookupFn / runAsEUIDFn 是注入点（门禁里换成假的：绝不真的 setuid）。
var (
	runAsLookupFn = user.Lookup
	runAsEUIDFn   = os.Geteuid
)

// resolveRunAs 把用户名解析成 uid/gid/家目录。解析不到、空名、root、或自己不是 root
// ⇒ 返回 nil（就按当前身份跑，绝不猜）。
func resolveRunAs(name string) *RunAsUser {
	name = strings.TrimSpace(name)
	if name == "" || runAsEUIDFn() != 0 {
		return nil
	}
	u, err := runAsLookupFn(name)
	if err != nil || u == nil {
		return nil
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil || uid == 0 {
		return nil
	}
	return &RunAsUser{Name: u.Username, UID: uint32(uid), GID: uint32(gid), Home: u.HomeDir}
}

func (e *Executor) timeout() time.Duration {
	if e == nil || e.Timeout <= 0 {
		return 90 * time.Second
	}
	return e.Timeout
}

// mountOptions 是两类网络盘各自挂载时要带的选项。
//
// NAS 会关机，**必须让 I/O 失败而不是永久挂起**：
//   - SMB：nodev,nosuid,soft（man mount_smbfs：soft = 若干秒后让文件系统调用失败），
//     只读再加 rdonly；
//   - NFS：nosuid,nodev,soft,timeo=10,retrans=2,vers=3（man mount_nfs：soft 在 retrans
//     轮超时后失败，timeo 单位是十分之一秒；soft+只读会自动带 deadtimeout=60），只读再加 ro。
func mountOptions(m Mount) []string {
	if m.KindOrDefault() == KindNFS {
		opts := []string{"nosuid", "nodev", "soft", "timeo=10", "retrans=2", "vers=3"}
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		return opts
	}
	opts := []string{"nodev", "nosuid", "soft"}
	// nomdatacache：关掉**元数据**缓存。不关的话刚传上去的文件在 40s+ 内都不会出现在
	// 目录列表里（本机真机实测：关掉后 0.0s 就出现）—— 用户要"传文件"，看不见就是不能用。
	// 只关元数据；文件数据缓存照旧（播放/下载性能不受影响）。
	opts = append(opts, "nomdatacache")
	if m.ReadOnly {
		opts = append(opts, "rdonly")
	}
	return opts
}

// MountArgs 拼完整命令行，**不含口令**。SMB 走 pty 包装（argv[0] 是 /usr/bin/script，
// 口令另走 pty 通道）；NFS 无凭据、不需要 tty，直接 mount_nfs。导出让门禁断言构造。
func MountArgs(m Mount, mountPoint string) []string {
	if m.KindOrDefault() == KindNFS {
		args := []string{LookBin("mount_nfs")}
		if opts := mountOptions(m); len(opts) > 0 {
			args = append(args, "-o", strings.Join(opts, ","))
		}
		return append(args, m.Source(), mountPoint)
	}
	args := []string{LookBin("script"), "-q", "/dev/null", LookBin("mount_smbfs")}
	if opts := mountOptions(m); len(opts) > 0 {
		args = append(args, "-o", strings.Join(opts, ","))
	}
	return append(args, m.URL(), mountPoint)
}

// Mount 执行一次挂载：建挂载点 → 起 mount_smbfs（pty 喂口令）或 mount_nfs →
// **回读真实状态**（挂载表里在不在 + 目录读不读得到）。任何一步不成立都如实报失败。
func (e *Executor) Mount(ctx context.Context, m Mount, mountPoint string) Result {
	res := Result{Action: "mount", MountPoint: mountPoint}
	if err := Normalize(&m); err != nil {
		res.Reason, res.Error = "invalid", err.Error()
		res.Remedy = "改完配置再挂载。"
		return res
	}
	isNFS := m.KindOrDefault() == KindNFS
	cmdName := "mount_smbfs"
	if isNFS {
		cmdName = "mount_nfs"
	}
	// 挂载点已经被**别人**占着就拒绝：盖上去会把原来的挂载藏掉
	// （<安装根>/mnt 下可能已经有卷，比如镜像盘），藏掉别人的挂载是破坏性的。
	if table, terr := MountTable(ctx); terr == nil {
		if e, found := EntryAt(table, mountPoint); found && e.Source != m.Source() {
			res.Reason = "mount_point_busy"
			res.Error = "挂载点 " + mountPoint + " 已经被 " + e.Source + " 占着（不是这条网络盘）。"
			res.Remedy = "改掉这条网络盘的名字（挂载点由名字决定），或先卸载原来那个。"
			return res
		}
	}
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		res.Reason, res.Error = "mount_point", "创建挂载点目录失败："+err.Error()
		res.Remedy = "确认面板进程有权限写 " + filepath.Dir(mountPoint) + "。"
		return res
	}
	// 以"要用它的那个用户"（Jellyfin 的真实用户）身份挂载：挂载归执行身份所有。
	as := resolveRunAs(e.RunAs)
	if as != nil {
		res.RunAs = as.Name
		if err := os.Chown(mountPoint, int(as.UID), int(as.GID)); err != nil {
			res.Note = "挂载点归属交给 " + as.Name + " 失败：" + err.Error()
		}
	}
	argv := MountArgs(m, mountPoint)
	res.Command = strings.Join(argv, " ")

	// NFS 无凭据 ⇒ 直接跑，绝不经过 pty；SMB 一行不变（走 pty 喂口令）。
	var out string
	var exit int
	var err error
	if isNFS {
		out, exit, err = runDirect(ctx, e.timeout(), argv, as)
	} else {
		out, exit, err = runWithPTY(ctx, e.timeout(), argv, m.Password, as)
	}
	out = Scrub(out, m.Password)
	res.Output, res.ExitCode = out, exit

	// 先回读再判退出码：命令报错也可能已经挂上了（反之亦然），以真实状态为准。
	ver := VerifyAt(ctx, mountPoint)
	res.Verified = ver.Verified
	res.Mounted = ver.Mounted
	res.FSType, res.Source = ver.FSType, ver.Source
	res.Entries = ver.Entries

	if err != nil || exit != 0 {
		res.Reason, res.Error, res.Remedy = classifyMount(m.KindOrDefault(), out, exit, err)
		if res.Verified && res.Mounted {
			res.Note = "命令报错，但回读显示已经挂上了 —— 以真实状态为准。"
		}
		return res
	}
	if !ver.Verified {
		res.Reason = "verify_failed"
		res.Error = cmdName + " 退出码为 0，但回读失败：" + ver.Error
		res.Remedy = "以真实状态为准：这次**不算挂载成功**。确认共享可读、面板有「完全磁盘访问」授权。"
		if isNFS {
			res.Remedy = "以真实状态为准：这次**不算挂载成功**。确认导出路径可读、服务器放行了本机。"
		}
		return res
	}
	if !ver.Mounted {
		res.Reason = "verify_failed"
		res.Error = cmdName + " 退出码为 0，但 " + mountPoint + " 不在挂载表里 —— 这次**不算挂载成功**。"
		res.Remedy = "看下面的原样输出；多数是口令/共享名不对，或共享已经被别的会话占用。"
		if isNFS {
			res.Remedy = "看下面的原样输出；多数是导出路径/权限不对，或已被别的会话占用。"
		}
		return res
	}
	if len(res.Entries) == 0 {
		res.Note = "已挂载，但这个共享里一个文件都没有（空共享？）。"
	}
	return res
}

// Share 是 NAS 上的一个 SMB 共享（`smbutil view` 的结果）。
type Share struct {
	Name string `json:"name"`
	Type string `json:"type"` // Disk / Pipe
}

// ListShares 列出 NAS 上的 SMB 共享，用户不必背共享名（真机：把挂载名当共享名填，
// 服务端只会回一句 0xC0000225，用户完全无从下手）。
//
// 口令通道与挂载完全同源：`smbutil view` 与 `mount_smbfs` 用同一个 readpassphrase 提示
// （2026-09-29 真机实测：`Password for <nas>:`，走 pty 写入口令即可）。
func (e *Executor) ListShares(ctx context.Context, host, user, domain, password string) ([]Share, string, error) {
	m := Mount{Host: strings.TrimSpace(host), User: strings.TrimSpace(user), Domain: strings.TrimSpace(domain)}
	if m.Host == "" || len(m.Host) > 253 || !hostRe.MatchString(m.Host) {
		return nil, "", errors.New("NAS 地址不合法（只填 IP 或主机名）")
	}
	argv := []string{LookBin("script"), "-q", "/dev/null", LookBin("smbutil"), "view", m.authPrefix() + m.Host}
	out, exit, err := runWithPTY(ctx, e.timeout(), argv, password, resolveRunAs(e.RunAs))
	out = Scrub(out, password)
	if err != nil || exit != 0 {
		_, msg, remedy := classifyMount(KindSMB, out, exit, err)
		if strings.TrimSpace(msg) == "" {
			msg = "列出共享失败"
		}
		return nil, out, errors.New(strings.TrimSpace(msg + " " + remedy))
	}
	return ParseShareTable(out), out, nil
}

// ParseShareTable 解析 `smbutil view` 的表格，只留 Disk（IPC$ 之类的 Pipe 不要）。
//
// 表头下方每行是「共享名 + 类型 + 备注」，列之间是多个空格；共享名本身不含空格。
func ParseShareTable(out string) []Share {
	var shares []Share
	inTable := false
	seen := map[string]bool{}
	for _, ln := range strings.Split(out, "\n") {
		line := strings.TrimRight(ln, " \t\r")
		if strings.HasPrefix(line, "---") {
			inTable = true
			continue
		}
		if !inTable || strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "listed") || strings.HasPrefix(strings.TrimSpace(line), "Share ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[1], "Disk") || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		shares = append(shares, Share{Name: f[0], Type: f[1]})
	}
	return shares
}

// Unmount 卸载一个挂载点。忙（有进程在用）时如实拒绝，绝不 -f 强卸。
func (e *Executor) Unmount(ctx context.Context, mountPoint string) Result {
	res := Result{Action: "unmount", MountPoint: mountPoint}
	bin := LookBin("umount")
	argv := []string{bin, mountPoint}
	res.Command = strings.Join(argv, " ")
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	exit := 0
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		exit = ee.ExitCode()
	}
	text := strings.TrimSpace(buf.String())
	if cctx.Err() == context.DeadlineExceeded {
		text += "\numount 超时（30 秒）"
	}
	res.Output, res.ExitCode = text, exit

	ver := VerifyAt(ctx, mountPoint)
	res.Verified, res.Mounted = ver.Verified, ver.Mounted
	if !ver.Verified {
		res.Reason, res.Error = "verify_failed", "回读挂载表失败："+ver.Error
		return res
	}
	if res.Mounted {
		low := strings.ToLower(text)
		switch {
		case strings.Contains(low, "resource busy") || strings.Contains(low, "busy"):
			res.Reason = "busy"
			res.Error = "有进程正在使用 " + mountPoint + "，没有卸载。"
			res.Remedy = "先停掉在用它的小程序/服务（例如 Jellyfin 正在扫描媒体库），再重试。"
		case cctx.Err() == context.DeadlineExceeded:
			res.Reason = "timeout"
			res.Error = "umount 超时，设备可能处于中间态；以回读为准：仍在挂载表里。"
		default:
			res.Reason = "failed"
			res.Error = "umount 没有卸下来（仍在挂载表里）。" + tail(text, 200)
			res.Remedy = "确认路径没写错、面板是 root。"
		}
		return res
	}
	if exit != 0 && !strings.Contains(strings.ToLower(text), "not currently mounted") {
		res.Note = "umount 报了错，但回读显示已经不在挂载表里 —— 以真实状态为准。"
	}
	return res
}

// 有界读：NAS 关机/掉线时挂载点上的 ReadDir 可能长时间不返回。
// 面板任何路径都不许被它挂住 ⇒ 统一走这个带超时的读（默认 5s）。
var (
	readDirFn      = os.ReadDir
	readDirTimeout = 5 * time.Second
)

// SetReadDirForTest 只给门禁用：替换底层读函数，返回还原函数（负向对照用阻塞读）。
func SetReadDirForTest(fn func(string) ([]os.DirEntry, error)) (restore func()) {
	prev := readDirFn
	readDirFn = fn
	return func() { readDirFn = prev }
}

// SetReadDirTimeoutForTest 只给门禁用：改有界读超时，返回还原函数。
func SetReadDirTimeoutForTest(d time.Duration) (restore func()) {
	prev := readDirTimeout
	readDirTimeout = d
	return func() { readDirTimeout = prev }
}

// ErrMountUnresponsive 表示"读挂载点超时"——NAS 很可能离线了。
var ErrMountUnresponsive = errors.New("挂载点无响应（NAS 可能离线）")

// IsMountUnresponsive 判断读挂载点失败是不是"网络盘失去响应"（超时，或死挂载的典型 errno）。
//
// 为什么必须区分：只有这一类才值得自动 remount。权限之类的错误重挂一百次也没用，
// 还会把正在用这个盘跑的应用（Jellyfin 扫库）打断 —— 那才是"影响面板运行"。
func IsMountUnresponsive(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrMountUnresponsive) {
		return true
	}
	for _, e := range []error{
		syscall.EIO, syscall.ESTALE, syscall.ENXIO, syscall.ETIMEDOUT,
		syscall.ENETDOWN, syscall.EHOSTDOWN, syscall.ECONNRESET, syscall.ENOTCONN,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// ReadDirBounded 带超时地读一个目录；超时返回人话错误，绝不把调用方挂在那里。
func ReadDirBounded(dir string) ([]os.DirEntry, error) {
	type result struct {
		entries []os.DirEntry
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := readDirFn(dir)
		ch <- result{e, err}
	}()
	select {
	case r := <-ch:
		return r.entries, r.err
	case <-time.After(readDirTimeout):
		return nil, ErrMountUnresponsive
	}
}

// Verify 是回读结论（挂载表 + 目录可读性）。
type Verify struct {
	Verified   bool
	Mounted    bool
	MountPoint string
	Source     string
	FSType     string
	Entries    []string
	Error      string
}

// VerifyAt 回读一个挂载点：先看它在不在真实挂载表里、是不是网络盘，
// 再真的把目录读一次（读不到 = 没挂成功/被隐私保护挡住）。
//
// 判据：fstype 必须是 smbfs（source 形如 //host/share）或 nfs（source 形如 host:/export），
// 且 source 形态与 fstype 匹配。本地盘（apfs 等）占着这个挂载点一律不算成功。
func VerifyAt(ctx context.Context, mountPoint string) Verify {
	v := Verify{MountPoint: mountPoint}
	entries, err := MountTable(ctx)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	e, ok := EntryAt(entries, mountPoint)
	if !ok {
		v.Verified = true // 挂载表读到了，"不在表里"是确定结论
		return v
	}
	switch {
	case e.FSType == "smbfs" && strings.HasPrefix(e.Source, "//"):
	case e.FSType == "nfs" && isNFSSource(e.Source):
	default:
		v.Error = "挂载点在挂载表里，但文件系统是 " + e.FSType + "（源 " + e.Source + "），不是 SMB/NFS 网络盘"
		return v
	}
	v.Mounted, v.Verified = true, true
	v.FSType, v.Source = e.FSType, e.Source

	dir, derr := ReadDirBounded(mountPoint)
	if derr != nil {
		v.Mounted, v.Verified = false, false
		v.Error = "已挂载，但读不到目录内容：" + derr.Error()
		return v
	}
	for _, d := range dir {
		if len(v.Entries) >= 20 {
			break
		}
		v.Entries = append(v.Entries, d.Name())
	}
	return v
}

// isNFSSource 判断挂载表里的源是不是 NFS 的 host:/export 形态。
func isNFSSource(s string) bool {
	if strings.HasPrefix(s, "/") {
		return false
	}
	i := strings.Index(s, ":")
	return i > 0 && strings.HasPrefix(s[i+1:], "/")
}

// ---------------------------------------------------------------------------
//  口令保护
// ---------------------------------------------------------------------------

// Scrub 把口令从任何要落日志/进响应的文本里抹掉（pty 回显是唯一可能的泄漏点）。
func Scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "********")
}

// Clean 去掉 pty 回显里的控制字符（script 退出时会打一个 EOT），只留可读文本。
func Clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || r >= 0x20 {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func tail(s string, n int) string {
	s = Clean(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ---------------------------------------------------------------------------
//  pty 执行
// ---------------------------------------------------------------------------

// PromptMarker 是框架提示口令的那句话的开头（server.c: "Password for %s: "）。
const PromptMarker = "assword for"

// runWithPTY 用 `script` 分配一个 pty 跑命令，看到口令提示后写入口令。
// as 非空时把整个 pty 链降权到该本地用户（挂载归它所有）。
// 返回：合并输出（未 scrub）、退出码、错误。
func runWithPTY(ctx context.Context, timeout time.Duration, argv []string, password string, as *RunAsUser) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	// 独立进程组：超时/取消时连 script 的子进程一起杀掉，不留孤儿。
	attr := &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()
	if as != nil {
		attr.Credential = &syscall.Credential{Uid: as.UID, Gid: as.GID}
		// HOME 必须跟着换：mount_smbfs 会去读 ~/Library/Preferences/nsmb.conf。
		cmd.Env = append(cmd.Env, "HOME="+as.Home, "USER="+as.Name, "LOGNAME="+as.Name)
	}
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second

	pr, pw, err := os.Pipe()
	if err != nil {
		return "", -1, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return "", -1, err
	}

	var mu sync.Mutex
	var acc bytes.Buffer
	promptCh := make(chan struct{})
	var promptOnce sync.Once
	readDone := make(chan struct{})

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return "", -1, err
	}
	_ = pw.Close() // 父进程这份关掉，子进程退出时读端才会 EOF

	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				mu.Lock()
				acc.Write(buf[:n])
				seen := strings.Contains(acc.String(), PromptMarker)
				mu.Unlock()
				if seen {
					promptOnce.Do(func() { close(promptCh) })
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// 只有真的看到提示才写口令：写早了会落在 pty 的回显里（虽然最后也会 scrub）。
	// 主动关掉 stdin 会让 script 给 pty 送 EOF，子进程可能读不到口令（实测），所以不关。
	if password != "" {
		go func() {
			select {
			case <-promptCh:
				_, _ = io.WriteString(stdin, password+"\n")
			case <-cctx.Done():
			case <-readDone:
			}
		}()
	}

	werr := cmd.Wait()
	_ = stdin.Close()
	_ = pr.Close()
	<-readDone

	mu.Lock()
	out := acc.String()
	mu.Unlock()

	exit := 0
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	if cctx.Err() == context.DeadlineExceeded {
		return out, exit, errMountTimeout
	}
	if errors.Is(cctx.Err(), context.Canceled) {
		return out, exit, context.Canceled
	}
	return out, exit, werr
}

var errMountTimeout = errors.New("命令超时")

// runDirect 直接跑命令，不分配 pty（NFS 无凭据、框架不会提示口令）。
// 与 runWithPTY 同源：独立进程组 + context 超时 + 组杀 + RunAs 降权。
func runDirect(ctx context.Context, timeout time.Duration, argv []string, as *RunAsUser) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	attr := &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()
	if as != nil {
		attr.Credential = &syscall.Credential{Uid: as.UID, Gid: as.GID}
		cmd.Env = append(cmd.Env, "HOME="+as.Home, "USER="+as.Name, "LOGNAME="+as.Name)
	}
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second

	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf

	werr := cmd.Run()
	out := buf.String()
	exit := 0
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	if cctx.Err() == context.DeadlineExceeded {
		return out, exit, errMountTimeout
	}
	if errors.Is(cctx.Err(), context.Canceled) {
		return out, exit, context.Canceled
	}
	return out, exit, werr
}

// ---------------------------------------------------------------------------
//  错误分类（人话 + 出路）
// ---------------------------------------------------------------------------

// classifyMount 按类型给结论：NFS 与 SMB 的错误文本完全不同，绝不互相套用。
func classifyMount(kind, out string, exit int, err error) (reason, msg, remedy string) {
	low := strings.ToLower(out)
	raw := tail(out, 300)
	if kind == KindNFS {
		return classifyNFS(low, raw, exit, err)
	}
	switch {
	case errors.Is(err, errMountTimeout):
		return "timeout", "挂载超时：NAS 没有在超时时间内返回。", "确认地址/网络；NAS 离线时面板会按退避自动重试。"
	case strings.Contains(low, "server rejected the connection") || exit == 77:
		return "auth", "认证失败：用户名或口令不对，或该账号没有这个共享的权限。", "核对用户名/口令（域账号要填域）；改完点「重新挂载」。"
	case strings.Contains(low, "share connection failed"):
		return "share", "连上了 NAS，但共享不存在或账号无权访问。", "核对共享名（不是路径、不含斜杠）；在 NAS 上看这个账号的共享权限。"
	case strings.Contains(low, "server connection failed") || exit == 68:
		return "unreachable", "连不上 NAS（地址不对、网络不通、或 NAS 关机）。", "确认 NAS 地址可从本机 ping 通、共享服务已开；面板会按退避自动重试。" +
			"若网络确实通，看 macOS 15 的「本地网络」隐私门：面板 系统设置 → 局域网访问（需重启面板），或 系统设置 → 隐私与安全性 → 本地网络。"
	case strings.Contains(low, "url parsing failed"):
		return "bad_url", "NAS 地址或共享名不合法，mount_smbfs 拒绝解析。", "地址只填 IP/主机名；共享名不要带斜杠。"
	case strings.Contains(low, "could not find mount point") || strings.Contains(low, "can't mount on"):
		return "mount_point", "挂载点不可用：" + raw, "确认挂载点目录存在且面板有权限写它。"
	case strings.Contains(low, "operation not permitted") || strings.Contains(low, "permission denied"):
		return "denied", "系统拒绝了这次挂载（权限/隐私保护）。", "确认面板以 root 的 LaunchDaemon 运行；" +
			"也可能是 macOS 15 的「本地网络」门：面板 系统设置 → 局域网访问（需重启面板），或 系统设置 → 隐私与安全性 → 本地网络。"
	}
	// SMB 状态码翻译。mount_smbfs 只回一句 "Unknown error: -1073741275"，不翻译等于没提示
	// （真机：mini 上共享名填成 fnnas-media，NAS 上只有 media ⇒ 就是这个 0xC0000225）。
	if h, ok := smbStatusHint(low); ok {
		return "server_status", h.msg, h.how
	}
	switch {
	case exit != 0:
		return "failed", "mount_smbfs 失败（退出码 " + fmt.Sprint(exit) + "）：" + raw, "看下面的原样输出定位；改完点「重新挂载」。"
	default:
		return "failed", "挂载失败：" + raw, ""
	}
}

// smbStatusHint 把 SMB 的 NT 状态码翻成人话（十进制与十六进制都认，输出里两种都可能出现）。
type smbStatusHintT struct {
	msg string
	how string
}

var smbStatusHints = map[string]smbStatusHintT{
	"-1073741275": {"NAS 上找不到这个共享：共享名不对，或共享已被删。",
		"到 NAS 上确认共享名（就是光名字，像 media；不是路径、也不是面板里那个挂载名字）；改完点「重新挂载」。"},
	"c0000225": {"NAS 上找不到这个共享：共享名不对，或共享已被删。",
		"到 NAS 上确认共享名（就是光名字，像 media；不是路径、也不是面板里那个挂载名字）；改完点「重新挂载」。"},
	"-1073741620": {"共享名不对：NAS 上没有这个名字的共享。", "核对共享名；别把路径或挂载名填进「共享名」。"},
	"c00000cc":    {"共享名不对：NAS 上没有这个名字的共享。", "核对共享名；别把路径或挂载名填进「共享名」。"},
	"-1073741715": {"认证失败：用户名或口令不对。", "核对用户名/口令（域账号要填域）。"},
	"c000006d":    {"认证失败：用户名或口令不对。", "核对用户名/口令（域账号要填域）。"},
	"-1073741790": {"拒绝访问：这个账号没有该共享的权限。", "到 NAS 上给这个账号开共享权限。"},
	"c0000022":    {"拒绝访问：这个账号没有该共享的权限。", "到 NAS 上给这个账号开共享权限。"},
}

func smbStatusHint(low string) (smbStatusHintT, bool) {
	for code, h := range smbStatusHints {
		if strings.Contains(low, code) {
			return h, true
		}
	}
	return smbStatusHintT{}, false
}

// classifyNFS 把 mount_nfs 的常见失败翻成人话（不复用 SMB 的结论）。
func classifyNFS(low, raw string, exit int, err error) (reason, msg, remedy string) {
	switch {
	case errors.Is(err, errMountTimeout):
		return "timeout", "挂载超时：NFS 服务器没有在超时时间内返回。", "确认地址/网络；服务器离线时面板会按退避自动重试。"
	case strings.Contains(low, "no such file or directory") || strings.Contains(low, "not exported") ||
		strings.Contains(low, "no such export"):
		return "export", "NFS 导出路径不存在（或没对本机导出）。", "在服务器上核对导出路径与 /etc/exports 的客户端允许列表。"
	case strings.Contains(low, "program not registered") || strings.Contains(low, "rpc") ||
		strings.Contains(low, "timed out") || strings.Contains(low, "can't contact") ||
		strings.Contains(low, "connection refused") || strings.Contains(low, "no route to host"):
		return "unreachable", "连不上 NFS 服务器（RPC/端口不可达）。", "确认地址能 ping 通、服务器上 nfsd 与 rpcbind 在跑、防火墙放行了 NFS 端口。"
	case strings.Contains(low, "permission denied") || strings.Contains(low, "not allowed") ||
		strings.Contains(low, "access denied"):
		return "denied", "服务器拒绝访问：导出未放行本机或权限不足。", "在服务器上核对导出对本机 IP 的 rw/ro 与 squash 设置。"
	case strings.Contains(low, "incompatible") || strings.Contains(low, "not supported") ||
		strings.Contains(low, "version") || strings.Contains(low, "protocol"):
		return "version", "NFS 版本不匹配（服务器不支持本机用的版本）。", "确认服务器支持的 NFS 版本并调整导出；本机按系统默认协商。"
	case strings.Contains(low, "operation not permitted"):
		return "denied", "系统拒绝了这次挂载（权限/隐私保护）。", "确认面板以 root 的 LaunchDaemon 运行，或当前用户有挂载权限。"
	case exit != 0:
		return "failed", "mount_nfs 失败（退出码 " + fmt.Sprint(exit) + "）：" + raw, "看下面的原样输出定位；改完点「重新挂载」。"
	default:
		return "failed", "挂载失败：" + raw, ""
	}
}
