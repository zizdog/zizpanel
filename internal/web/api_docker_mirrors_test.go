package web

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  Docker 加速源：**只读缓存**接口的契约（2026-09-17 用户反馈驱动）
//
//  事故背景（用户原话）："点击后要等很久，应该立即加载，加一个点击检测的
//  按钮，让用户主动选择操作触发源的检测"。
//  根因有两个，都在"进页面就打网络"上：
//    1. GET /api/v1/docker/mirrors 里同步探 registry-1.docker.io（超时 8 秒）；
//    2. 前端进页面自动探一轮全部候选（14 个源）。
//
//  所以这里锁住两条**不能退化**的契约：
//    · 读接口（现状 / 缓存）**永远不触发探测** —— 页面因此能立即渲染；
//    · 只有用户主动调检测接口，才把结果写进缓存；缓存为空时如实返回
//      "尚未检测"（cached=false），而不是假装已检测。
// ============================================================================

// TestDockerMirrorCachedIsReadOnlyUntilUserProbes 断言：
//  1. 空缓存时 GET /mirrors 与 GET /mirrors/cached 都**不触发探测**；
//  2. 缓存接口如实报告"尚未检测"；
//  3. 用户点了检测（POST /probe）之后，缓存才被写入，且只读接口能读回结果
//     与检测时间。
//
// 探测动作被换成假实现（dockerMirrorProbeFn，本包既有的注入约定）：
// 真实探测要连外网，单测不许碰。
func TestDockerMirrorCachedIsReadOnlyUntilUserProbes(t *testing.T) {
	sock, _ := startFakeDocker(t)
	srv, ts, cookies := newDockerTestServerWithSrv(t, sock)

	var probeCalls atomic.Int32
	prevProbe := dockerMirrorProbeFn
	dockerMirrorProbeFn = func(_ *services.Manager, _ context.Context, urls []string) []services.DockerMirrorProbe {
		probeCalls.Add(1)
		out := make([]services.DockerMirrorProbe, 0, len(urls))
		for _, u := range urls {
			out = append(out, services.DockerMirrorProbe{
				DockerMirror: services.DockerMirror{URL: u},
				OK:           true, Status: 401, LatencyMs: 7, Detail: "可用（需要鉴权，属正常）",
			})
		}
		return out
	}
	t.Cleanup(func() { dockerMirrorProbeFn = prevProbe })

	readCached := func(t *testing.T) map[string]any {
		t.Helper()
		res, out, _ := doJSON(t, ts, "GET", "/api/v1/docker/mirrors/cached", nil, cookies)
		if res.StatusCode != 200 {
			t.Fatalf("缓存接口应 200，实际 %d: %v", res.StatusCode, out)
		}
		data, _ := out["data"].(map[string]any)
		if data == nil {
			t.Fatalf("缓存接口响应缺少 data：%v", out)
		}
		return data
	}

	// ---- 1) 缓存为空：如实说"尚未检测"，且**一次探测都没发生** ----
	d := readCached(t)
	if d["cached"] != false {
		t.Errorf("缓存为空时 cached 应为 false，实际 %v", d["cached"])
	}
	if probes, _ := d["probes"].([]any); len(probes) != 0 {
		t.Errorf("缓存为空时不该有探测结果，实际 %v", probes)
	}
	if _, has := d["checked_at"]; has {
		t.Errorf("从未检测过就不该有 checked_at（会谎报检测时间）：%v", d)
	}
	if n := probeCalls.Load(); n != 0 {
		t.Fatalf("读缓存接口触发了 %d 次探测 —— 页面又会卡住", n)
	}

	// 再读一次：读取必须**无副作用**（不能"顺手探一轮再写缓存"）
	d = readCached(t)
	if d["cached"] != false {
		t.Errorf("第二次读缓存也不该凭空出现结果：%v", d)
	}
	if n := probeCalls.Load(); n != 0 {
		t.Fatalf("重复读缓存触发了 %d 次探测", n)
	}

	// ---- 2) 进页面的现状接口同样不探测（这是"立即加载"的关键） ----
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/docker/mirrors", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("现状接口应 200，实际 %d: %v", res.StatusCode, out)
	}
	if n := probeCalls.Load(); n != 0 {
		t.Fatalf("打开页面（GET /mirrors）触发了 %d 次探测 —— 用户又要干等", n)
	}
	// 没检测过时，官方源结论**不能**出现在响应里（空 = 没测过，不是"不通"）
	if st, _ := out["data"].(map[string]any)["state"].(map[string]any); st != nil {
		if _, has := st["hub_direct"]; has {
			t.Errorf("没检测过就不该有 hub_direct：%v", st["hub_direct"])
		}
	}

	// ---- 3) 用户点「检测」：缓存才被写入 ----
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/docker/mirrors/probe",
		map[string][]string{"urls": {"https://mirror.example.invalid"}}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("检测接口应 200，实际 %d: %v", res.StatusCode, out)
	}
	if n := probeCalls.Load(); n != 1 {
		t.Fatalf("检测接口应恰好触发 1 次探测，实际 %d", n)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("检测接口响应缺少 data：%v", out)
	}
	gotProbes, _ := data["probes"].([]any)
	// 检测必须把官方源也算上（用户抱怨的"慢"就是它）
	if len(gotProbes) != 2 {
		t.Fatalf("应探 2 条（官方源 + 传进来的 1 条），实际 %d: %v", len(gotProbes), gotProbes)
	}
	first, _ := gotProbes[0].(map[string]any)
	if first["url"] != dockerHubMirrorURL {
		t.Errorf("官方源应排在第一条，实际 %v", first["url"])
	}
	at, _ := data["checked_at"].(string)
	if at == "" {
		t.Fatalf("检测响应应带 checked_at（前端靠它显示检测时间），实际 %v", data)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("checked_at 应是 RFC3339，实际 %q err=%v", at, err)
	}

	// ---- 4) 检测之后，只读缓存接口能读回同一批结果（页面再次打开时用） ----
	d = readCached(t)
	if d["cached"] != true {
		t.Fatalf("检测后 cached 应为 true，实际 %v", d["cached"])
	}
	if got, _ := d["checked_at"].(string); got != at {
		t.Errorf("缓存里的检测时间应与检测响应一致：%q != %q", got, at)
	}
	probes, _ := d["probes"].([]any)
	if len(probes) != 2 {
		t.Fatalf("检测后缓存应有 2 条结果，实际 %d: %v", len(probes), probes)
	}
	if n := probeCalls.Load(); n != 1 {
		t.Errorf("读缓存又触发了探测（总次数 %d，应为 1）", n)
	}

	// 最后确认接口背后确实是同一个 Server 上的缓存（不是靠响应拼出来的假象）
	if cached, ok := srv.cachedDockerMirrorProbe(dockerHubMirrorURL); !ok || !cached.OK {
		t.Errorf("官方源结果应写进 Server 的内存缓存，实际 ok=%v probe=%+v", ok, cached)
	}
}
