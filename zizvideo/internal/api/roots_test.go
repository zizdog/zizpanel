package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/domain"
)

// rootList is the decoded shape of GET /api/v1/media/roots.
type rootList struct {
	Roots []struct {
		Path      string `json:"path"`
		Exists    bool   `json:"exists"`
		IsDir     bool   `json:"is_dir"`
		Readable  bool   `json:"readable"`
		InUse     bool   `json:"in_use"`
		Libraries int    `json:"libraries"`
	} `json:"roots"`
	ConfigPath  string   `json:"config_path"`
	EnvOverride bool     `json:"env_override"`
	Starts      []string `json:"starts"`
}

// readConfigBytes snapshots config.json for byte-level comparison.
func readConfigBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMediaRootsListReportsRealState(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	e.newLibrary("主库", e.Root)

	res, env, raw := e.do(http.MethodGet, "/api/v1/media/roots", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d: %s", res.StatusCode, raw)
	}
	var body rootList
	decodeInto(t, env.Data, &body)
	if len(body.Roots) != 1 || body.Roots[0].Path != e.Root {
		t.Fatalf("roots = %+v", body.Roots)
	}
	first := body.Roots[0]
	if !first.Exists || !first.IsDir || !first.Readable {
		t.Fatalf("真实目录应报 exists/is_dir/readable: %+v", first)
	}
	if !first.InUse || first.Libraries != 1 {
		t.Fatalf("被库使用的根应如实标注: %+v", first)
	}
	if body.ConfigPath != e.CfgPath {
		t.Fatalf("config_path = %q, 期望 %q", body.ConfigPath, e.CfgPath)
	}
	if body.EnvOverride {
		t.Fatal("未设置环境变量时 env_override 应为 false")
	}
	home, _ := os.UserHomeDir()
	if len(body.Starts) == 0 || body.Starts[0] != home {
		t.Fatalf("starts 应以 $HOME 打头: %v", body.Starts)
	}
}

func TestAddMediaRootRejectsBadPathsWithoutTouchingConfig(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	before := readConfigBytes(t, e.CfgPath)

	file := filepath.Join(e.Base, "clip.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.Base, "away")
	if err := os.Symlink("/private/tmp", link); err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		name string
		path string
	}{
		{"relative", "media/x"},
		{"missing", filepath.Join(e.Base, "nope")},
		{"file", file},
		{"dirty", e.Base + "/"},
		{"covered", filepath.Join(e.Root, "sub")},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			res, _, raw := e.write(http.MethodPost, "/api/v1/media/roots",
				map[string]string{"path": tc.path})
			if res.StatusCode != http.StatusBadRequest && res.StatusCode != http.StatusConflict {
				t.Fatalf("应拒绝 %q, 状态 = %d: %s", tc.path, res.StatusCode, raw)
			}
			if now := readConfigBytes(t, e.CfgPath); string(now) != string(before) {
				t.Fatalf("被拒后 config.json 被改动:\n%s\n%s", before, now)
			}
			if got := e.Roots.List(); len(got) != 1 || got[0] != e.Root {
				t.Fatalf("内存白名单被改动: %v", got)
			}
		})
	}
	// /media/{id} must not shadow the literal /media/roots route.
	if res, _, _ := e.do(http.MethodGet, "/api/v1/media/roots", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("路由被 /media/{id} 抢走: %d", res.StatusCode)
	}
}

func TestAddMediaRootPersistsAndRereads(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	extra := filepath.Join(e.Base, "external")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	res, env, raw := e.write(http.MethodPost, "/api/v1/media/roots",
		map[string]string{"path": extra})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("状态 = %d: %s", res.StatusCode, raw)
	}
	var body struct {
		Added     string   `json:"added"`
		RootsList []string `json:"roots_list"`
	}
	decodeInto(t, env.Data, &body)
	if body.Added != extra {
		t.Fatalf("added = %q, 期望 %q", body.Added, extra)
	}
	// Read the file back: the root must really be in it, not just in memory.
	back, err := config.RootsFromFile(e.CfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[1] != extra {
		t.Fatalf("config.json 回读 = %v", back)
	}
	if got := e.Roots.List(); len(got) != 2 || got[1] != extra {
		t.Fatalf("内存快照 = %v", got)
	}
	if _, _, err := config.Load(e.CfgPath); err != nil {
		t.Fatalf("写回后配置必须仍可加载: %v", err)
	}
	info, err := os.Stat(e.CfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config.json 权限 = %o", perm)
	}
	// Adding the same directory twice must be refused, not silently duplicated.
	if res, _, _ := e.write(http.MethodPost, "/api/v1/media/roots",
		map[string]string{"path": extra}); res.StatusCode != http.StatusConflict {
		t.Fatalf("重复添加应 409, 得到 %d", res.StatusCode)
	}
}

