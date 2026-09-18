package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  文件上传：上限、明确报错、文件夹（相对路径）与读超时
//
//  这些用例锁的是 2026-09-20 那次报障的**每一条**原因，而不只是被点名的那一个：
//   1. 上限太小 + 超限是在传完之后才判 → 413 必须能在接收 body 之前给出，
//      并且文案要含"你传了多少 / 上限多少 / 怎么办"；
//   2. 全局 ReadTimeout=30s 会掐断任何慢上传 → 上传路由必须自己解除它；
//   3. 「上传文件夹」按相对路径重建目录树 → 相对路径是用户可控输入，
//      必须拒绝 .. / 绝对路径 / 盘符 / 反斜杠 / 软链接穿越。
// ============================================================================

// uploadPart 是一个要放进 multipart 请求体的文件。
type uploadPart struct {
	filename string
	data     string
}

// buildUploadBody 造 multipart 请求体。
//
// fields 先写、files 按给定顺序后写 —— 顺序很重要：后端按 `files` 的出现顺序
// 与 relpaths 数组一一对齐，顺序错了就会把 A 目录的文件写进 B 目录。
func buildUploadBody(t *testing.T, dir string, rels []string, files []uploadPart) (*bytes.Buffer, string) {
	t.Helper()
	return buildUploadBodyWith(t, dir, rels, files, nil)
}

