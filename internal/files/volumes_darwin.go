//go:build darwin

package files

import (
	"path/filepath"
	"syscall"
)

// extraVolumeMounts 用一次 getfsstat 系统调用拿到所有挂载点，挑出非系统卷。
//
// 为什么还要这一步：绝大多数外接盘挂在 /Volumes 下（scanVolumesDir 已经覆盖），
// 但用户完全可能手工 `mount` 到 /opt/xxx 或 /mnt/xxx。getfsstat 不 fork 进程、
// 不解析文本，代价可忽略；用它兜住这些"挂在不标准位置"的卷。
func extraVolumeMounts() []string {
	n, err := syscall.Getfsstat(nil, 0)
	if err != nil || n <= 0 {
		return nil
	}
	buf := make([]syscall.Statfs_t, n)
	n, err = syscall.Getfsstat(buf, 0)
	if err != nil || n <= 0 {
		return nil
	}
	var out []string
	for i := 0; i < n; i++ {
		fsType := cString(buf[i].Fstypename[:])
		switch fsType {
		case "devfs", "autofs", "fdesc", "procfs":
			continue
		}
		mp := cString(buf[i].Mntonname[:])
		if mp == "" || isSystemMountPoint(mp) {
			continue
		}
		out = append(out, filepath.Clean(mp))
	}
	return out
}

// cString 把 syscall 的 NUL 结尾字符数组转成 Go 字符串。
func cString(b []int8) string {
	buf := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}
