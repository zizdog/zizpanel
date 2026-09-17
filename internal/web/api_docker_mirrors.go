package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Docker 镜像加速源（换源）
//
//  用户的抱怨很直接："现在的拉取太慢了，没法用"。根因是这台机器直连
//  registry-1.docker.io 会超时（面板自己探测也会把这一条显示出来）。
//
//  四个接口分工：
//    GET  /api/v1/docker/mirrors          现状（运行时、配置文件、已配置、生效中）
//    GET  /api/v1/docker/mirrors/cached   "上次检测"的缓存结果（**只读，绝不探测**）
//    POST /api/v1/docker/mirrors/probe    检测可用性（并发探测，每条约 8 秒超时）
//    POST /api/v1/docker/mirrors           保存并重启运行时（**长任务**）
//
//  为什么要有 cached 这个只读接口（2026-09-17，用户原话："点击后要等很久，
//  应该立即加载，加一个点击检测的按钮，让用户主动选择操作触发源的检测"）：
//  过去打开页面就**自动**探一轮（14 个源 × 8 秒超时），点进去要干等；
//  现在页面先用手上的缓存渲染、没有缓存就如实显示"尚未检测"，
//  探测只由用户点「检测」触发。老接口（probe）行为不变。
//
//  缓存**只存内存**（见 server.go 的 dockerMirrorProbes）：不建表、不落盘，
//  面板重启后回到"尚未检测"，用户再点一次即可 —— 展示数据不值得为它建表。
// ============================================================================

// dockerHubMirrorURL 是 Docker Hub 官方地址：用户抱怨的"慢"就是它，
// 所以每次检测都把它一起探、并摆在界面最前面。
const dockerHubMirrorURL = "https://registry-1.docker.io"

// dockerMirrorProbeFn 是"探测一批地址"的动作（默认就是真实探测）。
//
// 之所以做成包级变量：真实探测要发网络请求，而单测必须能提供假结果，
// 否则"点了检测才写缓存"这条契约只能靠真连外网来验证（违反"单测不许碰
// 真实服务"）。这是本包既有的注入约定，见 server.go 的 proxyLookupHostFn
// 与 api_systemsettings_lan.go 里的同一句说明。
var dockerMirrorProbeFn = func(m *services.Manager, ctx context.Context, urls []string) []services.DockerMirrorProbe {
	return m.ProbeDockerMirrors(ctx, urls)
}

// handleDockerMirrors 返回加速源现状 + 内置候选。
//
// **不做任何网络探测**：官方源的探测结果只从"上次检测"的缓存里取（用户
// 主动点过「检测」之后才有值）。这样点进本页是立刻渲染，而不是等 8 秒。
func (s *Server) handleDockerMirrors(w http.ResponseWriter, r *http.Request) {
	st := s.svcManager().DockerMirrorStatus(r.Context())
	if hub, ok := s.cachedDockerMirrorProbe(dockerHubMirrorURL); ok {
		st.HubDirect = &hub
	}
	ok(w, map[string]any{
		"state":      st,
		"candidates": s.svcManager().DockerMirrorCandidates(),
	})
}

// handleDockerMirrorCached 返回"上次检测"的结果（带检测时间）。
//
// 只读：**不会**触发任何探测。缓存为空时如实返回 cached=false + 空列表，
// 让前端显示"尚未检测"，而不是等一次探测或假装已检测。
func (s *Server) handleDockerMirrorCached(w http.ResponseWriter, r *http.Request) {
	probes, at := s.dockerMirrorProbeCache()
	payload := map[string]any{
		"probes": probes,
		"cached": !at.IsZero(),
	}
	if !at.IsZero() {
		payload["checked_at"] = at.UTC().Format(time.RFC3339)
	}
	ok(w, payload)
}

