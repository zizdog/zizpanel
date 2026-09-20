package services

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/sites"
)

// 一键安装 LNMP（nginx + PHP-FPM + MySQL）：brew 装完只是包在，网站还不能用。
// 实测四个坑：nginx 默认 listen 8080；vhosts 目录与 include 不自动创建；
// MySQL 数据目录未初始化则拒绝启动；`brew services start` 装的是用户级
// LaunchAgent，无头开机不加载（坑 130）。收尾统一串在这里；
// 写系统 plist 复用 tools/system-services.sh（真机验证过）。

// LNMPFormulas 是**默认**三件套（nginx / PHP / **MariaDB**），顺序有意义：nginx 是入口。
// ⚠️ 语义已收窄：本次装什么由参数 LNMPSelection 承载；它只剩默认值来源（测试锁死）与
// 老调用方兼容入口。PHP 默认 **8.2**（用户 2026-09-17 要求），数据库默认 **MariaDB**
// （用户 2026-09-20 要求；MySQL 仍可选，但两者只能装一个，见 lnmp_engine.go）。
var LNMPFormulas = []string{"nginx", "php@8.2", "mariadb"}

// LNMPPorts 是**默认**三件套的判定端口，同样是默认值（本次端口见 LNMPSelection.Ports()）。
// 用端口而不是 HTTP：PHP-FPM 说 FastCGI、数据库说 MySQL 协议，过不了 HTTP 检查。
// PHP 的 0 有意义 —— php@x.y 监听专属 socket（sites.EnsureListen），留个假的 9000 只会误报。
var LNMPPorts = map[string]int{
	"nginx":   80,
	"php@8.2": 0,
	"mariadb": 3306,
}

