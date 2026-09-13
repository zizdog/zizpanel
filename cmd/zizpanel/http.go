package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/logx"
	"github.com/zizdog/zizpanel/internal/tlsx"
)

// newHTTPServer 构造 http.Server，统一超时与 TLS 参数。
//
// 超时取舍：面板是管理后台，普通请求都很短，设置读超时可防 Slowloris；
// 但指标推送是 SSE 长连接，所以 WriteTimeout 必须为 0（不限制），
// 改用 IdleTimeout + 保活心跳来防连接泄漏。
func newHTTPServer(cfg *config.Config, handler http.Handler, log *logx.Logger) *http.Server {
	_ = log
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
}

// listenAndServe 根据配置以 HTTP 或 HTTPS 启动。
func listenAndServe(cfg *config.Config, srv *http.Server) error {
	if cfg.TLSEnable {
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		return srv.ServeTLS(plainHTTPRedirectListener{Listener: ln}, cfg.TLSCert, cfg.TLSKey)
	}
	return srv.ListenAndServe()
}

// ---------------------------------------------------------------------------
//  明文 HTTP 误访问 HTTPS 端口时自动跳转
// ---------------------------------------------------------------------------
//
// 现象：在地址栏只输入 "192.168.1.4:8443"（不写 scheme）时，浏览器默认用
// http:// 发请求，而面板这个端口只接受 TLS —— Go 的标准应答是一句
//   Client sent an HTTP request to an HTTPS server.
// 用户看到这句话完全不知道该怎么办（实测：真机上就这么卡住了）。
//
// 做法：在 TLS 之前偷看连接的第一个字节。TLS 握手记录的首字节固定是 0x16，
// 不是它就说明这是明文 HTTP，于是回一个 301 把浏览器引到 https:// 去。
// 这样"少打几个字符"不会变成一个需要排查的故障。
//
// 安全性：只做重定向，不把明文请求交给业务处理器；也就是说这个旁路
// 不会让任何请求绕过 TLS 到达面板逻辑。

type plainHTTPRedirectListener struct {
	net.Listener
}

func (l plainHTTPRedirectListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &plainHTTPRedirectConn{Conn: c}, nil
}

type plainHTTPRedirectConn struct {
	net.Conn
	br      *bufio.Reader
	checked bool
}

// tlsHandshakeRecord 是 TLS 握手记录的类型字节。
const tlsHandshakeRecord = 0x16

func (c *plainHTTPRedirectConn) Read(b []byte) (int, error) {
	if c.br == nil {
		c.br = bufio.NewReader(c.Conn)
	}
	if !c.checked {
		c.checked = true
		if first, err := c.br.Peek(1); err == nil && len(first) == 1 && first[0] != tlsHandshakeRecord {
			return c.redirectToHTTPS()
		}
	}
	return c.br.Read(b)
}

// redirectToHTTPS 解析明文请求并回一个 301。
//
// 保留原始路径：收藏了 /api/... 之类的地址也能正确跳到对应页面。
func (c *plainHTTPRedirectConn) redirectToHTTPS() (int, error) {
	// 请求行 + 头部最多读 8KB，防止有人拿这个旁路灌数据
	br := io.LimitReader(c.br, 8<<10)
	reader := bufio.NewReader(br)

	requestLine, _ := reader.ReadString('\n')
	path := "/"
	if parts := strings.Fields(requestLine); len(parts) >= 2 {
		path = parts[1]
	}

	// 优先用 Host 头（反代后面它才是用户看到的地址），否则用本地地址
	host := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			break
		}
		if v, ok := strings.CutPrefix(strings.ToLower(line), "host:"); ok {
			host = strings.TrimSpace(v)
			break
		}
	}
	if host == "" {
		host = c.Conn.LocalAddr().String()
	}
	// Host 里没带端口就补上本机端口，否则会跳到 443
	if !strings.Contains(host, ":") {
		if _, port, err := net.SplitHostPort(c.Conn.LocalAddr().String()); err == nil {
			host = net.JoinHostPort(host, port)
		}
	}

	location := "https://" + host + path
	_, _ = io.WriteString(c.Conn,
		"HTTP/1.1 301 Moved Permanently\r\n"+
			"Location: "+location+"\r\n"+
			"Connection: close\r\n"+
			"Content-Length: 0\r\n\r\n")
	return 0, io.EOF
}

// panelURLs 生成面板的可访问地址列表。
//
// 必须从 cfg.Listen 里解析端口，不能直接把 Listen 拼到 IP 后面 ——
// Listen 形如 ":8443"，直接拼会得到 "192.168.1.5:8443" 看似正确，
// 但若 Listen 是 "127.0.0.1:8443" 就会拼成 "192.168.1.5127.0.0.1:8443"。
// 这个拼接错误在真实机器上复现过，所以这里统一走一个函数。
func panelURLs(cfg *config.Config) []string {
	scheme := "https"
	if !cfg.TLSEnable {
		scheme = "http"
	}
	port := cfg.Port()
	portPart := ""
	if port > 0 && port != 80 && port != 443 {
		portPart = ":" + strconv.Itoa(port)
	} else if port > 0 {
		portPart = ":" + strconv.Itoa(port)
	}

	out := []string{fmt.Sprintf("%s://127.0.0.1%s", scheme, portPart)}
	if ip := tlsx.PrimaryIP(); ip != "" && ip != "127.0.0.1" {
		host := ip
		if strings.Contains(ip, ":") { // IPv6 需要方括号
			host = "[" + ip + "]"
		}
		out = append(out, fmt.Sprintf("%s://%s%s", scheme, host, portPart))
	}
	return out
}

// readPassword 从终端读取密码（关闭回显）。非终端环境（管道）退化为读一行。
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
