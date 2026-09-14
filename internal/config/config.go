// Package config 负责面板配置的加载、默认值与持久化。
//
// 设计要点：
//   - 配置文件是 JSON，落在数据目录（默认 /opt/zizpanel/data/config.json）。
//   - 首次启动由 Bootstrap 生成，并写入随机 JWT/会话密钥。
//   - 所有路径集中在这里，其他模块不再硬编码系统路径。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Config 是面板的完整运行时配置。
type Config struct {
	// ---------- 面板自身 ----------
	Version   string `json:"version"`
	DataDir   string `json:"data_dir"`   // 数据目录（db、证书、备份清单）
	LogDir    string `json:"log_dir"`    // 日志目录
	RunDir    string `json:"run_dir"`    // 运行时文件（pid、socket）
	WorkDir   string `json:"work_dir"`   // 工作目录（compose 文件、服务代码）
	BinDir    string `json:"bin_dir"`    // 安装目录下的 bin
	Secret    string `json:"secret"`     // 会话签名密钥，首次启动随机生成
	InstallID string `json:"install_id"` // 安装唯一标识，用于遥测/迁移识别

	// ---------- 数据库连接（面板管理 MySQL 用）----------
	//
	// 为什么必须可配置：早期只从 ~/www/.env.local 读 root 密码 ——
	// 那是某个具体项目的约定。换一台机器（或那台机器的 MySQL root 有密码
	// 而文件不存在）就永远连不上，界面上只显示"无法连接 MySQL"，
	// 用户完全不知道该怎么办。真机上就是这么卡住的。
	// 现在：面板里可以直接填，并且会持久化。
	MySQLHost     string `json:"mysql_host"`
	MySQLPort     int    `json:"mysql_port"`
	MySQLSocket   string `json:"mysql_socket"`
	MySQLUser     string `json:"mysql_user"`
	MySQLPassword string `json:"mysql_password"`

	// ---------- 在线升级 ----------
	// UpgradeSource 是升级源地址（放 manifest.json / manifest.json.sig 的目录）。
	// 留空表示不启用网络升级，此时只能用手动上传升级包那条离线路径。
	UpgradeSource string `json:"upgrade_source"`
	// UpgradeNotes 缓存最近一次检查更新拿到的发布说明，供界面展示。
	UpgradeNotes string `json:"upgrade_notes"`

	// ---------- 监听 ----------
	Listen    string `json:"listen"`     // 面板监听地址，如 :8443
	TLSEnable bool   `json:"tls_enable"` // 是否启用 HTTPS（自签或自有证书）
	TLSCert   string `json:"tls_cert"`
	TLSKey    string `json:"tls_key"`
	// AccessMode: any=任意来源, local=仅本机, whitelist=仅白名单
	AccessMode  string   `json:"access_mode"`
	IPWhitelist []string `json:"ip_whitelist"`
	// TrustProxy 为 true 时，从 X-Forwarded-For 取真实 IP（面板挂在 nginx 后面时开）。
	TrustProxy bool `json:"trust_proxy"`

	// ---------- Web 终端 ----------
	// TerminalEnabled 默认 false：终端等于把 shell 交给浏览器，
	// 必须由用户显式开启，而不是"装好就有"。
	TerminalEnabled     bool   `json:"terminal_enabled"`
	TerminalShell       string `json:"terminal_shell"`
	TerminalIdleMins    int    `json:"terminal_idle_mins"`
	TerminalMaxSessions int    `json:"terminal_max_sessions"`
	// FileRoots 是文件管理器允许访问的根目录（留空则用默认集合）
	FileRoots []string `json:"file_roots"`

	// ---------- 安全 ----------
	SessionHours   int    `json:"session_hours"`   // 会话有效期
	LoginMaxFail   int    `json:"login_max_fail"`  // 连续失败几次锁定
	LoginLockMins  int    `json:"login_lock_mins"` // 锁定时长（分钟）
	Require2FA     bool   `json:"require_2fa"`     // 强制所有账号开启两步验证
	PanelPublicURL string `json:"panel_public_url"`
	// AppProxy 控制"把有界面的应用挂到 /<slug>/ 下"这个能力（默认开）。
	//
	// 为什么做成开关：面板的 /<slug>/ 是**公开路径**（应用自己鉴权，面板不拦），
	// 于是它对"把面板暴露到公网"的用户来说等于多开了一组入口。
	// 默认开是因为直连端口本来就在局域网上开着、且这是用户明确要的便利；
	// 但只要用户觉得不合适，一个开关就能全关掉（页面上的入口也会跟着消失）。
	AppProxy bool `json:"app_proxy"`

	// ---------- 环境（LNMP 等由面板管理的系统组件） ----------
	User     string `json:"user"`      // 面板运行用户（安装时确定）
	UserHome string `json:"user_home"` // 该用户家目录
	UserUID  int    `json:"user_uid"`  // 该用户 uid（launchd gui/<uid> 域需要）
	WWWRoot  string `json:"www_root"`  // 网站根目录，如 /Users/zizdog/www

	BrewPrefix string `json:"brew_prefix"` // /opt/homebrew
	BrewBin    string `json:"brew_bin"`
	NginxBin   string `json:"nginx_bin"`
	NginxConf  string `json:"nginx_conf"`
	VhostDir   string `json:"vhost_dir"`
	LogRoot    string `json:"log_root"`
	PHPSvc     string `json:"php_svc"` // 默认 PHP 服务名，如 php@8.3
	PHPVer     string `json:"php_ver"`
	PHPEtc     string `json:"php_etc"`
	MySQLSvc   string `json:"mysql_svc"`
	MySQLBin   string `json:"mysql_bin"`
	PmaDir     string `json:"pma_dir"`

	// DockerSocket 为空表示 Docker 不可用。
	DockerSocket string `json:"docker_socket"`

	mu   sync.RWMutex
	path string
}

