package proxies

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 反向代理缓存：location 级指令由 Rule.Generate 写出，而 proxy_cache_path **只能**
// 出现在 http 上下文 —— 反代规则却在 server 级，所以缓存区声明走面板管理的
// conf.d/<CacheConfName>（nginx.conf 的 http 块 include conf.d/*.conf，见 priv.EnsureUpgradeMap）。
// 这样改缓存不用动 nginx.conf，天然幂等，也避免和站点 vhost 抢文件。

const (
	// CacheDefaultSize / CacheDefaultValid 是开启缓存时的默认上限与有效期。
	CacheDefaultSize  = "1g"
	CacheDefaultValid = "1h"
	// CacheConfName 是面板管理的 http 级缓存区声明文件（放在 nginx 的 conf.d 下）。
	CacheConfName = "zizpanel-cache.conf"
	// CacheZonePrefix 是缓存区名前缀，后面接规则 id —— 区名同时是"这条规则的缓存"的
	// 唯一标识，重命名规则不会让已缓存的键漂移。
	CacheZonePrefix = "zp_proxy_"
	// cacheZoneKeys 是每个缓存区分配的共享内存（键索引）大小。10m ≈ 8 万个键，
	// 对"回源一次、之后本地命中"的镜像站足够，且不会为每条规则吃掉几十 MB。
	cacheZoneKeys = "10m"
)

var (
	// reCacheSize 只接受 nginx 的 max_size 写法（数字+单位，如 512m / 1g）。
	reCacheSize = regexp.MustCompile(`^([0-9]{1,5})([kKmMgG])$`)
	// reCacheValid 只接受单个单位的 nginx 时间（如 30m / 1h / 1d）。
	reCacheValid = regexp.MustCompile(`^([0-9]{1,4})([smhd])$`)
)

// NormalizeCacheSize 校验并归一化缓存上限（统一小写、去掉空白）。
// 只认 `数字+单位`，拒绝 nginx 不接受的写法 —— 免得写出一份 nginx -t 直接失败的配置。
func NormalizeCacheSize(v string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	m := reCacheSize.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("缓存上限格式：1g / 512m（当前 %q）", v)
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 {
		return "", fmt.Errorf("缓存上限要大于 0（当前 %q）", v)
	}
	return s, nil
}

// NormalizeCacheValid 校验并归一化有效期（nginx 的 proxy_cache_valid 时间）。
func NormalizeCacheValid(v string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	m := reCacheValid.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("有效期格式：30m / 1h / 1d（当前 %q）", v)
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 {
		return "", fmt.Errorf("有效期要大于 0（当前 %q）", v)
	}
	return s, nil
}

// CacheZoneName 返回某条规则的缓存区名（keys_zone）。
func CacheZoneName(id int64) string { return CacheZonePrefix + strconv.FormatInt(id, 10) }

// CacheZoneID 从缓存区名反解规则 id（不是本面板命名的返回 false）。
func CacheZoneID(zone string) (int64, bool) {
	s, ok := strings.CutPrefix(strings.TrimSpace(zone), CacheZonePrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// CacheZoneDir 返回某条规则的缓存文件目录（磁盘上的真实落点）。
func CacheZoneDir(root string, id int64) string {
	return filepath.Join(root, CacheZoneName(id))
}

// CacheZoneDecl 返回该规则在 http 上下文的 `proxy_cache_path` 声明行（不含换行）。
//
// 参数显式传入而不是读 Rule 字段，是为了让"生成声明"在没有 Rule 的地方（例如
// 启动时按磁盘上的引用对齐）也能用同一份逻辑，避免两处写法漂移。
func CacheZoneDecl(root string, id int64, size, valid string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("缓存目录未配置，无法声明缓存区")
	}
	if id <= 0 {
		return "", fmt.Errorf("规则还没有保存，无法声明缓存区")
	}
	sz, err := NormalizeCacheSize(size)
	if err != nil {
		return "", err
	}
	vl, err := NormalizeCacheValid(valid)
	if err != nil {
		return "", err
	}
	zone := CacheZoneName(id)
	return fmt.Sprintf(
		"proxy_cache_path %s levels=1:2 keys_zone=%s:%s max_size=%s inactive=%s use_temp_path=off;",
		CacheZoneDir(root, id), zone, cacheZoneKeys, sz, vl), nil
}

// CacheZoneDeclFor 是 CacheZoneDecl 的 Rule 版本（用在 Generate 里）。
func (r *Rule) CacheZoneDeclFor(root string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("规则为空")
	}
	return CacheZoneDecl(root, r.ID, r.CacheSize, r.CacheValid)
}

// CacheZoneDirFor 返回该规则的缓存目录（用 r.CacheRoot）。
func (r *Rule) CacheZoneDirFor() string {
	if r == nil {
		return ""
	}
	return CacheZoneDir(r.CacheRoot, r.ID)
}

// CacheConfHeader 是缓存区声明文件的头部两行（内容为空时整个文件会被删除）。
// 站点级回源缓存（zp_site_<id>）与反代规则共用这个文件，所以标题写"回源缓存"。
const CacheConfHeader = "# 由 ZizPanel「回源缓存」生成 —— 请勿手工编辑（会被面板覆盖）\n" +
	"# proxy_cache_path 只能在 http 上下文；本文件由 nginx.conf 的 include conf.d/*.conf 加载。\n"

// GenerateCacheConf 生成缓存区声明文件的内容。
//
// 只声明**入参里真的启用了缓存且规则本身已启用**的缓存区；一个都没有时返回空串，
// 调用方据此删除文件 —— 这样"没有任何规则开缓存"时磁盘上与升级前完全一致。
// 输出按区名排序，保证同样的规则集生成同样的字节（幂等）。
func GenerateCacheConf(rules []*Rule, root string) (string, error) {
	decls := []string{}
	for _, r := range rules {
		if r == nil || !r.Enabled || !r.CacheEnabled {
			continue
		}
		decl, err := r.CacheZoneDeclFor(root)
		if err != nil {
			return "", fmt.Errorf("规则「%s」的缓存区无法声明：%w", r.Name, err)
		}
		decls = append(decls, decl)
	}
	if len(decls) == 0 {
		return "", nil
	}
	sort.Strings(decls)
	return CacheConfHeader + strings.Join(decls, "\n") + "\n", nil
}
