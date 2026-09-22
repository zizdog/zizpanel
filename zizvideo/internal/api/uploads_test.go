package api_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizvideo/internal/config"
)

// 上传三入口门禁：落点正确、流式落盘、越界拒绝、.part 清理、重复上传幂等。

type uploadStartBody struct {
	UploadID  string `json:"upload_id"`
	Dir       string `json:"dir"`
	LibraryID string `json:"library_id"`
	SeriesID  string `json:"series_id"`
	Files     []struct {
		Index int    `json:"index"`
		Name  string `json:"name"`
		Path  string `json:"path"`
		URL   string `json:"url"`
	} `json:"files"`
}

type uploadFinishBody struct {
	Dir          string `json:"dir"`
	SeriesID     string `json:"series_id"`
	Registered   int    `json:"registered"`
	Added        int    `json:"added"`
	Skipped      int    `json:"skipped"`
	Recognized   int    `json:"recognized"`
	Unidentified int    `json:"unidentified"`
	EpisodeCount int    `json:"episode_count"`
}

// callRaw sends a non-JSON body (the streaming upload) with the CSRF header.
func (e *env) callRaw(method, path, contentType string, body []byte, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.TS.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-CSRF-Token", e.CSFRaw())
	res, err := e.Client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw := readAll(e.t, res.Body)
	return res, raw
}

func (e *env) uploadStart(t *testing.T, body map[string]any) uploadStartBody {
	t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/uploads/start", body)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("start 失败 %d: %s", res.StatusCode, raw)
	}
	var out uploadStartBody
	decodeInto(t, env.Data, &out)
	if out.UploadID == "" || len(out.Files) == 0 {
		t.Fatalf("start 响应缺字段: %s", raw)
	}
	return out
}

func (e *env) uploadPut(t *testing.T, url string, data []byte, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	return e.callRaw(http.MethodPut, url, "application/octet-stream", data, header)
}

func (e *env) uploadFinish(t *testing.T, uploadID string) uploadFinishBody {
	t.Helper()
	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/uploads/"+uploadID+"/finish", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("finish 失败 %d: %s", res.StatusCode, raw)
	}
	var out uploadFinishBody
	decodeInto(t, env.Data, &out)
	return out
}

func mediaTotal(t *testing.T, e *env) int {
	t.Helper()
	_, env, raw := e.do(http.MethodGet, "/api/v1/media?per_page=1", nil)
	var meta struct {
		Total int `json:"total"`
	}
	if env.Meta == nil {
		t.Fatalf("媒体列表没有 meta: %s", raw)
	}
	decodeInto(t, env.Meta, &meta)
	return meta.Total
}

// 入口 1：库页上传 → 落到库根，只登记媒体、不建集。
func TestUploadToLibraryOnlyRegistersMedia(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)

	start := e.uploadStart(t, map[string]any{
		"library_id": lib.ID,
		"files":      []map[string]any{{"name": "clip.mp4", "size": 3}},
	})
	if start.Dir != libRoot {
		t.Fatalf("库上传落点 = %s, 期望 %s", start.Dir, libRoot)
	}
	res, raw := e.uploadPut(t, start.Files[0].URL, []byte("abc"), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT 失败 %d: %s", res.StatusCode, raw)
	}
	if _, err := os.Stat(filepath.Join(libRoot, "clip.mp4")); err != nil {
		t.Fatalf("文件没有落到库根: %v", err)
	}
	out := e.uploadFinish(t, start.UploadID)
	if out.Registered != 1 || out.Added != 0 || out.EpisodeCount != 0 {
		t.Fatalf("库上传只应登记媒体: %+v", out)
	}
	if n := mediaTotal(t, e); n != 1 {
		t.Fatalf("媒体总数 = %d, 期望 1", n)
	}
}

