// Package priv 实现 zizpanel-helper 的全部特权操作。
// 本包函数以 root 执行：每个入参都必须在这里重新做白名单校验，绝不假设面板已校验
// —— 这是"面板被攻破也不能提权"这条底线的落点。
package priv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------- 路径解析 ----------

// Root 返回面板安装根目录。支持 ZIZPANEL_ROOT 覆盖（测试与自定义安装）。
func Root() string {
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	return "/opt/zizpanel"
}

// HomebrewPrefix 探测 Homebrew 安装前缀。
func HomebrewPrefix() string {
	// 环境变量优先：没有它，任何"确保某个 brew 目录存在"的测试都会去写真实的
	// /opt/homebrew；**测试不许碰用户真实环境**（历史事故：真实 LaunchAgents plist 被覆盖成空）。
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_BREW_PREFIX")); v != "" {
		return v
	}
	if _, err := os.Stat("/opt/homebrew/bin/brew"); err == nil {
		return "/opt/homebrew"
	}
	return "/usr/local"
}

func NginxBin() string {
	return filepath.Join(HomebrewPrefix(), "bin", "nginx")
}

func NginxConf() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "nginx.conf")
}

func VhostDir() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "vhosts")
}

// NginxLaunchLabel 是 nginx 的 LaunchDaemon 标签（由 LNMP 安装脚本创建）。
const NginxLaunchLabel = "cn.zizdog.nginx"

// PanelLaunchLabel 是面板自己的 LaunchDaemon 标签。
const PanelLaunchLabel = "cn.zizpanel.panel"

var (
	// 域名必须是常规主机名；RE2 不支持负向前瞻，用"首尾字母数字"结构表达。
	reDomain = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)
	// vhost 文件名只允许安全字符，从根本上排除 ../ 之类穿越
	reSafeFile = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// 允许 @：Homebrew 的版本化服务名一律带它（php@8.3、mysql@8.4、node@20 …）。
	// 早期正则不含 @，状态检查直接报"label 含非法字符" —— 真机上 php/mysql 一律显示异常。
	reLabel = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)
	rePort  = regexp.MustCompile(`^\d{1,5}$`)
)

func ValidateDomain(d string) error {
	d = strings.TrimSpace(d)
	if d == "" {
		return errors.New("域名不能为空")
	}
	if len(d) > 253 {
		return errors.New("域名过长")
	}
	if !reDomain.MatchString(d) {
		return fmt.Errorf("域名格式不合法: %q", d)
	}
	return nil
}

// safeJoin 把用户提供的名称拼到受信目录下，并确保结果没有逃出该目录。
// 字符白名单与结果路径前缀校验缺一不可（符号链接可绕过前者）。
// macOS 坑：/var、/tmp、/etc 是 /private/... 的软链接，两侧都要解析后再比前缀。
func safeJoin(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("名称不能为空")
	}
	if !reSafeFile.MatchString(name) {
		return "", fmt.Errorf("名称含非法字符: %q", name)
	}

	// 目录可能尚不存在（首次安装），所以解析失败时退回清净化路径
	realDir := resolvePath(dir)
	full := resolvePath(filepath.Join(realDir, name))

	if full != realDir && !strings.HasPrefix(full, realDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径越界: %q", name)
	}
	// 必须是目录的直接子项，不允许写入子目录
	if filepath.Dir(full) != realDir {
		return "", fmt.Errorf("不允许写入子目录: %q", name)
	}
	return full, nil
}

// resolvePath 尽力解析软链接；路径不存在时把已存在的父级解析后拼接。
func resolvePath(p string) string {
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolvePath(parent), filepath.Base(p))
}

// ---------- 命令执行 ----------

type cmdResult struct {
	stdout string
	stderr string
	err    error
}

func run(name string, args ...string) cmdResult {
	cmd := exec.Command(name, args...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	return cmdResult{stdout: so.String(), stderr: se.String(), err: err}
}

// runFn 是所有外部命令的注入口（默认就是 run）：launchd 域解析与"操作后验证"
// 必须能脱离真实 launchctl 测试，否则只能去真机停一个真实服务来证明不谎报成功
// （同 internal/services/ready.go 的 readyWaitPort，生产路径不变）。
var runFn = run

func runTimeout(d time.Duration, name string, args ...string) cmdResult {
	cmd := exec.Command(name, args...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return cmdResult{err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return cmdResult{stdout: so.String(), stderr: se.String(), err: err}
	case <-time.After(d):
		_ = cmd.Process.Kill()
		return cmdResult{stderr: se.String(), err: fmt.Errorf("命令执行超时: %s", name)}
	}
}

func (r cmdResult) combined() string {
	out := strings.TrimSpace(r.stdout + "\n" + r.stderr)
	return strings.TrimSpace(out)
}

// ---------- nginx ----------

// nginxPIDAlive 判断 nginx 是否存活。
// 必须多方式判断：nginx 会把进程标题改成 "nginx: master process ..."，
// `pgrep -x nginx` 精确匹配必然失败（旧脚本"80 端口占用却杀不掉"的根因）。
func nginxPIDAlive() bool {
	pidFile := filepath.Join(HomebrewPrefix(), "var", "run", "nginx.pid")
	if b, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			if syscall.Kill(pid, 0) == nil {
				return true
			}
		}
	}
	for _, pat := range []string{`^nginx: master`, `^nginx: worker`} {
		if r := run("/usr/bin/pgrep", "-f", pat); r.err == nil && strings.TrimSpace(r.stdout) != "" {
			return true
		}
	}
	return false
}

// NginxStatus 返回 "running" 或 "stopped"。
func NginxStatus() (string, error) {
	if nginxPIDAlive() {
		return "running", nil
	}
	return "stopped", nil
}

// NginxTest 校验配置。
func NginxTest() (string, error) {
	r := run(NginxBin(), "-c", NginxConf(), "-t")
	out := r.combined()
	if r.err != nil {
		return out, fmt.Errorf("nginx 配置检查未通过")
	}
	return out, nil
}

// NginxReload 重载配置；未运行时直接启动。
func NginxReload() error {
	if nginxPIDAlive() {
		r := run(NginxBin(), "-c", NginxConf(), "-s", "reload")
		if r.err != nil {
			return fmt.Errorf("nginx 重载失败: %s", r.combined())
		}
		return nil
	}
	return NginxStart()
}

// NginxStart 启动 nginx。优先用 launchd（开机自启已配置），否则直接拉起。
func NginxStart() error {
	if nginxPIDAlive() {
		return nil
	}
	r := run("/bin/launchctl", "kickstart", "-k", "system/"+NginxLaunchLabel)
	if r.err == nil {
		if waitAlive(5 * time.Second) {
			return nil
		}
	}
	r = run(NginxBin(), "-c", NginxConf())
	if r.err != nil {
		return fmt.Errorf("nginx 启动失败: %s", r.combined())
	}
	if !waitAlive(5 * time.Second) {
		return errors.New("nginx 启动后未检测到进程，请检查 80 端口占用")
	}
	return nil
}

// NginxStop 优雅停止 nginx。
func NginxStop() error {
	if !nginxPIDAlive() {
		return nil
	}
	if r := run("/bin/launchctl", "bootout", "system/"+NginxLaunchLabel); r.err == nil {
		if waitDead(5 * time.Second) {
			return nil
		}
	}
	_ = run(NginxBin(), "-c", NginxConf(), "-s", "quit")
	if waitDead(5 * time.Second) {
		return nil
	}
	return nginxKill()
}

// NginxRestart 停止后启动。
func NginxRestart() error {
	if err := NginxStop(); err != nil {
		return err
	}
	return NginxStart()
}

