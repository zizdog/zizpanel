package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/mysql"
)

// ============================================================================
//  数据库引擎维度（MySQL 8.4 / MariaDB）的门禁
//
//  产品规则（用户 2026-09-20）：**只可能装一个**，面板不帮忙停/卸。
//    ① ParseLNMPSelection：缺省 db_engine → **MariaDB**；显式 mysql 仍可用；
//       未知引擎拒绝；
//    ② 互斥护栏：目标自己已装→放行；另一个已装（不管在不在跑）→拒绝并给出
//       `brew services stop X && brew uninstall X`；两个都装→两个入口都拒、
//       只有"自己就是当前生效引擎"放行；端口被无关进程占用→拒绝并点名；
//    ③ 初始化命令按引擎选（MariaDB 不认 --initialize-insecure）。
//
//  护栏探测**全部注入**：默认实现会跑真实 brew 与 lsof 3306。
// ============================================================================

// guardManager 造一个完全离线的 Manager（launchd 目录也隔离）。
func guardManager(t *testing.T) *Manager {
	t.Helper()
	m, _ := sandboxManager(t)
	prefix := t.TempDir()
	m.opt.BrewBin = filepath.Join(prefix, "bin", "brew")
	m.SetLaunchdDirsForTest([]string{filepath.Join(prefix, "LaunchDaemons")})
	return m
}

// TestParseLNMPSelectionDefaultsToMariaDB 锁住**产品默认值**：缺省 → MariaDB。
//
// 用户 2026-09-20 明确要求默认 MariaDB（此前是 MySQL）。只有 body 里**真的没有**
// 数据库线索（既没 db_engine 也没 mysql）时才走默认；显式给了 mysql 的老客户端
// 必须继续拿到 MySQL（见下一个用例）。
func TestParseLNMPSelectionDefaultsToMariaDB(t *testing.T) {
	for _, body := range []string{"", "  ", "{}", "null", `{"php":"php@8.2"}`} {
		sel, err := ParseLNMPSelection([]byte(body))
		if err != nil {
			t.Fatalf("body=%q 应当合法（用默认值），实际：%v", body, err)
		}
		if got, want := strings.Join(sel.Formulas(), ","), "nginx,php@8.2,mariadb"; got != want {
			t.Errorf("body=%q：Formulas=%q，期望 %q（默认数据库是 MariaDB）", body, got, want)
		}
		if strings.Contains(strings.Join(sel.Formulas(), ","), "mysql@8.4") {
			t.Errorf("body=%q：默认值里不该出现 mysql@8.4（两个引擎只能装一个）", body)
		}
		if sel.DBEngine() != "mariadb" {
			t.Errorf("body=%q：默认引擎应为 mariadb，实际 %q", body, sel.DBEngine())
		}
		if got, want := sel.ComponentsText(), "nginx / PHP 8.2 / MariaDB 13.0"; got != want {
			t.Errorf("body=%q：ComponentsText=%q，期望 %q（任务标题必须是 MariaDB）", body, got, want)
		}
		if sel.Ports()["mariadb"] != 3306 || sel.Ports()["nginx"] != 80 || sel.Ports()["php@8.2"] != 0 {
			t.Errorf("body=%q：判定端口不对：%v", body, sel.Ports())
		}
	}
}

