//go:build !darwin

package files

// 非 darwin 平台没有 getfsstat：拿不到挂载类型就按"不是网络卷"处理（放行）。
func fsTypeOfMount(string) string { return "" }

func isNetworkFilesystem(string) bool { return false }