// buildUploadBodyWith 与 buildUploadBody 相同，但可以多带几个表单字段
// （如 on_conflict —— 同名文件"覆盖还是共存"的用户选择）。
func buildUploadBodyWith(t *testing.T, dir string, rels []string, files []uploadPart, extra map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("dir", dir); err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if rels != nil {
		raw, err := json.Marshal(rels)
		if err != nil {
			t.Fatal(err)
		}
		if err := mw.WriteField("relpaths", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		fw, err := mw.CreateFormFile("files", f.filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// postUpload 直接调 handleFileUpload（绕过鉴权与 CSRF：这里测的是上传本身，
// 鉴权另有门禁）。
func postUpload(t *testing.T, srv *Server, body io.Reader, contentType string) (*httptest.ResponseRecorder, uploadResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/upload", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	srv.handleFileUpload(rec, req)
	var resp uploadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

type uploadResponse struct {
	OK   bool   `json:"ok"`
	Msg  string `json:"msg"`
	Data struct {
		Uploaded []uploadedFile `json:"uploaded"`
		Failed   []string       `json:"failed"`
		Msg      string         `json:"msg"`
	} `json:"data"`
}

// siteDir 造一个上传目标目录（在测试沙箱的 WWWRoot 里，绝不会碰真实站点）。
func siteDir(t *testing.T, srv *Server, name string) string {
	t.Helper()
	dir := filepath.Join(srv.Cfg.WWWRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ---------- ① 超限：接收 body 之前就 413，且文案含上限与建议 ----------

func TestFileUploadRejectsOversizeContentLengthBeforeReadingBody(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := siteDir(t, srv, "site")

	// 造一个"声明长度超限"的请求：ContentLength 说它 6GB，而 body 里其实只有 1 字节。
	// 这正是 978MB 那次报障的等价物 —— 关键是断言处理器**一个字节都没读**，
	// 也就是"不会让用户先白传几百 MB"。
	probe := &countingReader{r: strings.NewReader("x")}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/upload", nil)
	req.Body = io.NopCloser(probe)
	req.ContentLength = 6 << 30 // 6 GiB
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nope")

	rec := httptest.NewRecorder()
	srv.handleFileUpload(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，想要 413（Request Entity Too Large）；body=%s", rec.Code, rec.Body.String())
	}
	if probe.n != 0 {
		t.Fatalf("超限请求竟然读了 body 的 %d 字节 —— 用户会先白传几百 MB 才被拒", probe.n)
	}
	var resp uploadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, want := range []string{"6.00 GB", "4.00 GB", "上传文件夹", "分卷压缩"} {
		if !strings.Contains(resp.Msg, want) {
			t.Errorf("413 文案里缺少 %q，实际文案：%s", want, resp.Msg)
		}
	}
	t.Logf("413 文案 = %s", resp.Msg)
	// 目标目录必须仍为空（拒绝发生在写盘之前）
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("被拒绝的超限上传竟然在目标目录留下了 %d 项", len(entries))
	}
}

// countingReader 记录被读走的字节数。
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadUploadFormClassifiesOverLimitAsTooLarge(t *testing.T) {
	// 单测没法真造一个 >4GB 的请求体，所以用一个小上限走完
	// "MaxBytesReader 截断 → 判成超限" 这条路径（ContentLength 未知的情况：
	// 分块传输 / 前端谎报长度）。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("dir", "/tmp"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("files", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("A"), 4096)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	const smallLimit = 512
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = -1 // 声明长度未知 → 只能边收边算

	err = readUploadForm(req, smallLimit)
	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("readUploadForm 返回 %v，想要 errUploadTooLarge", err)
	}

	// 文案（含实际大小未知、上限、建议）也要能过关
	msg := uploadLimitMessage(req.ContentLength, smallLimit, "请求体过大")
	if !strings.Contains(msg, "512 B") {
		t.Errorf("文案里没有上限 512 B：%s", msg)
	}
	if !strings.Contains(msg, "上传文件夹") || !strings.Contains(msg, "分卷压缩") {
		t.Errorf("文案里没有可执行的建议：%s", msg)
	}
	t.Logf("分块超限文案 = %s", msg)
}

// ---------- ② 正常单文件上传仍然工作（回归） ----------

func TestFileUploadSingleFileStillWorksAndDoesNotOverwrite(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := siteDir(t, srv, "site")

	body, ct := buildUploadBody(t, dir, nil, []uploadPart{{filename: "index.php", data: "<?php echo 1;"}})
	rec, resp := postUpload(t, srv, body, ct)
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("普通上传失败：code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "index.php"))
	if err != nil {
		t.Fatalf("文件没落盘: %v", err)
	}
	if string(got) != "<?php echo 1;" {
		t.Fatalf("文件内容 = %q", got)
	}

	// 同名再传一次：旧行为是**不覆盖**，自动加 -1 序号（不能悄悄改掉）
	body2, ct2 := buildUploadBody(t, dir, nil, []uploadPart{{filename: "index.php", data: "<?php echo 2;"}})
	if rec2, _ := postUpload(t, srv, body2, ct2); rec2.Code != http.StatusOK {
		t.Fatalf("第二次上传失败：code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	again, err := os.ReadFile(filepath.Join(dir, "index.php"))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != "<?php echo 1;" {
		t.Fatalf("普通上传把同名文件覆盖了（旧行为是不覆盖）：%q", again)
	}
	if _, err := os.Stat(filepath.Join(dir, "index-1.php")); err != nil {
		t.Fatalf("同名文件应当被存成 index-1.php: %v", err)
	}
}

// ---------- ③ 上传文件夹：相对路径建出子目录 ----------

func TestFileUploadFolderRebuildsTree(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := siteDir(t, srv, "webroot")

	rels := []string{"mysite/index.php", "mysite/assets/css/app.css", "mysite/assets/img/logo.svg"}
	files := []uploadPart{
		{filename: "index.php", data: "INDEX"},
		{filename: "app.css", data: "CSS"},
		{filename: "logo.svg", data: "SVG"},
	}
	body, ct := buildUploadBody(t, dir, rels, files)
	rec, resp := postUpload(t, srv, body, ct)
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("文件夹上传失败：code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(resp.Data.Uploaded) != 3 {
		t.Fatalf("成功数 = %d，想要 3（body=%s）", len(resp.Data.Uploaded), rec.Body.String())
	}
	want := map[string]string{
		"mysite/index.php":           "INDEX",
		"mysite/assets/css/app.css":  "CSS",
		"mysite/assets/img/logo.svg": "SVG",
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s 没落盘: %v", rel, err)
		}
		if string(got) != content {
			t.Errorf("%s 内容 = %q，想要 %q", rel, got, content)
		}
	}
	// 相对路径必须原样回给前端（前端靠它显示"传到哪了"）
	gotRels := map[string]bool{}
	for _, u := range resp.Data.Uploaded {
		gotRels[u.RelPath] = true
	}
	for rel := range want {
		if !gotRels[rel] {
			t.Errorf("响应里没有 rel_path=%s（实际：%v）", rel, gotRels)
		}
	}
	t.Logf("上传结果 msg = %s", resp.Data.Msg)
}

func TestFileUploadFolderOverwritesSameName(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := siteDir(t, srv, "webroot")
	if err := os.WriteFile(filepath.Join(dir, "index.php"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, ct := buildUploadBody(t, dir, []string{"index.php"}, []uploadPart{{filename: "index.php", data: "NEW"}})
	rec, resp := postUpload(t, srv, body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "index.php"))
	if err != nil {
		t.Fatal(err)
	}
	// 传整站时把 index.php 存成 index-1.php 会让站点直接跑不起来，所以文件夹上传是覆盖语义
	if string(got) != "NEW" {
		t.Fatalf("文件夹上传没有覆盖同名文件：%q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "index-1.php")); err == nil {
		t.Fatal("文件夹上传不该产生 index-1.php")
	}
	if len(resp.Data.Uploaded) != 1 || !resp.Data.Uploaded[0].Overwritten {
		t.Fatalf("结果里必须如实标出 overwritten=true：%+v", resp.Data.Uploaded)
	}
}

// ---------- ④ 恶意相对路径必须被拒绝，且绝不写到目标目录之外 ----------

func TestFileUploadFolderRejectsTraversalPaths(t *testing.T) {
	bad := []string{
		"../escaped.txt",
		"/abs.txt",
		"a/../../escaped.txt",
		"..\\escaped.txt",
		"C:evil.txt",
		"a//b.txt",
		"a/./b.txt",
	}
	for _, rel := range bad {
		t.Run(rel, func(t *testing.T) {
			srv, _ := newTestServer(t)
			root := srv.Cfg.WWWRoot
			dir := siteDir(t, srv, "site")
			outside := filepath.Join(root, "escaped.txt")

			body, ct := buildUploadBody(t, dir, []string{rel}, []uploadPart{{filename: "x.txt", data: "PWNED"}})
			rec, _ := postUpload(t, srv, body, ct)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("relpath=%q 的状态码 = %d，想要 400；body=%s", rel, rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(outside); err == nil {
				t.Fatalf("恶意相对路径 %q 把文件写到了目标目录之外：%s", rel, outside)
			}
			// 目标目录里也不该有任何东西
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("被拒绝的请求竟然在目标目录留下了 %d 项", len(entries))
			}
		})
	}
}

func TestFileUploadFolderRejectsSymlinkEscape(t *testing.T) {
	srv, _ := newTestServer(t)
	root := srv.Cfg.WWWRoot
	dir := siteDir(t, srv, "site")
	// 目标是站点目录里的一个软链接 → 指向白名单之外
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatalf("造软链接失败: %v", err)
	}

	body, ct := buildUploadBody(t, dir, []string{"link/evil.txt"}, []uploadPart{{filename: "evil.txt", data: "PWNED"}})
	rec, _ := postUpload(t, srv, body, ct)
	if rec.Code == http.StatusOK {
		t.Fatalf("借软链接穿越的上传竟然成功了：body=%s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatalf("文件被写到了软链接指向的目录之外：%s", filepath.Join(outside, "evil.txt"))
	}
	_ = root
}

func TestFileUploadFolderRejectsRelpathCountMismatch(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := siteDir(t, srv, "site")
	// 2 条相对路径但只有 1 个文件：宁可整单拒绝，也不能猜哪一个对应哪一个
	// （猜错就把文件写到了别的目录）。
	body, ct := buildUploadBody(t, dir, []string{"a/x.txt", "b/y.txt"},
		[]uploadPart{{filename: "x.txt", data: "X"}})
	rec, _ := postUpload(t, srv, body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，想要 400；body=%s", rec.Code, rec.Body.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("数量不一致的请求不该写盘，却留下了 %d 项", len(entries))
	}
}

// ---------- ⑤ 上传路由不受全局 30 秒 ReadTimeout 限制 ----------

// TestUploadRouteSurvivesShortServerReadTimeout 是这次报障**最核心**的一条。
//
// cmd/zizpanel/http.go 给整个面板设了 ReadTimeout=30s，而 Go 的 ReadTimeout 是
// "从连接建立到读完整个请求体"的绝对截止时间 —— 978MB 在 30 秒内传完需要
// 约 32MB/s，所以任何真实的大上传都会被服务端在 30 秒处掐断，浏览器只看到
// 网络错误（用户看到的就是"点了没反应"）。
//
// 这里用一个 ReadTimeout=150ms 的 http.Server + 分块慢速 body（总耗时 >600ms）
// 来证明：**如果 handler 没解除读截止时间，这个请求必然失败**。
// 断言了"body 真的超过 ReadTimeout"，否则这个测试什么都没证明。
func TestUploadRouteSurvivesShortServerReadTimeout(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := siteDir(t, srv, "slow")

	mux := http.NewServeMux()
	mux.HandleFunc("/upload", srv.handleFileUpload)
	ts := httptest.NewUnstartedServer(mux)
	// 模拟 cmd/zizpanel/http.go 的全局读超时（真机是 30s，这里缩短到 150ms
	// 才能在单测里跑完；被测的是同一条 ReadTimeout 机制）。
	ts.Config.ReadTimeout = 150 * time.Millisecond
	ts.Start()
	defer ts.Close()

	const boundary = "zp-test-boundary"
	pr, pw := io.Pipe()
	go func() {
		// 分三段吐，每段之间停 250ms —— 总耗时 >600ms，远超 150ms 的读超时。
		chunks := []string{
			"--" + boundary + "\r\nContent-Disposition: form-data; name=\"dir\"\r\n\r\n" + dir + "\r\n",
			"--" + boundary + "\r\nContent-Disposition: form-data; name=\"files\"; filename=\"slow.txt\"\r\n" +
				"Content-Type: text/plain\r\n\r\nslow-body-ok\r\n",
			"--" + boundary + "--\r\n",
		}
		for _, c := range chunks {
			if _, err := io.WriteString(pw, c); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		_ = pw.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/upload", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("慢速上传请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if elapsed <= ts.Config.ReadTimeout {
		t.Fatalf("body 用时 %v 没有超过服务端 ReadTimeout(%v) —— 这个测试没证明任何东西",
			elapsed, ts.Config.ReadTimeout)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("慢速上传被拒：code=%d body=%s（用时 %v，ReadTimeout=%v）",
			resp.StatusCode, bodyBytes, elapsed, ts.Config.ReadTimeout)
	}
	got, err := os.ReadFile(filepath.Join(dir, "slow.txt"))
	if err != nil {
		t.Fatalf("慢速上传的文件没落盘: %v", err)
	}
	if string(got) != "slow-body-ok" {
		t.Fatalf("文件内容 = %q", got)
	}
	t.Logf("慢速上传成功：用时 %v（服务端 ReadTimeout=%v）", elapsed, ts.Config.ReadTimeout)
}

// TestAllowLongUploadExtendsDeadline 直接验证"读截止时间被推后"这一行为本身。
func TestAllowLongUploadExtendsDeadline(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	err := allowLongUpload(rec, req)
	// httptest.ResponseRecorder 不支持 SetReadDeadline，必须返回 ErrNotSupported
	// 而不是 panic —— 生产环境的 http.Server 支持它，测试环境不支持。
	// 这里如实断言我们**没有把它当成致命错误**（调用方只记录日志）。
	if err == nil {
		t.Log("该 ResponseWriter 支持 SetReadDeadline（已延长）")
		return
	}
	if !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("allowLongUpload 返回了非 ErrNotSupported 的错误：%v", err)
	}
	t.Logf("测试用的 ResponseWriter 不支持 SetReadDeadline（%v）—— 调用方只记日志、不拦请求，符合设计", err)

	// 真实 http.Server 上必须成功延长
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := allowLongUpload(w, r); err != nil {
			t.Errorf("真实 http.Server 上 SetReadDeadline 失败: %v", err)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Start()
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
}

// ---------- ⑥ 同类路由：升级包上传 / 音色上传 ----------

// 用户点名的只是文件管理，但"全局 ReadTimeout=30s 掐断慢上传"是**一类**问题：
// 所有收 multipart 的路由都中招。这两个用例锁住另外两条。

func TestUpgradeUploadOversizeContentLengthIs413(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/upgrade/upload", nil)
	req.Body = io.NopCloser(strings.NewReader("x"))
	req.ContentLength = maxUploadBytes + 1
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nope")
	rec := httptest.NewRecorder()
	srv.handleUpgradeUpload(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("升级包上传的状态码 = %d，想要 413；body=%s", rec.Code, rec.Body.String())
	}
	var resp uploadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, want := range []string{"256.00 MB", "上传文件夹", "分卷压缩"} {
		if !strings.Contains(resp.Msg, want) {
			t.Errorf("413 文案里缺少 %q：%s", want, resp.Msg)
		}
	}
	t.Logf("升级包 413 文案 = %s", resp.Msg)
}

func TestVoiceUploadOversizeContentLengthIs413(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/voice/receiver/sources", nil)
	req.Body = io.NopCloser(strings.NewReader("x"))
	req.ContentLength = maxVoiceUpload + (1 << 20) + 1
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nope")
	rec := httptest.NewRecorder()
	srv.handleVoiceSourceUpload(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("音色上传的状态码 = %d，想要 413；body=%s", rec.Code, rec.Body.String())
	}
	var resp uploadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Msg, "音频文件过大") {
		t.Errorf("413 文案不对：%s", resp.Msg)
	}
	t.Logf("音色 413 文案 = %s", resp.Msg)
}

// TestEveryMultipartRouteClearsReadDeadline 是"覆盖整类"的门禁。
//
// 只修被点名的 handleFileUpload 是不够的：下一个新增的上传路由（以及
// 已经存在的升级包/音色上传）会原样再犯。这条测试遍历 web 包的所有源码，
// 断言**每个解析 multipart 的函数**都调用了 allowLongUpload。
func TestEveryMultipartRouteClearsReadDeadline(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// readUploadForm 是共用的解析辅助函数（自己不是 handler），
	// 调用它的函数由下面的第二条规则检查。
	skip := map[string]bool{"readUploadForm": true}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, fn := range splitTopLevelFuncs(string(b)) {
			name, body := fn.name, fn.body
			if skip[name] {
				continue
			}
			parsesMultipart := strings.Contains(body, "ParseMultipartForm(") ||
				strings.Contains(body, "readUploadForm(")
			if !parsesMultipart {
				continue
			}
			checked++
			if !strings.Contains(body, "allowLongUpload(") {
				t.Errorf("%s::%s 解析 multipart 却没有调用 allowLongUpload —— "+
					"全局 ReadTimeout(30s) 会在 30 秒处掐断它，而浏览器只看到网络错误（「点了没反应」）",
					name, fn.name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有扫描到任何 multipart 路由 —— 这条门禁失去了意义（正则/文件列表不对？）")
	}
	t.Logf("已检查 %d 个 multipart 处理函数", checked)
}

type srcFunc struct{ file, name, body string }

// funcNameRe 匹配顶层函数声明行（含方法接收者）。
var funcNameRe = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// splitTopLevelFuncs 粗切顶层 func（够用来做源码级不变量检查，不需要真解析器）。
// 只看行首的 `func ` —— 缩进的函数字面量与嵌套函数不会命中，正是我们想要的。
func splitTopLevelFuncs(src string) []srcFunc {
	var out []srcFunc
	lines := strings.Split(src, "\n")
	cur := -1
	curName := ""
	flush := func(end int) {
		if cur < 0 {
			return
		}
		out = append(out, srcFunc{name: curName, body: strings.Join(lines[cur:end], "\n")})
		cur = -1
	}
	for i, l := range lines {
		m := funcNameRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		flush(i)
		cur = i
		curName = m[1]
	}
	flush(len(lines))
	return out
}

// ---------- ⑦ 文件管理的默认目录必须是网站根目录 ----------

// TestFileListDefaultsToWWWRootNotAlphabeticalFirst 锁住一个真机复现过的坑：
//
// 白名单里的目录是**按字母排序**的（files.NewManager 里排过），而白名单还包含
// 「面板安装的应用的配置目录」。真机上 /opt/homebrew/etc 排在网站根目录前面，
// 于是用户点开「文件管理」看到的是 Homebrew 的配置目录、顺手就把网站文件传进去
// （2026-09-20 实测：上传目标显示为 /opt/homebrew/etc）。
// 代码注释一直写着"默认打开网站根目录"，但实现用的是 roots[0] —— 注释在说谎。
func TestFileListDefaultsToWWWRootNotAlphabeticalFirst(t *testing.T) {
	srv, _ := newTestServer(t)
	// 造一个按字母序会排在网站根目录**前面**的白名单目录
	early := filepath.Join(filepath.Dir(srv.Cfg.WWWRoot), "aaa-earlier-root")
	if err := os.MkdirAll(early, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.Cfg.FileRoots = []string{early, srv.Cfg.WWWRoot, srv.Cfg.DataDir}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/files", nil)
	rec := httptest.NewRecorder()
	srv.handleFileList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := resolveExistingForTest(srv.Cfg.WWWRoot)
	if resp.Data.Path != want {
		t.Fatalf("默认打开的目录 = %q，想要网站根目录 %q（按字母序的第一个是 %q）",
			resp.Data.Path, want, early)
	}
	// 传了 path 时仍然要按用户给的走
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/files?path="+early, nil)
	rec2 := httptest.NewRecorder()
	srv.handleFileList(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("显式 path 失败：code=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

// resolveExistingForTest 解析软链接（macOS 上 /var → /private/var），与 files 包同口径。
func resolveExistingForTest(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// TestFileExtractAndCompressGoThroughTaskCenter：打包/解压必须是**任务**（202）。
//
// 用户 2026-09-18 报障："我传了文件解压缩，一点反应都没有！900M 的文件解压
// 没有任何进度" —— 老实现是同步跑 unzip/tar：请求挂着、页面无输出、
// 用户一刷新还会把解压**杀掉**（任务挂在 r.Context 上）。现在两条都走任务中心，
// 进度写在任务日志里，关掉窗口也能找回。
func TestFileExtractAndCompressGoThroughTaskCenter(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	dir := filepath.Join(srv.Cfg.WWWRoot, "arch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 先打包（应当 202 + task_id）
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/files/compress",
		map[string]any{"dir": dir, "names": []string{"a.txt"}, "format": "zip", "output": "pack.zip"}, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("打包应创建任务（202），实际 %d（body=%v）", res.StatusCode, out)
	}
	if asString(out["data"].(map[string]any)["task_id"]) == "" {
		t.Errorf("202 必须带 task_id（关掉窗口也要能找回进度）：%v", out)
	}

	// 参数错误要**当场** 400（不建任务）
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/files/compress",
		map[string]any{"dir": dir, "names": []string{}, "format": "zip", "output": "x.zip"}, cookies)
	if res.StatusCode != 400 {
		t.Errorf("没有选中任何项时应 400，实际 %d（%v）", res.StatusCode, out)
	}

	// 解压（归档先造出来，测试里直接用 unzip 生成，避免依赖任务完成时序）
	zipPath := filepath.Join(dir, "pack.zip")
	if err := makeTestZip(t, zipPath, map[string]string{"b.txt": "world"}); err != nil {
		t.Fatal(err)
	}
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/files/extract",
		map[string]any{"archive": zipPath, "dest": dir}, cookies)
	if res.StatusCode != 202 {
		t.Fatalf("解压应创建任务（202），实际 %d（body=%v）", res.StatusCode, out)
	}
	if asString(out["data"].(map[string]any)["task_id"]) == "" {
		t.Errorf("202 必须带 task_id：%v", out)
	}

	// 归档越界（zip-slip）必须在**开任务之前**被拒绝
	slip := filepath.Join(dir, "slip.zip")
	if err := makeTestZip(t, slip, map[string]string{"../escape.txt": "pwn"}); err != nil {
		t.Fatal(err)
	}
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/files/extract",
		map[string]any{"archive": slip, "dest": dir}, cookies)
	if res.StatusCode != 400 {
		t.Errorf("含穿越路径的归档应 400（不建任务），实际 %d（%v）", res.StatusCode, out)
	}
}

// makeTestZip 造一个测试用 zip（键是归档内路径）。
func makeTestZip(t *testing.T, path string, files map[string]string) error {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(body)); err != nil {
			return err
		}
	}
	return zw.Close()
}

// ============================================================================
//  ⑥ 同名文件：用户必须先被问"覆盖还是共存"（2026-09-22 用户要求）
//
//  这条门禁锁的是**行为**（后端的两种策略与 overwritten 标记），前端的询问弹窗
//  由 files_upload_frontend_test.go 里的静态断言 + uitest 覆盖。
//  两条不变量：
//    · 普通上传默认 **不覆盖**（改名保留两者）—— 静默覆盖是不可原谅的默认值；
//    · 用户明确选 overwrite 时必须真的覆盖，并且如实回 overwritten=true。
// ============================================================================

func TestFileUploadConflictRenameAndOverwrite(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := siteDir(t, srv, "conflict")

	// 先放一个同名旧文件
	old := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(old, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ① 不传 on_conflict（普通上传默认 rename）：旧文件必须原样保留，新文件改名
	body, ct := buildUploadBody(t, dir, nil, []uploadPart{{filename: "data.txt", data: "NEW1"}})
	rec, resp := postUpload(t, srv, body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("默认上传应当成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(resp.Data.Uploaded) != 1 {
		t.Fatalf("应当上传 1 个文件：%+v", resp.Data)
	}
	got := resp.Data.Uploaded[0]
	if got.Name == "data.txt" || got.Overwritten {
		t.Errorf("普通上传默认必须改名保留两者，实际 name=%q overwritten=%v", got.Name, got.Overwritten)
	}
	if b, _ := os.ReadFile(old); string(b) != "OLD" {
		t.Errorf("默认策略绝不能动旧文件，实际内容 %q", b)
	}
	if b, _ := os.ReadFile(got.Path); string(b) != "NEW1" {
		t.Errorf("改名后的新文件应当是新内容，实际 %q", b)
	}
	if !strings.Contains(resp.Data.Msg, "改名") {
		t.Errorf("汇总文案要如实说明「自动改名保留两者」，实际 %q", resp.Data.Msg)
	}

	// ② on_conflict=overwrite：真覆盖，且如实标记
	body2, ct2 := buildUploadBodyWith(t, dir, nil, []uploadPart{{filename: "data.txt", data: "NEW2"}},
		map[string]string{"on_conflict": "overwrite"})
	rec2, resp2 := postUpload(t, srv, body2, ct2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("覆盖上传应当成功，实际 %d：%s", rec2.Code, rec2.Body.String())
	}
	got2 := resp2.Data.Uploaded[0]
	if got2.Name != "data.txt" || !got2.Overwritten {
		t.Errorf("选了覆盖就必须真覆盖并标记 overwritten，实际 name=%q overwritten=%v", got2.Name, got2.Overwritten)
	}
	if b, _ := os.ReadFile(old); string(b) != "NEW2" {
		t.Errorf("覆盖后旧文件内容应被替换成 NEW2，实际 %q", b)
	}
	if !strings.Contains(resp2.Data.Msg, "覆盖") {
		t.Errorf("汇总文案要如实说明「覆盖了同名文件」，实际 %q", resp2.Data.Msg)
	}

	// ③ 非法策略要明确 400，不许悄悄按默认值办
	body3, ct3 := buildUploadBodyWith(t, dir, nil, []uploadPart{{filename: "data.txt", data: "NEW3"}},
		map[string]string{"on_conflict": "whatever"})
	rec3, _ := postUpload(t, srv, body3, ct3)
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("非法 on_conflict 应当 400，实际 %d：%s", rec3.Code, rec3.Body.String())
	}
}
