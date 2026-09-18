package sites

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 本文件解决"PHP 多版本共存"里最核心、也最容易踩的一环：
//
//	**每个 PHP 版本的 php-fpm 必须监听在各自唯一的 FastCGI 端点上。**
//
// 背景（2026-09 在本机实测）：Homebrew 的每个 php@x.y 的
// etc/php/<版本>/php-fpm.d/www.conf 出厂都写着 `listen = 127.0.0.1:9000`。
// 于是"装第二个版本"必然变成抢同一个端口 —— 先起的那个赢，
// 后起的 fpm **起不来**（日志里是 "Address already in use"），
// 而 nginx 那边仍然把 8.4 站点的请求发到 9000，由 8.3 应答。
// 现象是"我明明选了 8.4，跑的却是 8.3"，且两个版本在界面上都声称在 9000。
//
// 因此面板的口径是：
//
//  1. 每个版本有**确定的**一个端点 —— 默认 Unix socket
//     <brew>/var/run/php-fpm-<版本>.sock（天然按版本唯一，不会互相抢端口，
//     也不占 TCP 端口、不受 macOS 端口占用影响）。
//  2. 这个端点由面板在"启用该版本"时**写进它的 www.conf**（幂等、有备份）；
//     只解析不落实，会出现"配置里写了一个没人监听的地址"的假共存。
//  3. 解析不到端点时**明确报错**，绝不静默回退到 9000。
//     静默回退正是多版本互相抢端口的根源：错误被藏起来，症状出现在别处。

// ---------- 端点分配 ----------

// fpmPortBase 是 TCP 端点的起算端口。
//
// 只有 `fpmTCPVersions` 里显式登记的版本才用 TCP；其余版本一律走 Unix socket。
// 保留这个映射是为了兼容"某些环境里用 TCP 更省事"的诉求，
// 同时把端口选择收敛成**一处**，不允许在业务代码里散落魔数。
const fpmPortBase = 9000

// fpmTCPVersions 登记使用 TCP 端点的版本 -> 端口。
// 默认（不在本表里）一律用 Unix socket。
var fpmTCPVersions = map[string]int{}

// fpmSocketMaxLen 是 Unix domain socket 的路径长度上限。
//
// macOS 的 sun_path 是 104 字节，超了 bind() 直接失败（报 "AF_UNIX path too long"）。
// 自定义 Homebrew 前缀（/Volumes/... 之类）很容易超，所以提前给出可读的报错，
// 而不是让 php-fpm 在后台悄悄起不来。
const fpmSocketMaxLen = 104

// UsesUnixSocket 判断某个 PHP 版本是否使用 Unix socket 端点。
func UsesUnixSocket(version string) bool {
	_, tcp := fpmTCPVersions[version]
	return !tcp
}

// SocketPath 返回某个 PHP 版本的 Unix socket 端点。
//
// 路径按版本号唯一 —— 这就是"多版本不会互相抢端点"的根据。
func SocketPath(brewPrefix, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", errors.New("PHP 版本号为空，无法分配 FastCGI 端点")
	}
	if !rePHPVersion.MatchString(version) {
		return "", fmt.Errorf("PHP 版本号 %q 格式不合法（应形如 8.3）", version)
	}
	if strings.TrimSpace(brewPrefix) == "" {
		return "", fmt.Errorf("未探测到 Homebrew 前缀，无法为 PHP %s 分配套接字路径", version)
	}
	p := filepath.Join(brewPrefix, "var", "run", "php-fpm-"+version+".sock")
	if len(p) > fpmSocketMaxLen {
		return "", fmt.Errorf(
			"PHP %s 的套接字路径过长（%d 字符，上限 %d）：%s —— "+
				"Homebrew 前缀太深，Unix socket 无法绑定",
			version, len(p), fpmSocketMaxLen, p)
	}
	return p, nil
}

// VersionPort 返回某个 PHP 版本的 TCP 端点端口。
//
// 显式登记过就用登记值；否则按 fpmPortBase + (minor+1) 推导（8.1→9002、8.3→9004…）。
// 只对走 TCP 的版本有意义（见 fpmTCPVersions）。
func VersionPort(version string) int {
	if p, okk := fpmTCPVersions[version]; okk {
		return p
	}
	minor, err := phpMinor(version)
	if err != nil {
		return 0
	}
	return fpmPortBase + minor + 1
}

