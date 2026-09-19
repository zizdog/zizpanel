package sites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 站点根目录白名单矩阵（用户 2026-09-19：通用站点能力，支持外接卷/家目录）。
//
// 拒绝必须给出**具体原因**：用户看到"不合法"不知道该改哪里。

func TestValidateRootPathDeniedWithReason(t *testing.T) {
	pol := RootPolicy{WWWRoot: "/Users/alice/www", UserHome: "/Users/alice", BrewPrefix: "/opt/homebrew"}
	cases := []struct {
		path string
		want string
	}{
		{"/", "整个系统盘"},
		{"/System", "macOS 系统目录"},
		{"/System/Volumes/Data/x", "macOS 系统目录"},
		{"/Library", "macOS 系统资源库"},
		{"/Library/WebServer/Documents", "macOS 系统资源库"},
		{"/usr", "系统程序目录"},
		{"/usr/local/var/www", "系统程序目录"},
		{"/bin", "系统命令目录"},
		{"/sbin", "系统命令目录"},
		{"/etc", "系统配置目录"},
		{"/etc/nginx", "系统配置目录"},
		{"/private/etc", "系统配置目录"},
		{"/dev", "设备文件目录"},
		{"/dev/fd", "设备文件目录"},
		{"/var/db", "系统数据库目录"},
		{"/var/db/foo", "系统数据库目录"},
		{"/private/var/db", "系统数据库目录"},
		{"/opt/homebrew", "Homebrew"},
		{"/opt/homebrew/var/www", "Homebrew"},
	}
	reasons := map[string]bool{}
	for _, c := range cases {
		_, err := ValidateRootPath(c.path, pol)
		if err == nil {
			t.Errorf("%s 必须被拒绝", c.path)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 的拒绝原因必须具体（应含 %q），实际: %v", c.path, c.want, err)
		}
		if !strings.Contains(err.Error(), c.path) {
			t.Errorf("%s 的错误信息里应回显这个路径，实际: %v", c.path, err)
		}
		reasons[c.want] = true
	}
	if len(reasons) < 6 {
		t.Errorf("拒绝原因不够具体（只有 %d 类）——用户需要知道具体是哪一类系统目录", len(reasons))
	}
}

func TestValidateRootPathAllowsVolumesHomeAndOpt(t *testing.T) {
	pol := RootPolicy{WWWRoot: "/Users/alice/www", UserHome: "/Users/alice", BrewPrefix: "/opt/homebrew"}
	allowed := []string{
		"/Volumes/ZPMirror/site",
		"/Volumes",             // 外接卷挂载点本身（用户自己负责）
		"/Volumes/My Passport", // 盘名带空格：要能通过（生成 vhost 时会加引号）
		"/Users/alice/mirror",  // 家目录下的任意目录
		"/Users/alice/www/demo.test",
		"/opt/zpmirror", // /opt 下的自定义目录
		"/opt/zpmirror/download",
		"/tmp/zp-site", // /tmp 是 /private/tmp 的软链接，属于普通目录
		"/Users/alice",
	}
	for _, p := range allowed {
		got, err := ValidateRootPath(p, pol)
		if err != nil {
			t.Errorf("%q 应放行，却被拒: %v", p, err)
			continue
		}
		if got != filepath.Clean(p) {
			t.Errorf("%q 应返回清理后的绝对路径 %q，实际 %q", p, filepath.Clean(p), got)
		}
	}
}

func TestValidateRootPathRejectsNonAbsoluteTraversalAndInjection(t *testing.T) {
	bad := []string{
		"", "   ", "relative/x", "C:/sites",
		"/tmp/new\nsite", "/tmp/site\tx",
		"/tmp/site;}", "/tmp/site{x}", "/tmp/site$var", "/tmp/site'q'", `/tmp/site\back`,
		"/Volumes/x/../../etc", // Clean 后落到 /etc
	}
	for _, p := range bad {
		if _, err := ValidateRootPath(p, RootPolicy{}); err == nil {
			t.Errorf("%q 必须被拒绝（绝对路径/无路径穿越/无 nginx 注入字符）", p)
		}
	}
}

// 软链接逃逸：/Volumes/xxx -> /etc 这类绕行必须在校验期就被挡住，
// 否则"黑名单"形同虚设。
func TestValidateRootPathResolvesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	escape := filepath.Join(dir, "escape")
	if err := os.Symlink("/etc", escape); err != nil {
		t.Skipf("无法创建软链接（跳过）: %v", err)
	}
	if _, err := ValidateRootPath(filepath.Join(escape, "nginx"), RootPolicy{}); err == nil {
		t.Fatal("软链接指向 /etc 时必须拒绝")
	}
	// 指向普通目录的软链接必须放行（/Volumes 在真机上就是软链接或挂载点）
	real := filepath.Join(dir, "real-site")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(dir, "ok-link")
	if err := os.Symlink(real, ok); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRootPath(ok, RootPolicy{}); err != nil {
		t.Fatalf("指向普通目录的软链接应放行: %v", err)
	}
}

