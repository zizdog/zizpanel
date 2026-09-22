// Package dirimport 按目录建剧场：只遍历用户选中的那一个目录（可限深），
// 集号识别一律调 internal/detect，绝不复制文件、绝不扫全库。
package dirimport

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zizdog/zizvideo/internal/detect"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
)

// ErrTooManyFiles / ErrTooManyDirs 是"如实拒绝"的判据：调用方必须回报超限，
// 不许截断后假装全部处理了。
var (
	ErrTooManyFiles = errors.New("目录内视频超过上限")
	ErrTooManyDirs  = errors.New("一级子目录超过上限")
)

// Unlimited 是 depth 的"不限层数"取值（B 的子目录内递归）。
const Unlimited = -1

// File 是一个候选视频及其识别结果；Recognized=false 表示文件名识别不到集号。
type File struct {
	Path       string
	Size       int64
	Season     *int
	Episode    *int
	Label      string
	Recognized bool
}

// Counts 是单目录计数；预览不保留路径，几十万文件也不会堆进内存。
type Counts struct {
	Files        int
	Recognized   int
	Unidentified int
	OverLimit    bool
}

// Scan 列出 root 下 depth 层内的视频；depth=0 只看本层，Unlimited=不限。
// 超过 maxFiles 时返回 ErrTooManyFiles（不截断）。
func Scan(root string, depth, maxFiles int, extAllowed func(string) bool) ([]File, error) {
	out := make([]File, 0, 64)
	err := walk(root, depth, extAllowed, func(path string, size int64) error {
		if len(out) >= maxFiles {
			return ErrTooManyFiles
		}
		out = append(out, describe(path, size))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Count 只计数不保留路径（B 预览用）；超限时返回已计数结果 + ErrTooManyFiles，
// 让预览能如实标出"这个子目录会被跳过"。
func Count(root string, depth, maxFiles int, extAllowed func(string) bool) (Counts, error) {
	var c Counts
	err := walk(root, depth, extAllowed, func(path string, size int64) error {
		if c.Files >= maxFiles {
			return ErrTooManyFiles
		}
		c.Files++
		if _, _, ok := detect.EpisodesFor(domain.Media{Path: path}); ok {
			c.Recognized++
		} else {
			c.Unidentified++
		}
		return nil
	})
	if errors.Is(err, ErrTooManyFiles) {
		c.OverLimit = true
	}
	return c, err
}

// ListSubdirs 流式读一级子目录（不一次 Readdirnames(-1)），最多 max 个；
// 超过就 ErrTooManyDirs，绝不静默截断。
func ListSubdirs(root string, max int) ([]string, error) {
	f, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([]string, 0, 16)
	for {
		entries, rerr := f.ReadDir(256)
		for _, e := range entries {
			if !e.IsDir() || media.IgnoredName(e.Name(), media.DefaultIgnoreRules) {
				continue
			}
			if len(out) >= max {
				return nil, ErrTooManyDirs
			}
			out = append(out, filepath.Join(root, e.Name()))
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, rerr
		}
	}
	sort.Strings(out)
	return out, nil
}

// walk 是唯一遍历入口：忽略规则、符号链接、扩展名白名单与扫描器完全一致。
func walk(root string, depth int, extAllowed func(string) bool, visit func(string, int64) error) error {
	rules := media.DefaultIgnoreRules
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // 单个子项读不到就跳过，别让一个坏目录毁掉整次遍历
		}
		if path == root {
			if !d.IsDir() {
				return fs.ErrInvalid
			}
			return nil
		}
		if media.IgnoredName(d.Name(), rules) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		level := strings.Count(rel, string(os.PathSeparator))
		if d.IsDir() {
			if depth != Unlimited && level+1 > depth {
				return filepath.SkipDir
			}
			return nil
		}
		if depth != Unlimited && level > depth {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if extAllowed != nil && !extAllowed(ext) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		return visit(path, info.Size())
	})
}

// Describe 给单个已落盘文件跑一次识别（上传 finish 与目录扫描共用同一内核）。
func Describe(path string, size int64) File { return describe(path, size) }

// describe 复用 detect 的唯一识别内核；识别不到一律留空 + 未识别。
func describe(path string, size int64) File {
	season, episode, ok := detect.EpisodesFor(domain.Media{Path: path})
	return File{
		Path: path, Size: size, Season: season, Episode: episode,
		Label: media.EpisodeLabel(season, episode), Recognized: ok,
	}
}
