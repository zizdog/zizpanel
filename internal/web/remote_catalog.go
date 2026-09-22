package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/upgrade"
)

// remote_catalog.go —— 镜像站下发的**应用目录**（用户 2026-09-22：
// "增加应用……我测试不更新 panel 是否能显示"）。
//
// 形态：镜像站上 <mirror>/apps/catalog.json + apps/catalog.json.sig（Ed25519，
// 复用面板内嵌的发布公钥 —— 与在线升级同一套信任根）。
//
// 安全边界（别松，这是"远端数据"唯一被信任的地方）：
//  1. **必须验签**：没有公钥 / 取不到签名 / 验签失败一律不采用（fail closed），
//     继续用上一份缓存或内置目录；
//  2. **只追加新 ID**：与内置撞 ID 的远端条目被忽略（远端数据不许改内置应用的安装行为）；
//  3. **只认通用轨**：rail ∈ {brew, compose}，安装走面板已有的通用流程。
//     其它轨（含需要新安装器的）如实拒绝并写明原因 —— 远端数据**不许**携带任意命令步骤，
//     否则"改一份镜像文件"就等于对所有面板远程执行命令。
const (
	remoteCatalogAsset    = "apps/catalog.json"
	remoteCatalogTTL      = 30 * time.Minute
	remoteCatalogTimeout  = 8 * time.Second
	remoteCatalogMaxBytes = 2 << 20 // 目录不该有多大；给 2MB 上限防呆
)

// remoteCatalogFile 是远端目录文件的格式。
type remoteCatalogFile struct {
	Version     string         `json:"version"`
	GeneratedAt string         `json:"generated_at"`
	Apps        []services.App `json:"apps"`
}

// remoteCatalogState 是进程内状态：来源 / 时间 / 失败原因 / 被拒条目。
// 接口据此**如实汇报**（"远端目录这次没复核成"必须能被用户看见，不能假装没有）。
var remoteCatalogState struct {
	mu       sync.Mutex
	loaded   bool
	source   string // remote（这次真拉了）/ cache（用的是本地缓存）/ ""（没有）
	fetched  time.Time
	err      string
	rejected []string
	inflight bool
}

// remoteCatalogURL 返回目录与签名的地址；没配镜像基址时返回空（调用方如实跳过）。
func (s *Server) remoteCatalogURL() (data, sig string) {
	base := strings.TrimRight(strings.TrimSpace(s.Cfg.MirrorBase), "/")
	if base == "" {
		return "", ""
	}
	return base + "/" + remoteCatalogAsset, base + "/" + remoteCatalogAsset + ".sig"
}

// catalogCachePath 是验签通过后落盘的缓存（面板重启后先用它，避免每次启动都等网络）。
func (s *Server) catalogCachePath() string {
	return filepath.Join(s.Cfg.DataDir, "cache", "remote-catalog.json")
}

// certCatalogCache 是落盘的形态：验签并校验过的条目 + 拉取时间（不再存签名，本地文件与配置同等信任）。
type cachedRemoteCatalog struct {
	FetchedAt string         `json:"fetched_at"`
	Source    string         `json:"source"`
	Apps      []services.App `json:"apps"`
}

// ensureRemoteCatalog 保证"远端目录已被考虑过一次"：先读缓存，再按 TTL 决定要不要联网刷。
// 绝不阻塞首屏：联网刷新走后台（fresh=1 时由调用方要求同步刷）。
func (s *Server) ensureRemoteCatalog(ctx context.Context) {
	remoteCatalogState.mu.Lock()
	loaded := remoteCatalogState.loaded
	remoteCatalogState.mu.Unlock()
	if !loaded {
		s.loadRemoteCatalogCache()
	}
	remoteCatalogState.mu.Lock()
	at := remoteCatalogState.fetched
	src := remoteCatalogState.source
	remoteCatalogState.mu.Unlock()
	if src == "remote" && time.Since(at) < remoteCatalogTTL {
		return
	}
	// 后台刷一次（不占请求路径）。失败只在接口里如实汇报，不影响列表。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), remoteCatalogTimeout)
		defer cancel()
		_ = s.refreshRemoteCatalog(ctx)
	}()
}

// loadRemoteCatalogCache 读本地缓存并装上（不联网）。
func (s *Server) loadRemoteCatalogCache() {
	remoteCatalogState.mu.Lock()
	remoteCatalogState.loaded = true
	remoteCatalogState.mu.Unlock()

	raw, err := os.ReadFile(s.catalogCachePath())
	if err != nil {
		// 没有缓存是正常状态（新装/没发布过远端目录）：保持内置目录，不报错。
		return
	}
	var c cachedRemoteCatalog
	if json.Unmarshal(raw, &c) != nil {
		return
	}
	at, _ := time.Parse(time.RFC3339, c.FetchedAt)
	services.SetRemoteCatalog(s.validRemoteApps(c.Apps, true), "cache", at)
	remoteCatalogState.mu.Lock()
	remoteCatalogState.source = "cache"
	remoteCatalogState.fetched = at
	remoteCatalogState.mu.Unlock()
}

