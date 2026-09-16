package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Docker 镜像加速源（换源）
//
//  用户的抱怨很直接："现在的拉取太慢了，没法用"。根因是这台机器直连
//  registry-1.docker.io 会超时（面板自己探测也会把这一条显示出来）。
//
//  三个接口分工：
//    GET  /api/v1/docker/mirrors         现状（运行时、配置文件、已配置、生效中）
//    POST /api/v1/docker/mirrors/probe   检测可用性（并发探测，几秒钟）
//    POST /api/v1/docker/mirrors         保存并重启运行时（**长任务**）
// ============================================================================

// handleDockerMirrors 返回加速源现状 + 内置候选。
func (s *Server) handleDockerMirrors(w http.ResponseWriter, r *http.Request) {
	st := s.svcManager().DockerMirrorStatus(r.Context())
	ok(w, map[string]any{
		"state":      st,
		"candidates": s.svcManager().DockerMirrorCandidates(),
	})
}

// handleDockerMirrorProbe 探测一组地址的可用性。
//
// 请求体可选：`{"urls": [...]}`；不传就探内置候选 + 已配置的那些。
// 并发探测，每条约 8 秒超时 —— 整体几秒钟返回，不需要走任务中心。
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
	if !containsString(urls, "https://registry-1.docker.io") {
		urls = append([]string{"https://registry-1.docker.io"}, urls...)
	}
	probes := s.svcManager().ProbeDockerMirrors(r.Context(), urls)
	s.audit(r, "docker_mirror_probe", "docker", fmt.Sprintf("检测 %d 个加速源", len(probes)), true, "")
	ok(w, map[string]any{"probes": probes})
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