func TestAddMediaRootResolvesSymlink(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	link := filepath.Join(e.Base, "tmp-link")
	if err := os.Symlink("/private/tmp", link); err != nil {
		t.Fatal(err)
	}
	res, env, raw := e.write(http.MethodPost, "/api/v1/media/roots",
		map[string]string{"path": link})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("状态 = %d: %s", res.StatusCode, raw)
	}
	var body struct {
		Added string `json:"added"`
	}
	decodeInto(t, env.Data, &body)
	if body.Added != "/private/tmp" {
		t.Fatalf("软链接应存真实路径, 得到 %q", body.Added)
	}
}

func TestRemoveMediaRootRefusesWhileLibraryUsesIt(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	e.newLibrary("在用库", e.Root)

	res, env, raw := e.write(http.MethodDelete,
		"/api/v1/media/roots?path="+e.Root, nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("应 409, 得到 %d: %s", res.StatusCode, raw)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "在用库") {
		t.Fatalf("错误里必须点名库名: %s", raw)
	}
	if back, _ := config.RootsFromFile(e.CfgPath); len(back) != 1 {
		t.Fatalf("拒绝后文件被改动: %v", back)
	}
}

func TestRemoveMediaRootPersistsWhenUnused(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	extra := filepath.Join(e.Base, "spare")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if res, _, raw := e.write(http.MethodPost, "/api/v1/media/roots",
		map[string]string{"path": extra}); res.StatusCode != http.StatusCreated {
		t.Fatalf("添加失败 %d: %s", res.StatusCode, raw)
	}
	res, env, raw := e.write(http.MethodDelete,
		"/api/v1/media/roots?path="+extra, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200, 得到 %d: %s", res.StatusCode, raw)
	}
	var body struct {
		Removed   string   `json:"removed"`
		RootsList []string `json:"roots_list"`
	}
	decodeInto(t, env.Data, &body)
	if body.Removed != extra {
		t.Fatalf("removed = %q", body.Removed)
	}
	back, err := config.RootsFromFile(e.CfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0] != e.Root {
		t.Fatalf("回读 = %v", back)
	}
	// Deleting a path that is not an allow root must be honest.
	if res, _, _ := e.write(http.MethodDelete,
		"/api/v1/media/roots?path="+extra, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除应 404, 得到 %d", res.StatusCode)
	}
}

