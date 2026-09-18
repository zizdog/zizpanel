package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PruneOldPackages 清掉升级暂存目录里**旧的**升级包，只保留最近 keep 个版本。
//
// 为什么必须在面板里自动做（2026-09-18）：每次在线升级都会把发布包（arm64+amd64
// 约 50MB）下载到 <work>/upgrade/download 里，**从不清理**。本机实测：一天之内
// 攒了 139 个文件、1.8GB；用户的 mini 升级过几十次，同样会越滚越大 ——
// 而**磁盘满的后果正是"上传就 500"**：nginx 会把请求体缓冲到 client_body_temp，
// 空间不够时它不返回 413，而是直接 500（真机上就是这么表现）。
//
// 只删"文件名能解析出版本号、且不在保留名单里"的文件；解析不出来的**一个都不动**
// （不认识的东西不碰），暂存中的下载（tmp/incomplete）也保留给正在跑的任务。
func PruneOldPackages(workDir string, keep int) (removed int, freed int64, err error) {
	if strings.TrimSpace(workDir) == "" || keep < 1 {
		return 0, 0, nil
	}
	dir := filepath.Join(workDir, "upgrade", "download")
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		if os.IsNotExist(derr) {
			return 0, 0, nil
		}
		return 0, 0, derr
	}
	type item struct {
		path    string
		version string
		size    int64
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// 只认发布包命名：zizpanel_<版本>_darwin_<arch>.tar.gz
		if !strings.HasPrefix(name, "zizpanel_") || !strings.Contains(name, "_darwin_") {
			continue
		}
		if strings.HasSuffix(name, ".incomplete") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		ver := strings.SplitN(strings.TrimPrefix(name, "zizpanel_"), "_darwin_", 2)[0]
		if ver == "" || !strings.ContainsAny(ver, "0123456789") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(dir, name), version: ver, size: info.Size()})
	}
	if len(items) <= keep {
		return 0, 0, nil
	}
	// 版本号排序：按"数字分段"比较，1.2.10 > 1.2.9（字符串比较会反）。
	versionLess := func(a, b string) bool {
		as, bs := strings.Split(a, "."), strings.Split(b, ".")
		for i := 0; i < len(as) && i < len(bs); i++ {
			ai, bi := atoiSafe(as[i]), atoiSafe(bs[i])
			if ai != bi {
				return ai < bi
			}
		}
		return len(as) < len(bs)
	}
	sort.Slice(items, func(i, j int) bool { return versionLess(items[i].version, items[j].version) })

	keepFrom := len(items) - keep
	var errs []string
	for _, it := range items[:keepFrom] {
		if rerr := os.Remove(it.path); rerr != nil {
			errs = append(errs, rerr.Error())
			continue
		}
		removed++
		freed += it.size
	}
	if len(errs) > 0 {
		return removed, freed, fmt.Errorf("部分旧升级包删除失败: %s", strings.Join(errs, "; "))
	}
	return removed, freed, nil
}

// atoiSafe 解析版本号里的一段数字（解析不了当 0）。
func atoiSafe(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return n
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
