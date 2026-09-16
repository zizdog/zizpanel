package web

import (
	"testing"
)

// ============================================================================
//  基础依赖只读接口（GET /api/v1/system/deps）
//
//  为什么要这个接口：ffmpeg 曾经"静默消失"过（2026-09-16，被 brew autoremove
//  之类带走），而它的缺席不会让任何健康检查变红 —— 表现只是 TTS 合成返回
//  HTTP 200 + 0 字节 body，用户看到"所有作业全败"却完全推不到"缺 ffmpeg"。
//  这个接口就是那个"能一眼看到"的入口。
// ============================================================================

// TestSystemDepsEndpointReturnsBaseDependencies
// 接口必须如实列出每一项，并且每项都带"为什么需要/缺失怎么办"。
func TestSystemDepsEndpointReturnsBaseDependencies(t *testing.T) {
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/deps", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("基础依赖接口应 200，实际 %d", res.StatusCode)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatal("响应里没有 data")
	}
	list, _ := data["list"].([]any)
	if len(list) == 0 {
		t.Fatal("基础依赖列表不能为空（至少要含 ffmpeg）")
	}
	if _, ok := data["missing"].(float64); !ok {
		t.Errorf("响应应含 missing 计数，实际 %v", data["missing"])
	}
	if _, ok := data["all_satisfied"].(bool); !ok {
		t.Errorf("响应应含 all_satisfied，实际 %v", data["all_satisfied"])
	}

	var ffmpeg map[string]any
	for _, it := range list {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		if _, ok := m["satisfied"].(bool); !ok {
			t.Errorf("%v 缺少 satisfied 布尔值", m["command"])
		}
		// 只报"缺"而不说后果，用户没法判断该不该处理 —— 这个项目反复吃过这个亏。
		if asString(m["why"]) == "" {
			t.Errorf("%v 缺少 why（为什么需要它）", m["command"])
		}
		if asString(m["hint"]) == "" {
			t.Errorf("%v 缺少 hint（缺失时怎么办）", m["command"])
		}
		if m["command"] == "ffmpeg" {
			ffmpeg = m
		}
	}
	if ffmpeg == nil {
		t.Fatal("基础依赖列表里必须有 ffmpeg —— 否则缺它时用户无处可见")
	}
	if asString(ffmpeg["fix_cmd"]) == "" {
		t.Errorf("ffmpeg 的补救命令不能为空，实际 %v", ffmpeg["fix_cmd"])
	}
}

// TestSystemDepsRequiresAuth 只读接口也要登录：它会暴露这台机器装了什么。
func TestSystemDepsRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t)
	res, _, _ := doJSON(t, ts, "GET", "/api/v1/system/deps", nil, nil)
	if res.StatusCode != 401 {
		t.Fatalf("未登录访问基础依赖应 401，实际 %d", res.StatusCode)
	}
}
