package sites

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDomain(t *testing.T) {
	bad := []string{
		"", "../etc/passwd", "evil.com/../x", "evil.com; rm -rf /", "evil.com && id",
		"a", "-bad.com", "bad-.com", "a..b", "中文.com", "evil.com\nx", "evil.com\tx",
		strings.Repeat("a", 260) + ".com",
	}
	for _, d := range bad {
		if err := ValidateDomain(d); err == nil {
			t.Fatalf("非法域名未被拒绝: %q", d)
		}
	}
	good := []string{"example.com", "www.example.com", "demo.test", "my-site.local", "a.b.c.d"}
	for _, d := range good {
		if err := ValidateDomain(d); err != nil {
			t.Fatalf("合法域名被拒绝: %q (%v)", d, err)
		}
	}
}

// 反代地址必须严格校验：它会被直接拼进 nginx 配置，
// 如果允许任意字符，`; }` 之类可以闭合指令块并注入新指令。
func TestValidateProxyPass(t *testing.T) {
	bad := []string{
		"localhost:3000",         // 缺少协议
		"http://",                // 缺少主机
		"http://a; }\nserver {",  // 指令注入
		"http://a b",             // 空格
		"http://a/$(id)",         // 命令替换字符
		"http://a\nproxy_pass x", // 换行
		"ftp://a",                // 协议不支持
	}
	for _, p := range bad {
		if err := ValidateProxyPass(p); err == nil {
			t.Fatalf("非法反代地址未被拒绝: %q", p)
		}
	}
	good := []string{
		"", "http://127.0.0.1:8080", "https://localhost:3000",
		"http://192.168.1.10:5678", "http://127.0.0.1:8080/api",
		"http://[::1]:8080",
	}
	for _, p := range good {
		if err := ValidateProxyPass(p); err != nil {
			t.Fatalf("合法反代地址被拒绝: %q (%v)", p, err)
		}
	}
}

func TestSiteDirRejectsTraversal(t *testing.T) {
	www := "/Users/test/www"
	bad := []string{"../evil", "a/b", "..", "", "a b", "a;b"}
	for _, d := range bad {
		if _, err := SiteDir(www, d); err == nil {
			t.Fatalf("越界目录名未被拒绝: %q", d)
		}
	}
	got, err := SiteDir(www, "demo.test")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(www, "demo.test") {
		t.Fatalf("目录拼接错误: %s", got)
	}
}

func TestRewritePresetsComplete(t *testing.T) {
	// 每个模板都必须能生成非空 location 块
	for _, p := range RewritePresets {
		loc := rewriteLocation(p.Name)
		if !strings.Contains(loc, "location /") {
			t.Fatalf("模板 %s 未生成 location 块: %s", p.Name, loc)
		}
		if strings.Contains(loc, "{{") {
			t.Fatalf("模板 %s 含未替换的占位符", p.Name)
		}
	}
	// 未知模板名必须能识别出来
	if _, ok := RewritePresetByName("not-exist"); ok {
		t.Fatal("未知模板不应被找到")
	}
	// 空名回退到 none
	if p, ok := RewritePresetByName(""); !ok || p.Name != "none" {
		t.Fatal("空模板名应回退到 none")
	}
}

// TestNewSiteRewritePresets 锁住 Flarum / emlog / 可道云三条**来自官方**的伪静态规则。
//
// 规则来源（写死在注释里，改规则时必须一并更新）：
//
//	· flarum：安装包内 public/.nginx.conf 的 try_files；docroot 必须 public/
//	· emlog ：官方文档 emlog.net/docs/faq#nginx 与包内 nginx.conf 的 if + rewrite
//	· kodbox：官方 README_zh-CN.md「nginx rewrite」的 try_files
func TestNewSiteRewritePresets(t *testing.T) {
	cases := []struct {
		name      string
		publicDir string
		must      []string
	}{
		{"flarum", "public", []string{"try_files $uri $uri/ /index.php?$query_string;"}},
		{"emlog", "", []string{"index index.php index.html;", "if (!-e $request_filename)", "rewrite ^/(.*)$ /index.php last;"}},
		{"kodbox", "", []string{"try_files $uri $uri/ /index.php?$query_string;"}},
	}
	for _, c := range cases {
		p, ok := RewritePresetByName(c.name)
		if !ok {
			t.Errorf("伪静态预设 %q 不存在", c.name)
			continue
		}
		if p.PublicDir != c.publicDir {
			t.Errorf("%s 的 PublicDir = %q，期望 %q", c.name, p.PublicDir, c.publicDir)
		}
		loc := rewriteLocation(c.name)
		for _, m := range c.must {
			if !strings.Contains(loc, m) {
				t.Errorf("%s 的 location 块缺少官方规则片段 %q，实际：\n%s", c.name, m, loc)
			}
		}
	}
}

