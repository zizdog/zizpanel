package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ============================================================================
//  批量文件操作（复制 / 移动 / 删除）的**带进度**实现
//
//  为什么单独一层：这三件事在面板里已经放进任务中心 —— 用户从一个盘向另一个盘
//  剪切粘贴 80 个 100MB 的文件时，同步 HTTP 请求会让浏览器干等几分钟，
//  中途刷新还会掐断请求（用户报障原文："过程中没有任何进度提示"）。
//
//  单条目核心（copyOne / moveOne）只有一份实现，老的 Copy/Move 是它的
//  "无进度、单条目"薄封装 —— 行为与错误链完全一致，既有调用方与单测不受影响。
//
//  取消语义（最要紧的一条）：
//    · 跨卷移动本质是"复制 + 删源"，**中断时绝不删源文件**；
//    · 没写完的目标（半成品）一律删掉，绝不留一个"看起来成功、其实残缺"的副本；
//    · 已经完成的条目保持完成，没轮到的条目原样不动（汇总里如实报"未处理 N 项"）。
// ============================================================================

// OpPhase 是批量操作当前所处的阶段。
type OpPhase string

const (
	// OpPhaseScan 是"正在统计大小"阶段（大目录的 stat 也可能要几秒，必须可见）。
	OpPhaseScan   OpPhase = "scan"
	OpPhaseCopy   OpPhase = "copy"
	OpPhaseMove   OpPhase = "move"
	OpPhaseDelete OpPhase = "delete"
)

// OpProgress 是一次批量操作的结构化进度（字节级 + 文件级）。
//
// FilesDone/FilesTotal 的分子分母口径一致：都是**文件个数**（目录按其中包含的文件数
// 展开统计），所以粘贴 80 个文件是 80，粘贴一个含 200 个文件的目录也是 200。
// 注意与 Scanned 的区别：统计阶段总量还没出来，用 Scanned 报"已扫多少项"；
// 执行阶段才用 FilesDone/FilesTotal。两者分开，进度才是单调的。
type OpProgress struct {
	Phase OpPhase
	// Scanned 只在 scan 阶段有意义（已扫描的条目数）。
	Scanned    int
	FilesDone  int
	FilesTotal int
	DoneBytes  int64
	TotalBytes int64
	// Current 是当前正在处理的条目名（基名）。
	Current string
}

// OpProgressFunc 是进度回调。它可能被**高频**调用（按块复制），
// 限流（多久推一次、多少行日志）由调用方负责。
type OpProgressFunc func(OpProgress)

// FilePair 是一次复制/移动的源与目标。
type FilePair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// OpItem 是批量操作里一条的结局（逐条如实报，失败不许吞）。
type OpItem struct {
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	Name string `json:"name"`
	// Skipped 为 true 表示按用户选择跳过了（目标已存在 / 剪切到原目录）。
	Skipped       bool   `json:"skipped,omitempty"`
	SkippedReason string `json:"skipped_reason,omitempty"`
	// Overwritten 表示确实删掉了目标处的原有内容再写入。
	Overwritten bool `json:"overwritten,omitempty"`
	// Way 是移动实际采用的方式：rename / copy+delete。
	Way   string `json:"way,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	Error string `json:"error,omitempty"`
	// err 是原始错误（不导出、不进 JSON）：单条目路径靠它保住错误链
	// （调用方要用 errors.Is 判 ErrForbidden / EXDEV）。
	err error
}

// BatchResult 是一次批量操作（复制/移动/删除）的汇总。
type BatchResult struct {
	Op      string `json:"op"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	// Pending 是**没轮到**（被中断）的条目数。中断时它非零，且这些条目原样未动。
	Pending int `json:"pending"`
	// CrossVolume 是移动里"跨卷：复制后删除源"的条目数（用户要能一眼看出这次
	// 移动是瞬时的 rename 还是搬了数据 —— 两者的风险与耗时完全不同）。
	CrossVolume int      `json:"cross_volume,omitempty"`
	Bytes       int64    `json:"bytes"`
	Msg         string   `json:"msg"`
	Items       []OpItem `json:"items"`
}

