package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  aria2 的网页界面（AriaNg，内置在面板里）与 RPC 代理
//
//  为什么界面由**面板**托管，而不是让应用自带一个 web 服务：
//    · aria2 本体只提供 JSON-RPC（6800），它自己不会发静态文件；
//    · 用户点名"不要建站" ⇒ 不走站点轨（nginx + 目录 + PHP 那一套）；
//    · AriaNg 的资源引用全是相对路径，挂在 /aria/ 下不需要任何改写，
//      而面板是唯一知道"用户登没登录"的地方 —— 界面与 RPC 都挂在这里，
//      浏览器只需要面板这一个入口（不需要把 6800 暴露到局域网）。
//
//  两个入口：
//    · GET  /aria/**          → 内置的 AriaNg 静态资源（要登录）
//    · POST /aria/jsonrpc     → 转发给 127.0.0.1:6800/jsonrpc（要登录）
//      （另外挂一个 POST /jsonrpc：AriaNg 的默认 interface 就是 jsonrpc，
//        挂上它，"RPC 界面"那一栏可以保持默认值）
//
//  鉴权说明（别顺手删）：
//    · 界面与应用界面同一策略（AppProxyAuth 打开时要求登录，见 requireAppProxyAuth）；
//    · RPC 是**控制面**（能删文件、能加下载），无论 AppProxyAuth 怎么配都要求登录；
//    · RPC 只收 application/json，并拒绝跨站来源（Sec-Fetch-Site）——
//      跨站表单提交做不到这个 Content-Type，等价于 CSRF 防线；
//      所以这里**不**走面板的 CSRF 双提交（AriaNg 是第三方前端，不会带那个头）。
// ============================================================================

// aria2AssetDir 是内置 AriaNg 资源在 assets/ 下的目录名（= assets/ariang）。
// 注意它**不等于** URL 里的 slug（/aria/）：目录名跟着上游项目名，路径跟着用户看得懂的短名。
const aria2AssetDir = "assets/ariang"

// aria2RPCUpstream 是 RPC 的本机地址（RPC 绑 0.0.0.0，见 services/aria2.go）。
// 做成变量是为了单测能把它指到假上游（绝不在单测里连真实 6800）。
var aria2RPCUpstream = func() *url.URL {
	return &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", services.Aria2RPCPort), Path: "/jsonrpc"}
}

// registerAria2UI 把界面与 RPC 代理挂到 mux 上。
//
// 两个 mux 都要挂（调用方传两次）：
//
//	· 外层 mux：用户把 /aria/ 直接存书签时要能用（与应用界面同层，不要求先知道安全后缀）；
//	· 内层 mux（安全后缀之后）：前端的「打开」用的是 panelPath('aria/')，也就是
//	  `/<后缀>/aria/` —— SelfConf 应用不走 app-proxy，这条路径必须由我们自己接住，
//	  否则点「打开」会落到 SPA 回落页（看着像"应用坏了"，2026-09-17 的坑 214 同形）。
func (s *Server) registerAria2UI(root *http.ServeMux) {
	slug := services.Aria2Slug
	var ui http.Handler = http.HandlerFunc(s.handleAria2UI)
	if s.Cfg.AppProxyAuth {
		ui = s.requireAppProxyAuth(slug, ui)
	}
	root.Handle("/"+slug+"/", ui)
	root.Handle("/"+slug, http.RedirectHandler("/"+slug+"/", http.StatusMovedPermanently))

	rpc := http.HandlerFunc(s.handleAria2RPC)
	root.Handle("POST /"+slug+"/jsonrpc", rpc)
	root.Handle("POST /jsonrpc", rpc)
}

// handleAria2UI 提供内置的 AriaNg。
func (s *Server) handleAria2UI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	ui := s.aria2UI
	if ui == nil {
		writeErr(w, http.StatusInternalServerError, "面板里没有内置的 aria2 界面资源")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/"+services.Aria2Slug+"/")
	if rest == "" || strings.Contains(rest, "..") {
		rest = "index.html"
	}
	b, err := fs.ReadFile(ui, rest)
	if err != nil {
		// AriaNg 内部路由走 hash（#!/...），不会打到服务端；其它未知路径同样回首页。
		rest, b, err = "index.html", nil, nil
		b, err = fs.ReadFile(ui, rest)
		if err != nil {
			writeErr(w, http.StatusNotFound, "界面资源不存在")
			return
		}
	}
	if rest == "index.html" {
		// 缓存策略与面板自己的前端一致：index.html 不缓存，其余按类型给短缓存。
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(s.aria2IndexHTML(string(b))))
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", contentTypeByExt(rest))
	_, _ = w.Write(b)
}

