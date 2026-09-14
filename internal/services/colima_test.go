package services

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestColimaEnvIncludesBrewBin 锁定最关键的坑：colima 靠 PATH 找 limactl。
//
// 实测：同一条 `colima status`，带上 /opt/homebrew/bin 就正常返回，不带就报
// "dependency check failed for VM: lima not found" —— 而 lima 其实是装好的。
// 面板、launchd、sudo -n -u 的默认 PATH 都不含 brew 目录，所以必须显式注入。
func TestColimaEnvIncludesBrewBin(t *testing.T) {
	// 家目录用临时目录：测试不该依赖（更不该写入）真实用户目录
	home := t.TempDir()
	m := NewManager(nil, Options{
		BrewBin:  "/opt/homebrew/bin/brew",
		UserHome: home,
		UserName: "zizdog",
	})
	env := m.colimaEnv()

	var path string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if path == "" {
		t.Fatal("colimaEnv 必须包含 PATH，否则 colima 找不到 limactl")
	}
	if !strings.Contains(path, "/opt/homebrew/bin") {
		t.Errorf("PATH 必须含 brew 目录，实际: %s", path)
	}
	if !strings.Contains(strings.Join(env, " "), "HOME="+home) {
		t.Errorf("必须设置 HOME，否则 colima 会用错家目录: %v", env)
	}
}

// TestColimaEnvUsesBrewBinDir 覆盖不同 brew 前缀（Intel 是 /usr/local）。
func TestColimaEnvUsesBrewBinDir(t *testing.T) {
	m := NewManager(nil, Options{BrewBin: "/usr/local/bin/brew", UserHome: "/Users/u"})
	env := strings.Join(m.colimaEnv(), " ")
	if !strings.Contains(env, "/usr/local/bin") {
		t.Errorf("PATH 应跟随 brew 前缀，实际: %s", env)
	}
}

// TestFirstMeaningfulLine 校验 colima 的日志式报错能被提取成人话。
func TestFirstMeaningfulLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			in:   `time="2026-09-13T21:23:20+08:00" level=fatal msg="Error: dependency check failed for VM: lima not found, run 'brew install lima' to install"`,
			want: "Error: dependency check failed for VM: lima not found, run 'brew install lima' to install",
		},
		{in: "plain error text", want: "plain error text"},
		{in: "", want: ""},
		{
			in:   "some noise\n" + `time="x" level=info msg="real problem"` + "\nmore",
			want: "real problem",
		},
	}
	for _, c := range cases {
		if got := firstMeaningfulLine(c.in); got != c.want {
			t.Errorf("firstMeaningfulLine(%q)\n  得到: %q\n  期望: %q", c.in, got, c.want)
		}
	}
}

// TestColimaBinFollowsBrewPrefix 校验 colima 路径解析。
func TestColimaBinFollowsBrewPrefix(t *testing.T) {
	m := NewManager(nil, Options{BrewBin: "/opt/homebrew/bin/brew"})
	if got, want := m.colimaBin(), "/opt/homebrew/bin/colima"; got != want {
		t.Errorf("colimaBin() = %q, 期望 %q", got, want)
	}
}

// TestColimaRuntimeIsNotUninstallable 面板不应提供卸载运行时的能力。
func TestColimaRuntimeIsNotUninstallable(t *testing.T) {
	d := newColimaDriver(Options{}, &Service{Name: ColimaServiceName})
	if err := d.Uninstall(nil); err == nil {
		t.Error("卸载 Docker 运行时必须被拒绝")
	}
}

// TestDriverForColimaWithoutDockerSocket 是本次改动的核心保证：
// Docker 引擎没起来（socket 为空）时，运行时驱动仍必须能构造出来 ——
// 否则用户永远无法从面板把它启动起来，形成死锁。
func TestDriverForColimaWithoutDockerSocket(t *testing.T) {
	m := NewManager(nil, Options{BrewBin: "/opt/homebrew/bin/brew", UserName: "zizdog"})
	drv, err := m.DriverFor(&Service{Name: ColimaServiceName, Kind: KindColima})
	if err != nil {
		t.Fatalf("socket 为空时也必须能管理运行时，却报错: %v", err)
	}
	if drv.Kind() != KindColima {
		t.Errorf("驱动类型错误: %s", drv.Kind())
	}
}

