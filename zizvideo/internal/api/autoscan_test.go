package api_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// P4 门禁：自动扫描设置（仅管理员 / 越界拒绝 / 回读一致 / 关闭时状态如实）。

type autoScanSettingsView struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
	EventsEnabled   bool `json:"events_enabled"`
	DebounceSeconds int  `json:"debounce_seconds"`
}

type autoScanView struct {
	Settings   autoScanSettingsView `json:"settings"`
	EventsOK   bool                 `json:"events_ok"`
	EventsNote string               `json:"events_note"`
	LastRun    *struct {
		Trigger string `json:"trigger"`
		Status  string `json:"status"`
		Note    string `json:"note"`
	} `json:"last_run"`
	Summary string `json:"summary"`
}

func getAutoScan(t *testing.T, e *env) autoScanView {
	t.Helper()
	res, env, raw := e.do(http.MethodGet, "/api/v1/admin/autoscan", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("读取自动扫描设置失败 %d: %s", res.StatusCode, raw)
	}
	var view autoScanView
	decodeInto(t, env.Data, &view)
	return view
}

func TestAutoScanRequiresAdmin(t *testing.T) {
	e := newEnv(t)
	// 未登录：401。
	res, raw := e.call(http.MethodGet, "/api/v1/admin/autoscan", nil, false)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录读取 = %d，期望 401: %s", res.StatusCode, raw)
	}
	e.setupAdmin()
	if res, _, raw := e.write(http.MethodPost, "/api/v1/users",
		map[string]any{"username": "bob", "password": "bobpass123", "role": "user"}); res.StatusCode != http.StatusCreated {
		t.Fatalf("建普通用户失败 %d: %s", res.StatusCode, raw)
	}
	c := e.anonClient()
	if res, raw := e.loginAs(c, "bob", "bobpass123"); res.StatusCode != http.StatusOK {
		t.Fatalf("普通用户登录失败 %d: %s", res.StatusCode, raw)
	}
	if res, _, raw := e.doAs(c, http.MethodGet, "/api/v1/admin/autoscan"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户读取 = %d，期望 403: %s", res.StatusCode, raw)
	}
	if res, _, raw := e.writeAs(c, http.MethodPatch, "/api/v1/admin/autoscan",
		map[string]any{"enabled": false}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户修改 = %d，期望 403: %s", res.StatusCode, raw)
	}
	if res, _, raw := e.writeAs(c, http.MethodPost, "/api/v1/admin/autoscan/run", map[string]any{}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户触发 = %d，期望 403: %s", res.StatusCode, raw)
	}
}

func TestAutoScanDefaultsAndValidation(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()

	view := getAutoScan(t, e)
	if !view.Settings.Enabled || view.Settings.IntervalMinutes != 5 ||
		!view.Settings.EventsEnabled || view.Settings.DebounceSeconds != 8 {
		t.Fatalf("默认值不对: %+v", view.Settings)
	}
	if view.LastRun != nil {
		t.Fatalf("初次不应有运行记录: %+v", view.LastRun)
	}
	if view.Summary == "" {
		t.Fatalf("状态摘要为空")
	}

	// 越界一律 400，且不得落库。
	for _, bad := range []map[string]any{
		{"interval_minutes": 0},
		{"interval_minutes": 1441},
		{"interval_minutes": -1},
		{"debounce_seconds": 4},
		{"debounce_seconds": 11},
	} {
		res, _, raw := e.write(http.MethodPatch, "/api/v1/admin/autoscan", bad)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%v = %d，期望 400: %s", bad, res.StatusCode, raw)
		}
	}
	if got := getAutoScan(t, e); got.Settings.IntervalMinutes != 5 || got.Settings.DebounceSeconds != 8 {
		t.Fatalf("越界请求污染了设置: %+v", got.Settings)
	}

	// 合法修改 + 回读一致。
	res, _, raw := e.write(http.MethodPatch, "/api/v1/admin/autoscan",
		map[string]any{"interval_minutes": 10, "debounce_seconds": 6, "enabled": false})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("合法修改失败 %d: %s", res.StatusCode, raw)
	}
	got := getAutoScan(t, e)
	if got.Settings.IntervalMinutes != 10 || got.Settings.DebounceSeconds != 6 || got.Settings.Enabled {
		t.Fatalf("回读不一致: %+v", got.Settings)
	}
	if got.Summary != "自动扫描已关闭" {
		t.Fatalf("关闭时摘要应如实: %q", got.Summary)
	}
}

func TestAutoScanRunHonestWhenDisabled(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	if res, _, raw := e.write(http.MethodPatch, "/api/v1/admin/autoscan",
		map[string]any{"enabled": false}); res.StatusCode != http.StatusOK {
		t.Fatalf("关闭失败 %d: %s", res.StatusCode, raw)
	}
	res, _, raw := e.write(http.MethodPost, "/api/v1/admin/autoscan/run", map[string]any{})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("关闭时触发 = %d，期望 409: %s", res.StatusCode, raw)
	}
}

// 门禁：不可读媒体根手动触发一轮 → 202 且如实 skipped，绝不报成功；无 task_id。
func TestAutoScanRunUnreadableRootHonest(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	e.newLibrary("坏库", filepath.Join(e.Root, "not-mounted"))

	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/autoscan/run", map[string]any{})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("触发 = %d，期望 202: %s", res.StatusCode, raw)
	}
	var body struct {
		Run struct {
			Status  string `json:"status"`
			Started int    `json:"started"`
			Skipped int    `json:"skipped"`
			Note    string `json:"note"`
		} `json:"run"`
		TaskIDs []string `json:"task_ids"`
		Note    string   `json:"note"`
	}
	decodeInto(t, env.Data, &body)
	if body.Run.Status != "skipped" || body.Run.Started != 0 || body.Run.Skipped != 1 {
		t.Fatalf("不可读根必须如实跳过: %+v", body.Run)
	}
	if !strings.Contains(body.Run.Note, "媒体根不可读") {
		t.Fatalf("原因不真实: %q", body.Run.Note)
	}
	if len(body.TaskIDs) != 0 {
		t.Fatalf("不可读根不得产生扫描任务: %v", body.TaskIDs)
	}
	if !strings.Contains(body.Note, "媒体根不可读") {
		t.Fatalf("响应 note 与 run 结论不一致: %q", body.Note)
	}
}
