package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ============================================================================
//  上传分批门禁（真机报障：4.16 GB 网站被「上传文件夹」拒绝）
//
//  根因是**判据用错层级**：拿整批总大小去比单次请求上限，而提示还建议用户
//  用同一个功能分流 —— 自相矛盾、必然失败，一个字节都没传。
//
//  修法对应的判据只有两条，这里都用**运行体**验证（不是源码里出现了某个词）：
//    1. 总大小 > 上限但每个文件都 < 上限 ⇒ 允许，并分成多批，每批 ≤ 上限；
//    2. 单个文件 > 上限 ⇒ 拒绝，且错误里含该文件名。
//  分批逻辑放在纯函数模块 assets/js/uploadplan.js，由 node 直接跑来断言。
// ============================================================================

// uploadPlanHarness 是喂给 node 的探针：只 import 纯函数模块。
//
// 用绝对 file:// URL import —— 不依赖测试的工作目录，也不会往源码目录写临时文件。
const uploadPlanHarness = `
import { planUploadBatches, oversizeAdvice, uploadReserve } from %s;
const LIMIT = 4 * 1024 * 1024;          // 4 MiB：与小上限真机验证同一口径
const MB = (n) => Math.round(n * 1024 * 1024);
const sizes = [MB(1.5), MB(1.5), MB(1.5), MB(1.5)];  // 总 6MB > 4MB，每个都 < 4MB
const plan = planUploadBatches(sizes, LIMIT);
const sum = (arr, src) => arr.reduce((s, i) => s + src[i], 0);
const mixedSizes = [1024, MB(5), 3 * 1024 * 1024];   // 中间那个单文件超限
const mixed = planUploadBatches(mixedSizes, LIMIT);
const exact = planUploadBatches([LIMIT], LIMIT);
console.log(JSON.stringify({
  limit: LIMIT,
  total: sizes.reduce((a, b) => a + b, 0),
  batches: plan.batches,
  oversize: plan.oversize,
  batchSums: plan.batches.map((b) => sum(b, sizes)),
  mixedBatches: mixed.batches,
  mixedOversize: mixed.oversize,
  mixedSums: mixed.batches.map((b) => sum(b, mixedSizes)),
  exactOversize: exact.oversize,
  exactBatches: exact.batches,
  reserve: uploadReserve(LIMIT),
  advice: oversizeAdvice(),
}));
`

type uploadPlanProbe struct {
	Limit      int64    `json:"limit"`
	Total      int64    `json:"total"`
	Batches    [][]int  `json:"batches"`
	Oversize   []int    `json:"oversize"`
	BatchSums  []int64  `json:"batchSums"`
	MixedBatch [][]int  `json:"mixedBatches"`
	MixedOver  []int    `json:"mixedOversize"`
	MixedSums  []int64  `json:"mixedSums"`
	ExactOver  []int    `json:"exactOversize"`
	ExactBatch [][]int  `json:"exactBatches"`
	Reserve    int64    `json:"reserve"`
	Advice     []string `json:"advice"`
}

// runUploadPlanProbe 用 node 跑一次纯函数模块，返回结构化结果。
func runUploadPlanProbe(t *testing.T) uploadPlanProbe {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		// make check 本身就用 node 跑 acorn，所以这里缺失只可能是环境异常。
		// 如实跳过并说明，绝不假装验证过。
		t.Skip("找不到 node，跳过前端分批纯函数门禁（make check 的 acorn 检查也需要它）")
	}
	abs, err := filepath.Abs(filepath.Join("assets", "js", "uploadplan.js"))
	if err != nil {
		t.Fatal(err)
	}
	url := "file://" + filepath.ToSlash(abs)
	script := strings.Replace(uploadPlanHarness, "%s", "\""+url+"\"", 1)

	dir := t.TempDir()
	harness := filepath.Join(dir, "probe.mjs")
	if err := os.WriteFile(harness, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harness).CombinedOutput()
	if err != nil {
		t.Fatalf("node 跑 uploadplan.js 失败: %v\n%s", err, out)
	}
	// node 会把 "MODULE_TYPELESS_PACKAGE_JSON" 之类的告警写到 stderr；
	// 探针的 JSON 是最后一行，只取它（否则告警会污染解析）。
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	var probe uploadPlanProbe
	if err := json.Unmarshal([]byte(text), &probe); err != nil {
		t.Fatalf("解析探针输出失败: %v\n输出: %s", err, out)
	}
	return probe
}

