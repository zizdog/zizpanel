package services

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// ============================================================================
//  服务注册脚本必须随二进制走
//
//  真机根因（2026-09-17 mini）：面板的在线升级只换二进制、不发 tools/**，
//  于是 /opt/zizpanel/system-services.sh 不存在，一键 LNMP 的最后一步
//  （注册系统级 LaunchDaemon）必然失败 —— 用户看到的就是"装了一半、
//  服务管理里什么都没有、报错看不懂"。这一组测试锁住"内置 + 落盘 + 不漂移"。
// ============================================================================

// TestEmbeddedSystemServicesScriptMatchesRepoCopy 是防漂移护栏。
//
// 脚本有两份物理副本：仓库里的 tools/system-services.sh（install.sh 会分发它）
// 与 assets/ 下的构建期快照（go:embed 用的）。两份不一致时运行时用的是内置那份，
// 于是"改了 tools/ 却没生效"会变成最难查的一类问题。这里直接断言字节相同。
func TestEmbeddedSystemServicesScriptMatchesRepoCopy(t *testing.T) {
	repo, err := os.ReadFile(filepath.Join("..", "..", "tools", "system-services.sh"))
	if err != nil {
		t.Fatalf("读不到仓库里的 tools/system-services.sh: %v", err)
	}
	if string(repo) != embeddedSystemServices {
		t.Error("内置脚本与 tools/system-services.sh 不一致（面板运行时用的是内置那份）：" +
			"请执行 `cp tools/system-services.sh internal/services/assets/system-services.sh`")
	}
	if !strings.Contains(embeddedSystemServices, "DEFAULT_FORMULAS=(nginx php@8.2 mysql@8.4)") {
		t.Error("内置脚本的默认 formula 没跟上（应默认 php@8.2）")
	}
	if !strings.Contains(embeddedSystemServices, "check_php_fpm") {
		t.Error("内置脚本必须按专属 socket 验证 php-fpm：只查 9000 会把装好的机器报成失败")
	}
	// MariaDB 与 MySQL 共用数据目录，但初始化命令不通用；端口复核也要认它，
	// 否则脚本会"注册了 mariadb 却不验证它"（2026-09-20 加 MariaDB 时补）。
	if !strings.Contains(embeddedSystemServices, "mariadb-install-db") ||
		!strings.Contains(embeddedSystemServices, "--auth-root-authentication-method=normal") {
		t.Error("内置脚本必须用 MariaDB 自己的初始化命令：" +
			"mysqld --initialize-insecure 在 MariaDB 上会失败")
	}
	if !strings.Contains(embeddedSystemServices, `check_port "mariadb" 3306`) {
		t.Error("内置脚本的端口复核必须认 mariadb（否则它的 3306 不会被验证）")
	}
}

