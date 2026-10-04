package web

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/scheduler"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// mirrorHTTPClient 给预取用的 HTTP 客户端：下载大文件不设总超时（由 ctx 控制），
// 小请求（HEAD/校验清单）用短超时。timeout<=0 表示不设总超时。
func mirrorHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		return &http.Client{}
	}
	return &http.Client{Timeout: timeout}
}

// isSHA256Hex 判断是不是 64 位十六进制（索引里给坏值时必须拒绝）。
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// ============================================================================
//  镜像站应用包「先备好」（预取）
//
//  与 api_mirror_sync.go（面板发布件同步）是两件事，别混：
//    · mirror-sync   同步**面板自己**的发布件（验签清单 + 资产 sha256）；
//    · 本文件        把**应用市场**里每个应用的上游产物预取进镜像的 apps/ 布局。
//
//  信任链：上游给了 sha256 清单（GitHub release 的 checksums.txt 之类）就逐个核；
//  没给就退回"大小 + 能正确解包/读取"，并在日志里如实说明这一次校验弱在哪。
//  只有校验通过的字节才可能出现在 apps/ 目录里；索引一律 .tmp 写完 rename 原子发布，
//  并且**只列已落盘且校验通过的产物**。
//
//  布局（与 services/mirror.go 文件头、tools/sync-nas-apps.sh 完全一致）：
//    <dir>/apps/<id>/manifest.json            应用索引（latest + assets[] + published_at）
//    <dir>/apps/<id>/<version>/manifest.json  版本清单（name/sha256/size）
//    <dir>/apps/<id>/<version>/<asset>        产物本体
//    <dir>/apps/announce.json                 轻量公告（见 api_mirror_announce.go）
//
//  幂等：已在镜像且校验通过的不再下载（第二次跑不产生任何上游请求）。
//  保留：每个应用只留最近 3 个版本，更旧的删除（发布成功之后才删，失败就不删）。
// ============================================================================

const (
	// mirrorAppsSubdir 是镜像上应用包的目录名（与 services.mirrorAppsDir 同值）。
	mirrorAppsSubdir = "apps"
	// mirrorPrefetchKeepVersions 是每个应用在镜像上保留的版本数。
	mirrorPrefetchKeepVersions = 3
	// mirrorPrefetchReserveBytes 是空间预检额外要求的余量。
	mirrorPrefetchReserveBytes = 64 << 20
	// mirrorPrefetchChecksumLimit 是上游 sha256 清单的大小上限。
	mirrorPrefetchChecksumLimit = 1 << 20
)

// mirrorPrefetchTarget 是一个"要出现在镜像上的产物"。
type mirrorPrefetchTarget struct {
	AppID string
	// Name 是应用名（只用于日志）。
	Name string
	// Version 是目录名里的版本（原样，含 v 前缀）。
	Version string
	// Asset 是原始产物文件名。
	Asset string
	// UpstreamURL 是官方下载地址（绝不默认镜像自己）。
	UpstreamURL string
	// ChecksumName 是上游 sha256 清单文件名（空 = 上游不提供）。
	ChecksumName string
	// Arch 写进索引（面板只支持原生 arm64）。
	Arch string
}

// mirrorPrefetchOptions 是一次预取的全部输入（测试可注入假上游与假空间）。
type mirrorPrefetchOptions struct {
	Dir     string
	Targets []mirrorPrefetchTarget
	// Skipped 是"知道但这次不处理"的应用（消息直接进日志，绝不假装覆盖了它们）。
	Skipped []string
	// FreeBytes 读镜像卷可用字节（nil = 真实 statfs）。
	FreeBytes func(dir string) (int64, error)
	// BeforePublish 在**任何 rename 之前**调用；返回错误 = 模拟"发布前被杀"。
	BeforePublish func() error
	Now           func() time.Time
}

// mirrorAppIndex / mirrorAppIndexAsset 是 <dir>/apps/<id>/manifest.json 的形状。
// 必须能被 services 里读 zizvideo 索引与镜像清单的代码解析（字段名冻结）。
type mirrorAppIndex struct {
	App         string                `json:"app"`
	Latest      string                `json:"latest"`
	PublishedAt string                `json:"published_at,omitempty"`
	Assets      []mirrorAppIndexAsset `json:"assets"`
}

