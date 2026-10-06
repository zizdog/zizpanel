package services

// ============================================================================
//  Transmission（下载器）安装器 —— 面板托管（2026-10-06 改造，模板是 aria2）
//
//  权限/进程模型：面板写**系统级 LaunchDaemon** com.zizdog.transmission，跑的是
//  面板自己的二进制 `zizpanel transmission-supervise`（root）→ fork 后 setuid 到
//  真实用户起 transmission-daemon。与 aria2 同一套：文件归属用户；用户把下载目录
//  指到外接盘/受保护目录时**继承面板的 TCC 授权**（坑 217），不依赖 brew 的 adhoc 身份。
//
//  安装步骤：
//    1. brew install transmission-cli（30 分钟超时）
//    2. **先停掉 brew 的旧作业并摘 plist**（homebrew.mxcl.* 与 sh.brew.* 两套前缀 +
//       /Library/LaunchDaemons 里那份系统化的），否则两份实例抢 9091
//    3. 复核下载目录存在且**以运行用户身份**可写（不可写就如实失败，见坑 226）
//    4. 停 → 等端口释放 → 合并写回 settings.json →
//       写我们的 plist → bootstrap（顺序不可换，见坑 226）
//    5. **回读 settings.json**：transmission 启动时会把明文口令换成带盐哈希
//       （`{<salt><hash>`），明文还在 = 没生效 → 安装如实失败
//    6. RPC 自检：无凭据必须 401、带凭据必须 200/409（否则如实失败）
//    7. 健康检查 GET /transmission/web/ + **回读进程树**（daemon 必须是
//       transmission-supervise 的子进程、以真实用户运行，否则不算成功）
//    8. 登记服务
//
//  改凭据/改下载目录走 `SetTransmissionRPCSettings`（面板正规入口，同一套顺序）：
//  用户手工编辑 settings.json 再重启，会被 daemon 退出时的回写覆盖。
//
//  安全设计（与 miniflux 同一条约定）：口令由 crypto/rand 从 [A-Za-z0-9] 生成；
//  明文只出现在"写进 settings.json 的那一瞬间"（transmission 自己会哈希它）与
//  InstallResult.Credentials，绝不进任务步骤/日志。
// ============================================================================

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

const (
	transmissionFormula = "transmission-cli"
	// transmissionLabel 是面板自己写的系统级 launchd 标签（不再是 brew 的 homebrew.mxcl.*）。
	transmissionLabel = "com.zizdog.transmission"
	transmissionPort  = 9091

	// 用户名 12 位、口令 20 位（[A-Za-z0-9]，与 Syncthing/Miniflux 同一字符集）。
	transmissionUserLen     = 12
	transmissionPasswordLen = 20
)

// TransmissionLabel 与 transmissionLabel 同值，导出给 web 层判断服务记录用。
const TransmissionLabel = transmissionLabel

// transmissionBrewLabels 返回 brew 可能给 transmission 用过的**全部** launchd 标签。
//
// 两套命名（homebrew.mxcl.* / sh.brew.*）都认：只看一个会漏掉另一份，重启后两份实例
// 抢同一个 9091（同 systemdaemon.go 的教训）。
func transmissionBrewLabels() []string {
	return []string{"homebrew.mxcl." + transmissionFormula, "sh.brew." + transmissionFormula}
}

// ---------- 路径（一律从 brew 前缀推导，单测才能落在临时目录里） ----------

// TransmissionPaths 是这个应用在磁盘上的全部路径（便携于测试注入的家目录/前缀）。
type TransmissionPaths struct {
	Home      string
	ConfigDir string
	Settings  string
	LogFile   string
	// Bin 是 brew 装出来的 transmission-daemon（plist 里交给 supervisor 去跑）。
	Bin    string
	Plist  string
	OutLog string
	ErrLog string
	// DownloadDir 是**默认**下载目录（settings.json 里已有 download-dir 时以文件为准）。
	DownloadDir string
}

// transmissionConfigDir 沿用它自己的位置 <brew>/var/transmission/（老安装的配置与
// 已有下载队列都在这里，2026-10-06 只换托管方式、不搬配置）。
// 推导而不写死 /opt/homebrew：Intel 机器上是 /usr/local。
func (m *Manager) transmissionConfigDir() string {
	return filepath.Join(m.brewPrefix(), "var", "transmission")
}

func (m *Manager) transmissionSettingsPath() string {
	return filepath.Join(m.transmissionConfigDir(), "settings.json")
}

func (m *Manager) transmissionLogPath() string {
	return filepath.Join(m.transmissionConfigDir(), "transmission-daemon.log")
}

// transmissionDaemonBin 返回 brew 装出来的 transmission-daemon 绝对路径。
// 拿不到 brew 前缀就返回空串：宁可让安装器当场拒绝，也不要写出一条指向
// "某个碰巧叫 transmission-daemon" 的 plist（launchd 的工作目录不是用户家目录）。
func (m *Manager) transmissionDaemonBin() string {
	if b := strings.TrimSpace(m.opt.BrewBin); b != "" {
		return filepath.Join(filepath.Dir(b), "transmission-daemon")
	}
	return ""
}

// transmissionPaths 汇总全部路径（家目录缺失时为家目录相关字段返回空串，由调用方拒绝）。
func (m *Manager) transmissionPaths() TransmissionPaths {
	home := strings.TrimSpace(m.opt.UserHome)
	if home == "" && strings.TrimSpace(m.opt.UserName) != "" {
		home = "/Users/" + m.opt.UserName
	}
	cfg := m.transmissionConfigDir()
	p := TransmissionPaths{
		Home:      home,
		ConfigDir: cfg,
		Settings:  filepath.Join(cfg, "settings.json"),
		LogFile:   filepath.Join(cfg, "transmission-daemon.log"),
		Bin:       m.transmissionDaemonBin(),
		Plist:     transmissionPlistPath(),
	}
	if home != "" {
		p.OutLog = filepath.Join(home, "Library", "Logs", "zizpanel-transmission.out.log")
		p.ErrLog = filepath.Join(home, "Library", "Logs", "zizpanel-transmission.err.log")
		// 默认下载目录统一为「用户-下载-应用名」（2026-10-06 用户要求，与 aria2 同规矩）：
		// 落在家目录的下载文件夹下，靠面板 supervisor 继承面板授权（坑 217）。
		p.DownloadDir = filepath.Join(home, "Downloads", "transmission")
	}
	return p
}

// ---------- 纯函数：settings.json 合并 ----------

// transmissionDesiredSettings 是面板**负责**的那几个键（其余字段原样保留）。
//
// rpc-whitelist-enabled 必须关掉：绑 0.0.0.0 而白名单只含回环 = 局域网仍然进不来
// （用户 2026-09-23 要求局域网直连）。门只剩 rpc-username/口令（随机生成、必填）。
//
// dht / lpd（LSD 局域网发现）/ port-forwarding（UPnP-NAT-PMP）的取舍：
//   - lpd 与 port-forwarding 走**局域网组播**，会触发 macOS「本地网络」授权弹窗
//     （ad-hoc 签名的 brew daemon 在升级后授权失效、反复弹窗），默认关掉；
//   - dht 是**公网单播**、不碰本地网络，而且无 tracker 的磁力链只能靠它找 peer。
//     真机实测（2026-09-20）：dht 关了以后加磁力链永远 peers=0 / metadata=0%、
//     不报错 —— 就是用户看到的「新建下载任务没反应」（坑 226）。所以 dht 默认开。
func transmissionDesiredSettings(user, password string) map[string]any {
	return map[string]any{
		"rpc-enabled":                 true,
		"rpc-authentication-required": true,
		"rpc-username":                user,
		"rpc-password":                password,
		"rpc-bind-address":            "0.0.0.0", // 用户 2026-09-23：局域网直连（RPC 有用户名/口令保护）
		"rpc-whitelist-enabled":       false,
		"rpc-whitelist":               "127.0.0.1,::1",
		"rpc-port":                    transmissionPort,
		// dht 必须开：磁力链没有 tracker 时唯一能找到 peer 的途径（不上本地网络）。
		"dht-enabled":             true,
		"lpd-enabled":             false,
		"port-forwarding-enabled": false,
	}
}

