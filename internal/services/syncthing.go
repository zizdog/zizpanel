package services

// ============================================================================
//  Syncthing（文件同步）安装器
//
//  为什么不能走通用 brew 流程：syncthing 的默认配置把网页界面绑在
//  **回环地址**（127.0.0.1:8384），而面板是给"远程网页操作"用的 ——
//  局域网里的用户根本打不开它。装完不修这一条，用户看到的就是
//  "装好了但点进去连不上"（与 Uptime Kuma 的"页面标题对、正文 404"同类）。
//
//  所以这里做四件事（每步都幂等、失败绝不谎报成功）：
//    1. brew install syncthing（30 分钟）+ brew services start（3 分钟）；
//    2. 等首次启动真的写出 config.xml 且 /rest/noauth/health 返回 200
//       （≤60 秒，1 秒轮询；超时带 syncthing.log 尾部与"踢一下"的做法）；
//    3. 幂等地把 <address> 改成 0.0.0.0:8384、打开 sendBasicAuthPrompt，
//       并设置用户名 + **bcrypt 哈希**口令（Syncthing v2 不接受明文口令：
//       实测 config.xml 里写明文，POST /rest/noauth/auth/password 返回 401）；
//    4. 用**新凭据实打一次** POST /rest/noauth/auth/password → 204 才算成功
//       （≤60 秒）；不是 204 就返回真错误，绝不写"已就绪"。
//
//  三条硬约束：
//    · **绝不在没有凭据的情况下把界面开到局域网**（纯函数 patchSyncthingGUI
//      会 refuse：口令哈希为空 / 不是 bcrypt / 找不到 <address> 都返回 error）；
//    · 口令**只**进 InstallResult.Credentials（一次性凭据区块）。任务步骤、
//      实时日志、审计里都不许出现 —— 它们会被长期保存与转发；
//    · 单测不许碰真实服务 / 真实 brew / 真实 Syncthing / 真实家目录。
//      副作用出口与 ready.go 同一约定：包级变量在生产路径上是默认实现，
//      单测用 t.Cleanup 覆盖（本轮不改 services.go 的 Manager 字段，
//      避免与并发编辑同一文件的改动互相破坏，见 DEVELOPMENT.md #104）。
// ============================================================================

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	syncthingFormula     = "syncthing"
	syncthingDisplayName = "Syncthing（文件同步）"
	syncthingIcon        = "🔄"
	// syncthingLabel 是 brew services 的约定 label，仅在 brewServiceInfo
	// 读不到真实 label 时兜底。
	syncthingLabel = "homebrew.mxcl.syncthing"
	syncthingPort  = 8384
	// syncthingAPIBase 是 Syncthing 的 Web GUI 基址（面板与它同机，走回环）。
	syncthingAPIBase = "http://127.0.0.1:8384"
	// syncthingLANAddress 是"局域网可达"的监听地址。
	//
	// 只改这一个值就够了：Syncthing 会重读手写的 config.xml（实测重启后
	// 真的绑定 *:8384）。0.0.0.0 比具体网卡地址稳 —— 换网络/换 IP 不用重装。
	syncthingLANAddress = "0.0.0.0:8384"

	syncthingGUIUser = "admin"
	// syncthingPasswordLen 是面板随机口令的长度（[A-Za-z0-9]）。
	syncthingPasswordLen = 20
)

// syncthingSecretAlphabet 只用大小写字母与数字。
//
// config.xml 是 XML，口令要经过 bcrypt 后写进 <password>：口令本身不落盘，
// 但保持"无引号 / 无 XML 元字符"就等于少掉一整类转义问题。
const syncthingSecretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ---------------------------------------------------------------------------
//  仅供测试注入的副作用出口
//
//  与 ready.go 的 readyWaitPort / readyWaitJSONBool 同一约定：默认值是生产
//  实现；单测（不并行）用 t.Cleanup 覆盖回去。这样单测既不联网、不碰真实
//  Syncthing，也不会因为"开发机上 8384 恰好有东西在听"给出飘忽的结论。
// ---------------------------------------------------------------------------