// TestUploadPlanSplitsByRequestLimitNotTotal 是这次报障的**核心门禁**。
//
// 总大小 > 上限、但每个文件都 < 上限 ⇒ 必须允许，并分成多批；每批累计 ≤ 上限。
func TestUploadPlanSplitsByRequestLimitNotTotal(t *testing.T) {
	p := runUploadPlanProbe(t)
	if p.Total <= p.Limit {
		t.Fatalf("探针造的数据没有超过上限（总 %d，上限 %d）—— 这条测试什么都没证明", p.Total, p.Limit)
	}
	if len(p.Oversize) != 0 {
		t.Fatalf("每个文件都小于上限，却被判超限：%v", p.Oversize)
	}
	if len(p.Batches) < 2 {
		t.Fatalf("总大小 %d 超过上限 %d，却只分了 %d 批：%v", p.Total, p.Limit, len(p.Batches), p.Batches)
	}
	seen := map[int]bool{}
	for i, b := range p.Batches {
		if len(b) == 0 {
			t.Fatalf("第 %d 批是空的", i)
		}
		if p.BatchSums[i] > p.Limit {
			t.Errorf("第 %d 批累计 %d，超过上限 %d —— 服务端会 413", i, p.BatchSums[i], p.Limit)
		}
		for _, idx := range b {
			if seen[idx] {
				t.Errorf("索引 %d 出现在多个批次里（会重复上传）", idx)
			}
			seen[idx] = true
		}
	}
	if len(seen) != 4 {
		t.Errorf("分批后覆盖 %d 个文件，想要 4（不能漏传也不能重复）", len(seen))
	}
	if p.Reserve <= 0 {
		t.Error("multipart 开销余量为 0 —— 请求体可能刚好压线超过上限")
	}
	t.Logf("总 %d > 上限 %d → %d 批（各批累计 %v，余量 %d）",
		p.Total, p.Limit, len(p.Batches), p.BatchSums, p.Reserve)
}

// TestUploadPlanRejectsOnlyOversizeSingleFile 锁"只有单个文件 > 上限才拒绝"。
func TestUploadPlanRejectsOnlyOversizeSingleFile(t *testing.T) {
	p := runUploadPlanProbe(t)
	if len(p.MixedOver) != 1 || p.MixedOver[0] != 1 {
		t.Fatalf("单个 5MiB 文件（上限 4MiB）应被判超限且索引为 1，实际 %v", p.MixedOver)
	}
	// 超限文件必须**不**出现在任何批次里（一个字节都不发）
	for _, b := range p.MixedBatch {
		for _, idx := range b {
			if idx == 1 {
				t.Fatalf("超限文件被塞进了批次：%v", p.MixedBatch)
			}
		}
	}
	for i, s := range p.MixedSums {
		if s > p.Limit {
			t.Errorf("第 %d 批累计 %d 超过上限 %d", i, s, p.Limit)
		}
	}
	// 单个文件正好等于上限：允许（不进 oversize），且独占一批
	if len(p.ExactOver) != 0 {
		t.Fatalf("单个文件正好等于上限应被允许，实际判超限：%v", p.ExactOver)
	}
	if len(p.ExactBatch) != 1 || len(p.ExactBatch[0]) != 1 {
		t.Fatalf("单个等于上限的文件应独占一批，实际 %v", p.ExactBatch)
	}
	t.Logf("单文件 5MiB 判超限=%v；等于上限的文件独占一批=%v", p.MixedOver, p.ExactBatch)
}