// TestParseLNMPSelectionExplicitMySQLStillWorks 老客户端要 MySQL 时必须能拿到。
func TestParseLNMPSelectionExplicitMySQLStillWorks(t *testing.T) {
	cases := []string{
		`{"nginx":"nginx","php":"php@8.2","mysql":"mysql@8.4"}`, // 老前端的三字段写法
		`{"mysql":"mysql@8.4"}`,
		`{"db_engine":"mysql"}`,
		`{"db_engine":"mysql","mysql":"mysql@8.4"}`,
	}
	for _, body := range cases {
		sel, err := ParseLNMPSelection([]byte(body))
		if err != nil {
			t.Fatalf("body=%q 应当合法：%v", body, err)
		}
		if sel.MySQL != "mysql@8.4" || sel.DBEngine() != "mysql" {
			t.Errorf("body=%q：应显式选到 mysql@8.4，实际 %+v", body, sel)
		}
		if got, want := strings.Join(sel.Formulas(), ","), "nginx,php@8.2,mysql@8.4"; got != want {
			t.Errorf("body=%q：Formulas=%q，期望 %q", body, got, want)
		}
		if got, want := sel.ComponentsText(), "nginx / PHP 8.2 / MySQL 8.4"; got != want {
			t.Errorf("body=%q：ComponentsText=%q，期望 %q", body, got, want)
		}
	}
}

