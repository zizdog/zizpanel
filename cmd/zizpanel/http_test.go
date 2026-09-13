package main

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// 说明：这些测试刻意使用**真实 TCP 连接**而不是 net.Pipe。
// net.Pipe 是无缓冲的同步管道：当服务端还没读完请求就去写响应时，
// 客户端仍阻塞在写请求上 —— 双方互等，测试直接死锁。
// 真实 socket 有内核缓冲区，不会出现这种情况，也更接近实际路径。

// startRedirectServer 起一个真实监听器（带明文跳转包装），返回它的地址。
func startRedirectServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	wrapped := plainHTTPRedirectListener{Listener: ln}
	go func() {
		for {
			c, err := wrapped.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				handle(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func dialTest(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

// TestPlainHTTPGetsRedirectedToHTTPS 锁定用户实际遇到的问题：
// 地址栏少写 scheme 时浏览器发的是明文 HTTP，面板应该把他引到 https，
// 而不是回一句 "Client sent an HTTP request to an HTTPS server." ——
// 那句话对用户毫无指导意义（真机上就是这么卡住的）。
func TestPlainHTTPGetsRedirectedToHTTPS(t *testing.T) {
	addr := startRedirectServer(t, func(c net.Conn) {
		buf := make([]byte, 1024)
		_, _ = c.Read(buf) // 触发旁路判定并写出 301
	})

	c := dialTest(t, addr)
	if _, err := io.WriteString(c, "GET /sites HTTP/1.1\r\nHost: 192.168.1.4:8443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	s := string(resp)
	if !strings.HasPrefix(s, "HTTP/1.1 301") {
		t.Fatalf("应当返回 301 重定向，实际：%q", s)
	}
	if !strings.Contains(s, "Location: https://192.168.1.4:8443/sites") {
		t.Fatalf("重定向地址不对（应保留路径并带端口），实际：%q", s)
	}
	if !strings.Contains(s, "Connection: close") {
		t.Errorf("应当关闭连接，实际：%q", s)
	}
}

// TestRedirectFallsBackToLocalAddr 没有 Host 头时用监听地址，且要补回端口。
func TestRedirectFallsBackToLocalAddr(t *testing.T) {
	addr := startRedirectServer(t, func(c net.Conn) {
		buf := make([]byte, 1024)
		_, _ = c.Read(buf)
	})

	c := dialTest(t, addr)
	_, _ = io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
	resp, _ := io.ReadAll(c)
	s := string(resp)

	_, port, _ := net.SplitHostPort(addr)
	if !strings.Contains(s, "Location: https://127.0.0.1:"+port+"/") {
		t.Fatalf("缺少 Host 头时应当回退到监听地址并带上端口，实际：%q", s)
	}
}

// TestTLSHandshakePassesThrough 是最重要的一条反面断言：
// 真正的 TLS 客户端（首字节 0x16）必须**原样放行**，
// 否则这个贴心跳转会把所有正常访问也打断 —— 那就成了灾难。
func TestTLSHandshakePassesThrough(t *testing.T) {
	got := make(chan []byte, 1)
	addr := startRedirectServer(t, func(c net.Conn) {
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		got <- buf[:n]
	})

	// 真实 ClientHello 的首字节就是 0x16
	hello := []byte{tlsHandshakeRecord, 0x03, 0x01, 0x00, 0x05, 1, 2, 3, 4, 5}
	c := dialTest(t, addr)
	if _, err := c.Write(hello); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if string(g) != string(hello) {
			t.Fatalf("TLS 握手数据必须原样透传\n  期望: %v\n  实际: %v", hello, g)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("服务端没有拿到 TLS 握手数据（连接被旁路吃掉了）")
	}
}

// TestTLSConstantIsRight 把"0x16 是 TLS 握手首字节"这个前提钉住。
// 如果这里改了，上面那条透传断言就失去意义了。
func TestTLSConstantIsRight(t *testing.T) {
	if tlsHandshakeRecord != 0x16 {
		t.Fatalf("TLS 握手记录类型字节应为 0x16，实际 0x%02x", tlsHandshakeRecord)
	}
}
