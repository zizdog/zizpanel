package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  Ollama 模型镜像的契约
//
//  单测全程走 httptest / 注入点，不碰真实 registry.ollama.ai、不碰真实镜像站。
//  真实的端到端验证（镜像站 → ollama list → ollama run）在交付说明里单独记录。
// ============================================================================

// makeOllamaTestSpec 造一个只有几十字节 blob 的测试模型。
// 返回 spec 与 [manifest, config, layer1, layer2...] 的字节。
func makeOllamaTestSpec(upstream string) (OllamaModelSpec, [][]byte) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	blobs := [][]byte{
		[]byte("config-blob-bytes"),
		[]byte("layer-one-bytes"),
		[]byte("layer-two-bytes"),
	}
	spec := OllamaModelSpec{
		Name: "testmodel", Tag: "1b", Namespace: "library",
		ManifestSHA256: sha256Hex(manifest),
		upstreamBase:   upstream,
		Config:         OllamaBlob{Digest: sha256Hex(blobs[0]), Size: int64(len(blobs[0]))},
		Layers: []OllamaBlob{
			{Digest: sha256Hex(blobs[1]), Size: int64(len(blobs[1]))},
			{Digest: sha256Hex(blobs[2]), Size: int64(len(blobs[2]))},
		},
	}
	return spec, append([][]byte{manifest}, blobs...)
}

// registerOllamaTestModel 临时登记测试模型，返回清理函数。
func registerOllamaTestModel(t *testing.T, spec OllamaModelSpec) {
	t.Helper()
	key := spec.Ref()
	prev, had := ollamaModels[key]
	ollamaModels[key] = spec
	t.Cleanup(func() {
		if had {
			ollamaModels[key] = prev
		} else {
			delete(ollamaModels, key)
		}
	})
}

// serveOllamaModel 起一个本地 registry：mirrorPrefix 为空时按公网布局，
// 否则按镜像站布局（/models/ollama/...）。返回可能被替换 blob 的服务。
func serveOllamaModel(t *testing.T, spec OllamaModelSpec, payloads [][]byte, tamperLayer bool) *httptest.Server {
	t.Helper()
	manifest, blobs := payloads[0], payloads[1:]
	mux := http.NewServeMux()
	write := func(name string, body []byte) {
		mux.HandleFunc(name, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) })
	}
	// 公网布局：/v2/<ns>/<name>/{manifests/<tag>,blobs/sha256:<hex>}
	write("/v2/"+spec.Namespace+"/"+spec.Name+"/manifests/"+spec.Tag, manifest)
	for i, b := range blobs {
		body := b
		if tamperLayer && i == len(blobs)-1 {
			body = []byte("tampered blob content")
		}
		write("/v2/"+spec.Namespace+"/"+spec.Name+"/blobs/sha256:"+spec.blobs()[i].Digest, body)
	}
	// 镜像站布局：/models/ollama/{manifests/registry.ollama.ai/...,blobs/sha256-...}
	write("/models/ollama/manifests/"+ollamaRegistryHost+"/"+spec.Namespace+"/"+spec.Name+"/"+spec.Tag, manifest)
	for i, b := range blobs {
		body := b
		if tamperLayer && i == len(blobs)-1 {
			body = []byte("tampered blob content")
		}
		write("/models/ollama/blobs/"+ollamaBlobSeparator+spec.blobs()[i].Digest, body)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", path, err)
	}
	return string(b)
}

