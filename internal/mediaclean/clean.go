package mediaclean

// clean.go —— 「去内嵌封面 + 清广告元数据（无损流拷贝）」的单文件流程与批量循环。
//
// 单文件流程（每一步失败都**绝不改动源文件**）：
//   ① ffprobe 找 attached_pic 视频流号（内嵌封面/广告图）；
//   ② ffprobe 读 format_tags 的 title/comment/description；
//   ③ 两者都空 ⇒ 记 skipped「本来就是干净的」，一个字节都不写、一次 ffmpeg 都不调；
//   ④ 否则：ffmpeg 流拷贝（-c copy，绝不重编码），逐个 -map -0:<idx> 去掉内嵌封面，
//      并把三个标签清空；产物写到**同目录**临时名 .zp-clean-*<ext>（同文件系统）；
//   ⑤ 校验：产物存在且非空 + ffprobe 读得出时长、与输入时长差 < 1 秒；
//   ⑥ 通过 ⇒ 原文件改名 <名字>.bak（已存在就 .bak-2/.bak-3…，绝不覆盖既有备份），
//      临时文件再改名成原文件名；结果里报"省下 N 字节"（负数如实说没变小）。
//
// 失败/取消的语义（与代码保持一致，报告里也这么写）：
//   · **临时文件一律删除**（时长核对不过也删，不留 .zp-clean-* 残渣）；
//   · **源文件永远保留**；备份只在"校验通过、准备替换"的那一刻才产生，
//     替换失败会把 .bak 改回原名 ⇒ 失败/取消时 .bak 一个都不会多出来。

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zizdog/zizpanel/internal/files"
)

// supportedExts 是支持无损去广告的容器扩展名（用户点名的清单）。
var supportedExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true,
	".avi": true, ".ts": true, ".webm": true,
}

// IsSupported 判断文件名是不是支持的视频类型（只看扩展名）。
func IsSupported(name string) bool {
	return supportedExts[strings.ToLower(filepath.Ext(name))]
}

// SupportedExts 返回支持的扩展名（升序副本，给前端说明用）。
// 从 supportedExts 派生：扩展名清单只有一份，改一处不会两边漂移。
func SupportedExts() []string {
	out := make([]string, 0, len(supportedExts))
	for ext := range supportedExts {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// NotSupportedReason 是"不是支持的视频类型"的统一口径（结果里逐条显示）。
const NotSupportedReason = "不是支持的视频类型"

// tempPrefix 是我们自己的临时文件前缀：递归扫描必须跳过它（上次失败留下的残渣
// 不该被当成新素材再处理一遍）。
const tempPrefix = ".zp-clean-"

// Item 是一个待处理条目（收集阶段产出，Bytes 是源文件大小）。
type Item struct {
	Path       string
	Name       string
	Bytes      int64
	SkipReason string
}

// CollectResult 是收集阶段的结论。
type CollectResult struct {
	Items []Item
	// IgnoredUnsupported 是"在选中的文件夹里按扩展名排掉、不逐个上报"的文件数。
	IgnoredUnsupported int
}

// Collect 把选中的路径展开成待处理清单（只看扩展名，不跑 ffprobe/ffmpeg）。
//
// 语义：
//   - 直接选中的文件：受支持 ⇒ 待处理；其它类型 ⇒ 逐条记 skipped（用户看得见）；
//   - 直接选中的目录：递归找受支持的文件；其中的非视频只计数，不逐个列（避免噪音）；
//   - 软链接与非普通文件一律不碰（改名/写临时文件动的是链接本身，语义会骗人）；
//   - ctx 取消立即返回（大目录的遍历不会让"中断"失灵）。
func Collect(ctx context.Context, paths []string, onScan func(done int, current string)) (CollectResult, error) {
	var res CollectResult
	seen := make(map[string]bool)
	add := func(p string, size int64, reason string) {
		if seen[p] {
			return
		}
		seen[p] = true
		it := Item{Path: p, Name: filepath.Base(p), Bytes: size, SkipReason: reason}
		res.Items = append(res.Items, it)
		if onScan != nil {
			onScan(len(res.Items), it.Name)
		}
	}

	for _, root := range paths {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		st, err := os.Lstat(root)
		if err != nil {
			add(root, 0, "读不到："+oneLine(err.Error()))
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			add(root, 0, "是软链接，未处理")
			continue
		}
		if !st.IsDir() {
			reason := ""
			if !supportedExts[strings.ToLower(filepath.Ext(root))] {
				reason = NotSupportedReason
			}
			add(root, st.Size(), reason)
			continue
		}
		werr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // 单个子项读不到不中断整批（它不会进入清单）
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if d.IsDir() {
				return nil
			}
			// WalkDir 不跟随软链接；Info 用的是 Lstat 口径，所以这里能认出链接。
			info, ierr := d.Info()
			if ierr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil
			}
			if strings.HasPrefix(filepath.Base(p), tempPrefix) {
				return nil
			}
			if !supportedExts[strings.ToLower(filepath.Ext(p))] {
				res.IgnoredUnsupported++
				return nil
			}
			add(p, info.Size(), "")
			return nil
		})
		if werr != nil && ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	return res, nil
}

