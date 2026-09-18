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

// ============================================================================
//  一键安装 LNMP（nginx + PHP-FPM + MySQL）
//
//  为什么需要这个"组合动作"，而不是让用户逐个点市场条目：
//    brew 装完这三个包之后，**网站功能还是不能用的**，实测有三个坑：
//      1. brew 的 nginx 默认 `listen 8080`，不是 80 —— 站点和面板入口都不通
//      2. nginx 的 vhosts 目录与 include 不会自动创建 —— 面板写不进站点配置
//      3. MySQL 数据目录没初始化时 mysqld 直接拒绝启动
//    再加上第四个容易被忽略的：
//      4. `brew services start` 装的是**用户级** LaunchAgent ——
//         服务器不接显示器、重启后停在登录界面时，它不会启动。
//
//    这四条都不是"包管理"能解决的，属于本机约定的收尾工作，
//    所以必须有一个人把它们串起来。串在这里，而不是写在文档里让用户照做。
//
//  设计取舍：真正"写系统 plist"那一步复用随包分发的
//  tools/system-services.sh。那段逻辑（find plist → 注入 UserName →
//  装到 /Library/LaunchDaemons → bootstrap）在真机上验证过，
//  在 Go 里重写一遍只会多一份会走样的实现。
// ============================================================================

// LNMPFormulas 是**默认**三件套（nginx / PHP / MySQL），顺序有意义：nginx 是
// 入口，先装它，用户能在最早的时刻看到东西。
//
// ⚠️ 语义已经收窄，别再把它当"本次要装的东西"：2026-09-19 起一键 LNMP
// 先让用户选版本，本次选择由 LNMPSelection 承载，所有安装/收尾路径都从
// **参数传进来的选择**取值（见 lnmp_options.go）。
//
// 这个变量现在只剩两个用途：
//  1. 默认选择的唯一来源（DefaultLNMPSelection 与它由测试锁死必须一致）；
//  2. 已有调用方/测试的兼容入口（历史上它是"LNMP 要装哪三个包"的答案）。
//
// 它**不是**"应用市场里 LNMP 板块的条目清单" —— 那是 Catalog() 里
// category=网站环境 的条目（还包含 postgresql@17 这种不参与一键 LNMP 的）。
// 谁要"本机都装了哪些 formula"用 Manager.InstalledFormulas，"市场有哪些组件"
// 用 Catalog()/LNMPOptions，都不要读这个变量。
//
// PHP 默认 **8.2**（用户 2026-09-17 明确要求："默认装 php8.2"）：
// 它是当前各站点/框架生态兼容性最好的选择，也避免"默认版本"与
// 面板 config 里的 PHPSvc 不一致。应用市场只上架 8.2 与 8.4 两个版本
// （用户 2026-09-17 明确要求："php只保留8.2和8.4"）；已经装在机器上的
// 8.1/8.3 不受影响，仍可继续跑站点的既有站点。
var LNMPFormulas = []string{"nginx", "php@8.2", "mysql@8.4"}

// LNMPPorts 是**默认**三件套每个服务的判定端口（"是否真的起来了"用）。
//
// ⚠️ 与 LNMPFormulas 同理：这是默认值，不是"本次选择的端口表"。
// 本次安装请用 LNMPSelection.Ports()（它按组件给端口，所以 php@8.4 / mysql@9.0
// 这类不在默认表里的版本也能拿到正确的判定目标）。
//
// 用端口而不是 HTTP：PHP-FPM 说 FastCGI、MySQL 说 MySQL 协议，
// 它们永远不可能通过 HTTP 检查（真机上就是这么被误判成"异常"的）。
//
// PHP 的 0 是**有意义**的：面板的多版本设计让每个 php@x.y 监听自己专属的
// Unix socket（见 sites.EnsureListen），不再占固定的 9000。所以 PHP 不能用
// "端口在听"来判定，必须用端点判定 —— 见 lnmpComponentLive。留一个假的 9000
// 在这里，只会在验证时误报"PHP 没在监听"，而它其实好好的。
var LNMPPorts = map[string]int{
	"nginx":     80,
	"php@8.2":   0,
	"mysql@8.4": 3306,
}

