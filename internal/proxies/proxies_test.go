package proxies

import (
	"strings"
	"testing"
)

// TestRuleGenerateBasic 锁住生成的 nginx 配置里**必须有**的东西。
//
// 反代最容易"看起来成功、实际半残"：缺一个 header 后端就拿到错误的客户端信息，
// 缺 Upgrade 就 WebSocket 用不了，缺超时设置就会把大文件/慢后端掐断。
// 这些都不是编译期错误，只能靠断言钉住。
func TestRuleGenerateBasic(t *testing.T) {
	r := &Rule{
		ID: 7, Name: "NAS 镜像站", Listen: 8090,
		Target: "http://192.168.1.8:8090", Websocket: true, Enabled: true,
	}
	got, err := r.Generate("/tmp/logs")
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	for _, want := range []string{
		"listen      8090;",
		"server_name _;", // 没填域名 => 匹配所有 Host
		"proxy_pass http://192.168.1.8:8090;",
		"proxy_set_header Host 192.168.1.8:8090;", // 默认改成目标 Host（否则后端常 404）
		"proxy_set_header X-Real-IP $remote_addr;",
		"proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;",
		"proxy_set_header X-Forwarded-Proto $scheme;",
		"proxy_http_version 1.1;",
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_set_header Connection $connection_upgrade;",
		"proxy_read_timeout    3600s;",
		"/tmp/logs/proxy-7.access.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("生成的配置缺少 %q：\n%s", want, got)
		}
	}
}

// TestRuleGenerateDefaultsPortAndPreserveHost 锁住两个容易写错的细节：
//  1. 目标没写端口时，Host 头**不能**带上 :80/:443（带了会让后端按错误 Host 分站）；
//  2. PreserveHost 打开时透传原始 Host —— 且必须用 **$http_host**（带端口），
//     不能用 $host（端口会被丢掉）：公网只放行 8889、多个站点共用这一个端口时，
//     丢掉端口会让 WordPress / Typecho 这类应用把浏览器重定向到没放行的 443。
func TestRuleGenerateDefaultsPortAndPreserveHost(t *testing.T) {
	r := &Rule{ID: 1, Name: "x", Listen: 8081, Target: "http://example.com", Enabled: true}
	got, err := r.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "proxy_set_header Host example.com;") {
		t.Errorf("默认端口的 Host 头不该带端口：\n%s", got)
	}

	r2 := &Rule{ID: 2, Name: "y", Listen: 8082, Target: "http://example.com:8080", Enabled: true, PreserveHost: true}
	got2, err := r2.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got2, "proxy_set_header Host $http_host;") {
		t.Errorf("PreserveHost 打开时应用 $http_host 透传原始 Host（含端口）：\n%s", got2)
	}
	if strings.Contains(got2, "proxy_set_header Host $host;") {
		t.Errorf("不能用 $host：它会把端口丢掉，后端生成的绝对 URL 会指向没放行的端口：\n%s", got2)
	}
	// 非默认端口要带上
	r3 := &Rule{ID: 3, Name: "z", Listen: 8083, Target: "https://example.com:8443", Enabled: true}
	got3, _ := r3.Generate("")
	if !strings.Contains(got3, "proxy_set_header Host example.com:8443;") {
		t.Errorf("非默认端口应写进 Host 头：\n%s", got3)
	}
}

// TestStandardHeadersKeepPortInForwardedHost 锁住 X-Forwarded-Host 也带端口。
func TestStandardHeadersKeepPortInForwardedHost(t *testing.T) {
	r := &Rule{ID: 4, Name: "h", Listen: 8889, Domains: "wp.example.com",
		Target: "https://127.0.0.1:443", Enabled: true, PreserveHost: true, StandardHeaders: true}
	got, err := r.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "proxy_set_header X-Forwarded-Host $http_host;") {
		t.Errorf("X-Forwarded-Host 也必须是 $http_host（带端口）：\n%s", got)
	}
}

// TestRuleValidateRejects 锁住校验的**报错内容**（它直接展示给用户）。
//
// 反代规则里几乎每个字段都能填错，笼统一句"参数错误"等于让用户去猜。
func TestRuleValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want string
	}{
		{"没有名字", Rule{Listen: 80, Target: "http://a.com"}, "规则名称"},
		{"端口越界", Rule{Name: "n", Listen: 70000, Target: "http://a.com"}, "监听端口"},
		{"目标为空", Rule{Name: "n", Listen: 80}, "目标地址"},
		{"目标协议不对", Rule{Name: "n", Listen: 80, Target: "ftp://a.com"}, "http"},
		{"目标没有主机", Rule{Name: "n", Listen: 80, Target: "http://"}, "主机名"},
		{"路径没有斜杠", Rule{Name: "n", Listen: 80, Target: "http://a.com", Path: "api"}, "以 / 开头"},
		{"域名带协议", Rule{Name: "n", Listen: 80, Target: "http://a.com", Domains: "http://x.com"}, "非法字符"},
	}
	for _, c := range cases {
		err := c.rule.Validate()
		if err == nil {
			t.Errorf("%s：应当报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：报错里应包含 %q，实际 %q", c.name, c.want, err.Error())
		}
	}
	// 合法的最小规则
	if err := (&Rule{Name: "ok", Listen: 80, Target: "http://a.com"}).Validate(); err != nil {
		t.Errorf("这条应该合法，却被拒：%v", err)
	}
}

