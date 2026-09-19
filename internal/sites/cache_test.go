package sites

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/store"
)

// ============================================================================
//  站点级回源缓存的门禁
//
//  证据分四层：
//    1. 关闭时生成的 vhost 与加这个开关之前**逐字一致**（老站点升级不被改写）；
//    2. 开启时只有 server 级一组 proxy_cache 指令（location 从这里继承）；
//    3. 坑 189：extra_conf 里的 proxy_buffering off 必须被改写成 on，否则 nginx 不写缓存；
//    4. 老库（没有 proxy_cache 列）迁移后默认关、生成物不变。
// ============================================================================

// reGenAt 把"生成于 <时间>"那一行归一化：它是唯一随时间变化的内容，
// 不归一化就没法做整段逐字相等断言。
var reGenAt = regexp.MustCompile(`(?m)^# 由 ZizPanel 生成于 .*$`)

func normalizeGenAt(s string) string {
	return reGenAt.ReplaceAllString(s, "# 由 ZizPanel 生成于 <TS> —— 手工修改会在面板下次保存时被覆盖")
}

// mirrorCacheFixture 是"镜像站"形状的站点：明文 HTTP、外接盘根目录、
// extra_conf 里一条回源 location，并**故意**带 proxy_buffering off（坑 189 的现场）。
func mirrorCacheFixture() *Site {
	return &Site{
		Domain:  "mirror.test",
		Root:    "/Volumes/ZPMirror/mirror",
		Rewrite: "none",
		ExtraConf: "location /brew/ {\n" +
			"\tproxy_pass https://mirrors.ustc.edu.cn/brew/;\n" +
			"\tproxy_buffering off;\n" +
			"}",
	}
}

// goldenOffVhost 是加"回源缓存"这个开关**之前**同一站点生成的整份 vhost。
// 关缓存必须与它逐字一致（时间戳归一化后做相等断言，不是"包含/不包含"）。
func goldenOffVhost() string {
	return strings.Join([]string{
		"# 站点: mirror.test",
		"# 由 ZizPanel 生成于 <TS> —— 手工修改会在面板下次保存时被覆盖",
		"server {",
		"\tlisten      80;",
		"\tserver_name mirror.test;",
		"\troot        /Volumes/ZPMirror/mirror;",
		"\tindex       index.php index.html index.htm;",
		"",
		"\taccess_log  /tmp/zplogs/mirror.test.access.log;",
		"\terror_log   /tmp/zplogs/mirror.test.error.log warn;",
		"",
		"\tcharset utf-8;",
		"\tclient_max_body_size 512m;",
		"",
		"\t# ---- 安全响应头 ----",
		"\tadd_header X-Content-Type-Options nosniff always;",
		"\tadd_header X-Frame-Options SAMEORIGIN always;",
		"\tadd_header Referrer-Policy strict-origin-when-cross-origin always;",
		"",
		"\t# ---- 安全: 隐藏敏感文件 ----",
		"\tlocation ~ /\\. { deny all; access_log off; log_not_found off; }",
		"\tlocation ~* \\.(ini|log|conf|sql|bak|swp)$ { deny all; }",
		"",
		"\t# ---- 静态资源缓存 ----",
		"\tlocation ~* \\.(?:css|js|jpg|jpeg|gif|png|ico|svg|webp|avif|woff2?|ttf|eot|mp4|webm)$ {",
		"\t\texpires 7d;",
		"\t\taccess_log off;",
		"\t}",
		"",
		"\t# ---- 伪静态规则（模板：none）----",
		"\tlocation / {",
		"\t\ttry_files $uri $uri/ =404;",
		"\t}",
		"",
		"\t# ---- 纯静态站点：拒绝执行 PHP ----",
		"\tlocation ~ \\.php$ { return 403; }",
		"",
		"\t# ---- 自定义配置 ----",
		"location /brew/ {",
		"\tproxy_pass https://mirrors.ustc.edu.cn/brew/;",
		"\tproxy_buffering off;",
		"}",
		"}",
		"",
	}, "\n")
}