// 生成静态站点配置
func TestGenerateStaticSite(t *testing.T) {
	s := &Site{
		Domain: "static.test", Root: "/tmp/static.test",
		Rewrite: "none", Enabled: true,
	}
	conf, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	must := []string{
		"listen      80;",
		"server_name static.test;",
		"root        /tmp/static.test;",
		"try_files $uri $uri/ =404;",
		"location ~ \\.php$ { return 403; }", // 静态站点必须禁止执行 PHP
		"access_log  /tmp/zplogs/static.test.access.log;",
	}
	for _, m := range must {
		if !strings.Contains(conf, m) {
			t.Fatalf("配置缺少 %q\n---\n%s", m, conf)
		}
	}
	if strings.Contains(conf, "fastcgi_pass") {
		t.Fatal("静态站点不应包含 fastcgi_pass")
	}
}

// 生成 PHP 站点配置
func TestGeneratePHPSite(t *testing.T) {
	s := &Site{
		Domain: "php.test", Root: "/tmp/php.test",
		PHPVersion: "8.3", Rewrite: "typecho", Enabled: true,
	}
	conf, err := s.Generate(Options{LogDir: "/tmp/zplogs", FastCGIPass: "127.0.0.1:9000"})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	must := []string{
		"fastcgi_pass 127.0.0.1:9000;",
		"fastcgi_param SCRIPT_FILENAME $request_filename;",
		"fastcgi_param PATH_INFO $fastcgi_path_info;",
		"try_files $uri $uri/ /index.php$is_args$args;",
		"include", // 不应出现 include php-fpm.conf（否则无法按站点指定版本）
	}
	for _, m := range must[:4] {
		if !strings.Contains(conf, m) {
			t.Fatalf("配置缺少 %q\n---\n%s", m, conf)
		}
	}
	if strings.Contains(conf, "php-fpm.conf") {
		t.Fatal("不应 include 全局 php-fpm.conf：那会导致所有站点共用同一 PHP 版本")
	}
	// 指定了 PHP 版本却没给 FastCGI 地址，必须报错而不是生成坏配置
	if _, err := s.Generate(Options{LogDir: "/tmp"}); err == nil {
		t.Fatal("缺少 FastCGI 地址时应报错")
	}
}

// 生成 SSL 站点：HTTP 必须 301 到 HTTPS
func TestGenerateSSLSite(t *testing.T) {
	s := &Site{
		Domain: "ssl.test", Root: "/tmp/ssl.test", Rewrite: "none",
		SSLEnabled: true, SSLCert: "/tmp/ssl.test.crt", SSLKey: "/tmp/ssl.test.key",
		Enabled: true,
	}
	conf, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	must := []string{
		"listen      443 ssl;",
		"return 301 https://$http_host$request_uri;",
		"ssl_certificate     /tmp/ssl.test.crt;",
		"ssl_certificate_key /tmp/ssl.test.key;",
		"ssl_protocols       TLSv1.2 TLSv1.3;",
		"/.well-known/acme-challenge/", // 必须放行，否则证书无法续期
	}
	for _, m := range must {
		if !strings.Contains(conf, m) {
			t.Fatalf("SSL 配置缺少 %q\n---\n%s", m, conf)
		}
	}
	// 开启 SSL 却没有证书路径必须报错
	s2 := *s
	s2.SSLCert = ""
	if _, err := s2.Generate(Options{LogDir: "/tmp"}); err == nil {
		t.Fatal("开启 SSL 但缺少证书路径时应报错")
	}
}

