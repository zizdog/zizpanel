package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/appproxy"
)

// ============================================================================
//  默认站点（localhost）与 phpMyAdmin 入口
//
//  用户提的两件事（2026-09-14）：
//   1. `http://localhost/_panel/` 与 `http://localhost/phpmyadmin/` 都还暴露着 ——
//      前者把面板放在 80 端口的固定路径上，后者直接把 MySQL 管理页暴露出去。
//      「直接删除」。
//   2. 模仿宝塔：安装时建一个 `www/localhost` 站点，只有一个 index.html。
//
//  于是默认站点被彻底重写：
//    · root 指向 `~/www/localhost`（只有一个占位 index.html）
//    · **不再有** `/_panel` 入口（面板只在 `:8443/<安全后缀>/` 上）
//    · phpMyAdmin 的 location 仍然存在，但**只允许 127.0.0.1** ——
//      它只服务于"面板自己反代过去"这一条路径，外部直接访问一律 403。
//      面板那条路径要求先登录（见 phpMyAdminProxy）。
//    · 应用界面代理（`/iopaint/` 等）照旧保留：它们是给用户浏览器用的。
// ============================================================================

const localhostIndexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>localhost</title>
  <style>
    body { font-family: -apple-system, "PingFang SC", sans-serif; margin: 15vh auto; max-width: 32rem;
           padding: 0 1.5rem; color: #222; line-height: 1.8; }
    code { background: #f2f2f4; padding: .1rem .35rem; border-radius: .25rem; }
    .muted { color: #888; font-size: .9rem; }
  </style>
</head>
<body>
  <h1>localhost</h1>
  <p>这是本机 Web 服务的默认站点。它只放这一张占位页，用来接住没匹配到具体域名的请求。</p>
  <p class="muted">面板不在这个端口上：请用面板自己的地址（HTTPS + 安全后缀）访问。<br>
  需要管理数据库？在面板里打开 phpMyAdmin（需先登录面板）。</p>
</body>
</html>
`

// handleDefaultSiteApply 整理默认站点：建占位页 + 重写 000-default.conf。
//
// 秒级动作（写两个文件 + nginx -t + reload），按项目约定同步返回。
func (s *Server) handleDefaultSiteApply(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.Cfg.WWWRoot, "localhost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, "创建 "+dir+" 失败: "+err.Error())
		return
	}
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); os.IsNotExist(err) {
		if err := os.WriteFile(index, []byte(localhostIndexHTML), 0o644); err != nil {
			fail(w, http.StatusInternalServerError, "写入 index.html 失败: "+err.Error())
			return
		}
	}
	// 归属交给面板运行用户，别留下 root 属主的站点目录
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		if err := chownTreeTo(dir, s.Cfg.User); err != nil {
			s.Log.Warn("调整 %s 归属失败（站点可能仍是 root 属主）：%v", dir, err)
		}
	}

	content := s.buildDefaultVhost()
	if err := s.writeVhost(r.Context(), "000-default", content); err != nil {
		fail(w, http.StatusInternalServerError, "写入默认站点配置失败（已自动回滚）: "+err.Error())
		return
	}
	// 日志目录的归属必须在 reload 之前修好。
	//
	// 真机事故（mini，2026-09-14）：vhost 里的 access_log 指向 ~/www/_logs/，
	// 而那个目录被之前的操作留成了 root 属主 —— nginx 的 worker 以普通用户运行，
	// 打不开日志文件，于是 **reload 时报 [emerg] 却仍然返回退出码 0**：
	// 面板以为成功、nginx 继续用旧配置，表现成"点了没反应"。
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		_ = chownTreeTo(filepath.Join(s.Cfg.WWWRoot, "_logs"), s.Cfg.User)
	}
	if err := s.nginxReload(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, "配置已写入，但 nginx 重载失败: "+err.Error())
		return
	}
	// reload 命令成功 ≠ 新配置生效：nginx 读配置失败时会在错误日志里写 [emerg]
	// 而 `-s reload` 依然返回 0。所以这里**按真实结果复核**：本机首页应当能拿到
	// 我们刚写的那张占位页。拿不到就如实报错，并给出最可能的原因。
	if code, body := fetchLocal("http://127.0.0.1/"); code != http.StatusOK || !strings.Contains(body, "这是本机 Web 服务的默认站点") {
		fail(w, http.StatusInternalServerError, fmt.Sprintf(
			"配置已写入，但 nginx 没有真正生效（本机首页返回 %d）。"+
				"最常见的原因是 %s 的日志文件归属/权限不对（nginx worker 打不开 access_log 时"+
				"reload 会失败但退出码仍是 0）。请到「日志中心 → nginx error_log」看 [emerg] 行。",
			code, filepath.Join(s.Cfg.WWWRoot, "_logs")))
		return
	}
	s.audit(r, "default_site_apply", "nginx", "整理默认站点（去 /_panel、phpMyAdmin 限本机）", true, "")
	ok(w, map[string]any{
		"dir":     dir,
		"index":   index,
		"message": "默认站点已指向 " + dir + "；面板入口已从 80 端口移除；phpMyAdmin 仅允许本机（须登录面板访问）",
	})
}

// buildDefaultVhost 生成完整的默认站点配置（幂等：内容只由代码决定）。
func (s *Server) buildDefaultVhost() string {
	wwwRoot := s.Cfg.WWWRoot
	localhostDir := filepath.Join(wwwRoot, "localhost")
	defaultDir := filepath.Join(wwwRoot, "_default") // 仅作为 error_page 的落点，不再对外
	phpmyadmin := filepath.Join(s.Cfg.BrewPrefix, "share", "phpmyadmin")
	fpm := filepath.Join(s.Cfg.BrewPrefix, "etc", "nginx", "includes", "php-fpm.conf")

	var b strings.Builder
	b.WriteString("# 默认站点（由 ZizPanel 生成 —— 请勿手工编辑，面板会整份重写）\n")
	b.WriteString("# 作用：接住没匹配到具体域名的请求，并提供两个受控入口：\n")
	b.WriteString("#   · /phpmyadmin/  **只允许 127.0.0.1**：面板登录后反代过来才能用，外部直连 403\n")
	b.WriteString("#   · /<应用>/      应用界面代理（见「应用市场 → 打开」），转发给面板统一改写\n")
	b.WriteString("# 面板本身**不在这里**：它只监听自己的端口 + 安全后缀。\n")
	b.WriteString("server {\n")
	b.WriteString("    listen       80 default_server;\n")
	b.WriteString("    server_name  _;\n\n")
	fmt.Fprintf(&b, "    root   %s;\n", localhostDir)
	b.WriteString("    index  index.html index.php;\n\n")
	fmt.Fprintf(&b, "    access_log  %s/localhost.access.log;\n", filepath.Join(wwwRoot, "_logs"))
	fmt.Fprintf(&b, "    error_log   %s/localhost.error.log warn;\n\n", filepath.Join(wwwRoot, "_logs"))

	// 应用界面代理：**用与应用市场那个按钮完全相同的生成函数**（含 BEGIN/END 标记）。
	//
	// 为什么必须带标记：不带的话，应用市场点「生成 nginx 入口」时
	// upsertAppProxyBlock 找不到旧块，于是**再插一份** → nginx 报
	// `duplicate location "/filebrowser"`，整份配置回滚（用户 2026-09-15 实测）。
	// 两处生成同一份内容、同一套标记，才谈得上幂等。
	entries := appProxyEntries()
	if len(entries) > 0 {
		b.WriteString(appProxyBlock(entries, s.proxyUpstream()))
		b.WriteString("\n")
	}

	b.WriteString("    location / {\n        try_files $uri $uri/ =404;\n    }\n\n")
	fmt.Fprintf(&b, "    location ~ \\.php$ {\n        include %s;\n    }\n\n", fpm)

	// phpMyAdmin：仍然由 nginx + php-fpm 直接服务（它是个 PHP 站点，不是反代），
	// 但**只允许本机**：面板登录后把请求反代到这里，外部直连一律 403。
	//
	// 注意 location 的写法：**不能带尾斜杠**。`alias` 与 `try_files` 一起用时，
	// nginx 会用 `$uri` 去拼 alias（而不是"剥掉 location 前缀后的部分"），
	// 写成 `location ^~ /phpmyadmin/` 会去找 `<alias>/phpmyadmin/index.php`，
	// 落到目录索引上 → **403 directory index is forbidden**（真机踩过）。
	// 下面这种"无尾斜杠 location + alias + try_files 回落到 /phpmyadmin/index.php"
	// 是这台机器上一直跑通的写法。
	b.WriteString("    # phpMyAdmin：只允许本机（面板登录后反代）—— 外部直连一律 403\n")
	b.WriteString("    location = /phpmyadmin {\n        return 301 /phpmyadmin/;\n    }\n")
	b.WriteString("    location ^~ /phpmyadmin {\n")
	b.WriteString("        allow 127.0.0.1;\n")
	b.WriteString("        allow ::1;\n")
	b.WriteString("        deny all;\n")
	b.WriteString("        alias " + phpmyadmin + ";\n")
	b.WriteString("        index index.php;\n")
	b.WriteString("        try_files $uri $uri/ /phpmyadmin/index.php$is_args$args;\n\n")
	b.WriteString("        location ~ \\.php$ {\n")
	fmt.Fprintf(&b, "            include %s;\n", fpm)
	b.WriteString("        }\n")
	b.WriteString("    }\n")
	_ = defaultDir
	b.WriteString("}\n")
	return b.String()
}

// ---------- phpMyAdmin：必须登录面板才能访问 ----------

// phpAdminProxy 把 `/<suffix>/phpmyadmin/` 反代到本机 nginx 的 phpMyAdmin。
//
// 为什么这么绕（面板 → nginx → php-fpm），而不是让面板直接跑 PHP：
//
//	· phpMyAdmin 是一个 PHP 应用，必须有 php-fpm 与 nginx 的 fastcgi 环境；
//	· 而 80 端口上的那个 location 已经被收紧到只允许 127.0.0.1，
//	  所以"能进 phpMyAdmin 的路"只剩下面板这一条，而面板这条要求登录。
//
// 用户的要求就是这一句："phpmyadmin 不能直接访问，必须已经登录面板才可以访问。"
func (s *Server) phpAdminProxy() http.Handler {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:80"}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			// 路径原样带过去（nginx 那个 location 就是 /phpmyadmin/）
			pr.Out.Host = target.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `<h1>502 phpMyAdmin 不可用</h1><p>面板无法连接本机 nginx 上的 phpMyAdmin：%s</p>
<p>常见原因：nginx 没在跑，或 phpMyAdmin 还没安装（应用市场 → phpMyAdmin）。</p>`, escHTML(err.Error()))
		},
	}
}

// handlePhpMyAdmin 是入口：未登录直接拦住（不是重定向，避免暴露面板地址）。
func (s *Server) handlePhpMyAdmin(w http.ResponseWriter, r *http.Request) {
	// 直接查会话（不用 requireAuth：这里要的不是 JSON 错误，而是一张
	// 告诉用户"先去登录面板"的说明页）
	if _, err := s.Auth.AuthSession(r.Context(), s.sessionToken(r)); err != nil {
		s.phpAdminLoginRequired(w, r)
		return
	}
	s.phpAdminProxy().ServeHTTP(w, r)
}

// phpAdminLoginRequired 给未登录用户一张说明页：说清"要先去面板登录"，
// 并给出登录地址（只有知道安全后缀的人才看得到这一页）。
func (s *Server) phpAdminLoginRequired(w http.ResponseWriter, r *http.Request) {
	entry := s.PanelEntryPath()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `<h1>401 需要先登录面板</h1>
<p>phpMyAdmin 只对已登录的面板用户开放。</p>
<p>请先打开面板（<a href="%s">%s</a>）登录，再从「应用市场 → phpMyAdmin → 打开」进入。</p>`,
		entry, entry)
}

func escHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// chownTreeTo 递归把目录归属改成指定用户（只用于我们自己创建的站点目录）。
func chownTreeTo(path, user string) error {
	uid, err := strconv.Atoi(strings.TrimSpace(runOutputCmd("/usr/bin/id", "-u", user)))
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(strings.TrimSpace(runOutputCmd("/usr/bin/id", "-g", user)))
	if err != nil {
		return err
	}
	return filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个条目失败不影响其余
		}
		_ = os.Chown(p, uid, gid)
		return nil
	})
}

func runOutputCmd(name string, args ...string) string {
	out, _ := execCommand(context.Background(), name, args...).Output()
	return string(out)
}

var _ = appproxy.Slugs

// fetchLocal 取本机 nginx 上的一个页面（用于复核"配置真的生效了"）。
func fetchLocal(u string) (int, string) {
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, string(b)
}
