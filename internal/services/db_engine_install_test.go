package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  "只可能装一个数据库引擎"的 Install 级门禁（用户 2026-09-20 定的产品规则）
//
//  真机 2026-09-20 的教训：MariaDB 在跑时点 mysql84 的「安装」得到 202 + succeeded
//  （服务层幂等短路抢在护栏之前），steps 里还写着"3306 由它自己占用"——谎报成功。
//  所以这里从 Install() 入口开始验，而不是只验护栏函数：
//    ① 已安装 + 另一个引擎装着（在跑）→ 拒绝，且一条命令都不执行；
//    ② 已安装 + 就是它自己在跑 → 照旧幂等成功（别把正常重跑拒了）；
//    ③ 端口被无关进程占用 → 拒绝并点名占用者；
//    ④ 幂等提示在"占用者不是自己"时**不许**说"正常状态"。
// ============================================================================

// installGuardManager 造一个离线的 Manager：假 brew（一被调用就记一笔）+ 注入探测。
func installGuardManager(t *testing.T, installed map[string]string,
	holders []string) (*Manager, *Repository, string) {
	t.Helper()
	m, repo := sandboxIdempotentManager(t)
	marker := filepath.Join(t.TempDir(), "brew-called")
	m.opt.BrewBin = writeFailingFakeBrew(t, marker)
	m.SetBrewInstalledProbeForTest(func(context.Context) (map[string]string, bool) {
		return installed, true
	})
	port := 3306
	m.portCheckOverride = func(p int) (bool, []string, error) {
		if p == port && len(holders) > 0 {
			return true, holders, nil
		}
		return false, nil, nil
	}
	return m, repo, marker
}

// 门禁①：mysql84 有记录（已安装）+ brew 里 MariaDB 也在、3306 上跑着 mariadbd
// → Install 必须拒绝，且不执行任何命令（真机的"谎报成功"就是这么来的）。
func TestInstallRefusesOtherEngineEvenWhenTargetAlreadyInstalled(t *testing.T) {
	m, repo, marker := installGuardManager(t,
		map[string]string{"mysql@8.4": "8.4.11_4", "mariadb": "13.0.2"},
		[]string{"mariadbd (pid 35985)"})
	ctx := context.Background()
	if err := repo.Create(ctx, &Service{
		Name: "mysql84", DisplayName: "MySQL 8.4", Kind: KindNative,
		LaunchLabel: "sh.brew.mysql@8.4", Port: 3306, Managed: true,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := m.Install(ctx, "mysql84")
	if err == nil {
		t.Fatal("两个引擎都装着时必须拒绝（幂等短路不许抢在护栏之前）")
	}
	for _, want := range []string{"两个引擎都装着", "3306 上生效的是 MariaDB", "mariadbd (pid 35985)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("拒绝原因里应含 %q，实际：%v", want, err)
		}
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("拒绝时不该执行任何 brew 命令，实际调用记录存在：%s", marker)
	}
	if list, _ := repo.List(ctx); len(list) != 1 {
		t.Errorf("拒绝时不该改注册表，实际 %d 条", len(list))
	}
}

// 门禁②：MariaDB 有记录 + 3306 上就是 mariadbd → 幂等成功（正常重跑不许被拒）。
func TestInstallStillSucceedsWhenOwnEngineIsRunning(t *testing.T) {
	m, repo, marker := installGuardManager(t,
		map[string]string{"mariadb": "13.0.2"},
		[]string{"mariadbd (pid 35985)"})
	ctx := context.Background()
	if err := repo.Create(ctx, &Service{
		Name: "mariadb", DisplayName: "MariaDB 13.0", Kind: KindNative,
		LaunchLabel: "sh.brew.mariadb", Port: 3306, Managed: true,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := m.Install(ctx, "mariadb")
	if err != nil {
		t.Fatalf("自己已装且在跑时必须幂等成功，实际：%v", err)
	}
	if !strings.Contains(res.Message, "已经装过") || !strings.Contains(res.Message, "跳过") {
		t.Errorf("终态说明要写明已安装/跳过，实际 %q", res.Message)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "由它自己占用") {
		t.Errorf("真实监听者就是它自己，步骤里应当如实这么写：\n%s", joined)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("幂等跳过我执行了 brew 命令：%s", marker)
	}
}

// 门禁③：3306 被无关进程占用 → 拒绝并点名占用者（这条不许因为新规则丢掉）。
func TestInstallRefusesUnrelatedPortHolder(t *testing.T) {
	m, _, _ := installGuardManager(t, map[string]string{}, []string{"node (pid 777)"})
	_, err := m.Install(context.Background(), "mariadb")
	if err == nil {
		t.Fatal("端口被无关进程占用必须拒绝")
	}
	for _, want := range []string{"node (pid 777)", "3306", "不会停别人的进程"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("拒绝原因里应含 %q，实际：%v", want, err)
		}
	}
}

// 门禁④：幂等提示的端口那一步必须基于**真实监听者**，不许再出现假话。
func TestAlreadyInstalledPortStepNeverClaimsSelfWithoutEvidence(t *testing.T) {
	cases := []struct {
		name, appID string
		holders     []string
		portErr     error
		wantSub     string
	}{
		{"MySQL 的端口上其实是另一个引擎", "mysql84", []string{"mariadbd (pid 35985)"}, nil,
			"另一个数据库引擎"},
		{"端口被无关进程占用", "nginx", []string{"node (pid 9)"}, nil, "未复核"},
		{"就是它自己（引擎进程名）", "mariadb", []string{"mariadbd (pid 1)"}, nil, "由它自己占用"},
		{"读不到端口占用", "nginx", nil, errors.New("lsof 执行失败"), "未复核"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := sandboxIdempotentManager(t)
			app, ok := FindApp(c.appID)
			if !ok {
				t.Fatalf("目录里没有 %s", c.appID)
			}
			m.portCheckOverride = func(p int) (bool, []string, error) {
				if p != app.Port {
					return false, nil, nil
				}
				if c.portErr != nil {
					return false, nil, c.portErr
				}
				return true, c.holders, nil
			}
			rec := &Service{
				Name: app.ID, DisplayName: app.Name, Kind: app.Kind,
				LaunchLabel: app.ServiceLabel, Port: app.Port, Managed: true,
			}
			res := m.alreadyInstalledResult(context.Background(), app, rec)
			joined := strings.Join(res.Steps, "\n")
			if !strings.Contains(joined, c.wantSub) {
				t.Errorf("步骤里应含 %q，实际：\n%s", c.wantSub, joined)
			}
			if c.wantSub != "由它自己占用" && strings.Contains(joined, "正常状态") {
				t.Errorf("占用者不是它自己时绝不许说'正常状态'：\n%s", joined)
			}
		})
	}
}
