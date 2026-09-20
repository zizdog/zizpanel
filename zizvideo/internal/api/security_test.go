package api_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/storage"
)

// makeMediaRow registers a row for an arbitrary path without validating it, so
// the stream handler's own checks are what the test exercises.
func (e *env) makeMediaRow(libID, path string) *domain.Media {
	e.t.Helper()
	m := &domain.Media{ID: domain.NewID("med"), LibraryID: libID, Path: path,
		Title: "x", Size: 10, Status: domain.MediaReady,
		Codecs: domain.Codecs{Video: "h264"}}
	if err := e.DB.InsertMedia(m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

// TestStreamRefusesPathsOutsideTheLibrary covers traversal, allow-root and
// symlink escapes, and proves no bytes were served in any of them.
func TestStreamRefusesPathsOutsideTheLibrary(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)

	outside := filepath.Join(e.Base, "outside", "secret.mp4")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("TOPSECRETCONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the library pointing at the outside file.
	if err := os.Symlink(outside, filepath.Join(e.Root, "link.mp4")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		path     string
		wantCode int
	}{
		{"absolute path outside allow roots", outside, http.StatusForbidden},
		{"dot-dot traversal", e.Root + "/../outside/secret.mp4", http.StatusBadRequest},
		{"symlink pointing outside the library", filepath.Join(e.Root, "link.mp4"), http.StatusForbidden},
		{"system path", "/etc/passwd", http.StatusForbidden},
		{"not clean", e.Root + "/sub/../x.mp4", http.StatusBadRequest},
		{"trailing slash", e.Root + "/x.mp4/", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := e.makeMediaRow(lib.ID, tc.path)
			res, raw := e.call(http.MethodGet, "/api/v1/media/"+m.ID+"/stream", nil, false)
			if res.StatusCode != tc.wantCode {
				t.Fatalf("状态 = %d, 期望 %d (body=%s)", res.StatusCode, tc.wantCode, raw)
			}
			if bytes.Contains(raw, []byte("TOPSECRETCONTENT")) {
				t.Fatal("越界请求泄露了文件内容")
			}
			if res.StatusCode >= 400 && !bytes.Contains(raw, []byte("\"code\"")) {
				t.Fatalf("错误响应缺少错误码: %s", raw)
			}
		})
	}
}

// TestStreamDeletedFileIsReported keeps the fd-leak path honest: a row whose
// file is gone must not be served.
func TestStreamDeletedFileIsReported(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	p := filepath.Join(e.Root, "gone.mp4")
	m := e.newMedia(lib.ID, p, body(128))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	res, raw := e.call(http.MethodGet, "/api/v1/media/"+m.ID+"/stream", nil, false)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("状态 = %d, 期望 404 (%s)", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "MEDIA_FILE_GONE") {
		t.Fatalf("错误码不符: %s", raw)
	}
}

func TestStreamRefusesDisabledLibrary(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	m := e.newMedia(lib.ID, filepath.Join(e.Root, "clip.mp4"), body(32))
	disabled := false
	if _, err := e.DB.UpdateLibrary(lib.ID, storage.LibraryPatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	res, raw := e.call(http.MethodGet, "/api/v1/media/"+m.ID+"/stream", nil, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("禁用库不可播放, 状态 = %d (%s)", res.StatusCode, raw)
	}
}

// TestLibraryPathValidation drives every documented library-path rule.
func TestLibraryPathValidation(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()

	fileNotDir := filepath.Join(e.Root, "afile.mp4")
	if err := os.WriteFile(fileNotDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	notAllowed := filepath.Join(e.Base, "notallowed")
	if err := os.MkdirAll(notAllowed, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		root     string
		wantCode int
		wantErr  string
	}{
		{"relative", "media/sub", http.StatusBadRequest, "MEDIA_PATH_NOT_ABSOLUTE"},
		{"not clean", e.Root + "/", http.StatusBadRequest, "MEDIA_PATH_NOT_CLEAN"},
		{"dot dot", e.Root + "/..", http.StatusBadRequest, "MEDIA_PATH_NOT_CLEAN"},
		{"outside allow roots", notAllowed, http.StatusForbidden, "MEDIA_PATH_NOT_ALLOWED"},
		{"does not exist", filepath.Join(e.Root, "nope"), http.StatusBadRequest, "MEDIA_PATH_NOT_FOUND"},
		{"is a file", fileNotDir, http.StatusBadRequest, "MEDIA_PATH_NOT_DIR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, env, raw := e.write(http.MethodPost, "/api/v1/libraries", map[string]any{
				"name": "lib-" + tc.name, "root_path": tc.root})
			if res.StatusCode != tc.wantCode {
				t.Fatalf("状态 = %d, 期望 %d (%s)", res.StatusCode, tc.wantCode, raw)
			}
			if env.Error == nil || env.Error.Code != tc.wantErr {
				t.Fatalf("错误码 = %+v, 期望 %s", env.Error, tc.wantErr)
			}
			if !strings.Contains(string(raw), "MEDIA_PATH") {
				t.Fatalf("越界原因未如实返回: %s", raw)
			}
		})
	}

	t.Run("valid root is accepted", func(t *testing.T) {
		res, _, raw := e.write(http.MethodPost, "/api/v1/libraries", map[string]any{
			"name": "ok", "root_path": e.Root})
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("状态 = %d (%s)", res.StatusCode, raw)
		}
	})
}

// TestFfmpegArgsAreNeverShellInterpolated feeds a hostile file name into the
// scanner and asserts ffprobe received it as exactly one verbatim argument.
func TestFfmpegArgsAreNeverShellInterpolated(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	t.Setenv("ZV_TEST_ARZV_LOG", e.ArgvLog)
	lib := e.newLibrary("l", e.Root)

	// The extension whitelist forces the suffix; the metacharacters are intact.
	hostile := "; rm -rf ~.mp4"
	full := filepath.Join(e.Root, hostile)
	if err := os.WriteFile(full, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, env, raw := e.write(http.MethodPost, "/api/v1/libraries/"+lib.ID+"/scan",
		map[string]string{"kind": "full"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("状态 = %d (%s)", res.StatusCode, raw)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	decodeInto(t, env.Data, &out)
	waitTask(t, e, out.TaskID)

	log, err := os.ReadFile(e.ArgvLog)
	if err != nil {
		t.Fatalf("ffprobe 未被调用: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	// -v error -print_format json -show_format -show_streams <path>
	if len(lines) != 7 {
		t.Fatalf("ffprobe 参数个数 = %d, 期望 7: %q", len(lines), lines)
	}
	if lines[6] != full {
		t.Fatalf("路径参数被改写:\n得到 %q\n期望 %q", lines[6], full)
	}
	for _, l := range lines[:6] {
		if strings.ContainsAny(l, ";&|`$") {
			t.Fatalf("参数 %q 里出现了 shell 元字符", l)
		}
	}
	rows, _, err := e.DB.ListMedia(filterAll())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != full {
		t.Fatalf("媒体记录不符: %+v", rows)
	}
}
