package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 「brew 目录缺失」这一类故障的门禁（坑 178）：识别 + 具体建议 + 回读失败不谎报。
// 全部用临时前缀里的**假 brew**，不碰 /opt/homebrew、不跑真 brew。
// 真机（生产机）上用户看到的那条 stderr。开头/结尾都可能被上游改写，
// 但 `dir_initialize` 与路径这两段是判据。
const realMissingCaskroomStderr = "Error: No such file or directory @ dir_initialize - /opt/homebrew/Caskroom"

func TestDiagnoseBrewDirErrorClassifiesMissingBrewDir(t *testing.T) {
	cases := []struct {
		name    string
		stderr  string
		wantDir string
		wantOK  bool
	}{
		{
			name:    "生产机原话（Caskroom）",
			stderr:  realMissingCaskroomStderr,
			wantDir: "/opt/homebrew/Caskroom",
			wantOK:  true,
		},
		{
			name:    "Cellar 缺失",
			stderr:  "Error: No such file or directory @ dir_initialize - /opt/homebrew/Cellar",
			wantDir: "/opt/homebrew/Cellar",
			wantOK:  true,
		},
		{
			name:    "Intel 机器的 /usr/local",
			stderr:  "Error: No such file or directory @ dir_initialize - /usr/local/Caskroom",
			wantDir: "/usr/local/Caskroom",
			wantOK:  true,
		},
		{
			name:    "多行输出里夹着真实报错（真机常态）",
			stderr:  "Warning: /opt/homebrew/bin is not in your PATH\nError: No such file or directory @ dir_initialize - /opt/homebrew/Caskroom\n",
			wantDir: "/opt/homebrew/Caskroom",
			wantOK:  true,
		},
		{
			name:   "别的 brew 故障（formula 不存在）不该被当成目录缺失",
			stderr: "Error: No available formula with the name \"nope\".",
			wantOK: false,
		},
		{
			name:   "校验失败不该被当成目录缺失",
			stderr: "Error: Bottle reports different checksum: a5dd\nSHA-256 checksum of downloaded file: e3b0",
			wantOK: false,
		},
		{
			name:   "No such file 但路径不在 Homebrew 前缀下",
			stderr: "Error: No such file or directory @ dir_initialize - /Users/someone/work/foo",
			wantOK: false,
		},
		{
			name:   "只是前缀名很像（边界判断）",
			stderr: "Error: No such file or directory @ dir_initialize - /opt/homebrew-other/Caskroom",
			wantOK: false,
		},
		{
			name:   "超时文案不是目录缺失",
			stderr: "`/opt/homebrew/bin/brew list --versions` 超过 30 秒没有返回（超时）",
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, okd := DiagnoseBrewDirError(c.stderr)
			if okd != c.wantOK {
				t.Fatalf("识别结果 = %v，期望 %v（stderr=%q）", okd, c.wantOK, c.stderr)
			}
			if !c.wantOK {
				return
			}
			if got.MissingDir != c.wantDir {
				t.Errorf("MissingDir = %q，期望 %q", got.MissingDir, c.wantDir)
			}
			if got.Prefix == "" || !strings.HasPrefix(got.MissingDir, got.Prefix+"/") {
				t.Errorf("Prefix=%q 与 MissingDir=%q 不自洽", got.Prefix, got.MissingDir)
			}
		})
	}
}

