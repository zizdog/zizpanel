package web

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/auth"
	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/store"
	"github.com/zizdog/zizpanel/internal/sysinfo"
	"github.com/zizdog/zizpanel/internal/tasks"
	"github.com/zizdog/zizpanel/internal/version"
)

// ============================================================================
//  Docker 接口的端到端测试
//
//  用**真实的 unix socket** + 一个假 Docker 守护进程，而不是 mock 掉服务层。
//  理由：这条链路上最容易错的是"HTTP 路径怎么拼"（镜像名/容器名里的 `/`
//  会不会把路由拆断、编码后的 %2F 能不能还原），mock 掉之后这些全都测不到。
// ============================================================================

type fakeDaemon struct {
	mu       sync.Mutex
	requests []string
}

func (f *fakeDaemon) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
}

func (f *fakeDaemon) sawPath(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func (f *fakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		path := r.URL.Path

		switch {
		case path == "/version":
			writeJSON(w, 200, map[string]any{"Version": "29.5.2", "ApiVersion": "1.51"})

		case path == "/containers/json":
			writeJSON(w, 200, []map[string]any{{
				"Id": "abc123def4567890", "Names": []string{"/uptime-kuma"},
				"Image": "louislam/uptime-kuma:1", "State": "running",
				"Status": "Up 2 hours", "Created": 1757000000,
				"Ports": []map[string]any{{"IP": "0.0.0.0", "PrivatePort": 3001, "PublicPort": 3001, "Type": "tcp"}},
			}})

		case path == "/images/json":
			writeJSON(w, 200, []map[string]any{
				{"Id": "sha256:aaa111", "RepoTags": []string{"nginx:1.27"}, "Size": 187654321, "Created": 1757000000},
				// 悬空镜像：RepoTags 为 null，前端必须能处理
				{"Id": "sha256:bbb222", "RepoTags": nil, "Size": 1024, "Created": 1757000000},
			})

		case path == "/images/create":
			writeJSON(w, 200, map[string]any{"status": "Downloaded newer image for " + r.URL.Query().Get("fromImage")})

		case strings.HasPrefix(path, "/images/"):
			// 关键：带 `/` 的镜像名必须以**还原后**的形式到达这里。
			// 编码没处理好时会是 %2F，那样 Docker 会找不到镜像。
			writeJSON(w, 200, []map[string]any{{"Deleted": strings.TrimPrefix(path, "/images/")}})

		case path == "/images/prune":
			writeJSON(w, 200, map[string]any{"SpaceReclaimed": 4096})

		case path == "/volumes":
			writeJSON(w, 200, map[string]any{"Volumes": []map[string]any{
				{"Name": "my_proj_db", "Driver": "local", "Mountpoint": "/var/lib/docker/volumes/my_proj_db/_data", "Scope": "local"},
			}})

		case path == "/volumes/prune":
			writeJSON(w, 200, map[string]any{"SpaceReclaimed": 8192})

		case strings.HasPrefix(path, "/volumes/"):
			writeJSON(w, 204, nil)

		case path == "/networks":
			writeJSON(w, 200, []map[string]any{
				{"Id": "net-bridge", "Name": "bridge", "Driver": "bridge", "Scope": "local"},
				{"Id": "net-mine", "Name": "my-net", "Driver": "bridge", "Scope": "local",
					"IPAM": map[string]any{"Config": []map[string]any{{"Subnet": "172.20.0.0/16"}}}},
			})

		case strings.HasPrefix(path, "/networks/"):
			writeJSON(w, 204, nil)

		case path == "/containers/create":
			writeJSON(w, 201, map[string]any{"Id": "newcont1234567890", "Warnings": []string{}})

		case strings.HasSuffix(path, "/start") || strings.HasSuffix(path, "/stop") ||
			strings.HasSuffix(path, "/restart"):
			writeJSON(w, 204, nil)

		case strings.HasSuffix(path, "/logs"):
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("2026-09-14T00:00:00Z hello from container\n"))

		case path == "/containers/json" || strings.HasPrefix(path, "/containers/"):
			writeJSON(w, 200, map[string]any{"Id": "abc123def4567890", "State": map[string]any{"Status": "running"}})

		default:
			writeJSON(w, 404, map[string]any{"message": "no such endpoint: " + path})
		}
	})
	return mux
}