// TestOllamaModelRegistryPinned 锁住写死的 qwen2.5:7b 规格。
func TestOllamaModelRegistryPinned(t *testing.T) {
	spec, ok := OllamaModelFor("qwen2.5:7b")
	if !ok {
		t.Fatal("注册表里应有 qwen2.5:7b（应用市场的 ollama 提示就是它）")
	}
	if spec.Namespace != "library" {
		t.Errorf("namespace 应为 library，实际 %q", spec.Namespace)
	}
	if len(spec.ManifestSHA256) != 64 {
		t.Errorf("清单 sha256 必须是 64 位十六进制，实际 %q", spec.ManifestSHA256)
	}
	blobs := spec.blobs()
	if len(blobs) != 5 {
		t.Fatalf("qwen2.5:7b 应有 config + 4 层，实际 %d", len(blobs))
	}
	for _, b := range blobs {
		if len(b.Digest) != 64 || b.Size <= 0 {
			t.Errorf("blob 规格不完整：%+v", b)
		}
	}
	// 4.68 GB 的模型 blob 必须在（写错会让"镜像看起来完整、实际拉不动"）。
	var biggest int64
	for _, b := range blobs {
		if b.Size > biggest {
			biggest = b.Size
		}
	}
	if biggest != 4683073952 {
		t.Errorf("最大 blob 大小应为 4683073952，实际 %d", biggest)
	}
	if spec.upstreamManifestURL() != "https://registry.ollama.ai/v2/library/qwen2.5/manifests/7b" {
		t.Errorf("公网清单地址不对：%s", spec.upstreamManifestURL())
	}
}

// TestOllamaModelSourcesMirrorFirst：镜像站探得通 → 镜像排第一，blob 走 sha256- 布局。
func TestOllamaModelSourcesMirrorFirst(t *testing.T) {
	spec, _ := makeOllamaTestSpec("")
	m := &Manager{opt: Options{MirrorBase: "https://mirror.example.com:8888"}}
	m.mirrorFileProbeOverride = func(_ context.Context, url string) (int64, error) {
		if !strings.HasSuffix(url, "/models/ollama/manifests/registry.ollama.ai/library/testmodel/1b") {
			t.Errorf("探测地址不符合镜像布局：%s", url)
		}
		return 100, nil
	}
	var logs []string
	srcs := m.ollamaModelSources(context.Background(), spec, func(s string) { logs = append(logs, s) })
	if len(srcs) != 2 {
		t.Fatalf("镜像可用时应给出镜像 + 公网两个候选，实际 %d", len(srcs))
	}
	if srcs[0].label != "镜像站" {
		t.Errorf("第一位应是镜像站，实际 %q", srcs[0].label)
	}
	u := srcs[0].blobURL(spec.Config.Digest)
	want := "https://mirror.example.com:8888/models/ollama/blobs/sha256-" + spec.Config.Digest
	if u != want {
		t.Errorf("镜像 blob 地址不对：\n got %s\nwant %s", u, want)
	}
	if srcs[1].label != "公网 registry.ollama.ai" {
		t.Errorf("回落候选应是公网 registry，实际 %q", srcs[1].label)
	}
}

// TestOllamaModelSourcesFallback：镜像站探不通 → 直接公网并如实写明原因。
func TestOllamaModelSourcesFallback(t *testing.T) {
	spec, _ := makeOllamaTestSpec("")
	m := &Manager{opt: Options{MirrorBase: "https://mirror.example.com:8888"}}
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) {
		return -1, os.ErrNotExist
	}
	var logs []string
	srcs := m.ollamaModelSources(context.Background(), spec, func(s string) { logs = append(logs, s) })
	if len(srcs) != 1 || srcs[0].label != "公网 registry.ollama.ai" {
		t.Fatalf("镜像不可达时应只给公网候选，实际 %+v", srcs)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "回落到公网 registry") {
		t.Errorf("要如实写明回落原因，实际：%q", logs)
	}
}

