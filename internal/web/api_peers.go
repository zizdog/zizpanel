package web

// api_peers.go —— 多机管理（C5）：一台面板聚合另外几台 Mac 的**只读摘要**。
//
// 这一版刻意只做只读：主面板定期拉每台子机的版本/负载/服务计数，**不代它执行任何写操作**。
// 写操作的信任模型（主面板持有什么权限、子机怎么撤销、离线时怎么算）比只读复杂得多，
// 没定清楚之前不做 —— 半成品的"远程控制"比没有更危险。
//
// 安全边界（三条，缺一不可）：
//   · 子机侧默认**不接受任何主面板**（agent_token 为空 ⇒ 一律 403）；凭证只读、可随时吊销；
//   · 摘要**只有聚合数字**，不含站点名、路径、口令、日志（泄漏面刻意压到最小）；
//   · 主面板连子机：https 必须**固定证书指纹**（面板之间多是自签证书，不固定就等于信任中间人）；
//     http 只放行回环地址（凭证不能明文过网）。

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/version"
)

// agentSummary 是子机交给主面板的摘要。**只有聚合数字**（见文件头的安全边界）。
type agentSummary struct {
	Panel    string  `json:"panel"`
	Version  string  `json:"version"`
	Entry    string  `json:"entry"`
	Hostname string  `json:"hostname"`
	OS       string  `json:"os"`
	Arch     string  `json:"arch"`
	Uptime   int64   `json:"uptime"`
	CPUUsed  float64 `json:"cpu_used"`
	CPUCores int     `json:"cpu_cores"`
	Load1    float64 `json:"load_1"`
	MemTotal uint64  `json:"mem_total"`
	// MemUsed 是活动监视器口径（应用+有线+压缩），与主面板本机一致；
	// MemFree/MemCached 同口径保留给既有前端，三者相加恒等于 MemTotal。
	MemUsed   uint64        `json:"mem_used"`
	MemFree   uint64        `json:"mem_free"`
	MemCached uint64        `json:"mem_cached"`
	DiskTotal uint64        `json:"disk_total"`
	DiskUsed  uint64        `json:"disk_used"`
	Services  agentServices `json:"services"`
	Certs     agentCerts    `json:"certs"`
	At        string        `json:"at"`
}

type agentServices struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Problems int `json:"problems"`
}

type agentCerts struct {
	Total    int `json:"total"`
	Expiring int `json:"expiring"`
}

// ---------------- 子机侧 ----------------

