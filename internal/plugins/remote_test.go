package plugins_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/plugins"
	"github.com/zizdog/zizpanel/internal/upgrade"
)

// remoteFixture 起一个假镜像站：目录 + 签名 + 插件声明。
// key 由调用方保管，测试自己决定"签名对不对"。
func remoteFixture(t *testing.T, priv ed25519.PrivateKey, specJSON string, tamperIndex bool) (base string, entry plugins.RemoteEntry) {
	t.Helper()
	sum := sha256.Sum256([]byte(specJSON))
	mux := http.NewServeMux()
	mux.HandleFunc("/plugins/index.json", func(w http.ResponseWriter, r *http.Request) {
		idx := map[string]any{
			"schema": "zizpanel.plugins/v1",
			"plugins": []map[string]any{{
				"id": "myapp", "name": "远端应用", "icon": "📦", "version": "1.2.3",
				"url": "myapp.json", "sha256": hex.EncodeToString(sum[:]),
			}},
		}
		body, _ := json.Marshal(idx)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/plugins/index.json.sig", func(w http.ResponseWriter, r *http.Request) {
		idx := map[string]any{
			"schema": "zizpanel.plugins/v1",
			"plugins": []map[string]any{{
				"id": "myapp", "name": "远端应用", "icon": "📦", "version": "1.2.3",
				"url": "myapp.json", "sha256": hex.EncodeToString(sum[:]),
			}},
		}
		body, _ := json.Marshal(idx)
		if tamperIndex {
			body = append(body, ' ') // 签名与内容对不上
		}
		_, _ = w.Write(ed25519.Sign(priv, body))
	})
	mux.HandleFunc("/plugins/myapp.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(specJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, plugins.RemoteEntry{ID: "myapp", Name: "远端应用", Version: "1.2.3", URL: "myapp.json",
		SHA256: hex.EncodeToString(sum[:])}
}

const remoteSpecJSON = `{
  "schema":"zizpanel.app/v1","id":"myapp","name":"远端应用","icon":"📦",
  "source":{"kind":"brew","formula":"myapp","checksum":"sha256"},
  "run":{"mode":"brew-service"},
  "expose":{"port":12345,"bind":"0.0.0.0","ui":"app"},
  "verify":{"any_of":[{"kind":"http","path":"/healthz","expect":"ok"}]},
  "health":{"kind":"http","path":"/healthz","expect":"ok"},
  "uninstall":{"always":["/Library/LaunchDaemons/homebrew.mxcl.myapp.plist"],"formula":"myapp"}
}`

// withTestKey 换上测试钥匙（内嵌公钥在测试里通常为空，等于"没有信任根"）。
func withTestKey(t *testing.T, priv ed25519.PrivateKey) {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	old := plugins.VerifySignature
	plugins.VerifySignature = upgrade.VerifyManifest
	oldHex := upgrade.PubKeyHex
	upgrade.PubKeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { plugins.VerifySignature = old; upgrade.PubKeyHex = oldHex })
}

// TestRemoteInstallHappyPath 正路：验签 → sha256 → 严格校验 → 落盘（**默认未启用**）。
func TestRemoteInstallHappyPath(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	withTestKey(t, priv)
	base, entry := remoteFixture(t, priv, remoteSpecJSON, false)
	dir := t.TempDir()

	idx, err := plugins.FetchRemoteIndex(context.Background(), base+"/plugins/index.json")
	if err != nil {
		t.Fatalf("取签名目录应当成功：%v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("目录应有 1 条，实际 %d", len(idx.Plugins))
	}
	spec, err := plugins.InstallRemote(context.Background(), dir, entry,
		plugins.InstallOptions{IndexURL: base + "/plugins/index.json", Version: entry.Version})
	if err != nil {
		t.Fatalf("安装应当成功：%v", err)
	}
	if spec.ID != "myapp" {
		t.Errorf("id 不对：%q", spec.ID)
	}
	// 落盘了、而且是**未启用**（双重确认：先装进来，再在面板里启用）
	got, err := os.Stat(filepath.Join(dir, "myapp.json"))
	if err != nil {
		t.Fatalf("声明没落盘：%v", err)
	}
	if got.Mode().Perm() != 0o600 {
		t.Errorf("声明权限应为 0600，实际 %o", got.Mode().Perm())
	}
	if plugins.ReadEnabled(dir)["myapp"] {
		t.Error("远端插件落盘后不该是启用状态")
	}
	// 来源记录：能看出来自远端（供"移除"与展示用）
	if rec := plugins.RemoteRecords(dir); rec["myapp"].Version != "1.2.3" {
		t.Errorf("来源记录不对：%+v", rec)
	}
	// 本地插件目录里能看到它，且 .remote.json 不会被当成插件
	list := plugins.LoadDir(dir)
	if len(list) != 1 || list[0].ID != "myapp" || list[0].Err != "" {
		t.Errorf("本地加载结果不对：%+v", list)
	}
	// 移除：声明与记录都清掉
	if err := plugins.RemoveRemote(dir, "myapp"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "myapp.json")); !os.IsNotExist(err) {
		t.Error("移除后声明应当没了")
	}
}

