package sites

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 站点级回源缓存：站点 vhost 写在 server 级，缓存区声明（proxy_cache_path 只能在
// http 上下文）与反代规则共用 conf.d/zizpanel-cache.conf，见 internal/web。
//
// 为什么写在 server 级：proxy_cache / proxy_cache_valid / proxy_cache_key /
// proxy_cache_use_stale / proxy_cache_lock 都会被本站所有 location 继承 ——
// 用户在 extra_conf 里写的回源 location 不必改一个字就能缓存。
const (
	// SiteCacheZonePrefix 是站点缓存区名前缀，后面接站点 id（区名同时是"这个站点的缓存"的唯一标识）。
	SiteCacheZonePrefix = "zp_site_"
	// SiteCacheDefaultSize 是站点缓存上限：镜像站动辄几个 G，太小会一直淘汰。
	SiteCacheDefaultSize = "5g"
	// SiteCacheValid 是 200/301/302 的有效期；SiteCacheNotFoundValid 是 404 的短有效期
	// （上游临时缺文件时不该把 404 也钉住很久）。
	SiteCacheValid         = "7d"
	SiteCacheNotFoundValid = "1m"
	// siteCacheZoneKeys 是每个站点缓存区分配的共享内存（键索引）大小。
	siteCacheZoneKeys = "10m"
)

// reProxyBufferingOff 匹配 `proxy_buffering off;`（坑 189：这样写 nginx 根本不缓存）。
var reProxyBufferingOff = regexp.MustCompile(`(?i)\bproxy_buffering[ \t]+off[ \t]*;`)

// HasProxyBufferingOff 判断配置里是否残留 `proxy_buffering off;`（开缓存时不该有）。
func HasProxyBufferingOff(text string) bool { return reProxyBufferingOff.MatchString(text) }

// CacheZoneName 返回某站点的缓存区名（keys_zone）。
func CacheZoneName(id int64) string { return SiteCacheZonePrefix + strconv.FormatInt(id, 10) }

// CacheZoneID 从缓存区名反解站点 id（不是本面板命名的返回 false）。
func CacheZoneID(zone string) (int64, bool) {
	s, ok := strings.CutPrefix(strings.TrimSpace(zone), SiteCacheZonePrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// CacheZoneDir 返回某站点的缓存文件目录（与反代缓存同根：<DataDir>/proxy-cache）。
func CacheZoneDir(root string, id int64) string {
	return filepath.Join(root, CacheZoneName(id))
}

// CacheZoneDecl 返回该站点在 http 上下文的 `proxy_cache_path` 声明行（不含换行）。
func CacheZoneDecl(root string, id int64) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("缓存目录未配置，无法声明缓存区")
	}
	if id <= 0 {
		return "", fmt.Errorf("站点还没有保存，无法声明缓存区")
	}
	return fmt.Sprintf(
		"proxy_cache_path %s levels=1:2 keys_zone=%s:%s max_size=%s inactive=%s use_temp_path=off;",
		CacheZoneDir(root, id), CacheZoneName(id), siteCacheZoneKeys,
		SiteCacheDefaultSize, SiteCacheValid), nil
}

// cacheServerBlock 是开缓存时追加到 server 块的指令块（含开头的空行与结尾换行）。
func cacheServerBlock(id int64) string {
	var b strings.Builder
	b.WriteString("\n\t# ---- 回源缓存：上游文件缓存到本地（缓存区由面板在 conf.d 声明）----\n")
	fmt.Fprintf(&b, "\tproxy_cache %s;\n", CacheZoneName(id))
	// 与 nginx 默认键同形，但显式写出：键里含上游主机，换了上游不会吃到旧上游的缓存。
	b.WriteString("\tproxy_cache_key $scheme$proxy_host$request_uri;\n")
	fmt.Fprintf(&b, "\tproxy_cache_valid 200 301 302 %s;\n", SiteCacheValid)
	fmt.Fprintf(&b, "\tproxy_cache_valid 404 %s;\n", SiteCacheNotFoundValid)
	// updating：后台刷新时先给旧副本；error/timeout/5xx：上游抖动时不至于整站坏掉。
	b.WriteString("\tproxy_cache_use_stale updating error timeout http_500 http_502 http_503 http_504;\n")
	b.WriteString("\tproxy_cache_lock on;\n")
	b.WriteString("\tproxy_cache_bypass $http_upgrade;\n")
	b.WriteString("\tproxy_no_cache $http_upgrade;\n")
	// 坑 189：proxy_buffering off 时 nginx 不写缓存；开缓存必须显式 on。
	b.WriteString("\tproxy_buffering on;\n")
	return b.String()
}
