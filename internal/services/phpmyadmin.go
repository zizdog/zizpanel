package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/sites"
)

// phpMyAdmin 部署：面板只做运维动作（安装、启停、连接信息、入口），
// 库表与权限交给 phpMyAdmin，面板自研那套保留为"应急入口"。
// 路径照搬本机已验证布局（实测 HTTP 200）：web 根 <prefix>/share/phpmyadmin，
// 配置 <prefix>/etc/phpmyadmin.config.inc.php（share 下软链到它），
// nginx 默认站里一段 `location ^~ /phpmyadmin`，访问 http://<host>/phpmyadmin/。

type pmaPaths struct {
	Share    string // web 根
	ConfLink string // share 下的 config.inc.php（软链）
	ConfReal string // 真实配置文件
	Temp     string // phpMyAdmin 需要的可写临时目录
}

func (m *Manager) pmaPaths() pmaPaths {
	share := filepath.Join(m.brewPrefix(), "share", "phpmyadmin")
	return pmaPaths{
		Share:    share,
		ConfLink: filepath.Join(share, "config.inc.php"),
		ConfReal: filepath.Join(m.brewPrefix(), "etc", "phpmyadmin.config.inc.php"),
		Temp:     filepath.Join(share, "tmp"),
	}
}

// PhpMyAdminConfPath 返回 phpMyAdmin 真实配置文件的路径（**唯一**推导处）：web 层的
// 「上传与执行限制」改 max_execution_time 后要同步 ExecTimeLimit，否则大 SQL 导入仍 PHP 超时。
func PhpMyAdminConfPath(brewPrefix string) string {
	return filepath.Join(brewPrefix, "etc", "phpmyadmin.config.inc.php")
}

func (m *Manager) limits() sites.Limits {
	return m.opt.UploadLimits.Normalize()
}

// InstallPhpMyAdmin 安装并接好 phpMyAdmin。
func (m *Manager) InstallPhpMyAdmin(ctx context.Context, result *InstallResult) error {
	var err error
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		return fmt.Errorf("未安装 Homebrew，无法安装 phpMyAdmin")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 phpMyAdmin 需要以 root 运行")
	}

	// ---- 1. 装包 ----
	// 走 brew 不走官网：实测 files.phpmyadmin.net 在国内**完全不可达**，brew 的 bottle 走国内镜像能下。
	if !m.brewHas(ctx, "phpmyadmin") {
		result.step(ctx, "正在 brew install phpmyadmin")
		if _, err := m.brewInstall(ctx, result, 15*time.Minute, "phpmyadmin"); err != nil {
			return fmt.Errorf("安装 phpmyadmin 失败: %w", err)
		}
	} else {
		result.step(ctx, "phpmyadmin 已安装，跳过")
	}

	p := m.pmaPaths()
	if _, err := os.Stat(p.Share); err != nil {
		return fmt.Errorf("找不到 phpMyAdmin 的 web 目录 %s（安装可能未完成）", p.Share)
	}

	// ---- 2. 临时目录（模板编译缓存；不存在或不可写时页面报错）----
	if err := os.MkdirAll(p.Temp, 0o755); err != nil {
		return fmt.Errorf("创建 phpMyAdmin 临时目录失败: %w", err)
	}
	if m.opt.UserName != "" {
		_ = chownTo(m.opt.UserName, p.Temp)
	}

	// ---- 3. 配置文件 ----
	// 复用已有 blowfish_secret（换掉会踢掉所有人当前的登录会话），但复用前必须**校验**：
	// 不是我们生成的随机值就重建 —— 可预测的密钥等于会话可被伪造，实测"非空就复用"会固化垃圾值。
	secret := existingBlowfishSecret(p.ConfReal)
	if !validBlowfishSecret(secret) {
		secret, err = randomHex(16)
		if err != nil {
			return err
		}
	}
	// AllowNoPassword 按**安装当时**的真实连接结果写；LNMP 里 root 随机口令在其后才定下来，安装末尾还要校正（D43）。
	noPass := m.mysqlRootHasNoPassword(ctx)
	if err := os.MkdirAll(filepath.Dir(p.ConfReal), 0o755); err != nil {
		return err
	}
	conf := pmaConfig(secret, p.Temp, noPass, m.limits().MaxExecutionTime)
	tmp := p.ConfReal + ".tmp"
	if err := os.WriteFile(tmp, []byte(conf), 0o600); err != nil {
		return fmt.Errorf("写入 phpMyAdmin 配置失败: %w", err)
	}
	// 权限 0600 + 归属真实用户：phpMyAdmin 由 php-fpm 以「真实用户」执行，
	// root 属主会让它读不到（"Existing configuration file (...) is not readable."）。
	if m.opt.UserName != "" {
		if err := chownTree(m.opt.UserName, tmp); err != nil {
			return fmt.Errorf("设置配置归属失败: %w", err)
		}
	}
	if err := os.Rename(tmp, p.ConfReal); err != nil {
		return fmt.Errorf("替换 phpMyAdmin 配置失败: %w", err)
	}
	// brew 装的 config.inc.php 本是指向它的软链；缺失或被替换成实体文件时补上。
	if fi, err := os.Lstat(p.ConfLink); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		_ = os.Remove(p.ConfLink)
		rel, rerr := filepath.Rel(p.Share, p.ConfReal)
		if rerr == nil {
			_ = os.Symlink(rel, p.ConfLink)
		}
	}
	result.step(ctx, "已写入 phpMyAdmin 配置（cookie 登录、随机 blowfish_secret）")
	if noPass {
		result.Steps = append(result.Steps,
			"安装时 MySQL root 无密码，已临时允许空密码登录（稍后会按真实口令状态校正；建议尽快在面板里给它设一个）")
	}

	// ---- 4. 接进 nginx ----
	if err := m.ensureFPMInclude(ctx, result); err != nil {
		return err
	}
	// 先确保"默认站点"这个容器存在：**只在缺失时**补一份最小默认站点，已存在一字不改。
	// 缺了它会报"读取默认站点配置失败: no such file"，用户看到的是"phpMyAdmin 装了但 404"。
	if err := m.ensureDefaultVhost(ctx, result); err != nil {
		return err
	}
	if err := m.ensurePMAVhost(ctx, result); err != nil {
		return err
	}

	// ---- 5. 验证：只认真的返回 200 ----
	// **必须看页面内容**：phpMyAdmin 的错误页 HTTP 状态**也是 200**，只看状态码会把"打不开"判成成功。
	// 按项目铁律**不许只记日志然后报成功** —— 如实返回错误。
	body, ok := waitHTTPBody(ctx, pmaProbeURL, 10*time.Second)
	if !ok {
		return fmt.Errorf("phpMyAdmin 配置已写入 %s，但 /phpmyadmin/ 打不开：%s。"+
			"请到「日志中心」看 nginx 的 error_log（常见原因是 nginx 重载失败："+
			"日志文件属主是 root，而 nginx 以真实用户运行）", p.ConfReal, describePMAFailure(body))
	}
	result.step(ctx, "phpMyAdmin 已可用：http://<本机地址>/phpmyadmin/")

	// ---- 6. 按真实连接结果校正 AllowNoPassword（D43）----
	// 安装期判据只是"当时"的状态；幂等，一致时一个字都不写。
	if changed, serr := m.SyncPhpMyAdminAllowNoPassword(ctx); serr != nil {
		return fmt.Errorf("phpMyAdmin 已可用，但按真实 MySQL 口令状态校正 AllowNoPassword 失败: %w", serr)
	} else if changed {
		result.step(ctx, "已按真实 MySQL 口令状态校正 phpMyAdmin 的 AllowNoPassword")
	}
	return nil
}

