package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  aria2（下载器）—— 面板安装器
//
//  为什么不能走通用 brew 流程（与 stt 同一类理由）：
//    · brew 的 aria2 formula **没有 service 块**（`brew info --json=v2` 里
//      service 为 null）⇒ `brew services start aria2` 起不来，通用流程会留下
//      "装了但启动失败"的假警告 + 一条永远没有状态的假记录；
//    · 它本体只是一个 RPC 服务（jsonrpc 6800），界面（AriaNg）由**面板托管**
//      （见 internal/web/aria2_web.go，挂在 /aria/）；
//    · 下载目录、RPC 密钥这些收尾必须真复核（拿 aria2.getVersion 真的问一次），
//      通用流程只看得见 brew 的退出码。
//
//  规矩（与 stt / transmission 一致）：
//    · 面板以 root 往用户家目录写完，**立刻把归属交还真实用户**；
//    · 复核贴着运行体：RPC 真的回版本号才算装好，端口在听不算；
//    · 卸载**绝不碰** ~/Downloads（那是用户的文件）。
// ============================================================================

const (
	// Aria2AppID 是目录里的应用 ID。
	Aria2AppID = "aria2"
	// Aria2Label 是面板注册的系统级 launchd 标签。
	Aria2Label = "com.zizdog.aria2"
	// Aria2Slug 是面板托管界面的子路径（/aria/）。
	Aria2Slug = "aria"
	// Aria2RPCPort 是 aria2 的 JSON-RPC 端口（绑 0.0.0.0，靠 rpc-secret 保护）。
	Aria2RPCPort = 6800
	// Aria2Formula 是 Homebrew 包名。
	Aria2Formula = "aria2"
	// Aria2UIVersion 是内置 AriaNg 的版本（换界面时必须一起改，见 assets/ariang/PROVENANCE.md）。
	Aria2UIVersion = "1.3.14"
	// Aria2ConfName / Aria2SessionName 是安装目录下的文件名。
	Aria2ConfName    = "aria2.conf"
	Aria2SessionName = "aria2.session"
	// aria2SecretLen 是随机 RPC 密钥长度。
	aria2SecretLen = 24
)

// Aria2Paths 是这个应用在磁盘上的全部路径（便携于测试注入的家目录）。
type Aria2Paths struct {
	Home        string
	Root        string
	Conf        string
	Session     string
	DownloadDir string
	// Bin 是 aria2c 可执行文件（brew 装的），plist 里交给 supervisor 去跑。
	Bin    string
	Plist  string
	OutLog string
	ErrLog string
}

func (m *Manager) aria2Paths() Aria2Paths {
	home := strings.TrimSpace(m.opt.UserHome)
	if home == "" && strings.TrimSpace(m.opt.UserName) != "" {
		home = "/Users/" + m.opt.UserName
	}
	root := filepath.Join(home, "aria")
	return Aria2Paths{
		Home:    home,
		Root:    root,
		Conf:    filepath.Join(root, Aria2ConfName),
		Session: filepath.Join(root, Aria2SessionName),
		// 默认下载目录统一为「用户-下载-应用名」（2026-10-06 用户要求，与 transmission 同规矩）。
		// 它落在受保护的 ~/Downloads 下，靠面板 supervisor 继承面板的完全磁盘访问权限（坑 217）。
		DownloadDir: filepath.Join(home, "Downloads", "aria2"),
		Bin:         m.aria2cBin(),
		Plist:       aria2PlistPath(),
		OutLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.out.log"),
		ErrLog:      filepath.Join(home, "Library", "Logs", "zizpanel-aria2.err.log"),
	}
}

// aria2cBin 返回 brew 装出来的 aria2c 绝对路径（与安装时那条 os.Stat 同源）。
// 拿不到 brew 前缀就返回空串：宁可让安装器当场拒绝，也不要写出一条指向
// "某个碰巧叫 aria2c" 的 plist（launchd 的工作目录不是用户家目录，相对路径必挂）。
func (m *Manager) aria2cBin() string {
	if b := strings.TrimSpace(m.opt.BrewBin); b != "" {
		return filepath.Join(filepath.Dir(b), "aria2c")
	}
	return ""
}