// Hooks 是执行期的外部依赖：结构化进度与产物归属（面板以 root 跑，写出来的文件要交还用户）。
type Hooks struct {
	OnProgress files.OpProgressFunc
	Chown      func(path string)
}

// OneResult 是单个文件的结局。
type OneResult struct {
	Skipped    bool
	SkipReason string
	SavedBytes int64
	// Note 是成功项"去掉了什么"的一句话（结果行显示，别只说一个笼统数字）。
	Note string
	Err  error
}

// CleanBatch 逐个处理清单，返回文件管理既有的 files.BatchResult（进度/结果只有一套结构）。
//
// 取消语义：当前文件临时文件已删、源未动；没轮到的条目原样未动并计入 Pending。
// ctx 取消时同时返回 ctx.Err()，任务中心才会如实标「已中断」（只回结果会被当成成功）。
func CleanBatch(ctx context.Context, items []Item, eng *Engine, h Hooks) (*files.BatchResult, error) {
	res := &files.BatchResult{Op: "media_clean", Total: len(items), Items: make([]files.OpItem, 0, len(items))}
	var totalBytes, doneBytes int64
	for _, it := range items {
		totalBytes += it.Bytes
	}
	alreadyClean := 0
	report := func(filesDone int, current string, inFile int64) {
		if h.OnProgress == nil {
			return
		}
		h.OnProgress(files.OpProgress{
			Phase: files.OpPhaseClean, FilesDone: filesDone, FilesTotal: len(items),
			DoneBytes: doneBytes + inFile, TotalBytes: totalBytes, Current: current,
		})
	}
	report(0, "", 0)

	for i, it := range items {
		if err := ctx.Err(); err != nil {
			res.Pending = len(items) - i
			res.Msg = cleanCancelMsg(res)
			return res, err
		}
		item := files.OpItem{From: it.Path, Name: it.Name, Bytes: it.Bytes}
		if it.SkipReason != "" {
			item.Skipped = true
			item.SkippedReason = it.SkipReason
			res.Skipped++
			res.Items = append(res.Items, item)
			doneBytes += it.Bytes
			report(i+1, it.Name, 0)
			continue
		}

		report(i, it.Name, 0)
		out := eng.cleanOne(ctx, it, h.Chown, func(written int64) {
			if written < 0 {
				written = 0
			}
			if written > it.Bytes {
				written = it.Bytes
			}
			report(i, it.Name, written)
		})
		switch {
		case out.Err != nil && ctx.Err() != nil:
			// 取消：临时文件已在 cleanOne 里删掉，源文件没动过。
			res.Pending = len(items) - i
			res.Msg = cleanCancelMsg(res)
			return res, ctx.Err()
		case out.Err != nil:
			item.Error = out.Err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
		case out.Skipped:
			item.Skipped = true
			item.SkippedReason = out.SkipReason
			res.Skipped++
			if out.SkipReason == cleanSkipReason {
				alreadyClean++
			}
			res.Items = append(res.Items, item)
		default:
			item.SavedBytes = out.SavedBytes
			item.Note = out.Note
			res.Done++
			res.Bytes += it.Bytes
			res.SavedBytes += out.SavedBytes
			res.Items = append(res.Items, item)
		}
		doneBytes += it.Bytes
		report(i+1, it.Name, 0)
	}
	res.Msg = cleanDoneMsg(res, alreadyClean)
	return res, nil
}

// cleanSkipReason 是"本来就是干净的"的统一文案。
const cleanSkipReason = "本来就是干净的"

