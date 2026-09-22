package services

// zizvideo_index_e2e_test.go —— 应用级索引（apps/zizvideo/manifest.json）的沙箱端到端门禁。
//
// 假镜像用 httptest 起在 127.0.0.1；取件走**默认实现**（真跑 curl 到本地假镜像，绝不碰外网）。
// 锁住三件事：① 索引说了算（版本/产物/sha 都按索引，面板常量被无视）；
// ② 索引 sha 对不上 ⇒ 拒绝安装且不落安装位；③ 刷新按索引替换，索引不可达 ⇒ 如实失败且不动已装二进制。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeZizvideoMirror 是"镜像站"：应用级索引 + 索引点名的 arm64 产物。
type fakeZizvideoMirror struct {
	srv      *httptest.Server
	version  string
	asset    string
	payload  []byte
	indexSHA string // 索引里写的 sha256（改成坏值即可测"sha 不匹配必须失败"）
}

func newFakeZizvideoMirror(t *testing.T, version string, payload []byte) *fakeZizvideoMirror {
	t.Helper()
	sum := sha256.Sum256(payload)
	f := &fakeZizvideoMirror{
		version:  version,
		asset:    "zizvideo_" + version + "_darwin_arm64",
		payload:  payload,
		indexSHA: hex.EncodeToString(sum[:]),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps/zizvideo/manifest.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"app": "zizvideo", "latest": f.version, "generated_at": "2026-09-22T00:00:00+0800",
				"assets": []map[string]any{{
					"name": f.asset, "version": f.version, "arch": "arm64",
					"sha256": f.indexSHA, "size": len(f.payload),
				}},
			})
		case "/apps/zizvideo/" + f.version + "/" + f.asset:
			_, _ = w.Write(f.payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// useRealZizvideoFetch 暂时用默认取件实现（真跑 curl 到本地假镜像），返回恢复函数。
func useRealZizvideoFetch() func() {
	prev := zizvideoFetch
	zizvideoFetch = defaultZizvideoFetch
	return func() { zizvideoFetch = prev }
}

// 索引说了算：面板常量仍是 0.1.1-mvp，索引写 9.9.9-test ⇒ 装的是 9.9.9-test，并按索引 sha 校验。
func TestZizvideoInstallFollowsMirrorIndex(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	if err := os.Remove(env.source); err != nil {
		t.Fatal(err)
	}
	if ZizvideoVersion == "9.9.9-test" {
		t.Fatal("前置不成立：面板内置常量不该等于索引版本")
	}
	payload := []byte("#!/bin/sh\necho 'zizvideo 9.9.9-test'\n")
	f := newFakeZizvideoMirror(t, "9.9.9-test", payload)
	env.m.opt.MirrorBase = f.srv.URL
	zizvideoVersionFn = func(*Manager, context.Context, string) (string, error) {
		return "zizvideo 9.9.9-test", nil
	}
	defer useRealZizvideoFetch()()

	result := &InstallResult{}
	if err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID}, result); err != nil {
		t.Fatalf("索引可用时应装成功（版本来自索引 9.9.9-test）：%v", err)
	}
	got, err := os.ReadFile(ZizvideoBin())
	if err != nil {
		t.Fatalf("安装位二进制没落盘：%v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("安装位内容不是索引点名的产物：%q", got)
	}
	t.Logf("面板内置常量=%s，索引 latest=%s；安装位内容=%q", ZizvideoVersion, f.version, string(got))
	if joined := strings.Join(result.Steps, " | "); !strings.Contains(joined, "9.9.9-test") {
		t.Errorf("任务步骤要如实写出索引里的版本 9.9.9-test：%v", result.Steps)
	}
}

// 索引里的 sha256 被改坏 ⇒ 必须失败，且一个字节都不落安装位。
func TestZizvideoInstallRejectsIndexSHAMismatch(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.stubHealth("ok", 200, nil)
	if err := os.Remove(env.source); err != nil {
		t.Fatal(err)
	}
	f := newFakeZizvideoMirror(t, "9.9.9-test", []byte("#!/bin/sh\necho tampered\n"))
	f.indexSHA = strings.Repeat("a", 64)
	env.m.opt.MirrorBase = f.srv.URL
	zizvideoVersionFn = func(*Manager, context.Context, string) (string, error) {
		return "zizvideo 9.9.9-test", nil
	}
	defer useRealZizvideoFetch()()

	err := env.m.InstallZizvideo(context.Background(), App{ID: ZizvideoAppID}, &InstallResult{})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("索引 sha 对不上必须拒绝安装，实际：%v", err)
	}
	if fileExecutable(ZizvideoBin()) {
		t.Error("坏包不该落到安装位")
	}
}