// InstallLNMP 一键把 LNMP 环境装好并跑起来。sel 是**本次要装的三件套**，空选择当默认三件套。
// 必须显式传参而不是读全局 LNMPFormulas：逐包安装、PHP 端点闭环、系统级守护进程注册、
// 服务登记、人读文案**五条路径**都得用同一个选择（DEVELOPMENT.md 140/141）。
func (m *Manager) InstallLNMP(ctx context.Context, result *InstallResult, sel LNMPSelection) error {
	if result == nil {
		result = &InstallResult{App: "lnmp"}
	}
	// 空选择 = 默认三件套（向后兼容）；非空但非法的选择一律当场拒绝，
	// 绝不用默认值替用户做决定。
	if len(sel.Formulas()) == 0 {
		sel = DefaultLNMPSelection()
	}
	if err := sel.Validate(); err != nil {
		return fmt.Errorf("一键 LNMP 的版本选择无效：%w", err)
	}
	formulas := sel.Formulas()
	ports := sel.Ports()

	// ---- 0. 数据库引擎互斥护栏（必须在任何写操作之前）----
	// MariaDB 与 MySQL 默认共用数据目录 <brew>/var/mysql 与 3306：对方正在跑时
	// **拒绝**这次安装，绝不替用户停服务（那可能是他的生产库，见 lnmp_engine.go）。
	mysqlFormula := lnmpSelectedMySQL(formulas)
	if mysqlFormula != "" {
		if c := m.CheckDBEngineConflict(ctx, mysqlFormula); c.Blocked != "" {
			return fmt.Errorf("不能安装 %s：%s", mysqlFormula, c.Blocked)
		} else if c.Warning != "" {
			result.Warning = appendLNMPWarning(result.Warning, c.Warning)
			result.step(ctx, "警告："+c.Warning)
		}
	}

	// 全新 macOS 上没有 Homebrew，而 LNMP 三件套全靠它：缺什么装什么（含前置的 CLT），
	// 不再把用户赶回命令行。
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		result.step(ctx, "没有 Homebrew，先在面板里把它装上（含命令行开发者工具）")
		if err := m.EnsureHomebrew(ctx, result); err != nil {
			return err
		}
	}
	// 写 /Library/LaunchDaemons 需要 root：本地调试实例不是 root，明确拒绝而不是半途而废。
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 LNMP 需要以 root 运行（面板正式安装时由 LaunchDaemon 以 root 启动）")
	}

	result.step(ctx, "开始安装 LNMP 环境（"+sel.ComponentsText()+"）")

	// ---- 1. 逐包安装 ----
	if err := m.installLNMPPackages(ctx, result, formulas); err != nil {
		return err
	}

	// ---- 2. 本机约定的收尾工作 ----
	if m.brewHas(ctx, "nginx") {
		changed, err := m.fixNginxBaseConfig(ctx, result)
		if err != nil {
			// nginx 的基础配置修不好，后面站点功能一定是坏的：当致命错误，不"警告但继续"。
			return err
		}
		// 配置改了但进程还在用旧配置 = "改了不生效"（文件在、服务在、就是不生效）。
		// 只在 nginx **已经在跑**时重载它；没在跑就不在这里拉起 —— 那会先起一个游离
		// 进程，随后系统级 LaunchDaemon 反而 bind 不上 80（真机上刚踩过这个连环坑）。
		if changed {
			if st, _ := priv.NginxStatus(); st == "running" {
				if rerr := priv.NginxReload(); rerr != nil {
					msg := "nginx.conf 已更新，但重载 nginx 失败：" + rerr.Error() +
						"（新配置尚未生效；可在「服务管理」里重启 nginx）"
					result.Warning = appendLNMPWarning(result.Warning, msg)
					result.step(ctx, "警告："+msg)
				} else {
					result.step(ctx, "已重载 nginx，基础配置改动立即生效")
				}
			}
		}
	}
	// ---- 2c. nginx 要写的日志目录必须归属真实用户 ----
	//
	// 真机现场（2026-09-17 mini）：nginx 以**真实用户**运行，而 <家目录>/www/_logs/proxy/
	// 是面板早先以 root 建出来的 —— 日志打不开，[emerg] open() ... Permission denied，
	// 整个 80 起不来。启动服务**之前**把这棵树交还真实用户（失败只告警，不阻断）。
	m.ensureNginxLogOwnership(ctx, result)

	// ---- 2a. 数据库数据目录 ----
	//
	// 按**本次选的数据库 formula** 判断：写死版本时用户选了别的版本就会静默跳过
	// （数据目录没初始化 → 服务起不来）。初始化命令本身也是引擎相关的（见该函数）。
	if mysqlFormula != "" && m.brewHas(ctx, mysqlFormula) {
		if err := m.initMySQLDataDir(ctx, result, mysqlFormula); err != nil {
			return err
		}
	}

	// ---- 2b. PHP 端点闭环（必须在注册系统级服务**之前**，否则会和别的版本抢 9000）----
	// 复用 php_endpoint.go 的 ensurePHPListenEndpoint；少这一步只在站点上表现为"选了 8.4
	// 跑的却是 8.3"，联想不到安装路径。失败不当致命错误，但必须如实进 Warning。
	m.ensureLNMPPHPEndpoints(ctx, result, formulas)

	// ---- 3. 注册为系统级守护进程（不依赖用户登录）----
	// 安全前提：**已经在跑的服务不要动**（这一步会摘掉用户级 agent、改写 plist、重建
	// launchd 注册，实测能把调好的服务搞成失败的）。先看端口，都在监听就跳过。
	if m.allLNMPRunning(ctx, formulas) {
		result.Steps = append(result.Steps,
			sel.ComponentsText()+" 均已在运行，跳过服务注册（不改动现有配置）")
	} else {
		// 注册之前先清场：端口被"不在 launchd 里"的游离进程占着时，bootstrap 出来的
		// 守护进程会因 Address already in use 反复退出，而端口检查照样显示"在监听" ——
		// 面板说一切正常，托管的其实是坏的（真机现场：mini 的 80 上就有一个游离 nginx）。
		m.freeStaleNginxPort(ctx, result)

		result.step(ctx, "正在注册为系统级后台服务（不依赖用户登录）")
		if err := m.installSystemDaemons(ctx, result, formulas); err != nil {
			// 兜底：上面可能刚把游离的 nginx 停掉，这里再失败机器就"连原来那个能用的
			// nginx 都没了"。尽力把它拉回来并如实说明。
			m.restoreNginxAfterFailure(ctx, result, err)
			return err
		}
	}

	// ---- 回流：登记进面板的服务注册表。必须在 allLNMPRunning 分支**之后**且不在分支里：
	// 跳过分支跳过的是改 plist、不是登记，"都在跑"的机器才最需要登记；也要早于端口验证的
	// 提前 return。失败不当致命错误，但绝不谎报"已登记"。
	m.registerLNMPComponents(ctx, result, formulas, ports)

	// ---- 3b. 基础依赖 ffmpeg（用户明确要求，tts 要用）；与单应用安装共用 Ensure ----
	// ffmpeg 不参与 LNMP 运行，装不上不该让整个任务失败（会掩盖"LNMP 其实已经好了"）：
	// 只写 Warning 与手工命令，绝不谎报"基础依赖已就绪"。
	if err := m.EnsureBaseDependencies(ctx, result); err != nil {
		result.Warning = appendLNMPWarning(result.Warning, "基础依赖（ffmpeg）补装失败："+err.Error())
		result.step(ctx, "警告："+result.Warning)
	}

	// ---- 4. 验证：只认端点/端口真的在监听 ----
	// PHP 不能用端口判定（每个 php@x.y 监听专属 Unix socket，sel.Ports() 里记 0），统一走
	// lnmpComponentLive，避免"验证说 PHP 没在监听、其实它好好的"这种误报。
	result.step(ctx, "正在验证服务是否真的可用")
	var notUp []string
	var upDesc []string
	for _, f := range formulas {
		label, live := m.waitLNMPComponent(ctx, f, ports, 20*time.Second)
		if !live {
			notUp = append(notUp, label)
			continue
		}
		upDesc = append(upDesc, label)
	}
	if len(notUp) > 0 {
		result.Warning = appendLNMPWarning(result.Warning,
			"以下服务未能确认在监听："+strings.Join(notUp, "、")+
				"。可到「服务管理」逐个查看日志。")
		result.step(ctx, "警告："+result.Warning)
		return nil
	}
	result.step(ctx, strings.Join(upDesc, " / ")+" 均已在监听")

	// ---- 4b. 默认站点（用户 2026-09-17 明确要求）----
	// 放收尾阶段：nginx/PHP 已起来，能把真在监听的端点写进 fastcgi_pass，也不被
	// phpMyAdmin 的失败连累；幂等，旧机器重跑即可补齐。
	result.step(ctx, "正在确保默认站点（http://localhost/ 与 http://<本机IP>/ 可直接访问）")
	if err := m.ensureDefaultVhost(ctx, result); err != nil {
		result.Warning = appendLNMPWarning(result.Warning,
			"默认站点未能就绪："+err.Error()+"（可在「设置 → 整理默认站点」里重试）")
		result.step(ctx, "警告："+result.Warning)
	}

	// ---- 5. phpMyAdmin ----
	// 放最后（依赖 nginx 与 PHP-FPM 都已就绪），失败不阻断整个 LNMP。
	// 没选数据库就跳过（只会得到一个永远登录不上的入口，比没装更糟），但要如实说明。
	if mysqlFormula == "" {
		result.step(ctx, "本次没有选择数据库，跳过 phpMyAdmin（数据库管理界面）的部署")
	} else {
		result.step(ctx, "正在部署 phpMyAdmin（数据库管理界面）")
		if err := m.InstallPhpMyAdmin(ctx, result); err != nil {
			result.Warning = appendLNMPWarning(result.Warning,
				"LNMP 已就绪，但 phpMyAdmin 部署失败："+err.Error())
			result.step(ctx, "警告："+result.Warning)
		}
	}

	// ---- 6. root 凭据闭环（必须有，见 lnmp_mysql_credentials.go）----
	// 放最后：失败＝任务失败（凭据不一致会让之后每次库操作都撞 1045），但前面装好的不报废。
	// 没选数据库就没有这一步，绝不因此报"凭据核对失败"。
	if mysqlFormula != "" {
		result.step(ctx, "正在核对 "+dbEngineFormulaDisplay(mysqlFormula)+" root 凭据（面板配置与服务器是否一致）")
		if err := m.ensureMySQLRootCredential(ctx, result); err != nil {
			return err
		}
	}
	return nil
}