// InstallLNMP 一键把 LNMP 环境装好并跑起来。
//
// sel 是**本次要装的三件套**（用户选的版本）。空选择（三个字段都空）会被
// 当成默认三件套 —— 这是给"老调用方/测试"留的后路：正常路径上 web 层已经用
// ParseLNMPSelection 校验过，永远不会把非法选择送到这里。
//
// 为什么必须显式传参而不是继续读全局 LNMPFormulas：用户选了 PHP 8.4 之后，
// 逐包安装、PHP 端点闭环、系统级守护进程注册、服务登记、人读文案**五条路径**
// 都要用同一个选择。只改其中一条的后果是"装了 8.4、注册/文案还是 8.2"——
// 本项目历史上正是这类"多条路径只改了一条"的坑（DEVELOPMENT.md 140/141）。
//
// 每一步都追加到 result.Steps，前端按顺序展示 —— 这个流程动辄十几分钟，
// 用户必须能看到"现在到哪了"，否则会以为卡死。
func (m *Manager) InstallLNMP(ctx context.Context, result *InstallResult, sel LNMPSelection) error {
	if result == nil {
		result = &InstallResult{App: "lnmp"}
	}
	// 空选择 = 默认三件套（向后兼容）；非空但非法的选择一律当场拒绝，
	// 绝不用默认值替用户做决定（那会装出与用户选择不一样的版本）。
	if len(sel.Formulas()) == 0 {
		sel = DefaultLNMPSelection()
	}
	if err := sel.Validate(); err != nil {
		return fmt.Errorf("一键 LNMP 的版本选择无效：%w", err)
	}
	formulas := sel.Formulas()
	ports := sel.Ports()

	// 全新 macOS 上没有 Homebrew，而 LNMP 三件套全靠它。
	// 以前这里只会拒绝（"请先安装 Homebrew"），把用户赶回命令行 ——
	// 与"装完面板后所有操作都在面板里完成"矛盾。现在缺什么装什么（含前置的 CLT）。
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		result.step(ctx, "没有 Homebrew，先在面板里把它装上（含命令行开发者工具）")
		if err := m.EnsureHomebrew(ctx, result); err != nil {
			return err
		}
	}
	// 写 /Library/LaunchDaemons 需要 root。正式安装的面板由 LaunchDaemon
	// 以 root 运行；本地调试实例不是，这时明确拒绝而不是半途而废。
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 LNMP 需要以 root 运行（面板正式安装时由 LaunchDaemon 以 root 启动）")
	}

	result.step(ctx, "开始安装 LNMP 环境（"+sel.ComponentsText()+"）")

	// ---- 1. 逐包安装 ----
	for _, f := range formulas {
		if m.brewHas(ctx, f) {
			result.step(ctx, f+" 已安装，跳过")
			continue
		}
		result.Steps = append(result.Steps,
			fmt.Sprintf("正在 brew install %s（国内镜像下通常几分钟）", f))
		if _, err := m.brewInstall(ctx, result, 40*time.Minute, f); err != nil {
			// 失败要能让用户自己救：给出手工命令与镜像提示，
			// 而不是只抛一句 "brew install 失败"。
			return fmt.Errorf("安装 %s 失败: %w；可在终端手工重试 `brew install %s`"+
				"（若下载很慢，面板已优先走 NAS 镜像，重跑本任务会继续用镜像）", f, err, f)
		}
		result.step(ctx, f+" 安装完成")
	}

	// ---- 2. 本机约定的收尾工作 ----
	if m.brewHas(ctx, "nginx") {
		changed, err := m.fixNginxBaseConfig(ctx, result)
		if err != nil {
			// nginx 的基础配置修不好，后面站点功能一定是坏的，
			// 所以这里当成致命错误，而不是"警告但继续"。
			return err
		}
		// 配置改了但进程还在用旧配置 = "改了不生效"（本项目反复踩过的坑：
		// 文件在、服务在、就是不生效）。只在 nginx **已经在跑**时重载它；
		// 没在跑就不在这里拉起 —— 那会先起一个游离进程，随后系统级
		// LaunchDaemon 反而 bind 不上 80（真机上刚踩过这个连环坑）。
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
	// 真机现场（2026-09-17 mini）：一键 LNMP 之后 nginx 以**真实用户**运行
	// （系统级 LaunchDaemon + UserName），而 <家目录>/www/_logs/proxy/ 是面板
	// 早先以 root 建出来的 —— nginx 连日志都打不开，直接
	//   [emerg] open() "/Users/zizdog/www/_logs/proxy/proxy-3.access.log" failed (13: Permission denied)
	// 整个 80 起不来。以前不会暴露，是因为 nginx 一直以 root 跑。
	// 这里在启动服务**之前**把这棵树交还真实用户（失败只告警，不阻断）。
	m.ensureNginxLogOwnership(ctx, result)

	// ---- 2a. MySQL 数据目录 ----
	//
	// 按**本次选的 MySQL formula** 判断：以前写死 "mysql@8.4"，一旦用户在弹窗里
	// 选了别的版本，这一步就会静默跳过（数据目录没初始化 → mysqld 起不来 → 任务
	// 最后报"MySQL 未能确认在监听"，而真正的原因在前面的路径漏改）。
	// 认不出 MySQL 组件时就跳过（调用方已经校验过选择，正常不会发生）。
	mysqlFormula := lnmpSelectedMySQL(formulas)
	if mysqlFormula != "" && m.brewHas(ctx, mysqlFormula) {
		if err := m.initMySQLDataDir(ctx, result); err != nil {
			return err
		}
	}

	// ---- 2b. PHP 端点闭环（必须在注册系统级服务**之前**）----
	//
	// 为什么顺序是"先写端点、再注册"：注册那一步会 bootstrap 系统级
	// LaunchDaemon，php-fpm 在那一刻读 www.conf 并 bind socket。先写配置再启动，
	// fpm 一次就 bind 到专属端点；反过来要先启动成 9000、再改写、再重启一次，
	// 中间那段时间它已经和别的版本在抢 9000 了。
	//
	// 复用 php_endpoint.go 的 ensurePHPListenEndpoint（与通用 brew 安装路径同一份
	// 实现）：历史上只有通用路径做了这一步，一键 LNMP 装出来的 PHP 仍然听 9000，
	// 用户随后从市场装第二个版本必然互相抢端口 —— 而症状（"我明明选了 8.4，
	// 跑的却是 8.3"）出现在站点上，根本联想不到是安装路径少了一步。
	//
	// 失败**不当致命错误**：nginx/mysql 仍然可用，"装了一半"的机器重跑本任务
	// 也还能继续修；但必须如实进 Warning 与 Steps（含补救动作），不谎报成功。
	m.ensureLNMPPHPEndpoints(ctx, result, formulas)

	// ---- 3. 注册为系统级守护进程（不依赖用户登录）----
	//
	// 安全前提：**已经在跑的服务不要动**。
	//
	// 这一步会摘掉用户级 agent、改写 plist、重建 launchd 注册。
	// 对一台刚 brew install 完、什么也没跑的干净机器，这正是我们要的；
	// 但对一台**已经调好了 LNMP 的机器**（例如本机），它会拆掉能用的配置 ——
	// 我在本机实测时就是把正常工作的服务搞成失败的。
	// 所以先看端口：都在监听就说明环境已经好了，直接跳过。
	if m.allLNMPRunning(ctx, formulas) {
		result.Steps = append(result.Steps,
			sel.ComponentsText()+" 均已在运行，跳过服务注册（不改动现有配置）")
	} else {
		// 注册之前先清场：端口被"不在 launchd 里"的游离进程占着时，
		// 直接 bootstrap 出来的守护进程会因为 Address already in use 反复退出，
		// 而端口检查照样显示"在监听" —— 面板说一切正常，托管的其实是坏的。
		// 这是真机现场（mini 的 80 上就有一个游离 nginx）。
		m.freeStaleNginxPort(ctx, result)

		result.step(ctx, "正在注册为系统级后台服务（不依赖用户登录）")
		if err := m.installSystemDaemons(ctx, result, formulas); err != nil {
			// 兜底：上面可能刚把游离的 nginx 停掉，这里再失败就会让机器
			// "连原来那个能用的 nginx 都没了"。尽力把它拉回来并如实说明。
			m.restoreNginxAfterFailure(ctx, result, err)
			return err
		}
	}

	// ---- 回流：把三件套登记进面板自己的服务注册表 ----
	//
	// 必须放在 allLNMPRunning 的 if/else **之后**、且不在这两个分支里面：
	//   · "已经装好并都在跑"的机器会走上面的跳过分支（它跳过的是**改 launchd
	//     plist**，不是登记）—— 而那类机器恰恰最需要登记，因为它们的服务
	//     早就起来了。真机反馈正是这种：日志说装好了，服务管理里什么都没有。
	//   · 放在端口验证之前：验证不通过会提前 return，那样登记就被跳过了，
	//     可"装是装上了、只是没在跑"的服务照样应该出现在服务管理里。
	//
	// 失败不当致命错误（LNMP 本身可用），但必须如实写进 Steps 与 Warning，
	// 并告诉用户还能在应用市场手动纳管 —— 绝不谎报"已登记"。
	m.registerLNMPComponents(ctx, result, formulas, ports)

	// ---- 3b. 基础依赖：ffmpeg ----
	//
	// 用户明确要求："面板安装就应该安装 ffmpeg 这是基础环境，tts 要用到，
	// 以后开发功能也要用到！" 一键 LNMP 是"把机器配置成能用的服务器"的收尾
	// 动作，所以在这里补一次（与单个应用的安装路径共用同一套 Ensure，
	// 不在这里另写 brew 调用）。
	//
	// 与 TTS 两个安装器不同：ffmpeg 不参与 nginx/PHP/MySQL 的运行，装不上不该
	// 让整个 LNMP 任务失败 —— 那会掩盖"LNMP 其实已经好了"这个事实。所以这里
	// 失败只写进 Warning 并如实列出原因与手工命令（绝不谎报"基础依赖已就绪"）。
	if err := m.EnsureBaseDependencies(ctx, result); err != nil {
		result.Warning = appendLNMPWarning(result.Warning, "基础依赖（ffmpeg）补装失败："+err.Error())
		result.step(ctx, "警告："+result.Warning)
	}

	// ---- 4. 验证：只认端点/端口真的在监听 ----
	//
	// PHP 不能用端口判定：面板的多版本设计让每个 php@x.y 监听自己专属的
	// Unix socket（sel.Ports() 里 PHP 记的就是 0）。这里统一走
	// lnmpComponentLive，避免"验证说 PHP 没在监听、其实它好好的"这种误报 ——
	// 误报同样会让用户以为装失败了。
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

	// ---- 4b. 默认站点（一键安装的"默认结果"，用户明确要求）----
	//
	// "一键安装脚本安装完成后要默认建一个站，localhost/ip 访问，静态网站，
	//  有一个默认 index.php 就行。"（2026-09-17 用户原话）
	//
	// 为什么放在这里、而不是只在装 phpMyAdmin 时顺手做：
	//   · phpMyAdmin 那一步可能因为网络/包失败提前返回，默认站点不该被它连累；
	//   · 这里（收尾阶段）nginx 与 PHP-FPM 都已经起来了，能解析出**真的在监听**的
	//     专属端点写进 fastcgi_pass。
	// 为什么不放在 install.sh：面板安装时 nginx/PHP 可能还没装（它是"面板本身"的
	// 安装），硬写 nginx 配置只会在面板起来前造出一份指向空气的配置。
	// 旧机器（以前装过 LNMP 但没默认站点）的修复路径就是重跑一次一键 LNMP —— 这一步
	// 是幂等的（已存在不改、是我们早期创建的会原地升级），重跑即可补齐。
	result.step(ctx, "正在确保默认站点（http://localhost/ 与 http://<本机IP>/ 可直接访问）")
	if err := m.ensureDefaultVhost(ctx, result); err != nil {
		result.Warning = appendLNMPWarning(result.Warning,
			"默认站点未能就绪："+err.Error()+"（可在「设置 → 整理默认站点」里重试）")
		result.step(ctx, "警告："+result.Warning)
	}

	// ---- 5. phpMyAdmin ----
	// 放在最后：它依赖 nginx 与 PHP-FPM 都已就绪，否则装完也打不开。
	// 失败不阻断整个 LNMP —— 网站功能已经可用了，phpMyAdmin 可以稍后单独装。
	//
	// 没选 MySQL 就不部署它：phpMyAdmin 是**数据库管理界面**，没有数据库时
	// 装出来只会得到一个永远登录不上的入口（比没装更糟）。如实说明而不是
	// 静默跳过 —— 用户选组件时要知道自己放弃了什么。
	if mysqlFormula == "" {
		result.step(ctx, "本次没有选择 MySQL，跳过 phpMyAdmin（数据库管理界面）的部署")
	} else {
		result.step(ctx, "正在部署 phpMyAdmin（数据库管理界面）")
		if err := m.InstallPhpMyAdmin(ctx, result); err != nil {
			result.Warning = appendLNMPWarning(result.Warning,
				"LNMP 已就绪，但 phpMyAdmin 部署失败："+err.Error())
			result.step(ctx, "警告："+result.Warning)
		}
	}

	// ---- 6. MySQL root 凭据闭环（必须有，见 lnmp_mysql_credentials.go）----
	//
	// 为什么放在最后一步：这一步失败＝任务失败（凭据不一致会让之后每次库操作
	// 都撞 1045），但前面的 LNMP、默认站点、phpMyAdmin 都已经装好了 ——
	// 把它们一起报废只会让用户更难收拾。放在最后，既如实失败、也不白装。
	//
	// 没选 MySQL 就**没有这一步**（凭据闭环是 MySQL 专属的收尾）：不能因为
	// 用户没装 MySQL 就报"凭据核对失败"——那会把一次成功的安装变成红色失败。
	if mysqlFormula != "" {
		result.step(ctx, "正在核对 MySQL root 凭据（面板配置与服务器是否一致）")
		if err := m.ensureMySQLRootCredential(ctx, result); err != nil {
			return err
		}
	}
	return nil
}