// PreferredEndpoint 返回**面板为该版本分配**的端点（不读磁盘）。
//
// "已配置成我们要的端点了吗"这类判断都以它为准（见 NeedsListenFix）。
func PreferredEndpoint(brewPrefix, version string) (string, error) {
	if UsesUnixSocket(version) {
		p, err := SocketPath(brewPrefix, version)
		if err != nil {
			return "", err
		}
		return "unix:" + p, nil
	}
	return fmt.Sprintf("127.0.0.1:%d", VersionPort(version)), nil
}

var (
	rePHPVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	// 合法的 listen 值：Unix socket 绝对路径，或 host:port（host 仅允许
	// 回环地址/主机名）。刻意不接受任意字符串 —— 这个值会被写进
	// nginx 的 fastcgi_pass 与 php-fpm 的 listen，等于配置注入面。
	reListenTCP = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+|\[[0-9a-fA-F:]+\]):([0-9]{1,5})$`)
	reListenAbs = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// phpMinor 从 "8.3" 里取 minor 号。
func phpMinor(version string) (int, error) {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) < 2 {
		return 0, fmt.Errorf("版本号 %q 格式不合法（应形如 8.3）", version)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 0 || n > 99 {
		return 0, fmt.Errorf("版本号 %q 的 minor 段不合法", version)
	}
	return n, nil
}

// ---------- 端点解析 ----------

// FPMConfPath 返回某版本 php-fpm 的 www.conf 路径（版本号非法时返回空串）。
//
// 这里必须校验版本号：它来自 HTTP 请求（站点表单），未校验的
// `../../etc/passwd` 之类会拼出一条越界路径。
func FPMConfPath(brewPrefix, version string) string {
	version = strings.TrimSpace(version)
	if !rePHPVersion.MatchString(version) {
		return ""
	}
	return filepath.Join(brewPrefix, "etc", "php", version, "php-fpm.d", "www.conf")
}

// ResolveConfigEndpoint 只读 www.conf，返回里面**实际写着**的 listen 值。
//
// 不做"没人监听就报错"的判断 —— 界面需要在版本未运行时照样展示端点。
// 解析失败一律返回带排障信息的错误（版本、文件路径、原因），
// 绝不像旧实现那样静默回退 127.0.0.1:9000。
func ResolveConfigEndpoint(brewPrefix, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", errors.New("站点未指定 PHP 版本（空表示纯静态站点）")
	}
	// 先校验版本号，再碰文件系统：非法版本号不允许拼出路径去试探
	if !rePHPVersion.MatchString(version) {
		return "", fmt.Errorf(
			"PHP 版本号 %q 格式不合法（只接受形如 8.3 的版本号，不接受路径或其它字符）", version)
	}
	conf := FPMConfPath(brewPrefix, version)
	b, err := os.ReadFile(conf)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(
				"PHP %s 未安装或配置缺失：读不到 %s。"+
					"请先在「应用市场」安装该版本，或用「修复监听端点」重新生成配置",
				version, conf)
		}
		return "", fmt.Errorf("PHP %s 的配置文件 %s 读取失败: %w", version, conf, err)
	}

	raw, lineNo := findListenDirective(string(b))
	if raw == "" {
		return "", fmt.Errorf(
			"PHP %s 的 %s 里找不到有效的 listen 指令（文件存在但没有生效的 listen 行）。"+
				"请点「修复监听端点」让面板写入该版本专属的套接字地址",
			version, conf)
	}
	endpoint, err := NormalizeListen(raw)
	if err != nil {
		return "", fmt.Errorf(
			"PHP %s 的 %s 第 %d 行的 listen = %s 无法使用: %w",
			version, conf, lineNo, raw, err)
	}
	return endpoint, nil
}

// ResolveEndpoint 解析站点的 fastcgi_pass 目标，并且要求它**真的在监听**。
//
// 这是站点生成配置时用的口径：宁可明确报错，也不生成一份指向空气的 vhost
// （症状会是站点 502，而 502 的原因要翻 nginx 错误日志才能看出来）。
func ResolveEndpoint(brewPrefix, version string) (string, error) {
	endpoint, err := ResolveConfigEndpoint(brewPrefix, version)
	if err != nil {
		return "", err
	}
	if !EndpointLive(endpoint) {
		return "", fmt.Errorf(
			"PHP %s 配置的端点 %s 上没有进程在监听："+
				"该版本的 php-fpm 没在运行（或启动失败，例如端口/套接字被占用）。"+
				"请到「服务」页启动它，然后重试",
			version, endpoint)
	}
	return endpoint, nil
}

// NormalizeListen 把 www.conf 里的 listen 值规范成 nginx fastcgi_pass 可直接使用的形式。
//
//	9000                -> 127.0.0.1:9000      （php-fpm 的裸端口写法）
//	127.0.0.1:9000      -> 127.0.0.1:9000
//	/path/x.sock        -> unix:/path/x.sock   （php-fpm 的裸路径写法）
//	unix:/path/x.sock   -> unix:/path/x.sock
func NormalizeListen(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("listen 值为空")
	}
	if strings.HasPrefix(v, "unix:") {
		p := strings.TrimSpace(strings.TrimPrefix(v, "unix:"))
		if err := validateSocketPath(p); err != nil {
			return "", err
		}
		return "unix:" + p, nil
	}
	if strings.HasPrefix(v, "/") {
		if err := validateSocketPath(v); err != nil {
			return "", err
		}
		return "unix:" + v, nil
	}
	// 裸端口：php-fpm 允许 `listen = 9000`，相当于 127.0.0.1:9000
	if n, err := strconv.Atoi(v); err == nil {
		if n < 1 || n > 65535 {
			return "", fmt.Errorf("端口 %d 超出范围", n)
		}
		return fmt.Sprintf("127.0.0.1:%d", n), nil
	}
	if m := reListenTCP.FindStringSubmatch(v); m != nil {
		port, _ := strconv.Atoi(m[1])
		if port < 1 || port > 65535 {
			return "", fmt.Errorf("端口 %d 超出范围", port)
		}
		return v, nil
	}
	return "", fmt.Errorf("既不是绝对的 Unix socket 路径，也不是 host:port：%q", v)
}

// validateSocketPath 校验套接字绝对路径。
func validateSocketPath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("套接字路径必须是绝对路径：%q", p)
	}
	if !reListenAbs.MatchString(p) {
		return fmt.Errorf("套接字路径含非法字符（只允许字母数字与 . _ / -）：%q", p)
	}
	if len(p) > fpmSocketMaxLen {
		return fmt.Errorf("套接字路径过长（%d 字符，macOS 上限 %d）：%s", len(p), fpmSocketMaxLen, p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("套接字路径不是规范形式：%q", p)
	}
	return nil
}

// findListenDirective 在 www.conf 内容里找生效的 listen 指令。
//
// 返回指令值与行号（1 起，用于报错定位）。
// 已注释的行（`;listen = ...`）必须跳过 —— www.conf 出厂就带着一堆
// 注释的 listen.* 示例，把它们当成配置会读出根本不存在的端点。
func findListenDirective(content string) (string, int) {
	for i, ln := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		rest, okk := strings.CutPrefix(trimmed, "listen")
		if !okk {
			continue
		}
		// 必须真的是 `listen`，不能是 listen.owner / listen.group / listen.mode
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(rest, "="))
		// 行尾注释：`listen = 9000 ; 说明`
		if j := strings.IndexAny(v, ";#"); j >= 0 {
			v = strings.TrimSpace(v[:j])
		}
		if v != "" {
			return v, i + 1
		}
	}
	return "", 0
}

// EndpointLive 判断一个 FastCGI 端点当前是否真的有人监听。
//
// Unix socket 用 os.Stat 判类型（**不能只判存在**：残留的普通文件会让探测假阳性）；
// TCP 用 net.DialTimeout 真连一次。
func EndpointLive(endpoint string) bool {
	if strings.HasPrefix(endpoint, "unix:") {
		fi, err := os.Stat(strings.TrimPrefix(endpoint, "unix:"))
		if err != nil {
			return false
		}
		return fi.Mode()&os.ModeSocket != 0
	}
	if endpoint == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", endpoint, 700*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// EndpointIsPreferred 判断端点是否就是面板为该版本分配的那一个。
func EndpointIsPreferred(brewPrefix, version, endpoint string) bool {
	want, err := PreferredEndpoint(brewPrefix, version)
	if err != nil {
		return false
	}
	return want == endpoint
}

// NeedsListenFix 报告某版本是否**需要**把 www.conf 改成面板分配的端点。
//
// 两种情况需要修：
//   - www.conf 读不出端点（缺失/没有 listen 行）
//   - 读出来的端点与面板分配的不一致（典型就是出厂的 127.0.0.1:9000）
func NeedsListenFix(brewPrefix, version string) (bool, string) {
	ep, err := ResolveConfigEndpoint(brewPrefix, version)
	if err != nil {
		return true, err.Error()
	}
	// 特殊形态：`listen = unix:<路径>`。它能通过 ResolveConfigEndpoint（规范形式
	// 就是带 unix: 的），于是"端点看起来完全正确"，但 php-fpm 根本起不来
	// （invalid port value）。面板早期版本写过这种配置，必须在重跑时被改写掉，
	// 否则用户会看到"配置是对的、服务就是起不来"。
	if conf := FPMConfPath(brewPrefix, version); conf != "" {
		if b, rerr := os.ReadFile(conf); rerr == nil {
			if raw, _ := findListenDirective(string(b)); strings.HasPrefix(strings.TrimSpace(raw), "unix:") {
				return true, fmt.Sprintf(
					"当前 listen = %s 带了 `unix:` 前缀 —— php-fpm 只接受裸的绝对路径，"+
						"这个写法会让它启动时报 invalid port value", strings.TrimSpace(raw))
			}
		}
	}
	if EndpointIsPreferred(brewPrefix, version, ep) {
		return false, ""
	}
	want, _ := PreferredEndpoint(brewPrefix, version)
	return true, fmt.Sprintf("当前 listen = %s，面板为该版本分配的是 %s（多版本共用端点会互相抢）", ep, want)
}

// ---------- 落实监听端点（幂等写 www.conf）----------

// ListenFixResult 描述一次"确保监听端点"的结果，便于界面/日志如实说明。
type ListenFixResult struct {
	Version   string `json:"version"`
	Endpoint  string `json:"endpoint"`   // 生效后的监听端点
	Conf      string `json:"conf"`       // 改写的文件
	Changed   bool   `json:"changed"`    // 是否真的改了（false = 本来就对，幂等）
	Backup    string `json:"backup"`     // 首次改写的备份路径
	OwnerNote string `json:"owner_note"` // socket 属主是否被显式写进配置
}

// EnsureListenOptions 控制 EnsureListen 的行为。
type EnsureListenOptions struct {
	// SocketUser / SocketGroup 是写进 listen.owner / listen.group 的值。
	// 传空表示自己推导（先取 nginx.conf 的 user，再退回面板的运行用户）。
	// 这两个值让 nginx 一定能连上 socket —— 不写的话 php-fpm 按自己的
	// user/group 建 socket，某些组合下 nginx 会 permission denied。
	SocketUser  string
	SocketGroup string
	// NginxConfPath 用于推导 SocketUser。为空表示跳过推导。
	NginxConfPath string
	// PanelUser 是兜底属主（nginx 与面板同用户时可直接用）。
	PanelUser string
	// KeepBackup 为 false 时不写备份（测试用；生产必须为 true）。
	KeepBackup bool
	// NoBackup 为 true 时明确不写备份。默认写。
	NoBackup bool
}

// EnsureListen 把某版本的 php-fpm 配置改成监听面板分配的端点。
//
// 为什么要动 Homebrew 自己的 www.conf：
//
//	php-fpm.conf 里写着 `include=<prefix>/etc/php/<版本>/php-fpm.d/*.conf`，
//	而池配置（[www]）只认一次 listen。想"多版本各听各的"，就必须让每个版本的
//	www.conf 里的 listen 各不相同。想着"再丢一个 *.conf 覆盖它"是行不通的：
//	php-fpm 不允许重复的池名，换个池名则会**再开一个**监听池，
//	结果是同一个版本听两个地址、且默认池仍然是 9000。
//
// 幂等性：已经把 listen 写成目标值（且属主一致）时什么都不做。
// 安全性：整文件原子替换（写临时文件 + rename），首次改写前留一份备份。
func EnsureListen(brewPrefix, version string, opt EnsureListenOptions) (*ListenFixResult, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, errors.New("PHP 版本号为空，无法配置监听端点")
	}
	target, err := PreferredEndpoint(brewPrefix, version)
	if err != nil {
		return nil, err
	}
	conf := FPMConfPath(brewPrefix, version)
	if _, err := os.Stat(conf); err != nil {
		return nil, fmt.Errorf(
			"PHP %s 的配置文件不存在：%s（该版本未安装？）: %w", version, conf, err)
	}

	b, err := os.ReadFile(conf)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", conf, err)
	}
	content := string(b)

	// 套接字属主：让 nginx 一定能连上
	sockUser, sockGroup := opt.SocketUser, opt.SocketGroup
	if sockUser == "" {
		sockUser = detectNginxUser(opt.NginxConfPath)
	}
	if sockUser == "" {
		sockUser = strings.TrimSpace(opt.PanelUser)
	}
	if sockGroup == "" && sockUser != "" {
		sockGroup = groupOfUser(sockUser)
	}

	// Unix socket 时顺带把套接字属主/mode 写进配置。
	// TCP 不需要（不涉及文件权限）。
	setOwner := strings.HasPrefix(target, "unix:") && sockUser != ""

	// 组名解析不出来时**绝不写空值**。
	//
	// 真机事故（2026-09-17 mini）：写出来的 `listen.group = `（等号后什么都没有）
	// 会让 php-fpm **启动直接失败**：
	//   ERROR: [www.conf:492] value is NULL for a ZEND_INI_PARSER_ENTRY
	//   ERROR: failed to load configuration file .../php-fpm.conf
	// 而 `php-fpm -t`（配置检查）当时还报 "test is successful" —— 也就是说
	// 光靠语法检查发现不了，必须真启动一次。macOS 上用户的主组名不在
	// /etc/group 的成员列表里，最容易踩这个坑。
	// 拿不到组名就不写这一行：php-fpm 会用**进程自身的组**，而我们的场景里
	// nginx 与 php-fpm 跑在同一个用户下，属主位（listen.owner）就够了。
	setGroup := setOwner && strings.TrimSpace(sockGroup) != ""

	// 套接字所在目录**必须先存在，且 php-fpm 的运行用户可写**。
	//
	// 踩过的坑（真机上会遇到）：php-fpm 是在**启动时** bind 这个 socket 的，
	// 目录不存在它直接起不来；如果目录存在但不是运行用户可写（例如被 root
	// 以 755 建出来），同样 bind 失败（Permission denied），而那时的日志
	// 只说 "unable to bind listening socket"，看不出是目录权限问题。
	if strings.HasPrefix(target, "unix:") {
		if err := ensureSocketDir(strings.TrimPrefix(target, "unix:")); err != nil {
			return nil, err
		}
	}

	// **写进 php-fpm 的必须是裸的绝对路径**：它不认 nginx 那种 `unix:` 前缀，
	// 写了前缀会在启动时报
	//   ERROR: invalid port value '/path/to/x.sock'
	//   ERROR: FPM initialization failed
	// （2026-09-17 用真 php-fpm 8.3.33 实测；`php-fpm -t` 当时还报 successful，
	// 所以只有"真启动一次"能发现 —— 单测也因此必须真启动一个 fpm 进程）。
	// 对外（nginx 的 fastcgi_pass、面板界面）仍用带 unix: 的规范形式。
	writeTarget := target
	if strings.HasPrefix(target, "unix:") {
		writeTarget = strings.TrimPrefix(target, "unix:")
	}
	out, changed := rewriteListenConfig(content, writeTarget, sockUser, sockGroup, setOwner, setGroup)
	if !changed {
		return &ListenFixResult{
			Version: version, Endpoint: target, Conf: conf, Changed: false,
		}, nil
	}
	if err := writeFileAtomicPreserve(conf, out); err != nil {
		return nil, fmt.Errorf("写入 %s 失败: %w", conf, err)
	}

	res := &ListenFixResult{Version: version, Endpoint: target, Conf: conf, Changed: true}
	if setOwner {
		if setGroup {
			res.OwnerNote = fmt.Sprintf("listen.owner=%s listen.group=%s listen.mode=0660", sockUser, sockGroup)
		} else {
			res.OwnerNote = fmt.Sprintf("listen.owner=%s listen.mode=0660（组名不可知，未写 listen.group）", sockUser)
		}
	}
	// 备份放在写入之后：即使备份失败，配置也已经生效了（不因为备份问题谎报失败）。
	if !opt.NoBackup {
		if bak, err := backupOnce(conf, content); err == nil {
			res.Backup = bak
		}
	}
	return res, nil
}

// ensureSocketDir 保证套接字目录存在。
//
// 属主调整不在这里做（sites 包不碰系统用户查询），由 web 层用 lookupIDs
// 在写完后按需 chown（与"站点根目录归属真实用户"是同一套做法）。
// 这里的 0755 对"php-fpm 与目录属主同用户"这一常见情形已经足够。
func ensureSocketDir(sockPath string) error {
	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 php-fpm 套接字目录 %s 失败: %w", dir, err)
	}
	return nil
}

// rewriteListenConfig 把 [www] 段里的 listen（以及可选的 listen.owner/group/mode）
// 改写成目标值。返回新内容与"是否发生了变化"。
//
// 只动 [www] 段：www.conf 里可能有多个池（少见但合法），
// 全局段或其它池里的 listen 不该被我们碰。
func rewriteListenConfig(content, target, sockUser, sockGroup string, setOwner, setGroup bool) (string, bool) {
	lines := strings.Split(content, "\n")
	section := ""
	doneListen, doneOwner, doneGroup, doneMode := false, false, false, false
	changed := false

	setKV := func(line, key, val string) (string, bool) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			return line, false
		}
		if trimmed != key {
			if !strings.HasPrefix(trimmed, key) {
				return line, false
			}
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
			if !strings.HasPrefix(rest, "=") {
				return line, false
			}
		}
		// 保留原有的缩进风格
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		nl := strings.HasSuffix(line, "\r")
		newLine := indent + key + " = " + val
		if nl {
			newLine += "\r"
		}
		return newLine, true
	}

	var out []string
	for _, ln := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		// 修复历史遗留的 `#` 注释（本面板早期版本往 php-fpm 配置里写过一行
		// `# 由 ZizPanel 补齐：…`）。php-fpm 的 INI 解析器**不认 `#`**，
		// 遇到它就报 `value is NULL for a ZEND_INI_PARSER_ENTRY` 并拒绝启动
		// ——而这一行不修掉，文件里其余内容改得再对也白搭（真机 2026-09-17
		// mini 上 php-fpm 一直在 crash-loop，第 492 行就是它）。
		// 只动我们自己写的那一行（含 ZizPanel 标记），不碰用户的内容。
		if strings.HasPrefix(trimmed, "#") && strings.Contains(trimmed, "ZizPanel") {
			changed = true
			out = append(out, "; "+strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
			out = append(out, ln)
			continue
		}
		if section != "www" {
			out = append(out, ln)
			continue
		}
		if v, okk := setKV(ln, "listen", target); okk {
			// 注意：`listen.owner` 不会被这里命中（setKV 要求 key 后紧跟 = 或整行相等）
			doneListen = true
			if v != ln {
				changed = true
			}
			out = append(out, v)
			continue
		}
		if setOwner {
			if v, okk := setKV(ln, "listen.owner", sockUser); okk {
				doneOwner = true
				if v != ln {
					changed = true
				}
				out = append(out, v)
				continue
			}
			if setGroup {
				if v, okk := setKV(ln, "listen.group", sockGroup); okk {
					doneGroup = true
					if v != ln {
						changed = true
					}
					out = append(out, v)
					continue
				}
			} else if isEmptyKV(trimmed, "listen.group") {
				// 修掉历史遗留 / 别处写下的空值：空值让 php-fpm 起不来，
				// 注释掉（php-fpm 用进程自身的组）是唯一安全的修法。
				doneGroup = true
				changed = true
				out = append(out, "; "+trimmed)
				continue
			}
			if v, okk := setKV(ln, "listen.mode", "0660"); okk {
				doneMode = true
				if v != ln {
					changed = true
				}
				out = append(out, v)
				continue
			}
		}
		out = append(out, ln)
	}

	// 目标文件里没有这些指令时补写（例如 www.conf 被精简过）。
	// 补写位置放在文件末尾的 [www] 段之后：追加在最后一行前面，
	// 保证仍处于 [www] 段内（文件末尾没有新的段头就算是同段）。
	var add []string
	if !doneListen {
		add = append(add, "listen = "+target)
	}
	if setOwner {
		if !doneOwner {
			add = append(add, "listen.owner = "+sockUser)
		}
		// 组名不可知时**不补这一行**（空值会让 php-fpm 启动失败，见 EnsureListen 注释）
		if setGroup && !doneGroup {
			add = append(add, "listen.group = "+sockGroup)
		}
		if !doneMode {
			add = append(add, "listen.mode = 0660")
		}
	}
	if len(add) > 0 {
		changed = true
		// 注释必须用 `;`：php-fpm 的 INI 解析器**不认 `#`**，会把它当成一条
		// 值缺失的配置项，直接报
		//   ERROR: [www.conf:492] value is NULL for a ZEND_INI_PARSER_ENTRY
		//   ERROR: FPM initialization failed
		// 2026-09-17 mini 真机就是被这一行卡死的（面板把 `#` 注释当成人读的说明写进去，
		// 而 php-fpm 完全起不来）；`php-fpm -t` 也照样报 successful。
		out = append(out, "; 由 ZizPanel 补齐：多版本 PHP 各自监听独立端点", "")
		out = append(out, add...)
	}
	return strings.Join(out, "\n"), changed
}

// isEmptyKV 判断一行是否是"值被写了但等于空"的指令，例如 `listen.group = `。
//
// 这类行在 php-fpm 里是**致命**的（value is NULL for a ZEND_INI_PARSER_ENTRY），
// 必须能被识别出来并修掉。
func isEmptyKV(trimmed, key string) bool {
	if !strings.HasPrefix(trimmed, key) {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
	if !strings.HasPrefix(rest, "=") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(rest, "=")) == ""
}

// writeFileAtomicPreserve 原子替换文件内容，尽量保留原权限与属主。
//
// 与面板写 nginx vhost 用同一套思路：先写同目录临时文件再 rename，
// 避免"写了一半"留下半个配置文件（php-fpm 会直接起不来）。
func writeFileAtomicPreserve(path, content string) error {
	fi, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".zp-fpm-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

// backupOnce 为文件留一份一次性备份（已存在同名备份就不覆盖）。
//
// 为什么不带时间戳：面板反复点"修复监听端点"会刷出一串时间戳备份（本项目
// 实测的 php-fpm.d 目录里就有一堆 www.conf.bak.<时间>）。
// 留"最初那一份"才有意义 —— 它是唯一能回到出厂配置的凭据。
func backupOnce(path, content string) (string, error) {
	bak := path + ".zizpanel.bak"
	if _, err := os.Stat(bak); err == nil {
		return bak, nil
	}
	if err := os.WriteFile(bak, []byte(content), 0o644); err != nil {
		return "", err
	}
	return bak, nil
}

// ---------- 从 nginx.conf 推导 socket 属主 ----------

// nginxUserPattern 匹配 nginx.conf 顶层的 `user  <用户名> <组名>;`。
var nginxUserPattern = regexp.MustCompile(`(?m)^\s*user\s+([A-Za-z0-9._-]+)(?:\s+([A-Za-z0-9._-]+))?\s*;`)

// detectNginxUser 从 nginx.conf 里读 worker 的运行用户。
//
// 为什么需要它：Unix socket 的权限由 php-fpm 建 socket 时决定。
// 如果这个版本的池跑在别的用户下（实测 php@8.4 的出厂配置是 `user = _www`，
// 而 nginx worker 跑在**另一个**用户下），nginx 连 socket 会 permission denied。
// 与其让用户去猜，不如把 nginx 的用户显式写进 listen.owner。
func detectNginxUser(nginxConfPath string) string {
	if strings.TrimSpace(nginxConfPath) == "" {
		return ""
	}
	b, err := os.ReadFile(nginxConfPath)
	if err != nil {
		return ""
	}
	m := nginxUserPattern.FindStringSubmatch(string(b))
	if m == nil {
		return ""
	}
	return m[1]
}

// groupOfUser 尽力取用户的主组名；取不到就返回空（配置里会省略 group）。
func groupOfUser(name string) string {
	b, err := os.ReadFile("/etc/group")
	if err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			parts := strings.Split(ln, ":")
			if len(parts) < 4 {
				continue
			}
			for _, m := range strings.Split(parts[3], ",") {
				if strings.TrimSpace(m) == name {
					return parts[0]
				}
			}
		}
	}
	// 成员列表里没有：macOS 上**大多数用户的主组不会出现在成员列表里**
	// （zizdog 的主组是 staff，而 /etc/group 的 staff 行成员为空）。
	// 这时用主 gid 反查组名 —— 组名必须能查出来，否则只能不写 listen.group。
	if u, err := user.Lookup(name); err == nil && u.Gid != "" {
		if g, gerr := user.LookupGroupId(u.Gid); gerr == nil && g.Name != "" {
			return g.Name
		}
	}
	// 查不到就留空（调用方会明确不写这个字段，而不是写一个空值）
	return ""
}
