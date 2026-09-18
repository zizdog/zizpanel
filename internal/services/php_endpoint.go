package services

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  PHP 端点闭环：装完一个 PHP 版本，立刻把它固定到**自己专属**的监听端点
//
//  背景（2026-09 真机反复踩到）：Homebrew 的每个 php@x.y 出厂都写
//  `listen = 127.0.0.1:9000`。装第二个版本时先起的那个占着 9000，
//  后起的 fpm 直接起不来（日志里是 "Address already in use"），
//  而 nginx 那边照样把请求发到 9000 —— 现象是"我明明选了 8.4，跑的却是 8.3"。
//
//  为什么必须做成"安装流程内闭环"而不是给一个按钮：
//  用户不可能知道要手工去改 www.conf。之前界面上只有一个
//  「🔧 修复端点并重启」，结果是"装完了还得再去点一下"（用户 2026-09-16 反馈）。
//
//  为什么只有这一份实现：通用 brew 安装路径（install.go 的 installViaBrew）
//  与一键 LNMP（lnmp.go 的 InstallLNMP）都要走这一步。历史上只有前者做了，
//  一键 LNMP 装出来的 PHP 仍然听 9000 —— 于是"一键装完再用市场装第二个版本"
//  必然互相抢端口。两份实现迟早走样，所以共用本文件里的
//  ensurePHPListenEndpoint，两条路径只负责在正确的时机调用它。
// ============================================================================

// errPHPNotManaged 表示"这个 PHP 版本还没有被 launchd 托管"。
//
// 这**不是**失败：一键 LNMP 会先写好端点、紧接着注册系统级守护进程
// （那一刻 fpm 会按新配置首次 bind socket）；通用 brew 路径在
// `brew services start` 失败时也可能落到这里。调用方据此给出
// "稍后注册/启动时会生效"的说明，而不是谎报"已重启生效"。
var errPHPNotManaged = errors.New("该 PHP 版本尚未注册到 launchd")