// installLNMPPackages 逐个 brew 安装本次选中的 formula（顺序由调用方给定，nginx 在前）。
//
// 单独成函数是为了可测试：InstallLNMP 第一件事就要求 root，单测到不了这个循环；
// 而"选了 MariaDB 却去装 mysql@8.4"正是最该被门禁钉住的一类错。
func (m *Manager) installLNMPPackages(ctx context.Context, result *InstallResult, formulas []string) error {
	for _, f := range formulas {
		if m.brewHas(ctx, f) {
			result.step(ctx, f+" 已安装，跳过")
			continue
		}
		result.Steps = append(result.Steps,
			fmt.Sprintf("正在 brew install %s（国内镜像下通常几分钟）", f))
		if _, err := m.brewInstall(ctx, result, 40*time.Minute, f); err != nil {
			// 失败要能让用户自己救：给出手工命令与镜像提示，不只抛一句 "brew install 失败"。
			return fmt.Errorf("安装 %s 失败: %w；可在终端手工重试 `brew install %s`"+
				"（若下载很慢，面板已优先走镜像站，重跑本任务会继续用镜像）", f, err, f)
		}
		result.step(ctx, f+" 安装完成")
	}
	return nil
}

// lnmpSelectedMySQL 从本次选择的 formula 列表里挑出 MySQL 那个（没有则空串）。
// 单独成函数是为了让"哪个是 MySQL"只有一处判据：lnmpGroupOf 的 mysql@ 前缀。
func lnmpSelectedMySQL(formulas []string) string {
	for _, f := range formulas {
		if g, ok := lnmpGroupOf(f); ok && g.Key == "mysql" {
			return f
		}
	}
	return ""
}

// fixNginxBaseConfig 修正 brew 默认 nginx 配置（8080→80、建 vhosts 目录、include vhosts/*.conf；
// 缺了这些站点与面板入口都不通）。
// 返回"文件是否真的被改动"：改了不 reload 就是不生效。
func (m *Manager) fixNginxBaseConfig(ctx context.Context, result *InstallResult) (bool, error) {
	conf := filepath.Join(m.brewPrefix(), "etc", "nginx", "nginx.conf")
	data, err := os.ReadFile(conf)
	if err != nil {
		return false, fmt.Errorf("读取 nginx.conf 失败: %w", err)
	}
	text := string(data)
	orig := text

	if strings.Contains(text, "listen       8080;") {
		text = strings.Replace(text, "listen       8080;", "listen       80;", 1)
		result.step(ctx, "已把 nginx 默认端口 8080 改为 80")
	}

	vhostDir := filepath.Join(m.brewPrefix(), "etc", "nginx", "vhosts")
	if err := os.MkdirAll(vhostDir, 0o755); err != nil {
		return false, fmt.Errorf("创建 vhosts 目录失败: %w", err)
	}
	// 目录归属真实用户：nginx 以该用户运行，面板也要能往里写
	if m.opt.UserName != "" {
		_ = chownTo(m.opt.UserName, vhostDir)
	}

	if !strings.Contains(text, "vhosts/*.conf") {
		// 插到最后一个 } 之前（http{} 块末尾）
		i := strings.LastIndex(strings.TrimRight(text, "\n"), "}")
		if i < 0 {
			return false, fmt.Errorf("nginx.conf 结构异常（找不到 http 块结束符），拒绝修改")
		}
		insert := "\n    # 由 ZizPanel 添加：加载站点配置\n" +
			"    include " + vhostDir + "/*.conf;\n"
		text = text[:i] + insert + text[i:]
		result.step(ctx, "已在 nginx.conf 中启用 vhosts 目录")
	}

	if text != orig {
		// 改之前先备份一次，方便用户对照/回退
		if _, err := os.Stat(conf + ".zizpanel.bak"); err != nil {
			_ = os.WriteFile(conf+".zizpanel.bak", []byte(orig), 0o644)
		}
		// 原子写入：配置写到一半会直接让 nginx 起不来
		tmp := conf + ".tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return false, fmt.Errorf("写入 nginx.conf 失败: %w", err)
		}
		if err := os.Rename(tmp, conf); err != nil {
			return false, fmt.Errorf("替换 nginx.conf 失败: %w", err)
		}
	}
	result.step(ctx, "nginx 基础配置已就绪")
	return text != orig, nil
}