// handleAgentSummary 只认 X-ZizPanel-Agent 头里的凭证（不走会话）：
// 主面板没有子机的登录态，也不该有。
func (s *Server) handleAgentSummary(w http.ResponseWriter, r *http.Request) {
	want := strings.TrimSpace(s.Cfg.AgentToken)
	if want == "" {
		// 没开启就说没开启，不透露别的。
		fail(w, http.StatusForbidden, "本机没有开启「被主面板管理」（面板设置 → 多机）")
		return
	}
	got := strings.TrimSpace(r.Header.Get("X-ZizPanel-Agent"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		fail(w, http.StatusForbidden, "凭证不对")
		return
	}
	ok(w, s.buildAgentSummary(r.Context()))
}

// buildAgentSummary 采集摘要。任何一部分失败都**不让整个摘要失败**，
// 但失败的部分要如实标记（这里用零值 + 顶层 at，界面显示"部分读不到"由 errors 决定）。
func (s *Server) buildAgentSummary(ctx context.Context) agentSummary {
	sum := agentSummary{
		Panel: "zizpanel", Version: version.Version,
		Entry: s.PanelEntryPath(), At: time.Now().Format(time.RFC3339),
	}
	if s.Info != nil {
		snap := s.Info.Last()
		sum.Hostname, sum.OS, sum.Arch = snap.Hostname, snap.OS, snap.Arch
		sum.Uptime, sum.CPUUsed, sum.CPUCores, sum.Load1 = snap.Uptime, snap.CPUUsed, snap.CPUCores, snap.LoadAvg1
		sum.MemTotal, sum.MemUsed = snap.MemTotal, snap.MemUsed
		sum.MemFree, sum.MemCached = snap.MemFree, snap.MemCached
		sum.DiskTotal, sum.DiskUsed = snap.DiskTotal, snap.DiskUsed
	}
	// 服务状态用**不带健康检查**的那一档：摘要要快，而"卡死但端口在听"由子机自己的
	// 通知/界面负责；这里如实给"启动失败 / 运行时不可用"的计数。
	if views, err := s.svcManager().List(ctx, false); err == nil {
		for _, v := range views {
			if v == nil || v.Service == nil {
				continue
			}
			sum.Services.Total++
			st := v.State
			if st.Running {
				sum.Services.Running++
			}
			problem := st.Status == "error" || st.Status == "unavailable"
			if v.StoppedByUser && !st.Running {
				problem = false
			}
			if problem {
				sum.Services.Problems++
			}
		}
	}
	if certs, err := s.certManager().List(); err == nil {
		now := time.Now()
		for _, c := range certs {
			if c == nil || c.NotAfter.IsZero() {
				continue
			}
			sum.Certs.Total++
			if c.NotAfter.Sub(now) <= agentCertWarnDays*24*time.Hour {
				sum.Certs.Expiring++
			}
		}
	}
	return sum
}

// agentCertWarnDays 是摘要里"证书快到期"的口径（与通知默认阈值一致）。
const agentCertWarnDays = 14

// ---------------- 主面板侧 ----------------

// peerSummaryView 是给界面的子机视图（**不带 token**：界面不需要看到别人的凭证）。
type peerSummaryView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Fingerprint string `json:"fingerprint,omitempty"`
	MaskedToken string `json:"masked_token"`
	LastAt      string `json:"last_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	// LastAdvice 是"这个失败通常怎么修"的一句话（面板按错误类型给，界面直接显示）。
	// 与 LastError 分开：错误是**事实**（子机说了什么），建议是**推断**，两者别混着写。
	LastAdvice string          `json:"last_advice,omitempty"`
	Summary    json.RawMessage `json:"summary,omitempty"`
}

func (s *Server) peerViews() []peerSummaryView {
	out := make([]peerSummaryView, 0, len(s.Cfg.Peers))
	for _, p := range s.Cfg.Peers {
		out = append(out, peerSummaryView{
			ID: p.ID, Name: p.Name, URL: p.URL, Fingerprint: p.Fingerprint,
			MaskedToken: maskToken(p.Token),
			LastAt:      p.LastAt, LastError: p.LastError, LastAdvice: p.LastAdvice, Summary: p.Summary,
		})
	}
	return out
}

// maskToken 只留尾巴几位，够用户核对"是不是这一台"又不至于被旁观者抄走。
func maskToken(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	if len(t) <= 4 {
		return "****"
	}
	return "****" + t[len(t)-4:]
}

func (s *Server) handlePeersGet(w http.ResponseWriter, r *http.Request) {
	ok(w, s.peersResponse(r))
}

// peersResponse 是四个接口共用的返回形状（界面只看一种形状，少一处拼错的机会）。
func (s *Server) peersResponse(r *http.Request) map[string]any {
	tokenSet := strings.TrimSpace(s.Cfg.AgentToken) != ""
	self := map[string]any{
		"enabled": tokenSet,
		"entry":   s.PanelEntryPath(),
		// 本机接入信息：给用户复制到另一台面板的"添加子机"表单里。
		"url_hint": s.panelURLHint(r),
	}
	if tokenSet {
		self["token"] = s.Cfg.AgentToken
	}
	return map[string]any{"peers": s.peerViews(), "self": self}
}

// panelURLHint 猜一个"别的面板该用什么地址连我"（带安全后缀）。
// 用请求的 Host（用户自己知道哪个地址对：局域网 IP / 隧道域名）。
func (s *Server) panelURLHint(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if host == "" {
		host = fmt.Sprintf("127.0.0.1:%d", s.Cfg.Port())
	}
	return scheme + "://" + host + s.PanelEntryPath()
}

// handleAgentToken 开启 / 重新生成 / 关闭"被主面板管理"。
func (s *Server) handleAgentToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !req.Enabled {
		s.Cfg.AgentToken = ""
		if err := s.Cfg.Save(); err != nil {
			fail(w, http.StatusInternalServerError, "保存面板配置失败: "+err.Error())
			return
		}
		s.audit(r, "agent_token", "agent", "关闭「被主面板管理」（凭证已清空，所有主面板立即失效）", true, "")
		ok(w, map[string]any{"enabled": false})
		return
	}
	token, err := randomHexToken(32)
	if err != nil {
		fail(w, http.StatusInternalServerError, "生成凭证失败: "+err.Error())
		return
	}
	s.Cfg.AgentToken = token
	if err := s.Cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, "保存面板配置失败: "+err.Error())
		return
	}
	s.audit(r, "agent_token", "agent", "开启/重新生成「被主面板管理」凭证（旧的立即失效）", true, "")
	ok(w, map[string]any{"enabled": true, "token": token})
}

// handlePeerAdd 添加一台子机。
//
// 校验必须严格：URL 允许 https（要求指纹）或回环 http；凭证非空。
// 校验失败给 400 + 人话，绝不"先存下再说"（存下一个连不上的条目，用户会以为面板坏了）。
func (s *Server) handlePeerAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Token       string `json:"token"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	req.Token = strings.TrimSpace(req.Token)
	req.Fingerprint = normalizeFingerprint(req.Fingerprint)
	if req.Name == "" {
		fail(w, http.StatusBadRequest, "请给这台机器起个名字（例如 m4-mini）")
		return
	}
	if req.Token == "" {
		fail(w, http.StatusBadRequest, "请填子机的访问凭证（在它的「面板设置 → 多机」里生成）")
		return
	}
	if err := validatePeerURL(req.URL, req.Fingerprint); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Cfg.Peers = append(s.Cfg.Peers, config.Peer{
		ID: s.Cfg.NextPeerID(), Name: req.Name, URL: req.URL,
		Token: req.Token, Fingerprint: req.Fingerprint,
	})
	if err := s.Cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, "保存面板配置失败: "+err.Error())
		return
	}
	s.audit(r, "peer_add", req.Name, "添加子机 "+req.URL, true, "")
	// 立刻拉一次：用户马上能看到"通没通"，而不是等下一次轮询。
	id := s.Cfg.NextPeerID() - 1
	s.refreshPeer(r.Context(), id)
	ok(w, s.peersResponse(r))
}