// 入口 2：剧场上传 → 落到 <库根>/<剧场名>，按文件名识别建集，顺序正确。
func TestUploadToSeriesDerivesDirAndOrdersEpisodes(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)
	series := e.createSeries("庆余年")

	start := e.uploadStart(t, map[string]any{
		"series_id":  series.ID,
		"library_id": lib.ID,
		"files": []map[string]any{
			{"name": "S01E02.mp4", "size": 2},
			{"name": "S01E01.mp4", "size": 2},
		},
	})
	wantDir := filepath.Join(libRoot, "庆余年")
	if start.Dir != wantDir {
		t.Fatalf("剧场落点 = %s, 期望 %s", start.Dir, wantDir)
	}
	for i, f := range start.Files {
		if _, raw := e.uploadPut(t, f.URL, []byte("ab"), nil); raw == nil {
			t.Fatalf("PUT #%d 无响应", i)
		}
	}
	out := e.uploadFinish(t, start.UploadID)
	if out.Registered != 2 || out.Added != 2 || out.Recognized != 2 || out.EpisodeCount != 2 {
		t.Fatalf("剧场上传结果不对: %+v", out)
	}
	detail := e.seriesDetail(series.ID)
	got := []string{}
	for _, item := range detail.List {
		got = append(got, item.EpisodeLabel)
	}
	if len(got) != 2 || got[0] != "S1E1" || got[1] != "S1E2" {
		t.Fatalf("上传后集号顺序不对: %v", got)
	}
	// 剧场记住了自己的目录：下次只给 series_id 也能落回同一目录（不外发路径）。
	again := e.uploadStart(t, map[string]any{
		"series_id": series.ID,
		"files":     []map[string]any{{"name": "S01E03.mp4", "size": 2}},
	})
	if again.Dir != wantDir {
		t.Fatalf("剧场第二次上传落点 = %q, 期望记住的 %q", again.Dir, wantDir)
	}
}

// 入口 3：新建剧场并上传 → 建目录 + 建剧场 + 识别排集。
func TestUploadNewSeriesCreatesDirAndSeries(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)

	start := e.uploadStart(t, map[string]any{
		"new_series_name": "狂飙",
		"library_id":      lib.ID,
		"files":           []map[string]any{{"name": "EP01.mp4", "size": 2}},
	})
	if filepath.Base(start.Dir) != "狂飙" {
		t.Fatalf("新建剧场的目录 = %s", start.Dir)
	}
	if start.SeriesID == "" {
		t.Fatal("new_series_name 必须建出剧场")
	}
	e.uploadPut(t, start.Files[0].URL, []byte("ab"), nil)
	out := e.uploadFinish(t, start.UploadID)
	if out.Added != 1 || out.Recognized != 1 {
		t.Fatalf("新建剧场上传结果不对: %+v", out)
	}
	created := e.seriesByTitle(t, "狂飙")
	if created.EpisodeCount != 1 {
		t.Fatalf("新剧场集数 = %d, 期望 1", created.EpisodeCount)
	}
}

// 负向对照：落点目录越界必须被拒。
func TestUploadRejectsOutsideAllowRoot(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	e.newLibrary("上传库", libRoot)
	outside := filepath.Join(e.Base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	res, _, raw := e.write(http.MethodPost, "/api/v1/admin/uploads/start", map[string]any{
		"dir_path": outside,
		"files":    []map[string]any{{"name": "a.mp4", "size": 1}},
	})
	if res.StatusCode == http.StatusCreated {
		t.Fatalf("越界目录必须被拒: %s", raw)
	}
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusBadRequest {
		t.Fatalf("越界目录状态 = %d: %s", res.StatusCode, raw)
	}
	// 文件名带路径也要拒。
	res, _, raw = e.write(http.MethodPost, "/api/v1/admin/uploads/start", map[string]any{
		"dir_path": libRoot,
		"files":    []map[string]any{{"name": "../escape.mp4", "size": 1}},
	})
	if res.StatusCode == http.StatusCreated {
		t.Fatalf("带路径的文件名必须被拒: %s", raw)
	}
}

// 负向对照：单文件上限可配，超限如实拒绝。
func TestUploadRejectsOversizeFile(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.UploadMaxFileMB = 1 })
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)
	res, _, raw := e.write(http.MethodPost, "/api/v1/admin/uploads/start", map[string]any{
		"library_id": lib.ID,
		"files":      []map[string]any{{"name": "big.mp4", "size": 2 << 20}},
	})
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限状态 = %d, 期望 413: %s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upload_max_file_mb") {
		t.Fatalf("超限理由要说清怎么改: %s", raw)
	}
}