// initMySQLDataDir 在数据目录为空时初始化数据库，命令**按引擎**选：
//   - MySQL 8.4：`mysqld --initialize-insecure`（root 空口令）；
//   - MariaDB：`mariadb-install-db`（MariaDB 不认 --initialize-insecure；
//     --auth-root-authentication-method=normal 也是要 root 空口令，面板随后设口令）。
//
// 二进制走 <brew>/opt/<formula>/bin（keg-only 的 mysql@8.4 不在 <brew>/bin 下，
// 写 <brew>/bin/mysqld 会得到一个"莫名其妙初始化失败"——真机上跑着的就是
// /opt/homebrew/opt/mysql@8.4/bin/mysqld）。
//
// 必须降权到真实用户：数据库服务拒绝以 root 运行，而且数据目录的归属必须与后续
// 运行身份一致，否则启动时会报权限错误。
func (m *Manager) initMySQLDataDir(ctx context.Context, result *InstallResult, formula string) error {
	name := dbEngineFormulaDisplay(formula)
	datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
	if result != nil {
		result.mysqlFormula = formula
	}
	if entries, err := os.ReadDir(datadir); err == nil && len(entries) > 0 {
		return nil // 已经初始化过
	}
	result.step(ctx, name+" 数据目录未初始化，正在初始化（root 初始无口令，随后会设置一个随机口令并记进面板配置）")
	if err := os.MkdirAll(datadir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 数据目录失败: %w", name, err)
	}
	if m.opt.UserName != "" {
		_ = chownTo(m.opt.UserName, datadir)
	}
	exe, args := mysqlInitCommand(m.brewPrefix(), formula, datadir)
	if _, err := m.runAsUser(ctx, 5*time.Minute, exe, args...); err != nil {
		return fmt.Errorf("%s 初始化失败: %w", name, err)
	}
	result.step(ctx, name+" 数据目录初始化完成")
	// 只有本次初始化出来的库，面板才敢设随机口令；已有数据目录的机器不动用户的口令。
	result.mysqlFreshInit = true
	return nil
}

// mysqlInitCommand 返回"初始化数据目录"的命令（**按引擎选**）。
//
// 单独抽出来是为了能被门禁钉住：MySQL 用 `mysqld --initialize-insecure`，
// MariaDB 用 `mariadb-install-db`（MariaDB 不认 --initialize-insecure，
// 拿 MySQL 的命令去跑只会得到一个看不懂的失败）。
func mysqlInitCommand(brewPrefix, formula, datadir string) (string, []string) {
	optDir := filepath.Join(brewPrefix, "opt", formula)
	binDir := filepath.Join(optDir, "bin")
	if dbEngineOfFormula(formula) == "mariadb" {
		return filepath.Join(binDir, "mariadb-install-db"), []string{
			"--no-defaults", "--basedir=" + optDir, "--datadir=" + datadir,
			"--auth-root-authentication-method=normal",
		}
	}
	return filepath.Join(binDir, "mysqld"), []string{"--initialize-insecure", "--datadir=" + datadir}
}

// ensureLNMPPHPEndpoints 对**本次选中的** PHP 版本做专属端点闭环（只对 php@x.y 生效，
// nginx/mysql 没有 www.conf）。单独一个函数是为了让"端点闭环必须在注册系统级服务之前"
// 这条顺序约束能在源码级被测试锁住 —— 见 TestInstallLNMPCallsSharedPHPListenFix。
func (m *Manager) ensureLNMPPHPEndpoints(ctx context.Context, result *InstallResult, formulas []string) {
	for _, f := range formulas {
		if _, ok := phpVersionFromFormula(f); !ok {
			continue
		}
		if !m.brewHas(ctx, f) {
			continue
		}
		if err := m.ensurePHPListenEndpoint(ctx, f, result); err != nil {
			result.step(ctx, "警告："+f+" 的专属端点未就绪，指向它的站点会解析不了 PHP"+
				"（原因见上一行；可在「网站管理 → 🐘 PHP 环境」里点「修复端点并重启」重试）")
		}
	}
}

// installSystemDaemons 把**本次选中的** LNMP 组件注册成系统级 LaunchDaemon。
// formulas 来自本次选择（nginx 在前）：注册成 8.2 会让"装的是 8.4、开机起来的是 8.2"。
func (m *Manager) installSystemDaemons(ctx context.Context, result *InstallResult, formulas []string) error {
	return m.installSystemDaemonsFor(ctx, result, formulas, 10*time.Minute)
}

