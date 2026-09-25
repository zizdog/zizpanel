package web

// api_files_dirsize_gate_test.go —— 目录「按需计算大小」门禁。
//
// 为什么现有门禁抓不到：此前**没有任何**"递归大小/有界统计"的断言 ——
// 列表接口只报单个条目的 Lstat 大小（目录永远是 0/—），既没有递归求和，
// 也没有"超预算必须如实标未统计完"这条语义，更没有"不跟随符号链接"的约束。
//
// 断言（全部在 t.TempDir() 里造树，绝不碰真实用户目录）：
//  ① 递归总字节数 = 各文件之和（含嵌套子目录），且不跟随符号链接；
//  ② 注入极小条目上限 ⇒ truncated=true 且只返回已统计的部分（负向对照：
//     把 Truncated 恒置 false 时这条必红）；
//  ③ 白名单外的路径 ⇒ 403；白名单内的**文件**（非目录）⇒ 400；
//  ④ 读不到的目录 ⇒ 计入 skipped（root 下无法生效，显式跳过）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/files"
)

// dirSizeHTTP 走 HTTP handler（而不是直接调 files.Manager），这样"字段有没有
// 真的下发""状态码映射对不对"也在断言范围内。
func dirSizeHTTP(t *testing.T, s *Server, p string) (int, files.DirSizeResult) {
	t.Helper()
	q := url.Values{}
	q.Set("path", p)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/dir-size?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	s.handleFileDirSize(rec, req)
	var body struct {
		OK   bool                `json:"ok"`
		Msg  string              `json:"msg"`
		Data files.DirSizeResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v（原文 %q）", err, rec.Body.String())
	}
	return rec.Code, body.Data
}

func writeSizedFile(t *testing.T, p string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("建目录失败 %s: %v", p, err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写文件失败 %s: %v", p, err)
	}
}

func TestFileDirSizeGate(t *testing.T) {
	s, f := newFakeFileRootsServer(t)

	// 已知大小的目录树（全部在临时目录里）：
	//   tree/a.bin        1500
	//   tree/sub/b.bin    2500
	//   tree/sub/deep/c.bin 3000   ⇒ 共 7000 字节 / 3 文件 / 2 目录
	tree := filepath.Join(f.www, "tree")
	writeSizedFile(t, filepath.Join(tree, "a.bin"), 1500)
	writeSizedFile(t, filepath.Join(tree, "sub", "b.bin"), 2500)
	writeSizedFile(t, filepath.Join(tree, "sub", "deep", "c.bin"), 3000)

	// 白名单**之外**的一个大文件：树里的软链接指向它，跟随就会把 9000 加进来。
	outside := filepath.Join(f.base, "outside")
	writeSizedFile(t, filepath.Join(outside, "big.bin"), 9000)
	if err := os.Symlink(filepath.Join(outside, "big.bin"), filepath.Join(tree, "link-out")); err != nil {
		t.Fatalf("建软链接失败: %v", err)
	}

	t.Run("①递归求和且不跟随符号链接", func(t *testing.T) {
		code, res := dirSizeHTTP(t, s, tree)
		if code != http.StatusOK {
			t.Fatalf("期望 200，得到 %d", code)
		}
		if res.Bytes != 7000 {
			t.Errorf("递归总大小 = %d，期望 7000（1500+2500+3000；跟随了软链接会多 9000）", res.Bytes)
		}
		if res.Files != 3 || res.Dirs != 2 {
			t.Errorf("文件/目录计数 = %d/%d，期望 3/2", res.Files, res.Dirs)
		}
		if res.Symlinks != 1 {
			t.Errorf("符号链接计数 = %d，期望 1（不跟随也要如实计数）", res.Symlinks)
		}
		if res.Truncated {
			t.Errorf("小目录树不该被标未统计完：%s", res.Reason)
		}
		if res.Skipped != 0 {
			t.Errorf("这里没有读不到的子项，skipped 应为 0，得到 %d", res.Skipped)
		}
		if res.Path == "" || res.Ms < 0 {
			t.Errorf("响应缺少 path/耗时：path=%q ms=%d", res.Path, res.Ms)
		}
	})

	t.Run("②超预算必须如实标未统计完", func(t *testing.T) {
		oldLimits := dirSizeLimits
		defer func() { dirSizeLimits = oldLimits }()
		// 注入极小上限：只有前 2 个条目能被统计（排序后是 a.bin、link-out），
		// 剩下的一律不统计 —— 必须报 truncated=true，绝不假装是全部。
		dirSizeLimits = files.DirSizeLimits{Budget: time.Minute, MaxEntries: 2}

		code, res := dirSizeHTTP(t, s, tree)
		if code != http.StatusOK {
			t.Fatalf("期望 200（超预算也是正常返回，只是标未统计完），得到 %d", code)
		}
		if !res.Truncated {
			t.Fatalf("超预算却没有标 truncated=true —— 这就是「假装统计完了」（bytes=%d files=%d）",
				res.Bytes, res.Files)
		}
		if res.Reason == "" {
			t.Errorf("truncated=true 却没有一句人话原因")
		}
		if res.Bytes >= 7000 {
			t.Errorf("标了未统计完，却给出了完整的 7000 字节（bytes=%d）—— 数字在骗人", res.Bytes)
		}
		if res.Bytes != 1500 {
			t.Errorf("只统计了前 2 条（a.bin + 一条软链接）应为 1500 字节，得到 %d", res.Bytes)
		}

		// 时间预算这条闸也要能独立触发。
		dirSizeLimits = files.DirSizeLimits{Budget: time.Nanosecond, MaxEntries: 0}
		code2, res2 := dirSizeHTTP(t, s, tree)
		if code2 != http.StatusOK || !res2.Truncated {
			t.Errorf("预算极小（1ns）时必须标 truncated=true，得到 code=%d truncated=%v", code2, res2.Truncated)
		}
	})

	t.Run("③白名单外 403、非目录 400", func(t *testing.T) {
		if code, _ := dirSizeHTTP(t, s, outside); code != http.StatusForbidden {
			t.Errorf("白名单外的目录：期望 403，得到 %d（越界必须由白名单拦下）", code)
		}
		plain := filepath.Join(f.www, "plain.txt")
		writeSizedFile(t, plain, 10)
		if code, _ := dirSizeHTTP(t, s, plain); code != http.StatusBadRequest {
			t.Errorf("白名单内的普通文件：期望 400（不是目录），得到 %d", code)
		}
	})

	t.Run("④读不到的目录计入 skipped", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 能读任何目录，这个用例在 root 下无法生效")
		}
		locked := filepath.Join(tree, "locked")
		writeSizedFile(t, filepath.Join(locked, "hidden.bin"), 4000)
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatalf("chmod 失败: %v", err)
		}
		defer func() { _ = os.Chmod(locked, 0o755) }()

		code, res := dirSizeHTTP(t, s, tree)
		if code != http.StatusOK {
			t.Fatalf("期望 200，得到 %d", code)
		}
		if res.Skipped != 1 {
			t.Errorf("读不到的目录应计 skipped=1（绝不静默当 0），得到 %d", res.Skipped)
		}
		if res.Bytes != 7000 {
			t.Errorf("读不到的 4000 字节不该被算进来：bytes=%d，期望 7000", res.Bytes)
		}
	})
}
