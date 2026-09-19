package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizpanel/internal/upgrade"
)

// TestUpgradeStatusExposesProgressAndLogs 是"前端能拿到真实进度"的接口门禁。
//
// 用户报的问题是"页面上没有明显的升级进度"，而前端唯一的进度来源就是这个
// GET /api/v1/system/upgrade。所以这里断言接口**如实透出**结构化进度与日志：
// 阶段 id、已下载/总字节、百分比、日志行 —— 少任何一项，界面就画不出那块面板。
func TestUpgradeStatusExposesProgressAndLogs(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	// 模拟后端在真实下载过程中写下的状态（写入逻辑本身由 upgrade 包的单测覆盖）。
	st := &upgrade.State{
		Status: upgrade.StatusDownloading,
		From:   "1.0.0",
		To:     "1.0.1",
	}
	sw := upgrade.NewStateWriter(srv.Cfg.WorkDir, st)
	sw.StartDownload(1000)
	sw.Stage(upgrade.StageDownload)
	sw.Log("info", "开始下载升级包")
	sw.SetDownloaded(250, 1000)
	sw.Save()

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/upgrade", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("状态接口应 200，实际 %d：%v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应缺少 data：%v", out)
	}
	stJSON, _ := data["state"].(map[string]any)
	if stJSON == nil {
		t.Fatalf("响应缺少 state：%v", data)
	}
	prog, _ := stJSON["progress"].(map[string]any)
	if prog == nil {
		t.Fatalf("state 里没有 progress —— 前端拿不到进度条要的数据：%v", stJSON)
	}
	if got := prog["stage"]; got != upgrade.StageDownload {
		t.Errorf("progress.stage 应为 %q，实际 %v", upgrade.StageDownload, got)
	}
	if got := prog["stage_label"]; got != upgrade.StageLabel(upgrade.StageDownload) {
		t.Errorf("progress.stage_label 应为 %q，实际 %v", upgrade.StageLabel(upgrade.StageDownload), got)
	}
	if got, _ := prog["downloaded_bytes"].(float64); got != 250 {
		t.Errorf("progress.downloaded_bytes 应为 250，实际 %v", prog["downloaded_bytes"])
	}
	if got, _ := prog["total_bytes"].(float64); got != 1000 {
		t.Errorf("progress.total_bytes 应为 1000，实际 %v", prog["total_bytes"])
	}
	if got, _ := prog["percent"].(float64); got != 25 {
		t.Errorf("progress.percent 应为 25，实际 %v", prog["percent"])
	}
	logs, _ := stJSON["logs"].([]any)
	if len(logs) == 0 {
		t.Fatalf("state 里没有 logs —— logbox 会是空的：%v", stJSON)
	}
	first, _ := logs[0].(map[string]any)
	if first == nil || first["text"] == "" {
		t.Errorf("日志行应有 text 字段，实际 %v", logs[0])
	}
}

// TestUpgradeStatusToleratesLegacyState 保证加字段对老状态向后兼容：
// 磁盘上是一份没有 progress/logs 的旧 state.json 时，接口必须照常 200。
func TestUpgradeStatusToleratesLegacyState(t *testing.T) {
	srv, ts := newTestServer(t)
	cookies := loginPanel(t, ts)

	dir := filepath.Join(srv.Cfg.WorkDir, "upgrade")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"status":"failed","from":"1.0.0","to":"1.0.1","stage":"","message":"旧格式状态","error":"校验失败"}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/upgrade", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("旧格式状态应仍能读取，实际 %d：%v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	stJSON, _ := data["state"].(map[string]any)
	if stJSON == nil || stJSON["status"] != "failed" || stJSON["error"] != "校验失败" {
		t.Fatalf("旧字段必须原样保留：%v", stJSON)
	}
	if _, ok := stJSON["progress"]; ok {
		t.Error("没有进度时不应凭空造一个 progress 出来")
	}
}