// pmaConfig 生成 phpMyAdmin 配置内容。
// 用 cookie 登录：config 模式等于**任何能访问到这个 URL 的人**都直接拥有数据库权限。
func pmaConfig(secret, tmpDir string, allowNoPassword bool, execTimeLimit int) string {
	allow := "false"
	if allowNoPassword {
		allow = "true"
	}
	if execTimeLimit <= 0 {
		execTimeLimit = sites.DefaultPHPMaxExecutionTime
	}
	return fmt.Sprintf(`<?php
/* ZIZPANEL-MANAGED phpMyAdmin 配置（由面板生成，手工改动可能被覆盖） */
$i = 0;
$i++;
$cfg['Servers'][$i]['auth_type']       = 'cookie';
$cfg['Servers'][$i]['host']            = '127.0.0.1';
$cfg['Servers'][$i]['port']            = '3306';
$cfg['Servers'][$i]['compress']        = false;
$cfg['Servers'][$i]['AllowNoPassword'] = %s;
$cfg['Servers'][$i]['connect_type']    = 'tcp';
$cfg['UploadDir']                      = '';
$cfg['SaveDir']                        = '';
$cfg['blowfish_secret']                = '%s';
$cfg['TempDir']                        = '%s';
$cfg['ThemeManager']                   = true;
$cfg['ThemeDefault']                   = 'pmahomme';
$cfg['DefaultLang']                    = 'zh_CN';
$cfg['ServerDefault']                  = 1;
/* 与面板「上传与执行限制」里的 max_execution_time 一致：导入大 SQL 会跑很久，
   用 phpMyAdmin 自己的默认值（300 秒/或 PHP 的 max_execution_time）会把大文件
   的导入中途掐断。0 = 不限制，这里刻意不写 0（不限制等于请求永远不会超时释放）。 */
$cfg['ExecTimeLimit']                  = %d;
`, allow, secret, tmpDir, execTimeLimit)
}

// pmaExecTimeLimitRe 定位配置里的 ExecTimeLimit 那一行（保留缩进与等号两侧空白）。
var pmaExecTimeLimitRe = regexp.MustCompile(
	`(?m)^([ \t]*\$cfg\['ExecTimeLimit'\][ \t]*=[ \t]*)([0-9]+)([ \t]*;)`)

// SyncPhpMyAdminExecTimeLimit 幂等地把 phpMyAdmin 的 ExecTimeLimit 改成 want 秒：
// 用户后来调大 max_execution_time，phpMyAdmin 仍用安装当时的值（"面板说改好了、导入还是超时"）。

// 语义：不存在/值一致 → (false,nil) 不写；值不同 → 原地改写；没有该行 → 追加到末尾；
// user 非空时把归属交还真实用户，否则 php-fpm 读不到。
func SyncPhpMyAdminExecTimeLimit(confPath string, want int, user string) (bool, error) {
	if want <= 0 {
		want = sites.DefaultPHPMaxExecutionTime
	}
	data, err := os.ReadFile(confPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // 没装 phpMyAdmin
		}
		return false, fmt.Errorf("读取 phpMyAdmin 配置失败: %w", err)
	}
	text := string(data)
	next := ""
	if m := pmaExecTimeLimitRe.FindStringSubmatch(text); m != nil {
		cur, _ := strconv.Atoi(m[2])
		if cur == want {
			return false, nil
		}
		next = pmaExecTimeLimitRe.ReplaceAllString(text, "${1}"+strconv.Itoa(want)+"${3}")
	} else {
		line := "$cfg['ExecTimeLimit']                  = " + strconv.Itoa(want) + ";\n"
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		next = text + line
	}
	tmp := confPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(next), 0o600); err != nil {
		return false, fmt.Errorf("写入 phpMyAdmin 配置失败: %w", err)
	}
	if user != "" {
		if err := chownTree(user, tmp); err != nil {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("设置 phpMyAdmin 配置归属失败: %w", err)
		}
	}
	if err := os.Rename(tmp, confPath); err != nil {
		return false, fmt.Errorf("替换 phpMyAdmin 配置失败: %w", err)
	}
	after, err := os.ReadFile(confPath)
	if err != nil {
		return false, fmt.Errorf("复核 phpMyAdmin 配置失败: %w", err)
	}
	if m := pmaExecTimeLimitRe.FindStringSubmatch(string(after)); m == nil || m[2] != strconv.Itoa(want) {
		return false, fmt.Errorf("phpMyAdmin 配置写入后复核失败：ExecTimeLimit 应为 %d（%s）", want, confPath)
	}
	return true, nil
}

// mysqlRootPasswordState 是"root 目前有没有口令"的**真实连接**结论。
type mysqlRootPasswordState int

const (
	// mysqlRootPasswordUnknown：连不上 / 没有 mysql 客户端 —— 判不出来。
	mysqlRootPasswordUnknown mysqlRootPasswordState = iota
	mysqlRootPasswordEmpty
	mysqlRootPasswordSet
)

// mysqlRootPasswordState 用真实连接判断 root 是空口令还是已有口令（D43）。
// 不能靠"配置里有没有口令"：LNMP 里 root 随机口令最后才设，安装期判据是旧状态。
// 不带口令连得上=空口令；明确 Access denied (1045)=已有口令；其它=未知（不据此改写，避免来回抖）。
func (m *Manager) mysqlRootPasswordState(ctx context.Context) mysqlRootPasswordState {
	mysql := filepath.Join(m.brewPrefix(), "bin", "mysql")
	if _, err := os.Stat(mysql); err != nil {
		return mysqlRootPasswordUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, mysql, "-u", "root", "--protocol=TCP",
		"-h", "127.0.0.1", "-e", "SELECT 1").CombinedOutput()
	if err == nil {
		return mysqlRootPasswordEmpty
	}
	text := string(out)
	if strings.Contains(text, "Access denied") || strings.Contains(text, "1045") {
		return mysqlRootPasswordSet
	}
	return mysqlRootPasswordUnknown
}

// mysqlRootHasNoPassword 探测 root 是否空密码。
// brew 装的 MySQL 首次初始化后 root 无密码；AllowNoPassword=false 时空密码登不进去（用户只看到"无法登录"）。
// 探测到的只是**当时**状态，安装末尾与凭据闭环后都要用 SyncPhpMyAdminAllowNoPassword 再校正。
func (m *Manager) mysqlRootHasNoPassword(ctx context.Context) bool {
	return m.mysqlRootPasswordState(ctx) == mysqlRootPasswordEmpty
}

// pmaAllowNoPassRe 定位配置文件里的 AllowNoPassword 那一行（保留缩进与等号两侧空白）。
var pmaAllowNoPassRe = regexp.MustCompile(
	`(?m)^([ \t]*\$cfg\['Servers'\]\[\$i\]\['AllowNoPassword'\][ \t]*=[ \t]*)(true|false)(;)`)

// pmaAllowNoPasswordFromConfig 读出配置里当前的 AllowNoPassword；第二个返回值表示找没找到。
func pmaAllowNoPasswordFromConfig(conf string) (bool, bool) {
	m := pmaAllowNoPassRe.FindStringSubmatch(conf)
	if len(m) < 3 {
		return false, false
	}
	return m[2] == "true", true
}

// setPMAAllowNoPassword 幂等地把那一行改成 want（找不到那一行时原样返回）。
func setPMAAllowNoPassword(conf string, want bool) string {
	val := "false"
	if want {
		val = "true"
	}
	return pmaAllowNoPassRe.ReplaceAllString(conf, "${1}"+val+"${3}")
}

