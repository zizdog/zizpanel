package services

import "testing"

// TestParseBrewOutdatedJSON 锁住"没看懂 ≠ 已是最新"。
//
// 这条判据是更新检测最危险的一处：解析失败如果返回空 map + true，界面上就是
// "没有可更新的"（等于对用户说"已是最新"），而真实情况是"我们没查成"。
func TestParseBrewOutdatedJSON(t *testing.T) {
	// ① 正常输出：给出 formula → 上游当前版本。
	vers, ok := parseBrewOutdatedJSON([]byte(`{"formulae":[{"name":"nginx","installed_versions":["1.27.0"],"current_version":"1.29.0"},{"name":"php@8.2","installed_versions":["8.2.20"],"current_version":"8.2.33"}],"casks":[]}`))
	if !ok || vers["nginx"] != "1.29.0" || vers["php@8.2"] != "8.2.33" {
		t.Fatalf("正常输出应解析出两个可升级包，实际 ok=%v map=%v", ok, vers)
	}

	// ② 确实没有可升级的：空 formulae 是**真结论**（ok=true），必须与④区分开。
	vers, ok = parseBrewOutdatedJSON([]byte(`{"formulae":[],"casks":[]}`))
	if !ok || len(vers) != 0 {
		t.Fatalf("空 formulae 应表示'确实没有可升级的'（ok=true, 空 map），实际 ok=%v map=%v", ok, vers)
	}

	// ③ 合并流里混进提示行：仍要能解析出 JSON（执行通道把 stderr 合成一路）。
	vers, ok = parseBrewOutdatedJSON([]byte("Warning: Some installed formulae are deprecated\n" +
		`{"formulae":[{"name":"vips","current_version":"8.16.0"}],"casks":[]}` + "\n"))
	if !ok || vers["vips"] != "8.16.0" {
		t.Fatalf("提示行混入时应仍能解析出 JSON，实际 ok=%v map=%v", ok, vers)
	}

	// ④ 看不懂的输出：必须 ok=false —— 不许当成"已是最新"。
	if _, ok := parseBrewOutdatedJSON([]byte("brew: command not found\n")); ok {
		t.Fatalf("无法解析时必须 ok=false（否则会把'没查成'谎报成'已是最新'）")
	}
}