// TestRemoteRejectsBadSignature 没签名/签名不对一律拒绝（信任根只有一把）。
func TestRemoteRejectsBadSignature(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, otherPriv, _ := ed25519.GenerateKey(nil)
	_ = otherPub
	dim := t.TempDir()
	// ① 验签器缺失（面板没配公钥）
	old := plugins.VerifySignature
	plugins.VerifySignature = func(data, sig []byte) error { return os.ErrPermission }
	base, _ := remoteFixture(t, priv, remoteSpecJSON, false)
	if _, err := plugins.FetchRemoteIndex(context.Background(), base+"/plugins/index.json"); err == nil ||
		!strings.Contains(err.Error(), "验签失败") {
		t.Errorf("没配信任根时必须拒绝，实际：%v", err)
	}
	plugins.VerifySignature = upgrade.VerifyManifest
	oldHex := upgrade.PubKeyHex
	upgrade.PubKeyHex = hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	defer func() { plugins.VerifySignature = old; upgrade.PubKeyHex = oldHex }()

	// ② 内容被改过（签名对不上）
	base2, _ := remoteFixture(t, priv, remoteSpecJSON, true)
	if _, err := plugins.FetchRemoteIndex(context.Background(), base2+"/plugins/index.json"); err == nil {
		t.Error("签名对不上时必须拒绝")
	}
	// ③ 用别的私钥签
	upgrade.PubKeyHex = hex.EncodeToString(otherPriv.Public().(ed25519.PublicKey))
	if _, err := plugins.FetchRemoteIndex(context.Background(), base+"/plugins/index.json"); err == nil {
		t.Error("换了公钥就对不上，必须拒绝")
	}
	_ = dim
}

// TestRemoteRejectsTamperedSpec 声明本体按 sha256 校验：内容被改就拒绝（签名只覆盖目录）。
func TestRemoteRejectsTamperedSpec(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	withTestKey(t, priv)
	base, entry := remoteFixture(t, priv, remoteSpecJSON, false)
	dir := t.TempDir()
	entry.SHA256 = strings.Repeat("0", 64) // 目录里写的哈希与实际不符
	if _, err := plugins.InstallRemote(context.Background(), dir, entry,
		plugins.InstallOptions{IndexURL: base + "/plugins/index.json"}); err == nil ||
		!strings.Contains(err.Error(), "sha256") {
		t.Errorf("声明哈希不符必须拒绝，实际：%v", err)
	}
	if plugins.LoadDir(dir) != nil {
		t.Error("被拒绝时不该留下任何文件")
	}
}

// TestRemoteRefusesReservedID 远端插件不许覆盖面板自带应用或已存在的本地插件。
func TestRemoteRefusesReservedID(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	withTestKey(t, priv)
	base, entry := remoteFixture(t, priv, remoteSpecJSON, false)
	dir := t.TempDir()
	_, err := plugins.InstallRemote(context.Background(), dir, entry, plugins.InstallOptions{
		IndexURL: base + "/plugins/index.json",
		Reserved: func(id string) bool { return id == "myapp" },
	})
	if err == nil || !strings.Contains(err.Error(), "占用") {
		t.Errorf("撞 id 必须拒绝，实际：%v", err)
	}
}

// TestRemoteIndexRequiresSource 远端插件默认关闭：不给地址就明说，绝不偷偷联网。
func TestRemoteIndexRequiresSource(t *testing.T) {
	if _, err := plugins.FetchRemoteIndex(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "默认关闭") {
		t.Errorf("空来源必须明确拒绝，实际：%v", err)
	}
}

// TestRemoteInvalidSpecRejected 远端声明也要过同一套严格校验（未知键/缺字段都拒）。
func TestRemoteInvalidSpecRejected(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	withTestKey(t, priv)
	bad := strings.Replace(remoteSpecJSON, `"formula":"myapp"`, `"formula":"myapp","steps":[{"cmd":"x"}]`, 1)
	base, entry := remoteFixture(t, priv, bad, false)
	if _, err := plugins.InstallRemote(context.Background(), t.TempDir(), entry,
		plugins.InstallOptions{IndexURL: base + "/plugins/index.json"}); err == nil {
		t.Error("夹带未知键的远端声明必须被拒绝")
	}
}