// startFakeDocker 起一个绑在临时 unix socket 上的假 Docker 守护进程。
//
// socket 路径刻意用很短的 /tmp/xxx：macOS 的 unix socket 路径上限约 104 字节，
// 而 t.TempDir() 给出的路径（含完整测试名）轻易就超了，报错是
// "bind: invalid argument" —— 看起来像参数写错，其实是路径太长。
func startFakeDocker(t *testing.T) (string, *fakeDaemon) {
	t.Helper()
	dir, derr := os.MkdirTemp("/tmp", "zpd")
	if derr != nil {
		t.Fatalf("创建临时目录失败: %v", derr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("监听 unix socket 失败: %v", err)
	}
	f := &fakeDaemon{}
	ts := httptest.NewUnstartedServer(f.handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)

	return sock, f
}

// newDockerTestServer 起一个面板实例，并把它的 Docker socket 指向假守护进程。
func newDockerTestServer(t *testing.T, sock string) (*httptest.Server, []*http.Cookie) {
	_, ts, cookies := newDockerTestServerWithSrv(t, sock)
	return ts, cookies
}

// newDockerTestServerWithSrv 与 newDockerTestServer 相同，但把 *Server 也返回，
// 给需要直接访问任务中心（srv.Tasks）的测试用。
func newDockerTestServerWithSrv(t *testing.T, sock string) (*Server, *httptest.Server, []*http.Cookie) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.LogDir = dir + "/logs"
	cfg.RunDir = dir + "/run"
	cfg.WorkDir = dir + "/work"
	cfg.BinDir = dir + "/bin"
	cfg.TLSCert = dir + "/tls/panel.crt"
	cfg.TLSKey = dir + "/tls/panel.key"
	cfg.TLSEnable = false
	cfg.Secret = strings.Repeat("a", 64)
	cfg.AccessMode = "any"
	cfg.DockerSocket = sock
	cfg.User = "zizdog"
	// 与 newTestServer 同样的隔离：Cfg.UserHome/WWWRoot/LogRoot 绝不能指向真实家目录
	// （见 README 坑 57：测试把用户真实 launchd plist 覆盖成空 plist 的事故）
	cfg.UserHome = dir + "/home"
	cfg.WWWRoot = cfg.UserHome + "/www"
	cfg.LogRoot = cfg.WWWRoot + "/_logs"
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cfg.UserHome, cfg.WWWRoot, cfg.LogRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv, err := New(cfg, st, auth.New(st, cfg.Secret, 72, 5, 15), sysinfo.NewCollector(dir))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	res, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("初始化失败 %d", res.StatusCode)
	}
	return srv, ts, cookies
}

// TestDockerInfoReportsEngine 环境探测要如实报告引擎版本。
func TestDockerInfoReportsEngine(t *testing.T) {
	sock, _ := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/docker/info", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("info 应 200，实际 %d: %v", res.StatusCode, out)
	}
	data := out["data"].(map[string]any)
	if data["available"] != true {
		t.Fatalf("应报告 available=true，实际 %v", data)
	}
	if data["version"] != "29.5.2" {
		t.Errorf("引擎版本应为 29.5.2，实际 %v", data["version"])
	}
	counts, _ := data["counts"].(map[string]any)
	if counts == nil {
		t.Fatal("应带上 counts 概览")
	}
	if counts["containers"] != float64(1) {
		t.Errorf("容器数应为 1，实际 %v", counts["containers"])
	}
	if counts["containers_running"] != float64(1) {
		t.Errorf("运行中容器数应为 1，实际 %v", counts["containers_running"])
	}
}

