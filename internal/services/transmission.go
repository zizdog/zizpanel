package services

// ============================================================================
//  Transmission（下载器）安装器
//
//  为什么不能走通用 brew 流程：formula 的 service 块会起
//  `transmission-daemon --foreground --config-dir <brew>/var/transmission/`，
//  而上游默认 `rpc-authentication-required: false` + `rpc-whitelist: 127.0.0.1`
//  —— 谁打开 9091 谁就能改下载目录、加种子、删文件。所以这里把"设口令"做掉：
//
//    1. brew install transmission-cli（30 分钟超时）
//    2. brew services start（它自己会写出首份 settings.json）
//    3. 合并写回 settings.json：rpc-username/rpc-password + 强制认证 + 只绑回环
//    4. 重启服务，让它读新配置
//    5. **回读 settings.json**：transmission 启动时会把明文口令换成带盐哈希
//       （`{<salt><hash>`），明文还在 = 没生效 → 安装如实失败
//    6. RPC 自检：无凭据必须 401、带凭据必须 200/409（否则如实失败）
//    7. 健康检查 GET /transmission/web/ + 登记服务
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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

const (
	transmissionFormula = "transmission-cli"
	transmissionLabel   = "homebrew.mxcl.transmission-cli"
	transmissionPort    = 9091

	// 用户名 12 位、口令 20 位（[A-Za-z0-9]，与 Syncthing/Miniflux 同一字符集）。
	transmissionUserLen     = 12
	transmissionPasswordLen = 20
)

// ---------- 路径（一律从 brew 前缀推导，单测才能落在临时目录里） ----------

// transmissionConfigDir 是 formula 的 service 块写死的位置：<brew>/var/transmission/。
// 推导而不写死 /opt/homebrew：Intel 机器上是 /usr/local，写死会让安装器去改一个
// 不存在的位置，而服务其实从另一个位置读配置。
func (m *Manager) transmissionConfigDir() string {
	return filepath.Join(m.brewPrefix(), "var", "transmission")
}

func (m *Manager) transmissionSettingsPath() string {
	return filepath.Join(m.transmissionConfigDir(), "settings.json")
}

func (m *Manager) transmissionLogPath() string {
	return filepath.Join(m.transmissionConfigDir(), "transmission-daemon.log")
}

// ---------- 纯函数：settings.json 合并 ----------