func TestMediaRootsRequireAdmin(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	res, _, _ := e.write(http.MethodPost, "/api/v1/users", map[string]any{
		"username": "bob", "password": "bobpass1234", "display_name": "Bob", "role": "user"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("建普通用户失败: %d", res.StatusCode)
	}
	bob := e.anonClient()
	if res, raw := e.loginAs(bob, "bob", "bobpass1234"); res.StatusCode != http.StatusOK {
		t.Fatalf("登录失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, _ := e.doAs(bob, http.MethodGet, "/api/v1/media/roots"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户读允许根应 403, 得到 %d", res.StatusCode)
	}
	if res, _, _ := e.doAs(bob, http.MethodGet, "/api/v1/fs/browse?path=/tmp"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户浏览目录应 403, 得到 %d", res.StatusCode)
	}
	if res, _, _ := e.writeAs(bob, http.MethodPost, "/api/v1/media/roots",
		map[string]string{"path": "/private/tmp"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户加根应 403, 得到 %d", res.StatusCode)
	}
}

func TestMediaRootWritesRequireCSRF(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	extra := filepath.Join(e.Base, "csrf-target")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	res, raw := e.call(http.MethodPost, "/api/v1/media/roots",
		map[string]string{"path": extra}, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 的写操作应 403, 得到 %d: %s", res.StatusCode, raw)
	}
	if now := readConfigBytes(t, e.CfgPath); strings.Contains(string(now), extra) {
		t.Fatal("CSRF 被拒后配置仍被写入")
	}
}

func TestOutOfBoundsLibraryIsRejectedWithoutTraversal(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()

	outside := filepath.Join(e.Base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "clip.mp4"), []byte("not-really-a-video"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bypass the handler: store a library row the server did not validate.
	lib := e.newLibrary("越界库", outside)

	res, _, raw := e.write(http.MethodPost, "/api/v1/libraries/"+lib.ID+"/scan", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("越界扫描应 403, 得到 %d: %s", res.StatusCode, raw)
	}
	if _, err := os.Stat(e.ArgvLog); !os.IsNotExist(err) {
		t.Fatal("被拒扫描不得调用 ffprobe，更不得遍历目录")
	}
	tasks, err := e.DB.ListLibraries()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("库数量 = %d", len(tasks))
	}

	// Creating through the API with an out-of-bounds path is refused too.
	if res, _, _ := e.write(http.MethodPost, "/api/v1/libraries", map[string]any{
		"name": "越界", "root_path": outside}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("越界建库应 403, 得到 %d", res.StatusCode)
	}
	// Patching a live library out of the allow roots is refused as well.
	if res, _, _ := e.write(http.MethodPatch, "/api/v1/libraries/"+lib.ID,
		map[string]any{"root_path": outside}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("越界改库应 403, 得到 %d", res.StatusCode)
	}
	stored, err := e.DB.GetLibrary(lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RootPath != outside {
		t.Fatalf("被拒后库路径被改动: %q", stored.RootPath)
	}
}

func TestScanRefusedAfterRootRemovedFromConfig(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	extra := filepath.Join(e.Base, "vault")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "clip.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, _, raw := e.write(http.MethodPost, "/api/v1/media/roots",
		map[string]string{"path": extra}); res.StatusCode != http.StatusCreated {
		t.Fatalf("加根失败 %d: %s", res.StatusCode, raw)
	}
	lib := e.newLibrary("库", extra)

	// Take the root away from a different instance, as a panel/script would,
	// then point the server at the reloaded view.
	other := config.NewRoots(e.CfgPath, e.Roots.List())
	if _, err := other.Remove(extra); err != nil {
		t.Fatal(err)
	}
	e.S.Roots = other
	e.S.Tasks.Roots = other
	res, env, _ := e.write(http.MethodPost, "/api/v1/libraries/"+lib.ID+"/scan", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("应 403, 得到 %d", res.StatusCode)
	}
	if env.Error == nil || env.Error.Code != domain.ErrPathNotAllowed.Code {
		t.Fatalf("错误码不符: %s", env.Error)
	}
	if _, err := os.Stat(e.ArgvLog); !os.IsNotExist(err) {
		t.Fatal("被拒扫描不得调用 ffprobe")
	}
}

func TestBrowseReturnsDirectoriesOnly(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	format := filepath.Join(e.Base, "format")
	for _, dir := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(format, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"movie.mp4", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(format, file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, env, raw := e.do(http.MethodGet, "/api/v1/fs/browse?path="+format, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d: %s", res.StatusCode, raw)
	}
	var body struct {
		Path    string `json:"path"`
		Parent  string `json:"parent"`
		Entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"entries"`
		Total   int      `json:"total"`
		HasMore bool     `json:"has_more"`
		Starts  []string `json:"starts"`
	}
	decodeInto(t, env.Data, &body)
	names := []string{}
	for _, entry := range body.Entries {
		names = append(names, entry.Name)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("目录列表 = %v", names)
	}
	if strings.Contains(string(raw), "movie.mp4") || strings.Contains(string(raw), "notes.txt") {
		t.Fatalf("响应里出现了文件名: %s", raw)
	}
	if body.Parent != e.Base {
		t.Fatalf("parent = %q", body.Parent)
	}
	if len(body.Starts) == 0 {
		t.Fatal("starts 不应为空")
	}

	// Paging keeps a huge directory from flooding one response.
	res2, env2, raw2 := e.do(http.MethodGet, "/api/v1/fs/browse?path="+format+"&limit=1", nil)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("分页状态 = %d: %s", res2.StatusCode, raw2)
	}
	decodeInto(t, env2.Data, &body)
	if len(body.Entries) != 1 || !body.HasMore || body.Total != 2 {
		t.Fatalf("分页结果 = %+v", body)
	}
}

func TestBrowseRejectsBadPaths(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	file := filepath.Join(e.Base, "clip.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		query  string
		status int
	}{
		{"relative", "?path=media", http.StatusBadRequest},
		{"empty", "", http.StatusBadRequest},
		{"missing", "?path=" + filepath.Join(e.Base, "ghost"), http.StatusBadRequest},
		{"file", "?path=" + file, http.StatusBadRequest},
		{"dirty", "?path=" + e.Base + "/", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _, raw := e.do(http.MethodGet, "/api/v1/fs/browse"+tc.query, nil)
			if res.StatusCode != tc.status {
				t.Fatalf("状态 = %d, 期望 %d: %s", res.StatusCode, tc.status, raw)
			}
		})
	}

	if os.Geteuid() == 0 {
		t.Skip("root 不走权限位，跳过不可读目录")
	}
	locked := filepath.Join(e.Base, "locked")
	if err := os.MkdirAll(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	res, _, raw := e.do(http.MethodGet, "/api/v1/fs/browse?path="+locked, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("不可读目录应 403, 得到 %d: %s", res.StatusCode, raw)
	}
}

// TestBrowseHidesEntriesOfHugeDirs keeps the response shape JSON-clean on a
// directory with more than one page of entries.
func TestBrowseHidesEntriesOfHugeDirs(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	big := filepath.Join(e.Base, "big")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := os.MkdirAll(filepath.Join(big, "d"+string(rune('a'+i))), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	res, env, raw := e.do(http.MethodGet, "/api/v1/fs/browse?path="+big+"&limit=2&offset=2", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d: %s", res.StatusCode, raw)
	}
	var body struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
		Offset  int  `json:"offset"`
	}
	decodeInto(t, env.Data, &body)
	if body.Total != 5 || body.Offset != 2 || len(body.Entries) != 2 || !body.HasMore {
		t.Fatalf("分页 = %+v: %s", body, raw)
	}
}