// DefaultRoot 是面板的默认安装根目录。
const DefaultRoot = "/opt/zizpanel"

// root 返回面板安装根目录。
//
// 支持环境变量重定位有两个实际用途：
//   - 把面板装到外置盘或其他路径（改 ZIZPANEL_ROOT 即可）
//   - 单元测试隔离，避免触碰 /opt 下的真实数据
func root() string {
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	return DefaultRoot
}

// DefaultConfigPath 返回默认配置文件路径（受 ZIZPANEL_ROOT 影响）。
func DefaultConfigPath() string {
	return filepath.Join(root(), "data", "config.json")
}

// PanelUser 返回面板应当使用的"真实用户"。
//
// 探测顺序（重要性递减）：
//  1. ZIZPANEL_USER  —— 安装脚本写入 launchd 环境变量。
//     面板以 root 运行时 $HOME 是 /var/root、$SUDO_USER 为空，
//     只有这个变量能可靠告知"网站目录该属于谁"。
//  2. SUDO_USER / SUDO_UID —— 通过 sudo 直接运行时可用。
//  3. 当前用户（非 root 时）。
//
// 如果全都没有，才退回 root。这一步错了会导致网站根目录算成
// /var/root/www，所有站点都找不到 —— 所以顺序不能随意调整。
func PanelUser() string {
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_USER")); v != "" && v != "root" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("SUDO_USER")); v != "" && v != "root" {
		return v
	}
	if cu, err := user.Current(); err == nil && cu.Username != "root" {
		return cu.Username
	}
	if cu, err := user.LookupId(os.Getenv("SUDO_UID")); err == nil && cu.Username != "root" {
		return cu.Username
	}
	return "root"
}

