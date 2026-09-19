package proxies

import (
	"strings"
	"testing"
)

// TestNormalizeCacheSizeAndValid：只认 nginx 能接受的写法，其余一律报错。
//
// 为什么必须严格：写进去的是 nginx 的 max_size / inactive，格式错的后果是
// `nginx -t` 直接失败、整条反代规则写不进去 —— 与其让用户面对一句 emerg，
// 不如在保存时就拦住。
func TestNormalizeCacheSizeAndValid(t *testing.T) {
	for _, want := range []string{"1g", "512m", "10G", " 2g ", "256k"} {
		got, err := NormalizeCacheSize(want)
		if err != nil {
			t.Errorf("NormalizeCacheSize(%q) 应通过，实际 %v", want, err)
			continue
		}
		if got != strings.ToLower(strings.TrimSpace(want)) {
			t.Errorf("NormalizeCacheSize(%q) = %q，应归一化成小写去空白", want, got)
		}
	}
	for _, bad := range []string{"", "0g", "1", "1gb", "g", "-1g", "1t", "999999g", "1g; rm -rf /"} {
		if _, err := NormalizeCacheSize(bad); err == nil {
			t.Errorf("NormalizeCacheSize(%q) 应报错", bad)
		}
	}
	for _, want := range []string{"1h", "30m", "1d", "7D", " 10m "} {
		if _, err := NormalizeCacheValid(want); err != nil {
			t.Errorf("NormalizeCacheValid(%q) 应通过，实际 %v", want, err)
		}
	}
	for _, bad := range []string{"", "0h", "1", "1w", "1h30m", "h", "1d; x"} {
		if _, err := NormalizeCacheValid(bad); err == nil {
			t.Errorf("NormalizeCacheValid(%q) 应报错", bad)
		}
	}
}

// TestCacheZoneNameRoundTrip：区名只由 id 决定（重命名规则不会让缓存漂移），
// 且能反解回 id（面板按磁盘引用对齐声明时靠它找设置）。
func TestCacheZoneNameRoundTrip(t *testing.T) {
	for _, id := range []int64{1, 7, 123456} {
		zone := CacheZoneName(id)
		got, ok := CacheZoneID(zone)
		if !ok || got != id {
			t.Errorf("CacheZoneID(%q) = (%d,%v)，期望 (%d,true)", zone, got, ok, id)
		}
	}
	for _, bad := range []string{"", "proxy_1", "zp_proxy_", "zp_proxy_x", "zp_proxy_0", "other_3"} {
		if _, ok := CacheZoneID(bad); ok {
			t.Errorf("CacheZoneID(%q) 不该认", bad)
		}
	}
}

// TestGenerateCacheConfOnlyEnabledCachedRules：只声明"启用且开了缓存"的规则；
// 一个都没有时返回空串（调用方据此删除文件 = 与升级前逐字一致）。
func TestGenerateCacheConfOnlyEnabledCachedRules(t *testing.T) {
	on := &Rule{ID: 2, Name: "开缓存", Enabled: true, CacheEnabled: true, CacheSize: "5g", CacheValid: "1d"}
	off := &Rule{ID: 3, Name: "没开", Enabled: true, CacheEnabled: false}
	disabled := &Rule{ID: 4, Name: "停用了", Enabled: false, CacheEnabled: true, CacheSize: "1g", CacheValid: "1h"}
	got, err := GenerateCacheConf([]*Rule{on, off, disabled, nil}, "/opt/zizpanel/data/proxy-cache")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "\nproxy_cache_path "); n != 1 {
		t.Fatalf("只该有一条声明，实际 %d 条：\n%s", n, got)
	}
	for _, want := range []string{
		"proxy_cache_path /opt/zizpanel/data/proxy-cache/zp_proxy_2",
		"keys_zone=zp_proxy_2:",
		"max_size=5g",
		"inactive=1d",
		"use_temp_path=off;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("声明缺少 %q：\n%s", want, got)
		}
	}
	// 没有任何规则开缓存 → 空串（调用方删文件，而不是留一个空文件）。
	if empty, err := GenerateCacheConf([]*Rule{off, disabled}, "/x"); err != nil || empty != "" {
		t.Errorf("没有规则开缓存时应返回空串，实际 %q err=%v", empty, err)
	}
	// 上限非法要报错，不能写出会让 nginx -t 失败的行。
	if _, err := GenerateCacheConf([]*Rule{{ID: 1, Name: "坏", Enabled: true,
		CacheEnabled: true, CacheSize: "1gb", CacheValid: "1h"}}, "/x"); err == nil {
		t.Error("上限非法时应报错")
	}
}