// TestBrewDirAdviceIsActionable 锁住建议的"可操作性"：
// 缺哪个目录、用什么命令补、归属交给谁，三样都要有；而且**不许猜**属主。
func TestBrewDirAdviceIsActionable(t *testing.T) {
	d, okd := DiagnoseBrewDirError(realMissingCaskroomStderr)
	if !okd {
		t.Fatal("生产机原话必须被识别为「目录缺失」")
	}
	own := BrewDirOwner{Known: true, Name: "zizdog", Group: "admin", UID: 501, GID: 20}
	advice := FormatBrewDirAdvice(d, own)
	for _, want := range []string{
		"/opt/homebrew/Caskroom",                                    // 缺哪个目录
		"sudo install -d -o zizdog -g admin /opt/homebrew/Caskroom", // 该用什么命令补
		"zizdog:admin",         // 归属交给谁
		"homebrew",             // 说明为什么（brew 前缀）
		"未能复核",                 // 说清"不是没装"
		"brew list --versions", // 复核方式
	} {
		if !strings.Contains(advice, want) {
			t.Errorf("建议里缺少 %q：\n%s", want, advice)
		}
	}
	// 应用市场那一条"原因："会被截断到 140 字符（apps.js），
	// 所以第一句必须自带"缺哪个目录 + 怎么补"。
	first := strings.SplitN(advice, "\n", 2)[0]
	if n := utf8.RuneCountInString(first); n > 140 {
		t.Errorf("建议第一句 %d 字符（>140），在应用市场会被截断到看不见关键命令：\n%s", n, first)
	}
	if !strings.Contains(first, "/opt/homebrew/Caskroom") || !strings.Contains(first, "install -d") {
		t.Errorf("建议第一句必须同时给出缺失目录与补建命令：\n%s", first)
	}

	// 读不到属主时**不许编一个用户名**，只给"先看属主"的指引。
	unknown := FormatBrewDirAdvice(d, BrewDirOwner{})
	if strings.Contains(unknown, "zizdog") {
		t.Error("读不到属主时不得编造用户名")
	}
	if !strings.Contains(unknown, "ls -ld /opt/homebrew") {
		t.Errorf("读不到属主时应指引用户去看真实属主：\n%s", unknown)
	}
}

// 不存在的 brew 二进制：连前缀都定不下来，必须明确报"先装 Homebrew"，
// 而不是凭空在别处建目录。
func TestPlanBrewDirRepairWithoutBrewBinaryFailsHonestly(t *testing.T) {
	m := &Manager{opt: Options{BrewBin: filepath.Join(t.TempDir(), "nope", "brew"), UserName: ""}}
	if _, err := m.PlanBrewDirRepair(context.Background()); err == nil {
		t.Fatal("找不到 brew 时探测必须报错（不许猜前缀）")
	}
}

// 点名了别的前缀下的目录时：只能如实"跳过"，绝不建到那个前缀下面。
func TestPlanBrewDirRepairSkipsDirsOutsidePrefix(t *testing.T) {
	prefix := t.TempDir()
	brew := writeBrewDirsFakeBrew(t, prefix, fmt.Sprintf("case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\nesac\nexit 0\n", prefix))
	m := &Manager{opt: Options{BrewBin: brew, UserHome: t.TempDir(), UserName: ""}}

	plan, err := m.PlanBrewDirRepair(context.Background(), "/usr/local/Caskroom")
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0] != "/usr/local/Caskroom" {
		t.Errorf("不属于当前前缀的目录必须如实进 Skipped，实际 %+v", plan.Skipped)
	}
	for _, d := range plan.Missing {
		if !strings.HasPrefix(d, prefix+"/") {
			t.Errorf("缺失清单里出现了不属于 %s 的路径：%s", prefix, d)
		}
		if d == "/usr/local/Caskroom" {
			t.Errorf("别的前缀下的目录绝不能被列进待补建清单：%s", d)
		}
	}
	if _, serr := os.Stat("/usr/local/Caskroom"); serr == nil {
		t.Error("不该在别的前缀下创建任何东西")
	}
}