// transmissionSettingsWithCredentials 把凭据与回环绑定合并进 settings.json 的原始字节。
//
// 用 map 合并而不是写一份完整 schema：settings.json 有上百个字段，写死一份会把
// 上游新增或用户手改的字段整体冲掉。保留原有 JSON 类型，只覆盖我们负责的键。
//
// 返回 (新内容, 是否有变化, error)。内容没变化时调用方可以不写盘。
func transmissionSettingsWithCredentials(raw []byte, user, password string) ([]byte, bool, error) {
	return transmissionMergeSettings(raw, transmissionDesiredSettings(user, password))
}

// transmissionMergeSettings 是通用合并：desired 里的键覆盖，其余字段原样保留。
func transmissionMergeSettings(raw []byte, desired map[string]any) ([]byte, bool, error) {
	cfg := map[string]any{}
	if strings.TrimSpace(string(raw)) != "" {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, false, fmt.Errorf("现有 settings.json 不是合法 JSON（%v）："+
				"面板不覆盖它，请先修好或删掉再重装", err)
		}
	}
	changed := false
	for k, v := range desired {
		if cur, ok := cfg[k]; !ok || fmt.Sprint(cur) != fmt.Sprint(v) {
			changed = true
		}
		cfg[k] = v
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), changed, nil
}

// transmissionSettingsString 读回 settings.json 里的一个字符串字段（读不到返回空串）。
func transmissionSettingsString(raw []byte, key string) string {
	cfg := map[string]any{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}

// transmissionSettingsBool 读回 settings.json 里的一个布尔字段；缺失返回 (false, false)。
func transmissionSettingsBool(raw []byte, key string) (bool, bool) {
	cfg := map[string]any{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false, false
	}
	v, ok := cfg[key].(bool)
	return v, ok
}

// ---------- 可注入的出口（单测不许真起服务、真发 HTTP） ----------
//
// 与 Syncthing 同一套做法：注入点是**包级变量**（不是 Manager 字段）——
// Manager 上每加一个测试字段都要动 services.go，而那个文件被多个并行改动共享。
var (
	// transmissionHTTPProbe 是 RPC / Web UI 的一次 GET 探测（单测替换它，避免连真机 9091）。
	transmissionHTTPProbe = func(m *Manager, ctx context.Context, rawURL, user, password string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return 0, err
		}
		if user != "" {
			req.SetBasicAuth(user, password)
		}
		client := &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{Proxy: nil}}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode, nil
	}
	// transmissionWaitOverride 是"等它起来"的上限（单测把它压到毫秒级）。
	transmissionWaitOverride = 60 * time.Second
	// transmissionWriteProbe 实测"运行 daemon 的身份能不能在目录里建文件"
	// （单测注入失败实现做负向对照：不可写时不许报成功）。坑 226。
	transmissionWriteProbe = func(m *Manager, ctx context.Context, probePath string) error {
		if m.opt.UserName != "" {
			if _, err := m.runAsUser(ctx, 15*time.Second, "/usr/bin/touch", probePath); err != nil {
				return err
			}
			_ = os.Remove(probePath)
			return nil
		}
		f, err := os.OpenFile(probePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_ = f.Close()
		return os.Remove(probePath)
	}

	// ---- 面板托管（plist / launchd / 进程树）的注入点，与 aria2 同一套 ----
	//
	// transmissionPlistPath 返回面板自己的系统级 plist 路径（单测指到临时目录，
	// 绝不碰真实 /Library/LaunchDaemons）。
	transmissionPlistPath = func() string { return SystemDaemonPlistPath(transmissionLabel) }
	// transmissionExecutable 返回面板二进制自身的路径（plist 用它跑 supervisor）。
	transmissionExecutable = os.Executable
	// transmissionLaunch 装载并启动我们的 LaunchDaemon（默认 bootstrapService，会先 bootout 再 bootstrap）。
	transmissionLaunch = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.bootstrapService(ctx, label, plist)
	}
	// transmissionStop 停我们的服务并撤 plist + 面板记录（卸载用）。
	transmissionStop = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.removeService(ctx, label, plist)
	}
	// transmissionBootout / transmissionDisable 是"摘掉 brew 旧作业"的两个动作
	// （单测注入记录器；真实实现走 launchctl，绝不在这里造第二套 launchd 逻辑）。
	transmissionBootout = func(m *Manager, ctx context.Context, label string) error {
		return priv.LaunchUnload(label)
	}
	transmissionDisable = func(m *Manager, ctx context.Context, label string) error {
		var firstErr error
		if m.opt.UID > 0 {
			if _, err := m.runRoot(ctx, 15*time.Second, "/bin/launchctl",
				"disable", fmt.Sprintf("gui/%d/%s", m.opt.UID, label)); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if _, err := m.runRoot(ctx, 15*time.Second, "/bin/launchctl", "disable", "system/"+label); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	// transmissionProcessTable 回读整张进程表（默认 `ps -Ao pid=,ppid=,user=,command=`）。
	transmissionProcessTable = defaultTransmissionProcessTable
)

// ensureTransmissionDownloadDir 确保下载目录存在、归属运行用户、且**实测可写**。
// 不可写时如实报错并给出可照做的动作，绝不"能登录但什么都下不了"（坑 226）。
func (m *Manager) ensureTransmissionDownloadDir(ctx context.Context, dir string) error {
	return m.ensureUserWritableDir(ctx, dir, "下载目录",
		"Transmission 以 "+transmissionRunAsName(m)+" 身份运行",
		"请在面板里改成一个可写目录，不要手工改 settings.json（会被回写覆盖）")
}

func transmissionRunAsName(m *Manager) string {
	if m.opt.UserName != "" {
		return m.opt.UserName
	}
	return "当前进程"
}

// transmissionEffectiveDownloadDir 取 settings.json 里的 download-dir；没有就按
// 默认给 <home>/Downloads/transmission。
//
// 旧默认 <home>/Downloads 视为"用户没自己设过"，迁到新默认（2026-10-06 统一路径）；
// 其它值一律原样保留（用户自己设的外接盘不能被面板改掉）。读不到 settings 时返回默认值。
func (m *Manager) transmissionEffectiveDownloadDir(raw []byte) string {
	def := m.transmissionPaths().DownloadDir
	if d := transmissionSettingsString(raw, "download-dir"); d != "" {
		old := ""
		if home := strings.TrimSpace(m.opt.UserHome); home != "" {
			old = filepath.Join(home, "Downloads")
		}
		if def != "" && old != "" && filepath.Clean(d) == old {
			return def
		}
		return d
	}
	return def
}

// transmissionHTTP 发一次 GET，返回状态码；user 非空时带 basic auth。
// 用 Go 客户端而不是 `curl -u user:pass`：后者会把口令放进 argv（本项目出过 argv 泄漏）。
func (m *Manager) transmissionHTTP(ctx context.Context, rawURL, user, password string) (int, error) {
	return transmissionHTTPProbe(m, ctx, rawURL, user, password)
}

// transmissionTimeout 是"等它起来"的上限（单测把它压到毫秒级）。
func (m *Manager) transmissionTimeout() time.Duration {
	return transmissionWaitOverride
}

// ---------- 面板托管：plist / 迁移 brew 旧作业 / 回读进程树 ----------

// TransmissionSuperviseArgs 拼出系统 plist 里的 ProgramArguments：**面板自己的二进制** +
// transmission-supervise + 真实用户与 transmission 的全部路径（冻结契约，改它等于改兼容）。
//
// 为什么让面板托管（与 aria2 同一套，2026-10-06 用户点名）：面板二进制 fork 后 setuid
// 到真实用户（下载文件归属用户）；系统级 LaunchDaemon 保证无头开机就在；与面板同一代码
// 要求 ⇒ 用户把下载目录指到受保护目录/外接盘时继承面板的「完全磁盘访问权限」（坑 217）。
// 直跑 brew 的 transmission-daemon 不行：adhoc 签名每次 brew 升级都换，授权随即失效。
func TransmissionSuperviseArgs(panelBin, userName string, p TransmissionPaths) []string {
	return []string{
		panelBin, "transmission-supervise",
		"--user", userName,
		"--bin", p.Bin,
		"--config-dir", p.ConfigDir,
		"--log", p.LogFile,
		"--home", p.Home,
	}
}

// transmissionPlistContent 生成系统级 LaunchDaemon 定义（跑的是面板二进制的 supervisor）。
//
// 刻意**不写 UserName**：这个作业必须以 root 运行 —— supervisor fork 之后用
// SysProcAttr.Credential 降到真实用户（同 aria2/坑 202）。系统级（不是 ~/Library/LaunchAgents）
// 是无头 macOS 重启后它还能自己起来（坑 130）。
func transmissionPlistContent(label string, args []string, outLog, errLog string) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "    <key>Label</key>\n    <string>%s</string>\n", xmlEscape(label))
	b.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "        <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("    </array>\n")
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