// aria2IndexHTML 往 AriaNg 的 index.html 里注入一段"自动配好 RPC"的脚本。
//
// 为什么要注入（用户体验的全部意义所在）：AriaNg 默认连 http://localhost:6800，
// 而面板仍让浏览器走 /aria/jsonrpc 同源入口 —— 不注入的话，
// 用户打开界面看到的是"未连接"，还得自己去设置页填主机/端口/密钥。
//
// 注入的行为（刻意保守，别加码）：
//
//	· 从来没配过（没有 AriaNg.Options）→ 用 AriaNg 自己的
//	  `#!/settings/rpc/set/...` 路由配好（它会保存并跳到任务列表）；
//	· 配的是**本面板**（host/port 一致）但密钥变了 → 重新配一次（密钥在重装时
//	  会变的情况：用户删了 ~/aria 再装）；
//	· 用户自己在 AriaNg 里配过别的服务器（host 不同）→ **一个字都不动**。
//
// 密钥没有 base64 之外的处理：AriaNg 的那个路由要求 secret 是 base64url。
func (s *Server) aria2IndexHTML(html string) string {
	conf := services.Aria2ConfPath(s.Cfg.UserHome)
	secret := services.ReadAria2Secret(conf)
	if strings.TrimSpace(secret) == "" {
		// 还没装/配置读不到：明确说清，别丢一个"连不上"的空白界面给用户。
		return aria2NotInstalledHTML
	}
	enc := base64.RawURLEncoding.EncodeToString([]byte(secret))
	script := fmt.Sprintf(`<script>
(function(){
  try {
    var secret = %q;
    var proto = (location.protocol || "http:").replace(":", "");
    var port = location.port || (proto === "https" ? "443" : "80");
    var cur = null;
    try { cur = JSON.parse(localStorage.getItem("AriaNg.Options") || "null"); } catch (e) { cur = null; }
    var norm = function(v){ return String(v == null ? "" : v).replace(/=+$/, "").replace(/-/g, "+").replace(/_/g, "/"); };
    var sameHost = !!cur && String(cur.rpcHost || "") === location.hostname && String(cur.rpcPort || "") === String(port);
    var sameSecret = sameHost && (norm(cur.secret) === norm(secret) || norm(cur.secret) === norm(btoa(secret)));
    if (!cur || (sameHost && !sameSecret)) {
      location.hash = "#!/settings/rpc/set/" + proto + "/" + location.hostname + "/" + port + "/jsonrpc/" + secret;
    }
  } catch (e) {}
})();
</script>
`, enc)
	// 插在 </body> 之前：AriaNg 的脚本都在 body 里，而 angular 是在
	// DOMContentLoaded 时才 bootstrap —— 那时 hash 已经设好了。
	if i := strings.LastIndex(html, "</body>"); i >= 0 {
		return html[:i] + script + html[i:]
	}
	return html + script
}

// aria2NotInstalledHTML 是"界面在、服务不在"时给的说明页。
//
// 为什么要专门做一张页（而不是让它显示"未连接"）：这条路用户一定会撞到
// —— 首次安装前从浏览器收藏夹打开 /aria/、或者卸载之后。空白界面 + "未连接"
// 会让人以为是面板坏了。
const aria2NotInstalledHTML = `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>aria2 还没有安装</title>
<style>body{font:14px/1.8 -apple-system,BlinkMacSystemFont,"PingFang SC",sans-serif;margin:0;padding:48px 24px;background:#f6f7f9;color:#222}
main{max-width:560px;margin:0 auto;background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:24px}
h1{font-size:18px;margin:0 0 12px}code{background:#f3f4f6;padding:2px 6px;border-radius:4px}</style></head>
<body><main>
<h1>aria2 还没有安装（或配置读不到）</h1>
<p>这个页面是面板托管的下载界面（AriaNg）。它需要本机的 aria2 服务在跑。</p>
<p>到「应用 → 应用市场 → aria2（下载器）」点「安装」即可；装好之后刷新本页就能用。</p>
<p>如果你刚卸载了它，或者手工删过 <code>~/aria/aria2.conf</code>，重新安装会把服务与配置一起建回来。</p>
</main></body></html>`

