package main

// ============================================================================
//  zizpanel-assets audit —— 应用市场在线审计（第 3 层）
//
//  一条命令、全部 27 个应用、一张表。用户的原话：
//    "如果以后每加一个应用都要一点一点慢慢调试，那这个应用市场就没什么实用价值了。"
//
//  这个子命令就是"一条命令告诉你缺什么"：
//    · NAS 路径：真实 HTTP HEAD / Range GET，记录**状态码 + 体积 + 速度**
//    · 上游 URL：真实可达性（每个请求都有超时，绝不无限等）
//    · Docker 镜像：真的取 manifest，判断有没有 linux/arm64
//    · sha256：镜像 manifest / 上游 checksums.txt / 文件实算，**三方比对**；
//      声明里没有期望值时报"未声明"，绝不编造
//    · 有缺口就**非零退出**（这样能进 CI / 可选门禁）
//
//  它**不重复实现**已有工具：
//    · 声明与静态不变量来自 internal/services（MarketApps / MarketInvariantProblems）
//    · 镜像路径布局与 apps/<id>/<tag>/manifest.json 契约来自 services.ReleaseBinaryAssets()
//    · 同步仍然由 tools/sync-nas-apps.sh、tools/build-offline-bundle.sh 负责
//    · 真机安装验收仍然由 tools/market-install-verify.py 负责（见文件末尾的指引）
//
//  用法：
//    go run ./cmd/zizpanel-assets audit                 # 全部应用（在线）
//    go run ./cmd/zizpanel-assets audit --offline       # 只跑静态不变量（不联网）
//    go run ./cmd/zizpanel-assets audit --only squoosh  # 只审一个（加新应用时用）
//    go run ./cmd/zizpanel-assets audit --json          # 机器可读
//    go run ./cmd/zizpanel-assets audit --fast          # 跳过"下载整包算 sha256"
// ============================================================================

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ---------------------------------------------------------------------------
//  状态与结果结构
// ---------------------------------------------------------------------------

type auditStatus int

const (
	stOK auditStatus = iota
	stInfo
	stWarn
	stFail
)

func (s auditStatus) Mark() string {
	switch s {
	case stOK:
		return "✅"
	case stInfo:
		return "ℹ️"
	case stWarn:
		return "⚠️"
	}
	return "❌"
}

func (s auditStatus) Name() string {
	switch s {
	case stOK:
		return "ok"
	case stInfo:
		return "info"
	case stWarn:
		return "warn"
	}
	return "fail"
}

func worse(a, b auditStatus) auditStatus {
	if b > a {
		return b
	}
	return a
}

type auditItem struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type auditRow struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Status string      `json:"status"`
	Points int         `json:"points"`
	Items  []auditItem `json:"items"`
}

type auditReport struct {
	GeneratedAt string            `json:"generated_at"`
	Mirror      string            `json:"mirror"`
	MirrorFrom  string            `json:"mirror_from"`
	Fast        bool              `json:"fast"`
	Static      []string          `json:"static_problems"`
	Rows        []auditRow        `json:"rows"`
	Summary     map[string]int    `json:"summary"`
	Hints       map[string]string `json:"hints,omitempty"`
}

// ---------------------------------------------------------------------------
//  入口
// ---------------------------------------------------------------------------

func runAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		only    = fs.String("only", "", "只审这几个应用（逗号分隔的 ID）")
		asJSON  = fs.Bool("json", false, "输出 JSON（便于机器处理）")
		offline = fs.Bool("offline", false, "只跑静态不变量，不联网")
		fast    = fs.Bool("fast", false, "跳过需要下载整包的 sha256 比对")
		mirror  = fs.String("mirror", "", "镜像站基址（默认从面板配置/环境变量取）")
		timeout = fs.Duration("timeout", 10*time.Second, "单个网络请求的超时")
		workers = fs.Int("workers", 6, "并发探测数")
		quiet   = fs.Bool("quiet", false, "只输出失败项与汇总")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法：zizpanel-assets audit [选项]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	opts := auditOpts{
		asJSON: *asJSON, offline: *offline, fast: *fast,
		timeout: *timeout, workers: *workers, quiet: *quiet,
	}
	if strings.TrimSpace(*only) != "" {
		for _, id := range strings.Split(*only, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := services.MarketAppFor(id); !ok {
				fmt.Fprintf(os.Stderr, "未知应用 ID %q；可用：%s\n", id, strings.Join(services.MarketIDs(), ", "))
				return 2
			}
			opts.only = append(opts.only, id)
		}
	}

	rep := auditReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Static:      services.MarketInvariantProblems(),
		Summary:     map[string]int{},
		Hints:       map[string]string{},
	}
	sort.Strings(rep.Static)

	apps := marketAppsToAudit(opts.only)

	if opts.offline {
		// 静态门禁：不联网。问题清单就是结论。
		rep.Mirror = "(未探测：--offline)"
		for _, m := range apps {
			row := auditRow{ID: m.ID, Name: marketAppName(m.ID), Points: len(m.Downloads), Status: stOK.Name()}
			if len(rep.Static) > 0 {
				row.Status = stWarn.Name()
			}
			rep.Rows = append(rep.Rows, row)
		}
		rep.Summary["apps"] = len(apps)
		rep.Summary["static_problems"] = len(rep.Static)
		if opts.asJSON {
			writeJSON(rep)
		} else {
			printOffline(rep, opts.quiet)
		}
		if len(rep.Static) > 0 {
			return 1
		}
		return 0
	}

	base, from := resolveMirrorBase(*mirror)
	rep.Mirror, rep.MirrorFrom = base, from
	if base == "" {
		rep.Hints["mirror"] = "没有可用的镜像站基址 —— 用 --mirror 指定，或设置 ZIZPANEL_MIRROR_BASE"
	} else {
		rep.Hints["mirror"] = fmt.Sprintf("镜像站 %s（%s）；探测超时 %s、并发 %d", base, from, opts.timeout, opts.workers)
	}
	if opts.fast {
		rep.Hints["fast"] = "--fast：跳过了「下载镜像站上的整包实算 sha256」这一步"
	}

	a := &auditor{opts: opts, mirror: base, client: &http.Client{}}
	rep.Rows = a.auditAll(apps)
	rep.Summary = summarize(rep.Rows, len(rep.Static))

	// 真机验收档的指引（不自动跑：它会真的安装软件）
	rep.Hints["real_machine"] = "静态 + 在线审计**不能**替代真机装一遍（服务起不来只有真机能发现）：" +
		"python3 tools/market-install-verify.py <base_url> <user> <pass> --only <id>"

	if opts.asJSON {
		writeJSON(rep)
	} else {
		printReport(rep, opts.quiet)
	}
	if rep.Summary["fail"] > 0 || rep.Summary["warn"] > 0 || len(rep.Static) > 0 {
		return 1
	}
	return 0
}

