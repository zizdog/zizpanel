package services

// Transmission 中文界面（面板内嵌的第三方前端）的门禁。
//
// 锁四件事：
//   ① 内嵌的那份资产**真的是中文界面**（index.html 引用自己的 assets/*.js，且 js 里有汉字）
//      —— 换资产时忘了更新或者放错一份，这里先红；
//   ② 写进 web 根目录是幂等的，且官方 index.html 只备份一次（覆盖前就备份）；
//   ③ `brew upgrade` 把 web 根目录换回官方英文那一份之后，判据必须发现（不能只看标记文件），
//      并且重写能修好；
//   ④ 端到端判据能**失败**：喂官方英文页面 / 英文脚本必须报错，喂中文才通过（负向对照）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// transmissionWebUITestManager 造一个 brew 前缀在临时目录里的 Manager + 已装好官方界面的 web 根目录。
func transmissionWebUITestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	m, _ := sandboxIdempotentManager(t)
	prefix := t.TempDir()
	// brewPrefix() = dirname(dirname(BrewBin))，所以这样写前缀就落在 prefix 里。
	m.opt.BrewBin = filepath.Join(prefix, "bin", "brew")
	m.opt.UserName = ""
	dir := filepath.Join(prefix, "share", "transmission", "public_html")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 官方那一份（英文，index.html 引用相对路径的 transmission-app.js）
	if err := os.WriteFile(filepath.Join(dir, "index.html"), stockTransmissionIndex, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transmission-app.js"), []byte("var app='Transmission Web Interface';"), 0o644); err != nil {
		t.Fatal(err)
	}
	return m, dir
}

var stockTransmissionIndex = []byte(`<!doctype html><html><head>` +
	`<script type="text/javascript" src="./transmission-app.js"></script>` +
	`<title>Transmission Web Interface</title></head><body></body></html>`)

// TestTransmissionWebUIEmbeddedIsChinese ①：内嵌资产本身必须就是中文界面那一份。
func TestTransmissionWebUIEmbeddedIsChinese(t *testing.T) {
	idx, err := transmissionWebUIFS.ReadFile(transmissionWebUIAssetsRoot + "/index.html")
	if err != nil {
		t.Fatalf("内嵌的 index.html 读不到：%v", err)
	}
	ref := transmissionWebUIJSRefRe.FindSubmatch(idx)
	if ref == nil {
		t.Fatal("内嵌 index.html 里没有 assets/*.js 引用：资产放错了")
	}
	js, err := transmissionWebUIFS.ReadFile(transmissionWebUIAssetsRoot + "/" +
		strings.TrimPrefix(string(ref[1]), "./"))
	if err != nil {
		t.Fatalf("内嵌界面脚本读不到（%s）：%v", ref[1], err)
	}
	if !containsCJK(js) {
		t.Fatal("内嵌界面脚本里没有汉字：这不是中文界面那一份")
	}
	// 负向对照：官方那份 index.html 必须被判成"不是中文界面"。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), stockTransmissionIndex, 0o644); err != nil {
		t.Fatal(err)
	}
	if why := transmissionWebUIServeProblem(dir); why == "" {
		t.Fatal("官方英文 index.html 被判成了中文界面（判据失效）")
	}
	// 正负对照：把内嵌这一份铺进去必须判成中文界面。
	if err := writeEmbeddedTransmissionWebUI(dir); err != nil {
		t.Fatal(err)
	}
	if why := transmissionWebUIServeProblem(dir); why != "" {
		t.Fatalf("内嵌界面铺进去后判据仍报问题：%s", why)
	}
}