// ensurePHPListenEndpoint 是"装完 PHP 立刻固定端点"的唯一实现。
//
// 四步，顺序不能换：
//  1. sites.EnsureListen 把 <brew>/etc/php/<版本>/php-fpm.d/www.conf 的 listen
//     改写成该版本专属的 Unix socket（幂等；首次改写前留一份一次性备份）；
//  2. 套接字目录归属真实用户 —— php-fpm 以该用户运行，目录若是 root:0755，
//     它 bind 时会报 "unable to bind listening socket"，而那句日志看不出是权限问题；
//  3. 重启该版本让它按新配置 bind（未注册到 launchd 时如实说明稍后生效）；
//  4. 轮询确认端点上**真的有人监听** —— 只报"命令返回 0"不够，
//     launchctl 的退出码在本项目里多次谎报成功（见 AGENTS.md 第三节）。
//
// 失败一律写进 result.Warning 与 Steps，并给出用户能照做的补救动作。
func (m *Manager) ensurePHPListenEndpoint(ctx context.Context, formula string, res *InstallResult) error {
	version, ok := phpVersionFromFormula(formula)
	if !ok {
		// 不是版本化 PHP（nginx / mysql@8.4 / 无后缀的 php）。无后缀 php 的实际
		// 版本随 Homebrew 漂移，拿它去改 www.conf 等于猜 —— 明确不碰。
		return nil
	}
	prefix := m.phpBrewPrefix()
	if prefix == "" {
		return fmt.Errorf("未探测到 Homebrew 前缀，无法为 PHP %s 分配监听端点", version)
	}

	res.step(ctx, "正在为 PHP "+version+" 分配专属监听端点（避免与其它版本抢 9000）")
	fix, err := sites.EnsureListen(prefix, version, sites.EnsureListenOptions{
		NginxConfPath: filepath.Join(prefix, "etc", "nginx", "nginx.conf"),
		PanelUser:     m.opt.UserName,
		KeepBackup:    true,
	})
	if err != nil {
		msg := fmt.Sprintf("PHP %s 的监听端点配置失败：%v。"+
			"该版本的站点会不可用；可在「网站管理 → 🐘 PHP 环境」里点「修复端点并重启」重试", version, err)
		res.Warning = appendWarning(res.Warning, msg)
		res.step(ctx, "错误："+msg)
		return err
	}
	if fix.Changed {
		res.step(ctx, "已把 PHP "+version+" 的监听端点改为 "+fix.Endpoint+"（原配置已备份）")
	} else {
		res.step(ctx, "PHP "+version+" 的监听端点已是 "+fix.Endpoint+"（无需改动）")
	}

	// 上传/执行限制片段：与端点同一时机写入（都是"这个 PHP 版本刚装好"的收尾）。
	//
	// 为什么必须在安装流程内做：出厂的 upload_max_filesize=2M / post_max_size=8M
	// 会让 phpMyAdmin 导入稍大的 SQL 就失败；用户不该装完还要自己去翻配置
	// （这正是那次 413 报障的另一半）。片段写在 conf.d，属于面板自己的文件。
	limits := m.limits()
	if lr, lerr := sites.EnsurePHPLimits(prefix, version, limits, false); lerr != nil {
		// 不致命：端点已经修好，站点仍可用；但必须如实写进 Warning/Steps。
		msg := fmt.Sprintf("PHP %s 的上传/执行限制片段写入失败：%v。"+
			"大型 SQL 导入/上传仍可能失败（默认 upload_max_filesize=2M、post_max_size=8M）；"+
			"可稍后在「面板设置 → 上传与执行限制」里重新应用", version, lerr)
		res.Warning = appendWarning(res.Warning, msg)
		res.step(ctx, "警告："+msg)
	} else if lr.Changed {
		res.step(ctx, "已写入 PHP "+version+" 的上传/执行限制片段："+
			lr.Path+"（upload "+limits.UploadMaxFilesize+" / post "+limits.PostMaxSize+
			" / memory "+limits.MemoryLimit+" / max_execution_time "+
			strconv.Itoa(limits.MaxExecutionTime)+"s）")
	}

	// 套接字目录归属真实用户（见文件头第 2 条）。失败不致命：目录本来就可能
	// 已经是正确属主（brew 自己建的 var/run 就是），如实留给最后一步的验证去判定。
	m.chownSocketDir(fix.Endpoint)

	// 只有"配置真的改了"或"端点上根本没人听"才重启：健康机器上重复点一键安装
	// 不该白白把 php-fpm 踢一次（站点会有一瞬间 502）。幂等也包括"不折腾"。
	if !fix.Changed && sites.EndpointLive(fix.Endpoint) {
		res.step(ctx, "PHP "+version+" 已在监听 "+fix.Endpoint+"（无需重启）")
	} else {
		if err := m.restartPHPFPM(ctx, formula); err != nil {
			if errors.Is(err, errPHPNotManaged) {
				// 服务还没进 launchd —— 这不是失败：一键 LNMP 会紧接着注册系统级
				// 服务，那一刻 fpm 会按新配置首次 bind。如实说明，绝不报"已重启"。
				res.step(ctx, "PHP "+version+" 尚未注册到 launchd，稍后注册系统级服务时会按端点 "+
					fix.Endpoint+" 启动")
				return nil // 压根没启动，没有"现在是否在监听"可言
			}
			msg := fmt.Sprintf("PHP %s 的端点已写入 %s，但重启失败：%v。"+
				"未按新端点重启前，指向它的站点会连不上；可到「服务管理」里重启该服务后重试", version, fix.Endpoint, err)
			res.Warning = appendWarning(res.Warning, msg)
			res.step(ctx, "错误："+msg)
			return err
		}
	}

	if !waitEndpoint(ctx, fix.Endpoint, 15*time.Second) {
		msg := fmt.Sprintf("PHP %s 已配置为监听 %s，但该端点上没有进程在监听："+
			"php-fpm 没起来（常见原因：另一个版本仍占着旧的 9000、或 www.conf 有语法错误）。"+
			"请到「服务管理」查看它的日志，或在「🐘 PHP 环境」里点「修复端点并重启」重试", version, fix.Endpoint)
		res.Warning = appendWarning(res.Warning, msg)
		res.step(ctx, "错误："+msg)
		return errors.New(msg)
	}
	res.step(ctx, "PHP "+version+" 已监听在 "+fix.Endpoint)
	return nil
}

