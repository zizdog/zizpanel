package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  数据库引擎维度（MySQL 8.4 / MariaDB）的门禁
//
//  三类东西：
//    ① ParseLNMPSelection 的引擎维度：老请求逐字不变、mariadb 装 mariadb、
//       未知引擎拒绝；
//    ② 互斥护栏：注入"另一个引擎在跑/已装/端口被占"等现场，断言拒绝或告警，
//       并锁住"绝不会为了装 A 去停 B"这条产品底线；
//    ③ 初始化命令按引擎选（MariaDB 不认 --initialize-insecure）。
//
//  护栏探测**全部注入**：默认实现会跑真实 brew 与 lsof 3306，而本机 MySQL 正在跑，
//  结论会随机器漂（违反"单测不许碰真实服务"）。
// ============================================================================

// guardManager 造一个完全离线的 Manager，并注入引擎探测。
func guardManager(t *testing.T) *Manager {
	t.Helper()
	m, _ := sandboxManager(t)
	prefix := t.TempDir()
	m.opt.BrewBin = filepath.Join(prefix, "bin", "brew")
	m.SetLaunchdDirsForTest([]string{filepath.Join(prefix, "LaunchDaemons")})
	return m
}

// TestParseLNMPSelectionDefaultIsByteIdentical 锁住"老请求逐字不变"。
//
// 老前端不带任何字段（或只带老三个字段），生成物必须与加引擎维度之前完全一样：
// formula 列表、判定端口、任务标题文案三样都锁。
func TestParseLNMPSelectionDefaultIsByteIdentical(t *testing.T) {
	for _, body := range []string{"", "{}", "null",
		`{"nginx":"nginx","php":"php@8.2","mysql":"mysql@8.4"}`,
		`{"php":"php@8.2"}`} {
		sel, err := ParseLNMPSelection([]byte(body))
		if err != nil {
			t.Fatalf("body=%q 应当合法（向后兼容），实际：%v", body, err)
		}
		if got, want := strings.Join(sel.Formulas(), ","), "nginx,php@8.2,mysql@8.4"; got != want {
			t.Errorf("body=%q：Formulas=%q，期望 %q（老行为不许变）", body, got, want)
		}
		if sel.DBEngine() != "mysql" {
			t.Errorf("body=%q：默认引擎应为 mysql，实际 %q", body, sel.DBEngine())
		}
		if got, want := sel.ComponentsText(), "nginx / PHP 8.2 / MySQL 8.4"; got != want {
			t.Errorf("body=%q：ComponentsText=%q，期望 %q（任务标题不许变）", body, got, want)
		}
		ports := sel.Ports()
		if ports["mysql@8.4"] != 3306 || ports["nginx"] != 80 || ports["php@8.2"] != 0 {
			t.Errorf("body=%q：判定端口变了：%v", body, ports)
		}
	}
}

// TestParseLNMPSelectionDatabaseEngine 锁住引擎维度的两条映射。
func TestParseLNMPSelectionDatabaseEngine(t *testing.T) {
	sel, err := ParseLNMPSelection([]byte(`{"db_engine":"mariadb"}`))
	if err != nil {
		t.Fatalf("db_engine=mariadb 应合法：%v", err)
	}
	if sel.MySQL != "mariadb" || sel.DBEngine() != "mariadb" {
		t.Fatalf("应选到 mariadb，实际 %+v", sel)
	}
	if got, want := strings.Join(sel.Formulas(), ","), "nginx,php@8.2,mariadb"; got != want {
		t.Errorf("Formulas=%q，期望 %q（不许混进 mysql@8.4）", got, want)
	}
	if strings.Contains(strings.Join(sel.Formulas(), ","), "mysql@8.4") {
		t.Error("选了 MariaDB 就不该再装 mysql@8.4（两个会抢数据目录与 3306）")
	}
	if sel.Ports()["mariadb"] != 3306 {
		t.Errorf("mariadb 的判定端口应为 3306，实际 %v", sel.Ports())
	}
	if got, want := sel.ComponentsText(), "nginx / PHP 8.2 / MariaDB 13.0"; got != want {
		t.Errorf("ComponentsText=%q，期望 %q（标题里不许写 MySQL）", got, want)
	}

	// 显式两种写法一致时也合法
	if _, err := ParseLNMPSelection([]byte(`{"db_engine":"mariadb","mysql":"mariadb"}`)); err != nil {
		t.Errorf("db_engine 与 mysql 一致时应当合法：%v", err)
	}
	if _, err := ParseLNMPSelection([]byte(`{"db_engine":"mysql"}`)); err != nil {
		t.Errorf("db_engine=mysql 应当合法：%v", err)
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

// TestCheckDBEngineConflictBlocksOtherRunning 是护栏的**核心**：对方在跑就拒绝，
// 且原因必须点名数据目录、端口与"面板不会替你停它"。
func TestCheckDBEngineConflictBlocksOtherRunning(t *testing.T) {
	cases := []struct {
		name       string
		installing string
		holders    []string
	}{
		{"装 MariaDB 时 MySQL 在跑", "mariadb", []string{"mysqld (pid 950)"}},
		{"装 MySQL 时 MariaDB 在跑", "mysql@8.4", []string{"mariadbd (pid 951)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := guardManager(t)
			m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
				return DBEngineProbeResult{ProbeOK: true, Holders: c.holders}
			})
			got := m.CheckDBEngineConflict(context.Background(), c.installing)
			if got.Blocked == "" {
				t.Fatal("对方正在跑时必须拒绝（否则新引擎会去开另一个引擎的数据目录）")
			}
			datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
			for _, want := range []string{datadir, "3306", c.holders[0], "不会替你停"} {
				if !strings.Contains(got.Blocked, want) {
					t.Errorf("拒绝原因里应含 %q，实际：%s", want, got.Blocked)
				}
			}
			if got.Warning != "" {
				t.Errorf("拒绝时不该同时给告警：%q", got.Warning)
			}
		})
	}
}