// installSystemDaemonsFor 把**任意** formula 列表注册成系统级 LaunchDaemon（一键 LNMP、
// postgresql/ollama/php@8.x 等单装应用都要）：无头 macOS 开机不加载 ~/Library/LaunchAgents
// （坑 130），常驻服务必须落到系统域。复用真机验证过的脚本，不再写一份 Go 版。
func (m *Manager) installSystemDaemonsFor(ctx context.Context, result *InstallResult,
	formulas []string, timeout time.Duration) error {
	if len(formulas) == 0 {
		return fmt.Errorf("没有要注册的服务（formula 列表为空）")
	}
	// 脚本**内置在二进制里**（见 system_services.go）：在线升级只换二进制、不发 tools/**，
	// "从磁盘找脚本"在升级过的机器上必然失败（mini 上就这么卡住）。先从内置内容落盘再执行。
	script, isTemp, err := m.materializeSystemServicesScript(ctx, result)
	if err != nil {
		return err
	}
	if isTemp {
		defer func() { _ = os.Remove(script) }()
	}
	args := append([]string{script}, formulas...)
	// 显式带上 HOME/USER：LaunchDaemon 启动的面板**没有 HOME**，脚本里 `$HOME` 在 set -u
	// 下会当场退出（真机踩过：HOME: unbound variable → 一键安装卡在最后一步）。脚本自己也
	// 补了兜底，这里再给一份是防御纵深。
	env := []string{}
	if m.opt.UserHome != "" {
		env = append(env, "HOME="+m.opt.UserHome)
	}
	if m.opt.UserName != "" {
		env = append(env, "USER="+m.opt.UserName, "LOGNAME="+m.opt.UserName)
		// 脚本优先看 SUDO_USER，而面板**自己就是 root**（没有 sudo），无头机器上 /dev/console
		// 的属主也是 root —— 会得到"无法确定真实用户"（mini 真机实测：整批迁移全线失败）。
		env = append(env, "ZIZPANEL_REAL_USER="+m.opt.UserName)
	}
	out, err := m.runRootEnv(ctx, timeout, env, "/bin/bash", args...)
	if err != nil {
		// 带上脚本真实输出：只回一句"注册失败"对用户毫无帮助（"报错看不懂"就是这么来的）。
		return fmt.Errorf("注册系统级服务失败: %v（脚本输出：%s）", err, tailText(stripANSI(out), 1500))
	}
	result.step(ctx, "已注册为系统级 LaunchDaemon（开机自启，不依赖登录）")
	return nil
}

// registerLNMPComponents 把**本次选中的**组件登记进面板的服务注册表：端口/版本必须来自
// 本次选择（读默认表会让服务管理里的记录对不上）。复用 RegisterInstalledService（幂等），
// 不另写一套入库逻辑；过去只注册 LaunchDaemon 不写表，真机上表现为"装好了但列表里没有"。
func (m *Manager) registerLNMPComponents(ctx context.Context, result *InstallResult,
	formulas []string, ports map[string]int) {
	result.step(ctx, "正在把 "+lnmpComponentsTextOf(formulas)+" 登记进「服务管理」")
	for _, formula := range formulas {
		// 目录条目是端口/分类/图标/显示名的权威来源；查不到时用 formula 兜底。
		name, icon, category, port := formula, "🧩", "lnmp", ports[formula]
		if a, ok := lnmpCatalogApp(formula); ok {
			if a.Name != "" {
				name = a.Name
			}
			if a.Icon != "" {
				icon = a.Icon
			}
			if a.Category != "" {
				category = a.Category
			}
			if p := a.WebPort(); p > 0 {
				port = p
			}
		}

		label := m.lnmpComponentLabel(formula)
		if label == "" {
			msg := fmt.Sprintf("登记失败：找不到 %s 的 launchd 服务定义（服务可能不在 launchd 里），"+
				"可在「应用 → 已安装」里点「+ 注册服务」把它加入面板", formula)
			result.step(ctx, msg)
			result.Warning = appendLNMPWarning(result.Warning, msg)
			continue
		}
		if err := m.RegisterInstalledService(ctx, label, name, icon, category, port); err != nil {
			msg := fmt.Sprintf("登记失败：%s（%s）没能加入「服务管理」：%v；"+
				"服务本身已装好，可在应用市场对应条目上点「添加到面板」补登记", formula, label, err)
			result.step(ctx, msg)
			result.Warning = appendLNMPWarning(result.Warning, msg)
			continue
		}
		result.step(ctx, fmt.Sprintf("%s 已登记进「服务管理」（%s）", name, label))
	}
}

// lnmpComponentLabel 找出某个 formula 在本机 launchd 里的**真实**标签：不能信目录里写死的
// ServiceLabel（实测同机混用 `homebrew.mxcl.*` 与 `sh.brew.*`，系统级还可能是 cn.zizdog.*），
// 标签用错＝指向不存在的 plist，显示成"已登记但查不到"的僵尸条目。找不到时退回目录标签。
func (m *Manager) lnmpComponentLabel(formula string) string {
	if label := BrewLabelFor(m.opt.UserHome, formula); label != "" {
		return label
	}
	if a, ok := lnmpCatalogApp(formula); ok {
		return a.ServiceLabel
	}
	return ""
}

// lnmpCatalogApp 按 brew formula 在应用目录里找条目。
// 不写死 ID（nginx / php83 / mysql84）映射：条目 ID 与 formula 不是一回事，写死会在目录
// 改 ID 时静默失效；按 BrewFormula 反查则加条目、改 ID 都不用动这里。
func lnmpCatalogApp(formula string) (App, bool) {
	for _, a := range Catalog() {
		if a.BrewFormula != "" && a.BrewFormula == formula {
			return a, true
		}
	}
	return App{}, false
}

// appendLNMPWarning 追加告警而不是覆盖：直接赋值会把前面"某个组件没登记上"从 Warning
// 里抹掉，用户只看摘要就会以为什么都成功了 —— 与"不谎报成功"的要求冲突。
func appendLNMPWarning(cur, add string) string {
	if cur == "" {
		return add
	}
	return cur + "；" + add
}

// lnmpComponentsTextOf 生成"nginx / PHP 8.4 / MySQL 8.4"这种人看的组件清单。
// 由**本次选择**推导（formula → 目录条目展示名），不写死字符串也不读全局默认值 ——
// 写死的日志会变成"日志说 8.2、装的是 8.4"；目录查不到退回 formula 全名。
func lnmpComponentsTextOf(formulas []string) string {
	parts := make([]string, 0, len(formulas))
	for _, f := range formulas {
		if a, ok := lnmpCatalogApp(f); ok && a.Name != "" {
			parts = append(parts, a.Name)
			continue
		}
		parts = append(parts, f)
	}
	return strings.Join(parts, " / ")
}

