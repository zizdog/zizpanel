package services

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================================
//  compose 记录的健康检查对齐（2026-09-17）
//
//  真机证据（mini，0.14.2）：portainer 的服务记录停在
//  port=9001 / health_url=http://127.0.0.1:9001/，而当时 9001 已经是
//  MinIO 的控制台 —— 面板健康列照样回 code=200，检查打的是**别的应用**。
//
//  根因：ReconcileHealthURLs 开头 `if s.LaunchLabel == "" { continue }`，
//  而 compose 记录从不写 LaunchLabel（install.go 的登记分支只写 ComposeFile），
//  所以 compose 应用永远不参与对齐；reconcileInstalledRecord 又只在字段为零值
//  时才补，非零旧值永不覆盖。
//
//  这四条测试锁住修复后的契约：对齐、幂等、下架清空、原生回归。
// ============================================================================

// composeCatalogAppForTest 从真实目录里挑一个"有健康路径、有端口"的 compose
// 条目当期望值。不硬编码端口/路径：目录改了测试跟着变，不会变成假绿。
func composeCatalogAppForTest(t *testing.T) App {
	t.Helper()
	for _, a := range Catalog() {
		if a.Kind == KindCompose && a.HealthPath != "" && a.WebPort() > 0 {
			return a
		}
	}
	t.Fatal("目录里没有可用于对齐测试的 compose 条目（需要有 HealthPath 与端口）")
	return App{}
}

// TestReconcileHealthURLsAlignsComposeRecord ①：
// 一条 compose 记录（LaunchLabel 为空）+ 目录里有对应条目 →
// 旧 port / health_url 被**覆盖**成目录的真实值（不是只在零值时才补）。
func TestReconcileHealthURLsAlignsComposeRecord(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()
	app := composeCatalogAppForTest(t)

	oldPort := app.WebPort() + 111
	oldHealth := fmt.Sprintf("http://127.0.0.1:%d/", oldPort) // 打的是"另一个应用"
	rec := &Service{
		Name: app.ID, DisplayName: app.Name, Kind: KindCompose,
		Port: oldPort, HealthURL: oldHealth,
		ComposeFile: filepath.Join(m.composeDir(), app.ID, "docker-compose.yml"),
		// LaunchLabel 故意留空：compose 记录本来就是这样，也是这条 bug 的根因。
	}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}

	n, err := m.ReconcileHealthURLs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("旧值应被覆盖（改动 1 条），实际 changed=%d", n)
	}

	got, err := repo.Get(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != app.WebPort() {
		t.Errorf("端口应被改成目录的真实值 %d，实际 %d", app.WebPort(), got.Port)
	}
	if want := healthURLFor(app); got.HealthURL != want {
		t.Errorf("健康地址应被改成目录的真实值 %q，实际 %q", want, got.HealthURL)
	}
}

// TestReconcileHealthURLsComposeIsIdempotent ②：
// 同样的记录再跑一次 → **不写库**（changed=0，updated_at 一字未动）。
//
// updated_at 是秒级精度，所以第一次对齐后跨过一个秒边界再跑第二次；
// 若实现偷偷写了库，updated_at 会变成新的秒 —— 用时间戳把"真的没写"钉死，
// 而不只是"值看起来还对"。
func TestReconcileHealthURLsComposeIsIdempotent(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()
	app := composeCatalogAppForTest(t)

	rec := &Service{
		Name: app.ID, DisplayName: app.Name, Kind: KindCompose,
		Port: app.WebPort() + 77, HealthURL: "http://127.0.0.1:9/",
	}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if n, err := m.ReconcileHealthURLs(ctx); err != nil || n != 1 {
		t.Fatalf("第一次对齐应改动 1 条，实际 changed=%d err=%v", n, err)
	}
	aligned, err := repo.Get(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 跨秒，让"又被写了一次"一定能从 updated_at 上看出来。
	time.Sleep(1100 * time.Millisecond)

	n2, err := m.ReconcileHealthURLs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("值已经对齐时第二次和解必须无改动，实际 changed=%d", n2)
	}
	again, err := repo.Get(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.UpdatedAt != aligned.UpdatedAt {
		t.Errorf("幂等时不该写库（mtime 不该被搅动）：%q → %q",
			aligned.UpdatedAt, again.UpdatedAt)
	}
	if again.Port != aligned.Port || again.HealthURL != aligned.HealthURL {
		t.Errorf("幂等时字段不该变：port %d→%d health %q→%q",
			aligned.Port, again.Port, aligned.HealthURL, again.HealthURL)
	}
}

// TestReconcileHealthURLsClearsComposeHealthWhenCatalogEntryGone ③：
// 一条 compose 记录 + 目录里**没有**对应条目（例如刚下架的 portainer / minio）→
// health_url 被清空（面板显示"未配置检查地址"，而不是拿旧地址假绿）；
// Port 推不出来就保持不动。
func TestReconcileHealthURLsClearsComposeHealthWhenCatalogEntryGone(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()

	// 前提：这两个条目确实已从目录下架（0.14.3）。若哪天又上架了，
	// 这个测试必须被重新审视，而不是悄悄变成"对齐"测试。
	for _, id := range []string{"portainer", "minio"} {
		if _, ok := FindApp(id); ok {
			t.Fatalf("测试前提变了：目录里又有 %s 了，请改用另一个已下架条目", id)
		}
	}

	const stalePort = 9001
	rec := &Service{
		Name: "portainer", DisplayName: "Portainer", Kind: KindCompose,
		Port: stalePort, HealthURL: fmt.Sprintf("http://127.0.0.1:%d/", stalePort),
		ComposeFile: filepath.Join(m.composeDir(), "portainer", "docker-compose.yml"),
	}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}

	n, err := m.ReconcileHealthURLs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("目录里已无该条目时应清空健康地址（改动 1 条），实际 changed=%d", n)
	}
	got, err := repo.Get(ctx, "portainer")
	if err != nil {
		t.Fatal(err)
	}
	if got.HealthURL != "" {
		t.Errorf("目录里已无对应条目时必须清空 health_url（否则会打到别的应用假绿），实际 %q",
			got.HealthURL)
	}
	if got.Port != stalePort {
		t.Errorf("端口无法从目录推导时应保持不动，实际 %d", got.Port)
	}

	// 幂等：已经清空后再跑不该继续写库。
	if n2, err := m.ReconcileHealthURLs(ctx); err != nil || n2 != 0 {
		t.Errorf("清空后应幂等，实际 changed=%d err=%v", n2, err)
	}
}

