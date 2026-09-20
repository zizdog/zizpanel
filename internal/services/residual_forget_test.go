package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  残留记录清理的门禁（真机 2026-09-20）
//
//  现场：mysql@8.4 的 keg 与 plist 都卸了，services 表里只剩 sh-brew-mysql8-4
//  （port 3306），而 3306 上跑的是 MariaDB。旧实现有三处让它永远删不掉：
//    · native 驱动先看端口 → 把 MariaDB 的监听当成"它还在跑"；
//    · 站点依赖（zizdog.cn / mirror.zizdog.com）把计划 400 掉；
//    · 没有任何"只删面板记录"的 HTTP 入口。
//
//  判据换成贴运行体：自己的 plist / keg / 进程名 / 安装体都不在 ⇒ 残留，
//  只删面板记录是安全的；只要它自己还在，就一律拒绝（绝不留隐身运行的服务）。
// ============================================================================

// residualManager 造"只剩一条死记录 + 3306 上是 MariaDB"的现场（完全离线）。
func residualManager(t *testing.T) (*Manager, *Repository, string) {
	t.Helper()
	// 用 idempotent 那个沙箱：它会把包级 launchDaemonsDirs 也钉到临时目录，
	// 避免 ServiceRuntimeAlive 去读真机 /Library/LaunchDaemons。
	m, repo := sandboxIdempotentManager(t)
	marker := filepath.Join(t.TempDir(), "brew-called")
	m.opt.BrewBin = writeFailingFakeBrew(t, marker)
	m.SetLaunchdDirsForTest([]string{filepath.Join(t.TempDir(), "LaunchDaemons")})
	m.SetBrewInstalledProbeForTest(func(context.Context) (map[string]string, bool) {
		return map[string]string{"mariadb": "13.0.2"}, true
	})
	m.SetPortCheckProbeForTest(func(port int) (bool, []string, error) {
		if port == 3306 {
			return true, []string{"mariadbd (pid 35985)"}, nil
		}
		return false, nil, nil
	})
	return m, repo, marker
}

// staleMySQLRecord 在仓库里造出真机那条残留记录。
func staleMySQLRecord(t *testing.T, repo *Repository) *Service {
	t.Helper()
	rec := &Service{
		Name: "sh-brew-mysql8-4", DisplayName: "MySQL 8.4", Kind: KindNative,
		LaunchLabel: "sh.brew.mysql@8.4", Port: 3306, Managed: true,
	}
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// ① 残留记录 + 3306 上跑着另一个引擎 → 可以只删记录，且**不执行任何命令**。
func TestResidualRecordCanBeForgottenWhileOtherEngineRuns(t *testing.T) {
	m, repo, marker := residualManager(t)
	ctx := context.Background()
	rec := staleMySQLRecord(t, repo)

	alive, why := m.ServiceRuntimeAlive(ctx, rec)
	if alive {
		t.Fatalf("keg/plist 都不在、3306 上是 mariadbd 时必须判成残留，实际 alive=true（%s）", why)
	}
	for _, want := range []string{"Homebrew 里没有 mysql@8.4", "不是它"} {
		if !strings.Contains(why, want) {
			t.Errorf("判定依据里应含 %q，实际：%s", want, why)
		}
	}

	if err := m.ForgetResidual(ctx, rec.Name); err != nil {
		t.Fatalf("残留记录必须能只删面板记录，实际：%v", err)
	}
	if list, _ := repo.List(ctx); len(list) != 0 {
		t.Errorf("记录应当已删除，实际还剩 %d 条", len(list))
	}
	// "没有停任何东西"的硬证据：整个过程中一条外部命令都没执行（brew/launchctl 都没跑）。
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("只删记录不该执行任何 brew 命令，实际调用记录存在：%s", marker)
	}
}

// ② 残留计划不被站点依赖拦住，且如实说清"删记录不影响站点用的 MariaDB"。
func TestResidualPlanIsForgetAndIgnoresSiteDependencies(t *testing.T) {
	m, repo, _ := residualManager(t)
	ctx := context.Background()
	rec := staleMySQLRecord(t, repo)
	m.opt.SiteDependents = func() []SiteRef {
		return []SiteRef{{Domain: "zizdog.cn", Enabled: true}, {Domain: "mirror.zizdog.com", Enabled: true}}
	}
	app, ok := FindApp("mysql84")
	if !ok {
		t.Fatal("目录里应有 mysql84")
	}

	plan := m.PlanUninstallForBrew(ctx, app, rec, BrewState{})
	if plan.Kind != "forget" {
		t.Fatalf("残留记录的计划应为 forget（只删记录），实际 %q", plan.Kind)
	}
	if plan.Blocked != "" {
		t.Errorf("残留清理不该被依赖拦住（站点依赖的是这台机器上的数据库服务），实际 blocked=%q", plan.Blocked)
	}
	if len(plan.Dependents) == 0 {
		t.Error("依赖提示仍要如实列出来（只是不拦）")
	}
	joined := strings.Join(plan.Steps, "\n")
	if !strings.Contains(joined, "不停止任何服务") {
		t.Errorf("残留计划必须写清'不停止任何服务'，实际：%v", plan.Steps)
	}
	if strings.Contains(joined, "停止并移除") || strings.Contains(joined, "停止这个服务") {
		t.Errorf("残留计划里不该有真正停止的步骤（它已经没有可停的东西）：%v", plan.Steps)
	}
	for _, want := range []string{"只删面板记录", "MariaDB", "不会被停", "zizdog.cn"} {
		if !strings.Contains(plan.KeepNote, want) {
			t.Errorf("说明里应含 %q，实际：%s", want, plan.KeepNote)
		}
	}
}

