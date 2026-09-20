package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  网络提示接线门禁
//
//  新接的联网入口：任务失败原因是**网络类**时，错误文本里必须带统一提示
//  （services.NetworkHintMarker）；非网络类必须**原样**返回、一个字都不加。
//  判据与文案的唯一来源是 internal/services/netfail.go（这里只锁"接没接上"）。
//
//  纪律：注入假实现，绝不真的拉镜像 / 跑 compose / 跑 brew（AGENTS 第三节）。
// ============================================================================

// netEvidence 是一条带真实证据的网络类错误文本（Go net/http 的原话）。
const netEvidence = `Get "https://github.com/typecho/typecho/releases/download/v1.3.0/typecho.zip": dial tcp: lookup github.com: no such host`

// nonNetCases 是反例：这些都不是网络问题，判据不许命中，接线也不许硬加提示。
var nonNetCases = []struct{ name, msg string }{
	{"permission denied", "open /opt/zizpanel/data/panel.db: permission denied"},
	{"no such file", "open /opt/homebrew/bin/brew: no such file or directory"},
	{"HTTP 404", "下载源码失败（试过 2 个地址）：HTTP 404"},
	{"signature mismatch", "清单签名验证失败：ed25519: bad signature"},
	{"sha256 不匹配", "升级包校验和不匹配（期望 abc123，实际 def456），文件已丢弃"},
}

// taskErrorAfterPost 发一个创建长任务的请求，等它结束，返回任务失败原因。
func taskErrorAfterPost(t *testing.T, srv *Server, ts *httptest.Server,
	path string, body any, cookies []*http.Cookie) string {
	t.Helper()
	res, out, _ := doJSON(t, ts, "POST", path, body, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s 应 202（长任务），实际 %d：%v", path, res.StatusCode, out)
	}
	tk := waitTaskDone(t, srv, taskIDFrom(t, out))
	if tk.Status() != tasks.StatusFailed {
		t.Fatalf("POST %s 的任务应失败（注入的就是失败），实际 %s", path, tk.Status())
	}
	return tk.Meta().Error
}

// assertNetworkHint 断言"网络类带提示、非网络类原样"。
// raw 是**没有提示时**任务应给出的原文（网络类则必须是它的前缀 + 一行提示）。
func assertNetworkHint(t *testing.T, entry, got, raw string, wantHint bool) {
	t.Helper()
	hasMarker := strings.Contains(got, services.NetworkHintMarker)
	if wantHint {
		if !hasMarker {
			t.Errorf("%s：网络类失败必须带统一提示，实际：%q", entry, got)
		}
		if !strings.HasPrefix(got, raw) {
			t.Errorf("%s：追加提示不能丢掉原文（应以 %q 开头），实际：%q", entry, raw, got)
		}
		return
	}
	if hasMarker {
		t.Errorf("%s：非网络失败一个字都不许加，实际：%q", entry, got)
	}
	if got != raw {
		t.Errorf("%s：非网络失败必须原样返回，实际 %q，期望 %q", entry, got, raw)
	}
}

