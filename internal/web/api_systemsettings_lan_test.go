package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zizdog/zizpanel/internal/sysconfig"
)

// ============================================================================
//  POST /api/v1/system/settings/lan-preauth
//
//  这个接口会写 macOS 偏好域，所以测试里**只有假实现**：
//  探针 / 写入 / 撤销三个包级 fn 变量全部换掉，绝不跑 defaults、
//  绝不读真实偏好域、也不需要重启。
//
//  这里同时锁住两件容易退化的事：
//   1. 路由优先级：/lan-preauth 必须命中这个处理器，而不是 {action} 通配
//      （后者会回 400「未知的系统设置动作」）；
//   2. 诚实：写入失败绝不能返回成功。
// ============================================================================

// withLANEndpointFakes 换掉三个注入点并在测试结束还原，返回记录用的计时器。
type lanEndpointFake struct {
	probeCalls  int
	appliedRaw  []string
	rollbackCnt int
	applyErr    error
	rollbackErr error
	states      []sysconfig.LANPreauthState // 探针依次返回的状态（用完复用最后一个）
}

func withLANEndpointFakes(t *testing.T, f *lanEndpointFake) {
	t.Helper()
	prevProbe, prevApply, prevRollback := lanPreauthProbeFn, lanPreauthApplyFn, lanPreauthRollbackFn
	lanPreauthProbeFn = func(context.Context) sysconfig.LANPreauthState {
		f.probeCalls++
		if len(f.states) == 0 {
			return sysconfig.LANPreauthState{Supported: true, Readable: true}
		}
		idx := f.probeCalls - 1
		if idx >= len(f.states) {
			idx = len(f.states) - 1
		}
		return f.states[idx]
	}
	lanPreauthApplyFn = func(_ context.Context, rawCIDRs string, _ sysconfig.LogFunc) error {
		f.appliedRaw = append(f.appliedRaw, rawCIDRs)
		return f.applyErr
	}
	lanPreauthRollbackFn = func(context.Context, sysconfig.LogFunc) error {
		f.rollbackCnt++
		return f.rollbackErr
	}
	t.Cleanup(func() {
		lanPreauthProbeFn, lanPreauthApplyFn, lanPreauthRollbackFn = prevProbe, prevApply, prevRollback
	})
}

func setupAdmin(t *testing.T) (*Server, *httptest.Server, []*http.Cookie) {
	t.Helper()
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	return srv, ts, cookies
}

func TestLANPreauthEndpointApplyReturnsProbedState(t *testing.T) {
	_, ts, cookies := setupAdmin(t)
	f := &lanEndpointFake{states: []sysconfig.LANPreauthState{{
		Supported: true, Readable: true, Enabled: true,
		SystemSet: true, UserSet: true, CIDRs: []string{"192.168.1.0/24"},
	}}}
	withLANEndpointFakes(t, f)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/settings/lan-preauth",
		map[string]any{"enabled": true, "cidrs": "192.168.1.5/24"}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("应用应 200（若为 400 说明命中了 {action} 通配），实际 %d: %v", res.StatusCode, out)
	}
	if len(f.appliedRaw) != 1 || f.appliedRaw[0] != "192.168.1.0/24" {
		t.Fatalf("服务层应收到归一化后的 CIDR，实际 %v", f.appliedRaw)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应缺少 data：%v", out)
	}
	if data["enabled"] != true {
		t.Errorf("应用后应报告 enabled=true，实际 %v", data["enabled"])
	}
	// 刚写完一定还没生效：必须提示需要重启。
	if data["reboot_required"] != true {
		t.Errorf("刚写入必须 reboot_required=true，实际 %v", data["reboot_required"])
	}
	if asString(data["reboot_note"]) == "" {
		t.Error("需要重启时必须说明原因")
	}
	// 代价必须随状态一起给前端（对所有程序生效）。
	if asString(data["warning"]) == "" && asString(data["note"]) == "" {
		t.Error("响应应带上代价/状态说明")
	}
}

