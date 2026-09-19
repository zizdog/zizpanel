package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

func TestSearchToolsRegistered(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAll(reg)
	for _, w := range []struct {
		id    string
		async bool
	}{
		{"search.spotlight", false},
		{"search.metadata", false},
	} {
		m := metaOf(t, reg, w.id)
		if m.Category != "search" {
			t.Errorf("%s 分类应为 search，实际 %s", w.id, m.Category)
		}
		if m.Async != w.async {
			t.Errorf("%s async 应为 %v", w.id, w.async)
		}
		if len([]rune(m.Summary)) > 40 {
			t.Errorf("%s summary 超过 40 字：%q", w.id, m.Summary)
		}
	}
}

func TestSearchUnavailableWhenCommandsMissing(t *testing.T) {
	pathWithShims(t, nil)
	reg := tool.NewRegistry()
	RegisterAll(reg)
	for id, bin := range map[string]string{"search.spotlight": "mdfind", "search.metadata": "mdls"} {
		m := metaOf(t, reg, id)
		if m.Available {
			t.Errorf("%s 在无命令环境应不可用", id)
		}
		if !strings.Contains(m.UnavailableReason, bin) {
			t.Errorf("%s 原因应点出 %s，实际 %q", id, bin, m.UnavailableReason)
		}
	}
}

// TestSpotlightFiltersOutOfRange：mdfind 返回的越界/敏感路径必须逐条丢弃并计数。
func TestSpotlightFiltersOutOfRange(t *testing.T) {
	b := newBench(t)
	inside := filepath.Join(b.home, "hit-inside.txt")
	outside := filepath.Join(t.TempDir(), "hit-outside.txt")
	missing := filepath.Join(b.home, "hit-missing.txt")
	sshDir := filepath.Join(b.home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sshFile := filepath.Join(sshDir, "id_rsa")
	for _, p := range []string{inside, outside, sshFile} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pathWithShims(t, map[string]string{
		"mdfind": printfShim(inside, outside, sshFile, missing),
	})
	res, _ := b.run("search.spotlight", map[string]any{
		"query": "hit", "scope": "all", "kind": "any", "limit": 50})
	data := res.Data.(map[string]any)
	if data["scanned"].(int) != 4 {
		t.Fatalf("应扫描 4 条，实际 %v", data["scanned"])
	}
	if data["returned"].(int) != 1 {
		t.Fatalf("只有读根内的 1 条该返回，实际 %v", data["returned"])
	}
	if data["filtered"].(int) != 3 {
		t.Fatalf("越界/敏感/不存在共 3 条应被过滤，实际 %v", data["filtered"])
	}
	items := data["items"].([]map[string]any)
	if len(items) != 1 || items[0]["path"] != inside {
		t.Fatalf("保留项应为 %s，实际 %+v", inside, items)
	}
	if !strings.Contains(res.Msg, "已过滤 3 条越界结果") {
		t.Errorf("消息应报告过滤条数，实际 %q", res.Msg)
	}
}

// TestSpotlightArgsBuiltFromPresets：范围与类型只拼固定谓词，不带头部注入风险。
func TestSpotlightArgsBuiltFromPresets(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want []string
	}{
		{"all-any", map[string]any{"query": "cat", "scope": "all", "kind": "any"},
			[]string{`(kMDItemFSName == "*cat*"c || kMDItemTextContent == "*cat*"c)`}},
		{"subdir-image", map[string]any{"query": "cat", "scope": "subdir", "kind": "image"},
			[]string{"-onlyin", "HOME", `((kMDItemFSName == "*cat*"c || kMDItemTextContent == "*cat*"c)) && (kMDItemContentTypeTree == "public.image")`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t)
			capture := filepath.Join(t.TempDir(), "args.txt")
			pathWithShims(t, map[string]string{
				"mdfind": fmt.Sprintf("printf '%%s\\n' \"$@\" > %s", capture),
			})
			body := map[string]any{}
			for k, v := range c.body {
				body[k] = v
			}
			if body["scope"] == "subdir" {
				body["path"] = b.home
			}
			if _, code, err := b.runner.Run(t.Context(), "tester", "search.spotlight", body); err != nil {
				t.Fatalf("执行失败 code=%d: %v", code, err)
			}
			blob, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			got := splitOutputLines(string(blob))
			want := c.want
			for i := range want {
				if want[i] == "HOME" {
					want[i] = b.home
				}
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("mdfind 参数 = %v，期望 %v", got, want)
			}
		})
	}
}