// Default 返回一份默认配置，账号与网站目录基于真实用户推导。
func Default() *Config {
	u := PanelUser()
	home := ""
	if cu, err := user.Lookup(u); err == nil {
		home = cu.HomeDir
	}
	if home == "" {
		home = os.Getenv("HOME")
	}

	brew := "/opt/homebrew"
	if _, err := os.Stat(brew); err != nil {
		brew = "/usr/local" // Intel Mac 或自定义安装
	}

	r := root()
	c := &Config{
		Version:    "0.1.0",
		DataDir:    filepath.Join(r, "data"),
		LogDir:     filepath.Join(r, "logs"),
		RunDir:     filepath.Join(r, "run"),
		WorkDir:    filepath.Join(r, "work"),
		BinDir:     filepath.Join(r, "bin"),
		Listen:     ":8443",
		TLSEnable:  true,
		AccessMode: "any",
		// 有界面的应用默认挂到 /<slug>/ 下（用户明确要求；可在设置里关掉）
		AppProxy:      true,
		SessionHours:  72,
		LoginMaxFail:  5,
		LoginLockMins: 15,
		// 终端默认关闭；文件管理器默认只开放网站目录与面板目录
		TerminalEnabled:     false,
		TerminalIdleMins:    30,
		TerminalMaxSessions: 3,
		MySQLHost:           "127.0.0.1",
		MySQLPort:           3306,
		MySQLSocket:         "/tmp/mysql.sock",
		MySQLUser:           "root",
		User:                u,
		UserHome:            home,
		UserUID:             lookupUID(u),
		WWWRoot:             filepath.Join(home, "www"),
		BrewPrefix:          brew,
		BrewBin:             filepath.Join(brew, "bin", "brew"),
		NginxBin:            filepath.Join(brew, "bin", "nginx"),
		NginxConf:           filepath.Join(brew, "etc", "nginx", "nginx.conf"),
		VhostDir:            filepath.Join(brew, "etc", "nginx", "vhosts"),
		LogRoot:             filepath.Join(home, "www", "_logs"),
		PHPSvc:              "php@8.3",
		PHPVer:              "8.3",
		PHPEtc:              filepath.Join(brew, "etc", "php", "8.3"),
		MySQLSvc:            "mysql@8.4",
		MySQLBin:            filepath.Join(brew, "opt", "mysql@8.4", "bin", "mysql"),
		PmaDir:              filepath.Join(brew, "share", "phpmyadmin"),
		DockerSocket:        "/var/run/docker.sock",
	}
	c.TLSCert = filepath.Join(c.DataDir, "tls", "panel.crt")
	c.TLSKey = filepath.Join(c.DataDir, "tls", "panel.key")
	return c
}

// Load 读取配置文件；不存在则返回错误 ErrNotInstalled。
var ErrNotInstalled = errors.New("面板尚未初始化：配置文件不存在")

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotInstalled
		}
		return nil, err
	}
	c := Default()
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("配置文件解析失败 %s: %w", path, err)
	}
	c.path = path
	c.fill()
	return c, nil
}

// Bootstrap 首次初始化：确保目录存在、生成密钥、写出配置文件。
// 已存在配置文件时直接加载，不会覆盖。
//
// 关键行为：数据/日志/运行等目录都相对"配置文件所在目录"推导，
// 而不是固定用 /opt/zizpanel。这样 --config 指向别处时，
// 面板会完整地装在那个位置，既不会误改 /opt 下的目录，也不会因权限失败。
func Bootstrap(path string) (*Config, bool, error) {
	if c, err := Load(path); err == nil {
		return c, false, nil
	} else if !errors.Is(err, ErrNotInstalled) {
		return nil, false, err
	}

	c := Default()
	c.path = path
	c.Secret = randomHex(32)
	c.InstallID = randomHex(8)

	// 以配置文件所在目录作为本实例的数据目录，兄弟目录作为 logs/run/work/bin
	base := filepath.Dir(path)
	parent := filepath.Dir(base)
	c.DataDir = base
	c.LogDir = filepath.Join(parent, "logs")
	c.RunDir = filepath.Join(parent, "run")
	c.WorkDir = filepath.Join(parent, "work")
	c.BinDir = filepath.Join(parent, "bin")
	c.TLSCert = filepath.Join(base, "tls", "panel.crt")
	c.TLSKey = filepath.Join(base, "tls", "panel.key")
	c.fill()

	// 先建目录再写配置：Save 需要目录已存在
	for _, d := range c.allDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, false, fmt.Errorf("创建目录失败 %s: %w", d, err)
		}
	}
	if err := c.Save(); err != nil {
		return nil, false, err
	}
	// 首次初始化时也要修正数据目录归属（Save 只处理文件与父目录）
	c.fixDataDirOwnership()
	return c, true, nil
}