// handleAria2RPC 把浏览器的 JSON-RPC 请求转发给回环上的 aria2。
//
// 鉴权：**无条件**要求面板会话（这是控制面）。CSRF 用两道等价防线替代
// 面板的双提交令牌：只收 application/json + 拒绝跨站来源 —— 见文件头说明。
func (s *Server) handleAria2RPC(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Auth.AuthSession(r.Context(), s.sessionToken(r)); err != nil {
		writeErr(w, http.StatusUnauthorized, "未登录或会话已过期：请先在面板里登录，再刷新下载界面")
		return
	}
	s.aria2RPCProxy(w, r)
}

// handleAria2RPCLoopback 是 **AriaNg 独立端口**上的 RPC 代理：不要求面板会话。
//
// 边界是**自带 access-control 口令**（端口绑 0.0.0.0，同网段可直连），
// 与导航页独立端口同一取舍；RPC 本身仍由 aria2 的 `rpc-secret` 保护。
// 少了它，用户把独立端口反代出去后 AriaNg 依然连不上（浏览器没有面板 Cookie）。
func (s *Server) handleAria2RPCLoopback(w http.ResponseWriter, r *http.Request) {
	s.aria2RPCProxy(w, r)
}

// ariaSlugPath 返回界面 slug（不带斜杠）。抽出来是为了让"面板内 /aria/"与
// "独立端口根路径"两处引用同一个常量，避免改名时漏掉一处。
func ariaSlugPath() string { return services.Aria2Slug }

// aria2RPCTimeout 是面板等 aria2 RPC 应答的上限。
//
// 没有它，**卡死的 aria2 会让界面永远停在"连接中…"**：真机撞到过 —— aria2 写
// `.aria2` 控制文件抛异常后进程还在、端口还在听（lsof 是它、launchd 也 running），
// 但 HTTP/RPC 一个字都不回（连 `GET /` 都超时），于是面板的代理跟着一直等，
// 用户既看不到错也点不动。超时后回 502 + 能照做的提示。
var aria2RPCTimeout = 8 * time.Second

// aria2RPCProxy 是两条入口共用的代理实现（是否要求会话由调用方先判）。
func (s *Server) aria2RPCProxy(w http.ResponseWriter, r *http.Request) {
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" &&
		site != "same-origin" && site != "none" {
		writeErr(w, http.StatusForbidden, "拒绝跨站来源的 RPC 请求")
		return
	}
	if ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "aria2 的 RPC 只接受 application/json")
		return
	}
	up := aria2RPCUpstream()
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = up.Scheme
			req.URL.Host = up.Host
			req.URL.Path = up.Path
			req.Host = up.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			where := "127.0.0.1:" + fmt.Sprint(services.Aria2RPCPort)
			if aria2RPCTimedOut(r.Context(), err) {
				writeErr(w, http.StatusBadGateway,
					"aria2 在 "+fmt.Sprint(int(aria2RPCTimeout.Seconds()))+" 秒内没有应答（"+where+
						"）：进程可能还在跑但已经卡死；到「应用 → 已安装 → aria2」点一次「重启」")
				return
			}
			writeErr(w, http.StatusBadGateway,
				"连不上 aria2（"+where+"）："+err.Error()+
					"；到「应用 → 已安装 → aria2」点一次「启动」再试")
		},
	}
	ctx, cancel := context.WithTimeout(r.Context(), aria2RPCTimeout)
	defer cancel()
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// aria2RPCTimedOut 判断这次失败是不是"上游不应答"（而不是拒连/配置错）。
// ReverseProxy 把上下文取消包装成多层错误，errors.Is 与文案两条判据都要留。
func aria2RPCTimedOut(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "Client.Timeout")
}