// TestRuleGenerateCacheOffIsByteIdentical：关闭缓存时输出与加缓存之前**逐字一致**
// —— 一个 proxy_cache 指令都不写（nginx 默认就是 off），老规则一次升级不能被改。
func TestRuleGenerateCacheOffIsByteIdentical(t *testing.T) {
	plain := &Rule{ID: 7, Name: "镜像站", Listen: 8090, Domains: "a.com b.com",
		Target: "http://192.168.1.8:8090", Websocket: true, Enabled: true}
	withOffFields := *plain
	// 带一堆"关着但填过"的缓存字段：也不能影响输出。
	withOffFields.CacheEnabled = false
	withOffFields.CacheSize = "5g"
	withOffFields.CacheValid = "1d"
	withOffFields.CacheRoot = "/opt/zizpanel/data/proxy-cache"

	a, err := plain.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	b, err := withOffFields.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("关闭缓存时输出必须逐字一致：\n--- 无缓存字段 ---\n%s\n--- 关闭态字段 ---\n%s", a, b)
	}
	if strings.Contains(a, "proxy_cache") {
		t.Errorf("关闭缓存时不该出现任何 proxy_cache 指令：\n%s", a)
	}
	if !strings.Contains(a, "proxy_buffering       off;") {
		t.Errorf("关闭缓存时必须保持原来的 proxy_buffering off：\n%s", a)
	}
}

// TestRuleGenerateCacheOn：开启后 location 里必须有缓存指令，且必须开缓冲。
//
// 真机 1.31.5 实测：`proxy_buffering off` 时 nginx **根本不写缓存**（同一 location
// 三次全 MISS、上游被打三次）。所以"开缓存"的配置里绝不能留着 off —— 否则这个
// 开关就是谎报成功。
func TestRuleGenerateCacheOn(t *testing.T) {
	r := &Rule{ID: 7, Name: "镜像站", Listen: 8090, Domains: "a.com",
		Target: "http://192.168.1.8:8090", Websocket: true, Enabled: true,
		CacheEnabled: true, CacheSize: "5g", CacheValid: "1d",
		CacheRoot: "/opt/zizpanel/data/proxy-cache"}
	out, err := r.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"proxy_cache zp_proxy_7;",
		"proxy_cache_valid 200 301 302 1d;",
		"proxy_cache_bypass $http_upgrade;",
		"proxy_no_cache $http_upgrade;",
		"proxy_buffering       on;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("开启缓存后缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "proxy_buffering       off;") {
		t.Errorf("开启缓存时不能同时写 proxy_buffering off（那样 nginx 不会缓存）：\n%s", out)
	}
	// 缓存区**绝不能**写进 vhost：proxy_cache_path 在 server 上下文是非法的
	//（真机 nginx -t 报 "directive is not allowed here"）。
	if strings.Contains(out, "proxy_cache_path") {
		t.Errorf("proxy_cache_path 只能在 http 上下文，不该出现在 vhost 里：\n%s", out)
	}
	// 关闭时同一份规则必须回到"一行缓存指令都没有"。
	r.CacheEnabled = false
	off, err := r.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "proxy_cache") {
		t.Errorf("关闭后不该残留缓存指令：\n%s", off)
	}
	t.Logf("开启缓存生成的 vhost：\n%s\n关闭缓存生成的 vhost：\n%s", out, off)
}

// TestRuleGenerateCacheFailsClosed：开了缓存却没有目录/还没保存 → 报错，
// 绝不生成一份"引用空气缓存区"的配置（那会让 nginx -t 报 unknown zone）。
func TestRuleGenerateCacheFailsClosed(t *testing.T) {
	base := &Rule{ID: 7, Name: "r", Listen: 8090, Target: "http://192.168.1.8:8090",
		Enabled: true, CacheEnabled: true, CacheSize: "1g", CacheValid: "1h"}
	if _, err := base.Generate(""); err == nil {
		t.Error("没有缓存目录时应报错")
	}
	noID := *base
	noID.ID = 0
	noID.CacheRoot = "/tmp/cache"
	if _, err := noID.Generate(""); err == nil {
		t.Error("规则还没保存（id=0）时应报错")
	}
	bad := *base
	bad.CacheRoot = "/tmp/cache"
	bad.CacheSize = "1gb"
	if _, err := bad.Generate(""); err == nil {
		t.Error("上限非法时应报错")
	}
}

// TestRuleValidateCache：启用时校验/归一化；关闭时不校验（重新开启不用重填）。
func TestRuleValidateCache(t *testing.T) {
	r := &Rule{Name: "r", Listen: 8090, Target: "http://192.168.1.8:8090",
		CacheEnabled: true, CacheSize: " 5G ", CacheValid: "1D"}
	if err := r.Validate(); err != nil {
		t.Fatalf("合法缓存配置应通过：%v", err)
	}
	if r.CacheSize != "5g" || r.CacheValid != "1d" {
		t.Errorf("Validate 应归一化：size=%q valid=%q", r.CacheSize, r.CacheValid)
	}
	bad := &Rule{Name: "r", Listen: 8090, Target: "http://192.168.1.8:8090",
		CacheEnabled: true, CacheSize: "1gb", CacheValid: "1h"}
	if err := bad.Validate(); err == nil {
		t.Error("启用 + 上限非法 应报错")
	}
	off := &Rule{Name: "r", Listen: 8090, Target: "http://192.168.1.8:8090",
		CacheEnabled: false, CacheSize: "垃圾值", CacheValid: "垃圾值"}
	if err := off.Validate(); err != nil {
		t.Errorf("关闭时不该校验缓存字段：%v", err)
	}
}