// ---- 小工具 ----

func (m *Manager) brewPrefix() string {
	return filepath.Dir(filepath.Dir(m.opt.BrewBin))
}

// allLNMPRunning 判断**本次选中的**组件是否都已在监听（PHP 按专属端点判断）。
// 环境已经能用时动 plist 只有坏处；判定口径必须与最终验证一致（都走 lnmpComponentLive）。
func (m *Manager) allLNMPRunning(ctx context.Context, formulas []string) bool {
	for _, f := range formulas {
		if _, live := m.lnmpComponentLive(ctx, f, nil); !live {
			return false
		}
	}
	return true
}

// lnmpComponentLive 判断组件当前是否真的可用：PHP 走面板分配的**专属 Unix socket**，
// 必须按 sites.EndpointLive 判定（用端口必误报）；其它按判定端口连一次，不走 HTTP。
// ports 为 nil 时按 formula 前缀推导，**不读全局 LNMPPorts**（选了 mysql@9.0 会假失败）。
func (m *Manager) lnmpComponentLive(ctx context.Context, formula string, ports map[string]int) (string, bool) {
	if version, ok := phpVersionFromFormula(formula); ok {
		endpoint, err := sites.PreferredEndpoint(m.phpBrewPrefix(), version)
		if err != nil {
			return fmt.Sprintf("PHP %s（端点未分配：%v）", version, err), false
		}
		return fmt.Sprintf("PHP %s(%s)", version, endpoint), sites.EndpointLive(endpoint)
	}
	port, ok := ports[formula]
	if !ok {
		group, known := lnmpGroupOf(formula)
		if !known {
			// 既不是 PHP、又不属于 LNMP 三件套：不能假装它在跑。
			return fmt.Sprintf("%s（没有可判定的端口）", formula), false
		}
		port = group.Port
	}
	if port <= 0 {
		// 组件没给判定端口：不能假装它在跑（PHP 那条分支已经在上面处理了）。
		return fmt.Sprintf("%s（没有可判定的端口）", formula), false
	}
	return fmt.Sprintf("%s(端口 %d)", formula, port), portOpen(ctx, port)
}

