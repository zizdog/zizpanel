package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  "重复安装必须幂等" 的回归测试
//
//  背景（真机 Mac mini，2026-09-16，27 个应用逐个安装取数）：在已经装过的应用上
//  再点一次「安装」，任务失败而不是"已安装/跳过"：
//
//    · nginx      端口 80 已被面板管理的服务占用：homebrew-mxcl-nginx
//    · mysql84    端口 3306 已被 sh-brew-mysql8-4 占用
//    · ollama     端口 11434 已被 ollama 占用
//    · uptime-kuma 端口 3001 已被 uptime-kuma 占用
//    · php81/82/83/84  服务已安装但写入注册表失败: 服务名 php83 已存在
//
//  下面这组测试就是把这 8 个场景逐一钉死，同时锁住两条不能动的既有语义：
//    · 真冲突（占用者是**别的**服务/进程）必须失败并点名占用者；
//    · 卸载（保留数据）之后必须还能重装（产物还在 ≠ 已安装）。
// ============================================================================

// realMachineAlreadyInstalled 复刻真机上那 8 个"已安装"现场。
//
// record/label/port 都按真机取数时的原样：nginx 与 mysql 的记录名是标签归一化
// 出来的（homebrew-mxcl-nginx / sh-brew-mysql8-4），PHP 的记录名就是目录 ID。
func realMachineAlreadyInstalled() []struct {
	id     string
	record string
	label  string
	port   int
	kind   Kind
} {
	return []struct {
		id     string
		record string
		label  string
		port   int
		kind   Kind
	}{
		{"nginx", "homebrew-mxcl-nginx", "homebrew.mxcl.nginx", 80, KindNative},
		{"mysql84", "sh-brew-mysql8-4", "sh.brew.mysql@8.4", 3306, KindNative},
		{"ollama", "ollama", "homebrew.mxcl.ollama", 11434, KindNative},
		{"uptime-kuma", "uptime-kuma", "", 3001, KindCompose},
		{"php81", "php81", "homebrew.mxcl.php@8.1", 0, KindNative},
		{"php82", "php82", "homebrew.mxcl.php@8.2", 0, KindNative},
		{"php83", "php83", "homebrew.mxcl.php@8.3", 0, KindNative},
		{"php84", "php84", "homebrew.mxcl.php@8.4", 0, KindNative},
	}
}

// sandboxIdempotentManager 把 launchd 目录与镜像探测也隔离掉。
//
// 为什么必须隔离 launchd：开发机/用户机上可能真的装着 nginx、php ——
// 不隔离的话"没有记录时应走正常安装"这类断言会随机器状态飘
// （web 层的 launchDaemonsDir 变量就是为同一个原因抽出来的）。
func sandboxIdempotentManager(t *testing.T) (*Manager, *Repository) {
	t.Helper()
	m, repo := sandboxManager(t)
	orig := launchDaemonsDirs
	launchDaemonsDirs = []string{t.TempDir()}
	t.Cleanup(func() { launchDaemonsDirs = orig })
	// brewEnv 默认会去探测真实镜像（发网络请求）——单测不许联网。
	m.mirrorProbeOverride = func(ctx context.Context, probeFormula string) (string, string) {
		return "", ""
	}
	return m, repo
}