// TestDockerUnavailableReturnsConflict 没装 Docker 时必须给 409 + 可读原因，
// 而不是 500。macOS 上这是常态，500 会让用户以为面板坏了。
func TestDockerUnavailableReturnsConflict(t *testing.T) {
	ts, cookies := newDockerTestServer(t, "") // 空 socket = 不可用

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/docker/containers", nil, cookies)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("Docker 不可用应 409，实际 %d: %v", res.StatusCode, out)
	}
	msg, _ := out["msg"].(string)
	if !strings.Contains(msg, "Docker") {
		t.Errorf("错误信息应说明是 Docker 环境问题，实际: %s", msg)
	}
	if !strings.Contains(msg, "应用市场") && !strings.Contains(msg, "socket") {
		t.Errorf("错误信息应给出下一步动作（去哪里装/怎么启动），实际: %s", msg)
	}

	// info 接口不能报错：环境不可用本身就是要显示给用户的信息
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/docker/info", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("info 即使不可用也应 200，实际 %d", res.StatusCode)
	}
	data := out["data"].(map[string]any)
	if data["available"] != false {
		t.Errorf("应报告 available=false，实际 %v", data["available"])
	}
	if strings.TrimSpace(asString(data["error"])) == "" {
		t.Error("不可用时应给出原因，不能是空字符串")
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// TestDockerImageNameWithSlashRoutes 镜像名里的 `/` 必须能正确路由与还原。
//
// 这是这套路由最容易错的地方：Go 1.22 的 `{id}` 路径参数**不跨 /**，
// 所以 `library/redis` 这种名字必须靠 `{path...}` + URL 编码。
// 一旦写错，表现是"删镜像报 404"，而且很难看出原因。
func TestDockerImageNameWithSlashRoutes(t *testing.T) {
	sock, fake := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	// 拉取：带斜杠的镜像名走请求体，不受路由影响
	res, _, _ := doJSON(t, ts, "POST", "/api/v1/docker/images/pull",
		map[string]string{"image": "library/redis:7"}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("拉取应 200，实际 %d", res.StatusCode)
	}
	if !fake.sawPath("fromImage=library%2Fredis%3A7") {
		t.Errorf("拉取请求没带上正确的镜像名，实际请求：%v", fake.requests)
	}

	// 删除：带斜杠的镜像名走路径，必须还原成 library/redis:7
	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/docker/images/library%2Fredis%3A7", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除带斜杠镜像名应 200，实际 %d: %v", res.StatusCode, out)
	}
	if !fake.sawPath("DELETE /images/library/redis:7") {
		t.Errorf("镜像名没有还原成 library/redis:7，实际请求：%v", fake.requests)
	}
}

// TestDockerContainerCreateValidatesPorts 端口映射的畸形输入必须被拦住并说明原因。
func TestDockerContainerCreateValidatesPorts(t *testing.T) {
	sock, _ := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	cases := []struct {
		name string
		body map[string]any
		want int
		msg  string
	}{
		{"缺镜像", map[string]any{"ports": []string{"8080:80"}}, 400, "镜像"},
		{"端口越界", map[string]any{"image": "nginx", "ports": []string{"70000:80"}}, 400, "端口"},
		{"端口非数字", map[string]any{"image": "nginx", "ports": []string{"abc:80"}}, 400, "端口"},
		{"协议不支持", map[string]any{"image": "nginx", "ports": []string{"8080:80/ftp"}}, 400, "协议"},
		{"环境变量缺等号", map[string]any{"image": "nginx", "env": []string{"TZ"}}, 400, "环境变量"},
		{"挂载不是绝对路径", map[string]any{"image": "nginx", "binds": []string{"rel:/data"}}, 400, "挂载"},
		{"容器名非法", map[string]any{"image": "nginx", "name": "bad name!"}, 400, "容器名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/containers", c.body, cookies)
			if res.StatusCode != c.want {
				t.Fatalf("应 %d，实际 %d: %v", c.want, res.StatusCode, out)
			}
			if msg := asString(out["msg"]); !strings.Contains(msg, c.msg) {
				t.Errorf("错误信息应包含 %q，实际: %s", c.msg, msg)
			}
		})
	}

	// 正常路径必须能过
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/containers",
		map[string]any{"image": "nginx:alpine", "name": "my-nginx",
			"ports": []string{"8080:80", "127.0.0.1:5353:53/udp"}}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("合法输入应 200，实际 %d: %v", res.StatusCode, out)
	}
}

// TestDockerBuiltinNetworkProtected 内置网络不能被删（删了会破坏默认网络行为）。
func TestDockerBuiltinNetworkProtected(t *testing.T) {
	sock, fake := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	res, out, _ := doJSON(t, ts, "DELETE", "/api/v1/docker/networks/bridge", nil, cookies)
	if res.StatusCode == 200 {
		t.Fatal("删除内置网络 bridge 必须被拒绝")
	}
	if !strings.Contains(asString(out["msg"]), "内置网络") {
		t.Errorf("应说明是内置网络，实际: %v", out["msg"])
	}
	if fake.sawPath("DELETE /networks/bridge") {
		t.Error("不该真的把删除请求发给 Docker")
	}

	// 普通网络可以删
	res, _, _ = doJSON(t, ts, "DELETE", "/api/v1/docker/networks/net-mine", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除普通网络应 200，实际 %d", res.StatusCode)
	}
}

