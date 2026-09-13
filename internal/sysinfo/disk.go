package sysinfo

import (
	"syscall"
)

// diskUsage 用 statfs 统计指定路径所在卷的容量。
//
// 用 syscall.Statfs 而不是 `df` 命令：少一次进程创建，且没有输出解析歧义。
// 注意 macOS 上 Bavail 对普通用户会扣除保留块，这里按 root/普通用户
// 的实际可用量呈现（与「关于本机」的口径一致）。
func diskUsage(path string) (total, used, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	free = st.Bavail * bsize
	if total < free {
		free = 0
	}
	used = total - free
	return total, used, free
}