// TestMaterializeSystemServicesScriptWritesExecutable：
// 内置脚本要能落到安装根目录（<WorkDir>/..），且是可执行、内容一致、幂等。
func TestMaterializeSystemServicesScriptWritesExecutable(t *testing.T) {
	m, _ := sandboxManager(t)
	ctx := context.Background()

	path, isTemp, err := m.materializeSystemServicesScript(ctx, &InstallResult{})
	if err != nil {
		t.Fatalf("落盘内置脚本失败: %v", err)
	}
	if isTemp {
		t.Fatalf("安装目录可写时不该退到临时文件：%s", path)
	}
	want := filepath.Join(filepath.Dir(filepath.Clean(m.opt.WorkDir)), "system-services.sh")
	if path != want {
		t.Errorf("脚本应落在安装根目录 %s，实际 %s", want, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("脚本没写出来: %v", err)
	}
	if string(b) != embeddedSystemServices {
		t.Error("落盘内容与内置脚本不一致")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o100 == 0 {
		t.Errorf("脚本必须可执行，实际权限 %v", st.Mode().Perm())
	}

	// 幂等：内容一致时不再写盘（mtime 不变）
	path2, _, err := m.materializeSystemServicesScript(ctx, &InstallResult{})
	if err != nil {
		t.Fatalf("第二次落盘失败: %v", err)
	}
	if path2 != path {
		t.Errorf("幂等时路径应一致：%s vs %s", path, path2)
	}
	st2, _ := os.Stat(path2)
	if !st2.ModTime().Equal(st.ModTime()) {
		t.Error("内容已经一致却又写了一次盘（应跳过）")
	}
}

// TestMaterializeSystemServicesScriptFallsBackToTemp：
// 拿不到安装根目录（WorkDir 为空）时必须退到临时文件，而不是报错 ——
// 注册服务不能让整个一键安装因为"路径推导不出来"而失败。
func TestMaterializeSystemServicesScriptFallsBackToTemp(t *testing.T) {
	m, _ := sandboxManager(t)
	m.opt.WorkDir = ""
	ctx := context.Background()

	path, isTemp, err := m.materializeSystemServicesScript(ctx, &InstallResult{})
	if err != nil {
		t.Fatalf("退到临时文件不该失败: %v", err)
	}
	if !isTemp {
		t.Fatalf("WorkDir 为空时应退到临时文件，实际 %s", path)
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("临时脚本不存在: %v", err)
	}
	if string(b) != embeddedSystemServices {
		t.Error("临时脚本内容与内置不一致")
	}
}

// TestSystemServicesScriptRunsWithoutHOME 是本次真机 bug 的回归测试。
//
// 真机现场（2026-09-17 mini）：面板是 LaunchDaemon（root）拉起的，环境里
// **没有 HOME**；而脚本开着 `set -u`，于是
//
//	/opt/zizpanel/system-services.sh: line 105: HOME: unbound variable
//
// 让整个注册以 exit 1 结束 —— 一键 LNMP 装完 nginx/php/mysql 却卡在最后一步，
// 服务管理里一条都没有，而报错只有那一句。单测当初没覆盖到，是因为它跑在
// 有 HOME 的 shell 里。
//
// 两条断言，确保这个坑不会回来：
//  1. 脚本里不许再出现裸 `$HOME`（唯一允许的是顶部的 `${HOME:-}` 兜底）；
//  2. 真的用 `env -i`（完全没有 HOME）跑一次 `--dry-run`：不碰系统，
//     但只要脚本在缺 HOME 时会崩，就一定复现。
func TestSystemServicesScriptRunsWithoutHOME(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("只验证 macOS（脚本用的是 dscl / launchctl）")
	}
	// 断言 1：静态扫描（只看代码行；注释里提到 $HOME 是为了记录这个坑）
	bad := regexp.MustCompile(`[^:{]\$HOME|\$HOME[^:}]`)
	for i, ln := range strings.Split(embeddedSystemServices, "\n") {
		code := strings.TrimSpace(ln)
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		if m := bad.FindString(code); m != "" {
			t.Errorf("脚本第 %d 行出现了没有兜底的 $HOME（%q）：LaunchDaemon 环境下 set -u 会让它当场退出。"+
				"请改用 dscl 查真实家目录，或写成 ${HOME:-}", i+1, m)
		}
	}

	// 断言 2：真的在"没有 HOME"的环境里跑一遍 --dry-run
	u, err := user.Current()
	if err != nil {
		t.Skipf("取不到当前用户: %v", err)
	}
	script := filepath.Join(t.TempDir(), "system-services.sh")
	if err := os.WriteFile(script, []byte(embeddedSystemServices), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/env", "-i",
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"SUDO_USER="+u.Username, // 让脚本能确定"真实用户"，不必依赖 /dev/console
		"/bin/bash", script, "--dry-run", "nginx")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if strings.Contains(text, "unbound variable") {
		t.Fatalf("脚本在没有 HOME 的环境里被 set -u 打断了（这正是真机上的一键安装失败原因）：\n%s", text)
	}
	if err != nil && !strings.Contains(text, "找不到可用的服务定义") {
		// 开发机没装 nginx 时脚本会以"没有服务定义"退出，那不是本测试要管的事
		t.Fatalf("脚本在无 HOME 环境下失败: %v\n%s", err, text)
	}
}

// TestSystemServicesScriptFindsUserWithoutSudoUser 锁住**无头机器上的真实用户推断**。
//
// 真机事故（mini 2026-09-17，坑 133）：面板自己就是 root（没有 sudo），无头 Mac
// 上 /dev/console 的属主又是 root —— 脚本只认 SUDO_USER 与 /dev/console 时会以
// "无法确定真实用户"退出，整批迁移全线失败（日志里七条失败一模一样），
// 而那正是这把脚本最主要的目标场景。现在按 SUDO_USER → ZIZPANEL_REAL_USER →
// USER/LOGNAME → 控制台属主 依次推断。
func TestSystemServicesScriptFindsUserWithoutSudoUser(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("只验证 macOS（脚本用的是 dscl / launchctl）")
	}
	u, err := user.Current()
	if err != nil {
		t.Skipf("取不到当前用户: %v", err)
	}
	script := filepath.Join(t.TempDir(), "system-services.sh")
	if err := os.WriteFile(script, []byte(embeddedSystemServices), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(user string) (string, error) {
		cmd := exec.Command("/usr/bin/env", "-i",
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			// 刻意**不设** SUDO_USER / USER / LOGNAME：面板以 root 被 launchd 拉起时
			// 环境里就是没有它们，而 /dev/console 的属主在无头机器上是 root。
			"ZIZPANEL_REAL_USER="+user,
			"/bin/bash", script, "--dry-run", "zizpanel-nonexistent-formula")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	text, _ := run(u.Username)
	if strings.Contains(text, "无法确定真实用户") {
		t.Fatalf("给了 ZIZPANEL_REAL_USER 仍报「无法确定真实用户」（无头机器上就是这个失败）：\n%s", text)
	}
	if !strings.Contains(text, "运行身份: "+u.Username) {
		t.Fatalf("脚本没有采用 ZIZPANEL_REAL_USER：\n%s", text)
	}

	// 用户不存在时必须**拒绝**：写进 plist 的 UserName 不存在时 launchd 会拒绝
	// 加载，而那时的报错完全指不到原因。
	badText, _ := run("zizpanel-no-such-user-xyz")
	if !strings.Contains(badText, "不存在") {
		t.Fatalf("传了不存在的用户却没有拒绝：\n%s", badText)
	}
}
