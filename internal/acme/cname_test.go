package acme

import (
	"os"
	"strings"
	"testing"
)

// TestCNAMEPolicyDisablesFollowByDefault 是这条真机坑的门禁。
//
// 用户域名有泛解析 `* CNAME lede.zizdog.com` 时，lego 会把挑战 TXT 写到
// `lede.zizdog.com` 上，而 Let's Encrypt **不跟随泛解析 CNAME**，
// 于是永远报 "No TXT record found" —— 面板日志却显示"记录已提交"，极难排查。
// 默认必须关掉跟随（即设置 LEGO_DISABLE_CNAME_SUPPORT=1），并且用完要还原。
func TestCNAMEPolicyDisablesFollowByDefault(t *testing.T) {
	m, _, _ := newTestManager(t)
	t.Setenv(envDisableCNAME, "0") // 预置一个值，验证还原成它

	restore := m.applyCNAMEPolicy()
	if got := os.Getenv(envDisableCNAME); got != "1" {
		t.Fatalf("%s 应被设为 1，实际 %q", envDisableCNAME, got)
	}
	restore()
	if got := os.Getenv(envDisableCNAME); got != "0" {
		t.Fatalf("还原后应回到 0，实际 %q", got)
	}
}

// TestCNAMEPolicyUnsetsWhenItWasUnset 覆盖"调用前该变量不存在"的还原路径。
func TestCNAMEPolicyUnsetsWhenItWasUnset(t *testing.T) {
	m, _, _ := newTestManager(t)
	_ = os.Unsetenv(envDisableCNAME)
	t.Cleanup(func() { _ = os.Unsetenv(envDisableCNAME) })

	restore := m.applyCNAMEPolicy()
	if got := os.Getenv(envDisableCNAME); got != "1" {
		t.Fatalf("%s 应被设为 1，实际 %q", envDisableCNAME, got)
	}
	restore()
	if _, ok := os.LookupEnv(envDisableCNAME); ok {
		t.Fatalf("调用前不存在该变量时，还原后必须仍然不存在")
	}
}

// TestCNAMEPolicyFollowLeavesEnvUntouched：显式要求跟随委派时不设该变量（保持 lego 默认）。
func TestCNAMEPolicyFollowLeavesEnvUntouched(t *testing.T) {
	m, rec, _ := newTestManager(t)
	m.DNS01FollowCNAME = true
	_ = os.Unsetenv(envDisableCNAME)
	t.Cleanup(func() { _ = os.Unsetenv(envDisableCNAME) })

	restore := m.applyCNAMEPolicy()
	if _, ok := os.LookupEnv(envDisableCNAME); ok {
		t.Fatalf("跟随模式下不该设置 %s", envDisableCNAME)
	}
	restore()
	if !strings.Contains(rec.all(), "跟随 CNAME 委派") {
		t.Fatalf("跟随模式应留下一条说明日志，实际：%v", rec.all())
	}
}
