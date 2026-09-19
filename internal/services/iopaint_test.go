package services

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  IOPaint 权重下载 / 服务就绪等待的测试
//
//  这一组锁的是两个**真机上出过事**的约束（2026-09-16 Mac mini）：
//
//   1. LaMa 权重来自 **GitHub release**（不是 HuggingFace），所以必须
//      "镜像站静态镜像优先 + 探通才算数 + MD5 校验"，而且任务日志要**如实**
//      写出这次到底走了哪个来源 —— 不能再出现"日志说走镜像站、进程在连 GitHub"。
//
//   2. 模型下载与服务就绪**都必须有超时**，超时后**如实失败**：
//      既不许任务永远停在 running（用户看着一个假进度），
//      也不许超时后还报"任务完成 ✅"（那台机器上真的这么报了）。
//
//  全部离线：任何网络动作都通过 Manager 的 override 注入 ——
//  单测不许碰真实服务/真实镜像站（见 AGENTS.md 第三节）。
// ============================================================================

// newIOPaintTestManager 造一个完全离线的 Manager（UserHome 必然是临时目录）。
func newIOPaintTestManager(t *testing.T, mirror string) *Manager {
	t.Helper()
	return NewManager(nil, Options{
		UserHome:   t.TempDir(),
		UserName:   "zizdog",
		MirrorBase: mirror,
	})
}

