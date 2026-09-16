package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// ============================================================================
//  CLT 镜像分片：逐片内容校验 + "没有适配条目"时的诚实说明
//
//  这一组的由来（2026-09-16 审计发现的两处不一致）：
//   1) 代码注释宣称"每片单独校验"，实际 fetchParts 只校验**大小** ——
//      "大小正好、内容坏掉"的分片会被当成"下好了"永久跳过：每次重试都拿同一片
//      坏的拼包，卡在整体 sha256 上，而报错里看不出是哪一片。
//      修法：cltPart 增加可选 sha256，给了就逐片校验，坏片删掉重下。
//   2) 清单里没有适用条目时，错误信息只说"没有适用的包集合"，用户看到的是
//      后面苹果 CDN 极慢（15 分钟 1 MB）却不知道为什么。
//
//  全部走本地 httptest 假镜像：不联网、不碰真 NAS、不下载 631 MiB。
// ============================================================================

func cltTestSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestFetchPartsVerifiesPerPartSHA256AndHealsBadPart 锁住三件事：
//   - 清单给了分片 sha256 而服务器发的内容不对 → 必须报错；
//   - 坏片必须被删掉，不能被留在盘上冒充"已下好"；
//   - 大小对但内容坏的分片，下次必须**重下这一片**，而不是沿用。
func TestFetchPartsVerifiesPerPartSHA256AndHealsBadPart(t *testing.T) {
	// 两段等长内容：这样才能只靠 sha256 区分"大小对、内容坏"
	want := []byte(strings.Repeat("good-part-data-", 8))
	bad := []byte(strings.Repeat("evil-part-data-", 8))
	if len(want) != len(bad) {
		t.Fatalf("测试前提被破坏：两段内容必须等长（%d vs %d）", len(want), len(bad))
	}

	var payload1 atomic.Value
	payload1.Store(bad)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		switch filepath.Base(r.URL.Path) {
		case "pkg.part-000":
			_, _ = w.Write(want)
		case "pkg.part-001":
			_, _ = w.Write(payload1.Load().([]byte))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &Manager{}
	result := &InstallResult{Steps: []string{}}

	// --- 用例 A：清单声明 part-001 应该是 want，服务器发 bad → 必须报错 ---
	dirA := t.TempDir()
	partsA := []cltPart{
		{File: "pkg.part-000", Size: int64(len(want)), SHA256: cltTestSHA(want)},
		{File: "pkg.part-001", Size: int64(len(bad)), SHA256: cltTestSHA(want)},
	}
	err := m.fetchParts(context.Background(), result, srv.URL, "pkg", dirA, partsA)
	if err == nil {
		t.Fatal("分片内容与清单 sha256 不符时必须报错，不能当成下载成功")
	}
	if !strings.Contains(err.Error(), "pkg.part-001") {
		t.Errorf("报错必须点名是哪一片，实际：%v", err)
	}
	if _, serr := os.Stat(filepath.Join(dirA, "pkg.part-001")); !os.IsNotExist(serr) {
		t.Error("校验失败的分片必须删掉：留着会被续传当成已下好，每次都失败在同一处")
	}

	// --- 用例 B：大小对但内容坏的分片必须被重下（这是修掉的真实缺陷）---
	dirB := t.TempDir()
	// 预置坏片：大小与清单一致，内容不对
	if werr := os.WriteFile(filepath.Join(dirB, "pkg.part-001"), bad, 0o644); werr != nil {
		t.Fatal(werr)
	}
	if werr := os.WriteFile(filepath.Join(dirB, "pkg.part-000"), want, 0o644); werr != nil {
		t.Fatal(werr)
	}
	payload1.Store(want) // 这次源站发的是正确内容
	atomic.StoreInt32(&hits, 0)
	partsB := []cltPart{
		{File: "pkg.part-000", Size: int64(len(want)), SHA256: cltTestSHA(want)},
		{File: "pkg.part-001", Size: int64(len(want)), SHA256: cltTestSHA(want)},
	}
	if err := m.fetchParts(context.Background(), result, srv.URL, "pkg", dirB, partsB); err != nil {
		t.Fatalf("坏片重下后应当成功：%v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("只该重下坏的那一片（1 次请求），实际 %d 次；"+
			"次数不对说明好片被重下、或坏片被跳过", got)
	}
	gotBody, rerr := os.ReadFile(filepath.Join(dirB, "pkg.part-001"))
	if rerr != nil || string(gotBody) != string(want) {
		t.Errorf("坏片没有被修好：err=%v len=%d", rerr, len(gotBody))
	}

	// --- 用例 C：清单没给分片 sha256 时退回"只校验大小"（兼容旧清单）---
	dirC := t.TempDir()
	partsC := []cltPart{
		{File: "pkg.part-000", Size: int64(len(want))},
		{File: "pkg.part-001", Size: int64(len(bad))},
	}
	if err := m.fetchParts(context.Background(), result, srv.URL, "pkg", dirC, partsC); err != nil {
		t.Errorf("清单没给分片 sha256 时，大小正确就应当接受：%v", err)
	}
}

// TestCLTOverallSHAMismatchDropsParts 锁住"整体 sha256 不符时连分片一起丢掉"。
//
// 为什么这条值得单独测：旧代码只删拼出来的坏包，分片留着。下次重试时
// fetchParts 看到"大小都对"就全部跳过，于是在同一份坏数据上永远失败 ——
// 用户看到的是"点多少次重试都一样"，而且错误信息指向整体校验，看不出根因。
func TestCLTOverallSHAMismatchDropsParts(t *testing.T) {
	p0 := []byte("first-part-bytes--")
	p1 := []byte("second-part-bytes-")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "pkg.part-000":
			_, _ = w.Write(p0)
		case "pkg.part-001":
			_, _ = w.Write(p1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("ZIZPANEL_CLT_MIRROR", srv.URL)
	item := cltIndexItem{
		Path: "clt/x", Pkgs: []string{"pkg"},
		Parts: []cltPkgParts{{Pkg: "pkg", Parts: []cltPart{
			{File: "pkg.part-000", Size: int64(len(p0))},
			{File: "pkg.part-001", Size: int64(len(p1))},
		}}},
	}
	dst := filepath.Join(t.TempDir(), "pkg")
	m := &Manager{}
	result := &InstallResult{Steps: []string{}}
	// 故意给一个不可能对上的整体 sha256
	_, err := m.fetchCLTPkg(context.Background(), result, item, "pkg", dst, strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("整体 sha256 不符时必须报错")
	}
	if !strings.Contains(err.Error(), "校验不一致") {
		t.Errorf("错误信息应指出校验不一致，实际：%v", err)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Error("整体校验失败时必须删掉拼出来的坏包")
	}
	if _, serr := os.Stat(dst + ".parts"); !os.IsNotExist(serr) {
		t.Error("整体校验失败时必须连分片一起丢掉，否则坏片会被续传永久跳过")
	}
}

// TestCLTNoApplicableItemErrorTellsTheTruth 锁住"没有适配条目"时说实话。
//
// 用户能接受慢，不能接受"不知道为什么慢"。这条错误后面紧跟的就是苹果 CDN
// （实测 15 分钟只下 1 MB），所以必须写清：当前系统版本、镜像里有什么、
// 接下来走哪条路、大概多慢 —— 以及"这不是网络问题"。
func TestCLTNoApplicableItemErrorTellsTheTruth(t *testing.T) {
	items := []cltIndexItem{
		{Name: "Command Line Tools 16.2", Path: "clt/072-44426-A", MaxOS: 15,
			Pkgs: []string{"CLTools_Executables.pkg"}},
	}
	err := cltNoApplicableItemError(items, 26)
	if err == nil {
		t.Fatal("必须返回错误（调用方据此回落苹果 CDN）")
	}
	msg := err.Error()
	t.Logf("诚实说明原文（macOS 26 遇到只有 max_os=15 的镜像时）：%s", msg)
	for _, want := range []string{
		"macOS 26", // 当前系统版本
		"macOS 15", // 镜像里实际有什么上限
		"不是网络问题",   // 别让人去查网络
		"苹果 CDN",   // 接下来走哪条路
		"慢",        // 真实代价
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里应包含 %q，实际：%s", want, msg)
		}
	}
	// 空清单也要能说清（而不是拼出半句话）
	empty := cltNoApplicableItemError(nil, 26).Error()
	if !strings.Contains(empty, "macOS 26") || !strings.Contains(empty, "一条可用条目") {
		t.Errorf("空清单的说明不清楚：%s", empty)
	}
	// 有兜底条目（max_os=0）时不该走到这里；但万一调用，摘要应写"不限"
	if s := cltMaxOSSummary([]cltIndexItem{{MaxOS: 0}}); !strings.Contains(s, "不限") {
		t.Errorf("max_os=0 应显示为不限，实际 %q", s)
	}

	// 光造出这条信息不够：必须真的在"清单里找不到适用条目"那一刻用它，
	// 否则用户看到的还是老那句"没有适用的包集合"，等于没改。
	src, rerr := os.ReadFile("homebrew_clt_mirror.go")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(src), "return cltNoApplicableItemError(idx.Items, macMajor)") {
		t.Error("installCLTFromMirror 必须调用 cltNoApplicableItemError")
	}
	// 而且调用方（homebrew.go，本轮不归我改）必须把这段文字原样写进任务步骤 ——
	// 只读断言：接线断了这条说明就到不了用户眼前。
	hb, herr := os.ReadFile("homebrew.go")
	if herr != nil {
		t.Fatal(herr)
	}
	if !strings.Contains(string(hb), `"镜像这条路没走通："+err.Error()`) {
		t.Error("homebrew.go 必须把镜像路的错误原文写进任务步骤，否则用户看不到诚实说明")
	}
}