// 刷新按索引：已装 0.1.1-mvp、索引说 9.9.9-test ⇒ 替换并重启。
func TestZizvideoRefreshReplacesByMirrorIndex(t *testing.T) {
	env := newZizvideoTestEnv(t)
	installed := ZizvideoBin()
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("#!/bin/sh\necho 'zizvideo "+ZizvideoVersion+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SystemDaemonPlistPath(ZizvideoLabel), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := newFakeZizvideoMirror(t, "9.9.9-test", []byte("#!/bin/sh\necho 'zizvideo 9.9.9-test'\n"))
	env.m.opt.MirrorBase = f.srv.URL

	prevSelf, prevModuleFetch := moduleSelfCheckRun, zizvideoModuleFetch
	t.Cleanup(func() { moduleSelfCheckRun, zizvideoModuleFetch = prevSelf, prevModuleFetch })
	zizvideoModuleFetch = defaultZizvideoModuleFetch
	moduleSelfCheckRun = func(_ *Manager, _ context.Context, bin string, _ ...string) (string, error) {
		if strings.Contains(readFileOrFail(t, bin), "9.9.9-test") {
			return "zizvideo 9.9.9-test", nil
		}
		return "zizvideo " + ZizvideoVersion, nil
	}
	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}

	results := env.m.RefreshInstalledModules(context.Background(), t.TempDir())
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleRefreshed {
		t.Fatalf("索引说 9.9.9-test 时应替换并重启，实际 %s（%s）", r.Status, r.Reason)
	}
	if got := readFileOrFail(t, installed); !strings.Contains(got, "9.9.9-test") {
		t.Errorf("安装位没有被替换成索引点名的版本：%q", got)
	} else {
		t.Logf("刷新后安装位内容=%q（索引 latest=%s）", got, f.version)
	}
	if !launched {
		t.Error("替换通过后应重启守护进程")
	}
}

// 索引不可达 ⇒ 如实失败，已装二进制 sha 前后一致（未被改动），也不重启。
func TestZizvideoRefreshFailsWhenIndexUnreachable(t *testing.T) {
	env := newZizvideoTestEnv(t)
	installed := ZizvideoBin()
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("#!/bin/sh\necho 'zizvideo "+ZizvideoVersion+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SystemDaemonPlistPath(ZizvideoLabel), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("索引不可达前安装位 sha256=%s", before)

	prevSelf := moduleSelfCheckRun
	t.Cleanup(func() { moduleSelfCheckRun = prevSelf })
	moduleSelfCheckRun = func(*Manager, context.Context, string, ...string) (string, error) {
		return "zizvideo " + ZizvideoVersion, nil
	}
	launched := false
	zizvideoLaunch = func(*Manager, context.Context, string, string) error {
		launched = true
		return nil
	}
	// 127.0.0.1:1 必然连不上：索引不可用，且本地没有携带位。
	env.m.opt.MirrorBase = "http://127.0.0.1:1"

	results := env.m.RefreshInstalledModules(context.Background(), t.TempDir())
	r := moduleRefreshResultFor(t, results, ZizvideoAppID)
	if r.Status != ModuleFailed {
		t.Fatalf("索引不可达时必须如实失败，实际 %s（%s）", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "镜像索引不可用") || !strings.Contains(r.Reason, "未被改动") {
		t.Errorf("失败原因要说清是索引不可用且安装位未动，实际 %q", r.Reason)
	}
	if launched {
		t.Error("索引不可达时不该重启守护进程")
	}
	after, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("索引不可达时已装二进制被改动了：before=%s after=%s", before, after)
	}
	t.Logf("索引不可达后安装位 sha256=%s（与前后一致 ⇒ 未被改动）", after)
}