// SyncPhpMyAdminAllowNoPassword 按**真实连接结果**修正配置里的 AllowNoPassword（D43），幂等：
// 值一致时一个字都不写；凭据闭环后再调一次（LNMP 里 phpMyAdmin 先装、root 随机口令后设）。
// 返回是否真的改了配置。没装 phpMyAdmin / 判不出口令状态时返回 (false, nil)。
func (m *Manager) SyncPhpMyAdminAllowNoPassword(ctx context.Context) (bool, error) {
	confPath := m.pmaPaths().ConfReal
	data, err := os.ReadFile(confPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // 没装 phpMyAdmin，无需处理
		}
		return false, fmt.Errorf("读取 phpMyAdmin 配置失败: %w", err)
	}
	state := m.mysqlRootPasswordState(ctx)
	if state == mysqlRootPasswordUnknown {
		// 判不出来就**不动**：写一个猜测值比保持现状更糟（会在两种状态之间抖）。
		return false, nil
	}
	want := state == mysqlRootPasswordEmpty
	cur, found := pmaAllowNoPasswordFromConfig(string(data))
	if !found {
		return false, fmt.Errorf("phpMyAdmin 配置 %s 里找不到 AllowNoPassword 行，拒绝猜测（请重装 phpMyAdmin）", confPath)
	}
	if cur == want {
		return false, nil
	}
	tmp := confPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(setPMAAllowNoPassword(string(data), want)), 0o600); err != nil {
		return false, fmt.Errorf("写入 phpMyAdmin 配置失败: %w", err)
	}
	// 归属要给真实用户：phpMyAdmin 由 php-fpm 以真实用户身份执行，root 属主会读不到。
	if m.opt.UserName != "" {
		if err := chownTree(m.opt.UserName, tmp); err != nil {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("设置配置归属失败: %w", err)
		}
	}
	if err := os.Rename(tmp, confPath); err != nil {
		return false, fmt.Errorf("替换 phpMyAdmin 配置失败: %w", err)
	}
	// 本行不依赖 reload（php-fpm 每请求重读），但按约定"改了配置就让它真正加载"。
	if err := m.reloadNginx(ctx); err != nil {
		return false, fmt.Errorf("AllowNoPassword 已改为 %v，但 nginx reload 失败: %w", want, err)
	}
	// 复核 1：重读磁盘确认那一行真的改对了（写盘成功 ≠ 内容对）。
	after, err := os.ReadFile(confPath)
	if err != nil {
		return false, fmt.Errorf("复核 phpMyAdmin 配置失败: %w", err)
	}
	if got, ok := pmaAllowNoPasswordFromConfig(string(after)); !ok || got != want {
		return false, fmt.Errorf("phpMyAdmin 配置写入后复核失败：AllowNoPassword 应为 %v，实际 %v（%s）", want, got, confPath)
	}
	// 复核 2（请求级）：确认改动没有把入口弄坏（写盘成功 ≠ 服务仍可用）。
	if _, ok := waitHTTPBody(ctx, pmaProbeURL, 10*time.Second); !ok {
		return false, fmt.Errorf("AllowNoPassword 已改为 %v，但 /phpmyadmin/ 请求级复核没通过（配置可能有语法问题）：%s",
			want, confPath)
	}
	return true, nil
}

// syncPMAAllowNoPasswordAfterCredential 在 MySQL 凭据闭环之后校正一次（D43）。
// 这里**不让凭据闭环失败**：闭环本身的成果比这一行配置重要得多；但失败必须如实进
// result.Warning —— 不能只写日志然后当成功。
func (m *Manager) syncPMAAllowNoPasswordAfterCredential(ctx context.Context, result *InstallResult) {
	changed, err := m.SyncPhpMyAdminAllowNoPassword(ctx)
	if err != nil {
		msg := "MySQL 凭据已闭环，但按真实口令状态校正 phpMyAdmin 的 AllowNoPassword 失败：" + err.Error()
		if result != nil {
			result.Warning = appendLNMPWarning(result.Warning, msg)
			result.step(ctx, "警告："+msg)
		}
		return
	}
	if changed && result != nil {
		result.step(ctx, "已按真实 MySQL 口令状态校正 phpMyAdmin 的 AllowNoPassword")
	}
}

// ensureFPMInclude 确保 nginx 里有可复用的 php-fpm 片段（面板的站点生成也会用它）：
// 单独装 phpMyAdmin 时缺了它，vhost 的 include 指向不存在的文件，nginx -t 直接失败。
func (m *Manager) ensureFPMInclude(ctx context.Context, result *InstallResult) error {
	dir := filepath.Join(m.brewPrefix(), "etc", "nginx", "includes")
	path := filepath.Join(dir, "php-fpm.conf")
	if existing, err := os.ReadFile(path); err == nil {
		// 已有该文件时**不要无条件覆盖**（用户可能手调过），只修早期版本写坏的那份：
		// 用了 alias 场景不可用的 $document_root$fastcgi_script_name 且没有 $request_filename
		// （真机上 phpMyAdmin 打不开的原因）。
		broken := strings.Contains(string(existing), "$document_root$fastcgi_script_name") &&
			!strings.Contains(string(existing), "$request_filename")
		if !broken {
			return nil
		}
		result.Steps = append(result.Steps,
			"检测到 php-fpm 片段是为 alias 场景不可用的旧版本，已替换为正确写法")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 nginx includes 目录失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(fpmIncludeContent), 0o644); err != nil {
		return fmt.Errorf("写入 php-fpm 片段失败: %w", err)
	}
	// **改完片段必须 reload**：真机上就踩到片段已换好、但无人触发 reload，phpMyAdmin 依旧 404。
	// 改配置不 reload 等于没改。
	if err := m.reloadNginx(ctx); err != nil {
		return err
	}
	// **reload 之后必须请求级复核**（D30）：`nginx -s reload` 退出码 0 不代表配置已加载。
	// 这份片段只有"既引用它、又声明 /phpmyadmin 的 vhost"才有可请求的观测点；找不到时如实说明，
	// 真正的端到端复核落在安装末尾的 waitHTTPBody(/phpmyadmin/)。
	if name, ok := m.fpmIncludeProbeVhost(); ok {
		if _, ok2 := waitHTTPBody(ctx, pmaProbeURL, 10*time.Second); !ok2 {
			result.step(ctx, "php-fpm 片段已更新，但引用它的站点还没生效，正在重新加载后复核")
			if rerr := m.reloadNginx(ctx); rerr != nil {
				return fmt.Errorf("php-fpm 片段已写入，但让 nginx 加载它失败：%w", rerr)
			}
			body, ok3 := waitHTTPBody(ctx, pmaProbeURL, 20*time.Second)
			if !ok3 {
				return fmt.Errorf("php-fpm 片段已写入 %s 并重载，但引用它的站点 %s 里的 /phpmyadmin/ 仍打不开：%s",
					path, name, describePMAFailure(body))
			}
		}
		result.step(ctx, "已生成 nginx 的 php-fpm 转发片段并重载（已按 "+name+" 做请求级复核）")
	} else {
		result.step(ctx, "已生成 nginx 的 php-fpm 转发片段并重载"+
			"（暂无引用它的 phpMyAdmin 站点；端到端复核见 /phpmyadmin/ 探针）")
	}
	return nil
}

// fpmIncludeProbeVhost 找出"既 include 了历史 php-fpm 片段、又声明了 /phpmyadmin 入口"的 vhost。
// 拿普通 PHP 站点去探 /phpmyadmin/ 会返回 404，那是误报（片段本身没问题）。
func (m *Manager) fpmIncludeProbeVhost() (string, bool) {
	dir := filepath.Join(m.brewPrefix(), "etc", "nginx", "vhosts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		text := string(b)
		if strings.Contains(text, "includes/php-fpm.conf") && strings.Contains(text, "/phpmyadmin") {
			return e.Name(), true
		}
	}
	return "", false
}

// reloadNginx 先测语法再重载。语法不过就拒绝重载并报错 ——
// 一个语法错误会让**所有站点**下线，绝不能盲载。
func (m *Manager) reloadNginx(ctx context.Context) error {
	nginx := filepath.Join(m.brewPrefix(), "bin", "nginx")
	nginxConf := filepath.Join(m.brewPrefix(), "etc", "nginx", "nginx.conf")
	out, err := m.runRoot(ctx, 20*time.Second, nginx, "-c", nginxConf, "-t")
	if err != nil || !strings.Contains(out, "test is successful") {
		return fmt.Errorf("nginx 配置校验未通过，已放弃重载：%s", tailText(stripANSI(out), 300))
	}
	// **关键顺序**：`nginx -t` 以 root 跑，会把 access_log/error_log 这些文件**创建出来**
	// （属主 root）；以真实用户运行的 nginx master reload 时打不开它们，配置根本没加载，
	// 但 `nginx -s reload` 的退出码仍然是 0（真机 2026-09-17）。所以校验之后、重载之前必须交还日志属主。
	m.chownNginxLogTrees()
	if _, err := m.runRoot(ctx, 20*time.Second, nginx, "-c", nginxConf, "-s", "reload"); err != nil {
		return fmt.Errorf("nginx reload 失败: %w", err)
	}
	return nil
}

// chownNginxLogTrees 把 nginx 需要写的日志目录（递归）交给真实用户。
// 不报步骤、失败也不阻断：真正的判据是最后"页面真的能打开"。
func (m *Manager) chownNginxLogTrees() {
	user := strings.TrimSpace(m.opt.UserName)
	if user == "" || user == "root" {
		return
	}
	for _, root := range m.nginxLogRoots() {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		_, _, _ = chownTreeTo(user, root)
	}
}

