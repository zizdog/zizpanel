package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// httpHealth 对一个带 HTTP 接口的服务做健康检查。
//
// 为什么用 curl 而不是 Go 的 http.Client：
// 被检查的服务通常监听 127.0.0.1 的明文 HTTP，也可能有自签 HTTPS。
// 用 curl 能复用系统的 CA 信任链（包括 mkcert），
// 而 Go 的默认 TLS 配置需要额外处理；同时 curl 的 -w 输出比手工计时更简单。
//
// 判定规则：
//   - 期望值（health_expect）为空时，HTTP 2xx/3xx 视为健康
//   - 指定了期望值时，要求响应体包含该子串（用于 {"status":"ok"} 这类接口）
func httpHealth(ctx context.Context, s *Service) Health {
	h := Health{Checked: false, URL: s.HealthURL}
	if s.HealthURL == "" {
		return h
	}
	h.Checked = true
	h.CheckedAt = time.Now().Format("2006-01-02 15:04:05")

	timeout := 6 * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	args := []string{
		"-sS", "-k", "--max-time", strconv.Itoa(int(timeout.Seconds())),
		"-o", "-", "-w", "\n__ZP_HTTP__%{http_code}\n__ZP_REDIR__%{redirect_url}",
		s.HealthURL,
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/curl", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	h.Latency = time.Since(start).Milliseconds()

	// curl 的失败原因写在 stderr（例如 "Connection refused"），
	// 只取 stdout 会得到一个没信息量的 "exit status 7"
	body := string(out)
	if err != nil && strings.TrimSpace(stderr.String()) != "" {
		body = strings.TrimSpace(stderr.String())
	}
	// 先取 redirect_url 再取 http_code：`-w` 的输出顺序是
	// <body> "\n__ZP_HTTP__<code>\n__ZP_REDIR__<url>"，`body[:i]` 截断会把
	// 后面的标记一起切掉，所以必须**从后往前**解析。
	redirect := ""
	if i := strings.LastIndex(body, "__ZP_REDIR__"); i >= 0 {
		rest := body[i+len("__ZP_REDIR__"):]
		if j := strings.IndexAny(rest, "\r\n"); j >= 0 {
			rest = rest[:j]
		}
		redirect = strings.TrimSpace(rest)
		body = body[:i]
	}
	code := 0
	if i := strings.LastIndex(body, "__ZP_HTTP__"); i >= 0 {
		rest := strings.TrimRight(body[i+len("__ZP_HTTP__"):], "\r\n")
		if n, e := strconv.Atoi(strings.TrimSpace(rest)); e == nil {
			code = n
		}
		body = body[:i]
	}
	h.Code = code

	if err != nil && code == 0 {
		h.OK = false
		h.Message = humanizeCurlError(err, body)
		return h
	}

	// 「安全超时锁定」必须判成**不健康**。
	//
	// 真机反馈（2026-09-17）：Portainer 锁定时 GET / 返回 307 → /timeout.html，
	// 而通用规则是"2xx/3xx 都算健康" → 面板对一个已经打不开、只剩
	// "…timed out for security purposes" 页的实例报 health.ok=true ——
	// 这正是本仓库最忌讳的"谎报成功"。所以这里单独把这种 3xx 挑出来：
	// 目标里含 timeout.html，或正文里直接出现锁定文案（未跟随重定向时是兜底）。
	if code >= 300 && code < 400 {
		lowerRedirect := strings.ToLower(redirect)
		lowerBody := strings.ToLower(body)
		if strings.Contains(lowerRedirect, "timeout.html") ||
			strings.Contains(lowerBody, "timed out for security purposes") {
			h.OK = false
			if app, ok := FindAppByService(s); ok && app.ID == "portainer" {
				h.Message = fmt.Sprintf("HTTP %d：Portainer 已因超时锁定（跳到 %s）；"+
					"重启容器即可重新进入 setup 页（口令设置窗口 5 分钟）", code, redirect)
			} else {
				h.Message = fmt.Sprintf("HTTP %d：该服务已因安全超时锁定（跳到 %s）；"+
					"需要按它自己的方式重新解锁", code, redirect)
			}
			return h
		}
	}

	if want := strings.TrimSpace(s.HealthExpect); want != "" {
		if strings.Contains(body, want) {
			h.OK = true
			h.Message = fmt.Sprintf("HTTP %d，响应包含 %q", code, want)
		} else {
			h.OK = false
			h.Message = fmt.Sprintf("HTTP %d，但响应不包含 %q（实际内容：%s）",
				code, want, truncate(strings.TrimSpace(body), 160))
		}
		return h
	}

	// 401 / 403 判为**健康**。
	//
	// 用户报过这个误报：Stirling PDF 这类应用装完首次打开就要求设账号密码
	// （那是它自己的登录），设完之后面板的健康检查必然拿到 401，
	// 于是服务管理里一直显示"健康检查失败" —— 可服务完全正常。
	//
	// HTTP 健康检查的本质是"这个服务还活着吗"。能返回 401/403 恰恰证明
	// HTTP 服务在监听、能处理请求、只是这个地址需要身份验证。
	// 真想校验业务内容，用上面的「期望包含内容」字段。
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		h.OK = true
		h.Message = fmt.Sprintf("HTTP %d（需要身份验证，服务本身正常）", code)
		return h
	}

	h.OK = code >= 200 && code < 400
	h.Message = fmt.Sprintf("HTTP %d", code)
	return h
}

