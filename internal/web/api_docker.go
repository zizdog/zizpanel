package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Docker 管理接口
//
//  设计要点：
//
//  1. **Docker 不可用不是 500。** macOS 上 Docker 不是系统自带能力，没装是常态。
//     这里统一返回 409 + 一句人能看懂的话（含"去哪里装/怎么启动"），
//     前端据此显示引导卡片，而不是一个红色报错。
//
//  2. **容器名/镜像名/卷名可能含 `/`**（例如 nginx:1.27、library/redis、my_proj_db）。
//     Go 1.22 的 `{id}` 路径参数不跨 `/`，所以这些接口一律用尾部通配 `{path...}`，
//     把剩下的整段当标识符 —— 否则 `nginx:1.27` 会 404。
//
//  3. **破坏性操作必须能区分"删了什么"**。prune 只清"悬空/未使用"，
//     带标签的镜像、被容器占用的卷一律不动；要删它们得显式指定。
// ============================================================================

// dockerUnavailable 判断错误是不是"Docker 环境不可用"，并给出 HTTP 状态。
func dockerErrorStatus(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "没有可用的 Docker socket"),
		strings.Contains(msg, "Docker socket 不存在"):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) dockerFail(w http.ResponseWriter, err error) {
	fail(w, dockerErrorStatus(err), err.Error())
}

// ---------- 概览 ----------

// handleDockerInfo 返回 Docker 环境信息 + 容器/镜像/卷/网络的数量概览。
//
// 为什么把数量也一起给：Docker 页首屏需要决定"显示引导还是显示内容"，
// 多打几个 API 会让首屏闪烁。这里一次给全。
func (s *Server) handleDockerInfo(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	info := mgr.DockerInfo(r.Context())
	// 运行时（Colima）的**真实**状态：环境不可用时前端靠它区分
	// "没装 → 一键安装" 与 "装了但引擎没跑 → 启动"（见 assets/js/docker.js）。
	// 探测只读（stat + 一次本机 socket 连接）；socket 文件在而连不上时，
	// info.Error 已经如实说明了，这里只补"运行时装没装"这一层。
	runtime := s.dockerRuntimeStatus(r.Context())

	out := map[string]any{
		"available":      info.Available,
		"socket":         info.Socket,
		"version":        info.Version,
		"error":          info.Error,
		"runtime":        runtime,
		"runtime_state":  runtime.State,
		"runtime_app_id": "docker-runtime",
	}
	if info.Available {
		counts := map[string]int{}
		if list, err := mgr.DockerContainers(r.Context(), true); err == nil {
			running := 0
			for _, c := range list {
				if c.State == "running" {
					running++
				}
			}
			counts["containers"] = len(list)
			counts["containers_running"] = running
		}
		if list, err := mgr.DockerImages(r.Context(), false); err == nil {
			counts["images"] = len(list)
		}
		if list, err := mgr.DockerVolumes(r.Context()); err == nil {
			counts["volumes"] = len(list)
		}
		if list, err := mgr.DockerNetworks(r.Context()); err == nil {
			counts["networks"] = len(list)
		}
		if list, err := mgr.DockerComposeProjects(r.Context()); err == nil {
			counts["compose_projects"] = len(list)
		}
		out["counts"] = counts
	}
	ok(w, out)
}

// ---------- 容器 ----------

func (s *Server) handleDockerContainers(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") != "0"
	mgr := s.svcManager()
	list, err := mgr.DockerContainers(r.Context(), all)
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	if list == nil {
		list = []services.DockerContainerView{}
	}
	ok(w, map[string]any{"list": list})
}