// NginxHardRestart 强制清场后启动：用于残留进程导致 "Address already in use"。
func NginxHardRestart() error {
	_ = nginxKill()
	for i := 0; i < 20; i++ {
		if !nginxPIDAlive() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return NginxStart()
}

func nginxKill() error {
	for _, pat := range []string{`nginx: master`, `nginx: worker`} {
		_ = run("/usr/bin/pkill", "-9", "-f", pat)
	}
	_ = run("/usr/bin/pkill", "-9", "-x", "nginx")
	pidFile := filepath.Join(HomebrewPrefix(), "var", "run", "nginx.pid")
	if b, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if nginxPIDAlive() {
		return errors.New("无法终止 nginx 进程")
	}
	return nil
}

func waitAlive(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if nginxPIDAlive() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nginxPIDAlive()
}

func waitDead(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !nginxPIDAlive() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !nginxPIDAlive()
}

// ---------- vhost 文件 ----------

func ListVhosts() ([]string, error) {
	entries, err := os.ReadDir(VhostDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

func ReadVhost(name string) (string, error) {
	path, err := safeJoin(VhostDir(), vhostFileName(name))
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	return string(b), nil
}

// WriteVhostAtomic 原子写入 vhost：写临时文件 → 替换 → nginx -t 整体校验，不通过就回滚。
// 顺序至关重要：nginx -t 只看最终配置树，所以必须先替换再校验并保留回滚点。
func WriteVhostAtomic(name, content string) error {
	path, err := safeJoin(VhostDir(), vhostFileName(name))
	if err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("配置内容不能为空")
	}
	if err := os.MkdirAll(VhostDir(), 0o755); err != nil {
		return err
	}

	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()

	// nginx -t 只能校验最终配置树：先备份原文件、替换、校验，不通过就回滚。
	var backup []byte
	hadOld := false
	if old, err := os.ReadFile(path); err == nil {
		backup, hadOld = old, true
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换配置失败: %w", err)
	}
	if out, err := NginxTest(); err != nil {
		if hadOld {
			_ = os.WriteFile(path, backup, 0o644)
		} else {
			_ = os.Remove(path)
		}
		return fmt.Errorf("配置语法错误，已回滚：\n%s", out)
	}
	return nil
}

func DeleteVhost(name string) error {
	path, err := safeJoin(VhostDir(), vhostFileName(name))
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// vhostFileName 把域名（或已带 .conf 的名字）规范成文件名。
func vhostFileName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasSuffix(name, ".conf") {
		name += ".conf"
	}
	return name
}

// ---------- hosts ----------

const hostsTag = "zizpanel-local"

// HostsAdd 添加本地解析（幂等）。
func HostsAdd(domain string) error {
	if err := ValidateDomain(domain); err != nil {
		return err
	}
	hosts, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	if regexp.MustCompile(`(?m)^\s*[^#\n]*\s` + regexp.QuoteMeta(domain) + `(\s|$)`).Match(hosts) {
		return nil
	}
	line := fmt.Sprintf("127.0.0.1\t%s\t# %s\n", domain, hostsTag)
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	return flushDNS()
}

// HostsDel 删除由本面板添加的解析（只删带标记的行）。
func HostsDel(domain string) error {
	if err := ValidateDomain(domain); err != nil {
		return err
	}
	hosts, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	var out []string
	re := regexp.MustCompile(`^\s*#?\s*127\.0\.0\.1\s+` + regexp.QuoteMeta(domain) + `\s+#\s*` + hostsTag + `\s*$`)
	for _, ln := range strings.Split(string(hosts), "\n") {
		if re.MatchString(ln) {
			continue // 丢弃
		}
		out = append(out, ln)
	}
	if err := writeHosts(strings.Join(out, "\n")); err != nil {
		return err
	}
	return flushDNS()
}

// HostsToggle 批量启用/停用由本面板添加的解析（注释/取消注释，而不是删除）。
// 保留域名列表，用户从"本地测试"切回"访问线上"时不必重新添加。
func HostsToggle(on bool) error {
	hosts, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	next, changed := toggleHostsContent(string(hosts), on)
	if !changed {
		return nil
	}
	if err := writeHosts(next); err != nil {
		return err
	}
	return flushDNS()
}

// toggleHostsContent 是 HostsToggle 的纯函数部分，便于单元测试。
// 只处理带 zizpanel-local 标记的行，绝不触碰用户自己写的解析。
func toggleHostsContent(content string, on bool) (string, bool) {
	lines := strings.Split(content, "\n")
	tagged := regexp.MustCompile(`^(\s*#\s*)?(127\.0\.0\.1\s+[^\s#]+\s+#\s*` + hostsTag + `\s*)$`)
	changed := false
	for i, ln := range lines {
		m := tagged.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		if on {
			if m[1] != "" { // 当前被注释
				lines[i] = m[2]
				changed = true
			}
		} else if m[1] == "" { // 当前是启用状态
			lines[i] = "# " + m[2]
			changed = true
		}
	}
	return strings.Join(lines, "\n"), changed
}

// writeHosts 原子写 /etc/hosts：直接截断写入一旦断电会留下空 hosts，系统解析立刻出问题。
// 因此先写临时文件再 rename。
func writeHosts(content string) error {
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	tmp := "/etc/hosts.zizpanel.tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if st, err := os.Stat("/etc/hosts"); err == nil {
		_ = os.Chmod(tmp, st.Mode().Perm())
	}
	return os.Rename(tmp, "/etc/hosts")
}

func flushDNS() error {
	_ = run("/usr/bin/dscacheutil", "-flushcache")
	_ = run("/usr/bin/killall", "-HUP", "mDNSResponder")
	return nil
}

// ---------- launchd ----------

// LaunchState 描述一个 launchd 任务的状态。
// 字段只增不减（面板与 helper 的 JSON 契约）。
// ProbedDomains 是本次实际探测过的域，用于诊断"为什么状态说未加载"。
type LaunchState struct {
	Label         string   `json:"label"`
	Loaded        bool     `json:"loaded"`
	Running       bool     `json:"running"`
	PID           int      `json:"pid"`
	ExitCode      int      `json:"exit_code"`
	Domain        string   `json:"domain"`
	ProbedDomains []string `json:"probed_domains,omitempty"`
}

// launchctlBin 返回 launchctl 的可执行路径。
// 支持 ZIZPANEL_LAUNCHCTL 覆盖：端到端测试要把整条通路指到假 launchctl 上，
// 才能验证"底层失败时 HTTP 不许返回 2xx"；生产默认值永远是系统路径。
func launchctlBin() string {
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_LAUNCHCTL")); v != "" {
		return v
	}
	return "/bin/launchctl"
}

// launchctl 通过 runFn 执行一次 launchctl 调用（便于单测注入）。
func launchctl(args ...string) cmdResult {
	return runFn(launchctlBin(), args...)
}

var (
	// userHomesFn 返回所有用户主目录（默认读 /Users）。
	userHomesFn = userHomes
	// uidOfHomeFn 通过 home 目录反查 uid。
	uidOfHomeFn = uidOfHome
	// fileExistsFn 是 os.Stat 的布尔包装（测试可脱离真实 plist 文件）。
	fileExistsFn = func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}
	// launchDomainsFn 返回 label 的候选域（默认按 plist 位置推断）。
	// 抽成变量：单测要能锁死"user/501 与 gui/501 都探测"，不能依赖真实 plist。
	launchDomainsFn = launchDomainCandidates
)

