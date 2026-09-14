package services

import (
	"context"
	_ "embed"
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
//  TtsVoice 音色样本接收端（receiver.py）
//
//  契约来源：网站侧项目 zizdog.cn 的
//  usr/plugins/TtsVoice/HANDOFF-TO-MINI.md —— 那份文档是自包含的，
//  receiver.py 的完整代码就在里面。这里**逐字节照搬**，不做任何"优化"：
//
//    · 文档第 6 节明确写了"不要修改 receiver.py 的接口路径 / 参数名 /
//      响应字段"，网站插件按那份契约调用，改了会直接失联。
//    · 保持字节一致还有一个好处：网站侧可以用哈希核对这台机器上跑的是
//      不是同一份代码（版本漂移是这种跨项目对接最容易出的问题）。
//
//  它同时干两件事：
//    1. 接收网站上传的音色样本（落到本机，因为上游只认本地文件路径）
//    2. 作为**带鉴权的反向代理**对外提供 /v1/*，把请求转发给
//       127.0.0.1:8880 上那个没有鉴权的 mlx-audio
//
//  网络拓扑（这是这次改动最核心的一点）：
//    8880 只监听 127.0.0.1  ← 上游没鉴权，绝不能对外
//    8899 监听 0.0.0.0      ← 唯一对外入口，共享密钥鉴权
//
//  与文档的一处刻意不同：文档用 ~/Library/LaunchAgents（用户级 agent），
//  这里注册为**系统级 LaunchDaemon + UserName**。理由与 Qwen 服务相同：
//  用户级服务要有人登录才跑，而这是一台不接显示器、无人登录的服务器。
// ============================================================================

//go:embed voice-receiver.py
var voiceReceiverPy []byte

const (
	receiverLabel = "com.zizdog.voicereceiver"
	receiverPort  = 8899
	// receiverTokenPrefix 是自动生成密钥的前缀（与交接文档 5.2 步的格式一致）
	receiverTokenPrefix = "ttsv-"
	// 与 Qwen 一致：代理对外的端口，上游在 127.0.0.1:8880
	qwenUpstream = "http://127.0.0.1:8880"
)

// receiverPaths 是接收端的目录约定（与文档一致）。
type receiverPaths struct {
	Dir     string // ~/tts/voice-receiver
	Script  string // ~/tts/voice-receiver/receiver.py
	Samples string // ~/tts/voice-samples
	Jobs    string // ~/tts/jobs（v1.3.0 的作业队列落盘目录）
	OutLog  string
	ErrLog  string
	Plist   string // /Library/LaunchDaemons/com.zizdog.voicereceiver.plist
}

func (m *Manager) receiverPaths() receiverPaths {
	home := m.opt.UserHome
	if home == "" {
		home = "/Users/" + m.opt.UserName
	}
	dir := filepath.Join(home, "tts", "voice-receiver")
	return receiverPaths{
		Dir:     dir,
		Script:  filepath.Join(dir, "receiver.py"),
		Samples: filepath.Join(home, "tts", "voice-samples"),
		Jobs:    filepath.Join(home, "tts", "jobs"),
		OutLog:  filepath.Join(dir, "launchd.out.log"),
		ErrLog:  filepath.Join(dir, "launchd.err.log"),
		Plist:   "/Library/LaunchDaemons/" + receiverLabel + ".plist",
	}
}

// InstallVoiceReceiver 部署接收端，并返回共享密钥与对外地址。
//
// 密钥的处理：已存在就复用（换掉会让网站那边立刻失联），
// 不存在才生成。生成后既要写进 plist，也要回显给调用方 ——
// 网站插件要把同一个值填到 openaiKey 与 refUploadToken 两处。
// ReceiverOptions 是部署接收端时用户可做的选择。
type ReceiverOptions struct {
	// Token 是指定密钥；为空且 NoAuth=false 时自动生成
	Token string
	// NoAuth 为 true 时使用空密钥部署（= 不鉴权）。
	// Host 是监听地址。空 = 0.0.0.0（网站可能在别的机器上，这是历史默认值）。
	// 本机自己用（网站与接收端同一台）时应传 127.0.0.1 —— 交接文档明确这么要求：
	// "本机只给网站用，127.0.0.1 就够"，没有必要把接收端暴露到局域网。
	Host string

	// receiver.py 的语义：--token 为空时 hmac 比较的是空串，
	// 不带鉴权头的请求直接通过，且 /voice/health 会报 auth:false。
	// 这意味着**任何人都能往这台机器投放文件**，所以只能在内网自用。
	NoAuth bool
}

func (m *Manager) InstallVoiceReceiver(ctx context.Context, result *InstallResult, opt ReceiverOptions) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("部署接收端需要以 root 运行")
	}
	if m.opt.UserName == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户")
	}
	p := m.receiverPaths()

	// ---- 1. 目录与脚本 ----
	for _, d := range []string{p.Dir, p.Samples, p.Jobs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建 %s 失败: %w", d, err)
		}
	}
	if err := chownTree(m.opt.UserName, filepath.Join(p.Dir, "..")); err != nil {
		return fmt.Errorf("设置目录归属失败: %w", err)
	}

	// 脚本内容始终以面板内置的这份为准：它逐字节来自交接文档，
	// 若机器上是别的内容（手工改过 / 旧版本），这里覆盖并明确告知。
	if old, err := os.ReadFile(p.Script); err != nil || string(old) != string(voiceReceiverPy) {
		if err != nil {
			result.step(ctx, "已写入 receiver.py（来自交接文档）")
		} else {
			result.Steps = append(result.Steps,
				"检测到 receiver.py 与交接文档的版本不一致，已替换为文档版本")
		}
		if werr := os.WriteFile(p.Script, voiceReceiverPy, 0o755); werr != nil {
			return fmt.Errorf("写入 receiver.py 失败: %w", werr)
		}
	} else {
		result.step(ctx, "receiver.py 已是最新（与交接文档一致）")
	}
	_ = chownTree(m.opt.UserName, p.Dir)

	// ---- 2. 共享密钥 ----
	// 优先级：用户指定 > 已有（复用，换掉会让网站立刻失联）> 自动生成
	token := strings.TrimSpace(opt.Token)
	switch {
	case opt.NoAuth:
		token = ""
		result.Steps = append(result.Steps,
			"按选择**不启用鉴权**（密钥留空）——同内网任何人都能上传文件与调用合成")
	case token != "":
		result.step(ctx, "使用你指定的共享密钥")
	default:
		if existing := m.existingReceiverToken(p); existing != "" {
			token = existing
			result.step(ctx, "复用已有共享密钥（换掉会让网站那边失联）")
		} else {
			t, err := randomHex(16)
			if err != nil {
				return err
			}
			token = receiverTokenPrefix + t // 前缀与文档第 5.2 步生成的格式一致
			result.step(ctx, "已自动生成共享密钥")
		}
	}

	// ---- 3. 系统级服务 ----
	// 监听地址：只接受 127.0.0.1 / 0.0.0.0 两个值（其他值交给 Python 报错没有意义，
	// 而且这里要写进 root 拥有的 plist，宁可先校验）。
	bindHost := strings.TrimSpace(opt.Host)
	if bindHost == "" {
		bindHost = "0.0.0.0"
	}
	if bindHost != "0.0.0.0" && bindHost != "127.0.0.1" {
		return fmt.Errorf("监听地址只支持 0.0.0.0（对局域网开放）或 127.0.0.1（仅本机）")
	}
	if err := m.applyReceiverPlist(ctx, result, p, token, bindHost); err != nil {
		return err
	}

	// ---- 4. 验证：只认真的返回了 auth:true ----
	wantAuth := !opt.NoAuth
	if !waitJSONBoolValue(ctx, fmt.Sprintf("http://127.0.0.1:%d/voice/health", receiverPort),
		"auth", wantAuth, 20*time.Second) {
		result.Warning = fmt.Sprintf("接收端已注册，但 /voice/health 未返回 auth:true。请看日志：%s", p.ErrLog)
		result.step(ctx, "警告："+result.Warning)
		return nil
	}
	result.step(ctx, "接收端已就绪并启用鉴权")

	if err := m.RegisterInstalledService(ctx, receiverLabel, "TtsVoice 音色接收端", "🔐", "ai", receiverPort); err != nil {
		result.step(ctx, "（自动登记到服务管理失败："+err.Error()+"）")
	}

	// ---- 5. 把网站那边要的两样东西直接列出来 ----
	//
	// 插件从 2026-09-13 起会**自动推导**接收端地址与密钥：
	//   refUploadUrl   = openaiBaseUrl 去掉结尾的 /v1
	//   refUploadToken = openaiKey
	// 所以这里只给这两项，不再让用户填四个字段（多填一处就多一个填错的机会）。
	host := m.primaryIP()
	key := token
	if key == "" {
		key = "（留空 —— 未启用鉴权）"
	}
	result.Steps = append(result.Steps,
		"",
		"┌─────────────────────────────────────────────┐",
		"│  请记录下面两项，填进网站 TtsVoice 插件     │",
		"└─────────────────────────────────────────────┘",
		"  本机地址 = "+host,
		"  共享密钥 = "+(map[bool]string{true: "（见下方可复制区块）", false: "（未启用鉴权）"})[key != ""],
		"",
		"插件里这样填：",
		fmt.Sprintf("  openaiBaseUrl = http://%s:%d/v1", host, receiverPort),
		"  openaiKey     = 见下方可复制区块",
		"",
		"  接收端地址与密钥会自动从上面两项推导，不用另外填。",
		"  （密钥只放在结果里、不写进步骤文本：步骤会进操作审计，密钥不该留在那里。）",
	)
	result.Token = token
	result.Address = host
	return nil
}