// lnmpSelectedMySQL 从本次选择的 formula 列表里挑出 MySQL 那个（没有则空串）。
//
// 单独一个函数是为了让"哪个是 MySQL"的判据只有一处：lnmpGroupOf 的前缀判据
// （mysql@）。散在各处写 strings.HasPrefix(f, "mysql@") 迟早会漏一处。
func lnmpSelectedMySQL(formulas []string) string {
	for _, f := range formulas {
		if g, ok := lnmpGroupOf(f); ok && g.Key == "mysql" {
			return f
		}
	}
	return ""
}

// fixNginxBaseConfig 修正 brew 默认 nginx 配置，使其适合当服务器用。
//
// 做三件事：
//  1. `listen 8080` → `80`：brew 默认监听 8080，而站点与 /_panel 入口都按 80 写。
//     不修的话装完 nginx 也访问不到任何站点。
//  2. 建 vhosts 目录：面板的站点管理往这里写配置。
//  3. 在 nginx.conf 里 include vhosts/*.conf：不 include 的话，写进去也不生效。
//
// 返回"文件是否真的被改动"：调用方据此决定要不要重载已经在跑的 nginx
// ——改了不 reload 就是本项目最经典的"文件在、服务在、就是不生效"。
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
		// 插到最后一个 } 之前 —— 也就是 http{} 块的末尾
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