// launchDomainCandidates 返回该 label 可能所在的 launchd 域，按优先级排序：必须同时认
// user/<uid> 与 gui/<uid>（user 在前），找不到 plist 时也探测全部候选域 —— plist 被删但作业
// 还挂着是真实情况（真机事故 0.12.9：落在 user/501 的作业被旧代码按 gui/501 打偏 → 谎报成功）。
func launchDomainCandidates(label string) []string {
	if fileExistsFn(filepath.Join("/Library/LaunchDaemons", label+".plist")) {
		return []string{"system"}
	}
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}
	foundPlist := false
	for _, home := range userHomesFn() {
		if !fileExistsFn(filepath.Join(home, "Library", "LaunchAgents", label+".plist")) {
			continue
		}
		foundPlist = true
		uid := uidOfHomeFn(home)
		add("user/" + uid)
		add("gui/" + uid)
	}
	if foundPlist {
		return out
	}
	// 没有 plist：system 优先，然后各用户的 user/gui 域。
	add("system")
	for _, home := range userHomesFn() {
		uid := uidOfHomeFn(home)
		add("user/" + uid)
		add("gui/" + uid)
	}
	if len(out) == 0 {
		add("system")
	}
	return out
}

// launchQuery 是一次 `launchctl print <domain>/<label>` 的结论。
type launchQuery struct {
	state LaunchState
	// found=true：该域里确实有这个作业。
	found bool
	// inconclusive=true：查询失败且原因不是"没有这个作业"（权限不足、域不支持该动作…）
	// —— **绝不能当成"未加载"**。
	inconclusive bool
	// detail 是无法判定时的原始输出，用于如实上报。
	detail string
}

func queryLaunchDomain(domain, label string) launchQuery {
	base := LaunchState{Label: label, Domain: domain, ExitCode: -1}
	r := launchctl("print", domain+"/"+label)
	if r.err == nil {
		base.Loaded = true
		parseLaunchPrint(&base, r.stdout)
		return launchQuery{state: base, found: true}
	}
	out := r.combined()
	if out == "" && r.err != nil {
		out = r.err.Error()
	}
	if launchOutputSaysMissing(out) {
		return launchQuery{state: base}
	}
	return launchQuery{state: base, inconclusive: true, detail: out}
}

// parseLaunchPrint 解析 `launchctl print` 输出（state / pid / last exit code）。
func parseLaunchPrint(st *LaunchState, stdout string) {
	for _, ln := range strings.Split(stdout, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "pid = "):
			if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "pid = "))); err == nil {
				st.PID = v
				st.Running = true
			}
		case strings.HasPrefix(ln, "state = "):
			if strings.TrimSpace(strings.TrimPrefix(ln, "state = ")) == "running" {
				st.Running = true
			}
		case strings.HasPrefix(ln, "last exit code = "):
			if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "last exit code = "))); err == nil {
				st.ExitCode = v
			}
		}
	}
}

// launchOutputSaysMissing 判断 launchctl 输出是否**明确**表示"这个域里没有该作业"。
// 真机实测：只有 `Could not find service … user gui: 501`（113）与 `Could not find domain
// … user gui: 999`（112）算"未加载"；权限/域能力类错误必须当"无法判定"（否则谎报成功）。
func launchOutputSaysMissing(out string) bool {
	low := strings.ToLower(out)
	if strings.TrimSpace(low) == "" {
		return false
	}
	for _, s := range []string{
		"could not find service",
		"service not found",
		"could not find specified service",
		"no such process",
	} {
		if strings.Contains(low, s) {
			return true
		}
	}
	// 域本身不存在也算"没有"：必须**同时**匹配 "could not print domain" + "domain does
	// not support specified action"（125；headless 机器上 gui/<uid> 即如此）。单独的
	// "Operation not permitted" 是权限问题，必须如实报错、不许混成"没有"。
	if strings.Contains(low, "could not find domain") {
		return true
	}
	return strings.Contains(low, "could not print domain") &&
		strings.Contains(low, "domain does not support specified action")
}

// launchResolve 按优先级探测候选域，返回作业真正所在域的状态。
// (state, nil) 且 Loaded=false：所有候选域都成功查过，确实没有这个作业。
// error：至少一个候选域给不出确定答复（权限/域能力问题），绝不许假装"未加载"。
func launchResolve(label string, domains []string) (LaunchState, error) {
	var inconclusive []string
	for _, d := range domains {
		q := queryLaunchDomain(d, label)
		if q.found {
			st := q.state
			st.ProbedDomains = domains
			// 兜底（实测）：`launchctl print` 对按需拉起的服务可能不给 pid 行
			// （php-fpm 报 `state = spawn scheduled`），但 `launchctl list` 里有 PID —— 少了会误判"未运行"。
			if !st.Running {
				if pid := pidFromLaunchctlList(label); pid > 0 {
					st.PID = pid
					st.Running = true
				}
			}
			return st, nil
		}
		if q.inconclusive {
			inconclusive = append(inconclusive, d+": "+q.detail)
		}
	}
	st := LaunchState{Label: label, ExitCode: -1, ProbedDomains: domains}
	if len(domains) > 0 {
		st.Domain = domains[0]
	}
	if len(inconclusive) > 0 {
		return st, fmt.Errorf("无法查询 %s 的 launchd 状态（%s），不能当成未加载",
			label, strings.Join(inconclusive, "；"))
	}
	return st, nil
}

// LaunchStatus 查询任务状态。
// "查不到"与"查不了"必须分开：把任何查询错误都当 Loaded=false 就是谎报成功的根源。
func LaunchStatus(label string) (LaunchState, error) {
	if !reLabel.MatchString(label) {
		return LaunchState{}, fmt.Errorf("label 含非法字符: %q", label)
	}
	return launchResolve(label, launchDomainsFn(label))
}

// pidFromLaunchctlList 从 `launchctl list` 取 label 的 PID（三列，未运行时 PID 为 "-"）。
func pidFromLaunchctlList(label string) int {
	r := launchctl("list")
	if r.err != nil {
		return 0
	}
	for _, ln := range strings.Split(r.stdout, "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 || f[2] != label {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			return pid
		}
	}
	return 0
}

// LaunchLoad 加载并启动任务。已加载在任一候选域 → kickstart 到**那个**域；
// 未加载 → 按候选顺序 bootstrap（user/<uid> 在前、gui/<uid> 兜底），
// 且每次 bootstrap 后必须用**新查询**确认真的加载了。
func LaunchLoad(label string) error {
	if !reLabel.MatchString(label) {
		return fmt.Errorf("label 含非法字符: %q", label)
	}
	plist, err := findPlist(label)
	if err != nil {
		return err
	}
	domains := launchDomainsFn(label)

	st, rerr := launchResolve(label, domains)
	if rerr != nil {
		return fmt.Errorf("加载 %s 失败：%v", label, rerr)
	}
	if st.Loaded {
		return launchKick(label, st.Domain)
	}

	var errs []string
	for _, d := range domains {
		r := launchctl("bootstrap", d, plist)
		if r.err != nil {
			errs = append(errs, fmt.Sprintf("bootstrap %s: %s", d, r.combined()))
			continue
		}
		// 命令成功不等于真的加载：必须复核终态。
		after, verr := launchResolve(label, domains)
		if verr != nil {
			return fmt.Errorf("加载 %s 失败：bootstrap %s 返回成功，但无法确认状态（%v）",
				label, d, verr)
		}
		if !after.Loaded {
			return fmt.Errorf("加载 %s 失败：bootstrap %s 返回成功，但 launchd 里查不到该作业", label, d)
		}
		return nil
	}
	if len(errs) == 0 {
		errs = append(errs, "没有可用的 launchd 域")
	}
	return fmt.Errorf("加载 %s 失败: %s", label, strings.Join(errs, "；"))
}

// bootout 之后的等待参数。`launchctl bootout` 是**异步**的：命令返回后作业可能还在
// 卸载中（print 仍能看到 state = SIGTERMed），不等待就会把成功的停止报成失败。
// 抽成变量是为单测（hermetic 测试设 0）。
var (
	launchUnloadWait = 3 * time.Second
	launchUnloadPoll = 250 * time.Millisecond
)

