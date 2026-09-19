package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  站点级回源缓存的门禁（面板侧）
//
//  证据分五层：
//    1. 开缓存后接口回读 cache_verified=true，vhost/声明文件/缓存目录都对得上；
//    2. 幂等：连续保存两次声明文件字节不变、zone 恰好声明一次；
//    3. 只动这个站点：conf.d 里别的规则声明逐字不变；
//    4. 拿不到 nginx worker 用户 → 失败且不猜 nobody；
//    5. 真 nginx：MISS → HIT（上游只被打一次），并给出 proxy_buffering off 的反例。
//
//  所有真 nginx 只跑在临时 prefix + 临时配置里，绝不碰 /opt/homebrew/etc/nginx。
// ============================================================================

func stubSiteCacheNginxTest(t *testing.T) {
	t.Helper()
	prev := proxyCacheTestFn
	proxyCacheTestFn = func(*Server, context.Context) error { return nil }
	t.Cleanup(func() { proxyCacheTestFn = prev })
}

func readCacheConf(t *testing.T, srv *Server) string {
	t.Helper()
	b, err := os.ReadFile(srv.proxyCacheConfPath())
	if err != nil {
		t.Fatalf("读取缓存区声明失败: %v", err)
	}
	return string(b)
}

func cacheDeclLine(t *testing.T, conf, zone string) string {
	t.Helper()
	for _, ln := range strings.Split(conf, "\n") {
		if strings.Contains(ln, "keys_zone="+zone+":") {
			return ln
		}
	}
	t.Fatalf("声明文件里找不到 %s 的声明行：\n%s", zone, conf)
	return ""
}

// TestSiteCacheEnableReadbackAndIdempotent：开缓存 → 回读生效值 → 连续保存两次仍幂等。
func TestSiteCacheEnableReadbackAndIdempotent(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	stubSiteChannelDisk(t, srv)
	stubProxyCacheOwner(t, true)
	stubSiteCacheNginxTest(t)
	cookies := loginTestPanel(t, ts)
	ctx := context.Background()

	extra := "location /brew/ {\n\tproxy_pass https://mirrors.ustc.edu.cn/brew/;\n\tproxy_buffering off;\n}"
	create := map[string]any{
		"domain": "mirror.test", "rewrite": "none", "extra_conf": extra,
		"proxy_cache": true, "autoindex": true,
	}
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", create, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("开缓存创建站点应 200，实际 %d：%v", res.StatusCode, out)
	}
	data := apiData(t, out)
	if data["cache_enabled"] != true {
		t.Fatalf("响应应回读 cache_enabled=true：%v", data)
	}
	if data["cache_verified"] != true {
		t.Fatalf("缓存已落盘，回读应 cache_verified=true（note=%v）", data["cache_verify_error"])
	}
	site, err := srv.siteMgr().Get(ctx, "mirror.test")
	if err != nil {
		t.Fatal(err)
	}
	if !site.ProxyCache {
		t.Fatal("数据库里的 proxy_cache 应为 true")
	}
	zone := sites.CacheZoneName(site.ID)

	vhost := readVhost(t, srv, "mirror.test")
	for _, want := range []string{
		"\tproxy_cache " + zone + ";",
		"\tproxy_cache_valid 200 301 302 " + sites.SiteCacheValid + ";",
		"\tproxy_cache_valid 404 " + sites.SiteCacheNotFoundValid + ";",
		"\tproxy_buffering on;",
	} {
		if !strings.Contains(vhost, want) {
			t.Errorf("vhost 缺少 %q：\n%s", want, vhost)
		}
	}
	if sites.HasProxyBufferingOff(vhost) {
		t.Errorf("开缓存后 vhost 里不能残留 proxy_buffering off（坑 189）：\n%s", vhost)
	}
	conf := readCacheConf(t, srv)
	if n := strings.Count(conf, "keys_zone="+zone+":"); n != 1 {
		t.Fatalf("缓存区声明应恰好 1 条，实际 %d 条：\n%s", n, conf)
	}
	if !strings.Contains(conf, "max_size="+sites.SiteCacheDefaultSize+" ") {
		t.Errorf("声明里应有 max_size=%s：\n%s", sites.SiteCacheDefaultSize, conf)
	}

	// 缓存目录必须真的建出来，且归属 = 真实 worker（stub 成当前用户，stat 回读）。
	dir := sites.CacheZoneDir(srv.siteCacheRoot(), site.ID)
	uid, gid := statOwner(t, dir)
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Fatalf("站点缓存目录归属应为 worker %d:%d，实际 %d:%d", os.Getuid(), os.Getgid(), uid, gid)
	}

	// 幂等：同样的保存再来一次，声明文件必须字节不变、zone 仍恰好一条。
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/mirror.test", create, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("第二次保存应 200，实际 %d：%v", res.StatusCode, out)
	}
	again := readCacheConf(t, srv)
	if again != conf {
		t.Errorf("连续保存两次声明文件必须字节一致：\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", conf, again)
	}
	if n := strings.Count(again, "keys_zone="+zone+":"); n != 1 {
		t.Errorf("重复保存后 zone 仍应恰好 1 条，实际 %d 条：\n%s", n, again)
	}
}