// TestUploadAdviceNeverSuggestsUploadingAgain 锁"提示不许把上传功能自己当解法"。
//
// 用户报障就是因为提示让他用「上传文件夹」把 4.16 GB 分批 —— 他刚用的就是它。
func TestUploadAdviceNeverSuggestsUploadingAgain(t *testing.T) {
	p := runUploadPlanProbe(t)
	joined := strings.Join(p.Advice, "；")
	for _, bad := range []string{"上传文件夹", "重新上传", "再传一次", "分次上传这个"} {
		if strings.Contains(joined, bad) {
			t.Errorf("出路里出现了 %q —— 用户刚用的就是上传功能，这不是出路：%s", bad, joined)
		}
	}
	for _, want := range []string{"分卷", "命令行"} {
		if !strings.Contains(joined, want) {
			t.Errorf("出路里缺少可执行的做法 %q：%s", want, joined)
		}
	}
	t.Logf("单文件超限的出路 = %s", joined)
}

// TestUploadLimitMessageDoesNotSuggestItsOwnFeature 锁后端 413 文案同样不自我指涉。
func TestUploadLimitMessageDoesNotSuggestItsOwnFeature(t *testing.T) {
	msg := uploadLimitMessage(6<<30, 4<<30, "请求体过大")
	if strings.Contains(msg, "上传文件夹") {
		t.Errorf("413 文案又建议用户用「上传文件夹」：%s", msg)
	}
	for _, want := range []string{"6.00 GB", "4.00 GB", "分卷", "命令行"} {
		if !strings.Contains(msg, want) {
			t.Errorf("413 文案缺少 %q：%s", want, msg)
		}
	}
	t.Logf("413 文案 = %s", msg)
}

// TestNoUploadAdviceTellsUserToReuseUpload 是"覆盖整类"的源码门禁。
//
// 只改被点名的那一处不够：同类文案在文件管理器与其它上传路由里都有，
// 下次新增一处又会复发。这里扫所有**用户可见文案的来源**（前端 JS + web 包 Go），
// 禁用那句把上传功能当解法的原话。
func TestNoUploadAdviceTellsUserToReuseUpload(t *testing.T) {
	banned := []string{"把网站按子目录分批", "用「⬆ 上传文件夹」把"}
	check := func(path, src string) {
		for _, b := range banned {
			if strings.Contains(src, b) {
				t.Errorf("%s 里又出现了把上传功能当解法的文案 %q", path, b)
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join("assets", "js")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
				continue
			}
			b, err := os.ReadFile(filepath.Join("assets", "js", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			check(e.Name(), string(b))
		}
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		check(e.Name(), string(b))
	}
}

// TestFilesFrontendUploadBatchesWiring 锁前端真的接上了分批实现。
func TestFilesFrontendUploadBatchesWiring(t *testing.T) {
	js := readAssetJS(t, "files.js")
	for _, want := range []string{
		"planUploadBatches(",         // 真的用它算批次
		"oversizeAdvice()",           // 出路来自纯函数（门禁覆盖它的措辞）
		"api.fileUploadLimit()",      // 上限回读
		"第 ${curNo}/${batchCount} 批", // 进度里显示第几批
		"${e.rel} — ",                // 超限提示必须点名文件（相对路径 + 大小）
	} {
		if !strings.Contains(js, want) {
			t.Errorf("files.js 缺少 %q —— 分批/点名/回读有一项没接上", want)
		}
	}
	// 旧的"整批总大小超限"判据必须彻底消失（它就是这次报障的根因）
	if strings.Contains(js, "超过单次上传上限") {
		t.Error("files.js 里还留着「总大小超过单次上传上限」的判据 —— 判据用错层级会原样复发")
	}
	if regexp.MustCompile(`total\s*<=\s*[A-Za-z0-9]`).MatchString(js) {
		t.Error("files.js 里还有拿总大小与上限比较的写法 —— 总大小不参与拒绝")
	}
}