// SetRenameFuncForTest 替换 rename 实现，返回恢复函数。
//
// 为什么必须导出：跨卷回退（EXDEV → 复制后删除）是移动语义里最容易写错的一条
// 分支，而单测环境里所有临时目录都在同一卷上，**根本触发不到 EXDEV**。
// 生产路径永远是 os.Rename；注入只发生在单测里（与 SetBrewUsesProbeForTest 同一套做法）。
func SetRenameFuncForTest(fn func(oldpath, newpath string) error) (restore func()) {
	prev := renameFunc
	if fn == nil {
		renameFunc = os.Rename
	} else {
		renameFunc = fn
	}
	return func() { renameFunc = prev }
}

// isCrossDevice 判断一次 rename 失败是不是"跨卷"（EXDEV）。
//
// 单一实现：单条目 Move 与批量 MoveItems 共用同一个判据 ——
// 两处各写一份的话，"跨卷回退复制"这条最难测的分支迟早会漂移。
func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// batchProgress 贯穿一次批量操作的计数器 + 回调。
type batchProgress struct {
	phase      OpPhase
	scanned    int
	filesDone  int
	filesTotal int
	doneBytes  int64
	totalBytes int64
	current    string
	fn         OpProgressFunc
}

func (b *batchProgress) report() {
	if b == nil || b.fn == nil {
		return
	}
	b.fn(OpProgress{
		Phase: b.phase, Scanned: b.scanned, FilesDone: b.filesDone, FilesTotal: b.filesTotal,
		DoneBytes: b.doneBytes, TotalBytes: b.totalBytes, Current: b.current,
	})
}

// workItem 是一条待处理条目（带统计出的文件数与字节量）。
type workItem struct {
	from  string
	to    string
	files int
	bytes int64
}

// countPath 统计一个条目的文件数与字节量（只看 stat，不读内容）。
//
// 软链接按"1 个文件、0 字节"计（复制的是链接本身，不复制它指向的数据）。
// ctx 取消时立即返回错误 —— 否则一个大目录的统计会让"中断"按钮失灵。
func countPath(ctx context.Context, p string) (int, int64, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return 0, 0, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return 1, 0, nil
	}
	if !st.IsDir() {
		return 1, st.Size(), nil
	}
	files := 0
	var bytes int64
	err = filepath.WalkDir(p, func(_ string, d os.DirEntry, werr error) error {
		// 单个子项读不到不中断统计（它会在执行阶段如实失败），
		// 但取消必须立刻生效。
		if werr != nil {
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		files++
		if info.Mode()&os.ModeSymlink == 0 {
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes, err
}

// scanTotals 先统计总量（这步本身也报进度，phase=scan），再进入执行阶段。
func scanTotals(ctx context.Context, items []workItem, b *batchProgress) error {
	b.phase = OpPhaseScan
	b.scanned = 0
	b.filesTotal = 0
	b.report()
	for i := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		items[i].files, items[i].bytes, _ = countPath(ctx, items[i].from)
		b.filesTotal += items[i].files
		b.totalBytes += items[i].bytes
		b.scanned = i + 1
		b.current = filepath.Base(items[i].from)
		b.report()
	}
	return nil
}

// copyFileProgress 复制单个文件：按 1MB 块累计字节并检查取消。
func copyFileProgress(ctx context.Context, src, dst string, perm os.FileMode, b *batchProgress) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			if b != nil {
				b.doneBytes += int64(n)
				b.report()
			}
		}
		if rerr == io.EOF {
			// 取消优先：正好读完也算取消（否则调用方可能继续删源文件）。
			if err := ctx.Err(); err != nil {
				return err
			}
			if b != nil {
				b.filesDone++
			}
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// copyTreeProgress 递归复制（与 copyPath 语义一致），带 ctx 与字节进度。
//
// b 可以为 nil（老的单条目同步路径）；此时不做任何进度上报。
func copyTreeProgress(ctx context.Context, src, dst string, b *batchProgress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		// 复制软链接本身，而不是它指向的内容（避免意外复制到根目录之外）
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		if b != nil {
			b.filesDone++
		}
		return nil
	}
	if st.IsDir() {
		if err := os.MkdirAll(dst, st.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTreeProgress(ctx, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), b); err != nil {
				return err
			}
		}
		return nil
	}
	return copyFileProgress(ctx, src, dst, st.Mode().Perm(), b)
}

