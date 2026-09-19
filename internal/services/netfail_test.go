package services

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestIsNetworkFailureTypicalErrors 是这条判据的核心门禁：
// 喂**真实的**网络报错串（Go net/http、curl、brew/git、docker 的原话），必须命中。
func TestIsNetworkFailureTypicalErrors(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"Go DNS 解析失败", `Get "https://github.com/typecho/typecho/releases/download/v1.3.0/typecho.zip": dial tcp: lookup github.com: no such host`},
		{"Go dial i/o timeout", `Get "https://zizdog.com/zizpanel/manifest.json": dial tcp 1.2.3.4:443: i/o timeout`},
		{"TLS 握手超时", `Get "https://mirror.zizdog.com:8888/zizpanel/manifest.json": net/http: TLS handshake timeout`},
		{"证书校验失败", `Get "https://x.test/a": tls: failed to verify certificate: x509: certificate signed by unknown authority`},
		{"代理连不上（proxyconnect）", `Get "https://github.com/x": proxyconnect tcp: dial tcp 127.0.0.1:7890: connect: connection refused`},
		{"git/curl 解析失败", `fatal: unable to access 'https://github.com/typecho/typecho/': Could not resolve host: github.com`},
		{"curl 超时", `curl: (28) Connection timed out after 30001 milliseconds`},
		{"glibc 解析失败", `Temporary failure in name resolution`},
		{"curl 连不上镜像", `curl: (7) Failed to connect to mirror.zizdog.com port 443 after 10 ms: Couldn't connect to server`},
		{"局域网镜像被拒", `dial tcp 192.168.1.8:8090: connect: connection refused`},
		{"docker 拉取超时", `Error response from daemon: Get "https://registry-1.docker.io/v2/": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)`},
		{"HTTP 客户端整体超时（带 URL）", `Get "https://zizdog.com/zizpanel/download/1.4.9/manifest.json": context deadline exceeded`},
		{"DNS server misbehaving", `lookup zizdog.com on 192.168.1.1:53: server misbehaving`},
		{"no route to host", `dial tcp 10.0.0.1:443: connect: no route to host`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !IsNetworkFailure(errors.New(c.msg)) {
				t.Errorf("应判为网络问题，实际漏报：%s", c.msg)
			}
			if !IsNetworkFailureMessage(c.msg) {
				t.Errorf("IsNetworkFailureMessage 应判为网络问题：%s", c.msg)
			}
		})
	}
}

// TestIsNetworkFailureTypedEvidence 锁住"类型证据"这条独立路径：
// 错误文本里没有任何网络关键词时，靠 *net.DNSError / *net.OpError 也要能认出来。
func TestIsNetworkFailureTypedEvidence(t *testing.T) {
	dns := &net.DNSError{Err: "opaque-internal", Name: "example.test"}
	if strings.Contains(strings.ToLower(dns.Error()), "no such host") {
		t.Fatalf("这条用例的前提是错误文本里没有网络关键词，实际: %v", dns)
	}
	if !IsNetworkFailure(dns) {
		t.Errorf("*net.DNSError 应判为网络问题: %v", dns)
	}

	op := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("opaque-internal")}
	if !IsNetworkFailure(op) {
		t.Errorf("*net.OpError(tcp) 应判为网络问题: %v", op)
	}

	sys := &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}
	if !IsNetworkFailure(sys) {
		t.Errorf("syscall ECONNREFUSED 应判为网络问题: %v", sys)
	}
}

// TestIsNetworkFailureDoesNotMisreportNonNetwork 是**反例门禁**：
// 这些失败都不是网络问题，必须原样返回 false（误报比漏报更糟）。
func TestIsNetworkFailureDoesNotMisreportNonNetwork(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"权限不足", `open /opt/zizpanel/data/panel.db: permission denied`},
		{"文件不存在", `open /opt/homebrew/bin/brew: no such file or directory`},
		{"brew 未安装", `exec: "brew": executable file not found in $PATH`},
		{"brew 未安装（中文）", `Homebrew 未安装，无法继续：/opt/homebrew/bin/brew 不存在`},
		{"formula 不存在", `Error: No available formula with the name "not-a-real-formula".`},
		{"镜像上没有这个版本（404）", `下载清单失败: HTTP 404`},
		{"镜像返回 403", `下载清单失败: HTTP 403`},
		{"签名校验失败", `清单签名验证失败：ed25519: bad signature`},
		{"sha256 不匹配", `升级包校验和不匹配（期望 abc123，实际 def456），文件已丢弃`},
		{"磁盘空间不足", `write /tmp/x.tar.gz: no space left on device`},
		{"端口被占用", `listen tcp :8880: bind: address already in use`},
		{"裸 context 超时（无 URL）", `context deadline exceeded`},
		{"取消请求", `context canceled`},
		{"文件截断", `unexpected EOF`},
		{"任务日志里一条普通失败", `任务失败：brew install 失败`},
		// 本地端点：服务没起来 ≠ 要翻墙。
		{"docker 守护进程没运行", `Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?`},
		{"docker socket 被拒", `Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.24/containers/json": dial unix /var/run/docker.sock: connect: connection refused`},
		{"本地应用探测被拒", `Get "http://127.0.0.1:8880/v1/models": dial tcp 127.0.0.1:8880: connect: connection refused`},
		{"本地 nginx 探测超时", `Get "http://localhost:80/health": context deadline exceeded`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if IsNetworkFailure(errors.New(c.msg)) {
				t.Errorf("这不是网络问题，不该误报：%s", c.msg)
			}
			if IsNetworkFailureMessage(c.msg) {
				t.Errorf("IsNetworkFailureMessage 不该误报：%s", c.msg)
			}
		})
	}
}