// fixDataDirOwnership 让数据目录归属真实用户，便于用户查看与备份。
func (c *Config) fixDataDirOwnership() {
	if os.Geteuid() != 0 {
		return
	}
	uid, gid := c.ownerIDs()
	if uid == 0 {
		return
	}
	for _, d := range []string{c.DataDir, filepath.Join(c.DataDir, "tls")} {
		_ = os.Chown(d, uid, gid)
	}
}

// fill 补齐缺失字段（老版本配置升级用），保证默认值不被空串覆盖。
func (c *Config) fill() {
	d := Default()
	if c.Secret == "" {
		c.Secret = randomHex(32)
	}
	if c.InstallID == "" {
		c.InstallID = randomHex(8)
	}
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.SessionHours <= 0 {
		c.SessionHours = d.SessionHours
	}
	if c.LoginMaxFail <= 0 {
		c.LoginMaxFail = d.LoginMaxFail
	}
	if c.LoginLockMins <= 0 {
		c.LoginLockMins = d.LoginLockMins
	}
	if c.AccessMode == "" {
		c.AccessMode = d.AccessMode
	}
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.BrewPrefix == "" {
		c.BrewPrefix = d.BrewPrefix
	}
	if c.DockerSocket == "" {
		c.DockerSocket = d.DockerSocket
	}
	if c.UserHome == "" {
		c.UserHome = d.UserHome
	}
	if c.UserUID == 0 {
		c.UserUID = lookupUID(c.User)
	}
	if c.WWWRoot == "" {
		c.WWWRoot = d.WWWRoot
	}
	if c.RunDir == "" {
		c.RunDir = d.RunDir
	}
	// 老配置文件没有这些字段：补齐默认，避免 0 端口之类的问题
	if c.MySQLHost == "" {
		c.MySQLHost = d.MySQLHost
	}
	if c.MySQLPort == 0 {
		c.MySQLPort = d.MySQLPort
	}
	if c.MySQLSocket == "" {
		c.MySQLSocket = d.MySQLSocket
	}
	if c.MySQLUser == "" {
		c.MySQLUser = d.MySQLUser
	}
	if c.WorkDir == "" {
		c.WorkDir = d.WorkDir
	}
	if c.LogDir == "" {
		c.LogDir = d.LogDir
	}
	if c.TLSCert == "" {
		c.TLSCert = filepath.Join(c.DataDir, "tls", "panel.crt")
	}
	if c.TLSKey == "" {
		c.TLSKey = filepath.Join(c.DataDir, "tls", "panel.key")
	}
}

func (c *Config) allDirs() []string {
	return []string{
		c.DataDir, c.LogDir, c.RunDir, c.WorkDir, c.BinDir,
		filepath.Join(c.DataDir, "tls"),
		filepath.Join(c.WorkDir, "compose"),
		filepath.Join(c.WorkDir, "services"),
		filepath.Join(c.WorkDir, "backup"),
	}
}

