package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/sites"
)

// execCommand / jsonUnmarshal 是薄封装，
// 便于在测试里替换，也让调用点更简洁。
func execCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// lookupIDs 把用户名解析为 uid/gid。
func lookupIDs(username string) (int, int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// SiteCheckResult 是站点诊断结果。
type SiteCheckResult struct {
	Domain     string   `json:"domain"`
	URL        string   `json:"url"`
	HTTPCode   string   `json:"http_code"`
	OK         bool     `json:"ok"`
	PHPWorks   bool     `json:"php_works"`
	PHPRaw     bool     `json:"php_raw"`
	ProbeBody  string   `json:"probe_body"`
	HTTPS      bool     `json:"https"`
	CertIssuer string   `json:"cert_issuer"`
	CertExpiry string   `json:"cert_expiry"`
	Issues     []string `json:"issues"`
	Hints      []string `json:"hints"`
	ErrorLog   string   `json:"error_log"`
}

// siteCheckProbeFn 是诊断探测的注入点（默认 curlSite）；单测据此断言探的是站点真实端口。
var siteCheckProbeFn = curlSite

// checkSite 对站点做一次真实访问诊断。
//
// 为什么绕过 DNS：站点域名通常只在本机 hosts 里解析，或者干脆还没解析。
// 直接用 --resolve 把域名钉到 127.0.0.1，才能测到 nginx 而不会被 DNS 干扰。
//
// 端口取站点自己的（EffectiveListenPort）；SSL 固定 443。
// 写死 80 会对自定义端口站点探错目标，把默认站点的响应当成它的结论（谎报）。
func checkSite(ctx context.Context, site *sites.Site, logDir string) *SiteCheckResult {
	res := &SiteCheckResult{Domain: site.Domain, Issues: []string{}, Hints: []string{}}

	scheme := "http"
	port := site.EffectiveListenPort()
	if site.SSLEnabled {
		scheme, port = "https", 443
	}
	res.HTTPS = site.SSLEnabled
	res.URL = siteCheckURL(scheme, site.Domain, port)

	// 1) 主页可访问性
	code, _, err := siteCheckProbeFn(ctx, scheme, site.Domain, port, "/", 8*time.Second)
	res.HTTPCode = code
	switch {
	case err != nil:
		res.Issues = append(res.Issues, "无法连接站点："+err.Error())
		res.Hints = append(res.Hints, fmt.Sprintf(
			"确认 nginx 正在运行：面板「网站管理」页可直接重载；或检查 %d 端口是否被占用", port))
	case code == "000":
		res.Issues = append(res.Issues, "请求超时或连接被拒绝")
		res.Hints = append(res.Hints, "检查站点根目录是否存在、nginx 是否已重载")
	case strings.HasPrefix(code, "5"):
		res.Issues = append(res.Issues, "服务端返回 "+code+"（通常是 PHP 或后端异常）")
		res.Hints = append(res.Hints, "查看错误日志定位具体原因")
	case strings.HasPrefix(code, "4"):
		res.Issues = append(res.Issues, "返回 "+code+"（文件不存在或权限不足）")
		res.Hints = append(res.Hints, "确认站点根目录下有 index 文件，且目录权限可读")
	default:
		res.OK = true
	}

	// 2) PHP 是否真的在解析
	//    写一个探针文件，请求它，检查返回的是令牌还是 PHP 源码。
	//    源码泄漏是最危险的配置错误之一，必须主动检测。
	if site.PHPVersion != "" && site.Root != "" && site.ProxyPass == "" {
		token := "ZPOK" + strconv.FormatInt(time.Now().UnixNano(), 36)
		probe := filepath.Join(site.Root, "__zp_probe.php")
		if err := os.WriteFile(probe, []byte("<?php echo '"+token+"';"), 0o644); err == nil {
			// nginx 的 try_files $uri =404 要求文件真实存在，所以先写文件再请求
			code2, body, _ := siteCheckProbeFn(ctx, scheme, site.Domain, port, "/__zp_probe.php", 8*time.Second)
			_ = os.Remove(probe)
			if strings.Contains(body, token) {
				res.PHPWorks = true
			} else if strings.Contains(body, "<?php") {
				res.PHPRaw = true
				res.Issues = append(res.Issues, "PHP 源码被直接输出（探针文件未被解析）")
				res.Hints = append(res.Hints, "PHP-FPM 未生效：检查 PHP 版本对应的 FPM 是否在运行、fastcgi_pass 地址是否正确")
			} else if code2 != "" && !strings.HasPrefix(code2, "5") && code2 != "000" {
				res.Issues = append(res.Issues, "PHP 探针未返回预期内容（HTTP "+code2+"）")
				res.Hints = append(res.Hints, "检查站点根目录是否为运行目录（Laravel/ThinkPHP 需要指向 public）")
			}
		}
	}

	// 3) HTTPS 证书信息
	if site.SSLEnabled && site.SSLCert != "" {
		if subject, issuer, notAfter, err := priv.CertInfo(site.SSLCert); err == nil {
			res.CertIssuer = issuer
			if !notAfter.IsZero() {
				res.CertExpiry = notAfter.Format("2006-01-02")
				if time.Until(notAfter) < 30*24*time.Hour {
					res.Issues = append(res.Issues, "证书将在 30 天内到期："+res.CertExpiry)
					res.Hints = append(res.Hints, "重新签发证书（自签或 mkcert）")
				}
			}
			_ = subject
		} else {
			res.Issues = append(res.Issues, "无法读取证书："+err.Error())
		}
	}

	// 4) 最近的错误日志（最有价值的排障线索）
	errLog := filepath.Join(logDir, site.Domain+".error.log")
	if content, _, err := tailFile(errLog, 30); err == nil && strings.TrimSpace(content) != "" {
		res.ErrorLog = content
	}

	if len(res.Issues) == 0 {
		res.Hints = append(res.Hints, "站点访问正常")
	}
	return res
}

// siteCheckURL 拼诊断/展示用的站点地址：非默认端口必须带上，
// 否则界面显示的地址是错的（自定义端口的站点在 80 上根本不存在）。
func siteCheckURL(scheme, domain string, port int) string {
	if (scheme == "http" && port == 80) || (scheme == "https" && port == 443) {
		return scheme + "://" + domain + "/"
	}
	return fmt.Sprintf("%s://%s:%d/", scheme, domain, port)
}

// curlSite 用 curl 请求站点，返回 HTTP 状态码与响应体。
//
// 用 --resolve 把域名固定解析到 127.0.0.1：
// 这样即使域名没有 DNS 解析、或者 hosts 没配，也能测到本机 nginx。
func curlSite(ctx context.Context, scheme, domain string, port int, path string, timeout time.Duration) (code, body string, err error) {
	url := fmt.Sprintf("%s://%s:%d%s", scheme, domain, port, path)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-sS", "-k", "--max-time", strconv.Itoa(int(timeout.Seconds())),
		"--resolve", fmt.Sprintf("%s:%d:127.0.0.1", domain, port),
		"-o", "-", "-w", "\n__ZP_CODE__%{http_code}",
		url,
	}
	out, err := execCommand(ctx, "/usr/bin/curl", args...).Output()
	if err != nil && len(out) == 0 {
		return "000", "", err
	}
	s := string(out)
	if i := strings.LastIndex(s, "__ZP_CODE__"); i >= 0 {
		code = strings.TrimSpace(s[i+len("__ZP_CODE__"):])
		s = s[:i]
	}
	// 响应体截断，避免把巨大页面塞进接口响应
	if len(s) > 2000 {
		s = s[:2000] + "\n…（已截断）"
	}
	return code, s, nil
}
