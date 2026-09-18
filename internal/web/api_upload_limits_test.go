package web

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  「上传与执行限制」接口（用户报障：phpMyAdmin 导入 413，面板找不到入口）
//
//  这组测试全部跑在 newTestServer 的沙箱里（BrewPrefix/VhostDir/WWWRoot 都是
//  临时目录），并且把"写 vhost / reload / 请求级复核"三个提权步骤注入成假实现
//  —— 单测不许调提权助手、不许碰真实 nginx。
// ============================================================================

// newUploadLimitsServer 造好沙箱、假 PHP 与一个静态站点，并注入假的特权步骤。
func newUploadLimitsServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	srv, ts := newTestServer(t)
	seedFakePHP(t, srv.Cfg.BrewPrefix, "8.2")
	if err := os.MkdirAll(srv.Cfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 假的特权通道：把内容写进沙箱 vhost 目录，reload 与探针都不真的发生。
	prevWrite, prevReload, prevProbe, prevDefaultProbe :=
		siteWriteVhostFn, siteReloadFn, siteProbeFn, defaultProbeFn
	siteWriteVhostFn = func(s *Server, _ context.Context, domain, content string) error {
		return os.WriteFile(filepath.Join(s.Cfg.VhostDir, domain+".conf"), []byte(content), 0o644)
	}
	siteReloadFn = func(*Server, context.Context) error { return nil }
	siteProbeFn = func(context.Context, string, string, int, string, time.Duration) (string, string, error) {
		return "403", "", nil
	}
	defaultProbeFn = func(string) (int, string) { return 200, sites.LocalhostIndexMarker }
	t.Cleanup(func() {
		siteWriteVhostFn, siteReloadFn, siteProbeFn, defaultProbeFn =
			prevWrite, prevReload, prevProbe, prevDefaultProbe
	})
	// 一个静态站点：保存限制时面板会重写它的 vhost。
	if err := srv.siteMgr().Create(context.Background(), &sites.Site{
		Domain: "big.test", Root: filepath.Join(srv.Cfg.WWWRoot, "big.test"),
		Rewrite: "none", Enabled: true,
	}); err != nil {
		t.Fatalf("造测试站点失败: %v", err)
	}
	return srv, ts
}

// waitTask 等任务结束并返回元信息（绝不真睡满超时）。
func waitTask(t *testing.T, srv *Server, id string) (status, errMsg string, result any) {
	t.Helper()
	task := srv.Tasks.Get(id)
	if task == nil {
		t.Fatalf("任务 %s 不存在", id)
	}
	select {
	case <-task.Done():
	case <-time.After(30 * time.Second):
		t.Fatalf("任务 %s 30 秒内没有结束", id)
	}
	m := task.Meta()
	return string(m.Status), m.Error, m.Result
}

// TestUploadLimitsGetReturnsDefaults：新装面板必须直接给出能用的默认值，
// 不能让用户先撞 413 才知道要改。
func TestUploadLimitsGetReturnsDefaults(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "GET", "/api/v1/settings/upload-limits", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("GET 返回 %d: %v", res.StatusCode, body)
	}
	d := apiData(t, body)
	lim, _ := d["limits"].(map[string]any)
	if lim == nil {
		t.Fatalf("响应缺少 limits: %v", body)
	}
	if lim["client_max_body_size"] != "512m" {
		t.Errorf("nginx 默认上限应为 512m，实际 %v", lim["client_max_body_size"])
	}
	for k, want := range map[string]any{
		"upload_max_filesize": "512M", "post_max_size": "512M", "memory_limit": "512M",
		"max_execution_time": float64(300),
	} {
		if lim[k] != want {
			t.Errorf("%s 默认值应为 %v，实际 %v", k, want, lim[k])
		}
	}
	def, _ := d["defaults"].(map[string]any)
	if def == nil || def["client_max_body_size"] != "512m" {
		t.Errorf("响应必须带 defaults（界面显示默认值用）：%v", body)
	}
}