// EnsureDirs 在每次启动时调用，防止用户手工删掉目录。
//
// 同时顺带修正配置文件与数据目录的归属：老版本安装可能留下
// root 拥有的 config.json，导致普通用户执行 `zizpanel status`
// 报权限错误。放在启动路径上可以自愈，不需要用户重装。
func (c *Config) EnsureDirs() error {
	for _, d := range c.allDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建目录失败 %s: %w", d, err)
		}
	}
	// 自愈：修正归属（仅在以 root 运行时生效）
	if path := c.Path(); path != "" {
		if _, err := os.Stat(path); err == nil {
			c.fixOwnership(path)
		}
	}
	c.fixDataDirOwnership()
	// 日志目录也要让真实用户能读（排障时经常需要直接看日志）
	if os.Geteuid() == 0 {
		if uid, gid := c.ownerIDs(); uid != 0 {
			for _, d := range []string{c.LogDir, c.RunDir, c.WorkDir} {
				_ = os.Chown(d, uid, gid)
			}
		}
	}
	return nil
}

// Save 原子写回配置文件（先写临时文件再 rename，避免半截文件）。
func (c *Config) Save() error {
	c.mu.RLock()
	b, err := json.MarshalIndent(c, "", "  ")
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	path := c.Path()
	if path == "" {
		return errors.New("配置路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	c.fixOwnership(path)
	return nil
}

// fixOwnership 让配置文件归属真实用户。
//
// 为什么需要：面板由 LaunchDaemon 以 root 启动，首次初始化时写出的
// config.json 属于 root 且权限 0600。于是普通用户在终端执行
// `zizpanel status` 会得到 "permission denied" —— 而这条命令
// 恰恰是在面板打不开时用来排障的，读不到配置就失去了意义。
//
// 只改归属、不放宽权限：文件仍然只有真实用户与 root 可读，
// 而里面含有会话签名密钥，不应该让所有用户都读到。
func (c *Config) fixOwnership(path string) {
	if os.Geteuid() != 0 {
		return // 非 root 时文件本来就属于当前用户
	}
	uid, gid := c.ownerIDs()
	if uid == 0 {
		return
	}
	_ = os.Chown(path, uid, gid)
	// 目录本身也让真实用户可以进入（否则连遍历都做不到）
	if dir := filepath.Dir(path); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.Mode().Perm()&0o100 == 0 {
			_ = os.Chmod(dir, 0o700)
		}
		_ = os.Chown(dir, uid, gid)
	}
}

// ownerIDs 返回真实用户的 uid/gid。
func (c *Config) ownerIDs() (int, int) {
	if c.User == "" || c.User == "root" {
		return 0, 0
	}
	u, err := user.Lookup(c.User)
	if err != nil {
		return 0, 0
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return uid, gid
}

// Path 返回配置文件路径。
func (c *Config) Path() string {
	if c.path != "" {
		return c.path
	}
	return filepath.Join(c.DataDir, "config.json")
}

// SetPath 用于安装阶段指定非默认路径。
func (c *Config) SetPath(p string) { c.path = p }

// ---------- 小工具 ----------

// Port 从 Listen 中解析端口号。
func (c *Config) Port() int {
	_, p, err := splitHostPort(c.Listen)
	if err != nil {
		return 0
	}
	return p
}

func splitHostPort(addr string) (string, int, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("非法监听地址: %s", addr)
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0, err
	}
	return addr[:i], p, nil
}

// ServicePath 把服务名（如 nginx）解析为面板 bin 目录下的包装脚本路径。
func (c *Config) ServicePath(name string) string {
	return filepath.Join(c.BinDir, name)
}

// lookupUID 把用户名解析为 uid，失败返回 0。
func lookupUID(name string) int {
	if name == "" || name == "root" {
		return 0
	}
	if u, err := user.Lookup(name); err == nil {
		if n, err := strconv.Atoi(u.Uid); err == nil {
			return n
		}
	}
	return 0
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于系统级异常，退化为时间戳也要保证能启动
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}

// RepairRuntimeAccess 在启动最早期修复"面板自己不在意的那些文件"的归属与权限。
//
// 为什么需要它（真机事故，不是理论问题）：
//
//	面板自身的文件有三类身份来源 —— 安装脚本以 root 写、面板进程按 plist 运行、
//	某些操作又按"真实用户"（config.user）落地。只要这三者漂移一次，就会出现：
//	  · TLS 私钥是 root:600，而这次面板以 zizdog 身份被拉起 → 读证书时报
//	    permission denied，进程在**写下任何日志之前**就退出（launchd 只报
//	    `last exit code = 78: EX_CONFIG`），远程看到的现象就是"升级完面板没了"。
//	  · 日志文件属于另一个身份 → logx.Init 打不开，同样在启动最早期失败。
//
//	两种情况的共同点是：**面板还没有日志可看**，所以排查成本极高。
//	这里在打开日志之前先自我修复一次；修不动的只记警告，绝不阻断启动。
//
// 只在以 root 运行时才改归属：普通用户没有权限，也没必要。
func (c *Config) RepairRuntimeAccess() []string {
	// 面板这次以什么身份在跑：root（uid 0）就不必改归属 —— root 本来就能读能写。
	if os.Geteuid() == 0 {
		return nil
	}
	return c.repairRuntimeAccessFor(os.Geteuid(), os.Getegid())
}

// repairRuntimeAccessFor 是 RepairRuntimeAccess 的可测内核：
// 身份由参数传入，测试里可以模拟"面板不是以这个文件的属主身份运行"。
func (c *Config) repairRuntimeAccessFor(uid, gid int) []string {
	var warnings []string

	// heal 尝试把一个文件修成"当前身份可读/可写"，失败时把原因写进 warnings。
	//
	// 关键：**不能用归属来判断是否已达目标**。文件属于自己、但权限位被改成
	// 000 的情况真实存在（安装脚本、手工 chmod、编辑器另存都可能造成），
	// 那时它依然读不到 —— 而这正是面板会在启动最早期静默退出的原因。
	// 所以一律以"真的能不能打开"为准。
	//
	// 两种修法，按代价从低到高：
	//  1. 自己就是属主 → chmod 恢复权限即可（不需要 root）
	//  2. 不是属主     → 得 chown；非 root 身份做不了，只能如实报出可执行指令
	heal := func(path, what string, mode os.FileMode, usable func(string) bool) {
		if usable(path) {
			return
		}
		if isOwnedBy(path, uid) {
			if err := os.Chmod(path, mode); err == nil && usable(path) {
				warnings = append(warnings, "已修复 "+path+" 的权限（"+what+"，原权限无法打开）")
				return
			}
		}
		if err := os.Chown(path, uid, gid); err == nil && usable(path) {
			warnings = append(warnings, "已把 "+path+" 的归属修正为当前运行身份（"+what+"）")
			return
		}
		warnings = append(warnings, path+" "+what+"，且无法自动修复。请以 root 执行："+
			" chown -R "+c.User+" "+filepath.Dir(path))
	}

	// 日志与运行时文件：打不开日志会让 logx.Init 直接失败，同样发生在最早期。
	for _, dir := range []string{c.LogDir, c.RunDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			heal(p, "不可写", 0o644, canWrite)
		}
	}

	// TLS 私钥与证书：读不到就必然起不来，且失败得太早、日志都来不及写。
	// 还没生成的（首次安装）直接跳过，交给 tlsx 自己生成。
	for _, f := range []string{c.TLSKey, c.TLSCert} {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			continue
		}
		heal(f, "不可读（TLS 起不来）", 0o600, canRead)
	}

	return warnings
}

// isOwnedBy 判断文件属主是否为指定 uid。
func isOwnedBy(path string, uid int) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(sys.Uid) == uid
}

// canRead / canWrite 用"真的打开一次"来判断权限，
// 而不是解析 mode 位 —— ACL、继承位、SIP 这些用 mode 位都算不准。
func canRead(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func canWrite(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
