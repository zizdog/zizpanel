package web

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/appproxy"
)

// ============================================================================
//  应用界面代理：/<slug>/ → 应用自己的端口
//
//  为什么要面板自己反代，而不是只在 nginx 里写 location：
//  面板有**多个入口**（直连 https://<主机>:8443、nginx 的 http://<主机>/_panel/、
//  以及隧道出去的 https://panel.zizdog.com:8888）。只在 nginx 里写，
//  经隧道访问时就打不开了。放在面板进程里，所有入口自动都通 ——
//  改写规则也只有一份（见 internal/appproxy）。
//
//  安全：只反代到 127.0.0.1 上、且**目录里声明过**的端口，请求方无法指定目标。
//  这些应用本来就直接监听在局域网可访问的端口上（如 8080），
//  所以这里没有扩大暴露面；如果不想让它们经面板访问，把设置里的
//  「应用界面代理」关掉即可（入口也会一起消失）。
// ============================================================================

// registerAppProxy 给每个有界面的应用注册 /<slug>/ 与 /<slug>（补斜杠跳转）。
func (s *Server) registerAppProxy(root *http.ServeMux) {
	if !s.Cfg.AppProxy {
		return
	}
	for _, app := range appproxy.Slugs() {
		slug := app.UI.Slug
		h := appproxy.Handler(app)
		if s.Cfg.AppProxyAuth {
			h = s.requireAppProxyAuth(slug, h)
		}
		root.Handle("/"+slug+"/", h)
		// 不带斜杠时补一个跳转：否则相对路径会解析到站点根（与 /_panel 同理）
		root.Handle("/"+slug, http.RedirectHandler("/"+slug+"/", http.StatusMovedPermanently))
	}
}

// ============================================================================
//  把子路径写进 nginx（让 `http://<主机>/iopaint/` 这种入口也能用）
//
//  面板自己已经反代了 /<slug>/，所以直连面板端口与经隧道的访问都通了。
//  这里解决第三个入口：nginx 的 80 端口 —— 也就是用户真正想要的
//  `http://192.168.1.4/iopaint/`。
//
//  做法与「面板入口」(tools/takeover-panel-entry.sh) 完全同构：
//  在 000-default.conf 里维护一段成对标记的 location 块 ——
//  幂等（先删旧块再插新块）、锚点固定在 `location / {` 之前，
//  写完由 helper 做 nginx -t，不通过自动回滚（priv.WriteVhostAtomic）。
//
//  为什么让 nginx 转发给面板、而不是直接转发给应用：
//  改写规则（/assets/、/api/v1、socket.io、Location 头）只在 internal/appproxy
//  里实现了一份。nginx 直连应用就得把那套规则用 sub_filter 再抄一遍，
//  两处一旦不一致，就会出现"面板入口好、80 端口坏"这种最难查的问题。
// ============================================================================

const (
	appProxyBegin = "# BEGIN ZIZPANEL APP PROXIES"
	appProxyEnd   = "# END ZIZPANEL APP PROXIES"
	// defaultVhost 是承载这些 location 的 vhost（listen 80 default_server）。
	defaultVhost = "000-default"
)

type appEntry struct {
	Slug string
	Name string
	Port int
}

