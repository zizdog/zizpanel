package web

// nav_listen_test.go —— 「导航页独立端口」的门禁（坑 222）。
//
// 覆盖四类：
//   · 端口校验（0 / 负数 / 低端口 / 越界 / 合法边界）；
//   · 端口被占用时**如实报错**且不动旧监听；
//   · 幂等（同一端口重复保存不重绑）与"改端口真的换监听"；
//   · **未登录（无面板会话）时独立端口可访问**（需求核心）。
//
// 全部用 127.0.0.1 上的临时端口；不碰真实面板、不碰 8443、不碰生产机。

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestValidateNavListenPort(t *testing.T) {
	bad := []int{0, -1, 80, 443, 1023, 65536, 70000}
	for _, p := range bad {
		if err := ValidateNavListenPort(p); err == nil {
			t.Errorf("端口 %d 应被拒绝，实际通过了", p)
		}
	}
	for _, p := range []int{1024, 8896, 65535} {
		if err := ValidateNavListenPort(p); err != nil {
			t.Errorf("端口 %d 应被接受，实际被拒: %v", p, err)
		}
	}
	// 0 的文案必须指向开关，而不是让用户继续猜
	if err := ValidateNavListenPort(0); err == nil || !strings.Contains(err.Error(), "开关") {
		t.Errorf("端口 0 的错误应提示用开关关闭，实际: %v", err)
	}
}