// humanizeCurlError 把 curl 的失败转成用户能理解的话。
//
// 注意 curl 的措辞随版本/平台变：老版本说 "Connection refused"，
// Homebrew 上较新的 curl 说 "Failed to connect to 127.0.0.1 port 8080
// after 0 ms: Couldn't connect to server"。只匹配老措辞的话，
// 用户看到的是一整行原始 curl 报错（实测就是这样），所以两种都要认。
func humanizeCurlError(err error, body string) string {
	msg := strings.TrimSpace(body)
	switch {
	case strings.Contains(msg, "Connection refused"),
		strings.Contains(msg, "Couldn't connect to server"),
		strings.Contains(msg, "Failed to connect"):
		return "连接不上（该端口没有在监听，或服务没启动）"
	case strings.Contains(msg, "Operation timed out"), strings.Contains(msg, "timed out"):
		return "连接超时（服务可能卡住或未启动）"
	case strings.Contains(msg, "Empty reply from server"):
		return "服务接受了连接但没有返回内容（可能正在启动）"
	case strings.Contains(msg, "Could not resolve host"):
		return "域名无法解析"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "SSL"):
		return "证书校验失败：" + truncate(msg, 120)
	case msg != "":
		return truncate(msg, 160)
	}
	return err.Error()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DetectDocker 检测 Docker 是否可用，返回 socket 路径。
//
// 这里的判断只依赖 socket 文件是否存在且可连接 ——
// 不去调 `docker` 命令，因为 docker CLI 可能没装，
// 而 socket 存在就意味着我们可以直接用 HTTP API 管理容器。
//
// userHome 必须传**真实用户**的家目录（config.UserHome），不能用
// os.UserHomeDir()：面板以 root 通过 LaunchDaemon 运行，
// os.UserHomeDir() 会得到 /var/root，于是永远找不到
// ~/.orbstack/run/docker.sock —— OrbStack 明明在跑却报"未安装 Docker"。
func DetectDocker(socket, userHome string) (string, string) {
	candidates := []string{socket, "/var/run/docker.sock"}
	if userHome == "" {
		userHome = userHomeDir()
	}
	// OrbStack / Colima / Docker Desktop 的用户级 socket
	if userHome != "" {
		candidates = append(candidates,
			userHome+"/.orbstack/run/docker.sock",
			userHome+"/.colima/default/docker.sock",
			userHome+"/.docker/run/docker.sock",
		)
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		conn, err := net.DialTimeout("unix", c, 800*time.Millisecond)
		if err != nil {
			continue
		}
		_ = conn.Close()
		// 能连通就读一次版本，确认确实是 Docker API
		if v, err := dockerVersion(c); err == nil {
			return c, v
		}
		return c, "unknown"
	}
	return "", ""
}

// dockerVersion 通过 socket 调 /version，返回简短版本串。
func dockerVersion(socket string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 2 * time.Second}
				return d.DialContext(ctx, "unix", socket)
			},
		},
		Timeout: 3 * time.Second,
	}
	resp, err := client.Get("http://docker/version")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var v struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.Version == "" {
		return "unknown", nil
	}
	return v.Version, nil
}