// TestRuleGenerateDomainList 锁住多域名写法（server_name 用空格分隔）。
func TestRuleGenerateDomainList(t *testing.T) {
	r := &Rule{ID: 1, Name: "n", Listen: 80, Target: "http://a.com",
		Domains: "A.com, b.com  c.com", Enabled: true}
	got, err := r.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "server_name a.com b.com c.com;") {
		t.Errorf("多域名应统一转小写并用空格分隔：\n%s", got)
	}
}

// TestVhostNameHasPrefix 锁住配置文件命名。
//
// 必须带 proxy- 前缀：站点配置是 `<域名>.conf`，反代规则如果也叫域名，
// 同一个 vhosts 目录里就会互相覆盖 —— 那是"建了反代、站点没了"的隐蔽故障。
func TestVhostNameHasPrefix(t *testing.T) {
	r := &Rule{ID: 42}
	if got := r.VhostName(); got != "proxy-42" {
		t.Errorf("vhost 文件名应为 proxy-<id>，实际 %q", got)
	}
	if !strings.HasPrefix(r.VhostName(), "proxy-") {
		t.Error("vhost 文件名必须带 proxy- 前缀，避免与站点配置撞名")
	}
}

// TestGenerateRejectBlocksUnmatchedHost 锁住「域名限制真的生效」这件事。
//
// 背景（2026-09-16 真机实测）：规则写了 domains=lede.zizdog.com，但**不带 Host 头
// 访问同样返回 200**。原因是 nginx 把"该端口上唯一的 server 块"当作默认 server，
// 于是 server_name 形同虚设 —— 任何 Host 都会被打到后端。
// 修法是给该端口生成一个显式 default_server 兜底块并直接 444。
//
// 这条测试盯的是"兜底块必须存在且必须拒绝"，缺任何一个都会让域名限制
// 退化成"什么域名都反代"，而那正是用户看不出来的那种失效。
func TestGenerateRejectBlocksUnmatchedHost(t *testing.T) {
	got := GenerateReject(18092, "/tmp/logs")

	for _, want := range []string{
		"listen      18092 default_server;", // 必须是 default_server，否则抢不到默认位
		"server_name _;",
		"return 444;", // 444 = 不响应直接断开，不把请求漏给后端
		"/tmp/logs/proxy-reject-18092.access.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("兜底拒绝块缺少 %q：\n%s", want, got)
		}
	}
	// 兜底块绝不能反代：一旦带 proxy_pass，它就从"拒绝"变成了"又一条通配规则"
	if strings.Contains(got, "proxy_pass") {
		t.Errorf("兜底拒绝块不允许出现 proxy_pass：\n%s", got)
	}
}

// TestRejectVhostNameFitsInclude 锁住兜底块的文件名仍落在 vhosts 包含范围内。
//
// 面板的 nginx 配置只 include `vhosts/*.conf`：名字里多一个斜杠或少了后缀，
// 生成的兜底块就不会被加载 —— 表现为"文件写了、域名限制照样不生效"。
func TestRejectVhostNameFitsInclude(t *testing.T) {
	name := RejectVhostName(8080)
	if name != "proxy-reject-8080" {
		t.Fatalf("兜底块文件名应为 proxy-reject-<port>，实际 %q", name)
	}
	if strings.ContainsAny(name, "/\\") {
		t.Error("文件名不能含路径分隔符，否则不会落在 vhosts/ 目录里")
	}
	if !strings.HasPrefix(name, "proxy-") {
		t.Error("必须与规则文件共用 proxy- 前缀，避免和站点 <域名>.conf 撞名")
	}
}

// TestPreserveHostAlsoRewritesPortlessUpstreamLocation 锁住 2026-09-17 用户报障：
//
// 站点被反代（TLS 在这一层终止）时，上游应用可能按**自己保存的、不带端口的**站点
// 地址生成绝对跳转（WordPress siteurl）。公网只放行 8889，跳到 https://<域名>/
// 就是打不开。所以 PreserveHost 打开时，除了 Host 头用 $http_host，还要把
// **本规则域名的**绝对 Location 改写成客户端真正请求的 authority（带端口）。
// 跨域跳转必须保持原样。
func TestPreserveHostAlsoRewritesPortlessUpstreamLocation(t *testing.T) {
	r := &Rule{ID: 7, Name: "wp", Listen: 8889, Domains: "wp.zizdog.com",
		Target: "https://127.0.0.1:443", Enabled: true, Websocket: true,
		PreserveHost: true, SSLEnabled: true, SSLProvider: "manual",
		SSLCert: "/tmp/c", SSLKey: "/tmp/k"}
	got, err := r.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	want := `proxy_redirect ~^https?://wp\.zizdog\.com(:[0-9]+)?(/.*)$ $scheme://$http_host$2;`
	if !strings.Contains(got, want) {
		t.Errorf("缺少按客户端 authority 改写 Location 的 proxy_redirect：\n%s", got)
	}
	// 同端口上的另一条规则若没开 PreserveHost，就不该有这行（避免意外的跨站改写）。
	r2 := &Rule{ID: 8, Name: "nas", Listen: 8889, Domains: "site2.zizdog.com",
		Target: "http://192.168.1.8:8081", Enabled: true, Websocket: true}
	got2, err := r2.Generate("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got2, "proxy_redirect") {
		t.Errorf("没开 PreserveHost 的规则不该注入 proxy_redirect：\n%s", got2)
	}
}
