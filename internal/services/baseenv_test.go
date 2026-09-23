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
//  基础环境（运行依赖层）的行为锁
//
//  2026-09-19 产品负责人把「基础环境」拆成两层：
//    · 运行依赖层（本文件）：CLT → Homebrew → ffmpeg，所有 brew 类应用都要；
//    · 网站环境层：nginx / PHP / MySQL / PostgreSQL（一键 LNMP，lnmp.go）。
//  这些测试锁住三件事（全部**不碰真实环境** —— 用注入的探测器与临时目录）：
//    1) 探测口径以现实为准：CLT/brew/ffmpeg 各自怎么算"缺"；
//    2) 顺序与 missing 文案稳定（前端横幅按它显示"还缺什么"）；
//    3) 安装链只做 Homebrew + ffmpeg，**绝不**碰 nginx / PHP / MySQL。
// ============================================================================

// baseEnvManager 造一个"什么都不装、什么都不碰"的 Manager：
// CLT 探测注入成指定值，brew 候选钉到临时目录，命令探测用 fakeProbe。
func baseEnvManager(t *testing.T, clt bool, brewCandidates []string, present ...string) *Manager {
	t.Helper()
	prev := brewBinCandidates
	brewBinCandidates = func() []string { return brewCandidates }
	t.Cleanup(func() { brewBinCandidates = prev })

	m := &Manager{opt: Options{LookPath: fakeProbe(present...)}}
	m.baseEnvCLTProbe = func(context.Context) bool { return clt }
	return m
}

// TestBaseEnvStatusMissingOrderAndShape 锁住探测口径与 missing 顺序：
// 缺 CLT 报「命令行开发者工具」，缺 brew 报「Homebrew」，缺依赖报命令名，
// 顺序是 CLT → Homebrew → ffmpeg/ffprobe（前端横幅按这个顺序显示）。
//
// 这一条同时是**契约样例**：clt_ok=true / brew_ok=false / deps_ok=false 时，
// missing 必须正好是 ["Homebrew","ffmpeg"]（与接口契约里给的例子逐字一致）。
func TestBaseEnvStatusMissingOrderAndShape(t *testing.T) {
	dir := t.TempDir()
	m := baseEnvManager(t, true, []string{filepath.Join(dir, "nope", "bin", "brew")}, "ffprobe")

	st := m.BaseEnvStatus(context.Background())
	if !st.CLTOK {
		t.Fatalf("CLT 探测注入为 true，不该报缺，实际 %+v", st)
	}
	if st.BrewOK || st.DepsOK || st.Ready {
		t.Fatalf("brew/依赖都缺时不该报就绪，实际 %+v", st)
	}
	want := []string{"Homebrew", "ffmpeg"}
	if strings.Join(st.Missing, ",") != strings.Join(want, ",") {
		t.Fatalf("missing = %v，期望 %v（契约样例）", st.Missing, want)
	}

	// 连 CLT 也缺时，它必须排在第一个（它是 Homebrew 的前置依赖）。
	m2 := baseEnvManager(t, false, []string{filepath.Join(dir, "nope", "bin", "brew")})
	st2 := m2.BaseEnvStatus(context.Background())
	if len(st2.Missing) == 0 || st2.Missing[0] != "命令行开发者工具" {
		t.Fatalf("缺 CLT 时 missing 第一项应为命令行开发者工具，实际 %v", st2.Missing)
	}
	if st2.Missing[1] != "Homebrew" {
		t.Fatalf("Homebrew 应排在 CLT 之后，实际 %v", st2.Missing)
	}
}

