package services

import (
	"context"
	"testing"

	"github.com/zizdog/zizpanel/internal/store"
)

// TestAppVersionDerivesFromEachRailsSource 锁住"版本号不许手写"这条：
// 每个轨的版本都取自它**唯一的事实源**，取不到就是空
// （空 = 面板这边没有版本真源，调用方必须说"未知"，不许当"已是最新"）。
//
// 为什么值得一条门禁：手写版本号会与事实源漂（上游换 tag 没人回来改），
// 症状是界面显示的版本与实际装的东西对不上 —— 那属于谎报。
func TestAppVersionDerivesFromEachRailsSource(t *testing.T) {
	// ① 独立产物：注册表写死的 Tag 就是版本。
	want := releaseBinaryApps["frpc"].Tag
	if want == "" {
		t.Fatal("前提错了：frpc 在注册表里应有写死的 Tag")
	}
	if got := AppVersion(App{ID: "frpc"}); got != want {
		t.Errorf("独立产物的版本应取注册表 Tag %q，实际 %q", want, got)
	}

	// ② 动态条目（zizvideo）：Tag 留空 ⇒ 真源在镜像索引，这里**必须**是空。
	if tag := releaseBinaryApps[ZizvideoAppID].Tag; tag != "" {
		t.Fatalf("前提错了：%s 应是 Dynamic 条目（Tag 留空），实际 %q", ZizvideoAppID, tag)
	}
	if got := AppVersion(App{ID: ZizvideoAppID}); got != "" {
		t.Errorf("动态条目的版本只能运行时读索引，静态派生必须是空，实际 %q", got)
	}

	// ③ brew：版本由 brew 决定，目录不 pin。
	if got := AppVersion(App{ID: "nginx", BrewFormula: "nginx"}); got != "" {
		t.Errorf("brew 条目不该 pin 版本（那会与 brew 漂），实际 %q", got)
	}

	// ④ compose：取第一个镜像的 tag。
	compose := App{ID: "uptime-kuma", Kind: KindCompose,
		ComposeYAML: "services:\n  app:\n    image: louislam/uptime-kuma:2\n"}
	if got := AppVersion(compose); got != "2" {
		t.Errorf("compose 版本应取镜像 tag 2，实际 %q", got)
	}

	// ⑤ `:latest` 不是版本：拿它当版本会把"两边都是 latest"判成已是最新，
	//    而镜像其实可能早就更新了 —— 如实返回空。
	latest := App{ID: "gitea", Kind: KindCompose, ComposeYAML: "services:\n  app:\n    image: gitea/gitea:latest\n"}
	if got := AppVersion(latest); got != "" {
		t.Errorf("`:latest` 不该被当成版本，实际 %q", got)
	}

	// ⑥ 整个目录都要能给出结论（空或非空都合法），且绝不 panic。
	for _, a := range Catalog() {
		_ = AppVersion(a)
	}
}

// TestRecordInstalledVersionWritesByLabel 锁住"已装版本落库"这条：
// 安装器手上确定有的只有 launchd 标签，记录必须按标签找到那一条并写进去；
// 标签对不上时**什么都不写**（绝不写到别人的记录上）。
func TestRecordInstalledVersionWritesByLabel(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repo := NewRepository(st)
	m := &Manager{repo: repo}
	ctx := context.Background()

	rec := &Service{Name: "frpc", DisplayName: "frp 客户端", Kind: KindNative, LaunchLabel: "com.zizdog.frpc"}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}

	m.RecordInstalledVersion(ctx, "com.zizdog.frpc", "v0.71.0")
	got, err := repo.Get(ctx, "frpc")
	if err != nil {
		t.Fatal(err)
	}
	if got.InstalledVersion != "v0.71.0" {
		t.Errorf("按标签应把版本写进那条记录，实际 %q", got.InstalledVersion)
	}

	// 标签对不上：不许动任何记录（写错记录比不写更糟）。
	m.RecordInstalledVersion(ctx, "com.zizdog.nobody", "v9.9.9")
	got2, _ := repo.Get(ctx, "frpc")
	if got2.InstalledVersion != "v0.71.0" {
		t.Errorf("标签不匹配时不该改任何记录，实际 %q", got2.InstalledVersion)
	}

	// 空版本 = 没有真源：不许把空串当成"已装 v''"，直接不写。
	m.RecordInstalledVersion(ctx, "com.zizdog.frpc", "")
	got3, _ := repo.Get(ctx, "frpc")
	if got3.InstalledVersion != "v0.71.0" {
		t.Errorf("空版本不该覆盖已记录的版本，实际 %q", got3.InstalledVersion)
	}
}
