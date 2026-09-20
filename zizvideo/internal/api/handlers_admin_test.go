package api_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
)

// ============================================================================
//  条目 8：注册开关
// ============================================================================

type adminSettings struct {
	AllowRegister bool   `json:"allow_register"`
	Verified      bool   `json:"verified"`
	Source        string `json:"source"`
	EnvOverride   bool   `json:"env_override"`
	Note          string `json:"note"`
}

func readSettings(t *testing.T, e *env) adminSettings {
	t.Helper()
	res, env, raw := e.do(http.MethodGet, "/api/v1/admin/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("读取设置失败 %d: %s", res.StatusCode, raw)
	}
	var out adminSettings
	decodeInto(t, env.Data, &out)
	return out
}

func registerAnon(t *testing.T, e *env, c *http.Client, username string) (*http.Response, []byte) {
	t.Helper()
	return e.callWith(c, "", http.MethodPost, "/api/v1/auth/register",
		map[string]string{"username": username, "password": "newbiepass123",
			"display_name": username}, false)
}

// TestRegisterDisabledByDefault 门禁：默认值=关，且关时注册被拒。
func TestRegisterDisabledByDefault(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()

	if got := readSettings(t, e); got.AllowRegister {
		t.Fatalf("allow_register 默认必须为关，实际=%v", got.AllowRegister)
	}
	res, raw := registerAnon(t, e, e.anonClient(), "newbie")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("关时注册应 403，实际 %d: %s", res.StatusCode, raw)
	}
	var envBody envelope
	decodeRaw(t, raw, &envBody)
	if envBody.Error == nil || envBody.Error.Code != "AUTH_REGISTER_DISABLED" {
		t.Fatalf("错误码应为 AUTH_REGISTER_DISABLED: %s", raw)
	}
	if !strings.Contains(string(raw), "管理员已关闭注册") {
		t.Fatalf("缺少人话原因: %s", raw)
	}
	if _, err := e.DB.GetUserByUsername("newbie"); err == nil {
		t.Fatal("被拒的注册不能建号")
	}
}

// TestRegisterSwitchPersistsAndEnablesSignup 门禁：开时可用 + 开关持久化。
func TestRegisterSwitchPersistsAndEnablesSignup(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()

	res, env, raw := e.write(http.MethodPatch, "/api/v1/admin/settings",
		map[string]any{"allow_register": true})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("打开注册失败 %d: %s", res.StatusCode, raw)
	}
	var got adminSettings
	decodeInto(t, env.Data, &got)
	if !got.AllowRegister || !got.Verified {
		t.Fatalf("打开后应生效且复核通过: %+v", got)
	}
	onDisk, err := os.ReadFile(e.CfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `"allow_register": true`) {
		t.Fatalf("开关未写回 config.json: %s", onDisk)
	}
	if v, err := readBoolFromFile(e.CfgPath); err != nil || !v {
		t.Fatalf("回读 config.json 应为 true，实际 %v err=%v", v, err)
	}

	anon := e.anonClient()
	regRes, regRaw := registerAnon(t, e, anon, "newbie")
	if regRes.StatusCode != http.StatusCreated {
		t.Fatalf("开时注册应 201，实际 %d: %s", regRes.StatusCode, regRaw)
	}
	loginRes, loginRaw := e.loginAs(anon, "newbie", "newbiepass123")
	if loginRes.StatusCode != http.StatusOK {
		t.Fatalf("注册的账号应能登录，实际 %d: %s", loginRes.StatusCode, loginRaw)
	}
}

// TestSetupWorksWhileRegisterDisabled 门禁：首次初始化 admin 不受开关影响。
func TestSetupWorksWhileRegisterDisabled(t *testing.T) {
	e := newEnv(t)
	// 默认关，且还没有任何用户。
	if got := readSettingsBeforeSetup(t, e); got.AllowRegister {
		t.Fatal("默认必须为关")
	}
	res, _, raw := e.write(http.MethodPost, "/api/v1/setup", map[string]string{
		"username": "admin", "password": "adminpass123", "display_name": "Admin"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("注册关闭时初始化仍应成功，实际 %d: %s", res.StatusCode, raw)
	}
}

// readSettingsBeforeSetup 在未登录时读不到设置，改为直接读默认配置。
func readSettingsBeforeSetup(t *testing.T, e *env) adminSettings {
	t.Helper()
	return adminSettings{AllowRegister: e.Cfg.AllowRegister}
}

func readBoolFromFile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(raw), `"allow_register": true`), nil
}