// TestReconcileHealthURLsComposeDoesNotAlignToNativeCatalogEntry：
// 名字撞上目录里的**原生**条目时（用户自己用 Docker 页起了个叫 "nginx" 的
// compose 项目），绝不能把它对齐到 native nginx 的 80 端口 —— 否则健康检查
// 又变成"打的是别的应用"（这正是本次要修的假绿）。当作"目录里没有 compose
// 对应条目"处理：清空健康地址、端口不动。
func TestReconcileHealthURLsComposeDoesNotAlignToNativeCatalogEntry(t *testing.T) {
	native, ok := FindApp("nginx")
	if !ok || native.Kind == KindCompose {
		t.Fatal("测试前提：目录里应有一个原生条目 nginx")
	}

	m, repo := sandboxManager(t)
	ctx := context.Background()
	rec := &Service{
		Name: "nginx", DisplayName: "nginx", Kind: KindCompose,
		Port: 18080, HealthURL: "http://127.0.0.1:18080/",
	}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}

	if n, err := m.ReconcileHealthURLs(ctx); err != nil || n != 1 {
		t.Fatalf("应清空该记录的健康地址（改动 1 条），实际 changed=%d err=%v", n, err)
	}
	got, err := repo.Get(ctx, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if got.HealthURL != "" {
		t.Errorf("不该把 compose 记录对齐到原生条目的健康地址，实际 %q", got.HealthURL)
	}
	if got.Port != 18080 {
		t.Errorf("端口不该被原生条目的 %d 覆盖，实际 %d", native.WebPort(), got.Port)
	}
}

// TestReconcileHealthURLsNativeBehaviorUnchanged ④（回归）：
// 非 compose 记录（有 LaunchLabel）的行为一字不变 ——
//
//	(a) 目录里能查到的标签：照旧**覆盖**成目录值（包括非零旧值）；
//	(b) 目录里查不到的标签：照旧保持原样，**绝不**像 compose 那样被清空。
func TestReconcileHealthURLsNativeBehaviorUnchanged(t *testing.T) {
	m, repo := sandboxManager(t)
	ctx := context.Background()

	// (a) 已知标签 + 非零旧值 → 覆盖（这是既有的"双向对齐"行为）
	app, ok := FindApp("voicereceiver")
	if !ok || app.ServiceLabel == "" {
		t.Fatal("目录里没有 voicereceiver / 它没有 ServiceLabel，测试前提不成立")
	}
	want := healthURLFor(app)
	if want == "" {
		t.Fatal("voicereceiver 应声明 HealthPath，否则这条断言没意义")
	}
	known := &Service{
		Name: "native-known", DisplayName: app.Name, Kind: KindNative,
		LaunchLabel: app.ServiceLabel,
		Port:        app.Port, HealthURL: "http://127.0.0.1:1/stale",
	}
	if err := repo.Create(ctx, known); err != nil {
		t.Fatal(err)
	}

	// (b) 未知标签（用户自己纳管的服务）→ 原样保留
	const ownHealth = "http://127.0.0.1:12345/health"
	own := &Service{
		Name: "my-own-thing", DisplayName: "我自己的服务", Kind: KindNative,
		LaunchLabel: "com.example.my-own-thing",
		Port:        12345, HealthURL: ownHealth,
	}
	if err := repo.Create(ctx, own); err != nil {
		t.Fatal(err)
	}

	n, err := m.ReconcileHealthURLs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("只有已知标签那条该被覆盖，实际 changed=%d", n)
	}

	gotKnown, err := repo.Get(ctx, "native-known")
	if err != nil {
		t.Fatal(err)
	}
	if gotKnown.HealthURL != want {
		t.Errorf("已知标签的非零旧值应被覆盖成 %q，实际 %q", want, gotKnown.HealthURL)
	}

	gotOwn, err := repo.Get(ctx, "my-own-thing")
	if err != nil {
		t.Fatal(err)
	}
	if gotOwn.HealthURL != ownHealth {
		t.Errorf("未知标签的原生记录不该被清空/改写：期望 %q，实际 %q",
			ownHealth, gotOwn.HealthURL)
	}
	if gotOwn.Port != 12345 {
		t.Errorf("未知标签的原生记录端口不该被动：期望 12345，实际 %d", gotOwn.Port)
	}
}