// initMySQLDataDir 在数据目录为空时初始化 MySQL。
//
// 必须降权到真实用户：mysqld 拒绝以 root 运行，而且数据目录的归属
// 必须与后续运行身份一致，否则启动时会报权限错误。
func (m *Manager) initMySQLDataDir(ctx context.Context, result *InstallResult) error {
	datadir := filepath.Join(m.brewPrefix(), "var", "mysql")
	if entries, err := os.ReadDir(datadir); err == nil && len(entries) > 0 {
		return nil // 已经初始化过
	}
	result.step(ctx, "MySQL 数据目录未初始化，正在初始化（root 初始无口令，随后会设置一个随机口令并记进面板配置）")
	if err := os.MkdirAll(datadir, 0o755); err != nil {
		return fmt.Errorf("创建 MySQL 数据目录失败: %w", err)
	}
	if m.opt.UserName != "" {
		_ = chownTo(m.opt.UserName, datadir)
	}
	mysqld := filepath.Join(m.brewPrefix(), "bin", "mysqld")
	if _, err := m.runAsUser(ctx, 5*time.Minute, mysqld,
		"--initialize-insecure", "--datadir="+datadir); err != nil {
		return fmt.Errorf("MySQL 初始化失败: %w", err)
	}
	result.step(ctx, "MySQL 数据目录初始化完成")
	// 记下"root 此刻确实是空口令"这个**已知事实**：只有本次初始化出来的库，
	// 面板才敢（也应该）给它设一个随机口令；已有数据目录的机器不动用户的口令。
	result.mysqlFreshInit = true
	return nil
}

