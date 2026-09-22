package services

// zizvideo_update_test.go —— 「市场卡片上的更新检查」的沙箱端到端门禁。
//
// 假镜像用 httptest 起在 127.0.0.1，版本命令用注入的假实现：**绝不碰真实镜像站、
// 也绝不碰真实模块**。三种结论都有负向对照：有更新 / 无更新 / unknown（索引不可达）。

import (
	"context"
	"encoding/json"
	"testing"
)

// logUpdateCheck 把结论按接口原样打成 JSON（报告里贴原文用）。
func logUpdateCheck(t *testing.T, label string, c ZizvideoUpdateCheck) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", label, b)
}

// ① 镜像索引说 9.9.9、已装模块报 0.1.1-mvp ⇒ update_available=true。
func TestZizvideoUpdateCheckAvailable(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.writeInstalledBin(t)
	f := newFakeZizvideoMirror(t, "9.9.9", []byte("#!/bin/sh\necho 'zizvideo 9.9.9'\n"))
	env.m.opt.MirrorBase = f.srv.URL
	zizvideoVersionFn = func(*Manager, context.Context, string) (string, error) {
		return "zizvideo 0.1.1-mvp", nil
	}

	chk := env.m.CheckZizvideoUpdate(context.Background())
	logUpdateCheck(t, "① 索引 9.9.9 vs 已装 0.1.1-mvp", chk)
	if chk.Unknown {
		t.Fatalf("索引与版本都拿得到时不该 unknown：%+v", chk)
	}
	if chk.Installed != "0.1.1-mvp" || chk.Latest != "9.9.9" {
		t.Errorf("已装/最新版本解析错：installed=%q latest=%q", chk.Installed, chk.Latest)
	}
	if !chk.UpdateAvailable {
		t.Error("9.9.9 > 0.1.1-mvp 必须判有更新（否则市场永远等不到更新按钮）")
	}
	if chk.Error != "" {
		t.Errorf("拿得到值时不该有 error：%q", chk.Error)
	}
}

// ② 已装就是索引里的最新版 ⇒ update_available=false，且不是 unknown。
func TestZizvideoUpdateCheckUpToDate(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.writeInstalledBin(t)
	f := newFakeZizvideoMirror(t, "9.9.9", []byte("#!/bin/sh\necho 'zizvideo 9.9.9'\n"))
	env.m.opt.MirrorBase = f.srv.URL
	zizvideoVersionFn = func(*Manager, context.Context, string) (string, error) {
		return "zizvideo 9.9.9", nil
	}

	chk := env.m.CheckZizvideoUpdate(context.Background())
	logUpdateCheck(t, "② 索引 9.9.9 vs 已装 9.9.9", chk)
	if chk.Unknown {
		t.Fatalf("两边版本都拿得到时不该 unknown：%+v", chk)
	}
	if chk.UpdateAvailable {
		t.Error("已装等于索引最新版时必须判无更新")
	}
	if chk.Installed != "9.9.9" || chk.Latest != "9.9.9" {
		t.Errorf("版本解析错：installed=%q latest=%q", chk.Installed, chk.Latest)
	}
}

// ③ 索引不可达 ⇒ unknown=true + error 写人话，且绝不假装"有更新"或"已是最新"。
func TestZizvideoUpdateCheckUnknownWhenIndexUnreachable(t *testing.T) {
	env := newZizvideoTestEnv(t)
	env.writeInstalledBin(t)
	zizvideoVersionFn = func(*Manager, context.Context, string) (string, error) {
		return "zizvideo 0.1.1-mvp", nil
	}
	// 127.0.0.1:1 必然连不上（与 zizvideo_index_e2e_test.go 同一种沙箱手法）。
	env.m.opt.MirrorBase = "http://127.0.0.1:1"

	chk := env.m.CheckZizvideoUpdate(context.Background())
	logUpdateCheck(t, "③ 索引不可达", chk)
	if !chk.Unknown {
		t.Fatalf("索引不可达必须 unknown=true（不许猜结论）：%+v", chk)
	}
	if chk.UpdateAvailable {
		t.Error("索引不可达时绝不能说有更新")
	}
	if chk.Error == "" {
		t.Error("unknown 必须带人话 error，否则界面只能显示一个没有原因的徽标状态")
	}
	if chk.Installed != "0.1.1-mvp" {
		t.Errorf("索引读不到不影响已装版本的读取，实际 %q", chk.Installed)
	}
}

// ④ 未安装（二进制不在）⇒ installed 为空、不给徽标；且**不去跑**版本命令。
func TestZizvideoUpdateCheckNotInstalledSkipsProbe(t *testing.T) {
	env := newZizvideoTestEnv(t)
	called := false
	zizvideoVersionFn = func(*Manager, context.Context, string) (string, error) {
		called = true
		return "zizvideo 0.1.1-mvp", nil
	}

	chk := env.m.CheckZizvideoUpdate(context.Background())
	logUpdateCheck(t, "④ 未安装", chk)
	if called {
		t.Error("没装二进制时不该起 --version 子进程")
	}
	if chk.Installed != "" || chk.UpdateAvailable || chk.Unknown {
		t.Errorf("未安装应给空 installed 且无徽标语义，实际 %+v", chk)
	}
}

// ⑤ supports_update_check 只能由静态声明派生；当前动态版本条目**只有 zizvideo**。
//
// 后端接口对非动态条目直接 400（api_market_update.go），所以"只有它"是那个
// 硬编码分支的护栏：将来新增动态条目时这里会先红。
func TestSupportsUpdateCheckIsStaticAndOnlyZizvideo(t *testing.T) {
	if !SupportsUpdateCheck(ZizvideoAppID) {
		t.Error("zizvideo 是动态版本条目，supports_update_check 必须为 true")
	}
	for _, id := range []string{"frpc", "nginx", "php82", "iopaint", "typecho"} {
		if SupportsUpdateCheck(id) {
			t.Errorf("%s 的版本不由镜像索引决定，不该支持更新检查", id)
		}
	}
	var dynamic []string
	for _, a := range MarketApps() {
		for _, d := range a.Downloads {
			if d.Purpose == MarketFetchReleaseBinary && d.Upstream.Dynamic {
				dynamic = append(dynamic, a.ID)
			}
		}
	}
	if len(dynamic) != 1 || dynamic[0] != ZizvideoAppID {
		t.Errorf("动态版本条目的硬编码分支只服务 zizvideo，实际声明里是 %v", dynamic)
	}
}
