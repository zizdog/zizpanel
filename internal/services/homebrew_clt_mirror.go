package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
//  命令行开发者工具（CLT）的镜像安装路径
//
//  为什么需要这条路：CLT 是 Homebrew 与 `/usr/bin/python3` 的前置依赖，
//  而全新 macOS 上两者都没有。苹果自己的两条路在国内网络都不可靠：
//
//    1) `softwareupdate -i "Command Line Tools for Xcode-16.2"` —— 包体在
//       swdist/swcdn.apple.com 上，真机实测（抹机后的 mini，无代理）：
//       15 分钟只下了 1 MB，随后彻底停住；DNS 还会被解析到假地址（198.18.0.67）。
//    2) `xcode-select --install` —— 弹图形对话框，无人值守/远程场景就是死等。
//
//  所以这里加了第三条：**从项目自己的镜像整包下载苹果原始 pkg，再本地安装**。
//  包体是苹果原封不动的产物（安装回执、pkgutil 记录都在），只是换了一条
//  国内可达的传输路径 —— 与面板升级源、应用市场镜像同一套思路。
//
//  镜像目录约定（见 `clt/index.json`）：
//
//     <mirror>/clt/index.json          —— 清单（下面的 cltIndex）
//     <mirror>/clt/<dir>/<pkg 文件名>   —— 苹果原始 pkg
//
//  清单里按 macOS 主版本给候选，逐个尝试；**成功过的那条排在最前**，
//  这样同型号机器第二次安装几乎不会踩到已经失败的条目。
// ============================================================================

// cltMirrorBaseDefault 是内置镜像地址（面板升级也用它，见配置里的 upgrade_source）。
const cltMirrorBaseDefault = "https://zizdog.com/zizpanel"

// cltInstallPkgs 是真正需要安装的组件：CLTools_Executables.pkg（576 MB，真正的
// 工具链，`/usr/bin/python3`、`clang`、`git` 都在里面）与 CLTools_macOSNMOS_SDK.pkg
// （55 MB，macOS SDK，编译要用的头文件）。
//
// 其余组件（CLTools_SwiftBackDeploy、CLTools_macOSLMOS_SDK）是给旧系统/旧编译器
// 回退用的，`CLTools_macOS_DevSDK_Remove_*` 是清理旧 SDK 的，都不装 ——
// 少下 105 MB，且不影响 Homebrew / Python / 编译。
var cltInstallPkgs = []string{"CLTools_Executables.pkg", "CLTools_macOSNMOS_SDK.pkg"}

// cltIndex 是镜像上的清单文件。
type cltIndex struct {
	// Updated 是维护时间，纯给人看。
	Updated string `json:"updated"`
	// Items 每一组是一份完整可用的 CLT 包集合。
	Items []cltIndexItem `json:"items"`
}

type cltIndexItem struct {
	// Name 人类可读的名字，会写进任务步骤，如 "Command Line Tools 16.2"。
	Name string `json:"name"`
	// Dir 组包在镜像上的子目录（相对 <mirror>/clt/）。留空则用 Path。
	Dir string `json:"dir"`
	// Path 是组包在镜像上的完整相对路径（相对 <mirror>/），供"按苹果产品号
	// 归档"的布局使用，如 "clt/072-44426-A"。与 Dir 二选一。
	Path string `json:"path"`
	// MaxOS 是这份包适用的最高 macOS 主版本；0 表示不限制。
	// 用最高而不是最低：CLT 只要求 macOS 不高于它编译时对应的版本。
	MaxOS int `json:"max_os"`
	// Pkgs 是包文件名列表（必须在 Dir/Path 里真的存在）。
	Pkgs []string `json:"pkgs"`
	// Bytes 是包体总大小，用于下载前估算与进度展示（0 表示不校验）。
	Bytes int64 `json:"bytes"`
	// SHA256 可选，形如 "CLTools_Executables.pkg=abc…"，提供就校验。
	SHA256 []string `json:"sha256"`
	// Parts 可选：把大包在镜像上切成固定大小的分片（`split -b 32m -d`）。
	// 给了就**分片并行下载 + 逐片校验 + 断点续传**，全部成功后拼回原文件
	// 再核对整体 sha256。实测这条网络单连接会抖动（偶发 connection reset），
	// 576 MB 单文件要 20~50 分钟且断一次就前功尽弃；分片之后断一片只重下那一片。
	Parts []cltPkgParts `json:"parts,omitempty"`
}