// ensureLNMPPHPEndpoints 对**本次选中的** PHP 版本做专属端点闭环。
//
// 单独一个函数（而不是把循环留在 InstallLNMP 里）是为了让"端点闭环必须在注册
// 系统级服务之前"这条顺序约束能在源码级被测试锁住 —— 见
// TestInstallLNMPCallsSharedPHPListenFix。
//
// 只对 php@x.y 生效：nginx/mysql 没有 www.conf，走的是端口。
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
//
// formulas 是本次选择推导出来的（顺序 nginx 在前），不再是全局默认三件套：
// 用户选了 PHP 8.4 之后，这里必须注册 8.4 的那个 plist/服务定义，
// 注册成 8.2 会让"装的是 8.4、开机起来的是 8.2"。
func (m *Manager) installSystemDaemons(ctx context.Context, result *InstallResult, formulas []string) error {
	return m.installSystemDaemonsFor(ctx, result, formulas, 10*time.Minute)
}

// installSystemDaemonsFor 把**任意** formula 列表注册成系统级 LaunchDaemon。
//
// 为什么把 formula 列表提成参数：除了一键 LNMP，单个安装的应用（postgresql /
// ollama / php@8.x）以及 miniflux、syncthing（它们自带安装器）都需要同一件事 ——
// 无头 macOS 开机不加载 ~/Library/LaunchAgents（坑 130），常驻服务必须落到
// 系统域。复用这把已在真机上验证过的脚本，而不是再写一份 Go 版。
func (m *Manager) installSystemDaemonsFor(ctx context.Context, result *InstallResult,
	formulas []string, timeout time.Duration) error {
	if len(formulas) == 0 {
		return fmt.Errorf("没有要注册的服务（formula 列表为空）")
	}
	// 脚本**内置在二进制里**（见 system_services.go 的说明）：在线升级只换
	// 二进制、不发 tools/**，所以"从磁盘找脚本"在升级过的机器上必然失败 ——
	// mini 上就是这么卡住的。这里改成先从内置内容落盘，再执行。
	script, isTemp, err := m.materializeSystemServicesScript(ctx, result)
	if err != nil {
		return err
	}
	if isTemp {
		defer func() { _ = os.Remove(script) }()
	}
	args := append([]string{script}, formulas...)
	// 显式带上 HOME/USER：面板由 LaunchDaemon 以 root 启动，环境里**没有 HOME**。
	// 脚本里任何一处 `$HOME` 在 set -u 下都会当场退出（真机踩过：line 105:
	// HOME: unbound variable → 一键安装卡在最后一步）。脚本自己也补了兜底，
	// 这里再给一份是防御纵深：以后脚本新增的用法不必再各自小心。
	env := []string{}
	if m.opt.UserHome != "" {
		env = append(env, "HOME="+m.opt.UserHome)
	}
	if m.opt.UserName != "" {
		env = append(env, "USER="+m.opt.UserName, "LOGNAME="+m.opt.UserName)
		// 脚本判断"服务以谁的身份运行"时优先看 SUDO_USER，而面板**自己就是 root**
		// （没有 sudo），无头机器上 /dev/console 的属主又是 root —— 只靠它们会得到
		// "无法确定真实用户"（mini 真机实测：整批迁移全线失败）。显式传一份。
		env = append(env, "ZIZPANEL_REAL_USER="+m.opt.UserName)
	}
	out, err := m.runRootEnv(ctx, timeout, env, "/bin/bash", args...)
	if err != nil {
		// 把脚本的真实输出带上：它逐项打印了每个服务是"已加载"还是失败原因，
		// 只回一句"注册失败"对用户毫无帮助（"报错看不懂"就是这么来的）。
		return fmt.Errorf("注册系统级服务失败: %v（脚本输出：%s）", err, tailText(stripANSI(out), 1500))
	}
	result.step(ctx, "已注册为系统级 LaunchDaemon（开机自启，不依赖登录）")
	return nil
}

