package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ============================================================================
//  镜像站公告「再广播」+ 面板侧轻量轮询
//
//  镜像侧的预取（api_mirror_prefetch.go）发布完成后会写 <mirror>/apps/announce.json：
//    {"revision":"…","published_at":"…","apps":{"<应用>":"<镜像上的最新版本>"}}
//  revision 只由 apps 决定 —— 内容没变就不改动公告，所有面板也就不会被打扰。
//
//  面板侧：启动后 + 每 6 小时拉一次这个**几百字节**的公告，带 If-None-Match。
//    · 304 命中 / revision 没变 ⇒ 不重算、不提示（changed=false）；
//    · revision 变了 ⇒ 走**既有**的更新检查流程（marketUpdates）算出真实的可更新数；
//    · 拉取失败 / 没有数据 ⇒ ready=false、不显示任何"可更新"提示（绝不猜）。
//  市场缓存与面板自身升级流程都不受影响：公告只是一个变更信号。
// ============================================================================

const (
	// mirrorAnnounceName 是镜像站上的公告文件名（与预取侧一致）。
	mirrorAnnounceName = "announce.json"
	// mirrorAnnounceLimit 是公告大小上限（它只该有几百字节）。
	mirrorAnnounceLimit = 64 << 10
	// mirrorAnnounceSettingKey 是面板记住"上次看到的公告"的设置键。
	mirrorAnnounceSettingKey = "mirror_announce_state"
	// mirrorAnnounceTimeout 是单次公告请求的超时。
	mirrorAnnounceTimeout = 15 * time.Second
)

// mirrorAnnounceDoc 是公告文件的形状。
type mirrorAnnounceDoc struct {
	Revision    string            `json:"revision"`
	PublishedAt string            `json:"published_at"`
	Apps        map[string]string `json:"apps"`
}