// TestNetworkHintOnMarketInstallEntries 覆盖 stt / miniflux / syncthing 三个
// "brew + 下载"安装入口（macspeech 不做任何下载，故意不接，见 api_macspeech.go）。
func TestNetworkHintOnMarketInstallEntries(t *testing.T) {
	cases := []struct {
		id   string
		stub func(t *testing.T, err error)
	}{
		{"stt", func(t *testing.T, err error) {
			prev := sttInstallFn
			sttInstallFn = func(*Server, context.Context, services.App, *services.InstallResult) error {
				return err
			}
			t.Cleanup(func() { sttInstallFn = prev })
		}},
		{"miniflux", func(t *testing.T, err error) {
			prev := minifluxInstallFn
			minifluxInstallFn = func(*Server, context.Context, *services.InstallResult) error {
				return err
			}
			t.Cleanup(func() { minifluxInstallFn = prev })
		}},
		{"syncthing", func(t *testing.T, err error) {
			prev := syncthingInstallFn
			syncthingInstallFn = func(*Server, context.Context, *services.InstallResult) error {
				return err
			}
			t.Cleanup(func() { syncthingInstallFn = prev })
		}},
		{"transmission", func(t *testing.T, err error) {
			prev := transmissionInstallFn
			transmissionInstallFn = func(*Server, context.Context, *services.InstallResult) error {
				return err
			}
			t.Cleanup(func() { transmissionInstallFn = prev })
		}},
	}

	for _, c := range cases {
		path := "/api/v1/market/" + c.id + "/install"

		t.Run(c.id+"/网络", func(t *testing.T) {
			srv, ts := newTestServer(t)
			cookies := loginTestPanel(t, ts)
			c.stub(t, errors.New(netEvidence))
			got := taskErrorAfterPost(t, srv, ts, path, nil, cookies)
			assertNetworkHint(t, c.id, got, netEvidence, true)
		})

		for _, nn := range nonNetCases {
			t.Run(c.id+"/非网络-"+nn.name, func(t *testing.T) {
				srv, ts := newTestServer(t)
				cookies := loginTestPanel(t, ts)
				c.stub(t, errors.New(nn.msg))
				got := taskErrorAfterPost(t, srv, ts, path, nil, cookies)
				assertNetworkHint(t, c.id, got, nn.msg, false)
			})
		}
	}
}

// TestNetworkHintOnSiteInstallEntry 覆盖一键建站的源码包下载路径。
func TestNetworkHintOnSiteInstallEntry(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		net  bool
	}{
		{"网络", netEvidence, true},
		{"permission denied", "open /tmp/site.zip: permission denied", false},
		{"HTTP 404", "下载源码失败（试过 2 个地址）：HTTP 404", false},
		{"signature mismatch", "SHA256 不匹配：期望 abc123，实际 def456", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, ts := newTestServer(t)
			cookies := loginTestPanel(t, ts)
			// 前置检查换成通过：否则会先在"缺 PHP 扩展"上失败（那是本地错误，测不到下载路径）。
			prevPre := sitePHPPreflightFn
			sitePHPPreflightFn = func(*Server, string, *services.SiteAppSpec, string) error { return nil }
			t.Cleanup(func() { sitePHPPreflightFn = prevPre })
			prevFetch := sitePackageFetchFn
			sitePackageFetchFn = func(*Server, context.Context, string, string,
				func(string)) (services.SiteSource, string, error) {
				return services.SiteSource{}, "", errors.New(c.msg)
			}
			t.Cleanup(func() { sitePackageFetchFn = prevFetch })

			got := taskErrorAfterPost(t, srv, ts, "/api/v1/market/typecho/install-site",
				map[string]string{"domain": "hint.test"}, cookies)
			assertNetworkHint(t, "一键建站", got, c.msg, c.net)
		})
	}
}

// TestNetworkHintOnComposeUpEntry 覆盖 compose 部署（up 会拉镜像）。
func TestNetworkHintOnComposeUpEntry(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		net  bool
	}{
		{"网络", netEvidence, true},
		{"permission denied", "open /tmp/compose/demo/docker-compose.yml: permission denied", false},
		{"HTTP 404", "manifest unknown: HTTP 404", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, ts := newTestServer(t)
			cookies := loginTestPanel(t, ts)
			prev := composeActionFn
			composeActionFn = func(*services.Manager, context.Context, string, string, bool) (string, error) {
				return "", errors.New(c.msg)
			}
			t.Cleanup(func() { composeActionFn = prev })

			got := taskErrorAfterPost(t, srv, ts, "/api/v1/docker/compose/demo/actions?action=up", nil, cookies)
			assertNetworkHint(t, "compose 部署", got, c.msg, c.net)
		})
	}
}