// registerLNMPComponents 把**本次选中的** LNMP 组件登记进面板自己的服务注册表。
//
// formulas / ports 都来自本次选择（ports 是 LNMPSelection.Ports()）：
// 登记用的端口必须是**这个版本**的判定端口，不能读默认表 —— 否则用户选了
// php@8.4 时登记出来的记录仍然带着默认那套端口/版本，服务管理里对不上。
//
// 为什么必须在 InstallLNMP 里显式做：一键 LNMP 过去只把三个服务注册成系统级
// LaunchDaemon（那是 **launchd** 层面），从不写面板的 services 表。后果真机上
// 就是这样：安装日志显示 nginx/PHP/MySQL 都装好了，但「服务管理」里一个都没有，
// 应用市场也停在"已安装·未纳管"，用户还得自己再点一次纳管。
// 单个应用的安装路径（catalog.go 的 Install）本来就会登记，漏的只有这条路。
//
// 登记复用 RegisterInstalledService（它内部走 AdoptCandidate，幂等）：
// 不另写一套入库逻辑 —— 两份实现迟早会不一致，而不一致的服务记录会让
// 状态/日志/端口对不上，排查成本极高。
func (m *Manager) registerLNMPComponents(ctx context.Context, result *InstallResult,
	formulas []string, ports map[string]int) {
	result.step(ctx, "正在把 "+lnmpComponentsTextOf(formulas)+" 登记进「服务管理」")
	for _, formula := range formulas {
		// 目录条目是端口/分类/图标/显示名的权威来源；查不到时用 formula 兜底，
		// 至少不会登记成一条无名无端口的记录。
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
				"可在「服务管理」→「扫描可纳管服务」或应用市场对应条目上手动纳管", formula)
			result.step(ctx, msg)
			result.Warning = appendLNMPWarning(result.Warning, msg)
			continue
		}
		if err := m.RegisterInstalledService(ctx, label, name, icon, category, port); err != nil {
			msg := fmt.Sprintf("登记失败：%s（%s）没能加入「服务管理」：%v；"+
				"服务本身已装好，可在应用市场对应条目上点「纳管」补登记", formula, label, err)
			result.step(ctx, msg)
			result.Warning = appendLNMPWarning(result.Warning, msg)
			continue
		}
		result.step(ctx, fmt.Sprintf("%s 已登记进「服务管理」（%s）", name, label))
	}
}

// lnmpComponentLabel 找出某个 formula 在本机 launchd 里的**真实**标签。
//
// 为什么不能直接用目录里写死的 ServiceLabel：同一台机器上 Homebrew 混用
// `homebrew.mxcl.*` 与 `sh.brew.*` 两套前缀（实测 php 是 sh.brew.php@8.3，
// 而目录里写的是 homebrew.mxcl.php@8.3），系统级改造还可能换成自研前缀
// （本机 nginx 的 LaunchDaemon 就叫 cn.zizdog.nginx）。
// 标签用错的后果是记录指向一个不存在的 plist：服务管理里状态永远查不到，
// 界面还会显示成一个"已登记但查不到"的僵尸条目。
//
// 磁盘上确实找不到时退回目录条目的标签：那只是个候选，真的不存在时
// RegisterInstalledService 会如实报错，这里不替它掩盖失败。
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
//
// 不在这里写死 ID（nginx / php83 / mysql84）映射：目录条目的 ID 与 formula
// 本来就不是一回事，写死映射会在目录改 ID 时静默失效；按 BrewFormula 反查
// 则加条目、改 ID 都不用动这里。
func lnmpCatalogApp(formula string) (App, bool) {
	for _, a := range Catalog() {
		if a.BrewFormula != "" && a.BrewFormula == formula {
			return a, true
		}
	}
	return App{}, false
}