func TestRunRootAppendsPublicOnce(t *testing.T) {
	cases := []struct{ base, rewrite, want string }{
		{"/Volumes/X/app", "laravel", "/Volumes/X/app/public"},
		{"/Volumes/X/app/public", "laravel", "/Volumes/X/app/public"},
		{"/Volumes/X/app", "thinkphp", "/Volumes/X/app/public"},
		{"/Volumes/X/app", "none", "/Volumes/X/app"},
		{"/Volumes/X/app", "wordpress", "/Volumes/X/app"},
	}
	for _, c := range cases {
		if got := RunRoot(c.base, c.rewrite); got != c.want {
			t.Errorf("RunRoot(%q,%q) = %q，期望 %q", c.base, c.rewrite, got, c.want)
		}
	}
}

func TestFindRootDirective(t *testing.T) {
	conf := "# root /tmp/not-this;\nserver {\n\troot        \"/Volumes/My Passport/site\";\n}\n"
	if got := FindRootDirective(conf); got != "/Volumes/My Passport/site" {
		t.Fatalf("应读出带引号的 root，实际 %q", got)
	}
	if got := FindRootDirective("server { root /tmp/x; }\n"); got != "/tmp/x" {
		t.Fatalf("单行写法应读出 /tmp/x，实际 %q", got)
	}
	if got := FindRootDirective("server { root_path /tmp/x; }\n"); got != "" {
		t.Fatalf("root_path 不能被当成 root，实际 %q", got)
	}
	if got := FindRootDirective("# 只有注释里的 root /tmp/x;\n"); got != "" {
		t.Fatalf("注释里的 root 不该被读出，实际 %q", got)
	}
}

// 老站点（AutoIndex=false）生成的 vhost 必须逐字与加这个字段之前一致：
// 关 = 不写 autoindex 行（nginx 默认 off），升级后不重写用户配置。
func TestGenerateAutoIndexOnOff(t *testing.T) {
	s := &Site{Domain: "mirror.test", Root: "/tmp/mirror.test", Rewrite: "none", Enabled: true}
	off, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "autoindex") {
		t.Fatalf("默认关闭时 vhost 不该出现 autoindex：\n%s", off)
	}
	s.AutoIndex = true
	on, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on, "autoindex   on;") {
		t.Fatalf("开启时必须写 autoindex on：\n%s", on)
	}
	// PHP 站点开目录索引不能破坏 PHP 转发（autoindex 在 server 级，PHP 正则 location 独立）
	php := &Site{Domain: "php-mirror.test", Root: "/tmp/php-mirror.test", Rewrite: "generic",
		PHPVersion: "8.3", AutoIndex: true, Enabled: true}
	pc, err := php.Generate(Options{LogDir: "/tmp/zplogs", FastCGIPass: "127.0.0.1:9000"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"autoindex   on;", "fastcgi_pass 127.0.0.1:9000;", "try_files $uri $uri/ /index.php$is_args$args;"} {
		if !strings.Contains(pc, want) {
			t.Errorf("PHP 站点开目录索引后缺少 %q：\n%s", want, pc)
		}
	}
	// 反代站点也允许开（对 proxy_pass 无副作用，但配置要合法）
	proxy := &Site{Domain: "p.test", Root: "/tmp/p.test", Rewrite: "none",
		ProxyPass: "http://127.0.0.1:5678", AutoIndex: true, Enabled: true}
	pconf, err := proxy.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pconf, "autoindex   on;") || !strings.Contains(pconf, "proxy_pass http://127.0.0.1:5678;") {
		t.Fatalf("反代站点开目录索引的配置不对：\n%s", pconf)
	}
}

// 外接盘名带空格：root 必须加引号，否则 nginx 按空白切词直接 -t 失败。
func TestGenerateQuotesRootWithSpace(t *testing.T) {
	s := &Site{Domain: "space.test", Root: "/Volumes/My Passport/site", Rewrite: "none", Enabled: true}
	conf, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, `root        "/Volumes/My Passport/site";`) {
		t.Fatalf("带空格的根目录必须加引号：\n%s", conf)
	}
	if FindRootDirective(conf) != "/Volumes/My Passport/site" {
		t.Fatalf("回读带引号的 root 失败：%q", FindRootDirective(conf))
	}
}

func TestDirOwnerUIDReadsBackOwnership(t *testing.T) {
	dir := t.TempDir()
	uid, gid, err := DirOwnerUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Fatalf("回读归属 = %d:%d，期望 %d:%d", uid, gid, os.Getuid(), os.Getgid())
	}
	if _, _, err := DirOwnerUID(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("不存在的目录必须报错")
	}
}

func TestDirReadableByChecksPermissionBits(t *testing.T) {
	dir := t.TempDir()
	if err := DirReadableBy(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("普通目录应可读: %v", err)
	}
	sub := filepath.Join(dir, "no-read")
	if err := os.Mkdir(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if err := DirReadableBy(sub, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("权限 000 的目录必须判为不可读（否则建了站却 403，等于谎报成功）")
	}
	if err := DirReadableBy(filepath.Join(dir, "missing"), os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("不存在的目录必须报错")
	}
}
