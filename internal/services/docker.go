package services

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// userHomeDir 返回当前进程对应的真实用户家目录。
func userHomeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

// ---------------------------------------------------------------------------
//  Docker API 客户端
//
//  直接用 unix socket 上的 HTTP API，不调用 docker CLI。原因：
//    - 用户可能只装了 Docker 引擎（OrbStack/Colima 都提供 socket），没有 CLI
//    - 走 API 不存在命令拼接风险，参数是结构化的
//    - 不需要解析 CLI 的文本输出（不同版本格式会变）
// ---------------------------------------------------------------------------

type dockerClient struct {
	socket string
	http   *http.Client
}

func newDockerClient(socket string) *dockerClient {
	return &dockerClient{
		socket: socket,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 3 * time.Second}
					return d.DialContext(ctx, "unix", socket)
				},
				// 拉镜像可能很久，但 API 调用本身不该挂太久；
				// 长耗时操作（pull）由调用方单独放宽超时。
				ResponseHeaderTimeout: 60 * time.Second,
			},
			Timeout: 0, // 由 ctx 控制
		},
	}
}

// do 发起一次 API 请求，返回响应体。
func (c *dockerClient) do(ctx context.Context, method, path string, query url.Values) ([]byte, int, error) {
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Docker API 不可达: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return body, resp.StatusCode, fmt.Errorf("Docker API 返回 %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, resp.StatusCode, nil
}

// doStream 发起一次 API 请求并返回**未读完的响应体**（调用方负责 Close）。
//
// 为什么需要它：POST /images/create 的响应是"边拉边推"的 JSON 进度流，
// 用 do() 会把它整段攒在内存里、直到拉取结束才拿到 —— 用户在整个拉取期间
// 看不到任何进展（正是任务中心要消灭的那种"请等待"）。错误状态码在这里
// 就地读一小段并转成 error，避免调用方拿到一个只能读一次的 body。
func (c *dockerClient) doStream(ctx context.Context, method, path string, query url.Values) (io.ReadCloser, int, error) {
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Docker API 不可达: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, resp.StatusCode, fmt.Errorf("Docker API 返回 %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, resp.StatusCode, nil
}

// imageExists 报告本地是否已有该镜像（GET /images/{ref}/json）。
//
// 用它决定"创建容器前要不要先拉镜像"：/containers/create 在本地没有镜像时
// 直接报 No such image，**不会自己拉**，用户看到的是一句看不出所以然的失败；
// 先拉再建才能把"这次为什么要等"讲清楚。
//
// 任何错误（404 不存在、socket 断了）都返回 false：让调用方走"尝试拉取"
// 这条路，拉取失败时给出的错误才是真正有用的那条。
func (c *dockerClient) imageExists(ctx context.Context, ref string) bool {
	_, _, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil)
	return err == nil
}

// dockerContainer 是 /containers/json 返回的条目（只取需要的字段）。
type dockerContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`  // running / exited / created
	Status  string            `json:"Status"` // 人类可读描述
	Ports   []dockerPort      `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
}

type dockerPort struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// PrimaryName 返回容器主名称（去掉 Docker 自动加的前导斜杠）。
func (c dockerContainer) PrimaryName() string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// hostPort 返回容器的宿主端口（取第一个映射）。
func (c dockerContainer) hostPort() int {
	for _, p := range c.Ports {
		if p.PublicPort > 0 {
			return p.PublicPort
		}
	}
	return 0
}

// serverVersion 返回 Docker 引擎版本（GET /version）。
//
// 用它做"引擎真的能用了吗"的判据：unix socket 文件存在只说明进程起了，
// 不代表 API 已经能应答 —— 这正是本项目反复强调的"存在 ≠ 可用"。
func (c *dockerClient) serverVersion(ctx context.Context) (string, error) {
	body, code, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("Docker /version 返回 %d", code)
	}
	var out struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// listContainers 列出容器。all=true 时包含已停止的。
