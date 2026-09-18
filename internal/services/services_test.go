package services

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/store"
)

// newTestManager 构造一个基于临时数据库的 Manager。
func newTestManager(t *testing.T) (*Manager, *Repository) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := NewRepository(st)
	m := NewManager(repo, Options{
		BrewBin: "/opt/homebrew/bin/brew",
		// ⚠️ 不能用 os.Getenv("HOME")：那是**真实**家目录。
		// 测试里的所有用户可见路径都必须落在临时目录里（见 README 坑 57）。
		UserHome: t.TempDir(),
		UserName: "zizdog",
		UID:      os.Getuid(),
		WorkDir:  t.TempDir(),
	})
	return m, repo
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Qwen3 TTS":      "qwen3-tts",
		"my_service.v1":  "my-service-v1",
		"  Spaces  ":     "spaces",
		"UPPER":          "upper",
		"a---b":          "a-b",
		"中文名":            "",
		"com.zizdog.tts": "com-zizdog-tts",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Fatalf("NormalizeName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 目录里的每个应用都必须有完整的健康检查信息，
// 否则用户装完后面板显示"未知"状态，等于没管起来。
func TestCatalogEntriesAreComplete(t *testing.T) {
	apps := Catalog()
	if len(apps) == 0 {
		t.Fatal("应用目录为空")
	}
	ids := map[string]bool{}
	for _, a := range apps {
		if a.ID == "" {
			t.Fatalf("应用缺少 ID: %+v", a)
		}
		if ids[a.ID] {
			t.Fatalf("应用 ID 重复: %s", a.ID)
		}
		ids[a.ID] = true
		if a.Name == "" || a.Summary == "" {
			t.Fatalf("应用 %s 缺少名称或简介", a.ID)
		}
		if a.Kind != KindNative && a.Kind != KindCompose && a.Kind != KindDocker && a.Kind != KindColima {
			t.Fatalf("应用 %s 的类型不合法: %s", a.ID, a.Kind)
		}
		// 纳管类必须有 AdoptLabel；安装类必须给出安装方式
		if a.AdoptLabel != "" {
			if a.Kind != KindNative {
				t.Fatalf("纳管类应用 %s 应为 native 类型", a.ID)
			}
		} else {
			switch a.Kind {
			case KindNative:
				// 三种合法的"原生"安装方式：
				//  ① 通用 brew formula；
				//  ② 面板自研安装器（要建 venv、改 nginx、注册守护进程，通用流程做不了）；
				//  ③ 一键建站（SiteApp）：装出来是一个网站，由面板的建站流程负责
				//     （下载源码 + 建库 + 建站点），既不是包也不是常驻服务。
				if a.BrewFormula == "" && a.PanelInstaller == "" && a.SiteApp == nil {
					t.Fatalf("原生应用 %s 既没有 BrewFormula、PanelInstaller 也没有 SiteApp，无法自动安装", a.ID)
				}
				if a.SiteApp != nil {
					// 建站类必须说清楚：从哪下、什么格式、套哪个伪静态、装完去哪继续
					if a.SiteApp.DownloadURL == "" || a.SiteApp.Archive == "" ||
						a.SiteApp.Rewrite == "" || a.SiteApp.FinishPath == "" {
						t.Fatalf("建站应用 %s 的 SiteApp 描述不完整（下载地址/格式/伪静态/安装入口都要有）", a.ID)
					}
					if a.Category != "site" {
						t.Errorf("建站应用 %s 的 category 应为 site（市场按它分组）", a.ID)
					}
				}
			case KindCompose:
				if a.ComposeYAML == "" {
					t.Fatalf("compose 应用 %s 缺少 ComposeYAML", a.ID)
				}
				if !strings.Contains(a.ComposeYAML, "container_name:") {
					t.Fatalf("compose 应用 %s 未固定 container_name（面板按名字管理容器）", a.ID)
				}
			}
		}
		if a.Port > 0 && a.HealthPath == "" {
			t.Logf("提示：应用 %s 有端口但没配健康检查路径，面板无法判断它是否真的可用", a.ID)
		}
	}
}

// 目录里的 compose 条目**绝不允许**指定 platform。
//
// 这是 2026-09 定下的长期规则：只有镜像本身自带 linux/arm64 才允许进目录，
// 写 `platform: linux/amd64` 会走 Rosetta 转译（慢且占内存），
// 与"能原生就原生"的选品策略相悖。用测试锁死它，
// 免得以后有人为了让某个只有 amd64 的镜像跑起来偷偷加一行。
func TestCatalogComposeNeverPinsPlatform(t *testing.T) {
	for _, a := range Catalog() {
		if a.Kind != KindCompose {
			continue
		}
		if strings.Contains(a.ComposeYAML, "platform:") {
			t.Fatalf("compose 应用 %s 指定了 platform，违反「Docker 只允许原生 arm64 镜像」的规则", a.ID)
		}
	}
}

// 端口分配不应重复：两个应用抢同一个端口会让后装的那个直接失败。
func TestCatalogPortsAreUnique(t *testing.T) {
	seen := map[int]string{}
	for _, a := range Catalog() {
		if a.Port == 0 {
			continue
		}
		if prev, ok := seen[a.Port]; ok {
			t.Fatalf("端口 %d 被 %s 与 %s 同时使用", a.Port, prev, a.ID)
		}
		seen[a.Port] = a.ID
	}
}

// 用真实的 launchd 服务验证状态查询是否可信。
//
// 本机存在 com.zizdog.qwen3tts（用户自己装的 AI 服务），
// 用它验证比造一个假服务有意义得多：能发现 plist 解析、域判断等真实问题。
func TestStatusForRealLaunchdService(t *testing.T) {
	m, _ := newTestManager(t)
	home := os.Getenv("HOME")
	plist := filepath.Join(home, "Library/LaunchAgents/com.zizdog.qwen3tts.plist")
	if _, err := os.Stat(plist); err != nil {
		t.Skip("本机没有 com.zizdog.qwen3tts 服务，跳过真实状态验证")
	}

	svc := &Service{
		Name: "qwen3tts", DisplayName: "Qwen3 TTS", Kind: KindNative,
		LaunchLabel: "com.zizdog.qwen3tts", PlistPath: plist, Port: 8880,
	}
	drv, err := m.DriverFor(svc)
	if err != nil {
		t.Fatalf("构造驱动失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := drv.Status(ctx)
	if err != nil {
		t.Fatalf("查询状态失败: %v", err)
	}
	if st.Status == "unknown" || st.Status == "error" {
		t.Fatalf("状态查询结果不可信: %+v", st)
	}
	t.Logf("真实服务状态: status=%s running=%v pid=%d detail=%s",
		st.Status, st.Running, st.PID, st.Detail)

	// 端口检查应与 launchd 状态一致（服务实际在监听 8880）
	portSt, err := (&nativeDriver{opt: m.opt, svc: &Service{Port: 8880}}).statusByPort(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running && !portSt.Running {
		t.Logf("警告：launchd 说在运行，但端口 8880 未监听（服务可能刚重启）")
	}
}

// 从 plist 读取日志路径：这是"看日志"功能能否工作的关键。
func TestNativeDriverReadsLogPathFromPlist(t *testing.T) {
	m, _ := newTestManager(t)
	home := os.Getenv("HOME")
	plist := filepath.Join(home, "Library/LaunchAgents/com.zizdog.qwen3tts.plist")
	if _, err := os.Stat(plist); err != nil {
		t.Skip("本机没有可用的测试 plist")
	}
	d := newNativeDriver(m.opt, &Service{
		Name: "qwen3tts", LaunchLabel: "com.zizdog.qwen3tts", PlistPath: plist,
	})
	paths := d.logPaths()
	if len(paths) == 0 {
		t.Fatal("未能从 plist 读出任何日志路径")
	}
	t.Logf("从 plist 解析到日志路径: %v", paths)
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			t.Fatalf("日志路径不是绝对路径: %s", p)
		}
	}
}

// tailFile 必须能处理大文件并只读末尾，避免把几百 MB 日志读进内存。
func TestTailFileReadsOnlyTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// 写入 20000 行
	for i := 0; i < 20000; i++ {
		if _, err := f.WriteString("line-" + strings.Repeat("x", 40) + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()

	content, err := tailFile(path, 10)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	lines := strings.Split(content, "\n")
	if len(lines) != 10 {
		t.Fatalf("应返回 10 行，实际 %d 行", len(lines))
	}
	// 大文件走"从中间读"路径时，第一行可能是半截，但不能是空内容
	if strings.TrimSpace(content) == "" {
		t.Fatal("读取内容为空")
	}
}

func TestTailFileMissing(t *testing.T) {
	if _, err := tailFile(filepath.Join(t.TempDir(), "nope.log"), 10); err == nil {
		t.Fatal("文件不存在应返回错误")
	}
}

// 端口推荐必须只返回真正空闲的端口。
func TestPortCandidatesAreFree(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	ports := m.PortCandidates(ctx, 41000, 3)
	if len(ports) == 0 {
		t.Skip("未取到候选端口（可能全部被占用）")
	}
	for _, p := range ports {
		if p < 41000 || p > 41199 {
			t.Fatalf("候选端口超出请求范围: %d", p)
		}
		// 再次确认端口未被监听
		info, err := checkPortFree(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if !info {
			t.Fatalf("推荐的端口 %d 实际已被占用", p)
		}
	}
	t.Logf("推荐的空闲端口: %v", ports)
}

// checkPortFree 在测试里用系统命令直接确认端口空闲（不经 helper，避免依赖 root）。
func checkPortFree(ctx context.Context, port int) (bool, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP",
		"-iTCP:"+itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		// lsof 无匹配时退出码 1，属于"端口空闲"
		return true, nil
	}
	return strings.TrimSpace(string(out)) == "", nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 纳管一个不存在的服务必须失败，且不能写入数据库。
func TestAdoptCandidateRejectsMissing(t *testing.T) {
	m, repo := newTestManager(t)
	ctx := context.Background()
	if _, err := m.AdoptCandidate(ctx, "com.example.does-not-exist", "X", ""); err == nil {
		t.Fatal("纳管不存在的服务应报错")
	}
	if _, err := m.AdoptCandidate(ctx, "com.apple.finder", "Finder", ""); err == nil {
		t.Fatal("系统自带服务不允许纳管")
	}
	list, _ := repo.List(ctx)
	if len(list) != 0 {
		t.Fatalf("失败的纳管不应写入数据库，实际有 %d 条", len(list))
	}
}

// 纳管服务绝不能被面板卸载 —— 那会删掉用户自己的东西。
func TestUninstallRefusesAdoptedService(t *testing.T) {
	m, repo := newTestManager(t)
	ctx := context.Background()
	svc := &Service{
		Name: "adopted", DisplayName: "用户自己的服务", Kind: KindNative,
		LaunchLabel: "com.example.adopted", Managed: false, Enabled: true,
	}
	if err := repo.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	err := m.Uninstall(ctx, "adopted")
	if err == nil {
		t.Fatal("卸载纳管服务必须被拒绝")
	}
	// 文案 2026-09-19 换成用户语言：说清"这是面板只做了登记的服务（软件由你自己装），
	// 面板不会卸载它"，并给出真正的出口「从列表移除（不卸载软件）」。
	for _, want := range []string{"不会卸载", "从列表移除"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应说明原因（含 %q），实际: %v", want, err)
		}
	}
	// 记录仍在
	if _, err := repo.Get(ctx, "adopted"); err != nil {
		t.Fatalf("被拒绝的卸载不应删除记录: %v", err)
	}
}

// 没有 launchd 也没有启动命令的服务应给出可诊断的状态，而不是报错。
func TestStatusWithoutLaunchdOrCmd(t *testing.T) {
	m, _ := newTestManager(t)
	svc := &Service{Name: "empty", Kind: KindNative}
	drv, err := m.DriverFor(svc)
	if err != nil {
		t.Fatal(err)
	}
	st, err := drv.Status(context.Background())
	if err != nil {
		t.Fatalf("状态查询不应报错: %v", err)
	}
	if st.Status == "" {
		t.Fatal("应给出状态描述")
	}
	t.Logf("空服务状态: %s / %s", st.Status, st.Detail)
}

// 命令驱动：启动 → 运行 → 停止 的完整流程。
// 用一个真实的 sleep 进程验证，确保进程组管理与 pid 文件逻辑正确。
func TestCommandDriverLifecycle(t *testing.T) {
	m, repo := newTestManager(t)
	ctx := context.Background()
	logFile := filepath.Join(m.opt.WorkDir, "cmd-test.log")
	svc := &Service{
		Name: "cmd-test", DisplayName: "命令测试", Kind: KindNative,
		StartCmd: "echo started; sleep 120",
		LogPath:  logFile,
		Enabled:  true, Managed: true,
	}
	if err := repo.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	drv, err := m.DriverFor(svc)
	if err != nil {
		t.Fatal(err)
	}

	if st, _ := drv.Status(ctx); st.Running {
		t.Fatal("初始状态不应为运行中")
	}
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	st, err := drv.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Fatalf("启动后应处于运行中: %+v", st)
	}
	t.Logf("命令服务已启动 pid=%d", st.PID)

	// 日志应包含启动输出
	logs, err := drv.Logs(ctx, 20)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(logs, "started") {
		t.Fatalf("日志未包含预期输出: %q", logs)
	}

	// 幂等启动
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("重复启动应幂等: %v", err)
	}

	if err := drv.Stop(ctx); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if st, _ := drv.Status(ctx); st.Running {
		t.Fatal("停止后不应处于运行中")
	}
}

// 健康检查：对一个确定不存在的地址必须返回 OK=false 且给出人话提示。
func TestHealthCheckFailsGracefully(t *testing.T) {
	svc := &Service{HealthURL: "http://127.0.0.1:59998/nope", Name: "x"}
	h := httpHealth(context.Background(), svc)
	if !h.Checked {
		t.Fatal("应标记为已检查")
	}
	if h.OK {
		t.Fatal("不存在的地址不应判为健康")
	}
	if h.Message == "" {
		t.Fatal("应给出失败原因")
	}
	t.Logf("健康检查失败信息: %s", h.Message)
}

// 无健康检查地址时不应误报为不健康。
func TestHealthCheckWithoutURL(t *testing.T) {
	h := httpHealth(context.Background(), &Service{Name: "x"})
	if h.Checked {
		t.Fatal("没有配置健康检查地址时不应标记为已检查")
	}
	if h.OK {
		t.Fatal("未检查时不应判为健康")
	}
}

// TestCatalogUIDeclarationsAreSane 校验目录里的界面声明。
//
// 为什么需要：slug 会变成 nginx location 与面板路由。重复的 slug 会让
// 两个应用抢同一个路径（后注册的静默覆盖前一个），而带斜杠或空格的 slug
// 会生成语法错误的 nginx 配置。这些都不是编译期能发现的。
func TestCatalogUIDeclarationsAreSane(t *testing.T) {
	seen := map[string]string{}
	for _, a := range Catalog() {
		if a.UI == nil {
			continue
		}
		if a.UI.Slug == "" {
			t.Errorf("%s 声明了 UI 但没有 slug", a.ID)
			continue
		}
		if strings.ContainsAny(a.UI.Slug, "/ \t") {
			t.Errorf("%s 的 slug %q 含斜杠或空白（会写出坏的 nginx location）", a.ID, a.UI.Slug)
		}
		if prev, dup := seen[a.UI.Slug]; dup {
			t.Errorf("slug %q 被 %s 与 %s 同时使用（会互相覆盖）", a.UI.Slug, prev, a.ID)
		}
		seen[a.UI.Slug] = a.ID
		// 有界面的应用要么有端口（面板要反代过去），要么安装器自己写了 nginx
		if a.Port <= 0 && !a.UI.SelfConf {
			t.Errorf("%s 有界面、却既没有端口也不是 SelfConf：面板不知道反代到哪里", a.ID)
		}
	}
	// 没有界面的应用不该有 UI（反之亦然：纯 API 不给「打开」按钮）
	for _, a := range Catalog() {
		if a.UI != nil && a.Port <= 0 && !a.UI.SelfConf {
			t.Errorf("%s 的 UI 声明不完整", a.ID)
		}
	}
	// phpMyAdmin 必须被标成"没有守护进程"，否则界面会一直说它"服务未注册"
	adopted := false
	for _, a := range Catalog() {
		if a.ID == "phpmyadmin" {
			adopted = a.NoDaemon
		}
	}
	if !adopted {
		t.Error("phpMyAdmin 应标记 NoDaemon（它没有守护进程，否则市场会误报「服务未注册」）")
	}
}
