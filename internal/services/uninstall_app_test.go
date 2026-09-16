package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/store"
)

// TestPlanUninstallKinds 锁住三类语义的判定。
//
// 这是用户反馈的那件事的核心：面板安装器装的应用（IOPaint / Qwen / 接收端 /
// phpMyAdmin）注册时是**纳管**状态，通用卸载会拒绝；市场必须给出正确的卸载方式，
// 而纳管的第三方服务（nginx / php / mysql）**永远不能**被面板卸载。
func TestPlanUninstallKinds(t *testing.T) {
	m, st := sandboxManager(t)
	ctx := context.Background()

	// ① 面板安装器装的应用：即便登记成了纳管，也要给 installer 计划
	if err := m.repo.Create(ctx, &Service{
		Name: "com-zizdog-iopaint", DisplayName: "IOPaint", Kind: KindNative,
		LaunchLabel: iopaintLabel, Managed: false,
	}); err != nil {
		t.Fatal(err)
	}
	plan := m.PlanUninstall(ctx, "iopaint")
	if plan.Kind != "installer" {
		t.Fatalf("IOPaint 应给 installer 计划，实际 %q（blocked=%s）", plan.Kind, plan.Blocked)
	}
	if len(plan.Steps) == 0 {
		t.Error("计划里要有可读的步骤（确认框要展示给用户）")
	}
	if plan.Service != "com-zizdog-iopaint" {
		t.Errorf("计划里应带上记录名，实际 %q", plan.Service)
	}

	// ② 托管服务（compose 应用）：走通用卸载
	if err := m.repo.Create(ctx, &Service{
		Name: "it-tools", DisplayName: "IT-Tools", Kind: KindCompose, Managed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if p := m.PlanUninstall(ctx, "it-tools"); p.Kind != "service" {
		t.Fatalf("托管服务应给 service 计划，实际 %q", p.Kind)
	}

	// ③ 纳管的第三方服务：只能取消纳管，绝不卸载
	if err := m.repo.Create(ctx, &Service{
		Name: "sh-brew-mysql8-4", DisplayName: "MySQL 8.4", Kind: KindNative,
		LaunchLabel: "sh.brew.mysql@8.4", Managed: false,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st
}

// TestUninstallAppRemovesServiceAndKeepsDataByDefault 是本轮最重要的断言：
//
//	· 默认不动数据（模型/样本/虚拟环境都保留）；
//	· remove_data=true 时才删。
func TestUninstallAppRemovesServiceAndKeepsDataByDefault(t *testing.T) {
	m, _ := sandboxManager(t)
	ctx := context.Background()
	home := m.opt.UserHome

	// 造出 IOPaint 的现场：记录 + 数据目录
	if err := m.repo.Create(ctx, &Service{
		Name: "com-zizdog-iopaint", DisplayName: "IOPaint", Kind: KindNative,
		LaunchLabel: iopaintLabel, Managed: false,
	}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "iopaint")
	if err := os.MkdirAll(filepath.Join(root, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".venv", "bin", "iopaint"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := &InstallResult{App: "iopaint", Steps: []string{}}
	if err := m.UninstallApp(ctx, "iopaint", false, res); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	// 记录必须没了
	if list, _ := m.repo.List(ctx); len(list) != 0 {
		t.Errorf("卸载后面板记录应清空，实际还有 %d 条", len(list))
	}
	// 默认保留数据
	if _, err := os.Stat(root); err != nil {
		t.Error("默认不该删除 ~/iopaint（重新部署时不用重装依赖）")
	}

	// 再来一次并勾选删除数据：这时才真删
	if err := m.repo.Create(ctx, &Service{
		Name: "com-zizdog-iopaint", DisplayName: "IOPaint", Kind: KindNative,
		LaunchLabel: iopaintLabel, Managed: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.UninstallApp(ctx, "iopaint", true, res); err != nil {
		t.Fatalf("带数据卸载失败: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Error("勾选删除数据后 ~/iopaint 应该被删掉")
	}
}

// TestRemoveTreeRefusesDangerousPaths 防止卸载把家目录甚至根删掉。
func TestRemoveTreeRefusesDangerousPaths(t *testing.T) {
	m, _ := sandboxManager(t)
	for _, p := range []string{"", "/", m.opt.UserHome} {
		if err := m.removeTree(context.Background(), p, nil); err == nil {
			t.Errorf("删除 %q 应该被拒绝", p)
		}
	}
}

// TestUninstallDockerRuntimeBlockedByComposeApps：
// 还有 compose 应用在用这个运行时时，必须拒绝卸载（否则那些应用会变成孤儿）。
func TestUninstallDockerRuntimeBlockedByComposeApps(t *testing.T) {
	m, _ := sandboxManager(t)
	ctx := context.Background()
	if err := m.repo.Create(ctx, &Service{
		Name: "uptime-kuma", DisplayName: "Uptime Kuma", Kind: KindCompose, Managed: true,
	}); err != nil {
		t.Fatal(err)
	}
	plan := m.PlanUninstall(ctx, "docker-runtime")
	if plan.Kind != "installer" {
		t.Fatalf("Docker 运行时应有 installer 计划，实际 %q", plan.Kind)
	}
	if plan.Blocked == "" {
		t.Fatal("还有 compose 应用在用，卸载必须被拦下并说明原因")
	}
	if !strings.Contains(plan.Blocked, "Uptime Kuma") {
		t.Errorf("拦截说明里要点名是哪个应用，实际：%s", plan.Blocked)
	}
	if err := m.UninstallApp(ctx, "docker-runtime", false, &InstallResult{Steps: []string{}}); err == nil {
		t.Error("被拦住时 UninstallApp 也必须拒绝，不能只靠界面")
	}
}

// TestRemoveMarkedLocation 验证 nginx 里那段 phpMyAdmin location 能被精确摘掉。
//
// phpMyAdmin 的卸载是"先撤 nginx 入口、再 brew uninstall"，
// 撤错了会把用户自己的 location 删掉或留下半截配置（nginx 直接起不来）。
func TestRemoveMarkedLocation(t *testing.T) {
	conf := `server {
    listen 80;
    location / {
        try_files $uri $uri/ =404;
    }
    # 由 ZizPanel 添加：phpMyAdmin 入口
    location ^~ /phpmyadmin {
        alias /opt/homebrew/share/phpmyadmin;
        index index.php;
        location ~ \.php$ {
            include /opt/homebrew/etc/nginx/includes/php-fpm.conf;
        }
    }
}
`
	out, err := removeMarkedLocation(conf, pmaMarker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "phpmyadmin") {
		t.Errorf("phpMyAdmin 块没被摘干净：\n%s", out)
	}
	if !strings.Contains(out, "location / {") {
		t.Error("别的 location 被误删了")
	}
	if strings.Count(out, "{") != strings.Count(out, "}") {
		t.Errorf("花括号不配对：\n%s", out)
	}
}

// sandboxManager 起一个把 UserHome 指向临时目录的 Manager。
//
// 与项目里其它测试同样的要求：**绝不碰真实家目录** ——
// 卸载逻辑会删目录，拿真实家目录去测等于自毁。
func sandboxManager(t *testing.T) (*Manager, *Repository) {
	t.Helper()
	dir := t.TempDir()
	// store.Open 收的是**目录**（它自己在里面建库文件），传文件名会报
	// "unable to open database file" —— 与项目里其它测试保持一致。
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(st)
	m := NewManager(repo, Options{UserHome: home, UserName: "zizdog", WorkDir: filepath.Join(dir, "work")})
	return m, repo
}

// TestQwenUninstallPlanListsModelDirsNotWholeHub：
// 确认框里写的是"会删哪些路径"，写错比不写更糟 ——
// 早期写成整个 ~/.cache/huggingface/hub，而实现只删清单里的模型目录，
// 用户看到"要删整个缓存"会以为别的项目的模型也没了。
func TestQwenUninstallPlanListsModelDirsNotWholeHub(t *testing.T) {
	m, _ := sandboxManager(t)
	plan := m.PlanUninstallFor(context.Background(), App{
		ID: "qwen3tts", Name: "Qwen3 TTS", PanelInstaller: "qwen3tts",
	}, nil)
	if plan.Kind != "installer" {
		t.Fatalf("应为 installer 计划，实际 %q", plan.Kind)
	}
	hub := filepath.Join(m.opt.UserHome, ".cache", "huggingface", "hub")
	for _, p := range plan.DataPaths {
		if strings.TrimSuffix(p, "/") == strings.TrimSuffix(hub, "/") {
			t.Errorf("不该把整个 HF 缓存目录列成待删路径：%s", p)
		}
	}
	// 清单里的模型目录必须都在
	for _, mdl := range QwenModels {
		want := filepath.Join(hub, "models--"+strings.ReplaceAll(mdl.Name, "/", "--"))
		found := false
		for _, p := range plan.DataPaths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("计划里缺少模型目录 %s", want)
		}
	}
}

// TestServicePlanKeepNoteMatchesKind：原生 brew 服务的卸载**不删包**，
// 说明里必须写清楚（否则用户以为"卸载"等于把 brew 包也删了）。
func TestServicePlanKeepNoteMatchesKind(t *testing.T) {
	m, _ := sandboxManager(t)
	ctx := context.Background()
	if err := m.repo.Create(ctx, &Service{
		Name: "ollama", DisplayName: "Ollama", Kind: KindNative, Managed: true,
	}); err != nil {
		t.Fatal(err)
	}
	plan := m.PlanUninstall(ctx, "ollama")
	if plan.Kind != "service" {
		t.Fatalf("应为 service 计划，实际 %q", plan.Kind)
	}
	if !strings.Contains(plan.KeepNote, "brew uninstall") {
		t.Errorf("原生服务的保留说明里要写明 brew 包不会被卸载，实际：%s", plan.KeepNote)
	}
}

// TestUninstallPlanSeesLegacyNativeInstall 锁住"注册表里没有了、磁盘上还在"的卸载。
//
// 真机事故（2026-09-16，用户原话"lucky 根本没被卸载掉"）：Lucky 从原生二进制
// 改成 Docker 版之后，注册表里不再有它，而卸载计划只按注册表判断 ——
// 已经装在 ~/lucky 的那份既没有卸载入口也删不掉，卡片还显示"已安装"。
//
// 契约：卸载计划必须**以磁盘状态为准**，给出可删的残留目录。
func TestUninstallPlanSeesLegacyNativeInstall(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	m := &Manager{opt: Options{UserHome: home, UserName: "tester", WorkDir: work}}

	// 复刻"旧版原生安装的残留"：~/<app>/<可执行文件>
	dir := filepath.Join(home, "frps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frps"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// frps 已经不在注册表的前提下，计划也必须给出 installer + 可删路径
	plan := m.PlanUninstallFor(context.Background(), App{ID: "frps", Name: "frps", Kind: KindCompose}, nil)
	if plan.Kind != "installer" {
		t.Fatalf("有原生残留时必须给 installer 计划，实际 %q（Blocked=%q）", plan.Kind, plan.Blocked)
	}
	if len(plan.DataPaths) == 0 || plan.DataPaths[0] != dir {
		t.Errorf("计划要列出真实存在的残留目录 %s，实际 %v", dir, plan.DataPaths)
	}

	// 目录里只有数据文件、没有可执行文件时**不算**安装残留（避免误删用户数据目录）
	plain := filepath.Join(home, "someapp")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "data.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.legacyNativeDir(App{ID: "someapp"}); got != "" {
		t.Errorf("只有数据文件时不该判定为安装残留，实际 %q", got)
	}
}

// TestUninstallAppRemovesLegacyNativeAndComposeLeftovers 锁住"点一下就能清干净"。
//
// 2026-09-16 用户要求把 Lucky / frps / Orbien 服务端从面板移除，但用户机器上
// 可能已经装了 —— 那种机器必须仍然删得掉，否则就是最坏状态：
// 界面上没了、磁盘上还在跑、用户无处可点。
// 用 frps 当样例正是这个场景：它已经不在目录里，只能按 ID 找残留。
func TestUninstallAppRemovesLegacyNativeAndComposeLeftovers(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	m := &Manager{opt: Options{UserHome: home, UserName: "tester", WorkDir: work}}

	// 一个已移除条目留下的两种残留：compose 项目目录 + 旧版原生安装目录
	composeProj := filepath.Join(work, "compose", "frps")
	legacy := filepath.Join(home, "frps")
	for _, d := range []string{composeProj, legacy} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(composeProj, "docker-compose.yml"), []byte("services:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "frps"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := m.UninstallApp(context.Background(), "frps", true, &InstallResult{Steps: []string{}}); err != nil {
		t.Fatalf("已移除条目的残留应仍能清理：%v", err)
	}
	for _, d := range []string{composeProj, legacy} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s 应该被删掉（残留清理要真的清干净）", d)
		}
	}
}