// TestSiteGenerateCacheOffIsByteIdentical：关缓存时整段文本与加开关前逐字一致
// （一个 proxy_cache 指令都不写，proxy_buffering off 原样保留）。
func TestSiteGenerateCacheOffIsByteIdentical(t *testing.T) {
	s := mirrorCacheFixture()
	s.ID = 7
	s.ProxyCache = false
	out, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normalizeGenAt(out), goldenOffVhost(); got != want {
		t.Errorf("关缓存时生成物必须与开启这个开关之前逐字一致：\n--- 实际 ---\n%s\n--- 期望 ---\n%s", got, want)
	}
	if strings.Contains(out, "proxy_cache ") {
		t.Errorf("关缓存时不该出现任何 proxy_cache 指令：\n%s", out)
	}
	if !strings.Contains(out, "\tproxy_buffering off;") {
		t.Errorf("关缓存时必须原样保留 extra_conf 里的 proxy_buffering off：\n%s", out)
	}
}

// TestSiteGenerateCacheOnServerLevel：开缓存后指令写在 server 级（location 继承），
// 并且 extra_conf 里的 proxy_buffering off 被改写成 on（坑 189：否则 nginx 不写缓存）。
func TestSiteGenerateCacheOnServerLevel(t *testing.T) {
	s := mirrorCacheFixture()
	s.ID = 7
	s.ProxyCache = true
	out, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n\tproxy_cache zp_site_7;\n",
		"\tproxy_cache_valid 200 301 302 " + SiteCacheValid + ";\n",
		"\tproxy_cache_valid 404 " + SiteCacheNotFoundValid + ";\n",
		"\tproxy_cache_use_stale updating error timeout http_500 http_502 http_503 http_504;\n",
		"\tproxy_cache_lock on;\n",
		"\tproxy_buffering on;\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("开启缓存后缺少 server 级指令 %q：\n%s", want, out)
		}
	}
	if HasProxyBufferingOff(out) {
		t.Errorf("开启缓存时必须把 proxy_buffering off 改写成 on（坑 189）：\n%s", out)
	}
	// 缓存区声明只能在 http 上下文：vhost 里出现 proxy_cache_path = nginx -t [emerg]。
	if strings.Contains(out, "proxy_cache_path") {
		t.Errorf("proxy_cache_path 只能在 http 上下文，不该出现在 vhost 里：\n%s", out)
	}
	// 指令必须在自定义配置**之前**的 server 级（extra_conf 里的 location 才继承得到）。
	cacheAt := strings.Index(out, "\tproxy_cache zp_site_7;")
	extraAt := strings.Index(out, "# ---- 自定义配置 ----")
	if cacheAt < 0 || extraAt < 0 || cacheAt > extraAt {
		t.Errorf("proxy_cache 必须在 server 级（自定义配置之前），实际 cacheAt=%d extraAt=%d：\n%s",
			cacheAt, extraAt, out)
	}
	// 关掉就回到逐字一致（同一个站点对象改一个字段）。
	s.ProxyCache = false
	off, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normalizeGenAt(off), goldenOffVhost(); got != want {
		t.Errorf("再次关闭后必须回到原样：\n--- 实际 ---\n%s\n--- 期望 ---\n%s", got, want)
	}
}

// TestSiteGenerateCacheFailsClosed：开了缓存但站点还没保存（id<=0）→ 报错，
// 绝不生成一份引用空气缓存区的配置（会让 nginx -t 报 unknown zone）。
func TestSiteGenerateCacheFailsClosed(t *testing.T) {
	s := mirrorCacheFixture()
	s.ProxyCache = true
	if _, err := s.Generate(Options{LogDir: "/tmp/zplogs"}); err == nil {
		t.Error("站点还没保存（id=0）时开缓存应报错")
	}
}

// TestSiteCacheZoneNaming：区名只由站点 id 决定，缓存目录/声明与反代同一套写法。
func TestSiteCacheZoneNaming(t *testing.T) {
	for _, id := range []int64{1, 42, 100000} {
		zone := CacheZoneName(id)
		got, ok := CacheZoneID(zone)
		if !ok || got != id {
			t.Errorf("CacheZoneID(%q) = (%d,%v)，期望 (%d,true)", zone, got, ok, id)
		}
	}
	for _, bad := range []string{"", "zp_site_", "zp_site_x", "zp_site_0", "zp_proxy_3", "site_3"} {
		if _, ok := CacheZoneID(bad); ok {
			t.Errorf("CacheZoneID(%q) 不该认", bad)
		}
	}
	decl, err := CacheZoneDecl("/d/proxy-cache", 42)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"proxy_cache_path /d/proxy-cache/zp_site_42",
		"keys_zone=zp_site_42:",
		"max_size=" + SiteCacheDefaultSize,
		"inactive=" + SiteCacheValid,
		"use_temp_path=off;",
	} {
		if !strings.Contains(decl, want) {
			t.Errorf("声明缺少 %q：%s", want, decl)
		}
	}
	if _, err := CacheZoneDecl("", 42); err == nil {
		t.Error("没有缓存目录时应报错")
	}
	if _, err := CacheZoneDecl("/d", 0); err == nil {
		t.Error("id<=0 时应报错")
	}
	if got := CacheZoneDir("/d/proxy-cache", 42); got != "/d/proxy-cache/zp_site_42" {
		t.Errorf("缓存目录 = %q", got)
	}
}