// handleDockerContainerAction 启停/重启/暂停/恢复/强杀。
func (s *Server) handleDockerContainerAction(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	if name == "" || action == "" {
		fail(w, http.StatusBadRequest, "缺少 name 或 action 参数")
		return
	}
	mgr := s.svcManager()
	if err := mgr.DockerContainerAction(r.Context(), name, action, 10); err != nil {
		s.audit(r, "docker_container_"+action, name, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_container_"+action, name, "完成", true, "")
	ok(w, map[string]any{"action": action, "name": name})
}

// handleDockerContainerLogs 读取容器日志。
//
// lines=0 是**显式的"要完整日志"**（Docker 的 tail=all）：容器首次启动时打出的
// 初始口令（File Browser 的 admin 密码就是这一类）往往在很久以前，只取尾部会把它
// 翻没。参数缺省时仍只取 300 行，保证"点一下日志"不会把 8MB 文本灌进浏览器。
func (s *Server) handleDockerContainerLogs(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		fail(w, http.StatusBadRequest, "缺少 name 参数")
		return
	}
	tail := 300
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			tail = n
		}
	}
	mgr := s.svcManager()
	text, err := mgr.DockerContainerLogs(r.Context(), name, tail)
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	ok(w, map[string]any{"name": name, "lines": tail, "logs": text})
}

// handleDockerContainerRemove 删除容器。默认不强杀（安全默认），要强杀得显式传 force。
func (s *Server) handleDockerContainerRemove(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.PathValue("path"), "/")
	force := r.URL.Query().Get("force") == "1"
	mgr := s.svcManager()
	if err := mgr.DockerRemoveContainer(r.Context(), name, force); err != nil {
		s.audit(r, "docker_container_remove", name, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_container_remove", name, fmt.Sprintf("force=%v", force), true, "")
	ok(w, map[string]any{"removed": name})
}

func (s *Server) handleDockerContainerInspect(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		fail(w, http.StatusBadRequest, "缺少 name 参数")
		return
	}
	mgr := s.svcManager()
	detail, err := mgr.DockerInspectContainer(r.Context(), name)
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	ok(w, detail)
}

// dockerCreateRequest 是创建容器的请求体。
//
// 前端传"人类友好"的形式（字符串数组），服务端负责解析与校验；
// 不接受自由格式的 HostConfig（那是完整的 root 等价面）。
type dockerCreateRequest struct {
	Name          string   `json:"name"`
	Image         string   `json:"image"`
	Command       string   `json:"command"` // shell 形式的命令，按空白切分
	Env           []string `json:"env"`     // KEY=VALUE
	Ports         []string `json:"ports"`   // "8080:80" / "8080:80/udp" / "80"
	Binds         []string `json:"binds"`   // "/host:/container[:ro]"
	Volumes       []string `json:"volumes"` // 匿名卷路径
	RestartPolicy string   `json:"restart_policy"`
	Network       string   `json:"network"`
	AutoStart     bool     `json:"auto_start"`
	Privileged    bool     `json:"privileged"`
}

// parsePortSpec 解析 "8080:80"、"8080:80/udp"、"127.0.0.1:8080:80"、"80"。
func parsePortSpec(spec string) (services.DockerPortBind, error) {
	var out services.DockerPortBind
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return out, fmt.Errorf("端口映射为空")
	}

	proto := "tcp"
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		proto = strings.ToLower(spec[i+1:])
		spec = spec[:i]
		if proto != "tcp" && proto != "udp" && proto != "sctp" {
			return out, fmt.Errorf("不支持的协议: %s", proto)
		}
	}

	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return out, fmt.Errorf("端口映射格式不对（最多 host_ip:host_port:container_port）: %s", spec)
	}

	// 最后一段永远是容器端口
	cont, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil || cont <= 0 || cont > 65535 {
		return out, fmt.Errorf("容器端口不合法: %s", spec)
	}
	out.ContPort = cont
	out.Proto = proto

	switch len(parts) {
	case 1:
		// 只给容器端口：由 Docker 随机分配宿主端口
	case 2:
		host, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || host < 0 || host > 65535 {
			return out, fmt.Errorf("宿主端口不合法: %s", spec)
		}
		out.HostPort = host
	case 3:
		out.HostIP = strings.TrimSpace(parts[0])
		host, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || host < 0 || host > 65535 {
			return out, fmt.Errorf("宿主端口不合法: %s", spec)
		}
		out.HostPort = host
	}
	return out, nil
}