// waitLaunchGone 在 launchUnloadWait 内轮询，直到作业从该域消失。
// gone=false → 超时仍在，或查询无法判定（last 里带着原因）。
func waitLaunchGone(domain, label string) (bool, launchQuery) {
	deadline := time.Now().Add(launchUnloadWait)
	for {
		q := queryLaunchDomain(domain, label)
		if !q.found && !q.inconclusive {
			return true, q
		}
		if !time.Now().Before(deadline) {
			return false, q
		}
		time.Sleep(launchUnloadPoll)
	}
}

// LaunchUnload 停止并卸载任务。
// 关键不变量：只有**成功查询**确认该作业已从所有候选域消失后才返回 nil
// —— 旧实现把"查询失败"当"未加载"，于是什么都没做也报成功。
func LaunchUnload(label string) error {
	if !reLabel.MatchString(label) {
		return fmt.Errorf("label 含非法字符: %q", label)
	}
	domains := launchDomainsFn(label)

	st, err := launchResolve(label, domains)
	if err != nil {
		return fmt.Errorf("停止 %s 失败：%v", label, err)
	}
	if !st.Loaded {
		// 经成功查询确认本来就没加载：幂等的空操作成功。
		return nil
	}

	// 逐个卸载**真正加载了它**的域并等待消失（旧代码只打一个域，打偏就什么都不做还报成功）。
	var failures []string
	for _, d := range domains {
		q := queryLaunchDomain(d, label)
		if q.inconclusive {
			failures = append(failures, fmt.Sprintf("无法查询 %s: %s", d, q.detail))
			continue
		}
		if !q.found {
			continue
		}
		r := launchctl("bootout", d+"/"+label)
		gone, last := waitLaunchGone(d, label)
		if gone {
			continue
		}
		switch {
		case last.inconclusive:
			failures = append(failures,
				fmt.Sprintf("bootout %s/%s 后无法确认状态: %s", d, label, last.detail))
		case r.err != nil:
			failures = append(failures, fmt.Sprintf("bootout %s/%s: %s", d, label, r.combined()))
		default:
			failures = append(failures, fmt.Sprintf("%s/%s 在 bootout 后仍存在", d, label))
		}
	}

	// 终态验证：每个候选域都必须查不到它，才算真的停掉。
	for _, d := range domains {
		q := queryLaunchDomain(d, label)
		if q.inconclusive {
			return fmt.Errorf("停止 %s 失败：无法确认 %s 里是否还有该作业（%s）", label, d, q.detail)
		}
		if q.found {
			pid := ""
			if q.state.PID > 0 {
				pid = fmt.Sprintf("（pid %d）", q.state.PID)
			}
			return fmt.Errorf("停止 %s 失败：它仍加载在 %s%s", label, d, pid)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("停止 %s 失败: %s", label, strings.Join(failures, "；"))
	}
	return nil
}

// LaunchKickstart 重启任务（-k 表示先杀掉正在运行的实例）。
func LaunchKickstart(label string) error {
	if !reLabel.MatchString(label) {
		return fmt.Errorf("label 含非法字符: %q", label)
	}
	domains := launchDomainsFn(label)
	st, err := launchResolve(label, domains)
	if err != nil {
		return fmt.Errorf("重启 %s 失败：%v", label, err)
	}
	if st.Loaded {
		return launchKick(label, st.Domain)
	}
	// 未加载时 kickstart 一定失败：先 bootstrap（RunAtLoad 的作业同时被拉起）。
	// 已经跑起来就不要再 kick —— 对刚启动的作业再 kick 会撞 launchd 的 10s
	// 节流窗口，实测卡 10.0s（坑 225）；只有确实没跑起来时才补一次。
	if lerr := LaunchLoad(label); lerr != nil {
		return lerr
	}
	after, rerr := launchResolve(label, domains)
	if rerr != nil {
		return fmt.Errorf("重启 %s 失败：加载后无法确认状态（%v）", label, rerr)
	}
	if !after.Loaded {
		return fmt.Errorf("重启 %s 失败：加载后 launchd 里仍查不到该作业", label)
	}
	if after.Running {
		return nil
	}
	return launchKick(label, after.Domain)
}

// LaunchEnsureRunning 确保任务在运行：已在跑就**空操作**，未加载才 bootstrap，
// 已加载但没跑才 kickstart 一次。
//
// 不能直接用 LaunchLoad：「启动」一个正在跑的服务时它会 kickstart -k 把服务
// 白杀一次（实测 pid 变了），刚起过的还会撞 launchd 的 10s 节流窗口（坑 225）。
func LaunchEnsureRunning(label string) error {
	if !reLabel.MatchString(label) {
		return fmt.Errorf("label 含非法字符: %q", label)
	}
	domains := launchDomainsFn(label)
	st, err := launchResolve(label, domains)
	if err != nil {
		return fmt.Errorf("启动 %s 失败：%v", label, err)
	}
	if !st.Loaded {
		return LaunchLoad(label) // 未加载：bootstrap，RunAtLoad 会拉起它
	}
	if st.Running {
		return nil // 已在跑：启动是幂等空操作，绝不白踢
	}
	return launchKick(label, st.Domain)
}

// launchKick 在指定域里 kickstart 并复核。
func launchKick(label, domain string) error {
	r := launchctl("kickstart", "-k", domain+"/"+label)
	if r.err != nil {
		return fmt.Errorf("重启 %s 失败: %s", label, r.combined())
	}
	// 命令成功不等于真的生效：kickstart 之后它必须仍加载在该域里。
	q := queryLaunchDomain(domain, label)
	if q.inconclusive {
		return fmt.Errorf("重启 %s 后无法确认状态（%s）", label, q.detail)
	}
	if !q.found {
		return fmt.Errorf("重启 %s 后 launchd 里查不到该作业（域 %s）", label, domain)
	}
	return nil
}

func findPlist(label string) (string, error) {
	sys := filepath.Join("/Library/LaunchDaemons", label+".plist")
	if fileExistsFn(sys) {
		return sys, nil
	}
	for _, home := range userHomesFn() {
		p := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
		if fileExistsFn(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到 plist 文件: %s", label)
}

func userHomes() []string {
	entries, err := os.ReadDir("/Users")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "Shared" {
			continue
		}
		out = append(out, filepath.Join("/Users", e.Name()))
	}
	return out
}

func uidOfHome(home string) string {
	user := filepath.Base(home)
	r := run("/usr/bin/id", "-u", user)
	if r.err == nil {
		return strings.TrimSpace(r.stdout)
	}
	return "501"
}

// ---------- 端口 ----------

type PortInfo struct {
	Port    int      `json:"port"`
	InUse   bool     `json:"in_use"`
	Holders []string `json:"holders"`
}

func CheckPort(portArg string) (PortInfo, error) {
	if !rePort.MatchString(portArg) {
		return PortInfo{}, fmt.Errorf("非法端口: %q", portArg)
	}
	p, _ := strconv.Atoi(portArg)
	if p < 1 || p > 65535 {
		return PortInfo{}, fmt.Errorf("端口超出范围: %d", p)
	}
	info := PortInfo{Port: p}
	r := run("/usr/sbin/lsof", "-nP", fmt.Sprintf("-iTCP:%d", p), "-sTCP:LISTEN")
	// lsof 无匹配时退出码为 1，这是正常的"端口空闲"
	if strings.TrimSpace(r.stdout) == "" {
		return info, nil
	}
	info.InUse = true
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	for _, ln := range lines[1:] {
		f := strings.Fields(ln)
		if len(f) >= 2 {
			info.Holders = append(info.Holders, f[0]+" (pid "+f[1]+")")
		}
	}
	return info, nil
}

// ---------- 防火墙 ----------

func FirewallState() (string, error) {
	r := run("/usr/libexec/ApplicationFirewall/socketfilterfw", "--getglobalstate")
	if r.err != nil {
		return "", fmt.Errorf("读取防火墙状态失败: %s", r.combined())
	}
	return strings.TrimSpace(r.stdout), nil
}

// FirewallOpenPort 尝试放行端口。
// macOS 应用防火墙（ALF）按"应用"而非端口放行，socketfilterfw 没有开端口的能力
// —— 这里返回明确提示，不假装成功。
func FirewallOpenPort(portArg string) (string, error) {
	if !rePort.MatchString(portArg) {
		return "", fmt.Errorf("非法端口: %q", portArg)
	}
	st, err := FirewallState()
	if err != nil {
		return "", err
	}
	if strings.Contains(st, "disabled") {
		return "防火墙未开启，端口无需放行", nil
	}
	return "macOS 应用防火墙按应用授权，不支持按端口放行；请用 firewall-ensure-app 添加应用，或在「系统设置 → 网络 → 防火墙 → 选项」中允许 ZizPanel", nil
}

// EnsureFirewallApp 把可执行文件包装成 .app 并在防火墙中放行。
// macOS 应用防火墙只认 .app bundle 或已签名二进制，直接添加 CLI 会持续弹窗或在更新后失效。
func EnsureFirewallApp(binPath, appName, appsDir string) (string, error) {
	if !filepath.IsAbs(binPath) {
		return "", fmt.Errorf("必须是绝对路径: %s", binPath)
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("可执行文件不存在: %s", binPath)
	}
	if appName == "" || strings.ContainsAny(appName, `/\`) {
		return "", fmt.Errorf("非法应用名: %q", appName)
	}
	if appsDir == "" {
		appsDir = "/Applications"
	}
	if !filepath.IsAbs(appsDir) {
		return "", fmt.Errorf("App 目录必须是绝对路径: %s", appsDir)
	}
	appName = strings.TrimSuffix(appName, ".app")

	st, err := FirewallState()
	if err != nil {
		return "", err
	}
	if strings.Contains(st, "disabled") {
		return "防火墙未开启，跳过应用放行", nil
	}

	appDir := filepath.Join(appsDir, appName+".app")
	macOSDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macOSDir, 0o755); err != nil {
		return "", fmt.Errorf("创建 App 目录失败: %w", err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>` + appName + `</string>
  <key>CFBundleExecutable</key><string>` + appName + `</string>
  <key>CFBundleIdentifier</key><string>cn.zizpanel.panel</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>LSBackgroundOnly</key><true/>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(appDir, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		return "", err
	}
	// 复制而不是软链：防火墙按 inode/签名记录，软链在更新后会失配
	dest := filepath.Join(macOSDir, appName)
	if err := copyFile(binPath, dest, 0o755); err != nil {
		return "", fmt.Errorf("复制可执行文件失败: %w", err)
	}
	// ad-hoc 重新签名；失败不算致命（未签名的 App 仍可被手动允许）
	signOut := ""
	if r := run("/usr/bin/codesign", "--force", "--deep", "--sign", "-", appDir); r.err != nil {
		signOut = "（ad-hoc 签名失败，可能需要在系统设置中手动允许）"
	}

	r := run("/usr/libexec/ApplicationFirewall/socketfilterfw", "--add", appDir)
	if r.err != nil {
		return "", fmt.Errorf("添加应用到防火墙失败: %s", r.combined())
	}
	_ = run("/usr/libexec/ApplicationFirewall/socketfilterfw", "--unblockapp", appDir)

	return fmt.Sprintf("已添加 %s 到防火墙并放行%s", appDir, signOut), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// ---------- nginx 环境自愈 ----------

func NginxConfD() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "conf.d")
}

// NginxIncludesDir 返回存放"只能在特定上下文使用"的配置片段目录。
// conf.d/*.conf 被 http 级 include，含 fastcgi_pass 的片段留在那里会让 nginx -t 直接失败；
// 约定：conf.d/ 放 http 上下文配置，includes/ 放被 vhost/location 显式 include 的片段。
func NginxIncludesDir() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "includes")
}

// LegacyFPMIncludePath 是历史遗留的 PHP-FPM 片段路径（在 conf.d 下，上下文错误）。
func LegacyFPMIncludePath() string {
	return filepath.Join(NginxConfD(), "php-fpm.conf")
}

// MigrateFPMInclude 把 conf.d/php-fpm.conf 迁到 includes/php-fpm.conf，并改写所有引用。
// 幂等；返回是否发生了迁移及具体做了什么。
func MigrateFPMInclude() (bool, string, error) {
	legacy := LegacyFPMIncludePath()
	newPath := filepath.Join(NginxIncludesDir(), "php-fpm.conf")

	_, legacyErr := os.Stat(legacy)
	_, newErr := os.Stat(newPath)

	if os.IsNotExist(legacyErr) {
		if newErr == nil {
			n, err := rewriteFPMReferences(newPath)
			if err != nil {
				return false, "", err
			}
			return n > 0, fmt.Sprintf("PHP 片段已在 includes/，改写了 %d 处引用", n), nil
		}
		return false, "", nil // 两者都不存在，无需处理
	}

	if err := os.MkdirAll(NginxIncludesDir(), 0o755); err != nil {
		return false, "", err
	}
	body, err := os.ReadFile(legacy)
	if err != nil {
		return false, "", err
	}
	if err := os.WriteFile(newPath, body, 0o644); err != nil {
		return false, "", err
	}
	// 删掉旧文件：留在 conf.d 会导致 nginx 无法通过语法校验
	if err := os.Remove(legacy); err != nil {
		return false, "", fmt.Errorf("删除旧 php-fpm.conf 失败: %w", err)
	}
	n, err := rewriteFPMReferences(newPath)
	if err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("已把 PHP-FPM 片段迁移到 includes/，改写 %d 处引用", n), nil
}

// rewriteFPMReferences 把所有配置文件里对旧 php-fpm.conf 路径的引用改为新路径。
// 只处理 nginx 配置目录内的文件，不碰其它任何地方。
func rewriteFPMReferences(newPath string) (int, error) {
	oldRef := LegacyFPMIncludePath()
	root := filepath.Join(HomebrewPrefix(), "etc", "nginx")
	changed := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".conf") {
			return nil
		}
		if strings.HasPrefix(path, NginxIncludesDir()) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(b)
		if !strings.Contains(content, oldRef) {
			return nil
		}
		next := strings.ReplaceAll(content, oldRef, newPath)
		if err := os.WriteFile(path, []byte(next), info.Mode().Perm()); err != nil {
			return fmt.Errorf("改写引用失败 %s: %w", path, err)
		}
		changed++
		return nil
	})
	return changed, err
}

// EnsureNginxContexts 做一次完整的上下文整理：迁移上下文敏感的 php-fpm.conf、
// 写入 upgrade map、确保 conf.d 被 include。
// 顺序不能颠倒：必须先迁移再加 include，否则中间会经过一个 nginx 语法非法的状态。
func EnsureNginxContexts(upgradeMapContent string) (string, error) {
	migrated, msg, err := MigrateFPMInclude()
	if err != nil {
		return "", err
	}
	parts := []string{}
	if msg != "" {
		parts = append(parts, msg)
	}
	// nginx 的工作目录必须存在，否则**任何**配置校验都会失败（真机实测 2026-09-16：
	// 全新安装的 nginx 没有 <brew>/var/log/nginx，`nginx -t` 报 could not open error
	// log file，反代规则写不进去、用户只看到"规则保存不了"）；这些目录由面板补齐并改归属。
	if logMsg, lerr := ensureNginxRuntimeDirs(); lerr != nil {
		return "", lerr
	} else if logMsg != "" {
		parts = append(parts, logMsg)
	}
	before, _, _ := ConfDIncluded()
	beforeVhost, _, _ := VhostsIncluded()
	beforeMap, _ := os.ReadFile(filepath.Join(NginxConfD(), "upgrade-map.conf"))
	if err := EnsureUpgradeMap(upgradeMapContent); err != nil {
		return "", err
	}
	if err := EnsureVhostsInclude(); err != nil {
		return "", err
	}
	// worker_processes 不能是 1：一个请求卡住就拖垮全部站点（2026-09-20 外置盘事故）。
	workerFixed, werr := EnsureNginxWorkerProcesses()
	if werr != nil {
		return "", werr
	}
	if workerFixed {
		parts = append(parts, "worker_processes 已从 1 改为 auto（单个请求卡住不再拖垮所有站点）")
	}
	after, _, _ := ConfDIncluded()
	afterVhost, _, _ := VhostsIncluded()
	afterMap, _ := os.ReadFile(filepath.Join(NginxConfD(), "upgrade-map.conf"))
	changed := migrated || before != after || beforeVhost != afterVhost || string(beforeMap) != string(afterMap) || workerFixed

	if !changed {
		return "nginx 环境已就绪（无变化，未重载）", nil
	}
	parts = append(parts, "nginx 环境已更新（WebSocket 升级支持 + conf.d 加载）")
	return strings.Join(parts, "；"), nil
}

// EnsureUpgradeMap 写入 WebSocket 升级所需的 map 片段，并确保主配置 include 了 conf.d。
// $connection_upgrade 只能在 http 上下文用 map 定义，缺失时 nginx -t 报 unknown variable、
// 所有反代站点都无法启用；本函数幂等，不覆盖用户对 map 的修改。
func EnsureUpgradeMap(mapContent string) error {
	confD := NginxConfD()
	if err := os.MkdirAll(confD, 0o755); err != nil {
		return fmt.Errorf("创建 conf.d 目录失败: %w", err)
	}
	mapPath := filepath.Join(confD, "upgrade-map.conf")

	// 只在内容真的变化时才写入：本函数每次启动都被调用，无条件写 + 写后重载
	// 会在每次开机触发一次全站 reload（没必要，且可能造成请求抖动）。
	changed := true
	if old, err := os.ReadFile(mapPath); err == nil && string(old) == mapContent {
		changed = false
	}
	if changed {
		if err := os.WriteFile(mapPath, []byte(mapContent), 0o644); err != nil {
			return fmt.Errorf("写入 upgrade-map.conf 失败: %w", err)
		}
	}
	if err := ensureConfDIncluded(); err != nil {
		return err
	}
	return nil
}

// ensureConfDIncluded：nginx.conf 的 http 块若没 include conf.d/*.conf 就插入一次。
func ensureConfDIncluded() error {
	confPath := NginxConf()
	b, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("读取 nginx.conf 失败: %w", err)
	}
	content := string(b)

	if regexp.MustCompile(`(?m)^\s*include\s+\S*conf\.d/\*\.conf\s*;`).MatchString(content) {
		return nil
	}

	// 找到 http { 的位置：必须是行首的 http 块（避免匹配到注释里的 http）
	re := regexp.MustCompile(`(?m)^([ \t]*)http\s*\{`)
	loc := re.FindStringSubmatchIndex(content)
	if loc == nil {
		return errors.New("nginx.conf 中未找到 http 块，无法插入 conf.d include")
	}
	insertAt := loc[1]
	line := "\n    # 由 ZizPanel 添加：加载 conf.d 下的通用片段（如 WebSocket 升级 map）\n" +
		"    include " + NginxConfD() + "/*.conf;\n"

	backup := confPath + ".zizpanel.bak"
	if err := os.WriteFile(backup, b, 0o644); err != nil {
		return fmt.Errorf("备份 nginx.conf 失败: %w", err)
	}
	next := content[:insertAt] + line + content[insertAt:]

	tmp := confPath + ".zizpanel.tmp"
	if err := os.WriteFile(tmp, []byte(next), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, confPath); err != nil {
		return err
	}

	// 立即校验；失败就回滚，绝不留下一个起不来的 nginx
	if out, err := NginxTest(); err != nil {
		_ = os.WriteFile(confPath, b, 0o644)
		return fmt.Errorf("加入 conf.d include 后 nginx 配置校验失败，已回滚：\n%s", out)
	}
	return nil
}

func NginxConfBackupPath() string { return NginxConf() + ".zizpanel.bak" }

// reWorkerProcesses 只匹配 main 上下文的 worker_processes 指令（非注释行）。
var reWorkerProcesses = regexp.MustCompile(`(?m)^(\s*worker_processes\s+)([^;\n]+)(;)`)

// NormalizeWorkerProcesses 把 `worker_processes 1;` 纠正为 auto（幂等）。
// 坑 210：唯一 worker 被一个卡死的请求占住 = 全站（含别的站点）一起超时。
func NormalizeWorkerProcesses(text string) (string, bool) {
	changed := false
	out := reWorkerProcesses.ReplaceAllStringFunc(text, func(m string) string {
		sub := reWorkerProcesses.FindStringSubmatch(m)
		if sub == nil || strings.TrimSpace(sub[2]) != "1" {
			return m
		}
		changed = true
		return sub[1] + "auto" + sub[3]
	})
	return out, changed
}

// EnsureNginxWorkerProcesses 修 nginx.conf 里的 worker_processes=1：备份 → 改写 →
// `nginx -t` → 失败回滚。幂等：不是 1 就一个字节都不写。
func EnsureNginxWorkerProcesses() (bool, error) {
	path := NginxConf()
	before, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("读取 nginx.conf 失败: %w", err)
	}
	next, changed := NormalizeWorkerProcesses(string(before))
	if !changed {
		return false, nil
	}
	if _, serr := os.Stat(NginxConfBackupPath()); serr != nil {
		_ = os.WriteFile(NginxConfBackupPath(), before, 0o644)
	}
	tmp := path + ".zizpanel.tmp"
	if werr := os.WriteFile(tmp, []byte(next), 0o644); werr != nil {
		return false, fmt.Errorf("写入 nginx.conf 失败: %w", werr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		return false, fmt.Errorf("替换 nginx.conf 失败: %w", rerr)
	}
	// 立即校验；失败就回滚，绝不留下一个起不来的 nginx
	if out, terr := NginxTest(); terr != nil {
		_ = os.WriteFile(path, before, 0o644)
		return false, fmt.Errorf("worker_processes 改为 auto 后 nginx -t 未通过，已回滚：\n%s", out)
	}
	return true, nil
}

// EnsureNginxEnv 确保 nginx 具备面板所需的通用环境（upgrade map + conf.d include）。
// 返回人类可读的执行说明，便于面板记录做了什么。
func EnsureNginxEnv() (string, error) {
	return EnsureNginxContexts(nginxUpgradeMapContent)
}

// NginxEnvChanged 判断返回值是否表示发生了实际改动（调用方据此决定是否 reload）。
func NginxEnvChanged(msg string) bool {
	return strings.Contains(msg, "已更新") || strings.Contains(msg, "已把")
}

// nginxUpgradeMapContent 由主程序在启动时通过 SetUpgradeMapContent 注入，
// 避免 priv 包反向依赖 sites 包（helper 也需要这个内容，但 helper 只依赖 priv）。
var nginxUpgradeMapContent = "map $http_upgrade $connection_upgrade {\n    default upgrade;\n    ''      close;\n}\n"

// SetUpgradeMapContent 注入 upgrade map 的内容（由主程序调用，保持单一数据源）。
func SetUpgradeMapContent(c string) {
	if strings.TrimSpace(c) != "" {
		nginxUpgradeMapContent = c
	}
}

// ConfDIncluded 检查 nginx.conf 的 http 块是否 include 了 conf.d/*.conf。
func ConfDIncluded() (bool, string, error) {
	b, err := os.ReadFile(NginxConf())
	if err != nil {
		return false, "", err
	}
	re := regexp.MustCompile(`(?m)^\s*include\s+(\S*conf\.d/\*\.conf)\s*;`)
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		return false, "nginx.conf 未 include conf.d/*.conf", nil
	}
	return true, "已包含 " + m[1], nil
}

func BackupNginxConf() (string, error) {
	b, err := os.ReadFile(NginxConf())
	if err != nil {
		return "", err
	}
	path := NginxConfBackupPath()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ---------- 证书 ----------

// MakeSelfSignedCert 为指定域名生成自签证书（openssl 命令行，便于用户手工复现）。
func MakeSelfSignedCert(domain, certPath, keyPath string, days int) (string, error) {
	if err := ValidateDomain(domain); err != nil {
		return "", err
	}
	if days <= 0 || days > 3650 {
		days = 825 // 浏览器对自签证书的有效期上限约为 825 天
	}
	if !filepath.IsAbs(certPath) || !filepath.IsAbs(keyPath) {
		return "", errors.New("证书路径必须是绝对路径")
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return "", err
	}
	san := "subjectAltName=DNS:" + domain + ",DNS:www." + domain
	r := runTimeout(30*time.Second, "/usr/bin/openssl", "req", "-x509", "-newkey", "rsa:2048",
		"-sha256", "-days", strconv.Itoa(days), "-nodes",
		"-keyout", keyPath, "-out", certPath,
		"-subj", "/CN="+domain, "-addext", san)
	if r.err != nil {
		return "", fmt.Errorf("生成自签证书失败: %s", r.combined())
	}
	_ = os.Chmod(keyPath, 0o600)
	return certPath, nil
}

func CertInfo(certPath string) (subject, issuer string, notAfter time.Time, err error) {
	r := runTimeout(15*time.Second, "/usr/bin/openssl", "x509", "-in", certPath,
		"-noout", "-subject", "-issuer", "-enddate")
	if r.err != nil {
		return "", "", time.Time{}, fmt.Errorf("读取证书失败: %s", r.combined())
	}
	for _, ln := range strings.Split(r.stdout, "\n") {
		ln = strings.TrimSpace(ln)
		if v, ok := strings.CutPrefix(ln, "subject="); ok {
			subject = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(ln, "issuer="); ok {
			issuer = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(ln, "notAfter="); ok {
			if t, e := time.Parse("Jan  2 15:04:05 2006 MST", strings.TrimSpace(v)); e == nil {
				notAfter = t
			} else if t, e := time.Parse("Jan 2 15:04:05 2006 MST", strings.TrimSpace(v)); e == nil {
				notAfter = t
			}
		}
	}
	return subject, issuer, notAfter, nil
}

// ---------- mkcert ----------

// MkcertTrust 用 mkcert 生成并信任本地 CA（未安装时返回明确指引，不静默失败）。
func MkcertTrust() (string, error) {
	bin := ""
	for _, p := range []string{
		filepath.Join(HomebrewPrefix(), "bin", "mkcert"),
		"/usr/local/bin/mkcert",
	} {
		if _, err := os.Stat(p); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return "", errors.New("未安装 mkcert，请先执行: brew install mkcert nss")
	}
	r := runTimeout(60*time.Second, bin, "-install")
	if r.err != nil {
		return "", fmt.Errorf("mkcert 安装 CA 失败: %s", r.combined())
	}
	return r.combined(), nil
}

func MkcertIssue(hosts []string, outCert, outKey string) (string, error) {
	bin := filepath.Join(HomebrewPrefix(), "bin", "mkcert")
	if _, err := os.Stat(bin); err != nil {
		return "", errors.New("未安装 mkcert，请先执行: brew install mkcert nss")
	}
	if len(hosts) == 0 {
		return "", errors.New("至少需要一个域名或 IP")
	}
	// 逐个校验，避免把奇怪参数传给 mkcert
	for _, h := range hosts {
		if netIPOK(h) {
			continue
		}
		if err := ValidateDomain(h); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(outCert) || !filepath.IsAbs(outKey) {
		return "", errors.New("输出路径必须是绝对路径")
	}
	args := append([]string{"-cert-file", outCert, "-key-file", outKey}, hosts...)
	r := runTimeout(120*time.Second, bin, args...)
	if r.err != nil {
		return "", fmt.Errorf("mkcert 签发失败: %s", r.combined())
	}
	return r.combined(), nil
}

func netIPOK(s string) bool {
	re := regexp.MustCompile(`^[0-9a-fA-F:.]+$`)
	return re.MatchString(s) && strings.ContainsAny(s, ":. ")
}

// ensureNginxRuntimeDirs 确保 nginx 运行所需的**所有目录**都存在。
// 真机 2026-09-16 连环踩到：缺 var/log/nginx、var/run、*_temp 任一，任何 vhost 都写不进去，
// 而面板只回"配置语法错误，已回滚" —— 路径一律从 nginx.conf 解析、不写死；返回改动说明。

// ---------- nginx worker 属主判定（判据贴着运行体） ----------

// nginxWorkerProbe 从**正在运行的 worker 进程**反查 uid/gid。
// 做成可注入变量：开发机/CI 上真的跑着 nginx，不注入就没法测"没有 worker"分支。
var nginxWorkerProbe = func() (uid, gid, pid int, ok bool) {
	out, err := exec.Command("/usr/bin/pgrep", "-f", "nginx: worker process").Output()
	if err != nil {
		return 0, 0, 0, false
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, 0, 0, false
	}
	ps, err := exec.Command("/bin/ps", "-o", "uid=,gid=", "-p", fields[0]).Output()
	if err != nil {
		return 0, 0, 0, false
	}
	f := strings.Fields(string(ps))
	if len(f) < 2 {
		return 0, 0, 0, false
	}
	u, e1 := strconv.Atoi(f[0])
	g, e2 := strconv.Atoi(f[1])
	if e1 != nil || e2 != nil {
		return 0, 0, 0, false
	}
	p, _ := strconv.Atoi(fields[0])
	return u, g, p, true
}

// SetNginxWorkerProbeForTest 替换"从 worker 进程反查属主"的实现，返回恢复函数。
// 只给测试用：web 包要验证"属主未知时不许 chown"，而真机上真的跑着 nginx。
func SetNginxWorkerProbeForTest(fn func() (uid, gid, pid int, ok bool)) (restore func()) {
	prev := nginxWorkerProbe
	nginxWorkerProbe = fn
	return func() { nginxWorkerProbe = prev }
}

// NginxWorkerOwner 返回 nginx worker **真正**运行的用户（及判据来源，给日志用）。
// 优先级：运行中的 worker 进程（唯一权威事实）→ nginx.conf 的 `user` 指令 →
// 都拿不到就 ok=false，调用方**不许猜**（尤其不许猜 nobody：猜错等于没修还藏因，报障即此坑）。
func NginxWorkerOwner(confPath string) (name string, uid, gid int, how string, ok bool) {
	if u, g, pid, pok := nginxWorkerProbe(); pok {
		n := strconv.Itoa(u)
		if lu, err := user.LookupId(n); err == nil && lu.Username != "" {
			n = lu.Username
		}
		return n, u, g, fmt.Sprintf("运行中的 worker 进程（pid %d）", pid), true
	}
	if n := NginxWorkerUserFromConf(confPath); n != "" {
		if u, g, err := lookupIDs(n); err == nil {
			return n, u, g, "nginx.conf 的 user 指令", true
		}
		return n, -1, -1, "nginx.conf 的 user 指令（该用户不存在）", false
	}
	return "", -1, -1, "既没有运行中的 worker，nginx.conf 里也没有 user 指令", false
}

// NginxWorkerUserFromConf 从 nginx.conf 读 `user <name> [group];`（跳过注释行）。
func NginxWorkerUserFromConf(confPath string) string {
	b, err := os.ReadFile(confPath)
	if err != nil {
		return ""
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "user") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if len(f) >= 2 {
			return f[1]
		}
	}
	return ""
}

// nginxTempDirs 返回 nginx 落盘请求体/代理响应要用的临时目录（来自 `nginx -V` 编译参数）。
// 这些目录缺失或属主不是 worker 用户时，**任何超过 client_body_buffer_size 的请求体**
// 都会让 nginx 直接回自己的 500 HTML 页面（上传、音色样本、大 SQL 导入）。
func nginxTempDirs() []string {
	base := filepath.Join(HomebrewPrefix(), "var", "run", "nginx")
	out := []string{base}
	for _, sub := range []string{"client_body_temp", "proxy_temp", "fastcgi_temp", "uwsgi_temp", "scgi_temp"} {
		out = append(out, filepath.Join(base, sub))
	}
	return out
}

func ensureNginxRuntimeDirs() (string, error) {
	dirs, err := nginxRequiredDirs(NginxConf())
	if err != nil {
		return "", err
	}
	// 面板反代规则自己的日志目录（nginx.conf 里不会出现，由面板约定）
	dirs = append(dirs, filepath.Join(HomebrewPrefix(), "var", "log", "nginx", "proxy"))

	changed := []string{}
	for _, d := range dedupeStrings(dirs) {
		if _, serr := os.Stat(d); serr == nil {
			continue
		}
		if merr := os.MkdirAll(d, 0o755); merr != nil {
			return "", fmt.Errorf("创建 nginx 运行时目录 %s 失败: %w", d, merr)
		}
		changed = append(changed, d)
	}
	if len(changed) == 0 {
		return "", nil
	}
	// 临时目录：**worker 用户**必须能写（见 nginxTempDirs 的说明）。
	// 属主判定走 NginxWorkerOwner：运行体优先，读不到就**不改属主**并如实说明。
	tempDirs := nginxTempDirs()
	tempChanged := []string{}
	for _, d := range tempDirs {
		if _, serr := os.Stat(d); serr == nil {
			continue
		}
		if merr := os.MkdirAll(d, 0o700); merr != nil {
			return "", fmt.Errorf("创建 nginx 临时目录 %s 失败: %w", d, merr)
		}
		tempChanged = append(tempChanged, d)
	}
	ownerNote := ""
	if len(tempChanged) > 0 {
		name, uid, gid, how, ok := NginxWorkerOwner(NginxConf())
		if ok {
			for _, d := range tempChanged {
				_ = os.Chown(d, uid, gid)
			}
			ownerNote = fmt.Sprintf("（属主改为 nginx worker 用户 %s，判据：%s）", name, how)
		} else {
			// 绝不猜 nobody：猜错等于没修，而且把真正的原因藏起来。
			ownerNote = "（**无法确定 nginx worker 用户**：" + how +
				"；目录已建但未改属主，大请求体仍可能报 500 —— 请在 nginx.conf 里写上 user 指令）"
		}
	}
	// 归属真实用户：nginx 以该用户身份跑，日志/pid 目录属主不对会写不进去。
	// 只 chown 我们自己新建的目录，不递归（真实事故：`chown -R` 把 brew 的 etc 也改成 root，配置全废）。
	if u := strings.TrimSpace(os.Getenv("ZIZPANEL_USER")); u != "" && u != "root" {
		if uid, gid, uerr := lookupIDs(u); uerr == nil {
			for _, d := range changed {
				_ = os.Chown(d, uid, gid)
			}
		}
	}
	if len(changed) == 0 && len(tempChanged) == 0 {
		return "", nil
	}
	msg := ""
	if len(changed) > 0 {
		msg = fmt.Sprintf("已补齐 nginx 运行时目录 %d 个（%s）", len(changed), strings.Join(changed, "、"))
	}
	if len(tempChanged) > 0 {
		if msg != "" {
			msg += "；"
		}
		msg += fmt.Sprintf("已补齐 nginx 临时目录 %d 个%s", len(tempChanged), ownerNote)
	}
	return msg, nil
}

// nginxRequiredDirs 从 nginx.conf 解析出所有需要的目录（error_log/access_log/pid 取所在目录，
// *_temp_path 与 *_temp 参数本身即目录）。
// 读不到文件返回错误；读到但没有任何路径指令时返回空列表 —— 用户自定义布局，面板不猜。
func nginxRequiredDirs(confPath string) ([]string, error) {
	b, err := os.ReadFile(confPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", confPath, err)
	}
	// "path" / "log" 类指令：最后一个参数是文件，取它的目录
	fileDirective := regexp.MustCompile(`(?m)^\s*(?:error_log|access_log|pid)\s+([^;\s]+)`)
	// "*_temp_path" 类：参数本身就是目录
	dirDirective := regexp.MustCompile(`(?m)^\s*\w*_temp_path\s+([^;\s]+)`)
	// "*_temp" 类（新式写法）：参数是目录
	plainTempDirective := regexp.MustCompile(`(?m)^\s*(?:client_body_temp|proxy_temp|fastcgi_temp|uwsgi_temp|scgi_temp)\s+([^;\s]+)`)

	var out []string
	push := func(p string) {
		p = strings.Trim(p, `"'`)
		if p == "" || !filepath.IsAbs(p) {
			return // 相对路径由 nginx 的 prefix 决定，面板不猜
		}
		if p == "off" || p == "/dev/stderr" || p == "/dev/stdout" || p == "stderr" {
			return
		}
		out = append(out, p)
	}
	for _, m := range fileDirective.FindAllStringSubmatch(string(b), -1) {
		push(filepath.Dir(m[1]))
	}
	for _, re := range []*regexp.Regexp{dirDirective, plainTempDirective} {
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			push(m[1])
		}
	}
	return out, nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func lookupIDs(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, uerr := strconv.Atoi(u.Uid)
	if uerr != nil {
		return 0, 0, uerr
	}
	gid, gerr := strconv.Atoi(u.Gid)
	if gerr != nil {
		return 0, 0, gerr
	}
	return uid, gid, nil
}

// VhostsIncluded 检查 nginx.conf 是否 include 了 vhosts/*.conf。
func VhostsIncluded() (bool, string, error) {
	return includePresent(VhostDir())
}

// EnsureVhostsInclude 确保 nginx.conf 加载了 vhosts 目录。
// 真机事故 2026-09-16：`brew reinstall nginx` 会把 nginx.conf 还原成默认版、include 丢失，
// 用户看到"配置写进去了却 404/连不上"；所以每次自愈都要补齐。
func EnsureVhostsInclude() error {
	vhostDir := VhostDir()
	if err := os.MkdirAll(vhostDir, 0o755); err != nil {
		return fmt.Errorf("创建 vhosts 目录失败: %w", err)
	}
	ok, _, err := VhostsIncluded()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// 插到 http 块末尾（最后一个 } 之前）
	b, rerr := os.ReadFile(NginxConf())
	if rerr != nil {
		return fmt.Errorf("读取 nginx.conf 失败: %w", rerr)
	}
	text := string(b)
	i := strings.LastIndex(strings.TrimRight(text, "\n"), "}")
	if i < 0 {
		return fmt.Errorf("nginx.conf 结构异常（找不到 http 块结束符），拒绝修改")
	}
	insert := "\n    # 由 ZizPanel 添加：加载站点与反向代理配置\n" +
		"    include " + vhostDir + "/*.conf;\n"
	text = text[:i] + insert + text[i:]
	if _, err := os.Stat(NginxConfBackupPath()); err != nil {
		_ = os.WriteFile(NginxConfBackupPath(), b, 0o644)
	}
	tmp := NginxConf() + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return fmt.Errorf("写入 nginx.conf 失败: %w", err)
	}
	if err := os.Rename(tmp, NginxConf()); err != nil {
		return fmt.Errorf("替换 nginx.conf 失败: %w", err)
	}
	return nil
}

func includePresent(dir string) (bool, string, error) {
	b, err := os.ReadFile(NginxConf())
	if err != nil {
		return false, "", err
	}
	pattern := dir + "/*.conf"
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "#") && strings.Contains(t, pattern) {
			return true, "已包含 " + pattern, nil
		}
	}
	return false, "nginx.conf 未 include " + pattern, nil
}
