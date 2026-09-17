package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  "brew install 失败即换源" 的回归测试（2026-09-20 真机事故）
//
//  现场：装 Qwen3 TTS 时 `brew install python@3.11` 失败，报
//      Error: Bottle reports different checksum:   a5dd571f…
//      SHA-256 checksum of downloaded file: e3b0c442…（空文件的 sha256）
//  根因：面板强制走自建镜像，而镜像侧把上游 403 缓存成了 0 字节文件；
//        这个 bottle 在国内几个公共镜像上都没有（USTC 403、其余 404），
//        只有官方 ghcr.io 有 —— 于是"失败直接判死刑"必须变成"逐源重试"。
//
//  这一组测试全部用可注入的假执行器，**不执行真实 brew、不碰真实用户家目录、
//  不联网**（mirrorProbeOverride + brewSourceRunOverride + brewCacheDirOverride）。
// ============================================================================

// emptyFileSHA256 是 e3b0c442… 的完整值（空文件的 SHA-256）。
// 单独起个短名只是为了让下面的假输出好读。
const emptyFileSHA256 = sha256OfEmptyFile

// brewFallbackTestManager 造一个只够跑换源逻辑的 Manager。
//
// UserHome 刻意留空：任何"没注入缓存目录"的分支都会走"跳过清理"，
// 从而不可能读到/删掉开发机真实用户的家目录。
func brewFallbackTestManager(t *testing.T, mirrorBase string) *Manager {
	t.Helper()
	// 运行环境里可能真的设了 HOMEBREW_*（开发机自己就在用 Homebrew）：
	// 不清掉的话 brewEnv 的 pick() 会优先用户值，断言会随机器状态飘。
	t.Setenv("HOMEBREW_API_DOMAIN", "")
	t.Setenv("HOMEBREW_BOTTLE_DOMAIN", "")
	m := &Manager{opt: Options{MirrorBase: mirrorBase}}
	// 默认探测会发真实网络请求（违反"单测不许联网"）。
	m.mirrorProbeOverride = func(ctx context.Context, probeFormula string) (string, string) {
		if mirrorBase == "" {
			return "", ""
		}
		base := strings.TrimRight(mirrorBase, "/") + "/brew"
		return base + "/api", base
	}
	return m
}

// realCorruptBottleOutput 是事故里 brew 的真实输出（截取关键几行）。
const realCorruptBottleOutput = `==> Downloading https://mirror.zizdog.com:8888/brew/python@3.11-3.11.16.arm64_sequoia.bottle.tar.gz
Already downloaded: /Users/someone/Library/Caches/Homebrew/downloads/a5dd571f54091ffd66c7ad74b24554bc8c3fd069e134a7218d12adfc16d093ca--python@3.11-3.11.16.arm64_sequoia.bottle.tar.gz
Error: Bottle reports different checksum:   a5dd571f54091ffd66c7ad74b24554bc8c3fd069e134a7218d12adfc16d093ca
       SHA-256 checksum of downloaded file: ` + emptyFileSHA256

// TestBrewInstallSourceOrder 锁住换源顺序：当前镜像 → 清华 → 官方源。
func TestBrewInstallSourceOrder(t *testing.T) {
	m := brewFallbackTestManager(t, "https://mirror.zizdog.com:8888")

	srcs := m.brewInstallSources(context.Background(), "python@3.11")
	if len(srcs) != 3 {
		t.Fatalf("应有 3 个源（自建镜像→清华→官方），实际 %d 个：%+v", len(srcs), srcs)
	}
	if !strings.Contains(srcs[0].Name, "自建镜像") {
		t.Errorf("第 1 个源应是自建镜像，实际 %q", srcs[0].Name)
	}
	if got := brewEnvValue(srcs[0].Env, "HOMEBREW_BOTTLE_DOMAIN"); got != "https://mirror.zizdog.com:8888/brew" {
		t.Errorf("第 1 个源应把瓶域指向自建镜像，实际 %q", got)
	}
	if srcs[1].Name != "清华大学镜像（tuna）" {
		t.Errorf("第 2 个源应是清华，实际 %q", srcs[1].Name)
	}
	if got := brewEnvValue(srcs[1].Env, "HOMEBREW_BOTTLE_DOMAIN"); got != brewTUNABase {
		t.Errorf("第 2 个源的瓶域应为 %s，实际 %q", brewTUNABase, got)
	}
	// 官方源：必须**显式清除**两个镜像变量，而不是"不设"——
	// 面板进程/用户 shell 里残留的值会被子进程继承。
	official := srcs[2]
	if !strings.Contains(official.Name, "官方源") {
		t.Errorf("第 3 个源应是官方源，实际 %q", official.Name)
	}
	if len(official.Unset) != 2 {
		t.Fatalf("官方源必须显式 unset HOMEBREW_API_DOMAIN / HOMEBREW_BOTTLE_DOMAIN，实际 %v", official.Unset)
	}
	if brewEnvValue(official.Env, "HOMEBREW_BOTTLE_DOMAIN") != "" ||
		brewEnvValue(official.Env, "HOMEBREW_API_DOMAIN") != "" {
		t.Errorf("官方源不应注入任何镜像域，实际 %v", official.Env)
	}
	if !strings.Contains(strings.Join(official.Env, " "), "HOMEBREW_NO_AUTO_UPDATE=1") {
		t.Errorf("官方源也应带上公共开关，实际 %v", official.Env)
	}
}

