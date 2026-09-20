package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tasks"
	"github.com/zizdog/zizpanel/internal/upgrade"
)

// ============================================================================
//  镜像发布件同步（坑 217，2026-09-20 事故后新增）
//
//  镜像站的文档根在 mini 的外置盘上，只有面板守护进程有 TCC 授权能写；
//  本机 scp 已写不进去。所以面板提供"把公网源上的发布件同步到镜像目录"。
//
//  信任链不变：清单必须先用内置发布公钥验签（upgrade.VerifyManifest），每个资产
//  再逐个核 sha256；全部校验通过后才从临时目录就位 —— 绝不让未校验的字节或
//  半截文件出现在公网可读的镜像目录里。
//
//  清单优先取源上的 manifest-mirror.json（url 指向镜像自己），没有才回落到源站
//  manifest.json 并在日志里如实说明；写盘后还会自证清单地址是否真的指向镜像（坑 218）。
// ============================================================================

// mirrorDirSettingKey 是镜像目录在面板 settings 表里的键（与 config.json 分开：
// 它是运行时可改的数据，不是启动配置）。
const mirrorDirSettingKey = "mirror_dir"

const mirrorInstallShLimit = 1 << 20 // install.sh 上限 1 MB

// mirrorManifestName 是发布侧放到源上的"镜像版清单"（url 指向镜像自己，坑 218）。
const mirrorManifestName = "manifest-mirror.json"

const mirrorManifestLimit = 256 << 10 // 与 upgrade 的清单上限一致

// errMirrorManifestAbsent 表示源上还没有镜像版清单（404/410），可以回落源站清单。
var errMirrorManifestAbsent = errors.New("源上没有镜像版清单")