// TestDriverForDockerStillRequiresSocket 反面保证：普通 docker 应用
// 在引擎不可用时仍应明确报错，不能被上面那条放宽影响。
func TestDriverForDockerStillRequiresSocket(t *testing.T) {
	m := NewManager(nil, Options{})
	if _, err := m.DriverFor(&Service{Name: "n8n", Kind: KindCompose}); err == nil {
		t.Error("引擎不可用时 compose 驱动应报错")
	}
}

// TestAutoRegisterKnownSkipsColima 防止 Docker 运行时被登记两次。
//
// 真实故障：AutoRegisterKnown 先按目录条目的 ServiceLabel 把
// com.zizdog.colima 登记成一条 kind=native 服务，随后 EnsureColimaRuntime
// 又登记了 kind=colima 的运行时，于是「服务管理」里同一个东西出现两遍，
// 其中 native 那条因为 colima 的开机任务是一次性作业，永远显示"已加载但未运行"。
func TestAutoRegisterKnownSkipsColima(t *testing.T) {
	var colimaEntries int
	for _, a := range Catalog() {
		if a.Kind == KindColima {
			colimaEntries++
			if a.ServiceLabel == "" {
				t.Error("Docker 运行时条目应带 ServiceLabel，供市场判断是否已纳管")
			}
		}
	}
	if colimaEntries != 1 {
		t.Fatalf("应恰好有一个容器运行时条目，实际 %d 个", colimaEntries)
	}
}

// TestColimaCatalogEntryConsistent 校验目录条目与运行时登记的字段对得上，
// 这是"市场状态"与"服务列表"不打架的前提。
func TestColimaCatalogEntryConsistent(t *testing.T) {
	var entry *App
	for i := range Catalog() {
		if Catalog()[i].Kind == KindColima {
			entry = &Catalog()[i]
		}
	}
	if entry == nil {
		t.Fatal("目录里没有容器运行时条目")
	}
	if entry.ServiceLabel != ColimaLaunchLabel {
		t.Errorf("目录 ServiceLabel(%s) 必须等于运行时登记的 label(%s)",
			entry.ServiceLabel, ColimaLaunchLabel)
	}
	if entry.BrewFormula != "colima" {
		t.Errorf("BrewFormula 应为 colima，实际 %q（市场靠它判断装没装）", entry.BrewFormula)
	}
	if entry.PanelInstaller != "docker-runtime" {
		t.Errorf("PanelInstaller 应为 docker-runtime，实际 %q", entry.PanelInstaller)
	}
}

// TestComposeEntriesHaveImage 校验每个 compose 应用都写了镜像引用。
//
// 回归背景：目录里的 pic-smaller 曾写成 `joyqi/sfz:latest`，而该镜像在
// Docker Hub 上根本不存在（项目也没有官方镜像），用户点安装必然失败，
// 且失败发生在"拉镜像"这一步，报错信息与面板无关，很难看出是目录数据的问题。
// 这里至少保证镜像引用非空且形如 repo[:tag]。
func TestComposeEntriesHaveImage(t *testing.T) {
	for _, a := range Catalog() {
		if a.Kind != KindCompose {
			continue
		}
		if strings.TrimSpace(a.ComposeYAML) == "" {
			t.Errorf("compose 应用 %s 缺少 ComposeYAML", a.ID)
			continue
		}
		// 从 compose 文本里抽出 image: 行
		var images []string
		for _, ln := range strings.Split(a.ComposeYAML, "\n") {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(ln, "image:") {
				if v := strings.TrimSpace(strings.TrimPrefix(ln, "image:")); v != "" {
					images = append(images, v)
				}
			}
		}
		if len(images) == 0 {
			t.Errorf("compose 应用 %s 的 ComposeYAML 里没有 image 引用", a.ID)
			continue
		}
		for _, img := range images {
			// 形如 name 或 name:tag 或 registry/name:tag；至少要有仓库名
			if !strings.Contains(img, "/") {
				t.Errorf("应用 %s 的镜像 %q 缺少仓库前缀（形如 owner/name:tag）", a.ID, img)
			}
			if strings.Contains(img, " ") {
				t.Errorf("应用 %s 的镜像 %q 含空格", a.ID, img)
			}
		}
	}
}

// TestPicSmallerRemoved 记录 pic-smaller 被移除的原因，防止有人凭印象加回来。
func TestPicSmallerRemoved(t *testing.T) {
	for _, a := range Catalog() {
		if a.ID == "pic-smaller" {
			t.Fatal("pic-smaller 的镜像 joyqi/sfz 在 Docker Hub 上不存在，" +
				"该项目也没有官方镜像；如需恢复，必须先确认镜像来源")
		}
	}
}

