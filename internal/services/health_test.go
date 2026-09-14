package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
//  健康检查的判定语义
//
//  用户报过的误报：Stirling PDF 装完首次打开要求设账号密码（应用自己的登录），
//  设完之后健康检查拿到 401，面板就报"健康检查失败" —— 服务其实完全正常。
//  HTTP 健康检查问的是"你还活着吗"，401/403 恰恰证明活着。
// ============================================================================

// healthForCode 起一个只会返回固定状态码的本地服务，检查它。
// 测试自己起服务、结束就关，不碰任何真实服务。
func healthForCode(t *testing.T, code int, body string) Health {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	return httpHealth(context.Background(), &Service{Name: "t", HealthURL: ts.URL + "/"})
}

func TestHealthTreatsAuthRequiredAsHealthy(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		h := healthForCode(t, code, "Unauthorized")
		if !h.Checked {
			t.Fatalf("HTTP %d：应当标记为已检查", code)
		}
		if !h.OK {
			t.Errorf("HTTP %d 说明服务在监听并能处理请求，不该判成不健康（用户看到的会是误报）", code)
		}
		if !strings.Contains(h.Message, "身份验证") {
			t.Errorf("HTTP %d 的说明要讲清「需要身份验证」，实际 %q", code, h.Message)
		}
		if h.Code != code {
			t.Errorf("状态码要如实保留，期望 %d 实际 %d", code, h.Code)
		}
	}
}

func TestHealthStillFailsOnRealProblems(t *testing.T) {
	cases := []struct {
		code int
		why  string
	}{
		{http.StatusNotFound, "地址写错了：404 是要修的问题，不能算健康"},
		{http.StatusInternalServerError, "服务端 500 是真故障"},
		{http.StatusBadGateway, "网关错误是真故障"},
	}
	for _, c := range cases {
		h := healthForCode(t, c.code, "")
		if h.OK {
			t.Errorf("HTTP %d 不该算健康：%s", c.code, c.why)
		}
	}
}

func TestHealthAcceptsSuccessCodes(t *testing.T) {
	for _, code := range []int{200, 204, 301, 302} {
		h := healthForCode(t, code, "ok")
		if !h.OK {
			t.Errorf("HTTP %d 应算健康，实际 %+v", code, h)
		}
	}
}

// TestHealthExpectOverridesAuthLeniency 锁住优先级：
// 用户显式配了「期望包含内容」时，一切以内容为准 ——
// 哪怕返回 401，只要内容对得上就算健康、对不上就算不健康。
func TestHealthExpectOverridesAuthLeniency(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	h := httpHealth(context.Background(), &Service{
		Name: "t", HealthURL: ts.URL + "/", HealthExpect: `"status":"ok"`,
	})
	if !h.OK {
		t.Errorf("期望内容匹配时应算健康，实际 %+v", h)
	}

	h = httpHealth(context.Background(), &Service{
		Name: "t", HealthURL: ts.URL + "/", HealthExpect: `"status":"degraded"`,
	})
	if h.OK {
		t.Error("期望内容不匹配时应算不健康（这是用户显式要求的校验）")
	}
}

// TestHealthUnreachableIsFriendly 锁住"连不上"的报错是人话。
func TestHealthUnreachableIsFriendly(t *testing.T) {
	// 端口 1 上不会有服务；curl 立刻拒绝。
	//
	// 这里断言"是人话"而不是"非空"：实测 Homebrew 的 curl 报的是
	// "Failed to connect to 127.0.0.1 port 1 after 0 ms: Couldn't connect to server"，
	// 而老的 humanize 只认 "Connection refused"，于是整行原始报错被原样丢给用户。
	h := httpHealth(context.Background(), &Service{Name: "t", HealthURL: "http://127.0.0.1:1/"})
	if h.OK {
		t.Fatal("连不上不该算健康")
	}
	if !strings.Contains(h.Message, "连接不上") && !strings.Contains(h.Message, "连接超时") {
		t.Errorf("连不上时应给一句人话（如「连接不上（该端口没有在监听…）」），实际 %q", h.Message)
	}
	if strings.Contains(h.Message, "curl:") || strings.Contains(h.Message, "exit status") {
		t.Errorf("不该把 curl 的原始报错丢给用户，实际 %q", h.Message)
	}
}

// TestHealthNoURLIsSkipped 没有检查地址时不该假装检查过。
func TestHealthNoURLIsSkipped(t *testing.T) {
	h := httpHealth(context.Background(), &Service{Name: "x"})
	if h.Checked || h.OK {
		t.Errorf("没有健康检查地址时应报「未检查」，实际 %+v", h)
	}
}

// TestListReportsAuthRequiredAsHealthy 是**用户可见契约**那一层：
// 服务列表接口给前端的 health.ok 必须是 true，
// 否则卡片上的红标签和页头"N 个健康检查失败"会继续误报。
func TestListReportsAuthRequiredAsHealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Please log in"))
	}))
	defer ts.Close()

	repo := newTestRepo(t)
	// 用 native + 一个不存在的 label：DriverFor 能给出驱动（状态会报未安装），
	// 但健康检查仍然会真的去打 HTTP —— 这正是要验证的那条路径。
	svc := &Service{
		Name: "stirling-pdf", DisplayName: "Stirling PDF", Kind: KindNative,
		LaunchLabel: "com.zizdog.test-auth-health",
		HealthURL:   ts.URL + "/", HealthExpect: "",
	}
	if err := repo.Create(context.Background(), svc); err != nil {
		t.Fatalf("登记服务失败: %v", err)
	}

	// 直接走 Manager.List（它就是 /api/v1/services 的入口）
	mm := NewManager(repo, Options{UserHome: t.TempDir(), UserName: "zizdog"})
	got, err := mm.List(context.Background(), true)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应返回 1 个服务，实际 %d", len(got))
	}
	if !got[0].Health.Checked {
		t.Fatal("配了检查地址就该真的检查")
	}
	if !got[0].Health.OK {
		t.Fatalf("要登录的服务（HTTP 401）应报告健康，实际 %+v", got[0].Health)
	}
	if !strings.Contains(got[0].Health.Message, "身份验证") {
		t.Errorf("说明里要讲清为什么算健康，实际 %q", got[0].Health.Message)
	}
}