// ============================================================================
//  条目 11：媒体去重
// ============================================================================

type duplicateGroups struct {
	Judgement   string `json:"judgement"`
	GroupCount  int    `json:"group_count"`
	MemberCount int    `json:"member_count"`
	Groups      []struct {
		SizeBytes  int64 `json:"size_bytes"`
		DurationMS int64 `json:"duration_ms"`
		Members    []struct {
			ID         string `json:"id"`
			Path       string `json:"path"`
			LibraryID  string `json:"library_id"`
			FileExists bool   `json:"file_exists"`
		} `json:"members"`
	} `json:"groups"`
}

func duplicateFixture(t *testing.T, e *env) (a, b, c *domain.Media) {
	t.Helper()
	lib := e.newLibrary("lib1", e.Root)
	a = e.newMedia(lib.ID, filepath.Join(e.Root, "a.mp4"), bytes.Repeat([]byte("x"), 1000))
	b = e.newMedia(lib.ID, filepath.Join(e.Root, "b.mp4"), bytes.Repeat([]byte("y"), 1000))
	c = e.newMedia(lib.ID, filepath.Join(e.Root, "c.mp4"), bytes.Repeat([]byte("z"), 2000))
	return a, b, c
}

func listDuplicates(t *testing.T, e *env) duplicateGroups {
	t.Helper()
	res, env, raw := e.do(http.MethodGet, "/api/v1/admin/duplicates", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("检测失败 %d: %s", res.StatusCode, raw)
	}
	var out duplicateGroups
	decodeInto(t, env.Data, &out)
	return out
}

// TestDuplicateDetectionGroupsBySizeAndDuration 门禁：只按 (size,duration) 分组，
// 且必须标明是"疑似重复"而不是内容完全相同。
func TestDuplicateDetectionGroupsBySizeAndDuration(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	a, b, c := duplicateFixture(t, e)

	got := listDuplicates(t, e)
	if got.GroupCount != 1 || len(got.Groups) != 1 {
		t.Fatalf("应只有 1 组疑似重复，实际 %d: %+v", got.GroupCount, got.Groups)
	}
	group := got.Groups[0]
	if group.SizeBytes != 1000 || group.DurationMS != 5000 {
		t.Fatalf("分组键应为 (1000,5000)，实际 (%d,%d)", group.SizeBytes, group.DurationMS)
	}
	if len(group.Members) != 2 {
		t.Fatalf("该组应 2 个成员，实际 %d", len(group.Members))
	}
	ids := map[string]bool{}
	for _, m := range group.Members {
		ids[m.ID] = true
		if m.Path == "" || m.LibraryID == "" {
			t.Fatalf("成员缺少路径/库: %+v", m)
		}
	}
	if !ids[a.ID] || !ids[b.ID] {
		t.Fatalf("同 size+duration 的两个必须是成员: %+v", ids)
	}
	if ids[c.ID] {
		t.Fatal("不同尺寸的文件不得进组")
	}
	if !strings.Contains(got.Judgement, "疑似") {
		t.Fatalf("判据文案必须说明疑似: %s", got.Judgement)
	}
	if strings.Contains(got.Judgement, "完全相同") {
		t.Fatalf("不得声称内容完全相同: %s", got.Judgement)
	}
}

// TestDuplicateDeleteRecordsKeepsFiles 门禁：只删记录时磁盘文件仍在。
func TestDuplicateDeleteRecordsKeepsFiles(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	a, _, _ := duplicateFixture(t, e)

	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/duplicates/delete-records",
		map[string]any{"ids": []string{a.ID}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删记录失败 %d: %s", res.StatusCode, raw)
	}
	var data struct {
		Deleted        int  `json:"deleted"`
		FilesUntouched bool `json:"files_untouched"`
	}
	decodeInto(t, env.Data, &data)
	if data.Deleted != 1 || !data.FilesUntouched {
		t.Fatalf("应删 1 条且文件未动: %+v", data)
	}
	if _, err := os.Stat(a.Path); err != nil {
		t.Fatalf("只删记录绝不能动文件: %v", err)
	}
	if _, err := e.DB.GetMediaIn(domain.LibraryScope{All: true}, a.ID); err == nil {
		t.Fatal("面板记录应已被软删")
	}
}