// appendLNMPWarning 追加告警而不是覆盖。
//
// InstallLNMP 里有多处给 result.Warning 赋值（端口验证、phpMyAdmin 部署），
// 直接赋值会把前面"某个组件没登记上"这件事从 Warning 里抹掉 ——
// 用户只看摘要时就会以为什么都成功了。这与项目"不谎报成功"的要求冲突，
// 所以这些位置统一改成追加。
func appendLNMPWarning(cur, add string) string {
	if cur == "" {
		return add
	}
	return cur + "；" + add
}

// lnmpComponentsTextOf 生成"nginx / PHP 8.4 / MySQL 8.4"这种人看的组件清单。
//
// 由**本次选择**推导（formula → 目录条目展示名），而不是在日志里写死字符串，
// 也不读全局默认值：默认版本从 8.2 换成 8.4 时，写死的日志就成了
// "日志说 8.2、装的是 8.4"的假信息 —— 用户排查时最恨这种自相矛盾。
// 目录查不到就退回 formula 全名。
//
// 顺序跟着传进来的 formulas（调用方保证 nginx 在前）。
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

// ---------------------------------------------------------------------------
//  小工具
// ---------------------------------------------------------------------------

func (m *Manager) brewPrefix() string {
	return filepath.Dir(filepath.Dir(m.opt.BrewBin))
}

// allLNMPRunning 判断**本次选中的**组件是否都已在监听（PHP 按专属端点判断）。
//
// 用它来避免"重复改造"：环境已经能用的时候，动 plist 只有坏处没有好处。
// 判定口径必须与最终验证一致（都走 lnmpComponentLive），否则会出现
// "这里说都在跑、那里说没在跑"的自相矛盾，用户没法相信任何一条。
func (m *Manager) allLNMPRunning(ctx context.Context, formulas []string) bool {
	for _, f := range formulas {
		if _, live := m.lnmpComponentLive(ctx, f, nil); !live {
			return false
		}
	}
	return true
}

// lnmpComponentLive 判断某个 LNMP 组件当前是否真的可用，并返回给人看的标签。
//
// 两种口径：
//   - PHP：它监听的是面板分配的**专属 Unix socket**（不占端口），
//     必须按 sites.EndpointLive 判定。用端口判它必然误报（见 LNMPPorts 注释）。
//   - 其它：按各自的判定端口连一次。PHP-FPM 说 FastCGI、MySQL 说 MySQL 协议，
//     它们永远不可能通过 HTTP 检查（真机上就是这么被误判成"异常"的）。
//
// ports 是本次选择的判定端口表（LNMPSelection.Ports()）；传 nil 时按 formula
// 的组件前缀推导（lnmpGroupOf），**不读全局 LNMPPorts** —— 用户选了 mysql@9.0
// 时全局表里没有它，读全局会得到"没有可判定的端口"这种假失败。
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

