package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  Compose 项目接口
//
//  与「服务管理」的分工：
//    · 这里管**项目本身**：yml 的编辑、部署（up -d）、停止（down）、删除目录。
//    · 服务管理管**生命周期**：启停、日志、健康检查。
//  保存时可以顺带登记成一条 compose 服务，这样"部署完就能在服务管理里看到"，
//  不需要用户再去「可纳管」里手工加一遍。
//
//  路径安全：项目名不允许 `/`、`..`、不可见字符，且拼出的路径必须仍在
//  <work_dir>/compose 下（两道校验都在 services 层，见 composeProjectFile）。
// ============================================================================

// handleDockerComposeProjects 列出 compose 项目（并标出哪些已登记为服务）。
func (s *Server) handleDockerComposeProjects(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	list, err := mgr.DockerComposeProjects(r.Context())
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	if list == nil {
		list = []services.DockerComposeProject{}
	}
	ok(w, map[string]any{"list": list, "root": mgr.ComposeRoot()})
}

// handleDockerComposeRead 读取某个项目的 yml。
func (s *Server) handleDockerComposeRead(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	mgr := s.svcManager()
	content, err := mgr.DockerComposeRead(name)
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	ok(w, map[string]any{"name": name, "content": content})
}

// handleDockerComposeSave 保存 yml（可顺带登记为受管服务）。
func (s *Server) handleDockerComposeSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Content  string `json:"content"`
		Register bool   `json:"register"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	mgr := s.svcManager()
	msg, err := mgr.DockerComposeSave(r.Context(), strings.TrimSpace(req.Name), req.Content, req.Register)
	if err != nil {
		s.audit(r, "docker_compose_save", req.Name, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_compose_save", req.Name, msg, true, "")
	ok(w, map[string]any{"name": req.Name, "message": msg})
}

// handleDockerComposeAction 部署 / 停止 / 重启 / 查看状态。
//
// 用 URL 查询参数区分动作，而不是给每个动作开一条路由：
// 动作用的是同一份参数（项目名 + 可选 remove_volumes），
// 开四条路由只会让 server.go 变长。
func (s *Server) handleDockerComposeAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action := r.URL.Query().Get("action")
	removeVolumes := r.URL.Query().Get("remove_volumes") == "1"

	if action == "" {
		action = "up"
	}

	mgr := s.svcManager()
	out, err := mgr.DockerComposeAction(r.Context(), name, action, removeVolumes)
	if err != nil {
		s.audit(r, "docker_compose_"+action, name, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_compose_"+action, name, "成功", true, "")

	// 部署成功后顺手把项目登记成服务 —— 用户不必再去「可纳管」里加一遍。
	registered := false
	if action == "up" {
		if file, ferr := mgr.ComposeProjectFile(name); ferr == nil {
			if rerr := mgr.RegisterComposeProject(r.Context(), name, file); rerr == nil {
				registered = true
			}
		}
	}

	ok(w, map[string]any{"name": name, "action": action, "output": out, "registered": registered})
}

// handleDockerComposeDelete 删除项目目录（会先 down，避免留下孤儿容器）。
func (s *Server) handleDockerComposeDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	removeVolumes := r.URL.Query().Get("remove_volumes") == "1"

	mgr := s.svcManager()
	if err := mgr.DockerComposeDeleteProject(r.Context(), name, removeVolumes); err != nil {
		s.audit(r, "docker_compose_delete", name, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_compose_delete", name, "已删除项目目录", true, "")
	ok(w, map[string]any{"deleted": name})
}