// waitLNMPComponent 轮询等某个组件就绪（服务启动需要时间，立即检查必然失败）。
func (m *Manager) waitLNMPComponent(ctx context.Context, formula string,
	ports map[string]int, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var label string
	for {
		var live bool
		label, live = m.lnmpComponentLive(ctx, formula, ports)
		if live {
			return label, true
		}
		if time.Now().After(deadline) {
			return label, false
		}
		select {
		case <-ctx.Done():
			return label, false
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// freeStaleNginxPort 注册前收掉"占着 80 但不在 launchd 里"的游离 nginx：不收会让 launchd
// 里的 nginx 因 "Address already in use" 反复退出，而端口检查照样显示"80 在监听"（谎报正常）。
// 安全边界：占用者必须**每一个**都认成 nginx 才动手，否则只告警；停了没释放也只告警。
func (m *Manager) freeStaleNginxPort(ctx context.Context, result *InstallResult) {
	if !m.brewHas(ctx, "nginx") || !portOpen(ctx, 80) {
		return
	}
	info, err := priv.CheckPort("80")
	if err != nil || !info.InUse || len(info.Holders) == 0 {
		return
	}
	// 判定标准是"**launchd 托管的那个实例自己**占着 80"，不是"磁盘上有没有 plist"。
	//
	// 真机现象（2026-09-17 mini）：僵尸 plist 还在、进程早退出，80 上仍是兜底 root nginx；
	// 只看 plist 会以为"已托管、不用管"，新起的系统级 nginx 永远抢不到 80（界面还显示 running）。
	// 单测抓不到这个组合，只能锁住判定函数本身（holdersContainPID）。
	if m.nginxLaunchdInstanceHolds(info.Holders) {
		return // 托管的实例自己占着 80：正常，不动
	}
	if !holdersAllMatch(info.Holders, "nginx") {
		msg := fmt.Sprintf("端口 80 被非 nginx 的进程占用（%s），面板没有自动处理它："+
			"请先确认这是什么程序，腾出 80 后重跑本任务，否则 nginx 起不来",
			strings.Join(info.Holders, ", "))
		result.Warning = appendLNMPWarning(result.Warning, msg)
		result.step(ctx, "警告："+msg)
		return
	}
	result.step(ctx, "端口 80 上是未纳入 launchd 的 nginx 进程（"+strings.Join(info.Holders, ", ")+
		"），正在停掉它以便系统级服务接管")
	if err := priv.NginxStop(); err != nil {
		msg := "停止游离的 nginx 失败：" + err.Error() +
			"；系统级 nginx 可能因端口被占而起不来，可在终端确认后手工停止它"
		result.Warning = appendLNMPWarning(result.Warning, msg)
		result.step(ctx, "警告："+msg)
		return
	}
	if !waitPortClosed(ctx, 80, 15*time.Second) {
		msg := "已要求停止游离的 nginx，但 80 端口仍被占用；系统级 nginx 可能起不来，" +
			"请到「服务管理」查看它的日志"
		result.Warning = appendLNMPWarning(result.Warning, msg)
		result.step(ctx, "警告："+msg)
		return
	}

	// 清掉它可能留下的 pid 文件（root 建的 0644）：以**真实用户**运行的系统级 nginx 写不进
	// 去就会启动失败，报的却是 `open() ".../nginx.pid" failed (13: Permission denied)`。
	// 只在进程确实已经退出时才删（还有 nginx 活着就绝不能动它）。
	if st, _ := priv.NginxStatus(); st == "stopped" {
		if pidFile := m.nginxPIDFilePath(); pidFile != "" {
			if _, err := os.Stat(pidFile); err == nil {
				if err := os.Remove(pidFile); err == nil {
					result.step(ctx, "已清掉旧 nginx 留下的 pid 文件 "+pidFile+
						"（它属于 root，以真实用户运行的新 nginx 写不进去）")
				}
			}
		}
	}

	result.step(ctx, "已停掉游离的 nginx，80 端口已释放（紧接着由系统级服务重新监听）")
}

// nginxPIDFilePath 从 nginx.conf 里读 pid 指令，读不到就退回 brew 的默认路径。
// 不写死是因为用户可以自定义位置 —— 而清错文件比不清更糟。
var reNginxPID = regexp.MustCompile(`(?m)^\s*pid\s+([^;\s]+)\s*;`)

func (m *Manager) nginxPIDFilePath() string {
	conf := filepath.Join(m.brewPrefix(), "etc", "nginx", "nginx.conf")
	if b, err := os.ReadFile(conf); err == nil {
		if mm := reNginxPID.FindStringSubmatch(string(b)); mm != nil {
			if p := strings.Trim(mm[1], `"'`); filepath.IsAbs(p) {
				return p
			}
		}
	}
	return filepath.Join(m.brewPrefix(), "var", "run", "nginx.pid")
}

// restoreNginxAfterFailure：installSystemDaemons 失败时的兜底。上面可能刚停掉游离的
// nginx，注册又失败机器就"完全没有 nginx"了 —— 尽力拉起并如实写明，绝不静默留 80 上没人的现场。
func (m *Manager) restoreNginxAfterFailure(ctx context.Context, result *InstallResult, cause error) {
	if !m.brewHas(ctx, "nginx") || portOpen(ctx, 80) {
		return
	}
	result.step(ctx, "注册系统级服务失败，正在把 nginx 恢复到可用状态（"+cause.Error()+"）")
	if err := priv.NginxStart(); err != nil {
		result.Warning = appendLNMPWarning(result.Warning,
			"注册系统级服务失败，且 nginx 未能重新启动："+err.Error()+
				"；请到终端执行 `sudo brew services start nginx` 或重跑本任务")
		return
	}
	result.step(ctx, "nginx 已恢复监听 80（但仍是未纳入 launchd 的进程，请重跑本任务完成注册）")
}

// nginxLaunchdInstanceHolds 判断"占着 80 的那个进程"是不是 launchd 托管的实例：取磁盘上
// 真实存在的 label，查 launchd 里该服务的 PID，再看端口占用者里有没有它。服务已加载但没
// 进程（上一轮失败留下的僵尸 plist）时 PID=0 → false，游离进程会被正常接管。
func (m *Manager) nginxLaunchdInstanceHolds(holders []string) bool {
	label := BrewLabelFor(m.opt.UserHome, "nginx")
	if label == "" {
		// 兜底：面板 install.sh 用的系统标签
		if fileExists(filepath.Join("/Library/LaunchDaemons", priv.NginxLaunchLabel+".plist")) {
			label = priv.NginxLaunchLabel
		}
	}
	if label == "" {
		return false
	}
	st, err := priv.LaunchStatus(label)
	if err != nil || st.PID <= 0 {
		return false
	}
	return holdersContainPID(holders, st.PID)
}

// holdersContainPID 判断端口占用者列表里有没有指定 pid（Holders 形如 ["nginx (pid 74181)"]）。
func holdersContainPID(holders []string, pid int) bool {
	if pid <= 0 {
		return false
	}
	needle := fmt.Sprintf("pid %d)", pid)
	for _, h := range holders {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

// holdersAllMatch 判断端口占用者是否"每一个"都是期望的程序名。
// 对不上就返回 false —— 调用方只告警不动手：宁可留着问题让人看，也不误杀别的进程。
func holdersAllMatch(holders []string, want string) bool {
	if len(holders) == 0 {
		return false
	}
	want = strings.ToLower(want)
	for _, h := range holders {
		name := strings.ToLower(strings.TrimSpace(strings.SplitN(h, "(", 2)[0]))
		if name == "" || !strings.Contains(name, want) {
			return false
		}
	}
	return true
}

// waitPortClosed 轮询等端口释放（stop 之后端口不会立刻可用）。
func waitPortClosed(ctx context.Context, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !portOpen(ctx, port) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
	return !portOpen(ctx, port)
}

// ensureNginxLogOwnership 把 nginx 需要写的日志目录（递归）交给真实用户。
//
// 面板（root）写站点配置时会建出 root 属主的 <家目录>/www/_logs，而 nginx 以**真实用户**
// 运行 → access_log 打不开、连启动都失败（真机 [emerg] Permission denied，整机 80 全没）。
// 只处理两棵树，绝不递归整个 www；失败只告警，但必须让用户看得见。
func (m *Manager) ensureNginxLogOwnership(ctx context.Context, result *InstallResult) {
	if !m.brewHas(ctx, "nginx") {
		return
	}
	user := strings.TrimSpace(m.opt.UserName)
	if user == "" || user == "root" {
		return
	}
	// 网站根目录本身也要归属真实用户（**不递归**，各站点目录各有归属）。
	// 真机现场（2026-09-17 mini）：www 是 root 属主时用户在自己的网站根下建不了目录、传不了
	// 文件（面板能建），看起来像"面板把目录锁了"。这里只改 www 这一层，不动里面的站点。
	if m.opt.UserHome != "" {
		wwwRoot := filepath.Join(m.opt.UserHome, "www")
		if st, err := os.Stat(wwwRoot); err == nil && st.IsDir() {
			_ = chownTo(user, wwwRoot)
		}
	}
	for _, root := range m.nginxLogRoots() {
		if _, err := os.Stat(root); err != nil {
			continue // 不存在就不用管（父目录可写时 nginx 自己会建）
		}
		changed, total, err := chownTreeTo(user, root)
		if err != nil {
			msg := "把 nginx 日志目录 " + root + " 的属主改为 " + user + " 失败：" + err.Error() +
				"；nginx 可能因为写不进日志而起不来（可手工 `sudo chown -R " + user + " " + root + "`）"
			result.Warning = appendLNMPWarning(result.Warning, msg)
			result.step(ctx, "警告："+msg)
			continue
		}
		if changed > 0 {
			result.step(ctx, fmt.Sprintf("已把 nginx 日志目录 %s 的属主改回 %s（%d/%d 项）",
				root, user, changed, total))
		}
	}
}

// chownTreeTo 递归把一棵树的属主改成指定用户（尽力而为，不跟随符号链接）。
// 返回改动项数与遍历项数；单项失败不中断（可能有正当的其它属主），错误如实带回调用方。
func chownTreeTo(user, root string) (changed, total int, err error) {
	uid, uerr := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-u", user)))
	gid, gerr := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-g", user)))
	if uerr != nil || gerr != nil {
		return 0, 0, fmt.Errorf("查不到用户 %s 的 uid/gid", user)
	}
	if _, serr := os.Stat(root); serr != nil {
		return 0, 0, serr // 根不存在/不可读：如实报，调用方据此跳过
	}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // 子条目读不动就跳过，不影响其它
		}
		total++
		if fi, ierr := d.Info(); ierr == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				if int(st.Uid) == uid && int(st.Gid) == gid {
					return nil
				}
			}
		}
		if cerr := os.Chown(path, uid, gid); cerr == nil {
			changed++
		}
		return nil
	})
	return changed, total, walkErr
}

