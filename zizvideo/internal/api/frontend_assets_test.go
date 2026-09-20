package api_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 前端资产门禁（不碰真机，只读仓库里的 JS/CSS）。

func assetsDir() string { return filepath.Join("..", "web", "assets") }

func jsFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(assetsDir(), "js", "*.js"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("找不到前端 JS 资产")
	}
	return files
}

// 门禁：JS 用 classList 切换 .hidden，app.css 就必须真有 .hidden{display:none}。
// 现象3 的根因：缺这条规则 ⇒ chip「加载中…」/toast/设置面板 classList.add("hidden") 无效，永远显示。
func TestHiddenClassHasCSSRule(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(assetsDir(), "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	if !regexp.MustCompile(`(?m)^\s*\.hidden\s*\{[^}]*display\s*:\s*none`).MatchString(css) {
		t.Fatal("app.css 缺少 .hidden{display:none} —— classList 切换 .hidden 的元件会一直显示")
	}
	usedBy := ""
	for _, name := range jsFiles(t) {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), `classList.add("hidden")`) {
			usedBy = filepath.Base(name)
			break
		}
	}
	if usedBy == "" {
		t.Fatal("没有任何 JS 使用 classList.add(\"hidden\")，请同步删掉 app.css 的 .hidden 规则或说明原因")
	}
}

var spreadRef = regexp.MustCompile(`\.\.\.\s*([A-Za-z_$][A-Za-z0-9_$]*)`)

// 门禁：前端只允许展开 rest 参数或返回数组的方法。
// 现象1 的根因：entryRows 在空目录时返回单个 DOM 节点，被 ... 展开即
// “Spread syntax requires ...iterable[Symbol.iterator] to be a function”。
func TestFrontendSpreadOnlyArrays(t *testing.T) {
	for _, name := range jsFiles(t) {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, loc := range spreadRef.FindAllStringSubmatchIndex(text, -1) {
			id := text[loc[2]:loc[3]]
			if id == "children" { // dom.js 的 rest 参数
				continue
			}
			rest := text[loc[3]:]
			if strings.HasPrefix(rest, ".map(") || strings.HasPrefix(rest, ".filter(") {
				continue
			}
			t.Fatalf("%s 展开 ...%s 可能不是数组（单个节点/对象）—— 换遍历或用 asArray 收敛", filepath.Base(name), id)
		}
	}
}

// 门禁：接口字段当数组用之前必须显式收敛（Array.isArray / asArray）。
func TestFrontendArrayFieldsGuarded(t *testing.T) {
	cases := []struct {
		file   string
		tokens []string
	}{
		{"roots.js", []string{"asArray(entries)", "asArray(starts)", "entryRows"}},
		{"admin.js", []string{"asArray(list)", "asArray(group && group.members)", "asArray(data && data.groups)"}},
	}
	for _, c := range cases {
		raw, err := os.ReadFile(filepath.Join(assetsDir(), "js", c.file))
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		for _, token := range c.tokens {
			if !strings.Contains(body, token) {
				t.Fatalf("%s 缺少数组收敛 %q —— 接口返回非数组时会把异常抛给用户", c.file, token)
			}
		}
	}
}

// 门禁：后台「播放设置」必须就地展开并复用同一份设置语义与同一个 PATCH，不许跳去播放页。
func TestMePlaySettingsOpensInPlace(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(assetsDir(), "js", "me.js"))
	if err != nil {
		t.Fatal(err)
	}
	me := string(raw)
	if strings.Contains(me, `href: "#/feed"`) {
		t.Fatal(`me.js 的「播放设置」还在 href="#/feed" 跳走`)
	}
	for _, token := range []string{"play-settings.js", "createFeedSettingsForm", "patchFeedSettings"} {
		if !strings.Contains(me, token) {
			t.Fatalf("me.js 缺少 %q —— 播放设置没有复用共享实现", token)
		}
	}
	shared, err := os.ReadFile(filepath.Join(assetsDir(), "js", "play-settings.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shared), "seek_seconds") {
		t.Fatal("play-settings.js 没有共用 seek_seconds 字段语义")
	}
	feed, err := os.ReadFile(filepath.Join(assetsDir(), "js", "feed.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(feed), "createFeedSettingsForm") {
		t.Fatal("feed.js 的 ⚙ 没有复用 play-settings.js，语义会漂移")
	}
	// 两个入口都必须落到同一个 PATCH /api/v1/feed/settings。
	for _, f := range []struct{ name, body string }{{"me.js", me}, {"feed.js", string(feed)}} {
		if !strings.Contains(f.body, "patchFeedSettings") {
			t.Fatalf("%s 没有走 patchFeedSettings", f.name)
		}
	}
}
