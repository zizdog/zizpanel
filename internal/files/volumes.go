package files

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 外接盘（非系统卷）的枚举。
//
// 为什么单独写在这里、而不是让上层直接跑 diskutil：
//   - 文件管理器的根目录**每次请求都会重新构造**（见 api_files.go 的注释：
//     用户可能刚装完应用，缓存根目录会让"编辑配置文件"报越界）。所以枚举必须便宜 ——
//     读 /Volumes 目录 +（darwin 上）一次 getfsstat 系统调用，不 fork 任何进程。
//   - 插拔后要能刷新：不做缓存，每次构造都重新枚举，代价只有一次 readdir。
//
// 只收"真实存在的目录"：/Volumes 下的 `Macintosh HD` 这类是指向 / 的软链接，
// 必须排除，否则外部盘列表里会冒出一个通到系统盘的入口。

// volumesDir 是 macOS 挂载外接盘的标准位置。变量而非常量：单测把它指到临时目录，
// 就能在不插真盘的前提下验证"软链接被排除 / 真实目录被收录"。
var volumesDir = "/Volumes"

// extraMountsFn 是"非 /Volumes 位置挂载的卷"的探测入口。
//
// 变量而不是直接调 extraVolumeMounts：单测必须能把它关掉，否则测试机的真实
// 外接盘会混进结果，"只收录真实目录"这类断言就没法写。
var extraMountsFn = extraVolumeMounts

// systemMountPrefixes 是"系统盘自有目录"挂载点的前缀，一律不开放。
//
// 这些路径即使被单独挂载（如 /System/Volumes/Data、/System/Volumes/Preboot），
// 也属于系统盘的一部分，文件管理器不该把它们当成"外接盘"。
//
// 刻意**不**包含 /var 与 /tmp：它们在 macOS 上都只是 /private 下的软链接，
// 真实挂载点会以 /private/var/... 出现（已被 /private 覆盖）；把它们写进来只会
// 误伤"用户/测试用临时目录充当挂载点"的正常场景。
var systemMountPrefixes = []string{
	"/System", "/private", "/dev", "/Library", "/usr", "/bin", "/sbin",
	"/etc", "/cores",
}

// isSystemMountPoint 判断挂载点是否属于系统盘。"/" 本身当然算。
func isSystemMountPoint(mp string) bool {
	mp = filepath.Clean(mp)
	if mp == "/" || mp == "." {
		return true
	}
	for _, p := range systemMountPrefixes {
		if mp == p || strings.HasPrefix(mp, p+"/") {
			return true
		}
	}
	return false
}

// scanVolumesDir 收录 dir 下的真实子目录（排除软链接）。
//
// 这里刻意**不再**做"系统路径"过滤：/Volumes 下的真实目录本来就都是卷，
// 唯一要挡的是 /Volumes/Macintosh HD -> / 这类软链接（见上）。
// 系统路径过滤只作用于 getfsstat 那一路（挂载点可能是 /System/Volumes/...）。
func scanVolumesDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		// 用 Lstat：/Volumes/Macintosh HD -> / 是软链接，必须排除
		li, err := os.Lstat(full)
		if err != nil || li.Mode()&os.ModeSymlink != 0 || !li.IsDir() {
			continue
		}
		out = append(out, full)
	}
	return out
}

// NonSystemVolumeMounts 返回所有"非系统卷"的挂载点（已解析软链接、去重、排序）。
//
// 两个来源合并：
//  1. /Volumes 下的真实子目录（外接盘、网络卷最标准的挂载位置）；
//  2. darwin 上由 getfsstat 拿到的、挂在别处（如 /opt/xxx、/mnt/xxx）的非系统卷。
//
// 任何一步失败都只是"少一个候选根"，绝不报错、绝不阻塞文件管理器构造。
func NonSystemVolumeMounts() []string {
	cands := scanVolumesDir(volumesDir)
	for _, p := range extraMountsFn() {
		if p == "" || isSystemMountPoint(p) {
			continue
		}
		cands = append(cands, p)
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(cands))
	for _, p := range cands {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			continue
		}
		real := resolveExisting(p)
		if seen[real] {
			continue
		}
		seen[real] = true
		out = append(out, real)
	}
	sort.Strings(out)
	return out
}
