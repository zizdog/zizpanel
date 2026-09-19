package web

import (
	"bytes"
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
	"syscall"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  反向代理「缓存」的测试
//
//  证据分四层：
//    1. 关闭时不写任何 proxy_cache 指令（与升级前逐字一致，见 proxies 包）；
//    2. 开启时 proxy_cache_path 在 **http 上下文**（conf.d），proxy_cache 在该
//       规则的 location 里 —— 用**真实 nginx**证明：放进 server 块会 [emerg]；
//    3. 缓存真的工作（同一 URL 第二次不回源）——真实沙箱 nginx + 真上游计数；
//    4. 缓存目录归属 = nginx worker 真实用户；拿不到用户就失败，不许猜 nobody。
//
//  所有真 nginx 只跑在临时 prefix + 临时配置里，绝不碰 /opt/homebrew/etc/nginx。
// ============================================================================

// stubProxyCacheHooks 让"写 vhost"真的落盘（回读才看得到），并替换掉
// 校验/重载/chown/探测：单测不许跑真 nginx、不许 reload。
func stubProxyCacheHooks(t *testing.T, srv *Server) {
	t.Helper()
	prevWrite, prevTest := proxyWriteVhostFn, proxyCacheTestFn
	prevReload, prevChown, prevProbe := proxyReloadFn, proxyChownLogsFn, proxyProbeFn
	prevWait, prevEvery, prevSettle := proxyVerifyWait, proxyVerifyEvery, proxyLogSettle
	t.Cleanup(func() {
		proxyWriteVhostFn, proxyCacheTestFn = prevWrite, prevTest
		proxyReloadFn, proxyChownLogsFn, proxyProbeFn = prevReload, prevChown, prevProbe
		proxyVerifyWait, proxyVerifyEvery, proxyLogSettle = prevWait, prevEvery, prevSettle
	})
	proxyWriteVhostFn = func(s *Server, _ context.Context, name, content string) error {
		if err := os.MkdirAll(s.Cfg.VhostDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.Cfg.VhostDir, name+".conf"), []byte(content), 0o644); err != nil {
			return err
		}
		// 复核靠"该规则自己的访问日志增长"：先把日志文件建出来，探测才有东西可追加。
		if id := strings.TrimPrefix(name, "proxy-"); id != name && id != "" {
			f, ferr := os.OpenFile(filepath.Join(s.proxyLogDir(), "proxy-"+id+".access.log"),
				os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if ferr == nil {
				_ = f.Close()
			}
		}
		return nil
	}
	proxyCacheTestFn = func(*Server, context.Context) error { return nil }
	proxyReloadFn = func(*Server, context.Context) error { return nil }
	proxyChownLogsFn = func(*Server) {}
	proxyProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		matches, _ := filepath.Glob(filepath.Join(srv.proxyLogDir(), "proxy-*.access.log"))
		for _, m := range matches {
			appendToFile(t, m)
		}
		return "200", "upstream-ok", nil
	}
	proxyVerifyWait = 80 * time.Millisecond
	proxyVerifyEvery = time.Millisecond
	proxyLogSettle = 0
}

// stubProxyCacheOwner 把"worker 用户"判据换成当前进程的 uid/gid：非 root 下
// chown 到自己是允许的，于是测试能真的 stat 回读归属（而不是只看调用）。
func stubProxyCacheOwner(t *testing.T, ok bool) {
	t.Helper()
	uid, gid := os.Getuid(), os.Getgid()
	restore := priv.SetNginxWorkerProbeForTest(func() (int, int, int, bool) {
		if !ok {
			return 0, 0, 0, false
		}
		return uid, gid, os.Getpid(), true
	})
	t.Cleanup(restore)
}

func statOwner(t *testing.T, path string) (int, int) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("拿不到 %s 的 stat", path)
	}
	return int(st.Uid), int(st.Gid)
}

// TestEnsureProxyCacheDirOwnsToRealWorker：缓存目录归属必须是 worker 用户判据给出的
// uid/gid（真实 stat 回读），而不是"我以为改了"。
func TestEnsureProxyCacheDirOwnsToRealWorker(t *testing.T) {
	srv := newProxyTestServer(t)
	stubProxyCacheOwner(t, true)
	root, err := srv.ensureProxyCacheDir()
	if err != nil {
		t.Fatalf("建缓存目录失败：%v", err)
	}
	uid, gid := statOwner(t, root)
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Fatalf("缓存目录归属应为 worker 用户 %d:%d，实际 %d:%d", os.Getuid(), os.Getgid(), uid, gid)
	}
	fi, _ := os.Stat(root)
	if fi.Mode().Perm() != 0o750 {
		t.Errorf("缓存目录权限应为 0750，实际 %v", fi.Mode().Perm())
	}
}