// TestInstalledFormulaVersionsErrExplainsMissingBrewDir 是"如实且可操作"的端到端门禁：
// 用**假 brew**（在临时前缀下，绝不碰 /opt/homebrew）复现真机 stderr，
// 断言失败消息里既有真实 stderr，也有具体修复建议。
func TestInstalledFormulaVersionsErrExplainsMissingBrewDir(t *testing.T) {
	prefix := shortPrefix(t)
	brew := writeBrewDirsFakeBrew(t, prefix, fmt.Sprintf(
		"case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\n  list) echo %q >&2; exit 1 ;;\nesac\nexit 0\n",
		prefix, "Error: No such file or directory @ dir_initialize - "+prefix+"/Caskroom"))
	m := &Manager{opt: Options{BrewBin: brew, UserHome: t.TempDir(), UserName: ""}}

	_, err := m.InstalledFormulaVersionsErr(context.Background())
	if err == nil {
		t.Fatal("假 brew 非零退出时探测必须报错")
	}
	msg := err.Error()
	// 真实原因（可诊断）
	if !strings.Contains(msg, "dir_initialize") {
		t.Errorf("失败消息里丢了真实 stderr：\n%s", msg)
	}
	// 可操作的修复建议
	missing := filepath.Join(prefix, "Caskroom")
	if !strings.Contains(msg, missing) {
		t.Errorf("失败消息里没点名缺哪个目录（%s）：\n%s", missing, msg)
	}
	if !strings.Contains(msg, "sudo install -d") {
		t.Errorf("失败消息里没给出补建命令：\n%s", msg)
	}
	if !strings.Contains(msg, "Homebrew 目录") {
		t.Errorf("失败消息里没给出面板入口：\n%s", msg)
	}
	if own := BrewDirOwnerOf(prefix); own.Known && own.Name != "" && !strings.Contains(msg, own.Name) {
		t.Errorf("失败消息里没写明归属（%s）：\n%s", own.Name, msg)
	}
	// 应用市场那条"原因："在 apps.js 里只显示前 140 字符 —— 补建命令必须落在里面。
	head := []rune(msg)
	if len(head) > 140 {
		head = head[:140]
	}
	if !strings.Contains(string(head), "install -d") || !strings.Contains(string(head), missing) {
		t.Errorf("前 140 字符里必须同时有缺失目录与补建命令（否则市场那一条看不到怎么修）：\n%s", string(head))
	}
}

