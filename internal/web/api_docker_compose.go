package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
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

	// 「部署」是唯一的长动作：compose up -d 可能拉几分钟镜像，
	// 同步请求期间用户只能看"正在部署"，而且一刷新就把 docker 拉取打断。
	// 交给任务中心，进度（镜像层下载）走 SSE。
	//
	// stop / restart / status 都是秒级动作，保持同步返回 ——
	// 前端已有的即时反馈（"已停止"）不需要改成任务。
	if action == "up" {
		s.launchTask(w, r, "deploy", "compose:"+name, "部署 Docker Compose 项目 "+name,
			"docker_compose_up", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
				mgr := s.svcManager()
				out, err := mgr.DockerComposeAction(ctx, name, "up", removeVolumes)
				if err != nil {
					return nil, err
				}
				// 部署成功后顺手把项目登记成服务 —— 用户不必再去「可纳管」里加一遍。
				registered := false
				if file, ferr := mgr.ComposeProjectFile(name); ferr == nil {
					if rerr := mgr.RegisterComposeProject(ctx, name, file); rerr == nil {
						registered = true
					}
				}

				// 把项目下的容器列出来（前端据此给每个容器一个「日志」入口），
				// 并从刚启动的容器日志里捞"首次启动随机生成的账号口令"——
				// File Browser 这类镜像的初始口令只在日志里，用户点完部署不会
				// 想到去翻日志，于是永远登不进去（用户原话见任务说明）。
				// 口令只进结果的 credentials（唯一允许出现明文的地方），日志里不写。
				containers := mgr.DockerComposeProjectContainers(ctx, name)
				if len(containers) > 0 {
					services.EmitProgress(ctx, tasks.LevelStep,
						fmt.Sprintf("项目 %s 下共 %d 个容器：%s", name, len(containers), containerNames(containers)))
				}
				creds := scrapeStartupCredentials(ctx, mgr, containers)

				return map[string]any{
					"name": name, "action": "up", "output": out, "registered": registered,
					"containers": containers, "credentials": creds,
				}, nil
			})
		return
	}

	mgr := s.svcManager()
	out, err := mgr.DockerComposeAction(r.Context(), name, action, removeVolumes)
	if err != nil {
		s.audit(r, "docker_compose_"+action, name, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_compose_"+action, name, "成功", true, "")

	ok(w, map[string]any{"name": name, "action": action, "output": out})
}

// startupCredWait 是"等容器把首启日志打出来"的时长。
//
// 与 install.go 的 scrapeGeneratedCredentials 同因：compose up -d 返回时容器才刚起来，
// 初始口令那一行可能还没落盘。1.5 秒足够，对一次动辄几分钟的部署可以忽略。
const startupCredWait = 1500 * time.Millisecond

// scrapeStartupCredentials 在部署成功后等一小会儿，再扫容器日志找初始口令。
//
// 没有运行中的容器就**不等**（没有日志可扫，白等 1.5 秒）。
// 扫描失败不影响部署结论：这是附加信息，不是部署成功与否的一部分。
func scrapeStartupCredentials(ctx context.Context, mgr *services.Manager, containers []services.DockerProjectContainer) []services.Credential {
	running := false
	for _, c := range containers {
		if c.State == "running" {
			running = true
			break
		}
	}
	if !running {
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(startupCredWait):
	}
	return mgr.DockerScrapeStartupCredentials(ctx, containers)
}

// containerNames 把容器列表拼成一行（日志与提示里用）。
func containerNames(list []services.DockerProjectContainer) string {
	names := make([]string, 0, len(list))
	for _, c := range list {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
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
