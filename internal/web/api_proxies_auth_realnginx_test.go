package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/proxies"
)

// ============================================================================
//  反代「需要用户名密码」的**行为级**门禁：真的起一个沙箱 nginx
//
//  用户要求：开启后访问该反代地址必须 HTTP Basic Auth，
//  未通过 → 401 + WWW-Authenticate: Basic realm=...。
//
//  只断言"生成的文本里有 auth_basic"是不够的：哈希格式、文件路径、权限、
//  $connection_upgrade 变量……任何一处不对，nginx 要么起不来、要么一律 401、
//  要么根本不校验。所以这里用真实 nginx 起在随机高端口，真的发三种请求。
//
//  沙箱化：nginx 用临时 prefix/端口，绝不碰生产的 80/443、也绝不碰
//  /opt/homebrew/etc/nginx/vhosts（与 api_default_site_realnginx_test.go 同规矩）。
// ============================================================================

func TestProxyBasicAuthServedByRealNginx(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过反代鉴权真实服务校验")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	for _, d := range []string{confDir, runDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	port := freePortForTest(t)

	// 用**生产同一套**哈希与生成器，只把监听端口换成随机高端口（不需要 root）。
	hash, err := hashProxyBasicAuthPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	htpasswd := filepath.Join(confDir, "users.htpasswd")
	if err := os.WriteFile(htpasswd, []byte("tester:"+hash+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rule := &proxies.Rule{
		ID: 1, Name: "鉴权沙箱", Listen: port, Target: upstream.URL,
		Enabled: true, Websocket: true,
		AuthEnabled: true, AuthUser: "tester", AuthHash: hash, AuthFile: htpasswd,
	}
	content, err := rule.Generate("") // logDir=""：沙箱不写访问日志，避免属主问题
	if err != nil {
		t.Fatalf("生成鉴权规则的 nginx 配置失败：%v", err)
	}
	if !strings.Contains(content, "auth_basic ") {
		t.Fatalf("生成器没有产出 auth_basic：\n%s", content)
	}
	vhostPath := filepath.Join(confDir, "proxy-1.conf")
	if err := os.WriteFile(vhostPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// $connection_upgrade 是面板在 conf.d 里提供的 map；规则开了 WebSocket 就会引用它。
	if err := os.WriteFile(filepath.Join(confDir, "upgrade-map.conf"),
		[]byte("map $http_upgrade $connection_upgrade { default upgrade; '' close; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	main := `worker_processes 1;
error_log ` + filepath.Join(logDir, "error.log") + ` warn;
pid ` + filepath.Join(runDir, "nginx.pid") + `;
events { worker_connections 64; }
http {
    include /opt/homebrew/etc/nginx/mime.types;
    default_type application/octet-stream;
    access_log ` + filepath.Join(logDir, "access.log") + `;
    client_body_temp_path ` + filepath.Join(runDir, "body") + `;
    proxy_temp_path ` + filepath.Join(runDir, "proxy") + `;
    fastcgi_temp_path ` + filepath.Join(runDir, "fastcgi") + `;
    uwsgi_temp_path ` + filepath.Join(runDir, "uwsgi") + `;
    scgi_temp_path ` + filepath.Join(runDir, "scgi") + `;
    include ` + filepath.Join(confDir, "upgrade-map.conf") + `;
    include ` + vhostPath + `;
}
`
	mainPath := filepath.Join(confDir, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(nginxBin, "-c", mainPath, "-t").CombinedOutput(); err != nil {
		t.Fatalf("鉴权规则的配置没通过真实 nginx -t：\n%s", out)
	}
	if out, err := exec.Command(nginxBin, "-c", mainPath).CombinedOutput(); err != nil {
		t.Fatalf("启动沙箱 nginx 失败: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(nginxBin, "-c", mainPath, "-s", "stop").Run()
	})
	waitPortForTest(t, port)

	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"

	// ① 无凭据 → 401，且 WWW-Authenticate 里带 Basic realm。
	code, body, hdr := fetchWithAuth(t, url, "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，实际 %d（body 前 200：%s）", code, firstN(body, 200))
	}
	challenge := hdr.Get("WWW-Authenticate")
	if !strings.Contains(challenge, "Basic realm=") {
		t.Errorf("401 响应缺少 WWW-Authenticate: Basic realm=...，实际 %q", challenge)
	}
	if !strings.Contains(challenge, proxies.ProxyAuthRealm) {
		t.Errorf("realm 里应带 %q，实际 %q", proxies.ProxyAuthRealm, challenge)
	}
	t.Logf("真实 nginx %s @ 127.0.0.1:%d：无凭据 → %d，WWW-Authenticate=%q",
		nginxVersion(t, nginxBin), port, code, challenge)

	// ② 正确凭据 → 200，且真的转到了上游。
	code, body, _ = fetchWithAuth(t, url, "tester", "s3cret")
	if code != http.StatusOK {
		t.Fatalf("正确凭据应 200，实际 %d（body 前 200：%s）\nnginx error.log:\n%s",
			code, firstN(body, 200), readFileOrEmpty(filepath.Join(logDir, "error.log")))
	}
	if !strings.Contains(body, "upstream-ok") {
		t.Errorf("200 的响应体应来自上游，实际：%s", firstN(body, 300))
	}
	t.Logf("正确凭据 → %d（body=%q）", code, firstN(body, 40))

	// ③ 错误凭据 → 401。
	code, _, hdr = fetchWithAuth(t, url, "tester", "wrong")
	if code != http.StatusUnauthorized {
		t.Fatalf("错误凭据应 401，实际 %d", code)
	}
	if !strings.Contains(hdr.Get("WWW-Authenticate"), "Basic realm=") {
		t.Errorf("错误凭据的 401 也应带 WWW-Authenticate，实际 %q", hdr.Get("WWW-Authenticate"))
	}
	t.Logf("错误凭据 → %d", code)
}

// nginxVersion 读一句 `nginx -v`（证据里写清是哪一版校验通过的）。
func nginxVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "-v").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// fetchWithAuth 发一次 GET，可选带 Basic Auth，返回 (状态码, 响应体, 响应头)。
func fetchWithAuth(t *testing.T, url, user, pass string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败：%v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "（读不到 " + path + "）"
	}
	return string(b)
}