func marketAppsToAudit(only []string) []services.MarketApp {
	all := services.MarketApps()
	if len(only) == 0 {
		return all
	}
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	out := make([]services.MarketApp, 0, len(only))
	for _, m := range all {
		if want[m.ID] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func marketAppName(id string) string {
	if app, ok := services.FindApp(id); ok {
		return app.Name
	}
	return id
}

func summarize(rows []auditRow, staticProblems int) map[string]int {
	s := map[string]int{"apps": len(rows), "static_problems": staticProblems}
	for _, r := range rows {
		s[r.Status]++
	}
	return s
}

// resolveMirrorBase 决定"审哪个镜像站"。
//
// 顺序：--mirror > $ZIZPANEL_MIRROR_BASE > 面板配置（/opt/zizpanel/data/config.json）
// > 局域网 NAS 默认值。返回值第二项是来源说明 —— 报告里必须写清楚"审的是哪一个"，
// 否则"审计全绿"可能只是审了一个空地址。
func resolveMirrorBase(flagVal string) (string, string) {
	clean := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
	if v := clean(flagVal); v != "" {
		return v, "--mirror"
	}
	if v := clean(os.Getenv("ZIZPANEL_MIRROR_BASE")); v != "" {
		return v, "环境变量 ZIZPANEL_MIRROR_BASE"
	}
	for _, p := range []string{"/opt/zizpanel/data/config.json"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			MirrorBase string `json:"mirror_base"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			if v := clean(cfg.MirrorBase); v != "" {
				return v, "面板配置 " + p
			}
		}
	}
	return "http://192.168.1.8:8090", "内置默认（局域网 NAS）"
}

// ---------------------------------------------------------------------------
//  审计器
// ---------------------------------------------------------------------------

type auditOpts struct {
	only    []string
	asJSON  bool
	offline bool
	fast    bool
	timeout time.Duration
	workers int
	quiet   bool
}

type auditor struct {
	opts   auditOpts
	mirror string
	client *http.Client
}

func (a *auditor) auditAll(apps []services.MarketApp) []auditRow {
	rows := make([]auditRow, len(apps))
	for i, m := range apps {
		rows[i] = auditRow{ID: m.ID, Name: marketAppName(m.ID), Points: len(m.Downloads), Status: stOK.Name()}
	}

	// 每个下载点一个探测器，全部并发跑（受 workers 限制）。
	type job struct {
		row int
		pt  services.MarketDownloadPoint
	}
	var jobs []job
	for i, m := range apps {
		for _, d := range m.Downloads {
			jobs = append(jobs, job{row: i, pt: d})
		}
	}

	sem := make(chan struct{}, max(1, a.opts.workers)) // Go 1.21+ 的内建 max
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, j := range jobs {
		// 静态项（每个下载点必然有）先放进去，保证即使网络全挂也能看到"声明了什么"。
		mu.Lock()
		rows[j.row].Items = append(rows[j.row].Items, staticItem(j.pt))
		mu.Unlock()

		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), a.opts.timeout*3)
			defer cancel()
			item := a.probePoint(ctx, j.pt)
			mu.Lock()
			rows[j.row].Items = append(rows[j.row].Items, item)
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	for i := range rows {
		sort.SliceStable(rows[i].Items, func(x, y int) bool {
			return statusFromName(rows[i].Items[x].Status) > statusFromName(rows[i].Items[y].Status)
		})
		worstStatus := stOK
		for _, it := range rows[i].Items {
			worstStatus = worse(worstStatus, statusFromName(it.Status))
		}
		rows[i].Status = worstStatus.Name()
		if worstStatus == stInfo {
			rows[i].Status = stOK.Name()
		}
	}
	return rows
}

func statusFromName(s string) auditStatus {
	switch s {
	case "ok":
		return stOK
	case "info":
		return stInfo
	case "warn":
		return stWarn
	case "fail":
		return stFail
	}
	return stWarn
}

// staticItem 把声明里"本来就该回答"的东西先摆出来：超时、arm64 证据、校验现状。
func staticItem(d services.MarketDownloadPoint) auditItem {
	it := auditItem{Name: d.Label, Kind: string(d.Purpose), Status: stOK.Name()}
	var parts []string
	if d.Timeout > 0 {
		parts = append(parts, fmt.Sprintf("超时 %s（声明）", d.Timeout))
	} else {
		it.Status = stWarn.Name()
		parts = append(parts, "**没有超时**："+d.TimeoutReason)
	}
	if d.Checksum.SHA256 != "" {
		parts = append(parts, "sha256 已声明（来源："+d.Checksum.Source+")")
	} else if d.Checksum.Asset != "" {
		parts = append(parts, "sha256 未声明；校验依据："+d.Checksum.Asset)
	} else {
		parts = append(parts, "sha256 未声明："+d.Checksum.Note)
	}
	parts = append(parts, "arm64："+d.ARM64)
	if d.Note != "" {
		parts = append(parts, d.Note)
	}
	it.Detail = strings.Join(parts, "；")
	return it
}

// ---------------------------------------------------------------------------
//  逐下载点探测
// ---------------------------------------------------------------------------

func (a *auditor) probePoint(ctx context.Context, d services.MarketDownloadPoint) auditItem {
	it := auditItem{Name: d.Label, Kind: string(d.Purpose), Status: stOK.Name()}
	var details []string
	var fixes []string
	st := stOK

	add := func(s auditStatus, format string, args ...any) {
		st = worse(st, s)
		details = append(details, fmt.Sprintf(format, args...))
	}

	// ---- NAS 缺口类：声明里已经承认的缺口，直接报，不用探测 ----
	if d.NAS.State == services.NASMissing {
		add(stFail, "NAS 缺口（声明里就是这么写的）：%s", d.NAS.Reason)
		fixes = append(fixes, fixHint(d))
	}
	if d.NAS.State == services.NASNotNeeded {
		add(stInfo, "NAS 不需要：%s", d.NAS.Reason)
	}

	// ---- 逐用途探测 ----
	switch d.Purpose {
	case services.MarketFetchBrewBottle:
		a.probeBrew(ctx, d, add)
	case services.MarketFetchDockerImage:
		a.probeDocker(ctx, d, add, &fixes)
	case services.MarketFetchReleaseBinary:
		a.probeReleaseBinary(ctx, d, add, &fixes)
	case services.MarketFetchVMImage:
		a.probeMirrorFile(ctx, d, d.NAS.Path, true, add, &fixes)
	case services.MarketFetchModelFile:
		a.probeMirrorFile(ctx, d, d.NAS.Path, false, add, &fixes)
	case services.MarketFetchCLT:
		a.probeCLT(ctx, d, add, &fixes)
	case services.MarketFetchSiteSource:
		a.probeUpstreamURL(ctx, d, add)
	case services.MarketFetchPipPackage:
		if d.NAS.State == services.NASMissing {
			a.probeUpstreamURL(ctx, d, add)
		}
	case services.MarketFetchBrewInstaller, services.MarketFetchBrewGit:
		a.probeUpstreamURL(ctx, d, add)
	}

	if st == stFail && len(fixes) == 0 {
		fixes = append(fixes, "见 docs/新增应用工作流.md 的「如果审计报 ❌ 怎么办」一节")
	}
	it.Status = st.Name()
	it.Detail = strings.Join(details, "；")
	it.Fix = strings.Join(fixes, "；")
	return it
}

// probeBrew 探镜像站的 /brew 对某个 formula 是否有清单。
func (a *auditor) probeBrew(ctx context.Context, d services.MarketDownloadPoint, add func(auditStatus, string, ...any)) {
	if a.mirror == "" || d.NAS.State != services.NASMirrored {
		return
	}
	url := a.mirror + "/brew/api/formula/" + d.Upstream.ID + ".json"
	code, size, took, err := a.head(ctx, url)
	switch {
	case err != nil:
		add(stFail, "镜像站 /brew 探测失败（%s）：%v", url, err)
	case code == 200:
		add(stOK, "镜像站 /brew 有 %s 的清单（%d B，%d ms）", d.Upstream.ID, size, took.Milliseconds())
	default:
		add(stFail, "镜像站 /brew 上没有 %s 的清单（HTTP %d，%s）", d.Upstream.ID, code, url)
	}
	add(stInfo, "上游：%s（由 brew 自己按 formula 解析，无单一 URL）", d.Upstream.Note)
}

// probeUpstreamURL 探一个上游 URL 的真实可达性。
func (a *auditor) probeUpstreamURL(ctx context.Context, d services.MarketDownloadPoint, add func(auditStatus, string, ...any)) {
	if d.Upstream.URL == "" {
		return
	}
	code, size, took, err := a.head(ctx, d.Upstream.URL)
	switch {
	case err != nil:
		// 上游是**回落源**：有镜像站时不致命，镜像站不可达时这一步就装不上。
		// 所以报 ⚠️ 而不是 ❌ —— 但绝不当成"通过"（2026-09-16 实测本机到 GitHub
		// 完全不通：curl 20 s 一个字节都没有）。
		add(stWarn, "上游回落源不可达：%s（%v）→ 此时**只能**靠镜像站", d.Upstream.URL, err)
	case code >= 400 && code < 500:
		add(stFail, "上游返回 HTTP %d（客户端错误，回落源已失效）：%s", code, d.Upstream.URL)
	case code >= 500:
		add(stWarn, "上游返回 HTTP %d（服务端错误，可能是暂时性）：%s", code, d.Upstream.URL)
	case code >= 200 && code < 400:
		if d.Upstream.Size > 0 && size > 0 && size != d.Upstream.Size {
			add(stWarn, "上游可达（HTTP %d，%d ms）但体积变了：实测 %d B，声明 %d B → 声明要更新",
				code, took.Milliseconds(), size, d.Upstream.Size)
		} else {
			add(stOK, "上游可达：HTTP %d，%d B，%d ms", code, size, took.Milliseconds())
		}
	default:
		add(stFail, "上游返回 HTTP %d：%s（%s）", code, d.Upstream.URL, took.Round(time.Millisecond))
	}
}

// probeMirrorFile 探镜像站上的一个静态文件（模型权重 / VM 镜像这类）。
func (a *auditor) probeMirrorFile(ctx context.Context, d services.MarketDownloadPoint, path string, withSHA512 bool,
	add func(auditStatus, string, ...any), fixes *[]string) {
	if a.mirror == "" {
		add(stWarn, "没有镜像站基址，跳过 NAS 探测")
		return
	}
	if path == "" {
		return
	}
	url := a.mirror + "/" + strings.TrimLeft(path, "/")
	code, size, took, err := a.head(ctx, url)
	switch {
	case err != nil:
		add(stFail, "镜像站上没有（%s）：%v", url, err)
		*fixes = append(*fixes, fixHint(d))
	case code == 200:
		msg := fmt.Sprintf("镜像站有：%d B，%d ms", size, took.Milliseconds())
		if d.Upstream.Size > 0 && size > 0 {
			if size == d.Upstream.Size {
				msg += "（与声明体积一致 ✅）"
			} else {
				msg += fmt.Sprintf("（⚠️ 声明体积 %d B，不一致）", d.Upstream.Size)
				add(stWarn, "%s", msg)
				msg = ""
			}
		}
		if msg != "" {
			add(stOK, "%s", msg)
		}
		// 真实吞吐（Range GET 前 1 MiB）：只在大文件上做，小文件没意义。
		if size > 1<<20 {
			got, speed, rerr := a.rangeSpeed(ctx, url, 1<<20)
			if rerr == nil {
				add(stOK, "实测吞吐 %.1f MB/s（Range GET %d B）", speed/1e6, got)
			} else {
				add(stWarn, "Range GET 测速失败：%v", rerr)
			}
		}
	default:
		add(stFail, "镜像站上返回 HTTP %d（%s）", code, url)
		*fixes = append(*fixes, fixHint(d))
	}
	if withSHA512 {
		code, size, _, err := a.head(ctx, url+".sha512sum")
		switch {
		case err != nil || code != 200:
			add(stWarn, "镜像站上没有 %s.sha512sum（%v/HTTP %d）→ 预热代码只能跳过校验", url, err, code)
		default:
			add(stOK, "镜像站有 .sha512sum（%d B）", size)
		}
	}
	a.probeUpstreamURL(ctx, d, add)
}

// probeCLT 探 CLT 清单，并顺着清单里的 path+pkgs 去探**真实载荷**。
//
// 为什么不能只看 index.json：清单在、载荷不在是本仓库真实出现过的情况
// （NAS 上 zizpanel/clt/072-44426-A/ 是空目录）。只探清单会给出一个假的绿。
func (a *auditor) probeCLT(ctx context.Context, d services.MarketDownloadPoint,
	add func(auditStatus, string, ...any), fixes *[]string) {
	if a.mirror == "" {
		add(stWarn, "没有镜像站基址，跳过 NAS 探测")
		return
	}
	// CLT 清单在镜像站上有两种布局（<base>/clt/index.json 与
	// <base>/zizpanel/clt/index.json，见 services 里的 cltMirrorSubdirs）。
	// 这里按真实代码的顺序逐个探，用**探通的那个前缀**去拼载荷地址 —— 而不是
	// 拿清单里的相对 path 直接拼镜像根：那正是 2026-09-16 清点报告里
	// "整套包都在、代码却每次都去探 404、于是静默跳过 NAS" 的同一个坑。
	var prefix, found string
	for _, sub := range []string{"", "/zizpanel"} {
		u := a.mirror + sub + "/clt/index.json"
		if code, _, _, err := a.head(ctx, u); err == nil && code == 200 {
			prefix, found = a.mirror+sub, u
			break
		}
	}
	if prefix == "" {
		add(stFail, "镜像站的 CLT 清单在两种布局下都不可用（%s/clt/index.json、%s/zizpanel/clt/index.json）",
			a.mirror, a.mirror)
		*fixes = append(*fixes, fixHint(d))
		a.probeUpstreamURL(ctx, d, add)
		return
	}
	add(stOK, "CLT 清单可用：%s", found)
	body, gerr := a.getText(ctx, found, 1<<20)
	if gerr != nil {
		add(stWarn, "CLT 清单取到了但读不出来：%v", gerr)
		return
	}
	var idx struct {
		Items []struct {
			Name  string   `json:"name"`
			Path  string   `json:"path"`
			MaxOS int      `json:"max_os"`
			Bytes int64    `json:"bytes"`
			Pkgs  []string `json:"pkgs"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &idx) != nil || len(idx.Items) == 0 || idx.Items[0].Path == "" || len(idx.Items[0].Pkgs) == 0 {
		add(stWarn, "CLT 清单能取到但结构不认识（items=%d）", len(idx.Items))
		return
	}
	cltPath, cltPkgs := idx.Items[0].Path, idx.Items[0].Pkgs
	add(stOK, "CLT 清单可用（%s：path=%s，max_os=%d，%d 个包，%d B）",
		idx.Items[0].Name, cltPath, idx.Items[0].MaxOS, len(cltPkgs), idx.Items[0].Bytes)
	pkgURL := strings.TrimSuffix(prefix, "/") + "/" + strings.Trim(cltPath, "/") + "/" + cltPkgs[0]
	pcode, psize, ptook, perr := a.head(ctx, pkgURL)
	switch {
	case perr != nil:
		add(stFail, "CLT 载荷探不到：%s（%v）", pkgURL, perr)
		*fixes = append(*fixes, fixHint(d))
	case pcode == 200:
		// ⚠️ 200 不等于"已经预置在 NAS 本地"：镜像站的 @pull 回退会现拉现给。
		add(stInfo, "CLT 载荷 %s：HTTP 200，%d B（%d ms）。注意：镜像站有 @pull 回退，"+
			"200 **不等于**已本地预置 —— 若要比吞吐，看下面的测速", cltPkgs[0], psize, ptook.Milliseconds())
		if psize > 1<<20 {
			if _, speed, rerr := a.rangeSpeed(ctx, pkgURL, 1<<20); rerr == nil {
				add(stOK, "CLT 载荷实测吞吐 %.1f MB/s（局域网应有几十 MB/s；只有几百 KB/s 说明是回源拉的）", speed/1e6)
			} else {
				add(stWarn, "CLT 载荷测速失败：%v（体积对得上，但没量到速度）", rerr)
			}
		}
	default:
		add(stFail, "CLT 载荷 %s 返回 HTTP %d", cltPkgs[0], pcode)
		*fixes = append(*fixes, fixHint(d))
	}
	a.probeUpstreamURL(ctx, d, add)
}

// releaseManifest 与 NAS 上的 manifest.json 对应。
type releaseManifest struct {
	App     string `json:"app"`
	Version string `json:"version"`
	Assets  []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"assets"`
}

// probeReleaseBinary 探 release 二进制：NAS 包 + manifest + 三方 sha256 比对 + 上游。
func (a *auditor) probeReleaseBinary(ctx context.Context, d services.MarketDownloadPoint,
	add func(auditStatus, string, ...any), fixes *[]string) {
	var nasURL string
	if a.mirror != "" && d.NAS.State == services.NASMirrored {
		nasURL = a.mirror + "/" + strings.TrimLeft(d.NAS.Path, "/")
		code, size, took, err := a.head(ctx, nasURL)
		switch {
		case err != nil:
			add(stFail, "镜像站上没有该包：%s（%v）", nasURL, err)
			*fixes = append(*fixes, fixHint(d))
			nasURL = ""
		case code == 200:
			if d.Upstream.Size > 0 && size > 0 && size != d.Upstream.Size {
				add(stWarn, "镜像站上的包体积是 %d B，声明/清点是 %d B —— 可能同步了别的版本", size, d.Upstream.Size)
			} else {
				add(stOK, "镜像站有该包：%d B，%d ms", size, took.Milliseconds())
			}
		default:
			add(stFail, "镜像站上的包返回 HTTP %d：%s", code, nasURL)
			*fixes = append(*fixes, fixHint(d))
			nasURL = ""
		}
	}

	// manifest.json：镜像模式下 sha256 的唯一来源。
	manifestSHA := ""
	if nasURL != "" {
		dir := nasURL[:strings.LastIndex(nasURL, "/")]
		body, err := a.getText(ctx, dir+"/manifest.json", 1<<20)
		if err != nil {
			add(stWarn, "镜像站上没有可读的 manifest.json（%v）→ 安装器回落公网源、没有内容校验", err)
		} else {
			var mm releaseManifest
			if json.Unmarshal(body, &mm) != nil {
				add(stWarn, "镜像站的 manifest.json 不是合法 JSON")
			} else {
				for _, as := range mm.Assets {
					if as.Name == d.Upstream.Asset {
						manifestSHA = strings.ToLower(as.SHA256)
						add(stOK, "manifest.json 声明 sha256=%s（%d B）", short(manifestSHA), as.Size)
					}
				}
				if manifestSHA == "" {
					add(stWarn, "manifest.json 里没有 %s 这一条", d.Upstream.Asset)
				}
			}
		}
	}

	// 期望值：声明里记录的 sha256（如果有）。
	want := strings.ToLower(d.Checksum.SHA256)
	if want != "" {
		add(stInfo, "声明里记录的期望 sha256=%s（来源：%s）", short(want), d.Checksum.Source)
	}

	// 上游 checksums.txt：ddns-go / frpc 这种"上游有清单"的，取完整值。
	upstreamSHA := ""
	if d.Checksum.UpstreamFile != "" && d.Upstream.Repo != "" {
		cu := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s",
			d.Upstream.Repo, d.Upstream.Tag, d.Checksum.UpstreamFile)
		body, err := a.getText(ctx, cu, 4<<20)
		if err != nil {
			add(stWarn, "取上游 %s 失败：%v（镜像站不可达时就没有任何内容校验）", d.Checksum.UpstreamFile, err)
		} else if sum := shaFromChecksumFile(body, d.Upstream.Asset); sum != "" {
			upstreamSHA = sum
			add(stOK, "上游 %s 声明 sha256=%s", d.Checksum.UpstreamFile, short(sum))
		} else {
			add(stWarn, "上游 %s 里没有 %s 这一行", d.Checksum.UpstreamFile, d.Upstream.Asset)
		}
	} else if d.Checksum.UpstreamFile == "" {
		add(stWarn, "上游**没有**校验清单（%s）—— 公网模式下这个包没有任何内容校验",
			firstNonEmpty(d.Checksum.Note, "声明里没有 UpstreamFile"))
	}

	// 三方比对（要下整包，--fast 跳过）。
	if nasURL != "" && !a.opts.fast {
		sum, n, err := a.downloadSHA256(ctx, nasURL, 512<<20)
		if err != nil {
			add(stWarn, "下载整包实算 sha256 失败：%v", err)
		} else {
			got := sum
			okAll := true
			if manifestSHA != "" && got != manifestSHA {
				okAll = false
				add(stFail, "镜像站上的**字节**与它自己的 manifest 不一致：实算 %s，manifest %s —— "+
					"这就是「镜像永不命中、静默回落慢源」那类事故", short(got), short(manifestSHA))
			}
			if want != "" && got != want {
				okAll = false
				add(stFail, "实算 sha256 %s ≠ 声明里记录的 %s", short(got), short(want))
			}
			if upstreamSHA != "" && got != upstreamSHA {
				okAll = false
				add(stFail, "实算 sha256 %s ≠ 上游 checksums 里的 %s", short(got), short(upstreamSHA))
			}
			if okAll {
				add(stOK, "三方 sha256 一致（实算 %s，%d B）", short(got), n)
			}
		}
	} else if nasURL != "" && a.opts.fast {
		add(stInfo, "--fast：跳过了下载整包实算 sha256（只比对了 manifest 与声明）")
	}
	if nasURL != "" && manifestSHA == "" && want == "" && upstreamSHA == "" {
		add(stWarn, "没有任何可信的期望 sha256 —— 审计报「未声明」而不是「通过」")
	}

	a.probeUpstreamURL(ctx, d, add)
}

// probeDocker 取真的 manifest，判断有没有 linux/arm64。
//
// 顺序：自建镜像站（Docker Hub 的 pull-through）→ 官方 registry（带 Bearer token）。
// 对 Docker Hub 镜像，自建站往往是唯一能通的那条路（registry-1.docker.io 实测被 DNS 污染）。
func (a *auditor) probeDocker(ctx context.Context, d services.MarketDownloadPoint,
	add func(auditStatus, string, ...any), fixes *[]string) {
	host, repo, tag := services.MarketImageRef(d.Upstream.ID)
	var errs []string

	if a.mirror != "" && host == "" {
		url := fmt.Sprintf("%s/docker/v2/%s/manifests/%s", a.mirror, repo, tag)
		if body, _, err := a.registryManifest(ctx, url, ""); err == nil {
			a.reportPlatforms(ctx, body, url, add)
			a.probeUpstreamURL(ctx, d, add)
			return
		} else {
			errs = append(errs, fmt.Sprintf("自建镜像站 %s：%v", url, err))
		}
	}
	endpoint := host
	if endpoint == "" {
		endpoint = "registry-1.docker.io"
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", endpoint, repo, tag)
	if body, _, err := a.registryManifest(ctx, url, endpoint); err == nil {
		a.reportPlatforms(ctx, body, url, add)
		a.probeUpstreamURL(ctx, d, add)
		return
	} else {
		errs = append(errs, fmt.Sprintf("%s：%v", url, err))
	}

	add(stFail, "取不到 manifest（%s）→ **无法证明它自带 linux/arm64**（铁律②）：%s",
		d.Upstream.ID, strings.Join(errs, "；"))
	*fixes = append(*fixes, fixHint(d))
	if a.mirror != "" && host != "" {
		add(stWarn, "自建镜像站的 /docker 只反代 Docker Hub，不覆盖 %s；要么直连可用，要么在镜像站加一层反代", host)
	}
}

// reportPlatforms 从 manifest 里读出平台列表并判断 arm64。
func (a *auditor) reportPlatforms(ctx context.Context, body []byte, src string,
	add func(auditStatus, string, ...any)) {
	var mf struct {
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &mf); err != nil {
		add(stWarn, "manifest 不是合法 JSON（%s）：%v", src, err)
		return
	}
	if len(mf.Manifests) > 0 {
		var plats []string
		hasARM := false
		for _, m := range mf.Manifests {
			p := m.Platform.OS + "/" + m.Platform.Architecture
			if m.Platform.Variant != "" && m.Platform.Variant != "v8" {
				p += "/" + m.Platform.Variant
			}
			plats = append(plats, p)
			if m.Platform.OS == "linux" && m.Platform.Architecture == "arm64" {
				hasARM = true
			}
		}
		if hasARM {
			add(stOK, "manifest 里有 linux/arm64（经 %s）；平台：%s", src, strings.Join(plats, ", "))
		} else {
			add(stFail, "manifest 里**没有 linux/arm64** —— 违反铁律②，不能上架；平台：%s",
				strings.Join(plats, ", "))
		}
		return
	}
	if mf.Config.Digest != "" {
		// 单架构 manifest：只能去问 config blob 里的 architecture。
		blobURL := src[:strings.LastIndex(src, "/manifests/")] + "/blobs/" + mf.Config.Digest
		b, err := a.getText(ctx, blobURL, 1<<20)
		if err != nil {
			add(stWarn, "单架构 manifest，且取不到 config blob（%v）→ 架构未验证", err)
			return
		}
		var cfg struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		}
		if json.Unmarshal(b, &cfg) != nil || cfg.Architecture == "" {
			add(stWarn, "config blob 里读不出 architecture → 架构未验证")
			return
		}
		if cfg.OS == "linux" && cfg.Architecture == "arm64" {
			add(stOK, "单架构 manifest，config 里是 linux/arm64（经 %s）", src)
		} else {
			add(stFail, "单架构 manifest，config 里是 %s/%s —— 不是 linux/arm64，违反铁律②", cfg.OS, cfg.Architecture)
		}
		return
	}
	add(stWarn, "manifest 既没有 manifests 列表也没有 config digest —— 结构不认识")
}

// registryManifest 取 registry 的 manifest，自动处理 Bearer token 挑战。
func (a *auditor) registryManifest(ctx context.Context, url, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", services.MarketImageAcceptHeader)
	req.Header.Set("User-Agent", "zizpanel-market-audit/1")
	res, err := a.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusUnauthorized {
		challenge := res.Header.Get("WWW-Authenticate")
		realm, service, scope := parseBearerChallenge(challenge)
		if realm == "" {
			return nil, res.StatusCode, fmt.Errorf("401 且 WWW-Authenticate 里没有 realm（%q）", challenge)
		}
		if scope == "" {
			scope = "repository:" + strings.TrimPrefix(pathAfterV2(url), "/") + ":pull"
		}
		tokenURL := realm + "?service=" + service + "&scope=" + scope
		tb, terr := a.getText(ctx, tokenURL, 1<<20)
		if terr != nil {
			return nil, res.StatusCode, fmt.Errorf("401 → 取 token 失败（%s）：%w", tokenURL, terr)
		}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(tb, &tok)
		token := tok.Token
		if token == "" {
			token = tok.AccessToken
		}
		if token == "" {
			return nil, res.StatusCode, fmt.Errorf("401 → token 响应里没有 token")
		}
		req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req2.Header.Set("Accept", services.MarketImageAcceptHeader)
		req2.Header.Set("User-Agent", "zizpanel-market-audit/1")
		req2.Header.Set("Authorization", "Bearer "+token)
		res2, err2 := a.client.Do(req2)
		if err2 != nil {
			return nil, 0, err2
		}
		defer func() { _ = res2.Body.Close() }()
		if res2.StatusCode != http.StatusOK {
			return nil, res2.StatusCode, fmt.Errorf("带 token 重试仍返回 HTTP %d", res2.StatusCode)
		}
		b, rerr := io.ReadAll(io.LimitReader(res2.Body, 4<<20))
		return b, res2.StatusCode, rerr
	}
	if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return body, res.StatusCode, err
}

func pathAfterV2(url string) string {
	i := strings.Index(url, "/v2/")
	if i < 0 {
		return ""
	}
	return url[i+4:]
}

func parseBearerChallenge(h string) (realm, service, scope string) {
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return "", "", ""
	}
	for _, kv := range splitChallenge(strings.TrimSpace(h[len("Bearer "):])) {
		k, v, _ := strings.Cut(kv, "=")
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			realm = v
		case "service":
			service = v
		case "scope":
			scope = v
		}
	}
	return realm, service, scope
}

// splitChallenge 按逗号切分 challenge 参数（值里可能有逗号的情况这里不考虑：
// token 服务的 realm/service/scope 都不含逗号）。
func splitChallenge(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
//  HTTP 小工具（每个请求都有超时；失败一律返回错误而不是零值）
// ---------------------------------------------------------------------------

func (a *auditor) head(ctx context.Context, url string) (code int, size int64, took time.Duration, err error) {
	cctx, cancel := context.WithTimeout(ctx, a.opts.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("User-Agent", "zizpanel-market-audit/1")
	start := time.Now()
	res, err := a.client.Do(req)
	if err != nil {
		return 0, 0, time.Since(start), err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	took = time.Since(start)
	// 有些服务器不收 HEAD（405/501）—— 退化成 Range GET 0-0。
	if res.StatusCode == http.StatusMethodNotAllowed || res.StatusCode == http.StatusNotImplemented {
		return a.rangeGet(ctx, url, 1)
	}
	return res.StatusCode, res.ContentLength, took, nil
}

// rangeGet 取前 n 字节（状态码 + 实际拿到的字节数）。
func (a *auditor) rangeGet(ctx context.Context, url string, n int64) (code int, got int64, took time.Duration, err error) {
	cctx, cancel := context.WithTimeout(ctx, a.opts.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	req.Header.Set("User-Agent", "zizpanel-market-audit/1")
	start := time.Now()
	res, err := a.client.Do(req)
	if err != nil {
		return 0, 0, time.Since(start), err
	}
	defer func() { _ = res.Body.Close() }()
	w, _ := io.Copy(io.Discard, io.LimitReader(res.Body, n))
	return res.StatusCode, w, time.Since(start), nil
}

// rangeSpeed 返回 (字节数, 字节/秒)。
func (a *auditor) rangeSpeed(ctx context.Context, url string, n int64) (int64, float64, error) {
	code, got, took, err := a.rangeGet(ctx, url, n)
	if err != nil {
		return 0, 0, err
	}
	if code != http.StatusOK && code != http.StatusPartialContent {
		return 0, 0, fmt.Errorf("HTTP %d", code)
	}
	if got == 0 || took <= 0 {
		return got, 0, nil
	}
	return got, float64(got) / took.Seconds(), nil
}

func (a *auditor) getText(ctx context.Context, url string, limit int64) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, a.opts.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zizpanel-market-audit/1")
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return body, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return body, nil
}

func (a *auditor) downloadSHA256(ctx context.Context, url string, limit int64) (string, int64, error) {
	cctx, cancel := context.WithTimeout(ctx, a.opts.timeout*12)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "zizpanel-market-audit/1")
	res, err := a.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(res.Body, limit))
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ---------------------------------------------------------------------------
//  输出
// ---------------------------------------------------------------------------

func printOffline(rep auditReport, quiet bool) {
	fmt.Printf("应用市场静态门禁（--offline，未联网）\n")
	fmt.Printf("应用 %d 个 / 问题 %d 条\n\n", rep.Summary["apps"], rep.Summary["static_problems"])
	if len(rep.Static) == 0 {
		fmt.Println("✅ 全部静态不变量通过：")
		fmt.Println("   · 每个下载点都声明了超时（或写清了为什么没有）")
		fmt.Println("   · 每个下载点都回答了「NAS 优先？」（mirrored / missing / not_needed + 理由）")
		fmt.Println("   · 每个下载点都有 arm64 证据与校验现状说明")
		fmt.Println("   · 每个应用都显式回答了服务语义（launchd / container / site / none）")
		fmt.Println("   · 声明与目录 Catalog() 逐字段一致，与 release 注册表一致")
		return
	}
	for _, p := range rep.Static {
		fmt.Printf("❌ %s\n", p)
	}
	if !quiet {
		fmt.Println("\n修法见 docs/新增应用工作流.md 的「如果审计报 ❌ 怎么办」。")
	}
}

func printReport(rep auditReport, quiet bool) {
	fmt.Printf("应用市场在线审计\n")
	fmt.Printf("  镜像站：%s（%s）\n", rep.Mirror, rep.MirrorFrom)
	for _, k := range sortedKeys(rep.Hints) {
		if k == "mirror" {
			continue
		}
		fmt.Printf("  %s：%s\n", k, rep.Hints[k])
	}
	fmt.Printf("  静态门禁：%s\n", staticLine(rep.Static))
	fmt.Println()
	fmt.Printf("%-18s %-4s %-6s %s\n", "应用", "状态", "下载点", "结论")
	fmt.Println(strings.Repeat("-", 100))
	for _, r := range rep.Rows {
		if quiet && (r.Status == "ok" || r.Status == "info") {
			continue
		}
		counts := map[string]int{}
		for _, it := range r.Items {
			counts[it.Status]++
		}
		verdict := fmt.Sprintf("✅ 全过（%d 项探测）", counts["ok"]+counts["info"])
		if r.Status != "ok" {
			var bits []string
			if counts["fail"] > 0 {
				bits = append(bits, fmt.Sprintf("❌ %d 项", counts["fail"]))
			}
			if counts["warn"] > 0 {
				bits = append(bits, fmt.Sprintf("⚠️ %d 项", counts["warn"]))
			}
			verdict = strings.Join(bits, " / ")
		}
		fmt.Printf("%-18s %-4s %-6d %s\n", r.ID, statusMark(r.Status), r.Points, verdict)
		if quiet {
			continue
		}
		for _, it := range r.Items {
			if it.Status == "ok" || it.Status == "info" {
				continue
			}
			fmt.Printf("    %s [%s] %s\n", statusMark(it.Status), it.Kind, it.Name)
			fmt.Printf("        %s\n", it.Detail)
			if it.Fix != "" {
				fmt.Printf("        → 建议：%s\n", it.Fix)
			}
		}
	}
	fmt.Println(strings.Repeat("-", 100))
	fmt.Printf("汇总：应用 %d / ❌ %d / ⚠️ %d / ✅ %d；静态问题 %d\n",
		rep.Summary["apps"], rep.Summary["fail"], rep.Summary["warn"], rep.Summary["ok"], len(rep.Static))
	if rep.Summary["fail"] > 0 || rep.Summary["warn"] > 0 || len(rep.Static) > 0 {
		fmt.Println("有缺口 → 退出码 1（这份输出本身就是「当前应用市场哪里不健康」的体检报告）")
	}
	if rep.Hints["real_machine"] != "" {
		fmt.Printf("\n真机验收（静态+在线审计**替代不了**）：\n  %s\n", rep.Hints["real_machine"])
	}
}

func staticLine(problems []string) string {
	if len(problems) == 0 {
		return "✅ 通过"
	}
	return fmt.Sprintf("❌ %d 条问题（完整清单见 --offline）", len(problems))
}

func statusMark(s string) string { return statusFromName(s).Mark() }

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeJSON(rep auditReport) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}

func shaFromChecksumFile(body []byte, asset string) string {
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		name := strings.TrimPrefix(f[len(f)-1], "*")
		if name == asset || strings.HasSuffix(name, "/"+asset) {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12] + "…"
}

// fixHint 按用途给出"缺了该怎么做"。
func fixHint(d services.MarketDownloadPoint) string {
	switch d.Purpose {
	case services.MarketFetchBrewBottle:
		return "确认镜像站 /brew 反代可用（NAS 侧 nginx 的 /brew → 中科大 homebrew-bottles 按需缓存）"
	case services.MarketFetchReleaseBinary:
		return "跑 tools/sync-nas-apps.sh 同步到 apps/<id>/<tag>/（含 manifest.json）"
	case services.MarketFetchVMImage:
		return "把 colima-core 的 guest 镜像与 .sha512sum 同步到 apps/colima-core/<ver>/（版本取自 colima 二进制，别写死）"
	case services.MarketFetchModelFile:
		return "把这一步要的文件同步到镜像站（上游：" + d.Upstream.ID + "）—— " +
			"iopaint 权重放 models/iopaint/，HF 模型由镜像站的 /hf 按需缓存"
	case services.MarketFetchDockerImage:
		return "确认 NAS 的 /docker pull-through 在跑；或先在 NAS 上 docker pull 一次预热"
	case services.MarketFetchSiteSource:
		return "把源码包同步到 apps/<id>/<ver>/ 并把候选顺序改成 镜像优先（当前代码只试 DownloadURL + MirrorURLs）"
	case services.MarketFetchCLT:
		return "把 index.json 里声明的 pkg 与分片预置到 zizpanel/clt/<build>/（现在只有清单、没有载荷）"
	case services.MarketFetchPipPackage:
		return "给镜像站加 pypi 反代（当前没有 /pypi 入口）"
	}
	return "见 docs/新增应用工作流.md"
}