// ③ 它自己的运行体还在（keg / plist / 进程名）→ 拒绝只删记录，并点名凭什么。
func TestResidualForgetRefusedWhenOwnRuntimeAlive(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, m *Manager, rec *Service)
		wantSub string
	}{
		{
			"自己的 keg 还在",
			func(t *testing.T, m *Manager, _ *Service) {
				m.SetBrewInstalledProbeForTest(func(context.Context) (map[string]string, bool) {
					return map[string]string{"mysql@8.4": "8.4.11_4", "mariadb": "13.0.2"}, true
				})
			},
			"Homebrew 包还在",
		},
		{
			"自己的 plist 还在",
			func(t *testing.T, m *Manager, _ *Service) {
				dirs := []string{filepath.Join(t.TempDir(), "LaunchDaemons")}
				if err := os.MkdirAll(dirs[0], 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dirs[0], "sh.brew.mysql@8.4.plist"),
					[]byte("<plist/>"), 0o644); err != nil {
					t.Fatal(err)
				}
				m.SetLaunchdDirsForTest(dirs)
			},
			"launchd 服务定义还在",
		},
		{
			"端口上就是它自己",
			func(t *testing.T, m *Manager, _ *Service) {
				m.SetPortCheckProbeForTest(func(port int) (bool, []string, error) {
					if port == 3306 {
						return true, []string{"mysqld (pid 950)"}, nil
					}
					return false, nil, nil
				})
			},
			"就是它自己",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, repo, _ := residualManager(t)
			ctx := context.Background()
			rec := staleMySQLRecord(t, repo)
			c.setup(t, m, rec)

			alive, why := m.ServiceRuntimeAlive(ctx, rec)
			if !alive {
				t.Fatalf("它自己的运行体还在时必须判 alive，实际 false（%s）", why)
			}
			if !strings.Contains(why, c.wantSub) {
				t.Errorf("证据里应含 %q，实际 %q", c.wantSub, why)
			}
			err := m.ForgetResidual(ctx, rec.Name)
			if err == nil {
				t.Fatal("运行体还在时只删记录必须被拒绝（不许留下看不到却还在跑的服务）")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("拒绝原因里应含 %q，实际：%v", c.wantSub, err)
			}
			if list, _ := repo.List(ctx); len(list) != 1 {
				t.Errorf("拒绝时不该删记录，实际剩 %d 条", len(list))
			}
		})
	}
}

// ④ 有真实数据（data_paths 非空）时**不降级** —— remove_data 语义必须保持不变。
func TestResidualPlanKeepsRealDataSemantics(t *testing.T) {
	m, repo, _ := residualManager(t)
	rec := staleMySQLRecord(t, repo)
	// 直接驱动降级函数：带 data_paths 的计划原样保留。
	plan := UninstallPlan{
		Kind:      "installer",
		Service:   rec.Name,
		DataPaths: []string{"/tmp/some-real-data"},
		Steps:     []string{"删除 /tmp/some-real-data"},
	}
	m.downgradeResidualPlan(context.Background(), App{ID: "mysql84", Name: "MySQL 8.4"}, rec, &plan)
	if plan.Kind != "installer" {
		t.Errorf("带真实数据的计划不该被降级成 %q", plan.Kind)
	}
	if len(plan.DataPaths) != 1 || plan.DataPaths[0] != "/tmp/some-real-data" {
		t.Errorf("data_paths 必须原样保留（remove_data 语义不变），实际 %v", plan.DataPaths)
	}
	if len(plan.Steps) != 1 || plan.Steps[0] != "删除 /tmp/some-real-data" {
		t.Errorf("步骤不该被改写，实际 %v", plan.Steps)
	}

	// 不带 data_paths 的同类计划才降级。
	plan2 := UninstallPlan{Kind: "installer", Service: rec.Name, Steps: []string{"删除 venv"}}
	m.downgradeResidualPlan(context.Background(), App{ID: "mysql84", Name: "MySQL 8.4"}, rec, &plan2)
	if plan2.Kind != "forget" {
		t.Errorf("没有真实数据的残留计划应降级成 forget，实际 %q", plan2.Kind)
	}
}