// appProxyBlock 生成整段 location 块。
//
// 纯函数，便于单测：不碰文件系统、不跑 nginx。
func appProxyBlock(entries []appEntry, upstream string) string {
	var b strings.Builder
	b.WriteString(appProxyBegin + "\n")
	b.WriteString("# 以下 location 由 ZizPanel 生成（应用市场 → 打开）。请勿手工编辑，会被下一次生成覆盖。\n")
	b.WriteString("# 它们把 /<slug>/ 转给面板，由面板统一反代到应用端口 ——\n")
	b.WriteString("# 这样改写规则（/assets/、/api/、socket.io、Location）只有一份实现。\n")
	for _, a := range entries {
		b.WriteString("\n")
		fmt.Fprintf(&b, "    # %s（127.0.0.1:%d）\n", a.Name, a.Port)
		fmt.Fprintf(&b, "    location = /%s {\n        return 301 /%s/;\n    }\n", a.Slug, a.Slug)
		fmt.Fprintf(&b, "    location ^~ /%s/ {\n", a.Slug)
		fmt.Fprintf(&b, "        proxy_pass https://%s;\n", upstream)
		b.WriteString("        proxy_ssl_verify off;\n")
		b.WriteString("        proxy_ssl_server_name on;\n")
		b.WriteString("        proxy_http_version 1.1;\n")
		b.WriteString("        proxy_set_header Host $host;\n")
		b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
		b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("        proxy_set_header X-Forwarded-Proto $scheme;\n")
		b.WriteString("        proxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("        proxy_set_header Connection $connection_upgrade;\n")
		b.WriteString("        proxy_read_timeout 300s;\n")
		b.WriteString("        proxy_buffering off;\n")
		b.WriteString("        proxy_cache off;\n")
		b.WriteString("    }\n")
	}
	b.WriteString(appProxyEnd + "\n")
	return b.String()
}

var reLocationRoot = regexp.MustCompile(`^\s*location\s+(=|\^~)?\s*/\s*\{`)

// upsertAppProxyBlock 把新块插进 vhost 内容（先删旧块，保证幂等）。
//
// 插入点与 panel-entry.awk 保持一致：`location / {` 之前；没有就放在
// server 块最后一个 `}` 之前。找不到锚点时报错而不是硬写 ——
// 写到 server 块外面虽然 nginx -t 会拦，但报错信息很难懂。
func upsertAppProxyBlock(content, block string) (string, error) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+64)
	skipping := false
	for _, ln := range lines {
		if strings.Contains(ln, appProxyBegin) {
			skipping = true
			continue
		}
		if skipping {
			if strings.Contains(ln, appProxyEnd) {
				skipping = false
			}
			continue
		}
		out = append(out, ln)
	}
	if skipping {
		return "", fmt.Errorf("原配置里的 %s 没有配对的 %s，请先手工修好再生成", appProxyBegin, appProxyEnd)
	}

	at := -1
	for i, ln := range out {
		if reLocationRoot.MatchString(ln) {
			at = i
			break
		}
	}
	if at < 0 {
		for i := len(out) - 1; i >= 0; i-- {
			if strings.TrimSpace(out[i]) == "}" {
				at = i
				break
			}
		}
	}
	if at < 0 {
		return "", fmt.Errorf("在 %s.conf 里找不到插入点（既没有 location / ，也没有 server 块的结尾）", defaultVhost)
	}

	merged := make([]string, 0, len(out)+64)
	merged = append(merged, out[:at]...)
	merged = append(merged, strings.Split(strings.TrimRight(block, "\n"), "\n")...)
	merged = append(merged, out[at:]...)
	res := strings.Join(merged, "\n")
	if !strings.HasSuffix(res, "\n") {
		res += "\n"
	}
	return res, nil
}

// handleAppProxyApply 生成并写入 nginx 块。
//
// 秒级动作（一次文件读写 + nginx -t + reload），按项目约定**同步**返回，
// 不走任务中心。失败时把 nginx 的报错原样带回（helper 已自动回滚）。
func (s *Server) handleAppProxyApply(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.AppProxy {
		fail(w, http.StatusBadRequest, "「应用界面代理」已在面板设置里关闭，请先开启再生成 nginx 入口")
		return
	}
	entries := appProxyEntries()
	if len(entries) == 0 {
		fail(w, http.StatusInternalServerError, "目录里没有任何带界面的应用")
		return
	}
	// **直接整份重写默认站点**，而不是往现有文件里 upsert 一段。
	//
	// 为什么要改（用户 2026-09-15 实测 duplicate location）：
	// 上一版的默认站点生成器写进去的应用 location **没有 BEGIN/END 标记**，
	// 于是这个按钮找不到旧块、又插一份，nginx 直接语法冲突并整份回滚。
	// 与其在旧内容上做补丁，不如让"默认站点"只有**一个生成函数**
	// （buildDefaultVhost，它内部用的就是带标记的 appProxyBlock）：
	// 内容永远自洽，历史遗留的无标记块也在这一次重写里被清掉。
	res, err := s.callHelper(r.Context(), "vhost-read", defaultVhost)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取 nginx 配置失败: "+err.Error())
		return
	}
	content := ""
	if data, ok := res["data"].(map[string]any); ok {
		content, _ = data["content"].(string)
	}
	if strings.TrimSpace(content) == "" {
		// 读不到也照样能写（helper 会做 nginx -t 与回滚），但先记一笔便于排查
		s.Log.Warn("读不到 %s.conf 的现有内容，将直接整份重写", defaultVhost)
	}
	updated := s.buildDefaultVhost()
	if err := s.writeVhost(r.Context(), defaultVhost, updated); err != nil {
		fail(w, http.StatusInternalServerError, "写入 nginx 配置失败（已自动回滚）: "+err.Error())
		return
	}
	if err := s.nginxReload(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, "配置已写入，但 nginx 重载失败: "+err.Error())
		return
	}
	s.audit(r, "app_proxy_apply", "nginx", fmt.Sprintf("生成 %d 个应用子路径入口", len(entries)), true, "")
	slugs := make([]string, 0, len(entries))
	for _, e := range entries {
		slugs = append(slugs, e.Slug)
	}
	ok(w, map[string]any{"count": len(entries), "slugs": slugs})
}