// ensureTransmissionPlist 写出面板托管的系统 plist（幂等：内容一样也重写一遍，代价可忽略）。
// 任何一项路径算不出来都**拒绝写**：写出一条 launchd 起不来的 plist 只会让用户看到
// "装好了但没起来"。
func (m *Manager) ensureTransmissionPlist(ctx context.Context) (string, error) {
	p := m.transmissionPaths()
	if strings.TrimSpace(p.Home) == "" {
		return "", fmt.Errorf("无法确定运行 Transmission 的真实用户家目录（UserHome / UserName 都为空）")
	}
	if strings.TrimSpace(m.opt.UserName) == "" {
		return "", fmt.Errorf("无法确定运行 Transmission 的真实用户（UserName 为空）")
	}
	if strings.TrimSpace(p.Plist) == "" {
		return "", fmt.Errorf("Transmission 的 plist 路径为空")
	}
	panelBin, err := transmissionExecutable()
	if err != nil || strings.TrimSpace(panelBin) == "" {
		return "", fmt.Errorf("找不到面板自身的可执行文件路径（supervisor 要用它启动 Transmission）: %w", err)
	}
	if !filepath.IsAbs(panelBin) || !filepath.IsAbs(strings.TrimSpace(p.Bin)) {
		return "", fmt.Errorf("拒绝写入 launchd 起不来的 plist：面板二进制=%q，transmission-daemon=%q（两者都必须是绝对路径）",
			panelBin, p.Bin)
	}
	if err := os.MkdirAll(filepath.Dir(p.Plist), 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败（面板需要以 root 运行）：%w", filepath.Dir(p.Plist), err)
	}
	if err := os.MkdirAll(filepath.Dir(p.OutLog), 0o755); err != nil {
		return "", fmt.Errorf("创建日志目录 %s 失败：%w", filepath.Dir(p.OutLog), err)
	}
	content := transmissionPlistContent(transmissionLabel,
		TransmissionSuperviseArgs(panelBin, m.opt.UserName, p), p.OutLog, p.ErrLog)
	if err := os.WriteFile(p.Plist+".tmp", []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("写入 plist %s 失败（面板需要以 root 运行）：%w", p.Plist, err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return "", fmt.Errorf("安装 plist %s 失败：%w", p.Plist, err)
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(p.Plist, 0, 0)
	}
	return p.Plist, nil
}

// retireBrewTransmission 摘掉 brew 留下的 transmission 作业：bootout + disable + 删两处 plist。
//
// 为什么不能只"停一下"（2026-10-06 改造的核心）：brew 的服务 plist 带 RunAtLoad/KeepAlive，
// 重启后它会自己回来；我们自己的 LaunchDaemon 也 RunAtLoad ⇒ 两份实例抢 9091。
// 本机实测 brew 用两套前缀（homebrew.mxcl.* / sh.brew.*），而且历史安装还留下过
// /Library/LaunchDaemons/<brew label>.plist（systemdaemon 搬迁产物）—— 三处都要清。
//
// bootout 失败不在这里判死：端口没释放会由调用方的 waitTransmissionPortFree 如实拦下。
func (m *Manager) retireBrewTransmission(ctx context.Context, res *InstallResult) error {
	for _, label := range transmissionBrewLabels() {
		if err := transmissionBootout(m, ctx, label); err != nil && res != nil {
			res.step(ctx, "（旧作业 "+label+" 停止时报告："+err.Error()+"；随后按端口占用情况判定）")
		}
		if err := transmissionDisable(m, ctx, label); err != nil && res != nil {
			res.step(ctx, "（旧作业 "+label+" 未能 disable："+err.Error()+"；已删 plist，重启也不会被它拉起）")
		}
		for _, path := range []string{m.systemDaemonUserPlist(label), SystemDaemonPlistPath(label)} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("删除 brew 旧作业的 plist %s 失败：%w"+
					"（留着它会和面板托管的实例抢 %d）", path, err, transmissionPort)
			}
		}
	}
	return nil
}

// transmissionProc 是 `ps` 回读出来的一行。
type transmissionProc struct {
	PID     int
	PPID    int
	User    string
	Command string
}

// defaultTransmissionProcessTable 跑一次 `ps -Ao pid=,ppid=,user=,command=`。
//
// 为什么用整张进程表而不是按端口反查 PID：要证明的关系是"父进程是谁"，
// 端口只能告诉我们"有个东西在听"（AGENTS 第三节：判据要贴运行体）。
func defaultTransmissionProcessTable(ctx context.Context) ([]transmissionProc, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-Ao", "pid=,ppid=,user=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parseTransmissionProcessTable(string(out)), nil
}

// parseTransmissionProcessTable 解析 ps 输出（command 里有空格，取前 3 列后再合并余下）。
func parseTransmissionProcessTable(out string) []transmissionProc {
	var procs []transmissionProc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs = append(procs, transmissionProc{PID: pid, PPID: ppid, User: f[2], Command: strings.Join(f[3:], " ")})
	}
	return procs
}

// processCommandHasConfigDir 判断命令行里 --config-dir 的值是不是 dir（写法与结尾斜杠都容忍）。
func processCommandHasConfigDir(command, dir string) bool {
	fields := strings.Fields(command)
	for i, f := range fields {
		var val string
		switch {
		case f == "--config-dir" && i+1 < len(fields):
			val = fields[i+1]
		case strings.HasPrefix(f, "--config-dir="):
			val = strings.TrimPrefix(f, "--config-dir=")
		default:
			continue
		}
		if strings.TrimRight(filepath.Clean(strings.TrimSpace(val)), string(filepath.Separator)) == dir {
			return true
		}
	}
	return false
}