// TestUploadLimitsSaveRejectsInvalidInput：非法输入必须当场 400 + 人话，
// 而且**不创建任务**（把明显错误的输入丢进后台只会让用户在任务中心看到失败）。
func TestUploadLimitsSaveRejectsInvalidInput(t *testing.T) {
	srv, ts := newUploadLimitsServer(t)
	cookies := loginTestPanel(t, ts)

	cases := []map[string]any{
		{"client_max_body_size": "512 m"},
		{"client_max_body_size": "1;g"},
		{"upload_max_filesize": "abc"},
		{"post_max_size": "8M", "upload_max_filesize": "512M"}, // post < upload
		{"client_max_body_size": "1m", "upload_max_filesize": "512M"},
		{"max_execution_time": 0},
		{"max_execution_time": "半小时"},
	}
	for i, patch := range cases {
		res, body, _ := doJSON(t, ts, "POST", "/api/v1/settings/upload-limits", patch, cookies)
		if res.StatusCode != 400 {
			t.Errorf("第 %d 个非法输入应 400，实际 %d：%v", i+1, res.StatusCode, body)
			continue
		}
		msg, _ := body["msg"].(string)
		if msg == "" || !(strings.Contains(msg, "格式") || strings.Contains(msg, "不能") || strings.Contains(msg, "应")) {
			t.Errorf("第 %d 个非法输入的报错应是人话（含原因），实际：%q", i+1, msg)
		}
	}
	// 没有创建任何任务
	if list := srv.Tasks.List(); len(list) != 0 {
		t.Fatalf("非法输入不该创建任务，却出现了 %d 个：%+v", len(list), list)
	}
	// 配置也没被改
	if got := srv.Cfg.NginxClientMaxBodySize; got != "512m" {
		t.Fatalf("非法输入改动了配置：client_max_body_size=%q", got)
	}
}

