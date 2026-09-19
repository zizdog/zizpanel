package web

// api_files_roots_test.go —— 「位置」下拉门禁（用户报障：同路径/标签重复）。
// 断言 ① 路径唯一 ② 标签唯一 ③ 被父根覆盖且无独立用途的子目录不进下拉。全用假数据。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/files"
)

// fakeFileRoots 是一组构造出来的根目录（全在 t.TempDir() 下）。
type fakeFileRoots struct {
	base    string
	home    string
	www     string
	install string
	brew    string
	volume  string
	dataDir string
	logDir  string
	workDir string
}

// newFakeFileRootsServer：临时目录里搭出家目录/安装根/Homebrew/外接盘并返回 Server。
func newFakeFileRootsServer(t *testing.T) (*Server, fakeFileRoots) {
	t.Helper()
	base := t.TempDir()
	f := fakeFileRoots{
		base:    base,
		home:    filepath.Join(base, "Users", "tester"),
		install: filepath.Join(base, "opt", "zizpanel"),
		brew:    filepath.Join(base, "opt", "homebrew"),
		volume:  filepath.Join(base, "Volumes", "Backup"),
	}
	f.www = filepath.Join(f.home, "www")
	f.dataDir = filepath.Join(f.install, "data")
	f.logDir = filepath.Join(f.install, "logs")
	f.workDir = filepath.Join(f.install, "work")

	dirs := []string{
		f.www,
		filepath.Join(f.install, "bin"),
		f.logDir,
		f.workDir,
		f.dataDir,
		filepath.Join(f.brew, "etc", "nginx"),
		f.volume,
		// 两个应用都装在家目录下 —— 旧实现把它们都标成"用户目录"。
		filepath.Join(f.home, "ddns-go"),
		filepath.Join(f.home, "orbien-client"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("建测试目录失败 %s: %v", d, err)
		}
	}
	// brew 配置路径由 services.ConfigFilePath 用 HOMEBREW_PREFIX 推导，
	// 固定成临时前缀，断言才不会随开发机上真实 Homebrew 的位置而变。
	t.Setenv("HOMEBREW_PREFIX", f.brew)
	// 复用 api_files_tcc_test.go 的注入器：本机真的插着外接盘，不注入就会飘。
	withVolumeMounts(t, f.volume)

	s := &Server{Cfg: &config.Config{
		User:       "tester",
		UserHome:   f.home,
		WWWRoot:    f.www,
		DataDir:    f.dataDir,
		LogDir:     f.logDir,
		WorkDir:    f.workDir,
		BinDir:     filepath.Join(f.install, "bin"),
		BrewPrefix: f.brew,
	}}
	return s, f
}

// entryByPath 按"解析后的真实路径"找条目。
func entryByPath(t *testing.T, entries []fileRootEntry, p string) (fileRootEntry, bool) {
	t.Helper()
	key := resolveForCompare(p)
	for _, e := range entries {
		if e.key == key {
			return e, true
		}
	}
	return fileRootEntry{}, false
}

// TestDefaultFileRootEntriesUniqueAndPruned 是这次报障的主门禁。
func TestDefaultFileRootEntriesUniqueAndPruned(t *testing.T) {
	s, f := newFakeFileRootsServer(t)
	entries := s.defaultFileRootEntries()
	if len(entries) == 0 {
		t.Fatal("默认根集合是空的")
	}

	// ① 路径唯一（按解析后的真实路径）。
	seenPath := map[string]fileRootEntry{}
	for _, e := range entries {
		if e.key == "" {
			t.Errorf("条目 %q 没有解析后的真实路径 key", e.Path)
			continue
		}
		if prev, dup := seenPath[e.key]; dup {
			t.Errorf("同一个路径出现两次：%s（%q 与 %q）", e.key, prev.Path, e.Path)
		}
		seenPath[e.key] = e
	}

	// ② 标签唯一且非空。
	seenLabel := map[string]string{}
	for _, e := range entries {
		if strings.TrimSpace(e.Label) == "" {
			t.Errorf("根 %s 没有展示标签 —— 下拉里会退回裸路径", e.Path)
			continue
		}
		if prev, dup := seenLabel[e.Label]; dup {
			t.Errorf("标签重复：%q 同时属于 %s 与 %s", e.Label, prev, e.Path)
		}
		seenLabel[e.Label] = e.Path
	}

	// 白名单路径列表与条目同源同序（Hidden 的条目也在白名单里）。
	paths := s.defaultFileRoots()
	if len(paths) != len(entries) {
		t.Fatalf("defaultFileRoots 与 defaultFileRootEntries 数量不一致：%d vs %d", len(paths), len(entries))
	}
	for i := range paths {
		if paths[i] != entries[i].Path {
			t.Errorf("第 %d 项路径不一致：defaultFileRoots=%q entries=%q", i, paths[i], entries[i].Path)
		}
	}

	// ③ 被安装根覆盖、没有独立用途的子目录不进下拉；但必须仍在白名单里。
	for _, p := range []string{f.logDir, f.workDir, f.dataDir} {
		e, ok := entryByPath(t, entries, p)
		if !ok {
			t.Errorf("%s 丢了 —— Hidden 只是不进下拉，白名单必须保留它", p)
			continue
		}
		if !e.Hidden {
			t.Errorf("%s 不该出现在「位置」下拉里（被安装根完整覆盖、没有独立用途）", p)
		}
	}

	// 有独立用途的根一个都不能被折叠掉。
	for _, p := range []string{f.www, f.home, f.install, f.volume} {
		e, ok := entryByPath(t, entries, p)
		if !ok {
			t.Errorf("%s 丢了 —— 它必须还在根集合里", p)
			continue
		}
		if e.Hidden {
			t.Errorf("%s 不该被折叠掉（它有独立用途）", p)
		}
	}

	// 家目录下两个应用：标签必须能区分，类型必须是 app（不是笼统的 home）。
	ddns, okDDNS := entryByPath(t, entries, filepath.Join(f.home, "ddns-go"))
	orbien, okOrbien := entryByPath(t, entries, filepath.Join(f.home, "orbien-client"))
	if !okDDNS || !okOrbien {
		t.Fatalf("应用配置目录没进根集合：ddns-go=%v orbien-client=%v", okDDNS, okOrbien)
	}
	if ddns.Kind != fileRootKindApp || orbien.Kind != fileRootKindApp {
		t.Errorf("应用配置目录必须被认成 app：ddns-go kind=%q orbien-client kind=%q", ddns.Kind, orbien.Kind)
	}
	if ddns.Label == orbien.Label {
		t.Errorf("家目录下两个应用的标签必须不同：%q / %q", ddns.Label, orbien.Label)
	}
	for _, e := range []fileRootEntry{ddns, orbien} {
		// 位置可以是 ~/ddns-go 或完整路径，但必须点到具体目录，不能只写"用户目录"。
		if !strings.Contains(e.Label, filepath.Base(e.Path)) {
			t.Errorf("应用标签 %q 没点到目录名 %s，用户分不清选的是哪个", e.Label, filepath.Base(e.Path))
		}
	}
}

