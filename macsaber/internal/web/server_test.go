package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zizdog/macsaber/internal/config"
	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tasks"
	"github.com/zizdog/macsaber/internal/tool"
	"github.com/zizdog/macsaber/internal/tools"
)

// countingTool 记录被真正执行的次数：危险工具缺确认时必须是 0。
type countingTool struct{ runs *int64 }

func (countingTool) Meta() tool.Meta {
	return tool.Meta{ID: "demo.danger", Name: "危险示例", Category: "example", Summary: "需要确认",
		Danger: true, DangerFloor: "我已知晓", Available: true,
		Params: []tool.Param{{Name: "text", Label: "文本", Type: tool.TypeText}}}
}

func (c countingTool) Run(ctx context.Context, x *tool.Ctx, in tool.Input) (*tool.Result, error) {
	atomic.AddInt64(c.runs, 1)
	return &tool.Result{OK: true, Msg: "执行了"}, nil
}

type pathTool struct{}

func (pathTool) Meta() tool.Meta {
	return tool.Meta{ID: "demo.path", Name: "路径示例", Category: "example", Available: true,
		Params: []tool.Param{{Name: "file", Label: "文件", Type: tool.TypePath, Help: "读根内文件。"}}}
}

func (pathTool) Run(ctx context.Context, x *tool.Ctx, in tool.Input) (*tool.Result, error) {
	return &tool.Result{OK: true, Msg: "读了 " + in.Path("file")}, nil
}

type slowTool struct{}

func (slowTool) Meta() tool.Meta {
	return tool.Meta{ID: "demo.slow", Name: "异步示例", Category: "example", Async: true, Available: true}
}

func (slowTool) Run(ctx context.Context, x *tool.Ctx, in tool.Input) (*tool.Result, error) {
	x.Log("后台跑起来")
	time.Sleep(60 * time.Millisecond)
	return &tool.Result{OK: true, Msg: "后台完成"}, nil
}