// TestDuplicateDeleteFilesRequiresConfirmation 门禁：没手输确认就拒绝。
func TestDuplicateDeleteFilesRequiresConfirmation(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	a, _, _ := duplicateFixture(t, e)

	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/duplicates/delete-files",
		map[string]any{"ids": []string{a.ID}, "confirm": "delete"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺少确认应 400，实际 %d: %s", res.StatusCode, raw)
	}
	if env.Error == nil || env.Error.Code != "DEDUPE_CONFIRM_REQUIRED" {
		t.Fatalf("错误码应为 DEDUPE_CONFIRM_REQUIRED: %s", raw)
	}
	if _, err := os.Stat(a.Path); err != nil {
		t.Fatalf("被拒的请求不得删文件: %v", err)
	}
	if _, err := e.DB.GetMediaIn(domain.LibraryScope{All: true}, a.ID); err != nil {
		t.Fatal("被拒的请求不得删记录")
	}
}

// TestDuplicateDeleteFilesRemovesAndVerifies 门禁：真删文件要回读核对，文件不在才算成功。
func TestDuplicateDeleteFilesRemovesAndVerifies(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	a, _, _ := duplicateFixture(t, e)

	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/duplicates/delete-files",
		map[string]any{"ids": []string{a.ID}, "confirm": "删除文件"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("应 202 建任务，实际 %d: %s", res.StatusCode, raw)
	}
	var started struct {
		TaskID string `json:"task_id"`
	}
	decodeInto(t, env.Data, &started)
	if started.TaskID == "" {
		t.Fatal("缺少 task_id")
	}
	var task struct {
		Status  string `json:"status"`
		Updated int    `json:"updated"`
		Failed  int    `json:"failed"`
		Error   string `json:"error"`
	}
	decodeInto(t, waitTask(t, e, started.TaskID).Data, &task)
	if task.Status != domain.TaskSuccess || task.Updated != 1 || task.Failed != 0 {
		t.Fatalf("任务应成功删 1: %+v", task)
	}
	if _, err := os.Stat(a.Path); !os.IsNotExist(err) {
		t.Fatalf("文件应已被删除（回读核对）: err=%v", err)
	}
	if _, err := e.DB.GetMediaIn(domain.LibraryScope{All: true}, a.ID); err == nil {
		t.Fatal("删文件后记录也应软删")
	}
}

// TestDuplicateDeleteFilesRefusesOutsideRoots 门禁：越界路径不删文件、如实报失败。
func TestDuplicateDeleteFilesRefusesOutsideRoots(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("lib1", e.Root)
	outside := filepath.Join(e.Base, "outside.mp4")
	if err := os.WriteFile(outside, bytes.Repeat([]byte("q"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &domain.Media{
		ID: domain.NewID("med"), LibraryID: lib.ID, Path: outside, Title: "outside",
		Size: 1000, Container: "mov", DurationMS: 5000, Status: domain.MediaReady,
	}
	if err := e.DB.InsertMedia(m); err != nil {
		t.Fatal(err)
	}

	res, env, raw := e.write(http.MethodPost, "/api/v1/admin/duplicates/delete-files",
		map[string]any{"ids": []string{m.ID}, "confirm": "删除文件"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("应受理并如实报失败，实际 %d: %s", res.StatusCode, raw)
	}
	var started struct {
		TaskID string `json:"task_id"`
	}
	decodeInto(t, env.Data, &started)
	var task struct {
		Status string `json:"status"`
		Failed int    `json:"failed"`
		Error  string `json:"error"`
	}
	decodeInto(t, waitTask(t, e, started.TaskID).Data, &task)
	if task.Status != domain.TaskFailed || task.Failed != 1 {
		t.Fatalf("越界必须失败: %+v", task)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("越界文件绝不能删: %v", err)
	}
	if _, err := e.DB.GetMediaIn(domain.LibraryScope{All: true}, m.ID); err != nil {
		t.Fatal("路径校验失败时记录必须保留")
	}
}