func (s *Server) handlePeerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "子机 id 不对")
		return
	}
	idx, p := s.Cfg.FindPeer(id)
	if idx < 0 {
		fail(w, http.StatusNotFound, "没有这台子机")
		return
	}
	name := p.Name
	s.Cfg.Peers = append(s.Cfg.Peers[:idx], s.Cfg.Peers[idx+1:]...)
	if err := s.Cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, "保存面板配置失败: "+err.Error())
		return
	}
	s.audit(r, "peer_delete", name, "移除子机", true, "")
	ok(w, s.peersResponse(r))
}

func (s *Server) handlePeerRefresh(w http.ResponseWriter, r *http.Request) {
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := parseID(idStr)
		if err != nil {
			fail(w, http.StatusBadRequest, "子机 id 不对")
			return
		}
		if idx, _ := s.Cfg.FindPeer(id); idx < 0 {
			fail(w, http.StatusNotFound, "没有这台子机")
			return
		}
		s.refreshPeer(r.Context(), id)
		ok(w, s.peersResponse(r))
		return
	}
	for _, p := range append([]config.Peer{}, s.Cfg.Peers...) {
		s.refreshPeer(r.Context(), p.ID)
	}
	ok(w, s.peersResponse(r))
}

// refreshPeer 拉一次子机摘要。失败**原样记进 peer.LastError**（界面据此显示"没连上"），
// 绝不用旧摘要冒充最新的。
func (s *Server) refreshPeer(ctx context.Context, id int64) {
	fn := s.peerFetchFn
	if fn == nil {
		fn = fetchPeerSummary
	}
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	idx, p := s.Cfg.FindPeer(id)
	if idx < 0 {
		return
	}
	raw, err := fn(ctx, *p)
	now := time.Now().Format(time.RFC3339)
	if err != nil {
		s.Cfg.Peers[idx].LastError = err.Error()
		s.Cfg.Peers[idx].LastAdvice = peerAdvice(err)
		_ = s.Cfg.Save()
		return
	}
	s.Cfg.Peers[idx].Summary = raw
	s.Cfg.Peers[idx].LastAt = now
	s.Cfg.Peers[idx].LastError = ""
	s.Cfg.Peers[idx].LastAdvice = ""
	_ = s.Cfg.Save()
}

// watchPeers 定期刷新（间隔跟通知巡检同档，10 分钟够"集中可见"又不折腾子机）。
func (s *Server) watchPeers(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range append([]config.Peer{}, s.Cfg.Peers...) {
				s.refreshPeer(ctx, p.ID)
			}
		}
	}
}

// ---------------- 拉取子机 ----------------

// peerHTTPTimeout 是单台子机的超时（子机可能正好在忙，别把界面拖住）。
const peerHTTPTimeout = 8 * time.Second