// cleanOne 是单文件流程（见文件头注释）。
//
// 任何失败路径都保证：临时文件删除、源文件与 .bak 状态不变。
func (e *Engine) cleanOne(ctx context.Context, it Item, chown func(string), onWritten func(int64)) OneResult {
	// 封面有两种形态：MP4/MOV 的 attached_pic 视频流 + MKV 的图片附件（字体附件保留）。
	pics, err := e.attachedPics(ctx, it.Path)
	if err != nil {
		return OneResult{Err: err}
	}
	atts, err := e.imageAttachments(ctx, it.Path)
	if err != nil {
		return OneResult{Err: err}
	}
	tags, err := e.junkTags(ctx, it.Path)
	if err != nil {
		return OneResult{Err: err}
	}
	maps := mergeIndices(pics, atts)
	if len(maps) == 0 && len(tags) == 0 {
		// 干净文件：一次 ffmpeg 都不调、一个字节都不写。
		return OneResult{Skipped: true, SkipReason: cleanSkipReason}
	}
	inDur, err := e.duration(ctx, it.Path)
	if err != nil {
		return OneResult{Err: fmt.Errorf("读不出输入时长，未改动源文件：%w", err)}
	}
	tmp, err := tempPath(it.Path)
	if err != nil {
		return OneResult{Err: err}
	}
	// 失败/取消一律删临时文件：策略只有这一条（报告里也这么写）。
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if err := e.runCopy(ctx, CopyArgs(it.Path, tmp, maps), onWritten); err != nil {
		return OneResult{Err: err}
	}
	st, statErr := os.Stat(tmp)
	if statErr != nil {
		return OneResult{Err: fmt.Errorf("产物不存在，源文件未改动：%w", statErr)}
	}
	if st.Size() <= 0 {
		return OneResult{Err: fmt.Errorf("产物是空文件（0 字节），源文件未改动")}
	}
	outDur, derr := e.duration(ctx, tmp)
	if derr != nil {
		return OneResult{Err: fmt.Errorf("读不出产物时长，源文件未改动：%w", derr)}
	}
	if math.Abs(outDur-inDur) >= 1.0 {
		return OneResult{Err: fmt.Errorf("时长核对不过（原 %.3f 秒 → 新 %.3f 秒），源文件未改动", inDur, outDur)}
	}
	// 校验通过后、替换之前再确认一次没被取消 —— 取消时源文件必须完好。
	if cerr := ctx.Err(); cerr != nil {
		return OneResult{Err: cerr}
	}
	if err := replaceFile(it.Path, tmp); err != nil {
		return OneResult{Err: err}
	}
	tmp = "" // 已改名成正式文件，defer 不再删
	if chown != nil {
		chown(it.Path)
	}
	return OneResult{SavedBytes: it.Bytes - st.Size(), Note: cleanNote(len(pics), len(atts), tags)}
}

// mergeIndices 合并两类封面流号并去重（升序）：attached_pic 视频流 + MKV 图片附件。
func mergeIndices(a, b []int) []int {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[int]bool, len(a)+len(b))
	out := make([]int, 0, len(a)+len(b))
	for _, list := range [][]int{a, b} {
		for _, idx := range list {
			if !seen[idx] {
				seen[idx] = true
				out = append(out, idx)
			}
		}
	}
	sort.Ints(out)
	return out
}

// cleanNote 是结果行里"这一趟去掉了什么"的一句话（别只说一个笼统数字）。
func cleanNote(pics, atts int, tags []string) string {
	var parts []string
	if pics > 0 {
		parts = append(parts, fmt.Sprintf("视频流 %d 张", pics))
	}
	if atts > 0 {
		parts = append(parts, fmt.Sprintf("附件 %d 个", atts))
	}
	msg := ""
	if len(parts) > 0 {
		msg = "去内嵌封面（" + strings.Join(parts, " / ") + "）"
	}
	if len(tags) > 0 {
		t := "清空推广标签（" + strings.Join(tags, ", ") + "）"
		if msg == "" {
			msg = t
		} else {
			msg += "；" + t
		}
	}
	return msg
}

// CopyArgs 是"无损去广告"的 ffmpeg 参数（**唯一**映射点，门禁直接断言它）。
//
// 顺序即语义：-map 0 保留全部流；每个该去掉的流（attached_pic 视频流或 MKV 图片附件）
// 再来一条 -map -0:<idx> 把它剔掉；-c copy 是硬要求（绝不重编码）；
// 三个 -metadata 置空清掉推广文字。字体附件从不出现在 maps 里（见 imageAttachments）。
func CopyArgs(in, out string, attached []int) []string {
	args := []string{"-v", "error", "-nostdin", "-y", "-i", in, "-map", "0"}
	for _, idx := range attached {
		args = append(args, "-map", fmt.Sprintf("-0:%d", idx))
	}
	args = append(args, "-c", "copy", "-map_chapters", "0",
		"-metadata", "title=", "-metadata", "comment=", "-metadata", "description=",
		"-progress", "pipe:1", "-nostats", out)
	return args
}