// nginxLogRoots 是 nginx 需要写的日志目录（面板约定的两棵）。
// 收敛在一处：ensureNginxLogOwnership 与 chownNginxLogTrees 必须修同一批目录，否则会出现"一处修了、另一处没修"的假成功。
func (m *Manager) nginxLogRoots() []string {
	roots := []string{filepath.Join(m.brewPrefix(), "var", "log", "nginx")}
	if m.opt.UserHome != "" {
		roots = append(roots, filepath.Join(m.opt.UserHome, "www", "_logs"))
	}
	return roots
}

// fpmIncludeContent 是写入 <brew>/etc/nginx/includes/php-fpm.conf 的内容。
// **历史兼容文件，新配置一律不再引用它**（默认站点与站点 vhost 都用显式专属端点）；
// 仍然生成，只因早期 vhost 里可能还 include 它 —— 文件不存在会让 **nginx -t 直接失败**。

// SCRIPT_FILENAME 必须用 $request_filename：用 $document_root$fastcgi_script_name 在 alias 下会拼错路径。
// 等所有机器都重跑过一次一键 LNMP 且 vhost 都是新版生成后可删（删前扫 include 引用）。
var fpmIncludeContent = "# PHP-FPM 通用转发参数(在 location ~ \\.php$ 中 include 本文件)\n" +
	"fastcgi_pass   127.0.0.1:9000;\n" + sites.FastCGIParamsBlock() + "\n"

// DefaultVhostMarker 是"这份 000-default.conf 由 ZizPanel 生成"的**统一**标记。
// 历史上 services 与 internal/web 各写一套互不相认的值，导致动作判定分叉（D44）；
// 现在写盘一律写这个值，判定"是不是面板创建的"时**两个旧值都认**。
const DefaultVhostMarker = "# 默认站点（由 ZizPanel 生成 —— 请勿手工编辑，面板会整份重写）"

// defaultVhostMarkerLegacy 是 services 早期版本写的最小模板标记（只读兼容）。
const defaultVhostMarkerLegacy = "由 ZizPanel 创建：默认站点"

// 模板种类标记：区分"一键 LNMP 收尾补的最小模板"与"「整理默认站点」生成的完整模板"。
// 完整模板含**应用界面代理**、功能更全 —— ensureDefaultVhost 绝不能用自己的最小模板整份盖掉它（D14/D44）。
const (
	defaultVhostKindMinimal = "# 模板类型：最小（一键 LNMP 收尾）；要换成完整模板请用「设置 → 整理默认站点」"
	// DefaultVhostKindFull 由 internal/web 的 buildDefaultVhost 写入。
	DefaultVhostKindFull = "# 模板类型：完整（应用代理 + 受控 phpMyAdmin）"
)

// IsZizPanelDefaultVhost 判定"这份默认站点是不是面板生成的"。**新旧标记都认**，
// 否则已装机器会被判成"用户的文件"而永远得不到维护。
func IsZizPanelDefaultVhost(existing string) bool {
	return strings.Contains(existing, DefaultVhostMarker) ||
		strings.Contains(existing, defaultVhostMarkerLegacy)
}

// defaultVhostAction 根据现有内容决定怎么处理默认站点。纯函数，便于单测。
// 返回 "create"（不存在/空）、"upgrade"（面板创建的最小模板，可原地升级）、"keep"（完整模板或别人的内容，绝不碰）。
func defaultVhostAction(existing string) string {
	if strings.TrimSpace(existing) == "" {
		return "create"
	}
	if !IsZizPanelDefaultVhost(existing) {
		return "keep"
	}
	// 完整模板（含应用界面代理）功能更全：这条"补最小默认站点"的路径不能把它盖回最小版。
	if strings.Contains(existing, DefaultVhostKindFull) {
		return "keep"
	}
	// 旧版完整模板没有 kind 行 → 保守地不碰；旧版最小模板带 legacy 标记，照样能升级。
	if !strings.Contains(existing, defaultVhostKindMinimal) &&
		!strings.Contains(existing, defaultVhostMarkerLegacy) {
		return "keep"
	}
	return "upgrade"
}

// defaultVhostNeedsWrite 判断"这次是否需要动 000-default.conf"。纯函数（便于单测）：
// 不存在/空 → 要写；面板早期创建的旧内容 → 内容不同才写（原地升级），一致则一个字都不写；
// 其它内容（用户/完整模板）→ 永不写。
func defaultVhostNeedsWrite(existing, want string) bool {
	switch defaultVhostAction(existing) {
	case "create":
		return true
	case "upgrade":
		return existing != want
	default:
		return false
	}
}

// ensureDefaultVhost 确保默认站点存在：目录 <家目录>/www/localhost，vhost <brew>/etc/nginx/vhosts/000-default.conf。
// 缺失 → 创建；面板早期创建的（带标记）→ 原地升级；其它内容 → 一字不改。
// 用户 2026-09-17 要求一键安装后默认建一个站，所以挂在 InstallLNMP 收尾里**无条件确保**。
func (m *Manager) ensureDefaultVhost(ctx context.Context, result *InstallResult) error {
	conf := filepath.Join(m.brewPrefix(), "etc", "nginx", "vhosts", "000-default.conf")
	wwwRoot := filepath.Join(m.opt.UserHome, "www")
	siteRoot := filepath.Join(wwwRoot, "localhost")

	existing, readErr := os.ReadFile(conf)
	action := "create"
	if readErr == nil {
		action = defaultVhostAction(string(existing))
	}

	// 先把站点文件准备好（无论 vhost 动不动，缺了就补）：默认站点是**纯静态**的
	// （用户 2026-09-18 明确要求"只需要一个 index.html"），只建 index.html；phpMyAdmin 用它的 location。
	// 以前多建的 index.php 会把 index.html 顶下去、让"默认站点复核"永远失败（默认站点复核）。
	if _, _, herr := sites.EnsureLocalhostPlaceholder(wwwRoot); herr != nil {
		result.Warning = appendWarning(result.Warning, "默认站点占位页创建失败："+herr.Error())
	}

	if m.opt.UserName != "" {
		_, _, _ = chownTreeTo(m.opt.UserName, siteRoot)
	}

	switch action {
	case "keep":
		// 不碰，但如实告诉用户默认站点在哪（要求 3）。
		// D44：必须分清"面板的完整模板"与"别人的文件" —— 以前把前者报成"非面板创建"，误导用户。
		if IsZizPanelDefaultVhost(string(existing)) {
			result.step(ctx, "默认站点已是面板的完整模板（含应用代理，未改动）："+siteRoot)
		} else {
			result.step(ctx, "默认站点已存在（非面板创建，未改动）："+siteRoot)
		}
		return nil
	}

	// 解析要写进 fastcgi_pass 的端点：优先"真的在监听"的版本，其次面板默认 PHP 版本。
	pass, version := m.defaultFastCGIPass()
	if pass == "" {
		msg := "没有可用的 PHP 端点，默认站点暂时无法执行 PHP（请先确保 PHP 版本已安装并监听）；" +
			"站点目录与 index.html 已就绪：" + siteRoot
		result.Warning = appendWarning(result.Warning, msg)
		result.step(ctx, "警告："+msg)
		return nil
	}

	content := m.pmaDefaultVhostContent(wwwRoot, siteRoot, pass, version)
	// "upgrade" 只在**内容真的不同**时才写盘：一次任务里 InstallLNMP 与 phpMyAdmin
	// 都会确保默认站点，内容一致就什么都不做（幂等包括"不折腾"）。
	if !defaultVhostNeedsWrite(string(existing), content) {
		result.step(ctx, "默认站点已是最新（内容一致，未改写）："+siteRoot)
		return nil
	}
	oldText := ""
	if action == "upgrade" {
		// 到这一步才说"正在升级" —— 免得幂等重跑时出现"正在升级…"紧跟"已是最新"的自相矛盾两行。
		result.step(ctx, "默认站点是面板早期创建的，正在升级为「用专属 PHP 端点执行 index.php」的版本")
		oldText = string(existing)
	}
	if err := m.writeNginxConfAndReload(ctx, conf, oldText, content, result,
		"默认站点已就绪："+siteRoot+"（index.php 由 PHP "+version+" 执行，fastcgi_pass "+pass+"）"); err != nil {
		return err
	}
	return nil
}

