package backup

import (
	"fmt"
	"os"
	"path/filepath"
)

// apply.go 负责把解包出来的归档条目落回**当前机器**的真实路径。
//
// 关键点：映射关系用**当前配置**重新算一遍（Plan(当前 PlanOptions)），
// 而不是信归档里的绝对路径 —— 备份可能来自另一台机器、另一个根目录，
// 按归档里的路径写回去会写到一个不存在/不该写的地方。

// RestoreItem 把解包目录里的一个条目写回它来源的真实路径。
//
// 目录递归创建；文件一律 0600 起底（归档里含私钥与凭据），
// 目录 0700。失败即返回，由调用方决定整体是"回滚"还是"部分应用"。
func RestoreItem(extractDir string, it Item) error {
	src := filepath.Join(extractDir, filepath.FromSlash(it.ArchivePath))
	st, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 归档里没有这一项（可选项目），跳过
		}
		return err
	}
	if st.IsDir() {
		return copyDirTo(src, it.SourcePath)
	}
	if err := os.MkdirAll(filepath.Dir(it.SourcePath), 0o700); err != nil {
		return err
	}
	return copyFileTo(src, it.SourcePath)
}

func copyDirTo(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil // 归档里不该有特殊文件（Extract 已挡），这里再保一层
		}
		return copyFileTo(p, target)
	})
}

// ItemByArchivePath 在当前配置的计划里按归档路径找条目（恢复时用）。
func ItemByArchivePath(items []Item, archivePath string) (Item, bool) {
	for _, it := range items {
		if it.ArchivePath == archivePath {
			return it, true
		}
	}
	return Item{}, false
}

// CopyFile 暴露给调用方做"恢复前快照"以外的少量复制（如 config.json）。
func CopyFile(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	return copyFileTo(src, dst)
}

// ReadFile 读一个小文件（恢复前的二次确认、回读等）。
func ReadFile(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", p, err)
	}
	return b, nil
}