// CopyResult 是一次单条目复制的结果（批量路径要它来如实报"改了名/覆盖了/跳过了"）。
type CopyResult struct {
	From string
	To   string
	// Overwritten 表示覆盖了目标处的原有内容。
	Overwritten bool
	// Skipped 表示按 onConflict=skip 跳过了（目标已存在）。
	Skipped       bool
	SkippedReason string
}

// copyOne 是单条目复制的核心（老 Copy 与批量 CopyItems 共用）。
//
// onConflict 空串 = "目标已存在就报错"（保留老 Copy 的行为，绝不静默覆盖）。
func (m *Manager) copyOne(ctx context.Context, from, to, onConflict string, b *batchProgress) (*CopyResult, error) {
	src, err := m.Resolve(from, false)
	if err != nil {
		return nil, err
	}
	dst, err := m.Resolve(to, true)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		return nil, fmt.Errorf("不能把目录复制到它自己的子目录中")
	}
	res := &CopyResult{From: src, To: dst}
	if _, statErr := os.Lstat(dst); statErr == nil {
		switch strings.ToLower(strings.TrimSpace(onConflict)) {
		case MoveConflictSkip:
			return &CopyResult{From: src, To: dst, Skipped: true, SkippedReason: "目标已存在"}, nil
		case MoveConflictOverwrite:
			if err := m.removeTargetForOverwrite(dst); err != nil {
				return nil, err
			}
			res.Overwritten = true
		case MoveConflictRename:
			dst = uniquePath(dst)
			res.To = dst
		default:
			return nil, fmt.Errorf("目标已存在: %s", to)
		}
	}
	if err := copyTreeProgress(ctx, src, dst, b); err != nil {
		// 半成品必须删掉：留下一个残缺副本比直接失败更危险。
		_ = os.RemoveAll(dst)
		return nil, err
	}
	m.chownRealUser(dst)
	return res, nil
}

// moveOne 是老 Move 的完整语义（含跨卷回退），抽出来给批量路径共用。
func (m *Manager) moveOne(ctx context.Context, from, to, onConflict string, b *batchProgress) (*MoveResult, error) {
	src, err := m.Resolve(from, false)
	if err != nil {
		return nil, err
	}
	dst, err := m.Resolve(to, true)
	if err != nil {
		return nil, err
	}
	if src == dst {
		return &MoveResult{From: src, To: dst, Way: MoveStrategyNone}, nil
	}
	// 不允许把目录移进它自己的子目录（会失败并可能损坏数据）
	if strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		return nil, fmt.Errorf("不能把目录移动到它自己的子目录中")
	}

	res := &MoveResult{From: src, To: dst}
	if _, statErr := os.Lstat(dst); statErr == nil {
		switch strings.ToLower(strings.TrimSpace(onConflict)) {
		case MoveConflictSkip:
			res.Skipped = true
			res.Way = MoveStrategyNone
			return res, nil
		case MoveConflictOverwrite:
			if err := m.removeTargetForOverwrite(dst); err != nil {
				return nil, err
			}
			res.Overwritten = true
		default:
			res.To = uniquePath(dst)
			dst = res.To
		}
	}

	if err := renameFunc(src, dst); err == nil {
		res.Way = MoveStrategyRename
		m.chownRealUser(dst)
		return res, nil
	} else if !isCrossDevice(err) {
		return nil, fmt.Errorf("移动失败: %w", err)
	}

	// 跨卷：Rename 无法跨设备，回退为复制后删除。
	if err := copyTreeProgress(ctx, src, dst, b); err != nil {
		_ = os.RemoveAll(dst) // 清掉写了一半的目标，别留下残缺副本
		if ctx.Err() != nil {
			// **绝不删源**：源文件还是唯一完整的那一份。
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("跨卷移动失败（复制阶段）: %w", err)
	}
	// 删源之前**再确认一次没被取消**：这是"中断绝不删源"的最后一道闸。
	if cerr := ctx.Err(); cerr != nil {
		_ = os.RemoveAll(dst)
		return nil, cerr
	}
	if err := os.RemoveAll(src); err != nil {
		return nil, fmt.Errorf("跨卷移动：内容已复制到 %s，但删除源 %s 失败（源仍在，请手动清理）: %w",
			dst, src, err)
	}
	res.Way = MoveStrategyCopyDelete
	m.chownRealUser(dst)
	return res, nil
}