// stripANSI 去掉终端颜色转义：子脚本是给人看的、会输出颜色码，原样塞进 JSON 会让界面显示
// 一堆 [1m[34m 之类的噪声。
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= '@' && s[j] <= '~') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// waitPort 轮询等端口起来。服务启动需要时间，立即检查必然失败。
func waitPort(ctx context.Context, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portOpen(ctx, port) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(400 * time.Millisecond):
		}
	}
	return false
}

// runAsUser 以真实用户身份执行命令（brew / mysqld 都拒绝 root）。
func (m *Manager) runAsUser(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{"-n", "-u", m.opt.UserName, name}, args...)
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", full...)
	// brew 需要正确的 HOME 才能找到 Cellar 与缓存
	if m.opt.UserHome != "" {
		cmd.Env = append(os.Environ(), "HOME="+m.opt.UserHome)
	}
	return streamCmd(ctx, cmd)
}

// runRoot 以 root 执行命令（面板本身通常就是 root）。
func (m *Manager) runRoot(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return m.runRootEnv(ctx, timeout, nil, name, args...)
}

// runRootEnv 同上，但显式追加环境变量。
// LaunchDaemon 启动的面板**没有 HOME**，脚本开着 set -u 会当场退出，报错只有一句
// "HOME: unbound variable"，看不出是一键安装的关键步骤被打断。
func (m *Manager) runRootEnv(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return streamCmd(ctx, cmd)
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// portOpen 判断本机端口是否可连。
func portOpen(ctx context.Context, port int) bool {
	return portListening(ctx, port)
}

// chownTo 把路径归属改为指定用户（保持原有组）。
func chownTo(user, path string) error {
	uid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-u", user)))
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-g", user)))
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func runOutput(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return string(out)
}

// ---- launchd 服务的安全装载 ----

// bootstrapService 卸载旧实例并重新装载，**等旧实例真正消失再装**。
//
// `launchctl bootout` 是**异步**的：一返回不等于已卸载完，立刻 bootstrap 会失败
// （Bootstrap failed: 5），后续 kickstart 又无对象可踢 —— 服务没了。install.sh 踩过并修好
// （wait_service_stopped），这段 Go 代码没照做又在真机复现（Qwen 起不来、8880 无监听）。
func (m *Manager) bootstrapService(ctx context.Context, label, plist string) error {
	_, _ = m.runRoot(ctx, 20*time.Second, "/bin/launchctl", "bootout", "system/"+label)

	// ① 等 launchd 真的把服务摘掉
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.runRoot(ctx, 5*time.Second,
			"/bin/launchctl", "print", "system/"+label); err != nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	// ② 带重试地装载：卸载刚结束的一小段时间内 bootstrap 仍可能失败
	var lastOut string
	for attempt := 1; attempt <= 5; attempt++ {
		out, err := m.runRoot(ctx, 30*time.Second,
			"/bin/launchctl", "bootstrap", "system", plist)
		if err == nil {
			return nil
		}
		lastOut = out
		// 已加载的情况：重启它即可
		if _, perr := m.runRoot(ctx, 10*time.Second,
			"/bin/launchctl", "print", "system/"+label); perr == nil {
			if _, kerr := m.runRoot(ctx, 20*time.Second,
				"/bin/launchctl", "kickstart", "-k", "system/"+label); kerr == nil {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("装载服务失败（已重试 5 次）：%s", tailText(stripANSI(lastOut), 300))
}