// refreshRemoteCatalog 拉一次远端目录：取原文 + 签名 → **验签** → 校验条目 → 装上 + 落缓存。
// 任何一步失败都返回 error，并且**不动**已经装上的目录（fail closed：宁可显示旧的，
// 也不能显示没验签的东西）。
func (s *Server) refreshRemoteCatalog(ctx context.Context) error {
	dataURL, sigURL := s.remoteCatalogURL()
	if dataURL == "" {
		return errors.New("没有配置镜像基址，无法获取远端应用目录")
	}
	remoteCatalogState.mu.Lock()
	if remoteCatalogState.inflight {
		remoteCatalogState.mu.Unlock()
		return errors.New("上一次远端目录刷新还在进行中")
	}
	remoteCatalogState.inflight = true
	remoteCatalogState.mu.Unlock()
	defer func() {
		remoteCatalogState.mu.Lock()
		remoteCatalogState.inflight = false
		remoteCatalogState.mu.Unlock()
	}()

	// fail closed：没有内嵌公钥就一个请求都不发（拿了也没法验）。
	if !upgrade.HasPublicKey() {
		return errors.New("面板没有内嵌发布公钥，拒绝采用无法验签的远端目录")
	}
	body, err := httpGetLimited(ctx, dataURL, remoteCatalogMaxBytes)
	if err != nil {
		s.recordCatalogError("取远端目录失败：" + err.Error())
		return err
	}
	sig, err := httpGetLimited(ctx, sigURL, 1<<12)
	if err != nil {
		s.recordCatalogError("取远端目录签名失败：" + err.Error())
		return err
	}
	if err := upgrade.VerifyManifest(body, sig); err != nil {
		s.recordCatalogError("远端目录验签失败：" + err.Error())
		return err
	}
	var f remoteCatalogFile
	if err := json.Unmarshal(body, &f); err != nil {
		s.recordCatalogError("远端目录格式不对：" + err.Error())
		return err
	}
	apps := s.validRemoteApps(f.Apps, false)
	at := time.Now()
	services.SetRemoteCatalog(apps, "remote", at)
	remoteCatalogState.mu.Lock()
	remoteCatalogState.source = "remote"
	remoteCatalogState.fetched = at
	remoteCatalogState.err = ""
	remoteCatalogState.rejected = rejectedRemoteApps
	remoteCatalogState.mu.Unlock()
	s.saveRemoteCatalogCache(apps, at)
	return nil
}

// rejectedRemoteApps 记录"这次被拒的条目及原因"（接口如实汇报；不进目录）。
var rejectedRemoteApps []string

// validRemoteApps 校验远端条目，返回**可用**的那些（不可用的记原因、不采用）。
//
// 规则：ID 不能空；rail 只认 brew / compose 且必须给对应字段；与内置撞 ID 的一律忽略。
func (s *Server) validRemoteApps(in []services.App, fromCache bool) []services.App {
	builtin := map[string]bool{}
	for _, a := range services.BuiltinCatalogIDs() {
		builtin[a] = true
	}
	out := make([]services.App, 0, len(in))
	rej := make([]string, 0)
	seen := map[string]bool{}
	for _, a := range in {
		id := strings.TrimSpace(a.ID)
		switch {
		case id == "":
			rej = append(rej, "有条目没有 id")
			continue
		case seen[id]:
			rej = append(rej, id+"：重复条目")
			continue
		case builtin[id]:
			rej = append(rej, id+"：与内置条目撞 ID（远端只能追加新应用）")
			continue
		}
		a.ID = id
		a.Remote = true
		switch strings.ToLower(strings.TrimSpace(a.Rail)) {
		case "brew":
			if strings.TrimSpace(a.BrewFormula) == "" {
				rej = append(rej, id+"：rail=brew 但没写 brew_formula")
				continue
			}
			a.Kind = services.KindNative
		case "compose":
			if strings.TrimSpace(a.ComposeYAML) == "" {
				rej = append(rej, id+"：rail=compose 但没写 compose_yaml")
				continue
			}
			a.Kind = services.KindCompose
		default:
			rej = append(rej, id+"：只支持 rail=brew / compose（这条需要面板升级后才支持）")
			continue
		}
		seen[id] = true
		out = append(out, a)
	}
	if !fromCache {
		rejectedRemoteApps = rej
	}
	return out
}

// recordCatalogError 记下这次失败的真实原因（接口会把它发给前端如实显示）。
func (s *Server) recordCatalogError(msg string) {
	remoteCatalogState.mu.Lock()
	defer remoteCatalogState.mu.Unlock()
	remoteCatalogState.err = msg
}

// saveRemoteCatalogCache 把**已验签、已校验**的条目落盘（带拉取时间）。
func (s *Server) saveRemoteCatalogCache(apps []services.App, at time.Time) {
	path := s.catalogCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(cachedRemoteCatalog{
		FetchedAt: at.Format(time.RFC3339),
		Source:    "remote",
		Apps:      apps,
	}, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// remoteCatalogReport 给接口的如实汇报。
func remoteCatalogReport() map[string]any {
	remoteCatalogState.mu.Lock()
	defer remoteCatalogState.mu.Unlock()
	out := map[string]any{
		"source": remoteCatalogState.source, // remote / cache / ""（没有远端目录）
		"error":  remoteCatalogState.err,
	}
	if !remoteCatalogState.fetched.IsZero() {
		out["fetched_at"] = remoteCatalogState.fetched.Format(time.RFC3339)
	}
	if len(remoteCatalogState.rejected) > 0 {
		out["rejected"] = remoteCatalogState.rejected
	}
	return out
}

// httpGetLimited 取一个大小受限的 HTTP 响应（远端目录用的是 HTTPS 公网地址）。
func httpGetLimited(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: remoteCatalogTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("响应超过 %d 字节上限", limit)
	}
	return data, nil
}