// ---------------------------------------------------------------------------
//  仅测试用的注入点
//
//  安装会写 /Library/LaunchDaemons、调 launchctl bootstrap、跑 brew、发 RPC。
//  单测既不能真等，也绝不允许碰真实 launchd / 真实家目录（AGENTS 第三节）。
// ---------------------------------------------------------------------------

var (
	aria2PlistPath = func() string { return SystemDaemonPlistPath(Aria2Label) }
	aria2Launch    = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.bootstrapService(ctx, label, plist)
	}
	aria2Stop = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.removeService(ctx, label, plist)
	}
	// aria2RPCProbe 真的问一次 aria2.getVersion（默认实现打本机回环 RPC）。
	aria2RPCProbe = aria2RPCVersion
	// aria2Executable 返回面板二进制自身的路径（plist 用它跑 supervisor）。
	aria2Executable = os.Executable
	// aria2ReadyTimeout / aria2ReadyInterval 是"等 RPC 起来"的上限与轮询间隔
	// （单测把它们压到毫秒级，绝不真等 60 秒）。
	aria2ReadyTimeout  = 60 * time.Second
	aria2ReadyInterval = 500 * time.Millisecond
)

// aria2RPCVersion 发一条 aria2.getVersion，返回版本串；任何一步不成立都算没装好。
//
// 判据为什么不用"6800 在监听"：aria2c 起来了但配置坏了（会话文件读不到、
// 下载目录写不进去）时端口照样在听 —— 那时用户点开界面只能看到一堆报错。
func aria2RPCVersion(ctx context.Context, port int, secret string, timeout time.Duration) (string, error) {
	if port <= 0 {
		port = Aria2RPCPort
	}
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "zizpanel", "method": "aria2.getVersion",
		"params": []string{"token:" + secret},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/jsonrpc", port), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("RPC 返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Result struct {
			Version string `json:"version"`
		} `json:"result"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("RPC 响应不是 JSON：%s", strings.TrimSpace(string(raw)))
	}
	if out.Error.Message != "" {
		return "", fmt.Errorf("RPC 报错：%s（密钥不对？）", out.Error.Message)
	}
	if out.Result.Version == "" {
		return "", fmt.Errorf("RPC 没有返回版本号：%s", strings.TrimSpace(string(raw)))
	}
	return out.Result.Version, nil
}

// Aria2ConfPath 返回安装目录下配置文件的绝对路径（web 层读 RPC 密钥用）。
func Aria2ConfPath(userHome string) string {
	return filepath.Join(strings.TrimSpace(userHome), Aria2Slug, Aria2ConfName)
}

// ReadAria2Secret 读配置里的 rpc-secret（没装/读不到返回空字符串）。
func ReadAria2Secret(confPath string) string { return aria2SecretFromConf(confPath) }

// aria2SecretFromConf 读现有配置里的 rpc-secret（没有/读不到返回空）。
//
// 为什么要复用：AriaNg 把 RPC 配置存在浏览器 localStorage 里，密钥一换，
// 用户界面就会一直报"未授权"，还得手工去设置页改 —— 重装/升级不该换密钥。
func aria2SecretFromConf(confPath string) string {
	b, err := os.ReadFile(confPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "rpc-secret=") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, "rpc-secret="))
	}
	return ""
}

// aria2Conf 生成配置文件内容。
//
// 逐条都是"有理由"的，别顺手删：
//   - rpc-listen-all=true：用户 2026-09-23 要求局域网直连；rpc-secret 是唯一防线
//     （面板仍提供 /aria/jsonrpc 同源代理，见 aria2_web.go）；
//   - rpc-secret：面板随机生成，用户不必也不该手工设；
//   - dir：默认落在 <家目录>/Downloads/aria2（2026-10-06 统一路径；受保护的下载目录
//     由面板 supervisor 继承面板授权，坑 217）；
//   - bt-enable-lpd=false：lpd 走局域网组播，会触发 macOS 的"查找本地网络设备"
//     授权弹窗（transmission 那次就是这么被烦到的，见坑 229）；
//     DHT 是公网单播，不触发弹窗，保持开启（否则磁力链只能靠 tracker）；
//   - file-allocation=none：APFS 上预分配会让大文件开始下载前卡很久；
//   - save-session/input-file：断电/重启后能续上队列，否则任务列表一次全丢。
func aria2Conf(p Aria2Paths, secret string) string {
	var b strings.Builder
	b.WriteString("# 由 ZizPanel 生成/维护；改这里会被下一次「重新部署」覆盖。\n")
	fmt.Fprintf(&b, "dir=%s\n", p.DownloadDir)
	b.WriteString("continue=true\n")
	b.WriteString("file-allocation=none\n")
	b.WriteString("max-concurrent-downloads=5\n")
	b.WriteString("enable-rpc=true\n")
	b.WriteString("rpc-listen-all=true\n") // 用户 2026-09-23：局域网要直连 RPC（有 rpc-secret 保护）
	fmt.Fprintf(&b, "rpc-listen-port=%d\n", Aria2RPCPort)
	fmt.Fprintf(&b, "rpc-secret=%s\n", secret)
	fmt.Fprintf(&b, "save-session=%s\n", p.Session)
	fmt.Fprintf(&b, "input-file=%s\n", p.Session)
	b.WriteString("save-session-interval=30\n")
	b.WriteString("bt-enable-lpd=false\n")
	b.WriteString("enable-dht=true\n")
	// 走**系统解析器**而不是内置的 c-ares：macOS 上两者可能给出不同结果 ——
	// 真机撞到过 aria2 报 `Name resolution for mirror.zizdog.com failed:
	// DNS server returned answer with no data`，而同一台机器上 curl 解析正常
	// （系统解析器走 mDNSResponder，c-ares 只读 /etc/resolv.conf，
	// 遇到 CNAME/加密 DNS 配置就会分叉）。下载器要的是"能解析"，
	// 代价是 DNS 查询期间事件循环阻塞，可以接受。
	b.WriteString("async-dns=false\n")
	b.WriteString("follow-torrent=true\n")
	b.WriteString("console-log-level=warn\n")
	return b.String()
}