// 反向代理：必须包含 WebSocket 升级头
func TestGenerateProxySite(t *testing.T) {
	s := &Site{
		Domain: "proxy.test", Root: "/tmp/proxy.test",
		ProxyPass: "http://127.0.0.1:5678", Rewrite: "none", Enabled: true,
	}
	conf, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	must := []string{
		"proxy_pass http://127.0.0.1:5678;",
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_set_header Connection $connection_upgrade;",
		"proxy_set_header Host $host;",
		"proxy_set_header X-Forwarded-Proto $scheme;",
	}
	for _, m := range must {
		if !strings.Contains(conf, m) {
			t.Fatalf("反代配置缺少 %q\n---\n%s", m, conf)
		}
	}
	// 反代模式下不应再有 try_files 与 PHP 转发
	if strings.Contains(conf, "fastcgi_pass") {
		t.Fatal("反代模式下不应包含 fastcgi_pass")
	}
}

func TestGenerateRejectsBadInput(t *testing.T) {
	cases := []*Site{
		{Domain: "bad domain", Root: "/tmp/x"},
		{Domain: "ok.test", Root: "relative/path"},
		{Domain: "ok.test", Root: "/tmp/x", Rewrite: "no-such-preset"},
		{Domain: "ok.test", Root: "/tmp/x", ProxyPass: "http://a; }"},
	}
	for i, s := range cases {
		if _, err := s.Generate(Options{LogDir: "/tmp"}); err == nil {
			t.Fatalf("第 %d 个非法输入未被拒绝: %+v", i+1, s)
		}
	}
}

// ---------------------------------------------------------------------------
//  用真实 nginx 校验生成的配置语法
//
//  这是本包最重要的测试：配置生成错了，nginx -t 就是最后一道防线。
//  但 nginx 不能随便拿生产配置去试，所以这里在临时目录里搭一个
//  完整的最小 nginx 环境（自己的 nginx.conf、日志、pid、临时端口），
//  用真实的 nginx 二进制校验生成的 vhost。
// ---------------------------------------------------------------------------