// TestBaseEnvStatusReadyWhenAllPresent 全都就绪时必须 ready=true 且 missing 为空。
func TestBaseEnvStatusReadyWhenAllPresent(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "homebrew", "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := baseEnvManager(t, true, []string{brew}, "ffmpeg", "ffprobe")

	st := m.BaseEnvStatus(context.Background())
	if !st.CLTOK || !st.BrewOK || !st.DepsOK || !st.Ready {
		t.Fatalf("全都在时应 ready，实际 %+v", st)
	}
	if len(st.Missing) != 0 {
		t.Fatalf("全都在时 missing 必须为空，实际 %v", st.Missing)
	}
}

// TestBaseEnvBrewDetectionFollowsCandidates 锁住 brew 口径：
//   - 配置里存的是另一个架构的路径（Intel /usr/local）时，以**候选位置里真实
//     存在的那个**为准（Apple Silicon /opt/homebrew），与 EnsureHomebrew 同源；
//   - 候选里一个都没有 → 如实报缺，不因为"配置里写着路径"就谎报已装。
func TestBaseEnvBrewDetectionFollowsCandidates(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "opt-homebrew", "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := baseEnvManager(t, true, []string{real}, "ffmpeg", "ffprobe")
	m.opt.BrewBin = filepath.Join(dir, "usr-local", "bin", "brew") // 不存在
	if st := m.BaseEnvStatus(context.Background()); !st.BrewOK {
		t.Fatalf("候选里真有 brew 时应报 brew_ok=true，实际 %+v", st)
	}

	// 配置里有一个**存在**的路径，但一个候选都不存在：仍以现实为准 —— 路径存在就算装了。
	m2 := baseEnvManager(t, true, []string{filepath.Join(dir, "nope")}, "ffmpeg")
	m2.opt.BrewBin = real
	if st := m2.BaseEnvStatus(context.Background()); !st.BrewOK {
		t.Fatalf("配置路径真实存在时应报 brew_ok=true，实际 %+v", st)
	}

	// 什么都不存在 → 缺，且不瞎改配置（让后续报错保持可读）。
	m3 := baseEnvManager(t, true, []string{filepath.Join(dir, "nope")}, "ffmpeg")
	m3.opt.BrewBin = filepath.Join(dir, "usr-local", "bin", "brew")
	if st := m3.BaseEnvStatus(context.Background()); st.BrewOK {
		t.Fatalf("brew 根本不存在时不能报 brew_ok=true，实际 %+v", st)
	}
}

// TestEnsureBaseEnvironmentChainIsHomebrewThenDeps 锁住安装链的顺序与内容：
// Homebrew 在先、基础依赖在后，且**绝不涉及**网站环境组件。
//
// BrewBin 指向一个真实存在的临时文件、依赖探测注入成"都在"，
// 所以整个调用不会执行任何 brew、不联网、不装东西。
func TestEnsureBaseEnvironmentChainIsHomebrewThenDeps(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "homebrew", "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := baseEnvManager(t, true, []string{brew}, "ffmpeg", "ffprobe")
	m.opt.BrewBin = brew

	res := &InstallResult{Steps: []string{}}
	if err := m.EnsureBaseEnvironment(context.Background(), res); err != nil {
		t.Fatalf("全都就绪时安装应幂等通过，实际 %v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	brewAt := strings.Index(joined, "Homebrew 已安装")
	depsAt := strings.Index(joined, "基础依赖已就绪")
	if brewAt < 0 || depsAt < 0 {
		t.Fatalf("步骤里应同时出现 Homebrew 与基础依赖两步，实际：%s", joined)
	}
	if brewAt > depsAt {
		t.Errorf("Homebrew 必须排在基础依赖之前，实际：%s", joined)
	}
	for _, forbidden := range []string{"nginx", "PHP", "MySQL", "phpMyAdmin"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("基础环境安装步骤里出现了 %q —— 它属于网站环境，绝不能被顺带装：%s",
				forbidden, joined)
		}
	}
}

// TestBaseEnvironmentSourceNeverInstallsLNMP 源码级回归锁（与 basedep_test.go 同一思路）。
//
// EnsureBaseEnvironment 不好在单测里跑完整条失败链（会真的调 brew），
// 所以直接读源码、只看**函数体**：它只调用 EnsureHomebrew + EnsureBaseDependencies，
// 函数体里不出现任何网站环境组件/组合动作（注释里提到它们不算 —— 那是解释）。
func TestBaseEnvironmentSourceNeverInstallsLNMP(t *testing.T) {
	b, err := os.ReadFile("baseenv.go")
	if err != nil {
		t.Fatalf("读 baseenv.go 失败：%v", err)
	}
	src := string(b)
	const marker = "func (m *Manager) EnsureBaseEnvironment("
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("baseenv.go 里找不到 EnsureBaseEnvironment")
	}
	body := src[start:]
	if end := strings.Index(body, "\nfunc "); end >= 0 {
		body = body[:end]
	}

	for _, want := range []string{"EnsureHomebrew", "EnsureBaseDependencies"} {
		if !strings.Contains(body, want) {
			t.Errorf("EnsureBaseEnvironment 必须调用 %s（基础环境 = CLT → Homebrew → ffmpeg）", want)
		}
	}
	for _, forbidden := range []string{"InstallLNMP", "EnsureLNMP", `"nginx"`, `"php@`, `"mysql@`, "phpmyadmin"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("基础环境安装链里出现了 %q —— 它属于网站环境（一键 LNMP），绝不能装", forbidden)
		}
	}
}