// TestCLTPartSHA256SchemaRoundTrips 锁住新增字段的 JSON 名（清单是两边共用的契约）。
func TestCLTPartSHA256SchemaRoundTrips(t *testing.T) {
	const sample = `{"pkg":"CLTools_Executables.pkg","parts":[
	  {"file":"CLTools_Executables.pkg.part-000","size":33554432,"sha256":"AB12"},
	  {"file":"CLTools_Executables.pkg.part-001","size":662248}]}`
	var pp cltPkgParts
	if err := json.Unmarshal([]byte(sample), &pp); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(pp.Parts) != 2 || pp.Parts[0].SHA256 != "AB12" {
		t.Fatalf("分片 sha256 字段没解析出来：%+v", pp.Parts)
	}
	if pp.Parts[1].SHA256 != "" {
		t.Errorf("没写 sha256 时应为空（旧清单兼容），实际 %q", pp.Parts[1].SHA256)
	}
}

// TestCLTSyncScriptLocksTheLessons 是"文件内容"级测试：把踩过的坑钉在脚本里。
//
// 读的是仓库里的 tools/sync-clt-mirror.sh，不做任何网络/文件系统副作用。
func TestCLTSyncScriptLocksTheLessons(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "tools", "sync-clt-mirror.sh"))
	if err != nil {
		t.Fatalf("读不到同步脚本：%v", err)
	}
	src := string(b)
	for _, want := range []string{
		// ssh 绝不能吞掉 while-read 的 stdin（sync-nas-apps.sh 的真实事故）
		"ssh -n",
		"</dev/null",
		// 真 sha256：目标侧算、且和源声明对账
		"sha256sum",
		"shasum -a 256",
		"不写清单",
		// 幂等/增量：已有且校验通过的分片不重下
		"复用已有",
		// 失败要报告缺了什么
		"MISSING_REPORT",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("tools/sync-clt-mirror.sh 缺少 %q", want)
		}
	}
	// 绝不允许把 sha256 写死进脚本 —— 本仓库因为编造 sha256 出过
	// "镜像永远不命中、静默回落慢源"的事故。
	if m := regexp.MustCompile(`\b[0-9a-f]{64}\b`).FindString(src); m != "" {
		t.Errorf("同步脚本里出现了写死的 sha256（%s…）：sha256 必须现场算，不能编造", m[:12])
	}
	// 面板真正会下载的两个包必须在白名单里（漏了就等于镜像了用不上的东西）
	for _, pkg := range cltInstallPkgs {
		if !strings.Contains(src, pkg) {
			t.Errorf("同步脚本的包白名单缺了 %q", pkg)
		}
	}
}
