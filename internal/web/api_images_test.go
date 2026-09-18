package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  图片压缩的接口契约（应用「图片压缩（libvips）」）
//
//  三条底线（都是用户反复要求过的形态）：
//    ① 引擎没装 → **立刻**拒绝并指路（不创建一个注定失败的任务）；
//    ② 参数/路径非法 → 400 + 人话（不静默、不半途失败）；
//    ③ 目录里没有图片 → 400 + 说清支持哪些格式、怎么扫子目录。
// ============================================================================

// seedFakeVips 在沙箱的 brew 前缀里放一个假 vips（报告版本、产出小文件）。
func seedFakeVips(t *testing.T, srv *Server, mode string) {
	t.Helper()
	bin := filepath.Join(srv.Cfg.BrewPrefix, "bin", "vips")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo vips-8.18.6; exit 0; fi
out=""
for a in "$@"; do
  case "$a" in
    *.*) out="$a" ;;
  esac
done
out="${out%%[*}"
case "` + mode + `" in
  fail) echo "vips: unable to load from file" >&2; exit 1 ;;
  *) [ -n "$out" ] && printf 'x' > "$out" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedImages 在沙箱 wwwRoot 下造几张"图片"（内容不重要，接口只看扩展名与大小）。
func seedImages(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("A", 512)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestImageEngineHonestWithoutEngine：没装引擎时必须如实说"没装"，并给出应用 ID
// （界面据此渲染「一键安装」按钮）。
func TestImageEngineHonestWithoutEngine(t *testing.T) {
	// ⚠️ 必须把 PATH 也隔离掉：DetectEngine 会回落到 exec.LookPath("vips")
	//（那是真实产品行为 —— 用户可能从别处装了 vips），而开发机上现在真的装了
	// vips（2026-09-18 为复现 HEIC 问题装的）。不清 PATH，这条断言就变成
	// "看这台机器装没装"，而不是在测代码。
	t.Setenv("PATH", t.TempDir())
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/images/engine", nil, cookies)
	data, _ := out["data"].(map[string]any)
	if data["available"] == true {
		t.Fatalf("沙箱里没有 vips，不能说引擎可用：%v", out)
	}
	if !strings.Contains(asString(data["reason"]), "应用市场") {
		t.Errorf("原因里要告诉用户去哪装，实际 %q", asString(data["reason"]))
	}
	if asString(data["market_app_id"]) != "imgcompress" {
		t.Errorf("必须给出市场应用 ID（界面据此一键安装），实际 %q", asString(data["market_app_id"]))
	}
	// 压缩请求同样要被挡住：409 + 原因，**不创建任务**。
	res, out2, _ := doJSON(t, ts, "POST", "/api/v1/images/compress",
		map[string]any{"dir": srv2WWW(t, "engine-missing")}, cookies)
	if res.StatusCode != 409 {
		t.Fatalf("引擎没装时应 409，实际 %d（body=%v）", res.StatusCode, out2)
	}
	if !strings.Contains(asString(out2["msg"]), "应用市场") {
		t.Errorf("409 文案要指路，实际 %q", asString(out2["msg"]))
	}
}

// srv2WWW 返回沙箱里一个可用的目录（测试用；不需要真实存在）。
func srv2WWW(t *testing.T, sub string) string {
	t.Helper()
	return filepath.Join(os.TempDir(), "zp-img-test-"+sub)
}

// TestImageEngineCountsImages：扫描要如实报"几张图、共多大"，并支持递归。
func TestImageEngineCountsImages(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	dir := filepath.Join(srv.Cfg.WWWRoot, "img-scan")
	seedImages(t, dir, "a.jpg", "b.PNG", "notes.txt")
	seedImages(t, filepath.Join(dir, "sub"), "c.webp")

	_, out, _ := doJSON(t, ts, "GET", "/api/v1/images/engine?dir="+dir, nil, cookies)
	data, _ := out["data"].(map[string]any)
	if got := int(data["image_count"].(float64)); got != 2 {
		t.Errorf("不递归时应只数本层（a.jpg / b.PNG）2 张，实际 %d（%v）", got, out)
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/images/engine?dir="+dir+"&recursive=1", nil, cookies)
	data, _ = out["data"].(map[string]any)
	if got := int(data["image_count"].(float64)); got != 3 {
		t.Errorf("递归时应数到子目录里的 3 张，实际 %d（%v）", got, out)
	}
	if int64(data["total_bytes"].(float64)) <= 0 {
		t.Errorf("要报出总字节数（用户据此判断值不值得跑）：%v", out)
	}
}

// TestImageCompressValidatesInput：参数与路径的非法输入一律 400 + 人话。
func TestImageCompressValidatesInput(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeVips(t, srv, "ok")
	dir := filepath.Join(srv.Cfg.WWWRoot, "img-bad")
	seedImages(t, dir, "a.jpg")

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"质量越界", map[string]any{"dir": dir, "quality": 500}, "质量"},
		{"格式不支持", map[string]any{"dir": dir, "format": "gif"}, "格式"},
		{"最长边越界", map[string]any{"dir": dir, "max_edge": 999999}, "最长边"},
		{"目录越界", map[string]any{"dir": "/etc"}, ""},
		{"目录不存在", map[string]any{"dir": filepath.Join(srv.Cfg.WWWRoot, "nope")}, ""},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/images/compress", c.body, cookies)
		if res.StatusCode != 400 {
			t.Errorf("%s：应 400，实际 %d（body=%v）", c.name, res.StatusCode, out)
			continue
		}
		if c.want != "" && !strings.Contains(asString(out["msg"]), c.want) {
			t.Errorf("%s：错误信息应提到 %q，实际 %q", c.name, c.want, asString(out["msg"]))
		}
	}

	// 目录里没有图片：400 + 说明支持哪些格式、怎么扫子目录。
	empty := filepath.Join(srv.Cfg.WWWRoot, "img-empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/images/compress", map[string]any{"dir": empty}, cookies)
	if res.StatusCode != 400 {
		t.Fatalf("没有图片时应 400，实际 %d（%v）", res.StatusCode, out)
	}
	msg := asString(out["msg"])
	for _, want := range []string{"jpg", "子目录"} {
		if !strings.Contains(msg, want) {
			t.Errorf("提示里应包含 %q（用户要知道支持什么、怎么扫子目录），实际 %q", want, msg)
		}
	}
}

// TestImageCompressCreatesTask：合法请求 → 202 + task_id（长任务走任务中心）。
func TestImageCompressCreatesTask(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	seedFakeVips(t, srv, "ok")
	dir := filepath.Join(srv.Cfg.WWWRoot, "img-task")
	seedImages(t, dir, "a.jpg", "b.png")

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/images/compress",
		map[string]any{"dir": dir, "quality": 80, "format": "webp", "max_edge": 1920}, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("应创建任务（202），实际 %d（body=%v）", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if asString(data["task_id"]) == "" {
		t.Errorf("202 必须带 task_id（关掉窗口也要能找回进度）：%v", out)
	}
}
