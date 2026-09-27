package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  应用 → 「访问地址（自定义）」
//
//  用户 2026-09-27：家用场景几乎都走反向代理（或局域网地址）访问应用，面板
//  自己拼的 `http://<主机>:<端口>` 多半打不开。所以「打开」不再有默认目标：
//  用户在管理面板填一个地址，卡片上才出现「🌐 打开」。
//
//  存储：settings KV，键 app_access_urls，值 = {键: url} 的 JSON。
//  键有两套：目录条目的 **app id**（服务记录经 FindAppByService 解析），
//  以及解析不到时回退的 **svc:<服务名>**（用户自建服务 —— 家用用户对它们
//  同样会配反代，入口不能少）。
// ============================================================================

// appAccessURLsKey 是 settings 表里的键（值 = map[string]string 的 JSON）。
const appAccessURLsKey = "app_access_urls"

// appAccessServiceKeyPrefix 是"服务名键"的前缀：解析不到目录 app id 的条目
// （用户自建服务）用 `svc:<服务名>`。目录 id 只含字母/数字/点/下划线/短横线，
// 永远不带冒号，所以两套键不可能撞车（解析时也**先认目录 id**）。
const appAccessServiceKeyPrefix = "svc:"

// appAccessURLMaxLen 是访问地址长度上限（与接口校验共用）。
const appAccessURLMaxLen = 2048

// appAccessURLSchemes 是唯一允许的 scheme 白名单（小写，url.Parse 已归一化）。
var appAccessURLSchemes = map[string]bool{"http": true, "https": true}

// appAccessMu 串行化"读-改-写"：settings KV 没有事务，两个并发保存会丢一个。
var appAccessMu sync.Mutex

// ValidateAppAccessURL 校验并归一化用户填写的访问地址（纯函数，可单测）。
// 规则：去首尾空白；只接受 http/https；拒绝空白/控制字符、超长、缺主机名。
func ValidateAppAccessURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("访问地址不能为空")
	}
	if len(s) > appAccessURLMaxLen {
		return "", fmt.Errorf("访问地址太长（最多 %d 个字符）", appAccessURLMaxLen)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return "", errors.New("访问地址里不能有空格、换行等空白或控制字符")
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("访问地址格式不对：" + err.Error())
	}
	// scheme 白名单是**唯一**允许 http/https 的判据（javascript:/data:/file:/vbscript: 等一律拒）。
	if !appAccessURLSchemes[u.Scheme] {
		return "", errors.New("访问地址必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return "", errors.New("访问地址不完整：缺少主机名，例如 https://demo.example.com/app/")
	}
	return s, nil
}

// loadAppAccessURLs 读整份访问地址表。
func (s *Server) loadAppAccessURLs(r *http.Request) (map[string]string, error) {
	raw, err := s.Store.GetSetting(r.Context(), appAccessURLsKey)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("访问地址数据损坏（%s）: %w", appAccessURLsKey, err)
	}
	return out, nil
}

// saveAppAccessURLs 写整份访问地址表；空表写空串（不留一行空 JSON）。
func (s *Server) saveAppAccessURLs(r *http.Request, m map[string]string) error {
	if len(m) == 0 {
		return s.Store.SetSetting(r.Context(), appAccessURLsKey, "")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(r.Context(), appAccessURLsKey, string(b))
}

// catalogAppExists 判断这个 ID 真的在生效目录里（含远端/插件条目）。
func catalogAppExists(id string) bool {
	for _, a := range services.Catalog() {
		if a.ID == id {
			return true
		}
	}
	return false
}

// resolveAccessKey 把请求里的 id 解析成 KV 键：先认目录 app id（原样），
// 再认**真实存在**的服务名（加 svc: 前缀）。两者都不是 → ok=false（调用方 404）。
// 服务名必须查得到：不然任何垃圾字符串都能在设置里占一格。
func (s *Server) resolveAccessKey(r *http.Request, id string) (string, bool) {
	if catalogAppExists(id) {
		return id, true
	}
	if _, err := s.serviceRepo.Get(r.Context(), id); err == nil {
		return appAccessServiceKeyPrefix + id, true
	}
	return "", false
}

// accessURLFor 读一条条目的访问地址：有目录 app id 用目录键，否则用服务名键。
func accessURLFor(m map[string]string, appID, svcName string) string {
	if appID != "" {
		return m[appID]
	}
	if svcName != "" {
		return m[appAccessServiceKeyPrefix+svcName]
	}
	return ""
}

// handleMarketAccessURLSet 处理 PUT /api/v1/market/access-url：{id, url}。
// id 可以是目录 app id 或**真实存在的**服务名（后者存成 svc:<服务名>）。
// url 去空白后为空 = 清除这一条；非法 URL = 400 并说清原因；两种键都认不出 = 404。
func (s *Server) handleMarketAccessURLSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		fail(w, http.StatusBadRequest, "缺少应用 id")
		return
	}
	key, known := s.resolveAccessKey(r, id)
	if !known {
		fail(w, http.StatusNotFound, "应用目录里没有这个 id，面板里也没有这个服务："+id)
		return
	}
	raw := strings.TrimSpace(req.URL)

	appAccessMu.Lock()
	defer appAccessMu.Unlock()
	m, err := s.loadAppAccessURLs(r)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw == "" {
		delete(m, key)
		if err := s.saveAppAccessURLs(r, m); err != nil {
			fail(w, http.StatusInternalServerError, "清除访问地址失败: "+err.Error())
			return
		}
		s.audit(r, "app_access_url", id, "清除访问地址", true, "")
		ok(w, map[string]any{"id": id, "access_url": ""})
		return
	}
	clean, verr := ValidateAppAccessURL(raw)
	if verr != nil {
		s.audit(r, "app_access_url", id, "失败: "+verr.Error(), false, "")
		fail(w, http.StatusBadRequest, verr.Error())
		return
	}
	m[key] = clean
	if err := s.saveAppAccessURLs(r, m); err != nil {
		fail(w, http.StatusInternalServerError, "保存访问地址失败: "+err.Error())
		return
	}
	s.audit(r, "app_access_url", id, "设置访问地址", true, "")
	ok(w, map[string]any{"id": id, "access_url": clean})
}
