package web

import (
	"context"
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
	"github.com/zizdog/zizpanel/internal/upgrade"
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

	// containers 覆盖 /containers/json 的返回（nil = 用默认那台 uptime-kuma）。
	// 让单个测试能造出"带 compose 项目标签的容器"，而不必改默认响应。
	containers []map[string]any
	// logs 覆盖 /containers/{name}/logs 的返回（空 = 用默认那一行）。
	// 用它造"日志里有初始随机口令"的场景（File Browser 就是这种）。
	logs string
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

// setContainers / setLogs 在测试发起请求之前改假守护进程的返回。
func (f *fakeDaemon) setContainers(list []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = list
}

func (f *fakeDaemon) setLogs(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = text
}

func (f *fakeDaemon) containerList() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers
}

func (f *fakeDaemon) logText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs
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
			if override := f.containerList(); override != nil {
				writeJSON(w, 200, override)
				return
			}
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
			// 真实的 /images/create 是**边拉边推**的多行 JSON 进度流。
			// 这里刻意分多行、带层进度：拉取必须逐行进任务日志，
			// 如果退回"读完整段响应再返回最后一行"，这些中间行就看不到了。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"Pulling from ` + r.URL.Query().Get("fromImage") + `","id":"7"}` + "\n"))
			_, _ = w.Write([]byte(`{"status":"Pulling fs layer","progressDetail":{},"id":"abc123def456"}` + "\n"))
			_, _ = w.Write([]byte(`{"status":"Downloading","progressDetail":{"current":1048576,"total":50331648},"id":"abc123def456"}` + "\n"))
			_, _ = w.Write([]byte(`{"status":"Pull complete","progressDetail":{},"id":"abc123def456"}` + "\n"))
			_, _ = w.Write([]byte(`{"status":"Status: Downloaded newer image for ` + r.URL.Query().Get("fromImage") + `"}` + "\n"))

		case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
			// 镜像详情：默认"本地没有"（404），这样创建容器会走"先拉取"这条路 ——
			// 正是本地缺镜像时用户会遇到、也必须看到过程的场景。
			writeJSON(w, 404, map[string]any{"message": "No such image: " + strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")})

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
			if text := f.logText(); text != "" {
				_, _ = w.Write([]byte(text))
				return
			}
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
	return newDockerTestServerFull(t, sock, nil)
}

// newDockerTestServerFull 接受一个容器运行时状态探测的**注入实现**。
//
// 为什么必须能注入：市场卡片对 docker-runtime 的"已安装"判据是真实探测
// （看 PATH 上有没有 colima、连一次本机 docker socket），结论会随开发机装没装
// Colima 而变 —— 那正是"单测不许碰真实服务"禁止的。更重要的是要能造出
// 用户实测的那个现场并把它钉死（见 TestMarketZombieColimaPlistNotInstalled）。
func newDockerTestServerFull(t *testing.T, sock string, runtimeProbe func(context.Context) services.DockerRuntimeState) (*Server, *httptest.Server, []*http.Cookie) {
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
	// 系统级 LaunchDaemons 目录必须沙箱化（与 newTestServer 同样的理由）：
	// 容器运行时的"残留识别"会 stat com.zizdog.colima.plist，而单测**绝不能**
	// 去读写真实 /Library/LaunchDaemons —— 第一次写这条测试时就撞上了
	// "permission denied"，说明它真的碰到了系统目录。
	prevLaunchDaemonsDir := launchDaemonsDir
	launchDaemonsDir = dir + "/LaunchDaemons"
	if err := os.MkdirAll(launchDaemonsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { launchDaemonsDir = prevLaunchDaemonsDir })
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
	srv.dockerRuntimeProbeOverride = runtimeProbe
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
//
// 注意（2026-09 起）：拉取已经是**异步任务**（202 + task_id），
// 所以"请求有没有带对镜像名"要在任务跑完后从假守护进程的请求记录里核。
func TestDockerImageNameWithSlashRoutes(t *testing.T) {
	sock, fake := startFakeDocker(t)
	srv, ts, cookies := newDockerTestServerWithSrv(t, sock)

	// 拉取：带斜杠的镜像名走请求体，不受路由影响
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/images/pull",
		map[string]string{"image": "library/redis:7"}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("拉取是长任务，应立刻 202，实际 %d: %v", res.StatusCode, out)
	}
	task := waitDockerTask(t, srv, out)
	if task.Status() != tasks.StatusSucceeded {
		t.Fatalf("拉取任务应成功，实际 %s：%s", task.Status(), task.Meta().Error)
	}
	if !fake.sawPath("fromImage=library%2Fredis%3A7") {
		t.Errorf("拉取请求没带上正确的镜像名，实际请求：%v", fake.requests)
	}

	// 删除：带斜杠的镜像名走路径，必须还原成 library/redis:7
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/docker/images/library%2Fredis%3A7", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删除带斜杠镜像名应 200，实际 %d: %v", res.StatusCode, out)
	}
	if !fake.sawPath("DELETE /images/library/redis:7") {
		t.Errorf("镜像名没有还原成 library/redis:7，实际请求：%v", fake.requests)
	}
}