// Aria2SuperviseArgs 拼出系统 plist 里的 ProgramArguments：**面板自己的二进制** +
// aria2-supervise + 真实用户与 aria2 的全部路径（冻结契约，改它等于改兼容）。
//
// 为什么让面板托管 aria2（与 zizvideo 同一套，2026-09-23 用户点名）：
// 由面板二进制 fork 后 setuid 到真实用户（下载文件归属用户），系统级 LaunchDaemon
// 保证无头开机就在。与面板同一代码要求 ⇒ 用户把 dir= 指到受保护目录/外接盘时仍继承
// 面板的「完全磁盘访问权限」（默认目录已移出受保护位置，见 aria2Paths）。
//
// 直跑 `/opt/homebrew/bin/aria2c` 不行：它是 adhoc 签名、身份里带二进制哈希
// （aria2c-55554944…），每次 brew 升级都变，给它的授权会失效；而后台服务弹不出
// 授权框，系统会把 open() **挂住**（表现为端口在听、界面永远"连接中…"）。
func Aria2SuperviseArgs(panelBin, userName string, p Aria2Paths) []string {
	return []string{
		panelBin, "aria2-supervise",
		"--user", userName,
		"--bin", p.Bin,
		"--conf", p.Conf,
		"--root", p.Root,
		"--home", p.Home,
	}
}