// cltPkgParts 是"某个包的分片列表"。
//
// 用 {pkg, parts} 而不是把包名拼进分片文件名：分片来自 `split`，
// 文件名形如 `CLTools_Executables.pkg.part-000`，两边取 basename 就能对上，
// 不必让镜像的命名去迁就客户端。
type cltPkgParts struct {
	// Pkg 是包文件名（与 Pkgs 里的名字一致）。
	Pkg string `json:"pkg"`
	// Parts 按拼接顺序排列 —— **顺序以清单为准**，不靠文件名排序
	// （`split` 默认后缀是字母序、`-d` 是数字序，靠排序太脆）。
	Parts []cltPart `json:"parts"`
}

// cltPart 是镜像上一个分片。
type cltPart struct {
	// File 是分片文件名（相对包所在目录）。
	File string `json:"file"`
	// Size 是分片字节数（最后一片可能更小）。
	Size int64 `json:"size"`
}

// partsFor 返回某个包的分片列表（没配置就返回 nil，表示走单文件下载）。
func (it cltIndexItem) partsFor(pkg string) []cltPart {
	for _, pp := range it.Parts {
		if pp.Pkg == pkg {
			return pp.Parts
		}
	}
	return nil
}

// filesBase 返回该条目的安装包在镜像上的基址（结尾不含 /）。
func (it cltIndexItem) filesBase(mirror string) string {
	if p := strings.Trim(it.Path, "/"); p != "" {
		return strings.TrimSuffix(mirror, "/") + "/" + p
	}
	return strings.TrimSuffix(mirror, "/") + "/clt/" + strings.Trim(it.Dir, "/")
}