// TestDockerContainerCreateValidatesPorts 端口映射的畸形输入必须被拦住并说明原因。
//
// 参数错误全部发生在**提交任务之前**（仍是 400）；只有合法输入才会变成
// 202 + task_id（本地缺镜像时任务会先拉取，再由假守护进程创建容器）。
func TestDockerContainerCreateValidatesPorts(t *testing.T) {
	sock, fake := startFakeDocker(t)
	srv, ts, cookies := newDockerTestServerWithSrv(t, sock)

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

	// 正常路径：202 + task_id，任务负责"缺镜像先拉、再创建"
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/containers",
		map[string]any{"image": "nginx:alpine", "name": "my-nginx",
			"ports": []string{"8080:80", "127.0.0.1:5353:53/udp"}}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("合法输入应 202（长任务），实际 %d: %v", res.StatusCode, out)
	}
	task := waitDockerTask(t, srv, out)
	if task.Status() != tasks.StatusSucceeded {
		t.Fatalf("创建容器任务应成功，实际 %s：%s", task.Status(), task.Meta().Error)
	}
	if !fake.sawPath("POST /containers/create") {
		t.Errorf("任务应该真的去创建容器，实际请求：%v", fake.requests)
	}
	// 本地没有镜像（假守护进程的 /images/{ref}/json 返回 404）时要先拉取 ——
	// 这正是用户最需要看到过程的那条路。
	if !fake.sawPath("fromImage=nginx%3Aalpine") {
		t.Errorf("本地缺镜像时应先拉取，实际请求：%v", fake.requests)
	}
}