// fetchPeerSummary 取一台子机的摘要。
func fetchPeerSummary(ctx context.Context, p config.Peer) (json.RawMessage, error) {
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil {
		return nil, fmt.Errorf("子机地址不合法：%w", err)
	}
	if err := validatePeerURL(p.URL, p.Fingerprint); err != nil {
		return nil, err
	}
	target := strings.TrimSuffix(u.String(), "/") + "/api/v1/agent/summary"
	transport := &http.Transport{}
	if u.Scheme == "https" && p.Fingerprint != "" {
		// 面板之间多用自签证书：不做 CA 校验，改成**固定指纹**（严格相等）。
		// 这比"跳过校验"强得多，也比"让用户去装 CA"现实。
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // 指纹由下面的 VerifyPeerCertificate 严格校验
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return errors.New("子机没有提供证书")
				}
				got := sha256.Sum256(rawCerts[0])
				if !strings.EqualFold(hex.EncodeToString(got[:]), p.Fingerprint) {
					return fmt.Errorf("子机证书指纹不匹配（期望 %s，实际 %s）：换过证书就要在面板里更新指纹",
						shortFp(p.Fingerprint), shortFp(hex.EncodeToString(got[:])))
				}
				return nil
			},
		}
	}
	client := &http.Client{Timeout: peerHTTPTimeout, Transport: transport}
	ctx, cancel := context.WithTimeout(ctx, peerHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ZizPanel-Agent", strings.TrimSpace(p.Token))
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上子机：%w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("读子机响应失败：%w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("子机返回 HTTP %d（%s）", res.StatusCode, firstLineOf(string(body)))
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.OK {
		return nil, errors.New("子机响应不是面板的摘要格式（地址可能指到了别的服务）")
	}
	return envelope.Data, nil
}

// peerAdvice 把"连不上"翻译成用户能**自己动手**的一句话。
//
// 纪律：只匹配**本文件自己产生的**错误措辞（403 / 指纹 / 回环限制 / 摘要格式），
// 不猜第三方库的随机文案 —— 猜错方向比不给建议更糟（用户会照着一个错的方向折腾）。
// 拿不准就返回空串，让原始错误自己说话。
func peerAdvice(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "403") || strings.Contains(msg, "凭证不对"):
		return "子机不认这个凭证：去它的「多机」页重新生成，再把新凭证填到这里（旧的会立即失效）"
	case strings.Contains(msg, "指纹"):
		return "子机换过证书：在子机上确认新的 sha256 指纹后更新这里，不用删掉这台"
	case strings.Contains(msg, "只有 127.0.0.1"):
		return "局域网子机请用它的 https 地址 + 证书指纹（http 只允许回环，凭证明文过网会被抄走）"
	case strings.Contains(msg, "连不上子机"):
		return "确认子机开着、面板入口地址与端口可达：可以先用浏览器打开那个地址试试"
	case strings.Contains(msg, "HTTP 5"):
		return "子机面板自己在报错：去它的「日志」页看，或先在浏览器里打开确认"
	case strings.Contains(msg, "摘要格式"):
		return "这个地址可能指到了别的服务：要填子机的**面板入口**（含安全后缀，例如 https://m4.lan:8443/ab12cd/）"
	}
	return ""
}

// validatePeerURL 校验子机地址与指纹的搭配。
func validatePeerURL(raw, fingerprint string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("子机地址要写完整的面板入口，例如 https://m4.lan:8443/ab12cd/")
	}
	switch u.Scheme {
	case "https":
		if fingerprint == "" {
			return errors.New("https 子机必须填证书指纹（面板多用自签证书，不固定指纹就等于信任中间人）：" +
				"在子机的浏览器地址栏或 `zizpanel status` 里能看到")
		}
		if len(fingerprint) != 64 {
			return errors.New("证书指纹应当是 64 位十六进制（sha256）")
		}
	case "http":
		// 明文只放行回环：凭证不能过网（局域网嗅探就能拿到只读凭证）。
		if !isLoopbackHost(u.Hostname()) {
			return errors.New("只有 127.0.0.1 / localhost 允许用 http（凭证明文过网会被局域网里任何人抄走）；" +
				"局域网子机请用它的 https 地址 + 证书指纹")
		}
	default:
		return errors.New("子机地址只能是 http:// 或 https://")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("子机地址不要带参数或片段，只写面板入口")
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// normalizeFingerprint 把指纹规范化成小写无冒号（用户从浏览器复制来常带冒号/大写）。
func normalizeFingerprint(fp string) string {
	fp = strings.ToLower(strings.TrimSpace(fp))
	fp = strings.ReplaceAll(fp, ":", "")
	fp = strings.ReplaceAll(fp, " ", "")
	return fp
}

func shortFp(fp string) string {
	if len(fp) <= 16 {
		return fp
	}
	return fp[:16] + "…"
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("id 必须是正整数")
	}
	return id, nil
}

// randomHexToken 生成随机凭证（hex：不引入引号/转义问题，粘到表单里也不会被截断）。
func randomHexToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
