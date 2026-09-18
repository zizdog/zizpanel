package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  默认站点的**行为级**门禁：真的起一个 nginx，真的请求首页
//
//  为什么不能只断言"生成的文本里有 root/index 指令"：用户要的是"装完就能访问"，
//  这取决于面板写的东西 nginx **真的接受并真的服务**（本项目为此踩过多次：
//  一句写错位置的指令就能让整份配置被拒，而面板只看到退出码 0）。
//
//  这台机器上一个 PHP 都没有（不 seed php@*），所以它同时锁住用户那句
//  "不依赖 lnmp"：没有 PHP 也必须是一份能打开的静态默认站点。
//
//  沙箱化：nginx 用临时 prefix/端口，只读真实的 mime.types；
//  绝不碰生产的 80 端口与 /opt/homebrew/etc/nginx/vhosts。
// ============================================================================

func TestDefaultSiteServedByRealNginxWithoutPHP(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过默认站点真实服务校验")
	}
	srv, _ := newTestServer(t) // 不 seed PHP：这正是"不依赖 lnmp"的前提

	dir := t.TempDir()
	logDir := filepath.Join(srv.Cfg.WWWRoot, "_logs") // vhost 里的日志目录
	confDir := filepath.Join(dir, "conf")
	confD := filepath.Join(dir, "conf.d")
	runDir := filepath.Join(dir, "run")
	for _, d := range []string{logDir, confDir, confD, runDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	port := freePortForTest(t)

	// 用真实的生成器产出 vhost，只把 80 换成随机高端口（不需要 root）。
	content := srv.buildDefaultVhost()
	if !strings.Contains(content, "listen       80 default_server;") {
		t.Fatalf("默认站点 vhost 的 listen 行变了，测试需要同步：\n%s", content)
	}
	content = strings.Replace(content, "listen       80 default_server;",
		"listen       127.0.0.1:"+strconv.Itoa(port)+";", 1)
	// 80 端口上还会有 listen 443/其它指令吗？默认站点只有一处 listen，这里确认一下。
	if strings.Count(content, "listen ") != 1 {
		t.Fatalf("默认站点应当只监听一个端口，实际：\n%s", content)
	}
	vhostPath := filepath.Join(confDir, "000-default.conf")
	if err := os.WriteFile(vhostPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 面板的应用代理 location 里用了 $connection_upgrade（WebSocket 升级 map），
	// 真实机器上由「nginx 环境自愈」写进 conf.d —— 沙箱里也要照做，否则 -t 就报
	// unknown "connection_upgrade" variable（这本身就是一份真实约束）。
	if err := os.WriteFile(filepath.Join(confD, "upgrade-map.conf"),
		[]byte(sites.UpgradeMapConf()), 0o644); err != nil {
		t.Fatal(err)
	}
	// 占位页：面板的"创建默认站点"第一步就是它。
	if _, _, err := sites.EnsureLocalhostPlaceholder(srv.Cfg.WWWRoot); err != nil {
		t.Fatal(err)
	}

	main := `worker_processes 1;
error_log ` + logDir + `/error.log warn;
pid ` + filepath.Join(runDir, "nginx.pid") + `;
events { worker_connections 64; }
http {
    include /opt/homebrew/etc/nginx/mime.types;
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
		t.Fatalf("面板生成的默认站点配置没通过真实 nginx -t：\n%s", out)
	}
	if out, err := exec.Command(nginxBin, "-c", mainPath).CombinedOutput(); err != nil {
		t.Fatalf("启动沙箱 nginx 失败: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(nginxBin, "-c", mainPath, "-s", "stop").Run() })
	waitPortForTest(t, port)

	code, body := fetchLocal("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if code != http.StatusOK {
		t.Fatalf("默认站点首页应 200，实际 %d（body 前 200 字：%s）", code, firstN(body, 200))
	}
	if !strings.Contains(body, "这是本机 Web 服务的默认站点") {
		t.Errorf("默认站点首页应当是面板的占位页（这是面板复核生效用的标记），实际：%s", firstN(body, 300))
	}
	if strings.Contains(body, "<?php") {
		t.Errorf("没有 PHP 时不该把 PHP 源码当文本吐出来：%s", firstN(body, 200))
	}

	// 复核这一条：applyDefaultVhost 用的是"请求级复核"，把这里换成真实端口，
	// 整条链路（写盘 → nginx -t → reload → 请求首页 → 含标记）必须真的通过。
	prevWrite, prevReload, prevProbe := siteWriteVhostFn, siteReloadFn, defaultProbeFn
	siteWriteVhostFn = func(s *Server, _ context.Context, domain, c string) error {
		c = strings.Replace(c, "listen       80 default_server;",
			"listen       127.0.0.1:"+strconv.Itoa(port)+";", 1)
		return os.WriteFile(filepath.Join(confDir, domain+".conf"), []byte(c), 0o644)
	}
	siteReloadFn = func(*Server, context.Context) error {
		out, err := exec.Command(nginxBin, "-c", mainPath, "-s", "reload").CombinedOutput()
		if err != nil {
			return fmt.Errorf("reload 失败: %v\n%s", err, out)
		}
		return nil
	}
	defaultProbeFn = func(string) (int, string) {
		return fetchLocal("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	}
	t.Cleanup(func() {
		siteWriteVhostFn, siteReloadFn, defaultProbeFn = prevWrite, prevReload, prevProbe
	})

	srv.Cfg.VhostDir = confDir
	if err := srv.createDefaultSite(context.Background()); err != nil {
		t.Fatalf("创建默认站点（真实 nginx + 请求级复核）失败：%v", err)
	}
	if st := srv.defaultSiteStatusNow(); !st.Applied {
		t.Errorf("创建成功后状态接口应报 applied=true，实际 %+v", st)
	}
}

func freePortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func waitPortForTest(t *testing.T, port int) {
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
	t.Fatalf("沙箱 nginx 没有在 8 秒内监听 %d", port)
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