// cltMirrorBase 返回 CLT 镜像基址（环境变量优先，方便沙箱测试）。
//
// 注意：这是**静态兜底**基址；真正"先探通不通"的优先级由
// cltMirrorBaseFor(ctx) 决定 —— 面板里配的镜像站能用就优先用它。
func cltMirrorBase() string {
	if v := strings.TrimSpace(os.Getenv("ZIZPANEL_CLT_MIRROR")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return cltMirrorBaseDefault
}

// cltMirrorBaseFor 按"优先级 + 可用性"挑 CLT 镜像基址。
//
// 顺序（2026-09-16 用户要求："能用这个地址的尽量用"）：
//  1. 面板设置里的镜像基址（自建 NAS，省流量、同城速度）—— 但**必须探通**
//     （HEAD 它的 clt/index.json），不通就跳过；
//  2. 环境变量 ZIZPANEL_CLT_MIRROR（测试/临时覆盖）；
//  3. 内置的静态镜像常量。
//
// 探测失败不报错：CLT 安装有三条路（镜像 → softwareupdate → 弹窗），
// 这里只是挑"镜像那条路走哪个基址"，挑不出来就交给后面的路。
func (m *Manager) cltMirrorBaseFor(ctx context.Context) string {
	if m.MirrorEnabled() {
		base := m.mirrorBase()
		probe := base + "/clt/index.json"
		if err := m.checkMirrorURL(ctx, probe); err == nil {
			return base
		}
	}
	return cltMirrorBase()
}

// macMajorVersion 解析 Darwin 主版本（23 → 14、24 → 15、25 → 26）。
//
// 为什么不用 `sw_vers`：这是纯函数、要好测；`runtime.GOOS` 之外的信息
// 只在 macOS 上有意义，非 macOS 直接返回 0（调用方会跳过镜像路径）。
func macMajorVersion(darwinMajor int) int {
	if darwinMajor <= 0 {
		return 0
	}
	return darwinMajor - 9
}

// pickCLTItem 从清单里挑出第一个适用于该 macOS 主版本的包集合。
//
// 抽成纯函数：清单是"运营数据"，选错会把不兼容的 CLT 装到机器上，
// 而这种错误在真机上极难复盘，所以必须有单测锁住。
func pickCLTItem(items []cltIndexItem, macMajor int) (cltIndexItem, bool) {
	for _, it := range items {
		if strings.Trim(it.Dir, "/") == "" && strings.Trim(it.Path, "/") == "" {
			continue
		}
		if len(it.Pkgs) == 0 {
			continue
		}
		if it.MaxOS != 0 && macMajor != 0 && macMajor > it.MaxOS {
			continue
		}
		return it, true
	}
	return cltIndexItem{}, false
}

// cltNeededPkgs 把清单条目收敛成"确实要下、要装"的那几个包。
//
// 清单可能列全了苹果的组件（包含 Remove_* 这类），这里只取白名单里的两个；
// 清单里没写白名单里的包也不算致命 —— 有的版本没有 NMOS_SDK。
func cltNeededPkgs(item cltIndexItem) []string {
	have := map[string]bool{}
	for _, p := range item.Pkgs {
		have[p] = true
	}
	var out []string
	for _, want := range cltInstallPkgs {
		if have[want] {
			out = append(out, want)
		}
	}
	return out
}

// installCLTFromMirror 走镜像路径装 CLT。返回 error 表示"这条路也不行"，
// 调用方会回退到苹果自己的两条路。
func (m *Manager) installCLTFromMirror(ctx context.Context, result *InstallResult) error {
	base := m.cltMirrorBaseFor(ctx)
	indexURL := base + "/clt/index.json"
	result.step(ctx, "从镜像获取命令行开发者工具清单："+indexURL)

	raw, err := m.fetchSmallText(ctx, 30*time.Second, indexURL)
	if err != nil {
		return fmt.Errorf("取镜像清单失败：%w", err)
	}
	var idx cltIndex
	if err := json.Unmarshal([]byte(raw), &idx); err != nil {
		return fmt.Errorf("镜像清单不是合法 JSON：%w", err)
	}
	macMajor := macMajorVersion(darwinMajorVersion())
	item, ok := pickCLTItem(idx.Items, macMajor)
	if !ok {
		return fmt.Errorf("镜像清单里没有适用于 macOS %d 的包集合", macMajor)
	}
	pkgs := cltNeededPkgs(item)
	if len(pkgs) == 0 {
		return fmt.Errorf("镜像清单条目 %q 里没有需要的包", item.Name)
	}

	dir := "/tmp/zizpanel-clt"
	_ = os.MkdirAll(dir, 0o755)

	total := int64(0)
	wantSums := map[string]string{}
	for _, s := range item.SHA256 {
		if i := strings.Index(s, "="); i > 0 {
			wantSums[strings.TrimSpace(s[:i])] = strings.ToLower(strings.TrimSpace(s[i+1:]))
		}
	}

	paths := make([]string, 0, len(pkgs))
	for _, name := range pkgs {
		dst := dir + "/" + name
		want := wantSums[name]
		// 已下过且校验通过就不再重下（重试任务时省几十分钟）
		if st, serr := os.Stat(dst); serr == nil && st.Size() > 0 {
			if want == "" || sha256OfFile(dst) == want {
				result.step(ctx, "已有可用安装包，跳过下载："+name)
				paths = append(paths, dst)
				total += st.Size()
				continue
			}
		}
		result.step(ctx, "下载 "+item.Name+" 组件："+name)
		n, derr := m.fetchCLTPkg(ctx, result, item, name, dst, want)
		if derr != nil {
			return fmt.Errorf("下载 %s 失败：%w", name, derr)
		}
		paths = append(paths, dst)
		total += n
	}
	result.step(ctx, fmt.Sprintf("安装包已就绪（共 %s），开始安装", humanBytes(total)))

	// 逐个 install。`installer` 是自己 fork 的子进程，所以这里必须用
	// runRoot 而不是 runAsUser —— 后者是 `sudo -n -u <用户>`，installer
	// 会报 "You must be root"。
	installed := 0
	for _, p := range paths {
		result.step(ctx, "安装 "+baseName(p)+"（几分钟，请勿关闭面板）")
		out, ierr := m.runRoot(ctx, 20*time.Minute, "/usr/sbin/installer", "-pkg", p, "-target", "/")
		if ierr != nil {
			return fmt.Errorf("installer 安装 %s 失败：%s", baseName(p), lastLines(out, 6))
		}
		installed++
		_ = os.Remove(p) // 装完就删，别占着一台机器 600 MB 的 /tmp
	}
	result.step(ctx, fmt.Sprintf("已安装 %d 个组件", installed))

	// 装完立刻核对：xcode-select -p 存在，且 /usr/bin/python3 真的能跑。
	// 只看前者不够 —— 那个路径存在也可能是个空壳（见 cltInstalled 注释）。
	for i := 0; i < 30; i++ {
		if m.cltInstalled(ctx) && m.python3Works(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if !m.cltInstalled(ctx) {
		return fmt.Errorf("installer 跑完了但 xcode-select -p 仍不可用")
	}
	return fmt.Errorf("installer 跑完了但 /usr/bin/python3 仍不可用")
}

// downloadToFile 用 curl 下载（与项目其他地方一致），并把可读的失败原因带回任务步骤。
//
// 为什么不用 Go 的 http.Client：面板在 LaunchDaemon 里跑，curl 走的是系统
// 代理配置与钥匙串，行为与用户手工执行完全一致；而且断电/断网时 curl 自己的
// 重试与断点处理比我们自己写的稳。返回的是文件大小（字节）。
func (m *Manager) downloadToFile(ctx context.Context, url, dst string, timeout time.Duration, result *InstallResult, label string) (int64, error) {
	start := time.Now()
	// -f：HTTP 4xx/5xx 直接失败（不能把错误页面当安装包存下来）
	// -L：跟随跳转（镜像可能 302 到 CDN）
	// --retry-all-errors：连不上也重试，国内链路经常抖
	out, err := m.runRoot(ctx, timeout, "/usr/bin/curl",
		"-fL", "--retry", "5", "--retry-delay", "3", "--retry-all-errors",
		"--connect-timeout", "20", "-o", dst, url)
	if err != nil {
		_ = os.Remove(dst)
		return 0, fmt.Errorf("%s（耗时 %s）：%s", url, time.Since(start).Round(time.Second), truncate(strings.TrimSpace(out), 300))
	}
	st, serr := os.Stat(dst)
	if serr != nil || st.Size() == 0 {
		_ = os.Remove(dst)
		return 0, fmt.Errorf("下载完成但文件为空：%s", url)
	}
	if result != nil {
		result.step(ctx, fmt.Sprintf("%s 完成：%s（耗时 %s）", label, humanBytes(st.Size()), time.Since(start).Round(time.Second)))
	}
	return st.Size(), nil
}

// fetchSmallText 取一段小文本（清单文件），带大小上限，避免把大文件读进内存。
func (m *Manager) fetchSmallText(ctx context.Context, timeout time.Duration, url string) (string, error) {
	out, err := m.runRoot(ctx, timeout, "/usr/bin/curl",
		"-fsSL", "--connect-timeout", "15", "--max-time", fmt.Sprintf("%d", int(timeout.Seconds())),
		"--max-filesize", "1048576", url)
	if err != nil {
		return "", fmt.Errorf("curl %s：%s", url, truncate(strings.TrimSpace(out), 200))
	}
	return out, nil
}

// sha256OfFile 算文件 sha256；出错返回空串（调用方只在"清单提供了期望值"时才比对）。
func sha256OfFile(path string) string {
	sum, err := fileSHA256(path)
	if err != nil {
		return ""
	}
	return sum
}

// python3Works 确认 /usr/bin/python3 是**真的** Python 而不是占位程序。
//
// 全新 macOS 上这个路径存在但只会打印
// "xcode-select: note: No developer tools were found, requesting install."
// 并弹图形对话框 —— 所以必须看输出，不能看退出码。
func (m *Manager) python3Works(ctx context.Context) bool {
	out, err := m.runAsUser(ctx, 60*time.Second, "/usr/bin/python3", "-c",
		"import sys;print(sys.version_info[0])")
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(out), "3")
}

// darwinMajorVersion 返回 Darwin 主版本号（macOS 15 → 24）。
//
// 用 `sysctl -n kern.osrelease` 而不是引入 golang.org/x/sys：项目至今
// 只依赖标准库，这条只为拿一个整数，不值得破例。
func darwinMajorVersion() int {
	if runtime.GOOS != "darwin" {
		return 0
	}
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.osrelease").Output()
	if err != nil {
		return 0
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), ".", 2)
	n, cerr := strconv.Atoi(parts[0])
	if cerr != nil {
		return 0
	}
	return n
}

// baseName 取路径最后一段（只处理 / 分隔，够用）。
func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// lastLines 取输出的最后 n 行，用于把 installer 的报错塞进任务步骤。
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// ============================================================================
//  大包下载：分片并行 + 断点续传
//
//  为什么值得单独写：CLT 的 576 MB 包是整条"全新安装"链路里最大的一次传输。
//  真机实测（mini，无代理）单连接只有 200 KB/s 左右 —— 50 分钟，而且中途断一次
//  就要从 0 开始。镜像上把包切成 32 MB 的分片之后：
//    - **并行**：实测总吞吐明显高于单连接；
//    - **续传**：已下载并校验通过的分片直接跳过，重试任务的代价从"整包"变成"一片"；
//    - **早失败**：每片单独校验，坏片只重下那一片。
//
//  清单没给 parts 时自动退回单文件下载（见 fetchCLTPkg），所以镜像可以分阶段升级。
// ============================================================================

// cltDownloadParallel 是并行度。4 路在"每连接被限速"的链路上收益最大，
// 再高就只是把同一个瓶颈切得更碎，还会让镜像服务器的连接数无谓变高。
const cltDownloadParallel = 4

// fetchCLTPkg 把一个包下到 dst（可能走分片并行），成功后校验整体 sha256。
// 返回文件字节数。校验不通过会删掉产物并返回错误 —— 宁可重下，也不要把坏包
// 交给 installer（那会以"看不懂的方式"失败）。
func (m *Manager) fetchCLTPkg(ctx context.Context, result *InstallResult, item cltIndexItem,
	name, dst, wantSHA string) (int64, error) {

	parts := item.partsFor(name)
	if len(parts) == 0 {
		// 单文件路径：curl 自己带 --retry，失败删半成品
		n, err := m.downloadToFile(ctx, item.filesBase(m.cltMirrorBaseFor(ctx))+"/"+name, dst,
			45*time.Minute, result, "下载 "+name)
		if err != nil {
			return 0, err
		}
		if wantSHA != "" {
			if got := sha256OfFile(dst); got != wantSHA {
				_ = os.Remove(dst)
				return 0, fmt.Errorf("校验不一致（期望 %s，实际 %s）", wantSHA, got)
			}
		}
		return n, nil
	}

	partDir := dst + ".parts"
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		return 0, err
	}
	base := item.filesBase(m.cltMirrorBaseFor(ctx))
	if err := m.fetchParts(ctx, result, base, name, partDir, parts); err != nil {
		return 0, err
	}
	n, err := concatParts(partDir, parts, dst)
	if err != nil {
		return 0, err
	}
	// 拼回来还要核对**整体** sha256：分片自己的校验说明不了"顺序拼对了"。
	if wantSHA != "" {
		if got := sha256OfFile(dst); got != wantSHA {
			_ = os.Remove(dst)
			return 0, fmt.Errorf("分片拼接后校验不一致（期望 %s，实际 %s）", wantSHA, got)
		}
	}
	_ = os.RemoveAll(partDir) // 拼好就清掉分片，别在 /tmp 里留两份
	return n, nil
}

