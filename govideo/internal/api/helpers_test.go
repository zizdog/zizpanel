package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zizdog/govideo/internal/api"
	"github.com/zizdog/govideo/internal/auth"
	"github.com/zizdog/govideo/internal/config"
	"github.com/zizdog/govideo/internal/domain"
	"github.com/zizdog/govideo/internal/ffmpeg"
	"github.com/zizdog/govideo/internal/media"
	"github.com/zizdog/govideo/internal/storage"
	"github.com/zizdog/govideo/internal/task"
	"github.com/zizdog/govideo/internal/web"
)

// env is a fully isolated server: temp data dir, temp media root, stub tools.
type env struct {
	t       *testing.T
	S       *api.Server
	DB      *storage.DB
	Cfg     *config.Config
	TS      *httptest.Server
	Client  *http.Client
	Base    string
	Root    string
	ArgvLog string
}

// newEnv builds an env; tweak may adjust the config before wiring.
func newEnv(t *testing.T, tweak ...func(*config.Config)) *env {
	t.Helper()
	base := t.TempDir()
	// t.TempDir() may sit behind a symlink; resolve it so library paths are clean.
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	root := filepath.Join(base, "media")
	bins := filepath.Join(base, "bin")
	dataDir := filepath.Join(base, "data")
	for _, d := range []string{root, bins, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	probe := filepath.Join(bins, "ffprobe")
	ff := filepath.Join(bins, "ffmpeg")
	writeStub(t, probe, `#!/bin/sh
if [ "$1" = "-version" ]; then echo "ffprobe version 9.0.1-fake Copyright"; exit 0; fi
if [ -n "$GV_TEST_ARGV_LOG" ]; then printf '%s\n' "$@" >> "$GV_TEST_ARGV_LOG"; fi
src=""
for a in "$@"; do src="$a"; done
case "$src" in
  *bad*) echo "Invalid data found when processing input" >&2; exit 1;;
esac
cat <<'JSON'
{"streams":[{"codec_type":"video","codec_name":"h264","width":640,"height":360,"r_frame_rate":"30/1","duration":"5.000000"},
{"codec_type":"audio","codec_name":"aac"}],
"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"5.000000","bit_rate":"800000"}}
JSON
`)
	writeStub(t, ff, `#!/bin/sh
if [ "$1" = "-version" ]; then echo "ffmpeg version 9.0.1-fake Copyright"; exit 0; fi
if [ "$1" = "-hide_banner" ]; then echo " V....D h264_videotoolbox fake encoder"; exit 0; fi
out=""
for a in "$@"; do out="$a"; done
echo fake-jpeg > "$out"
`)

	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.DatabasePath = filepath.Join(dataDir, "govideo.db")
	cfg.MediaAllowRoots = []string{root}
	cfg.FFprobeBin = probe
	cfg.FFmpegBin = ff
	cfg.ScanWorkers = 2
	cfg.ScanRetryBackoffMS = []int{1, 1, 1}
	cfg.SessionTTLHours = 1
	cfg.LockoutThreshold = 3
	cfg.LockoutWindowMin = 1
	for _, f := range tweak {
		f(cfg)
	}

	db, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authMgr := auth.NewManager(db, []byte("test-secret-key"), cfg.SessionTTL(),
		cfg.LockoutThreshold, cfg.LockoutWindow())
	scanner := media.NewScanner(cfg, db, ffmpeg.ExecRunner{}, logger)
	tasks := task.NewManager(cfg, db, scanner, logger)
	srv := api.NewServer(cfg, db, authMgr, tasks, ffmpeg.ExecRunner{}, logger)

	ts := httptest.NewServer(web.Router(srv))
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	e := &env{t: t, S: srv, DB: db, Cfg: cfg, TS: ts,
		Client: &http.Client{Jar: jar}, Base: base, Root: root,
		ArgvLog: filepath.Join(base, "argv.log")}
	return e
}

func writeStub(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Meta  json.RawMessage `json:"meta"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *env) call(method, path string, body any, withCSRF bool) (*http.Response, []byte) {
	e.t.Helper()
	return e.callWith(e.Client, e.CSFRaw(), method, path, body, withCSRF)
}

// anonClient returns a fresh cookie jar, i.e. a second browser.
func (e *env) anonClient() *http.Client {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// callWith performs a request as an arbitrary client.
func (e *env) callWith(c *http.Client, csrf, method, path string, body any, withCSRF bool) (*http.Response, []byte) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.TS.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/130.0 Safari/537.36")
	if withCSRF {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return res, raw
}

// do performs a read request expecting the standard envelope.
func (e *env) do(method, path string, body any) (*http.Response, envelope, []byte) {
	e.t.Helper()
	res, raw := e.call(method, path, body, false)
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return res, env, raw
}

// write performs a write request with the CSRF header.
func (e *env) write(method, path string, body any) (*http.Response, envelope, []byte) {
	e.t.Helper()
	res, raw := e.call(method, path, body, true)
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return res, env, raw
}

// CSFRaw reads the readable CSRF cookie, as the browser frontend does.
func (e *env) CSFRaw() string { return csrfOf(e.t, e.Client, e.TS.URL) }

func csrfOf(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	for _, ck := range c.Jar.Cookies(mustParse(t, base)) {
		if ck.Name == api.CookieCSRF {
			return ck.Value
		}
	}
	return ""
}

// loginAs signs a specific client in.
func (e *env) loginAs(c *http.Client, username, password string) (*http.Response, []byte) {
	e.t.Helper()
	return e.callWith(c, "", http.MethodPost, "/api/v1/auth/login",
		map[string]string{"username": username, "password": password}, false)
}

// doAs issues a read request as a specific client.
func (e *env) doAs(c *http.Client, method, path string) (*http.Response, envelope, []byte) {
	e.t.Helper()
	res, raw := e.callWith(c, csrfOf(e.t, c, e.TS.URL), method, path, nil, false)
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return res, env, raw
}

// writeAs issues a CSRF-carrying write as a specific client.
func (e *env) writeAs(c *http.Client, method, path string, body any) (*http.Response, envelope, []byte) {
	e.t.Helper()
	res, raw := e.callWith(c, csrfOf(e.t, c, e.TS.URL), method, path, body, true)
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return res, env, raw
}

func sessionCookie(e *env) string {
	u := mustParse(e.t, e.TS.URL)
	for _, c := range e.Client.Jar.Cookies(u) {
		if c.Name == api.CookieSession {
			return c.Value
		}
	}
	return ""
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// setupAdmin completes first-run setup and keeps the session cookies.
func (e *env) setupAdmin() {
	e.t.Helper()
	res, _, raw := e.write(http.MethodPost, "/api/v1/setup", map[string]string{
		"username": "admin", "password": "adminpass123", "display_name": "Admin"})
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("初始化失败 %d: %s", res.StatusCode, raw)
	}
}

func (e *env) login(username, password string) (*http.Response, []byte) {
	e.t.Helper()
	return e.call(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"username": username, "password": password}, false)
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeRaw(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, raw)
	}
}

func decodeInto(t *testing.T, raw json.RawMessage, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("解析 data 失败: %v (%s)", err, raw)
	}
}

// waitTask polls a scan task until it reaches a terminal status.
func waitTask(t *testing.T, e *env, id string) envelope {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, env, raw := e.do(http.MethodGet, "/api/v1/scan-tasks/"+id, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("查询任务失败 %d: %s", res.StatusCode, raw)
		}
		var task struct {
			Status string `json:"status"`
		}
		decodeInto(t, env.Data, &task)
		switch task.Status {
		case "success", "failed", "interrupted":
			return env
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("任务 %s 未在超时内结束", id)
	return envelope{}
}

// filterAll lists every media row of the only library in the fixture.
func filterAll() storage.MediaFilter {
	return storage.MediaFilter{Page: 1, PerPage: 100, Sort: "id"}
}

// newLibrary inserts a library row directly; root must be inside an allow root.
func (e *env) newLibrary(name, root string) *domain.Library {
	e.t.Helper()
	lib := &domain.Library{ID: domain.NewID("lib"), Name: name, RootPath: root,
		Recursive: true, Enabled: true, IgnoreRules: []string{}}
	if err := e.DB.CreateLibrary(lib); err != nil {
		e.t.Fatal(err)
	}
	return lib
}

// newMedia writes a real file and registers it as a ready media row.
func (e *env) newMedia(libID, path string, content []byte) *domain.Media {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		e.t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		e.t.Fatal(err)
	}
	m := &domain.Media{
		ID: domain.NewID("med"), LibraryID: libID, Path: path, Title: "clip",
		Size: st.Size(), MtimeNS: st.ModTime().UnixNano(), Container: "mov",
		Codecs: domain.Codecs{Video: "h264", Audio: "aac"},
		Width:  640, Height: 360, DurationMS: 5000, Status: domain.MediaReady,
	}
	if err := e.DB.InsertMedia(m); err != nil {
		e.t.Fatal(err)
	}
	return m
}