// TestUploadLimitsSaveAppliesPersistsAndReadsBack 是核心用例：
// 保存 → 任务（写 vhost + conf.d）→ 回读 → 配置持久化（重载配置仍在）。
func TestUploadLimitsSaveAppliesPersistsAndReadsBack(t *testing.T) {
	srv, ts := newUploadLimitsServer(t)
	cookies := loginTestPanel(t, ts)

	res, body, _ := doJSON(t, ts, "POST", "/api/v1/settings/upload-limits", map[string]any{
		"client_max_body_size": "2g",
		"upload_max_filesize":  "1G",
		"post_max_size":        "1G",
		"memory_limit":         "512M",
		"max_execution_time":   600,
	}, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("保存应 202 + task_id，实际 %d：%v", res.StatusCode, body)
	}
	id, _ := apiData(t, body)["task_id"].(string)
	if id == "" {
		t.Fatalf("响应没有 task_id：%v", body)
	}
	status, errMsg, _ := waitTask(t, srv, id)
	task := srv.Tasks.Get(id)
	lines, _, _, _ := task.Snapshot(0, 4000)
	var logText strings.Builder
	for _, ln := range lines {
		logText.WriteString(ln.Text + "\n")
	}
	if status != "succeeded" {
		t.Fatalf("任务应成功，实际 status=%s err=%s\n--- 任务日志 ---\n%s", status, errMsg, logText.String())
	}
	// 任务日志里必须有回读行（"只报已保存"是不允许的）
	if !strings.Contains(logText.String(), "回读生效值") {
		t.Errorf("任务日志必须包含回读生效值：\n%s", logText.String())
	}

	// 1) 生成的站点 vhost 带自定义值
	vhost, err := os.ReadFile(filepath.Join(srv.Cfg.VhostDir, "big.test.conf"))
	if err != nil {
		t.Fatalf("站点 vhost 没写出来: %v", err)
	}
	if !strings.Contains(string(vhost), "client_max_body_size 2g;") {
		t.Errorf("站点 vhost 应带 client_max_body_size 2g：\n%s", vhost)
	}
	// 2) 默认站点/phpMyAdmin 入口也带（用户导入 phpMyAdmin 的那条路径）
	def, err := os.ReadFile(filepath.Join(srv.Cfg.VhostDir, "000-default.conf"))
	if err != nil {
		t.Fatalf("默认站点没写出来: %v", err)
	}
	if !strings.Contains(string(def), "client_max_body_size 2g;") {
		t.Errorf("默认站点应带 client_max_body_size 2g：\n%s", def)
	}
	if !strings.Contains(string(def), "location ^~ /phpmyadmin") {
		t.Errorf("默认站点应包含 phpMyAdmin 入口：\n%s", def)
	}
	// 3) PHP conf.d 片段（默认值来源）：内容正确
	frag, err := os.ReadFile(sites.PHPConfDPath(srv.Cfg.BrewPrefix, "8.2"))
	if err != nil {
		t.Fatalf("PHP 限制片段没写出来: %v", err)
	}
	for _, want := range []string{
		"upload_max_filesize = 1G", "post_max_size = 1G",
		"memory_limit = 512M", "max_execution_time = 600",
	} {
		if !strings.Contains(string(frag), want) {
			t.Errorf("片段缺少 %q：\n%s", want, frag)
		}
	}
	// 4) 配置持久化：从磁盘重新加载仍然是自定义值
	reloaded, err := config.Load(srv.Cfg.Path())
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	if reloaded.NginxClientMaxBodySize != "2g" || reloaded.PHPUploadMaxFilesize != "1G" ||
		reloaded.PHPPostMaxSize != "1G" || reloaded.PHPMemoryLimit != "512M" ||
		reloaded.PHPMaxExecutionTime != 600 {
		t.Fatalf("配置没有持久化: %+v", reloaded)
	}
	// 5) 重新生成（默认站点）仍然带自定义值 —— 持久化后的值真的被生成器用到
	content := srv.buildDefaultVhost()
	if !strings.Contains(content, "client_max_body_size 2g;") {
		t.Errorf("重新生成的默认站点没有带持久化的自定义值：\n%s", content)
	}
	// 6) GET 回读：nginx 侧应报告 2g 且标记已生效
	_, body2, _ := doJSON(t, ts, "GET", "/api/v1/settings/upload-limits", nil, cookies)
	d2 := apiData(t, body2)
	if d2["limits"] == nil {
		t.Fatalf("GET 缺 limits: %v", body2)
	}
	nginxList, _ := d2["nginx"].([]any)
	found := false
	for _, it := range nginxList {
		m, _ := it.(map[string]any)
		if m["file"] == "big.test.conf" {
			found = true
			if m["value"] != "2g" || m["ok"] != true {
				t.Errorf("回读 big.test.conf 应 value=2g ok=true，实际 %v", m)
			}
		}
	}
	if !found {
		t.Errorf("回读没有列出 big.test.conf：%v", d2["nginx"])
	}
}

