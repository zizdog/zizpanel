package web

// api_mirror_announce_test.go —— 「再广播」面板侧的门禁。
//
// 假镜像站（httptest）提供不带外网地址的 announce.json，并真的按 If-None-Match
// 回 304。断言三件事：304 不重解析/不重算；公告变化时计数正确；拉取失败不提示。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

type fakeAnnounceMirror struct {
	mu         sync.Mutex
	body       []byte
	etag       string
	bodyServes int // 真正返回 200 正文的次数（304 不算）
}

func newFakeAnnounceMirror(t *testing.T) (*fakeAnnounceMirror, *httptest.Server) {
	t.Helper()
	m := &fakeAnnounceMirror{}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+mirrorAppsSubdir+"/"+mirrorAnnounceName, func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.etag == "" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == m.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		m.bodyServes++
		w.Header().Set("ETag", m.etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(m.body)))
		_, _ = w.Write(m.body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return m, srv
}

// setApps 发布一份新公告（revision 由内容决定，etag 随之变化）。
func (m *fakeAnnounceMirror) setApps(apps map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc := mirrorAnnounceDoc{
		Revision: mirrorAnnounceRevisionOf(apps), PublishedAt: "2026-02-02T00:00:00Z", Apps: apps,
	}
	b, _ := json.Marshal(doc)
	m.body = append(b, '\n')
	m.etag = `"` + doc.Revision + `"`
}

func (m *fakeAnnounceMirror) serves() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodyServes
}

func TestMarketAnnounceETagChangeAndFailure(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	var probes atomic.Int64
	hasUpdate := true
	srv.marketUpdateCheckOverride = func(_ context.Context, id string) services.ZizvideoUpdateCheck {
		probes.Add(1)
		return services.ZizvideoUpdateCheck{
			App: id, Installed: "0.1.1-mvp", Latest: "0.2.0-mvp",
			UpdateAvailable: hasUpdate, CheckedAt: time.Now().Format(time.RFC3339), Source: "mirror-index",
		}
	}

	m, mirror := newFakeAnnounceMirror(t)
	m.setApps(map[string]string{"zizvideo": "0.2.0-mvp"})
	srv.Cfg.MirrorBase = mirror.URL

	get := func() map[string]any {
		t.Helper()
		res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/announce", nil, cookies)
		if res.StatusCode != 200 {
			t.Fatalf("announce 应 200，实际 %d: %v", res.StatusCode, out)
		}
		data, _ := out["data"].(map[string]any)
		if data == nil {
			t.Fatalf("announce 响应缺少 data: %v", out)
		}
		return data
	}

	// 第一次：没有缓存状态 ⇒ 200 ⇒ 变化 + 真的算了一次可更新数。
	d1 := get()
	if d1["ready"] != true || d1["changed"] != true || d1["prompt"] != true {
		t.Fatalf("第一次拉公告应 ready/changed/prompt 全 true：%v", d1)
	}
	if asInt(d1["available"]) != 1 {
		t.Errorf("公告变化后应算出 1 个可更新应用，实际 %v", d1["available"])
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("公告变化时必须走既有更新检查一次，实际 %d 次", n)
	}
	if m.serves() != 1 {
		t.Fatalf("第一次应真的取回公告正文，实际 %d 次", m.serves())
	}

	// 第二次：If-None-Match 命中 ⇒ 304 ⇒ 不重解析、不重算、不新提示。
	d2 := get()
	if d2["changed"] != false || d2["prompt"] != false {
		t.Errorf("304 命中时不许产生新提示：%v", d2)
	}
	if n := probes.Load(); n != 1 {
		t.Errorf("304 命中时不许重算可更新数，实际探测 %d 次", n)
	}
	if m.serves() != 1 {
		t.Errorf("304 命中时不许再取正文（不重复解析），实际 %d 次", m.serves())
	}
	// 上一次的结论仍然可见（不因 304 就谎报“没有更新”）。
	if asInt(d2["available"]) != 1 {
		t.Errorf("304 命中时应保留上次结论，实际 available=%v", d2["available"])
	}

	// 公告真的变了（新版本）⇒ 重新算一次，计数正确。
	m.setApps(map[string]string{"zizvideo": "0.3.0-mvp"})
	d3 := get()
	if d3["changed"] != true || d3["prompt"] != true || asInt(d3["available"]) != 1 {
		t.Errorf("公告变化后应重新算出 1 个可更新应用：%v", d3)
	}
	if n := probes.Load(); n != 2 {
		t.Errorf("公告变化后应重算一次（累计 2 次），实际 %d 次", n)
	}
	if m.serves() != 2 {
		t.Errorf("公告变化后应重新取回正文，实际 %d 次", m.serves())
	}

	// 公告又变、但这次没有真的可更新应用 ⇒ changed=true、prompt=false（不夸大）。
	hasUpdate = false
	m.setApps(map[string]string{"zizvideo": "0.4.0-mvp"})
	d4 := get()
	if d4["changed"] != true || d4["prompt"] != false || asInt(d4["available"]) != 0 {
		t.Errorf("没有真实可更新应用时不许提示：%v", d4)
	}
	hasUpdate = true
	if n := probes.Load(); n != 3 {
		t.Errorf("每次公告变化都应重算，实际 %d 次", n)
	}

	// 拉取失败 ⇒ 什么都不提示（ready=false、prompt=false），也不谎报“没有更新”。
	mirror.Close()
	d5 := get()
	if d5["ready"] != false || d5["prompt"] != false || asInt(d5["available"]) != 0 {
		t.Errorf("拉取失败时必须 ready=false 且不提示：%v", d5)
	}
	if asString(d5["reason"]) == "" {
		t.Error("拉取失败必须给出原因（界面上要能说为什么）")
	}
	if n := probes.Load(); n != 3 {
		t.Errorf("拉取失败时不该去重算可更新数，实际 %d 次", n)
	}
}

// asInt 把 JSON 数字（float64）读成 int。
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return -1
	}
}
