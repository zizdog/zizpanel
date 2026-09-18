package backup

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  清单覆盖门禁
//
//  目的：**代码新增了数据文件，却没加进备份清单**这件事必须变红。
//
//  做法：扫描 internal/ 与 cmd/ 下所有非测试 Go 源码，收集
//  `filepath.Join(<X>Dir, "<名字>")` 形式的一级名字，要求每个名字要么
//  被备份覆盖（Cover 名单 / Plan 真的产出了对应条目），要么在排除名单里
//  且写了原因。新增一个 <DataDir>/foo.json 而两边都没登记 → 这个测试失败。
//
//  这是启发式门禁（只看字面量），但足以拦住"顺手加了个数据文件忘了备份"
//  这类真实缺陷 —— 备份功能最怕的正是"以为备了，其实没有"。
// ============================================================================

// 注意用 [ \t]* 而不是 \s*：\s 会跨行，导致
//
//	"data_dir": s.Cfg.DataDir,
//	"user_count": count,
//
// 这种相邻的 map 字面量被误判成 <DataDir>/user_count。
var reDirLiteral = regexp.MustCompile(`\.(DataDir|WorkDir)[ \t]*,[ \t]*"([^"]+)"`)

func repoRoot(t *testing.T) string {
	t.Helper()
	// 测试工作目录是 internal/backup
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	return root
}

func TestBackupPlanCoversAllDataPaths(t *testing.T) {
	root := repoRoot(t)
	dataCovered := map[string]bool{}
	for _, n := range DataCoveredNames() {
		dataCovered[n] = true
	}
	dataExcluded := DataExcludedNames()
	workCovered := map[string]bool{}
	for _, n := range WorkCoveredNames() {
		workCovered[n] = true
	}
	workExcluded := WorkExcludedNames()

	var problems []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "cmd/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range reDirLiteral.FindAllStringSubmatch(string(b), -1) {
			kind, name := m[1], m[2]
			switch kind {
			case "DataDir":
				if !dataCovered[name] {
					if reason, ok := dataExcluded[name]; ok {
						if strings.TrimSpace(reason) == "" {
							problems = append(problems, rel+": 排除 "+name+" 但没写原因")
						}
						continue
					}
					problems = append(problems,
						rel+": <DataDir>/"+name+" 既不在备份清单里也没有排除说明")
				}
			case "WorkDir":
				if !workCovered[name] {
					if reason, ok := workExcluded[name]; ok {
						if strings.TrimSpace(reason) == "" {
							problems = append(problems, rel+": 排除 "+name+" 但没写原因")
						}
						continue
					}
					problems = append(problems,
						rel+": <WorkDir>/"+name+" 既不在备份清单里也没有排除说明")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("备份清单覆盖门禁失败：代码里有数据路径没被登记。\n"+
			"要么把它加进 backup.Plan（并更新 DataCoveredNames/WorkCoveredNames），\n"+
			"要么加进排除名单并写清原因：\n  - %s", strings.Join(problems, "\n  - "))
	}
}

// TestPlanSelfConsistency：Plan 真正产出的 data/apps 一级名字必须与覆盖名单一致，
// 避免"名单更新了、Plan 没更新"或反过来的漂移。
func TestPlanSelfConsistency(t *testing.T) {
	plan := Plan(PlanOptions{
		DataDir: "/data", WorkDir: "/work", BrewPrefix: "/brew", UserHome: "/home",
	})
	covered := map[string]bool{}
	for _, n := range DataCoveredNames() {
		covered[n] = true
	}
	for _, it := range plan {
		p := filepath.ToSlash(it.ArchivePath)
		if strings.HasPrefix(p, "data/") {
			top := strings.Split(strings.TrimPrefix(p, "data/"), "/")[0]
			if !covered[top] {
				t.Fatalf("Plan 产出了 data/%s，但 DataCoveredNames() 里没有它", top)
			}
		}
	}
	// 排除名单与覆盖名单不能重叠（重叠意味着自相矛盾）
	for _, n := range DataCoveredNames() {
		if _, bad := DataExcludedNames()[n]; bad {
			t.Fatalf("%s 同时出现在覆盖与排除名单里", n)
		}
	}
	for _, n := range WorkCoveredNames() {
		if _, bad := WorkExcludedNames()[n]; bad {
			t.Fatalf("%s 同时出现在覆盖与排除名单里", n)
		}
	}
}

// TestAppsTargetsFollowRegistry：release-binary 应用的含密配置项必须从注册表派生。
// 新加一个这类应用时它会自动出现在可勾选目标里（不会悄悄漏掉它的 token 配置）。
func TestAppsTargetsFollowRegistry(t *testing.T) {
	reg := services.ReleaseBinaryAppConfigRelPaths()
	targets := map[string]bool{}
	for _, tt := range AllTargets() {
		targets[tt] = true
	}
	if !targets[TargetCompose] {
		t.Fatal("Docker Compose 配置必须是可勾选目标")
	}
	for id := range reg {
		if !targets["apps:"+id] {
			t.Fatalf("应用 %s 有配置文件但不在备份可勾选目标里（apps:%s）", id, id)
		}
	}
	plan := Plan(PlanOptions{DataDir: "/d", WorkDir: "/w", BrewPrefix: "/b", UserHome: "/h"})
	for id, rel := range reg {
		want := "apps/" + id + "/" + filepath.Base(rel)
		found := false
		for _, it := range plan {
			if it.ArchivePath == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("Plan 里缺少应用 %s 的配置项 %s", id, want)
		}
	}
}

// TestPlanHasNoHardcodedSystemPaths：备份清单里绝不能出现写死的系统前缀。
//
// 既有实现的缺陷就是 /opt/zizpanel、/opt/homebrew 写死 ——
// ZIZPANEL_ROOT 重定位或 Intel 前缀（/usr/local）下会备错东西。
func TestPlanHasNoHardcodedSystemPaths(t *testing.T) {
	plan := Plan(PlanOptions{DataDir: "/d", WorkDir: "/w", BrewPrefix: "/b", UserHome: "/h"})
	if len(plan) == 0 {
		t.Fatal("Plan 不应为空")
	}
	for _, it := range plan {
		for _, bad := range []string{"/opt/zizpanel", "/opt/homebrew"} {
			if strings.Contains(it.SourcePath, bad) {
				t.Fatalf("条目 %s 的来源路径写死了系统前缀: %s", it.ArchivePath, it.SourcePath)
			}
		}
	}
}