func appProxyEntries() []appEntry {
	apps := appproxy.Slugs()
	out := make([]appEntry, 0, len(apps))
	for _, a := range apps {
		out = append(out, appEntry{Slug: a.UI.Slug, Name: a.Name, Port: a.Port})
	}
	return out
}

// ============================================================================
//  探测：这些子路径现在到底能不能打开
//
//  为什么必须探测，而不是"生成了就算成功"：子路径对前端绝对路径很敏感，
//  有些应用必须自己设 base path 才能用。用户要的是"能打开"，
//  不是一个看起来配好了、点开却白屏的按钮。
//
//  判定标准（**不是"HTTP 200 就算通"**）：
//    1. 本机直连端口有响应（应用真的在跑）；
//    2. 经代理取到的 HTML 里，它引用的脚本/样式在同源下也能取到 200。
//  第 2 条才是关键：代理没配好时页面照样返回 200，但资源全 404（白屏），
//  只看状态码会得出完全错误的结论 —— 这个项目为此吃过不止一次亏。
// ============================================================================

func (s *Server) handleAppProxyProbe(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.AppProxy {
		ok(w, map[string]any{"enabled": false, "items": []any{}})
		return
	}
	// 面板自己的入口：用请求里的 Host 推断，不能写死 8443 ——
	// 本地调试实例跑在别的端口上，写死会去问一个不属于自己的服务。
	panelBase := "http://" + r.Host
	if r.TLS != nil {
		panelBase = "https://" + r.Host
	}
	items := make([]map[string]any, 0)
	for _, a := range appproxy.Slugs() {
		it := map[string]any{
			"id": a.ID, "slug": a.UI.Slug, "port": a.Port,
			"path": "/" + a.UI.Slug + "/",
		}
		direct := probePage("http://127.0.0.1:"+fmt.Sprint(a.Port)+"/", true)
		it["direct_ok"] = direct.ok
		it["direct_code"] = direct.code

		viaNginx := probePage("http://127.0.0.1/"+a.UI.Slug+"/", false)
		// 自己代理自己时要绕过自签证书校验（面板用的是自签证书）
		viaPanel := probePageInsecure(panelBase + "/" + a.UI.Slug + "/")
		switch {
		case viaNginx.ok && viaNginx.assetsOK:
			it["proxy_ok"], it["proxy_via"], it["proxy_code"] = true, "nginx", viaNginx.code
		case viaPanel.ok && viaPanel.assetsOK:
			it["proxy_ok"], it["proxy_via"], it["proxy_code"] = true, "panel", viaPanel.code
		default:
			it["proxy_ok"], it["proxy_via"], it["proxy_code"] = false, "", viaNginx.code
			// 原因优先级：应用没在跑（最可操作）> 代理侧的问题。
			// 否则会出现"应用根本没启动，界面却说 HTTP 404"这种带偏方向的说法。
			if !direct.ok {
				it["reason"] = "应用没有响应（127.0.0.1:" + fmt.Sprint(a.Port) + "）：" + direct.reason
			} else {
				it["reason"] = firstNonEmpty(viaNginx.reason, viaPanel.reason)
			}
		}
		if a.UI.Note != "" {
			it["note"] = a.UI.Note
		}
		items = append(items, it)
	}
	ok(w, map[string]any{"enabled": true, "items": items})
}