// TestSiteCacheOnlyThisSiteChangedInConf：conf.d 里别的规则声明一个字都不能动。
func TestSiteCacheOnlyThisSiteChangedInConf(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	stubSiteChannelDisk(t, srv)
	stubProxyCacheOwner(t, true)
	stubSiteCacheNginxTest(t)
	cookies := loginTestPanel(t, ts)
	ctx := context.Background()

	// 造一条"别的规则"的缓存区声明（与真实反代走同一条生成路径）。
	if err := srv.saveProxyCache(ctx, 99, proxyCacheConfig{Enabled: true, Size: "1g", Valid: "1h"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.Cfg.VhostDir, "proxy-99.conf"),
		[]byte("server {\n\tlisten 18099;\n\tproxy_cache zp_proxy_99;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.applyCacheZones(ctx, ""); err != nil {
		t.Fatal(err)
	}
	proxyLine := cacheDeclLine(t, readCacheConf(t, srv), "zp_proxy_99")

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "mirror.test", "rewrite": "none", "proxy_cache": true,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d：%v", res.StatusCode, out)
	}
	site, err := srv.siteMgr().Get(ctx, "mirror.test")
	if err != nil {
		t.Fatal(err)
	}
	zone := sites.CacheZoneName(site.ID)
	conf := readCacheConf(t, srv)
	if got := cacheDeclLine(t, conf, "zp_proxy_99"); got != proxyLine {
		t.Errorf("别的规则声明被改动了：\n--- 改动前 ---\n%s\n--- 改动后 ---\n%s", proxyLine, got)
	}
	if n := strings.Count(conf, "keys_zone="+zone+":"); n != 1 {
		t.Fatalf("本站 zone 应恰好 1 条，实际 %d 条：\n%s", n, conf)
	}

	// 关缓存：本站声明消失，别的规则声明逐字不变。
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/mirror.test", map[string]any{
		"proxy_cache": false,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("关缓存应 200，实际 %d：%v", res.StatusCode, out)
	}
	if vhost := readVhost(t, srv, "mirror.test"); strings.Contains(vhost, "proxy_cache ") {
		t.Errorf("关缓存后 vhost 不该再有 proxy_cache：\n%s", vhost)
	}
	conf = readCacheConf(t, srv)
	if strings.Contains(conf, "keys_zone="+zone+":") {
		t.Errorf("关缓存后本站声明必须被清掉：\n%s", conf)
	}
	if got := cacheDeclLine(t, conf, "zp_proxy_99"); got != proxyLine {
		t.Errorf("关缓存不该动别的规则声明：\n--- 期望 ---\n%s\n--- 实际 ---\n%s", proxyLine, got)
	}

	// 再开回来：声明恢复恰好一条，别的规则声明仍然逐字不变。
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/mirror.test", map[string]any{
		"proxy_cache": true,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("重新开启应 200，实际 %d：%v", res.StatusCode, out)
	}
	conf = readCacheConf(t, srv)
	if n := strings.Count(conf, "keys_zone="+zone+":"); n != 1 {
		t.Errorf("重新开启后本站 zone 应恰好 1 条，实际 %d 条：\n%s", n, conf)
	}
	if got := cacheDeclLine(t, conf, "zp_proxy_99"); got != proxyLine {
		t.Errorf("重新开启不该动别的规则声明：\n--- 期望 ---\n%s\n--- 实际 ---\n%s", proxyLine, got)
	}
}

// TestSiteCacheRefusesToGuessWorker：拿不到 nginx worker 用户就失败，且不猜 nobody。
func TestSiteCacheRefusesToGuessWorker(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	stubSiteChannelDisk(t, srv)
	stubProxyCacheOwner(t, false)
	stubSiteCacheNginxTest(t)
	cookies := loginTestPanel(t, ts)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "guess.test", "rewrite": "none", "proxy_cache": true,
	}, cookies)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("拿不到 worker 用户时必须失败（绝不猜一个用户去 chown）：%v", out)
	}
	msg := fmt.Sprint(out)
	if !strings.Contains(msg, "worker") {
		t.Errorf("错误必须说清是「拿不到 nginx worker 用户」：%v", out)
	}
	if strings.Contains(strings.ToLower(msg), "nobody") {
		t.Errorf("绝不许猜 nobody：%v", out)
	}
	if _, err := srv.siteMgr().Get(context.Background(), "guess.test"); err == nil {
		t.Error("应用配置失败后站点记录必须回滚")
	}
}

