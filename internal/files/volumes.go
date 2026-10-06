package files

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// fsTypeOfMountFn 查一个挂载点的文件系统类型（注入点：单测要能造出"这个 /Volumes
// 子目录其实是 SMB 网络盘"，而不依赖测试机真的挂着一块网络盘）。
var fsTypeOfMountFn = fsTypeOfMount

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
// **网络卷不算**（2026-10-06 修）：来源 2 本来就把 smbfs/nfs/afpfs/webdav 排除了，
// 但来源 1 是"readdir 什么都收"，一合并网络盘又从 /Volumes 冒回来 —— 用户看到的
// 「可移除宗卷」目标列表里混着 SMB 盘就是这么来的（网络盘永远不需要这个 TCC 授权）。
// 所以统一在这一层按**文件系统类型**过滤一次，两条来源一视同仁。
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
		// 网络卷不是"可移除宗卷"：读它走的是网络协议，不需要（也不会因此获得）
		// 任何 TCC 授权。读不到挂载类型时**放行**（宁可多列一个，也别把真外接盘挡掉）。
		if isNetworkFilesystem(fsTypeOfMountFn(real)) {
			continue
		}
		seen[real] = true
		out = append(out, real)
	}
	sort.Strings(out)
	return out
}

// mntReadOnly 是 darwin mount(2) 的 MNT_RDONLY 标志位。
const mntReadOnly = 0x00000001

// OnReadOnlyFS 报告这个路径所在的文件系统是不是**只读挂载**。
//
// 为什么需要它：网络盘勾了「只读」（或外接盘写保护）时，"移动"必然在最后一步
// 删源失败 —— 用户 2026-09-29 实测：4GB 已经整份拷过去，才报 `read-only file system`，
// 白拷一遍。读不到 statfs 就返回 false（不猜，让真实操作去报错）。
func OnReadOnlyFS(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return st.Flags&mntReadOnly != 0
}