// applyReceiverPlist 写 plist 并（重新）装载守护进程。
//
// 安装与"更改共享密钥"共用：两处都必须走同一条路，否则很容易出现
// "改了 plist 但没重载"这种看着成功、实际还在用旧密钥的情况。
func (m *Manager) applyReceiverPlist(ctx context.Context, result *InstallResult,
	p receiverPaths, token, host string) error {
	plist := receiverPlist(p, m.opt.UserName, token, host)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist 失败: %w", err)
	}
	if err := m.bootstrapService(ctx, receiverLabel, p.Plist); err != nil {
		return err
	}
	result.step(ctx, "已注册为系统级后台服务（开机自启、不依赖用户登录）")
	return nil
}

// existingReceiverHost 从已有 plist 里读回监听地址；读不到返回 ""。
//
// 改密钥时**必须**保留原监听地址：本机是 127.0.0.1（只给同机的网站用），
// mini 是 0.0.0.0（网站在别的机器上）。顺手改成默认值等于悄悄把它暴露到局域网。
func (m *Manager) existingReceiverHost(p receiverPaths) string {
	b, err := os.ReadFile(p.Plist)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "--host") && i+1 < len(lines) {
			v := strings.TrimSpace(lines[i+1])
			v = strings.TrimPrefix(v, "<string>")
			v = strings.TrimSuffix(v, "</string>")
			if v == "127.0.0.1" || v == "0.0.0.0" {
				return v
			}
		}
	}
	return ""
}