// transmissionDesiredSettings 是面板**负责**的那几个键（其余字段原样保留）。
//
// rpc-whitelist-enabled 保持 true 而不是放开：它只绑回环 + 白名单只含回环，
// 等于"即使口令被猜到也进不来"的第二道门。用户要局域网访问时自行改这两项。
//
// dht / lpd（LSD 局域网发现）/ port-forwarding（UPnP-NAT-PMP）默认**关掉**：
// macOS 15+ 的「本地网络」隐私权限会被这三项触发，而 brew 的 transmission-daemon
// 是 ad-hoc 签名 ⇒ 授权会在 brew 升级/重装后失效、反复弹窗（与坑 202 同类）。
// 关掉它们只影响局域网发现与自动端口映射，公网 peer / tracker 能力不变。
func transmissionDesiredSettings(user, password string) map[string]any {
	return map[string]any{
		"rpc-enabled":                 true,
		"rpc-authentication-required": true,
		"rpc-username":                user,
		"rpc-password":                password,
		"rpc-bind-address":            "127.0.0.1",
		"rpc-whitelist-enabled":       true,
		"rpc-whitelist":               "127.0.0.1,::1",
		"rpc-port":                    transmissionPort,
		// 不触发 macOS「本地网络」授权弹窗（用户可在 Web UI 设置里自行打开）。
		"dht-enabled":             false,
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
	cfg := map[string]any{}
	if strings.TrimSpace(string(raw)) != "" {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, false, fmt.Errorf("现有 settings.json 不是合法 JSON（%v）："+
				"面板不覆盖它，请先修好或删掉再重装", err)
		}
	}
	changed := false
	for k, v := range transmissionDesiredSettings(user, password) {
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
)

// transmissionHTTP 发一次 GET，返回状态码；user 非空时带 basic auth。
// 用 Go 客户端而不是 `curl -u user:pass`：后者会把口令放进 argv（本项目出过 argv 泄漏）。
func (m *Manager) transmissionHTTP(ctx context.Context, rawURL, user, password string) (int, error) {
	return transmissionHTTPProbe(m, ctx, rawURL, user, password)
}

// transmissionTimeout 是"等它起来"的上限（单测把它压到毫秒级）。
func (m *Manager) transmissionTimeout() time.Duration {
	return transmissionWaitOverride
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

	// ---- 4. **先停服务**再改 settings.json ----
	//
	// 真机实测（2026-09-20）：transmission-daemon 关闭时会把**内存里的配置**写回
	// settings.json。运行中改写它再 `brew services restart`，旧进程退出时会把我们
	// 写进去的 rpc-username / rpc-authentication-required / rpc-bind-address 全冲掉
	// （只剩 rpc-password 的哈希），结果是"看起来设了口令、其实 RPC 无口令"。
	// 所以顺序必须是：停 → 等端口释放 → 改配置 → 启动。
	if err := m.stopTransmissionService(ctx); err != nil {
		if m.portHasListener(transmissionPort) {
			return fmt.Errorf("停不掉 transmission（端口 %d 仍在监听，改配置会被它回写覆盖）：%w；日志：%s",
				transmissionPort, err, m.transmissionLogPath())
		}
		res.step(ctx, "服务当前未在运行（无需停止）")
	} else {
		res.step(ctx, "已停止 "+transmissionFormula+"（改配置前必须先停，否则会被它回写覆盖）")
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
	merged, changed, err := transmissionSettingsWithCredentials(raw, user, password)
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

	// ---- 5. 启动让它读新配置（明文口令在这一步被哈希） ----
	res.step(ctx, "启动 "+transmissionFormula+" 让新配置生效")
	if err := m.startTransmissionService(ctx); err != nil {
		return fmt.Errorf("启动 %s 失败: %w；日志：%s",
			transmissionFormula, err, m.transmissionLogPath())
	}

	// ---- 6. 回读 settings.json，确认凭据真的生效 ----
	res.step(ctx, "回读 "+settings+" 确认 RPC 凭据真的生效")
	if ok, why := m.verifyTransmissionCredentialsApplied(settings, user, password); !ok {
		return fmt.Errorf("Transmission 的 RPC 凭据没有生效：%s。"+
			"面板**不会**把这次安装报成成功 —— 没有口令的 9091 等于谁都能改下载目录、删文件。"+
			"日志：%s", why, m.transmissionLogPath())
	}
	res.step(ctx, "回读确认：rpc-authentication-required=true、rpc-username 与凭据一致、"+
		"rpc-password 已是哈希（明文不存在）")

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

	// ---- 9. 装成系统级服务（开机自启；无头机器不加载用户级 agent，坑 130） ----
	if app, found := FindApp("transmission"); found && systemDaemonNeeded(app) {
		if _, _, err := systemDaemonEnsureFn(m, ctx, app, res); err != nil {
			// 与 Miniflux/Syncthing 同一取舍：不写警告就降级，等于让"重启后不自起"
			// 悄悄发生；但也不推翻已经验收过的服务。
			res.Warning = appendWarning(res.Warning,
				"Transmission 没能装成系统级服务（重启后不会自动起来）："+err.Error())
			res.step(ctx, "警告：Transmission 仍以用户级服务运行（重启后需手动启动）")
		} else {
			// 搬迁会重启服务：健康检查必须**重测**，不能沿用搬迁前的结论。
			if !m.waitTransmissionStatus(ctx, webURL, user, password,
				func(c int) bool { return c == http.StatusOK }) {
				return fmt.Errorf("装成系统级服务后 Transmission 没有恢复（%s 不可访问）；日志：%s",
					webURL, m.transmissionLogPath())
			}
			res.step(ctx, "系统级服务复核通过：重启机器后 Transmission 会自动起来")
		}
	}

	// ---- 10. 登记进服务管理 ----
	label, _, _ := m.brewServiceInfo(ctx, transmissionFormula)
	if label == "" {
		label = transmissionLabel
	}
	if err := m.RegisterInstalledService(ctx, label, "Transmission", "🧲", "tool", transmissionPort); err != nil {
		res.step(ctx, "（自动登记到面板失败："+err.Error()+"，可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
	}

	// 只绑回环：广告 LAN 地址等于给一个打不开的链接（与 BindAddress 的约定一致）。
	res.Address = "http://127.0.0.1:" + strconv.Itoa(transmissionPort) + "/transmission/web/"
	res.Message = "「Transmission（下载）」已安装并纳入管理"
	res.step(ctx,
		"打开 "+res.Address+"，用凭据区里的用户名与口令登录",
		"默认下载目录是 ~/Downloads；可在 Web UI 的「设置」里改。")
	return nil
}

// transmissionStopService / transmissionStartService 是"生命周期动作"的注入点（包级变量，理由同上面的 HTTP 探测）：单测不许跑真实 brew，
// 而 live 验证要能模拟"服务启动时把明文口令换成哈希"这个关键副作用。
var (
	transmissionStopService = func(m *Manager, ctx context.Context) error {
		return m.StopBrewService(ctx, transmissionFormula)
	}
	transmissionStartService = func(m *Manager, ctx context.Context) error {
		return m.StartBrewService(ctx, transmissionFormula)
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
	}
	return nil
}

// verifyTransmissionCredentialsApplied 回读 settings.json，确认服务真的读到了面板写的配置：
// rpc-authentication-required=true、rpc-username 与凭据一致、rpc-password 已变成哈希
// （`{<salt><hash>`）、以及三项"会触发 macOS 本地网络授权"的开关都是 false。
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
	for _, key := range []string{"dht-enabled", "lpd-enabled", "port-forwarding-enabled"} {
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

// ---------- 卸载 ----------

// uninstallTransmission 卸载 Transmission：停服务 + 删 plist + 删面板记录 + brew uninstall。
//
//   - 配置目录（RPC 凭据哈希、daemon 日志）**默认保留**，勾选 removeData 才删；
//   - **下载下来的文件永远不删**（它们在用户设置的下载目录里，面板不知道也不该猜）。
func (m *Manager) uninstallTransmission(ctx context.Context, removeData bool, result *InstallResult) error {
	label, plist, _ := m.brewServiceInfo(ctx, transmissionFormula)
	if label == "" {
		label = transmissionLabel
	}
	if plist == "" && m.opt.UserHome != "" {
		plist = filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
	}
	if result != nil {
		result.step(ctx, "停止并删除 launchd 服务 "+label)
	}
	if err := priv.LaunchUnload(label); err != nil {
		return fmt.Errorf("停止 %s 失败: %w", label, err)
	}
	if plist != "" {
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败: %w", plist, err)
		}
	}
	if _, err := m.ForgetByLabel(ctx, label); err != nil {
		return fmt.Errorf("删除面板服务记录失败: %w", err)
	}
	// 系统级 plist（装成 LaunchDaemon 时的那一份）也要摘掉，否则重启后
	// KeepAlive 会把它拉回来 —— 表现是"卸了但还在跑"。
	if sys := SystemDaemonPlistPath(label); fileExists(sys) {
		if err := priv.LaunchUnload(label); err != nil && result != nil {
			result.step(ctx, "（系统域服务停止失败，继续删 plist："+err.Error()+"）")
		}
		if err := os.Remove(sys); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除系统级 plist %s 失败: %w", sys, err)
		}
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
		result.step(ctx, "已下载的文件一个都没有删（默认下载目录 ~/Downloads 或你自己设置的目录）")
	}
	return nil
}
