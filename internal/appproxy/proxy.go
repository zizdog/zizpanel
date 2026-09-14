// Package appproxy 把"有网页界面的应用"挂到面板的 `/<slug>/` 子路径下。
//
// 为什么需要它（用户明确要求）：面板装了一堆带界面的应用，每个都占一个端口 ——
// 用户得记住 `192.168.1.4:8080` 是 IOPaint、`:3001` 是 Kuma……
// 而且这些端口一旦经反向代理/隧道暴露出去，路径也是乱的。
// 现在统一走 `http://<主机>/<slug>/`（经 nginx 的 80 端口）或
// `https://<面板地址>/<slug>/`（面板自己就能反代，无需 nginx）。
//
// 三个必须处理的现实问题（都不是"配个 proxy_pass"就完事的）：
//
//  1. **前端写的是绝对路径。** Vite/React 构建产物里是
//     `<script src="/assets/index-xxx.js">`、`fetch("/api/v1")`、`io("/socket.io")`。
//     挂在 `/iopaint/` 下时这些请求会打到站点根路径 → 白屏或接口 404。
//     所以要改写响应体（HTML/JS/CSS）里的这些前缀，规则由目录条目声明。
//  2. **重定向也是绝对路径。** Uptime Kuma 访问 `/` 会 302 到 `Location: /dashboard`，
//     不改写就会把用户甩出子路径。所以 Location 响应头也要改写。
//  3. **压缩会让改写失效。** 上游若返回 gzip，正文里根本没有明文可替换。
//     因此出站请求要摘掉 Accept-Encoding，让上游给未压缩内容。
//
// 另外要如实：子路径不是万能的（有些应用必须自己设 root_url / base path）。
// 所以这个包只负责"能做的部分"，能不能用由 API 的探测（probe）说了算 ——
// 探测不过就把「直连端口」当首选入口，而不是假装能打开。
package appproxy

import (
	"bytes"
	"html"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
)

// 默认改写规则：绝大多数前端构建产物的公共前缀。
//
// 放在这里而不是每个目录条目里重复一遍：它们是"前端构建产物的通病"，
// 与具体应用无关。目录条目只需要补充自己特有的（API 前缀、socket.io 路径等）。
var defaultRules = []services.UIRewrite{
	{From: "/assets/", To: "/{slug}/assets/"},
	{From: "/favicon.ico", To: "/{slug}/favicon.ico"},
}

// rewriter 把一个应用的改写规则编译成可直接用于字节替换的形式。
type rewriter struct {
	pairs [][2]string
}

func newRewriter(slug string, rules []services.UIRewrite) *rewriter {
	all := make([]services.UIRewrite, 0, len(defaultRules)+len(rules))
	all = append(all, defaultRules...)
	all = append(all, rules...)
	r := &rewriter{}
	seen := map[string]bool{}
	for _, rule := range all {
		if rule.From == "" {
			continue
		}
		// 目录里可能写重复规则（例如应用自己又写了一遍 /assets/），去重避免无意义替换
		if seen[rule.From] {
			continue
		}
		seen[rule.From] = true
		r.pairs = append(r.pairs, [2]string{rule.From, strings.ReplaceAll(rule.To, "{slug}", slug)})
	}
	return r
}

// body 对响应体做一次左到右的字面量替换。
//
// bytes.ReplaceAll 不会重新扫描被替换进来的内容，所以 "/assets/" → "/iopaint/assets/"
// 不会二次替换（这一点很重要：否则会变成 /iopaint/iopaint/assets/）。
func (r *rewriter) body(b []byte) []byte {
	for _, p := range r.pairs {
		if bytes.Contains(b, []byte(p[0])) {
			b = bytes.ReplaceAll(b, []byte(p[0]), []byte(p[1]))
		}
	}
	return b
}

// location 改写重定向目标：只改"根相对路径"，不碰绝对 URL 与已带前缀的。
func (r *rewriter) location(slug, loc string) string {
	if loc == "" || strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	prefix := "/" + slug
	if loc == prefix || strings.HasPrefix(loc, prefix+"/") {
		return loc
	}
	if !strings.HasPrefix(loc, "/") {
		return loc // 相对路径由浏览器按当前路径解析，不用管
	}
	return prefix + loc
}