// TestNavListenerRejectsOccupiedPort：端口被别的进程占用时必须报错、给可行动提示，
// 且**不许谎报运行**。
func TestNavListenerRejectsOccupiedPort(t *testing.T) {
	srv, _ := newTestServer(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port

	err = srv.ApplyNavListener(true, port)
	if err == nil {
		t.Fatal("端口已被占用，ApplyNavListener 却成功了")
	}
	if !strings.Contains(err.Error(), "已被占用") || !strings.Contains(err.Error(), "lsof") {
		t.Errorf("占用报错应说明端口被占用并给出 lsof 出路，实际: %v", err)
	}
	if st := srv.NavListenerState(); st.Running {
		t.Errorf("绑定失败后不应报 running: %+v", st)
	}
	if st := srv.NavListenerState(); st.Err == "" {
		t.Error("绑定失败后应保留后端原文供界面显示")
	}
}

// TestNavListenerIdempotentOnSamePort：重复保存同一端口 = 不重绑、不报错。
func TestNavListenerIdempotentOnSamePort(t *testing.T) {
	srv, _ := newTestServer(t)
	port := freePortForTest(t)
	t.Cleanup(func() { srv.ApplyNavListener(false, port) })

	if err := srv.ApplyNavListener(true, port); err != nil {
		t.Fatalf("首次启用失败: %v", err)
	}
	if err := srv.ApplyNavListener(true, port); err != nil {
		t.Fatalf("重复启用同一端口应幂等，实际报错: %v", err)
	}
	if n := srv.navListen.starts; n != 1 {
		t.Errorf("同一端口重复 apply 不应重绑，实际 bind 次数 %d", n)
	}
	st := srv.NavListenerState()
	if !st.Running || st.Port != port {
		t.Errorf("生效状态不对: %+v", st)
	}
}

// TestNavListenerPortChangeRebinds：改端口必须真的换监听（旧端口不再有人听）。
func TestNavListenerPortChangeRebinds(t *testing.T) {
	srv, _ := newTestServer(t)
	a, b := freePortForTest(t), freePortForTest(t)
	t.Cleanup(func() { srv.ApplyNavListener(false, b) })

	if err := srv.ApplyNavListener(true, a); err != nil {
		t.Fatalf("启用端口 %d 失败: %v", a, err)
	}
	if err := srv.ApplyNavListener(true, b); err != nil {
		t.Fatalf("改到端口 %d 失败: %v", b, err)
	}
	if st := srv.NavListenerState(); st.Port != b {
		t.Fatalf("生效端口应是 %d，实际 %+v", b, st)
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", a)); err == nil {
		_ = c.Close()
		t.Errorf("旧端口 %d 仍在监听", a)
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", b)); err != nil {
		t.Errorf("新端口 %d 没有在监听: %v", b, err)
	} else {
		_ = c.Close()
	}
}

// TestNavListenerChangeFailureKeepsOldPort：改到一个被占用的端口时必须失败，
// 且**旧监听继续服务**（先绑新的、成功才关旧的）。
func TestNavListenerChangeFailureKeepsOldPort(t *testing.T) {
	srv, _ := newTestServer(t)
	good := freePortForTest(t)
	t.Cleanup(func() { srv.ApplyNavListener(false, good) })
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	busy := held.Addr().(*net.TCPAddr).Port

	if err := srv.ApplyNavListener(true, good); err != nil {
		t.Fatalf("启用端口 %d 失败: %v", good, err)
	}
	if err := srv.ApplyNavListener(true, busy); err == nil {
		t.Fatal("改到被占用端口应当失败")
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", good)); err != nil {
		t.Errorf("改端口失败后旧端口 %d 应继续监听: %v", good, err)
	} else {
		_ = c.Close()
	}
}

// TestNavStandaloneServesWithoutPanelSession 是需求核心：
// 独立端口在没有面板会话（不带任何 cookie）时必须能打开导航页并拿到数据。
func TestNavStandaloneServesWithoutPanelSession(t *testing.T) {
	srv, ts, cookies := setupNavPanel(t)
	g := navCreateGroup(t, ts, cookies, "公网分组")
	gid := int64(g["id"].(float64))
	navCreateItem(t, ts, cookies, map[string]any{
		"group_id": gid, "name": "隧道站点", "url": "https://op.example.com", "icon": "🌍",
	})

	port := freePortForTest(t)
	if err := srv.ApplyNavListener(true, port); err != nil {
		t.Fatalf("启用独立端口失败: %v", err)
	}
	t.Cleanup(func() { srv.ApplyNavListener(false, port) })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	get := func(path string) (int, string, string) {
		t.Helper()
		// 刻意用全新 client：不携带任何面板 cookie/session。
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get("Content-Type")
	}

	// ① 域名根（隧道把域名根映射到 /）必须是导航页
	code, body, ctype := get("/")
	if code != http.StatusOK || !strings.HasPrefix(ctype, "text/html") {
		t.Fatalf("GET / 应为 200 HTML，实际 %d %s", code, ctype)
	}
	if !strings.Contains(body, "./nav.js") {
		t.Error("根路径返回的不是导航页（缺 nav.js 引用）")
	}
	// ② 页面用到的静态资源与数据接口，全部未登录可拿
	for _, p := range []string{"/nav/", "/nav.js", "/nav/nav.js", "/data", "/nav/data"} {
		code, body, _ := get(p)
		if code != http.StatusOK || len(body) == 0 {
			t.Errorf("未登录 GET %s 应 200 且非空，实际 %d（%d 字节）", p, code, len(body))
		}
	}
	if code, body, _ := get("/nav.js"); code == http.StatusOK && !strings.Contains(body, "fetch") {
		t.Error("/nav.js 不是导航页脚本")
	}
	// ③ 数据里必须有真实分组与条目（否则页面会空白）
	_, body, _ = get("/data")
	var doc struct {
		OK   bool `json:"ok"`
		Data struct {
			Groups []map[string]any `json:"groups"`
			Items  []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("解析 /data 失败: %v（body=%s）", err, firstN(body, 120))
	}
	if !doc.OK || len(doc.Data.Groups) != 1 || len(doc.Data.Items) != 1 {
		t.Fatalf("未登录 /data 内容不对: %s", firstN(body, 200))
	}
	// ④ whoami 在独立端口上如实回答"未登录、无面板入口"（避免指向不存在的后缀）
	_, body, _ = get("/whoami")
	if !strings.Contains(body, `"authenticated":false`) || !strings.Contains(body, `"panel_entry":""`) {
		t.Errorf("独立端口 whoami 应为未登录且不回显入口，实际: %s", firstN(body, 200))
	}
	// ⑤ 写接口一个都不挂在独立端口上
	if code, _, _ := get("/api/v1/nav/groups"); code != http.StatusNotFound {
		t.Errorf("独立端口不应暴露面板写接口，实际 %d", code)
	}
	// ⑥ 只读：POST 一律 405
	resp, err := http.Post(base+"/", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("独立端口 POST 应 405，实际 %d", resp.StatusCode)
	}
	// ⑦ 未登录不能改数据：写 API 不在这个端口上，数据自然不变
	code, body, _ = get("/data")
	if code != http.StatusOK || !strings.Contains(body, "隧道站点") {
		t.Errorf("独立端口读到的数据应含已建站点，实际 %d %s", code, firstN(body, 200))
	}
}

// TestNavListenSettingsEndpointAppliesAndReports：面板设置保存 → 立即生效 → 回读生效值。
func TestNavListenSettingsEndpointAppliesAndReports(t *testing.T) {
	srv, ts, cookies := setupNavPanel(t)
	port := freePortForTest(t)
	t.Cleanup(func() { srv.ApplyNavListener(false, port) })

	post := func(body map[string]any) (*http.Response, map[string]any) {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/settings", body, cookies)
		return res, out
	}

	res, out := post(map[string]any{"nav_listen_enabled": true, "nav_listen_port": port})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("保存导航页端口失败 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil || data["nav_listen_running"] != true {
		t.Fatalf("保存后应回读 running=true: %v", out)
	}
	// 前端消费的生效字段必须都在（少一个界面就显示不出真实状态）。
	for _, key := range []string{"nav_listen_enabled", "nav_listen_port", "nav_listen_running",
		"nav_listen_active_port", "nav_listen_url", "nav_listen_error"} {
		if _, exists := data[key]; !exists {
			t.Errorf("settingsView 没有下发 %s", key)
		}
	}
	wantURL := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if data["nav_listen_url"] != wantURL {
		t.Errorf("回读地址应为 %s，实际 %v", wantURL, data["nav_listen_url"])
	}
	if got := int(data["nav_listen_active_port"].(float64)); got != port {
		t.Errorf("回读生效端口应为 %d，实际 %d", port, got)
	}

	// 幂等：再保存一次同一端口，不应重绑
	_, _ = post(map[string]any{"nav_listen_enabled": true, "nav_listen_port": port})
	if n := srv.navListen.starts; n != 1 {
		t.Errorf("重复保存同一端口不应重绑，实际 bind 次数 %d", n)
	}

	// 非法端口：0 与越界都被拒
	for _, bad := range []int{0, 70000} {
		res, out = post(map[string]any{"nav_listen_enabled": true, "nav_listen_port": bad})
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("端口 %d 应 400，实际 %d %v", bad, res.StatusCode, out)
		}
	}

	// 被占用：报错并**不落库**（GET 回读仍是上一个合法值）
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	busy := held.Addr().(*net.TCPAddr).Port
	res, out = post(map[string]any{"nav_listen_enabled": true, "nav_listen_port": busy})
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["msg"]), "已被占用") {
		t.Fatalf("占用端口应 400 且贴后端原文，实际 %d %v", res.StatusCode, out)
	}
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/settings", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("回读设置失败 %d", res.StatusCode)
	}
	got, _ := out["data"].(map[string]any)
	if int(got["nav_listen_port"].(float64)) != port {
		t.Errorf("绑定失败不应改动已保存的端口，实际 %v", got["nav_listen_port"])
	}

	// 关闭：立即停止监听
	res, out = post(map[string]any{"nav_listen_enabled": false, "nav_listen_port": port})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("关闭失败 %d: %v", res.StatusCode, out)
	}
	got, _ = out["data"].(map[string]any)
	if got["nav_listen_running"] != false {
		t.Errorf("关闭后应 running=false，实际 %v", got["nav_listen_running"])
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		_ = c.Close()
		t.Error("关闭后端口仍在监听")
	}
}

// TestNavListenerDisabledAtStartDoesNotBind：关闭状态不绑端口（配置默认开关必须真的生效）。
func TestNavListenerDisabledAtStartDoesNotBind(t *testing.T) {
	srv, _ := newTestServer(t)
	if err := srv.ApplyNavListener(false, 8896); err != nil {
		t.Fatalf("关闭状态不应报错: %v", err)
	}
	if st := srv.NavListenerState(); st.Running {
		t.Errorf("关闭状态不应在监听: %+v", st)
	}
}