// ValidateReceiverToken 校验用户自定义的共享密钥。
//
// 为什么必须校验：这个值会被写进 **root 拥有的 plist XML**，
// 里面出现 & < > " 会让 plist 语法坏掉（launchd 直接拒绝装载，接收端起不来）。
// 所以只允许 URL/密钥里常见的安全字符，并且限制长度。
func ValidateReceiverToken(token string) error {
	if token == "" {
		return fmt.Errorf("密钥不能为空")
	}
	if len(token) < 8 || len(token) > 128 {
		return fmt.Errorf("密钥长度应在 8–128 个字符之间")
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("密钥只能包含字母、数字、连字符、下划线和点（不能有空格或 & < > 等字符）")
		}
	}
	return nil
}

// ChangeReceiverToken 只更换共享密钥：保留监听地址/脚本路径/上游等其余配置，
// 重载守护进程，并**用新密钥真打一次接口**确认它已经生效。
//
// 不重新写 receiver.py（那是安装的事），也不动样本目录 —— 换密钥不该有别的副作用。
func (m *Manager) ChangeReceiverToken(ctx context.Context, result *InstallResult, newToken string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("更改共享密钥需要以 root 运行")
	}
	if m.opt.UserName == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户")
	}
	p := m.receiverPaths()
	if _, err := os.Stat(p.Plist); err != nil {
		return fmt.Errorf("接收端还没部署过（找不到 %s），请先安装", p.Plist)
	}

	token := strings.TrimSpace(newToken)
	if token == "" {
		t, err := randomHex(16)
		if err != nil {
			return err
		}
		token = receiverTokenPrefix + t
		result.step(ctx, "已生成新的随机密钥")
	} else {
		if err := ValidateReceiverToken(token); err != nil {
			return err
		}
		result.step(ctx, "使用你指定的新密钥")
	}

	host := m.existingReceiverHost(p)
	if host == "" {
		host = "0.0.0.0"
		result.step(ctx, "（原 plist 里没读到监听地址，按 0.0.0.0 处理）")
	} else {
		result.step(ctx, "保持原监听地址 "+host+" 不变")
	}

	if err := m.applyReceiverPlist(ctx, result, p, token, host); err != nil {
		return err
	}

	// 用新密钥真的调一次：200 才算成功。只看 launchd 状态是不够的 ——
	// 服务起来了但还用着旧密钥，是这次操作最可能的失败形态。
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, err := httpGetStatus(ctx, fmt.Sprintf("http://127.0.0.1:%d/jobs", receiverPort), token)
		if err == nil && code == 200 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("接收端已重载，但用新密钥访问 /jobs 仍不通（最后状态：%d %v）。"+
				"服务可能没起来，请查看日志 %s", code, err, p.ErrLog)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	result.step(ctx, "新密钥已生效（/jobs 用新密钥返回 200）")

	// 旧密钥必须失效 —— 否则"换密钥"是假的。这一步是这次改动的安全语义。
	code, err := httpGetStatus(ctx, fmt.Sprintf("http://127.0.0.1:%d/jobs", receiverPort), "ttsv-definitely-not-the-key")
	if err == nil && code == 200 {
		return fmt.Errorf("新密钥生效了，但错误密钥也能访问 —— 鉴权没有真正开启，请检查 plist 的 --token")
	}
	result.step(ctx, "错误密钥仍被拒绝（403），鉴权正常")

	// 密钥只放在 result.Token 里（前端做成可复制区块），**不进步骤文本**：
	// 步骤会被写进操作审计，密钥不该留在审计日志里。
	result.Steps = append(result.Steps,
		"",
		"┌─────────────────────────────────────────────┐",
		"│  网站 TtsVoice 插件要同步改成新密钥         │",
		"└─────────────────────────────────────────────┘",
		"  需要改的是 openaiKey（接收端密钥由它自动推导）：",
		"    refUploadToken = openaiKey = 新密钥（见下方可复制区块）",
		"  改完之前，网站的上传音色与合成任务会 403。",
	)
	result.Token = token
	result.Address = m.primaryIP()
	return nil
}