func (c *dockerClient) listContainers(ctx context.Context, all bool) ([]dockerContainer, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	body, _, err := c.do(ctx, "GET", "/containers/json", q)
	if err != nil {
		return nil, err
	}
	var out []dockerContainer
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析容器列表失败: %w", err)
	}
	return out, nil
}

// inspect 查询单个容器详情。
func (c *dockerClient) inspect(ctx context.Context, name string) (map[string]any, error) {
	body, _, err := c.do(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// containerAction 执行 start/stop/restart 等动作。
func (c *dockerClient) containerAction(ctx context.Context, name, action string, timeout int) error {
	q := url.Values{}
	if timeout > 0 {
		q.Set("t", fmt.Sprint(timeout))
	}
	_, _, err := c.do(ctx, "POST", "/containers/"+url.PathEscape(name)+"/"+action, q)
	return err
}

// containerLogs 读取容器日志（stdout+stderr，带时间戳便于排障）。
//
// tail > 0 只取最后 tail 行；tail <= 0 表示**完整日志**（Docker 的 tail=all）。
// 为什么需要"完整"：容器首次启动时打出的初始口令（File Browser 的 admin 密码
// 就是这一类）往往在很久以前，只取尾部会把它翻没 —— 而那串口令只在日志里有。
func (c *dockerClient) containerLogs(ctx context.Context, name string, tail int) (string, error) {
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("timestamps", "1")
	if tail > 0 {
		q.Set("tail", fmt.Sprint(tail))
	} else {
		q.Set("tail", "all")
	}
	body, _, err := c.do(ctx, "GET", "/containers/"+url.PathEscape(name)+"/logs", q)
	if err != nil {
		return "", err
	}
	// Docker 的日志流每一帧前有 8 字节头（stream 类型 + 长度），需要剥掉
	return stripDockerLogFrames(body), nil
}

// stripDockerLogFrames 去掉 Docker 多路复用日志的 8 字节前缀。
//
// 这些前缀是二进制头（1 字节流类型 + 3 字节填充 + 4 字节长度），
// 直接当文本显示会出现乱码。
func stripDockerLogFrames(b []byte) string {
	var out strings.Builder
	i := 0
	for i < len(b) {
		if i+8 <= len(b) && (b[i] == 1 || b[i] == 2) && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 0 {
			size := int(b[i+4])<<24 | int(b[i+5])<<16 | int(b[i+6])<<8 | int(b[i+7])
			start := i + 8
			end := start + size
			if end > len(b) {
				end = len(b)
			}
			out.Write(b[start:end])
			i = end
			continue
		}
		// 不是标准帧（例如非 TTY 之外的格式），整段当文本
		out.Write(b[i:])
		break
	}
	return out.String()
}

// removeContainer 删除容器（force=true 时先强杀）。
func (c *dockerClient) removeContainer(ctx context.Context, name string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	q.Set("v", "1") // 同时删除匿名卷
	_, _, err := c.do(ctx, "DELETE", "/containers/"+url.PathEscape(name), q)
	return err
}

// ---------------------------------------------------------------------------
//  镜像 / 卷 / 网络
//
//  这几块原先只有驱动内部用到的一点点（列容器、起停），面板侧完全没有入口。
//  统一在这里补齐，Web 层只做参数校验与错误包装。
// ---------------------------------------------------------------------------

// dockerImage 是 /images/json 返回的条目。
type dockerImage struct {
	ID         string            `json:"Id"`
	Tags       []string          `json:"RepoTags"`
	Size       int64             `json:"Size"`
	Created    int64             `json:"Created"`
	Digests    []string          `json:"RepoDigests"`
	Labels     map[string]string `json:"Labels"`
	SharedSize int64             `json:"SharedSize"`
	Containers int64             `json:"Containers"`
}

// dockerVolume 是 /volumes 返回的条目。
type dockerVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Scope      string            `json:"Scope"`
}

// dockerNetwork 是 /networks 返回的条目。
type dockerNetwork struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Scope      string            `json:"Scope"`
	Internal   bool              `json:"Internal"`
	Created    string            `json:"Created"`
	Labels     map[string]string `json:"Labels"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
	IPAM struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// listImages 列出镜像（all=true 时包含中间层镜像）。
func (c *dockerClient) listImages(ctx context.Context, all bool) ([]dockerImage, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	body, _, err := c.do(ctx, "GET", "/images/json", q)
	if err != nil {
		return nil, err
	}
	var out []dockerImage
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析镜像列表失败: %w", err)
	}
	return out, nil
}

// dockerPullEvent 是 POST /images/create 进度流里的一行 JSON。
//
// 字段名与 Docker API 一致：status/id/progressDetail/error/errorDetail。
// 层进度（Downloading / Extracting 的字节数）就在 progressDetail 里 ——
// 把它解析出来才能给用户"真的在下载"的证据，而不是一句"正在拉取"。
type dockerPullEvent struct {
	Status         string `json:"status"`
	ID             string `json:"id"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// formatDockerPullEvent 把一条进度事件渲染成一行人类可读文本。
//
// 注意**不丢原始 status**：Docker 会推 "Pulling fs layer"、"Download complete"、
// "Digest: sha256:…"、"Status: Downloaded newer image for …" 这些没有进度的行，
// 它们正是判断"卡在哪一步"的关键。
func formatDockerPullEvent(ev dockerPullEvent) string {
	status := strings.TrimSpace(ev.Status)
	if status == "" {
		return ""
	}
	line := status
	if id := strings.TrimSpace(ev.ID); id != "" {
		line = id + ": " + status
	}
	switch {
	case ev.ProgressDetail.Total > 0:
		cur, total := ev.ProgressDetail.Current, ev.ProgressDetail.Total
		line += fmt.Sprintf(" %s/%s (%d%%)", humanBytes(cur), humanBytes(total), cur*100/total)
	case ev.ProgressDetail.Current > 0:
		line += " " + humanBytes(ev.ProgressDetail.Current)
	}
	return line
}

// pullImage 拉取镜像，并**逐行**把 Docker 的进度流推给任务中心。
//
// 走 POST /images/create 并边读边发：Docker 是边拉边推 JSON 事件，
// 过去这里用 do() 把整段响应读进内存、只在结束时返回最后一条 status ——
// 于是"拉取中"这个动作在任务日志里是一片空白，用户只能猜是不是卡住了。
//
// 返回值仍是"最后一条有意义的 status"（保持既有调用方与测试的语义），
// 但完整过程已经实时外发；emit 在没有进度接收器（非任务路径）时是空操作。
func (c *dockerClient) pullImage(ctx context.Context, ref string) (string, error) {
	q := url.Values{}
	q.Set("fromImage", ref)

	// 命令标签如实记录**实际发出的请求**：这里走 Docker API，不经过 CLI，
	// 所以不写成 `docker pull`（用户复制去执行是另一回事）。
	emit(ctx, tasks.LevelStep, "拉取镜像 "+ref)
	emit(ctx, tasks.LevelCmd, "# POST /images/create fromImage="+ref)

	body, _, err := c.doStream(ctx, http.MethodPost, "/images/create", q)
	if err != nil {
		emit(ctx, tasks.LevelErr, err.Error())
		return "", err
	}
	defer func() { _ = body.Close() }()

	sc := bufio.NewScanner(body)
	// 单行可能很大（层很多的镜像、被压缩成一行的 JSON）：默认 64KB 就报错，
	// 报错会让我们在拉到一半时误判成功/失败。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	last := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev dockerPullEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// 不是 JSON 也照样给用户看：宁可多一行噪音，也不静默吞掉输出。
			emit(ctx, tasks.LevelOut, line)
			continue
		}
		if msg := ev.Error; msg != "" || ev.ErrorDetail.Message != "" {
			if ev.ErrorDetail.Message != "" {
				msg = ev.ErrorDetail.Message
			}
			emit(ctx, tasks.LevelErr, "拉取失败: "+msg)
			return msg, fmt.Errorf("拉取失败: %s", msg)
		}
		if text := formatDockerPullEvent(ev); text != "" {
			emit(ctx, tasks.LevelOut, text)
			last = ev.Status
		}
	}
	// 流半路断掉（网络中断 / 被中断）**不能报成功**：
	// 报成功会让用户以为镜像已经就位，后续创建容器时才发现没有。
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return last, fmt.Errorf("拉取被中断: %w", ctx.Err())
		}
		return last, fmt.Errorf("读取拉取进度失败: %w", err)
	}
	if last == "" {
		last = "完成"
	}
	emit(ctx, tasks.LevelOK, "镜像 "+ref+" 拉取完成（"+last+"）")
	return last, nil
}