// defaultFastCGIPass 选一个"当前可用"的 PHP FastCGI 端点给默认站点用。
// 收敛在 sites.PreferredFastCGIPass（internal/web「整理默认站点」用同一份），否则两处会挑出不同版本。
func (m *Manager) defaultFastCGIPass() (pass, version string) {
	return sites.PreferredFastCGIPass(m.brewPrefix())
}

// pmaDefaultVhostContent 生成默认站点配置（最小但可用，且能跑 PHP）。
// fastcgiPass 必须是该机器当前 PHP 版本的**专属端点**（socket），由调用方经 defaultFastCGIPass 传入；
// 与 internal/web 的 buildDefaultVhost 两个入口都不会覆盖对方已有的内容。
func (m *Manager) pmaDefaultVhostContent(wwwRoot, siteRoot, fastcgiPass, phpVersion string) string {
	logDir := filepath.Join(wwwRoot, "_logs")
	params := strings.TrimSpace(sites.FastCGIParamsBlock())
	lim := m.limits()

	var b strings.Builder
	b.WriteString(DefaultVhostMarker + "\n")
	b.WriteString(defaultVhostKindMinimal + "\n")
	b.WriteString("# 只在缺失时创建；面板自己创建的最小模板会原地升级；其它内容一律不改。\n")
	b.WriteString("server {\n")
	b.WriteString("    listen       80 default_server;\n")
	b.WriteString("    server_name  _;\n\n")
	fmt.Fprintf(&b, "    root   %s;\n", siteRoot)
	// 纯静态默认站点：index.html 在前，index.php 在后（用户明确要求，同上）。
	b.WriteString("    index  index.html index.php;\n\n")
	fmt.Fprintf(&b, "    access_log  %s/localhost.access.log;\n", logDir)
	fmt.Fprintf(&b, "    error_log   %s/localhost.error.log warn;\n\n", logDir)
	// 请求体上限：默认 512m（nginx 出厂 1m 会让几十 MB 的 SQL 导入直接 413）。
	b.WriteString("    client_max_body_size " + lim.ClientMaxBodySize + ";\n\n")
	b.WriteString("    location / {\n        try_files $uri $uri/ =404;\n    }\n\n")
	b.WriteString("    # PHP：" + phpVersion + " 的专属 FastCGI 端点（多版本共存，不占 9000）\n")
	b.WriteString("    location ~ \\.php$ {\n")
	fmt.Fprintf(&b, "        fastcgi_pass %s;\n", fastcgiPass)
	for _, line := range strings.Split(params, "\n") {
		b.WriteString("        " + line + "\n")
	}
	b.WriteString("    }\n\n")
	// phpMyAdmin 入口**不在这里手写**：与 internal/web 完整模板、"插入已有默认站点"
	// 共用 PMAEntryBlock，安全指令（allow/deny/301）与卸载标记不可能再漂移（D10/D14）。
	b.WriteString(PMAEntryBlock(PMAEntryOptions{
		Share:             m.pmaPaths().Share,
		FastCGIPass:       fastcgiPass,
		PHPVersion:        phpVersion,
		ClientMaxBodySize: lim.ClientMaxBodySize,
	}))
	b.WriteString("}\n")
	return b.String()
}

// pmaProbeURL 是"phpMyAdmin 入口是否真的生效"的请求级复核地址。
const pmaProbeURL = "http://127.0.0.1/phpmyadmin/"

// phpMyAdmin nginx 入口的**唯一**生成器（D14）：以前三份各自手写的生成器已经漂移 ——
// insert 版既没有 allow/deny 也没有 301，phpMyAdmin 会对局域网/全网敞开（违反"必须登录面板才能访问"）。
// 现在三处都调用 PMAEntryBlock，安全指令只有这一份。

// pmaMarker 是安装器写进默认站点的注释，卸载时按它定位要删的整段入口。
// 不能只按 `/phpmyadmin` 匹配：那可能把用户自己写的 phpMyAdmin location 删掉。
const pmaMarker = "# 由 ZizPanel 添加：phpMyAdmin 入口"

// PMAEntryOptions 是生成 phpMyAdmin nginx 入口所需的全部输入。
type PMAEntryOptions struct {
	Share       string // phpMyAdmin 的 web 根（alias 目标）
	FastCGIPass string // 该机器当前 PHP 版本的专属 FastCGI 端点；空 = 没有可用端点
	PHPVersion  string // 只用于注释，方便排查
	Indent      string // 每行前缀；空则用 "    "
	// ClientMaxBodySize 写进这个 location 的 `client_max_body_size`（如 "512m"）。
	// location 里必须再写一遍：入口可能被插进**用户自己的默认站点**（server 级没有我们的上限），
	// 只靠 server 级会漏；这是用户报障 413 的直接路径。空 = 用 sites.DefaultClientMaxBodySize。
	ClientMaxBodySize string
}

// PMAEntryBlock 是 phpMyAdmin nginx 入口的唯一生成器（三处调用点共用）。
// 安全基线固定在函数里，调用方**无法**生成不带访问限制的版本：301 到 /phpmyadmin/，
// location ^~ /phpmyadmin 只允许 127.0.0.1/::1、其余 deny all；无 PHP 端点时**绝不**写 `fastcgi_pass ;`。
func PMAEntryBlock(opt PMAEntryOptions) string {
	indent := opt.Indent
	if indent == "" {
		indent = "    "
	}
	params := strings.TrimSpace(sites.FastCGIParamsBlock())
	var b strings.Builder
	w := func(s string) { b.WriteString(indent + s + "\n") }

	w("# " + pmaMarker + "（PHP " + opt.PHPVersion + " 的专属端点）")
	w("location = /phpmyadmin {")
	w("    return 301 /phpmyadmin/;")
	w("}")
	w("location ^~ /phpmyadmin {")
	w("    allow 127.0.0.1;")
	w("    allow ::1;")
	w("    deny all;")
	// 请求体上限必须**直接写在 phpMyAdmin 的 location 里**：默认站点可能是用户自己的，
	// server 级没有面板的 client_max_body_size，导入大 SQL 会重新变成 413（用户报障原文）。
	bodyLimit := opt.ClientMaxBodySize
	if strings.TrimSpace(bodyLimit) == "" {
		bodyLimit = sites.DefaultClientMaxBodySize
	}
	if err := sites.ValidateSizeValue("client_max_body_size", bodyLimit); err != nil {
		// 生成器不接受非法值：宁可退化成默认，也不写出会让 nginx 拒绝加载的配置。
		bodyLimit = sites.DefaultClientMaxBodySize
	}
	w("    client_max_body_size " + bodyLimit + ";")
	w("    alias " + opt.Share + ";")
	w("    index index.php;")
	w("    try_files $uri $uri/ /phpmyadmin/index.php$is_args$args;")
	w("")
	w("    location ~ \\.php$ {")
	if opt.FastCGIPass == "" {
		w("        # 本机没有可用 PHP 端点：这个入口暂时无法执行 PHP。")
		w("        # 到「网站管理 → 🐘 PHP 环境」修好端点后重试。")
	} else {
		w("        fastcgi_pass " + opt.FastCGIPass + ";")
		for _, line := range strings.Split(params, "\n") {
			w("        " + line)
		}
	}
	w("    }")
	w("}")
	return b.String()
}

// pmaVhostInsertBlock 保留旧签名（历史单测在用），内容改为统一生成器的插入形态。
func pmaVhostInsertBlock(share, fastcgiPass, phpVersion string) string {
	return "\n" + PMAEntryBlock(PMAEntryOptions{
		Share:       share,
		FastCGIPass: fastcgiPass,
		PHPVersion:  phpVersion,
	}) + "\n"
}

// pmaVhostAction 描述"已有默认站点里的 phpMyAdmin 入口"该怎么处理。
type pmaVhostAction string

const (
	pmaVhostInsert  pmaVhostAction = "insert"  // 没有入口 → 插入
	pmaVhostReplace pmaVhostAction = "replace" // 有入口但不是我们要的那份 → 原地替换
	pmaVhostKeep    pmaVhostAction = "keep"    // 已有入口且与期望完全一致 → 不动
)