// freeStaleNginxPort 在注册系统级服务前，把"占着 80 但不在 launchd 里"的
// 游离 nginx 进程收掉。
//
// 为什么必须做（2026-09-17 mini 真机现场）：80 上跑着一个旧脚本/手工拉起的
// nginx，/Library/LaunchDaemons 里没有它的 plist。此时直接 bootstrap 系统级
// LaunchDaemon 会得到最难查的一种状态：launchd 里的 nginx 因
// "Address already in use" 反复退出，而端口检查照样显示"80 在监听"——
// 面板每个角落都说正常，托管的服务其实是坏的。
//
// 安全边界（宁可不做，也不误杀）：
//   - 只有 nginx 已装、80 在听、且磁盘上找不到任何 nginx 的 launchd 定义时才考虑；
//   - 占用者必须**每一个**都能认成 nginx（按 priv.CheckPort 的进程名），
//     否则只如实告警，绝不动手；
//   - 停掉之后如果 80 没释放，只告警，任务继续（不把机器搞成"连原来那个都没了"）。
func (m *Manager) freeStaleNginxPort(ctx context.Context, result *InstallResult) {
	if !m.brewHas(ctx, "nginx") || !portOpen(ctx, 80) {
		return
	}
	info, err := priv.CheckPort("80")
	if err != nil || !info.InUse || len(info.Holders) == 0 {
		return
	}
	// 判定标准是"**launchd 托管的那个实例自己**占着 80"，而不是"磁盘上有没有 plist"。
	//
	// 真机现象（2026-09-17 mini，浪费了一整轮部署才发现）：上一轮失败留下的 plist
	// 还在 /Library/LaunchDaemons 里（服务已加载、进程早退出了），而 80 上仍然是
	// 兜底拉起的 root nginx（`ps` 里 master 是 root、ppid=1）。只看 plist 就会认为
	// "已托管、不用管"，于是新起的系统级 nginx 永远抢不到 80 —— 界面显示
	// "端口 80 正在监听、状态 running"，托管的那个进程其实根本不存在。
	// 这正是"端口在听 ≠ 我们托管的进程在听"。
	//
	// 为什么单测抓不到：它需要"真实的 launchd + 一个僵尸 plist + 一个游离进程占着
	// 同一个端口"三者同时成立。单测只能锁住判定函数本身（holdersContainPID），
	// 组合出来的现象只有真机能复现 —— 所以这条注释才写这么细。
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

	// 顺手清掉它可能留下的 pid 文件：那是 root 建的 0644 文件，而紧接着要起的
	// 系统级 nginx 以**真实用户**运行 —— 写不进这个文件就会直接启动失败，报的却是
	// `open() ".../nginx.pid" failed (13: Permission denied)`，看不出是属主问题。
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
//
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

// restoreNginxAfterFailure：installSystemDaemons 失败时的兜底。
//
// 上面可能刚把游离的 nginx 停掉；注册又失败的话，机器会从"有个能用的
// nginx"退化成"完全没有 nginx"。这里尽力把它拉起来并如实写明，
// 绝不静默留一个 80 上没人的现场。
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

// nginxLaunchdInstanceHolds 判断"占着 80 的那个进程"是不是 launchd 托管的实例。
//
// 做法：取磁盘上真实存在的 label（brew 两套前缀 + 面板自己的 cn.zizdog.nginx
// 都认），查 launchd 里该服务的 PID，再看端口占用者里有没有它。
// 服务已加载但没进程（上一轮失败留下的僵尸 plist）时 PID=0 → 返回 false，
// 于是游离进程会被正常接管（真机就是这么卡住的）。
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

// holdersContainPID 判断端口占用者列表里有没有指定 pid。
//
// priv.CheckPort 的 Holders 形如 ["nginx (pid 74181)"]。
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
//
// priv.CheckPort 的 Holders 形如 ["nginx (pid 39411)"]。名字对不上就返回
// false —— 调用方只会告警、不动手：宁可留着问题让人看，也不误杀别的进程。
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
// 为什么必须做：面板写站点/反代配置时（它自己是 root）会顺手建出
// <家目录>/www/_logs/... 这些目录，属主是 root；而一键 LNMP 之后 nginx 是以
// **真实用户**运行的，于是 `access_log` 打不开 → nginx 连启动都失败
// （真机：[emerg] open() ... Permission denied，整台机器的 80 都没了）。
// 以前 nginx 以 root 跑，所以这个坑一直藏着。
//
// 只处理两棵树，绝不递归整个 www：
//   - <brew>/var/log/nginx（nginx 自己的日志）
//   - <家目录>/www/_logs（面板的日志根，站点与反代日志都在这里）
//
// 失败只告警：日志属主修不好不该让整个 LNMP 失败，但必须让用户看得见。
func (m *Manager) ensureNginxLogOwnership(ctx context.Context, result *InstallResult) {
	if !m.brewHas(ctx, "nginx") {
		return
	}
	user := strings.TrimSpace(m.opt.UserName)
	if user == "" || user == "root" {
		return
	}
	// 网站根目录本身也要归属真实用户（**不递归**，各站点目录各有归属）。
	//
	// 真机现场（2026-09-17 mini）：<家目录>/www 是 root 属主，于是用户即使通过
	// SSH/编辑器也无法在自己的网站根下建目录、传文件 —— 面板能建（root），
	// 用户不能，看起来像"面板把目录锁了"。这里只改 www 这一层，不动里面的站点。
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
//
// 返回改动项数与遍历项数。单项失败不中断（日志目录里可能有正当的其它属主，
// 例如用户自己放进来的文件），最后把无法处理的错误如实带回调用方。
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

// stripANSI 去掉终端颜色转义。
//
// 子脚本（system-services.sh）是给人看的，会输出颜色码；
// 原样塞进 JSON 会让界面显示一堆 [1m[34m 之类的噪声。
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
//
// 为什么需要：LaunchDaemon 启动的面板**没有 HOME**，而它调用的 shell 脚本
// 往往开着 set -u —— 未定义的变量会让脚本当场退出，报错只有一句
// "HOME: unbound variable"，完全看不出是一键安装的关键步骤被它打断的。
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

// ---------------------------------------------------------------------------
//  launchd 服务的安全装载
// ---------------------------------------------------------------------------

// bootstrapService 卸载旧实例并重新装载，**等旧实例真正消失再装**。
//
// 为什么必须等：`launchctl bootout` 是**异步**的，它一返回并不代表服务
// 已经卸载完成。此时立刻 bootstrap 会失败（Bootstrap failed: 5），
// 而失败的 bootstrap 又让后续 kickstart 无对象可踢 —— 结果是服务没了。
//
// 这个坑在本项目的 install.sh 里踩过一次并修好了（wait_service_stopped），
// 但这段 Go 代码最初没照做，于是又在真机上复现了一遍（Qwen 服务被 bootout
// 后没起来、8880 无监听）。同一个教训要落实在所有地方，不能只在修过的那处。
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
