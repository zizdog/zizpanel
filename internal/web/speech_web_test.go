package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  语音合成服务的接口契约测试
//
//  纪律：**不碰真实服务**（AGENTS 第三节）。这里
//    · 引擎是注入的假引擎 —— 绝不真的跑 /usr/bin/say（那会出声、还会依赖
//      这台机器上恰好有哪些音色）；
//    · 音色清单喂的是固定夹具（真的那份随系统语言变，写死真值会让测试
//      在别的机器上红）；
//    · 只走 httptest，不监听端口、不写用户目录（临时文件在 t.TempDir 里）。
//
//  锁的是"契约"：状态码、响应头、JSON 字段名、以及**错误如实**（空文本/未知
//  音色/不可用格式必须是 4xx 且说明原因，不能是 200 也不能是笼统的 500）。
// ============================================================================

const webSayVoices = `Albert              en_US    # Hello! My name is Albert.
Eddy (中文（中国大陆）)     zh_CN    # 你好！我叫Eddy。
Meijia              zh_TW    # 你好，我叫美佳。
Sinji               zh_HK    # 你好！我叫善怡。
Tingting            zh_CN    # 你好！我叫婷婷。
`

// webTestWAV 造一个最小的合法 PCM WAV（44 字节标准头）。
func webTestWAV(samples []int16) []byte {
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}
	out := make([]byte, 0, 44+len(data))
	le32 := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	le16 := func(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
	out = append(out, "RIFF"...)
	out = append(out, le32(uint32(36+len(data)))...)
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = append(out, le32(16)...)
	out = append(out, le16(1)...)
	out = append(out, le16(1)...)
	out = append(out, le32(22050)...)
	out = append(out, le32(22050*2)...)
	out = append(out, le16(2)...)
	out = append(out, le16(16)...)
	out = append(out, "data"...)
	out = append(out, le32(uint32(len(data)))...)
	return append(out, data...)
}

// fakeEngine 造一个"引擎可用"的假 SpeechEngine（say/afconvert/ffmpeg 都是
// 临时目录里真实存在且可执行的文件；命令执行全被替换）。
func fakeEngine(t *testing.T, withFfmpeg bool) *services.SpeechEngine {
	t.Helper()
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ff := ""
	if withFfmpeg {
		ff = mk("ffmpeg")
	}
	return &services.SpeechEngine{
		SayBin:       mk("say"),
		AfconvertBin: mk("afconvert"),
		FfmpegBin:    ff,
		TempRoot:     dir,
		MaxChars:     20000,
		ChunkChars:   20,
		Run: func(_ context.Context, _ time.Duration, bin string, args ...string) (string, error) {
			switch filepath.Base(bin) {
			case "say":
				if len(args) >= 2 && args[0] == "-v" && args[1] == "?" {
					return webSayVoices, nil
				}
				for i, a := range args {
					if a == "-o" && i+1 < len(args) {
						return "", os.WriteFile(args[i+1], []byte("FAKE-AIFF"), 0o644)
					}
				}
				return "", nil
			case "afconvert":
				out := args[len(args)-1]
				return "", os.WriteFile(out, webTestWAV([]int16{1, 2, 3, 4}), 0o644)
			case "ffmpeg":
				out := args[len(args)-1]
				return "", os.WriteFile(out, []byte("FAKE-MP3"), 0o644)
			}
			return "", nil
		},
	}
}

