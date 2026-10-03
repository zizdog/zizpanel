package videoopt

// scan.go —— 递归子目录的扫描（「包含子目录」勾上时才走这里）。
//
// 安全边界（用户点名，门禁逐条断言）：
//   · 不跟随符号链接（Lstat/ReadDir 的 Type 判据），绝不走出基准目录；
//   · 跳过基准目录下的 output/（产物不能被当成源，否则反复压缩/自噬）；
//   · 深度与文件数有上限，超限**如实说明**（"达到上限，未继续"），绝不静默截断；
//   · 读不到的子目录跳过并记入跳过清单，不让整个任务失败；
//   · `.` 开头的隐藏目录跳过（与文件管理器默认隐藏点条目的口径一致）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultMaxScanDepth 是递归的最大层数（基准目录的直接子目录 = 第 1 层）。
	DefaultMaxScanDepth = 12
	// DefaultMaxScanFiles 是单次递归扫描的视频文件数上限。
	DefaultMaxScanFiles = 5000
	// scanReportEveryDirs / scanReportEveryTime 是扫描进度的节流阈值：
	// 每读这么多目录、或距上次上报这么久，才回调一次（大目录树逐文件回调等于刷屏）。
	scanReportEveryDirs = 50
	scanReportEveryTime = 200 * time.Millisecond
)

// ScanProgressFunc 是递归扫描过程中的真实进度回调（已按节流回调）。
//
// dirs = 已读完的目录数（含基准目录）、videos = 已发现的视频数、
// cur = 当前正在读的目录（相对基准目录、斜杠分隔；基准目录为 ""）。
// 三个值都是真的数出来的，不是估算。
type ScanProgressFunc func(dirs, videos int, cur string)

// scanReporter 是扫描进度的节流器：首个目录立刻上报一次，之后按目录数/时间节流；
// 收尾时必须用 force 补一次终值（终值要与最终结果逐字一致）。
type scanReporter struct {
	onScan   ScanProgressFunc
	lastDirs int
	lastTime time.Time
	started  bool
}

func (r *scanReporter) report(dirs, videos int, cur string, force bool) {
	if r == nil || r.onScan == nil {
		return
	}
	if !force && r.started &&
		dirs-r.lastDirs < scanReportEveryDirs &&
		time.Since(r.lastTime) < scanReportEveryTime {
		return
	}
	r.started = true
	r.lastDirs, r.lastTime = dirs, time.Now()
	r.onScan(dirs, videos, cur)
}

// ScanLimits 是一次递归扫描的上限；零值表示用默认值（见 normalized）。
type ScanLimits struct {
	MaxDepth int
	MaxFiles int
}

func (l ScanLimits) normalized() ScanLimits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxScanDepth
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxScanFiles
	}
	return l
}

// ScanSkip 是一条被跳过的子目录（无权限 / 符号链接），面板与任务日志如实展示。
type ScanSkip struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
}

// ScanReport 是递归扫描的如实说明（跳过了什么、哪里到了上限）。
type ScanReport struct {
	Skipped []ScanSkip `json:"skipped,omitempty"`
	Notes   []string   `json:"notes,omitempty"`
	// DirsScanned 是这次真的读完的目录数（含基准目录）—— 进度回调的终值必须等于它。
	DirsScanned int `json:"dirs_scanned,omitempty"`
}

// scanTree 递归扫描 dir 下的视频，outDir 是产物目录（必须跳过）。
//
// 返回的 Source.RelPath 是相对 dir 的斜杠路径（如 `第1季/01.mkv`）；
// 单个子目录读不到只记入 report，绝不整体失败。
//
// ctx 取消时在每个目录/每条目循环处立即返回 ctx.Err()（调用方不得再产生计划）；
// onScan 在扫描过程中按节流回调真实进度（可为 nil）。
func scanTree(ctx context.Context, dir, outDir string, limits ScanLimits, onScan ScanProgressFunc) ([]Source, ScanReport, error) {
	lim := limits.normalized()
	var out []Source
	var rep ScanReport
	prog := &scanReporter{onScan: onScan}
	stopped := false
	lastRel := ""

	hitFiles := func() bool {
		if len(out) < lim.MaxFiles {
			return false
		}
		if !stopped {
			stopped = true
			rep.Notes = append(rep.Notes,
				fmt.Sprintf("视频数量达到上限，未继续（上限 %d 个），请分批处理", lim.MaxFiles))
		}
		return true
	}

	var walk func(cur, rel string, depth int) error
	walk = func(cur, rel string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err // 取消：立刻停，绝不继续扫
		}
		if hitFiles() {
			return nil
		}
		ents, err := os.ReadDir(cur)
		if err != nil {
			if rel == "" {
				return err // 基准目录读不到 = 整个规划失败（与非递归口径一致）
			}
			rep.Skipped = append(rep.Skipped, ScanSkip{RelPath: filepath.ToSlash(rel),
				Reason: "读不到子目录（无权限或已失效）：" + oneLine(err.Error())})
			return nil
		}
		rep.DirsScanned++
		lastRel = rel
		prog.report(rep.DirsScanned, len(out), rel, false)
		for _, e := range ents {
			if err := ctx.Err(); err != nil {
				return err // 目录里条目很多时也要能立刻停（每层循环都查）
			}
			if hitFiles() {
				return nil
			}
			name := e.Name()
			// 符号链接（目录或文件）一律不跟：绝不走出基准目录。
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			p := filepath.Join(cur, name)
			if e.IsDir() {
				if strings.HasPrefix(name, ".") {
					continue // 隐藏目录：与文件管理器默认口径一致
				}
				if depth+1 > lim.MaxDepth {
					rep.Notes = append(rep.Notes, fmt.Sprintf(
						"子目录深度达到上限，未继续（上限 %d 层）：%s",
						lim.MaxDepth, filepath.ToSlash(filepath.Join(rel, name))))
					continue
				}
				if isSameDir(p, outDir) {
					continue // 产物目录不许被扫进来（含任何指向它的路径）
				}
				if werr := walk(p, filepath.Join(rel, name), depth+1); werr != nil {
					return werr
				}
				continue
			}
			if !IsVideoName(name) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			out = append(out, Source{
				Name: name, Path: p,
				RelPath: filepath.ToSlash(filepath.Join(rel, name)),
				Bytes:   info.Size(), ModTime: info.ModTime(),
			})
			prog.report(rep.DirsScanned, len(out), rel, false)
		}
		return nil
	}
	if err := walk(dir, "", 0); err != nil {
		return nil, rep, err
	}
	// 收尾补一次终值：与最终结果逐字一致（调用方据此断言"进度不是估的"）。
	prog.report(rep.DirsScanned, len(out), lastRel, true)
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, rep, nil
}

// isSameDir 判断 a 是不是 output 目录本身（含解析软链接后相同的路径）。
//
// 软链接在遍历时已被拒，这里再兜一层：基准目录被解析过、output 是真实路径时，
// 任何"换个写法/换条路径指向同一目录"的情况都不许钻进来。
func isSameDir(a, outDir string) bool {
	outDir = strings.TrimSpace(outDir)
	if outDir == "" {
		return false
	}
	a, outDir = filepath.Clean(a), filepath.Clean(outDir)
	if a == outDir {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(outDir)
	return err1 == nil && err2 == nil && ra == rb
}