var (
	// syncthingHealthProbe 打一次 GET /rest/noauth/health（无需认证）。
	syncthingHealthProbe = defaultSyncthingHealthProbe
	// syncthingAuthProbe 打一次 POST /rest/noauth/auth/password（用新凭据自检）。
	syncthingAuthProbe = defaultSyncthingAuthProbe

	// syncthingReadyTimeout / syncthingAuthTimeout / syncthingPollInterval
	// 是两段轮询的时长与间隔。调成变量是为了让"超时=如实失败"能在毫秒级被
	// 验证，而不是让测试真等 60 秒。
	syncthingReadyTimeout = 60 * time.Second
	syncthingAuthTimeout  = 60 * time.Second
	syncthingPollInterval = time.Second
)

// defaultSyncthingHealthProbe 是生产实现：GET /rest/noauth/health。
//
// 为什么用 noauth/health 而不是 /rest/system/version：后者要 API key 或
// CSRF token，探活阶段手里还没有凭据（实测不带认证打 /rest/system/version
// 会 403，而 /rest/noauth/health 返回 200 {"status":"OK"}）。
func defaultSyncthingHealthProbe(ctx context.Context) (int, error) {
	return syncthingHTTPDo(ctx, http.MethodGet, syncthingAPIBase+"/rest/noauth/health", nil)
}

// defaultSyncthingAuthProbe 是生产实现：POST /rest/noauth/auth/password。
//
// 这是**验收探针**：凭据正确返回 204，错误返回 403（实测）。刻意不用
// basic auth 打 /rest/*（那会因为 CSRF 返回 403，分不清"口令错"与"少了 CSRF"）。
//
// 口令只在请求体里（内存），不进 argv、不进步骤文本 —— 与 miniflux 的
// "带口令的 SQL 走 stdin"同一个理由。
func defaultSyncthingAuthProbe(ctx context.Context, user, password string) (int, error) {
	body, err := json.Marshal(map[string]string{"username": user, "password": password})
	if err != nil {
		return 0, err
	}
	return syncthingHTTPDo(ctx, http.MethodPost, syncthingAPIBase+"/rest/noauth/auth/password", body)
}

