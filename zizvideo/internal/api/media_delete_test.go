package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// 门禁：前台（播放页）删除入口只给管理员。
// 默认只删面板记录、磁盘文件不动；连文件一起删必须先过口令 + 路径校验；
// 文件删不掉就绝不删记录（"记录没了、文件还在"= 谎报）。
func TestDeleteMediaFromFrontend(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)

	// 非管理员：入口只给管理员（403）
	e.write(http.MethodPatch, "/api/v1/admin/settings", map[string]any{"allow_register": true})
	anon := e.anonClient()
	registerAnon(t, e, anon, "newbie")
	if res, raw := e.loginAs(anon, "newbie", "newbiepass123"); res.StatusCode != http.StatusOK {
		t.Fatalf("普通用户登录失败 %d: %s", res.StatusCode, raw)
	}
	viewerMedia := e.newMedia(lib.ID, filepath.Join(e.Root, "viewer.mp4"), body(8))
	res, _, _ := e.writeAs(anon, http.MethodDelete, "/api/v1/admin/media/"+viewerMedia.ID, map[string]any{})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户删除应 403，实际 %d", res.StatusCode)
	}

	// 只删记录：文件必须留在磁盘上
	recordOnly := filepath.Join(e.Root, "record-only.mp4")
	m1 := e.newMedia(lib.ID, recordOnly, body(64))
	res, env, raw := e.write(http.MethodDelete, "/api/v1/admin/media/"+m1.ID, map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("只删记录应 200，实际 %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Deleted     int  `json:"deleted"`
		FileDeleted bool `json:"file_deleted"`
	}
	decodeInto(t, env.Data, &out)
	if out.Deleted != 1 || out.FileDeleted {
		t.Fatalf("只删记录的响应不符: %+v", out)
	}
	if _, err := os.Stat(recordOnly); err != nil {
		t.Fatalf("只删记录不许动文件: %v", err)
	}
	if res, _, _ := e.do(http.MethodGet, "/api/v1/media/"+m1.ID, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后单条应 404，实际 %d", res.StatusCode)
	}

	// 删文件：缺口令时拒绝，且文件与记录都还在
	withFile := filepath.Join(e.Root, "with-file.mp4")
	m2 := e.newMedia(lib.ID, withFile, body(32))
	res, env, raw = e.write(http.MethodDelete, "/api/v1/admin/media/"+m2.ID,
		map[string]any{"delete_file": true})
	if res.StatusCode != http.StatusBadRequest || env.Error == nil || env.Error.Code != "MEDIA_CONFIRM_REQUIRED" {
		t.Fatalf("缺口令应 400/MEDIA_CONFIRM_REQUIRED，实际 %d: %s", res.StatusCode, raw)
	}
	if _, err := os.Stat(withFile); err != nil {
		t.Fatalf("被拒的删文件请求不许动文件: %v", err)
	}
	if res, _, _ := e.do(http.MethodGet, "/api/v1/media/"+m2.ID, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("被拒后记录必须还在，实际 %d", res.StatusCode)
	}

	// 带口令：文件真没了 + 记录也删掉
	res, env, raw = e.write(http.MethodDelete, "/api/v1/admin/media/"+m2.ID,
		map[string]any{"delete_file": true, "confirm": "删除文件"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("带口令删文件应 200，实际 %d: %s", res.StatusCode, raw)
	}
	decodeInto(t, env.Data, &out)
	if !out.FileDeleted {
		t.Fatalf("响应必须如实说文件已删: %+v", out)
	}
	if _, err := os.Stat(withFile); !os.IsNotExist(err) {
		t.Fatalf("文件应已删除，err=%v", err)
	}

	// 路径不在允许根内：拒绝且不动文件（否则网页端能删掉任意路径）
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	m3 := e.newMedia(lib.ID, outside, body(8))
	res, env, raw = e.write(http.MethodDelete, "/api/v1/admin/media/"+m3.ID,
		map[string]any{"delete_file": true, "confirm": "删除文件"})
	if res.StatusCode != http.StatusBadRequest || env.Error == nil || env.Error.Code != "MEDIA_PATH_REFUSED" {
		t.Fatalf("越界路径应 400/MEDIA_PATH_REFUSED，实际 %d: %s", res.StatusCode, raw)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("越界路径的文件不许被删: %v", err)
	}
	if res, _, _ := e.do(http.MethodGet, "/api/v1/media/"+m3.ID, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("越界被拒后记录必须还在，实际 %d", res.StatusCode)
	}
}
