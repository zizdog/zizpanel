package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/imgopt"
)

// ============================================================================
//  图片压缩 Web UI 独立服务的契约
//
//  这些用例覆盖用户要求里"不能糊过去"的几条：
//    · 引擎可用 → 真的压出更小的结果（大小变化可核对）；
//    · 引擎不可用 → 409/503 + 原因，绝不是"点了没反应"；
//    · 超限 / 坏格式 → 413/400 + 明确数字与原因；
//    · 坏图 → 500 且带上 vips 的真实 stderr（同步路径没有日志窗口）；
//    · 多文件 → 202 + task_id，SSE 有 meta/lines/done，结果可下载、可打包 zip。
//
//  全部用**假 vips**（不依赖本机装没装 libvips）：真机真实压缩另有一条
//  跳过式测试（internal/imgopt 的 TestCompressWithRealVips）。
// ============================================================================

// fakeVipsPrefix 造一个假的 brew 前缀：里面有一个假 vips 与它的参数日志。
func fakeVipsPrefix(t *testing.T, mode string) (prefix, argLog string) {
	t.Helper()
	prefix = t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argLog = filepath.Join(prefix, "vips-args.log")
	script := `#!/bin/sh
echo "$@" >> ` + argLog + `
if [ "$1" = "--version" ]; then echo vips-8.18.6; exit 0; fi
out=""
for a in "$@"; do
  case "$a" in *.*) out="$a" ;; esac
done
out="${out%%[*}"
if [ "$MODE" = "fail" ]; then echo "vips: unable to load from file" >&2; exit 1; fi
[ -n "$out" ] && printf 'x' > "$out"
exit 0
`
	script = strings.ReplaceAll(script, "$MODE", mode)
	if err := os.WriteFile(filepath.Join(binDir, "vips"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return prefix, argLog
}

// newImgCompressTestServer 起一个独立服务（可选改配置）。
func newImgCompressTestServer(t *testing.T, mode string, mutate func(*ImgCompressOptions)) (*ImgCompressServer, *httptest.Server) {
	t.Helper()
	prefix, _ := fakeVipsPrefix(t, mode)
	opt := ImgCompressOptions{
		BrewPrefix:   prefix,
		TempRoot:     t.TempDir(),
		DetectEngine: imgopt.DetectEngine,
	}
	if mutate != nil {
		mutate(&opt)
	}
	s := NewImgCompressServer(opt)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

type imgTestFile struct {
	name    string
	content string
}

// postImages 发一次 multipart 上传。
func postImages(t *testing.T, ts *httptest.Server, files []imgTestFile, fields map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		w, err := mw.CreateFormFile("files", f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, f.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/api/v1/compress", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("POST /api/v1/compress 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &out)
	if _, ok := out["ok"]; !ok {
		t.Fatalf("响应不是约定的 JSON 契约：%s", body)
	}
	return resp, out
}

func jsonData(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, ok := out["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data：%v", out)
	}
	return d
}

func asStringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// TestImgCompressUIAssetsServeAndHaveNoAbsoluteRefs：
// 页面必须能直接打开，且**不能引用绝对路径的 js/css** ——
// 否则挂在 /imgcompress/ 别名下时资源会 404（白屏），而面板的探测会因为
// 找不到绝对引用而"看起来通过"。
func TestImgCompressUIAssetsServeAndHaveNoAbsoluteRefs(t *testing.T) {
	_, ts := newImgCompressTestServer(t, "ok", nil)
	for _, p := range []string{"/", "/app.js", "/app.css"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", p, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(b) == 0 {
			t.Fatalf("GET %s 应为 200 且非空，实际 %d（%d 字节）", p, resp.StatusCode, len(b))
		}
	}
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	html := string(b)
	if !strings.Contains(html, "图片压缩") {
		t.Errorf("首页应包含标题「图片压缩」")
	}
	if refs := refsFromHTML(html); len(refs) != 0 {
		t.Errorf("首页引用了绝对路径资源 %v —— 挂到 /imgcompress/ 下会 404（白屏），"+
			"必须用相对路径（./app.js）", refs)
	}
}

// TestImgCompressHealthzTellsTruth：/healthz 是**能力**判据（引擎可用性），
// 引擎不在时必须 503 + ok:false —— 面板据此标红，绝不给绿灯。
func TestImgCompressHealthzTellsTruth(t *testing.T) {
	// 用注入的引擎探测，避免依赖本机装没装 vips（测试机装了就漂了）。
	missing := NewImgCompressServer(ImgCompressOptions{
		TempRoot:     t.TempDir(),
		DetectEngine: func(string) imgopt.Engine { return imgopt.Engine{Reason: "这台机器上还没有 libvips"} },
	})
	tsMissing := httptest.NewServer(missing.Handler())
	defer tsMissing.Close()

	resp, err := http.Get(tsMissing.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("引擎不可用时 /healthz 必须 503（面板才会标红），实际 %d：%s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"ok":false`) || !strings.Contains(string(b), "libvips") {
		t.Errorf("503 响应必须带 ok:false 与原因，实际 %s", b)
	}

	_, tsOK := newImgCompressTestServer(t, "ok", nil)
	resp2, err := http.Get(tsOK.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(b2), `"ok":true`) {
		t.Fatalf("引擎可用时 /healthz 应 200 + ok:true，实际 %d：%s", resp2.StatusCode, b2)
	}
}

// TestImgCompressEngineMissingRejectsUpload：引擎不在时上传必须**立刻**被挡住
// 并指路（409 + 应用市场），绝不创建一个注定失败的任务、更不是"没反应"。
func TestImgCompressEngineMissingRejectsUpload(t *testing.T) {
	s := NewImgCompressServer(ImgCompressOptions{
		TempRoot: t.TempDir(),
		DetectEngine: func(string) imgopt.Engine {
			return imgopt.Engine{Reason: "这台机器上还没有 libvips。到「应用市场 → 图片压缩（libvips）」一键安装"}
		},
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, out := postImages(t, ts, []imgTestFile{{"a.jpg", strings.Repeat("A", 2048)}}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("引擎不可用时应 409，实际 %d（%v）", resp.StatusCode, out)
	}
	msg := asStringField(out, "msg")
	if !strings.Contains(msg, "引擎不可用") || !strings.Contains(msg, "应用市场") {
		t.Errorf("409 文案要说清原因并指路，实际 %q", msg)
	}
}

// TestImgCompressSingleFileSyncAndDownload：单文件走同步返回，
// 结果里有压前压后大小，且能按任务 id + 索引下载到真实产物、能打包 zip。
func TestImgCompressSingleFileSyncAndDownload(t *testing.T) {
	_, ts := newImgCompressTestServer(t, "ok", nil)
	resp, out := postImages(t, ts, []imgTestFile{{"photo.jpg", strings.Repeat("A", 8192)}},
		map[string]string{"quality": "55", "format": "webp"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("单文件小图应同步 200，实际 %d（%v）", resp.StatusCode, out)
	}
	data := jsonData(t, out)
	if data["sync"] != true {
		t.Errorf("单文件应标记 sync=true，实际 %v", data)
	}
	taskID := asStringField(data, "task_id")
	if taskID == "" {
		t.Fatal("同步返回也必须带 task_id（下载与复用任务都靠它）")
	}
	result, ok := data["result"].(map[string]any)
	if !ok {
		t.Fatalf("同步返回必须带 result，实际 %v", data)
	}
	if got := int(result["done"].(float64)); got != 1 {
		t.Fatalf("应有 1 个成功，实际 %d（%v）", got, result)
	}
	before := int64(result["before_bytes"].(float64))
	after := int64(result["after_bytes"].(float64))
	if !(before > 0 && after > 0 && after < before) {
		t.Fatalf("必须看到真实的大小变化（压后更小）：%d → %d", before, after)
	}
	items := result["items"].([]any)
	item := items[0].(map[string]any)
	if got := asStringField(item, "dst"); got != "photo.min.webp" {
		t.Errorf("输出名应为 photo.min.webp（改格式后扩展名要跟着变），实际 %q", got)
	}

	dresp, err := http.Get(fmt.Sprintf("%s/api/v1/tasks/%s/files/0", ts.URL, taskID))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(dresp.Body)
	_ = dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("单张下载应 200 且非空，实际 %d（%d 字节）", dresp.StatusCode, len(body))
	}
	if cd := dresp.Header.Get("Content-Disposition"); !strings.Contains(cd, "photo.min.webp") {
		t.Errorf("下载响应头应带文件名，实际 %q", cd)
	}

	zresp, err := http.Get(fmt.Sprintf("%s/api/v1/tasks/%s/zip", ts.URL, taskID))
	if err != nil {
		t.Fatal(err)
	}
	zbody, _ := io.ReadAll(zresp.Body)
	_ = zresp.Body.Close()
	if zresp.StatusCode != http.StatusOK || len(zbody) == 0 {
		t.Fatalf("zip 打包应 200 且非空，实际 %d", zresp.StatusCode)
	}
	if ct := zresp.Header.Get("Content-Type"); !strings.Contains(ct, "zip") {
		t.Errorf("zip 响应 Content-Type 应为 zip，实际 %q", ct)
	}
}

// TestImgCompressOptionsReachEngine：界面选的选项必须原样传到 vips 参数里
// （参数写错 vips 会**静默忽略**，压缩率悄悄变差而没人发现）。
func TestImgCompressOptionsReachEngine(t *testing.T) {
	prefix, argLog := fakeVipsPrefix(t, "ok")
	s := NewImgCompressServer(ImgCompressOptions{
		BrewPrefix: prefix, TempRoot: t.TempDir(), DetectEngine: imgopt.DetectEngine,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, out := postImages(t, ts, []imgTestFile{{"a.jpg", strings.Repeat("A", 4096)}},
		map[string]string{"quality": "37", "format": "webp", "max_edge": "1920", "strip_metadata": "1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，实际 %d（%v）", resp.StatusCode, out)
	}
	b, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("假 vips 没有被调用：%v", err)
	}
	args := string(b)
	for _, want := range []string{"thumbnail", "Q=37", "effort=4", "--height", "1920", "strip"} {
		if !strings.Contains(args, want) {
			t.Errorf("传给 vips 的参数里缺少 %q\n实际：%s", want, args)
		}
	}
}

// TestImgCompressRejectsOversizeAndBadFormat：超限 413、坏格式 400，都要有明确数字/名单。
func TestImgCompressRejectsOversizeAndBadFormat(t *testing.T) {
	_, ts := newImgCompressTestServer(t, "ok", func(o *ImgCompressOptions) {
		o.MaxFileBytes = 1024
	})
	resp, out := postImages(t, ts, []imgTestFile{{"big.jpg", strings.Repeat("A", 4096)}}, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过单文件上限应 413，实际 %d（%v）", resp.StatusCode, out)
	}
	if msg := asStringField(out, "msg"); !strings.Contains(msg, "上限") || !strings.Contains(msg, "big.jpg") {
		t.Errorf("413 文案要说清是哪个文件、上限多少，实际 %q", msg)
	}

	resp2, out2 := postImages(t, ts, []imgTestFile{{"notes.txt", "hello"}}, nil)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("非图片格式应 400，实际 %d（%v）", resp2.StatusCode, out2)
	}
	if msg := asStringField(out2, "msg"); !strings.Contains(msg, "格式") || !strings.Contains(msg, "notes.txt") {
		t.Errorf("400 文案要列出不支持的文件与支持的格式，实际 %q", msg)
	}
}

// TestImgCompressBadImageSurfacesRealError：坏图必须把引擎的真实 stderr 带回
// （同步路径没有任务日志窗口，只报"失败"等于没说）。
func TestImgCompressBadImageSurfacesRealError(t *testing.T) {
	_, ts := newImgCompressTestServer(t, "fail", nil)
	resp, out := postImages(t, ts, []imgTestFile{{"broken.jpg", strings.Repeat("A", 2048)}}, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("全部失败时应 500，实际 %d（%v）", resp.StatusCode, out)
	}
	msg := asStringField(out, "msg")
	if !strings.Contains(msg, "unable to load from file") {
		t.Errorf("必须带上 vips 的真实报错（用户要知道哪张图、为什么），实际 %q", msg)
	}
}

// TestImgCompressAsyncTaskProgressAndZip：多文件 → 202 + task_id；
// SSE 有 meta/lines/done；结束后结果完整、可打包。
func TestImgCompressAsyncTaskProgressAndZip(t *testing.T) {
	_, ts := newImgCompressTestServer(t, "ok", nil)
	files := []imgTestFile{
		{"one.jpg", strings.Repeat("A", 4096)},
		{"two.png", strings.Repeat("B", 4096)},
	}
	resp, out := postImages(t, ts, files, map[string]string{"quality": "70"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("多文件应 202（走任务），实际 %d（%v）", resp.StatusCode, out)
	}
	data := jsonData(t, out)
	taskID := asStringField(data, "task_id")
	if taskID == "" {
		t.Fatal("202 必须带 task_id")
	}

	// SSE：读到 done 为止（任务很快，服务端会先补 meta+lines 再发 done）。
	client := &http.Client{Timeout: 15 * time.Second}
	sresp, err := client.Get(fmt.Sprintf("%s/api/v1/tasks/%s/stream", ts.URL, taskID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sresp.Body.Close() }()
	if ct := sresp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("进度流 Content-Type 应为 text/event-stream，实际 %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(sresp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	deadline := time.Now().Add(10 * time.Second)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "event: done") || time.Now().After(deadline) {
			break
		}
	}
	for _, want := range []string{"meta", "lines", "done"} {
		found := false
		for _, e := range events {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("SSE 必须发出 %q 事件（真实进度 + 结束信号），实际事件：%v", want, events)
		}
	}

	// 终态：从任务接口取结果，两个文件都应成功。
	tresp, err := http.Get(fmt.Sprintf("%s/api/v1/tasks/%s", ts.URL, taskID))
	if err != nil {
		t.Fatal(err)
	}
	tb, _ := io.ReadAll(tresp.Body)
	_ = tresp.Body.Close()
	tout := map[string]any{}
	if err := json.Unmarshal(tb, &tout); err != nil {
		t.Fatalf("任务接口不是 JSON：%s", tb)
	}
	td := jsonData(t, tout)
	task, _ := td["task"].(map[string]any)
	if td["done"] != true {
		t.Fatalf("任务应已结束，实际 %v（%s）", td["done"], tb)
	}
	result, _ := task["result"].(map[string]any)
	if result == nil {
		t.Fatalf("任务结束后必须带完整结果，实际 %s", tb)
	}
	if got := int(result["done"].(float64)); got != 2 {
		t.Errorf("两个文件都应成功，实际 done=%d（%v）", got, result)
	}
	if int64(result["saved_bytes"].(float64)) <= 0 {
		t.Errorf("应报出省下的字节数，实际 %v", result["saved_bytes"])
	}

	zresp, err := http.Get(fmt.Sprintf("%s/api/v1/tasks/%s/zip", ts.URL, taskID))
	if err != nil {
		t.Fatal(err)
	}
	zb, _ := io.ReadAll(zresp.Body)
	_ = zresp.Body.Close()
	if zresp.StatusCode != http.StatusOK || len(zb) == 0 {
		t.Fatalf("任务 zip 应为 200 且非空，实际 %d", zresp.StatusCode)
	}
}

// TestImgCompressTaskNotFoundIsHonest：服务重启/清理后任务不存在要如实 404，
// 不是 200 空结果（否则前端会显示一张空表，用户以为"没有文件"）。
func TestImgCompressTaskNotFoundIsHonest(t *testing.T) {
	_, ts := newImgCompressTestServer(t, "ok", nil)
	resp, err := http.Get(ts.URL + "/api/v1/tasks/t-does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的任务应 404，实际 %d", resp.StatusCode)
	}
	resp2, err := http.Get(ts.URL + "/api/v1/tasks/t-does-not-exist/files/0")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的任务下载应 404，实际 %d", resp2.StatusCode)
	}
}
