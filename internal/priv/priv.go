// Package priv 实现 zizpanel-helper 的全部特权操作。
//
// 安全模型：本包的所有函数都以 root 执行，因此每个入参都必须在这里
// 重新做白名单校验 —— 绝不能假设调用方（面板）已经校验过。
// 这是"面板被攻破也不能提权"这条底线的落点。
package priv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	if _, err := os.Stat("/opt/homebrew/bin/brew"); err == nil {
		return "/opt/homebrew"
	}
	return "/usr/local"
}

// NginxBin 返回 nginx 可执行文件路径。
func NginxBin() string {
	return filepath.Join(HomebrewPrefix(), "bin", "nginx")
}

// NginxConf 返回主配置文件路径。
func NginxConf() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "nginx.conf")
}

// VhostDir 返回 vhost 目录。
func VhostDir() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "vhosts")
}

// NginxLaunchLabel 是 nginx 的 LaunchDaemon 标签（由 LNMP 安装脚本创建）。
const NginxLaunchLabel = "cn.zizdog.nginx"

// PanelLaunchLabel 是面板自己的 LaunchDaemon 标签。
const PanelLaunchLabel = "cn.zizpanel.panel"

var (
	// 域名必须是常规主机名。注意：Go 的 regexp 是 RE2，不支持 (?!...) 负向前瞻，
	// 所以用 "字母数字开头 + 内部允许连字符" 的结构来表达"标签不能以连字符开头/结尾"。
	reDomain = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)
	// vhost 文件名只允许安全字符，从根本上排除 ../ 之类穿越
	reSafeFile = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// 允许 @：Homebrew 的**版本化**服务名一律带它
	// （homebrew.mxcl.php@8.3、sh.brew.mysql@8.4、node@20、python@3.12 …）。
	// 早期正则不含 @，导致这类服务的状态检查直接报
	// "label 含非法字符"，界面上一律显示"异常" —— 而服务其实好好的。
	// 真机上就是这么踩到的：php 与 mysql 显示异常，nginx（无 @）正常。
	reLabel = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)
	rePort  = regexp.MustCompile(`^\d{1,5}$`)
)

// ValidateDomain 校验域名合法性。
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
//
// 双重防护：字符白名单（排除 / \ ..）+ 结果路径前缀校验。
// 只做前缀校验不够（符号链接可绕过），只做字符校验也不够
// （不同文件系统的大小写/Unicode 折叠行为不同），两者都要。
//
// macOS 坑：/var、/tmp、/etc 都是指向 /private/... 的软链接。
// filepath.EvalSymlinks 会解析成 /private/var/...，如果另一边用未解析的
// /var/... 去比前缀就会误判"路径越界"。因此这里对两侧都做解析。
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
	// 逐级向上找到第一个存在的祖先，解析它再拼回剩余部分
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
//
// 必须用多种方式判断：nginx 启动后会把进程标题改成
// "nginx: master process ..."，所以 `pgrep -x nginx` 精确匹配必然失败 ——
// 这正是旧脚本里"80 端口被占用但杀不掉 nginx"的根因。
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
	// launchd 托管时用 bootstrap/kickstart；失败则退回直接启动
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
	// 等端口真正释放
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

// ListVhosts 列出所有 vhost 配置文件名。
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

// ReadVhost 读取 vhost 内容。
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

// WriteVhostAtomic 原子写入 vhost：先写临时文件、校验 nginx 语法、再替换。
//
// 顺序至关重要：先替换再校验的话，一旦配置有错 nginx 就会在某些时刻
// 加载到坏配置。这里改成"写临时文件 → nginx -t 校验 → rename"。
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

	// 用临时文件替换后再整体校验：nginx -t 只能校验最终配置文件树，
	// 因此这里先备份原文件、替换、校验，不通过就回滚。
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

// DeleteVhost 删除 vhost 配置。
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
	// 已存在同域名的任意解析就不再重复添加
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
//
// 用注释而不是删除：用户经常需要在"本地测试"和"访问线上站点"之间切换，
// 注释保留了域名列表，切换回来不需要重新添加。
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