// mirrorAnnounceRevisionOf 由"应用→版本"算公告版本（只依赖内容，与时间无关）。
// 内容不变 ⇒ 版本不变 ⇒ 公告不重写、面板不重算。
func mirrorAnnounceRevisionOf(apps map[string]string) string {
	if len(apps) == 0 {
		return ""
	}
	b, err := json.Marshal(apps)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mirrorAnnounceState 是面板落库的"上次看到的公告"。
type mirrorAnnounceState struct {
	ETag        string   `json:"etag"`
	Revision    string   `json:"revision"`
	Available   int      `json:"available"`
	Apps        []string `json:"apps,omitempty"`
	PublishedAt string   `json:"published_at,omitempty"`
	At          string   `json:"at,omitempty"`
}

// mirrorAnnounceResponse 是 GET /api/v1/market/announce 的响应。
//
// Prompt 是前端唯一该据此提示的字段：它只在"公告真的变了、而且确实有可更新应用"时为 true。
type mirrorAnnounceResponse struct {
	Ready       bool     `json:"ready"`
	Changed     bool     `json:"changed"`
	Prompt      bool     `json:"prompt"`
	Available   int      `json:"available"`
	Apps        []string `json:"apps,omitempty"`
	PublishedAt string   `json:"published_at,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

// handleMarketAnnounce 处理 GET /api/v1/market/announce（登录即可）。
func (s *Server) handleMarketAnnounce(w http.ResponseWriter, r *http.Request) {
	ok(w, s.mirrorAnnounce(r.Context()))
}

// mirrorAnnounce 拉一次公告并按需重算"可更新"计数。
func (s *Server) mirrorAnnounce(ctx context.Context) mirrorAnnounceResponse {
	base := strings.TrimRight(strings.TrimSpace(s.Cfg.MirrorBase), "/")
	if base == "" {
		return mirrorAnnounceResponse{Reason: "还没有配置应用包镜像基址（面板设置 → 应用包镜像）"}
	}
	st := s.loadMirrorAnnounceState(ctx)
	doc, etag, notModified, err := fetchMirrorAnnounce(ctx, base, st.ETag)
	if err != nil {
		// 拉不到就什么都不提示（不猜"没有更新"，也不猜"有更新"）。
		return mirrorAnnounceResponse{Reason: err.Error()}
	}
	prev := func() mirrorAnnounceResponse {
		return mirrorAnnounceResponse{
			Ready: true, Changed: false, Available: st.Available,
			Apps: st.Apps, PublishedAt: st.PublishedAt,
		}
	}
	if notModified {
		return prev()
	}
	if doc.Revision == st.Revision && doc.Revision != "" {
		return prev()
	}
	// 公告变了：走既有的更新检查流程拿真实结论（不新造一套版本比较）。
	updates := s.marketUpdates(ctx, true)
	apps := make([]string, 0, len(updates.Items))
	for id, it := range updates.Items {
		if it.UpdateAvailable && !it.Unknown {
			apps = append(apps, id)
		}
	}
	sort.Strings(apps)
	next := mirrorAnnounceState{
		ETag: etag, Revision: doc.Revision, Available: len(apps), Apps: apps,
		PublishedAt: doc.PublishedAt, At: time.Now().Format(time.RFC3339),
	}
	s.saveMirrorAnnounceState(ctx, next)
	return mirrorAnnounceResponse{
		Ready: true, Changed: true, Prompt: len(apps) > 0, Available: len(apps),
		Apps: apps, PublishedAt: doc.PublishedAt,
	}
}

// loadMirrorAnnounceState 读上次的公告状态（读不到/坏 JSON 就当没有，不猜）。
func (s *Server) loadMirrorAnnounceState(ctx context.Context) mirrorAnnounceState {
	var st mirrorAnnounceState
	v, err := s.Store.GetSetting(ctx, mirrorAnnounceSettingKey)
	if err != nil || strings.TrimSpace(v) == "" {
		return st
	}
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return mirrorAnnounceState{}
	}
	return st
}

// saveMirrorAnnounceState 记住这次看到的公告（写失败只记日志，下一次照常重算）。
func (s *Server) saveMirrorAnnounceState(ctx context.Context, st mirrorAnnounceState) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := s.Store.SetSetting(ctx, mirrorAnnounceSettingKey, string(b)); err != nil {
		s.Log.Warn("保存镜像公告状态失败（下次会重新检查）：%v", err)
	}
}

// fetchMirrorAnnounce 取公告。
//
// 304 ⇒ notModified=true（不解析、不重算）；其余非 200 一律 error（调用方不提示）。
// etag 为空时不带 If-None-Match，直接取回完整公告自行比对 revision。
func fetchMirrorAnnounce(ctx context.Context, base, etag string) (*mirrorAnnounceDoc, string, bool, error) {
	u := base + "/" + mirrorAppsSubdir + "/" + mirrorAnnounceName
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", false, fmt.Errorf("公告地址不合法（%s）：%w", u, err)
	}
	req.Header.Set("User-Agent", "zizpanel-announce")
	if strings.TrimSpace(etag) != "" {
		req.Header.Set("If-None-Match", etag)
	}
	res, err := mirrorHTTPClient(mirrorAnnounceTimeout).Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("取镜像公告失败（%s）：%w", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("镜像公告不可用（%s，HTTP %d）", u, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, mirrorAnnounceLimit+1))
	if err != nil {
		return nil, "", false, fmt.Errorf("读镜像公告失败（%s）：%w", u, err)
	}
	if len(body) > mirrorAnnounceLimit {
		return nil, "", false, fmt.Errorf("镜像公告超过 %d 字节上限（%s）", mirrorAnnounceLimit, u)
	}
	var doc mirrorAnnounceDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "", false, fmt.Errorf("镜像公告不是合法 JSON（%s）：%w", u, err)
	}
	if strings.TrimSpace(doc.Revision) == "" {
		// 公告没写 revision（手写/旧版）：按内容算一个，行为一致。
		sum := sha256.Sum256(body)
		doc.Revision = hex.EncodeToString(sum[:])
	}
	newETag := strings.TrimSpace(res.Header.Get("ETag"))
	if newETag == "" {
		newETag = `"` + doc.Revision + `"`
	}
	return &doc, newETag, false, nil
}
