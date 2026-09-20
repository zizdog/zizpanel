package web

import (
	"regexp"
	"strings"
	"testing"
)

// ============================================================================
//  网站列表「一键停止 / 启动」按钮的门禁
//
//  需求：每一行都要能不进编辑弹窗就启停站点，语义必须与编辑弹窗的
//  enabled 开关逐字一致（停止 = enabled:false → 删 vhost + reload）。
//
//  这类缺陷的复发形态是"按钮加了、接口没接"或"只在某一种视图里加了"：
//  页面看起来有按钮，点下去无声无息 / 换个视图又没了。所以这里既钉住
//  **行级按钮 → api.siteUpdate(enabled) 的调用链**，也遍历所有站点行函数
//  钉住"每种视图都要有"，并禁止另造站点启停端点。
// ============================================================================

// TestSiteEveryListRowHasToggleButton 遍历 sites.js 的全部函数：凡是画
// **已注册站点行**的函数（以 domainLink(s) 为标记），都必须挂上 siteToggleButton(s)。
// 这样以后新增卡片/分组视图时漏加按钮会立刻变红，而不是等用户报障。
func TestSiteEveryListRowHasToggleButton(t *testing.T) {
	js := readAssetJS(t, "sites.js")
	funcs := jsFunctions(js)
	if len(funcs) == 0 {
		t.Fatal("sites.js 一个函数都没解析出来 —— 解析器猜错了，门禁等于没跑")
	}
	rows := 0
	for _, fn := range funcs {
		if !strings.Contains(fn.body, "domainLink(s)") {
			continue
		}
		rows++
		if !strings.Contains(fn.body, "siteToggleButton(s)") {
			t.Errorf("sites.js 的 %s() 画了站点行却没有 siteToggleButton(s) —— "+
				"网站列表的每一种视图、每一行都要有一键停止/启动", fn.name)
		}
	}
	if rows == 0 {
		t.Fatal("没有解析到任何站点行函数（domainLink(s)）—— 门禁等于没跑")
	}
}

// TestSiteRowIsTheOnlyListMapping 锁住列表数据只有 siteRow 这一条渲染路径。
// 多一条 map 路径而没挂按钮，就是"只加一处"的复发。
func TestSiteRowIsTheOnlyListMapping(t *testing.T) {
	js := readAssetJS(t, "sites.js")
	mustContain(t, "sites.js", js, "...list.map((s) => siteRow(s)),")
	if n := strings.Count(js, "=> siteRow("); n != 1 {
		t.Errorf("siteRow 的调用点应恰好 1 处（列表唯一渲染路径），实际 %d 处", n)
	}
}

// TestSiteToggleButtonFollowsEnabled 锁住按钮文案只看后端字段 s.enabled，
// 且点击真的接到 toggleSite（不是只画一颗点不动的按钮）。
func TestSiteToggleButtonFollowsEnabled(t *testing.T) {
	js := readAssetJS(t, "sites.js")
	fn := jsFunction(t, js, "siteToggleButton")
	for _, want := range []string{"s.enabled", "'停止'", "'启动'", "toggleSite(s)"} {
		if !strings.Contains(fn, want) {
			t.Errorf("siteToggleButton 缺少 %q —— 文案必须按 s.enabled 决定，且点击要接到 toggleSite", want)
		}
	}
}

// TestSiteToggleUsesEnabledUpdatePath 锁住「按钮存在但接口没接」会变红：
// toggleSite 必须走现成确认框 → api.siteUpdate 的 enabled → 回执后 load()，
// 失败贴后端原文，并且请求在飞时忽略重复点击。
func TestSiteToggleUsesEnabledUpdatePath(t *testing.T) {
	js := readAssetJS(t, "sites.js")
	fn := jsFunction(t, js, "toggleSite")

	if !strings.Contains(fn, "api.siteUpdate(s.domain, { enabled: enable })") {
		t.Error("toggleSite 没有调 api.siteUpdate(s.domain, { enabled: enable }) —— 按钮存在但接口没接")
	}
	if !strings.Contains(fn, "confirmBox(") {
		t.Error("toggleSite 没有走面板现成的 confirmBox —— 停止会下线站点，必须先二次确认")
	}
	if !strings.Contains(fn, "从 nginx 移除配置") || !strings.Contains(fn, "停止对外服务") {
		t.Error("停止确认文案必须写清『会从 nginx 移除配置、站点停止对外服务』")
	}
	if !strings.Contains(fn, "toast(e.message, 'err'") {
		t.Error("toggleSite 失败时没有把后端返回的原文 toast 出来（铁律 11：不许谎报成功）")
	}
	if !strings.Contains(fn, "await load()") {
		t.Error("toggleSite 成功后没有 await load() 刷新列表 —— 状态必须来自后端回执，不做乐观更新")
	}
	if !strings.Contains(fn, "if (togglePending.has(s.domain)) return;") {
		t.Error("toggleSite 没有防连点判据 —— 请求进行中重复点击会重复下线/上线")
	}
	// 反向：不许自己造 confirm/alert，也不许乐观翻转状态。
	if strings.Contains(fn, "window.confirm") || strings.Contains(fn, "alert(") {
		t.Error("toggleSite 自己造了确认框 —— 必须用面板统一的 confirmBox")
	}
}