// TestRepairBrewDirsHonestWhenReadbackStillFails 是**最关键**的一条：
// 目录补建动作会成功，但回读 `brew list --versions` 仍失败 ——
// 这时必须明确失败（"仍未复核通过"），绝不谎报成功。
func TestRepairBrewDirsHonestWhenReadbackStillFails(t *testing.T) {
	prefix := t.TempDir()
	brew := writeBrewDirsFakeBrew(t, prefix, fmt.Sprintf(
		"case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\n  list) echo %q >&2; exit 1 ;;\nesac\nexit 0\n",
		prefix, "Error: No such file or directory @ dir_initialize - "+prefix+"/Caskroom"))
	m := &Manager{opt: Options{BrewBin: brew, UserHome: t.TempDir(), UserName: ""}}

	res := &InstallResult{}
	err := m.RepairBrewDirs(context.Background(), res)
	if err == nil {
		t.Fatal("回读复核失败时必须返回 error —— 目录建好了不等于 brew 能跑（不许谎报成功）")
	}
	if !strings.Contains(err.Error(), "仍未") {
		t.Errorf("错误文案必须明确说「仍未复核通过」，实际：%v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "仍未通过") {
		t.Errorf("任务步骤里必须如实说「仍未通过」，实际：\n%s", joined)
	}
	// 失败发生在回读，而不是更早：目录确实被补建了。
	if st, serr := os.Stat(filepath.Join(prefix, "Caskroom")); serr != nil || !st.IsDir() {
		t.Errorf("Caskroom 应已被补建（失败点是回读复核），实际 err=%v", serr)
	}
	assertSameOwner(t, filepath.Join(prefix, "Caskroom"), prefix)
}

// TestRepairBrewDirsSucceedsOnlyAfterRealReadback：假 brew 会在目录补齐后**真的**
// 转为成功 —— 证明"成功"这个结论来自回读，而不是来自"我们建了目录"。
func TestRepairBrewDirsSucceedsOnlyAfterReadbackPasses(t *testing.T) {
	prefix := t.TempDir()
	cask := filepath.Join(prefix, "Caskroom")
	brew := writeBrewDirsFakeBrew(t, prefix, fmt.Sprintf(
		"case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\n  list)\n    if [ -d %q ]; then echo 'nginx 1.31.5'; exit 0; fi\n    echo %q >&2; exit 1 ;;\nesac\nexit 0\n",
		prefix, cask, "Error: No such file or directory @ dir_initialize - "+cask))
	m := &Manager{opt: Options{BrewBin: brew, UserHome: t.TempDir(), UserName: ""}}

	res := &InstallResult{}
	if err := m.RepairBrewDirs(context.Background(), res); err != nil {
		t.Fatalf("目录补齐后回读应通过，实际：%v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "复核通过") {
		t.Errorf("成功时任务日志里必须写明回读复核通过：\n%s", joined)
	}
	if !strings.Contains(joined, "已补建 "+cask) {
		t.Errorf("任务日志里必须逐条写明补建了哪个目录：\n%s", joined)
	}
	assertSameOwner(t, cask, prefix)
}

// 已经健康的机器：不建任何东西，但**仍然回读**，回读通过才算成功。
func TestRepairBrewDirsOnHealthyPrefixStillReadsBack(t *testing.T) {
	prefix := t.TempDir()
	for _, d := range brewRepairDirs {
		if err := os.MkdirAll(filepath.Join(prefix, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	brew := writeBrewDirsFakeBrew(t, prefix, fmt.Sprintf(
		"case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\n  list) echo 'nginx 1.31.5'; exit 0 ;;\nesac\nexit 0\n", prefix))
	m := &Manager{opt: Options{BrewBin: brew, UserHome: t.TempDir(), UserName: ""}}

	res := &InstallResult{}
	if err := m.RepairBrewDirs(context.Background(), res); err != nil {
		t.Fatalf("健康前缀上应直接复核通过，实际：%v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "缺失目录：无") {
		t.Errorf("没有缺失目录时要如实说「无」：\n%s", joined)
	}
	if !strings.Contains(joined, "复核通过") {
		t.Errorf("没有缺失目录也必须回读复核（成功判据只有它）：\n%s", joined)
	}
}

// 安全门禁：待补建路径上已经有个**普通文件**时，面板绝不删改它，
// 只如实列出来，并最终以"回读失败"收场（不许把文件当目录用）。
func TestRepairBrewDirsNeverOverwritesConflicts(t *testing.T) {
	prefix := t.TempDir()
	cask := filepath.Join(prefix, "Caskroom")
	const content = "user data, do not touch"
	if err := os.WriteFile(cask, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	brew := writeBrewDirsFakeBrew(t, prefix, fmt.Sprintf(
		"case \"$1\" in\n  --prefix) echo %q; exit 0 ;;\n  list) echo %q >&2; exit 1 ;;\nesac\nexit 0\n",
		prefix, "Error: No such file or directory @ dir_initialize - "+cask))
	m := &Manager{opt: Options{BrewBin: brew, UserHome: t.TempDir(), UserName: ""}}

	res := &InstallResult{}
	if err := m.RepairBrewDirs(context.Background(), res); err == nil {
		t.Fatal("有冲突（存在但不是目录）且回读失败时必须如实报错")
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "存在但不是目录") {
		t.Errorf("冲突必须如实列出来：\n%s", joined)
	}
	got, err := os.ReadFile(cask)
	if err != nil {
		t.Fatalf("冲突文件被删了：%v", err)
	}
	if string(got) != content {
		t.Errorf("冲突文件内容被改了：%q", string(got))
	}
}

// ---------- 辅助 ----------

// shortPrefix 造一个**短**临时前缀：应用市场的"原因："只显示前 140 字符
// （apps.js），t.TempDir() 的 60+ 字符长路径会把补建命令挤出可见区，测不出真实行为。
func shortPrefix(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zpb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// writeBrewDirsFakeBrew 在给定的临时前缀里造一个**假 brew**（绝不是真 brew）：
// 路径就是 <prefix>/bin/brew，与真机布局一致 —— 这样"从 BrewBin 反推前缀"
// 这条判据才与真机同形。测试全程不碰 /opt/homebrew。
func writeBrewDirsFakeBrew(t *testing.T, prefix, body string) string {
	t.Helper()
	bin := filepath.Join(prefix, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// assertSameOwner 断言 path 的 uid/gid 与 ref 一致（归属交还的真实判据）。
func assertSameOwner(t *testing.T, path, ref string) {
	t.Helper()
	want := BrewDirOwnerOf(ref)
	got := BrewDirOwnerOf(path)
	if !want.Known {
		t.Fatalf("前置条件不成立：读不到 %s 的属主", ref)
	}
	if !got.Known || got.UID != want.UID || got.GID != want.GID {
		t.Errorf("%s 的归属是 %s，应为 %s（brew 前缀的真实属主）", path, got.Label(), want.Label())
	}
}
