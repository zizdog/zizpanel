package web

import (
	"context"
	"errors"
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
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
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

// handleDefaultSiteApply 整理默认站点：建占位页 + 重写 000-default.conf。
//
// 秒级动作（写两个文件 + nginx -t + reload），按项目约定同步返回。
//
// 与启动时的自动创建共用 createDefaultSite：**同一套实现**，包括"没有 nginx
// 就不要假装能建"这条判断（没有 nginx 时返回 409 + 人话，界面据此给「只安装 Nginx」）。
func (s *Server) handleDefaultSiteApply(w http.ResponseWriter, r *http.Request) {
	if present, why := s.nginxPresent(); !present {
		fail(w, http.StatusConflict, "还不能创建默认站点："+why+
			"。默认站点要由 nginx 监听 80 端口才有意义 —— 可以先只装 Nginx"+
			"（不需要 PHP 与 MySQL），装好后这里的「创建默认站点」就能用了。")
		return
	}
	if err := s.createDefaultSite(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := s.readDefaultSiteState()
	st.Attempted, st.Applied, st.NginxPresent = true, true, true
	st.IndexPath = filepath.Join(s.Cfg.WWWRoot, "localhost", "index.html")
	st.VhostPath = filepath.Join(s.Cfg.VhostDir, "000-default.conf")
	st.URL = "http://" + s.lanIP() + "/"
	st.Error = ""
	st.At = time.Now().Format(time.RFC3339)
	s.writeDefaultSiteState(st)

	s.audit(r, "default_site_apply", "nginx", "整理默认站点（去 /_panel、phpMyAdmin 限本机）", true, "")
	ok(w, map[string]any{
		"dir":   filepath.Dir(st.IndexPath),
		"index": st.IndexPath,
		"url":   st.URL,
		"message": "默认站点已指向 " + filepath.Dir(st.IndexPath) + "；面板入口已从 80 端口移除；" +
			"phpMyAdmin 仅允许本机（须登录面板访问）",
	})
}

// defaultProbeFn / defaultVerifyWait / defaultVerifyEvery 是默认站点复核的
// 可注入步骤与轮询窗口（与 applySite 的 siteVerifyWait 同一套思路）。
//
// 为什么默认站点也要轮询：它和站点 vhost 一样是"写盘 → reload → 请求级复核"，
// `nginx -s reload` 同样是异步的 —— 写完立刻取首页，很可能还是旧的占位页/旧配置，
// 一次性判定会把一次正常重载误报成失败。
var (
	defaultProbeFn     = fetchLocal
	defaultVerifyWait  = 6 * time.Second
	defaultVerifyEvery = 200 * time.Millisecond
)

// applyDefaultVhost 生成并应用一份**完整的**默认站点配置。
//
// 复用同一套写盘通道（helper 写 + nginx -t + 失败回滚 + reload + 真实复核），
// 不新造第二套"写 nginx 配置"的实现。
//
// 失败时撤销本次写入（还原旧的 000-default.conf，或删掉本次新建的），
// 避免"接口报错、磁盘上却留着一份没生效的新默认站点"。
func (s *Server) applyDefaultVhost(ctx context.Context) error {
	content := s.buildDefaultVhost()
	snap := s.snapshotVhost("000-default")
	if err := siteWriteVhostFn(s, ctx, "000-default", content); err != nil {
		return fmt.Errorf("写入默认站点配置失败（已自动回滚）: %w", err)
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
	if err := siteReloadFn(s, ctx); err != nil {
		return s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn,
			fmt.Errorf("配置已写入，但 nginx 重载失败: %w", err))
	}
	// reload 命令成功 ≠ 新配置生效：nginx 读配置失败时会在错误日志里写 [emerg]
	// 而 `-s reload` 依然返回 0。所以这里**轮询**复核：本机首页应当能拿到
	// 我们刚写的那张占位页。等满窗口仍拿不到才判失败，并如实给出排查检查点。
	deadline := time.Now().Add(defaultVerifyWait)
	tries := 0
	var lastCode int
	for {
		tries++
		var body string
		lastCode, body = defaultProbeFn("http://127.0.0.1/")
		// 必须同时满足"200"和"内容是我们刚写的占位页"：200 也可能来自别的
		// 默认 server（内容对不上就说明这份配置没被加载）。
		if lastCode == http.StatusOK && strings.Contains(body, sites.LocalhostIndexMarker) {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(defaultVerifyEvery):
			continue
		}
		break
	}
	return s.rollbackVhostWrite(ctx, snap, siteWriteVhostFn, siteReloadFn, s.defaultVhostTimeoutError(lastCode, tries))
}

// defaultVhostTimeoutError 是"等了整个窗口默认站点仍没生效"时的错误。
func (s *Server) defaultVhostTimeoutError(lastCode, tries int) error {
	logDir := filepath.Join(s.Cfg.WWWRoot, "_logs")
	vhostPath := filepath.Join(s.Cfg.VhostDir, "000-default.conf")
	pidPath := filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx.pid")
	errLog := filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log")
	msg := fmt.Sprintf("默认站点配置已写入，nginx 重载也已发出，但 %s 内新配置仍未生效"+
		"（共探测 %d 次，最后一次本机首页返回 %d）。请依次检查："+
		"① `nginx -t` 是否通过；"+
		"② nginx 进程与权限：`ps -o user,pid,command -p $(cat %s)` 里的用户能否读 %s 与日志目录 %s；"+
		"③ vhost 是否在 include 目录：%s 应存在且被 %s 的 include 覆盖；"+
		"④ 全局 error_log 末几行：`tail -n 20 %s`（面板「日志中心 → nginx 主错误日志」）看有没有 [emerg]。",
		humanWait(defaultVerifyWait), tries, lastCode, pidPath, vhostPath, logDir,
		vhostPath, s.Cfg.NginxConf, errLog)
	if tail := s.nginxErrorLogTail(5); tail != "" {
		msg += "\n（nginx error_log 末几行）\n" + tail
	}
	return errors.New(msg)
}

// buildDefaultVhost 生成完整的默认站点配置（幂等：内容只由代码决定）。
func (s *Server) buildDefaultVhost() string {
	wwwRoot := s.Cfg.WWWRoot
	localhostDir := filepath.Join(wwwRoot, "localhost")
	defaultDir := filepath.Join(wwwRoot, "_default") // 仅作为 error_page 的落点，不再对外
	phpmyadmin := filepath.Join(s.Cfg.BrewPrefix, "share", "phpmyadmin")

	// PHP 端点必须**按机器/版本解析**，不能 include 一份写死 127.0.0.1:9000
	// 的片段：面板的多版本设计里每个 php@x.y 听自己专属的 Unix socket，
	// 9000 上根本没人听 —— 真机现象是"点一次「整理默认站点」，默认站点从
	// 可用变成 502"。端点解析与参数文本都来自 internal/sites（与一键 LNMP、
	// phpMyAdmin 入口共用同一份，见 sites.PreferredFastCGIPass / FastCGIParamsBlock）。
	pass, phpVersion := sites.PreferredFastCGIPass(s.Cfg.BrewPrefix)
	phpParams := sites.FastCGIParamsBlock()
	lim := s.uploadLimits()

	var b strings.Builder
	writePHPLocationBody := func(indent string) {
		if pass == "" {
			b.WriteString(indent + "# 本机没有可用 PHP 端点：默认站点暂不解析 PHP。\n")
			b.WriteString(indent + "# 到「网站管理 → 🐘 PHP 环境」修好端点后，再点一次「整理默认站点」。\n")
			return
		}
		b.WriteString(indent + "fastcgi_pass " + pass + ";\n")
		for _, line := range strings.Split(phpParams, "\n") {
			b.WriteString(indent + line + "\n")
		}
	}

	// 头两行是**统一标记**（D44）：services 侧靠它判断"这份默认站点是不是面板的"。
	// 以前两份生成器各写各的标记，互相认不出来 —— 现在都写 services 导出的这一份。
	b.WriteString(services.DefaultVhostMarker + "\n")
	b.WriteString(services.DefaultVhostKindFull + "\n")
	b.WriteString("# 作用：接住没匹配到具体域名的请求，并提供两个受控入口：\n")
	b.WriteString("#   · /phpmyadmin/  **只允许 127.0.0.1**：面板登录后反代过来才能用，外部直连 403\n")
	b.WriteString("#   · /<应用>/      应用界面代理（见「应用市场 → 打开」），转发给面板统一改写\n")
	b.WriteString("# 面板本身**不在这里**：它只监听自己的端口 + 安全后缀。\n")
	b.WriteString("server {\n")
	b.WriteString("    listen       80 default_server;\n")
	b.WriteString("    server_name  _;\n\n")
	fmt.Fprintf(&b, "    root   %s;\n", localhostDir)
	// index.php 放在前面：用户要求"默认站点有一个 index.php"，而"整理默认站点"
	// 会把这份模板整份重写 —— 顺序写反了就会让 PHP 版默认站点退回旧的静态 index.html。
	// 用户 2026-09-18 明确要求："我从没要求默认站点要能跑 PHP！我一直说的是
	// 默认站点是纯静态的，只需要一个 index.html！" —— 所以 index.html 排在前面，
	// 而且面板**不再**往默认站点里放 index.php（见 phpmyadmin.go 里的说明）。
	// PHP 只在 phpMyAdmin 那个 location 里用（那是它自己的事，与首页无关）。
	b.WriteString("    index  index.html index.php;\n\n")
	fmt.Fprintf(&b, "    access_log  %s/localhost.access.log;\n", filepath.Join(wwwRoot, "_logs"))
	fmt.Fprintf(&b, "    error_log   %s/localhost.error.log warn;\n\n", filepath.Join(wwwRoot, "_logs"))
	// 请求体上限：nginx 出厂只有 1m，用户导入几十 MB 的 SQL 会先撞 413
	// （请求根本到不了 PHP）。默认 512m，可在「面板设置 → 上传与执行限制」里改。
	b.WriteString("    client_max_body_size " + lim.ClientMaxBodySize + ";\n\n")

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
	b.WriteString("    # PHP：" + phpVersion + " 的专属 FastCGI 端点（多版本共存，不写死 9000）\n")
	b.WriteString("    location ~ \\.php$ {\n")
	writePHPLocationBody("        ")
	b.WriteString("    }\n\n")

	// phpMyAdmin：仍然由 nginx + php-fpm 直接服务（它是个 PHP 站点，不是反代），
	// 但**只允许本机**：面板登录后把请求反代到这里，外部直连一律 403。
	//
	// 入口内容**不在这里手写**：收敛到 services.PMAEntryBlock（唯一实现）——
	// 与一键 LNMP 补的最小默认站点、以及"往已有默认站点插入"共用同一份，
	// 安全指令（allow/deny/301）与卸载标记不可能再漂移（D10/D14）。
	//
	// 注意 location 的写法在生成器里：**不能带尾斜杠**。`alias` 与 `try_files`
	// 一起用时，nginx 会用 `$uri` 去拼 alias（而不是"剥掉 location 前缀后的部分"），
	// 写成 `location ^~ /phpmyadmin/` 会去找 `<alias>/phpmyadmin/index.php`，
	// 落到目录索引上 → **403 directory index is forbidden**（真机踩过）。
	// 生成器用的是这台机器上一直跑通的"无尾斜杠 location + alias + try_files 回落"。
	b.WriteString(services.PMAEntryBlock(services.PMAEntryOptions{
		Share:             phpmyadmin,
		FastCGIPass:       pass,
		PHPVersion:        phpVersion,
		ClientMaxBodySize: lim.ClientMaxBodySize,
	}))
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
	return s.phpAdminProxyTo(&url.URL{Scheme: "http", Host: "127.0.0.1:80"})
}

// phpAdminProxyTo 是上面那个反代的本体（目标可注入，便于单测）。
func (s *Server) phpAdminProxyTo(target *url.URL) http.Handler {
	// 面板自己的入口前缀（例如 "/jab5c63"）：phpMyAdmin 的 Cookie 路径必须改写到它之下。
	// 后缀就是面板入口那一段（见 config.PanelSuffix 的注释）。
	prefix := ""
	if suf := strings.Trim(s.Cfg.PanelSuffix, "/"); suf != "" {
		prefix = "/" + suf
	}
	return &httputil.ReverseProxy{
		// 边收边转发（默认是"缓冲后再写"）：phpMyAdmin 的页面在慢机器/慢库上
		// 可能几秒才出正文，缓冲会让浏览器**一直白屏转圈**（用户 2026-09-18 报障：
		// "就这样卡着，一直加载不动"）。100ms 一次 flush 让它像直连一样逐段出现。
		FlushInterval: 100 * time.Millisecond,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			// 路径原样带过去（nginx 那个 location 就是 /phpmyadmin/）
			pr.Out.Host = target.Host
			pr.SetXForwarded()
		},
		// ⚠️ 必须改写 Set-Cookie 的 path（2026-09-18 用户报障："本机的 phpmyadmin
		// 登录不上了！Failed to set session cookie. Maybe you are using HTTP instead
		// of HTTPS" + "一点导入就 500"）。
		//
		// 根因：phpMyAdmin 按**自己的**路径写 Cookie：`path=/phpmyadmin/`；而浏览器
		// 访问的是面板入口 `/<安全后缀>/phpmyadmin/` —— 路径对不上，浏览器**不会**
		// 把它带回来，于是每个请求都是新会话：登录页提交后没有会话（"Failed to set
		// session cookie"），导入 POST 也因为没有会话/令牌而 500。
		//
		// 修法就是在反代这一层把 Cookie 的 path 补上面板前缀（不改 phpMyAdmin 一行配置，
		// 也不把它的路径写死到 vhost 里）。Location 同理（phpMyAdmin 会 302 到自己的
		// 绝对路径）。
		ModifyResponse: func(res *http.Response) error {
			if res == nil {
				return nil
			}
			if cks := res.Header.Values("Set-Cookie"); len(cks) > 0 {
				res.Header.Del("Set-Cookie")
				for _, ck := range cks {
					res.Header.Add("Set-Cookie", rewriteCookiePath(ck, "/phpmyadmin", prefix+"/phpmyadmin"))
				}
			}
			if loc := res.Header.Get("Location"); strings.HasPrefix(loc, "/phpmyadmin") {
				res.Header.Set("Location", prefix+loc)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			// 错误页要**可操作**：不只是报"连不上"，而是把最可能的三步直接写出来
			// （2026-09-18：用户遇到的是"页面一直转圈"，根因是 PHP-FPM 里有卡住的
			// 请求 / nginx 没跑 / phpMyAdmin 没装 —— 三者的排查顺序就按这个写）。
			_, _ = fmt.Fprintf(w, `<h1>502 phpMyAdmin 暂时不可用</h1>
<p>面板无法连接本机 nginx 上的 phpMyAdmin（或它超时未响应）：%s</p>
<p>如果刚才是"页面一直转圈没有反应"，**最可能是 PHP-FPM 里有卡住的请求**（phpMyAdmin 的
会话在请求期间加锁，卡住的那个请求会让后面每个页面都排队等它）：</p>
<ol>
  <li>如果刚才有导入/导出**一直在转圈**：去「网站管理 → 🐘 PHP 环境」把对应 PHP 版本**重启**一次
      （卡住的请求会一直占着 PHP 的会话，后面的页面就永远转圈），然后**只开一个 phpMyAdmin 标签页**重试。</li>
  <li>确认 nginx 在跑：「网站管理 → ⚙️ 调整配置 → nginx → 服务」应显示"运行中"，不在跑就点「▶ 启动」。</li>
  <li>确认 phpMyAdmin 已安装：「应用 → 应用市场 → phpMyAdmin」。</li>
</ol>`, escHTML(err.Error()))
		},
	}
}

// rewriteCookiePath 把 Set-Cookie 里的 Path 值按前缀改写（大小写不敏感，保留其余属性）。
//
// 只改**完全等于** from 或以其 + "/" 开头的 Path；其它 Path（例如 "/"）原样保留 ——
// 改错会让本来正常的 Cookie 失效，比不改更糟。
func rewriteCookiePath(cookie, from, to string) string {
	lower := strings.ToLower(cookie)
	idx := strings.Index(lower, "path=")
	if idx < 0 {
		return cookie
	}
	valStart := idx + len("path=")
	valEnd := valStart
	for valEnd < len(cookie) && cookie[valEnd] != ';' && cookie[valEnd] != ',' {
		valEnd++
	}
	val := strings.TrimSpace(cookie[valStart:valEnd])
	if val != from && !strings.HasPrefix(val, from+"/") {
		return cookie
	}
	newVal := to + strings.TrimPrefix(val, from)
	return cookie[:valStart] + newVal + cookie[valEnd:]
}

// handlePhpMyAdmin 是入口：未登录直接拦住（不是重定向，避免暴露面板地址）。
func (s *Server) handlePhpMyAdmin(w http.ResponseWriter, r *http.Request) {
	// 直接查会话（不用 requireAuth：这里要的不是 JSON 错误，而是一张
	// 告诉用户"先去登录面板"的说明页）
	if _, err := s.Auth.AuthSession(r.Context(), s.sessionToken(r)); err != nil {
		s.phpAdminLoginRequired(w, r)
		return
	}
	// ⚠️ phpMyAdmin 的**导入**是一个大 body 的 POST（走浏览器 → 面板 → nginx → PHP）。
	// 面板全局设了 `ReadTimeout: 30s`（防 Slowloris），它算的是"读完整个请求体"的
	// 绝对截止时间 —— 于是任何一段稍慢的上传都会在面板这一层被掐断，表现就是
	// **点了导入没反应/一直转圈**（2026-09-18 用户报障："我就是要用 phpMyAdmin"）。
	// 这里给这条路由单独把读截止时间推后（与文件上传同一条保护，见 api_upload_guard.go）。
	if r.Method == http.MethodPost {
		if err := allowLongUpload(w, r); err != nil {
			s.Log.Warn("延长 phpMyAdmin 反代的读超时失败（大文件导入可能被中断）：%v", err)
		}
	}
	// 给上游一个**明确的上限**：超过就由反代的 ErrorHandler 输出一张"可操作"的错误页，
	// 而不是让浏览器无限转圈（2026-09-18 用户报障："就这样卡着，一直加载不动"）。
	// 页面（GET）给 2 分钟，导入/导出（POST）给 15 分钟（4MB 的库本机只要 2 秒，
	// 15 分钟足够宽松；再大就该用「数据库 → 导入 SQL 文件」那条不走 PHP 的路）。
	timeout := 2 * time.Minute
	if r.Method == http.MethodPost {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	s.phpAdminProxy().ServeHTTP(w, r.WithContext(ctx))
}

// phpAdminLoginRequired 给未登录用户一张说明页：说清"要先去面板登录"，
// 并给出登录地址（只有知道安全后缀的人才看得到这一页）。
func (s *Server) phpAdminLoginRequired(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	// 同上：未登录时**不暴露面板入口**（安全后缀不该从错误页泄露）
	_, _ = io.WriteString(w, `<h1>401 需要先登录面板</h1>
<p>phpMyAdmin 只对已登录的面板用户开放。</p>
<p>请在你平时使用的面板地址登录后，再从「应用市场 → phpMyAdmin → 打开」进入。</p>`)
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
