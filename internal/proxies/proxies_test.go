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
//  2. PreserveHost 打开时透传原始 Host。
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
	if !strings.Contains(got2, "proxy_set_header Host $host;") {
		t.Errorf("PreserveHost 打开时应透传原始 Host：\n%s", got2)
	}
	// 非默认端口要带上
	r3 := &Rule{ID: 3, Name: "z", Listen: 8083, Target: "https://example.com:8443", Enabled: true}
	got3, _ := r3.Generate("")
	if !strings.Contains(got3, "proxy_set_header Host example.com:8443;") {
		t.Errorf("非默认端口应写进 Host 头：\n%s", got3)
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