type probeResult struct {
	ok       bool
	assetsOK bool
	code     int
	reason   string
}

func probePage(u string, insecure bool) probeResult {
	if insecure {
		return probePageInsecure(u)
	}
	return probeWith(u, &http.Client{Timeout: 6 * time.Second})
}

func probePageInsecure(u string) probeResult {
	c := &http.Client{
		Timeout: 6 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 只探本机自签面板
		},
	}
	return probeWith(u, c)
}

func probeWith(u string, client *http.Client) probeResult {
	res := probeResult{}
	resp, err := client.Get(u)
	if err != nil {
		res.reason = "连不上：" + err.Error()
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	res.code = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		res.reason = fmt.Sprintf("返回 HTTP %d", resp.StatusCode)
		return res
	}
	res.ok = true
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		res.assetsOK = true
		return res
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		res.reason = "读取页面失败：" + err.Error()
		return res
	}
	base, err := url.Parse(u)
	if err != nil {
		res.assetsOK = true
		return res
	}
	origin := base.Scheme + "://" + base.Host
	refs := refsFromHTML(string(body))
	checked := 0
	for _, ref := range refs {
		if checked >= 3 {
			break
		}
		ar, err := client.Get(origin + ref)
		if err != nil {
			res.reason = "资源取不到：" + ref
			return res
		}
		code := ar.StatusCode
		_ = ar.Body.Close()
		if code != http.StatusOK {
			res.reason = fmt.Sprintf("资源 %s 返回 HTTP %d（前端资源路径没有改写对，页面会白屏）", ref, code)
			return res
		}
		checked++
	}
	res.assetsOK = true
	return res
}

var reAssetRef = regexp.MustCompile(`(?:src|href)="(/[^"]+\.(?:js|css))"`)

// refsFromHTML 取出页面里引用的脚本/样式（绝对路径）。
//
// 只关心 js/css：图片 404 顶多缺个图标，脚本 404 一定是白屏。
func refsFromHTML(html string) []string {
	var out []string
	for _, m := range reAssetRef.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// lanIP 返回本机局域网地址（用于拼"直连端口"的入口）。
//
// 取 en0（Apple Silicon 上有线/无线都是它）；取不到就返回空串，
// 调用方据此不给直连链接（宁可少一个按钮，也不要给一个
// `http://:8080/` 这种打不开的地址）。
func (s *Server) lanIP() string {
	out, err := execCommand(context.Background(), "/usr/sbin/ipconfig", "getifaddr", "en0").Output()
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(out))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// proxyUpstream 是 nginx 要转发到的面板地址。
//
// 不能写死 127.0.0.1:8443：面板的监听地址是可配的（本地调试实例就跑在 18443），
// 写死会让生成出来的 location 指向一个没人听着的端口 —— 表现为
// "配置生成成功了，但 http://主机/iopaint/ 502"。
func (s *Server) proxyUpstream() string {
	addr := strings.TrimSpace(s.Cfg.Listen)
	addr = strings.TrimPrefix(addr, "tcp://")
	if addr == "" {
		return "127.0.0.1:8443"
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return "127.0.0.1" + addr[i:]
	}
	return addr
}

// requireAppProxyAuth 让应用界面也要求先登录面板。
//
// 用户提出的疑问（2026-09-15）：未登录时 File Browser / Squoosh 仍能访问，
// 是否合理？—— Squoosh 完全没有自己的鉴权，File Browser 有登录但登录页本身
// 也是公开的。既然它们挂在面板的端口上、而且面板已经是这台机器的总入口，
// 默认要求先登录面板更符合"面板管到底"的预期（可在设置里关掉，
// 关掉后就等同于直接访问端口，适合把某个应用单独开放出去的场景）。
func (s *Server) requireAppProxyAuth(slug string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.Auth.AuthSession(r.Context(), s.sessionToken(r)); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			entry := s.PanelEntryPath()
			_, _ = fmt.Fprintf(w, `<h1>401 需要先登录面板</h1>
<p>「%s」这个应用界面要求先登录面板（可在「面板设置 → 访问与安全」里关闭这个要求）。</p>
<p>请先打开面板（<a href="%s">%s</a>）登录，再回来访问。</p>`, escHTML(slug), entry, entry)
			return
		}
		next.ServeHTTP(w, r)
	})
}