// TestBaseEnvStatusDoesNotClaimMissingCLTWhenProbeFails 锁住用户 2026-09-23 的偶发误报：
// "后台偶发报缺少运行依赖：命令行开发者工具，刷新后消失"。
//
// 真因：`xcode-select -p` 偶发跑不起来（超时/被杀）时被当成"没装 CLT"。
// 判据：**未复核 ≠ 缺少** —— 探测失败时不许把"命令行开发者工具"列进 missing，
// 而要给出 probe_error 让界面如实说"没查成、点刷新重试"。
// 负向对照：命令成功但确实没装（baseEnvCLTProbe 返回 false）时必须照旧报缺失。
func TestBaseEnvStatusDoesNotClaimMissingCLTWhenProbeFails(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)

	// ① 探测失败（模拟 xcode-select 超时/被杀）
	oldProbe, oldFail := m.baseEnvCLTProbe, m.baseEnvCLTFailProbe
	t.Cleanup(func() { m.baseEnvCLTProbe, m.baseEnvCLTFailProbe = oldProbe, oldFail })
	m.baseEnvCLTProbe = nil
	m.baseEnvCLTFailProbe = func(context.Context) error { return errors.New("signal: killed") }

	st := m.BaseEnvStatus(t.Context())
	if st.CLTOK {
		t.Error("探测失败时不能报 CLT 已就绪")
	}
	for _, miss := range st.Missing {
		if strings.Contains(miss, "命令行开发者工具") {
			t.Errorf("探测失败≠缺少：不许把它列成缺失项，实际 missing=%v", st.Missing)
		}
	}
	if st.ProbeError == "" || !strings.Contains(st.ProbeError, "命令行开发者工具") {
		t.Errorf("必须如实交代「未复核」及原因，实际 probe_error=%q", st.ProbeError)
	}

	// ② 负向对照：命令成功但没有 CLT → 必须报缺失，且没有 probe_error
	m.baseEnvCLTFailProbe = nil
	m.baseEnvCLTProbe = func(context.Context) bool { return false }
	st2 := m.BaseEnvStatus(t.Context())
	found := false
	for _, miss := range st2.Missing {
		if strings.Contains(miss, "命令行开发者工具") {
			found = true
		}
	}
	if !found {
		t.Errorf("确实没装时必须报缺失，实际 missing=%v", st2.Missing)
	}
	if st2.ProbeError != "" {
		t.Errorf("已复核成功时不该有 probe_error，实际 %q", st2.ProbeError)
	}
}