// runCopy 跑一次流拷贝，并把 ffmpeg 的 total_size 当作文件内字节进度回报。
func (e *Engine) runCopy(ctx context.Context, args []string, onWritten func(int64)) error {
	if e == nil || strings.TrimSpace(e.Ffmpeg) == "" {
		return ErrEngineMissing
	}
	cmd := exec.CommandContext(ctx, e.Ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &errBuf, n: 4096}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("启动 ffmpeg 失败：%w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || key != "total_size" {
			continue
		}
		if n, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64); perr == nil && onWritten != nil {
			onWritten(n)
		}
	}
	werr := cmd.Wait()
	if werr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = werr.Error()
		}
		// 失败原因原样带出去（绝不换成一句"处理失败"）。
		return fmt.Errorf("ffmpeg 流拷贝失败：%s", msg)
	}
	return nil
}

// replaceFile 把校验通过的临时文件换成正式文件，原文件先改名成 .bak 保留。
//
// 关键不变量：要么"新文件就位 + .bak 存在"，要么"源文件原样、没有 .bak"。
// 所以替换失败时必须把 .bak 改回原名。
func replaceFile(orig, tmp string) error {
	bak := backupPath(orig)
	if bak == "" {
		return fmt.Errorf(".bak 序号已用尽（.bak / .bak-2 … 都存在），源文件未改动")
	}
	if err := renameFunc(orig, bak); err != nil {
		return fmt.Errorf("备份原文件失败（源文件未改动）：%w", err)
	}
	if err := renameFunc(tmp, orig); err != nil {
		if rerr := renameFunc(bak, orig); rerr != nil {
			// 还原也失败：如实把路径写清楚，绝不说"已恢复"。
			return fmt.Errorf("替换失败且备份还原也失败（源在 %s，请手动改名）：还原错误 %v；替换错误 %w", bak, rerr, err)
		}
		return fmt.Errorf("替换失败（已从 %s 还原源文件）：%w", filepath.Base(bak), err)
	}
	return nil
}

// backupPath 返回一个不存在的 .bak 名字：<名字>.bak，被占了就 <名字>.bak-2、.bak-3…
// 绝不覆盖既有备份（返回空串表示序号用尽）。
func backupPath(orig string) string {
	first := orig + ".bak"
	if pathFree(first) {
		return first
	}
	for i := 2; i <= 999; i++ {
		p := fmt.Sprintf("%s.bak-%d", orig, i)
		if pathFree(p) {
			return p
		}
	}
	return ""
}

// pathFree 只有"确实不存在"才算空闲：其它错误一律当作不空闲（宁可不替换也不覆盖）。
func pathFree(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, fs.ErrNotExist)
}

// tempPath 在**源文件同目录**创建临时名（同文件系统 ⇒ 后续改名是瞬时的、不跨卷拷贝）。
func tempPath(orig string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(orig), tempPrefix+"*"+filepath.Ext(orig))
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败：%w", err)
	}
	name := f.Name()
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("创建临时文件失败：%w", cerr)
	}
	return name, nil
}

// renameFunc 是替换实现的注入点（门禁要能造出"替换失败但源必须完好"这条分支）。
var renameFunc = os.Rename

// SetRenameFuncForTest 替换 rename 实现，返回恢复函数（与 files.SetRenameFuncForTest 同一做法）。
func SetRenameFuncForTest(fn func(oldpath, newpath string) error) (restore func()) {
	prev := renameFunc
	if fn == nil {
		renameFunc = os.Rename
	} else {
		renameFunc = fn
	}
	return func() { renameFunc = prev }
}

// cleanDoneMsg / cleanCancelMsg 是给用户看的一句话汇总（前端直接显示）。
func cleanDoneMsg(res *files.BatchResult, alreadyClean int) string {
	msg := fmt.Sprintf("去广告完成：成功 %d 个", res.Done)
	if res.Done > 0 {
		if res.SavedBytes > 0 {
			msg += "，共省 " + humanBytes(res.SavedBytes)
		} else {
			msg += "（没有变小）"
		}
	}
	if res.Skipped > 0 {
		other := res.Skipped - alreadyClean
		msg += fmt.Sprintf("，跳过 %d 个（本来就是干净的 %d / 其它 %d）", res.Skipped, alreadyClean, other)
	}
	if res.Failed > 0 {
		msg += fmt.Sprintf("，失败 %d 个", res.Failed)
	}
	return msg
}

func cleanCancelMsg(res *files.BatchResult) string {
	msg := fmt.Sprintf("去广告已中断：成功 %d 个", res.Done)
	if res.Skipped > 0 {
		msg += fmt.Sprintf("，跳过 %d 个", res.Skipped)
	}
	if res.Failed > 0 {
		msg += fmt.Sprintf("，失败 %d 个", res.Failed)
	}
	if res.Pending > 0 {
		msg += fmt.Sprintf("，未处理 %d 个（原样未动）", res.Pending)
	}
	return msg
}

// humanBytes 给日志/汇总用的体积文本。
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	idx := -1
	for v >= unit && idx < len(units)-1 {
		v /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", v, units[idx])
}
