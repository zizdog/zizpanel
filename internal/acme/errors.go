package acme

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	legoacme "github.com/go-acme/lego/v4/acme"
)

// safeError 是一个「对外文本已脱敏、但错误链完整」的错误。
//
// Error() 返回脱敏后的文本，Unwrap() 保留原始错误，
// 因此调用方既能安全打印，又能用 errors.As 判断速率限制等具体类型。
// 为什么需要它：lego / provider 的错误文本里可能带 DNS 凭据，
// 而 %w 包装会让 err.Error() 原样带出这些值。
type safeError struct {
	msg string
	err error
}

func (e *safeError) Error() string { return e.msg }
func (e *safeError) Unwrap() error { return e.err }

// safeWrap 生成脱敏错误：msg 会经过 redact，原始 err 保留在错误链里。
func safeWrap(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	head := fmt.Sprintf(format, args...)
	return &safeError{msg: redact(head + ": " + err.Error()), err: err}
}

// hintFor 按失败种类和验证方式给出「可能原因」。
// 目标：用户看到日志时能直接知道下一步查什么，而不是只看到 ACME 的原始报错。
func hintFor(err error, challenge ChallengeType) string {
	if err == nil {
		return ""
	}
	var hints []string

	// 速率限制：最常见、也最需要明确指引的一种失败。
	var rate *legoacme.RateLimitedError
	if errors.As(err, &rate) {
		hints = append(hints, "已触发 CA 速率限制：Let's Encrypt 生产环境同一注册域名每周 5 次签发，"+
			"建议先用 letsencrypt-staging 验证配置，或等配额恢复")
	}
	if isTimeout(err) {
		hints = append(hints, "请求 CA 超时：检查服务器能否出网（DNS 解析与 443 端口）")
	}
	if isDNSError(err) {
		hints = append(hints, "域名解析失败：检查服务器 /etc/resolv.conf 与 DNS 是否可用")
	}

	switch challenge {
	case ChallengeHTTP01:
		hints = append(hints, "http-01 失败常见原因：域名 A/AAAA 记录没有指向本机、"+
			"80 端口未放行（云安全组 / 防火墙 / 路由器映射）、"+
			"或 nginx 未把 /.well-known/acme-challenge/ 指到网站根目录")
	case ChallengeDNS01:
		hints = append(hints, "dns-01 失败常见原因：DNS 凭据无效或权限不足（需要该域名的解析读写权限）、"+
			"域名不在该账号下、_acme-challenge 存在冲突记录、"+
			"或 DNS 解析尚未生效（可调大 DNSTimeout 后重试）")
	}

	if len(hints) == 0 {
		return ""
	}
	return "。可能原因：" + strings.Join(hints, "；")
}

// isTimeout 判断错误链里是否有超时。
func isTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// isDNSError 判断是否是域名解析类错误。
func isDNSError(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
