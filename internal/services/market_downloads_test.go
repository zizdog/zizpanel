package services

// market_downloads_test.go —— 第 2 层静态门禁（**不联网**）
//
// 这个文件存在的唯一理由：让"目录改了、声明没改"变成**测试失败**，
// 而不是等到某次真机安装才发现。它是"加一个应用 = 填空 + 一条命令"能长期
// 成立的根本 —— 没有它，声明会在两个版本内退化成一份过期文档。
//
// 它跑得很快也完全离线：只读 Catalog() 与 market_downloadApps 两份静态数据，
// 不碰网络、不碰真实家目录、不碰 Docker、不碰面板。

import (
	"strings"
	"testing"
	"time"
)

// TestMarketDeclarationsCoverCatalogExactly 是反漂移的主测试：
// 两个方向都锁 —— 目录里的每个条目都要有声明，声明里的每个条目都要在目录里。
func TestMarketDeclarationsCoverCatalogExactly(t *testing.T) {
	catalog := Catalog()
	byID := map[string]App{}
	for _, a := range catalog {
		byID[a.ID] = a
	}

	declared := map[string]bool{}
	for _, m := range MarketApps() {
		declared[m.ID] = true
		if _, ok := byID[m.ID]; !ok {
			t.Errorf("声明里有 %s，但目录里没有这个条目 —— "+
				"条目被删/改名时，声明必须同步（否则这份声明就成了过期文档）", m.ID)
		}
	}
	for _, a := range catalog {
		if !declared[a.ID] {
			t.Errorf("目录里有 %s（%s），但 market_downloads.go 里没有它的下载点声明 —— "+
				"加应用时必须在同一轮把声明补上（见 docs/新增应用工作流.md）", a.ID, a.Name)
		}
	}

	if len(catalog) != 27 {
		t.Logf("注意：目录现在有 %d 个条目（写这份声明时是 27 个）—— "+
			"数量变化本身不是错误，但每一个新条目都必须有声明", len(catalog))
	}
}

// TestMarketInvariantsHold 跑全部静态不变量（超时 / NAS 回答 / arm64 证据 /
// 服务语义 / compose 不许 platform / sha256 来源）。
func TestMarketInvariantsHold(t *testing.T) {
	for _, p := range MarketInvariantProblems() {
		t.Errorf("静态不变量被打破：%s", p)
	}
}

// TestMarketDriftIsActuallyCaught 证明反漂移**不是空断言**。
//
// 做法：拿一份真实声明，分别把目录侧的 BrewFormula / ComposeImage / ServiceLabel /
// Kind / NoDaemon 改坏，断言 MarketDeclarationProblems 每次都报错。
// 如果哪天有人把比对写成"永远返回空"，这个测试会立刻红 ——
// 这正是"目录改了、声明没改 → 测试失败"能成立的前提。
func TestMarketDriftIsActuallyCaught(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		mutate func(*App)
	}{
		{"brew 包名改了", "nginx", func(a *App) { a.BrewFormula = "nginx-renamed" }},
		{"compose 镜像 tag 改了", "squoosh", func(a *App) {
			a.ComposeYAML = strings.Replace(a.ComposeYAML, "pjmeca/squoosh:1.1.0", "pjmeca/squoosh:2.0.0", 1)
		}},
		{"ServiceLabel 改了", "frpc", func(a *App) { a.ServiceLabel = "com.zizdog.frpc.renamed" }},
		{"Kind 改了", "uptime-kuma", func(a *App) { a.Kind = KindNative }},
		{"NoDaemon 改了", "phpmyadmin", func(a *App) { a.NoDaemon = false }},
		{"PanelInstaller 改了", "iopaint", func(a *App) { a.PanelInstaller = "iopaint-v2" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := MarketAppFor(tc.id)
			if !ok {
				t.Fatalf("声明里找不到 %s", tc.id)
			}
			app, ok := FindApp(tc.id)
			if !ok {
				t.Fatalf("目录里找不到 %s", tc.id)
			}
			if problems := MarketDeclarationProblems(m, app); len(problems) != 0 {
				t.Fatalf("原始声明对 %s 就已经有问题了：%v", tc.id, problems)
			}
			tc.mutate(&app)
			if problems := MarketDeclarationProblems(m, app); len(problems) == 0 {
				t.Fatalf("把目录里 %s 的字段改坏之后，比对**没有报错** —— "+
					"反漂移形同虚设", tc.id)
			}
		})
	}
}