// mirrorDirSetting 读当前配置的镜像目录（读不到返回空串，不猜）。
func (s *Server) mirrorDirSetting(ctx context.Context) string {
	v, err := s.Store.GetSetting(ctx, mirrorDirSettingKey)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// mirrorDefaultSource 是内置公网镜像源（internal/upgrade/source.go 的 MirrorSource）。
func (s *Server) mirrorDefaultSource() string { return upgrade.MirrorSource }

// mirrorPublicBase 是镜像站上 zizpanel 目录的公网基址（写盘自证用）：<mirror_base>/zizpanel；
// 没配 mirror_base 就用内置公网镜像（坑 218）。
func (s *Server) mirrorPublicBase() string {
	b := strings.TrimRight(strings.TrimSpace(s.Cfg.MirrorBase), "/")
	if b == "" {
		return upgrade.MirrorSource
	}
	return b + "/zizpanel"
}

type mirrorSyncRequest struct {
	// Dir 是镜像站文档根（留空 = 用面板设置里保存的那个）。
	Dir string `json:"dir"`
	// Source 是发布件源基址（留空 = 内置公网镜像源）。
	Source string `json:"source"`
}

// handleMirrorSync 把公网源上的发布件同步到镜像目录（管理员，走任务中心）。
func (s *Server) handleMirrorSync(w http.ResponseWriter, r *http.Request) {
	if u := userFrom(r.Context()); u == nil || !u.IsAdmin {
		fail(w, http.StatusForbidden, "只有管理员能同步镜像发布件")
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
	src := strings.TrimSpace(req.Source)
	if src == "" {
		src = s.mirrorDefaultSource()
	}
	base, nerr := upgrade.Source{BaseURL: src}.Normalize()
	if nerr != nil {
		fail(w, http.StatusBadRequest, "同步源不合法："+nerr.Error())
		return
	}
	// target 带目录：同一个镜像目录不会被重复点成两个任务。
	s.launchTask(w, r, "mirror-sync", "mirror:"+dir, "同步镜像发布件到 "+dir, "mirror_sync",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return runMirrorSync(ctx, log, dir, base, s.mirrorPublicBase())
		})
}

// resolveMirrorDir 取本次同步的目标目录：请求体优先，否则用面板设置里的值。
func (s *Server) resolveMirrorDir(ctx context.Context, raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "" {
		dir = s.mirrorDirSetting(ctx)
	}
	if dir == "" {
		return "", errors.New("还没有配置镜像目录：请在「面板设置 → 应用包镜像」里填写镜像站的文档根目录")
	}
	return s.normalizeMirrorDir(dir)
}

// normalizeMirrorDir 校验并规范化镜像目录（空串 = 清空，合法）。
// 复用站点根目录那个白名单校验：绝对路径 + 系统目录黑名单 + 软链接解析（坑 217）。
func (s *Server) normalizeMirrorDir(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	clean, err := sites.ValidateRootPath(raw, s.siteRootPolicy())
	if err != nil {
		return "", fmt.Errorf("镜像目录不合法：%w（应填镜像站的文档根，例如 /Volumes/盘名/mirror）", err)
	}
	return clean, nil
}

// mirrorSyncFile 是逐文件结果：哪个成功、哪个失败、为什么。
type mirrorSyncFile struct {
	Path   string `json:"path"`
	OK     bool   `json:"ok"`
	Bytes  int64  `json:"bytes,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Error  string `json:"error,omitempty"`
}

// mirrorSyncResult 是整个同步动作的结果（任务结束时审计/界面都用它）。
type mirrorSyncResult struct {
	Dir      string           `json:"dir"`
	Source   string           `json:"source"`
	Manifest string           `json:"manifest"` // 实际用的清单名（镜像版或源站版）
	Version  string           `json:"version"`
	Files    []mirrorSyncFile `json:"files"`
}

// pendingMirror 是一个"已在临时目录校验通过、等待就位"的文件。
type pendingMirror struct {
	staged string   // 临时目录里的绝对路径
	rels   []string // 相对镜像目录的落点（一个源文件可能有多个落点）
	sha    string
	size   int64
	what   string // 界面/日志里的名字
}

// runMirrorSync 执行一次完整同步：验签清单 → 逐个下载核 sha256 → 全部通过后才从临时
// 目录就位（失败/半截文件绝不外露，重复跑幂等）。mirrorBase 是镜像上 zizpanel 的公网
// 基址，仅用于写盘后的自证日志（坑 218）。
func runMirrorSync(ctx context.Context, log tasks.LogFunc, dir, base, mirrorBase string) (*mirrorSyncResult, error) {
	res := &mirrorSyncResult{Dir: dir, Source: base}
	step := func(format string, a ...any) { log(tasks.LevelStep, fmt.Sprintf(format, a...)) }
	out := func(format string, a ...any) { log(tasks.LevelOut, fmt.Sprintf(format, a...)) }
	warn := func(format string, a ...any) { log(tasks.LevelWarn, fmt.Sprintf(format, a...)) }
	bad := func(format string, a ...any) { log(tasks.LevelErr, fmt.Sprintf(format, a...)) }

	step("① 取公网源上的清单并验签")
	data, sig, err := fetchMirrorManifestRaw(ctx, base)
	switch {
	case errors.Is(err, errMirrorManifestAbsent):
		// 源上还没放镜像版清单：如实说明，别让用户以为镜像在分发（坑 218）。
		warn("源上没有镜像版清单，已用源站清单，下载地址可能仍指向源站")
		data, sig, err = upgrade.FetchManifestRaw(ctx, base)
		if err != nil {
			return res, fmt.Errorf("下载清单失败：%w", err)
		}
		res.Manifest = "manifest.json"
	case err != nil:
		return res, fmt.Errorf("下载镜像版清单失败：%w", err)
	default:
		res.Manifest = mirrorManifestName
		out("用的是镜像版清单（%s）", mirrorManifestName)
	}
	// 唯一一次信任根判断：两份清单一视同仁，验签不过就一个字节都不许往镜像目录写。
	if err := upgrade.VerifyManifest(data, sig); err != nil {
		bad("清单验签失败：%v", err)
		return res, fmt.Errorf("清单验签失败：%w（已拒绝写入镜像）", err)
	}
	m, err := upgrade.ParseManifest(data)
	if err != nil {
		return res, fmt.Errorf("清单格式不正确：%w", err)
	}
	res.Version = m.Version
	out("清单验签通过：版本 %s，%d 个架构", m.Version, len(m.Assets))

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("创建镜像目录失败：%w", err)
	}
	// 临时目录放在镜像目录内、权限 0700：nginx worker 既进不来也读不到半截文件。
	stage, err := os.MkdirTemp(dir, ".zp-sync-")
	if err != nil {
		return res, fmt.Errorf("创建临时目录失败：%w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	step("② 逐个下载资产并核 sha256")
	pending := make([]pendingMirror, 0, len(m.Assets)+3)
	// 清单一与签名：验签已通过（内容不重算，它就是要原样发布的字节）。
	manifestRel := "manifest.json"
	if err := writeStaged(stage, manifestRel, data); err != nil {
		return res, err
	}
	pending = append(pending, pendingMirror{staged: filepath.Join(stage, manifestRel), rels: []string{manifestRel},
		sha: sha256Bytes(data), size: int64(len(data)), what: manifestRel})
	sigRel := "manifest.json.sig"
	if err := writeStaged(stage, sigRel, sig); err != nil {
		return res, err
	}
	pending = append(pending, pendingMirror{staged: filepath.Join(stage, sigRel), rels: []string{sigRel},
		sha: sha256Bytes(sig), size: int64(len(sig)), what: sigRel})

	// install.sh：不在清单里（没有 sha 可核），从源取；取不到就是布局不完整，失败。
	install, ierr := mirrorGetBytes(ctx, base+"/install.sh", mirrorInstallShLimit)
	if ierr != nil {
		res.Files = append(res.Files, mirrorSyncFile{Path: "install.sh", Error: ierr.Error()})
		return res, fmt.Errorf("下载 install.sh 失败：%w", ierr)
	}
	installRel := "install.sh"
	if err := writeStaged(stage, installRel, install); err != nil {
		return res, err
	}
	pending = append(pending, pendingMirror{staged: filepath.Join(stage, installRel), rels: []string{installRel},
		sha: sha256Bytes(install), size: int64(len(install)), what: installRel})

	keys := make([]string, 0, len(m.Assets))
	for k := range m.Assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ref := m.Assets[key]
		rawURL, uerr := resolveMirrorAssetURL(base, ref.URL)
		if uerr != nil {
			return res, fmt.Errorf("资源 %s 的地址不合法：%w", key, uerr)
		}
		name := mirrorAssetName(rawURL)
		if name == "" {
			return res, fmt.Errorf("资源 %s 的地址解析不出文件名：%s", key, ref.URL)
		}
		// 镜像版清单的 url 指向镜像自己；照它下会打到正在写的镜像，必须回源站取（坑 218）。
		if mirrorBase != "" && mirrorURLBase(rawURL) == strings.TrimRight(mirrorBase, "/") {
			rawURL = strings.TrimRight(base, "/") + "/download/" + m.Version + "/" + name
		}
		rel := filepath.ToSlash(filepath.Join("download", m.Version, name))
		dst := filepath.Join(stage, filepath.FromSlash(rel))
		out("下载 %s（%s）", name, key)
		lastPct := int64(-1)
		_, derr := upgrade.DownloadTarballWithObserver(ctx, rawURL, dst, ref.SHA256,
			func(written, total int64) {
				if total <= 0 {
					return
				}
				pct := written * 100 / total
				if pct != lastPct && pct%25 == 0 {
					lastPct = pct
					out("  %s：%d%%", name, pct)
				}
			},
			func(_, detail string) { out("  %s：%s", name, detail) })
		if derr != nil {
			bad("%s 下载/校验失败：%v", name, derr)
			res.Files = append(res.Files, mirrorSyncFile{Path: rel, Error: derr.Error()})
			// 校验不过的字节只存在于 0700 临时目录，随 defer 一起删掉。
			return res, fmt.Errorf("资产 %s 下载或校验失败：%w（未写入镜像）", name, derr)
		}
		size := fileSize(dst)
		latest := mirrorLatestName(name, m.Version)
		pending = append(pending, pendingMirror{
			staged: dst,
			rels: []string{
				filepath.ToSlash(filepath.Join("download", m.Version, name)),
				filepath.ToSlash(filepath.Join("download", "latest", latest)),
				latest, // 顶层 latest 包（install.sh 的固定 URL 用）
			},
			sha: ref.SHA256, size: size, what: name,
		})
	}

	step("③ 全部校验通过，从临时目录就位")
	for _, p := range pending {
		for _, rel := range p.rels {
			final := filepath.Join(dir, filepath.FromSlash(rel))
			if perr := placeMirrorFile(p.staged, final); perr != nil {
				res.Files = append(res.Files, mirrorSyncFile{Path: rel, Error: perr.Error()})
				bad("写入 %s 失败：%v", rel, perr)
				return res, fmt.Errorf("写入 %s 失败：%w", rel, perr)
			}
			res.Files = append(res.Files, mirrorSyncFile{Path: rel, OK: true, Bytes: p.size, SHA256: p.sha})
			out("✓ %s（%.1f MB）", rel, float64(p.size)/1024/1024)
		}
	}
	step("④ 自证：写进镜像的清单是否指向镜像自己")
	bases := mirrorManifestBases(m)
	switch {
	case len(bases) == 0:
		out("清单里的下载地址是相对路径（按镜像基址解析）")
	case len(bases) == 1 && bases[0] == strings.TrimRight(mirrorBase, "/"):
		out("✓ 清单里的下载地址指向镜像基址 %s", bases[0])
	default:
		warn("清单下载地址指向 %s，不是镜像基址 %s：镜像分发可能没生效",
			strings.Join(bases, "、"), mirrorBase)
	}
	step("⑤ 完成：manifest.json / install.sh / download/%s 与 latest 都已就位", m.Version)
	return res, nil
}

// writeStaged 把内容写进临时目录（权限 0644：就位后 nginx 要能读）。
func writeStaged(stage, rel string, data []byte) error {
	p := filepath.Join(stage, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// placeMirrorFile 把临时文件就位：同目录下优先硬链接（省空间、原子），
// 文件系统不支持时退回复制。覆盖前先删掉旧文件/软链（部署脚本会留软链）。
func placeMirrorFile(staged, final string) error {
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(staged, final); err == nil {
		return nil
	}
	return copyMirrorFile(staged, final)
}

func copyMirrorFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// mirrorHTTPStatus 带上 HTTP 状态码，让调用方区分 404（源上没有）与真正的下载失败。
type mirrorHTTPStatus struct{ code int }

func (e *mirrorHTTPStatus) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// mirrorManifestAbsent 报告错误是不是"源上没有这个文件"（404/410）。
func mirrorManifestAbsent(err error) bool {
	var se *mirrorHTTPStatus
	return errors.As(err, &se) && (se.code == http.StatusNotFound || se.code == http.StatusGone)
}

// fetchMirrorManifestRaw 取镜像版清单与签名；源上没有（404/410）返回 errMirrorManifestAbsent。
// 清单在而签名不在 = 发布不完整，明确失败而**不**回落（坑 218）。
func fetchMirrorManifestRaw(ctx context.Context, base string) ([]byte, []byte, error) {
	data, err := mirrorGetBytes(ctx, base+"/"+mirrorManifestName, mirrorManifestLimit)
	if err != nil {
		if mirrorManifestAbsent(err) {
			return nil, nil, errMirrorManifestAbsent
		}
		return nil, nil, err
	}
	sig, err := mirrorGetBytes(ctx, base+"/"+mirrorManifestName+".sig", mirrorManifestLimit)
	if err != nil {
		if mirrorManifestAbsent(err) {
			return nil, nil, fmt.Errorf("源上有 %s 却没有配套签名（发布不完整，不回落）", mirrorManifestName)
		}
		return nil, nil, err
	}
	return data, sig, nil
}

// mirrorGetBytes 做一次带上限的 GET（install.sh 没有清单 sha，只能从源取）。
func mirrorGetBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zizpanel-mirror")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &mirrorHTTPStatus{code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("超过 %d 字节上限", limit)
	}
	return b, nil
}

// resolveMirrorAssetURL 把清单里的 url 补成绝对地址（相对地址按源基址解析）。
func resolveMirrorAssetURL(base, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	b, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return "", err
	}
	return b.ResolveReference(u).String(), nil
}

// mirrorAssetName 从资产地址里取文件名。
func mirrorAssetName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return filepath.Base(u.Path)
}

// mirrorURLBase 取资产地址的"站点+目录"基址（去掉 /download/ 之后的路径）；
// 相对地址返回空串（坑 218 的自证与回源判断都用它）。
func mirrorURLBase(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() {
		return ""
	}
	p := u.Path
	if i := strings.Index(p, "/download/"); i >= 0 {
		p = p[:i]
	} else if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[:i]
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(p, "/")
}

// mirrorManifestBases 汇总清单里 asset url 指向的绝对基址（去重、排序）。
func mirrorManifestBases(m *upgrade.Manifest) []string {
	seen := make(map[string]bool, len(m.Assets))
	out := make([]string, 0, len(m.Assets))
	for _, ref := range m.Assets {
		if b := mirrorURLBase(ref.URL); b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

// mirrorLatestName 把版本名换成 latest 名（zizpanel_1.6.6_darwin_arm64.tar.gz →
// zizpanel_latest_darwin_arm64.tar.gz；替换不到就原样返回，绝不瞎猜）。
func mirrorLatestName(name, version string) string {
	if version == "" {
		return name
	}
	token := "_" + version + "_"
	i := strings.Index(name, token)
	if i < 0 {
		return name
	}
	return name[:i] + "_latest_" + name[i+len(token):]
}

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileSize 返回文件字节数（读不到返回 0，只用于展示，不据此判成败）。
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