// TestBrewInstallSourcesNoDuplicateOfficial 锁住"探测不到镜像时不要排两遍官方源"：
// 第 1 条已经是官方源，再补一条一模一样的只会让用户以为面板在原地空转。
func TestBrewInstallSourcesNoDuplicateOfficial(t *testing.T) {
	m := brewFallbackTestManager(t, "")
	srcs := m.brewInstallSources(context.Background(), "nginx")
	official := 0
	for _, s := range srcs {
		if strings.Contains(s.Name, "官方源") {
			official++
		}
	}
	if official != 1 {
		t.Fatalf("官方源应只出现 1 次，实际 %d 次：%+v", official, srcs)
	}
}

// TestBrewInstallFallsBackOnCorruptBottle 是本轮修复的核心回归：
// 第 1 个源"校验失败/0 字节" → 必须当"该源不可用"继续换源，
// 而不是直接失败；换源前必须清掉相关坏包缓存。
func TestBrewInstallFallsBackOnCorruptBottle(t *testing.T) {
	m := brewFallbackTestManager(t, "https://mirror.zizdog.com:8888")

	// 造一个真实的缓存目录：坏包 + 无关条目各一个。
	cacheDir := t.TempDir()
	badName := "a5dd571f54091ffd66c7ad74b24554bc8c3fd069e134a7218d12adfc16d093ca--python@3.11-3.11.16.arm64_sequoia.bottle.tar.gz"
	unrelated := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe--phpmyadmin-5.2.2.arm64_sequoia.bottle.tar.gz"
	for _, n := range []string{badName, unrelated} {
		if err := os.WriteFile(filepath.Join(cacheDir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.brewCacheDirOverride = cacheDir

	var (
		gotSources []string
		removals   [][]string
	)
	m.brewSourceRunOverride = func(ctx context.Context, timeout time.Duration, src brewInstallSource, args ...string) (string, error) {
		gotSources = append(gotSources, src.Name)
		if !strings.Contains(strings.Join(args, " "), "install python@3.11") {
			t.Errorf("真实参数应以 `install python@3.11` 开头，实际 %v", args)
		}
		switch len(gotSources) {
		case 1:
			return realCorruptBottleOutput, errors.New("exit status 1")
		case 2:
			// 清华：清单里根本没有这个瓶。
			return "Error: Failed to download resource \"python@3.11\"\ncurl: (22) The requested URL returned error: 404",
				errors.New("exit status 1")
		default:
			return "==> Pouring python@3.11--3.11.16.arm64_sequoia.bottle.tar.gz\n🍺 /opt/homebrew/Cellar/python@3.11/3.11.16: 3,314 files", nil
		}
	}
	m.brewCacheRemoveOverride = func(ctx context.Context, paths []string) error {
		removals = append(removals, paths)
		for _, p := range paths {
			_ = os.Remove(p)
		}
		return nil
	}

	res := &InstallResult{}
	if _, err := m.brewInstall(context.Background(), res, time.Minute, "python@3.11"); err != nil {
		t.Fatalf("官方源应当成功，实际失败：%v", err)
	}

	if len(gotSources) != 3 {
		t.Fatalf("应在 3 个源上各试一次，实际 %d：%v", len(gotSources), gotSources)
	}
	if !strings.Contains(gotSources[0], "自建镜像") || !strings.Contains(gotSources[1], "清华") ||
		!strings.Contains(gotSources[2], "官方源") {
		t.Errorf("换源顺序不对：%v", gotSources)
	}

	// 换源前必须清坏包，且**只清相关条目**。
	if len(removals) == 0 {
		t.Fatal("换源前没有清理 brew 下载缓存 —— 下一个源会复用同一个 0 字节坏包")
	}
	if len(removals[0]) != 1 || filepath.Base(removals[0][0]) != badName {
		t.Errorf("第一次清理应只删那一个坏包，实际 %v", removals[0])
	}
	if _, err := os.Stat(filepath.Join(cacheDir, unrelated)); err != nil {
		t.Errorf("无关缓存条目被误删：%v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, badName)); !os.IsNotExist(err) {
		t.Errorf("坏包缓存应已被删除，实际 err=%v", err)
	}

	// 日志必须说清"用了哪个源、结果如何"，并把"源坏了"和"包装不上"区分开。
	joined := strings.Join(res.Steps, "\n")
	for _, want := range []string{"自建镜像", "清华大学", "官方源", "Bottle reports different checksum", "该源不可用", "安装成功"} {
		if !strings.Contains(joined, want) {
			t.Errorf("任务步骤里应包含 %q，实际：\n%s", want, joined)
		}
	}
}

// TestBrewInstallAllSourcesFailListsEveryRealError 锁住"别只贴输出尾部"：
// 全部失败时，每个源的真实错误都要逐条列出，并给出可行动的下一步。
func TestBrewInstallAllSourcesFailListsEveryRealError(t *testing.T) {
	m := brewFallbackTestManager(t, "https://mirror.zizdog.com:8888")

	var n int
	m.brewSourceRunOverride = func(ctx context.Context, timeout time.Duration, src brewInstallSource, args ...string) (string, error) {
		n++
		switch n {
		case 1:
			return realCorruptBottleOutput, errors.New("exit status 1")
		case 2:
			return "curl: (22) The requested URL returned error: 404", errors.New("exit status 1")
		default:
			// 官方源也不通时，brew 的真实报错长这样；关键是它不能被前面两条盖掉。
			return "==> Downloading https://ghcr.io/v2/homebrew/core/python/3.11/manifests/3.11.16\n" +
					"Error: Failed to download resource \"python@3.11\"\ncurl: (28) Operation timed out after 60001 milliseconds",
				errors.New("exit status 1")
		}
	}

	_, err := m.brewInstall(context.Background(), &InstallResult{}, time.Minute, "python@3.11")
	if err == nil {
		t.Fatal("三个源都失败时必须返回错误（绝不允许谎报成功）")
	}
	msg := err.Error()
	for _, want := range []string{
		"自建镜像", "清华大学", "官方源", // 三个源逐条列出
		"Bottle reports different checksum", // 第 1 条的真实原因
		"404",                               // 第 2 条的真实原因
		"curl: (28)",                        // 第 3 条的真实原因
		"brew install python@3.11",          // 可行动的手工命令
		"稍后重试",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("最终错误文案应包含 %q，实际：\n%s", want, msg)
		}
	}
	if n != 3 {
		t.Errorf("应把 3 个源都试过，实际 %d 次", n)
	}
}

// TestBrewInstallOfflineModeDoesNotFallBack 锁住与离线模式的交互：
// 「仅走 NAS」时禁止任何外网回落（项目硬规则），只试 NAS 那一个源。
func TestBrewInstallOfflineModeDoesNotFallBack(t *testing.T) {
	m := brewFallbackTestManager(t, "https://mirror.zizdog.com:8888")
	m.opt.OfflineOnly = true

	srcs := m.brewInstallSources(context.Background(), "python@3.11")
	if len(srcs) != 1 {
		t.Fatalf("离线模式只能有 NAS 一个源，实际 %d：%+v", len(srcs), srcs)
	}

	var n int
	m.brewSourceRunOverride = func(ctx context.Context, timeout time.Duration, src brewInstallSource, args ...string) (string, error) {
		n++
		return "Error: download failed", errors.New("exit status 1")
	}
	_, err := m.brewInstall(context.Background(), &InstallResult{}, time.Minute, "python@3.11")
	if err == nil {
		t.Fatal("离线模式失败必须如实返回错误")
	}
	if n != 1 {
		t.Errorf("离线模式不许回落公网/官方源，实际试了 %d 次", n)
	}
	if !strings.Contains(err.Error(), "离线模式") {
		t.Errorf("离线模式的错误文案应说清为什么不换源，实际：%v", err)
	}
}

// TestBrewBottleDownloadBroken 锁住"什么算源坏了"的判据。
func TestBrewBottleDownloadBroken(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"事故原文", realCorruptBottleOutput, true},
		{"只有空文件 sha", "SHA-256 checksum of downloaded file: " + emptyFileSHA256, true},
		{"只有 checksum 一句话", "Error: Bottle reports different checksum: abc", true},
		{"普通 404", "curl: (22) The requested URL returned error: 404", false},
		{"编译失败", "Error: undefined symbol: _foo", false},
		{"无输出", "", false},
	}
	for _, c := range cases {
		if got := brewBottleDownloadBroken(c.text); got != c.want {
			t.Errorf("%s：brewBottleDownloadBroken = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestPlanBrewCacheCleanOnlyRelated 是"只删相关条目、绝不误删"的纯函数测试。
//
// 用本机缓存实测到的真实文件名形状（`<sha>--<formula>-<version>…`）。
func TestPlanBrewCacheCleanOnlyRelated(t *testing.T) {
	const (
		shaPython = "a5dd571f54091ffd66c7ad74b24554bc8c3fd069e134a7218d12adfc16d093ca"
		shaPhp    = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
		shaRead   = "beefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeef"
		shaGo     = "0badc0de0badc0de0badc0de0badc0de0badc0de0badc0de0badc0de0badc0de"
	)
	names := []string{
		shaPython + "--python@3.11-3.11.16.arm64_sequoia.bottle.tar.gz",    // 相关：formula + sha
		shaPhp + "--php-8.3.14.arm64_sequoia.bottle.tar.gz",                // 无关
		shaPhp + "--phpmyadmin-5.2.2.arm64_sequoia.bottle.tar.gz",          // 无关（不能被 php 带删）
		shaPhp + "--python@3.12-3.12.9.arm64_sequoia.bottle.tar.gz",        // 无关（不能被 3.11 带删）
		"ffff--python@3.11-3.11.16.arm64_sequoia.bottle.tar.gz.incomplete", // 相关：断点文件
		shaRead + "--readline-8.3.6.arm64_sequoia.bottle.tar.gz",           // 只被 sha 命中（依赖坏）
		shaPhp + "--xz-5.8.4.arm64_sequoia.bottle.tar.gz",                  // 无关
		shaGo + "--go-1.23.4.arm64_sequoia.bottle.tar.gz",                  // 相关：装 go 时
		shaGo + "--go-task-3.40.0.arm64_sequoia.bottle.tar.gz",             // 无关（不能被 go 带删）
		shaGo + "--go--1.22.0.arm64_sequoia.bottle.tar.gz",                 // 相关：`--` 分隔的历史布局
		"manifest.json", // 无关
	}
	got := planBrewCacheClean(names, []string{"python@3.11", "go"}, []string{shaPython, shaRead})
	want := map[string]bool{
		names[0]: true,
		names[4]: true,
		names[5]: true,
		names[7]: true,
		names[9]: true,
	}
	if len(got) != len(want) {
		t.Fatalf("应删 %d 条，实际 %d：%v", len(want), len(got), got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("误删了无关条目：%s", n)
		}
	}
}

// TestCleanBrewDownloadCacheOnlyTouchesRelatedFiles 是"清缓存"的落地测试：
// 用临时目录当缓存目录（绝不碰真实用户家目录），只删相关条目。
func TestCleanBrewDownloadCacheOnlyTouchesRelatedFiles(t *testing.T) {
	m := brewFallbackTestManager(t, "https://mirror.zizdog.com:8888")
	dir := t.TempDir()
	m.brewCacheDirOverride = dir

	const shaPython = "a5dd571f54091ffd66c7ad74b24554bc8c3fd069e134a7218d12adfc16d093ca"
	related := shaPython + "--python@3.11-3.11.16.arm64_sequoia.bottle.tar.gz"
	unrelated := []string{
		"cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe--php-8.3.14.arm64_sequoia.bottle.tar.gz",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef--python@3.12-3.12.9.arm64_sequoia.bottle.tar.gz",
	}
	for _, n := range append([]string{related}, unrelated...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := &InstallResult{}
	m.cleanBrewDownloadCache(context.Background(), res, []string{"python@3.11"}, realCorruptBottleOutput)

	if _, err := os.Stat(filepath.Join(dir, related)); !os.IsNotExist(err) {
		t.Errorf("相关坏包应被删除，实际 err=%v", err)
	}
	for _, n := range unrelated {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("无关条目被误删：%s（%v）", n, err)
		}
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "已清除 1 个相关下载缓存条目") {
		t.Errorf("清理动作必须写进日志（删了什么要可追溯），实际：%s", joined)
	}
}

// TestCleanBrewDownloadCacheMissingDirIsNotAnError 锁住"目录不存在不是错误"：
// 还没下过东西的机器上不该因此失败。
func TestCleanBrewDownloadCacheMissingDirIsNotAnError(t *testing.T) {
	m := brewFallbackTestManager(t, "")
	m.brewCacheDirOverride = filepath.Join(t.TempDir(), "does-not-exist")
	res := &InstallResult{}
	m.cleanBrewDownloadCache(context.Background(), res, []string{"nginx"}, "")
	if len(res.Steps) == 0 {
		t.Fatal("应如实说明缓存目录不可读（哪怕是良性情况）")
	}
}

// TestBrewOfficialSourceCommandUnsetsMirrorEnv 验证官方源真的会把镜像变量
// 从子进程环境里**清掉**（不是"不设"），并验证 sudo 路径的 `env -u` 顺序。
func TestBrewOfficialSourceCommandUnsetsMirrorEnv(t *testing.T) {
	m := brewFallbackTestManager(t, "https://mirror.zizdog.com:8888")
	m.opt.BrewBin = "/opt/homebrew/bin/brew"

	srcs := m.brewInstallSources(context.Background(), "python@3.11")
	official := srcs[len(srcs)-1]

	// 非 root（本地调试/单测）路径：直接检查子进程环境。
	cmd := m.brewCommand(context.Background(), official, "install", "python@3.11")
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "HOMEBREW_BOTTLE_DOMAIN=") || strings.HasPrefix(kv, "HOMEBREW_API_DOMAIN=") {
			t.Errorf("官方源的子进程环境里仍有镜像变量：%s", kv)
		}
	}

	// root 路径：走 `/usr/bin/env -u KEY KEY=VAL <brew> …`，-u 必须排在赋值前。
	args := brewEnvArgs(official, m.opt.BrewBin, []string{"install", "python@3.11"})
	got := strings.Join(args, " ")
	if !strings.Contains(got, "-u HOMEBREW_API_DOMAIN -u HOMEBREW_BOTTLE_DOMAIN") {
		t.Errorf("官方源缺少显式 unset：%s", got)
	}
	if strings.Index(got, "-u HOMEBREW_BOTTLE_DOMAIN") > strings.Index(got, "/opt/homebrew/bin/brew") {
		t.Errorf("unset 必须排在 brew 二进制之前：%s", got)
	}
}

// TestBrewSourceNameIsTruthful 锁住"日志里的源名必须是真的"：
// 探测回落到公共镜像时，不能还写着"自建镜像"。
func TestBrewSourceNameIsTruthful(t *testing.T) {
	m := brewFallbackTestManager(t, "https://mirror.zizdog.com:8888")
	ustc := []string{"HOMEBREW_BOTTLE_DOMAIN=" + brewMirrorCandidates[0].Base}
	if got := m.brewSourceName(ustc); !strings.Contains(got, "中科大") {
		t.Errorf("中科大镜像应被点名为中科大，实际 %q", got)
	}
	if got := m.brewSourceName(nil); !strings.Contains(got, "官方源") {
		t.Errorf("没设镜像时应报官方源，实际 %q", got)
	}
}
