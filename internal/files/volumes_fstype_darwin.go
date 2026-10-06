//go:build darwin

package files

import "syscall"

// fsTypeOfMount 用一次 getfsstat 查出某个挂载点的文件系统类型（查不到返回空串）。
//
// 判据用内核给的 Fstypename，不看路径名字 —— 网络盘挂在哪都还是网络盘。
// 不 fork 进程（文件管理器根目录每次请求都会重建，必须便宜）。
func fsTypeOfMount(mountPoint string) string {
	n, err := syscall.Getfsstat(nil, 0)
	if err != nil || n <= 0 {
		return ""
	}
	buf := make([]syscall.Statfs_t, n)
	n, err = syscall.Getfsstat(buf, 0)
	if err != nil || n <= 0 {
		return ""
	}
	for i := 0; i < n; i++ {
		if cString(buf[i].Mntonname[:]) == mountPoint {
			return cString(buf[i].Fstypename[:])
		}
	}
	return ""
}

// isNetworkFilesystem 判定文件系统类型是不是网络卷。
//
// 网络卷永远不需要「可移除宗卷」这个 TCC 授权（用户 2026-09-29 报障：本机没插外接盘、
// 只挂着网络盘，面板却天天提醒缺这个授权），所以两条枚举路径都要排除它。
func isNetworkFilesystem(fsType string) bool {
	switch fsType {
	case "smbfs", "nfs", "afpfs", "webdav", "cifs":
		return true
	}
	return false
}