type fixture struct {
	ts    *httptest.Server
	store *config.Store
	home  string
	write string
	runs  int64
	tasks *tasks.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	writeRoot := filepath.Join(home, "MacSaberFiles")
	if err := os.MkdirAll(writeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	guard, err := fsroot.New([]string{home}, []string{writeRoot})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := tool.OpenAudit(filepath.Join(store.DataDir(), "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	tm := tasks.NewManager()
	reg := tool.NewRegistry()
	f := &fixture{store: store, home: home, write: writeRoot, tasks: tm}
	reg.Register(countingTool{runs: &f.runs})
	reg.Register(pathTool{})
	reg.Register(slowTool{})
	tools.RegisterAll(reg)
	runner := &tool.Runner{Reg: reg, Tasks: tm, Audit: audit,
		Ctx: &tool.Ctx{Ctx: context.Background(), Guard: guard, Exec: execx.New(),
			Probes: execx.NewProbeCache(), TempDir: t.TempDir(),
			Log: func(string) {}, Progress: func(int, string) {}}}
	srv := New(store, guard, reg, runner, tm, "test-version")
	f.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(f.ts.Close)
	return f
}

// initAndLogin 走真实的初始化 + 登录，返回带 cookie 的 client 与 csrf 值。
func (f *fixture) initAndLogin(t *testing.T) (*http.Client, string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	jar := &cookieJar{}
	client.Jar = jar
	// 未初始化时 setup/status 应报 inited=false。
	st := f.getJSON(t, client, "/api/setup/status")
	if st["inited"] != false {
		t.Fatalf("初始状态应为未初始化: %v", st)
	}
	res, body := f.postJSON(t, client, "/api/setup", map[string]any{
		"user": "tester", "password": "passw0rd!", "confirm": "passw0rd!"}, true)
	if res.StatusCode != 200 {
		t.Fatalf("初始化失败 %d: %v", res.StatusCode, body)
	}
	return client, jar.csrf
}

type cookieJar struct {
	session string
	csrf    string
}

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	for _, c := range cs {
		switch c.Name {
		case config.CookieSession:
			j.session = c.Value
		case config.CookieCSRF:
			j.csrf = c.Value
		}
	}
}

func (j *cookieJar) Cookies(*url.URL) []*http.Cookie {
	out := []*http.Cookie{}
	if j.session != "" {
		out = append(out, &http.Cookie{Name: config.CookieSession, Value: j.session})
	}
	if j.csrf != "" {
		out = append(out, &http.Cookie{Name: config.CookieCSRF, Value: j.csrf})
	}
	return out
}

func (f *fixture) getJSON(t *testing.T, c *http.Client, path string) map[string]any {
	t.Helper()
	res, err := c.Get(f.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("解析 %s 响应失败: %v", path, err)
	}
	return out
}

func (f *fixture) postJSON(t *testing.T, c *http.Client, path string, body map[string]any, csrf bool) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		if jar, ok := c.Jar.(*cookieJar); ok {
			req.Header.Set("X-CSRF-Token", jar.csrf)
		}
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// TestUnauthenticatedIs401：未登录访问受保护接口必须 401。
func TestUnauthenticatedIs401(t *testing.T) {
	f := newFixture(t)
	client := &http.Client{Timeout: 5 * time.Second}
	for _, p := range []string{"/api/tools", "/api/tasks", "/api/session", "/api/files/download?path=eA"} {
		res, err := client.Get(f.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 未登录应 401，实际 %d", p, res.StatusCode)
		}
	}
}

// TestCSRFRequiredForWrites：写操作缺 CSRF 头必须 403。
func TestCSRFRequiredForWrites(t *testing.T) {
	f := newFixture(t)
	client, _ := f.initAndLogin(t)
	res, body := f.postJSON(t, client, "/api/tools/demo.path/run",
		map[string]any{"file": filepath.Join(f.home, "a.txt")}, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 头应 403，实际 %d (%v)", res.StatusCode, body)
	}
	// 带上正确的头应通过。
	if err := os.WriteFile(filepath.Join(f.home, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, body2 := f.postJSON(t, client, "/api/tools/demo.path/run",
		map[string]any{"file": filepath.Join(f.home, "a.txt")}, true)
	if res2.StatusCode != 200 {
		t.Fatalf("带 CSRF 应 200，实际 %d (%v)", res2.StatusCode, body2)
	}
}

// TestDangerWithoutConfirmIs400AndDoesNotRun：危险工具缺确认 → 400 且绝不执行。
func TestDangerWithoutConfirmIs400AndDoesNotRun(t *testing.T) {
	f := newFixture(t)
	client, _ := f.initAndLogin(t)
	res, body := f.postJSON(t, client, "/api/tools/demo.danger/run", map[string]any{"text": "x"}, true)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺确认应 400，实际 %d (%v)", res.StatusCode, body)
	}
	if atomic.LoadInt64(&f.runs) != 0 {
		t.Fatal("缺确认时后端**绝不能执行**")
	}
	// 确认值不对也不行。
	if res, _ := f.postJSON(t, client, "/api/tools/demo.danger/run",
		map[string]any{"text": "x", "_confirm": "随便写"}, true); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("确认值不符应 400，实际 %d", res.StatusCode)
	}
	if atomic.LoadInt64(&f.runs) != 0 {
		t.Fatal("确认值不符时不能执行")
	}
	// 正确确认才执行。
	if res, body := f.postJSON(t, client, "/api/tools/demo.danger/run",
		map[string]any{"text": "x", "_confirm": "我已知晓"}, true); res.StatusCode != 200 {
		t.Fatalf("正确确认应 200，实际 %d (%v)", res.StatusCode, body)
	}
	if atomic.LoadInt64(&f.runs) != 1 {
		t.Fatalf("确认后应执行一次，实际 %d", f.runs)
	}
}

// TestPathEscapeIs403：路径越界必须 403（不是 400/500 含糊过去）。
func TestPathEscapeIs403(t *testing.T) {
	f := newFixture(t)
	client, _ := f.initAndLogin(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, body := f.postJSON(t, client, "/api/tools/demo.path/run",
		map[string]any{"file": outside}, true); res.StatusCode != http.StatusForbidden {
		t.Fatalf("读根外路径应 403，实际 %d (%v)", res.StatusCode, body)
	}
	// 敏感目录同样 403。
	ks := filepath.Join(f.home, "Library", "Keychains", "login.keychain-db")
	if err := os.MkdirAll(filepath.Dir(ks), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ks, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, body := f.postJSON(t, client, "/api/tools/demo.path/run",
		map[string]any{"file": ks}, true); res.StatusCode != http.StatusForbidden {
		t.Fatalf("敏感目录应 403，实际 %d (%v)", res.StatusCode, body)
	}
}

// TestToolsMetadataEndpoint：/api/tools 必须给出分类 + 参数 + available/reason。
func TestToolsMetadataEndpoint(t *testing.T) {
	f := newFixture(t)
	client, _ := f.initAndLogin(t)
	body := f.getJSON(t, client, "/api/tools")
	cats, ok := body["categories"].([]any)
	if !ok || len(cats) == 0 {
		t.Fatalf("categories 缺失: %v", body)
	}
	seen := map[string]map[string]any{}
	for _, c := range cats {
		cm := c.(map[string]any)
		for _, tt := range cm["tools"].([]any) {
			tm := tt.(map[string]any)
			seen[tm["id"].(string)] = tm
			if _, ok := tm["available"]; !ok {
				t.Errorf("%s 缺 available", tm["id"])
			}
			if tm["available"] == false && tm["unavailable_reason"] == "" {
				t.Errorf("%s 不可用却没给原因", tm["id"])
			}
			if _, ok := tm["params"]; !ok {
				t.Errorf("%s 缺 params", tm["id"])
			}
		}
	}
	for _, want := range []string{"img.convert", "text.hash", "sys.overview", "demo.danger"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("/api/tools 未下发 %s", want)
		}
	}
	if seen["demo.danger"]["danger"] != true || seen["demo.danger"]["danger_floor"] != "我已知晓" {
		t.Errorf("危险工具元数据不符: %v", seen["demo.danger"])
	}
	if seen["demo.slow"]["async"] != true {
		t.Errorf("异步工具元数据不符: %v", seen["demo.slow"])
	}
}

// TestAsyncRunThenPollTask：异步工具 202 + task_id，轮询能查到结果。
func TestAsyncRunThenPollTask(t *testing.T) {
	f := newFixture(t)
	client, _ := f.initAndLogin(t)
	res, body := f.postJSON(t, client, "/api/tools/demo.slow/run", map[string]any{}, true)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("异步应 202，实际 %d (%v)", res.StatusCode, body)
	}
	id, _ := body["task_id"].(string)
	if id == "" {
		t.Fatalf("202 应带 task_id: %v", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.getJSON(t, client, "/api/tasks/"+id)
		task := got["task"].(map[string]any)
		if task["status"] != "running" {
			if task["status"] != "succeeded" {
				t.Fatalf("任务应成功，实际 %v", task)
			}
			if task["result"] == nil {
				t.Fatal("成功后应能查到结果")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("轮询超时")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestDownloadRouteHonoursRoots：下载接口重新过闸门，不能当任意文件读取用。
func TestDownloadRouteHonoursRoots(t *testing.T) {
	f := newFixture(t)
	client, _ := f.initAndLogin(t)
	inside := filepath.Join(f.home, "ok.txt")
	if err := os.WriteFile(inside, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString([]byte(inside))
	res, err := client.Get(f.ts.URL + "/api/files/download?path=" + enc)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := res.Body.Read(buf)
	_ = res.Body.Close()
	if res.StatusCode != 200 || string(buf[:n]) != "hello" {
		t.Fatalf("读根内文件应可下载，实际 %d %q", res.StatusCode, string(buf[:n]))
	}

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	enc2 := base64.RawURLEncoding.EncodeToString([]byte(outside))
	res2, err := client.Get(f.ts.URL + "/api/files/download?path=" + enc2)
	if err != nil {
		t.Fatal(err)
	}
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("读根外下载应 403，实际 %d", res2.StatusCode)
	}
}

// TestLoginRejectsWrongPassword：口令错必须 401，且不给会话。
func TestLoginRejectsWrongPassword(t *testing.T) {
	f := newFixture(t)
	f.initAndLogin(t)
	fresh := &http.Client{Timeout: 5 * time.Second, Jar: &cookieJar{}}
	res, body := f.postJSON(t, fresh, "/api/login", map[string]any{"user": "tester", "password": "wrong"}, true)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401，实际 %d (%v)", res.StatusCode, body)
	}
	// 正确口令能登录。
	res2, body2 := f.postJSON(t, fresh, "/api/login", map[string]any{"user": "tester", "password": "passw0rd!"}, true)
	if res2.StatusCode != 200 {
		t.Fatalf("正确口令应 200，实际 %d (%v)", res2.StatusCode, body2)
	}
}

// TestStaticAssetsServed：前端三件套必须能取到，且 index.html 不缓存。
func TestStaticAssetsServed(t *testing.T) {
	f := newFixture(t)
	client := &http.Client{Timeout: 5 * time.Second}
	for _, p := range []string{"/", "/app.js", "/app.css", "/js/api.js", "/js/form.js", "/js/result.js", "/js/ui.js", "/js/cards.js"} {
		res, err := client.Get(f.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("%s 应 200，实际 %d", p, res.StatusCode)
		}
	}
	res, _ := client.Get(f.ts.URL + "/")
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("index.html 应 no-store，实际 %q", cc)
	}
	// 未知路径回落 index.html（前端 hash 路由）。
	res2, _ := client.Get(f.ts.URL + "/whatever/deep")
	if res2.StatusCode != 200 {
		t.Errorf("未知路径应回落 index.html，实际 %d", res2.StatusCode)
	}
}

// TestSecurityHeaders：基本安全头必须在。
func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(f.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	for _, h := range []string{"X-Content-Type-Options", "Content-Security-Policy", "X-Frame-Options"} {
		if res.Header.Get(h) == "" {
			t.Errorf("缺安全头 %s", h)
		}
	}
}