// pmaVhostPlan 纯函数：给定现有默认站点文本与期望入口块，决定插入 / 原地替换 / 不动。
// D30：**不能**用"配置里含 /phpmyadmin 字样"就跳过 —— 那可能是旧版本生成器写下的、
// 没有 allow/deny 的坏块，也可能 301 之后没有实际可用的块；只有内容一致才算"已就绪"。
func pmaVhostPlan(existing, wantBlock, share string) (pmaVhostAction, string, error) {
	if entry, found := findPMAEntry(existing, share); found {
		if normalizeNginxBlock(entry) == normalizeNginxBlock(wantBlock) {
			return pmaVhostKeep, existing, nil
		}
		stripped, err := removePMAEntry(existing, share)
		if err != nil {
			return "", "", err
		}
		// 移除后还留着别的 phpMyAdmin location：再插一份会 duplicate location，不能猜，如实报错。
		if hasPMAEntryLine(stripped) {
			return "", "", errPMAForeignEntry
		}
		merged, err := insertPMABlock(stripped, wantBlock)
		if err != nil {
			return "", "", err
		}
		return pmaVhostReplace, merged, nil
	}
	// 已有认不出的 /phpmyadmin location：再插一份会 `duplicate location` → nginx -t 直接失败
	// （整台机器站点一起下线），不能猜，如实报错让人工处理。
	if hasPMAEntryLine(existing) {
		return "", "", errPMAForeignEntry
	}
	merged, err := insertPMABlock(existing, wantBlock)
	if err != nil {
		return "", "", err
	}
	return pmaVhostInsert, merged, nil
}

// errPMAForeignEntry 表示默认站点里有一个面板认不出来的 /phpmyadmin 入口。
var errPMAForeignEntry = errors.New(
	"默认站点里已有 /phpmyadmin 入口，但它不是面板生成的格式，无法安全接管" +
		"（再插一份会造成 duplicate location，nginx 会拒绝加载）")

// ensurePMAVhost 把 phpMyAdmin 的 location 接进默认站点：不需要域名和额外端口。
// D30：跳过前必须**比对内容**（旧版只判"含 /phpmyadmin 字样"就跳过，坏块被一直留着）；
// 写完（或确认已就绪）后必须做**请求级复核**（reload 退出码 0 ≠ 配置已加载）。
func (m *Manager) ensurePMAVhost(ctx context.Context, result *InstallResult) error {
	vhostDir := filepath.Join(m.brewPrefix(), "etc", "nginx", "vhosts")
	conf := filepath.Join(vhostDir, "000-default.conf")
	if err := os.MkdirAll(vhostDir, 0o755); err != nil {
		return fmt.Errorf("创建 vhosts 目录失败: %w", err)
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		return fmt.Errorf("读取默认站点配置失败（%s）: %w", conf, err)
	}
	text := string(data)

	// PHP 端点必须按机器/版本解析（不能 include 写死 9000 的片段），
	// 写错会让 phpMyAdmin 在"PHP 只听专属 socket"的机器上 502。
	pass, phpVersion := m.defaultFastCGIPass()
	if pass == "" {
		return fmt.Errorf("本机没有可用的 PHP FastCGI 端点，暂时无法把 phpMyAdmin 接进默认站点：" +
			"请先到「网站管理 → 🐘 PHP 环境」把某个 PHP 版本的端点修好，然后重试")
	}
	share := m.pmaPaths().Share
	want := PMAEntryBlock(PMAEntryOptions{Share: share, FastCGIPass: pass, PHPVersion: phpVersion})

	action, newText, planErr := pmaVhostPlan(text, want, share)
	if planErr != nil {
		return fmt.Errorf("%w（位置：%s）", planErr, conf)
	}
	switch action {
	case pmaVhostKeep:
		result.step(ctx, "默认站点里的 phpMyAdmin 入口已是最新（内容一致，未改写）")
	default:
		msg := "已把 phpMyAdmin 接入默认站点（入口只允许本机，外部直连 403）"
		if action == pmaVhostReplace {
			msg = "默认站点里的 phpMyAdmin 入口与当前 PHP 端点/访问限制不一致，已原地替换"
		}
		if err := m.writeNginxConfAndReload(ctx, conf, text, newText, result, msg); err != nil {
			return err
		}
	}

	// 请求级复核：`nginx -s reload` 返回 0 不代表配置生效；失败就再自愈 reload 一次，仍失败如实报错。
	if _, ok := waitHTTPBody(ctx, pmaProbeURL, 10*time.Second); !ok {
		result.step(ctx, "phpMyAdmin 入口还不通，正在让 nginx 重新加载后复核")
		if rerr := m.reloadNginx(ctx); rerr != nil {
			return fmt.Errorf("让 nginx 加载默认站点失败：%w", rerr)
		}
		body, ok2 := waitHTTPBody(ctx, pmaProbeURL, 20*time.Second)
		if !ok2 {
			return fmt.Errorf("phpMyAdmin 入口已写入 %s，但 /phpmyadmin/ 仍打不开：%s",
				conf, describePMAFailure(body))
		}
	}
	result.step(ctx, "phpMyAdmin 入口已生效（请求级复核通过）")
	return nil
}

// insertPMABlock 把入口块插到最后一个 server 块结束符（}）之前。
func insertPMABlock(text, block string) (string, error) {
	trimmed := strings.TrimRight(text, "\n")
	i := strings.LastIndex(trimmed, "}")
	if i < 0 {
		return "", fmt.Errorf("默认站点配置结构异常（找不到 server 块结束符），拒绝修改")
	}
	return text[:i] + "\n" + block + text[i:], nil
}