// aria2Plist 生成系统级 LaunchDaemon 的 plist（跑的是**面板二进制**的 supervisor）。
//
// 刻意**不写 UserName**：这个作业必须以 root 运行 —— supervisor fork 之后用
// SysProcAttr.Credential 降到真实用户（同 zizvideo，见坑 202）。下载文件必须属于用户，
// root 跑出来的文件用户在 Finder 里删都删不掉。
// 系统级（不是 ~/Library/LaunchAgents）是无头 macOS 重启后它还能自己起来。
func aria2Plist(panelBin, user, root, bin, conf, outLog, errLog string) string {
	args := Aria2SuperviseArgs(panelBin, user, Aria2Paths{Bin: bin, Conf: conf, Root: root, Home: filepath.Dir(root)})
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "    <key>Label</key>\n    <string>%s</string>\n", xmlEscape(Aria2Label))
	b.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "        <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("    </array>\n")
	fmt.Fprintf(&b, "    <key>WorkingDirectory</key>\n    <string>%s</string>\n", xmlEscape(root))
	b.WriteString("    <key>RunAtLoad</key>\n    <true/>\n")
	b.WriteString("    <key>KeepAlive</key>\n    <true/>\n")
	b.WriteString("    <key>EnvironmentVariables</key>\n    <dict>\n")
	b.WriteString("        <key>PATH</key>\n")
	b.WriteString("        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n")
	b.WriteString("    </dict>\n")
	fmt.Fprintf(&b, "    <key>StandardOutPath</key>\n    <string>%s</string>\n", xmlEscape(outLog))
	fmt.Fprintf(&b, "    <key>StandardErrorPath</key>\n    <string>%s</string>\n", xmlEscape(errLog))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// InstallAria2 安装 aria2 + 写配置 + 注册系统级服务 + 复核 RPC 真的可用。
