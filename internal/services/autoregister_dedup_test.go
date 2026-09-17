package services

// autoregister_dedup_test.go —— AutoRegisterKnown 的「同 label 家族不重复登记」回归锁。
//
// 真机现象（2026-09-17 用户反馈）：同一个 php-fpm 在「服务管理」里出现两条 ——
// `php82` 与 `homebrew-mxcl-php8-2`。两处的写法互为别名：
// `homebrew.mxcl.` / `sh.brew.` 是两套前缀，`.` 与 `-` 只是归一化差异。
// 前端会做展示去重，但源头也要修：登记前若已存在同家族记录就跳过。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLabelFamilyTreatsAliasesAsOne 先锁住归一化本身。
func TestLabelFamilyTreatsAliasesAsOne(t *testing.T) {
	same := [][]string{
		{"homebrew.mxcl.php@8.2", "sh.brew.php@8.2", "homebrew-mxcl-php8-2", "sh-brew-php8-2"},
		{"homebrew.mxcl.nginx", "sh.brew.nginx", "homebrew-mxcl-nginx"},
	}
	for _, group := range same {
		want := labelFamily(group[0])
		if want == "" {
			t.Fatalf("%q 的家族键不该为空", group[0])
		}
		for _, l := range group[1:] {
			if got := labelFamily(l); got != want {
				t.Errorf("别名应归到同一家族：%q → %q，但 %q → %q", group[0], want, l, got)
			}
		}
	}
	// 不同软件 / 不同版本不能混为一谈
	if labelFamily("homebrew.mxcl.php@8.2") == labelFamily("homebrew.mxcl.php@8.4") {
		t.Error("php@8.2 与 php@8.4 是两个不同的服务，不能归为同一家族")
	}
	if labelFamily("homebrew.mxcl.nginx") == labelFamily("homebrew.mxcl.mysql") {
		t.Error("nginx 与 mysql 不能归为同一家族")
	}
	if labelFamily("") != "" {
		t.Error("空标签的家族键必须是空串")
	}
}

// TestAutoRegisterKnownSkipsSameLabelFamily 是核心回归：
// 已有同家族记录（无论点号形态还是归一化形态）时，再跑自动登记不能造第二条。
func TestAutoRegisterKnownSkipsSameLabelFamily(t *testing.T) {
	cases := []struct {
		name  string
		rec   string
		label string
	}{
		{"记录是目录 ID、label 是点号形态", "php82", "homebrew.mxcl.php@8.2"},
		{"记录是归一化名字、label 是另一套前缀", "homebrew-mxcl-php8-2", "sh.brew.php@8.2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, repo := sandboxIdempotentManager(t)
			ctx := context.Background()

			// 目标目录条目（php82）的 plist 放在沙箱家目录里，让"服务确实存在"成立。
			agents := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents")
			if err := os.MkdirAll(agents, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agents, "homebrew.mxcl.php@8.2.plist"),
				[]byte("<plist/>"), 0o644); err != nil {
				t.Fatal(err)
			}

			// 已有一条同家族记录。
			if err := repo.Create(ctx, &Service{
				Name: c.rec, DisplayName: "PHP 8.2", Kind: KindNative,
				LaunchLabel: c.label, Managed: true,
			}); err != nil {
				t.Fatal(err)
			}

			m.AutoRegisterKnown(ctx)

			// 只数 php@8.2 这一家族：AutoRegisterKnown 还会扫到本机真实存在的
			// 其它服务（测试机可能真装着 nginx/mysql），不能对总数做断言。
			list, err := repo.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			for _, s := range list {
				if labelFamily(s.LaunchLabel) == labelFamily("homebrew.mxcl.php@8.2") {
					got++
				}
			}
			if got != 1 {
				t.Errorf("php@8.2 家族应恰好 1 条记录，实际 %d 条（%+v）—— 这就是"+
					"用户看到的 php82 与 homebrew-mxcl-php8-2 各一条", got, list)
			}
		})
	}
}

// TestAutoRegisterKnownRegistersWhenNoRecordExists 是反面对照：
// 家族里一条记录都没有时必须照常登记（证明上面的跳过不是因为"永远不登记"）。
func TestAutoRegisterKnownRegistersWhenNoRecordExists(t *testing.T) {
	m, repo := sandboxIdempotentManager(t)
	ctx := context.Background()

	agents := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "homebrew.mxcl.php@8.2.plist"),
		[]byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.AutoRegisterKnown(ctx)

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, s := range list {
		if labelFamily(s.LaunchLabel) == labelFamily("homebrew.mxcl.php@8.2") {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("没有记录时应登记出恰好 1 条 php@8.2，实际 %d 条", got)
	}
}