// fetchParts 并行下载分片，逐个校验大小；已有的分片（大小对得上）直接跳过。
//
// 并发上限 + 每片一个 goroutine：用通道收集错误，**第一个错误就返回**，
// 但不会杀掉已经跑起来的下载（它们最多多下几十 MB，比"等下完再发现失败"划算）。
func (m *Manager) fetchParts(ctx context.Context, result *InstallResult,
	base, pkg, partDir string, parts []cltPart) error {

	type job struct {
		idx  int
		part cltPart
	}
	jobs := make(chan job)
	errs := make(chan error, len(parts))

	var wg sync.WaitGroup
	workers := cltDownloadParallel
	if workers > len(parts) {
		workers = len(parts)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				dst := filepath.Join(partDir, j.part.File)
				if st, serr := os.Stat(dst); serr == nil && st.Size() == j.part.Size {
					continue // 续传：这一片上次已经下好了
				}
				if _, derr := m.downloadToFile(ctx, base+"/"+j.part.File, dst,
					20*time.Minute, nil, "分片 "+j.part.File); derr != nil {
					errs <- derr
					return
				}
				if st, serr := os.Stat(dst); serr != nil || st.Size() != j.part.Size {
					errs <- fmt.Errorf("分片 %s 大小不对（期望 %d）", j.part.File, j.part.Size)
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i, p := range parts {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case jobs <- job{idx: i, part: p}:
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	if result != nil {
		var total int64
		for _, p := range parts {
			total += p.Size
		}
		result.step(ctx, fmt.Sprintf("分片下载完成：%s（%d 片）", humanBytes(total), len(parts)))
	}
	return nil
}

// concatParts 按清单顺序把分片拼成 dst，返回总字节数。
//
// 顺序**以清单为准**，不按文件名排序：`split` 的默认后缀是字母序，
// 但 `-d` 是数字序，靠"文件名排序对不对"太脆；拼接顺序错了整体 sha256 会不匹配，
// 所以宁可显式按清单顺序拼。
func concatParts(partDir string, parts []cltPart, dst string) (int64, error) {
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer func() { _ = out.Close() }()
	var total int64
	for _, p := range parts {
		in, oerr := os.Open(filepath.Join(partDir, p.File))
		if oerr != nil {
			_ = os.Remove(dst)
			return 0, fmt.Errorf("缺少分片 %s：%w", p.File, oerr)
		}
		n, cerr := io.Copy(out, in)
		_ = in.Close()
		if cerr != nil {
			_ = os.Remove(dst)
			return 0, cerr
		}
		if n != p.Size {
			_ = os.Remove(dst)
			return 0, fmt.Errorf("分片 %s 实际 %d 字节，清单写的是 %d", p.File, n, p.Size)
		}
		total += n
	}
	if err := out.Sync(); err != nil {
		return 0, err
	}
	return total, nil
}
