package proxies

import (
	"context"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/store"
)

// expectedVhostGolden 是 Rule.Generate 对一个固定规则的**逐字**期望输出。
//
// 为什么要有这条测试：反代配置是"一次生成、长期驻留"的产物，任何格式化改动
// 都会在下次保存某条规则时覆盖用户正在用的配置，所以输出必须是可预期的、
// 且每次有意改动都要在这里显式更新。
//
// 这一版有意加上了 `proxy_request_buffering off`（2026-09-18 生产事故）：
// 没有它时，超过 client_body_buffer_size 的请求体先整段落盘到 client_body_temp，
// 那个目录一旦不可写（属主不对 / 磁盘满），nginx 会在转发给上游**之前**直接回
// 它自己的 500 HTML 页 —— 端口在听、健康检查全绿，用户却一上传就坏
// （真实现场：经反代推 368KB 音色样本）。`proxy_buffering off` 只关响应缓冲，
// 关不掉请求体缓冲，必须显式写这一行，让请求体边收边转发给上游。
const expectedVhostGolden = "# 由 ZizPanel「反向代理」生成 —— 请勿手工编辑（会被面板覆盖）\n# 规则：镜像站\nserver {\n\tlisten      8090;\n\tserver_name a.com b.com;\n\n\t# 反代目标的真实地址（日志里用得上）\n\t# target: http://192.168.1.8:8090\n\n\tlocation /api {\n\t\tproxy_pass http://192.168.1.8:8090;\n\t\tproxy_set_header Host 192.168.1.8:8090;\n\t\tproxy_set_header X-Real-IP $remote_addr;\n\t\tproxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n\t\tproxy_set_header X-Forwarded-Proto $scheme;\n\t\tproxy_http_version 1.1;\n\t\t# WebSocket（upgrade map 由面板确保存在）\n\t\tproxy_set_header Upgrade $http_upgrade;\n\t\tproxy_set_header Connection $connection_upgrade;\n\n\t\tproxy_connect_timeout 60s;\n\t\tproxy_send_timeout    3600s;\n\t\tproxy_read_timeout    3600s;\n\t\tproxy_buffering       off;\n\t\tproxy_request_buffering off;\n\n\t\taccess_log /tmp/logs/proxy-7.access.log;\n\t\terror_log  /tmp/logs/proxy-7.error.log;\n\t}\n}\n"

func goldenRule() *Rule {
	return &Rule{
		ID: 7, Name: "镜像站", Listen: 8090, Domains: "a.com, b.com", Path: "/api",
		Target: "http://192.168.1.8:8090", PreserveHost: false, Websocket: true,
		Enabled: true, Remark: "x",
	}
}

// TestRuleGenerateWithoutSSLGolden：未启用 SSL 时的输出必须与 golden 逐字一致。
func TestRuleGenerateWithoutSSLGolden(t *testing.T) {
	got, err := goldenRule().Generate("/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	if got != expectedVhostGolden {
		t.Fatalf("未启用 SSL 的 vhost 与 golden 不一致：\n--- got ---\n%s\n--- want ---\n%s", got, expectedVhostGolden)
	}
	if !strings.Contains(got, "\t\tproxy_request_buffering off;\n") {
		t.Errorf("location 里必须关掉请求体缓冲（否则大请求体会落盘到 client_body_temp）：\n%s", got)
	}
	if strings.Contains(got, "ssl") {
		t.Errorf("未启用 SSL 时不该出现任何 ssl 指令：\n%s", got)
	}
}

// TestRuleGenerateSSL：启用 SSL 时必须有 `listen ... ssl` 与证书两行，
// 同时不能把原有的代理 / WebSocket / 日志逻辑弄丢。
func TestRuleGenerateSSL(t *testing.T) {
	r := goldenRule()
	r.SSLEnabled = true
	r.SSLCert = "/opt/zizpanel/certs/api.test/fullchain.pem"
	r.SSLKey = "/opt/zizpanel/certs/api.test/privkey.pem"
	r.SSLProvider = "acme"

	got, err := r.Generate("/tmp/logs")
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	for _, want := range []string{
		"listen      8090 ssl;",
		"ssl_certificate     /opt/zizpanel/certs/api.test/fullchain.pem;",
		"ssl_certificate_key /opt/zizpanel/certs/api.test/privkey.pem;",
		"ssl_protocols       TLSv1.2 TLSv1.3;",
		"server_name a.com b.com;",
		"proxy_pass http://192.168.1.8:8090;",
		"proxy_set_header X-Forwarded-Proto $scheme;",
		"proxy_set_header Upgrade $http_upgrade;",
		"/tmp/logs/proxy-7.access.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("启用 SSL 的配置缺少 %q：\n%s", want, got)
		}
	}
	// 不能出现"又 listen 8080;"这种两条 listen（nginx 会把它当成两个端口）
	if strings.Contains(got, "\tlisten      8090;\n") {
		t.Errorf("启用 SSL 时不该再输出非 SSL 的 listen：\n%s", got)
	}
}