func md5Hex(t *testing.T, b []byte) string {
	t.Helper()
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func stepsContain(res *InstallResult, want string) bool {
	for _, s := range res.Steps {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func iopaintTestSources() []weightSource {
	return []weightSource{
		{URL: "https://mirror.example:8888/models/iopaint/big-lama.pt", Label: "镜像站"},
		{URL: iopaintWeightURL, Label: "GitHub release（Sanster/models）"},
	}
}

// ---------------------------------------------------------------------------
//  下载本体：停滞 / 截断 / 成功
// ---------------------------------------------------------------------------

// 真机痛点：torch.hub 的下载没有超时，网络一断就无限期卡住。
// 这里锁死"连上了但不再来数据 → 必须失败"，而不是永远等下去。
func TestFetchToFileStallFailsInsteadOfHanging(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("12345"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // 再也不发数据：模拟"连上了但一直不动"
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	dest := filepath.Join(t.TempDir(), "big-lama.pt")
	start := time.Now()
	err := fetchToFile(context.Background(), srv.Client(), srv.URL, dest, 200*time.Millisecond, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("下载停滞时必须返回错误，实际返回 nil —— 这正是“卡住不报错”的缺陷")
	}
	if !strings.Contains(err.Error(), "停滞") {
		t.Fatalf("错误信息必须说明是“下载停滞”（否则用户不知道该查什么），实际：%v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("停滞看门狗没有及时生效，耗时 %v", elapsed)
	}
	if _, serr := os.Stat(dest); serr == nil {
		t.Fatal("失败的下载绝不能留下目标文件（否则下次会被当成『已经下好』）")
	}
	if _, serr := os.Stat(dest + ".part"); serr == nil {
		t.Fatal("失败的下载必须清掉 .part 临时文件")
	}
}

// 上游声明的长度 > 实际发来的字节数：必须当成失败，不能当成功收下。
func TestFetchToFileTruncatedDownloadIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("only-a-few-bytes"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "big-lama.pt")
	err := fetchToFile(context.Background(), srv.Client(), srv.URL, dest, 2*time.Second, nil)
	if err == nil {
		t.Fatal("被截断的下载必须报错（半包会被 md5 之外的检查漏过去）")
	}
	if _, serr := os.Stat(dest); serr == nil {
		t.Fatal("被截断的下载不能留下成品文件")
	}
}

func TestFetchToFileCompletesAndReportsProgress(t *testing.T) {
	payload := strings.Repeat("zizpanel", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 显式给长度：真实的上游（GitHub / nginx 静态文件）都会给，
		// 而"总长度"决定了任务日志里的百分比与"下完没有"的判断。
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "nested", "file.bin")
	var got, total int64
	calls := 0
	err := fetchToFile(context.Background(), srv.Client(), srv.URL, dest, 2*time.Second,
		func(g, tt int64) { calls++; got, total = g, tt })
	if err != nil {
		t.Fatalf("正常下载不该失败：%v", err)
	}
	b, rerr := os.ReadFile(dest)
	if rerr != nil || string(b) != payload {
		t.Fatalf("落盘内容不对：%v（读到 %d 字节）", rerr, len(b))
	}
	// 进度是**节流**的（最多 10 秒一次，免得把 4000 行的任务日志缓冲冲掉），
	// 所以这里只能断言"报了、总数对、已收字节在合理范围内"，
	// 不能要求回调里的 got 恰好等于文件总大小。
	if calls == 0 {
		t.Fatal("下载过程中必须报进度 —— 否则用户看到的就是『卡住了』")
	}
	if total != int64(len(payload)) {
		t.Fatalf("进度回调的总长度 = %d，期望 %d", total, len(payload))
	}
	if got <= 0 || got > int64(len(payload)) {
		t.Fatalf("进度回调的已收字节不合理：%d（文件共 %d 字节）", got, len(payload))
	}
	if _, serr := os.Stat(dest + ".part"); serr == nil {
		t.Fatal("成功后 .part 临时文件必须被改名，不该留下")
	}
}

// ---------------------------------------------------------------------------
//  "服务就绪"等待：超时 = 失败（不是警告、不是成功）
// ---------------------------------------------------------------------------

// 真机事故：180 秒内端口没起来，原来只写一条 Warning 然后 return nil，
// 任务于是显示"任务完成 ✅"，而服务当时还在慢慢从 GitHub 下权重、根本不可用。
func TestWaitIOPaintReadyTimeoutIsFailureNotSuccess(t *testing.T) {
	m := newIOPaintTestManager(t, "")
	var gotTimeout time.Duration
	var gotPort int
	m.iopaintWaitPortOverride = func(_ context.Context, port int, timeout time.Duration) bool {
		gotPort, gotTimeout = port, timeout
		return false
	}
	res := &InstallResult{App: "iopaint"}

	err := m.waitIOPaintReady(context.Background(), m.iopaintPaths(), res)
	if err == nil {
		t.Fatal("端口没起来时必须返回错误；返回 nil 就是谎报成功（真机上就是这么发生的）")
	}
	if gotPort != iopaintPort || gotTimeout != iopaintReadyTimeout {
		t.Fatalf("等待参数不对：port=%d timeout=%v", gotPort, gotTimeout)
	}
	// 错误必须可操作：说清端口、权重已预取（所以不是"还在下载"）、日志在哪。
	for _, want := range []string{"8080", "没有监听", "预取", m.iopaintWeightPath()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q（用户要知道下一步看哪），实际：%v", want, err)
		}
	}
}

func TestWaitIOPaintReadySuccess(t *testing.T) {
	m := newIOPaintTestManager(t, "")
	m.iopaintWaitPortOverride = func(context.Context, int, time.Duration) bool { return true }
	res := &InstallResult{App: "iopaint"}

	if err := m.waitIOPaintReady(context.Background(), m.iopaintPaths(), res); err != nil {
		t.Fatalf("端口起来了不该失败：%v", err)
	}
	if !stepsContain(res, "已就绪") {
		t.Fatalf("就绪时必须写一行日志，实际步骤：%v", res.Steps)
	}
}

// ---------------------------------------------------------------------------
//  权重：来源选择（镜像站优先，且"探通才算数"）
// ---------------------------------------------------------------------------

func TestIOPaintWeightSourcesPrefersReachableMirror(t *testing.T) {
	m := newIOPaintTestManager(t, "https://mirror.example:8888")
	wantMirror := "https://mirror.example:8888/models/iopaint/big-lama.pt"
	var probed string
	m.mirrorFileProbeOverride = func(_ context.Context, url string) (int64, error) {
		probed = url
		return iopaintWeightSize, nil
	}
	res := &InstallResult{}

	srcs := m.iopaintWeightSources(context.Background(), res)
	if probed != wantMirror {
		t.Fatalf("探的应该是镜像上的静态权重地址，实际探的是 %q", probed)
	}
	if len(srcs) != 2 {
		t.Fatalf("镜像可用时应当有『镜像 + 原址』两个来源，实际 %d 个", len(srcs))
	}
	if srcs[0].URL != wantMirror {
		t.Fatalf("镜像可用时必须优先走镜像，实际第一个来源是 %q", srcs[0].URL)
	}
	if srcs[1].URL != iopaintWeightURL {
		t.Fatalf("第二个来源应当是 GitHub 原址，实际 %q", srcs[1].URL)
	}
	if !stepsContain(res, "镜像站") {
		t.Fatalf("日志必须如实写出『这次走镜像站』，实际：%v", res.Steps)
	}
}

// 镜像上没有这个文件时必须**如实说**并回落，而不是打一行好看的日志然后
// 偷偷走公网（"日志说谎"是本项目最忌讳的一类问题）。
func TestIOPaintWeightSourcesFallsBackWhenMirrorMissing(t *testing.T) {
	m := newIOPaintTestManager(t, "https://mirror.example:8888")
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) {
		return -1, errors.New("镜像上没有这个文件（HTTP 404）")
	}
	res := &InstallResult{}

	srcs := m.iopaintWeightSources(context.Background(), res)
	if len(srcs) != 1 || srcs[0].URL != iopaintWeightURL {
		t.Fatalf("镜像缺件时必须回落 GitHub 原址，实际：%+v", srcs)
	}
	if !stepsContain(res, "回落") || !stepsContain(res, "404") {
		t.Fatalf("日志必须写明『镜像上没有、回落 GitHub』及原因，实际：%v", res.Steps)
	}
}

func TestIOPaintWeightSourcesWithoutMirrorUsesUpstream(t *testing.T) {
	m := newIOPaintTestManager(t, "")
	called := false
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) {
		called = true
		return 0, nil
	}
	res := &InstallResult{}

	srcs := m.iopaintWeightSources(context.Background(), res)
	if called {
		t.Fatal("没配镜像基址时不该去探镜像")
	}
	if len(srcs) != 1 || srcs[0].URL != iopaintWeightURL {
		t.Fatalf("未启用镜像时只该有 GitHub 原址，实际：%+v", srcs)
	}
	if !stepsContain(res, iopaintWeightURL) {
		t.Fatalf("日志必须写出真实来源 URL，实际：%v", res.Steps)
	}
}

// ---------------------------------------------------------------------------
//  权重：下载 + MD5 校验 + 回落 + 跳过
// ---------------------------------------------------------------------------

func TestEnsureWeightFilePrefersFirstSourceAndVerifiesMD5(t *testing.T) {
	payload := []byte("fake-lama-weights")
	want := md5Hex(t, payload)
	srcs := iopaintTestSources()

	var fetched []string
	m := newIOPaintTestManager(t, "")
	m.iopaintFetchOverride = func(_ context.Context, url, dest string, onProgress fetchProgressFunc) error {
		fetched = append(fetched, url)
		if onProgress != nil {
			onProgress(int64(len(payload)), int64(len(payload)))
		}
		return os.WriteFile(dest, payload, 0o644)
	}
	dest := filepath.Join(t.TempDir(), iopaintWeightFile)
	res := &InstallResult{}

	if err := m.ensureWeightFile(context.Background(), dest, want, srcs, res); err != nil {
		t.Fatalf("镜像下载成功时不该失败：%v", err)
	}
	if len(fetched) != 1 || fetched[0] != srcs[0].URL {
		t.Fatalf("第一个来源成功时只该下它一次，实际下了：%v", fetched)
	}
	b, rerr := os.ReadFile(dest)
	if rerr != nil || string(b) != string(payload) {
		t.Fatalf("权重没有正确落盘：%v", rerr)
	}
	if !stepsContain(res, "MD5 校验通过") || !stepsContain(res, "镜像站") {
		t.Fatalf("日志要写明来源与校验结论，实际：%v", res.Steps)
	}
}

func TestEnsureWeightFileFallsBackToSecondSource(t *testing.T) {
	payload := []byte("good-weights")
	want := md5Hex(t, payload)
	srcs := iopaintTestSources()

	var fetched []string
	m := newIOPaintTestManager(t, "")
	m.iopaintFetchOverride = func(_ context.Context, url, dest string, _ fetchProgressFunc) error {
		fetched = append(fetched, url)
		if url == srcs[0].URL {
			return errors.New("镜像返回 HTTP 500")
		}
		return os.WriteFile(dest, payload, 0o644)
	}
	dest := filepath.Join(t.TempDir(), iopaintWeightFile)
	res := &InstallResult{}

	if err := m.ensureWeightFile(context.Background(), dest, want, srcs, res); err != nil {
		t.Fatalf("有可用来源时不该失败：%v", err)
	}
	if len(fetched) != 2 || fetched[0] != srcs[0].URL || fetched[1] != srcs[1].URL {
		t.Fatalf("应当按顺序先试镜像再回落原址，实际：%v", fetched)
	}
	if !stepsContain(res, "回落") {
		t.Fatalf("回落必须在日志里说清楚，实际：%v", res.Steps)
	}
}

// 坏权重比没有权重更糟：服务能带着它起来，擦出来的图是错的且毫无提示。
func TestEnsureWeightFileRejectsMismatchedMD5AndRemovesFile(t *testing.T) {
	srcs := iopaintTestSources()
	var fetched []string
	m := newIOPaintTestManager(t, "")
	m.iopaintFetchOverride = func(_ context.Context, _ string, dest string, _ fetchProgressFunc) error {
		fetched = append(fetched, dest)
		return os.WriteFile(dest, []byte("corrupted-bytes"), 0o644)
	}
	dest := filepath.Join(t.TempDir(), iopaintWeightFile)
	res := &InstallResult{}

	err := m.ensureWeightFile(context.Background(), dest, "00000000000000000000000000000000", srcs, res)
	if err == nil {
		t.Fatal("MD5 与上游不一致时必须失败，绝不能把坏权重留给服务")
	}
	if len(fetched) != len(srcs) {
		t.Fatalf("每个来源都要试过，实际试了 %d 个", len(fetched))
	}
	if _, serr := os.Stat(dest); serr == nil {
		t.Fatal("校验不过的文件必须被删掉（留着会被下一次安装当成『已下好』）")
	}
	if !stepsContain(res, "校验不通过") {
		t.Fatalf("日志要写明校验不通过，实际：%v", res.Steps)
	}
	// 用户要能自己动手：错误里给出上游地址与目标路径。
	for _, want := range []string{iopaintWeightURL, dest} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}

func TestEnsureWeightFileSkipsWhenCachedFileIsCorrect(t *testing.T) {
	payload := []byte("already-downloaded")
	want := md5Hex(t, payload)
	dest := filepath.Join(t.TempDir(), iopaintWeightFile)
	if err := os.WriteFile(dest, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	m := newIOPaintTestManager(t, "")
	m.iopaintFetchOverride = func(context.Context, string, string, fetchProgressFunc) error {
		t.Fatal("缓存里已有校验通过的权重，不该再下 196MB")
		return nil
	}
	res := &InstallResult{}

	if err := m.ensureWeightFile(context.Background(), dest, want, iopaintTestSources(), res); err != nil {
		t.Fatalf("已有正确权重时不该失败：%v", err)
	}
	if !stepsContain(res, "跳过下载") {
		t.Fatalf("跳过时要写一行日志（用户要知道为什么这次很快），实际：%v", res.Steps)
	}
}

// 总超时：模拟"连上了、一直没数据、也不报错" —— 必须在超时后如实失败，
// 而不是让任务永远停在 running（用户看到的是一个假进度）。
func TestEnsureWeightFileTimesOutInsteadOfHanging(t *testing.T) {
	m := newIOPaintTestManager(t, "")
	m.iopaintWeightTimeoutOverride = 60 * time.Millisecond
	m.iopaintFetchOverride = func(ctx context.Context, _ string, _ string, _ fetchProgressFunc) error {
		<-ctx.Done() // 永不返回
		return ctx.Err()
	}
	dest := filepath.Join(t.TempDir(), iopaintWeightFile)
	res := &InstallResult{}

	start := time.Now()
	err := m.ensureWeightFile(context.Background(), dest, "deadbeef", iopaintTestSources(), res)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("超时必须返回错误（否则任务会永远 running）")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("总超时没有生效，耗时 %v", elapsed)
	}
	if !strings.Contains(err.Error(), "已试过 2 个来源") {
		t.Fatalf("错误要说清试过哪些来源，实际：%v", err)
	}
}

// ensureIOPaintWeight 端到端（离线）：镜像探通、下载到的却不是官方文件 → 必须失败。
func TestEnsureIOPaintWeightRejectsNonOfficialPayload(t *testing.T) {
	m := newIOPaintTestManager(t, "https://mirror.example:8888")
	m.mirrorFileProbeOverride = func(context.Context, string) (int64, error) {
		return iopaintWeightSize, nil
	}
	m.iopaintFetchOverride = func(_ context.Context, _ string, dest string, _ fetchProgressFunc) error {
		return os.WriteFile(dest, []byte("not-the-real-weights"), 0o644)
	}
	res := &InstallResult{}

	url, err := m.ensureIOPaintWeight(context.Background(), res)
	if err == nil {
		t.Fatal("下到的内容与上游 MD5 不一致时必须失败")
	}
	if url != "https://mirror.example:8888/models/iopaint/big-lama.pt" {
		t.Fatalf("返回的应当是首选来源（写进 plist 用），实际 %q", url)
	}
	if fileExists(m.iopaintWeightPath()) {
		t.Fatal("坏文件必须被删掉，不能留在 torch 缓存里")
	}
}

// ---------------------------------------------------------------------------
//  plist 与路径：锁住"面板真的会优先用镜像站"这件事
// ---------------------------------------------------------------------------

func TestIOPaintPlistPinsWeightSourceAndMD5(t *testing.T) {
	m := newIOPaintTestManager(t, "")
	mirrorWeight := "https://mirror.example:8888/models/iopaint/big-lama.pt"

	got := iopaintPlist(m.iopaintPaths(), "zizdog", "mps", "https://mirror.example:8888/hf", mirrorWeight)
	for _, want := range []string{
		"<key>LAMA_MODEL_URL</key>", mirrorWeight,
		"<key>LAMA_MODEL_MD5</key>", iopaintWeightMD5,
		"<key>HF_ENDPOINT</key>", "https://mirror.example:8888/hf",
		"--model=lama", "--device=mps", "--port=8080",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plist 里应包含 %q（否则服务会去公网下权重 / 校验失效）", want)
		}
	}
}