// TestParseLNMPSelectionRejectsBadEngine 未知/空/矛盾的引擎一律 400（人话）。
func TestParseLNMPSelectionRejectsBadEngine(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"db_engine":"postgres"}`, "postgres"},
		{`{"db_engine":""}`, "空值"},
		{`{"db_engine":"mariadb","mysql":"mysql@8.4"}`, "不一致"},
	}
	for _, c := range cases {
		if _, err := ParseLNMPSelection([]byte(c.body)); err == nil {
			t.Errorf("body=%s 应被拒绝", c.body)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("body=%s 的错误应含 %q，实际：%v", c.body, c.want, err)
		}
	}
}

// TestDBEngineGuardBlocksWhenOtherInstalled（规则②）：
// 另一个引擎已装（**当前没在跑**）→ 拒绝，并给出可直接照做的卸载命令。
func TestDBEngineGuardBlocksWhenOtherInstalled(t *testing.T) {
	cases := []struct {
		target, otherFormula, otherCmd string
	}{
		{"mariadb", "mysql@8.4", "brew services stop mysql@8.4 && brew uninstall mysql@8.4"},
		{"mysql@8.4", "mariadb", "brew services stop mariadb && brew uninstall mariadb"},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			m := guardManager(t)
			m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
				return DBEngineStatus{ProbeOK: true, TargetInstalled: false, OtherInstalled: true}
			})
			got := m.CheckDBEngineConflict(context.Background(), c.target)
			if got.Blocked == "" {
				t.Fatal("另一个引擎已装时必须拒绝（两者只可能装一个）")
			}
			for _, want := range []string{"只能装一个数据库引擎", c.otherFormula, c.otherCmd} {
				if !strings.Contains(got.Blocked, want) {
					t.Errorf("拒绝原因里应含 %q，实际：%s", want, got.Blocked)
				}
			}
		})
	}
}

// TestDBEngineGuardBlocksOtherEngineRunningWithoutBrewRecord（规则③）：
// 端口上跑着对方引擎、但 brew 里没有它的记录 → 同样拒绝并点名占用者。
func TestDBEngineGuardBlocksOtherEngineRunningWithoutBrewRecord(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
		return DBEngineStatus{ProbeOK: true, Holders: []string{"mariadbd (pid 951)"}}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mysql@8.4")
	if got.Blocked == "" {
		t.Fatal("3306 上跑着另一个引擎时必须拒绝")
	}
	for _, want := range []string{"mariadbd (pid 951)", "不会替你停"} {
		if !strings.Contains(got.Blocked, want) {
			t.Errorf("拒绝原因里应含 %q，实际：%s", want, got.Blocked)
		}
	}
}

// TestDBEngineGuardBothInstalledOnlyEffectiveEngineAllowed（规则③）：
// 两个都装着（本机现在的异常状态）→ 只有"当前生效的那个引擎"能重装，
// 另一个入口必须拒绝并指出 3306 上生效的是谁。
func TestDBEngineGuardBothInstalledOnlyEffectiveEngineAllowed(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
		return DBEngineStatus{ProbeOK: true, TargetInstalled: true, OtherInstalled: true,
			Holders: []string{"mariadbd (pid 35985)"}}
	})
	ctx := context.Background()

	if got := m.CheckDBEngineConflict(ctx, "mariadb"); got.Blocked != "" {
		t.Errorf("玛丽亚在 3306 上生效时，装它自己（幂等重装）应当放行，实际：%s", got.Blocked)
	}
	got := m.CheckDBEngineConflict(ctx, "mysql@8.4")
	if got.Blocked == "" {
		t.Fatal("两个都装着、且生效的是 MariaDB 时，装 MySQL 必须拒绝")
	}
	for _, want := range []string{"两个引擎都装着", "mysql@8.4", "mariadb", "3306 上生效的是 MariaDB", "mariadbd (pid 35985)", "<要卸的>"} {
		if !strings.Contains(got.Blocked, want) {
			t.Errorf("拒绝原因里应含 %q，实际：%s", want, got.Blocked)
		}
	}
}

// TestDBEngineGuardBothInstalledNeitherRunning：两个都装、谁都没跑 → 两个入口都拒。
func TestDBEngineGuardBothInstalledNeitherRunning(t *testing.T) {
	for _, target := range []string{"mariadb", "mysql@8.4"} {
		m := guardManager(t)
		m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
			return DBEngineStatus{ProbeOK: true, TargetInstalled: true, OtherInstalled: true}
		})
		got := m.CheckDBEngineConflict(context.Background(), target)
		if got.Blocked == "" {
			t.Fatalf("%s：两个都装着时必须拒绝（只有当前生效的那个能重装）", target)
		}
		if !strings.Contains(got.Blocked, "两个引擎都装着") ||
			!strings.Contains(got.Blocked, "判断不出该保留哪个") {
			t.Errorf("要如实说清判断不出生效引擎，实际：%s", got.Blocked)
		}
	}
}

// TestDBEngineGuardAllowsSameEngineInstalled（规则①）：目标自己已装 → 放行。
func TestDBEngineGuardAllowsSameEngineInstalled(t *testing.T) {
	for _, c := range []struct{ target, holder string }{
		{"mariadb", "mariadbd (pid 700)"},
		{"mysql@8.4", "mysqld (pid 950)"},
	} {
		m := guardManager(t)
		m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
			return DBEngineStatus{ProbeOK: true, TargetInstalled: true, Holders: []string{c.holder}}
		})
		if got := m.CheckDBEngineConflict(context.Background(), c.target); got.Blocked != "" || got.Warning != "" {
			t.Errorf("%s：自己已装且在跑时应当放行，实际 %+v", c.target, got)
		}
		// 自己已装但端口空闲（服务停着）→ 也要放行（幂等重装/启动）。
		m2 := guardManager(t)
		m2.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
			return DBEngineStatus{ProbeOK: true, TargetInstalled: true}
		})
		if got := m2.CheckDBEngineConflict(context.Background(), c.target); got.Blocked != "" {
			t.Errorf("%s：自己已装、服务没跑时也应当允许重装，实际：%s", c.target, got.Blocked)
		}
	}
}

// 端口空闲、什么都没装 → 放行。
func TestDBEngineGuardAllowsWhenClean(t *testing.T) {
	for _, f := range []string{"mariadb", "mysql@8.4"} {
		m := guardManager(t)
		m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
			return DBEngineStatus{ProbeOK: true, DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
		})
		if got := m.CheckDBEngineConflict(context.Background(), f); got.Blocked != "" || got.Warning != "" {
			t.Errorf("%s：干净的机器上不该拦也不该告警，实际 %+v", f, got)
		}
	}
}

// 3306 被无关进程占用 → 拒绝并点名占用者（端口冲突这条不许丢）。
func TestDBEngineGuardBlocksForeignPortHolder(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
		return DBEngineStatus{ProbeOK: true, Holders: []string{"nginx (pid 12)"}}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mariadb")
	if got.Blocked == "" {
		t.Fatal("3306 被别人占着时必须拒绝")
	}
	for _, want := range []string{"nginx (pid 12)", "3306", "不会停别人的进程"} {
		if !strings.Contains(got.Blocked, want) {
			t.Errorf("拒绝原因里应含 %q，实际：%s", want, got.Blocked)
		}
	}
}

// brew 探测失败 + 数据目录空 = 全新机器：必须放行（否则装不上第一个数据库）。
func TestDBEngineGuardProbeFailedAllowsFreshMachine(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
		return DBEngineStatus{ProbeOK: false, DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
	})
	if got := m.CheckDBEngineConflict(context.Background(), "mariadb"); got.Blocked != "" {
		t.Errorf("全新机器（没有 brew、数据目录空）必须能装，实际：%s", got.Blocked)
	}
}

// brew 探测失败 + 数据目录已有数据 = 说不清是谁的 → 拒绝（不猜）。
func TestDBEngineGuardProbeFailedWithDataDirBlocks(t *testing.T) {
	m := guardManager(t)
	datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
	m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
		return DBEngineStatus{ProbeOK: false, DataDir: datadir, DataDirNonEmpty: true}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mariadb")
	if got.Blocked == "" {
		t.Fatal("探测失败而数据目录已有数据时必须拒绝（面板不猜）")
	}
	for _, want := range []string{datadir, "无法复核"} {
		if !strings.Contains(got.Blocked, want) {
			t.Errorf("拒绝原因里应含 %q，实际：%s", want, got.Blocked)
		}
	}
}

// 数据目录非空、但没装任何引擎 → 放行 + 告警点名路径（不是"共存"分支）。
func TestDBEngineGuardWarnsDataDirFromPreviousEngine(t *testing.T) {
	m := guardManager(t)
	datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
	m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
		return DBEngineStatus{ProbeOK: true, DataDir: datadir, DataDirNonEmpty: true}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mariadb")
	if got.Blocked != "" {
		t.Fatalf("没装任何引擎时不该拦：%s", got.Blocked)
	}
	if !strings.Contains(got.Warning, datadir) {
		t.Errorf("告警必须点名数据目录，实际：%s", got.Warning)
	}
}

// 非数据库引擎的 formula 不受护栏影响（护栏只在数据库位上生效）。
func TestCheckDBEngineConflictIgnoresNonEngine(t *testing.T) {
	m := guardManager(t)
	called := false
	m.SetDBEngineProbeForTest(func(_ context.Context, _ mysql.DBEngine) DBEngineStatus {
		called = true
		return DBEngineStatus{}
	})
	if got := m.CheckDBEngineConflict(context.Background(), "nginx"); got.Blocked != "" || got.Warning != "" {
		t.Errorf("nginx 不该被数据库护栏影响，实际 %+v", got)
	}
	if called {
		t.Error("非数据库引擎不该触发探测（白白跑一次 brew + lsof）")
	}
}

// TestMySQLInitCommandIsEngineSpecific 锁住初始化命令按引擎选。
//
// 拿 MySQL 的 --initialize-insecure 去初始化 MariaDB 会直接失败，
// 而且报错只有一句"初始化失败"，用户完全看不出该换命令。
func TestMySQLInitCommandIsEngineSpecific(t *testing.T) {
	prefix := "/opt/homebrew"
	datadir := filepath.Join(prefix, "var", "mysql")

	exe, args := mysqlInitCommand(prefix, "mysql@8.4", datadir)
	if !strings.HasSuffix(exe, "/opt/mysql@8.4/bin/mysqld") {
		t.Errorf("MySQL 的初始化二进制应是 keg 内的 mysqld，实际 %s", exe)
	}
	if len(args) == 0 || args[0] != "--initialize-insecure" {
		t.Errorf("MySQL 应当用 --initialize-insecure，实际 %v", args)
	}

	exe, args = mysqlInitCommand(prefix, "mariadb", datadir)
	if !strings.HasSuffix(exe, "/opt/mariadb/bin/mariadb-install-db") {
		t.Errorf("MariaDB 的初始化二进制应是 mariadb-install-db，实际 %s", exe)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--initialize-insecure") {
		t.Errorf("MariaDB 不认 --initialize-insecure，实际参数：%v", args)
	}
	for _, want := range []string{"--datadir=" + datadir, "--auth-root-authentication-method=normal"} {
		if !strings.Contains(joined, want) {
			t.Errorf("MariaDB 初始化参数缺少 %q：%v", want, args)
		}
	}
}

// TestInstallLNMPPackagesRunsMariaDBSteps 用**假 brew** 证明：
// 选中 MariaDB 时执行的是 `brew install mariadb`，且不会顺带去装 mysql@8.4。
//
// InstallLNMP 本体要求 root（单测到不了那个循环），所以这里直接驱动它抽出来的
// 安装循环；假 brew 只负责"报告未安装"（exit 1），真正的执行走注入的假执行器。
func TestInstallLNMPPackagesRunsMariaDBSteps(t *testing.T) {
	m := guardManager(t)
	fakeBrew := filepath.Join(m.brewPrefix(), "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(fakeBrew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeBrew, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.opt.BrewBin = fakeBrew
	// 镜像探测会发真实网络请求，单测一律注入。
	m.mirrorProbeOverride = func(context.Context, string) (string, string) { return "", "" }
	var ran []string
	m.brewSourceRunOverride = func(_ context.Context, _ time.Duration, _ brewInstallSource, args ...string) (string, error) {
		ran = append(ran, strings.Join(args, " "))
		return "", nil
	}

	res := &InstallResult{App: "lnmp"}
	if err := m.installLNMPPackages(context.Background(), res,
		[]string{"nginx", "php@8.2", "mariadb"}); err != nil {
		t.Fatalf("假 brew 应当成功：%v", err)
	}
	joined := strings.Join(ran, " | ")
	if !strings.Contains(joined, "install mariadb") {
		t.Errorf("必须执行 brew install mariadb，实际：%s", joined)
	}
	if strings.Contains(joined, "mysql@8.4") {
		t.Errorf("选了 MariaDB 就不该装 mysql@8.4（会抢数据目录与 3306），实际：%s", joined)
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "brew install mariadb") {
		t.Errorf("任务步骤必须如实写出装的是 mariadb，实际：%s", steps)
	}
}

// TestUninstallPlanNamesSharedDataDirButNeverDeletesIt：
// 卸载 MariaDB（或 MySQL）时数据目录必须**点名保留**，绝不能进 DataPaths
// （那个勾选项会把它删掉＝删掉用户全部的库）。
func TestUninstallPlanNamesSharedDataDirButNeverDeletesIt(t *testing.T) {
	m := guardManager(t)
	datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
	for _, id := range []string{"mariadb", "mysql84"} {
		app, ok := FindApp(id)
		if !ok {
			t.Fatalf("目录里应有 %s", id)
		}
		plan := m.PlanUninstallForBrew(context.Background(), app, nil,
			BrewState{Formula: app.BrewFormula, Installed: true})
		if plan.Kind != "brew" {
			t.Fatalf("%s：installed=true 必须给得出卸载路径，实际 %q（%s）", id, plan.Kind, plan.Blocked)
		}
		if !hasStep(plan.Steps, "brew uninstall "+app.BrewFormula) {
			t.Errorf("%s：计划里没有 brew uninstall 这一步：%v", id, plan.Steps)
		}
		if !hasStepContaining(plan.Steps, datadir) {
			t.Errorf("%s：卸载计划必须点名数据目录 %s，实际 %v", id, datadir, plan.Steps)
		}
		for _, p := range plan.DataPaths {
			if p == datadir {
				t.Errorf("%s：数据目录绝不能进 DataPaths（勾选即删库）", id)
			}
		}
	}
}