func newSpeechTestServer(t *testing.T, eng *services.SpeechEngine, syncMax int) *httptest.Server {
	t.Helper()
	if syncMax <= 0 {
		syncMax = 600
	}
	srv := NewSpeechServer(SpeechOptions{Listen: "127.0.0.1:0", Engine: eng, SyncMaxChars: syncMax})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestSpeechStaticAssets(t *testing.T) {
	ts := newSpeechTestServer(t, fakeEngine(t, true), 0)
	// 页面本身
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", res.StatusCode)
	}
	html := string(body)
	if !strings.Contains(html, "./app.js") || !strings.Contains(html, "./app.css") {
		t.Error("页面必须用**相对路径**引入资源（绝对路径在面板别名下会白屏）")
	}
	// 不许从外部加载任何东西（离线可用）：src/href 必须是相对路径
	for _, bad := range []string{`src="http`, `href="http`, `src="//`, `href="//`, "https://cdn"} {
		if strings.Contains(html, bad) {
			t.Errorf("页面里不许出现外部资源引用 %q（必须离线可用）", bad)
		}
	}
	for _, asset := range []string{"/app.js", "/app.css"} {
		r2, err := http.Get(ts.URL + asset)
		if err != nil {
			t.Fatal(err)
		}
		b2, _ := io.ReadAll(r2.Body)
		_ = r2.Body.Close()
		if r2.StatusCode != http.StatusOK || len(b2) == 0 {
			t.Errorf("GET %s 应 200 且非空，实际 %d / %d 字节", asset, r2.StatusCode, len(b2))
		}
	}
	r3, err := http.Get(ts.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	_ = r3.Body.Close()
	if r3.StatusCode != http.StatusNoContent {
		t.Errorf("favicon 应 204（避免日志刷 404），实际 %d", r3.StatusCode)
	}
}

func TestSpeechHealthzContract(t *testing.T) {
	ts := newSpeechTestServer(t, fakeEngine(t, true), 0)
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
		t.Fatalf("healthz 必须返回 JSON: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || h["ok"] != true {
		t.Fatalf("引擎可用时 healthz 应 200 + ok:true，实际 %d / %v", res.StatusCode, h)
	}
	if h["voices"].(float64) != 5 || h["chinese_voices"].(float64) != 4 {
		t.Errorf("音色计数不对：%v / %v", h["voices"], h["chinese_voices"])
	}
	if h["service"] != services.MacSpeechAppID || h["market_app_id"] != services.MacSpeechAppID {
		t.Errorf("healthz 应自我标识是哪个应用：%v", h)
	}
	formats, _ := h["formats"].([]any)
	if len(formats) != 4 {
		t.Errorf("应逐个报告 4 个格式的可用性，实际 %d", len(formats))
	}
	if h["sample_rate"].(float64) != 22050 {
		t.Errorf("采样率应如实报告 22050，实际 %v", h["sample_rate"])
	}
}

// TestSpeechHealthzIsHonestWhenEngineMissing 是最重要的一条：
// 引擎不在时必须 503 + ok:false + 理由 —— 绝不给一个"服务在跑"的绿灯。
func TestSpeechHealthzIsHonestWhenEngineMissing(t *testing.T) {
	eng := fakeEngine(t, true)
	eng.SayBin = filepath.Join(t.TempDir(), "definitely-not-here")
	ts := newSpeechTestServer(t, eng, 0)
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	_ = json.NewDecoder(res.Body).Decode(&h)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("引擎不可用时应 503，实际 %d", res.StatusCode)
	}
	if h["ok"] != false || strings.TrimSpace(h["reason"].(string)) == "" {
		t.Errorf("必须 ok:false 且写清理由：%v", h)
	}
}

