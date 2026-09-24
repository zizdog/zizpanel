package web

import (
	"os"
	"strings"
	"testing"
)

// 门禁（坑 165/193）：服务动作 HTTP 快路径只做针对性探测 —— 不得在这里调用
// 全局昂贵探测（brew services list / 全量服务列表），也不要多余的
// mgr.Get（它会顺带做状态 + 健康探测，可能白等好几秒）。
func TestHandleServiceActionHasNoGlobalProbe(t *testing.T) {
	body := funcBodyForTest(t, "api_services.go", "func (s *Server) handleServiceAction(")
	for _, bad := range []string{
		"mgr.List(", "BrewCapture", `"services", "list"`, "brew services list",
		// mgr.Get 会给每个动作白做一次状态/健康探测；判 Qwen 只需读服务记录。
		"mgr.Get(",
	} {
		if strings.Contains(body, bad) {
			t.Errorf("handleServiceAction 出现全局昂贵探测/多余探测 %q（坑 225）", bad)
		}
	}
}

// 门禁：启动记忆必须是**进程级单例**。svcManager() 每次请求都新建 Manager，
// 记忆若建在 Manager 身上，点完启动的那一次刷新又会显示「已停止」
// （2026-09-24 实际踩到）。
func TestServiceManagerSharesProcessStartHints(t *testing.T) {
	body := funcBodyForTest(t, "api_services.go", "func (s *Server) svcManager(")
	if !strings.Contains(body, "s.startHints") {
		t.Error("svcManager 必须注入 Server 自己那一份 StartHints（进程级单例）")
	}
	if strings.Contains(body, "&services.StartHintStore{") {
		t.Error("svcManager 不得每次请求新建 StartHintStore —— 那样启动记忆活不过一次请求")
	}
	if !strings.Contains(funcBodyForTest(t, "server.go", "func New("), "startHints:") {
		t.Error("New() 必须为 Server 初始化 startHints 单例")
	}
}

// funcBodyForTest 取出 path 里以 sig 开头那个顶层函数的源码（到下一个顶层 func 为止）。
func funcBodyForTest(t *testing.T, path, sig string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	src := string(b)
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("%s 里找不到函数 %q", path, sig)
	}
	rest := src[i:]
	if j := strings.Index(rest[1:], "\nfunc "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}