// writeHosts 原子写 /etc/hosts。
// 直接截断写入很危险：写入过程中断电会得到一个空的 hosts 文件，
// 系统解析会立刻出问题。因此先写临时文件再 rename。
func writeHosts(content string) error {
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	tmp := "/etc/hosts.zizpanel.tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	// 保留原文件权限
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
type LaunchState struct {
	Label    string `json:"label"`
	Loaded   bool   `json:"loaded"`
	Running  bool   `json:"running"`
	PID      int    `json:"pid"`
	ExitCode int    `json:"exit_code"`
	Domain   string `json:"domain"`
}

// launchDomain 判断标签属于系统守护进程还是用户代理。
// LaunchDaemon（/Library/LaunchDaemons）需要 system 域且以 root 运行；
// LaunchAgent（~/Library/LaunchAgents）需要 gui/<uid> 域。
func launchDomain(label string) string {
	if _, err := os.Stat(filepath.Join("/Library/LaunchDaemons", label+".plist")); err == nil {
		return "system"
	}
	// 用户代理：找到 plist 所属用户
	for _, home := range userHomes() {
		if _, err := os.Stat(filepath.Join(home, "Library/LaunchAgents", label+".plist")); err == nil {
			return "gui/" + uidOfHome(home)
		}
	}
	return "system"
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

// uidOfHome 通过 home 目录反查 uid。
func uidOfHome(home string) string {
	user := filepath.Base(home)
	r := run("/usr/bin/id", "-u", user)
	if r.err == nil {
		return strings.TrimSpace(r.stdout)
	}
	return "501"
}

// LaunchStatus 查询任务状态。
func LaunchStatus(label string) (LaunchState, error) {
	if !reLabel.MatchString(label) {
		return LaunchState{}, fmt.Errorf("label 含非法字符: %q", label)
	}
	st := LaunchState{Label: label, Domain: launchDomain(label), ExitCode: -1}
	r := run("/bin/launchctl", "print", st.Domain+"/"+label)
	if r.err != nil {
		return st, nil // 未加载
	}
	st.Loaded = true
	// launchctl print 输出形如：
	//   state = running
	//   pid = 1234
	//   last exit code = 0
	for _, ln := range strings.Split(r.stdout, "\n") {
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

	// 兜底：`launchctl print` 对**按需拉起**的服务可能不给 pid 行
	// （实测 php-fpm 报 `state = spawn scheduled`，没有 pid = ），
	// 但 `launchctl list` 里它明明有 PID 而且在监听端口。
	// 少了这个兜底，这类服务会被误判成"未运行"，界面上显示异常。
	if !st.Running {
		if pid := pidFromLaunchctlList(label); pid > 0 {
			st.PID = pid
			st.Running = true
		}
	}
	return st, nil
}

// pidFromLaunchctlList 从 `launchctl list` 里取某个 label 的 PID。
//
// 输出是三列：PID  Status  Label，未运行时 PID 是 "-"。
func pidFromLaunchctlList(label string) int {
	r := run("/bin/launchctl", "list")
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

// LaunchLoad 加载并启动任务。
func LaunchLoad(label string) error {
	if !reLabel.MatchString(label) {
		return fmt.Errorf("label 含非法字符: %q", label)
	}
	plist, domain, err := findPlist(label)
	if err != nil {
		return err
	}
	// 已加载时用 kickstart 重新拉起
	if st, _ := LaunchStatus(label); st.Loaded {
		return LaunchKickstart(label)
	}
	r := run("/bin/launchctl", "bootstrap", domain, plist)
	if r.err != nil {
		return fmt.Errorf("加载 %s 失败: %s", label, r.combined())
	}
	return nil
}

// LaunchUnload 停止并卸载任务。
func LaunchUnload(label string) error {
	if !reLabel.MatchString(label) {
		return fmt.Errorf("label 含非法字符: %q", label)
	}
	domain := launchDomain(label)
	r := run("/bin/launchctl", "bootout", domain+"/"+label)
	if r.err != nil {
		// 未加载时 bootout 会报错，这不算失败
		if st, _ := LaunchStatus(label); !st.Loaded {
			return nil
		}
		return fmt.Errorf("停止 %s 失败: %s", label, r.combined())
	}
	return nil
}

// LaunchKickstart 重启任务（-k 表示先杀掉正在运行的实例）。
func LaunchKickstart(label string) error {
	if !reLabel.MatchString(label) {
		return fmt.Errorf("label 含非法字符: %q", label)
	}
	domain := launchDomain(label)
	r := run("/bin/launchctl", "kickstart", "-k", domain+"/"+label)
	if r.err != nil {
		return fmt.Errorf("重启 %s 失败: %s", label, r.combined())
	}
	return nil
}

// findPlist 定位 plist 文件并推断所属域。
func findPlist(label string) (path, domain string, err error) {
	sys := filepath.Join("/Library/LaunchDaemons", label+".plist")
	if _, e := os.Stat(sys); e == nil {
		return sys, "system", nil
	}
	for _, home := range userHomes() {
		p := filepath.Join(home, "Library/LaunchAgents", label+".plist")
		if _, e := os.Stat(p); e == nil {
			return p, "gui/" + uidOfHome(home), nil
		}
	}
	return "", "", fmt.Errorf("找不到 plist 文件: %s", label)
}

// ---------- 端口 ----------

// PortInfo 描述端口占用情况。
type PortInfo struct {
	Port    int      `json:"port"`
	InUse   bool     `json:"in_use"`
	Holders []string `json:"holders"`
}

// CheckPort 检测端口占用。
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

// FirewallState 返回全局状态描述。
func FirewallState() (string, error) {
	r := run("/usr/libexec/ApplicationFirewall/socketfilterfw", "--getglobalstate")
	if r.err != nil {
		return "", fmt.Errorf("读取防火墙状态失败: %s", r.combined())
	}
	return strings.TrimSpace(r.stdout), nil
}

// FirewallOpenPort 尝试放行端口。
//
// 说明：macOS 的应用防火墙（ALF）是按"应用"而不是按"端口"放行的，
// socketfilterfw 没有开放端口的能力。这里返回明确提示而不是假装成功。
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
//
// 为什么要做成 .app：macOS 应用防火墙只认 .app bundle 或已签名的二进制，
// 直接添加一个 CLI 二进制会持续弹窗或在更新后失效。
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
	// 用 Info.plist 让系统把它当作正常应用
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

// NginxConfD 返回 conf.d 目录。
func NginxConfD() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "conf.d")
}

// NginxIncludesDir 返回存放"只能在特定上下文使用"的配置片段目录。
//
// 为什么需要它：项目原有的 php-fpm.conf 里含 fastcgi_pass，
// 这个指令只在 location 上下文合法。而 conf.d/*.conf 是被 http 级 include 的，
// 一旦把 php-fpm.conf 留在 conf.d，nginx -t 会直接报
//
//	"fastcgi_pass" directive is not allowed here
//
// 从而整个 nginx 起不来。
//
// 约定：
//
//	conf.d/    → 只放 http 上下文合法的配置（map、log_format 等）
//	includes/  → 放需要被 vhost/location 显式 include 的片段（fastcgi 参数等）
func NginxIncludesDir() string {
	return filepath.Join(HomebrewPrefix(), "etc", "nginx", "includes")
}

// LegacyFPMIncludePath 是历史遗留的 PHP-FPM 片段路径（在 conf.d 下，上下文错误）。
func LegacyFPMIncludePath() string {
	return filepath.Join(NginxConfD(), "php-fpm.conf")
}

// MigrateFPMInclude 把 conf.d/php-fpm.conf 迁移到 includes/php-fpm.conf，
// 并把所有引用它的配置（vhost 与模板）里的路径一并改写。
//
// 这是幂等的：迁移过之后再次调用不会有任何变化。
// 返回是否发生了迁移以及具体做了什么。
func MigrateFPMInclude() (bool, string, error) {
	legacy := LegacyFPMIncludePath()
	newPath := filepath.Join(NginxIncludesDir(), "php-fpm.conf")

	_, legacyErr := os.Stat(legacy)
	_, newErr := os.Stat(newPath)

	if os.IsNotExist(legacyErr) {
		if newErr == nil {
			// 已经迁移过，只需确保引用已更新
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
		// 跳过 includes 目录下的文件自身
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

// EnsureNginxContexts 做一次完整的上下文整理：
//  1. 迁移上下文敏感的 php-fpm.conf
//  2. 写入 upgrade map
//  3. 确保 conf.d 被 include
//
// 顺序不能颠倒：必须先迁移，再加 include，
// 否则中间会经过一个 nginx 语法非法的状态。
func EnsureNginxContexts(upgradeMapContent string) (string, error) {
	migrated, msg, err := MigrateFPMInclude()
	if err != nil {
		return "", err
	}
	parts := []string{}
	if msg != "" {
		parts = append(parts, msg)
	}
	// 记录改动前的状态，用于判断是否真的需要重载 nginx
	before, _, _ := ConfDIncluded()
	beforeMap, _ := os.ReadFile(filepath.Join(NginxConfD(), "upgrade-map.conf"))
	if err := EnsureUpgradeMap(upgradeMapContent); err != nil {
		return "", err
	}
	after, _, _ := ConfDIncluded()
	afterMap, _ := os.ReadFile(filepath.Join(NginxConfD(), "upgrade-map.conf"))
	changed := migrated || before != after || string(beforeMap) != string(afterMap)

	if !changed {
		return "nginx 环境已就绪（无变化，未重载）", nil
	}
	parts = append(parts, "nginx 环境已更新（WebSocket 升级支持 + conf.d 加载）")
	return strings.Join(parts, "；"), nil
}

// EnsureUpgradeMap 写入 WebSocket 升级所需的 map 片段，并确保主配置 include 了 conf.d。
//
// 为什么必须由面板负责：反向代理配置里用到 $connection_upgrade 变量，
// 而它只能在 http 上下文用 map 定义，不能写在 server 块内。
// 如果这个 map 缺失，nginx -t 会直接报 unknown variable，
// 所有反代站点都无法启用 —— 所以这是面板的职责，而不是用户的。
//
// 本函数是幂等的：重复执行不会产生重复的 include，也不会覆盖用户对 map 的修改。
func EnsureUpgradeMap(mapContent string) error {
	confD := NginxConfD()
	if err := os.MkdirAll(confD, 0o755); err != nil {
		return fmt.Errorf("创建 conf.d 目录失败: %w", err)
	}
	mapPath := filepath.Join(confD, "upgrade-map.conf")

	// 只在内容真的变化时才写入。
	//
	// 为什么重要：这个函数会在面板每次启动时被调用（自愈）。
	// 如果无条件写文件，配合"写后重载 nginx"就会在每次开机时
	// 触发一次全站 reload —— 没必要，而且 reload 瞬间可能造成请求抖动。
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

// ensureConfDIncluded 检查 nginx.conf 的 http 块是否 include 了 conf.d/*.conf，
// 没有则插入（只在 http { 之后插入一次）。
func ensureConfDIncluded() error {
	confPath := NginxConf()
	b, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("读取 nginx.conf 失败: %w", err)
	}
	content := string(b)

	// 已包含（任意形式的 conf.d 通配）就什么都不做
	if regexp.MustCompile(`(?m)^\s*include\s+\S*conf\.d/\*\.conf\s*;`).MatchString(content) {
		return nil
	}

	// 找到 http { 的位置：必须是行首的 http 块（避免匹配到注释里的 http）
	re := regexp.MustCompile(`(?m)^([ \t]*)http\s*\{`)
	loc := re.FindStringSubmatchIndex(content)
	if loc == nil {
		return errors.New("nginx.conf 中未找到 http 块，无法插入 conf.d include")
	}
	insertAt := loc[1] // "http {" 的右花括号位置之后
	line := "\n    # 由 ZizPanel 添加：加载 conf.d 下的通用片段（如 WebSocket 升级 map）\n" +
		"    include " + NginxConfD() + "/*.conf;\n"

	// 先备份，再原子替换
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

// NginxConfBackupPath 返回 nginx.conf 的备份路径（供面板提示用户）。
func NginxConfBackupPath() string { return NginxConf() + ".zizpanel.bak" }

// EnsureNginxEnv 确保 nginx 具备面板所需的通用环境：
//   - conf.d/upgrade-map.conf（反向代理的 WebSocket 支持）
//   - nginx.conf 的 http 块 include 了 conf.d/*.conf
//
// 返回人类可读的执行说明，便于面板在日志里记录做了什么。
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

// BackupNginxConf 备份 nginx.conf，返回备份路径。
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

// MakeSelfSignedCert 为指定域名生成自签证书。
//
// 用 openssl 而不是自己写 x509：面板自身已经有 tlsx 包生成证书，
// 但站点证书需要由 root 写入 nginx 可读的路径，且要包含精确的 SAN，
// 复用 openssl 的命令行参数最直观、也便于用户手工复现。
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

// CertInfo 读取证书信息。
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

// MkcertTrust 用 mkcert 生成并信任本地 CA，让浏览器不再提示证书不受信任。
// 未安装 mkcert 时返回明确指引，而不是静默失败。
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

// MkcertIssue 为指定域名/IP 签发本地受信证书。
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