// TestNetworkHintOnDockerEntries 覆盖镜像拉取与容器创建（本地没镜像时会先拉）。
//
// 两条路都走**真实的假 Docker 守护进程**（unix socket + httptest），
// 只有 registry 那一步由假守护进程回一条错误事件。
func TestNetworkHintOnDockerEntries(t *testing.T) {
	const netMsg = `Get "https://registry-1.docker.io/v2/": dial tcp: lookup registry-1.docker.io: no such host`
	const nonNetMsg = "pull access denied for secret/private, repository does not exist (HTTP 404)"

	entries := []struct {
		name string
		path string
		body any
		// prefix 是拉取层给出的原文前缀（原文必须完整保留）。
		prefix string
	}{
		{"镜像拉取", "/api/v1/docker/images/pull", map[string]string{"image": "secret/private:1"}, "拉取失败: "},
		{"容器创建", "/api/v1/docker/containers", map[string]string{"image": "secret/private:1"}, "拉取镜像失败: 拉取失败: "},
	}

	for _, e := range entries {
		for _, c := range []struct {
			name     string
			msg      string
			wantHint bool
		}{
			{"网络", netMsg, true},
			{"非网络-404", nonNetMsg, false},
		} {
			t.Run(e.name+"/"+c.name, func(t *testing.T) {
				sock, fake := startFakeDocker(t)
				srv, ts, cookies := newDockerTestServerWithSrv(t, sock)
				fake.setPullError(c.msg)

				got := taskErrorAfterPost(t, srv, ts, e.path, e.body, cookies)
				assertNetworkHint(t, e.name, got, e.prefix+c.msg, c.wantHint)
			})
		}
	}

	// 本地端点反例：socket 文件不存在（错误文本里带 docker.sock）**不算**网络问题。
	t.Run("本地 socket 不存在", func(t *testing.T) {
		bogus := "/tmp/zp-no-such-docker/docker.sock"
		_ = os.RemoveAll("/tmp/zp-no-such-docker")
		srv, ts, cookies := newDockerTestServerWithSrv(t, bogus)
		got := taskErrorAfterPost(t, srv, ts, "/api/v1/docker/images/pull",
			map[string]string{"image": "nginx:1.27"}, cookies)
		if !strings.Contains(got, "docker.sock") {
			t.Fatalf("测试前提不成立：错误里应出现 docker.sock，实际 %q", got)
		}
		if strings.Contains(got, services.NetworkHintMarker) {
			t.Errorf("本地端点失败不是网络问题，不该加提示：%q", got)
		}
	})
}

// TestNetworkHintUsesSharedFrontendModule 锁住前端**唯一真源**：判据与文案住在
// assets/js/netfail.js，五个联网页面 import 使用，谁都不许再各抄一份证据表。
// 真正的语法门禁是 tools/check-js-syntax.mjs；这里防"某个页面漏接 / 又抄一份"。
func TestNetworkHintUsesSharedFrontendModule(t *testing.T) {
	shared, err := os.ReadFile("assets/js/netfail.js")
	if err != nil {
		t.Fatalf("读取 assets/js/netfail.js 失败：%v", err)
	}
	if !strings.Contains(string(shared), services.NetworkHintMarker) {
		t.Errorf("assets/js/netfail.js 里没有与后端逐字一致的识别短语 %q", services.NetworkHintMarker)
	}
	if !strings.Contains(string(shared), "export function isNetworkFailureText(") {
		t.Error("assets/js/netfail.js 缺 isNetworkFailureText 的定义 —— 各页面就没有可 import 的判据")
	}

	for _, p := range []string{
		"assets/js/apps.js",
		"assets/js/update.js",
		"assets/js/docker-images.js",
		"assets/js/docker-compose.js",
		"assets/js/sites.js",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", p, err)
		}
		src := string(b)
		if !strings.Contains(src, "from './netfail.js'") {
			t.Errorf("%s 没有 import 共享的 ./netfail.js —— 判据/文案必须同源", p)
		}
		if strings.Count(src, "isNetworkFailureText(") < 1 {
			t.Errorf("%s 没有调用共享判据 isNetworkFailureText", p)
		}
		if !strings.Contains(src, "networkHintBlock") && !strings.Contains(src, "networkPanel") {
			t.Errorf("%s 没有使用共享的醒目块渲染", p)
		}
		// 证据表/判据只许有一份：页面里再出现这些名字就是又抄了一份。
		for _, dup := range []string{"NET_EVIDENCE", "NET_LOCAL_ENDPOINT", "function isNetworkFailureText("} {
			if strings.Contains(src, dup) {
				t.Errorf("%s 里又出现了 %q —— 判据必须只在 netfail.js 定义一份", p, dup)
			}
		}
	}
}