// TestDockerImagePullIsTaskAndStreamsUpstreamProgress 是本轮改动的核心契约：
//
//	拉镜像 = 202 + task_id；任务的日志里能看到**逐行**的上游拉取进度
//	（层状态 + 下载字节数），而不是结束时贴一段尾巴。
//
// 同时锁住"命令标签从实际请求派生"：这里走的是 Docker API，不是 CLI，
// 所以标签必须是 POST /images/create fromImage=…，不能写成 `docker pull`。
func TestDockerImagePullIsTaskAndStreamsUpstreamProgress(t *testing.T) {
	sock, _ := startFakeDocker(t)
	srv, ts, cookies := newDockerTestServerWithSrv(t, sock)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/docker/images/pull",
		map[string]string{"image": "library/redis:7"}, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("拉取应立刻 202，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	taskID := taskIDFrom(t, out)
	if taskID == "" {
		t.Fatalf("202 响应里必须有 task_id，实际 %v", out)
	}
	if asString(data["title"]) == "" {
		t.Error("202 响应里应带标题（任务中心列表直接显示它）")
	}
	task := waitDockerTask(t, srv, out)
	if task.Status() != tasks.StatusSucceeded {
		t.Fatalf("拉取任务应成功，实际 %s：%s", task.Status(), task.Meta().Error)
	}

	logs := taskLinesText(t, task)
	for _, want := range []string{
		"step:拉取镜像 library/redis:7",
		// 命令标签从实际请求派生（AGENTS.md 的既有约定）
		"cmd:# POST /images/create fromImage=library/redis:7",
		// 模拟的上游拉取行：层状态必须逐行出现
		"out:abc123def456: Pulling fs layer",
		"out:abc123def456: Downloading 1.0 MB/48.0 MB (2%)",
		"out:abc123def456: Pull complete",
		"out:Status: Downloaded newer image for library/redis:7",
		"ok:镜像 library/redis:7 拉取完成",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("任务日志里缺少 %q\n实际日志：\n%s", want, logs)
		}
	}

	// 参数错误仍然当场 400：不能先给 202、再让用户在任务日志里等一句报错。
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/docker/images/pull",
		map[string]string{"image": "   "}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("空镜像名应 400，实际 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/docker/images/pull",
		map[string]string{"image": "bad image"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("带空白的镜像名应 400，实际 %d", res.StatusCode)
	}
}

// TestDockerContainerLogsFullLogs 是"完整日志"这条能力：lines=0 表示 tail=all。
//
// 为什么必须有它：File Browser 这类镜像的初始 admin 口令只在**首次启动**时
// 打进日志一次，只取尾部几百行会把它翻没 —— 而那串口令只在日志里有。
func TestDockerContainerLogsFullLogs(t *testing.T) {
	sock, fake := startFakeDocker(t)
	ts, cookies := newDockerTestServer(t, sock)

	// 默认（不传 lines）：只看尾部 300 行
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/docker/containers/logs?name=filebrowser", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("读日志应 200，实际 %d: %v", res.StatusCode, out)
	}
	if !fake.sawPath("tail=300") {
		t.Errorf("默认应只取尾部 300 行，实际请求：%v", fake.requests)
	}

	// lines=0：显式要完整日志 → Docker 的 tail=all
	res, _, _ = doJSON(t, ts, "GET", "/api/v1/docker/containers/logs?name=filebrowser&lines=0", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("读完整日志应 200，实际 %d", res.StatusCode)
	}
	if !fake.sawPath("tail=all") {
		t.Errorf("lines=0 应翻译成 Docker 的 tail=all，实际请求：%v", fake.requests)
	}
}

