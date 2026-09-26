package rename

// rename_gate_test.go —— 批量改名规则引擎的唯一门禁。
//
// 为什么现有门禁抓不到：此前只有 `handleFileRename`（单文件改名），它把
// "目标已存在就报错"交给 files.Manager.Rename，仓库里**没有任何**规则引擎、
// 中文数字解析、通配符捕获、批量冲突检测或互换环的断言 ——
// `TestFileOpProgressGate` 覆盖的是复制/移动的进度，`TestVideoCompressGate`
// 覆盖的是压缩计划。所以"第二十集没变成第20集""A↔B 互换把两个文件都覆盖掉"
// "非法名字照改不误"这三类事故可以一路全绿。
//
// 一条门禁覆盖整类：① 中文数字（含负向对照）② 通配符/正则/字面替换
// ③ insert 三种位置 ④ delete / 替换为空 ⑤ episode 识别与补零
// ⑥ 冲突（目标已存在 + 批内撞名）⑦ 非法（/、空名、超长；负向对照=删掉校验必红）
// ⑧ 扩展名默认不动 ⑨ 互换环用 t.TempDir() 真跑 os.Rename 并核对内容。
// 全部走 t.TempDir()，不碰任何真实文件。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sub 取子测试里唯一一条计划项。
func sub(t *testing.T, res Result, name string) Item {
	t.Helper()
	for _, it := range res.Items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("计划里没有 %q", name)
	return Item{}
}

func newName(t *testing.T, name string, rules []Rule, opts Options) string {
	t.Helper()
	res := Plan([]string{name}, nil, rules, opts)
	it := sub(t, res, name)
	if it.Status == StatusInvalid {
		t.Fatalf("%s 被判非法：%s", name, it.Reason)
	}
	return it.NewName
}