func TestSpeechVoicesContract(t *testing.T) {
	ts := newSpeechTestServer(t, fakeEngine(t, true), 0)
	res, err := http.Get(ts.URL + "/v1/voices")
	if err != nil {
		t.Fatal(err)
	}
	var j struct {
		Object string `json:"object"`
		Data   []struct {
			Name    string `json:"name"`
			Lang    string `json:"lang"`
			Chinese bool   `json:"chinese"`
		} `json:"data"`
		Count        int    `json:"count"`
		ChineseCount int    `json:"chinese_count"`
		Source       string `json:"source"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		t.Fatalf("voices 必须返回 JSON: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || j.Object != "list" {
		t.Fatalf("voices 契约不对：%d / %q", res.StatusCode, j.Object)
	}
	if j.Count != 5 || j.ChineseCount != 4 || len(j.Data) != 5 {
		t.Errorf("音色计数不对：count=%d chinese=%d len=%d", j.Count, j.ChineseCount, len(j.Data))
	}
	if j.Source != "say -v '?'" {
		t.Errorf("必须写明音色来源：%q", j.Source)
	}
	found := false
	for _, v := range j.Data {
		if v.Name == "Tingting" && v.Lang == "zh_CN" && v.Chinese {
			found = true
		}
	}
	if !found {
		t.Error("中文音色必须带 chinese=true 与正确的语言代码（界面靠它排序与打标）")
	}
}

func TestSpeechRejectsBadRequests(t *testing.T) {
	eng := fakeEngine(t, false) // 没有 ffmpeg：mp3 必须被如实拒绝
	ts := newSpeechTestServer(t, eng, 0)
	cases := []struct {
		name string
		body string
		want int
		note string
	}{
		{"空文本", `{"input":"   "}`, 400, "空"},
		{"缺 input", `{}`, 400, "空"},
		{"非法 JSON", `{oops`, 400, "JSON"},
		{"非法格式", `{"input":"你好","format":"flac"}`, 400, "格式"},
		{"未知音色", `{"input":"你好","voice":"NoSuchVoice"}`, 400, "音色"},
		{"没有 ffmpeg 的 mp3", `{"input":"你好","format":"mp3"}`, 400, "ffmpeg"},
		{"speed 超范围", `{"input":"你好","speed":9}`, 400, "speed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("应 %d，实际 %d：%s", tc.want, res.StatusCode, raw)
			}
			var j struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &j); err != nil {
				t.Fatalf("错误体必须是 JSON（OpenAI SDK 要读 message）：%s", raw)
			}
			if !strings.Contains(j.Error.Message, tc.note) {
				t.Errorf("错误信息里应说明原因（含 %q）：%q", tc.note, j.Error.Message)
			}
			// 绝不能把失败当成功：响应体不许是音频
			if strings.HasPrefix(res.Header.Get("Content-Type"), "audio/") {
				t.Error("失败的请求不能返回 audio/*")
			}
		})
	}
}

func TestSpeechSyncPathReturnsAudio(t *testing.T) {
	ts := newSpeechTestServer(t, fakeEngine(t, true), 600)
	body := `{"input":"你好，世界","voice":"Tingting","format":"wav","speed":1.0}`
	res, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("短文本应同步 200，实际 %d：%s", res.StatusCode, raw)
	}
	if ct := res.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type 应是 audio/wav，实际 %q", ct)
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" {
		t.Fatalf("返回的不是 WAV（%d 字节）", len(raw))
	}
	// 实际用了什么必须可核对（speed 会被换算并夹取）
	if got := res.Header.Get("X-Zizpanel-Voice"); got != "Tingting" {
		t.Errorf("X-Zizpanel-Voice 应是实际音色，实际 %q", got)
	}
	if got := res.Header.Get("X-Zizpanel-Rate"); got != "175" {
		t.Errorf("X-Zizpanel-Rate 应是实际语速 175，实际 %q", got)
	}
	if got := res.Header.Get("X-Zizpanel-Format"); got != "wav" {
		t.Errorf("X-Zizpanel-Format 不对：%q", got)
	}
	if got := res.Header.Get("X-Zizpanel-Segments"); got != "1" {
		t.Errorf("短文本应只切 1 段，实际 %q", got)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "speech.wav") {
		t.Errorf("下载文件名应带扩展名：%q", cd)
	}
}

func TestSpeechLongTextGoesThroughTask(t *testing.T) {
	// 同步上限压到 10 字：让 100 字走任务路径（真机上阈值是 600）
	ts := newSpeechTestServer(t, fakeEngine(t, true), 10)
	long := strings.Repeat("这是一句用于测试任务路径的中文。", 8)
	reqBody, _ := json.Marshal(map[string]any{"input": long, "voice": "Tingting", "format": "wav"})
	res, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("超过同步上限应返回 202，实际 %d：%s", res.StatusCode, raw)
	}
	var j struct {
		OK   bool `json:"ok"`
		Data struct {
			TaskID   string `json:"task_id"`
			Sync     bool   `json:"sync"`
			Segments int    `json:"segments"`
			Chars    int    `json:"chars"`
			Stream   string `json:"stream_url"`
			Audio    string `json:"audio_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatalf("202 的响应体必须是 JSON：%s", raw)
	}
	if !j.OK || j.Data.TaskID == "" || j.Data.Sync {
		t.Fatalf("202 必须带 task_id 且 sync=false：%s", raw)
	}
	if j.Data.Segments < 2 {
		t.Errorf("长文本应切成多段（进度才有意义），实际 %d", j.Data.Segments)
	}
	if !strings.Contains(j.Data.Stream, j.Data.TaskID) || !strings.Contains(j.Data.Audio, j.Data.TaskID) {
		t.Errorf("必须给出可用的进度/音频地址：%s", raw)
	}

	// 进度流：契约是 SSE（meta/lines/done）
	sres, err := http.Get(ts.URL + j.Data.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if ct := sres.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("进度流必须是 SSE，实际 %q", ct)
	}
	_ = sres.Body.Close()

	// 等任务结束，再取音频
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		tres, err := http.Get(ts.URL + "/v1/tasks/" + j.Data.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		var tj struct {
			Data struct {
				Task struct {
					Status string `json:"status"`
					Error  string `json:"error"`
					Result any    `json:"result"`
				} `json:"task"`
				Done bool `json:"done"`
			} `json:"data"`
		}
		_ = json.NewDecoder(tres.Body).Decode(&tj)
		_ = tres.Body.Close()
		status = tj.Data.Task.Status
		if tj.Data.Done {
			if status != "succeeded" {
				t.Fatalf("任务应成功，实际 %q（%s）", status, tj.Data.Task.Error)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status == "" || status == "running" {
		t.Fatalf("任务没有在 10 秒内结束（status=%q）", status)
	}

	ares, err := http.Get(ts.URL + "/v1/tasks/" + j.Data.TaskID + "/audio")
	if err != nil {
		t.Fatal(err)
	}
	audio, _ := io.ReadAll(ares.Body)
	_ = ares.Body.Close()
	if ares.StatusCode != http.StatusOK {
		t.Fatalf("任务成功后取音频应 200，实际 %d：%s", ares.StatusCode, audio)
	}
	if len(audio) < 44 || string(audio[0:4]) != "RIFF" {
		t.Errorf("任务音频不是合法 WAV（%d 字节）", len(audio))
	}
	// 进度是真的：分段数应体现在响应头里（>1）
	if got := ares.Header.Get("X-Zizpanel-Segments"); got == "" || got == "1" {
		t.Errorf("长文本的音频应标明多段，实际 %q", got)
	}
}

func TestSpeechTaskEndpointsOnUnknownID(t *testing.T) {
	ts := newSpeechTestServer(t, fakeEngine(t, true), 0)
	for _, path := range []string{"/v1/tasks/nope", "/v1/tasks/nope/stream", "/v1/tasks/nope/audio"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if path == "/v1/tasks/nope/stream" {
			// SSE 端点在未知任务上也是 404（不能挂住一个永远没事件的连接）
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("%s 应 404，实际 %d", path, res.StatusCode)
			}
			continue
		}
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s 应 404，实际 %d", path, res.StatusCode)
		}
	}
	// 取消一个不存在的任务
	res, err := http.Post(ts.URL+"/v1/tasks/nope/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("取消不存在的任务应 404，实际 %d", res.StatusCode)
	}
}

// TestSpeechTooLongTextIsRejectedBeforeRunning 锁住"超长要当场拒绝"：
// 不许静默截断（用户会以为整段都合成了），也不许开一个注定失败的任务。
func TestSpeechTooLongTextIsRejected(t *testing.T) {
	eng := fakeEngine(t, true)
	eng.MaxChars = 50
	ts := newSpeechTestServer(t, eng, 0)
	reqBody, _ := json.Marshal(map[string]any{"input": strings.Repeat("啊", 51)})
	res, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("超长文本应 400，实际 %d：%s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "上限") {
		t.Errorf("错误信息要写清上限：%s", raw)
	}
}

// TestSpeechBodyLimit：请求体有一个硬上限，超了要 413 而不是 OOM 或静默截断。
func TestSpeechBodyLimit(t *testing.T) {
	ts := newSpeechTestServer(t, fakeEngine(t, true), 0)
	big := strings.Repeat("a", speechMaxBodyBytes+1024)
	res, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("超大请求体应 413，实际 %d", res.StatusCode)
	}
}

// TestSpeechAppProxyBlockIncludesSpeechAlias 锁住「别名 + 反代」这一侧的接线：
// 目录条目的 UI.Slug 必须真的被 appproxy 挑出来，并且生成的 nginx location
// 指向面板自己（由面板统一改写路径），端口是声明的 8891。
//
// 为什么值得一条测试：别名不是"配一下就有"的 —— appproxy.Slugs() 会跳过
// UI 为空、SelfConf、或 WebPort<=0 的条目，任何一条写错，卡片上的「打开」
// 就指向一个不存在的 location（表现是 404 或打到面板 SPA），而**单元测试之外
// 很难发现**（真机上要点开才知道）。
func TestSpeechAppProxyBlockIncludesSpeechAlias(t *testing.T) {
	entries := appProxyEntries()
	found := false
	for _, e := range entries {
		if e.Slug != services.MacSpeechSlug {
			continue
		}
		found = true
		if e.Port != services.MacSpeechPort {
			t.Errorf("别名 %s 的端口应是 %d（目录 Port），实际 %d",
				e.Slug, services.MacSpeechPort, e.Port)
		}
	}
	if !found {
		t.Fatalf("appProxyEntries() 里没有 %s —— nginx 的 /%s/ 入口不会被生成",
			services.MacSpeechSlug, services.MacSpeechSlug)
	}
	block := appProxyBlock(entries, "127.0.0.1:8443")
	for _, want := range []string{
		"location = /speech {",
		"location ^~ /speech/ {",
		"proxy_pass https://127.0.0.1:8443;",
		"127.0.0.1:8891",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("生成的 nginx 块里缺少 %q：\n%s", want, block)
		}
	}
	// 把整块打进 -v 的输出里，便于人工核对（也是真机证据的一部分）。
	t.Logf("生成的 nginx 反代块：\n%s", block)
}