// TestEnsureTransmissionWebUIAppliesIdempotently ②③：幂等、只备份一次、被换回官方后能自愈。
func TestEnsureTransmissionWebUIAppliesIdempotently(t *testing.T) {
	m, dir := transmissionWebUITestManager(t)
	ctx := context.Background()
	res := &InstallResult{App: "transmission", Steps: []string{}}

	if err := m.EnsureTransmissionWebUI(ctx, res); err != nil {
		t.Fatalf("首次写入中文界面失败：%v", err)
	}
	// 官方 index.html 必须在**被覆盖之前**留了一份备份（内容与官方原文一致）。
	bak, err := os.ReadFile(filepath.Join(dir, transmissionWebUIStockBak))
	if err != nil {
		t.Fatalf("没有备份官方 index.html：%v", err)
	}
	if string(bak) != string(stockTransmissionIndex) {
		t.Fatal("备份下来的不是官方那份 index.html（备份时机不对）")
	}
	if why := transmissionWebUIServeProblem(dir); why != "" {
		t.Fatalf("写入后判据报问题：%s", why)
	}
	if ok, why := m.transmissionWebUIApplied(); !ok {
		t.Fatalf("写入后仍判成没有中文界面：%s", why)
	}
	marker, err := os.ReadFile(filepath.Join(dir, transmissionWebUIMarker))
	if err != nil || strings.TrimSpace(string(marker)) != transmissionWebUIVersion {
		t.Fatalf("标记文件不对：%q / %v", string(marker), err)
	}

	// 幂等：再写一次不该报错，也不该动备份（备份只留第一次那版官方文件）。
	if err := m.EnsureTransmissionWebUI(ctx, res); err != nil {
		t.Fatalf("第二次写入失败（应当直接跳过）：%v", err)
	}
	again, _ := os.ReadFile(filepath.Join(dir, transmissionWebUIStockBak))
	if string(again) != string(stockTransmissionIndex) {
		t.Fatal("第二次写入把官方备份覆盖掉了（备份应只留一次）")
	}

	// 判据看的是**磁盘上的文件**：把 js 换成英文（标记文件还在）必须能被发现。
	jsPath := filepath.Join(dir, "assets")
	entries, err := os.ReadDir(jsPath)
	if err != nil {
		t.Fatalf("内嵌 assets 没铺开：%v", err)
	}
	var jsName string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			jsName = e.Name()
		}
	}
	if jsName == "" {
		t.Fatal("assets 里没有 js")
	}
	if err := os.WriteFile(filepath.Join(jsPath, jsName), []byte("var app='English only';"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.transmissionWebUIApplied(); ok {
		t.Fatal("js 被换成英文后仍判成中文界面（判据只看标记文件 = 谎报）")
	}

	// brew upgrade 模拟：标记文件消失 + index.html 换回官方那份 ⇒ 自愈必须修好。
	if err := os.Remove(filepath.Join(dir, transmissionWebUIMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), stockTransmissionIndex, 0o644); err != nil {
		t.Fatal(err)
	}
	m.EnsureTransmissionWebUIQuiet(ctx)
	if ok, why := m.transmissionWebUIApplied(); !ok {
		t.Fatalf("官方界面换回来之后没有自愈：%s", why)
	}

	// 没装 Transmission（web 目录不存在）时必须如实报错，绝不凭空造一个目录。
	empty, _ := sandboxIdempotentManager(t)
	empty.opt.BrewBin = filepath.Join(t.TempDir(), "bin", "brew")
	empty.opt.UserName = ""
	if err := empty.EnsureTransmissionWebUI(ctx, nil); err == nil {
		t.Fatal("web 目录不存在时 EnsureTransmissionWebUI 竟然报了成功")
	}
}

// TestVerifyTransmissionWebUIServed ④：端到端判据必须能失败。
func TestVerifyTransmissionWebUIServed(t *testing.T) {
	m, _ := transmissionWebUITestManager(t)
	ctx := context.Background()
	idx, err := transmissionWebUIFS.ReadFile(transmissionWebUIAssetsRoot + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	ref := transmissionWebUIJSRefRe.FindSubmatch(idx)
	js, err := transmissionWebUIFS.ReadFile(transmissionWebUIAssetsRoot + "/" +
		strings.TrimPrefix(string(ref[1]), "./"))
	if err != nil {
		t.Fatal(err)
	}
	old := transmissionHTTPBody
	t.Cleanup(func() { transmissionHTTPBody = old })

	serve := func(index, script []byte) {
		transmissionHTTPBody = func(_ *Manager, _ context.Context, rawURL, _, _ string) (int, []byte, error) {
			if strings.HasSuffix(rawURL, "/transmission/web/") {
				return 200, index, nil
			}
			return 200, script, nil
		}
	}
	// 中文：通过
	serve(idx, js)
	if err := m.verifyTransmissionWebUIServed(ctx, "u", "p"); err != nil {
		t.Fatalf("中文界面竟然没通过：%v", err)
	}
	// 官方英文首页：必须报"还是官方英文界面"
	serve(stockTransmissionIndex, []byte("var app='Transmission Web Interface';"))
	err = m.verifyTransmissionWebUIServed(ctx, "u", "p")
	if err == nil || !strings.Contains(err.Error(), "官方英文界面") {
		t.Fatalf("官方英文页面没有被判成英文界面（err=%v）", err)
	}
	// 首页引用了 assets/js，但脚本是英文：同样必须失败
	serve(idx, []byte("var app='English only';"))
	if err := m.verifyTransmissionWebUIServed(ctx, "u", "p"); err == nil {
		t.Fatal("英文脚本被当成了中文界面（判据失效）")
	}
}