// removeImage 删除镜像。force=true 时强制删除（多个 tag 指向同一层时需要）。
func (c *dockerClient) removeImage(ctx context.Context, ref string, force bool) (string, error) {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	body, _, err := c.do(ctx, "DELETE", "/images/"+url.PathEscape(ref), q)
	if err != nil {
		return string(body), err
	}
	return string(body), nil
}

// pruneImages 清理悬空镜像（dangling），返回释放的空间。
//
// 只删悬空（<none>:<none>）是刻意的：把"清理"做成会删掉用户正在用的镜像，
// 是那种"点一下损失很大"的操作。要删带标签的镜像走 removeImage。
func (c *dockerClient) pruneImages(ctx context.Context) (int64, error) {
	body, _, err := c.do(ctx, "POST", "/images/prune", nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("解析清理结果失败: %w", err)
	}
	return out.SpaceReclaimed, nil
}

// listVolumes 列出数据卷。
func (c *dockerClient) listVolumes(ctx context.Context) ([]dockerVolume, error) {
	body, _, err := c.do(ctx, "GET", "/volumes", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Volumes []dockerVolume `json:"Volumes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析数据卷列表失败: %w", err)
	}
	return out.Volumes, nil
}

// removeVolume 删除数据卷。force=true 时即使有容器在用也删。
func (c *dockerClient) removeVolume(ctx context.Context, name string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	_, _, err := c.do(ctx, "DELETE", "/volumes/"+url.PathEscape(name), q)
	return err
}

// pruneVolumes 清理未被任何容器使用的数据卷。
func (c *dockerClient) pruneVolumes(ctx context.Context) (int64, error) {
	body, _, err := c.do(ctx, "POST", "/volumes/prune", nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("解析清理结果失败: %w", err)
	}
	return out.SpaceReclaimed, nil
}

// listNetworks 列出网络。
func (c *dockerClient) listNetworks(ctx context.Context) ([]dockerNetwork, error) {
	body, _, err := c.do(ctx, "GET", "/networks", nil)
	if err != nil {
		return nil, err
	}
	var out []dockerNetwork
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析网络列表失败: %w", err)
	}
	return out, nil
}

// removeNetwork 删除网络。
func (c *dockerClient) removeNetwork(ctx context.Context, id string) error {
	_, _, err := c.do(ctx, "DELETE", "/networks/"+url.PathEscape(id), nil)
	return err
}

// pruneNetworks 清理未被使用的网络。Docker 没有 prune 接口，
// 需要自己筛：不内置、没有容器连接的网络才算"可清理"。
func (c *dockerClient) pruneNetworks(ctx context.Context) (int, error) {
	nets, err := c.listNetworks(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, n := range nets {
		if isBuiltinNetwork(n.Name) || len(n.Containers) > 0 {
			continue
		}
		if err := c.removeNetwork(ctx, n.ID); err == nil {
			removed++
		}
	}
	return removed, nil
}

// isBuiltinNetwork 判断是否是 Docker 自带的网络。这几个不能删，
// 删了会破坏默认网络行为，而且 Docker 自己也会拒绝。
func isBuiltinNetwork(name string) bool {
	switch name {
	case "bridge", "host", "none":
		return true
	}
	return false
}

// DockerCreateSpec 是创建容器时需要的字段（Web 层解析后的结构化结果）。
type DockerCreateSpec struct {
	Name          string           `json:"name"`
	Image         string           `json:"image"`
	Cmd           []string         `json:"cmd"`
	Entrypoint    []string         `json:"entrypoint"`
	Env           []string         `json:"env"`
	Ports         []dockerPortBind `json:"ports"`
	Binds         []string         `json:"binds"`
	Volumes       []string         `json:"volumes"`
	RestartPolicy string           `json:"restart_policy"`
	Network       string           `json:"network"`
	AutoStart     bool             `json:"auto_start"`
	Privileged    bool             `json:"privileged"`
}

// dockerPortBind 是一条端口映射。HostPort 为 0 时由 Docker 随机分配。
type dockerPortBind struct {
	HostIP   string `json:"host_ip"`
	HostPort int    `json:"host_port"`
	ContPort int    `json:"container_port"`
	Proto    string `json:"proto"`
}

// createContainer 按结构化配置创建容器。
//
// 为什么不接受"自由格式的 HostConfig"：那是一整个 root 等价面，
// 让前端把任意 JSON 透传进来等于把 Docker API 完全开放给浏览器。
// 这里只暴露常用字段（端口/环境变量/挂载/重启策略/网络），够覆盖日常用法。
func (c *dockerClient) createContainer(ctx context.Context, spec DockerCreateSpec) (string, error) {
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}

	for _, p := range spec.Ports {
		if p.ContPort <= 0 {
			continue
		}
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", p.ContPort, proto)
		exposed[key] = struct{}{}

		hostIP := p.HostIP
		hostPort := ""
		if p.HostPort > 0 {
			hostPort = fmt.Sprint(p.HostPort)
		}
		bindings[key] = append(bindings[key], map[string]string{
			"HostIp": hostIP, "HostPort": hostPort,
		})
	}

	payload := map[string]any{
		"Image":        spec.Image,
		"ExposedPorts": exposed,
		"Env":          spec.Env,
		"HostConfig": map[string]any{
			"Binds":         spec.Binds,
			"PortBindings":  bindings,
			"RestartPolicy": map[string]any{"Name": normalizeRestartPolicy(spec.RestartPolicy)},
			"Privileged":    spec.Privileged,
		},
	}
	if len(spec.Cmd) > 0 {
		payload["Cmd"] = spec.Cmd
	}
	if len(spec.Entrypoint) > 0 {
		payload["Entrypoint"] = spec.Entrypoint
	}
	if len(spec.Volumes) > 0 {
		payload["Volumes"] = func() map[string]struct{} {
			m := map[string]struct{}{}
			for _, v := range spec.Volumes {
				m[v] = struct{}{}
			}
			return m
		}()
	}
	if spec.Network != "" {
		payload["HostConfig"].(map[string]any)["NetworkMode"] = spec.Network
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	q := url.Values{}
	if spec.Name != "" {
		q.Set("name", spec.Name)
	}

	respBody, code, err := c.doBody(ctx, http.MethodPost, "/containers/create", q, body)
	if err != nil {
		return "", err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return "", fmt.Errorf("创建容器返回 %d: %s", code, truncate(strings.TrimSpace(string(respBody)), 300))
	}
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("解析创建结果失败: %w", err)
	}
	return out.ID, nil
}

// normalizeRestartPolicy 把界面上的取值收敛成 Docker 认识的四种。
func normalizeRestartPolicy(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "always", "unless-stopped", "on-failure":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "no"
	}
}

// doBody 与 do 相同，但带请求体（创建容器、写文件类接口需要）。
func (c *dockerClient) doBody(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, int, error) {
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Docker API 不可达: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return out, resp.StatusCode, fmt.Errorf("Docker API 返回 %d: %s",
			resp.StatusCode, truncate(strings.TrimSpace(string(out)), 300))
	}
	return out, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
//  Docker 驱动
// ---------------------------------------------------------------------------

type dockerDriver struct {
	opt    Options
	svc    *Service
	client *dockerClient
}

func newDockerDriver(opt Options, s *Service) *dockerDriver {
	return &dockerDriver{opt: opt, svc: s, client: newDockerClient(opt.DockerSocket)}
}

func (d *dockerDriver) Kind() Kind { return KindDocker }

// target 返回容器名（优先用注册表里指定的，否则用服务名）。
func (d *dockerDriver) target() string {
	if d.svc.Container != "" {
		return d.svc.Container
	}
	return d.svc.Name
}

func (d *dockerDriver) Status(ctx context.Context) (State, error) {
	list, err := d.client.listContainers(ctx, true)
	if err != nil {
		return State{Status: "error", Detail: err.Error()}, nil
	}
	for _, c := range list {
		if c.PrimaryName() != d.target() && c.ID[:min(12, len(c.ID))] != d.target() {
			continue
		}
		st := State{Endpoint: d.endpointFor(c.hostPort())}
		switch c.State {
		case "running":
			st.Running = true
			st.Status = "running"
			st.Detail = c.Status
		case "exited":
			st.Status = "stopped"
			st.Detail = c.Status
		case "restarting":
			st.Status = "error"
			st.Detail = "容器正在反复重启（restarting），请查看日志"
		default:
			st.Status = c.State
			st.Detail = c.Status
		}
		return st, nil
	}
	return State{Status: "not-installed", Detail: "容器不存在（可能已被删除）"}, nil
}

func (d *dockerDriver) endpointFor(port int) string {
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *dockerDriver) Start(ctx context.Context) error {
	return d.client.containerAction(ctx, d.target(), "start", 0)
}

func (d *dockerDriver) Stop(ctx context.Context) error {
	// 给 10 秒优雅退出时间，超时 Docker 会强杀
	return d.client.containerAction(ctx, d.target(), "stop", 10)
}

func (d *dockerDriver) Restart(ctx context.Context) error {
	return d.client.containerAction(ctx, d.target(), "restart", 10)
}

func (d *dockerDriver) Logs(ctx context.Context, lines int) (string, error) {
	// 服务管理页的"日志"是固定行数预览；0/负数在这里兜底成 200，
	// 把"tail<=0 = 完整日志"这个约定留给 Docker 页的显式请求（见 Manager.DockerContainerLogs）。
	if lines <= 0 {
		lines = 200
	}
	return d.client.containerLogs(ctx, d.target(), lines)
}

// LogStream 用轮询实现（每 1 秒拉末 50 行，只推增量）。
//
// 为什么不用 Docker 的 /logs?follow=1 流式接口：
// 那个接口在连接期间会占用一条 socket，且断线后需要自己重连；
// 轮询实现虽然朴素，但行为可预测、可取消，对面板这种低频查看场景足够。
func (d *dockerDriver) LogStream(ctx context.Context) (<-chan string, error) {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		last := ""
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				content, err := d.client.containerLogs(ctx, d.target(), 50)
				if err != nil {
					continue
				}
				if content == last {
					continue
				}
				// 只推新增部分：找到上次内容的最后一行，从它之后开始
				chunk := diffLog(last, content)
				last = content
				if chunk != "" {
					select {
					case ch <- chunk:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch, nil
}

// diffLog 返回 newContent 中相对 oldContent 新增的部分。
func diffLog(oldContent, newContent string) string {
	if oldContent == "" {
		return newContent
	}
	oldLines := strings.Split(strings.TrimRight(oldContent, "\n"), "\n")
	newLines := strings.Split(strings.TrimRight(newContent, "\n"), "\n")
	// 找到最后一个 old 行在 new 中的位置
	last := oldLines[len(oldLines)-1]
	for i := len(newLines) - 1; i >= 0; i-- {
		if newLines[i] == last {
			if i+1 < len(newLines) {
				return strings.Join(newLines[i+1:], "\n") + "\n"
			}
			return ""
		}
	}
	return newContent
}

func (d *dockerDriver) Health(ctx context.Context) Health {
	return httpHealth(ctx, d.svc)
}

func (d *dockerDriver) Uninstall(ctx context.Context) error {
	return d.client.removeContainer(ctx, d.target(), true)
}

// ---------------------------------------------------------------------------
//  Compose 驱动
//
//  这里用 docker compose CLI 而不是 API：compose 会创建网络、卷、
//  多个容器并维护依赖关系，用 API 手工复现这套逻辑既复杂又容易与
//  compose 的语义不一致。CLI 是官方支持的管理入口。
// ---------------------------------------------------------------------------

type composeDriver struct {
	opt Options
	svc *Service
}

func newComposeDriver(opt Options, s *Service) *composeDriver {
	return &composeDriver{opt: opt, svc: s}
}

func (d *composeDriver) Kind() Kind { return KindCompose }

// composeBin 查找可用的 compose 命令。
//
// 依次尝试：docker compose（插件，v2，推荐）→ docker-compose（v1 独立二进制）。
// 返回命令与参数前缀。
func composeBin() (string, []string, bool) {
	if p, err := exec.LookPath("docker"); err == nil {
		// 确认 compose 插件可用
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, p, "compose", "version").Run(); err == nil {
			return p, []string{"compose"}, true
		}
	}
	// 只有 v1 独立二进制的情况（Colima 装出来的就是这种：
	// 插件目录里是 docker-compose 的软链，`docker compose` 子命令反而没有）。
	//
	// 这里必须**显式补上 brew 与 /usr/local 的绝对路径**：面板是 LaunchDaemon，
	// 它的 PATH 不含 /opt/homebrew/bin 时 LookPath 会找不到 ——
	// 真机就这么失败过：日志里一句 `docker: unknown command: docker compose`，
	// 而机器上 `docker-compose` 明明装好了。
	candidates := []string{}
	if p, err := exec.LookPath("docker-compose"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/homebrew/bin/docker-compose", "/usr/local/bin/docker-compose")
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil, true
		}
	}
	return "", nil, false
}

// run 执行 compose 命令。
func (d *composeDriver) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	bin, prefix, ok := composeBin()
	if !ok {
		return "", fmt.Errorf("未找到 docker compose 命令。" +
			"请先安装 Docker（OrbStack / Colima / Docker Desktop 任一即可）")
	}
	if d.svc.ComposeFile == "" {
		return "", fmt.Errorf("该 compose 服务未配置 compose 文件路径")
	}
	if _, err := os.Stat(d.svc.ComposeFile); err != nil {
		return "", fmt.Errorf("compose 文件不存在: %s", d.svc.ComposeFile)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{}, prefix...)
	full = append(full, "-f", d.svc.ComposeFile)
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = filepath.Dir(d.svc.ComposeFile)
	// compose 需要 docker socket；把 DOCKER_HOST 显式传给子进程，
	// 这样即使用户的 socket 在非默认位置（OrbStack/Colima）也能工作
	if d.opt.DockerSocket != "" && os.Getenv("DOCKER_HOST") == "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+d.opt.DockerSocket)
	}
	// compose 的镜像层进度是逐行输出的，必须流式 ——
	// 「正在拉取镜像」这一句话对用户毫无信息量，层进度才是真实进展。
	started := time.Now()
	out, err := streamCmd(ctx, cmd)
	if err != nil {
		// 必须把**失败原因**写出来。曾经这里只贴最后 400 字节层进度，
		// 于是"拉镜像超过 20 分钟被中止"在任务里显示成一句看不出所以然的
		// "compose 命令失败: … Downloading 128MB"，用户会以为是镜像本身有问题
		// （2026-09-16 mini 真机：n8n / stirling-pdf 恰好都在 ~1202s 失败）。
		reason := err.Error()
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			reason = fmt.Sprintf("超过 %s 上限被中止（镜像层没拉完；可稍后重试，或先手动 docker pull 提高命中率）",
				timeout)
		case errors.Is(ctx.Err(), context.Canceled):
			reason = "任务被取消"
		}
		return out, fmt.Errorf("compose 命令失败（用时 %s，%s）: %s",
			time.Since(started).Round(time.Second), reason, truncate(strings.TrimSpace(out), 400))
	}
	return out, nil
}

// Status 通过 `compose ps --format json` 判断运行状态。
func (d *composeDriver) Status(ctx context.Context) (State, error) {
	// 注意：老版本 compose 不支持 --format json，此时退回文本解析
	out, err := d.run(ctx, 20*time.Second, "ps", "--format", "json")
	if err != nil {
		// 文件缺失等配置问题直接上报，不掩盖
		if strings.Contains(err.Error(), "compose 文件不存在") ||
			strings.Contains(err.Error(), "未找到 docker compose") {
			return State{Status: "unavailable", Detail: err.Error()}, nil
		}
		return State{Status: "error", Detail: err.Error()}, nil
	}
	services := parseComposePSJSON(out)
	total, running := 0, 0
	for _, s := range services {
		total++
		if s.State == "running" {
			running++
		}
	}
	st := State{Endpoint: d.endpointFor()}
	switch {
	case total == 0:
		st.Status = "stopped"
		st.Detail = "compose 项目尚未启动（没有容器）"
	case running == total:
		st.Running = true
		st.Status = "running"
		st.Detail = fmt.Sprintf("%d/%d 个容器运行中", running, total)
	default:
		st.Status = "error"
		st.Detail = fmt.Sprintf("%d/%d 个容器运行中（部分异常）", running, total)
	}
	return st, nil
}

func (d *composeDriver) endpointFor() string {
	if d.svc.Port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", d.svc.Port)
}

// composePSEntry 是 `compose ps --format json` 的单条记录。
type composePSEntry struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Health  string `json:"Health"`
}

// parseComposePSJSON 兼容两种输出格式：
//   - 新版：每行一个 JSON 对象
//   - 更早版本：整个输出是一个 JSON 数组
func parseComposePSJSON(out string) []composePSEntry {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []composePSEntry
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			return arr
		}
	}
	var list []composePSEntry
	sc := bufio.NewScanner(strings.NewReader(trimmed))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e composePSEntry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			list = append(list, e)
		}
	}
	return list
}

func (d *composeDriver) Start(ctx context.Context) error {
	_, err := d.run(ctx, 10*time.Minute, "up", "-d")
	return err
}

func (d *composeDriver) Stop(ctx context.Context) error {
	_, err := d.run(ctx, 2*time.Minute, "stop")
	return err
}

func (d *composeDriver) Restart(ctx context.Context) error {
	_, err := d.run(ctx, 5*time.Minute, "restart")
	return err
}

func (d *composeDriver) Logs(ctx context.Context, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	return d.run(ctx, 30*time.Second, "logs", "--tail", fmt.Sprint(lines), "--no-color")
}

// LogStream 用 `compose logs -f` 子进程实现。
// 这是唯一合适的方式：compose 的日志来自多个容器，轮询无法正确合并顺序。
func (d *composeDriver) LogStream(ctx context.Context) (<-chan string, error) {
	bin, prefix, ok := composeBin()
	if !ok {
		return nil, fmt.Errorf("未找到 docker compose 命令")
	}
	full := append([]string{}, prefix...)
	full = append(full, "-f", d.svc.ComposeFile, "logs", "-f", "--tail", "50", "--no-color")
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = filepath.Dir(d.svc.ComposeFile)
	if d.opt.DockerSocket != "" && os.Getenv("DOCKER_HOST") == "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+d.opt.DockerSocket)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	ch := make(chan string, 128)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case ch <- sc.Text() + "\n":
			case <-ctx.Done():
				_ = cmd.Process.Kill()
				return
			}
		}
		_ = cmd.Wait()
	}()
	return ch, nil
}

func (d *composeDriver) Health(ctx context.Context) Health {
	return httpHealth(ctx, d.svc)
}

// Uninstall 停止并删除 compose 项目创建的容器与网络（保留具名卷）。
//
// 刻意不加 -v：具名卷通常放着用户数据（数据库文件等），
// 静默删除数据是不可接受的；需要彻底清理时会在返回信息里提示。
func (d *composeDriver) Uninstall(ctx context.Context) error {
	_, err := d.run(ctx, 3*time.Minute, "down")
	return err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