// TestRuleValidateSSL：开启 HTTPS 必须带证书与私钥，且必须是绝对路径。
func TestRuleValidateSSL(t *testing.T) {
	base := func(mut func(*Rule)) *Rule {
		r := &Rule{Name: "n", Listen: 8443, Target: "http://a.com", SSLEnabled: true}
		if mut != nil {
			mut(r)
		}
		return r
	}
	if err := base(nil).Validate(); err == nil || !strings.Contains(err.Error(), "证书") {
		t.Errorf("缺少证书时应报错并提到证书，实际：%v", err)
	}
	if err := base(func(r *Rule) {
		r.SSLCert = "relative/fullchain.pem"
		r.SSLKey = "/abs/privkey.pem"
	}).Validate(); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Errorf("相对路径应被拒绝，实际：%v", err)
	}
	if err := base(func(r *Rule) {
		r.SSLCert = "/abs/fullchain.pem"
		r.SSLKey = "/abs/privkey.pem"
	}).Validate(); err != nil {
		t.Errorf("合法的 SSL 规则不该被拒：%v", err)
	}
	// 关闭 SSL 时留着旧路径也不该报错（用于"关闭 HTTPS"的中间态）
	if err := (&Rule{Name: "n", Listen: 8443, Target: "http://a.com",
		SSLEnabled: false, SSLCert: "relative"}).Validate(); err != nil {
		t.Errorf("未启用 SSL 时证书字段不应参与校验：%v", err)
	}
}

// TestGenerateRejectWithCert：SSL 端口上的兜底拒绝块必须带证书。
//
// 真机实测（nginx 1.31.5）：同一端口上只要有一个 server 块写了 ssl，
// 所有 server 块都必须有 ssl_certificate，否则 `nginx -t` 直接 [emerg]。
func TestGenerateRejectWithCert(t *testing.T) {
	ssl := GenerateRejectWithCert(18443, "/tmp/logs", "/c/fullchain.pem", "/c/privkey.pem", true)
	for _, want := range []string{
		"listen      18443 ssl default_server;",
		"ssl_certificate     /c/fullchain.pem;",
		"ssl_certificate_key /c/privkey.pem;",
		"return 444;",
		"/tmp/logs/proxy-reject-18443.access.log",
	} {
		if !strings.Contains(ssl, want) {
			t.Errorf("SSL 兜底块缺少 %q：\n%s", want, ssl)
		}
	}
	if strings.Contains(ssl, "proxy_pass") {
		t.Errorf("兜底拒绝块不允许出现 proxy_pass：\n%s", ssl)
	}

	// 中性形态：listen 不带 ssl，但保留证书行 —— 用于 SSL 开关切换的中间态。
	neutral := GenerateRejectWithCert(18443, "/tmp/logs", "/c/fullchain.pem", "/c/privkey.pem", false)
	if !strings.Contains(neutral, "listen      18443 default_server;") {
		t.Errorf("中性形态的 listen 不该带 ssl：\n%s", neutral)
	}
	if !strings.Contains(neutral, "ssl_certificate     /c/fullchain.pem;") {
		t.Errorf("中性形态仍必须带证书行（否则切换中间态会被 nginx 判 emerg）：\n%s", neutral)
	}

	// 没有证书时退回旧形态，保证非 SSL 端口输出逐字不变。
	if got, want := GenerateRejectWithCert(8090, "/tmp/logs", "", "", false), GenerateReject(8090, "/tmp/logs"); got != want {
		t.Errorf("无证书时应与 GenerateReject 输出一致：\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRuleRepositoryRoundTripsSSLFields 锁住"新增列真的被写进/读回数据库"。
func TestRuleRepositoryRoundTripsSSLFields(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repo := NewRepository(st)
	ctx := context.Background()

	created, err := repo.Create(ctx, &Rule{
		Name: "ssl 规则", Listen: 18444, Domains: "a.test", Target: "http://127.0.0.1:9",
		Enabled: true, SSLEnabled: true,
		SSLCert: "/c/fullchain.pem", SSLKey: "/c/privkey.pem",
		SSLProvider: "acme", SSLExpires: "2026-12-01 00:00:00",
	})
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if !created.SSLEnabled || created.SSLCert != "/c/fullchain.pem" || created.SSLKey != "/c/privkey.pem" ||
		created.SSLProvider != "acme" || created.SSLExpires != "2026-12-01 00:00:00" {
		t.Fatalf("新建后 SSL 字段没有落库/读回：%+v", created)
	}

	// 更新为关闭 SSL 并把字段清空（与 handleProxyUpdate 的做法一致）
	updated := *created
	updated.SSLEnabled = false
	updated.SSLCert, updated.SSLKey, updated.SSLProvider, updated.SSLExpires = "", "", "", ""
	got, err := repo.Update(ctx, &updated)
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if got.SSLEnabled || got.SSLCert != "" || got.SSLProvider != "" {
		t.Fatalf("关闭 SSL 后字段没有清空：%+v", got)
	}
}