// TestSiteToggleNoInventedEndpoint 锁住"启停只走 enabled 一条路"：
// api.js 的站点更新端点只有 POST /sites/{domain}，且任何前端文件都不得
// 出现站点专用的 /stop、/start、/enable、/disable 端点。
func TestSiteToggleNoInventedEndpoint(t *testing.T) {
	api := readAssetJS(t, "api.js")
	mustContain(t, "api.js", api,
		"siteUpdate: (domain, patch) => request('POST', `${API_BASE}/sites/${encodeURIComponent(domain)}`, patch)")

	re := regexp.MustCompile(`/sites/[^'"` + "`" + `\n]{0,120}/(?:stop|start|enable|disable)`)
	for _, name := range listAssetJS(t) {
		if m := re.FindString(readAssetJS(t, name)); m != "" {
			t.Errorf("%s 出现了站点启停专用端点 %q —— 必须复用 POST /api/v1/sites/{domain} 的 enabled 字段", name, m)
		}
	}
	sites := readAssetJS(t, "sites.js")
	for _, bad := range []string{"api.siteStop", "api.siteStart", "api.siteToggle", "api.siteEnable"} {
		if strings.Contains(sites, bad) {
			t.Errorf("sites.js 调用了不存在的 %s —— 启停只能走 api.siteUpdate 的 enabled", bad)
		}
	}
	// api.js 里也不许新登记站点启停端点（名字本身就是判据）。
	for _, bad := range []string{"siteStop:", "siteStart:", "siteEnable:", "siteDisable:"} {
		if strings.Contains(api, bad) {
			t.Errorf("api.js 新登记了站点启停端点 %q —— 必须复用 siteUpdate 的 enabled", bad)
		}
	}
}

// ---------------- JS 函数体解析（门禁必须贴着真实函数体，不靠整文件字符串） ----------------

type jsFuncDef struct{ name, body string }

func jsFunction(t *testing.T, src, name string) string {
	t.Helper()
	for _, fn := range jsFunctions(src) {
		if fn.name == name {
			return fn.body
		}
	}
	t.Fatalf("sites.js 里找不到函数 %s 的定义 —— 门禁等于没跑", name)
	return ""
}

// jsFunctions 解析 `function name(` / `async function name(` 的函数体；
// 花括号配对会跳过字符串、模板串与注释里的括号。
func jsFunctions(src string) []jsFuncDef {
	re := regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(`)
	out := []jsFuncDef{}
	for _, loc := range re.FindAllStringSubmatchIndex(src, -1) {
		body, ok := jsBraceBody(src, loc[1])
		if !ok {
			continue
		}
		out = append(out, jsFuncDef{name: src[loc[2]:loc[3]], body: body})
	}
	return out
}

func jsBraceBody(src string, from int) (string, bool) {
	i := strings.IndexByte(src[from:], '{')
	if i < 0 {
		return "", false
	}
	start := from + i
	depth := 0
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '\'', '"', '`':
			j = jsSkipString(src, j)
		case '/':
			if j+1 < len(src) && src[j+1] == '/' {
				j = jsSkipLine(src, j)
			} else if j+1 < len(src) && src[j+1] == '*' {
				j = jsSkipBlock(src, j)
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : j+1], true
			}
		}
	}
	return "", false
}

// jsSkipString 返回字符串字面量收尾引号的下标（转义跳过；模板串按整体跳过）。
func jsSkipString(src string, i int) int {
	q := src[i]
	for j := i + 1; j < len(src); j++ {
		if src[j] == '\\' {
			j++
			continue
		}
		if src[j] == q {
			return j
		}
	}
	return len(src)
}

func jsSkipLine(src string, i int) int {
	if n := strings.IndexByte(src[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(src)
}

func jsSkipBlock(src string, i int) int {
	if n := strings.Index(src[i+2:], "*/"); n >= 0 {
		return i + 2 + n + 1
	}
	return len(src)
}