// httpGetStatus 用指定密钥 GET 一个地址，只看状态码。
func httpGetStatus(ctx context.Context, url, token string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("X-TtsVoice-Token", token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// existingReceiverToken 从已有 plist 里读回密钥，读不到返回空串。
func (m *Manager) existingReceiverToken(p receiverPaths) string {
	b, err := os.ReadFile(p.Plist)
	if err != nil {
		return ""
	}
	// 取 --token 后面那个字符串
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "--token") && i+1 < len(lines) {
			v := strings.TrimSpace(lines[i+1])
			v = strings.TrimPrefix(v, "<string>")
			v = strings.TrimSuffix(v, "</string>")
			if strings.HasPrefix(v, "ttsv-") {
				return v
			}
		}
	}
	return ""
}

// receiverPlist 生成系统级 LaunchDaemon 定义。
func receiverPlist(p receiverPaths, user, token, host string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <!-- 以真实用户运行：音色样本要落到该用户的家目录下 -->
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/bin/python3</string>
        <string>%s</string>
        <string>--dir</string>
        <string>%s</string>
        <!-- 作业队列目录也显式给绝对路径：launchd 下 HOME 不一定是真实用户家目录，
             而 ~ 的展开依赖 HOME —— --dir 一直是显式传的，这里保持一致。
             v1.3.0 的 /jobs/* 会把状态与分块落在这里（重启后据此恢复）。 -->
        <string>--jobs-dir</string>
        <string>%s</string>
        <!-- 监听地址：0.0.0.0 = 对局域网开放（网站可能在别的机器上）；
             127.0.0.1 = 只有本机能连（网站与接收端同一台机器时的推荐值） -->
        <string>--host</string>
        <string>%s</string>
        <string>--port</string>
        <string>%d</string>
        <string>--token</string>
        <string>%s</string>
        <!-- 上游 Qwen 服务只监听 127.0.0.1，由本代理对外提供鉴权 -->
        <string>--upstream</string>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, receiverLabel, user, p.Script, p.Samples, p.Jobs, host, receiverPort, token, qwenUpstream,
		p.Dir, p.OutLog, p.ErrLog)
}

// waitJSONBool 轮询某个 URL，直到 JSON 响应里指定字段为 true。
//
// 为什么必须解析 JSON 而不是做子串匹配：
// 我第一版写成找子串 `"auth":true`，而 Python 的 json.dumps 默认带空格，
// 实际返回的是 `"auth": true` —— 于是**服务明明是好的，却被判成失败**，
// 还带着一句"请查看日志"的误导提示。判 JSON 字段就要按 JSON 解，
// 不能对格式化后的文本做精确匹配。
//
// 也不能只看 HTTP 200：接收端在鉴权没生效时同样返回 200（只是 auth:false）。
// 没鉴权的接收端等于一个**文件投放点**，必须确认字段为 true 才算过。
func waitJSONBoolValue(ctx context.Context, url, field string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if out, err := runCurlCtx(ctx, url, 4); err == nil {
			var m map[string]any
			if json.Unmarshal([]byte(out), &m) == nil {
				if v, ok := m[field].(bool); ok && v == want {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}