func TestRenameRulesGate(t *testing.T) {
	// ① 中文数字（集数语境，默认）：表驱动 + 负向对照。
	t.Run("中文数字", func(t *testing.T) {
		cn := []Rule{{Type: RuleCNNum}}
		cases := []struct{ in, want string }{
			{"第十集.mp4", "第10集.mp4"},
			{"第二十集.mp4", "第20集.mp4"},
			{"第二十一集.mp4", "第21集.mp4"},
			{"第一百零二集.mkv", "第102集.mkv"},
			{"第十二集.mp4", "第12集.mp4"},
			{"第三十集.mp4", "第30集.mp4"},
			{"第三百二十集.mp4", "第320集.mp4"},
			{"第二十话.mp4", "第20话.mp4"},
			{"S02E二十.mp4", "S02E20.mp4"},
		}
		for _, c := range cases {
			if got := newName(t, c.in, cn, Options{}); got != c.want {
				t.Errorf("cnnum(%s) = %s，要 %s", c.in, got, c.want)
			}
		}
		// 负向对照 A：不是集数语境 ⇒ 默认一个字都不动（标题里的中文数字很常见）。
		for _, in := range []string{"三百二十个文件.txt", "三只小猪.mp4", "我的三百二十个朋友.mp4"} {
			if got := newName(t, in, cn, Options{}); got != in {
				t.Errorf("非集数语境 %s 被改成了 %s —— 默认范围必须是 episode", in, got)
			}
		}
		// 负向对照 B：显式 all 才全名替换。
		if got := newName(t, "三百二十个文件.txt", cn, Options{CNNumScope: ScopeAll}); got != "320个文件.txt" {
			t.Errorf("all 范围应替换全部：得到 %s", got)
		}
		// 负向对照 C：含万/亿的数字解析不了 ⇒ 原样保留，绝不猜错。
		for _, in := range []string{"第一万集.mp4", "第二亿集.mp4"} {
			if got := newName(t, in, cn, Options{}); got != in {
				t.Errorf("%s 被改成了 %s —— 解析不了就必须原样保留", in, got)
			}
		}
		// ParseCN 边界直接断言（单位前的 1 省略 / 逐位读法）。
		for _, c := range []struct {
			in   string
			want int
		}{{"十", 10}, {"十二", 12}, {"二十", 20}, {"两", 2}, {"两百", 200}, {"一千零二十", 1020}, {"二〇二", 202}, {"零", 0}} {
			got, ok := ParseCN(c.in)
			if !ok || got != c.want {
				t.Errorf("ParseCN(%s) = %d,%v，要 %d", c.in, got, ok, c.want)
			}
		}
		if _, ok := ParseCN("一万"); ok {
			t.Error("ParseCN(一万) 不该成功 —— 不支持的单位必须返回 false")
		}
	})

	// ② 替换：literal / wildcard（* ? + $1）/ regex / 替换为空=删除。
	t.Run("替换与通配符", func(t *testing.T) {
		if got := newName(t, "a b c.txt", []Rule{{Type: RuleReplace, Find: " ", Replace: "_"}}, Options{}); got != "a_b_c.txt" {
			t.Errorf("literal 全替换失败：%s", got)
		}
		// 通配符整名匹配，$1 是第一个 * 的捕获。
		if got := newName(t, "第20集.mp4", []Rule{{Type: RuleReplace, Mode: ModeWildcard, Find: "第*集", Replace: "EP$1"}}, Options{}); got != "EP20.mp4" {
			t.Errorf("通配符 * + $1 失败：%s", got)
		}
		// ? 单字符（每个 ? 一个捕获组，$2 取第二个）。
		if got := newName(t, "IMG_7.mp4", []Rule{{Type: RuleReplace, Mode: ModeWildcard, Find: "IMG_?", Replace: "PIC_$1"}}, Options{}); got != "PIC_7.mp4" {
			t.Errorf("通配符 ? + $1 失败：%s", got)
		}
		if got := newName(t, "S01E20.mkv", []Rule{{Type: RuleReplace, Mode: ModeWildcard, Find: "*E??", Replace: "$1-$2$3"}}, Options{}); got != "S01-20.mkv" {
			t.Errorf("通配符多捕获失败：%s", got)
		}
		// 正则（子串）+ $1。
		if got := newName(t, "剧名 第20集.mp4", []Rule{{Type: RuleReplace, Mode: ModeRegex, Find: `第([0-9]+)集`, Replace: "EP$1"}}, Options{}); got != "剧名 EP20.mp4" {
			t.Errorf("正则 + $1 失败：%s", got)
		}
		// 替换为空 = 删除。
		if got := newName(t, "abcXYZ.mp4", []Rule{{Type: RuleReplace, Find: "XYZ", Replace: ""}}, Options{}); got != "abc.mp4" {
			t.Errorf("替换为空应删除：%s", got)
		}
		// 删除预设：正则去掉 [组名] 标签。
		if got := newName(t, "[组名] 第三十集.mp4", []Rule{{Type: RuleReplace, Mode: ModeRegex, Find: `\[[^\]]*\]\s*`, Replace: ""}}, Options{}); got != "第三十集.mp4" {
			t.Errorf("删标签失败：%s", got)
		}
	})

	// ③ delete 规则（带模式）④ insert 三种位置。
	t.Run("删除与插入", func(t *testing.T) {
		if got := newName(t, "abc-def.mp4", []Rule{{Type: RuleDelete, Find: "-def"}}, Options{}); got != "abc.mp4" {
			t.Errorf("delete 失败：%s", got)
		}
		if got := newName(t, "x123y.mp4", []Rule{{Type: RuleDelete, Mode: ModeRegex, Find: `[0-9]+`}}, Options{}); got != "xy.mp4" {
			t.Errorf("delete 正则失败：%s", got)
		}
		ins := []struct {
			rule Rule
			want string
		}{
			{Rule{Type: RuleInsert, Position: PosPrefix, Text: "P_"}, "P_abc.mp4"},
			{Rule{Type: RuleInsert, Position: PosSuffix, Text: "_S"}, "abc_S.mp4"},
			{Rule{Type: RuleInsert, Position: PosAfter, At: 1, Text: "-"}, "a-bc.mp4"},
		}
		for _, c := range ins {
			if got := newName(t, "abc.mp4", []Rule{c.rule}, Options{}); got != c.want {
				t.Errorf("insert %+v = %s，要 %s", c.rule, got, c.want)
			}
		}
		// insert after 用 rune 计数，中文不会被劈成半个字节。
		if got := newName(t, "第三十集.mp4", []Rule{{Type: RuleInsert, Position: PosAfter, At: 1, Text: "-"}}, Options{}); got != "第-三十集.mp4" {
			t.Errorf("insert after 中文按字节切了：%s", got)
		}
	})

	// ⑤ episode：识别 + 补零宽度。
	t.Run("集数重写", func(t *testing.T) {
		ep := func(width int) []Rule { return []Rule{{Type: RuleEpisode, Prefix: "EP", Width: width}} }
		cases := []struct{ in, want string }{
			{"第二十集.mp4", "EP20.mp4"},
			{"第20集.mp4", "EP20.mp4"},
			{"EP20.mp4", "EP20.mp4"},
			{"E20.mp4", "EP20.mp4"},
			{"20集.mp4", "EP20.mp4"},
			{"第二十话.mp4", "EP20.mp4"},
			{"[组名] 第三十集.mp4", "[组名] EP30.mp4"},
		}
		for _, c := range cases {
			if got := newName(t, c.in, ep(0), Options{}); got != c.want {
				t.Errorf("episode(%s) = %s，要 %s", c.in, got, c.want)
			}
		}
		if got := newName(t, "第十集.mp4", ep(3), Options{}); got != "EP010.mp4" {
			t.Errorf("episode 补零失败：%s", got)
		}
		if got := newName(t, "第二十集.mp4", ep(2), Options{}); got != "EP20.mp4" {
			t.Errorf("episode 宽度 2 失败：%s", got)
		}
		// 负向对照：没有集数片段 ⇒ 不动。
		if got := newName(t, "花絮.mp4", ep(0), Options{}); got != "花絮.mp4" {
			t.Errorf("无集数片段却被改：%s", got)
		}
	})

	// ⑥ 冲突：目标已存在 / 批内两条撞同一目标 / 目标会被本批腾空（互换）不算冲突。
	t.Run("冲突", func(t *testing.T) {
		rules := []Rule{{Type: RuleReplace, Find: "a", Replace: "b"}}
		// 目标已存在。
		res := Plan([]string{"a1.txt"}, []string{"a1.txt", "b1.txt"}, rules, Options{})
		if it := sub(t, res, "a1.txt"); it.Status != StatusConflict || res.Conflict != 1 {
			t.Errorf("目标已存在应判 conflict：%+v", it)
		}
		// 批内两条撞同一目标。
		res = Plan([]string{"a1.txt", "a2.txt"}, []string{"a1.txt", "a2.txt"},
			[]Rule{{Type: RuleReplace, Mode: ModeRegex, Find: `a[0-9]`, Replace: "b"}}, Options{})
		if res.Conflict != 2 {
			t.Errorf("批内撞名应两条都 conflict，得到 %+v", res.Items)
		}
		// 互换：目标 = 批内另一个源名，而那个源名自己也会被改走 ⇒ 不是冲突。
		// （统一规则列表下唯一能造出"目标被本批腾空"的形态是链式：a.txt→ab.txt→abb.txt。）
		res = Plan([]string{"a.txt", "ab.txt"}, []string{"a.txt", "ab.txt"},
			[]Rule{{Type: RuleInsert, Position: PosSuffix, Text: "b"}}, Options{})
		if res.Conflict != 0 || res.OK != 2 {
			t.Errorf("目标会被本批腾空时不该判冲突：%+v", res.Items)
		}
		if it := sub(t, res, "a.txt"); it.NewName != "ab.txt" {
			t.Errorf("链式计划算错：%+v", it)
		}
	})

	// ⑦ 非法：`/`、空名、超 255 字节。负向对照：删掉 ValidateName 的检查，这里必红。
	t.Run("非法名", func(t *testing.T) {
		res := Plan([]string{"a.txt"}, nil, []Rule{{Type: RuleReplace, Find: "a", Replace: "x/y"}}, Options{})
		if it := sub(t, res, "a.txt"); it.Status != StatusInvalid {
			t.Errorf("含 / 必须 invalid：%+v", it)
		}
		res = Plan([]string{"a.txt"}, nil, []Rule{{Type: RuleReplace, Find: "a", Replace: ""}}, Options{})
		if it := sub(t, res, "a.txt"); it.Status != StatusInvalid {
			t.Errorf("主名删空（只剩 .txt）必须 invalid：%+v", it)
		}
		res = Plan([]string{"a.txt"}, nil, []Rule{{Type: RuleInsert, Position: PosSuffix, Text: strings.Repeat("x", 300)}}, Options{})
		if it := sub(t, res, "a.txt"); it.Status != StatusInvalid {
			t.Errorf("超 255 字节必须 invalid：%+v", it)
		}
		// 目录里没有的名字 + 合法规则 ⇒ ok（判据有效性自检，别让上面三条测空气）。
		res = Plan([]string{"a.txt"}, nil, []Rule{{Type: RuleReplace, Find: "a", Replace: "b"}}, Options{})
		if it := sub(t, res, "a.txt"); it.Status != StatusOK {
			t.Errorf("合法改名应为 ok：%+v", it)
		}
	})

	// ⑧ 扩展名默认不动；include_ext 才作用到扩展名。
	t.Run("扩展名", func(t *testing.T) {
		rules := []Rule{{Type: RuleReplace, Find: "mp4", Replace: "mkv"}}
		if got := newName(t, "第20集.mp4", rules, Options{}); got != "第20集.mp4" {
			t.Errorf("默认不该动扩展名：%s", got)
		}
		if got := newName(t, "第20集.mp4", rules, Options{IncludeExt: true}); got != "第20集.mkv" {
			t.Errorf("include_ext 应改扩展名：%s", got)
		}
	})

	// ⑨ 互换环：真跑 os.Rename，断言内容都对上了、临时名不残留。
	t.Run("互换环", func(t *testing.T) {
		dir := t.TempDir()
		files := map[string]string{"a.txt": "AAA", "b.txt": "BBB", "c.txt": "CCC"}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// A→B、B→C、C→A（三元素环）。规则列表是统一作用于每一条的，没法
		// 用一条规则造出这个环，所以直接手写"规则引擎对每条算出的计划结果"，
		// 只验证 Apply 的临时名路径本身。
		plan := Result{
			Items: []Item{
				{Name: "a.txt", NewName: "b.txt", Status: StatusOK},
				{Name: "b.txt", NewName: "c.txt", Status: StatusOK},
				{Name: "c.txt", NewName: "a.txt", Status: StatusOK},
			},
			OK: 3,
		}
		out, err := Apply(dir, plan)
		if err != nil {
			t.Fatalf("Apply 互换环失败：%v", err)
		}
		if out.Succeeded != 3 || out.Failed != 0 {
			t.Fatalf("互换环应 3 成功：%+v", out)
		}
		want := map[string]string{"a.txt": "CCC", "b.txt": "AAA", "c.txt": "BBB"}
		for name, content := range want {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("读 %s 失败：%v", name, err)
			}
			if string(b) != content {
				t.Errorf("%s 内容 = %q，要 %q", name, b, content)
			}
		}
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".zp-rename-") {
				t.Errorf("临时名残留：%s", e.Name())
			}
		}
		// Apply 必须拒绝还有冲突/非法的计划（绝不改一半）。
		if _, err := Apply(dir, Result{Items: []Item{{Name: "x", NewName: "y", Status: StatusConflict}}}); err == nil {
			t.Error("带 conflict 的计划必须被 Apply 拒绝")
		}

		// 规则真的能造出"目标会被本批腾空"的链式计划，并安全执行。
		chain := t.TempDir()
		for name, content := range map[string]string{"a.txt": "A", "ab.txt": "AB"} {
			if err := os.WriteFile(filepath.Join(chain, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cplan := Plan([]string{"a.txt", "ab.txt"}, []string{"a.txt", "ab.txt"},
			[]Rule{{Type: RuleInsert, Position: PosSuffix, Text: "b"}}, Options{})
		if cout, err := Apply(chain, cplan); err != nil || cout.Succeeded != 2 {
			t.Fatalf("链式计划执行失败：%+v %v", cout, err)
		}
		for name, content := range map[string]string{"ab.txt": "A", "abb.txt": "AB"} {
			b, err := os.ReadFile(filepath.Join(chain, name))
			if err != nil || string(b) != content {
				t.Errorf("链式改名后 %s = %q（err=%v），要 %q", name, b, err, content)
			}
		}
	})

	// ⑩ 汇总计数不重不漏（跳过项曾被算两次 —— 隔离实例 E2E 抓到）。
	t.Run("汇总计数", func(t *testing.T) {
		dir := t.TempDir()
		for name, content := range map[string]string{"a.txt": "A", "b.txt": "B"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		plan := Plan([]string{"a.txt", "b.txt"}, []string{"a.txt", "b.txt"},
			[]Rule{{Type: RuleReplace, Find: "a", Replace: "c"}}, Options{})
		out, err := Apply(dir, plan)
		if err != nil {
			t.Fatalf("Apply 失败：%v", err)
		}
		if out.Succeeded != 1 || out.Skipped != 1 || out.Failed != 0 {
			t.Fatalf("汇总计数错（跳过被算两次？）：%+v", out)
		}
		if !strings.Contains(out.Summary, "成功 1 项") || !strings.Contains(out.Summary, "跳过 1 项") {
			t.Errorf("汇总文案与计数不一致：%s", out.Summary)
		}
	})

	// ⑪ 指纹随计划变化（apply 靠它拒绝过期计划）。
	t.Run("指纹", func(t *testing.T) {
		rules := []Rule{{Type: RuleReplace, Find: "a", Replace: "b"}}
		p1 := Plan([]string{"a.txt"}, nil, rules, Options{})
		p2 := Plan([]string{"a.txt"}, nil, rules, Options{})
		if p1.Fingerprint() != p2.Fingerprint() {
			t.Error("相同计划指纹应一致")
		}
		p3 := Plan([]string{"a.txt"}, []string{"a.txt", "b.txt"}, rules, Options{})
		if p1.Fingerprint() == p3.Fingerprint() {
			t.Error("目标已存在（status 变了）指纹必须变")
		}
	})
}