type mirrorAppIndexAsset struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// mirrorVersionManifest 是 <dir>/apps/<id>/<version>/manifest.json 的形状
// （与 services.mirrorManifest 一致：面板安装时按它核 sha256）。
type mirrorVersionManifest struct {
	App     string               `json:"app"`
	Version string               `json:"version"`
	Assets  []mirrorVersionAsset `json:"assets"`
}

type mirrorVersionAsset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// mirrorPrefetchApp 是结果里的一条应用结论（任务中心/界面用它）。
type mirrorPrefetchApp struct {
	App     string `json:"app"`
	Version string `json:"version"`
	Asset   string `json:"asset"`
	Status  string `json:"status"` // prepared / present / downloaded
	Bytes   int64  `json:"bytes,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// mirrorPrefetchResult 是整个预取动作的结果。
type mirrorPrefetchResult struct {
	Dir              string              `json:"dir"`
	Downloaded       int                 `json:"downloaded"`
	Skipped          int                 `json:"skipped"`
	Removed          []string            `json:"removed,omitempty"`
	FreedBytes       int64               `json:"freed_bytes,omitempty"`
	Apps             []mirrorPrefetchApp `json:"apps"`
	AnnounceRevision string              `json:"announce_revision,omitempty"`
}

// mirrorStatfsFree 取某路径所在卷的可用字节（Bavail * Bsize）。
func mirrorStatfsFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// defaultMirrorPrefetchOptions 按市场注册表构造一次生产预取。
//
// 只覆盖 apps/<id>/<version>/<asset> 这套布局（官方 release 原生二进制）；
// 动态索引条目（zizvideo）的版本由它自己的发布流程写进镜像索引，预取不碰。
// brew / compose / pip 等导轨走的是镜像的其它子路径（见 services/mirror.go 文件头），
// 不在本预取范围 —— 这一点在日志里如实说明，不假装"全都备好了"。
func defaultMirrorPrefetchOptions(dir string) mirrorPrefetchOptions {
	var targets []mirrorPrefetchTarget
	var skipped []string
	for _, a := range services.ReleaseBinaryAssets() {
		if a.Dynamic {
			skipped = append(skipped, a.ID+"（版本由它自己的镜像索引决定，由发布流程产出）")
			continue
		}
		if strings.TrimSpace(a.Asset) == "" || strings.TrimSpace(a.Tag) == "" ||
			strings.TrimSpace(a.UpstreamURL) == "" {
			skipped = append(skipped, a.ID+"（注册表里没有可下载的上游产物）")
			continue
		}
		targets = append(targets, mirrorPrefetchTarget{
			AppID: a.ID, Name: a.Name, Version: a.Tag, Asset: a.Asset,
			UpstreamURL: a.UpstreamURL, ChecksumName: a.ChecksumAsset, Arch: "arm64",
		})
	}
	return mirrorPrefetchOptions{Dir: dir, Targets: targets, Skipped: skipped}
}

// handleMirrorAppPrefetch 手动触发一次应用包预取（管理员，走任务中心）。
func (s *Server) handleMirrorAppPrefetch(w http.ResponseWriter, r *http.Request) {
	if u := userFrom(r.Context()); u == nil || !u.IsAdmin {
		fail(w, http.StatusForbidden, "只有管理员能预取镜像应用包")
		return
	}
	var req mirrorSyncRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	dir, derr := s.resolveMirrorDir(r.Context(), req.Dir)
	if derr != nil {
		fail(w, http.StatusBadRequest, derr.Error())
		return
	}
	s.launchTask(w, r, "mirror-prefetch", "mirror:"+dir, "预取应用包到镜像 "+dir, "mirror_prefetch",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return runMirrorAppPrefetch(ctx, log, s.mirrorPrefetchOptions(dir))
		})
}

// mirrorPrefetchOptions 取本次预取的上游清单（单测用注入的假上游，绝不联网）。
func (s *Server) mirrorPrefetchOptions(dir string) mirrorPrefetchOptions {
	if s.mirrorPrefetchOptionsOverride != nil {
		return s.mirrorPrefetchOptionsOverride(dir)
	}
	return defaultMirrorPrefetchOptions(dir)
}

// startMirrorAppPrefetch 把"每天预取一次"登记进面板既有的调度器。
// 只登记，不在启动路径上做网络等待（与证书续期同一套机制，见 api_certs.go）。
func (s *Server) startMirrorAppPrefetch(ctx context.Context) {
	runner := scheduler.NewDailyRunner(mirrorPrefetchHour, mirrorPrefetchMinute, []scheduler.DailyTask{
		{Name: "mirror-app-prefetch", Run: s.prefetchMirrorAppsDaily},
	}, s.Log.Info)
	runner.Start(ctx)
	s.Log.Info("镜像应用包预取已登记：%s（每天一次）",
		runner.NextRun(time.Now()).Format("2006-01-02 15:04"))
}

// mirrorPrefetchHour/Minute 是每日预取的默认时刻（本地时间，避开凌晨续期）。
const (
	mirrorPrefetchHour   = 4
	mirrorPrefetchMinute = 30
)

// prefetchMirrorAppsDaily 是每日循环里的任务体：读面板设置里的镜像目录，跑一次预取。
func (s *Server) prefetchMirrorAppsDaily(ctx context.Context) error {
	dir := s.mirrorDirSetting(ctx)
	if dir == "" {
		return errors.New("未配置镜像路径（面板设置 → 应用包镜像 → 镜像目录），跳过本次预取")
	}
	if t := s.Tasks.RunningFor("mirror:" + dir); t != nil {
		s.Log.Info("跳过本次应用包预取：已有任务在跑（%s）", t.Meta().Title)
		return nil
	}
	ch := make(chan error, 1)
	s.Tasks.Start("mirror-prefetch", "mirror:"+dir, "预取应用包到镜像 "+dir,
		func(tctx context.Context, log tasks.LogFunc) (any, error) {
			_, err := runMirrorAppPrefetch(tctx, log, s.mirrorPrefetchOptions(dir))
			ch <- err
			return nil, err
		})
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// mirrorPrefetchPlan 是一个产物在本次运行里的落点与结论。
type mirrorPrefetchPlan struct {
	t                   mirrorPrefetchTarget
	assetPath           string
	versionManifestPath string
	appIndexPath        string
	present             bool
	sha                 string
	size                int64
	knownUpstreamSize   int64 // -1 = 上游没给
	stagedPath          string
}

// runMirrorAppPrefetch 执行一次完整预取。
func runMirrorAppPrefetch(ctx context.Context, log tasks.LogFunc, opt mirrorPrefetchOptions) (*mirrorPrefetchResult, error) {
	res := &mirrorPrefetchResult{Dir: opt.Dir}
	if log == nil {
		log = func(string, string) {}
	}
	step := func(f string, a ...any) { log(tasks.LevelStep, fmt.Sprintf(f, a...)) }
	out := func(f string, a ...any) { log(tasks.LevelOut, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { log(tasks.LevelWarn, fmt.Sprintf(f, a...)) }
	bad := func(f string, a ...any) { log(tasks.LevelErr, fmt.Sprintf(f, a...)) }

	if strings.TrimSpace(opt.Dir) == "" {
		return res, errors.New("还没有配置镜像路径：请在「面板设置 → 应用包镜像」里填写镜像站的文档根目录")
	}
	if opt.FreeBytes == nil {
		opt.FreeBytes = mirrorStatfsFree
	}
	if opt.BeforePublish == nil {
		opt.BeforePublish = func() error { return nil }
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	for _, sk := range opt.Skipped {
		warn("本次不处理 %s", sk)
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return res, fmt.Errorf("创建镜像目录失败：%w", err)
	}
	if len(opt.Targets) == 0 {
		return res, errors.New("没有任何可预取的应用产物（注册表为空）")
	}

	step("① 与镜像现有内容对比")
	plans := make([]*mirrorPrefetchPlan, 0, len(opt.Targets))
	for _, t := range opt.Targets {
		if err := validateMirrorTarget(t); err != nil {
			return res, err
		}
		p := &mirrorPrefetchPlan{
			t:                   t,
			assetPath:           filepath.Join(opt.Dir, mirrorAppsSubdir, t.AppID, t.Version, t.Asset),
			versionManifestPath: filepath.Join(opt.Dir, mirrorAppsSubdir, t.AppID, t.Version, mirrorManifestName),
			appIndexPath:        filepath.Join(opt.Dir, mirrorAppsSubdir, t.AppID, mirrorManifestName),
		}
		if sha, size, ok := mirrorVersionVerified(opt.Dir, t); ok {
			p.present, p.sha, p.size = true, sha, size
			res.Skipped++
			out("✓ 已在镜像且校验通过，跳过下载：%s/%s/%s", t.AppID, t.Version, t.Asset)
		}
		plans = append(plans, p)
	}

	step("② 空间预检")
	var need int64
	var unknown int64
	for _, p := range plans {
		if p.present {
			continue
		}
		size, err := mirrorHeadSize(ctx, p.t.UpstreamURL)
		if err != nil {
			p.knownUpstreamSize = -1
			unknown++
			warn("上游没给 %s 的大小（%v）：这部分无法参与空间预检", p.t.Asset, err)
			continue
		}
		p.knownUpstreamSize = size
		need += size
	}
	if need > 0 {
		free, err := opt.FreeBytes(opt.Dir)
		if err != nil {
			warn("读不到镜像卷剩余空间（%v）：跳过空间预检", err)
		} else if free < need+mirrorPrefetchReserveBytes {
			msg := fmt.Sprintf(
				"镜像卷剩余空间不足：剩 %d 字节（%s），本次共需 %d 字节（%s，其中产物 %d 字节 + 余量 %d 字节），"+
					"还差 %d 字节。已提前停止，未写入任何文件",
				free, mirrorHumanBytes(free),
				need+mirrorPrefetchReserveBytes, mirrorHumanBytes(need+mirrorPrefetchReserveBytes),
				need, mirrorPrefetchReserveBytes,
				need+mirrorPrefetchReserveBytes-free)
			bad("%s", msg)
			return res, errors.New(msg)
		} else {
			out("空间预检通过：需要 %s，剩余 %s", mirrorHumanBytes(need), mirrorHumanBytes(free))
		}
	}
	if unknown > 0 {
		warn("%d 个产物的上游没给大小，本次空间预检不覆盖它们", unknown)
	}

	step("③ 下载并校验")
	if need > 0 || unknown > 0 {
		// 临时目录放在镜像目录内、权限 0700：nginx worker 既进不来也读不到半截文件。
		stage, err := os.MkdirTemp(opt.Dir, ".zp-appfetch-")
		if err != nil {
			return res, fmt.Errorf("创建临时目录失败：%w", err)
		}
		defer func() { _ = os.RemoveAll(stage) }()
		for _, p := range plans {
			if p.present {
				continue
			}
			if err := ctx.Err(); err != nil {
				bad("已取消：%v（未发布任何索引）", err)
				return res, err
			}
			dst := filepath.Join(stage, filepath.FromSlash(path.Join(mirrorAppsSubdir, p.t.AppID, p.t.Version, p.t.Asset)))
			want := ""
			if p.t.ChecksumName != "" {
				want, err = mirrorFetchUpstreamChecksum(ctx, p.t)
				if err != nil {
					bad("%s 的上游 sha256 清单取不到：%v（未写入镜像）", p.t.Asset, err)
					return res, err
				}
			}
			out("下载 %s（%s %s）", p.t.Asset, p.t.AppID, p.t.Version)
			lastPct := int64(-1)
			sha, size, derr := mirrorDownloadTo(ctx, p.t.UpstreamURL, dst, want, func(written, total int64) {
				if total <= 0 {
					return
				}
				pct := written * 100 / total
				if pct != lastPct && pct%25 == 0 {
					lastPct = pct
					out("  %s：%d%%", p.t.Asset, pct)
				}
			})
			if derr != nil {
				bad("%s 下载/校验失败：%v（未写入镜像）", p.t.Asset, derr)
				return res, derr
			}
			if want == "" {
				// 上游没给 sha256：至少校验大小与"能正确解包/读取"，并如实说明。
				if err := verifyMirrorArtifact(dst, p.t.Asset); err != nil {
					bad("%s 完整性复核失败：%v（未写入镜像）", p.t.Asset, err)
					return res, fmt.Errorf("%s 完整性复核失败：%w（未写入镜像）", p.t.Asset, err)
				}
				warn("%s 上游没有 sha256 清单：本次只核了大小与可解包/可读（弱校验）", p.t.Asset)
			}
			p.sha, p.size, p.present = sha, size, true
			p.stagedPath = dst
			res.Downloaded++
			res.Apps = append(res.Apps, mirrorPrefetchApp{
				App: p.t.AppID, Version: p.t.Version, Asset: p.t.Asset,
				Status: "downloaded", Bytes: size, SHA256: sha,
			})
			out("✓ %s（%s，sha256 %s…）", p.t.Asset, mirrorHumanBytes(size), sha[:12])
		}
	}
	for _, p := range plans {
		if p.present && p.sha != "" && !mirrorPrefetchAppListed(res.Apps, p) {
			res.Apps = append(res.Apps, mirrorPrefetchApp{
				App: p.t.AppID, Version: p.t.Version, Asset: p.t.Asset,
				Status: "present", Bytes: p.size, SHA256: p.sha,
			})
		}
	}

	step("④ 构造索引（只列已落盘且校验通过的产物）")
	writes, apps, err := mirrorBuildPublish(opt.Dir, plans, opt.Now())
	if err != nil {
		bad("构造索引失败：%v（未发布）", err)
		return res, err
	}
	res.AnnounceRevision = mirrorAnnounceRevisionOf(apps)
	announcePath := filepath.Join(opt.Dir, mirrorAppsSubdir, mirrorAnnounceName)
	aw, err := mirrorAnnouncePendingWrite(announcePath, apps, opt.Now())
	if err != nil {
		bad("构造公告失败：%v（未发布）", err)
		return res, err
	}
	if aw == nil {
		out("公告内容未变化（revision 相同）：不重写，面板不会被惊动")
	} else {
		writes = append(writes, *aw)
	}

	// 发布前钩子：任何 rename 之前。返回错误 = 模拟进程被杀/发布中止。
	if err := opt.BeforePublish(); err != nil {
		bad("发布前中止：%v（索引保持旧版本不变）", err)
		return res, fmt.Errorf("发布前中止：%w", err)
	}

	step("⑤ 原子发布（.tmp 写完 rename）")
	// 先把校验过的产物从临时目录就位（同文件系统 rename，读者看不到半截文件），
	// 再写版本清单/索引/公告 —— 索引只可能引用已经落盘的产物。
	for _, p := range plans {
		if p.stagedPath == "" {
			continue
		}
		if err := placeMirrorFile(p.stagedPath, p.assetPath); err != nil {
			bad("产物 %s 就位失败：%v（索引未更新）", p.t.Asset, err)
			return res, fmt.Errorf("产物 %s 就位失败：%w", p.t.Asset, err)
		}
	}
	for _, w := range writes {
		if err := mirrorWriteAtomic(w.path, w.data); err != nil {
			bad("写入 %s 失败：%v", w.rel, err)
			return res, fmt.Errorf("写入 %s 失败：%w", w.rel, err)
		}
	}
	if rev := res.AnnounceRevision; rev != "" {
		out("公告版本 %s…（%d 个应用）", rev[:12], len(apps))
	}

	step("⑥ 版本保留（每个应用最近 %d 个）", mirrorPrefetchKeepVersions)
	removed, freed := mirrorPruneOldVersions(opt.Dir, apps, func(f string, a ...any) { out(f, a...) }, warn)
	res.Removed, res.FreedBytes = removed, freed

	step("⑦ 完成：下载 %d 个，跳过 %d 个（已在镜像且校验通过）", res.Downloaded, res.Skipped)
	return res, nil
}

func mirrorPrefetchAppListed(list []mirrorPrefetchApp, p *mirrorPrefetchPlan) bool {
	for _, a := range list {
		if a.App == p.t.AppID && a.Version == p.t.Version && a.Asset == p.t.Asset {
			return true
		}
	}
	return false
}

// validateMirrorTarget 拒绝会跳出镜像目录的 id/版本/文件名（防路径穿越）。
func validateMirrorTarget(t mirrorPrefetchTarget) error {
	for _, part := range []struct{ name, v string }{
		{"应用 ID", t.AppID}, {"版本", t.Version}, {"产物名", t.Asset},
	} {
		v := strings.TrimSpace(part.v)
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\\") {
			return fmt.Errorf("%s 不合法（%q）：拒绝写入镜像", part.name, part.v)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(t.UpstreamURL), "http://") &&
		!strings.HasPrefix(strings.TrimSpace(t.UpstreamURL), "https://") {
		return fmt.Errorf("应用 %s 的上游地址不是 http(s)（%q）：拒绝下载", t.AppID, t.UpstreamURL)
	}
	return nil
}

// mirrorVersionVerified 判断某个(应用,版本,产物)是否已在镜像且校验通过。
// 判据是**同目录的版本清单**里记的 sha256 与磁盘上文件一致（没有清单就不认）。
func mirrorVersionVerified(dir string, t mirrorPrefetchTarget) (string, int64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, mirrorAppsSubdir, t.AppID, t.Version, mirrorManifestName))
	if err != nil {
		return "", 0, false
	}
	var m mirrorVersionManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return "", 0, false
	}
	p := filepath.Join(dir, mirrorAppsSubdir, t.AppID, t.Version, t.Asset)
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return "", 0, false
	}
	for _, a := range m.Assets {
		if a.Name != t.Asset || !isSHA256Hex(a.SHA256) {
			continue
		}
		if a.Size > 0 && fi.Size() != a.Size {
			return "", 0, false
		}
		sum, err := mirrorFileSHA256(p)
		if err != nil || !strings.EqualFold(sum, a.SHA256) {
			return "", 0, false
		}
		return strings.ToLower(sum), fi.Size(), true
	}
	return "", 0, false
}

// mirrorPendingWrite 是一个待发布的原子写。
type mirrorPendingWrite struct {
	path string
	rel  string
	data []byte
}

// mirrorBuildPublish 构造版本清单 + 应用索引 + 公告的待写内容（不落盘）。
// 只把**已落盘且校验通过**的产物写进索引。
func mirrorBuildPublish(dir string, plans []*mirrorPrefetchPlan, now time.Time) ([]mirrorPendingWrite, map[string]string, error) {
	// 应用 → 版本 → 产物
	type verAssets map[string][]mirrorVersionAsset
	appVers := map[string]verAssets{}
	appOrder := []string{}
	seenApp := map[string]bool{}
	for _, p := range plans {
		if !p.present || !isSHA256Hex(p.sha) {
			continue
		}
		if !seenApp[p.t.AppID] {
			seenApp[p.t.AppID] = true
			appOrder = append(appOrder, p.t.AppID)
		}
		if appVers[p.t.AppID] == nil {
			appVers[p.t.AppID] = verAssets{}
		}
		appVers[p.t.AppID][p.t.Version] = append(appVers[p.t.AppID][p.t.Version],
			mirrorVersionAsset{Name: p.t.Asset, SHA256: strings.ToLower(p.sha), Size: p.size})
	}
	// 旧索引里仍然落盘且校验通过的版本也带上（不因一次预取丢掉历史版本）。
	for _, p := range plans {
		idx := mirrorReadAppIndex(p.appIndexPath)
		if idx == nil {
			continue
		}
		if !seenApp[p.t.AppID] {
			seenApp[p.t.AppID] = true
			appOrder = append(appOrder, p.t.AppID)
		}
		if appVers[p.t.AppID] == nil {
			appVers[p.t.AppID] = verAssets{}
		}
		for _, a := range idx.Assets {
			ver := strings.TrimSpace(a.Version)
			if ver == "" || !isSHA256Hex(a.SHA256) {
				continue
			}
			if _, dup := mirrorFindVersionAsset(appVers[p.t.AppID][ver], a.Name); dup {
				continue
			}
			pth := filepath.Join(dir, mirrorAppsSubdir, p.t.AppID, ver, a.Name)
			fi, err := os.Stat(pth)
			if err != nil || fi.IsDir() {
				continue
			}
			if a.Size > 0 && fi.Size() != a.Size {
				continue
			}
			sum, err := mirrorFileSHA256(pth)
			if err != nil || !strings.EqualFold(sum, a.SHA256) {
				continue
			}
			appVers[p.t.AppID][ver] = append(appVers[p.t.AppID][ver],
				mirrorVersionAsset{Name: a.Name, SHA256: strings.ToLower(sum), Size: fi.Size()})
		}
	}

	apps := map[string]string{}
	var writes []mirrorPendingWrite
	for _, appID := range appOrder {
		vers := appVers[appID]
		versions := make([]string, 0, len(vers))
		for v := range vers {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
		// 只保留最近 N 个版本：索引与磁盘必须一致（更旧的由 mirrorPruneOldVersions 删除）。
		if len(versions) > mirrorPrefetchKeepVersions {
			versions = versions[:mirrorPrefetchKeepVersions]
		}
		var assets []mirrorAppIndexAsset
		for _, v := range versions {
			for _, a := range vers[v] {
				assets = append(assets, mirrorAppIndexAsset{
					Name: a.Name, Version: v, Arch: "arm64", SHA256: a.SHA256, Size: a.Size,
				})
			}
		}
		if len(assets) == 0 {
			continue
		}
		// 版本清单：每个列进索引的版本都要有，否则下次预取会认不出它。
		for _, v := range versions {
			vm := mirrorVersionManifest{App: appID, Version: v, Assets: vers[v]}
			b, err := json.MarshalIndent(vm, "", "  ")
			if err != nil {
				return nil, nil, err
			}
			rel := path.Join(mirrorAppsSubdir, appID, v, mirrorManifestName)
			writes = append(writes, mirrorPendingWrite{
				path: filepath.Join(dir, filepath.FromSlash(rel)), rel: rel, data: append(b, '\n'),
			})
		}
		latest := versions[0]
		idx := mirrorAppIndex{App: appID, Latest: latest, PublishedAt: now.UTC().Format(time.RFC3339), Assets: assets}
		b, err := json.MarshalIndent(idx, "", "  ")
		if err != nil {
			return nil, nil, err
		}
		rel := path.Join(mirrorAppsSubdir, appID, mirrorManifestName)
		writes = append(writes, mirrorPendingWrite{
			path: filepath.Join(dir, filepath.FromSlash(rel)), rel: rel, data: append(b, '\n'),
		})
		apps[appID] = latest
	}
	return writes, apps, nil
}

func mirrorFindVersionAsset(list []mirrorVersionAsset, name string) (mirrorVersionAsset, bool) {
	for _, a := range list {
		if a.Name == name {
			return a, true
		}
	}
	return mirrorVersionAsset{}, false
}

// mirrorReadAppIndex 读应用索引（读不到/不是 JSON 返回 nil，不猜）。
func mirrorReadAppIndex(p string) *mirrorAppIndex {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var idx mirrorAppIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil
	}
	return &idx
}

// mirrorReadAnnounceDoc 读镜像上已有的公告（用于判断内容是否变化）。
func mirrorReadAnnounceDoc(p string) *mirrorAnnounceDoc {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var doc mirrorAnnounceDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil
	}
	return &doc
}

// mirrorAnnouncePendingWrite 构造公告的待写内容；内容（revision）没变就返回 nil
// —— 不重写公告，面板也就不会被打扰。
func mirrorAnnouncePendingWrite(p string, apps map[string]string, now time.Time) (*mirrorPendingWrite, error) {
	rev := mirrorAnnounceRevisionOf(apps)
	if rev == "" {
		return nil, nil
	}
	if old := mirrorReadAnnounceDoc(p); old != nil && strings.TrimSpace(old.Revision) == rev {
		return nil, nil
	}
	doc := mirrorAnnounceDoc{Revision: rev, PublishedAt: now.UTC().Format(time.RFC3339), Apps: apps}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	rel := path.Join(mirrorAppsSubdir, mirrorAnnounceName)
	return &mirrorPendingWrite{path: p, rel: rel, data: append(b, '\n')}, nil
}

// mirrorPruneOldVersions 每个应用只保留最近 N 个版本，更旧的目录删掉。
// 只在发布成功之后调用；删除失败只记日志、不影响已发布的内容。
func mirrorPruneOldVersions(dir string, apps map[string]string,
	out func(string, ...any), warn func(string, ...any)) ([]string, int64) {

	var removed []string
	var freed int64
	for appID := range apps {
		base := filepath.Join(dir, mirrorAppsSubdir, appID)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		var versions []string
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				versions = append(versions, e.Name())
			}
		}
		sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
		for i, v := range versions {
			if i < mirrorPrefetchKeepVersions {
				continue
			}
			p := filepath.Join(base, v)
			size := mirrorDirSize(p)
			if err := os.RemoveAll(p); err != nil {
				warn("删除旧版本 %s/%s 失败（保留不动）：%v", appID, v, err)
				continue
			}
			rel := path.Join(mirrorAppsSubdir, appID, v)
			removed = append(removed, rel)
			freed += size
			out("已删除旧版本 %s（保留最近 %d 个，释放 %s）", rel, mirrorPrefetchKeepVersions, mirrorHumanBytes(size))
		}
	}
	if len(removed) == 0 {
		out("没有需要清理的旧版本（每个应用都不超过 %d 个版本）", mirrorPrefetchKeepVersions)
	}
	return removed, freed
}

// mirrorDirSize 递归统计目录大小（只为日志里的"释放了多少"，读不到算 0）。
func mirrorDirSize(p string) int64 {
	var total int64
	_ = filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil || fi.IsDir() {
			return nil
		}
		total += fi.Size()
		return nil
	})
	return total
}

// mirrorHeadSize 用 HEAD 问上游产物大小（上游不给 Content-Length 就算失败）。
func mirrorHeadSize(ctx context.Context, rawURL string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return -1, err
	}
	req.Header.Set("User-Agent", "zizpanel-mirror")
	res, err := mirrorHTTPClient(0).Do(req)
	if err != nil {
		return -1, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return -1, &mirrorHTTPStatus{code: res.StatusCode}
	}
	if res.ContentLength <= 0 {
		return -1, errors.New("上游没有给 Content-Length")
	}
	return res.ContentLength, nil
}

// mirrorFetchUpstreamChecksum 取上游 sha256 清单里本产物的期望值。
// 清单在、但里面没有这个产物 = 明确失败（绝不"找不到就算了"）。
func mirrorFetchUpstreamChecksum(ctx context.Context, t mirrorPrefetchTarget) (string, error) {
	u, err := mirrorSiblingURL(t.UpstreamURL, t.ChecksumName)
	if err != nil {
		return "", err
	}
	body, err := mirrorGetBytes(ctx, u, mirrorPrefetchChecksumLimit)
	if err != nil {
		return "", fmt.Errorf("取校验清单失败（%s）：%w", u, err)
	}
	sum, ok := mirrorParseChecksum(string(body), t.Asset)
	if !ok {
		return "", fmt.Errorf("上游校验清单 %s 里没有 %s", u, t.Asset)
	}
	return sum, nil
}

// mirrorParseChecksum 从 sha256 清单里找某个文件名对应的值（支持 "sum  name" 与 "sum *name"）。
func mirrorParseChecksum(body, asset string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name != asset {
			continue
		}
		if isSHA256Hex(fields[0]) {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// mirrorDownloadTo 流式下载到 dst，边写边算 sha256。
// wantSHA 非空时不一致即失败；同时核对上游给的 Content-Length（防半截文件）。
func mirrorDownloadTo(ctx context.Context, rawURL, dst, wantSHA string,
	onProgress func(written, total int64)) (string, int64, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "zizpanel-mirror")
	res, err := mirrorHTTPClient(0).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", 0, &mirrorHTTPStatus{code: res.StatusCode}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var written int64
	total := res.ContentLength
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return "", written, err
		}
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Close()
				return "", written, werr
			}
			_, _ = h.Write(buf[:n])
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			return "", written, rerr
		}
	}
	if err := f.Close(); err != nil {
		return "", written, err
	}
	if written == 0 {
		return "", 0, errors.New("下载到 0 字节")
	}
	if total > 0 && written != total {
		return "", written, fmt.Errorf("下载不完整：上游声明 %d 字节，实际 %d 字节", total, written)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(sum, wantSHA) {
		return "", written, fmt.Errorf("sha256 不符：期望 %s，实际 %s", strings.ToLower(wantSHA), sum)
	}
	return sum, written, nil
}

// verifyMirrorArtifact 是"上游没给 sha256"时的兜底校验：非空 + 能正确解包/读取。
func verifyMirrorArtifact(p, asset string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return errors.New("产物是 0 字节")
	}
	if strings.HasSuffix(asset, ".tar.gz") || strings.HasSuffix(asset, ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("不是合法的 gzip：%w", err)
		}
		defer func() { _ = gz.Close() }()
		tr := tar.NewReader(gz)
		n := 0
		for {
			if _, err := tr.Next(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("tar 解包失败：%w", err)
			}
			n++
		}
		if n == 0 {
			return errors.New("tar 里没有任何成员")
		}
		return nil
	}
	buf := make([]byte, 512)
	if _, err := io.ReadFull(f, buf); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("读取产物失败：%w", err)
	}
	return nil
}

// mirrorSiblingURL 把 rawURL 换成同目录下的另一个文件名。
func mirrorSiblingURL(rawURL, name string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	u.Path = path.Join(path.Dir(u.Path), name)
	return u.String(), nil
}

// mirrorWriteAtomic 写 .tmp 再 rename（同目录、同文件系统，读者永远看到完整文件）。
func mirrorWriteAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, p)
}

// mirrorFileSHA256 算文件 sha256。
func mirrorFileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// mirrorHumanBytes 把字节数写成人看的大小（日志/错误文案用）。
func mirrorHumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return strconv.FormatFloat(v, 'f', 1, 64) + " " + u
		}
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " PB"
}

// compareVersions 是宽松的版本比较（去掉 v 前缀，按数字段比；非数字段按字典序）。
// 只用于排序/保留策略，不用于"有没有更新"的判断。
func compareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(strings.TrimSpace(a), "v"), ".")
	bs := strings.Split(strings.TrimPrefix(strings.TrimSpace(b), "v"), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		if xerr == nil && yerr == nil {
			if xn != yn {
				if xn > yn {
					return 1
				}
				return -1
			}
			continue
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}