// restartPHPFPM 让某个 PHP 版本按新配置重新 bind 端点。
//
// 两条路径都要覆盖，因为两种托管方式在真机上并存：
//   - 一键 LNMP 会把服务改造成**系统级 LaunchDaemon**（/Library/LaunchDaemons），
//     这时 `brew services restart` 管不到它（brew 只认 ~/Library/LaunchAgents）；
//   - 从应用市场单独装的版本走用户级 LaunchAgent，brew services 才是正路。
//
// 复用 priv.LaunchKickstart（它自己按 plist 位置判断 system / gui 域），
// 不在服务层重写 launchctl 调用 —— 两套实现迟早会不一致。
func (m *Manager) restartPHPFPM(ctx context.Context, formula string) error {
	label := m.brewLabelFor(formula)
	if label == "" {
		return errPHPNotManaged
	}
	if m.phpRestartOverride != nil {
		return m.phpRestartOverride(ctx, formula)
	}
	if err := priv.LaunchKickstart(label); err == nil {
		return nil
	}
	// kickstart 失败（未装载，或用户级域权限不足）时退回按域自动选路的重启：
	// 已系统化的用 launchctl bootstrap，未系统化的才用 brew services restart。
	// 一旦失败就必须如实冒泡，绝不吞掉。
	if err := m.RestartBrewService(ctx, formula); err != nil {
		// **权限/服务域不符**是失败，但原因和"配置写错"不同，要说清是哪一种：
		// 一键 LNMP 会把 PHP 改造成**系统域** LaunchDaemon
		// （/Library/LaunchDaemons/homebrew.mxcl.php@8.2.plist），
		// 而当前身份不是 root 时 `launchctl kickstart system/...` 只会
		// "Operation not permitted"。这时服务可能还跑在**旧**端点上，
		// 所以必须如实冒泡（调用方进 Warning + Steps + 补救动作），绝不吞掉。
		if isRestartNotPermitted(err) {
			// 身份/服务域不允许重启（plist 在系统域，而当前不是 root）：**仍然是失败**，
			// 必须冒泡 —— 调用方会把"端点已写入、但没生效"如实写进 Warning + Steps。
			// 区别于 errPHPNotManaged：那种情况服务压根还没启动，随后注册时会按新端点
			// 首次 bind；这里服务可能正跑在**旧**端点上，不重启用的是旧端点，站点会 502。
			return fmt.Errorf("无权重启 %s（该服务由系统域 launchd 托管，需要 root 身份；"+
				"可在「服务管理」里点重启）：%w", label, err)
		}
		return fmt.Errorf("launchctl kickstart %s 与重启服务都失败: %w", label, err)
	}
	return nil
}

// isRestartNotPermitted 判断失败是不是"身份/服务域不允许重启"（而不是配置写错）。
func isRestartNotPermitted(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"operation not permitted", "not permitted", "could not kickstart",
		"permission denied", "must be root", "requires root",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// chownSocketDir 把 Unix socket 所在目录归属真实用户（尽力而为）。
//
// 与 api_php.go 里"修复端点"接口同一套做法：php-fpm 以真实用户运行，
// 启动时要在该目录里 bind socket；目录不可写就起不来。
func (m *Manager) chownSocketDir(endpoint string) {
	if !strings.HasPrefix(endpoint, "unix:") {
		return // TCP 端点不涉及文件权限
	}
	user := strings.TrimSpace(m.opt.UserName)
	if user == "" || user == "root" {
		return
	}
	_ = chownTo(user, filepath.Dir(strings.TrimPrefix(endpoint, "unix:")))
}

// phpBrewPrefix 返回 php-fpm 配置所在的 Homebrew 前缀。
//
// 与 web 层一致：优先用配置里的 brew 路径推导（测试能把它指到 t.TempDir()），
// 拿不到时才退回 priv 的全局探测。绝不把"要改写的 www.conf 路径"建立在猜测上。
func (m *Manager) phpBrewPrefix() string {
	if m.opt.BrewBin != "" {
		return filepath.Dir(filepath.Dir(m.opt.BrewBin))
	}
	return priv.HomebrewPrefix()
}

// waitEndpoint 轮询等待 FastCGI 端点真的出现（socket 存在且是 socket，或 TCP 可连）。
func waitEndpoint(ctx context.Context, endpoint string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if sites.EndpointLive(endpoint) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
}