// writeNginxConfAndReload 原子写配置 → nginx -t → reload，失败则回滚。
// 一个语法错误就能让 nginx 拒绝 reload、严重时**所有站点**下线，所以永远留退路。
func (m *Manager) writeNginxConfAndReload(ctx context.Context, conf, oldText, newText string,
	result *InstallResult, okMsg string) error {
	backup := conf + ".zizpanel.bak"
	if _, err := os.Stat(backup); err != nil {
		_ = os.WriteFile(backup, []byte(oldText), 0o644)
	}
	tmp := conf + ".tmp"
	if err := os.WriteFile(tmp, []byte(newText), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", filepath.Base(conf), err)
	}
	if err := os.Rename(tmp, conf); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", filepath.Base(conf), err)
	}

	if err := m.reloadNginx(ctx); err != nil {
		// 回滚：配置还原回去。oldText 为空 = 文件本来不存在（例如刚补的默认站点）：
		// 那时应当**删掉**它 —— 空文件语法合法，却会让下一次判断误以为"默认站点已存在"。
		if strings.TrimSpace(oldText) == "" {
			_ = os.Remove(conf)
		} else {
			_ = os.WriteFile(conf, []byte(oldText), 0o644)
		}
		_ = m.reloadNginx(ctx)
		return fmt.Errorf("%v（已还原 %s）", err, filepath.Base(conf))
	}
	result.step(ctx, okMsg)
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// existingBlowfishSecret 从已有配置里读出 blowfish_secret，读不到返回空串。
// 用正则而不是"找第一个/最后一个引号"：那样会把键名一起圈进去，返回一段越滚越长的垃圾（实测踩到过）。
func existingBlowfishSecret(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := pmaSecretRe.FindSubmatch(b)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

// pmaSecretRe 匹配 $cfg['blowfish_secret'] = '....';
var pmaSecretRe = regexp.MustCompile(`blowfish_secret'\]\s*=\s*'([^']*)'`)

// validBlowfishSecret 判断读到的密钥是不是我们生成的那种（32 位十六进制）。
// 宁可换掉"看起来能用但来路不明"的值（代价只是当前会话失效一次），也不当成可信密钥。
func validBlowfishSecret(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// runCurlCtx 取回页面内容与 HTTP 状态码。
// 必须带状态码：nginx 的 404/502 错误页**也是有内容的 HTML**，旧实现只判断"body 非空"
// 就把 404 当成了"phpMyAdmin 已可用"（真机 2026-09-17）。
func runCurlCtx(ctx context.Context, url string, timeoutSec int) (body string, code int, err error) {
	out, cerr := exec.CommandContext(ctx, "/usr/bin/curl",
		"-sS", "--max-time", strconv.Itoa(timeoutSec),
		"-w", "\n__ZP_HTTP_CODE__%{http_code}", url).Output()
	text := string(out)
	code = 0
	if i := strings.LastIndex(text, "__ZP_HTTP_CODE__"); i >= 0 {
		if n, aerr := strconv.Atoi(strings.TrimSpace(text[i+len("__ZP_HTTP_CODE__"):])); aerr == nil {
			code = n
		}
		text = text[:i]
	}
	return text, code, cerr
}

// phpMyAdminLooksOK 判断响应体是否真的是 phpMyAdmin 的页面。
// 正向标志（登录表单字段 / 品牌名）比"没有错误关键字"可靠：后者会漏过 nginx 404 页和别的站点页面。
func phpMyAdminLooksOK(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{"pma_username", "phpmyadmin", "pma_navigation"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// waitHTTPBody 轮询直到 phpMyAdmin 首页**真的**能打开，返回响应体：
// 判定必须同时满足 HTTP 200 + 页面含正向标志 + 没有错误关键字。

// 真机 2026-09-17 报"已可用"而实际 404：reload 退出码仍是 0，404 页"非空、含 <html>"被旧判据当成成功。
// 教训：**"能谎报成功的功能比没做更糟"**（AGENTS.md 第三节）。
func waitHTTPBody(ctx context.Context, url string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		body, code, err := runCurlCtx(ctx, url, 6)
		if err == nil && strings.TrimSpace(body) != "" {
			last = body
			if code == 200 && phpMyAdminErrorIn(body) == "" && phpMyAdminLooksOK(body) {
				return body, true
			}
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return last, false
}

// describePMAFailure 给出"为什么打不开"的可读原因（用于告警）。
func describePMAFailure(body string) string {
	if bad := phpMyAdminErrorIn(body); bad != "" {
		return "页面返回了 phpMyAdmin 错误页：" + bad
	}
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "404 not found"):
		return "nginx 返回 404：/phpmyadmin 这个 location 没有被 nginx 加载（通常是重载失败或默认站点缺失）"
	case strings.Contains(lower, "502 bad gateway"), strings.Contains(lower, "504 gateway"):
		return "nginx 返回 502/504：php-fpm 没在运行或 fastcgi 端点不对"
	case strings.TrimSpace(body) == "":
		return "20 秒内没有任何响应"
	}
	return "页面不是 phpMyAdmin（缺 pma_username/phpMyAdmin 标志）"
}

// phpMyAdminErrorIn 检查响应体里有没有 phpMyAdmin 的典型错误，返回错误摘要
// （这些是实测/文档里出现过的原话，比只看状态码可靠）。
func phpMyAdminErrorIn(body string) string {
	for _, marker := range []string{
		"is not readable",
		"Existing configuration file",
		"cannot load or save configuration",
		"the mbstring extension is missing",
		"Fatal error",
		"Parse error",
	} {
		if strings.Contains(body, marker) {
			return marker
		}
	}
	return ""
}

// RepairPhpMyAdminConfigPerm 修正 phpMyAdmin 配置文件的归属与权限，返回是否做了修改。
// 文件由面板以 root 写入、php-fpm 以「真实用户」读取，归属一错就渲染 "Existing configuration file
// (...) is not readable."，而该错误页状态码同样是 200。

// 漂移来源：brew 升级重建、旧面板写成 root、手工 sudo 改过。
func (m *Manager) RepairPhpMyAdminConfigPerm() (bool, error) {
	conf := m.pmaPaths().ConfReal
	fi, err := os.Stat(conf)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // 没装 phpMyAdmin，无需处理
		}
		return false, err
	}
	// 面板自身可能不是 root；此修复只能在有权 chown 时进行。
	if m.opt.UserName == "" {
		return false, nil
	}
	uid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-u", m.opt.UserName)))
	if err != nil {
		return false, fmt.Errorf("解析用户 %s 的 uid 失败: %w", m.opt.UserName, err)
	}

	// 判定是否需要修复：归属不是真实用户，或属主读位缺失。
	// 不能用 os.Open 判断 —— 面板以 root 运行任何文件都能读，永远发现不了 php-fpm 读不到。
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	needChown := int(st.Uid) != uid
	needChmod := fi.Mode().Perm()&0o400 == 0
	if !needChown && !needChmod {
		return false, nil
	}

	gid, err := strconv.Atoi(strings.TrimSpace(runOutput("/usr/bin/id", "-g", m.opt.UserName)))
	if err != nil {
		return false, fmt.Errorf("解析用户 %s 的 gid 失败: %w", m.opt.UserName, err)
	}
	// 0600 + 归真实用户：php-fpm 读得到，又不像 brew 原始 0644 那样对所有人可读。
	if err := os.Chown(conf, uid, gid); err != nil {
		return false, fmt.Errorf("修正 phpMyAdmin 配置归属失败: %w", err)
	}
	if err := os.Chmod(conf, 0o600); err != nil {
		return false, fmt.Errorf("修正 phpMyAdmin 配置权限失败: %w", err)
	}
	return true, nil
}

// UninstallPhpMyAdmin 卸载 phpMyAdmin：撤掉 nginx 入口 + 删包 + 摘掉 web 根。
// 必须由面板来做：直接 brew uninstall 会留下指向不存在目录的 location（访问变 404/502）；
// 顺序**先撤入口、再删包**，反了会有一段时间是坏的。

// D10：识别先按标记、再按 alias 指向的 phpMyAdmin 目录；都认不出而配置里确实有 /phpmyadmin 时
// **如实报错**中止 —— 绝不静默跳过然后报成功。
// app.RuntimePath 是"装着"的证据：手工装的那份不在 Homebrew 里，卸载就得真的删掉它。
func (m *Manager) UninstallPhpMyAdmin(ctx context.Context, app App, removeData, force bool, result *InstallResult) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("卸载 phpMyAdmin 需要以 root 运行")
	}
	// ---- 1. 撤掉 nginx 入口 ----
	conf := filepath.Join(m.brewPrefix(), "etc", "nginx", "vhosts", "000-default.conf")
	data, readErr := os.ReadFile(conf)
	switch {
	case readErr == nil:
		text := string(data)
		action, newText, perr := planPMAEntryRemoval(text, m.pmaPaths().Share)
		if perr != nil {
			return fmt.Errorf("%w（位置：%s）", perr, conf)
		}
		switch action {
		case pmaRemovalRemove:
			if err := m.writeNginxConfAndReload(ctx, conf, text, newText, result,
				"已移除默认站点里的 phpMyAdmin 入口"); err != nil {
				return err
			}
		case pmaRemovalUnmanaged:
			// 找不到该删的块但配置里确实有 /phpmyadmin：如实报错、中止卸载 —— 不猜、不静默、不假报成功。
			return fmt.Errorf("没找到面板添加的 phpMyAdmin 入口（可能有残留）：%s 里出现了 /phpmyadmin，"+
				"但它不是面板生成的格式，无法安全移除。已中止卸载，以免删错用户自己的配置；"+
				"请手工检查该文件后重试", conf)
		default:
			if result != nil {
				result.step(ctx, "默认站点里没有 phpMyAdmin 入口（无需移除）")
			}
		}
	case os.IsNotExist(readErr):
	default:
		return fmt.Errorf("读取默认站点配置失败（%s）: %w", conf, readErr)
	}

	// ---- 2. 删包 ----
	// force=true 只由用户在确认框里明确选「强制卸载」时传入（brew --ignore-dependencies）。
	brewOwned := m.brewHas(ctx, "phpmyadmin")
	if brewOwned {
		if err := m.brewUninstall(ctx, "phpmyadmin", force, result); err != nil {
			return err
		}
	} else if result != nil {
		result.step(ctx, "phpmyadmin 未安装，跳过")
	}

	// ---- 2b. 手工装的 web 根 ----
	// 「已安装」的判据是**这个目录在**（app 的 RuntimePath）；手工装的是真目录，不删它卡片会永远停在
	// 「已安装」，用户点不到「安装」。

	// 只删声明的那一个安装体：RuntimePath 没声明 → 退回 pmaPaths().Share，软链 → 只摘链不动 Cellar。
	if !brewOwned {
		share := ResolveRuntimePath(app.RuntimePath, m.brewPrefix(), m.opt.UserHome)
		if share == "" {
			share = m.pmaPaths().Share
		}
		if _, err := os.Lstat(share); err == nil {
			if result != nil {
				result.step(ctx, "移除 phpMyAdmin 的 web 根目录 "+share+
					"（面板判它「已安装」靠的就是这个目录；你的数据库与库表不受影响）")
			}
			if err := os.RemoveAll(share); err != nil {
				return fmt.Errorf("删除 %s 失败: %w", share, err)
			}
		}
	}

	// ---- 3. 删面板自己的配置文件 ----
	confFile := filepath.Join(m.brewPrefix(), "etc", "phpmyadmin.config.inc.php")
	if removeData {
		if err := os.Remove(confFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败: %w", confFile, err)
		}
		if result != nil {
			result.step(ctx, "已删除 "+confFile)
		}
	} else if result != nil {
		result.step(ctx, "保留 "+confFile+"（需要删除请勾选「同时删除数据/配置」）")
	}
	return nil
}