// TestDockerComposeProjectLifecycle 保存 → 读取 → 列出的往返，含路径越界防护。
func TestDockerComposeProjectLifecycle(t *testing.T) {
	sock, _ := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	yml := "services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"8080:80\"\n"

	// 1) 保存（顺带登记为服务）
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/compose",
		map[string]any{"name": "myblog", "content": yml, "register": true}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("保存应 200，实际 %d: %v", res.StatusCode, out)
	}

	// 2) 读回来必须一模一样
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/docker/compose/myblog", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("读取应 200，实际 %d", res.StatusCode)
	}
	if got := asString(out["data"].(map[string]any)["content"]); got != yml {
		t.Errorf("读回的内容与写入不一致:\n写入: %q\n读回: %q", yml, got)
	}

	// 3) 列表里能看到它，且已标记为已登记
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/docker/compose", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("列表应 200，实际 %d", res.StatusCode)
	}
	list, _ := out["data"].(map[string]any)["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("应有一个项目，实际 %d: %v", len(list), list)
	}
	proj := list[0].(map[string]any)
	if proj["name"] != "myblog" || proj["exists"] != true {
		t.Errorf("项目属性不对: %v", proj)
	}
	if proj["registered"] != true {
		t.Errorf("保存时选了登记，registered 应为 true: %v", proj)
	}

	// 4) 项目名越界必须被拒 —— 这是"编辑 compose 文件"这条能力的边界
	for _, bad := range []string{"..%2F..%2Fetc", "a%2Fb", ".hidden", "bad%20name"} {
		res, _, _ := doJSON(t, ts, "POST", "/api/v1/docker/compose",
			map[string]any{"name": bad, "content": yml}, cookies)
		if res.StatusCode == 200 {
			t.Errorf("非法项目名 %q 不该被接受", bad)
		}
	}

	// 5) 删除项目目录
	res, _, _ = doJSON(t, ts, "DELETE", "/api/v1/docker/compose/myblog", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除项目应 200，实际 %d", res.StatusCode)
	}
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/docker/compose", nil, cookies)
	if list, _ := out["data"].(map[string]any)["list"].([]any); len(list) != 0 {
		t.Errorf("删除后应为空，实际 %v", list)
	}
}

// TestDockerComposeRejectsEmptyContent 空内容不能保存（否则 compose 一部署就报错）。
func TestDockerComposeRejectsEmptyContent(t *testing.T) {
	sock, _ := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/compose",
		map[string]any{"name": "empty", "content": "   "}, cookies)
	if res.StatusCode == 200 {
		t.Fatal("空 compose 内容必须被拒")
	}
	if !strings.Contains(asString(out["msg"]), "不能为空") {
		t.Errorf("应说明内容为空，实际: %v", out["msg"])
	}
}