// TestFilesRootLabelsReachDropdownWithoutDuplicates：走 HTTP handler 确认
// root_labels 真的下发（纯计算的门禁测不出"字段忘了加进响应"）。
func TestFilesRootLabelsReachDropdownWithoutDuplicates(t *testing.T) {
	s, f := newFakeFileRootsServer(t)

	q := url.Values{}
	q.Set("path", f.www)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	s.handleFileList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("列目录失败：HTTP %d，body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			files.ListResult
			RootLabels map[string]string `json:"root_labels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v（原文 %q）", err, rec.Body.String())
	}
	if !body.OK {
		t.Fatalf("ok=false：%s", rec.Body.String())
	}
	labels := body.Data.RootLabels
	if len(labels) == 0 {
		t.Fatal("root_labels 没下发 —— 前端下拉会退回本地启发式（重复标签的老问题）")
	}

	// 标签唯一，且只覆盖实际返回的根。
	seenLabel := map[string]string{}
	for p, l := range labels {
		if strings.TrimSpace(l) == "" {
			t.Errorf("根 %s 的标签为空", p)
		}
		if prev, dup := seenLabel[l]; dup {
			t.Errorf("标签重复：%q 同时属于 %s 与 %s", l, prev, p)
		}
		seenLabel[l] = p
		if !strings.HasPrefix(p, "/") {
			t.Errorf("root_labels 的键必须是绝对真实路径，实际 %q", p)
		}
	}

	// 安装根、www、用户目录、磁盘卷要在；被覆盖的 logs/work 与敏感数据目录不在。
	for _, p := range []string{f.install, f.www, f.home, f.volume} {
		if _, ok := labels[resolveForCompare(p)]; !ok {
			t.Errorf("%s 必须在下拉里（既有入口不能丢）", p)
		}
	}
	for _, p := range []string{f.logDir, f.workDir, f.dataDir} {
		if _, ok := labels[resolveForCompare(p)]; ok {
			t.Errorf("%s 不该在下拉里", p)
		}
	}

	// 敏感标记不能因为"下拉看不见了"就丢：数据目录仍要能被前端认出。
	foundSensitive := false
	for _, p := range body.Data.SensitiveRoots {
		if p == resolveForCompare(f.dataDir) {
			foundSensitive = true
		}
	}
	if !foundSensitive {
		t.Errorf("敏感标记丢了：sensitive_roots=%v，期望包含 %s", body.Data.SensitiveRoots, f.dataDir)
	}
}

// TestFilesFrontendUsesBackendRootLabels：锁死"标签由后端下发"这条接线，
// 防止有人改回前端按 kind 现拼固定文案（重复标签复发）。
func TestFilesFrontendUsesBackendRootLabels(t *testing.T) {
	js := readAssetJS(t, "files.js")

	if !strings.Contains(js, "root_labels") {
		t.Fatal("files.js 不再读后端下发的 root_labels —— 标签会退回按 kind 现拼的固定文案（重复标签复发）")
	}
	if strings.Contains(js, "'/opt/zizpanel（面板安装根）'") {
		t.Error("files.js 又写死了 '/opt/zizpanel（面板安装根）' —— 安装根标签要按真实路径生成")
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`case\s+'panel':\s*return\s+r\s*\+\s*'（面板安装根）'`),
		regexp.MustCompile(`case\s+'homebrew':\s*return\s+r\s*\+\s*'（Homebrew 配置）'`),
	} {
		if !re.MatchString(js) {
			t.Errorf("files.js 的 rootLabel 回退分支不再带路径：找不到 %s", re.String())
		}
	}
}
