package videoopt

// staleparts.go —— output/ 里遗留的未完成产物（*.part.mp4）的扫描。
//
// 转码全程写 <产物>.part.mp4（run.go 952），只有进程确认编码成功才 os.Rename；
// 面板中途被重启（升级）时进程没了，半成品就留在 output/ 里，下次开跑才被删。
// 坑 236：用户此前看不到"那次到底压完没有"，只能等下次重跑。
//
// 判据与 scanTree 同一套：不跟符号链接、不进 `.` 开头的目录、深度有上限、
// 读不到的子目录跳过（绝不让计划整体失败）；上限写死，超了如实说明（绝不静默截断）。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PartSuffix 是转码半成品后缀（与 run.go 的 <产物>.part.mp4 同源，只此一处定义）。
const PartSuffix = ".part.mp4"

// MaxStaleParts 是单次最多列出的遗留半成品数（写死；超了截断并如实说明）。
const MaxStaleParts = 200

// staleScanMaxDepth 是扫描 output/ 的最大层数（递归产物的分层与源目录一致）。
const staleScanMaxDepth = DefaultMaxScanDepth

// StalePart 是一条遗留的未完成产物（只读展示 + 清理接口按 Path 精确删除）。
type StalePart struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	Modified  time.Time `json:"modified"`
	// RelPath 是相对 output/ 根的斜杠路径（递归产物在子目录里时用它区分同名文件）。
	RelPath string `json:"rel_path"`
}

// IsStalePartName 判断文件名是不是转码半成品（只认后缀，口径只此一处）。
func IsStalePartName(name string) bool {
	return strings.HasSuffix(name, PartSuffix)
}

// ScanStaleParts 扫 outDir 下遗留的 *.part.mp4，最多 limit 条（limit<=0 用 MaxStaleParts）。
//
// 返回 truncated=true 表示确实还有更多没列出（调用方必须如实告诉用户）。
// outDir 不存在/读不到 ⇒ 返回空且不报错：没跑过压缩就没有 output/，不是故障。
func ScanStaleParts(outDir string, limit int) (parts []StalePart, truncated bool) {
	if limit <= 0 {
		limit = MaxStaleParts
	}
	outDir = filepath.Clean(strings.TrimSpace(outDir))
	if outDir == "" || outDir == "." {
		return nil, false
	}

	var walk func(cur, rel string, depth int)
	walk = func(cur, rel string, depth int) {
		ents, err := os.ReadDir(cur)
		if err != nil {
			return // 没建过 / 读不到：当没有，绝不让计划整体失败
		}
		for _, e := range ents {
			// 已经拿到 limit+1 条 = 确定超上限，再走也没意义。
			if len(parts) > limit {
				return
			}
			if e.Type()&os.ModeSymlink != 0 {
				continue // 绝不跟符号链接（与 scanTree 同判据）
			}
			name := e.Name()
			p := filepath.Join(cur, name)
			if e.IsDir() {
				if strings.HasPrefix(name, ".") {
					continue // 隐藏目录：与文件管理器默认口径一致
				}
				if depth+1 > staleScanMaxDepth {
					continue
				}
				walk(p, filepath.Join(rel, name), depth+1)
				continue
			}
			if !IsStalePartName(name) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			parts = append(parts, StalePart{
				Path: p, Name: name, SizeBytes: info.Size(), Modified: info.ModTime(),
				RelPath: filepath.ToSlash(filepath.Join(rel, name)),
			})
		}
	}
	walk(outDir, "", 0)
	sort.Slice(parts, func(i, j int) bool { return parts[i].RelPath < parts[j].RelPath })
	if len(parts) > limit {
		return parts[:limit], true
	}
	return parts, false
}