// TestDockerComposeActionRequiresFile 没有 yml 时不该去调 compose，而是明确报错。
//
// 注意（2026-09-14 起）：`up` 已经改成**异步任务**（compose up 可能拉几分钟镜像，
// 见 SPEC-任务中心.md），所以这里的契约变成"接口立刻 202 + 任务随后失败"。
// 失败原因仍然必须是"compose 文件不存在"，只是从 HTTP 响应挪到了任务里。
func TestDockerComposeActionRequiresFile(t *testing.T) {
	sock, _ := startFakeDocker(t)
	srv, ts, cookies := newDockerTestServerWithSrv(t, sock)

	res, out, _ := doJSON(t, ts, "POST",
		"/api/v1/docker/compose/nonexistent/actions?action=up", map[string]any{}, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("部署现在是长任务，应立刻 202，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	task := srv.Tasks.Get(asString(data["task_id"]))
	if task == nil {
		t.Fatal("应返回可查询的 task_id")
	}
	select {
	case <-task.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("没有 yml 时应快速失败，而不是卡住")
	}
	if task.Status() != tasks.StatusFailed {
		t.Fatalf("没有 yml 时任务应失败，实际 %s", task.Status())
	}
	if !strings.Contains(task.Meta().Error, "不存在") {
		t.Errorf("应说明 compose 文件不存在，实际: %v", task.Meta().Error)
	}
}

// TestDockerPathsStayInsideComposeRoot 直接测服务层：项目名解析出的路径必须在 compose 根目录下。
func TestDockerPathsStayInsideComposeRoot(t *testing.T) {
	m := services.NewManager(nil, services.Options{WorkDir: t.TempDir(), UserName: "zizdog"})
	root := m.ComposeRoot()

	// 合法名字
	file, err := m.ComposeProjectFile("my-blog_1")
	if err != nil {
		t.Fatalf("合法项目名不该报错: %v", err)
	}
	if !strings.HasPrefix(file, root+string(os.PathSeparator)) {
		t.Errorf("文件路径应落在 compose 根目录下: %s", file)
	}

	// 越界名字全部拒绝
	for _, bad := range []string{"", "..", "../x", "a/b", ".hidden", "a b", strings.Repeat("x", 65)} {
		if _, err := m.ComposeProjectFile(bad); err == nil {
			t.Errorf("非法项目名 %q 应被拒绝", bad)
		}
	}
}

// TestDockerNetworkPruneSkipsBuiltinAndUsed 清理网络只动"非内置且没人连"的。
func TestDockerNetworkPruneSkipsBuiltinAndUsed(t *testing.T) {
	// 复用假守护进程的响应：bridge（内置）+ my-net（无容器）
	sock, fake := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/networks/prune", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("清理应 200，实际 %d: %v", res.StatusCode, out)
	}
	if fake.sawPath("DELETE /networks/net-bridge") {
		t.Error("内置网络 bridge 不该被清理")
	}
	if !fake.sawPath("DELETE /networks/net-mine") {
		t.Errorf("未被使用的自定义网络应被清理，实际请求：%v", fake.requests)
	}
}

// TestSettingsCanClearUpgradeSource 升级源必须能通过设置接口清空。
//
// 这是一个真实缺口：在线升级的"检查更新"会把用过的源**写进配置**，
// 但之前没有任何界面/接口能把它清掉 —— 设置页的输入框清空后，
// 接口会回退到内存里的旧值，用户永远清不掉。
// UI 测试里"未配置升级源时给出明确提示"那条就是这样失败的，
// 所以这条断言锁住的不只是"能清空"，还有"清空后真的报未配置"。
func TestSettingsCanClearUpgradeSource(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatal("初始化失败")
	}

	// 1) 先设一个源
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/settings",
		map[string]any{"upgrade_source": "http://192.168.1.179:18877"}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("设置升级源应 200，实际 %d: %v", res.StatusCode, out)
	}
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/settings", nil, cookies)
	if got := asString(out["data"].(map[string]any)["upgrade_source"]); got != "http://192.168.1.179:18877" {
		t.Fatalf("升级源应被保存，实际 %q", got)
	}

	// 2) 用空串清空
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/settings",
		map[string]any{"upgrade_source": ""}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("清空升级源应 200，实际 %d", res.StatusCode)
	}
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/settings", nil, cookies)
	if got := asString(out["data"].(map[string]any)["upgrade_source"]); got != "" {
		t.Fatalf("升级源应被清空，实际 %q", got)
	}

	// 3) 清空后"检查更新"必须明确报"未配置"，而不是回退到某个幽灵地址
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/upgrade/check", map[string]any{}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("未配置升级源时检查更新应 400，实际 %d: %v", res.StatusCode, out)
	}
	if msg := asString(out["msg"]); !strings.Contains(msg, "升级源") {
		t.Errorf("提示应包含'升级源'，实际: %s", msg)
	}

	// 4) 非法地址必须被拒（避免把设置页卡在一个打不开的地址上）
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/settings",
		map[string]any{"upgrade_source": "ftp://example.com"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法升级源应 400，实际 %d: %v", res.StatusCode, out)
	}
}

// TestHealthReturnsPlainVersionForWatchdog 健康检查必须返回**纯版本号**。
//
// 事故背景：升级看门狗把 /api/v1/health 的 version 与清单里的版本号做字符串比较。
// 而这里原先返回 version.Full()，正式发布的二进制带 git commit（"0.3.1+9a304af"），
// 于是新版**明明起来并且正常服务**，看门狗却匹配不上，90 秒后判定"启动失败"
// 并把面板回滚 —— 真机上连续回滚了三次，面板一直升不上去。
//
// 更麻烦的是"自举"：看门狗脚本由**升级前的旧版本**生成，所以只改新版本的匹配
// 逻辑救不了当次升级；必须让健康检查返回旧版断言所期望的形式。
func TestHealthReturnsPlainVersionForWatchdog(t *testing.T) {
	_, ts := newTestServer(t)
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/health", nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("健康检查应 200，实际 %d", res.StatusCode)
	}
	data, _ := out["data"].(map[string]any)
	got := asString(data["version"])
	if got == "" {
		t.Fatal("健康检查必须带 version 字段（看门狗靠它判定新版是否起来）")
	}
	if strings.Contains(got, "+") {
		t.Errorf("健康检查的 version 不能带 +commit 后缀（会让看门狗匹配不上并回滚）: %q", got)
	}
	// 必须是精确的 version.Version，而不是别的形式
	if got != version.Version {
		t.Errorf("健康检查的 version 应为纯版本号 %q，实际 %q", version.Version, got)
	}
}
