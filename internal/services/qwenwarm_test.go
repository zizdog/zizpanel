package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/store"
)

// qwenFake 是一个假的上游 Qwen 服务，用来在单测里复刻 mlx-audio 的真实行为：
//
//	GET    /v1/models  → 只返回**已驻留内存**的模型（不是磁盘上有的）
//	POST   /v1/models  → 加载模型（幂等，已加载不重复计次）
//	DELETE /v1/models  → 卸载模型
//
// 为什么必须复刻这个语义：本次改动的全部依据就是"mlx 的 /v1/models 反映驻留状态"。
// 如果假服务笼统返回所有模型，测试就会在错误的前提下"通过"。
type qwenFake struct {
	mu     sync.Mutex
	loaded map[string]bool
	loads  map[string]int // 每个模型被真正加载了几次
	unload int
}

func newQwenFake(names ...string) *qwenFake {
	f := &qwenFake{loaded: map[string]bool{}, loads: map[string]int{}}
	// 初始一个都不驻留：模拟服务刚重启完的状态。
	for _, n := range names {
		f.loads[n] = 0
	}
	return f
}

func (f *qwenFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.URL.Query().Get("model_name")
		switch r.Method {
		case http.MethodGet:
			data := []map[string]any{}
			for n, on := range f.loaded {
				if on {
					data = append(data, map[string]any{"id": n, "object": "model"})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case http.MethodPost:
			if name == "" {
				http.Error(w, "model_name required", http.StatusBadRequest)
				return
			}
			if !f.loaded[name] {
				f.loads[name]++
			}
			f.loaded[name] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success"})
		case http.MethodDelete:
			delete(f.loaded, name)
			f.unload++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (f *qwenFake) loadCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads[name]
}

func (f *qwenFake) isLoaded(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded[name]
}

// fakeQwenManager 起一个假上游，并把 Manager 指过去。
// 不这么做的话测试会打到本机真实运行的 8880 —— 那里有另一个项目
// （TtsVoice）的 Qwen，测出来的不是被测代码的行为。
func fakeQwenManager(t *testing.T, names ...string) (*Manager, *qwenFake) {
	t.Helper()
	f := newQwenFake(names...)
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析假服务地址失败: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("解析端口失败: %v", err)
	}
	m := NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
	m.qwenPortOverride = port
	return m, f
}

// stubModelWeights 造出"权重已下载"的假象。
//
// modelDownloaded 会顺着 snapshots 目录找体积 >100MB 的 model.safetensors，
// 所以这里必须写够大小，否则走不到加载那一步。
func stubModelWeights(t *testing.T, home, model string) {
	t.Helper()
	dir := filepath.Join(home, ".cache", "huggingface", "hub",
		"models--"+strings.ReplaceAll(model, "/", "--"), "snapshots", "rev1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建假模型目录失败: %v", err)
	}
	f, err := os.Create(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		t.Fatalf("创建假权重文件失败: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(101 << 20); err != nil {
		t.Fatalf("扩展假权重文件失败: %v", err)
	}
}

// TestQwenWarmResidentLoadsBothModels 是这次需求的核心断言：
// 服务重启后两个模型都变冷，守温必须把它们都补回驻留状态。
func TestQwenWarmResidentLoadsBothModels(t *testing.T) {
	m, fake := fakeQwenManager(t, QwenModels[0].Name, QwenModels[1].Name)
	for _, mdl := range QwenModels {
		stubModelWeights(t, m.opt.UserHome, mdl.Name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	warmed, failed := m.QwenWarmResident(ctx)
	if failed != 0 {
		t.Fatalf("不应有失败，failed=%d", failed)
	}
	if warmed != len(QwenModels) {
		t.Fatalf("应补载 %d 个模型，实际 %d", len(QwenModels), warmed)
	}
	for _, mdl := range QwenModels {
		if !fake.isLoaded(mdl.Name) {
			t.Errorf("模型 %s 没有被加载", mdl.Name)
		}
		if n := fake.loadCount(mdl.Name); n != 1 {
			t.Errorf("模型 %s 应恰好加载 1 次，实际 %d 次", mdl.Name, n)
		}
	}
}

// TestQwenWarmResidentIsNoopWhenResident 已驻留时不能再加载一遍。
//
// 这条锁住的是守温循环的代价：它每 2 分钟跑一次，如果每次都重新加载，
// 就会把 GPU 一直占着，网站的合成请求反而被挤在后面。
func TestQwenWarmResidentIsNoopWhenResident(t *testing.T) {
	m, fake := fakeQwenManager(t, QwenModels[0].Name, QwenModels[1].Name)
	for _, mdl := range QwenModels {
		stubModelWeights(t, m.opt.UserHome, mdl.Name)
		fake.mu.Lock()
		fake.loaded[mdl.Name] = true
		fake.mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	warmed, failed := m.QwenWarmResident(ctx)
	if warmed != 0 || failed != 0 {
		t.Fatalf("已全部驻留时应为空操作，实际 warmed=%d failed=%d", warmed, failed)
	}
	for _, mdl := range QwenModels {
		if n := fake.loadCount(mdl.Name); n != 0 {
			t.Errorf("模型 %s 被重复加载了 %d 次", mdl.Name, n)
		}
	}
}

// TestQwenWarmResidentSkipsUndownloaded 权重没下载的模型不能去加载。
// 否则每次守温都会拿到一个难懂的加载失败，把日志刷满。
func TestQwenWarmResidentSkipsUndownloaded(t *testing.T) {
	m, fake := fakeQwenManager(t, QwenModels[0].Name, QwenModels[1].Name)
	// 只给第一个模型造权重
	stubModelWeights(t, m.opt.UserHome, QwenModels[0].Name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	warmed, failed := m.QwenWarmResident(ctx)
	if warmed != 1 || failed != 0 {
		t.Fatalf("应只补载已下载的那一个，实际 warmed=%d failed=%d", warmed, failed)
	}
	if !fake.isLoaded(QwenModels[0].Name) {
		t.Errorf("已下载的模型 %s 没有被加载", QwenModels[0].Name)
	}
	if fake.isLoaded(QwenModels[1].Name) {
		t.Errorf("未下载的模型 %s 不该被加载", QwenModels[1].Name)
	}
}

// TestQwenWarmResidentQuietWhenServiceDown 服务没起来时不能报错、也不能刷日志。
// 常驻循环每轮都会遇到这种情况（Qwen 没装 / 正在重启）。
func TestQwenWarmResidentQuietWhenServiceDown(t *testing.T) {
	m := NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
	m.qwenPortOverride = 1 // 端口 1 上不会有服务

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	warmed, failed := m.QwenWarmResident(ctx)
	if warmed != 0 || failed != 0 {
		t.Fatalf("服务不可用时应静默返回，实际 warmed=%d failed=%d", warmed, failed)
	}
}

// TestQwenKeepWarmStopsOnContext 常驻循环必须能被 ctx 收掉，
// 否则面板退出时（以及升级重启时）会留下一个后台 goroutine。
//
// 这里给 Manager 配一个真实的空数据库：repo 为 nil 时 qwenServiceRunning
// 会提前返回，那样测到的只是一个"什么都没做"的空壳。
func TestQwenKeepWarmStopsOnContext(t *testing.T) {
	m, _ := fakeQwenManager(t, QwenModels[0].Name, QwenModels[1].Name)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时数据库失败: %v", err)
	}
	defer st.Close()
	m.repo = NewRepository(st)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.StartQwenKeepWarm(ctx)
		close(done)
	}()

	// 首轮检查是立即执行的，给它一点时间跑完再取消，
	// 这样测到的是"检查跑过之后仍能被取消"，而不是"还没开始就退了"。
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 StartQwenKeepWarm 没有退出")
	}
}