// TestMarketPointInvariantsAreNotVacuous 证明"下载点的不变量"真的会拦东西：
// 逐个构造坏声明，断言每一条都被拦下。
func TestMarketPointInvariantsAreNotVacuous(t *testing.T) {
	base := MarketDownloadPoint{
		Purpose: MarketFetchModelFile,
		Label:   "测试用下载点",
		Upstream: MarketUpstream{
			ID:   "example.com/x.bin",
			URL:  "https://example.com/x.bin",
			Size: 10,
			Note: "实测 10 B",
		},
		NAS:      nasMirrored("models/x.bin"),
		Timeout:  time.Minute,
		Required: true,
		Checksum: MarketChecksum{Asset: "上游 checksums.txt"},
		ARM64:    "与架构无关",
	}
	app := App{ID: "test", Kind: KindNative, NoDaemon: true}
	decl := MarketApp{
		ID: "test", Kind: KindNative, NoDaemon: true,
		Runtime:   MarketRuntime{Mode: MarketRuntimeNone, LabelSource: "测试"},
		Downloads: []MarketDownloadPoint{base},
	}

	mutants := []struct {
		name   string
		mutate func(*MarketDownloadPoint)
		want   string
	}{
		{"没有超时也没理由", func(d *MarketDownloadPoint) { d.Timeout = 0 }, "没有超时也没有理由"},
		{"NAS 状态留空", func(d *MarketDownloadPoint) { d.NAS = MarketNAS{Reason: strings.Repeat("理由", 20)} }, "不合法"},
		{"not_needed 理由太短", func(d *MarketDownloadPoint) { d.NAS = nasNotNeeded("不需要") }, "理由太短"},
		{"missing 理由太短", func(d *MarketDownloadPoint) { d.NAS = nasMissing("没有") }, "理由太短"},
		{"mirrored 没有路径", func(d *MarketDownloadPoint) { d.NAS = MarketNAS{State: NASMirrored} }, "Path 为空"},
		{"可选步骤没写降级后果", func(d *MarketDownloadPoint) { d.Required = false }, "OptionalImpact"},
		{"sha256 没有来源", func(d *MarketDownloadPoint) { d.Checksum.SHA256 = "deadbeef" }, "没写 Source"},
		{"完全没有校验说明", func(d *MarketDownloadPoint) { d.Checksum = MarketChecksum{} }, "不许留空默认通过"},
		{"没有 arm64 证据", func(d *MarketDownloadPoint) { d.ARM64 = "" }, "arm64 证据"},
		{"上游没有标识", func(d *MarketDownloadPoint) { d.Upstream.ID = ""; d.Upstream.URL = "" }, "既没有 ID 也没有 URL"},
		{"体积没有来源", func(d *MarketDownloadPoint) { d.Upstream.Note = "" }, "没写来源"},
	}
	for _, tc := range mutants {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.mutate(&d)
			m := decl
			m.Downloads = []MarketDownloadPoint{d}
			problems := MarketDeclarationProblems(m, app)
			if len(problems) == 0 {
				t.Fatalf("坏声明没有被拦下：%+v", d)
			}
			joined := strings.Join(problems, " | ")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("报错内容里没有提到 %q，实际是：%s", tc.want, joined)
			}
		})
	}

	if problems := MarketDeclarationProblems(decl, app); len(problems) != 0 {
		t.Fatalf("基准声明本身应该是干净的，实际：%v", problems)
	}
}

// TestMarketComposeNeverUsesPlatform 单独把铁律②拎出来测一遍：
// 目录里任何一个 compose 都不许出现 platform:（尤其是 linux/amd64）。
func TestMarketComposeNeverUsesPlatform(t *testing.T) {
	for _, a := range Catalog() {
		if strings.Contains(a.ComposeYAML, "platform:") {
			t.Errorf("%s 的 compose 里出现了 platform:（铁律②：镜像必须自带 linux/arm64，不许 platform/Rosetta）", a.ID)
		}
	}
}

// TestMarketNASAnswersAreReviewable 把所有"不需要镜像"（not_needed）的理由
// 集中打印出来 —— 这是**给人复核**用的：审计通过不代表理由经得起看。
func TestMarketNASAnswersAreReviewable(t *testing.T) {
	seen := 0
	for _, m := range MarketApps() {
		for _, d := range m.Downloads {
			if d.NAS.State != NASNotNeeded {
				continue
			}
			seen++
			t.Logf("not_needed | %-16s %-28s %s", m.ID, d.Label, d.NAS.Reason)
		}
	}
	if seen == 0 {
		t.Fatal("一个 not_needed 都没有？要么全部镜像了（那很好，请把这条测试改成 missing=0），" +
			"要么状态被误填 —— 请人工确认一次")
	}
}

// TestMarketAccessors 锁住 --only 用到的访问器行为。
func TestMarketAccessors(t *testing.T) {
	if len(MarketIDs()) != len(MarketApps()) {
		t.Fatalf("MarketIDs() 与 MarketApps() 数量不一致：%d vs %d", len(MarketIDs()), len(MarketApps()))
	}
	if _, ok := MarketAppFor("squoosh"); !ok {
		t.Fatal("MarketAppFor(squoosh) 应该找得到")
	}
	if _, ok := MarketAppFor("no-such-app"); ok {
		t.Fatal("MarketAppFor 对不存在的 ID 应该返回 false")
	}
	host, repo, tag := MarketImageRef("pjmeca/squoosh:1.1.0")
	if host != "" || repo != "pjmeca/squoosh" || tag != "1.1.0" {
		t.Fatalf("MarketImageRef 解析错了：host=%q repo=%q tag=%q", host, repo, tag)
	}
}