// TestIsNetworkFailureNil 空错误永远不是网络问题。
func TestIsNetworkFailureNil(t *testing.T) {
	if IsNetworkFailure(nil) {
		t.Error("nil 不该判为网络问题")
	}
	if IsNetworkFailureMessage("") || IsNetworkFailureMessage("   ") {
		t.Error("空文本不该判为网络问题")
	}
}

// TestAppendNetworkHintKeepsOriginalAndIsIdempotent 追加提示时原文不丢、
// 且 %w 链不被破坏（上层 errors.Is 还要能用）。
func TestAppendNetworkHintKeepsOriginalAndIsIdempotent(t *testing.T) {
	base := errors.New(`Get "https://github.com/x": dial tcp: lookup github.com: no such host`)
	wrapped := AppendNetworkHint(fmt.Errorf("下载清单失败: %w", base))

	msg := wrapped.Error()
	if !strings.Contains(msg, "下载清单失败") || !strings.Contains(msg, "no such host") {
		t.Errorf("原有错误原文丢了：%s", msg)
	}
	if !strings.Contains(msg, NetworkHintMarker) {
		t.Errorf("没有附上网络提示：%s", msg)
	}
	if !errors.Is(wrapped, base) {
		t.Errorf("%%w 链断了，errors.Is 认不回原始错误")
	}

	again := AppendNetworkHint(wrapped)
	if strings.Count(again.Error(), NetworkHintMarker) != 1 {
		t.Errorf("重复调用应幂等，实际提示出现 %d 次：%s",
			strings.Count(again.Error(), NetworkHintMarker), again.Error())
	}
}

// TestNetworkHintIsShort 锁文案纪律：一句话、≤40 字、不含换行。
func TestNetworkHintIsShort(t *testing.T) {
	h := NetworkHint()
	if n := len([]rune(h)); n > 40 {
		t.Errorf("网络提示必须是一句话 ≤40 字，实际 %d 字：%s", n, h)
	}
	if strings.ContainsAny(h, "\n\r") {
		t.Errorf("网络提示不能多行：%q", h)
	}
	if !strings.Contains(h, NetworkHintMarker) {
		t.Errorf("提示里必须包含识别短语 %q：%s", NetworkHintMarker, h)
	}
}

// TestAppendNetworkHintLeavesNonNetworkAlone 非网络错误必须**原样**返回
// （连字符串都不要动，否则会污染用户看到的真实原因）。
func TestAppendNetworkHintLeavesNonNetworkAlone(t *testing.T) {
	base := errors.New("open /opt/homebrew/bin/brew: no such file or directory")
	if got := AppendNetworkHint(base); got != base {
		t.Errorf("非网络错误应原样返回，实际: %v", got)
	}
	if AppendNetworkHint(nil) != nil {
		t.Error("nil 应原样返回 nil")
	}

	txt := "Error: No available formula with the name \"foo\"."
	if got := AppendNetworkHintToText(txt); got != txt {
		t.Errorf("非网络文本应原样返回，实际: %q", got)
	}

	netTxt := `curl: (28) Connection timed out after 30001 milliseconds`
	got := AppendNetworkHintToText(netTxt)
	if !strings.Contains(got, "Connection timed out") || !strings.Contains(got, NetworkHintMarker) {
		t.Errorf("网络文本应保留原文并追加提示，实际: %q", got)
	}
	if strings.Count(AppendNetworkHintToText(got), NetworkHintMarker) != 1 {
		t.Errorf("AppendNetworkHintToText 也应幂等")
	}
}
