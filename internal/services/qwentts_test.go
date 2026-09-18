package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHFEndpointUsableRequiresWorkingAPI 锁住 2026-09-18 用户真机事故：
//
// NAS 的 `/hf/` 首页返回 200（一个落地页），但 `/hf/api/models/<repo>` 是 **502** ——
// 而 `hf download` 必须先用 API 列文件。旧探测只看首页 200，于是把 `HF_ENDPOINT`
// 指向 NAS，用户看到的是连续三次
//
//	Error: Local entry not found. [Errno 60] Operation timed out
//
// 排查方向被完全带偏（模型文件其实都缓存着）。
//
// 新判据：**API 与文件两条都要通**才算可用。
func TestHFEndpointUsableRequiresWorkingAPI(t *testing.T) {
	m, _ := sandboxManager(t)
	if len(QwenModels) == 0 {
		t.Skip("目录里没有模型")
	}
	repo := QwenModels[0].Name

	// ① 只有首页 200，API 502（NAS 当年的真实状态）→ 不可用
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte("<html>hf mirror</html>"))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("502 bad gateway: rename ...: file exists"))
	}))
	defer bad.Close()
	if m.hfEndpointUsable(context.Background(), bad.URL) {
		t.Error("API 502 时不该判为可用（首页 200 不算数）")
	}

	// ② API 200 但返回错误页（不是 JSON）→ 不可用
	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>login</html>"))
	}))
	defer notJSON.Close()
	if m.hfEndpointUsable(context.Background(), notJSON.URL) {
		t.Error("API 返回非 JSON 时不该判为可用")
	}

	// ③ API 与文件都通 → 可用
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/models/"+repo):
			_, _ = w.Write([]byte(`{"id":"` + repo + `","siblings":[]}`))
		case strings.HasSuffix(r.URL.Path, "/resolve/main/config.json"):
			_, _ = w.Write([]byte(`{"model_type":"qwen3_tts"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer good.Close()
	if !m.hfEndpointUsable(context.Background(), good.URL) {
		t.Error("API 与文件都通时应判为可用（这样才会优先走自建镜像）")
	}
}

// TestAsUserEnvArgsInjectsEnvThroughEnv 锁住 2026-09-18 用户真机事故的根因：
//
// `sudo` 默认 env_reset —— 把 HF_ENDPOINT 塞在 sudo **进程**的 Env 里，子进程收不到，
// `hf download` 于是去连 huggingface.co（国内不可达），用户看到的是"配了镜像却零速度 /
// Errno 60 timed out"。形状必须是 `sudo -n -u <user> /usr/bin/env KEY=VAL… <cmd>`。
func TestAsUserEnvArgsInjectsEnvThroughEnv(t *testing.T) {
	got := asUserEnvArgs("zizdog", "/Users/zizdog",
		[]string{"HF_ENDPOINT=https://mirror.example/hf", "HF_HUB_DISABLE_XET=1"},
		"/Users/zizdog/tts/qwen3/.venv/bin/hf", "download", "some/repo")
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"-n -u zizdog /usr/bin/env",
		"HOME=/Users/zizdog",
		"HF_ENDPOINT=https://mirror.example/hf",
		"HF_HUB_DISABLE_XET=1",
		"download some/repo",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("构造出的参数缺少 %q：%v", want, got)
		}
	}
	// 环境变量必须排在命令之前（env 的规则：先赋值、再执行命令）
	if iEnv, iCmd := strings.Index(joined, "HF_ENDPOINT="), strings.Index(joined, "/hf download"); iEnv < 0 || iCmd < 0 || iEnv > iCmd {
		t.Errorf("环境变量必须出现在命令之前：%v", got)
	}
}