// plist 是 launchd 唯一接受的格式：拼错一个标签，服务就直接起不来
// （launchctl 报的错还很难懂）。所以这里用 XML 解析器把结构锁住，
// 而不是只做字符串包含 —— 后者对"标签配对错了"完全无感。
// 真机上另用 `plutil -lint` 复核过一次（macOS 的权威校验器，结果 OK）。
func TestIOPaintPlistIsWellFormedXML(t *testing.T) {
	m := newIOPaintTestManager(t, "")
	got := iopaintPlist(m.iopaintPaths(), "zizdog", "mps", "https://mirror/hf",
		"https://mirror/models/iopaint/big-lama.pt")

	var doc struct {
		XMLName xml.Name `xml:"plist"`
		Dict    struct {
			Keys []string `xml:"key"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("plist 不是合法 XML（launchd 会直接拒绝这个服务）：%v\n%s", err, got)
	}
	have := map[string]bool{}
	for _, k := range doc.Dict.Keys {
		have[k] = true
	}
	for _, k := range []string{"Label", "UserName", "ProgramArguments", "RunAtLoad", "KeepAlive", "EnvironmentVariables", "StandardOutPath", "StandardErrorPath"} {
		if !have[k] {
			t.Fatalf("plist 缺少必需的键 %q（现有：%v）", k, doc.Dict.Keys)
		}
	}
}

// 权重落点必须与 iopaint（torch.hub）期望的目录一致 —— 不一致就等于没预取，
// 服务会再去 GitHub 下一次（真机上"下到一半卡住"的场景会原样复现）。
func TestIOPaintWeightPathMatchesTorchHubCacheLayout(t *testing.T) {
	m := newIOPaintTestManager(t, "")
	want := filepath.Join(m.opt.UserHome, ".cache", "torch", "hub", "checkpoints", iopaintWeightFile)
	if got := m.iopaintWeightPath(); got != want {
		t.Fatalf("权重落点 = %q，期望 %q", got, want)
	}
}

func TestEnsureUserWritableDirCreatesParents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".cache", "torch", "hub", "checkpoints")
	if err := ensureUserWritableDir("", "", dir); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("目录没建出来：%v", err)
	}
}