// syncthingHTTPDo 发一次请求并返回状态码（只读 64KB 后丢弃 body）。
//
// Proxy 显式置空：保证 127.0.0.1 一定直连，不受用户 shell 里 HTTP(S)_PROXY 影响。
func syncthingHTTPDo(ctx context.Context, method, url string, body []byte) (int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
//  路径（一律从真实用户家目录推导，单测才能落在临时目录里）
// ---------------------------------------------------------------------------

// syncthingDataDir 是 Syncthing 的配置与数据目录。
//
// brew services 不带 --home 启动时，Syncthing 自己用
// ~/Library/Application Support/Syncthing/（实测）。卸载计划也复用它，
// 保证"安装器写哪、卸载计划列哪"是同一份定义。
func (m *Manager) syncthingDataDir() string {
	return filepath.Join(m.opt.UserHome, "Library", "Application Support", "Syncthing")
}

func (m *Manager) syncthingConfigPath() string {
	return filepath.Join(m.syncthingDataDir(), "config.xml")
}

// syncthingOwnLogPath 是 Syncthing 自己写的日志（在数据目录里）。
func (m *Manager) syncthingOwnLogPath() string {
	return filepath.Join(m.syncthingDataDir(), "syncthing.log")
}

// syncthingBrewLogPath 是 brew 服务的 stdout/stderr 日志。
func (m *Manager) syncthingBrewLogPath() string {
	return filepath.Join(m.brewPrefix(), "var", "log", syncthingFormula+".log")
}

// syncthingBinaryHint 给用户一条可手工执行的命令（"踢一下"用）。
func (m *Manager) syncthingBinaryHint() string {
	return filepath.Join(m.brewPrefix(), "opt", syncthingFormula, "bin", syncthingFormula)
}

// syncthingLogTail 取日志尾部（优先 Syncthing 自己的日志，回落到 brew 日志）。
// 返回 (路径, 尾部文本)；尾部为空表示两条日志都没有可用内容。
func (m *Manager) syncthingLogTail() (string, string) {
	cands := []string{m.syncthingOwnLogPath(), m.syncthingBrewLogPath()}
	for _, p := range cands {
		if tail, err := tailFile(p, 30); err == nil {
			if t := strings.TrimSpace(tail); t != "" {
				return p, t
			}
		}
	}
	return cands[0], ""
}

// syncthingLogTailNote 把日志尾部拼成一句可直接放进错误信息的话。
//
// "没有日志"本身就是信息（AGENTS.md 第三节）：失败可能发生在日志初始化之前，
// 所以日志为空时要明说，而不是把路径一贴了事。
func (m *Manager) syncthingLogTailNote(secrets ...string) string {
	path, tail := m.syncthingLogTail()
	if tail == "" {
		return "；日志 " + path + " 为空或不存在（失败可能发生在日志初始化之前）"
	}
	return "；日志 " + path + " 末尾：" + syncthingRedact(tail, secrets...)
}

// syncthingRedact 把已知口令从**将要写进任务日志 / 错误信息**的文本里抹掉。
//
// 日志内容不受我们控制（Syncthing 可能在错误里回显它读到的配置片段），
// 而这些文本会进任务中心、SSE 与审计。口令短于 4 位不替换（只会误伤正常文本）。
func syncthingRedact(text string, secrets ...string) string {
	for _, s := range secrets {
		if len(s) >= 4 {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// ---------------------------------------------------------------------------
//  纯函数：口令生成 / config.xml 补丁
// ---------------------------------------------------------------------------

// generateSyncthingSecret 生成 n 位 [A-Za-z0-9] 随机串。
//
// 用拒绝采样（丢弃 >= 248 的字节）而不是直接取模：62 不整除 256，
// 取模会让前 8 个字符出现概率略高。密码学上无所谓，但"我知道它有点偏"
// 的代码不该出现在凭据生成里（与 miniflux 同一实现口径，刻意各写一份：
// 安装器之间的凭据生成不互相依赖）。
func generateSyncthingSecret(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("随机串长度必须为正数，实际 %d", n)
	}
	const limit = 256 - (256 % len(syncthingSecretAlphabet)) // 248
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, syncthingSecretAlphabet[int(b)%len(syncthingSecretAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

var (
	syncthingGUIBlockRe = regexp.MustCompile(`(?s)<gui\b[^>]*>.*?</gui>`)
	syncthingOpenGUIRe  = regexp.MustCompile(`\A<gui\b[^>]*>`)
	syncthingAddressRe  = regexp.MustCompile(`<address\s*>([^<]*)</address\s*>`)
	syncthingUserRe     = regexp.MustCompile(`<user\s*>([^<]*)</user\s*>`)
	syncthingPasswordRe = regexp.MustCompile(`<password\s*>([^<]*)</password\s*>`)
	// sendBasicAuthPrompt：Syncthing 默认 false，浏览器因此不会弹登录框。
	syncthingBasicAuthPromptRe = regexp.MustCompile(`(?i)\ssendBasicAuthPrompt\s*=\s*"[^"]*"`)
	// Syncthing v2 只接受 bcrypt（上游 lib/config/guiconfiguration.go 的
	// 正则 ^\$2[aby]\$\d+\$.{50,}，用 bcrypt.CompareHashAndPassword 校验）。
	// 明文口令写进 config.xml 的后果：界面暴露了，但**谁都登不上**（实测 401）。
	syncthingBcryptRe = regexp.MustCompile(`^\$2[aby]\$\d+\$.{50,}$`)
)

// syncthingElementText 取一个简单元素（无嵌套标签）的文本内容（TrimSpace）。
func syncthingElementText(xml string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(xml)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// syncthingAddressIsLoopback 判断一个监听地址是否只在回环上。
//
// 关键边界（都按"不确定就当作对外"处理）：
//   - "" → false（空地址确认不了，不许当成安全）；
//   - ":8384" → false（SplitHostPort 得到空 host，含义是**监听所有网卡**）；
//   - "localhost" / "127.0.0.1" / "[::1]" → true；
//   - 其它主机名 → false（解析不到就当作可能对外）。
func syncthingAddressIsLoopback(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false // ":8384" 绑定所有网卡
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// syncthingGUIState 读出现有 <gui> 段落里的用户名与"是否已对局域网开放"。
//
// 只在 <gui>…</gui> 内部查找：Syncthing 的配置里 <ldap> 也有一个 <address>，
// 拿整个 xml 去找会张冠李戴。
func syncthingGUIState(xml string) (user string, exposed bool) {
	loc := syncthingGUIBlockRe.FindStringIndex(xml)
	if loc == nil {
		return "", false
	}
	block := xml[loc[0]:loc[1]]
	addr := syncthingElementText(block, syncthingAddressRe)
	return syncthingElementText(block, syncthingUserRe), addr != "" && !syncthingAddressIsLoopback(addr)
}

// patchSyncthingGUI 是 config.xml 的补丁**纯函数**（不碰磁盘，可单测）。
//
// 语义（幂等）：
//   - 已有非空 <user> 且 <address> 不是回环 → 什么都不做，返回 (原 xml, false, nil)。
//     这是"用户自己配置过界面凭据"的情形：面板**不重置**它，也不上报它。
//   - 否则把 <address> 设成 0.0.0.0:8384、给 <gui> 打开 sendBasicAuthPrompt、
//     把 <user>/<password> 设成给定值，返回 (新 xml, true, nil)。
//
// 安全拒绝（返回 error，返回值是**原 xml**，即使被误写也不会暴露）：
//   - passwordHash 不是 Syncthing v2 接受的 bcrypt 形态（含空串）；
//   - 找不到 <gui> 段落或其中的 <address>（无法确认/设置监听地址）。
//
// 为什么拒绝而不是"只改地址、凭据留空"：那等于把一个**无需认证的远程控制
// 界面**（能改同步目录、能执行命令）暴露到整个局域网 —— 比装不上严重得多。
//
// 实现刻意用**定点替换**而不是 XML 反序列化再序列化：后者会重排/重写注释与
// 缩进，等于把用户配置文件整体重写一遍（"preserving every other element
// byte-for-byte"）。所有替换走 ReplaceAllLiteralString —— bcrypt 哈希里含 `$`，
// 用带解释的替换会把 `$2a$10$…` 当成捕获组引用而写出乱码。
func patchSyncthingGUI(xml, passwordHash string) (string, bool, error) {
	loc := syncthingGUIBlockRe.FindStringIndex(xml)
	if loc == nil {
		return xml, false, fmt.Errorf("配置里找不到 <gui>…</gui> 段落，拒绝改动：" +
			"无法在写入凭据的前提下调整界面监听地址（否则会把无需认证的远程控制界面暴露到局域网）")
	}
	block := xml[loc[0]:loc[1]]

	// 已经配置过（用户名 + 对外监听）→ 原样保留，不重置凭据。
	if user, exposed := syncthingGUIState(block); user != "" && exposed {
		return xml, false, nil
	}

	if !syncthingBcryptRe.MatchString(passwordHash) {
		return xml, false, fmt.Errorf("拒绝改动：界面口令必须是 Syncthing v2 接受的 bcrypt 哈希" +
			"（形如 $2a$10$…，≥60 字符）。明文口令在 Syncthing v2 里不生效（实测 401），" +
			"那会让界面暴露却谁都登不上")
	}

	open := syncthingOpenGUIRe.FindString(block)
	if open == "" {
		return xml, false, fmt.Errorf("拒绝改动：<gui> 开始标签无法解析")
	}
	if !syncthingAddressRe.MatchString(block) {
		return xml, false, fmt.Errorf("拒绝改动：<gui> 段落里找不到 <address> 元素，" +
			"无法确认界面会监听在哪里")
	}

	newBlock := syncthingAddressRe.ReplaceAllLiteralString(block,
		"<address>"+syncthingLANAddress+"</address>")
	// 只替换开始标签那一段（block 一定以 open 开头）。
	newBlock = syncthingSetBasicAuthPrompt(open) + newBlock[len(open):]

	newBlock, hasUser := syncthingSetElement(newBlock, syncthingUserRe, "user", syncthingGUIUser)
	newBlock, hasPass := syncthingSetElement(newBlock, syncthingPasswordRe, "password", passwordHash)
	if !hasUser || !hasPass {
		// 元素不存在（少见）→ 插到 </gui> 之前，保持其它元素一字不动。
		var ins strings.Builder
		if !hasUser {
			ins.WriteString("    <user>" + syncthingGUIUser + "</user>\n")
		}
		if !hasPass {
			ins.WriteString("    <password>" + passwordHash + "</password>\n")
		}
		i := strings.LastIndex(newBlock, "</gui>")
		if i < 0 {
			return xml, false, fmt.Errorf("拒绝改动：找不到 </gui>，无法插入凭据元素")
		}
		newBlock = newBlock[:i] + ins.String() + newBlock[i:]
	}

	// 结果自检：三个条件都成立才允许落盘（拒绝"改了地址却没带上凭据"）。
	user, exposed := syncthingGUIState(newBlock)
	if user == "" || !exposed {
		return xml, false, fmt.Errorf("拒绝改动：补丁后仍无法确认「对外监听 + 已设用户名」同时成立")
	}
	if !syncthingPasswordRe.MatchString(newBlock) ||
		!strings.Contains(newBlock, passwordHash) {
		return xml, false, fmt.Errorf("拒绝改动：补丁后 <password> 不是给定的 bcrypt 哈希")
	}

	return xml[:loc[0]] + newBlock + xml[loc[1]:], true, nil
}

// syncthingSetElement 定点替换一个简单元素的文本内容；元素不存在返回 false。
func syncthingSetElement(xml string, re *regexp.Regexp, tag, value string) (string, bool) {
	if !re.MatchString(xml) {
		return xml, false
	}
	return re.ReplaceAllLiteralString(xml, "<"+tag+">"+value+"</"+tag+">"), true
}

// syncthingSetBasicAuthPrompt 保证 <gui> 开始标签上有 sendBasicAuthPrompt="true"。
//
// 默认配置里它是 "false" —— 不打开的话浏览器不会弹登录框，用户会看到
// 一个"能打开但进不去、也没有登录提示"的页面。
func syncthingSetBasicAuthPrompt(openTag string) string {
	if syncthingBasicAuthPromptRe.MatchString(openTag) {
		return syncthingBasicAuthPromptRe.ReplaceAllLiteralString(openTag, ` sendBasicAuthPrompt="true"`)
	}
	i := strings.LastIndex(openTag, ">")
	if i < 0 {
		return openTag
	}
	return openTag[:i] + ` sendBasicAuthPrompt="true"` + openTag[i:]
}

// writeSyncthingConfigAtomic 原子写 config.xml：同目录临时文件 + rename。
//
// 权限 0600、归属真实用户：
//   - 文件里有 GUI 口令哈希，权限放宽等于泄露凭据；
//   - 属主是 root 的话，以真实用户身份运行的 Syncthing 读不到配置，
//     服务会静默起不来（与 miniflux / ACME 证书同一类坑）。
//
// 先写临时文件再 rename：写一半被打断也不会留下一个半截的 config.xml
// （Syncthing 会把它当坏配置，那比安装失败更难查）。
func writeSyncthingConfigAtomic(userName, path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config.xml.*")
	if err != nil {
		return fmt.Errorf("在 %s 下创建临时配置失败: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("设置临时配置权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时配置失败: %w", err)
	}
	if userName != "" {
		if err := chownTo(userName, tmpName); err != nil {
			cleanup()
			return fmt.Errorf("把临时配置归属改为 %s 失败: %w", userName, err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("用新配置替换 %s 失败: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
//  轮询
// ---------------------------------------------------------------------------

// waitSyncthingReady 轮询"config.xml 已存在 且 /rest/noauth/health 返回 200"。
func (m *Manager) waitSyncthingReady(ctx context.Context, cfgPath string) bool {
	deadline := time.Now().Add(syncthingReadyTimeout)
	for {
		if fileExists(cfgPath) {
			if code, err := syncthingHealthProbe(ctx); err == nil && code == http.StatusOK {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(syncthingPollInterval):
		}
	}
}

// waitSyncthingAuth 轮询"新凭据能通过 POST /rest/noauth/auth/password（204）"。
//
// 204 是唯一算数的事件：配置改了并不等于 Syncthing 已经重载并接受了新凭据。
func (m *Manager) waitSyncthingAuth(ctx context.Context, user, password string) bool {
	deadline := time.Now().Add(syncthingAuthTimeout)
	for {
		if code, err := syncthingAuthProbe(ctx, user, password); err == nil && code == http.StatusNoContent {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(syncthingPollInterval):
		}
	}
}

// ---------------------------------------------------------------------------
//  安装
// ---------------------------------------------------------------------------

// InstallSyncthing 安装 Syncthing 并把它的 Web GUI 安全地开放到局域网。
//
// 幂等：包已装就跳过 brew install；brew services start 本身可重复；配置已经
// "用户名 + 非回环地址"时**不重置**凭据（无凭据区块、无步骤噪音）。
// 任何一步失败都返回真错误（含日志尾部），不写"已安装"。
func (m *Manager) InstallSyncthing(ctx context.Context, res *InstallResult) error {
	if res == nil {
		res = &InstallResult{App: syncthingFormula}
	}
	if res.App == "" {
		res.App = syncthingFormula
	}
	if res.Name == "" {
		res.Name = syncthingDisplayName
	}

	// ---- 1. Homebrew 是硬前提 ----
	if strings.TrimSpace(m.opt.BrewBin) == "" {
		return fmt.Errorf("未配置 Homebrew 路径，无法自动安装 Syncthing。请先安装 Homebrew")
	}
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		return fmt.Errorf("未安装 Homebrew（%s 不存在），无法自动安装 Syncthing。请先安装 Homebrew", m.opt.BrewBin)
	}
	// 配置目录要从真实用户家目录推导；不知道就如实失败，绝不写到相对路径上。
	if strings.TrimSpace(m.opt.UserHome) == "" {
		return fmt.Errorf("面板不知道真实用户的家目录（UserHome 为空），无法定位 Syncthing 的配置目录")
	}

	// ---- 2. 本体（30 分钟：瓶 + 可能的依赖） ----
	if !m.brewHas(ctx, syncthingFormula) {
		res.step(ctx, "正在 brew install "+syncthingFormula+"（首次可能需要几分钟）")
		if _, err := m.brewRun(ctx, 30*time.Minute, "install", syncthingFormula); err != nil {
			return err
		}
		res.step(ctx, syncthingFormula+" 已安装")
	} else {
		res.step(ctx, syncthingFormula+" 已安装（跳过 brew install）")
	}

	// ---- 3. 交给 brew services 托管并启动（3 分钟） ----
	res.step(ctx, "注册为后台服务并启动（brew services start "+syncthingFormula+"）")
	if _, err := m.brewRun(ctx, 3*time.Minute, "services", "start", syncthingFormula); err != nil {
		return fmt.Errorf("brew services start %s 失败: %w%s",
			syncthingFormula, err, m.syncthingLogTailNote())
	}

	// ---- 4. 等首次启动真的写出配置并健康 ----
	cfgPath := m.syncthingConfigPath()
	res.step(ctx, "等待 Syncthing 就绪（"+cfgPath+" 出现，且 "+syncthingAPIBase+
		"/rest/noauth/health 返回 200，最多 "+syncthingReadyTimeout.String()+"）")
	if !m.waitSyncthingReady(ctx, cfgPath) {
		return fmt.Errorf("Syncthing 在 %s 内没有就绪（GET %s/rest/noauth/health 未返回 200）%s。"+
			"配置文件 %s %s；如果它不存在，说明进程可能根本没起来（Syncthing 首次运行需要先"+
			"生成一次配置）。可在终端执行 %s --no-browser --no-restart 生成配置后 Ctrl-C 退出，"+
			"再执行 brew services restart %s",
			syncthingReadyTimeout, syncthingAPIBase, m.syncthingLogTailNote(),
			cfgPath, syncthingPathState(cfgPath), m.syncthingBinaryHint(), syncthingFormula)
	}
	res.step(ctx, "Syncthing 已就绪：GET "+syncthingAPIBase+"/rest/noauth/health 返回 200")

	// ---- 5. 配置：默认只绑回环，局域网打不开 —— 修掉这个缺陷 ----
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("读取 Syncthing 配置 %s 失败: %w", cfgPath, err)
	}
	cfg := string(raw)

	if user, exposed := syncthingGUIState(cfg); user != "" && exposed {
		res.step(ctx, "现有配置的界面已经开放到局域网且设置了用户名 "+user+
			"：面板保留这份凭据、不重置它（也不会上报它）")
	} else {
		password, err := generateSyncthingSecret(syncthingPasswordLen)
		if err != nil {
			return fmt.Errorf("生成 Syncthing 界面口令失败: %w", err)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("生成 Syncthing 界面口令哈希失败: %w", err)
		}
		patched, changed, err := patchSyncthingGUI(cfg, string(hash))
		if err != nil {
			return fmt.Errorf("拒绝改动 %s: %w", cfgPath, err)
		}
		if !changed {
			// 理论上不会走到（上面刚查过），但保留这条以防两次判定之间状态变化。
			res.step(ctx, "现有配置的界面已经配置好凭据，面板不改动它")
		} else {
			if err := writeSyncthingConfigAtomic(m.opt.UserName, cfgPath, patched); err != nil {
				return err
			}
			res.step(ctx, "已把界面地址改为 "+syncthingLANAddress+
				" 并设置登录凭据（"+cfgPath+"，权限 0600，归属 "+m.opt.UserName+"）")
			res.step(ctx, "界面口令只在安装结果的一次性凭据区块里显示，不会写进任务日志")
			// 凭据区块**在自检之前**挂上：后面任何一步失败，口令的唯一记录
			// 也能到达用户（与 MySQL / Miniflux 凭据闭环同一约定）。
			res.Credentials = append(res.Credentials,
				Credential{Key: "syncthing_gui_user", Value: syncthingGUIUser,
					Label: "Syncthing 界面用户名"},
				Credential{Key: "syncthing_gui_password", Value: password,
					Label: "Syncthing 界面口令（面板随机生成，请自行保存）"})

			// ---- 6. 重启服务让新配置生效，再自检凭据 ----
			//
			// Syncthing v2 **不会热加载** GUI 的监听地址与凭据：只改 config.xml
			// （它还会自己写回文件）并不改变正在跑的那个进程的监听 socket。
			// 真机复现（2026-09-17 mini，全新安装）：文件已经写成
			// `<address>0.0.0.0:8384</address>` + bcrypt `<password>`，而进程日志
			// 仍是 `GUI and API listening (address=127.0.0.1:8384)`，自检端点
			// `/rest/noauth/auth/password` 返回 **404**（不是 403）→ 第一次安装
			// 必然在自检处失败（而"失败了要能重试"这件事本身也不该靠人肉）。
			//
			// 用 stop + start 而不是 `brew services restart`：面板跑 brew 的上下文
			// 里 restart 在有的机器上会落到不对的 launchd 域（真机实测普通 SSH 下
			// `Could not enable service: 125`），而 stop/start 这两条在本项目里
			// 已经到处在用、行为确定。stop 失败不算错（本来就没在跑）。
			res.step(ctx, "重启 Syncthing 让它加载新的监听地址与凭据")
			if _, err := m.brewRun(ctx, 2*time.Minute, "services", "stop", syncthingFormula); err != nil {
				res.step(ctx, "（停止旧实例时 brew 报错，继续启动：+"+err.Error()+"）")
			}
			if _, err := m.brewRun(ctx, 3*time.Minute, "services", "start", syncthingFormula); err != nil {
				return fmt.Errorf("配置已改好但重启 Syncthing 失败: %w%s", err, m.syncthingLogTailNote(password))
			}
			res.step(ctx, "等待 Syncthing 用新配置起来并自检新凭据（POST "+syncthingAPIBase+
				"/rest/noauth/auth/password 期望 204，最多 "+syncthingAuthTimeout.String()+"）")
			if !m.waitSyncthingAuth(ctx, syncthingGUIUser, password) {
				return fmt.Errorf("配置已改动并重启过，但 Syncthing 在 %s 内没有用新凭据通过校验"+
					"（POST %s/rest/noauth/auth/password 未返回 204）%s。"+
					"界面地址与凭据已经写进 %s，本次生成的口令仍在上方的一次性凭据区块里："+
					"可在终端执行 brew services stop %s && brew services start %s 后再试",
					syncthingAuthTimeout, syncthingAPIBase, m.syncthingLogTailNote(password),
					cfgPath, syncthingFormula, syncthingFormula)
			}
			res.step(ctx, "新凭据自检通过（POST "+syncthingAPIBase+
				"/rest/noauth/auth/password 返回 204）")
		}
	}

	// ---- 7. 登记进「服务管理」（真实 label 优先） ----
	label, _, _ := m.brewServiceInfo(ctx, syncthingFormula)
	if label == "" {
		label = syncthingLabel
	}
	if err := m.RegisterInstalledService(ctx, label, syncthingDisplayName, syncthingIcon, "tool", syncthingPort); err != nil {
		// 登记失败不让整个部署失败（与 Miniflux / Qwen3 TTS 同一取舍）：
		// 服务本身是好的，只是列表里暂时没有它，可手工纳管。
		res.step(ctx, "（自动登记到服务管理失败："+err.Error()+"，可在「可纳管」里手动加入）")
	}

	host := m.primaryIP()
	res.Address = "http://" + host + ":" + strconv.Itoa(syncthingPort)
	res.Message = "「" + syncthingDisplayName + "」已安装并纳入管理"
	res.step(ctx, "打开 http://"+host+":"+strconv.Itoa(syncthingPort)+
		"，用上方凭据区块里的用户名与口令登录（局域网内的设备也能打开）")
	return nil
}

// syncthingPathState 给错误信息用的一句"文件在不在"。
func syncthingPathState(path string) string {
	if fileExists(path) {
		return "已存在"
	}
	return "不存在"
}

// ---------------------------------------------------------------------------
//  卸载
// ---------------------------------------------------------------------------

// uninstallSyncthing 卸载 Syncthing：停服务 + 摘 launchd 定义 + 删面板记录 +
// brew uninstall；**默认保留**数据目录（设备身份、配对信息与同步索引）。
//
// 为什么保留是默认：~/Library/Application Support/Syncthing 里是本机的设备
// 密钥与同步索引，删掉等于重置设备身份 —— 所有对端都要重新配对，索引也要
// 重建。与 Miniflux 保留 PostgreSQL / IOPaint 保留 venv 是同一取舍：
// **面板绝不替用户删数据**，要彻底清理由调用方显式传 removeData=true。
//
// 注意**同步目录本身不在这个数据目录里**（用户配置在别处），所以卸载/删数据
// 都不影响已同步的文件 —— 这句话要如实写进步骤，免得用户以为文件也没了。
func (m *Manager) uninstallSyncthing(ctx context.Context, removeData bool, result *InstallResult) error {
	label, plist, _ := m.brewServiceInfo(ctx, syncthingFormula)
	if label == "" {
		label = syncthingLabel
	}
	if plist == "" && m.opt.UserHome != "" {
		plist = filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
	}

	// 先让 brew 自己停：brew services 起的是 gui/<uid> 域的用户代理，
	// `brew services stop` 会停掉进程并摘掉它自己写的 plist（比手工 bootout
	// system/<label> 可靠）。失败不致命 —— removeService 才是决定性的那一步。
	if strings.TrimSpace(m.opt.BrewBin) != "" && fileExists(m.opt.BrewBin) && m.brewHas(ctx, syncthingFormula) {
		if result != nil {
			result.step(ctx, "停止 Syncthing 服务（brew services stop "+syncthingFormula+"）")
		}
		if _, err := m.brewRun(ctx, 3*time.Minute, "services", "stop", syncthingFormula); err != nil && result != nil {
			result.step(ctx, "⚠️ brew services stop 失败，继续清理："+err.Error())
		}
	}
	// removeService：停 + 摘 launchd 定义 + 删面板记录（顺序不能反）。
	if err := m.removeService(ctx, label, plist); err != nil {
		return err
	}

	if m.brewHas(ctx, syncthingFormula) {
		if result != nil {
			result.step(ctx, "正在 brew uninstall "+syncthingFormula)
		}
		if _, err := m.brewRun(ctx, 5*time.Minute, "uninstall", syncthingFormula); err != nil {
			return fmt.Errorf("brew uninstall %s 失败: %w", syncthingFormula, err)
		}
	} else if result != nil {
		result.step(ctx, syncthingFormula+" 未安装（Homebrew 里没有它），跳过 brew uninstall")
	}

	dataDir := m.syncthingDataDir()
	if removeData {
		if !dirExists(dataDir) {
			if result != nil {
				result.step(ctx, dataDir+" 不存在，没有需要删除的数据")
			}
		} else if err := m.removeTree(ctx, dataDir, result); err != nil {
			return err
		}
	} else if result != nil {
		result.step(ctx, "保留 "+dataDir+"（设备密钥、配对信息与同步索引；需要彻底清理请勾选「删除数据」）")
	}
	if result != nil {
		result.step(ctx, "同步目录里的文件不受影响（Syncthing 的数据目录不含你的同步文件夹）")
	}
	return nil
}