func TestSpotlightSubdirRequiresPath(t *testing.T) {
	pathWithShims(t, map[string]string{"mdfind": "exit 0"})
	b := newBench(t)
	_, code, err := b.runner.Run(t.Context(), "tester", "search.spotlight",
		map[string]any{"query": "x", "scope": "subdir"})
	if err == nil || code != 500 || !strings.Contains(err.Error(), "请填写搜索目录") {
		t.Fatalf("选了指定目录却没给路径应报错，实际 code=%d err=%v", code, err)
	}
}

func TestSearchKindPredicateTable(t *testing.T) {
	for _, k := range []string{"video", "image", "audio", "document"} {
		if strings.TrimSpace(kindPredicates[k]) == "" {
			t.Errorf("类型 %s 缺谓词", k)
		}
	}
	if _, ok := kindPredicates["any"]; ok {
		t.Error("any 不该有谓词（不过滤）")
	}
}

func TestSearchMetadataWhitelist(t *testing.T) {
	b := newBench(t)
	f := filepath.Join(b.home, "probe.txt")
	if err := os.WriteFile(f, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("A", 5000)
	pathWithShims(t, map[string]string{
		"mdls": printfShim(
			`kMDItemFSName              = "probe.txt"`,
			"kMDItemFSSize              = 3",
			`kMDItemContentType         = "public.plain-text"`,
			`kMDItemTextContent         = "`+huge+`"`,
		),
	})
	res, _ := b.run("search.metadata", map[string]any{"path": f})
	vals := res.Data.(map[string]any)["values"].(map[string]any)
	if vals["kMDItemFSName"] != "probe.txt" {
		t.Errorf("名称解析错: %v", vals["kMDItemFSName"])
	}
	if vals["kMDItemContentType"] != "public.plain-text" {
		t.Errorf("类型解析错: %v", vals["kMDItemContentType"])
	}
	if _, leaked := vals["kMDItemTextContent"]; leaked {
		t.Error("超长正文键不该回显")
	}
	for _, v := range vals {
		if s, ok := v.(string); ok && len(s) > 301 {
			t.Errorf("白名单值超长未截断: %d", len(s))
		}
	}
}

func TestSearchMetadataSurfacesFailure(t *testing.T) {
	b := newBench(t)
	f := filepath.Join(b.home, "probe.txt")
	if err := os.WriteFile(f, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathWithShims(t, map[string]string{"mdls": "echo 'mdls: no such file' >&2\nexit 1"})
	code, err := b.runErr("search.metadata", map[string]any{"path": f})
	if err == nil || code != 500 {
		t.Fatalf("mdls 失败应 500，实际 code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("应回显真实 stderr，实际 %v", err)
	}
}

// TestSearchLiveReadOnly：真机跑一次真命令（不做任何写操作）。
func TestSearchLiveReadOnly(t *testing.T) {
	if _, ok := execx.LookPath("mdls"); !ok {
		t.Skip("本机没有 mdls")
	}
	b := newBench(t)
	f := filepath.Join(b.home, "live.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := b.run("search.metadata", map[string]any{"path": f})
	if res.Data.(map[string]any)["count"].(int) == 0 {
		t.Error("真机 mdls 至少该读到一个白名单字段")
	}
	if _, ok := execx.LookPath("mdfind"); !ok {
		t.Skip("本机没有 mdfind")
	}
	// 每种类型都要能生成 mdfind 认得的谓词（裸词 + && 会报 Failed to create query）。
	for _, kind := range []string{"any", "video", "image", "document", "audio"} {
		res2, _ := b.run("search.spotlight", map[string]any{
			"query": "msaber-no-such-term-9f8a7b6c", "scope": "subdir",
			"path": b.home, "kind": kind, "limit": 10})
		d2 := res2.Data.(map[string]any)
		if d2["returned"].(int) != 0 {
			t.Errorf("类型 %s：不存在的词不该有命中，实际 %v", kind, d2["returned"])
		}
	}
}

func TestSpotlightQuote(t *testing.T) {
	cases := map[string]string{
		"plain": "plain",
		`a"b`:   `a\"b`,
		`a\b`:   `a\\b`,
		`"`:     `\"`,
	}
	for in, want := range cases {
		if got := spotlightQuote(in); got != want {
			t.Errorf("spotlightQuote(%q) = %q，期望 %q", in, got, want)
		}
	}
}