// TestSiteCacheClearAndDeleteConvergence：清缓存只删文件不动配置；删站点清声明与缓存目录。
func TestSiteCacheClearAndDeleteConvergence(t *testing.T) {
	srv, ts := newProxyTestServerTS(t)
	stubSiteChannelDisk(t, srv)
	stubProxyCacheOwner(t, true)
	stubSiteCacheNginxTest(t)
	cookies := loginTestPanel(t, ts)
	ctx := context.Background()

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/sites", map[string]any{
		"domain": "mirror.test", "rewrite": "none", "proxy_cache": true,
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d：%v", res.StatusCode, out)
	}
	site, err := srv.siteMgr().Get(ctx, "mirror.test")
	if err != nil {
		t.Fatal(err)
	}
	conf := readCacheConf(t, srv)
	dir := sites.CacheZoneDir(srv.siteCacheRoot(), site.ID)
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "x"), []byte(strings.Repeat("x", 8192)), 0o644); err != nil {
		t.Fatal(err)
	}

	res, out, _ = doJSON(t, ts, "POST", "/api/v1/sites/mirror.test", map[string]any{"cache_clear": true}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("清空缓存应 200，实际 %d：%v", res.StatusCode, out)
	}
	data := apiData(t, out)
	if data["cleared"] != true {
		t.Fatalf("响应应 cleared=true：%v", data)
	}
	if freed, _ := data["freed_bytes"].(float64); freed < 8192 {
		t.Errorf("释放字节数应 ≥ 8192，实际 %v", data["freed_bytes"])
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("清空后缓存目录必须不存在，实际 err=%v", serr)
	}
	// 只清缓存不该动配置。
	if got := readCacheConf(t, srv); got != conf {
		t.Errorf("清空缓存不该改声明文件：\n%s\n---\n%s", conf, got)
	}

	// 删站点：vhost、声明、缓存目录都要收敛。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/sites/mirror.test", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删除站点应 200，实际 %d：%v", res.StatusCode, out)
	}
	if _, serr := os.Stat(filepath.Join(srv.Cfg.VhostDir, "mirror.test.conf")); !os.IsNotExist(serr) {
		t.Errorf("删除后 vhost 必须没了，实际 err=%v", serr)
	}
	if _, serr := os.Stat(srv.proxyCacheConfPath()); !os.IsNotExist(serr) {
		t.Errorf("删除后本站声明必须收敛掉（没有别的 zone 时文件应删除），实际 err=%v；内容：\n%s",
			serr, readCacheConf(t, srv))
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("删除站点后缓存目录必须清掉，实际 err=%v", serr)
	}
}

