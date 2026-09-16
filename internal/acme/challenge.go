package acme

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/challenge"
)

// 本文件是两种验证方式的「落地实现」。
//
// http-01：我们只负责把挑战文件写进网站根目录。面板生成的 nginx vhost 里已经有
// `location ^~ /.well-known/acme-challenge/ { root <站点根>; }`（见 internal/sites），
// 所以文件落盘后 CA 就能通过 80 端口取到，本进程不需要监听 80。
//
// dns-01：调用 lego 的 provider 注册表写 TXT 记录。provider 的凭据读取方式是
// lego 规定的环境变量（无参数构造函数），所以这里把 DNSProviderSpec.Env 注入进程
// 环境，用完立刻还原 —— 绝不自造一套传参协议。

// webrootProvider 把 keyAuth 写到 <root>/.well-known/acme-challenge/<token>。
type webrootProvider struct {
	root string
	emit func(format string, args ...any)
}

// Present 写入挑战文件。
func (p *webrootProvider) Present(domain, token, keyAuth string) error {
	if strings.TrimSpace(p.root) == "" {
		return errors.New("未配置 http-01 网站根目录")
	}
	// token 由 CA 生成，规范上只含 base64url 字符；仍然做白名单校验，
	// 否则一个被构造的 token（如 ../../x）就能让文件写到网站目录之外。
	if !isTokenSafe(token) {
		return fmt.Errorf("http-01 挑战 token 含非法字符（域名 %s）", domain)
	}

	dir := filepath.Join(p.root, ".well-known", "acme-challenge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 http-01 挑战目录失败（%s）: %w", dir, err)
	}

	path := filepath.Join(dir, token)
	if err := os.WriteFile(path, []byte(keyAuth), 0o644); err != nil {
		return fmt.Errorf("写入 http-01 挑战文件失败（域名 %s）: %w", domain, err)
	}
	// 只打印目录，不打印含 token 的文件名：token 虽然最终会被公开提供，
	// 但没必要提前写进会被持久化的任务日志。
	p.emit("http-01：挑战文件已写入 %s（域名 %s）", dir, domain)
	return nil
}

// CleanUp 删除挑战文件。lego 在验证结束（无论成功失败）后调用。
func (p *webrootProvider) CleanUp(domain, token, keyAuth string) error {
	if !isTokenSafe(token) {
		return nil
	}
	path := filepath.Join(p.root, ".well-known", "acme-challenge", token)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("清理 http-01 挑战文件失败（域名 %s）: %w", domain, err)
	}
	return nil
}