// TestHasProxyBufferingOff：只认 proxy_buffering off，别把 fastcgi_buffering off 也算进来。
func TestHasProxyBufferingOff(t *testing.T) {
	if !HasProxyBufferingOff("\tproxy_buffering off;\n") {
		t.Error("proxy_buffering off 应命中")
	}
	if HasProxyBufferingOff("\tproxy_buffering on;\n") {
		t.Error("proxy_buffering on 不该命中")
	}
	if HasProxyBufferingOff("\tfastcgi_buffering off;\n") {
		t.Error("fastcgi_buffering off 不该命中（改了它会坏 PHP 流式输出）")
	}
}

// TestOldSiteWithoutProxyCacheColumnMigratesOff：老库（sites 表没有 proxy_cache 列）
// 打开后字段默认关，生成的 vhost 与开启这个开关之前逐字一致。
func TestOldSiteWithoutProxyCacheColumnMigratesOff(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 加 proxy_cache 之前的 sites 表结构（逐字来自旧版 schema）。
	oldDDL := `CREATE TABLE sites (
	    id INTEGER PRIMARY KEY AUTOINCREMENT,
	    domain TEXT NOT NULL UNIQUE,
	    aliases TEXT NOT NULL DEFAULT '',
	    root TEXT NOT NULL,
	    php_version TEXT NOT NULL DEFAULT '',
	    rewrite TEXT NOT NULL DEFAULT 'none',
	    ssl_enabled INTEGER NOT NULL DEFAULT 0,
	    ssl_cert TEXT NOT NULL DEFAULT '',
	    ssl_key TEXT NOT NULL DEFAULT '',
	    ssl_provider TEXT NOT NULL DEFAULT '',
	    ssl_expires TEXT NOT NULL DEFAULT '',
	    proxy_pass TEXT NOT NULL DEFAULT '',
	    extra_conf TEXT NOT NULL DEFAULT '',
	    enabled INTEGER NOT NULL DEFAULT 1,
	    remark TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
	    updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
	)`
	if _, err := db.Exec(oldDDL); err != nil {
		t.Fatal(err)
	}
	fixture := mirrorCacheFixture()
	if _, err := db.Exec(`INSERT INTO sites(domain,root,rewrite,extra_conf) VALUES(?,?,?,?)`,
		fixture.Domain, fixture.Root, fixture.Rewrite, fixture.ExtraConf); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("老库迁移失败（老站点不能因缺 proxy_cache 列报错）: %v", err)
	}
	defer func() { _ = st.Close() }()

	got, err := NewManager(st, Options{}).Get(context.Background(), fixture.Domain)
	if err != nil {
		t.Fatalf("老站点读不出来: %v", err)
	}
	if got.ProxyCache {
		t.Fatalf("老库站点的 proxy_cache 必须默认关，实际 %+v", got)
	}
	out, err := got.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "proxy_cache ") {
		t.Errorf("老站点生成的 vhost 不该有任何 proxy_cache 指令：\n%s", out)
	}
	if gotNorm, want := normalizeGenAt(out), goldenOffVhost(); gotNorm != want {
		t.Errorf("老库站点迁移后生成的 vhost 必须与加开关前逐字一致：\n--- 实际 ---\n%s\n--- 期望 ---\n%s", gotNorm, want)
	}
	// 新列必须真的补上了：能写能读回（否则开缓存会静默丢）。
	got.ProxyCache = true
	if err := NewManager(st, Options{}).Update(context.Background(), got); err != nil {
		t.Fatalf("迁移后写入 proxy_cache 失败: %v", err)
	}
	back, err := NewManager(st, Options{}).Get(context.Background(), fixture.Domain)
	if err != nil {
		t.Fatal(err)
	}
	if !back.ProxyCache {
		t.Error("迁移补出的 proxy_cache 列写不进去（开缓存会静默失效）")
	}
}