// 门禁：大小不符要清理 .part，不留下半截文件。
func TestUploadMismatchCleansPart(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)
	start := e.uploadStart(t, map[string]any{
		"library_id": lib.ID,
		"files":      []map[string]any{{"name": "a.mp4", "size": 10}},
	})
	res, raw := e.uploadPut(t, start.Files[0].URL, []byte("abc"), nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("大小不符应 400，得到 %d: %s", res.StatusCode, raw)
	}
	entries, err := os.ReadDir(libRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".zvpart") {
			t.Fatalf("残留临时文件: %s", entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(libRoot, "a.mp4")); err == nil {
		t.Fatal("大小不符不应留下最终文件")
	}
}

// 负向对照：扩展名白名单外的文件不许登记成视频。
func TestUploadRejectsForeignExtension(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)
	res, _, raw := e.write(http.MethodPost, "/api/v1/admin/uploads/start", map[string]any{
		"library_id": lib.ID,
		"files":      []map[string]any{{"name": "notes.txt", "size": 1}},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf(".txt 应被拒（%d）: %s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "media_extensions") {
		t.Fatalf("拒绝理由要说清怎么放宽: %s", raw)
	}
}

// 门禁：Content-Range 分块（断点续传）——未传完保留 .part，续传对齐后原子落盘。
func TestUploadResumeByContentRange(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)
	start := e.uploadStart(t, map[string]any{
		"library_id": lib.ID,
		"files":      []map[string]any{{"name": "chunked.mp4", "size": 6}},
	})
	url := start.Files[0].URL
	res, raw := e.uploadPut(t, url, []byte("abc"), map[string]string{"Content-Range": "bytes 0-2/6"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("第一块应 202 等续传，得到 %d: %s", res.StatusCode, raw)
	}
	if _, err := os.Stat(filepath.Join(libRoot, "chunked.mp4.zvpart")); err != nil {
		t.Fatalf("未传完应保留 .part 以便续传: %v", err)
	}
	res, raw = e.uploadPut(t, url, []byte("def"), map[string]string{"Content-Range": "bytes 3-5/6"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("续传完成应 200，得到 %d: %s", res.StatusCode, raw)
	}
	data, err := os.ReadFile(filepath.Join(libRoot, "chunked.mp4"))
	if err != nil || string(data) != "abcdef" {
		t.Fatalf("续传内容不对: %q err=%v", data, err)
	}
	out := e.uploadFinish(t, start.UploadID)
	if out.Registered != 1 {
		t.Fatalf("续传后登记数 = %d, 期望 1", out.Registered)
	}
}

// 门禁：同名默认保留两者（改名），overwrite 覆盖同一路径时不重复登记集（幂等）。
func TestUploadSameNamePolicyAndIdempotent(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	libRoot := filepath.Join(e.Root, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := e.newLibrary("上传库", libRoot)
	series := e.createSeries("庆余年")

	body := func(overwrite bool) map[string]any {
		return map[string]any{
			"series_id": series.ID, "library_id": lib.ID, "overwrite": overwrite,
			"files": []map[string]any{{"name": "S01E01.mp4", "size": 2}},
		}
	}
	first := e.uploadStart(t, body(false))
	e.uploadPut(t, first.Files[0].URL, []byte("ab"), nil)
	e.uploadFinish(t, first.UploadID)

	second := e.uploadStart(t, body(false))
	if second.Files[0].Path == first.Files[0].Path {
		t.Fatalf("同名默认应改名保留两者: %s", second.Files[0].Path)
	}
	e.uploadPut(t, second.Files[0].URL, []byte("cd"), nil)
	e.uploadFinish(t, second.UploadID)
	if n := mediaTotal(t, e); n != 2 {
		t.Fatalf("改名后媒体总数 = %d, 期望 2", n)
	}

	// overwrite=true 覆盖同一路径：不新增媒体、不新增集。
	third := e.uploadStart(t, body(true))
	if third.Files[0].Path != first.Files[0].Path {
		t.Fatalf("overwrite 应落回同一路径: %s != %s", third.Files[0].Path, first.Files[0].Path)
	}
	e.uploadPut(t, third.Files[0].URL, []byte("ef"), nil)
	out := e.uploadFinish(t, third.UploadID)
	if out.Added != 0 || out.Registered != 0 {
		t.Fatalf("重复上传同路径必须幂等: %+v", out)
	}
	if n := mediaTotal(t, e); n != 2 {
		t.Fatalf("重复上传后媒体总数 = %d, 期望 2", n)
	}
}