//
// 幂等：包已装跳过 brew install；已有配置**复用原密钥**（AriaNg 里配好的凭据不失效）。
func (m *Manager) InstallAria2(ctx context.Context, app App, result *InstallResult) error {
	if result != nil {
		result.App = app.ID
	}
	if strings.TrimSpace(m.opt.BrewBin) == "" {
		return fmt.Errorf("未配置 Homebrew 路径，无法自动安装 aria2。请先安装 Homebrew")
	}
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		return fmt.Errorf("未安装 Homebrew（%s 不存在），无法自动安装 aria2。请先安装 Homebrew", m.opt.BrewBin)
	}
	p := m.aria2Paths()
	if strings.TrimSpace(p.Home) == "" {
		return fmt.Errorf("无法确定运行 aria2 的真实用户家目录（UserHome / UserName 都为空）")
	}
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行 aria2 的真实用户（UserName 为空）")
	}

	// ① 本体。
	if m.brewHas(ctx, Aria2Formula) {
		if result != nil {
			result.step(ctx, Aria2Formula+" 已经装好了（Homebrew 里已有），跳过安装")
		}
	} else {
		if result != nil {
			result.step(ctx, "正在 brew install "+Aria2Formula+"（单二进制，很快）")
		}
		if _, err := m.brewInstall(ctx, result, 15*time.Minute, Aria2Formula); err != nil {
			return fmt.Errorf("安装 %s 失败：%w", Aria2Formula, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(m.opt.BrewBin), "aria2c")); err != nil {
		return fmt.Errorf("brew 装完了但找不到 %s：请执行 `brew reinstall %s` 后重试",
			filepath.Join(filepath.Dir(m.opt.BrewBin), "aria2c"), Aria2Formula)
	}

	// ② 目录：安装目录 + 下载目录（下载目录必须**以运行用户身份实测可写**）。
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return fmt.Errorf("创建安装目录 %s 失败：%w", p.Root, err)
	}
	if err := os.MkdirAll(filepath.Dir(p.OutLog), 0o755); err != nil {
		return fmt.Errorf("创建日志目录 %s 失败：%w", filepath.Dir(p.OutLog), err)
	}
	if err := m.ensureUserWritableDir(ctx, p.DownloadDir,
		"下载目录", "aria2", "请在面板里把下载目录改成一个可写目录"); err != nil {
		return err
	}

	// ③ 密钥：已有配置就复用（换密钥会让 AriaNg 里配好的凭据失效）。
	secret := aria2SecretFromConf(p.Conf)
	reused := secret != ""
	if !reused {
		s, err := generateMinifluxSecret(aria2SecretLen)
		if err != nil {
			return fmt.Errorf("生成 RPC 密钥失败: %w", err)
		}
		secret = s
	}

	// ④ 配置与会话文件（面板以 root 写，写完把归属交还用户）。
	if err := os.WriteFile(p.Conf, []byte(aria2Conf(p, secret)), 0o600); err != nil {
		return fmt.Errorf("写入配置 %s 失败（面板需要以 root 运行）：%w", p.Conf, err)
	}
	if _, err := os.Stat(p.Session); os.IsNotExist(err) {
		if err := os.WriteFile(p.Session, nil, 0o600); err != nil {
			return fmt.Errorf("创建会话文件 %s 失败：%w", p.Session, err)
		}
	}
	if m.opt.UserName != "" {
		_ = chownTree(m.opt.UserName, p.Root)
	}
	if result != nil {
		result.step(ctx, "已写入配置 "+p.Conf+"（权限 0600；RPC 密钥不写进任务日志）")
	}

	// ⑤ plist + 装载。
	if strings.TrimSpace(p.Plist) == "" {
		return fmt.Errorf("aria2 的 plist 路径为空")
	}
	panelBin, err := aria2Executable()
	if err != nil || strings.TrimSpace(panelBin) == "" {
		return fmt.Errorf("找不到面板自身的可执行文件路径（supervisor 要用它启动 aria2）: %w", err)
	}
	if !filepath.IsAbs(panelBin) || !filepath.IsAbs(strings.TrimSpace(p.Bin)) {
		return fmt.Errorf("拒绝写入 launchd 起不来的 plist：面板二进制=%q，aria2c=%q（两者都必须是绝对路径）", panelBin, p.Bin)
	}
	plist := aria2Plist(panelBin, m.opt.UserName, p.Root, p.Bin, p.Conf, p.OutLog, p.ErrLog)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist %s 失败（面板需要以 root 运行）：%w", p.Plist, err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist %s 失败：%w", p.Plist, err)
	}
	if result != nil {
		result.step(ctx, "正在注册并启动系统级服务 "+Aria2Label+"（RPC 绑 0.0.0.0:"+fmt.Sprint(Aria2RPCPort)+"）")
	}
	if err := aria2Launch(m, ctx, Aria2Label, p.Plist); err != nil {
		return fmt.Errorf("启动 aria2 失败：%w", err)
	}
	if err := m.RegisterInstalledService(ctx, Aria2Label, app.Name, app.Icon, app.Category, Aria2RPCPort); err != nil {
		if result != nil {
			result.step(ctx, "警告：服务已启动，但登记进「服务管理」失败："+err.Error()+
				"（可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
		}
	}

	// ⑥ 复核：真的问一次 RPC（端口在听不算）。
	ver, err := m.waitAria2Ready(ctx, p, secret, result)
	if err != nil {
		return err
	}

	if result != nil {
		result.Credentials = append(result.Credentials, Credential{
			Key:   "aria2_rpc_secret",
			Value: secret,
			Label: "aria2 RPC 密钥（面板托管的 AriaNg 界面已自动配好，一般不用手填）",
		})
		result.Steps = append(result.Steps,
			"网页界面：面板托管在 /"+Aria2Slug+"/（从卡片点「打开」，需先登录面板）",
			"下载目录："+p.DownloadDir+"（卸载时不会动这里的文件）",
			"会话文件："+p.Session+"（重启后队列还能续上）",
			"RPC：http://<本机IP>:"+fmt.Sprint(Aria2RPCPort)+"/jsonrpc（绑 0.0.0.0，需带 rpc-secret；也可走面板同源代理）",
			"引擎版本："+ver,
		)
		if reused {
			result.Steps = append(result.Steps, "已复用原有 RPC 密钥（重装/升级不会让 AriaNg 里配好的连接失效）")
		}
		result.Message = fmt.Sprintf("「%s」已安装并纳入管理。%s", app.Name, app.PostInstallHint)
	}
	return nil
}