// transmissionSuperviseProblem 判断进程树对不对（空串 = 对），返回人话原因。
//
// 判据三条（缺一不可）：① 有以 --config-dir <dir> 运行的 transmission-daemon；
// ② 它以真实用户身份运行；③ 父进程是 transmission-supervise（面板二进制）。
// 抽成纯函数：单测不用真起进程就能把负向对照钉死。
func transmissionSuperviseProblem(procs []transmissionProc, configDir, userName string) string {
	dir := strings.TrimRight(filepath.Clean(strings.TrimSpace(configDir)), string(filepath.Separator))
	if dir == "" || dir == "." {
		return "配置目录为空，无法回读进程树"
	}
	byPID := map[int]transmissionProc{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	found := false
	var reasons []string
	for _, p := range procs {
		if !strings.Contains(p.Command, "transmission-daemon") || !processCommandHasConfigDir(p.Command, dir) {
			continue
		}
		found = true
		if userName != "" && p.User != userName {
			reasons = append(reasons, fmt.Sprintf("daemon(pid=%d) 以 %s 运行，应为 %s", p.PID, p.User, userName))
			continue
		}
		parent, ok := byPID[p.PPID]
		if !ok {
			reasons = append(reasons, fmt.Sprintf("daemon(pid=%d) 的父进程 %d 不在进程表里", p.PID, p.PPID))
			continue
		}
		if !strings.Contains(parent.Command, "transmission-supervise") {
			reasons = append(reasons, fmt.Sprintf("daemon(pid=%d) 的父进程是 %q，不是 transmission-supervise",
				p.PID, tailText(parent.Command, 80)))
			continue
		}
		return ""
	}
	if !found {
		return "进程表里没有以 --config-dir " + dir + " 运行的 transmission-daemon"
	}
	return strings.Join(reasons, "；")
}

// verifyTransmissionSupervised 轮询回读进程树，直到 daemon 确实是 supervisor 的子进程
// 且以真实用户运行；超时/对不上都如实失败（不许把"端口在听"当成装好了）。
func (m *Manager) verifyTransmissionSupervised(ctx context.Context, p TransmissionPaths, res *InstallResult) error {
	timeout := m.transmissionTimeout()
	deadline := time.Now().Add(timeout)
	last := "还没有回读进程树"
	for time.Now().Before(deadline) {
		procs, err := transmissionProcessTable(ctx)
		switch {
		case err != nil:
			last = "执行 ps 失败：" + err.Error()
		default:
			last = transmissionSuperviseProblem(procs, p.ConfigDir, m.opt.UserName)
			if last == "" {
				if res != nil {
					res.step(ctx, "已回读进程树："+p.Bin+" 是 transmission-supervise（面板二进制）的子进程，以 "+
						m.opt.UserName+" 身份运行")
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("回读进程树被中断：%s", last)
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("Transmission 的进程树不对：%s。"+
		"面板**不会**把这次安装报成成功 —— daemon 必须是 transmission-supervise（面板二进制）的子进程、"+
		"以 %s 身份运行，否则它继承不到面板的权限（外接盘 / 受保护目录会写不进去）；日志：%s",
		last, transmissionRunAsName(m), m.transmissionLogPath())
}

// ---------- 安装 ----------

// InstallTransmission 安装 transmission-cli、设好 RPC 口令、验收并纳入服务管理。
//
// 幂等：包已装就跳过 brew install；已有 settings.json 时**保留其它字段**（下载目录、
// 端口映射都可能被用户改过），只回填凭据。
func (m *Manager) InstallTransmission(ctx context.Context, res *InstallResult) error {
	if res == nil {
		res = &InstallResult{App: "transmission"}
	}
	if res.Name == "" {
		res.Name = "Transmission（下载）"
	}

	// ---- 1. Homebrew 是硬前提 ----
	if strings.TrimSpace(m.opt.BrewBin) == "" {
		return fmt.Errorf("未配置 Homebrew 路径，无法自动安装 Transmission。请先安装 Homebrew")
	}
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		return fmt.Errorf("未安装 Homebrew（%s 不存在），无法自动安装 Transmission。请先安装 Homebrew", m.opt.BrewBin)
	}

	// ---- 2. 本体 ----
	if !m.brewHas(ctx, transmissionFormula) {
		res.step(ctx, "正在 brew install "+transmissionFormula+"（首次可能需要几分钟）")
		if _, err := m.brewInstall(ctx, res, 30*time.Minute, transmissionFormula); err != nil {
			return err
		}
		res.step(ctx, transmissionFormula+" 已安装")
	} else {
		res.step(ctx, transmissionFormula+" 已安装（跳过 brew install）")
	}

	// ---- 2.5 路径：daemon 二进制必须真的在（否则要等 launchd 起不来才发现） ----
	p := m.transmissionPaths()
	if strings.TrimSpace(p.Home) == "" {
		return fmt.Errorf("无法确定运行 Transmission 的真实用户家目录（UserHome / UserName 都为空）")
	}
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行 Transmission 的真实用户（UserName 为空）")
	}
	if _, err := os.Stat(p.Bin); err != nil {
		return fmt.Errorf("brew 装完了但找不到 %s：请执行 `brew reinstall %s` 后重试",
			p.Bin, transmissionFormula)
	}

	// ---- 3. 生成凭据（先挂进凭据区：后面任一步失败，口令的唯一记录也能到达用户） ----
	user, err := generateMinifluxSecret(transmissionUserLen)
	if err != nil {
		return fmt.Errorf("生成 RPC 用户名失败: %w", err)
	}
	password, err := generateMinifluxSecret(transmissionPasswordLen)
	if err != nil {
		return fmt.Errorf("生成 RPC 口令失败: %w", err)
	}
	res.Credentials = append(res.Credentials, Credential{
		Key: "transmission_rpc_user", Value: user, Label: "Transmission Web UI / RPC 用户名",
	})
	res.Credentials = append(res.Credentials, Credential{
		Key: "transmission_rpc_password", Value: password,
		Label: "Transmission Web UI / RPC 口令（面板随机生成；在 Web UI 里改过之后就失效）",
	})

	// ---- 3.5 摘掉 brew 的旧作业（2026-10-06 起 transmission 由面板托管） ----
	//
	// 不摘干净就会出现**两份实例抢 9091**：brew 的 plist 带 RunAtLoad/KeepAlive，重启后
	// 它自己会回来。三处都清：用户级 + 系统级 plist（历史搬迁产物），两套前缀都认。
	res.step(ctx, "停掉 brew 的 transmission 旧作业（"+strings.Join(transmissionBrewLabels(), " / ")+"）")
	if err := m.retireBrewTransmission(ctx, res); err != nil {
		return err
	}

	// ---- 4. **先停服务**再改 settings.json ----
	//
	// 真机实测（2026-09-20）：transmission-daemon 关闭时会把**内存里的配置**写回
	// settings.json。运行中改写它再重启，旧进程退出时会把我们写进去的
	// rpc-username / rpc-authentication-required / rpc-bind-address 全冲掉
	// （只剩 rpc-password 的哈希），结果是"看起来设了口令、其实 RPC 无口令"。
	// 所以顺序必须是：停 → 等端口释放 → 改配置 → 启动。
	if err := m.stopTransmissionService(ctx); err != nil {
		if m.portHasListener(transmissionPort) {
			return fmt.Errorf("停不掉 transmission（端口 %d 仍在监听，改配置会被它回写覆盖）：%w；日志：%s",
				transmissionPort, err, m.transmissionLogPath())
		}
		res.step(ctx, "服务当前未在运行（无需停止）")
	} else {
		res.step(ctx, "已停止服务 "+transmissionLabel+"（改配置前必须先停，否则会被它回写覆盖）")
	}
	if !m.waitTransmissionPortFree(ctx) {
		return fmt.Errorf("端口 %d 在超时内仍被占用（transmission 没真的停下来）："+
			"现在改 settings.json 一定会被旧进程覆盖，安装已中止；日志：%s",
			transmissionPort, m.transmissionLogPath())
	}

	settings := m.transmissionSettingsPath()
	// 已有配置文件就保留其它字段（下载目录、端口映射都可能被用户改过）；
	// 没有就从空配置起（transmission 对缺失字段用默认值，部分 JSON 是合法的）。
	var raw []byte
	if b, rerr := os.ReadFile(settings); rerr == nil {
		raw = b
	} else if !os.IsNotExist(rerr) {
		return fmt.Errorf("读取现有配置 %s 失败: %w", settings, rerr)
	}
	// ---- 4.5 下载目录必须真能用（不可写就现在失败，不许"能登录但什么都下不了"）----
	// 真机实测（2026-09-20）：目录不可写时 transmission 照样把 torrent-add 报成
	// success（前端也不报错），用户只看到"加了种子没反应"（坑 226）。
	dlDir := m.transmissionEffectiveDownloadDir(raw)
	if dlDir == "" {
		return fmt.Errorf("读不到下载目录、也推导不出默认值（缺用户家目录）：请先指定下载目录再重试")
	}
	if err := m.ensureTransmissionDownloadDir(ctx, dlDir); err != nil {
		return err
	}
	res.step(ctx, "下载目录 "+dlDir+" 存在，且以运行用户身份实测可写")

	desired := transmissionDesiredSettings(user, password)
	desired["download-dir"] = dlDir
	merged, changed, err := transmissionMergeSettings(raw, desired)
	if err != nil {
		return fmt.Errorf("%v（%s）", err, settings)
	}
	if changed {
		if err := m.writeTransmissionSettings(settings, merged); err != nil {
			return err
		}
		res.step(ctx, "已写入 RPC 凭据与回环绑定到 "+settings+"（权限 0600；口令不会写进任务日志）")
	} else {
		res.step(ctx, "现有 "+settings+" 已是目标值（未改动）")
	}

	// ---- 5. 写面板自己的系统级 plist，再启动（明文口令在这一步被哈希） ----
	plist, err := m.ensureTransmissionPlist(ctx)
	if err != nil {
		return err
	}
	res.step(ctx, "已写入系统级 LaunchDaemon "+transmissionLabel+"（"+plist+"；由面板 supervisor 以 "+m.opt.UserName+" 身份运行 daemon）")
	if err := m.startTransmissionService(ctx); err != nil {
		return fmt.Errorf("启动 Transmission 失败: %w；日志：%s", err, m.transmissionLogPath())
	}

	// ---- 6. 回读 settings.json，确认凭据真的生效 ----
	res.step(ctx, "回读 "+settings+" 确认 RPC 凭据真的生效")
	if ok, why := m.verifyTransmissionCredentialsApplied(settings, user, password); !ok {
		return fmt.Errorf("Transmission 的 RPC 凭据没有生效：%s。"+
			"面板**不会**把这次安装报成成功 —— 没有口令的 9091 等于谁都能改下载目录、删文件。"+
			"日志：%s", why, m.transmissionLogPath())
	}
	res.step(ctx, "回读确认：rpc-authentication-required=true、rpc-username 与凭据一致、"+
		"rpc-password 已是哈希（明文不存在）、download-dir="+dlDir)
	if rb, rerr := os.ReadFile(settings); rerr != nil {
		return fmt.Errorf("回读 %s 失败: %w", settings, rerr)
	} else if got := transmissionSettingsString(rb, "download-dir"); got != dlDir {
		return fmt.Errorf("下载目录没有生效：写的是 %s，回读到 %s（非空 %q 才算数）", dlDir, got, got)
	}

	// ---- 7. RPC 自检：无凭据 401、带凭据 200/409 ----
	rpcURL := "http://127.0.0.1:" + strconv.Itoa(transmissionPort) + "/transmission/rpc"
	res.step(ctx, "RPC 自检：无凭据必须 401、带凭据必须 200（"+rpcURL+"）")
	unauth, auth, lastErr := m.probeTransmissionRPC(ctx, rpcURL, user, password)
	if unauth != http.StatusUnauthorized {
		return fmt.Errorf("Transmission RPC 自检失败：不带凭据的请求返回 %d（应为 401）—— "+
			"RPC 仍可在无口令下访问，本次安装**不算成功**（诊断：%s）；日志：%s",
			unauth, lastErr, m.transmissionLogPath())
	}
	// transmission 对**认证之后**的 GET /transmission/rpc 也可能回 409（缺 session id），
	// 所以 200 与 409 都算"凭据被接受"；401 才是"口令不对"。
	if auth != http.StatusOK && auth != http.StatusConflict {
		return fmt.Errorf("Transmission RPC 自检失败：带面板生成的凭据返回 %d（应为 200/409）—— "+
			"口令写进了文件但服务读到的不是它；日志：%s", auth, m.transmissionLogPath())
	}
	res.step(ctx, "RPC 自检通过：无凭据 "+strconv.Itoa(unauth)+"、带凭据 "+strconv.Itoa(auth))

	// ---- 8. 健康检查（Web UI，**带凭据**） ----
	//
	// 开了 rpc-authentication-required 之后，整站（含 /transmission/web/）都要 HTTP Basic：
	// 无凭据是 **401**，带凭据才是 200（真机实测）。所以健康检查必须用凭据打 ——
	// 这同时证明了"服务在听 + 口令真的生效 + 页面能出来"三件事。
	webURL := "http://127.0.0.1:" + strconv.Itoa(transmissionPort) + "/transmission/web/"
	if !m.waitTransmissionStatus(ctx, webURL, user, password,
		func(c int) bool { return c == http.StatusOK }) {
		return fmt.Errorf("Transmission Web UI 没有就绪（%s 带凭据未返回 200）—— 安装**不算成功**；日志：%s",
			webURL, m.transmissionLogPath())
	}
	res.step(ctx, "健康检查通过："+webURL+" 带凭据返回 200（无凭据 401 = 口令已强制）")

	// ---- 8.2 回读进程树：daemon 必须是面板 supervisor 的子进程、以真实用户运行 ----
	//
	// 这一条是 2026-10-06 改造的核心承诺（继承面板权限、文件归属用户）。只验端口/HTTP
	// 会漏掉"其实是 brew 旧实例还在跑"或"以 root 跑"这两种情况 —— 那就等于没换托管方式。
	res.step(ctx, "回读进程树：确认 daemon 是 transmission-supervise 的子进程、以 "+m.opt.UserName+" 身份运行")
	if err := m.verifyTransmissionSupervised(ctx, p, res); err != nil {
		return err
	}

	// ---- 8.5 中文界面（面板内嵌的第三方前端，写 web 根目录，不需要重启） ----
	//
	// 官方 4.x 网页版只有英文（见 transmission_webui.go 文件头），用户要中文。
	// 写不进去不影响下载器本体可用 ⇒ 只写警告，并把"现在打开看到的是什么"说清。
	// 真正生效的判据在最后一步（从 9091 取回页面核对中文），不在这里下结论。
	webUIApplied := false
	res.step(ctx, "写入中文界面（Transmission Next UI v"+transmissionWebUIVersion+"）")
	if err := m.EnsureTransmissionWebUI(ctx, res); err != nil {
		res.Warning = appendWarning(res.Warning,
			"中文界面没装上（现在打开是官方英文界面）："+err.Error())
	} else {
		webUIApplied = true
	}

	// ---- 9. 中文界面的**端到端**判据（放在服务状态定下来之后） ----
	//
	// 文件写进去只证明"磁盘上有"；这里从 9091 把首页与它引用的脚本取回来，
	// 脚本里必须有中文 —— 否则如实警告（不谎报"已是中文界面"）。
	if webUIApplied {
		if err := m.verifyTransmissionWebUIServed(ctx, user, password); err != nil {
			res.Warning = appendWarning(res.Warning,
				"中文界面没有生效（现在打开仍是官方英文界面）："+err.Error())
		} else {
			res.step(ctx, "已从 "+webURL+" 取回页面核对：界面脚本里有中文（中文界面生效）")
		}
	}

	// ---- 10. 登记进服务管理 ----
	//
	// label 固定用面板自己的（不再问 brew）：进程树那一步已经证明跑的是我们的 supervisor。
	if err := m.RegisterInstalledService(ctx, transmissionLabel, "Transmission", "🧲", "tool", transmissionPort); err != nil {
		res.step(ctx, "（自动登记到面板失败："+err.Error()+"，可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
	}

	// 绑 0.0.0.0（2026-09-23 局域网直连）⇒ 广告面板探测到的主机地址。
	res.Address = "http://" + m.primaryIP() + ":" + strconv.Itoa(transmissionPort) + "/transmission/web/"
	res.Message = "「Transmission（下载）」已安装并纳入管理"
	res.step(ctx,
		"打开 "+res.Address+"，用凭据区里的用户名与口令登录",
		"默认下载目录是 "+dlDir+"；可在 Web UI 的「设置」里改。")
	return nil
}

// transmissionStopService / transmissionStartService 是"生命周期动作"的注入点
// （包级变量，理由同上面的 HTTP 探测）：单测不许跑真实 launchd，
// 而 live 验证要能模拟"服务启动时把明文口令换成哈希"这个关键副作用。
//
// 默认实现走**面板自己的 LaunchDaemon**（不再是 brew services）：stop = bootout 我们的 label，
// start = 写 plist + bootstrap（bootstrapService 会先 bootout 再装，幂等）。
var (
	transmissionStopService = func(m *Manager, ctx context.Context) error {
		// 顺手摘掉 brew 的旧作业：老安装（记录还是 homebrew.mxcl.*）直接点「⚙️ RPC 设置」
		// 时也能完成迁移，不会卡在"端口 9091 仍被占用"。幂等，正常情况是空操作。
		if err := m.retireBrewTransmission(ctx, nil); err != nil {
			return err
		}
		return priv.LaunchUnload(transmissionLabel)
	}
	transmissionStartService = func(m *Manager, ctx context.Context) error {
		plist, err := m.ensureTransmissionPlist(ctx)
		if err != nil {
			return err
		}
		return transmissionLaunch(m, ctx, transmissionLabel, plist)
	}
)

// 注：没有 restartTransmissionService —— 改配置的正确顺序是"停 → 写 → 启动"，
// 用 restart 会让旧进程在退出时回写内存配置、覆盖刚写进去的凭据（真机实测，见文件头）。

func (m *Manager) stopTransmissionService(ctx context.Context) error {
	return transmissionStopService(m, ctx)
}

func (m *Manager) startTransmissionService(ctx context.Context) error {
	return transmissionStartService(m, ctx)
}

// waitTransmissionPortFree 等端口真的没有监听者（**在改配置之前**必须成立：
// 旧进程还活着就会在退出时把内存配置回写、覆盖我们写进去的凭据 —— 真机实测）。
func (m *Manager) waitTransmissionPortFree(ctx context.Context) bool {
	deadline := time.Now().Add(m.transmissionTimeout())
	for time.Now().Before(deadline) {
		if !m.portHasListener(transmissionPort) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
	return !m.portHasListener(transmissionPort)
}

// waitTransmissionSettingsFile 等 settings.json 出现（仅 live 排查用）。
// 读到了就返回内容；超时/读失败如实报错，不编造一份空配置顶上。
func (m *Manager) waitTransmissionSettingsFile(ctx context.Context, path string) ([]byte, error) {
	deadline := time.Now().Add(m.transmissionTimeout())
	var last error = fmt.Errorf("还没出现")
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("等不到 %s 落盘（%v）：Transmission 起不来或配置目录不可写；看日志 %s",
		path, last, m.transmissionLogPath())
}

// probeTransmissionRPC 打两次 /transmission/rpc：一次不带凭据、一次带凭据。
// 返回 (无凭据状态码, 带凭据状态码, 最后一次错误原文)。
func (m *Manager) probeTransmissionRPC(ctx context.Context, url, user, password string) (int, int, string) {
	deadline := time.Now().Add(m.transmissionTimeout())
	unauth, auth, lastErr := 0, 0, ""
	// transmission 的 rpc 端口起来得比 web 慢一点：轮询到两个结论都像样为止。
	for time.Now().Before(deadline) {
		if code, err := m.transmissionHTTP(ctx, url, "", ""); err == nil {
			unauth = code
		} else {
			lastErr = err.Error()
		}
		if code, err := m.transmissionHTTP(ctx, url, user, password); err == nil {
			auth = code
		} else {
			lastErr = err.Error()
		}
		if unauth == http.StatusUnauthorized && (auth == http.StatusOK || auth == http.StatusConflict) {
			return unauth, auth, lastErr
		}
		select {
		case <-ctx.Done():
			return unauth, auth, lastErr
		case <-time.After(time.Second):
		}
	}
	return unauth, auth, lastErr
}

// waitTransmissionStatus 轮询直到状态码满足条件（超时返回 false，由调用方如实报错）。
// 传 user/password：开了认证之后整站（含 Web UI）都要 Basic 凭据，无凭据只会拿到 401。
func (m *Manager) waitTransmissionStatus(ctx context.Context, url, user, password string, ok func(int) bool) bool {
	deadline := time.Now().Add(m.transmissionTimeout())
	for time.Now().Before(deadline) {
		if code, err := m.transmissionHTTP(ctx, url, user, password); err == nil && ok(code) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// writeTransmissionSettings 写 settings.json：0600 + 真实用户属主（服务以真实用户身份
// 运行，权限/属主不对它读不到也写不回）。
func (m *Manager) writeTransmissionSettings(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("设置 %s 权限失败: %w", path, err)
	}
	if m.opt.UserName != "" {
		if err := chownTo(m.opt.UserName, path); err != nil {
			return fmt.Errorf("把 %s 归属改为 %s 失败: %w", path, m.opt.UserName, err)
		}
		// 配置目录本身也必须归用户：全新安装时它是面板以 root 建的，而 daemon 要在里面
		// 建 Resume/ Torrents/ 等子目录、并用"临时文件 + rename"回写 settings.json ——
		// 目录是 root 所有的话这些全都失败（daemon 起得来但存不住任何状态）。
		if err := chownTo(m.opt.UserName, filepath.Dir(path)); err != nil {
			return fmt.Errorf("把配置目录 %s 归属改为 %s 失败: %w", filepath.Dir(path), m.opt.UserName, err)
		}
	}
	return nil
}

// verifyTransmissionCredentialsApplied 回读 settings.json，确认服务真的读到了面板写的配置：
// rpc-authentication-required=true、rpc-username 与凭据一致、rpc-password 已变成哈希
// （`{<salt><hash>`）、dht=true、lpd/port-forwarding=false（见 transmissionCredentialsProblem）。
//
// 为什么必须回读（AGENTS 第三节"判据贴运行体"）：写进去只证明"文件里有这一行"。
// 真机实测（2026-09-20）正是"文件里有哈希、auth-required 却是 false"—— 只看口令是否
// 被哈希会漏判，所以这里逐字段核对，并把 daemon 的最终落盘结果当成唯一判据。
func (m *Manager) verifyTransmissionCredentialsApplied(path, user, plaintext string) (bool, string) {
	deadline := time.Now().Add(m.transmissionTimeout())
	last := "还没有读到 " + path
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		switch {
		case err != nil:
			last = "读不到 " + path + "：" + err.Error()
		default:
			last = transmissionCredentialsProblem(raw, user, plaintext)
			if last == "" {
				return true, ""
			}
		}
		// 300ms 一跳：回读很快，不需要 1 秒粒度（真机启动到落盘通常在 1~2 跳内）。
		select {
		case <-time.After(300 * time.Millisecond):
		}
	}
	return false, last
}

// transmissionCredentialsProblem 返回"哪一项没生效"（空串 = 全部生效）。
// 抽成纯函数：单测不用起服务就能把每一条判据钉死。
func transmissionCredentialsProblem(raw []byte, user, plaintext string) string {
	if v, ok := transmissionSettingsBool(raw, "rpc-authentication-required"); !ok || !v {
		return "rpc-authentication-required 不是 true（RPC 仍可无口令访问）"
	}
	if got := transmissionSettingsString(raw, "rpc-username"); got != user {
		return "rpc-username 与面板写入的不一致（读到 " + strconv.Quote(got) + "）"
	}
	pw := transmissionSettingsString(raw, "rpc-password")
	switch {
	case pw == "":
		return "settings.json 里没有 rpc-password 字段"
	case pw == plaintext:
		return "rpc-password 仍是明文（服务还没读到新配置）"
	case !strings.HasPrefix(pw, "{"):
		return "rpc-password 既不是明文、也不像 transmission 的哈希（值形如 " +
			strconv.Quote(pw[:minInt(len(pw), 8)]) + "…）"
	}
	// dht 必须 true（磁力链没 tracker 时唯一的 peer 来源），lpd / port-forwarding
	// 必须 false（局域网组播会触发 macOS「本地网络」授权）。真机实测见坑 226。
	if v, ok := transmissionSettingsBool(raw, "dht-enabled"); !ok || !v {
		return "dht-enabled 不是 true（无 tracker 的磁力链会 peers=0、永远不动）"
	}
	for _, key := range []string{"lpd-enabled", "port-forwarding-enabled"} {
		if v, ok := transmissionSettingsBool(raw, key); !ok || v {
			return key + " 不是 false（会触发 macOS「本地网络」授权弹窗）"
		}
	}
	return ""
}

// minInt 是两数取小（显式写，避免依赖内建 min 的版本差异）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- 面板正规入口：改 RPC 凭据 / 下载目录 ----------

// TransmissionSettingsInfo 是面板能读到的、Transmission 的当前生效值（口令永不回显）。
type TransmissionSettingsInfo struct {
	Username     string `json:"username"`
	DownloadDir  string `json:"download_dir"`
	AuthRequired bool   `json:"auth_required"`
	DHTEnabled   bool   `json:"dht_enabled"`
	RPCBind      string `json:"rpc_bind_address"`
	SettingsPath string `json:"settings_path"`
}

// transmissionSettingsInfoFrom 从 settings.json 的原始字节读出信息（纯函数，可单测）。
func transmissionSettingsInfoFrom(path string, raw []byte) TransmissionSettingsInfo {
	auth, _ := transmissionSettingsBool(raw, "rpc-authentication-required")
	dht, _ := transmissionSettingsBool(raw, "dht-enabled")
	return TransmissionSettingsInfo{
		Username:     transmissionSettingsString(raw, "rpc-username"),
		DownloadDir:  transmissionSettingsString(raw, "download-dir"),
		AuthRequired: auth,
		DHTEnabled:   dht,
		RPCBind:      transmissionSettingsString(raw, "rpc-bind-address"),
		SettingsPath: path,
	}
}

// TransmissionSettingsInfo 读当前 settings.json（读不到就如实报错，不编默认值）。
func (m *Manager) TransmissionSettingsInfo() (TransmissionSettingsInfo, error) {
	path := m.transmissionSettingsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return TransmissionSettingsInfo{}, fmt.Errorf("读不到 %s（Transmission 可能还没装或还没启动过）: %w", path, err)
	}
	return transmissionSettingsInfoFrom(path, raw), nil
}

// SetTransmissionRPCSettings 是面板改 RPC 凭据 / 下载目录的**唯一正规入口**。
//
// 顺序固定为「停 → 等端口释放 → 写 → 启动 → 回读逐字段核对」（坑 226）：
// daemon 退出时会把内存配置回写 settings.json，运行中改写 / 手工改完直接重启都会丢。
// username/password 为空表示保留现有值；downloadDir 为空表示保留现有值。
func (m *Manager) SetTransmissionRPCSettings(ctx context.Context, res *InstallResult,
	username, password, downloadDir string) (TransmissionSettingsInfo, error) {
	if res == nil {
		res = &InstallResult{App: "transmission"}
	}
	path := m.transmissionSettingsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return TransmissionSettingsInfo{}, fmt.Errorf("读不到 %s（先在面板里安装并启动 Transmission）: %w", path, err)
	}
	// 目标值先算齐（缺的沿用现有值），任何一项算不出来都别动服务。
	user := strings.TrimSpace(username)
	if user == "" {
		user = transmissionSettingsString(raw, "rpc-username")
	}
	if user == "" {
		return TransmissionSettingsInfo{}, fmt.Errorf("RPC 用户名不能为空（当前 settings.json 里也没有）")
	}
	if strings.ContainsAny(user, "\r\n") {
		return TransmissionSettingsInfo{}, fmt.Errorf("RPC 用户名不能含换行")
	}
	pw := password
	keepPassword := false
	if pw == "" {
		// 留空 = 保留现有哈希（口令不重设）；现有值不是哈希（缺失/明文）才生成新的。
		if cur := transmissionSettingsString(raw, "rpc-password"); strings.HasPrefix(cur, "{") {
			keepPassword = true
		} else if pw, err = generateMinifluxSecret(transmissionPasswordLen); err != nil {
			return TransmissionSettingsInfo{}, fmt.Errorf("生成 RPC 口令失败: %w", err)
		}
	}
	if strings.ContainsAny(pw, "\r\n") {
		return TransmissionSettingsInfo{}, fmt.Errorf("RPC 口令不能含换行")
	}
	dlDir := strings.TrimSpace(downloadDir)
	if dlDir == "" {
		dlDir = transmissionSettingsString(raw, "download-dir")
	}
	if dlDir == "" {
		return TransmissionSettingsInfo{}, fmt.Errorf("下载目录不能为空：请在面板里指定一个可写目录")
	}

	// 1) 先停服务（运行中改配置会被 daemon 退出时的回写覆盖）。
	if err := m.stopTransmissionService(ctx); err != nil {
		if m.portHasListener(transmissionPort) {
			return TransmissionSettingsInfo{}, fmt.Errorf("停不掉 transmission（端口 %d 仍在监听，改配置会被回写覆盖）：%w",
				transmissionPort, err)
		}
		res.step(ctx, "服务当前未在运行（无需停止）")
	} else {
		res.step(ctx, "已停止 "+transmissionFormula+"（改配置前必须先停）")
	}
	// 2) 等端口真的释放。
	if !m.waitTransmissionPortFree(ctx) {
		return TransmissionSettingsInfo{}, fmt.Errorf("端口 %d 在超时内仍被占用（transmission 没真的停下来）："+
			"此刻改 settings.json 一定会被旧进程覆盖，已中止", transmissionPort)
	}
	// 3) 下载目录先验证可写（不可写就不写配置、不启动，如实失败）。
	if err := m.ensureTransmissionDownloadDir(ctx, dlDir); err != nil {
		return TransmissionSettingsInfo{}, err
	}
	// 4) 写配置（合并，保留用户其它字段；口令留空则保留现有哈希、不覆盖）。
	desired := transmissionDesiredSettings(user, pw)
	if keepPassword {
		delete(desired, "rpc-password")
	}
	desired["download-dir"] = dlDir
	merged, changed, err := transmissionMergeSettings(raw, desired)
	if err != nil {
		return TransmissionSettingsInfo{}, fmt.Errorf("%v（%s）", err, path)
	}
	if changed {
		if err := m.writeTransmissionSettings(path, merged); err != nil {
			return TransmissionSettingsInfo{}, err
		}
		res.step(ctx, "已写入 "+path+"（权限 0600；口令不会写进任务日志）")
	} else {
		res.step(ctx, "配置已是目标值（未改动），仍会重启并回读核对")
	}
	// 5) 启动让它读新配置（明文口令在这一步被哈希）。
	if err := m.startTransmissionService(ctx); err != nil {
		return TransmissionSettingsInfo{}, fmt.Errorf("启动 %s 失败: %w；日志：%s",
			transmissionFormula, err, m.transmissionLogPath())
	}
	// 6) 回读逐字段核对（凭据 + 下载目录 + 开关），任一项不符就如实失败。
	res.step(ctx, "回读 "+path+" 逐字段核对")
	if ok, why := m.verifyTransmissionCredentialsApplied(path, user, pw); !ok {
		return TransmissionSettingsInfo{}, fmt.Errorf("新的 RPC 凭据没有生效：%s。"+
			"面板**不会**把这次修改报成成功；日志：%s", why, m.transmissionLogPath())
	}
	rb, err := os.ReadFile(path)
	if err != nil {
		return TransmissionSettingsInfo{}, fmt.Errorf("回读 %s 失败: %w", path, err)
	}
	if got := transmissionSettingsString(rb, "download-dir"); got != dlDir {
		return TransmissionSettingsInfo{}, fmt.Errorf("下载目录没有生效：写的是 %s，回读到 %s", dlDir, got)
	}
	info := transmissionSettingsInfoFrom(path, rb)
	// 7) RPC 自检：无凭据必须 401（有明文口令时再验"带凭据 200/409"）。
	rpcURL := "http://127.0.0.1:" + strconv.Itoa(transmissionPort) + "/transmission/rpc"
	if keepPassword {
		// 口令沿用原哈希、面板不知道明文，带凭据自检做不了（只验"无凭据仍被拒"）。
		if code, herr := m.transmissionHTTP(ctx, rpcURL, "", ""); code != http.StatusUnauthorized {
			return TransmissionSettingsInfo{}, fmt.Errorf("改完之后无凭据访问 RPC 返回 %d（应为 401）：%v", code, herr)
		}
		res.step(ctx, "口令未重设（沿用原哈希），无凭据 401 已确认；带凭据自检需知道明文，已跳过")
	} else {
		unauth, auth, lastErr := m.probeTransmissionRPC(ctx, rpcURL, user, pw)
		if unauth != http.StatusUnauthorized || (auth != http.StatusOK && auth != http.StatusConflict) {
			return TransmissionSettingsInfo{}, fmt.Errorf("改完之后 RPC 自检失败：无凭据 %d（应为 401）、"+
				"带凭据 %d（应为 200/409）；诊断：%s", unauth, auth, lastErr)
		}
		res.step(ctx, "回读确认：RPC 认证开启、用户名与下载目录与面板写入一致、口令已是哈希")
	}
	res.Credentials = append(res.Credentials,
		Credential{Key: "transmission_rpc_user", Value: user, Label: "Transmission Web UI / RPC 用户名"})
	if !keepPassword {
		res.Credentials = append(res.Credentials,
			Credential{Key: "transmission_rpc_password", Value: pw,
				Label: "Transmission Web UI / RPC 口令（只在这次任务结果里出现一次）"})
	}
	return info, nil
}

// ---------- 卸载 ----------

// uninstallTransmission 卸载 Transmission：停服务 + 删 plist + 删面板记录 + brew uninstall。
//
//   - 配置目录（RPC 凭据哈希、daemon 日志）**默认保留**，勾选 removeData 才删；
//   - **下载下来的文件永远不删**（它们在用户设置的下载目录里，面板不知道也不该猜）。
func (m *Manager) uninstallTransmission(ctx context.Context, removeData bool, result *InstallResult) error {
	p := m.transmissionPaths()
	if result != nil {
		result.step(ctx, "停止并删除 launchd 服务 "+transmissionLabel)
	}
	if err := transmissionStop(m, ctx, transmissionLabel, p.Plist); err != nil {
		return fmt.Errorf("停止 %s 失败: %w", transmissionLabel, err)
	}
	// 历史遗留：brew 前缀的作业也摘一遍（正常安装时已清过；幂等，不会报错）。
	for _, label := range transmissionBrewLabels() {
		if err := transmissionBootout(m, ctx, label); err != nil && result != nil {
			result.step(ctx, "（旧作业 "+label+" 停止时报告："+err.Error()+"）")
		}
		for _, path := range []string{m.systemDaemonUserPlist(label), SystemDaemonPlistPath(label)} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("删除旧作业的 plist %s 失败: %w", path, err)
			}
		}
	}
	// 面板记录：新 label + brew 旧前缀都清（别名记录不清会在「已安装」里留残留卡片，坑 228）。
	if _, err := m.ForgetByLabels(ctx, append([]string{transmissionLabel}, transmissionBrewLabels()...)...); err != nil {
		return fmt.Errorf("删除面板服务记录失败: %w", err)
	}

	if m.brewHas(ctx, transmissionFormula) {
		if result != nil {
			result.step(ctx, "正在 brew uninstall "+transmissionFormula)
		}
		if _, err := m.brewRun(ctx, 5*time.Minute, "uninstall", transmissionFormula); err != nil {
			return fmt.Errorf("brew uninstall %s 失败: %w", transmissionFormula, err)
		}
	} else if result != nil {
		result.step(ctx, transmissionFormula+" 未安装，跳过 brew uninstall")
	}

	dir := m.transmissionConfigDir()
	if removeData {
		if err := m.removeTree(ctx, dir, result); err != nil {
			return err
		}
	} else if result != nil {
		result.step(ctx, "保留配置目录 "+dir+"（RPC 凭据与日志；需要彻底清理请勾选删除数据）")
	}
	if result != nil {
		result.step(ctx, "已下载的文件一个都没有删（默认下载目录 "+p.DownloadDir+" 或你自己设置的目录）")
	}
	return nil
}
