package sites

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
)

// ============================================================================
//  上传与执行限制（用户报障：phpMyAdmin 导入 413）
//
//  nginx 出厂 client_max_body_size 只有 1m、PHP 出厂 upload 2M / post 8M。
//  这组测试锁住三件事：
//   1. 默认值（512m / 512M）就写在面板生成物里 —— 不需要用户先撞 413 才知道要改；
//   2. 非法输入不会变成非法 nginx/PHP 配置（配置注入面）；
//   3. **真的**用一个临时 nginx 验证：>1MB 的请求体不再返回 413
//      （这是用户症状的判据，比"配置里有这行字"强得多）。
// ============================================================================

func TestLimitsDefaultsAndValidation(t *testing.T) {
	d := DefaultLimits()
	if d.ClientMaxBodySize != "512m" || d.UploadMaxFilesize != "512M" ||
		d.PostMaxSize != "512M" || d.MemoryLimit != "512M" || d.MaxExecutionTime != 300 {
		t.Fatalf("默认值不对: %+v", d)
	}
	// 空值补齐默认
	if got := (Limits{}).Normalize(); got != d {
		t.Fatalf("空 Limits 应补齐默认，实际 %+v", got)
	}
	// 合法输入
	for _, ok := range []string{"512m", "1g", "1024k", "1073741824", "512M", "1G"} {
		if err := ValidateSizeValue("x", ok); err != nil {
			t.Errorf("%q 应合法，却被拒: %v", ok, err)
		}
	}
	// 非法输入：空格、小数点、负数、单位错、纯单位、超长数字
	for _, bad := range []string{"", " ", "512 m", "512.5m", "-1m", "512x", "m", "99999999999g", "512mb", "1;g"} {
		if err := ValidateSizeValue("x", bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	// 交叉校验：post < upload、nginx < upload 都必须被挡（否则用户会再次"上传失败"）
	if err := (Limits{ClientMaxBodySize: "512m", UploadMaxFilesize: "512M",
		PostMaxSize: "8M", MemoryLimit: "512M", MaxExecutionTime: 300}).Validate(); err == nil {
		t.Error("post_max_size < upload_max_filesize 应被拒绝")
	}
	if err := (Limits{ClientMaxBodySize: "1m", UploadMaxFilesize: "512M",
		PostMaxSize: "512M", MemoryLimit: "512M", MaxExecutionTime: 300}).Validate(); err == nil {
		t.Error("nginx 上限 < upload_max_filesize 应被拒绝")
	}
	// 执行时间边界
	if err := (Limits{ClientMaxBodySize: "512m", UploadMaxFilesize: "512M",
		PostMaxSize: "512M", MemoryLimit: "512M", MaxExecutionTime: 0}).Validate(); err == nil {
		t.Error("max_execution_time=0 应被拒绝（0 会让请求永不超时释放）")
	}
	// 全部合法
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatalf("默认值必须通过校验: %v", err)
	}
}

// TestFindClientMaxBodySize：回读必须能读出 nginx 的真实写法
// （`client_max_body_size 512m;` 没有 `=`，而 php-fpm 的 listen 是 `listen = x`）。
func TestFindClientMaxBodySize(t *testing.T) {
	conf := "# client_max_body_size 1m;\nserver {\n\tclient_max_body_size 512m;\n}\n"
	v, line := FindClientMaxBodySize(conf)
	if v != "512m" || line != 3 {
		t.Fatalf("应读出 512m（第 3 行），实际 %q 第 %d 行", v, line)
	}
	// 注释行必须跳过
	if v, _ := FindClientMaxBodySize("\t# client_max_body_size 1m;\n"); v != "" {
		t.Fatalf("注释行不该被当成配置，却读出 %q", v)
	}
	// 容忍带 = 的写法
	if v, _ := FindClientMaxBodySize("client_max_body_size = 2g;"); v != "2g" {
		t.Fatalf("带 = 的写法应读出 2g，实际 %q", v)
	}
	if v, _ := FindClientMaxBodySize("server { }\n"); v != "" {
		t.Fatalf("没有该指令时应返回空，实际 %q", v)
	}
}

func TestPHPIniFragmentContentAndEnsure(t *testing.T) {
	dir := t.TempDir()
	// 没装这个版本（目录不存在）时必须报错，而不是凭空造一棵"看起来装了 PHP"的树。
	if _, err := EnsurePHPLimits(dir, "8.2", DefaultLimits(), true); err == nil {
		t.Fatal("PHP 目录不存在时应报错（不许创建假版本目录）")
	}
	if _, err := os.Stat(filepath.Join(dir, "etc", "php", "8.2")); err == nil {
		t.Fatal("失败的 EnsurePHPLimits 不应创建任何目录")
	}
	// 装好目录后再写
	if err := os.MkdirAll(filepath.Join(dir, "etc", "php", "8.2", "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	lim := Limits{ClientMaxBodySize: "1g", UploadMaxFilesize: "768M",
		PostMaxSize: "768M", MemoryLimit: "256M", MaxExecutionTime: 600}
	res, err := EnsurePHPLimits(dir, "8.2", lim, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("首次写入应报告 changed=true")
	}
	if filepath.Base(res.Path) != PHPLimitsFragmentName {
		t.Fatalf("片段文件名应为 %s，实际 %s", PHPLimitsFragmentName, res.Path)
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{
		"upload_max_filesize = 768M", "post_max_size = 768M",
		"memory_limit = 256M", "max_execution_time = 600",
		"ZizPanel",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("片段缺少 %q：\n%s", want, text)
		}
	}
	// 幂等：同值再写一次，文件内容不能变、changed 必须是 false
	before := string(b)
	res2, err := EnsurePHPLimits(dir, "8.2", lim, true)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Error("同值重写应幂等（changed=false）")
	}
	after, _ := os.ReadFile(res.Path)
	if string(after) != before {
		t.Error("幂等性被破坏：文件内容变了")
	}
	// 改值 → 必须重写
	lim.MemoryLimit = "1G"
	res3, err := EnsurePHPLimits(dir, "8.2", lim, true)
	if err != nil || !res3.Changed {
		t.Fatalf("改值后应重写（changed=true），实际 %+v err=%v", res3, err)
	}
	// 非法版本号不许拼出越界路径
	if PHPConfDPath(dir, "../../etc") != "" || PHPConfDPath(dir, "8.2/x") != "" {
		t.Error("非法版本号必须被拒绝")
	}
}

// TestConfigDefaultsMatchSitesDefaults：config 的默认值与 sites 的默认值必须一致。
//
// 为什么不合并成一份：config 是低层包，不想反向依赖 sites；但两处不一致会让
// "界面显示的默认值"与实际写进 vhost/PHP 的值不同（用户看到 512m、生效 1m）。
func TestConfigDefaultsMatchSitesDefaults(t *testing.T) {
	c := config.Default()
	d := DefaultLimits()
	if c.NginxClientMaxBodySize != d.ClientMaxBodySize ||
		c.PHPUploadMaxFilesize != d.UploadMaxFilesize ||
		c.PHPPostMaxSize != d.PostMaxSize ||
		c.PHPMemoryLimit != d.MemoryLimit ||
		c.PHPMaxExecutionTime != d.MaxExecutionTime {
		t.Fatalf("config 默认值 %+v 与 sites 默认值 %+v 不一致", c, d)
	}
}

// TestGenerateIncludesClientMaxBodySize：vhost 文本必须带请求体上限
// （默认 512m 与自定义值各一条）。
func TestGenerateIncludesClientMaxBodySize(t *testing.T) {
	s := &Site{Domain: "big.test", Root: "/tmp/big.test", Rewrite: "none", Enabled: true}
	// 默认（调用方不传）
	conf, err := s.Generate(Options{LogDir: "/tmp/zplogs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "client_max_body_size 512m;") {
		t.Fatalf("默认生成的 vhost 必须带 client_max_body_size 512m：\n%s", conf)
	}
	// 自定义
	conf, err = s.Generate(Options{LogDir: "/tmp/zplogs", ClientMaxBodySize: "2g"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "client_max_body_size 2g;") {
		t.Fatalf("自定义值必须写进 vhost：\n%s", conf)
	}
	if strings.Contains(conf, "512m") {
		t.Errorf("自定义值生效时不该还留着默认值：\n%s", conf)
	}
	// 反代站点同样带（server 级对 location 生效；location 里不再写死值覆盖它）
	proxy := &Site{Domain: "p.test", Root: "/tmp/p.test", Rewrite: "none",
		ProxyPass: "http://127.0.0.1:5678", Enabled: true}
	pconf, err := proxy.Generate(Options{LogDir: "/tmp/zplogs", ClientMaxBodySize: "3g"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pconf, "client_max_body_size 3g;") {
		t.Fatalf("反代站点也必须带请求体上限：\n%s", pconf)
	}
	if n := strings.Count(pconf, "client_max_body_size"); n != 1 {
		t.Errorf("反代站点只应有一处 client_max_body_size（location 里写死会覆盖用户设置），实际 %d 处：\n%s", n, pconf)
	}
	// 非法值必须被拒绝（配置注入面）
	if _, err := s.Generate(Options{LogDir: "/tmp", ClientMaxBodySize: "512m; } server { "}); err == nil {
		t.Fatal("非法 client_max_body_size 必须被拒绝")
	}
}

// TestGeneratedVhostAcceptsLargeBodyRealNginx 是本包针对 413 的**行为级**门禁。
//
// 只断言"配置里有这行字"不够：指令位置错了（例如只写在 server 之外）、
// 或值没生效，用户看到的仍然是 413。这里搭一个临时 nginx（随机高端口、
// 沙箱目录、跑完即停），用真实 HTTP 发 2MB 请求体：
//   - client_max_body_size 512m（面板默认）→ **不许 413**；
//   - client_max_body_size 1m（nginx 出厂默认，对照组）→ 必须 413。
//
// 代理目标是一个真的 Go HTTP 服务（会读完请求体），所以 413 判定不受
// "上游不存在"干扰 —— 对照组与实验组走的是同一条代码路径。
func TestGeneratedVhostAcceptsLargeBodyRealNginx(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过真实 413 行为校验")
	}

	// 上游：读完请求体再回 200（这样 nginx 必须把 body 转过去，才会真正校验上限）。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Received-Bytes", fmt.Sprintf("%d", n))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	confDir := filepath.Join(dir, "conf")
	confD := filepath.Join(dir, "conf.d")
	htmlDir := filepath.Join(dir, "html")
	runDir := filepath.Join(dir, "run")
	for _, d := range []string{logDir, confDir, confD, htmlDir, runDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	portBig := freePort(t)
	portSmall := freePort(t)

	mkVhost := func(domain string, port int, limit string) string {
		s := &Site{Domain: domain, Root: htmlDir, Rewrite: "none",
			ProxyPass: upstream.URL, Enabled: true}
		conf, err := s.Generate(Options{LogDir: logDir, ClientMaxBodySize: limit})
		if err != nil {
			t.Fatalf("生成 %s 失败: %v", domain, err)
		}
		// 把 80 换成随机高端口（不需要 root，也不碰生产 nginx）
		return strings.Replace(conf, "listen      80;", "listen      127.0.0.1:"+strconv.Itoa(port)+";", 1)
	}
	vhosts := mkVhost("big.test", portBig, "512m") + "\n" + mkVhost("small.test", portSmall, "1m")
	vhostPath := filepath.Join(confDir, "test-vhosts.conf")
	if err := os.WriteFile(vhostPath, []byte(vhosts), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confD, "upgrade-map.conf"),
		[]byte(UpgradeMapConf()), 0o644); err != nil {
		t.Fatal(err)
	}
	main := `worker_processes 1;
error_log ` + logDir + `/error.log warn;
pid ` + filepath.Join(runDir, "nginx.pid") + `;
events { worker_connections 64; }
http {
    include ` + filepath.Join(getNginxPrefix(t), "etc/nginx/mime.types") + `;
    default_type application/octet-stream;
    access_log ` + logDir + `/access.log;
    client_body_temp_path ` + filepath.Join(runDir, "body") + `;
    proxy_temp_path ` + filepath.Join(runDir, "proxy") + `;
    fastcgi_temp_path ` + filepath.Join(runDir, "fastcgi") + `;
    uwsgi_temp_path ` + filepath.Join(runDir, "uwsgi") + `;
    scgi_temp_path ` + filepath.Join(runDir, "scgi") + `;
    include ` + confD + `/*.conf;
    include ` + vhostPath + `;
}
`
	mainPath := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput(); err != nil {
		t.Fatalf("沙箱 nginx -t 未通过:\n%s", out)
	}
	// 启动（daemon on 时命令立刻返回），跑完无论如何都要停掉，别留进程。
	if out, err := exec.Command(nginxBin, "-c", mainPath).CombinedOutput(); err != nil {
		t.Fatalf("启动沙箱 nginx 失败: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(nginxBin, "-c", mainPath, "-s", "stop").Run()
	})

	waitPort(t, portBig)
	waitPort(t, portSmall)

	body := bytes.Repeat([]byte("x"), 2*1024*1024) // 2MB > 1m，< 512m
	post := func(port int) int {
		req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/upload", port),
			bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	// 实验组：面板默认 512m —— 用户症状（413）必须消失
	if code := post(portBig); code == http.StatusRequestEntityTooLarge {
		t.Fatalf("2MB 请求体仍然返回 413：client_max_body_size=512m 没有生效")
	} else if code != http.StatusOK {
		t.Fatalf("期望 200（上游读完 body），实际 %d", code)
	}
	// 对照组：nginx 出厂默认 1m —— 必须复现 413（证明这个测试真的在测 413）
	if code := post(portSmall); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("对照组（1m）应返回 413，实际 %d：说明本测试没有真正覆盖 413 行为", code)
	}
	t.Logf("真实 nginx 行为校验通过：512m → 非 413；1m → 413")
}

// freePort 让内核分配一个空闲端口（立即关闭，nginx 启动前有极小竞态，可接受）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// waitPort 等端口真的有人监听（比"nginx 命令返回 0"可靠）。
func waitPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("端口 %d 上一直没有 nginx 在监听", port)
}