// TestEnsureOllamaModelFromMirror：完整落盘 + 校验（真实文件系统，沙箱目录）。
func TestEnsureOllamaModelFromMirror(t *testing.T) {
	spec, payloads := makeOllamaTestSpec("")
	registerOllamaTestModel(t, spec)
	mirror := serveOllamaModel(t, spec, payloads, false)
	spec.upstreamBase = mirror.URL // 不会用到，镜像可用时不该回落
	registerOllamaTestModel(t, spec)

	m := &Manager{opt: Options{MirrorBase: mirror.URL}}
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) { return 100, nil }
	dir := t.TempDir()
	var logs []string
	label, err := m.EnsureOllamaModel(context.Background(), spec.Ref(), dir, func(s string) { logs = append(logs, s) })
	if err != nil {
		t.Fatalf("从镜像拉取应成功：%v", err)
	}
	if label != "镜像站" {
		t.Errorf("来源应为镜像站，实际 %q", label)
	}
	// 清单与每个 blob 都要真实存在且内容正确。
	if got := readAll(t, filepath.Join(dir, spec.manifestRelPath())); got != string(payloads[0]) {
		t.Errorf("清单内容不对：%q", got)
	}
	for i, b := range spec.blobs() {
		p := filepath.Join(dir, "blobs", ollamaBlobSeparator+b.Digest)
		if got := readAll(t, p); got != string(payloads[i+1]) {
			t.Errorf("blob %s 内容不对：%q", b.Digest[:12], got)
		}
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "清单 sha256 校验通过") || !strings.Contains(joined, "校验通过") {
		t.Errorf("日志要写出校验结果，实际：%q", joined)
	}
}

// TestEnsureOllamaModelFallsBackOnBadBlob：镜像上的 blob 哈希不符 → 删掉并回落公网，
// 最终产物必须来自公网且校验通过。
func TestEnsureOllamaModelFallsBackOnBadBlob(t *testing.T) {
	spec, payloads := makeOllamaTestSpec("")
	mirror := serveOllamaModel(t, spec, payloads, true) // 镜像最后一个 blob 被篡改
	upstream := serveOllamaModel(t, spec, payloads, false)
	spec.upstreamBase = upstream.URL
	registerOllamaTestModel(t, spec)

	m := &Manager{opt: Options{MirrorBase: mirror.URL}}
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) { return 100, nil }
	dir := t.TempDir()
	var logs []string
	label, err := m.EnsureOllamaModel(context.Background(), spec.Ref(), dir, func(s string) { logs = append(logs, s) })
	if err != nil {
		t.Fatalf("镜像坏 blob 时应能回落公网：%v", err)
	}
	if !strings.Contains(label, "回落") {
		t.Errorf("来源标签应标明回落，实际 %q", label)
	}
	for i, b := range spec.blobs() {
		p := filepath.Join(dir, "blobs", ollamaBlobSeparator+b.Digest)
		if got := readAll(t, p); got != string(payloads[i+1]) {
			t.Errorf("blob %s 内容不对（可能留下了坏文件）：%q", b.Digest[:12], got)
		}
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "sha256 不符") {
		t.Errorf("要如实写出镜像 blob 校验失败，实际：%q", joined)
	}
}

// TestEnsureOllamaModelIsIdempotent：第二次调用应跳过已存在且校验通过的 blob。
func TestEnsureOllamaModelIsIdempotent(t *testing.T) {
	spec, payloads := makeOllamaTestSpec("")
	registerOllamaTestModel(t, spec)
	mirror := serveOllamaModel(t, spec, payloads, false)
	m := &Manager{opt: Options{MirrorBase: mirror.URL}}
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) { return 100, nil }
	dir := t.TempDir()
	if _, err := m.EnsureOllamaModel(context.Background(), spec.Ref(), dir, nil); err != nil {
		t.Fatalf("第一次应成功：%v", err)
	}
	mirror.Close() // 关掉服务端：第二次若还想联网就会失败
	var logs []string
	if _, err := m.EnsureOllamaModel(context.Background(), spec.Ref(), dir,
		func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatalf("第二次应完全走本地、不再联网：%v", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "已存在且校验通过") {
		t.Errorf("应写出跳过了哪些已存在文件，实际：%q", logs)
	}
}

// TestEnsureOllamaModelUnknownRef：没登记固定版本的模型必须明确报错。
func TestEnsureOllamaModelUnknownRef(t *testing.T) {
	m := &Manager{}
	if _, err := m.EnsureOllamaModel(context.Background(), "not-registered:latest", t.TempDir(), nil); err == nil {
		t.Fatal("未登记的模型应显式报错（不追 latest）")
	}
}