func TestLANPreauthEndpointRejectsInvalidCIDRBeforeWriting(t *testing.T) {
	_, ts, cookies := setupAdmin(t)
	f := &lanEndpointFake{}
	withLANEndpointFakes(t, f)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/settings/lan-preauth",
		map[string]any{"enabled": true, "cidrs": "这不是网段"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 CIDR 应 400，实际 %d: %v", res.StatusCode, out)
	}
	if len(f.appliedRaw) != 0 {
		t.Fatalf("校验失败时绝不能调用写入，实际 %v", f.appliedRaw)
	}
	if asString(out["msg"]) == "" {
		t.Error("400 应带可读的错误信息")
	}
}

func TestLANPreauthEndpointDoesNotLieWhenWriteFails(t *testing.T) {
	_, ts, cookies := setupAdmin(t)
	f := &lanEndpointFake{applyErr: errors.New("defaults 拒绝写入（Operation not permitted）")}
	withLANEndpointFakes(t, f)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/settings/lan-preauth",
		map[string]any{"enabled": true, "cidrs": "192.168.1.0/24"}, cookies)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("写入失败应 500，实际 %d: %v", res.StatusCode, out)
	}
	if out["ok"] == true {
		t.Fatal("写入失败绝不能返回 ok=true")
	}
	if data, ok := out["data"]; ok && data != nil {
		t.Fatalf("失败响应不该带成功数据，实际 %v", data)
	}
}

func TestLANPreauthEndpointRollbackMarksRebootRequired(t *testing.T) {
	_, ts, cookies := setupAdmin(t)
	f := &lanEndpointFake{states: []sysconfig.LANPreauthState{
		// 第一次探针 = 撤销前的状态（确实写过 → 撤销后才需要重启恢复）
		{Supported: true, Readable: true, Enabled: true, SystemSet: true, UserSet: true, CIDRs: []string{"192.168.1.0/24"}},
		// 第二次 = 撤销后的状态
		{Supported: true, Readable: true},
	}}
	withLANEndpointFakes(t, f)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/settings/lan-preauth",
		map[string]any{"enabled": false}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("撤销应 200，实际 %d: %v", res.StatusCode, out)
	}
	if f.rollbackCnt != 1 {
		t.Fatalf("应调用一次撤销，实际 %d", f.rollbackCnt)
	}
	if f.probeCalls < 2 {
		t.Fatalf("撤销前后都要探测真实状态，实际探测 %d 次", f.probeCalls)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil || data["enabled"] != false {
		t.Fatalf("撤销后应报告 enabled=false，实际 %v", out)
	}
	if data["reboot_required"] != true {
		t.Errorf("撤销写入过设置后要重启才恢复隐私门，实际 %v", data["reboot_required"])
	}
}

func TestLANPreauthEndpointRollbackErrorIsReported(t *testing.T) {
	_, ts, cookies := setupAdmin(t)
	f := &lanEndpointFake{rollbackErr: errors.New("defaults delete 失败")}
	withLANEndpointFakes(t, f)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/settings/lan-preauth",
		map[string]any{"enabled": false}, cookies)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("撤销失败应 500，实际 %d: %v", res.StatusCode, out)
	}
	if out["ok"] == true {
		t.Fatal("撤销失败绝不能返回 ok=true")
	}
}

// TestLANPreauthEndpointRequiresCSRF 锁住"写操作走同一套 CSRF 中间件"的约定。
func TestLANPreauthEndpointRequiresCSRF(t *testing.T) {
	_, ts, cookies := setupAdmin(t)
	f := &lanEndpointFake{}
	withLANEndpointFakes(t, f)

	res, _, _ := doJSONOpt(t, ts, "POST", "/api/v1/system/settings/lan-preauth",
		map[string]any{"enabled": true, "cidrs": "192.168.1.0/24"}, cookies, true)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 头应 403，实际 %d", res.StatusCode)
	}
	if len(f.appliedRaw) != 0 {
		t.Fatal("CSRF 校验失败时不该执行写入")
	}
}