// splitCommand 把 shell 形式的命令切成参数。
//
// 刻意**不做完整的 shell 解析**（不处理引号转义、不做变量展开）：
// 这里的结果会作为 exec 的参数数组传给 Docker，不经过 shell，
// 所以多余的花样既没用处、还会让人误以为支持 shell 语法。
// 需要复杂命令时用 "sh -c '...'" 形式显式写出来。
func splitCommand(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (s *Server) handleDockerContainerCreate(w http.ResponseWriter, r *http.Request) {
	var req dockerCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	spec := services.DockerCreateSpec{
		Name:          strings.TrimSpace(req.Name),
		Image:         strings.TrimSpace(req.Image),
		Cmd:           splitCommand(req.Command),
		Env:           req.Env,
		Binds:         req.Binds,
		Volumes:       req.Volumes,
		RestartPolicy: req.RestartPolicy,
		Network:       strings.TrimSpace(req.Network),
		AutoStart:     req.AutoStart,
		Privileged:    req.Privileged,
	}

	// 镜像名是必填项，且不能含空白 —— 这属于**参数错误（400）**。
	// 放到服务层再报会变成 500，前端只能显示"服务器错误"，用户不知道要改什么。
	if spec.Image == "" {
		fail(w, http.StatusBadRequest, "必须指定镜像名")
		return
	}
	if strings.ContainsAny(spec.Image, " \t\n") {
		fail(w, http.StatusBadRequest, "镜像名不能包含空白字符")
		return
	}

	// 容器名有限制（Docker 自己也会校验），提前拦住能给出更好的提示
	if spec.Name != "" {
		for _, r := range spec.Name {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '_', r == '.', r == '-':
			default:
				fail(w, http.StatusBadRequest, "容器名只能用字母、数字与 . _ -")
				return
			}
		}
	}

	for _, p := range req.Ports {
		bind, err := parsePortSpec(p)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		spec.Ports = append(spec.Ports, bind)
	}

	// 环境变量必须是 KEY=VALUE，避免把畸形内容塞进容器
	for _, e := range spec.Env {
		if !strings.Contains(e, "=") {
			fail(w, http.StatusBadRequest, "环境变量必须是 KEY=VALUE 形式: "+e)
			return
		}
	}
	// 挂载必须是 host:container（相对路径没有意义，Docker 也会拒绝）
	for _, b := range spec.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "/") {
			fail(w, http.StatusBadRequest, "挂载必须是 /宿主绝对路径:/容器路径[:ro] 形式: "+b)
			return
		}
	}

	// 参数校验全部做完之后再提交任务：参数错误要当场 400 说清楚，
	// 而不是先给一个 202、再让用户在任务日志里等一条"必须指定镜像名"。
	mgr := s.svcManager()
	target := spec.Name
	if target == "" {
		target = spec.Image
	}
	// 创建容器可能先拉镜像（本地没有时），镜像层进度必须逐行可见；
	// 而且它挂在任务中心才不会被"用户刷新页面"杀掉。
	s.launchTask(w, r, "docker-container-create", "docker:container:"+target,
		"创建容器 "+target, "docker_container_create",
		func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			id, err := mgr.DockerCreateContainer(ctx, spec)
			if err != nil {
				// 创建成功但启动失败时 id 非空，提示要区分开
				if id != "" {
					return nil, fmt.Errorf("%s（容器已创建，可在列表里手动启动）", err.Error())
				}
				return nil, err
			}
			return map[string]any{"id": id, "name": spec.Name, "started": spec.AutoStart}, nil
		})
}

// ---------- 镜像 ----------

func (s *Server) handleDockerImages(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	mgr := s.svcManager()
	list, err := mgr.DockerImages(r.Context(), all)
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	if list == nil {
		list = []services.DockerImageView{}
	}
	ok(w, map[string]any{"list": list})
}