// handleDockerMirrorProbe 探测一组地址的可用性。
//
// 请求体可选：`{"urls": [...]}`；不传就探内置候选 + 已配置的那些。
// 并发探测，每条约 8 秒超时 —— 整体几秒钟返回，不需要走任务中心。
//
// 每次探测都会把**完整结果**写进内存缓存（覆盖上一批），并回一个 checked_at，
// 前端据此显示"上次检测：HH:MM:SS"。响应里多出的 checked_at 是**新增字段**，
// 老调用方可以忽略，接口语义（probes 的含义）没变。
func (s *Server) handleDockerMirrorProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	urls := req.URLs
	if len(urls) == 0 {
		for _, c := range s.svcManager().DockerMirrorCandidates() {
			urls = append(urls, c.URL)
		}
		st := s.svcManager().DockerMirrorStatus(r.Context())
		urls = append(urls, st.Configured...)
		urls = append(urls, st.Effective...)
	}
	urls = dedupeStrings(urls)
	// 官方源也一起探：用户抱怨的"慢"就是它，界面要把这条摆在最前面
	if !containsString(urls, dockerHubMirrorURL) {
		urls = append([]string{dockerHubMirrorURL}, urls...)
	}
	probes := dockerMirrorProbeFn(s.svcManager(), r.Context(), urls)
	checkedAt := time.Now()
	s.storeDockerMirrorProbes(probes, checkedAt)
	s.audit(r, "docker_mirror_probe", "docker", fmt.Sprintf("检测 %d 个加速源", len(probes)), true, "")
	ok(w, map[string]any{
		"probes":     probes,
		"checked_at": checkedAt.UTC().Format(time.RFC3339),
	})
}

// ---------- 「上次检测」的内存缓存 ----------
//
// 三个方法都拿 s.dockerMirrorMu，因为面板会并发处理请求（用户可能连点两次
// 「检测」）。加锁范围只覆盖切片复制，不做任何 I/O。

// storeDockerMirrorProbes 覆盖式写入一批探测结果（只由 probe 接口调用）。
func (s *Server) storeDockerMirrorProbes(probes []services.DockerMirrorProbe, at time.Time) {
	cp := make([]services.DockerMirrorProbe, len(probes))
	copy(cp, probes)
	s.dockerMirrorMu.Lock()
	defer s.dockerMirrorMu.Unlock()
	s.dockerMirrorProbes = cp
	s.dockerMirrorChecked = at
}

// dockerMirrorProbeCache 返回缓存副本与检测时间（缓存为空时时间零值）。
//
// 返回副本而不是内部切片：调用方（HTTP 响应编码）不持锁，共享底层数组
// 会在下一次写入时产生数据竞争。
func (s *Server) dockerMirrorProbeCache() ([]services.DockerMirrorProbe, time.Time) {
	s.dockerMirrorMu.Lock()
	defer s.dockerMirrorMu.Unlock()
	out := make([]services.DockerMirrorProbe, len(s.dockerMirrorProbes))
	copy(out, s.dockerMirrorProbes)
	return out, s.dockerMirrorChecked
}

// cachedDockerMirrorProbe 从缓存里取某一条地址的探测结果。
//
// 第二个返回值为 false 表示"**没测过**"（而不是"不可用"）—— 调用方必须
// 区分这两者，否则会把"尚未检测"显示成"连不上"。
func (s *Server) cachedDockerMirrorProbe(url string) (services.DockerMirrorProbe, bool) {
	probes, _ := s.dockerMirrorProbeCache()
	for _, p := range probes {
		if p.URL == url {
			return p, true
		}
	}
	return services.DockerMirrorProbe{}, false
}

// handleDockerMirrorSave 保存加速源并重启运行时（长任务）。
func (s *Server) handleDockerMirrorSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mirrors []string `json:"mirrors"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 先做一遍 URL 校验：明显的错（空、不是 http(s)、带查询参数）
	// 在提交任务前就说清楚，别让用户等任务跑完才发现写错了。
	for _, raw := range req.Mirrors {
		if _, err := services.ValidateMirrorURL(raw); err != nil {
			fail(w, http.StatusBadRequest, raw+"："+err.Error())
			return
		}
	}
	s.launchTask(w, r, "docker-mirror", "docker-mirrors", "更换 Docker 镜像加速源",
		"docker_mirror_save", func(ctx context.Context, log tasks.LogFunc) (any, error) {
			if err := s.svcManager().SetDockerMirrors(ctx, req.Mirrors); err != nil {
				return nil, err
			}
			st := s.svcManager().DockerMirrorStatus(ctx)
			return map[string]any{
				"msg":       "加速源已生效",
				"effective": st.Effective,
			}, nil
		})
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
