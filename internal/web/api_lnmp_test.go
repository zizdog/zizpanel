package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  一键 LNMP 的"先选版本再安装"接口契约（2026-09-19 用户要求）
//
//  服务层的校验与派生已经有单测（internal/services/lnmp_options_test.go）。
//  这里只锁**HTTP 契约**，也就是前端实际依赖的那部分：
//    · GET /market/lnmp-options 返回分组候选 + 默认选择；
//    · POST /market/install-lnmp 的 body 非法时**在开任务之前**就 400 + 人话；
//    · 空 body 仍然合法（向后兼容：老调用方不带 body）。
//
//  全部走 newTestServer 的沙箱（临时 HOME / 临时 Homebrew 前缀 / 假 brew），
//  不碰真实服务、真实家目录、真实 launchd。
// ============================================================================

// jsonBlob 把响应里的 data 重新序列化，用于"整段文本里不该出现某个词"这类断言。
func jsonBlob(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLNMPOptionsEndpointShape 锁住候选接口的形状（前端按它渲染三组单选）。
func TestLNMPOptionsEndpointShape(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/market/lnmp-options", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("候选接口应 200，实际 %d", res.StatusCode)
	}
	data, _ := out["data"].(map[string]any)
	groups, _ := data["groups"].([]any)
	if len(groups) != 3 {
		t.Fatalf("候选应有 3 组（nginx / PHP / MySQL），实际 %d 组：%+v", len(groups), data)
	}
	wantKeys := []string{"nginx", "php", "mysql"}
	for i, g := range groups {
		gm, _ := g.(map[string]any)
		if got := asString(gm["key"]); got != wantKeys[i] {
			t.Errorf("第 %d 组的 key 应为 %s，实际 %q", i+1, wantKeys[i], got)
		}
		if asString(gm["label"]) == "" {
			t.Errorf("「%s」组缺少中文显示名（弹窗里会是一片空白）", wantKeys[i])
		}
		if asString(gm["selected"]) == "" {
			t.Errorf("「%s」组没有默认选中项（前端无从选中）", wantKeys[i])
		}
		opts, _ := gm["options"].([]any)
		if len(opts) == 0 {
			t.Errorf("「%s」组没有候选（用户没法选）", wantKeys[i])
		}
		// recommended 必须与 selected 一致：不一致时弹窗会推荐一个"没被选中"的版本。
		for _, o := range opts {
			om, _ := o.(map[string]any)
			rec, _ := om["recommended"].(bool)
			if rec != (asString(om["formula"]) == asString(gm["selected"])) {
				t.Errorf("「%s」组 %s 的 recommended=%v 与 selected=%q 不一致",
					wantKeys[i], asString(om["formula"]), rec, asString(gm["selected"]))
			}
			if asString(om["name"]) == "" {
				t.Errorf("候选 %s 缺少展示名（弹窗里会只显示 formula）", asString(om["formula"]))
			}
		}
	}
	// 默认选择必须与后端默认同源（前端"不点直接确认"时装的与显示的要一致）
	def, _ := data["default"].(map[string]any)
	want := services.DefaultLNMPSelection()
	if asString(def["php"]) != want.PHP || asString(def["nginx"]) != want.Nginx ||
		asString(def["mysql"]) != want.MySQL {
		t.Errorf("default 与 services.DefaultLNMPSelection 不一致：%+v vs %+v", def, want)
	}
	// PostgreSQL 绝不能在候选里（一键 LNMP 的收尾对它无效）
	if strings.Contains(jsonBlob(t, data), "postgres") {
		t.Errorf("候选里出现了 PostgreSQL：一键 LNMP 的收尾（默认站点 / MySQL 初始化与 "+
			"root 凭据闭环）对它无效。实际响应：%s", jsonBlob(t, data))
	}
	// PHP 只有 8.2 与 8.4（用户明确要求），且新版在前（前端不做二次排序）
	phpGroup := groups[1].(map[string]any)
	formulas := []string{}
	for _, o := range phpGroup["options"].([]any) {
		formulas = append(formulas, asString(o.(map[string]any)["formula"]))
	}
	if strings.Join(formulas, ",") != "php@8.4,php@8.2" {
		t.Errorf("PHP 候选应为 php@8.4 与 php@8.2（新版在前），实际 %v", formulas)
	}
}

// TestInstallLNMPRejectsBadSelectionBeforeTask 锁住"非法选择不开任务"。
//
// 为什么这是产品级要求：如果先开任务再失败，用户会在任务中心看到一个红叉，
// 而真正的原因（版本选错了）本该是表单上的一句话。更糟的是**用默认值继续装**
// ——那会装出与用户选择不同的版本，而用户以为装的是自己选的。
func TestInstallLNMPRejectsBadSelectionBeforeTask(t *testing.T) {
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	before := len(srv.Tasks.List())
	cases := []struct {
		name string
		body map[string]string
		want string // 空 = 合法（应 202）
	}{
		{"postgres 顶替 MySQL", map[string]string{"php": "php@8.2", "mysql": "postgresql@17"}, "PostgreSQL"},
		{"postgres 顶替 nginx", map[string]string{"nginx": "postgresql@17"}, "PostgreSQL"},
		{"未知 PHP 版本", map[string]string{"php": "php@9.9"}, "php@9.9"},
		{"显式空值", map[string]string{"mysql": ""}, "空值"},
		{"合法：只覆盖 PHP（其余用默认）", map[string]string{"php": "php@8.4"}, ""},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/install-lnmp", c.body, cookies)
		if c.want == "" {
			// 合法输入：测试实例不是 root，任务会立刻失败，但任务必须已经建出来
			//（202 = 校验放行、走到了 launchTask）。
			if res.StatusCode != http.StatusAccepted {
				t.Errorf("%s：应 202（建任务），实际 %d：%v", c.name, res.StatusCode, out)
			}
			continue
		}
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s：应 400，实际 %d：%v", c.name, res.StatusCode, out)
			continue
		}
		msg := asString(out["msg"])
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s：400 原因里应含 %q，实际 %q", c.name, c.want, msg)
		}
	}
	// 非法的几次绝不能留下任务（只有最后那次合法 body 允许建 1 个）。
	after := srv.Tasks.List()
	if len(after) != before+1 {
		t.Errorf("非法选择不该创建任务：测试前 %d 个，现在 %d 个（只该多出合法那 1 个）",
			before, len(after))
	}
}

// TestInstallLNMPEmptyBodyStillAccepted 锁住向后兼容。
//
// 老前端/脚本调这个接口时**不带 body**（改造前就是这样），升级面板后
// 不能让它们突然收到 400 —— 那会把"重跑一次一键 LNMP"变成"表单都没了"。
func TestInstallLNMPEmptyBodyStillAccepted(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	// body=nil 时 doJSON 不发任何内容（等价于"老调用方空 body"）。
	for _, c := range []struct {
		name string
		body any
	}{
		{"不带 body", nil},
		{"空对象", map[string]string{}},
	} {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/market/install-lnmp", c.body, cookies)
		if res.StatusCode != http.StatusAccepted {
			t.Errorf("%s 应继续被接受（默认三件套），实际 %d：%v", c.name, res.StatusCode, out)
		}
	}
}
