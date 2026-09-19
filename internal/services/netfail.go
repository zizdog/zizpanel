// netfail.go —— 网络失败判据与统一文案（只认有真实证据的网络错误，拿不准返回 false）。
// 调用点：upgrade 的源与下载、web 的市场探测/安装；前端按 NetworkHintMarker 渲染醒目 pill。
package services

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
)

// NetworkHintMarker 是前后端共用的识别短语（前端 apps.js/update.js 各有一份同名常量）。
const NetworkHintMarker = "面板不会替你翻墙"

// NetworkHint 返回统一文案（一句话；入口名在前端折叠项里给，见 apps.js/update.js）。
func NetworkHint() string {
	return "🌐 网络问题：" + NetworkHintMarker + "，请自备代理/梯子后重试"
}

// IsNetworkFailure 判断 err 是否有真实证据表明失败原因是网络；判不准返回 false。
func IsNetworkFailure(err error) bool {
	if err == nil {
		return false
	}
	if hasNetworkErrorType(err) && !isLocalSocketError(err) {
		return true
	}
	return IsNetworkFailureMessage(err.Error())
}

// IsNetworkFailureMessage 与 IsNetworkFailure 同判据，吃文本（brew stderr、任务 error 字段）。
func IsNetworkFailureMessage(text string) bool {
	msg := strings.ToLower(strings.TrimSpace(text))
	if msg == "" {
		return false
	}
	if containsAny(msg, localOverrideMarkers) { // 走代理失败优先于本地端点保护
		return true
	}
	if containsAny(msg, localEndpointMarkers) { // 本地服务没起来 ≠ 要翻墙
		return false
	}
	if containsAny(msg, networkMarkers) {
		return true
	}
	// 裸 context 超时不算证据；HTTP 请求超时必然带 URL。
	return strings.Contains(msg, "context deadline exceeded") &&
		(strings.Contains(msg, "http://") || strings.Contains(msg, "https://"))
}

// AppendNetworkHint 把提示追加到错误上：原文不丢（%w 保留链）、幂等、非网络错误原样返回。
func AppendNetworkHint(err error) error {
	if err == nil || !IsNetworkFailure(err) {
		return err
	}
	if strings.Contains(err.Error(), NetworkHintMarker) {
		return err
	}
	return fmt.Errorf("%w\n%s", err, NetworkHint())
}

// AppendNetworkHintToText 是 AppendNetworkHint 的字符串版（原文不丢、幂等）。
func AppendNetworkHintToText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.Contains(trimmed, NetworkHintMarker) {
		return text
	}
	if !IsNetworkFailureMessage(trimmed) {
		return text
	}
	return text + "\n" + NetworkHint()
}

// localOverrideMarkers 优先判网络：proxyconnect 证明请求确实经过代理。
var localOverrideMarkers = []string{"proxyconnect"}

// localEndpointMarkers 命中即不算网络（docker.sock / 本机回环）。
var localEndpointMarkers = []string{
	"unix://", "dial unix", "docker.sock", "127.0.0.1", "localhost", "[::1]",
}

// networkMarkers 文本证据；刻意不含 eof、curl:(22)、中文"超时"等泛化词。
var networkMarkers = []string{
	"could not resolve host", "temporary failure in name resolution",
	"name or service not known", "no such host", "server misbehaving",
	"connection refused", "connection timed out", "connection timeout",
	"connection reset by peer", "no route to host", "network is unreachable",
	"network is down", "host is down", "i/o timeout", "operation timed out",
	"failed to connect to", "couldn't connect to server", "could not connect to server",
	"dial tcp", "dial udp",
	"tls handshake timeout", "tls: failed to verify certificate", "tls: bad certificate",
	"remote error: tls:", "x509:", "certificate signed by unknown authority",
	"certificate is not valid for",
	"client.timeout exceeded", "request canceled while waiting for connection",
	"curl: (6)", "curl: (7)", "curl: (28)", "curl: (35)", "curl: (52)",
	"curl: (56)", "curl: (60)", "ssl connect error",
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// hasNetworkErrorType 是类型层证据（比字符串可靠）。
func hasNetworkErrorType(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var certVerifyErr *tls.CertificateVerificationError
	if errors.As(err, &certVerifyErr) {
		return true
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}
	var unknownAuthErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthErr) {
		return true
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return true
	}
	var certInvalidErr x509.CertificateInvalidError
	if errors.As(err, &certInvalidErr) {
		return true
	}
	var sysErr *os.SyscallError
	return errors.As(err, &sysErr) && isNetworkErrno(sysErr.Err)
}

// isLocalSocketError 报告类型证据是不是本地 unix socket（那不是网络问题）。
func isLocalSocketError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && strings.HasPrefix(strings.ToLower(opErr.Net), "unix")
}

// isNetworkErrno 只认明确的网络 errno（不认权限/不存在/磁盘满/管道）。
func isNetworkErrno(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETDOWN)
}
