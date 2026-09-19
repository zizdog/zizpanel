package store

import (
	"context"
	"testing"
)

// nav_test.go —— 导航页数据层的单测。不碰真实服务、不碰用户家目录
// （Store 全部指向 t.TempDir()）。
//
// 备份/恢复：navSchema 由 nav.go 的 init() 登记进 KnownTables()（否则含导航数据的
// 备份会被判成「程序不认识的表」而拒绝恢复）。这条不变量由同目录的
// schema_known_tables_test.go 用「遍历全目录抽建表语句」的门禁锁死，这里不重复。

func newNavTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := EnsureNavTables(context.Background(), st.DB()); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return st
}

func TestEnsureNavTablesIsIdempotent(t *testing.T) {
	st := newNavTestStore(t)
	ctx := context.Background()
	// 重复执行不能报错，也不能清数据。
	g, err := st.CreateNavGroup(ctx, "常用", -1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := EnsureNavTables(ctx, st.DB()); err != nil {
			t.Fatalf("第 %d 次 EnsureNavTables 失败: %v", i+1, err)
		}
	}
	list, err := st.ListNavGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != g.ID {
		t.Fatalf("重复建表后数据应原样保留，got=%+v", list)
	}
}

func TestNavGroupCRUDAndOrder(t *testing.T) {
	st := newNavTestStore(t)
	ctx := context.Background()

	a, err := st.CreateNavGroup(ctx, "影音", -1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateNavGroup(ctx, "工具", -1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Sort != 0 || b.Sort != 1 {
		t.Fatalf("追加创建应依次排到末尾，got a.sort=%d b.sort=%d", a.Sort, b.Sort)
	}

	if err := st.UpdateNavGroup(ctx, a.ID, "影音娱乐", -1); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetNavGroup(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "影音娱乐" || got.Sort != 0 {
		t.Fatalf("改名不应动顺序，got=%+v", got)
	}

	// 排序：把 b 放到前面
	if err := st.ReorderNavGroups(ctx, []int64{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListNavGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("排序未生效，got=%+v", list)
	}

	// 删除分组要连子项一起删
	if _, err := st.CreateNavItem(ctx, NavItem{GroupID: a.ID, Name: "电影", URL: "https://movie.example.com", Sort: -1}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNavGroup(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetNavGroup(ctx, a.ID); err != ErrNavNotFound {
		t.Fatalf("删除后应返回 ErrNavNotFound，got=%v", err)
	}
	left, err := st.ListNavItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("删组应级联删掉其站点，got=%+v", left)
	}
	if err := st.DeleteNavGroup(ctx, a.ID); err != ErrNavNotFound {
		t.Fatalf("重复删除应返回 ErrNavNotFound，got=%v", err)
	}
}

func TestNavItemCRUDAndReorderWithinGroup(t *testing.T) {
	st := newNavTestStore(t)
	ctx := context.Background()
	g1, _ := st.CreateNavGroup(ctx, "组一", -1)
	g2, _ := st.CreateNavGroup(ctx, "组二", -1)

	x, err := st.CreateNavItem(ctx, NavItem{
		GroupID: g1.ID, Name: "X", URL: "https://x.example.com", Icon: "🧭",
		Description: "第一个", OpenNewTab: true, Sort: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	y, err := st.CreateNavItem(ctx, NavItem{GroupID: g1.ID, Name: "Y", URL: "https://y.example.com", Sort: -1})
	if err != nil {
		t.Fatal(err)
	}
	z, err := st.CreateNavItem(ctx, NavItem{GroupID: g2.ID, Name: "Z", URL: "https://z.example.com", Sort: -1})
	if err != nil {
		t.Fatal(err)
	}
	if x.Sort != 0 || y.Sort != 1 {
		t.Fatalf("同组内应按追加顺序排，got x=%d y=%d", x.Sort, y.Sort)
	}
	if z.Sort != 0 {
		t.Fatalf("新组里的第一个站点应从 0 开始，got=%d", z.Sort)
	}

	// 只重排组一，组二不受影响
	if err := st.ReorderNavItems(ctx, []int64{y.ID, x.ID}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListNavItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].ID != y.ID || list[1].ID != x.ID || list[2].ID != z.ID {
		t.Fatalf("组内排序结果不对，got=%+v", list)
	}

	// 更新：改地址与打开方式
	if err := st.UpdateNavItem(ctx, NavItem{
		ID: x.ID, GroupID: g2.ID, Name: "X2", URL: "https://x2.example.com",
		Icon: "🌐", Description: "搬组了", OpenNewTab: false, Sort: -1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetNavItem(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Sort 传 -1 = 保持原值；x 刚被重排到组一的第 2 位，所以仍是 1。
	if got.Name != "X2" || got.GroupID != g2.ID || got.OpenNewTab || got.Sort != 1 {
		t.Fatalf("更新结果不对，got=%+v", got)
	}

	if err := st.DeleteNavItem(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNavItem(ctx, x.ID); err != ErrNavNotFound {
		t.Fatalf("重复删除应返回 ErrNavNotFound，got=%v", err)
	}
}

// TestNavReplaceAllRoundTrip 是「导入导出往返一致」的数据层半边：
// ReplaceNav 写入后再读出来必须逐字段相同（含 id 与顺序）。
func TestNavReplaceAllRoundTrip(t *testing.T) {
	st := newNavTestStore(t)
	ctx := context.Background()

	groups := []NavGroup{{ID: 7, Name: "生产力"}, {ID: 9, Name: "影音", Sort: 5}}
	items := []NavItem{
		{ID: 11, GroupID: 7, Name: "面板", URL: "https://panel.example.com", Icon: "🛠️", Description: "本机", OpenNewTab: true, Sort: 0},
		{ID: 12, GroupID: 7, Name: "邮箱", URL: "http://mail.example.com", OpenNewTab: false, Sort: 1},
		{ID: 13, GroupID: 9, Name: "电影", URL: "https://movie.example.com", Icon: "https://movie.example.com/logo.png", Sort: 0},
	}
	if err := st.ReplaceNav(ctx, groups, items); err != nil {
		t.Fatal(err)
	}
	gotG, err := st.ListNavGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotI, err := st.ListNavItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotG) != 2 || gotG[0].ID != 7 || gotG[1].ID != 9 {
		t.Fatalf("分组往返不一致：%+v", gotG)
	}
	if gotG[1].Sort != 5 {
		t.Fatalf("显式 sort 应被保留，got=%d", gotG[1].Sort)
	}
	if len(gotI) != 3 {
		t.Fatalf("站点数量不对：%+v", gotI)
	}
	for i, want := range items {
		g := gotI[i]
		if g.ID != want.ID || g.GroupID != want.GroupID || g.Name != want.Name ||
			g.URL != want.URL || g.Icon != want.Icon || g.OpenNewTab != want.OpenNewTab {
			t.Fatalf("第 %d 条往返不一致：want=%+v got=%+v", i, want, g)
		}
	}

	// 再替换成空：整份替换必须真的清空
	if err := st.ReplaceNav(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.ListNavGroups(ctx); len(l) != 0 {
		t.Fatalf("替换为空后分组应清空，got=%+v", l)
	}
	if l, _ := st.ListNavItems(ctx); len(l) != 0 {
		t.Fatalf("替换为空后站点应清空，got=%+v", l)
	}
}

func TestNavIDPreservedAfterDeleteAndReinsert(t *testing.T) {
	st := newNavTestStore(t)
	ctx := context.Background()
	// AUTOINCREMENT 的下一号不能被"显式 id 插入"弄乱：先建一条 id=1，
	// 再整份替换成 id=5，之后新建应拿到 >5 的号（否则会撞主键）。
	g, err := st.CreateNavGroup(ctx, "第一", -1)
	if err != nil || g.ID != 1 {
		t.Fatalf("首条应为 id=1，got=%+v err=%v", g, err)
	}
	if err := st.ReplaceNav(ctx, []NavGroup{{ID: 5, Name: "第五"}}, nil); err != nil {
		t.Fatal(err)
	}
	next, err := st.CreateNavGroup(ctx, "之后", -1)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID <= 5 {
		t.Fatalf("显式 id 写入后新建的 id 应大于 5（否则主键会撞），got=%d", next.ID)
	}
}

// TestNavSettingsAppearanceRoundTrip：外观设置的存取（含三件套的默认值）。
//
// 为什么默认值必须由**数据层**给出：独立别名页（未登录）直接读 /nav/data，
// 老库里的 nav_settings 没有 mode/theme/size 三行 —— 如果读出来是空串，
// 访客就会看到一套没有色系/尺寸的页面（或者前端得各自再补一次默认，容易走样）。
func TestNavSettingsAppearanceRoundTrip(t *testing.T) {
	st := newNavTestStore(t)
	ctx := context.Background()

	// 一条都没有：读出来是具体默认值，不是空串。
	set, err := st.NavSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if set.Mode != NavDefaultMode || set.Theme != NavDefaultTheme || set.Size != NavDefaultSize {
		t.Fatalf("默认外观三件套应为 %s/%s/%s，得到 %s/%s/%s",
			NavDefaultMode, NavDefaultTheme, NavDefaultSize, set.Mode, set.Theme, set.Size)
	}

	// 存一整套（含空标题 = 恢复默认的语义）。
	want := NavSettings{
		Title: "首页", Subtitle: "副标题", Accent: "#3b82f6",
		Background: "https://example.com/bg.jpg",
		Mode:       "dark", Theme: "sunset", Size: "l",
	}
	if err := st.SaveNavSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := st.NavSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("回读与保存不一致：\n got=%+v\nwant=%+v", got, want)
	}

	// 空标题/主题色/背景图是**有效操作**（恢复默认），三件套仍是明确取值。
	if err := st.SaveNavSettings(ctx, NavSettings{Mode: "auto", Theme: "neon", Size: "m"}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.NavSettings(ctx)
	if got.Title != "" || got.Accent != "" || got.Background != "" {
		t.Fatalf("清空后应回落到空串，得到 %+v", got)
	}
	if got.Mode != "auto" || got.Theme != "neon" || got.Size != "m" {
		t.Fatalf("清空后三件套应保持显式取值，得到 %+v", got)
	}
}