// TestSiteCacheRealNginx：真实 nginx 上的三条证据。
//
//  1. 面板生成的站点 vhost（server 级 proxy_cache + extra_conf 回源 location）能被
//     真 nginx -t 通过；
//  2. 同一 URL 第二次 HIT、上游只被打一次（server 级指令真的继承进了回源 location）；
//  3. 反例：手写 `proxy_buffering off` 的同类配置两次都 MISS —— 证明第 2 条不是碰巧。
func TestSiteCacheRealNginx(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过缓存的真实校验")
	}
	mimeTypes := "/opt/homebrew/etc/nginx/mime.types"
	if _, err := os.Stat(mimeTypes); err != nil {
		t.Skip("找不到 mime.types，跳过缓存的真实校验")
	}
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	confD := filepath.Join(dir, "conf.d")
	vhostDir := filepath.Join(dir, "vhosts")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "logs")
	bodyDir := filepath.Join(dir, "body")
	cacheRoot := filepath.Join(dir, "proxy-cache")
	for _, d := range []string{confDir, confD, vhostDir, runDir, logDir, bodyDir, cacheRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "hello-"+r.URL.Path)
	}))
	defer upstream.Close()

	port := freePortForTest(t)
	portBad := freePortForTest(t)
	// 面板的真实生成路径：站点 vhost 走 site.Generate，缓存区声明走 sites.CacheZoneDecl。
	const siteID = 4242
	site := &sites.Site{
		ID: siteID, Domain: "mirror.test", Root: bodyDir, Rewrite: "none",
		ListenPort: port, Enabled: true, ProxyCache: true,
		ExtraConf: fmt.Sprintf("location /mirror/ {\n"+
			"\tproxy_pass %s/mirror/;\n"+
			"\tadd_header X-Cache-Status $upstream_cache_status always;\n"+
			"\tproxy_buffering off;\n}", upstream.URL),
	}
	vhost, err := site.Generate(sites.Options{LogDir: logDir})
	if err != nil {
		t.Fatalf("生成站点 vhost 失败：%v", err)
	}
	// 只绑回环，别在开发机上开出对外的监听。
	vhost = strings.Replace(vhost, "\tlisten      "+strconv.Itoa(port)+";",
		"\tlisten      127.0.0.1:"+strconv.Itoa(port)+";", 1)
	if err := os.WriteFile(filepath.Join(vhostDir, "mirror.test.conf"), []byte(vhost), 0o644); err != nil {
		t.Fatal(err)
	}
	// 反例：同一套机制，但保留 proxy_buffering off（坑 189：这样 nginx 根本不写缓存）。
	bad := fmt.Sprintf(`server {
    listen 127.0.0.1:%d;
    server_name nobuf.test;
    proxy_cache zp_site_9999;
    proxy_cache_valid 200 301 302 7d;
    proxy_buffering off;
    location /mirror/ {
        proxy_pass %s/mirror/;
        add_header X-Cache-Status $upstream_cache_status always;
    }
}
`, portBad, upstream.URL)
	if err := os.WriteFile(filepath.Join(vhostDir, "nobuf.test.conf"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	decl, err := sites.CacheZoneDecl(cacheRoot, siteID)
	if err != nil {
		t.Fatal(err)
	}
	badDecl, err := sites.CacheZoneDecl(cacheRoot, 9999)
	if err != nil {
		t.Fatal(err)
	}
	zoneConf := "# 由 ZizPanel「回源缓存」生成（测试夹具）\n" + decl + "\n" + badDecl + "\n"
	if err := os.WriteFile(filepath.Join(confD, "zizpanel-cache.conf"), []byte(zoneConf), 0o644); err != nil {
		t.Fatal(err)
	}

	main := fmt.Sprintf(`worker_processes 1;
error_log %s/error.log warn;
pid %s/nginx.pid;
events { worker_connections 64; }
http {
    include %s;
    default_type application/octet-stream;
    access_log off;
    client_body_temp_path %s;
    proxy_temp_path %s/proxy;
    fastcgi_temp_path %s/fastcgi;
    uwsgi_temp_path %s/uwsgi;
    scgi_temp_path %s/scgi;
    include %s/*.conf;
    include %s/*.conf;
}
`, logDir, runDir, mimeTypes, bodyDir, runDir, runDir, runDir, runDir, confD, vhostDir)
	mainPath := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput(); err != nil {
		t.Fatalf("沙箱 nginx -t 未通过：\n%s", out)
	} else {
		t.Logf("面板生成的站点缓存配置被真 nginx -t 通过：\n%s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(nginxBin, "-c", mainPath).CombinedOutput(); err != nil {
		t.Fatalf("启动沙箱 nginx 失败：%v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(nginxBin, "-c", mainPath, "-s", "stop").Run() })
	waitPortForTest(t, port)
	waitPortForTest(t, portBad)

	get := func(p int, host, path string) string {
		t.Helper()
		req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", p, path), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("请求 %s%s 失败：%v", host, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("请求 %s%s 应 200，实际 %d：%s", host, path, resp.StatusCode, b)
		}
		return resp.Header.Get("X-Cache-Status")
	}

	first := get(port, "mirror.test", "/mirror/a")
	second := get(port, "mirror.test", "/mirror/a")
	t.Logf("站点回源 location 两次请求：%s → %s（上游被打 %d 次）", first, second, atomic.LoadInt32(&hits))
	if first != "MISS" {
		t.Errorf("首次请求应 MISS，实际 %q（缓存没生效？）", first)
	}
	if second != "HIT" {
		t.Errorf("第二次相同请求应 HIT，实际 %q（server 级 proxy_cache 没继承进 location？）", second)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("命中缓存后上游不该再被打：上游实际被请求 %d 次", got)
	}

	// 反例：手写 proxy_buffering off 的配置两次都 MISS（坑 189 的真实复现）。
	b1 := get(portBad, "nobuf.test", "/mirror/a")
	b2 := get(portBad, "nobuf.test", "/mirror/a")
	t.Logf("反例（proxy_buffering off）两次请求：%s → %s（上游被打到 %d 次）", b1, b2, atomic.LoadInt32(&hits))
	if b1 != "MISS" || b2 != "MISS" {
		t.Errorf("proxy_buffering off 时 nginx 不该写缓存（坑 189），实际 %s → %s", b1, b2)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("反例必须两次都回源：上游应共被请求 3 次，实际 %d 次", got)
	}

	// 缓存文件真的落在该站点的缓存目录里（不是"返回 HIT 但磁盘空"）。
	zdir := sites.CacheZoneDir(cacheRoot, siteID)
	if st, serr := os.Stat(zdir); serr != nil || !st.IsDir() {
		t.Fatalf("站点缓存目录没有建出来：%v", serr)
	}
	if n, derr := dirSize(zdir); derr != nil || n <= 0 {
		t.Errorf("站点缓存目录里没有缓存文件（size=%d err=%v）", n, derr)
	}
}