// 可改写的响应类型。只处理文本：二进制（图片/字体/音频）改了会损坏。
func rewritableContentType(ct string) bool {
	ct = strings.ToLower(ct)
	for _, p := range []string{"text/html", "text/css", "javascript", "ecmascript", "application/json", "text/plain", "image/svg+xml", "text/xml", "application/xml"} {
		if strings.Contains(ct, p) {
			return true
		}
	}
	return false
}

// Handler 返回某个应用的反向代理。
//
// 传入的 App 必须带 UI（Slug / Port）；调用方负责校验。
func Handler(app services.App) http.Handler {
	ui := app.UI
	upstream := &url.URL{Scheme: "http", Host: "127.0.0.1:" + itoa(app.Port)}
	// SelfBase：应用自己已经带上级路径，任何改写都是画蛇添足（会双重加前缀）
	var rw *rewriter
	if !ui.SelfBase {
		rw = newRewriter(ui.Slug, ui.Rewrites)
	}
	prefix := "/" + ui.Slug

	proxy := &httputil.ReverseProxy{
		// DisableCompression 必须开：Go 的 Transport 在请求里没有 Accept-Encoding 时
		// 会自己加上 `gzip` 并透明解压。这里要的是"上游给未压缩正文"，
		// 所以显式关掉 —— 否则上游可能返回 br/zstd，正文里没有明文可改写，
		// 而且**不会有任何报错**（页面白屏，最难查的一类）。
		Transport: &http.Transport{DisableCompression: true},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = upstream.Scheme
			pr.Out.URL.Host = upstream.Host
			// 剥掉子路径前缀：/iopaint/api/v1 → /api/v1
			p := strings.TrimPrefix(pr.In.URL.Path, prefix)
			if p == "" {
				p = "/"
			}
			if ui.Target != "" && ui.Target != "/" {
				p = strings.TrimSuffix(ui.Target, "/") + p
			}
			pr.Out.URL.Path = p
			pr.Out.URL.RawPath = ""
			// 让上游返回未压缩正文，否则下面的改写找不到明文。
			pr.Out.Header.Del("Accept-Encoding")
			// 上游是应用自己，不需要面板的 Cookie；但保留 Host 让应用生成正确的相对链接。
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			// 应用要求的安全头（如 Squoosh 的 COOP/COEP）：必须在**每一条**响应上，
			// 包括 JS/CSS/图片，否则浏览器不会进入 crossOriginIsolated 状态。
			for k, v := range ui.Headers {
				resp.Header.Set(k, v)
			}
			if rw == nil {
				return nil // SelfBase：应用自己处理路径，不碰响应
			}
			// 重定向：把 Location 也拉到子路径下（Kuma 的 / → /dashboard 就是这种）
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rw.location(ui.Slug, loc))
			}
			if resp.StatusCode >= 300 || !rewritableContentType(resp.Header.Get("Content-Type")) {
				return nil
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return err
			}
			out := rw.body(body)
			resp.Body = io.NopCloser(bytes.NewReader(out))
			resp.ContentLength = int64(len(out))
			resp.Header.Set("Content-Length", itoa64(len(out)))
			// 改写后长度变了，不能再用上游的 ETag/校验头（否则浏览器可能拿到旧内容）
			resp.Header.Del("ETag")
			resp.Header.Del("Last-Modified")
			// 长度变了，Range 语义不再成立（否则浏览器可能按旧长度截断）
			resp.Header.Del("Accept-Ranges")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<h1>502 应用没有响应</h1><p>面板无法连接 "+ui.Slug+
				"（127.0.0.1:"+itoa(app.Port)+"）："+esc(err.Error())+"</p>"+
				"<p>这个应用可能没有启动。请到「服务管理」看它的状态，或到「应用市场」重新部署。</p>")
		},
	}
	return proxy
}

// ---------- 小工具 ----------

func itoa(n int) string   { return strconv.Itoa(n) }
func itoa64(n int) string { return strconv.Itoa(n) }
func esc(s string) string { return html.EscapeString(s) }

// Slugs 返回所有"面板需要生成代理"的应用（有 UI、有端口、且安装器不自己写 nginx）。
func Slugs() []services.App {
	var out []services.App
	for _, a := range services.Catalog() {
		if a.UI == nil || a.UI.SelfConf || a.Port <= 0 {
			continue
		}
		out = append(out, a)
	}
	return out
}

// FindApp 按子路径找应用。
func FindApp(slug string) (services.App, bool) {
	for _, a := range services.Catalog() {
		if a.UI != nil && a.UI.Slug == slug {
			return a, true
		}
	}
	return services.App{}, false
}
