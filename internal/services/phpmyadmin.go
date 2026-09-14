package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ============================================================================
//  phpMyAdmin 部署
//
//  为什么面板要装它，而不是自己实现一套数据库管理：
//    面板自研的库/表/账号/SQL 那套，功能上永远追不上 phpMyAdmin，
//    而用户几乎人人会用 phpMyAdmin。合理的分工是：
//      面板 → 运维动作（安装、启停、连接信息、打开入口）
//      phpMyAdmin → 真正的库表与权限管理
//    自研那套保留为"应急入口"（phpMyAdmin 起不来时还能改个密码）。
//
//  实现严格照搬本机已验证的既有约定（配置文件里标着 ZIZDOG-MANAGED）：
//    web 根      <prefix>/share/phpmyadmin
//    配置        <prefix>/etc/phpmyadmin.config.inc.php
//                （share/phpmyadmin/config.inc.php 是软链到它的）
//    nginx      默认站里一段 `location ^~ /phpmyadmin`
//    访问        http://<host>/phpmyadmin/
//
//  为什么坚持同一个布局：这台机器上那套是跑通的（实测 HTTP 200），
//  换一套新布局只会多一份要维护的东西，还容易和已有站点冲突。
// ============================================================================

// pmaPath 返回 phpMyAdmin 的各个约定路径。
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
	// 走 brew 而不是官网下载：实测 files.phpmyadmin.net 在国内**完全不可达**（连接超时），
	// 而 brew 的 phpmyadmin 是 bottled（预编译），走国内镜像能正常下载。
	if !m.brewHas(ctx, "phpmyadmin") {
		result.step(ctx, "正在 brew install phpmyadmin")
		if _, err := m.brewRun(ctx, 15*time.Minute, "install", "phpmyadmin"); err != nil {
			return fmt.Errorf("安装 phpmyadmin 失败: %w", err)
		}
	} else {
		result.step(ctx, "phpmyadmin 已安装，跳过")
	}

	p := m.pmaPaths()
	if _, err := os.Stat(p.Share); err != nil {
		return fmt.Errorf("找不到 phpMyAdmin 的 web 目录 %s（安装可能未完成）", p.Share)
	}

	// ---- 2. 临时目录 ----
	// phpMyAdmin 把模板编译缓存放在这里，目录不存在或不可写时页面会报错。
	if err := os.MkdirAll(p.Temp, 0o755); err != nil {
		return fmt.Errorf("创建 phpMyAdmin 临时目录失败: %w", err)
	}
	if m.opt.UserName != "" {
		_ = chownTo(m.opt.UserName, p.Temp)
	}

	// ---- 3. 配置文件 ----
	// 复用已有的 blowfish_secret：它是会话加密密钥，
	// 每次部署都换掉等于把所有人当前的 phpMyAdmin 登录踢掉 ——
	// 对"只想补个入口"的用户来说这是莫名其妙的掉线。
	// 复用前必须**校验**：只要它不是我们生成的那种随机值就重新生成。
	//
	// 为什么不能"非空就复用"：blowfish_secret 是 phpMyAdmin 用来加密会话的密钥，
	// 一个可预测的值等于会话可被伪造。实测踩到过 —— 早期一版取值写错，
	// 把整行垃圾当成了密钥，而"只要非空就复用"的逻辑又把这个垃圾值
	// 一路固化了下来（当时读出来的是字面量 "blowfish_secret"）。
	secret := existingBlowfishSecret(p.ConfReal)
	if !validBlowfishSecret(secret) {
		secret, err = randomHex(16)
		if err != nil {
			return err
		}
	}
	noPass := m.mysqlRootHasNoPassword(ctx)
	if err := os.MkdirAll(filepath.Dir(p.ConfReal), 0o755); err != nil {
		return err
	}
	conf := pmaConfig(secret, p.Temp, noPass)
	tmp := p.ConfReal + ".tmp"
	if err := os.WriteFile(tmp, []byte(conf), 0o600); err != nil {
		return fmt.Errorf("写入 phpMyAdmin 配置失败: %w", err)
	}
	// 权限与归属：**phpMyAdmin 是由 php-fpm 以「真实用户」身份执行的**，
	// 而本文件是面板以 root 写的。保持 0600 但不改归属的话，
	// php-fpm 读不到它，页面直接报
	//   "Existing configuration file (...) is not readable."
	// 0600 + chown 给真实用户：既让它读得到，又不像 brew 原始的 0644 那样对所有人可读。
	if m.opt.UserName != "" {
		if err := chownTree(m.opt.UserName, tmp); err != nil {
			return fmt.Errorf("设置配置归属失败: %w", err)
		}
	}
	if err := os.Rename(tmp, p.ConfReal); err != nil {
		return fmt.Errorf("替换 phpMyAdmin 配置失败: %w", err)
	}
	// brew 装出来的 config.inc.php 本来就是指向该文件的软链；
	// 若缺失或已被替换成实体文件，这里补上软链。
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
			"检测到 MySQL root 当前无密码，已允许空密码登录（建议尽快在面板里给它设一个）")
	}

	// ---- 4. 接进 nginx ----
	if err := m.ensureFPMInclude(ctx, result); err != nil {
		return err
	}
	if err := m.ensurePMAVhost(ctx, result); err != nil {
		return err
	}

	// ---- 5. 验证：只认真的返回 200 ----
	// 验证**必须看页面内容**，不能只看状态码。
	//
	// phpMyAdmin 在配置读不到时会渲染一个错误页，而那个页面的 HTTP 状态
	// **也是 200** —— 只看状态码会把"打不开"判成成功。真机上就这么漏过去了：
	// 安装报告成功，用户点开却看到 "configuration file is not readable"。
	// 这和本项目其它几处踩的是同一个坑（nginx -t 通过 ≠ 入口已生效、
	// 端口在监听 ≠ 服务能用）：**200 不等于能用**。
	body, ok := waitHTTPBody(ctx, "http://127.0.0.1/phpmyadmin/", 20*time.Second)
	if !ok {
		result.Warning = "phpMyAdmin 已安装并写入配置，但 20 秒内访问 /phpmyadmin/ 无响应。" +
			"请到「日志中心」查看 nginx 与 PHP-FPM 日志。"
		result.step(ctx, "警告："+result.Warning)
		return nil
	}
	if bad := phpMyAdminErrorIn(body); bad != "" {
		result.Warning = "phpMyAdmin 返回了错误页：" + bad +
			"（配置与权限请检查 " + p.ConfReal + "）"
		result.step(ctx, "警告："+result.Warning)
		return nil
	}
	result.step(ctx, "phpMyAdmin 已可用：http://<本机地址>/phpmyadmin/")
	return nil
}