// TestDockerComposeContainersAndInitialPasswordScrape 锁住"部署完成后能一眼拿到
// 容器日志，并且初始口令会被捞出来"这条链路。
//
// 目录里的 File Browser 就是靠这条：它的随机 admin 口令只打在首次启动日志里
// （见 catalog.go 的 PostInstallHint）。这里用假守护进程造出同样的场景：
// 一个带 com.docker.compose.project 标签的容器 + 一行 "randomly generated password"。
func TestDockerComposeContainersAndInitialPasswordScrape(t *testing.T) {
	sock, fake := startFakeDocker(t)
	srv, _, _ := newDockerTestServerWithSrv(t, sock)

	fake.setContainers([]map[string]any{{
		"Id": "fb1234567890abcd", "Names": []string{"/filebrowser"},
		"Image": "filebrowser/filebrowser:latest", "State": "running",
		"Status": "Up 3 seconds", "Created": 1757000000,
		"Labels": map[string]string{"com.docker.compose.project": "filebrowser"},
	}})
	fake.setLogs("2026-09-14T00:00:00Z User 'admin' initialized with randomly generated password: zNhlM0V4cDQCsuD2\n")

	mgr := srv.svcManager()

	// 只挑本项目：别的项目名不该匹配到它
	if got := mgr.DockerComposeProjectContainers(context.Background(), "other-project"); len(got) != 0 {
		t.Errorf("别的项目不该匹配到这个容器，实际 %v", got)
	}
	list := mgr.DockerComposeProjectContainers(context.Background(), "filebrowser")
	if len(list) != 1 {
		t.Fatalf("应找到 1 个容器，实际 %d: %v", len(list), list)
	}
	if list[0].Name != "filebrowser" || list[0].State != "running" {
		t.Errorf("容器信息不对：%+v", list[0])
	}

	creds := mgr.DockerScrapeStartupCredentials(context.Background(), list)
	var user, pass string
	for _, c := range creds {
		switch c.Key {
		case "username":
			user = c.Value
		case "password":
			pass = c.Value
		}
	}
	if user != "admin" || pass != "zNhlM0V4cDQCsuD2" {
		t.Fatalf("应从容器日志里捞出初始账号口令，实际 %v", creds)
	}

	// 安全约定（硬要求，见 InstallResult.Credentials）：明文口令**只**进
	// 结果的 credentials，任务日志（会被长期保存/转发）里一个字都不许有。
	var mu sync.Mutex
	var emitted []string
	ctx := services.WithProgress(context.Background(), func(level, text string) {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, level+":"+text)
	})
	if got := mgr.DockerScrapeStartupCredentials(ctx, list); len(got) != 2 {
		t.Fatalf("带上进度接收器时也应返回 2 条凭据，实际 %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(emitted, "\n")
	if !strings.Contains(joined, "用户名 admin") {
		t.Errorf("日志里应有一句「捞到了初始口令」的提示，实际：\n%s", joined)
	}
	if strings.Contains(joined, pass) {
		t.Errorf("明文口令绝不能出现在任务日志里（只允许进结果的 credentials）：\n%s", joined)
	}
}

// waitTaskDone 从 202 响应里取出 task_id，等到任务结束并返回它。
//
// 复用本包已有的 waitTaskDone（api_certs_test.go）与 taskIDFrom，避免两套等待逻辑。
func waitDockerTask(t *testing.T, srv *Server, out map[string]any) *tasks.Task {
	t.Helper()
	return waitTaskDone(t, srv, taskIDFrom(t, out))
}

// taskLinesText 把任务日志拼成 "level:text" 逐行文本，方便做精确断言。
func taskLinesText(t *testing.T, task *tasks.Task) string {
	t.Helper()
	lines, _, _, _ := task.Snapshot(0, 5000)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Level)
		b.WriteString(":")
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
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

	// 3) 清空后"检查更新"不应再报 400「尚未配置升级源地址」。
	//
	// 空源现在的语义是"按候选顺序自动选源"（同网段 NAS → 公网主源 →
	// 备用镜像 → GitHub 兜底，见 internal/upgrade/source.go），
	// 所以"没配源"不再是错误。测试环境没有内嵌发布公钥，
	// 请求会在联网之前 fail closed 返回 409 —— 关键是不能再是 400。
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/upgrade/check", map[string]any{}, cookies)
	if res.StatusCode == http.StatusBadRequest {
		t.Fatalf("清空升级源后检查更新不应再报 400，实际 %d: %v", res.StatusCode, out)
	}
	if upgrade.HasPublicKey() {
		t.Fatalf("测试环境不应配置发布公钥：否则这条检查会真的去联网")
	}
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("没有公钥时应 fail closed 返回 409，实际 %d: %v", res.StatusCode, out)
	}
	if msg := asString(out["msg"]); !strings.Contains(msg, "公钥") {
		t.Errorf("提示应说明缺少发布公钥，实际: %s", msg)
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

// ============================================================================
//  容器运行时状态：市场卡片与 Docker 版块（2026-09-19 用户实测的两个问题）
//
//  问题 1：本机没有 colima（~/.colima 不存在、which colima 为空、
//          /var/run/docker.sock 不存在），只有旧版安装留下的僵尸 plist
//          /Library/LaunchDaemons/com.zizdog.colima.plist，
//          市场却显示「Colima 已安装·未纳管」。
//  问题 2：Docker 版块在没有运行时只给「去应用市场 / 去服务管理」两个指路按钮，
//          而服务管理早已合并进应用市场 —— 用户找不到 Docker。
//
//  下面把这两条钉死：僵尸 plist ≠ 已安装；market 的 docker-runtime 条目必须
//  带**真实探测结论**（前端据此给「一键安装」）。
// ============================================================================

// zombieColimaRuntimeState 复刻用户实测的现场结论：二进制不在、socket 不在、
// 只有上一轮留下的 plist。
func zombieColimaRuntimeState(context.Context) services.DockerRuntimeState {
	return services.DockerRuntimeState{
		BinaryInstalled: false,
		State:           services.DockerRuntimeNotInstalled,
		PlistExists:     true,
		Artifacts:       true,
		VMState:         "未创建虚拟机实例",
		Note:            "未安装 Docker 运行时（Colima）；检测到上一次安装的残留（开机自启配置或配置目录），可重新安装覆盖",
	}
}

// TestMarketZombieColimaPlistNotInstalled 是用户那条假"已安装"的端到端回归。
//
// 关键断言：
//
//	· installed=false —— 僵尸 plist 绝不能算已安装（否则卡片没有安装入口）；
//	· artifacts=true  —— 残留要如实报出来，界面才好给「重新安装 / 清理残留」；
//	· docker_runtime.state=not-installed —— 前端据此给「🐳 一键安装 Docker」；
//	· note 里不能再说"已安装"。
func TestMarketZombieColimaPlistNotInstalled(t *testing.T) {
	srv, ts, cookies := newDockerTestServerFull(t, "", zombieColimaRuntimeState)

	// 真机现场：旧版留下的僵尸 plist（测试里 launchDaemonsDir 已被沙箱化，
	// 见 server_test.go —— 单测绝不碰真实 /Library/LaunchDaemons）。
	plist := filepath.Join(launchDaemonsDir, services.ColimaLaunchLabel+".plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = srv

	it := marketItem(t, ts, cookies, "docker-runtime")
	if it["installed"] != false {
		t.Fatalf("只有僵尸 plist、colima 二进制不在时，installed 必须是 false —— "+
			"否则市场卡片停在「已安装」，用户根本找不到安装入口（2026-09-19 用户实测）。实际 %v（note=%v）",
			it["installed"], it["note"])
	}
	if it["artifacts"] != true {
		t.Errorf("残留的 plist 要如实报成 artifacts=true（界面据此给「重新安装 / 清理残留」），实际 %v", it["artifacts"])
	}
	rt, _ := it["docker_runtime"].(map[string]any)
	if rt == nil {
		t.Fatal("docker-runtime 条目必须带 docker_runtime 真实探测结论（前端据此区分安装/启动）")
	}
	if got := asString(rt["state"]); got != services.DockerRuntimeNotInstalled {
		t.Errorf("docker_runtime.state 应为 %q，实际 %q", services.DockerRuntimeNotInstalled, got)
	}
	if rt["binary_installed"] != false {
		t.Errorf("binary_installed 应为 false，实际 %v", rt["binary_installed"])
	}
	if rt["plist_exists"] != true {
		t.Errorf("plist_exists 要如实报 true（残留识别就靠它），实际 %v", rt["plist_exists"])
	}
	if strings.Contains(asString(it["note"]), "已安装") {
		t.Errorf("没装就不能在 note 里说已安装，实际 %q", asString(it["note"]))
	}
}

// TestMarketColimaInstalledButEngineStopped 覆盖第二态：
// 装了 colima 但引擎没在跑 → installed=true（二进制在），note 如实写清"引擎没在运行"。
//
// 这一条是"不许把停止态谎报成运行态"的接口层保证：卡片可以显示已安装，
// 但必须告诉用户引擎没起来、并能从这里启动。
func TestMarketColimaInstalledButEngineStopped(t *testing.T) {
	probe := func(context.Context) services.DockerRuntimeState {
		return services.DockerRuntimeState{
			BinaryInstalled: true,
			BinPath:         "/opt/homebrew/bin/colima",
			State:           services.DockerRuntimeStopped,
			VMState:         "虚拟机未运行（实例 colima）",
			Note:            "已安装但引擎没在运行（可在 Docker 页点启动）",
		}
	}
	_, ts, cookies := newDockerTestServerFull(t, "", probe)

	it := marketItem(t, ts, cookies, "docker-runtime")
	if it["installed"] != true {
		t.Errorf("二进制在时应报已安装，实际 %v", it["installed"])
	}
	if got := asString(it["note"]); !strings.Contains(got, "没在运行") {
		t.Errorf("装了但引擎没跑时 note 必须如实写清（用户在卡片上要看到），实际 %q", got)
	}
	rt, _ := it["docker_runtime"].(map[string]any)
	if asString(rt["state"]) != services.DockerRuntimeStopped {
		t.Errorf("state 应为 %q，实际 %v", services.DockerRuntimeStopped, rt["state"])
	}
}

// TestDockerInfoExposesRuntimeState 锁住 Docker 版块首屏需要的契约。
//
// 前端 renderUnavailable 靠 info.runtime 区分三态：
//
//	· binary_installed=false → 「🐳 一键安装 Docker（Colima）」
//	· binary_installed=true  → 「▶ 启动 Docker 运行时」
//
// 少了这些字段，用户又只能看到"去别处装"的指路文案（用户实测的痛点）。
func TestDockerInfoExposesRuntimeState(t *testing.T) {
	_, ts, cookies := newDockerTestServerFull(t, "", zombieColimaRuntimeState)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/docker/info", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("info 应 200，实际 %d", res.StatusCode)
	}
	data, _ := out["data"].(map[string]any)
	if data["available"] != false {
		t.Errorf("没有引擎时 available 应为 false，实际 %v", data["available"])
	}
	rt, _ := data["runtime"].(map[string]any)
	if rt == nil {
		t.Fatal("info 必须带 runtime 真实探测结论（前端据此决定显示「一键安装」还是「启动」）")
	}
	if rt["binary_installed"] != false || asString(rt["state"]) != services.DockerRuntimeNotInstalled {
		t.Errorf("runtime 探测结论不对: %v", rt)
	}
	if got := asString(data["runtime_app_id"]); got != "docker-runtime" {
		t.Errorf("runtime_app_id 应为 docker-runtime（前端拿它提交安装任务），实际 %q", got)
	}
}

// TestDockerRuntimeInstallEndpointExists 锁住"一键安装"的落点。
//
// 用户要求 Docker 版块直接给「一键安装 docker」，而不是指路到应用市场 ——
// 所以前端点的那个接口（POST /api/v1/market/docker-runtime/install）必须存在、
// 必须走任务中心（202 + task_id），而不是同步挂在那里。
// 这里不断言安装成功（测试机没有 Homebrew，装了就是真的动了系统），
// 只断言"提交成功并给出可查询的任务"——失败会如实落在任务结果里。
func TestDockerRuntimeInstallEndpointExists(t *testing.T) {
	srv, ts, cookies := newDockerTestServerFull(t, "", zombieColimaRuntimeState)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/docker-runtime/install", nil, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("一键安装必须立刻 202（长任务，走任务中心），实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	id := asString(data["task_id"])
	if id == "" {
		t.Fatalf("202 响应里必须有 task_id，实际 %v", out)
	}
	if task := srv.Tasks.Get(id); task == nil {
		t.Fatal("任务中心里必须能查到刚提交的安装任务")
	}
}