// handleDockerImagePull 拉取镜像。
//
// 走任务中心（202 + task_id）：拉一个几百 MB 的镜像要几分钟，Docker 的层进度
// （Downloading/Extracting 的字节数）现在**逐行**进任务日志，关掉窗口也不中断。
// 拉取本身在 services 层流式读 /images/create 的进度流，这里只负责提交任务。
func (s *Server) handleDockerImagePull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	image := strings.TrimSpace(req.Image)
	// 参数错误在提交任务之前拦住：否则用户先拿到一个 202，
	// 再在任务日志里等一句"必须指定镜像名"。
	if image == "" {
		fail(w, http.StatusBadRequest, "必须指定镜像名")
		return
	}
	if strings.ContainsAny(image, " \t\n") {
		fail(w, http.StatusBadRequest, "镜像名不能包含空白字符")
		return
	}
	s.launchTask(w, r, "docker-image-pull", "docker:image:"+image, "拉取镜像 "+image,
		"docker_image_pull", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			mgr := s.svcManager()
			msg, err := mgr.DockerPullImage(ctx, image)
			if err != nil {
				return nil, err
			}
			return map[string]any{"image": image, "message": msg}, nil
		})
}

func (s *Server) handleDockerImageRemove(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimPrefix(r.PathValue("path"), "/")
	force := r.URL.Query().Get("force") == "1"
	mgr := s.svcManager()
	msg, err := mgr.DockerRemoveImage(r.Context(), ref, force)
	if err != nil {
		s.audit(r, "docker_image_remove", ref, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_image_remove", ref, "force="+strconv.FormatBool(force), true, "")
	ok(w, map[string]any{"removed": ref, "message": strings.TrimSpace(msg)})
}

func (s *Server) handleDockerImagePrune(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	reclaimed, err := mgr.DockerPruneImages(r.Context())
	if err != nil {
		s.audit(r, "docker_image_prune", "", err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_image_prune", "", fmt.Sprintf("释放 %d 字节", reclaimed), true, "")
	ok(w, map[string]any{"reclaimed": reclaimed})
}

// ---------- 卷 ----------

func (s *Server) handleDockerVolumes(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	list, err := mgr.DockerVolumes(r.Context())
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	if list == nil {
		list = []services.DockerVolumeView{}
	}
	ok(w, map[string]any{"list": list})
}

func (s *Server) handleDockerVolumeRemove(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.PathValue("path"), "/")
	force := r.URL.Query().Get("force") == "1"
	mgr := s.svcManager()
	if err := mgr.DockerRemoveVolume(r.Context(), name, force); err != nil {
		s.audit(r, "docker_volume_remove", name, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_volume_remove", name, "force="+strconv.FormatBool(force), true, "")
	ok(w, map[string]any{"removed": name})
}

func (s *Server) handleDockerVolumePrune(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	reclaimed, err := mgr.DockerPruneVolumes(r.Context())
	if err != nil {
		s.audit(r, "docker_volume_prune", "", err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_volume_prune", "", fmt.Sprintf("释放 %d 字节", reclaimed), true, "")
	ok(w, map[string]any{"reclaimed": reclaimed})
}

// ---------- 网络 ----------

func (s *Server) handleDockerNetworks(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	list, err := mgr.DockerNetworks(r.Context())
	if err != nil {
		s.dockerFail(w, err)
		return
	}
	if list == nil {
		list = []services.DockerNetworkView{}
	}
	ok(w, map[string]any{"list": list})
}

func (s *Server) handleDockerNetworkRemove(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.PathValue("path"), "/")
	mgr := s.svcManager()
	if err := mgr.DockerRemoveNetwork(r.Context(), id); err != nil {
		s.audit(r, "docker_network_remove", id, err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_network_remove", id, "完成", true, "")
	ok(w, map[string]any{"removed": id})
}

func (s *Server) handleDockerNetworkPrune(w http.ResponseWriter, r *http.Request) {
	mgr := s.svcManager()
	n, err := mgr.DockerPruneNetworks(r.Context())
	if err != nil {
		s.audit(r, "docker_network_prune", "", err.Error(), false, "")
		s.dockerFail(w, err)
		return
	}
	s.audit(r, "docker_network_prune", "", fmt.Sprintf("清理 %d 个", n), true, "")
	ok(w, map[string]any{"removed": n})
}