// pmaRemovalAction 描述卸载时对默认站点里 phpMyAdmin 入口的处理结果。
type pmaRemovalAction string

const (
	pmaRemovalNone      pmaRemovalAction = "none"      // 配置里没有 phpMyAdmin 入口
	pmaRemovalRemove    pmaRemovalAction = "remove"    // 找到并移除了面板的入口
	pmaRemovalUnmanaged pmaRemovalAction = "unmanaged" // 有 /phpmyadmin 但认不出格式 → 如实报告
)

// planPMAEntryRemoval 纯函数：决定卸载时怎么处理默认站点里的 phpMyAdmin 入口。
// 先按标记识别，识别不到再按"alias 指向 phpMyAdmin web 根"识别老版本残留；
// 两条都识别不出而配置里确实出现 /phpmyadmin 时返回 pmaRemovalUnmanaged —— 调用方必须**如实报错**（D10）。
func planPMAEntryRemoval(text, share string) (pmaRemovalAction, string, error) {
	if _, found := findPMAEntry(text, share); found {
		newText, err := removePMAEntry(text, share)
		if err != nil {
			return "", "", err
		}
		// 移除后仍留下别的 phpMyAdmin location：不能当"清干净了"报成功（D10：宁可如实报错）。
		if newText == text || hasPMAEntryLine(newText) {
			return pmaRemovalUnmanaged, text, nil
		}
		return pmaRemovalRemove, newText, nil
	}
	if hasPMAEntryLine(text) {
		return pmaRemovalUnmanaged, text, nil
	}
	return pmaRemovalNone, text, nil
}

// hasPMAEntryLine 判断配置里是否还有（非注释的）phpMyAdmin location 入口。
// 不能用朴素的 "/phpmyadmin" 子串判断：完整默认站点的头注释里就写着 "/phpmyadmin/" 字样，
// 那会把它误判成入口、把一次正常的卸载误报成"有残留"、甚至拒绝卸载。
func hasPMAEntryLine(text string) bool {
	for _, ln := range strings.Split(text, "\n") {
		if isPMAStartLine(ln) {
			return true
		}
	}
	return false
}

// findPMAEntry 返回现有配置里 phpMyAdmin 入口的那段文本。第二个返回值表示是否找到。
func findPMAEntry(text, share string) (string, bool) {
	lines := strings.Split(text, "\n")
	s, e, found, err := findPMAEntryRange(lines, share)
	if err != nil || !found {
		return "", false
	}
	return strings.Join(lines[s:e+1], "\n"), true
}

// findPMAEntryRange 返回入口在 lines 中的起止行号（含）。纯函数，便于单测。
func findPMAEntryRange(lines []string, share string) (start, end int, found bool, err error) {
	// 1) 面板标记优先（新版的三个生成器都写它；卸载就靠它精确定位）
	for i, ln := range lines {
		if !strings.Contains(ln, pmaMarker) {
			continue
		}
		if j := nextNonBlank(lines, i+1); j >= 0 && isPMAStartLine(lines[j]) {
			e, berr := phpMyAdminLocationRunEnd(lines, j)
			if berr != nil {
				return -1, -1, false, berr
			}
			return i, e, true, nil
		}
		// 标记行后面不是入口（异常文件）：至少把标记行本身交出去
		return i, i, true, nil
	}
	// 2) 老文件没有标记：按 alias 指向 phpMyAdmin 的 web 根识别
	for i, ln := range lines {
		if !isPMAStartLine(ln) {
			continue
		}
		e, berr := braceBlockEnd(lines, i)
		if berr != nil {
			continue
		}
		if !strings.Contains(strings.Join(lines[i:e+1], "\n"), share) {
			continue
		}
		s := i
		// 入口前面可能还跟着 `location = /phpmyadmin { return 301 ...; }`
		if k := prevNonBlank(lines, i-1); k >= 0 && strings.TrimSpace(lines[k]) == "}" {
			if b := braceBlockStart(lines, k); b >= 0 && strings.Contains(lines[b], "/phpmyadmin") {
				s = b
			}
		}
		return s, e, true, nil
	}
	return -1, -1, false, nil
}

// removePMAEntry 从配置里删掉识别到的 phpMyAdmin 入口（找不到就原样返回）。
func removePMAEntry(text, share string) (string, error) {
	// 有标记就走标记那条路（能一次摘掉 301 + 主体这组连续 location）。
	if strings.Contains(text, pmaMarker) {
		return removeMarkedLocation(text, pmaMarker)
	}
	// 老文件没有标记：按 alias 指向的 phpMyAdmin web 根定位。
	lines := strings.Split(text, "\n")
	s, e, found, err := findPMAEntryRange(lines, share)
	if err != nil {
		return "", err
	}
	if !found {
		return text, nil
	}
	out := append([]string{}, lines[:s]...)
	k := e + 1
	for k < len(lines) && strings.TrimSpace(lines[k]) == "" {
		k++
	}
	out = append(out, lines[k:]...)
	return strings.Join(out, "\n"), nil
}

// removeMarkedLocation 从 nginx 配置里删掉"以某条注释开头的那一整段 location"。
// 一个入口可能是连续的多个 location 块（301 + 主体），所以消费**连续的、都指向
// /phpmyadmin 的块**；按花括号配对跳过，找不到配对时报错（宁可不动，也不留半截配置）。
func removeMarkedLocation(text, marker string) (string, error) {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if !strings.Contains(ln, marker) {
			continue
		}
		end := i
		if j := nextNonBlank(lines, i+1); j >= 0 && isPMAStartLine(lines[j]) {
			e, err := phpMyAdminLocationRunEnd(lines, j)
			if err != nil {
				return "", err
			}
			end = e
		}
		out := append([]string{}, lines[:i]...)
		k := end + 1
		for k < len(lines) && strings.TrimSpace(lines[k]) == "" {
			k++
		}
		out = append(out, lines[k:]...)
		return strings.Join(out, "\n"), nil
	}
	return text, nil
}

// isPMAStartLine 判断一行是不是 phpMyAdmin 入口的 location 开头。
func isPMAStartLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "location") && strings.Contains(line, "/phpmyadmin")
}

// phpMyAdminLocationRunEnd 返回从 start 行开始、连续的 phpMyAdmin location 块的末行。
func phpMyAdminLocationRunEnd(lines []string, start int) (int, error) {
	j := start
	end := start
	for {
		e, err := braceBlockEnd(lines, j)
		if err != nil {
			return -1, err
		}
		end = e
		k := nextNonBlank(lines, e+1)
		if k < 0 || !isPMAStartLine(lines[k]) {
			break
		}
		j = k
	}
	return end, nil
}

// braceBlockEnd 返回从 start 行（含 '{'）开始、花括号配对结束的行号（含）。
func braceBlockEnd(lines []string, start int) (int, error) {
	if start < 0 || start >= len(lines) || !strings.Contains(lines[start], "{") {
		return -1, fmt.Errorf("第 %d 行不是带花括号的块开头", start+1)
	}
	depth := 0
	for j := start; j < len(lines); j++ {
		depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
		if depth <= 0 {
			return j, nil
		}
	}
	return -1, fmt.Errorf("块从第 %d 行开始但花括号不配对，拒绝改写", start+1)
}

// braceBlockStart 返回以 end 行（含 '}'）结束的块的起始行号。
func braceBlockStart(lines []string, end int) int {
	depth := 0
	for j := end; j >= 0; j-- {
		depth += strings.Count(lines[j], "}") - strings.Count(lines[j], "{")
		if depth <= 0 {
			return j
		}
	}
	return -1
}

// nextNonBlank / prevNonBlank 跳过空行找相邻的有效行，找不到返回 -1。
func nextNonBlank(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

func prevNonBlank(lines []string, from int) int {
	for i := from; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

// normalizeNginxBlock 归一化一段 nginx 配置用于比较：去掉行尾空白与首尾空行。
func normalizeNginxBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