// CopyItems 批量复制（面板的"粘贴"走它）。
//
// onConflict 语义与 Move 一致：skip / overwrite / rename；空串表示
// "目标已存在就报错"（保留老 Copy 的行为）。ctx 取消时返回部分结果 + ctx.Err()，
// 当前条目的半成品已删掉、没轮到的条目原样未动。
func (m *Manager) CopyItems(ctx context.Context, pairs []FilePair, onConflict string, onProgress OpProgressFunc) (*BatchResult, error) {
	items := make([]workItem, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, workItem{from: p.From, to: p.To})
	}
	res := &BatchResult{Op: "copy", Total: len(items), Items: make([]OpItem, 0, len(items))}
	b := &batchProgress{fn: onProgress}
	if err := scanTotals(ctx, items, b); err != nil {
		res.Pending = len(items)
		res.Msg = cancelMsg(res, "复制")
		return res, err
	}

	for i, it := range items {
		if err := ctx.Err(); err != nil {
			res.Pending = len(items) - i
			res.Msg = cancelMsg(res, "复制")
			return res, err
		}
		b.phase = OpPhaseCopy
		b.current = filepath.Base(it.from)
		b.report()

		item := OpItem{From: it.from, To: it.to, Name: filepath.Base(it.from), Bytes: it.bytes}
		cr, cerr := m.copyOne(ctx, it.from, it.to, onConflict, b)
		if cerr != nil {
			if ctx.Err() != nil {
				res.Pending = len(items) - i
				res.Msg = cancelMsg(res, "复制")
				return res, ctx.Err()
			}
			res.failed(&item, cerr, b)
			continue
		}
		item.From, item.To = cr.From, cr.To
		item.Overwritten = cr.Overwritten
		item.Skipped, item.SkippedReason = cr.Skipped, cr.SkippedReason
		if cr.Skipped {
			res.Skipped++
			b.filesDone += it.files // 跳过也算"处理过了"，否则进度条永远到不了头
		} else {
			res.Done++
			res.Bytes += it.bytes
		}
		res.Items = append(res.Items, item)
		b.report()
	}
	res.Msg = doneMsg(res, "复制")
	return res, nil
}

// MoveItems 批量移动（剪切粘贴走它）：单条目语义完全走 moveOne（跨卷回退也在那里）。
func (m *Manager) MoveItems(ctx context.Context, pairs []FilePair, onConflict string, onProgress OpProgressFunc) (*BatchResult, error) {
	items := make([]workItem, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, workItem{from: p.From, to: p.To})
	}
	res := &BatchResult{Op: "move", Total: len(items), Items: make([]OpItem, 0, len(items))}
	b := &batchProgress{fn: onProgress}
	if err := scanTotals(ctx, items, b); err != nil {
		res.Pending = len(items)
		res.Msg = cancelMsg(res, "移动")
		return res, err
	}

	for i, it := range items {
		if err := ctx.Err(); err != nil {
			res.Pending = len(items) - i
			res.Msg = cancelMsg(res, "移动")
			return res, err
		}
		b.phase = OpPhaseMove
		b.current = filepath.Base(it.from)
		b.report()

		item := OpItem{From: it.from, To: it.to, Name: filepath.Base(it.from), Bytes: it.bytes}
		mr, merr := m.moveOne(ctx, it.from, it.to, onConflict, b)
		if merr != nil {
			if ctx.Err() != nil {
				res.Pending = len(items) - i
				res.Msg = cancelMsg(res, "移动")
				return res, ctx.Err()
			}
			res.failed(&item, merr, b)
			continue
		}
		item.From, item.To = mr.From, mr.To
		item.Overwritten, item.Way = mr.Overwritten, string(mr.Way)
		item.Skipped = mr.Skipped
		if mr.Skipped {
			res.Skipped++
			b.filesDone += it.files
		} else {
			res.Done++
			res.Bytes += it.bytes
			if mr.Way == MoveStrategyCopyDelete {
				res.CrossVolume++
			}
			if mr.Way == MoveStrategyRename {
				// 同卷 rename 是瞬时的：一次性把它包含的文件与字节全记上。
				b.doneBytes += it.bytes
				b.filesDone += it.files
			}
		}
		res.Items = append(res.Items, item)
		b.report()
	}
	res.Msg = doneMsg(res, "移动")
	return res, nil
}