// pmaConfig 生成 phpMyAdmin 配置内容。
//
// 用 cookie 登录（而不是把账号密码写进配置的 config 模式）：
// 后者意味着**任何能访问到这个 URL 的人**都直接拥有数据库权限。
// 多输一次密码换掉这个风险，是值得的。
func pmaConfig(secret, tmpDir string, allowNoPassword bool) string {
	allow := "false"
	if allowNoPassword {
		allow = "true"
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
`, allow, secret, tmpDir)
}

// mysqlRootHasNoPassword 探测 MySQL root 是否为空密码。
//
// 为什么必须探：brew 装的 MySQL 首次初始化后 root 是**无密码**的，
// 而 phpMyAdmin 默认 AllowNoPassword=false 时，空密码账号根本登不进去 ——
// 用户会看到"无法登录"却完全不知道为什么。探测到空密码就放开这个开关。
func (m *Manager) mysqlRootHasNoPassword(ctx context.Context) bool {
	mysql := filepath.Join(m.brewPrefix(), "bin", "mysql")
	if _, err := os.Stat(mysql); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, mysql, "-u", "root", "--protocol=TCP",
		"-h", "127.0.0.1", "-e", "SELECT 1").Run() == nil
}

// ensureFPMInclude 确保 nginx 里有可复用的 php-fpm 片段。
//
// 面板的站点生成也会用到它；这里补一份是为了"单独装 phpMyAdmin"的场景，
// 否则 vhost 里的 include 指向一个不存在的文件，nginx -t 会直接失败。
func (m *Manager) ensureFPMInclude(ctx context.Context, result *InstallResult) error {
	dir := filepath.Join(m.brewPrefix(), "etc", "nginx", "includes")
	path := filepath.Join(dir, "php-fpm.conf")
	if existing, err := os.ReadFile(path); err == nil {
		// 已经有这个文件了。**不要无条件覆盖** —— 用户可能手调过它。
		// 只修一种情况：确认它是被早期版本写坏的那份
		// （用了对 alias 不友好的 $document_root$fastcgi_script_name，
		// 且没有 $request_filename）。这是真机上 phpMyAdmin 打不开的原因。
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
	// **改完片段必须 reload**，否则 nginx 仍在用旧内容 ——
	// 真机上就踩到了：片段已换成正确写法，但 vhost 那条路径走了"已有入口，跳过"，
	// 于是谁也没触发 reload，phpMyAdmin 依旧 404。
	// 改配置不 reload 等于没改，这一类"以为改好了"最难查。
	if err := m.reloadNginx(ctx); err != nil {
		return err
	}
	result.step(ctx, "已生成 nginx 的 php-fpm 转发片段并重载")
	return nil
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
	if _, err := m.runRoot(ctx, 20*time.Second, nginx, "-c", nginxConf, "-s", "reload"); err != nil {
		return fmt.Errorf("nginx reload 失败: %w", err)
	}
	return nil
}

// fpmIncludeContent 是 PHP-FPM 转发片段的内容。
//
// **照抄本机已验证可用的那一份，不要自己发明精简版。**
// 我第一版写了个"最小可用"版本，用
//
//	fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
//
// 它在普通 `root` 站点上没问题，但 phpMyAdmin 用的是 `alias` ——
// 嵌套的 `location ~ \.php$` 里 `$document_root` 指向的是外层 root，
// 拼出来的路径是错的，PHP-FPM 直接回
//
//	FastCGI sent in stderr: "Primary script unknown"
//
// 表现就是 phpMyAdmin 永远 404/502。正确写法是 `$request_filename`
// （它会按 alias 正确解析），这也是本机能工作的那份配置的做法。
const fpmIncludeContent = `# PHP-FPM 通用转发参数(在 location ~ \.php$ 中 include 本文件)
fastcgi_pass   127.0.0.1:9000;
fastcgi_index  index.php;
fastcgi_split_path_info ^(.+?\.php)(/.*)$;

fastcgi_param  SCRIPT_FILENAME    $request_filename;
fastcgi_param  QUERY_STRING       $query_string;
fastcgi_param  REQUEST_METHOD     $request_method;
fastcgi_param  CONTENT_TYPE       $content_type;
fastcgi_param  CONTENT_LENGTH     $content_length;
fastcgi_param  SCRIPT_NAME        $fastcgi_script_name;
fastcgi_param  REQUEST_URI        $request_uri;
fastcgi_param  DOCUMENT_URI       $document_uri;
fastcgi_param  DOCUMENT_ROOT      $document_root;
fastcgi_param  SERVER_PROTOCOL    $server_protocol;
fastcgi_param  REQUEST_SCHEME     $scheme;
fastcgi_param  HTTPS              $https if_not_empty;
fastcgi_param  GATEWAY_INTERFACE  CGI/1.1;
fastcgi_param  SERVER_SOFTWARE    nginx/$nginx_version;
fastcgi_param  REMOTE_ADDR        $remote_addr;
fastcgi_param  REMOTE_PORT        $remote_port;
fastcgi_param  SERVER_ADDR        $server_addr;
fastcgi_param  SERVER_PORT        $server_port;
fastcgi_param  SERVER_NAME        $server_name;
fastcgi_param  PATH_INFO          $fastcgi_path_info;
fastcgi_param  HTTP_AUTHORIZATION $http_authorization;
fastcgi_param  REDIRECT_STATUS    200;

fastcgi_read_timeout  300;
fastcgi_buffering     off;
`

// ensurePMAVhost 把 phpMyAdmin 的 location 接进默认站点。
//
// 放在默认站（而不是单独一个 server 块）：这样不需要域名、不需要额外端口，
// 直接 http://<地址>/phpmyadmin/ 就能用 —— 与本机已验证的既有约定一致。
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
	if strings.Contains(text, "/phpmyadmin") {
		result.step(ctx, "默认站点里已有 phpMyAdmin 入口，跳过")
		return nil
	}
	block := `
    # 由 ZizPanel 添加：phpMyAdmin 入口
    location ^~ /phpmyadmin {
        alias ` + m.pmaPaths().Share + `;
        index index.php;
        try_files $uri $uri/ /phpmyadmin/index.php$is_args$args;

        location ~ \.php$ {
            include ` + filepath.Join(m.brewPrefix(), "etc", "nginx", "includes", "php-fpm.conf") + `;
        }
    }
`
	i := strings.LastIndex(strings.TrimRight(text, "\n"), "}")
	if i < 0 {
		return fmt.Errorf("默认站点配置结构异常（找不到 server 块结束符），拒绝修改")
	}
	newText := text[:i] + block + text[i:]
	return m.writeNginxConfAndReload(ctx, conf, text, newText, result, "已把 phpMyAdmin 接入默认站点")
}

// writeNginxConfAndReload 原子写配置 → nginx -t → reload，失败则回滚。
//
// 必须"先测后载、失败回滚"：一个语法错误就能让 nginx 拒绝 reload，
// 严重时会让**所有站点**下线 —— 这是不可接受的代价，所以永远留退路。
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
		// 回滚：把配置还原回去，避免 nginx 处于"拒绝重载"的状态
		_ = os.WriteFile(conf, []byte(oldText), 0o644)
		_ = m.reloadNginx(ctx)
		return fmt.Errorf("%v（已还原 %s）", err, filepath.Base(conf))
	}
	result.step(ctx, okMsg)
	return nil
}

// waitHTTP 轮询等某个 URL 返回 200。
func waitHTTP(ctx context.Context, url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, "/usr/bin/curl",
			"-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "4", url).Output()
		if err == nil && strings.TrimSpace(string(out)) == "200" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// existingBlowfishSecret 从已有配置里读出 blowfish_secret，读不到返回空串。
//
// 用正则而不是"找第一个/最后一个引号"：那样会把
// `$cfg['blowfish_secret'] = 'abc';` 里的**键名**一起圈进去，
// 于是返回一整段垃圾，还会在每次运行时越滚越长（实测踩到过）。
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
//
// 宁可换掉一个"看起来能用但来路不明"的值，也不要把它当成可信密钥。
// 换掉只会让当前已登录的 phpMyAdmin 会话失效一次 —— 代价远小于弱密钥。
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

// runCurlCtx 执行一次 curl 并返回响应体（也用于需要看内容的健康检查）。
func runCurlCtx(ctx context.Context, url string, timeoutSec int) (string, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/curl",
		"-sS", "--max-time", strconv.Itoa(timeoutSec), url).Output()
	return string(out), err
}

// waitHTTPBody 轮询直到页面能取到内容，返回响应体。
func waitHTTPBody(ctx context.Context, url string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if out, err := runCurlCtx(ctx, url, 6); err == nil && strings.TrimSpace(out) != "" {
			last = out
			// phpMyAdmin 正常时首页会包含登录表单；错误页则不会。
			// 取到"看起来正常"的内容就立即返回，避免多等。
			if phpMyAdminErrorIn(out) == "" && strings.Contains(strings.ToLower(out), "<html") {
				return out, true
			}
		}
		select {
		case <-ctx.Done():
			return last, last != ""
		case <-time.After(500 * time.Millisecond):
		}
	}
	return last, last != ""
}

// phpMyAdminErrorIn 检查响应体里有没有 phpMyAdmin 的典型错误，返回错误摘要。
//
// 这些是实测/文档里出现过的原话，用它们判断比只看状态码可靠得多。
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

// RepairPhpMyAdminConfigPerm 修正 phpMyAdmin 配置文件的归属与权限。
//
// 为什么需要这个自愈：该文件由面板以 root 写入，而读取它的是 php-fpm —— 后者以
// 「真实用户」身份运行。归属一错，页面就渲染成
//
//	"Existing configuration file (...) is not readable."
//
// 而且 phpMyAdmin 这个错误页的 HTTP 状态码同样是 200，只看状态码会误判成正常。
//
// 归属为什么会漂移：brew 升级会重建文件、旧版本面板写出的就是 root 归属、
// 用户手工 sudo 改过配置。真机上就出现了本机报错而 mini 正常的情况 ——
// 同一份代码、两台机器不一致，正是需要启动自愈的典型场景。
//
// 返回是否做了修改。文件不存在（未装 phpMyAdmin）时返回 false, nil。
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
	// 注意不能用 os.Open 判断 —— 面板以 root 运行，任何文件它都能读，
	// 那样永远发现不了 php-fpm 读不到的问题。
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