// TestEnsureProxyCacheDirRefusesToGuessWorker：拿不到 worker 用户就**失败**，
// 绝不猜 nobody（坑 173：猜错等于没修还藏因）；但目录仍要建出来 —— nginx 启动时
// proxy_cache_path 的父目录不存在会直接 [emerg]。
func TestEnsureProxyCacheDirRefusesToGuessWorker(t *testing.T) {
	srv := newProxyTestServer(t)
	if err := os.MkdirAll(filepath.Dir(srv.Cfg.NginxConf), 0o755); err != nil {
		t.Fatal(err)
	}
	// 明确写一份没有 user 指令的 nginx.conf，把"读 conf 兜底"这条路也堵死。
	if err := os.WriteFile(srv.Cfg.NginxConf, []byte("worker_processes 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubProxyCacheOwner(t, false)
	root := srv.proxyCacheRoot()
	if _, err := srv.ensureProxyCacheDir(); err == nil {
		t.Fatal("拿不到 nginx worker 用户时必须失败，不许猜一个用户去 chown")
	} else if strings.Contains(strings.ToLower(err.Error()), "nobody") {
		t.Errorf("绝不许猜 nobody：%s", err)
	}
	if st, serr := os.Stat(root); serr != nil || !st.IsDir() {
		t.Errorf("目录必须建出来（否则 nginx 启动会 [emerg]）：err=%v", serr)
	}
}

// TestCacheZonesIdempotentEnableDisable：连续两次启用/关闭后文件字节不变、
// 也不会出现重复的 keys_zone（nginx 会因 duplicate zone 起不来）。
func TestCacheZonesIdempotentEnableDisable(t *testing.T) {
	proxyStatusResetCache()
	srv := newProxyTestServer(t)
	ctx := context.Background()
	rule := seedProxyRule(t, srv, testProxyRule(1, 18081, "a.test"))
	if err := srv.saveProxyCache(ctx, rule.ID, proxyCacheConfig{Enabled: true, Size: "1g", Valid: "1h"}); err != nil {
		t.Fatal(err)
	}
	writeVhost := func(content string) {
		t.Helper()
		if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srv.Cfg.VhostDir, rule.VhostName()+".conf"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hydrated, err := srv.proxyRepo().Get(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	hydrated.CacheEnabled, hydrated.CacheSize, hydrated.CacheValid = true, "1g", "1h"
	hydrated.CacheRoot = srv.proxyCacheRoot()
	onText, err := hydrated.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	writeVhost(onText)

	_, changed, err := srv.applyCacheZones(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("第一次启用应写入声明文件")
	}
	first, err := os.ReadFile(srv.proxyCacheConfPath())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(first), "keys_zone=zp_proxy_1:"); n != 1 {
		t.Fatalf("缓存区声明应恰好 1 条，实际 %d 条：\n%s", n, first)
	}
	if _, changed2, err := srv.applyCacheZones(ctx, ""); err != nil || changed2 {
		t.Fatalf("连续两次启用不该再改文件（幂等）：changed=%v err=%v", changed2, err)
	}
	second, _ := os.ReadFile(srv.proxyCacheConfPath())
	if !bytes.Equal(first, second) {
		t.Errorf("两次生成的声明文件字节不同：\n%s\n---\n%s", first, second)
	}

	// 关闭：vhost 里不再引用缓存区 → 声明文件应被删除。
	hydrated.CacheEnabled = false
	offText, err := hydrated.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	writeVhost(offText)
	if _, changed3, err := srv.applyCacheZones(ctx, ""); err != nil || !changed3 {
		t.Fatalf("关闭后应删掉声明文件：changed=%v err=%v", changed3, err)
	}
	if _, serr := os.Stat(srv.proxyCacheConfPath()); !os.IsNotExist(serr) {
		t.Errorf("没有任何规则开缓存时必须删除声明文件，实际 err=%v", serr)
	}
	if _, changed4, err := srv.applyCacheZones(ctx, ""); err != nil || changed4 {
		t.Fatalf("再关一次不该有改动（幂等）：changed=%v err=%v", changed4, err)
	}
}

// TestProxyCacheUpdateAndClearViaAPI：走真实接口开启缓存 → 回读真实文件 → 清空缓存
// （只删文件、不动配置）→ 关闭后声明与 location 指令一起消失。
func TestProxyCacheUpdateAndClearViaAPI(t *testing.T) {
	proxyStatusResetCache()
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyCacheHooks(t, srv)
	stubProxyCacheOwner(t, true)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "镜像站", Listen: 18460, Target: "http://127.0.0.1:9", Enabled: true,
	})
	cookies := loginTestPanel(t, ts)
	apiPath := "/api/v1/proxies/" + strconv.FormatInt(rule.ID, 10)
	zone := proxies.CacheZoneName(rule.ID)

	res, body, _ := doJSON(t, ts, "POST", apiPath, map[string]any{
		"name": "镜像站", "listen": 18460, "target": "http://127.0.0.1:9", "enabled": true,
		"cache_enabled": true, "cache_size": "5G", "cache_valid": "1d",
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("开启缓存应 200，实际 %d：%v", res.StatusCode, body)
	}
	data := apiData(t, body)
	if data["cache_enabled"] != true {
		t.Fatalf("响应应回读 cache_enabled=true：%v", data)
	}
	cv, _ := data["cache"].(map[string]any)
	if cv == nil || cv["verified"] != true {
		t.Fatalf("缓存配置已落盘，回读应 verified=true：%v（note=%v）", cv, cv["note"])
	}
	if cv["size"] != "5g" || cv["valid"] != "1d" {
		t.Errorf("上限/有效期应归一化后回读：%v", cv)
	}
	conf, err := os.ReadFile(srv.proxyCacheConfPath())
	if err != nil {
		t.Fatalf("conf.d 缓存区声明没有落盘：%v", err)
	}
	for _, want := range []string{"keys_zone=" + zone + ":", "max_size=5g ", "inactive=1d ", "use_temp_path=off;"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("缓存区声明缺少 %q：\n%s", want, conf)
		}
	}
	vhostPath := filepath.Join(srv.Cfg.VhostDir, rule.VhostName()+".conf")
	vh, _ := os.ReadFile(vhostPath)
	for _, want := range []string{
		"proxy_cache " + zone + ";",
		"proxy_cache_valid 200 301 302 1d;",
		"proxy_buffering       on;",
	} {
		if !strings.Contains(string(vh), want) {
			t.Errorf("vhost 缺少 %q：\n%s", want, vh)
		}
	}

	// ---- 清空缓存：造一个真实文件，只发 cache_clear ----
	dir := proxies.CacheZoneDir(srv.proxyCacheRoot(), rule.ID)
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "x"), bytes.Repeat([]byte("x"), 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	res, body, _ = doJSON(t, ts, "POST", apiPath, map[string]any{"cache_clear": true}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("清空缓存应 200，实际 %d：%v", res.StatusCode, body)
	}
	data = apiData(t, body)
	if data["cleared"] != true {
		t.Fatalf("响应应 cleared=true：%v", data)
	}
	if freed, _ := data["freed_bytes"].(float64); freed < 8192 {
		t.Errorf("释放字节数应 ≥ 8192，实际 %v", data["freed_bytes"])
	}
	// 回读：目录必须真的没了（不是"我发了删除命令"）。
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("清空后缓存目录必须不存在，实际 err=%v", serr)
	}
	// "只清缓存"不该动 nginx 配置。
	after, _ := os.ReadFile(srv.proxyCacheConfPath())
	if !bytes.Equal(conf, after) {
		t.Errorf("清空缓存不该改 conf.d 声明：\n%s\n---\n%s", conf, after)
	}
	if vh2, _ := os.ReadFile(vhostPath); !bytes.Equal(vh, vh2) {
		t.Errorf("清空缓存不该改 vhost")
	}

	// ---- 关闭缓存 ----
	res, body, _ = doJSON(t, ts, "POST", apiPath, map[string]any{
		"name": "镜像站", "listen": 18460, "target": "http://127.0.0.1:9", "enabled": true,
		"cache_enabled": false, "cache_size": "5g", "cache_valid": "1d",
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("关闭缓存应 200，实际 %d：%v", res.StatusCode, body)
	}
	if _, serr := os.Stat(srv.proxyCacheConfPath()); !os.IsNotExist(serr) {
		t.Errorf("关闭后不该残留缓存区声明，实际 err=%v", serr)
	}
	vh, _ = os.ReadFile(vhostPath)
	if strings.Contains(string(vh), "proxy_cache ") {
		t.Errorf("关闭后 vhost 不该再有 proxy_cache：\n%s", vh)
	}
	if !strings.Contains(string(vh), "proxy_buffering       off;") {
		t.Errorf("关闭后必须回到 proxy_buffering off：\n%s", vh)
	}
}

// TestProxyCacheBadSizeRejectedViaAPI：上限/有效期非法时 400，且不落库、不写盘。
func TestProxyCacheBadSizeRejectedViaAPI(t *testing.T) {
	proxyStatusResetCache()
	srv, ts := newProxyTestServerTS(t)
	seedProxyNginx(t, srv)
	stubProxyCacheHooks(t, srv)
	stubProxyCacheOwner(t, true)
	rule := seedProxyRule(t, srv, &proxies.Rule{
		Name: "规则", Listen: 18461, Target: "http://127.0.0.1:9", Enabled: true,
	})
	cookies := loginTestPanel(t, ts)
	apiPath := "/api/v1/proxies/" + strconv.FormatInt(rule.ID, 10)
	for _, c := range []map[string]any{
		{"cache_enabled": true, "cache_size": "1gb", "cache_valid": "1h"},
		{"cache_enabled": true, "cache_size": "1g", "cache_valid": "1h30m"},
	} {
		payload := map[string]any{"name": "规则", "listen": 18461, "target": "http://127.0.0.1:9", "enabled": true}
		for k, v := range c {
			payload[k] = v
		}
		res, body, _ := doJSON(t, ts, "POST", apiPath, payload, cookies)
		if res.StatusCode != 400 {
			t.Fatalf("非法缓存参数应 400，实际 %d：%v", res.StatusCode, body)
		}
	}
	if _, serr := os.Stat(srv.proxyCacheConfPath()); !os.IsNotExist(serr) {
		t.Errorf("被拒的请求不该写出缓存区声明，实际 err=%v", serr)
	}
}

// TestProxyCacheRealNginx：真实 nginx 上的四条证据。
//
//  1. 生成出来的配置（conf.d 声明 + 两个 vhost）能被 `nginx -t` 通过；
//  2. 反例：同一条 proxy_cache_path 放进 server 块 → nginx 明确报
//     "directive is not allowed here"（证明"必须在 http 块"这条真的被测到了）；
//  3. 同一 URL 第二次不回源、另一个 URL 首次回源、第二条规则独立回源；
//  4. 缓存文件真的落到了该规则的缓存目录里。
func TestProxyCacheRealNginx(t *testing.T) {
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
	for _, d := range []string{confDir, confD, vhostDir, runDir, logDir, bodyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 面板的"真实生成路径"要跑在沙箱里：VhostDir / DataDir 都指到临时目录，
	// 这样 cacheZonesContent（读 vhost + settings 产出 conf.d）才能被真 nginx 验证。
	srv := newProxyTestServer(t)
	srv.Cfg.VhostDir = vhostDir
	srv.Cfg.DataDir = dir
	cacheRoot := srv.proxyCacheRoot()
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "hello-"+r.URL.Path)
	}))
	defer upstream.Close()

	port1 := freePortForTest(t)
	port2 := freePortForTest(t)
	seed := func(port int, size, valid string) *proxies.Rule {
		t.Helper()
		db := seedProxyRule(t, srv, &proxies.Rule{
			Name: fmt.Sprintf("规则%d", port), Listen: port, Domains: "a.test",
			Target: upstream.URL, Enabled: true,
		})
		if err := srv.saveProxyCache(ctx, db.ID, proxyCacheConfig{
			Enabled: true, Size: size, Valid: valid,
		}); err != nil {
			t.Fatal(err)
		}
		r := *db
		r.CacheEnabled, r.CacheSize, r.CacheValid, r.CacheRoot = true, size, valid, cacheRoot
		conf, err := r.Generate(logDir)
		if err != nil {
			t.Fatalf("生成规则 %d 失败：%v", db.ID, err)
		}
		// 只加一个观测头（生成器里没有它）：用来直接看命中/未命中。
		conf = strings.Replace(conf, "location / {",
			"location / {\n\t\tadd_header X-Cache-Status $upstream_cache_status always;", 1)
		if err := os.WriteFile(filepath.Join(vhostDir, r.VhostName()+".conf"), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	r1 := seed(port1, "1g", "1h")
	r2 := seed(port2, "5g", "1d")
	// 声明文件由**面板自己的生成路径**产出（按磁盘上的 vhost 引用 + settings）。
	zoneConf, err := srv.cacheZonesContent(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"keys_zone=" + proxies.CacheZoneName(r1.ID) + ":",
		"max_size=1g ",
		"keys_zone=" + proxies.CacheZoneName(r2.ID) + ":",
		"max_size=5g ",
	} {
		if !strings.Contains(zoneConf, want) {
			t.Fatalf("面板生成的缓存区声明缺少 %q：\n%s", want, zoneConf)
		}
	}
	if err := os.WriteFile(filepath.Join(confD, proxies.CacheConfName), []byte(zoneConf), 0o644); err != nil {
		t.Fatal(err)
	}

	// conf.d 被 **http 块** include：proxy_cache_path 的合法位置。
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
	nginxTest := func() (string, error) {
		out, err := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput()
		return string(out), err
	}
	if out, err := nginxTest(); err != nil {
		t.Fatalf("沙箱 nginx -t 未通过（proxy_cache_path 在 conf.d/http 上下文）:\n%s", out)
	} else {
		t.Logf("面板生成的 %s：\n%s", proxies.CacheConfName, zoneConf)
		t.Logf("真实 nginx -t（临时前缀 + conf.d/http 上下文）：\n%s", strings.TrimSpace(out))
	}
	// 幂等：同样的规则集重新生成一遍，字节必须一致；重写后再过一次真 nginx -t。
	again, err := proxies.GenerateCacheConf([]*proxies.Rule{r1, r2}, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if again != zoneConf {
		t.Errorf("同样的规则集两次生成的缓存区声明不同：\n%s\n---\n%s", zoneConf, again)
	}
	if err := os.WriteFile(filepath.Join(confD, proxies.CacheConfName), []byte(again), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := nginxTest(); err != nil {
		t.Fatalf("重复写入声明后 nginx -t 未通过：\n%s", out)
	}

	// 反例：把同一条指令放进 server 块 → nginx 必须拒绝。没有它，这个测试就无法证明
	// "我们把声明放对了上下文"。
	badVhost := filepath.Join(vhostDir, "zz-bad.conf")
	if err := os.WriteFile(badVhost, []byte("server {\n    listen 127.0.0.1:1;\n"+
		"    proxy_cache_path /tmp/zp-nope keys_zone=zz_bad:10m max_size=1g;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := nginxTest()
	_ = os.Remove(badVhost)
	if err == nil {
		t.Fatalf("反例失效：proxy_cache_path 放进 server 块竟然通过了：\n%s", out)
	}
	if !strings.Contains(out, "not allowed here") {
		t.Errorf("反例报的不是上下文错误：\n%s", out)
	} else {
		t.Logf("反例（把同一条指令放进 server 块）被 nginx 拒绝：\n%s", strings.TrimSpace(out))
	}

	if out, err := exec.Command(nginxBin, "-c", mainPath).CombinedOutput(); err != nil {
		t.Fatalf("启动沙箱 nginx 失败：%v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(nginxBin, "-c", mainPath, "-s", "stop").Run() })
	waitPortForTest(t, port1)
	waitPortForTest(t, port2)

	get := func(port int, p string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, p), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "a.test"
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("请求 %s 失败：%v", p, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("请求 %s 应 200，实际 %d：%s", p, resp.StatusCode, b)
		}
		return resp.StatusCode, resp.Header.Get("X-Cache-Status")
	}

	if _, st := get(port1, "/a"); st != "MISS" {
		t.Errorf("首次请求应 MISS，实际 %q（缓存没生效？）", st)
	}
	if _, st := get(port1, "/a"); st != "HIT" {
		t.Errorf("第二次相同请求应 HIT，实际 %q", st)
	}
	t.Logf("规则1 /a 两次请求：MISS → HIT；上游此时被打 %d 次", atomic.LoadInt32(&hits))
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("命中缓存后上游不该再被打：上游实际被请求 %d 次", got)
	}
	get(port1, "/b")
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("不同 URL 必须回源：上游实际被请求 %d 次", got)
	}
	get(port2, "/a")
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("第二条规则有自己的缓存区，必须回源：上游实际被请求 %d 次", got)
	}
	// 缓存文件真的落在该规则的缓存目录里（不是"返回 HIT 但磁盘空"）。
	for _, id := range []int64{r1.ID, r2.ID} {
		zdir := proxies.CacheZoneDir(cacheRoot, id)
		if st, serr := os.Stat(zdir); serr != nil || !st.IsDir() {
			t.Fatalf("规则 %d 的缓存目录没有建出来：%v", id, serr)
		}
		if n, derr := dirSize(zdir); derr != nil || n <= 0 {
			t.Errorf("规则 %d 的缓存目录里没有缓存文件（size=%d err=%v）", id, n, derr)
		}
	}
}