// TestChownPathFixesRootOwnedDir 是"干净机器装不上 Colima"那个 bug 的回归。
//
// 真实故障：面板以 root 运行，写 LaunchDaemon 时顺带用 MkdirAll 建了
// ~/.colima，于是该目录属主是 root；而 Colima 以真实用户身份运行，接着要
// 在其中创建 default/，直接失败：
//
//	cannot make required directory: mkdir .../.colima/default: permission denied
//
// 结果"已安装 colima、自启也配好了，却起不来"。已装过 Colima 的机器因为目录
// 早已存在且归属正确，完全测不出来 —— 只有干净机器才会暴露。
func TestChownPathFixesRootOwnedDir(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip("取不到当前用户")
	}
	dir := filepath.Join(t.TempDir(), ".colima")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := chownPath(cur.Username, dir); err != nil {
		t.Fatalf("chownPath 应能成功: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("不支持 Stat_t")
	}
	want, _, err := lookupUIDGID(cur.Username)
	if err != nil {
		t.Fatal(err)
	}
	if int(st.Uid) != want {
		t.Errorf("目录属主 uid = %d，期望 %d（Colima 需要真实用户拥有该目录）", st.Uid, want)
	}
	if fi.Mode().Perm()&0o300 == 0 {
		t.Errorf("属主缺少写/执行位 %v，用户仍无法在其中建子目录", fi.Mode().Perm())
	}
}

// TestLookupUIDGID 校验 uid/gid 解析。
func TestLookupUIDGID(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip("取不到当前用户")
	}
	uid, gid, err := lookupUIDGID(cur.Username)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if uid <= 0 || gid <= 0 {
		t.Errorf("uid/gid 应大于 0，得到 %d/%d", uid, gid)
	}
	if _, _, err := lookupUIDGID("__no_such_user__"); err == nil {
		t.Error("不存在的用户应返回错误")
	}
}

// TestColimaFastState 是"服务管理页慢"那条的回归测试。
//
// 原来每次刷新都跑 `colima status`（实测 1.07 秒），现在改读 Lima 实例目录：
// hostagent 的 ha.pid 与 ssh.sock。这里用临时目录复刻三种状态。
func TestColimaFastState(t *testing.T) {
	// 家目录用 /tmp 下的短路径：macOS 的 unix socket 路径上限约 104 字节，
	// 而 t.TempDir() 的路径（含完整测试名）轻易就超了，报错是误导性的
	// "bind: invalid argument"，看起来像参数写错。
	home, err0 := os.MkdirTemp("/tmp", "zpc")
	if err0 != nil {
		t.Fatal(err0)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	m := NewManager(nil, Options{UserHome: home})

	// 1) 没有实例目录 → found=false（调用方据此回退到 CLI）
	if running, _, found := m.colimaFastState(); found {
		t.Error("没有实例目录时 found 应为 false")
	} else if running {
		t.Error("没有实例目录时不该报告运行中")
	}

	// 2) 造一个"正在运行"的实例：ha.pid 指向一个活着的进程（用测试进程自己），
	//    并且 ssh.sock 是一个真实的 unix socket。
	dir := filepath.Join(home, ".colima", "_lima", "colima")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ha.pid"), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "ssh.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("造 unix socket 失败: %v", err)
	}
	defer ln.Close()

	if running, detail, found := m.colimaFastState(); !found || !running {
		t.Errorf("pid 活着 + socket 存在时应报运行中，实际 running=%v found=%v (%s)", running, found, detail)
	}

	// 3) pid 指向一个不存在的进程 → 必须报未运行。
	//    这一条是关键：只看 socket 文件会被残留文件骗到（虚拟机没跑但 socket 还在）。
	if err := os.WriteFile(filepath.Join(dir, "ha.pid"), []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if running, _, found := m.colimaFastState(); !found || running {
		t.Errorf("pid 已死时应报未运行，实际 running=%v found=%v", running, found)
	}

	// 4) socket 没了 → 也必须报未运行（只看 pid 会被 PID 复用骗到）
	if err := os.WriteFile(filepath.Join(dir, "ha.pid"), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(sockPath)
	if running, _, found := m.colimaFastState(); !found || running {
		t.Errorf("socket 不存在时应报未运行，实际 running=%v found=%v", running, found)
	}
}