// TestSiteListExposesConfigFiles：网站管理要拿得到"手动改配置文件"的清单
// （用户要求：面板也要有手动改 php/nginx 的入口），且路径必须来自后端
// （brew 前缀在 Apple Silicon / Intel 不同，前端不许拼字符串）。
func TestSiteListExposesConfigFiles(t *testing.T) {
	srv, ts := newUploadLimitsServer(t)
	cookies := loginTestPanel(t, ts)

	_, body, _ := doJSON(t, ts, "GET", "/api/v1/sites", nil, cookies)
	d := apiData(t, body)
	if d["brew_prefix"] != srv.Cfg.BrewPrefix {
		t.Errorf("sites 接口应回传 brew_prefix，实际 %v（期望 %v）", d["brew_prefix"], srv.Cfg.BrewPrefix)
	}
	files, _ := d["config_files"].([]any)
	if len(files) == 0 {
		t.Fatalf("config_files 不能为空：%v", body)
	}
	byPath := map[string]map[string]any{}
	for _, it := range files {
		m, _ := it.(map[string]any)
		path, _ := m["path"].(string)
		byPath[path] = m
	}
	// nginx 主配置
	if m := byPath[srv.Cfg.NginxConf]; m == nil {
		t.Errorf("清单缺少 nginx 主配置 %s：%v", srv.Cfg.NginxConf, byPath)
	} else if m["service"] != "nginx" {
		t.Errorf("nginx 主配置应带重启用的服务名 nginx，实际 %v", m["service"])
	}
	// 测试站点的 vhost
	if m := byPath[filepath.Join(srv.Cfg.VhostDir, "big.test.conf")]; m == nil {
		t.Errorf("清单缺少站点 vhost：%v", byPath)
	}
	// 每个 PHP 版本的 php.ini / php-fpm.conf / www.conf（+ 面板限制片段）
	phpIni := filepath.Join(srv.Cfg.BrewPrefix, "etc", "php", "8.2", "php.ini")
	if m := byPath[phpIni]; m == nil {
		t.Errorf("清单缺少 PHP 8.2 的 php.ini %s：%v", phpIni, byPath)
	} else if m["service"] == "" {
		t.Errorf("PHP 条目应带重启用的服务名：%v", m)
	}
	if m := byPath[sites.FPMConfPath(srv.Cfg.BrewPrefix, "8.2")]; m == nil {
		t.Errorf("清单缺少 PHP 8.2 的 www.conf：%v", byPath)
	}
	if m := byPath[sites.PHPConfDPath(srv.Cfg.BrewPrefix, "8.2")]; m == nil {
		t.Errorf("清单缺少面板写的限制片段：%v", byPath)
	}
	// MySQL
	myCnf := filepath.Join(srv.Cfg.BrewPrefix, "etc", "my.cnf")
	if m := byPath[myCnf]; m == nil {
		t.Errorf("清单缺少 MySQL 主配置 %s：%v", myCnf, byPath)
	}
	// 路径必须是绝对路径（相对路径会让文件接口的越界校验拒绝，用户看到"打不开"）
	for p := range byPath {
		if !filepath.IsAbs(p) {
			t.Errorf("配置清单里的路径必须是绝对路径，实际 %q", p)
		}
	}
}

// TestBuildDefaultVhostCarriesClientMaxBodySize：默认站点（000-default）必须有
// 请求体上限 —— 这是 phpMyAdmin 导入路径所在的那份 vhost。
//
// 默认必须能用（512m），自定义值必须被采纳，而且 server 级 + phpMyAdmin
// location 各一份（用户自己的默认站点只有我们插进去的那一段）。
func TestBuildDefaultVhostCarriesClientMaxBodySize(t *testing.T) {
	srv, _ := newTestServer(t)
	def := srv.buildDefaultVhost()
	if !strings.Contains(def, "client_max_body_size 512m;") {
		t.Fatalf("默认站点必须带 client_max_body_size 512m：\n%s", def)
	}
	if n := strings.Count(def, "client_max_body_size 512m;"); n != 2 {
		t.Errorf("默认站点应有 2 处上限（server 级 + phpMyAdmin location），实际 %d：\n%s", n, def)
	}
	// 改成自定义值后重新生成：必须跟着变（配置 → 生成物这条链路）
	srv.Cfg.NginxClientMaxBodySize = "3g"
	def = srv.buildDefaultVhost()
	if !strings.Contains(def, "client_max_body_size 3g;") {
		t.Fatalf("默认站点应采用自定义上限 3g：\n%s", def)
	}
	if strings.Contains(def, "512m") {
		t.Errorf("自定义值生效后不该还留着 512m：\n%s", def)
	}
}
