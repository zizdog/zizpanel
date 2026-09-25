package videoopt

// place.go —— 跳过的文件**原样放进 output/**（用户要的工作流：跑完 output/ 就是完整一套）。
//
// 硬链接优先：output/ 就在源文件同目录下，**必然同卷** ⇒ 零额外空间、瞬时。
// 硬链接失败（EXDEV/不支持/EEXIST…）退回复制：**先查剩余空间**，不够就不复制并如实
// 回报"空间不足，未放入 output"；复制先写 <目标>.part 再改名，绝不半个文件留在那儿。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// 放法（RunItem.Placement）：link=硬链接、copy=复制、none=没放进 output。
const (
	PlacementLink = "link"
	PlacementCopy = "copy"
	PlacementNone = "none"
)

// SkippedListName 是 output/ 里那份**给人看**的已跳过清单（纯文本，不是 JSON）。
const SkippedListName = "_已跳过清单.txt"

// linkFile / freeBytes 是执行期的两个外部依赖：门禁注入"link 失败""空间不足"，
// 不必真去构造 EXDEV 卷或把磁盘写满。其余代码只走 placeFile。
var (
	linkFile  = os.Link
	freeBytes = freeSpace
)

// freeSpace 返回路径所在卷的可用字节数；读不到返回 ok=false（不拦复制）。
func freeSpace(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}

// PlaceResult 是一次"原样放入 output/"的结果。
type PlaceResult struct {
	Placement string
	Reason    string
	Bytes     int64
}

// placeFile 把 src 原样放进 dst（硬链接优先，失败退复制）。
func placeFile(src, dst string) PlaceResult {
	src, dst = strings.TrimSpace(src), strings.TrimSpace(dst)
	if src == "" || dst == "" {
		return PlaceResult{Placement: PlacementNone, Reason: "缺少源文件或目标路径"}
	}
	st, err := os.Lstat(src)
	if err != nil {
		return PlaceResult{Placement: PlacementNone, Reason: "读不到源文件：" + oneLine(err.Error())}
	}
	if !st.Mode().IsRegular() {
		return PlaceResult{Placement: PlacementNone, Reason: "源不是普通文件"}
	}
	size := st.Size()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return PlaceResult{Placement: PlacementNone, Bytes: size,
			Reason: "创建 output 目录失败：" + oneLine(err.Error())}
	}
	// 目标已存在：output 里已经有东西，绝不覆盖、也绝不重复链接/复制。
	if _, err := os.Lstat(dst); err == nil {
		return PlaceResult{Placement: PlacementNone, Bytes: size,
			Reason: "output 里已有同名文件，未重复放入"}
	}
	linkErr := linkFile(src, dst)
	if linkErr == nil {
		return PlaceResult{Placement: PlacementLink, Bytes: size, Reason: "同卷硬链接，不占额外空间"}
	}
	// 退回复制前先查剩余空间：不够就不复制，绝不把磁盘写爆、也绝不半个文件留下。
	if free, ok := freeBytes(filepath.Dir(dst)); ok && free < size {
		return PlaceResult{Placement: PlacementNone, Bytes: size,
			Reason: fmt.Sprintf("空间不足，未放入 output（需 %s，只剩 %s）", humanBytes(size), humanBytes(free))}
	}
	if err := copyFile(src, dst, st.Mode().Perm()); err != nil {
		return PlaceResult{Placement: PlacementNone, Bytes: size,
			Reason: "复制失败：" + oneLine(err.Error())}
	}
	return PlaceResult{Placement: PlacementCopy, Bytes: size,
		Reason: "硬链接失败（" + oneLine(linkErr.Error()) + "），已复制"}
}

// copyFile 先写 <dst>.part 再改名：失败时删掉半成品，绝不留半个文件冒充成品。
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	part := dst + ".part"
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(part)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		_ = os.Remove(part)
		return err
	}
	return nil
}

// writeSkippedList 在 output/ 写一份给人看的《已跳过清单》（逐条：文件名 / 原相对路径 /
// 大小 / 视频码率 / 分辨率 / 跳过原因 / 放入方式）。
func writeSkippedList(outDir string, res *RunResult, items []RunItem) (string, error) {
	path := filepath.Join(outDir, SkippedListName)
	var b strings.Builder
	b.WriteString("ZizPanel 视频压缩 · 已跳过清单（未转码的文件，已原样放进 output/）\n")
	fmt.Fprintf(&b, "生成时间：%s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "共 %d 个跳过：码率已到极限 %d 个、其它 %d 个\n\n",
		res.Skipped, res.CappedSkipped, res.Skipped-res.CappedSkipped)
	n := 0
	for _, it := range items {
		if it.SkipReason == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n", n, it.Name)
		rel := it.RelPath
		if rel == "" {
			rel = it.Name
		}
		fmt.Fprintf(&b, "   原相对路径：%s\n", rel)
		fmt.Fprintf(&b, "   大小：%s\n", humanBytes(it.Before))
		if it.SourceKbps > 0 {
			fmt.Fprintf(&b, "   视频码率：%d kbps\n", it.SourceKbps)
		} else {
			b.WriteString("   视频码率：读不到\n")
		}
		if it.SourceWidth > 0 && it.SourceHeight > 0 {
			fmt.Fprintf(&b, "   分辨率：%dx%d\n", it.SourceWidth, it.SourceHeight)
		} else {
			b.WriteString("   分辨率：读不到\n")
		}
		fmt.Fprintf(&b, "   跳过原因：%s\n", it.SkipReason)
		fmt.Fprintf(&b, "   放入方式：%s\n\n", placeDescribe(it))
	}
	if n == 0 {
		b.WriteString("（本次没有跳过的文件）\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return path, err
	}
	return path, nil
}

// placeDescribe 把放法写成一句人话（清单里那一行）。
func placeDescribe(it RunItem) string {
	switch it.Placement {
	case PlacementLink:
		return "硬链接（不占额外空间）" + placedAt(it)
	case PlacementCopy:
		return "复制" + placedAt(it)
	default:
		if it.PlaceReason != "" {
			return "未放入（" + it.PlaceReason + "）"
		}
		return "未放入"
	}
}

func placedAt(it RunItem) string {
	if strings.TrimSpace(it.PlaceName) == "" {
		return ""
	}
	return " → output/" + it.PlaceName
}
