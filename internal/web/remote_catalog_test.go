package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/upgrade"
)

// remote_catalog_test.go —— 远端应用目录的**安全边界**门禁。
//
// 这块唯一被信任的输入是"用面板内嵌发布公钥验签通过"的目录；下面每条都锁一个
// fail-closed 的性质（用户 2026-09-22："不发新版面板也能加应用"）。

// withTestPubKey 装上一把测试公钥（验签用），返回私钥与恢复函数。
func withTestPubKey(t *testing.T) (ed25519.PrivateKey, func()) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	prev := upgrade.PubKeyHex
	upgrade.PubKeyHex = hex.EncodeToString(pub)
	return priv, func() { upgrade.PubKeyHex = prev }
}

// serveCatalog 起一个本地 HTTP 服务，按目录内容 + 签名提供 apps/catalog.json(.sig)。
func serveCatalog(t *testing.T, body, sig []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/catalog.json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) })
	mux.HandleFunc("/apps/catalog.json.sig", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(sig) })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func catalogJSON(t *testing.T, apps []services.App) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": "test", "generated_at": "2026-09-22T00:00:00Z", "apps": apps})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRemoteCatalogRejectsInvalidSignature 锁 fail-closed：签名不对就**一个条目都不许装上**。
func TestRemoteCatalogRejectsInvalidSignature(t *testing.T) {
	_, restore := withTestPubKey(t)
	defer restore()

	body := catalogJSON(t, []services.App{{ID: "remote-demo", Name: "远程演示", Rail: "brew", BrewFormula: "jq"}})
	ts := serveCatalog(t, body, []byte(strings.Repeat("x", ed25519.SignatureSize)))

	srv, _ := newTestServer(t)
	srv.Cfg.MirrorBase = ts.URL
	if err := srv.refreshRemoteCatalog(context.Background()); err == nil {
		t.Fatal("验签失败时刷新必须返回错误")
	}
	if _, ok := services.FindApp("remote-demo"); ok {
		t.Fatal("验签失败的远端条目竟然被装进目录了（fail closed 被破坏）")
	}
	rep := remoteCatalogReport()
	if s, _ := rep["error"].(string); !strings.Contains(s, "验签失败") {
		t.Errorf("接口必须如实汇报验签失败，实际 %v", rep)
	}
}

// TestRemoteCatalogAppendsVerifiedApp 锁正向：验签通过的新 ID 必须能被市场看到（FindApp 找得到）。
func TestRemoteCatalogAppendsVerifiedApp(t *testing.T) {
	priv, restore := withTestPubKey(t)
	defer restore()

	app := services.App{
		ID: "remote-demo", Name: "远程演示", Icon: "🧪", Category: services.CategoryOther,
		Summary: "来自镜像站目录的演示条目", Rail: "brew", BrewFormula: "jq",
	}
	body := catalogJSON(t, []services.App{app})
	ts := serveCatalog(t, body, upgrade.SignManifest(priv, body))

	srv, _ := newTestServer(t)
	srv.Cfg.MirrorBase = ts.URL
	if err := srv.refreshRemoteCatalog(context.Background()); err != nil {
		t.Fatalf("验签通过的目录应当装上：%v", err)
	}
	got, ok := services.FindApp("remote-demo")
	if !ok {
		t.Fatal("验签通过的远端条目必须出现在目录里（这就是「不发版加应用」）")
	}
	if !got.Remote || got.Kind != services.KindNative || got.BrewFormula != "jq" {
		t.Errorf("远端条目字段不对：%+v", got)
	}
	// 重新加载缓存（模拟面板重启）：仍然在（不联网）。
	// 模拟面板重启：清掉内存里的远端目录，再从磁盘缓存装回来。
	services.SetRemoteCatalog(nil, "", time.Time{})
	srv.loadRemoteCatalogCache()
	if _, ok := services.FindApp("remote-demo"); !ok {
		t.Error("重启后必须能从本地缓存恢复远端目录")
	}
}

// TestRemoteCatalogNeverOverridesBuiltin 锁"只追加"：与内置撞 ID 的远端条目必须被忽略。
func TestRemoteCatalogNeverOverridesBuiltin(t *testing.T) {
	priv, restore := withTestPubKey(t)
	defer restore()

	// 内置 nginx 的 brew formula 是 nginx；远端试图把它改成别的包 —— 必须无效。
	body := catalogJSON(t, []services.App{{
		ID: "nginx", Name: "被篡改的 Nginx", Rail: "brew", BrewFormula: "evil-formula",
	}})
	ts := serveCatalog(t, body, upgrade.SignManifest(priv, body))

	srv, _ := newTestServer(t)
	srv.Cfg.MirrorBase = ts.URL
	if err := srv.refreshRemoteCatalog(context.Background()); err != nil {
		t.Fatalf("刷新本身应当成功（条目被拒不算刷新失败）：%v", err)
	}
	got, _ := services.FindApp("nginx")
	if got.BrewFormula == "evil-formula" || got.Name == "被篡改的 Nginx" {
		t.Fatalf("远端条目覆盖了内置应用（安装行为被远端数据改掉）：%+v", got)
	}
	rep := remoteCatalogReport()
	rej, _ := rep["rejected"].([]string)
	if len(rej) == 0 || !strings.Contains(strings.Join(rej, ";"), "撞 ID") {
		t.Errorf("被拒条目与原因必须如实汇报，实际 %v", rep)
	}
}

// TestRemoteCatalogRejectsUnsupportedRail 锁"只认通用轨"：远端数据不许携带任意命令/新安装器。
func TestRemoteCatalogRejectsUnsupportedRail(t *testing.T) {
	priv, restore := withTestPubKey(t)
	defer restore()

	body := catalogJSON(t, []services.App{
		{ID: "remote-tarball", Name: "需要新安装器的应用", Rail: "tarball"},
		{ID: "remote-run", Name: "想跑命令的应用", Rail: "brew", BrewFormula: "jq"},
	})
	ts := serveCatalog(t, body, upgrade.SignManifest(priv, body))

	srv, _ := newTestServer(t)
	srv.Cfg.MirrorBase = ts.URL
	if err := srv.refreshRemoteCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := services.FindApp("remote-tarball"); ok {
		t.Error("rail=tarball 的远端条目必须被拒（需要面板升级支持）")
	}
	if _, ok := services.FindApp("remote-run"); !ok {
		t.Error("rail=brew 且字段齐全的条目应当被采用")
	}
	rej := strings.Join(remoteCatalogReport()["rejected"].([]string), ";")
	if !strings.Contains(rej, "只支持 rail=brew / compose") {
		t.Errorf("拒绝原因要写清「只支持通用轨」，实际 %q", rej)
	}
}