func TestGeneratedConfigPassesNginxTest(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过真实语法校验")
	}

	dir := t.TempDir()
	// 沙箱 nginx 目录结构（写实路径，避免 nginx 报 error_log 打不开）
	logDir := filepath.Join(dir, "logs")
	confDir := filepath.Join(dir, "conf")
	for _, d := range []string{logDir, confDir, filepath.Join(dir, "html"), filepath.Join(dir, "run")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// nginx 用 8080 之外的高端口，避免与生产 nginx 抢 80
	listenPort := "18080"

	// 生成多个不同形态的站点配置
	spaceDir := filepath.Join(dir, "html space") // 外接盘名常带空格：root 必须加引号
	if err := os.MkdirAll(spaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []*Site{
		{Domain: "static.test", Root: filepath.Join(dir, "html"), Rewrite: "none", Enabled: true},
		{Domain: "php.test", Root: filepath.Join(dir, "html"), PHPVersion: "8.3", Rewrite: "typecho", Enabled: true},
		{Domain: "wp.test", Root: filepath.Join(dir, "html"), PHPVersion: "8.3", Rewrite: "wordpress", Enabled: true},
		{Domain: "laravel.test", Root: filepath.Join(dir, "html"), PHPVersion: "8.3", Rewrite: "laravel", Enabled: true},
		{Domain: "tp.test", Root: filepath.Join(dir, "html"), PHPVersion: "8.3", Rewrite: "thinkphp", Enabled: true},
		{Domain: "proxy.test", Root: filepath.Join(dir, "html"), ProxyPass: "http://127.0.0.1:19999", Rewrite: "none", Enabled: true},
		{Domain: "extra.test", Root: filepath.Join(dir, "html"), Rewrite: "none",
			ExtraConf: "location = /health { return 200 'ok'; }", Enabled: true},
		// 目录索引 + PHP：autoindex 在 server 级，不能破坏 PHP 转发
		{Domain: "diridx.test", Root: filepath.Join(dir, "html"), PHPVersion: "8.3",
			Rewrite: "generic", AutoIndex: true, Enabled: true},
		// 带空格的根目录（外接盘）：引号写法必须过 nginx -t
		{Domain: "space.test", Root: spaceDir, Rewrite: "none", AutoIndex: true, Enabled: true},
		// 自定义监听端口（镜像站独占端口）：nginx -t 不绑端口，这里只校验语法
		{Domain: "port8090.test", Root: filepath.Join(dir, "html"), Rewrite: "none",
			ListenPort: 8090, AutoIndex: true, Enabled: true},
	}

	var vhosts strings.Builder
	for _, s := range cases {
		conf, err := s.Generate(Options{LogDir: logDir, FastCGIPass: "127.0.0.1:19000"})
		if err != nil {
			t.Fatalf("生成 %s 失败: %v", s.Domain, err)
		}
		// 沙箱里把 80 改成高端口，避免需要 root 且不干扰生产 nginx
		conf = strings.Replace(conf, "listen      80;", "listen      "+listenPort+";", 1)
		vhosts.WriteString(conf)
		vhosts.WriteString("\n")
	}

	// SSL 站点单独校验（需要真实证书文件）
	cert, key := makeSelfSigned(t, dir)
	sslSite := &Site{
		Domain: "ssl.test", Root: filepath.Join(dir, "html"), Rewrite: "none",
		SSLEnabled: true, SSLCert: cert, SSLKey: key, Enabled: true,
	}
	sslConf, err := sslSite.Generate(Options{LogDir: logDir})
	if err != nil {
		t.Fatal(err)
	}
	// SSL 站点里 HTTP 块也要换端口，443 同样需要 root
	sslConf = strings.Replace(sslConf, "listen      80;", "listen      18081;", 1)
	sslConf = strings.Replace(sslConf, "listen      443 ssl;", "listen      18443 ssl;", 1)
	vhosts.WriteString(sslConf)

	vhostPath := filepath.Join(confDir, "test-vhosts.conf")
	if err := os.WriteFile(vhostPath, []byte(vhosts.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// 写一个 conf.d 环境，用于存放面板生成的、必须在 http 上下文生效的片段
	confD := filepath.Join(dir, "conf.d")
	if err := os.MkdirAll(confD, 0o755); err != nil {
		t.Fatal(err)
	}

	// 关键：这里不手写 map，而是让被测代码生成它。
	// 早期版本的测试在自己搭的 nginx.conf 里手写了 map，
	// 于是掩盖了"生产 nginx.conf 里没有这个 map"的真实故障 ——
	// 用户一建反代站点就会 nginx -t 失败。测试必须复现生产的真实条件。
	mapPath := filepath.Join(confD, "upgrade-map.conf")
	if err := os.WriteFile(mapPath, []byte(UpgradeMapConf()), 0o644); err != nil {
		t.Fatal(err)
	}

	// 主配置：结构与生产 nginx.conf 保持一致（同样是 http 块内 include conf.d）
	main := `worker_processes 1;
error_log ` + logDir + `/error.log warn;
pid ` + filepath.Join(dir, "run", "nginx.pid") + `;
events { worker_connections 64; }
http {
    include ` + filepath.Join(getNginxPrefix(t), "etc/nginx/mime.types") + `;
    default_type application/octet-stream;
    access_log ` + logDir + `/access.log;
    client_body_temp_path ` + filepath.Join(dir, "run", "body") + `;
    proxy_temp_path ` + filepath.Join(dir, "run", "proxy") + `;
    fastcgi_temp_path ` + filepath.Join(dir, "run", "fastcgi") + `;
    uwsgi_temp_path ` + filepath.Join(dir, "run", "uwsgi") + `;
    scgi_temp_path ` + filepath.Join(dir, "run", "scgi") + `;
    include ` + confD + `/*.conf;
    include ` + vhostPath + `;
}
`
	mainPath := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1) 语法校验
	out, err := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput()
	if err != nil {
		t.Fatalf("nginx -t 未通过:\n%s\n---生成内容---\n%s", out, vhosts.String())
	}
	if !strings.Contains(string(out), "successful") {
		t.Fatalf("nginx -t 输出异常: %s", out)
	}
	t.Logf("nginx -t 通过：%d 个站点配置", len(cases)+1)

	// 反向验证：如果 upgrade map 缺失，反代配置必须让 nginx -t 失败。
	// 这条断言保证"面板必须负责提供 map"这个约束不会被遗忘。
	if err := os.Remove(mapPath); err != nil {
		t.Fatal(err)
	}
	out2, err2 := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput()
	if err2 == nil {
		t.Fatalf("缺少 upgrade map 时 nginx -t 竟然通过了，说明反代配置没有真正依赖它：\n%s", out2)
	}
	if !strings.Contains(string(out2), "connection_upgrade") {
		t.Fatalf("缺少 map 时的报错信息不符合预期：\n%s", out2)
	}
	t.Log("反向验证通过：缺少 map 时 nginx -t 会正确报错，因此面板必须负责写入 map")
}

// getNginxPrefix 推断 nginx 安装前缀，用于 include mime.types。
func getNginxPrefix(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/opt/homebrew", "/usr/local"} {
		if _, err := os.Stat(filepath.Join(p, "bin", "nginx")); err == nil {
			return p
		}
	}
	t.Skip("找不到 nginx 前缀")
	return ""
}

// makeSelfSigned 生成测试用自签证书（避免测试依赖外部证书文件）。
func makeSelfSigned(t *testing.T, dir string) (string, string) {
	t.Helper()
	cert := filepath.Join(dir, "test.crt")
	key := filepath.Join(dir, "test.key")
	cmd := exec.Command("/usr/bin/openssl", "req", "-x509", "-newkey", "rsa:2048",
		"-keyout", key, "-out", cert, "-days", "2", "-nodes",
		"-subj", "/CN=ssl.test", "-addext", "subjectAltName=DNS:ssl.test,DNS:localhost,IP:127.0.0.1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("生成测试证书失败（跳过 SSL 校验）: %v\n%s", err, out)
	}
	return cert, key
}

// TestPHPSiteTrustsLoopbackForwardedProto 锁住"Lucky 式"反代下的 HTTPS 判定。
//
// 架构：证书只在面板的反代规则上，站点只跑明文 HTTP。此时 PHP 看到的 $scheme/$https
// 是 http，WordPress/Typecho 会生成 http:// 链接。所以站点 vhost 必须把**来自回环**的
// X-Forwarded-Proto 折算成 HTTPS/REQUEST_SCHEME（外部客户端伪造不了：$remote_addr 不是回环）。
func TestPHPSiteTrustsLoopbackForwardedProto(t *testing.T) {
	s := &Site{Domain: "trust.test", Root: "/tmp/trust.test", PHPVersion: "8.3", Rewrite: "none", Enabled: true}
	conf, err := s.Generate(Options{LogDir: "/tmp/zplogs", FastCGIPass: "127.0.0.1:9000"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set $zp_scheme $scheme;",
		`if ($remote_addr = 127.0.0.1) { set $zp_scheme $http_x_forwarded_proto; }`,
		`if ($zp_scheme != "https") { set $zp_scheme "http"; }`,
		`set $zp_https "";`,
		`if ($zp_scheme = "https") { set $zp_https "on"; }`,
		"fastcgi_param REQUEST_SCHEME $zp_scheme;",
		"fastcgi_param HTTPS $zp_https if_not_empty;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("vhost 里缺少 %q（反代终止 TLS 时 PHP 会误判为 http）：\n%s", want, conf)
		}
	}
	// 不能再出现裸 $scheme / $https —— 那等于没有折算。
	if strings.Contains(conf, "fastcgi_param REQUEST_SCHEME $scheme;") {
		t.Error("REQUEST_SCHEME 仍用 $scheme（没有折算反代传来的协议）")
	}
	if strings.Contains(conf, "fastcgi_param HTTPS $https") {
		t.Error("HTTPS 仍用 $https（没有折算反代传来的协议）")
	}
}