// DeleteItems 批量删除（保持原有语义：recursive=false 时目录必须为空）。
//
// 进度按顶层条目推进：目录的字节量来自统计阶段，删完一个条目记一次。
// （os.RemoveAll 不接受逐字节回调；改成手工递归删除会改变删除语义，不值得。）
func (m *Manager) DeleteItems(ctx context.Context, paths []string, recursive bool, onProgress OpProgressFunc) (*BatchResult, error) {
	items := make([]workItem, 0, len(paths))
	for _, p := range paths {
		items = append(items, workItem{from: p})
	}
	res := &BatchResult{Op: "delete", Total: len(items), Items: make([]OpItem, 0, len(items))}
	b := &batchProgress{fn: onProgress}
	if err := scanTotals(ctx, items, b); err != nil {
		res.Pending = len(items)
		res.Msg = cancelMsg(res, "删除")
		return res, err
	}

	for i, it := range items {
		if err := ctx.Err(); err != nil {
			res.Pending = len(items) - i
			res.Msg = cancelMsg(res, "删除")
			return res, err
		}
		b.phase = OpPhaseDelete
		b.current = filepath.Base(it.from)
		b.report()

		item := OpItem{From: it.from, Name: filepath.Base(it.from), Bytes: it.bytes}
		if err := m.Delete(it.from, recursive); err != nil {
			res.failed(&item, err, b)
			continue
		}
		res.Done++
		res.Bytes += it.bytes
		res.Items = append(res.Items, item)
		b.doneBytes += it.bytes
		b.filesDone += it.files
		b.report()
	}
	res.Msg = doneMsg(res, "删除")
	return res, nil
}

// removeTargetForOverwrite 删掉"要被覆盖"的目标（绝不删白名单根目录本身）。
func (m *Manager) removeTargetForOverwrite(dst string) error {
	for _, root := range m.roots {
		if dst == root {
			return fmt.Errorf("不允许覆盖根目录: %s", root)
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("覆盖目标失败: %w", err)
	}
	return nil
}

// failed 记一条失败（集中一处，免得每个分支各写一遍计数与追加）。
//
// 刻意**不**动 filesDone：分子是"已完成的文件数"，失败项没完成就不该计数，
// 进度条停在不到 100% 才是如实的。
func (r *BatchResult) failed(item *OpItem, err error, _ *batchProgress) {
	item.err = err
	item.Error = err.Error()
	r.Failed++
	r.Items = append(r.Items, *item)
}

// SrcErrors 返回逐条失败的原始错误（单条目路径靠它保住错误链）。
// 顺序与 Items 一致，只包含失败项。
func (r *BatchResult) SrcErrors() []error {
	var out []error
	for i := range r.Items {
		if r.Items[i].err != nil {
			out = append(out, r.Items[i].err)
		}
	}
	return out
}

// doneMsg / cancelMsg 是给用户看的汇总（一句话，前端直接显示）。
func doneMsg(r *BatchResult, verb string) string {
	msg := fmt.Sprintf("%s完成：成功 %d 项", verb, r.Done)
	if r.Skipped > 0 {
		msg += fmt.Sprintf("，跳过 %d 项", r.Skipped)
	}
	if r.Failed > 0 {
		msg += fmt.Sprintf("，失败 %d 项", r.Failed)
	}
	return msg + crossVolumeNote(r)
}

// crossVolumeNote：跨卷移动必须单独说一句（那是"复制 + 删源"，与瞬时改名不是一回事）。
func crossVolumeNote(r *BatchResult) string {
	if r.CrossVolume <= 0 {
		return ""
	}
	return fmt.Sprintf("（其中 %d 项跨卷：复制后删除源）", r.CrossVolume)
}

// cancelMsg 说清"哪些完成了、哪些没做" —— 中断时最要紧的就是这句。
func cancelMsg(r *BatchResult, verb string) string {
	msg := fmt.Sprintf("%s已中断：成功 %d 项", verb, r.Done)
	if r.Skipped > 0 {
		msg += fmt.Sprintf("，跳过 %d 项", r.Skipped)
	}
	if r.Failed > 0 {
		msg += fmt.Sprintf("，失败 %d 项", r.Failed)
	}
	if r.Pending > 0 {
		msg += fmt.Sprintf("，未处理 %d 项（原样未动）", r.Pending)
	}
	return msg + crossVolumeNote(r)
}