// 两个都没装、端口空闲 → 都能选。
func TestCheckDBEngineConflictAllowsWhenClean(t *testing.T) {
	for _, f := range []string{"mariadb", "mysql@8.4"} {
		m := guardManager(t)
		m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
			return DBEngineProbeResult{ProbeOK: true,
				DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
		})
		if got := m.CheckDBEngineConflict(context.Background(), f); got.Blocked != "" || got.Warning != "" {
			t.Errorf("%s：干净的机器上不该拦也不该告警，实际 %+v", f, got)
		}
	}
}

// 对方"装了但没跑" → 放行 + 告警（必须点名共用数据目录）。
func TestCheckDBEngineConflictWarnsWhenOtherInstalledIdle(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
		return DBEngineProbeResult{Installed: true, ProbeOK: true,
			DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mariadb")
	if got.Blocked != "" {
		t.Fatalf("对方只是装了没跑，不该拒绝：%s", got.Blocked)
	}
	if !strings.Contains(got.Warning, filepath.Join(m.brewPrefix(), "var", "mysql")) {
		t.Errorf("告警必须点名共用数据目录，实际：%s", got.Warning)
	}
	if !strings.Contains(got.Warning, "mysql@8.4") {
		t.Errorf("告警必须点名是哪个引擎，实际：%s", got.Warning)
	}
}

// 3306 被别的东西占着 → 拒绝并点名占用者（装起来也绑不上）。
func TestCheckDBEngineConflictBlocksForeignPortHolder(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
		return DBEngineProbeResult{ProbeOK: true, Holders: []string{"nginx (pid 12)"}}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mariadb")
	if got.Blocked == "" {
		t.Fatal("3306 被别人占着时必须拒绝")
	}
	if !strings.Contains(got.Blocked, "nginx (pid 12)") {
		t.Errorf("要点名占用者，实际：%s", got.Blocked)
	}
}

// 重跑同一个引擎（它自己在跑）→ 不是冲突，放行且不告警。
func TestCheckDBEngineConflictAllowsSameEngineRunning(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
		return DBEngineProbeResult{ProbeOK: true, Holders: []string{"mariadbd (pid 700)"}}
	})
	if got := m.CheckDBEngineConflict(context.Background(), "mariadb"); got.Blocked != "" || got.Warning != "" {
		t.Errorf("MariaDB 自己在跑时重跑应当放行，实际 %+v", got)
	}
}

// brew 探测失败 = 未复核（不是"什么都没装"）：放行但必须说出来。
func TestCheckDBEngineConflictWarnsWhenProbeFailed(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
		return DBEngineProbeResult{ProbeOK: false,
			DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mariadb")
	if got.Blocked != "" {
		t.Fatalf("探测失败不能当成冲突：%s", got.Blocked)
	}
	if !strings.Contains(got.Warning, "复核") {
		t.Errorf("必须如实说'没能复核'，实际：%s", got.Warning)
	}
}

// 数据目录已有数据（可能是另一个引擎留下的）→ 放行 + 告警点名路径。
func TestCheckDBEngineConflictWarnsWhenDataDirNotEmpty(t *testing.T) {
	m := guardManager(t)
	m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
		return DBEngineProbeResult{ProbeOK: true, DataDirNonEmpty: true,
			DataDir: filepath.Join(m.brewPrefix(), "var", "mysql")}
	})
	got := m.CheckDBEngineConflict(context.Background(), "mariadb")
	if !strings.Contains(got.Warning, filepath.Join(m.brewPrefix(), "var", "mysql")) {
		t.Errorf("告警必须点名数据目录，实际：%s", got.Warning)
	}
}

// 非数据库引擎的 formula 不受护栏影响（护栏只在数据库位上生效）。
func TestCheckDBEngineConflictIgnoresNonEngine(t *testing.T) {
	m := guardManager(t)
	called := false
	m.SetDBEngineProbeForTest(func(context.Context, string) DBEngineProbeResult {
		called = true
		return DBEngineProbeResult{}
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