// isTokenSafe 只接受 base64url 字符集（ACME 规范对 token 的要求）。
func isTokenSafe(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// loggingDNSProvider 包住 lego 的 DNS provider，用于输出进度，
// 并把传播等待参数原样传递给 lego。
type loggingDNSProvider struct {
	inner    challenge.Provider
	emit     func(format string, args ...any)
	timeout  time.Duration
	interval time.Duration
}

// Present 写 TXT 记录。只打印域名，绝不打印 token / keyAuth（keyAuth 的摘要
// 就是 TXT 值，token 是挑战凭据）。
func (p *loggingDNSProvider) Present(domain, token, keyAuth string) error {
	p.emit("dns-01：正在为 %s 写入 TXT 记录（_acme-challenge）", domain)
	if err := p.inner.Present(domain, token, keyAuth); err != nil {
		return fmt.Errorf("dns-01 写入 DNS 记录失败（域名 %s）: %w", domain, err)
	}
	p.emit("dns-01：记录已提交，等待 DNS 解析生效（域名 %s，最多等 %s）", domain, p.timeout)
	return nil
}

// CleanUp 删除 TXT 记录。
func (p *loggingDNSProvider) CleanUp(domain, token, keyAuth string) error {
	if err := p.inner.CleanUp(domain, token, keyAuth); err != nil {
		return fmt.Errorf("dns-01 清理 DNS 记录失败（域名 %s）: %w", domain, err)
	}
	return nil
}

// Timeout 实现 challenge.ProviderTimeout，让 lego 用我们解析出的等待参数。
// 即使 inner 没有实现该接口，也返回 lego 的默认值，行为与不包装时一致。
func (p *loggingDNSProvider) Timeout() (time.Duration, time.Duration) {
	return p.timeout, p.interval
}

// savedEnvVar 记录一个被 applyDNSEnv 改动过的环境变量。
type savedEnvVar struct {
	key   string
	value string
	had   bool
}

// applyDNSEnv 把 spec.Env 注入进程环境，返回还原函数。
//
// 为什么用进程环境而不是给 provider 传参：lego 的 provider 注册表
// （dns.NewDNSChallengeProviderByName）只提供无参构造函数，provider 一律用
// env.GetOrFile 从环境变量读凭据。这是 lego 支持的唯一方式。
func applyDNSEnv(spec *DNSProviderSpec) (func(), error) {
	saved := make([]savedEnvVar, 0, len(spec.Env))

	for k, v := range spec.Env {
		if err := validateEnvKey(k); err != nil {
			// 已经设置过的部分要还原，避免半途失败污染环境。
			restoreEnv(saved)
			return func() {}, err
		}
		prev, had := os.LookupEnv(k)
		saved = append(saved, savedEnvVar{key: k, value: prev, had: had})
		// 登记脱敏，保证这些值即使被第三方库带进日志也会被替换掉。
		rememberSecret(v)
		if err := os.Setenv(k, v); err != nil {
			restoreEnv(saved)
			return func() {}, fmt.Errorf("注入 DNS 凭据失败（%s）: %w", k, err)
		}
	}
	return func() { restoreEnv(saved) }, nil
}

// restoreEnv 还原被 applyDNSEnv 改动过的环境变量。
func restoreEnv(saved []savedEnvVar) {
	for _, s := range saved {
		// 用 _ = 忽略错误：还原失败也无力回天，不能因此中断签发。
		if s.had {
			_ = os.Setenv(s.key, s.value)
		} else {
			_ = os.Unsetenv(s.key)
		}
	}
}

// envDisableCNAME 是 lego 用来关掉"跟随 CNAME 委派"的环境变量。
//
// 2026-09-16 真机故障（mini，用户域名 zizdog.com）：该域有泛解析
// `* CNAME lede.zizdog.com`，lego 于是把挑战 TXT 写到 CNAME 目标
// `lede.zizdog.com` 上，而 **Let's Encrypt 不跟随由泛解析产生的 CNAME** ——
// 它直接查 `_acme-challenge.<域名>`，那里什么都没有，于是报
// `urn:ietf:params:acme:error:unauthorized :: No TXT record found at ...`，
// 而面板日志里显示的是"记录已提交"，看起来像是 DNS 没生效，极难排查。
// 关掉跟随之后 TXT 写在挑战名本身上；**显式记录会覆盖泛解析 CNAME**
// （真机验证：权威 NS 会正常返回该 TXT）。
const envDisableCNAME = "LEGO_DISABLE_CNAME_SUPPORT"

// applyCNAMEPolicy 决定本次 dns-01 是否跟随 CNAME 委派，返回还原函数。
//
// 默认**不跟随**：泛解析 CNAME 在 Let's Encrypt 侧本来就无效，跟随只会把记录
// 写到 CA 不看的地方，失败信息还误导人。真正要做 CNAME 委派的用户
// （把 `_acme-challenge` 显式指到另一个区/另一套工具）把 Manager.DNS01FollowCNAME
// 设为 true 即可。
func (m *Manager) applyCNAMEPolicy() func() {
	if m.DNS01FollowCNAME {
		m.emit("dns-01：按配置跟随 CNAME 委派（TXT 会写到 CNAME 的目标名上）")
		return func() {}
	}
	prev, had := os.LookupEnv(envDisableCNAME)
	if err := os.Setenv(envDisableCNAME, "1"); err != nil {
		// 设置失败时不假装成功：如实告警，让用户知道本次会按 lego 默认跟随 CNAME。
		m.emit("警告：无法设置 %s（%v），本次将按 lego 默认跟随 CNAME", envDisableCNAME, err)
		return func() {}
	}
	return func() {
		if had {
			_ = os.Setenv(envDisableCNAME, prev)
		} else {
			_ = os.Unsetenv(envDisableCNAME)
		}
	}
}

// validateEnvKey 校验环境变量名，避免奇怪键名被透传给 os.Setenv。
func validateEnvKey(k string) error {
	if strings.TrimSpace(k) == "" {
		return errors.New("DNS 凭据里存在空的环境变量名")
	}
	if strings.ContainsAny(k, "=\x00") {
		return fmt.Errorf("DNS 凭据的环境变量名 %q 非法", k)
	}
	return nil
}
