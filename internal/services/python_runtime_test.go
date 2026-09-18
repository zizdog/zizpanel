package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 假 brew：只实现本测试用到的子命令。写成脚本而不是"注入 override"，
// 是因为 brewHas / InstalledBrewVersion 走的就是真实可执行文件那条路
// （m.opt.BrewBin），用假脚本能顺带证明"路径推导 + 参数拼装"是对的。
func writeFakeBrew(t *testing.T, prefix string, listOut string) string {
	t.Helper()
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
  list) echo "` + listOut + `"; exit 0 ;;
  install) echo "==> Pouring fake bottle"; exit 0 ;;
  uninstall) echo "Uninstalling fake formula"; exit 0 ;;
esac
exit 0
`
	path := filepath.Join(bin, "brew")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// pythonRuntimeManager 造一个完全离线的 Manager：brew 指向假脚本、
// 镜像探测不走网络（否则单测会去访问阿里云/中科大 —— 违反"单测不许碰真实服务"）。
func pythonRuntimeManager(t *testing.T, listOut string) (*Manager, string) {
	t.Helper()
	m, _ := sandboxManager(t)
	prefix := t.TempDir()
	writeFakeBrew(t, prefix, listOut)
	m.opt.BrewBin = filepath.Join(prefix, "bin", "brew")
	m.opt.UserName = ""
	m.mirrorProbeOverride = func(context.Context, string) (string, string) {
		return "https://mirror.test/api", "https://mirror.test/brew"
	}
	return m, prefix
}

// TestPanelPythonPathsFollowFormula 是"换版本不再漏改"的核心契约。
//
// 以前解释器路径与 site-packages 目录名是**写死**的 "python3.11"：
// 换预置版本时改了一处、漏了另一处，表现是"venv 用一个版本建、
// 补丁文件写进另一个版本的目录"，而且当场不报错（见 python_runtime.go 的说明）。
func TestPanelPythonPathsFollowFormula(t *testing.T) {
	cases := []struct {
		formula, ver string
	}{
		{"python@3.11", "3.11"},
		{"python@3.12", "3.12"},
		{"python@3.13", "3.13"},
	}
	for _, c := range cases {
		if got := pythonFormulaVersion(c.formula); got != c.ver {
			t.Errorf("pythonFormulaVersion(%q) = %q，期望 %q", c.formula, got, c.ver)
		}
		wantBin := filepath.Join("/opt/homebrew", "opt", c.formula, "bin", "python"+c.ver)
		if got := panelPythonInterpreter("/opt/homebrew", c.formula); got != wantBin {
			t.Errorf("panelPythonInterpreter(%q) = %q，期望 %q", c.formula, got, wantBin)
		}
		wantSP := filepath.Join("/tmp/venv", "lib", "python"+c.ver, "site-packages")
		if got := panelPythonSitePackages("/tmp/venv", c.formula); got != wantSP {
			t.Errorf("panelPythonSitePackages(%q) = %q，期望 %q", c.formula, got, wantSP)
		}
	}
	// 没有 @ 的 formula 推导不出解释器：必须显式返回空（调用方据此报错，
	// 而不是拼出一个不存在的路径去撞运气）。
	if got := panelPythonInterpreter("/opt/homebrew", "python"); got != "" {
		t.Errorf("无版本后缀的 python 不该推导出解释器路径，实际 %q", got)
	}
	if got := panelPythonSitePackages("/tmp/venv", "python"); got != "" {
		t.Errorf("无版本后缀的 python 不该推导出 site-packages，实际 %q", got)
	}
}

// TestPythonVersionIsSingleSourceInInstallers 锁死"两个安装器不再写死版本"。
//
// 这是一条结构性断言（读源码文本），因为真正的风险不是逻辑写错，而是
// **下次换预置版本时漏改一处**：那种错误编译能过、单测也能过，
// 只有真机装到一半才会暴露。项目里已有同类先例（TestInstallLNMPCallsSharedPHPListenFix）。
func TestPythonVersionIsSingleSourceInInstallers(t *testing.T) {
	for _, f := range []string{"qwentts.go", "iopaint.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "\"python3.11\"") {
			t.Errorf("%s 里仍有写死的 \"python3.11\"：必须用 panelPythonInterpreter/"+
				"panelPythonSitePackages 从 panelPythonFormula 推导（否则换版本必漏改）", f)
		}
		if !strings.Contains(src, "panelPythonInterpreter(") {
			t.Errorf("%s 必须用 panelPythonInterpreter 取解释器路径", f)
		}
	}
	b, err := os.ReadFile("qwentts.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "panelPythonSitePackages(") {
		t.Error("qwentts.go 必须用 panelPythonSitePackages 取 site-packages 目录（IPv4 补丁靠它生效）")
	}
	// 预置版本本身只能出现在 python_runtime.go 这一处常量里。
	rt, err := os.ReadFile("python_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rt), "const panelPythonFormula = ") {
		t.Error("python_runtime.go 必须是 panelPythonFormula 的定义处")
	}
}

// TestInstallPythonRuntimeReportsRealVersion：装完必须报告**磁盘上的真实版本**。
//
// 用户 2026-09-18 的原话是"预置 3.11.16 是不正确的选择"—— 面板能决定的只有
// formula（python@3.11），补丁版本由 Homebrew 与镜像同步情况决定。所以任务日志
// 里必须是 `brew list --versions` 的真实输出，而不是我们写死的期望值。
func TestInstallPythonRuntimeReportsRealVersion(t *testing.T) {
	m, _ := pythonRuntimeManager(t, "python@3.11 3.11.16")

	res := &InstallResult{}
	if err := m.InstallPythonRuntime(context.Background(), "python@3.11", res); err != nil {
		t.Fatalf("安装不该失败：%v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "3.11.16") {
		t.Errorf("必须写出真实版本号（来自 brew list --versions），实际 Steps：\n%s", joined)
	}
	if !strings.Contains(joined, filepath.Join("opt", "python@3.11", "bin", "python3.11")) {
		t.Errorf("必须写出解释器位置（用户下一步就是用它），实际 Steps：\n%s", joined)
	}
	if strings.Contains(joined, "brew services") {
		t.Errorf("解释器没有 brew service，不该出现服务注册相关字样（假服务记录就是这么来的）：\n%s", joined)
	}
}

// TestInstallPythonRuntimeHonestWhenVersionUnreadable：读不到版本时如实说，
// 不许编一个版本号，也不许因此把"已安装"说成失败（包确实装好了）。
func TestInstallPythonRuntimeHonestWhenVersionUnreadable(t *testing.T) {
	// 假 brew 对 list 只回 formula 名、没有版本号
	m, _ := pythonRuntimeManager(t, "python@3.11")

	res := &InstallResult{}
	if err := m.InstallPythonRuntime(context.Background(), "python@3.11", res); err != nil {
		t.Fatalf("包已装好，读不出版本不该算安装失败：%v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "读不出实际版本") {
		t.Errorf("读不到版本必须如实说明，实际 Steps：\n%s", joined)
	}
	if strings.Contains(joined, "实际版本 ") {
		t.Errorf("没有版本号时不许写出「实际版本 x」，实际 Steps：\n%s", joined)
	}
}

// TestPythonRuntimeDependentsReadsRealVenvs：卸载前的依赖判断必须基于**真实磁盘状态**。
func TestPythonRuntimeDependentsReadsRealVenvs(t *testing.T) {
	m, _ := sandboxManager(t)
	home := m.opt.UserHome

	writeVenv := func(rel, body string) {
		t.Helper()
		dir := filepath.Join(home, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pyvenv.cfg"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeVenv("tts/qwen3/.venv", "home = /opt/homebrew/opt/python@3.11/bin\nexecutable = /opt/homebrew/Cellar/python@3.11/3.11.16/bin/python3.11\n")
	writeVenv("iopaint/.venv", "home = /opt/homebrew/opt/python@3.12/bin\nexecutable = /opt/homebrew/Cellar/python@3.12/3.12.14/bin/python3.12\n")

	deps311 := m.pythonRuntimeDependents("python@3.11")
	if len(deps311) != 1 || !strings.Contains(deps311[0], "Qwen3 TTS") {
		t.Errorf("python@3.11 的依赖方应只有 Qwen3 TTS，实际 %v", deps311)
	}
	deps312 := m.pythonRuntimeDependents("python@3.12")
	if len(deps312) != 1 || !strings.Contains(deps312[0], "IOPaint") {
		t.Errorf("python@3.12 的依赖方应只有 IOPaint，实际 %v", deps312)
	}
	if got := m.pythonRuntimeDependents("python@3.13"); len(got) != 0 {
		t.Errorf("没人用 3.13，不该报出依赖方，实际 %v", got)
	}
}

// TestUninstallPythonRuntimeHonestFailures：卸载的两条诚实性要求。
//
//  1. brew 本身不在时**报错**，不能顺着 brewHas 的 false 说"已经没装了"
//     （那是"探不到"，不是"没装过" —— 用户会以为删干净了）；
//  2. brew 在、包也在时：如实点名正在用它的虚拟环境，并真的执行卸载。
func TestUninstallPythonRuntimeHonestFailures(t *testing.T) {
	app := App{ID: "python311", Name: "Python 3.11", BrewFormula: "python@3.11", PanelInstaller: "python"}

	// ① brew 不存在
	m1, _ := sandboxManager(t)
	m1.opt.BrewBin = filepath.Join(t.TempDir(), "nope", "bin", "brew")
	if err := m1.UninstallPythonRuntime(context.Background(), app, &InstallResult{}); err == nil {
		t.Fatal("找不到 brew 时必须报错，不能假装已经卸载/没装")
	}

	// ② brew 在、包也在，且有 venv 正在用它
	m2, _ := pythonRuntimeManager(t, "python@3.11 3.11.16")
	venv := filepath.Join(m2.opt.UserHome, "tts", "qwen3", ".venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "home = /opt/homebrew/opt/python@3.11/bin\n"
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	res := &InstallResult{}
	if err := m2.UninstallPythonRuntime(context.Background(), app, res); err != nil {
		t.Fatalf("正常卸载不该失败：%v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "Qwen3 TTS") {
		t.Errorf("必须如实点名还在用它的环境（否则用户不知道服务会坏），实际 Steps：\n%s", joined)
	}
	if !strings.Contains(joined, "已卸载") {
		t.Errorf("成功卸载后要写清结果，实际 Steps：\n%s", joined)
	}
}