// waitAria2Ready 轮询 RPC 直到拿到版本号（或超时如实报错并给日志出路）。
func (m *Manager) waitAria2Ready(ctx context.Context, p Aria2Paths, secret string, result *InstallResult) (string, error) {
	timeout := aria2ReadyTimeout
	deadline := time.Now().Add(timeout)
	ver := ""
	var lastErr error
	err := assertReady(ctx, readySpec{
		What:    "aria2 RPC（jsonrpc " + fmt.Sprint(Aria2RPCPort) + "）",
		Expect:  "在 " + timeout.String() + " 内用配置里的密钥真的回一次版本号",
		Timeout: timeout,
		Probe: func(ctx context.Context) readyVerdict {
			for {
				v, err := aria2RPCProbe(ctx, Aria2RPCPort, secret, 5*time.Second)
				if err == nil {
					ver = v
					return readyVerdict{OK: true, Actual: "aria2 " + v}
				}
				lastErr = err
				if ctx.Err() != nil {
					return readyVerdict{Actual: "已取消：RPC 还没就绪"}
				}
				if time.Now().After(deadline) {
					return readyVerdict{Actual: "RPC 在 " + timeout.String() + " 内没有回版本号：" + err.Error()}
				}
				select {
				case <-ctx.Done():
					return readyVerdict{Actual: "已取消：RPC 还没就绪"}
				case <-time.After(aria2ReadyInterval):
				}
			}
		},
		LogPath: p.ErrLog,
		State:   "服务 " + Aria2Label + " 已注册并启动，配置与会话文件已就绪",
		Missing: "但 RPC 不可用，面板托管的界面（/" + Aria2Slug + "/）连不上它",
		Remedy:  aria2ReadyRemedy(p.DownloadDir, m.opt.UserHome),
		Result:  result,
	})
	if err != nil {
		if lastErr != nil {
			return "", fmt.Errorf("%w（最后一次 RPC 探测：%v）", err, lastErr)
		}
		return "", err
	}
	return ver, nil
}

// aria2ReadyRemedy 给"服务起来了但 RPC 不可用"配一条**具体**的出路。
//
// 为什么要按目录分岔（mini 真机 2026-09-23，用户报"永远连接中…"）：macOS 隐私保护
// （文件与文件夹 → 下载/桌面/文稿）会让后台服务访问这些目录时**挂在 open() 上**
// （不是拒绝、也不报错：连不需要凭据的 GET / 都不回），界面表现就是一直"连接中…"。
//
// 默认目录现在是 ~/Downloads/aria2（2026-10-06），正好在保护范围内 —— 但它由面板
// supervisor 拉起（继承面板授权，坑 217），所以要授权的是**面板程序**，不是 aria2c 本身。
func aria2ReadyRemedy(downloadDir, home string) string {
	const base = "在「服务管理 → aria2」里点「重启服务」再试"
	if name := tccProtectedFolderName(downloadDir, home); name != "" {
		return "先给面板程序开「系统设置 → 隐私与安全性 → 完全磁盘访问权限」" +
			"（aria2 由面板的 supervisor 拉起、继承面板授权；后台服务弹不出授权框，不开就是卡死），" +
			"再点「重启服务」；或者把「📝 编辑配置文件」里的 dir= 换成不受保护的位置（例如 ~/aria2/downloads）" +
			"—— 下载目录现在是 " + downloadDir + "（macOS 的" + name + "受隐私保护）。"
	}
	return base + "；仍然失败看下面的日志尾部" +
		"（常见原因：6800 被别的程序占用 / 配置文件被手工改坏）"
}

// tccProtectedFolderName 判断目录是不是落在 macOS 保护的那三个文件夹里
// （返回中文名，"" 表示不在）。只认家目录下的那一层，避免把 /data/Downloads 也误报。
func tccProtectedFolderName(dir, home string) string {
	dir, home = filepath.Clean(strings.TrimSpace(dir)), filepath.Clean(strings.TrimSpace(home))
	if dir == "" || dir == "." || home == "" || home == "." {
		return ""
	}
	for _, c := range []struct{ dir, label string }{
		{"Downloads", "下载文件夹"},
		{"Desktop", "桌面文件夹"},
		{"Documents", "文稿文件夹"},
	} {
		root := filepath.Join(home, c.dir)
		if dir == root || strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return c.label
		}
	}
	return ""
}

// Aria2RPCPortInUse 报告 6800 是否已被别人占着（安装前检查用）。
func (m *Manager) Aria2RPCPortInUse() bool {
	return m.portHasListener(Aria2RPCPort)
}