// writeFailingFakeBrew 写一个"一被调用就记一笔并失败"的假 brew。
//
// 故意让它失败：万一日后幂等判定被改坏、代码真的走到安装流程，测试会在
// brew install 处立刻失败返回，**不会**继续去跑 brew services / 改 php-fpm 配置
// ——单测不许碰真实服务与生产配置。
func writeFailingFakeBrew(t *testing.T, marker string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brew")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeRecordingFakeBrew 写一个"记录每次调用并成功"的假 brew。
//
// 用来验证"卸载（保留数据）后重装"确实执行了安装动作（而不是被幂等分支跳过）。
// list 返回成功 = 包已存在；services info 返回一份最小 JSON（brewServiceInfo 要解析它）。
func writeRecordingFakeBrew(t *testing.T, marker string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brew")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + marker + `'
case "$1 $2" in
  "services info")
    printf '%s' '[{"name":"fake","status":"started","file":"/tmp/zizpanel-test-fake.plist"}]'
    ;;
esac
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestInstallSkipsWhenAppAlreadyInstalled 锁住真机 8 个失败场景的核心契约：
// 已安装 + 端口被自己占用 / 记录名已存在 → 任务成功且说明"已安装/跳过"，
// 并且**不重复执行任何安装命令**。
func TestInstallSkipsWhenAppAlreadyInstalled(t *testing.T) {
	for _, c := range realMachineAlreadyInstalled() {
		t.Run(c.id, func(t *testing.T) {
			m, repo := sandboxIdempotentManager(t)
			ctx := context.Background()

			marker := filepath.Join(t.TempDir(), "brew-called")
			m.opt.BrewBin = writeFailingFakeBrew(t, marker)
			// 复刻真机：这 4 个应用的端口正被它们自己占着。
			m.portCheckOverride = func(port int) (bool, []string, error) {
				if c.port > 0 && port == c.port {
					return true, []string{"fakeproc (pid 1)"}, nil
				}
				return false, nil, nil
			}

			if err := repo.Create(ctx, &Service{
				Name: c.record, DisplayName: c.id, Kind: c.kind,
				LaunchLabel: c.label, Port: c.port, Managed: true,
			}); err != nil {
				t.Fatalf("准备现场失败: %v", err)
			}

			res, err := m.Install(ctx, c.id)
			if err != nil {
				t.Fatalf("已经装过的应用再点安装必须是良性终态，实际报错: %v", err)
			}
			if res == nil {
				t.Fatal("幂等跳过也要返回结果：步骤里要告诉用户「已安装、没有重复装」")
			}
			if !strings.Contains(res.Message, "已经装过") || !strings.Contains(res.Message, "跳过") {
				t.Errorf("终态说明要写明已安装/跳过，实际 %q", res.Message)
			}
			joined := strings.Join(res.Steps, "\n")
			if !strings.Contains(joined, "没有重复执行安装命令") {
				t.Errorf("步骤里要说明没有重复安装，实际步骤：\n%s", joined)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Errorf("幂等跳过时不该执行任何 brew 命令，实际调用记录存在：%s", marker)
			}
			// 不能又插一条重复记录（php83 那种"服务名已存在"的根因）
			list, lerr := repo.List(ctx)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if len(list) != 1 {
				t.Errorf("重复安装不该产生重复记录，实际 %d 条", len(list))
			}
		})
	}
}

// TestPreflightPortHeldByOwnServiceIsNotAConflict 锁住"自己占自己的端口不算冲突"。
//
// 真机上 nginx / mysql84 / ollama / uptime-kuma 就是死在这一步：
// 应用本来在跑，端口被它自己占着，旧代码一律判成"安装前检查未通过"。
func TestPreflightPortHeldByOwnServiceIsNotAConflict(t *testing.T) {
	for _, c := range realMachineAlreadyInstalled() {
		if c.port == 0 {
			continue // PHP-FPM 听 Unix socket，不走端口检查
		}
		t.Run(c.id, func(t *testing.T) {
			m, repo := sandboxIdempotentManager(t)
			ctx := context.Background()
			// compose 应用要过 docker 检查；否则 Ready 会因为"未检测到 Docker"为 false。
			m.opt.DockerSocket = "/tmp/zizpanel-test-docker.sock"
			// brew 必须"存在且包已装"，否则"未安装 Homebrew"这条不可修复的检查
			// 会把 Ready 压成 false，测出来的就不是端口这一条了。
			m.opt.BrewBin = writeRecordingFakeBrew(t, filepath.Join(t.TempDir(), "brew-calls"))
			m.portCheckOverride = func(port int) (bool, []string, error) {
				if port == c.port {
					return true, []string{"fakeproc (pid 1)"}, nil
				}
				return false, nil, nil
			}
			if err := repo.Create(ctx, &Service{
				Name: c.record, DisplayName: c.id, Kind: c.kind,
				LaunchLabel: c.label, Port: c.port, Managed: true,
			}); err != nil {
				t.Fatal(err)
			}

			app, ok := FindApp(c.id)
			if !ok {
				t.Fatalf("目录里没有 %s", c.id)
			}
			pf := m.Preflight(ctx, app)
			if !pf.PortFree {
				t.Errorf("端口被应用自己的服务占用不是冲突，PortFree 应为 true，实际 false（note=%s）", pf.PortNote)
			}
			if !pf.AlreadyInstalled {
				t.Error("这种情况必须标记为已安装（界面据此显示「已安装」而不是「安装」）")
			}
			if !pf.Ready {
				t.Errorf("已安装的应用预检查应为可安装（幂等成功），实际 Ready=false（note=%s）", pf.PortNote)
			}
			if !strings.Contains(pf.PortNote, "自己的服务") {
				t.Errorf("提示里要说明是它自己占的，实际 %q", pf.PortNote)
			}
		})
	}
}

// TestPreflightAndInstallStillFailOnRealPortConflict 锁住"真冲突不许被吞成已安装"。
//
// 两种真冲突都要失败，且错误里必须点名占用者：
//  1. 端口被面板管理的**别的**服务占用；
//  2. 端口被面板完全不知道的进程占用。
func TestPreflightAndInstallStillFailOnRealPortConflict(t *testing.T) {
	t.Run("面板里别的服务占着", func(t *testing.T) {
		m, repo := sandboxIdempotentManager(t)
		ctx := context.Background()
		m.opt.DockerSocket = "/tmp/zizpanel-test-docker.sock"
		m.opt.BrewBin = writeRecordingFakeBrew(t, filepath.Join(t.TempDir(), "brew-calls"))
		m.portCheckOverride = func(port int) (bool, []string, error) {
			if port == 11434 {
				return true, []string{"some-other (pid 42)"}, nil
			}
			return false, nil, nil
		}
		// 另一条与本应用无关的面板记录占着 11434。
		if err := repo.Create(ctx, &Service{
			Name: "some-other-service", DisplayName: "别的服务", Kind: KindNative,
			LaunchLabel: "com.example.someother", Port: 11434, Managed: true,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := m.Install(ctx, "ollama")
		if err == nil {
			t.Fatal("端口被别的服务占用是真冲突，必须失败（不许吞成已安装）")
		}
		if !strings.Contains(err.Error(), "11434") || !strings.Contains(err.Error(), "some-other-service") {
			t.Errorf("错误里要点名是哪个服务占用了端口，实际：%v", err)
		}
		if strings.Contains(err.Error(), "跳过") {
			t.Errorf("真冲突不能在错误里出现「跳过」字样：%v", err)
		}
	})

	t.Run("系统里别的进程占着", func(t *testing.T) {
		m, _ := sandboxIdempotentManager(t)
		ctx := context.Background()
		m.opt.DockerSocket = "/tmp/zizpanel-test-docker.sock"
		m.opt.BrewBin = writeRecordingFakeBrew(t, filepath.Join(t.TempDir(), "brew-calls"))
		m.portCheckOverride = func(port int) (bool, []string, error) {
			if port == 11434 {
				return true, []string{"node (pid 777)"}, nil
			}
			return false, nil, nil
		}
		_, err := m.Install(ctx, "ollama")
		if err == nil {
			t.Fatal("端口被无关进程占用必须失败")
		}
		if !strings.Contains(err.Error(), "11434") || !strings.Contains(err.Error(), "node (pid 777)") {
			t.Errorf("错误里要点名占用进程，实际：%v", err)
		}
	})
}

// TestRegisterAppServiceIsIdempotentForSameApp 锁住注册表这一步的幂等语义。
//
// 真机 php81/82/83/84 的失败原文是：
//
//	服务已安装但写入注册表失败: 服务名 php83 已存在
//
// 契约：同名记录**就是这个应用** → 幂等成功；同名记录是别的应用 → 如实报冲突。
func TestRegisterAppServiceIsIdempotentForSameApp(t *testing.T) {
	m, repo := sandboxIdempotentManager(t)
	ctx := context.Background()

	app, ok := FindApp("php83")
	if !ok {
		t.Fatal("目录里没有 php83")
	}
	// 已存在的记录与目录条目一致（同 label/同应用）
	if err := repo.Create(ctx, &Service{
		Name: "php83", DisplayName: app.Name, Kind: KindNative,
		LaunchLabel: app.ServiceLabel, Managed: true,
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := m.registerAppService(ctx, app, &Service{
		Name: "php83", DisplayName: app.Name, Kind: KindNative,
		LaunchLabel: app.ServiceLabel, Managed: true,
	})
	if err != nil {
		t.Fatalf("同名且同应用的记录必须幂等成功，实际报错: %v", err)
	}
	if stored == nil || stored.Name != "php83" {
		t.Fatalf("幂等成功也应返回已有记录，实际 %+v", stored)
	}
	if list, _ := repo.List(ctx); len(list) != 1 {
		t.Errorf("幂等成功不该再插一条，实际 %d 条", len(list))
	}

	// 同名但是**另一个**应用 → 必须如实报冲突（不许张冠李戴地当成功）
	other, ok := FindApp("php84")
	if !ok {
		t.Fatal("目录里没有 php84")
	}
	_, err = m.registerAppService(ctx, other, &Service{
		Name: "php83", DisplayName: other.Name, Kind: KindNative,
	})
	if err == nil {
		t.Fatal("同名记录属于别的应用时必须报冲突，不能当成「已安装」")
	}
	if !strings.Contains(err.Error(), "php83") {
		t.Errorf("冲突错误里要点名占用的记录名，实际：%v", err)
	}
}

// TestReinstallAfterUninstallKeepDataIsNotSkipped 是既有语义的回归：
// "卸载（保留数据）后必须能重装"。
//
// 卸载会删掉服务记录与 plist，只留下安装产物（~/iopaint、compose 目录、
// brew 包等）。产物**不是**"已安装"的证据 —— 这正是历史上踩过的坑。
// 这条测试同时证明：重装会真的走安装流程，而不是被幂等分支跳过。
func TestReinstallAfterUninstallKeepDataIsNotSkipped(t *testing.T) {
	m, repo := sandboxIdempotentManager(t)
	ctx := context.Background()

	app, ok := FindApp("ollama")
	if !ok {
		t.Fatal("目录里没有 ollama")
	}
	// 复刻"卸载（保留数据）"之后的现场：记录与 plist 都没了，产物还在。
	if err := os.MkdirAll(filepath.Join(m.opt.UserHome, ".ollama"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := m.installedRecordFor(ctx, app); rec != nil {
		t.Fatal("没有面板记录时不该判成已安装 —— 否则卸载后无法重装")
	}
	if label := m.presentServiceLabel(app); label != "" {
		t.Fatalf("plist 已随卸载删除，不该再判成已安装，实际命中 %s", label)
	}

	marker := filepath.Join(t.TempDir(), "brew-calls")
	m.opt.BrewBin = writeRecordingFakeBrew(t, marker)
	m.portCheckOverride = func(port int) (bool, []string, error) { return false, nil, nil }

	res, err := m.Install(ctx, "ollama")
	if err != nil {
		t.Fatalf("卸载（保留数据）之后必须还能重装，实际报错: %v", err)
	}
	if res == nil || res.Service == nil {
		t.Fatal("重装成功必须登记出服务记录")
	}
	if strings.Contains(res.Message, "跳过") {
		t.Errorf("产物还在不等于已安装，重装不该被跳过，实际 %q", res.Message)
	}
	list, lerr := repo.List(ctx)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(list) != 1 || list[0].Name != "ollama" {
		t.Fatalf("重装后应有一条 ollama 记录，实际 %+v", list)
	}
	calls, rerr := os.ReadFile(marker)
	if rerr != nil {
		t.Fatalf("假 brew 没有被调用，说明重装根本没走安装流程: %v", rerr)
	}
	if !strings.Contains(string(calls), "services start ollama") {
		t.Errorf("重装必须真的执行安装动作（brew services start），实际调用：\n%s", calls)
	}
}

// TestPresentServiceLabelIsSandboxed 说明并锁住"没有记录、但服务真在 launchd 里"
// 的处理：同样算已安装（卡片按 plist/brew 已经这么显示了），并提示用户去纳管。
func TestAlreadyInLaunchdWithoutRecordIsTreatedAsInstalled(t *testing.T) {
	m, repo := sandboxIdempotentManager(t)
	ctx := context.Background()

	// 服务真的注册在（沙箱的）系统域里，但面板注册表是空的。
	plist := filepath.Join(launchDaemonsDirs[0], "homebrew.mxcl.ollama.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if label := m.presentServiceLabel(App{ID: "ollama", Name: "Ollama", BrewFormula: "ollama"}); label != "homebrew.mxcl.ollama" {
		t.Fatalf("应从磁盘推出真实标签，实际 %q", label)
	}

	res, err := m.Install(ctx, "ollama")
	if err != nil {
		t.Fatalf("服务已在 launchd 里时不该再报 failed，实际 %v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "纳管") {
		t.Errorf("没有面板记录时要提示用户去「纳管」，实际步骤：\n%s", joined)
	}
	if list, _ := repo.List(ctx); len(list) != 0 {
		t.Errorf("这一步只跳过、不写记录（纳管由用户点），实际 %d 条", len(list))
	}
}