// TestSpeechMarketCardInstalledContract 锁住这个条目在**市场卡片**上的两条契约：
//
//  1. 只注册了服务（面板记录 + 系统级 plist）就必须显示「已安装」——
//     这正是本项目反复复发的那一族缺陷（「装了却显示未装 / 不在已安装里」）。
//     这个应用**没有** BrewFormula、也没有 RuntimePath（引擎是系统自带的
//     /usr/bin/say，不该由条目声明成"安装产物"），所以它的"已安装"证据
//     **只能**是服务记录/plist —— 少一条判据，用户装完就会看到「安装」按钮。
//  2. 已安装 ⇒ 必须给得出卸载路径（kind=installer + 非空步骤），
//     且步骤里要逐字写清"系统自带的 say 不会被删"（用户按确认前看得见）。
func TestSpeechMarketCardInstalledContract(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	it := marketItem(t, ts, cookies, services.MacSpeechAppID)
	if it["installed"] != false {
		t.Error("沙箱里什么都没装，不能显示已安装（谎报「已安装」和谎报「未安装」一样糟）")
	}
	if ui, _ := it["ui"].(map[string]any); asString(ui["slug"]) != services.MacSpeechSlug {
		t.Errorf("市场条目必须带界面别名 ui.slug=%s（否则卡片没有「打开」入口）：%v",
			services.MacSpeechSlug, it["ui"])
	}
	if got := asString(it["proxy_url"]); !strings.Contains(got, "/"+services.MacSpeechSlug+"/") {
		t.Errorf("proxy_url 应指向面板别名 /%s/，实际 %q", services.MacSpeechSlug, got)
	}
	if got := asString(it["port_url"]); !strings.Contains(got, strconv.Itoa(services.MacSpeechPort)) {
		t.Errorf("port_url 应指向直连端口 %d，实际 %q", services.MacSpeechPort, got)
	}

	// 模拟"面板装好了"的最小证据：服务真的注册进了 launchd（沙箱 LaunchDaemons
	// 目录里出现它的 plist）—— 面板安装器就是这么装的。
	// ⚠️ 2026-09-20 起**只有面板服务记录不算已安装**（真机 mysql84 的 keg/plist
	// 都卸了、记录还在，卡片却显示已安装）：它的运行体证据只能是这条 plist。
	if err := os.WriteFile(filepath.Join(launchDaemonsDir, services.MacSpeechLabel+".plist"),
		[]byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := services.NewRepository(srv.Store)
	if err := repo.Create(t.Context(), &services.Service{
		Name: "com-zizdog-macosspeech", DisplayName: "macOS 语音合成（say）",
		Kind: services.KindNative, LaunchLabel: services.MacSpeechLabel, Category: services.CategoryAI,
	}); err != nil {
		t.Fatalf("登记服务记录失败: %v", err)
	}

	it = marketItem(t, ts, cookies, services.MacSpeechAppID)
	if it["installed"] != true {
		t.Fatal("服务真的在 launchd 里（plist 在）时，卡片必须显示「已安装」——" +
			"否则用户装完看到的是「安装」按钮（本项目复发过多次的那一族）")
	}
	plan, _ := it["uninstall"].(map[string]any)
	if asString(plan["kind"]) != "installer" {
		t.Fatalf("已安装就必须给 installer 卸载路径，实际 %q", asString(plan["kind"]))
	}
	steps, _ := plan["steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("卸载计划必须有步骤（确认框里不能一片空白）")
	}
	joined := ""
	for _, s := range steps {
		joined += asString(s) + "\n"
	}
	if !strings.Contains(joined, services.MacSpeechLabel) {
		t.Errorf("卸载步骤里要逐字写出会删除的 launchd 服务 %s：\n%s", services.MacSpeechLabel, joined)
	}
	if !strings.Contains(joined, "/usr/bin/say") {
		t.Errorf("卸载步骤里必须写清「系统自带的 /usr/bin/say 不会被删除」（那是 macOS 的一部分）：\n%s", joined)
	}
}
