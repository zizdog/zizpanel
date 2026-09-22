package services

import (
	"context"
	"fmt"
	"testing"
)

// ============================================================================
//  卸载/忘记动作必须覆盖一个应用的全部身份键（坑 228）
//
//  只剩"当前标签"的删法会漏掉换过部署方式留下的旧标签记录：卸载后它不会消失，
//  而是以"残留卡片"再冒出来。
//  这条门禁遍历整个目录，对每个应用都造出"每个身份键各一条记录 + 一条只靠
//  展示名命中的记录 + 一条无关对照"，断言忘记动作把前两类清光、对照不受影响。
// ============================================================================

// TestAppIdentityLabelsCoverEveryDeclaredKey 身份键集合必须包含目录声明的一切写法。
func TestAppIdentityLabelsCoverEveryDeclaredKey(t *testing.T) {
	checked := 0
	for _, app := range Catalog() {
		got := map[string]bool{}
		for _, l := range AppIdentityLabels(app) {
			if l == "" {
				t.Errorf("%s 的身份键里有空串", app.ID)
			}
			if got[l] {
				t.Errorf("%s 的身份键 %q 重复了", app.ID, l)
			}
			got[l] = true
		}
		want := append([]string{app.ServiceLabel, app.AdoptLabel, app.ID, app.Name}, app.AliasLabels...)
		for _, k := range want {
			if k == "" {
				continue
			}
			if !got[k] {
				t.Errorf("%s 的身份键缺少 %q —— 卸载时它对应的记录会被漏掉", app.ID, k)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("一个身份键都没检查到 —— 门禁等于没跑")
	}
}

// TestForgetByLabelsRemovesEveryAppIdentity 遍历整个目录，逐应用验证忘记动作的覆盖面。
func TestForgetByLabelsRemovesEveryAppIdentity(t *testing.T) {
	m, repo := newTestManager(t)
	ctx := context.Background()
	controls, checked := 0, 0
	for i, app := range Catalog() {
		labels := AppIdentityLabels(app)
		if len(labels) == 0 {
			continue
		}
		want := 0
		// ① 每个身份键各一条记录（LaunchLabel/DisplayName 都写成那个键）。
		for j, k := range labels {
			name := fmt.Sprintf("zz-id-%d-%d", i, j)
			if err := repo.Create(ctx, &Service{
				Name: name, DisplayName: k, LaunchLabel: k, Kind: KindNative,
			}); err != nil {
				t.Fatalf("准备 %s 的身份记录失败: %v", app.ID, err)
			}
			want++
		}
		// ② 只靠"展示名"命中的记录（label 是目录没声明的旧标签，解析仍回到这个应用）。
		disp := fmt.Sprintf("zz-disp-%d", i)
		if err := repo.Create(ctx, &Service{
			Name: disp, DisplayName: app.Name, LaunchLabel: "zz.unknown." + app.ID, Kind: KindNative,
		}); err != nil {
			t.Fatalf("准备 %s 的展示名记录失败: %v", app.ID, err)
		}
		want++
		// ③ 对照：与这个应用无关的记录，一条都不许动。
		ctrl := fmt.Sprintf("zz-control-%d", i)
		if err := repo.Create(ctx, &Service{
			Name: ctrl, DisplayName: "无关服务", LaunchLabel: ctrl, Kind: KindNative,
		}); err != nil {
			t.Fatalf("准备对照记录失败: %v", err)
		}
		controls++

		got, err := m.ForgetByLabels(ctx, labels...)
		if err != nil {
			t.Fatalf("%s 的忘记动作失败: %v", app.ID, err)
		}
		if got != want {
			t.Errorf("%s：忘记动作删了 %d 条，期望 %d 条（身份键 %v）", app.ID, got, want, labels)
		}
		remaining, err := repo.List(ctx)
		if err != nil {
			t.Fatalf("读剩余记录失败: %v", err)
		}
		if len(remaining) != controls {
			t.Errorf("%s：忘记后只应剩 %d 条对照记录，实际 %d 条 —— 要么漏删了自己的记录，要么误删了无关记录",
				app.ID, controls, len(remaining))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("一个应用都没检查到 —— 门禁等于没跑")
	}
}
