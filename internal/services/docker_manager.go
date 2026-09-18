package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/logx"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Docker 管理门面（给 Web 层用）
//
//  为什么单独开一个文件而不是塞进 docker.go：
//  docker.go 里是**驱动**（把一条服务记录映射到容器/ compose 操作），
//  这里对外暴露的是**面板功能**（列容器、拉镜像、清理悬空镜像…）。
//  两者共用同一个 socket 客户端，但关注点不同：驱动要回答"这条服务在跑吗"，
//  这里要回答"这台机器上有哪些容器/镜像/卷"。
//
//  Docker 不可用时的处理原则：**明确告诉用户"没装/没启动"，而不是 500。**
//  macOS 上 Docker 不是系统自带能力，没装是常态；报一个看不懂的 500
//  会让人以为是面板坏了。
// ============================================================================

var dockerLog = logx.New("docker")

// DockerInfo 描述本机 Docker 环境，供前端决定是否禁用 Docker 页。
type DockerInfo struct {
	Available bool   `json:"available"`
	Socket    string `json:"socket"`
	Version   string `json:"version"`
	Error     string `json:"error,omitempty"`
}

// dockerClientOrErr 返回可用的客户端；不可用时给出人能看懂的原因。
func (m *Manager) dockerClientOrErr() (*dockerClient, error) {
	sock := strings.TrimSpace(m.opt.DockerSocket)
	if sock == "" {
		return nil, fmt.Errorf("没有可用的 Docker socket。" +
			"macOS 上 Docker 引擎不是系统自带：请在「Docker」页点「🐳 一键安装 Docker（Colima）」，" +
			"或安装 OrbStack / Docker Desktop 任一")
	}
	if _, err := os.Stat(sock); err != nil {
		// 指向「Docker」页的一键安装，而不是「服务管理」——
		// 服务管理已经合并进「应用 → 我的应用」，再指那里等于把用户送进死路
		// （2026-09-19 用户原话："还有一个入口『去服务管理』，现在服务管理已经
		//  合并到应用市场了，这个入口更没有意义了"）。
		return nil, fmt.Errorf("Docker socket 不存在（%s）：引擎没有在运行。"+
			"到「Docker」页点「🐳 一键安装 Docker（Colima）」（已装过则点「▶ 启动 Docker 运行时」）", sock)
	}
	return newDockerClient(sock), nil
}

// DockerInfo 返回 Docker 环境概览。
//
// 注意这里**不返回 error**：环境不可用本身就是要显示给用户的信息。
func (m *Manager) DockerInfo(ctx context.Context) DockerInfo {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return DockerInfo{Available: false, Socket: m.opt.DockerSocket, Error: err.Error()}
	}
	v, err := c.serverVersion(ctx)
	if err != nil {
		return DockerInfo{Available: false, Socket: c.socket, Error: err.Error()}
	}
	return DockerInfo{Available: true, Socket: c.socket, Version: v}
}

// ---------- 容器 ----------

// DockerContainers 列出容器。all=false 只看运行中的。
func (m *Manager) DockerContainers(ctx context.Context, all bool) ([]dockerContainer, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return nil, err
	}
	return c.listContainers(ctx, all)
}

// DockerContainerAction 对容器执行 start / stop / restart / pause / unpause / kill。
func (m *Manager) DockerContainerAction(ctx context.Context, name, action string, timeout int) error {
	switch action {
	case "start", "stop", "restart", "pause", "unpause", "kill":
	default:
		return fmt.Errorf("不支持的操作: %s", action)
	}
	c, err := m.dockerClientOrErr()
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("缺少容器名")
	}
	return c.containerAction(ctx, name, action, timeout)
}

// DockerContainerLogs 读取容器日志。
//
// tail == 0 是"要**完整**日志"（Docker 的 tail=all）：容器首次启动打出的初始
// 口令（File Browser 的 admin 密码）在很久以前，只取尾部会把它翻没。
// 其余越界值兜底成 300，避免调用方手滑传个天文数字。
func (m *Manager) DockerContainerLogs(ctx context.Context, name string, tail int) (string, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("缺少容器名")
	}
	if tail < 0 || tail > 5000 {
		tail = 300
	}
	return c.containerLogs(ctx, name, tail)
}

// DockerRemoveContainer 删除容器。force=true 会先强杀运行中的容器。
func (m *Manager) DockerRemoveContainer(ctx context.Context, name string, force bool) error {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("缺少容器名")
	}
	return c.removeContainer(ctx, name, force)
}

// DockerInspectContainer 返回容器详情（用于详情面板）。
func (m *Manager) DockerInspectContainer(ctx context.Context, name string) (map[string]any, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("缺少容器名")
	}
	return c.inspect(ctx, name)
}

// DockerCreateContainer 创建容器，并在 autoStart 时立即启动。
//
// 创建与启动分两步是刻意的：Docker 的 create 只建不跑，这样"创建成功但启动失败"
// （端口被占用、命令不存在）能被准确区分并报给用户，而不是笼统一个失败。
//
// 本地没有镜像时**先拉取**并把层进度实时外发：/containers/create 不会自己拉，
// 直接失败只会得到一句 "No such image"，用户既不知道为什么，也看不到过程。
func (m *Manager) DockerCreateContainer(ctx context.Context, spec DockerCreateSpec) (string, error) {
	if strings.TrimSpace(spec.Image) == "" {
		return "", fmt.Errorf("必须指定镜像")
	}
	if strings.ContainsAny(spec.Image, " \t\n") {
		return "", fmt.Errorf("镜像名不能包含空白字符")
	}
	c, err := m.dockerClientOrErr()
	if err != nil {
		return "", err
	}

	if c.imageExists(ctx, spec.Image) {
		emit(ctx, tasks.LevelStep, "本地已有镜像 "+spec.Image)
	} else {
		emit(ctx, tasks.LevelStep, "本地没有镜像 "+spec.Image+"，先拉取（这一步可能要几分钟）")
		if _, perr := c.pullImage(ctx, spec.Image); perr != nil {
			return "", fmt.Errorf("拉取镜像失败: %w", perr)
		}
	}

	what := "创建容器"
	if spec.Name != "" {
		what += " " + spec.Name
	}
	emit(ctx, tasks.LevelStep, what)

	id, err := c.createContainer(ctx, spec)
	if err != nil {
		emit(ctx, tasks.LevelErr, err.Error())
		return "", err
	}
	emit(ctx, tasks.LevelOut, "容器已创建："+shortID(id))

	if spec.AutoStart {
		target := spec.Name
		if target == "" {
			target = id
		}
		emit(ctx, tasks.LevelStep, "启动容器 "+target)
		if err := c.containerAction(ctx, target, "start", 0); err != nil {
			emit(ctx, tasks.LevelErr, err.Error())
			return id, fmt.Errorf("容器已创建（%s）但启动失败：%w", shortID(id), err)
		}
	}
	return id, nil
}

// DockerProjectContainer 是某个 compose 项目下的一个容器（部署结果里给前端用）。
//
// 刻意只给"看日志/认容器"需要的字段：部署结果会被写进任务结果并渲染出来，
// 塞进 Labels 之类的内部结构既没用、也容易把口令一类的东西漏出去。
type DockerProjectContainer struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
}

// DockerComposeProjectContainers 列出某个 compose 项目下的容器。
//
// 按容器标签 com.docker.compose.project 匹配，而不是按容器名：
// 容器名可以由 yml 里的 container_name 任意指定，标签是 compose 自己写的。
// 出错时返回空列表而不报错：这是"部署完之后顺手给的附加信息"，
// 不该因为它把一个已经成功的部署判成失败。
func (m *Manager) DockerComposeProjectContainers(ctx context.Context, project string) []DockerProjectContainer {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return nil
	}
	list, err := c.listContainers(ctx, true)
	if err != nil {
		return nil
	}
	out := []DockerProjectContainer{}
	for _, ct := range list {
		if !strings.EqualFold(strings.TrimSpace(ct.Labels["com.docker.compose.project"]), project) {
			continue
		}
		out = append(out, DockerProjectContainer{
			Name: ct.PrimaryName(), Image: ct.Image, State: ct.State, Status: ct.Status,
		})
	}
	return out
}

// DockerScrapeStartupCredentials 从刚启动的容器日志里捞"首次启动随机生成的账号口令"。
//
// 为什么值得做（用户原话："很多 docker 项目的登录信息都在日志里！如 filebrowser"）：
// File Browser 这类镜像的初始口令**只在首次启动时打进日志一次**，用户在 Docker 页
// 点完「部署」通常不会想到去翻日志，于是永远登不进去。这里把命中的凭据放进部署
// 结果（任务窗的结果卡片会渲染成可复制的凭据区块），让它一眼可见。
//
// 识别规则复用 install.go 的 generatedCredRe：同一套规则，避免两处口径不一致。
// 捞不到不算失败 —— 有些应用本来就不生成随机口令。
//
// 安全约定（硬要求，见 InstallResult.Credentials）：明文口令**只**进返回的
// Credentials，日志/审计里一个字都不写 —— 任务日志会被长期保存与转发。
func (m *Manager) DockerScrapeStartupCredentials(ctx context.Context, containers []DockerProjectContainer) []Credential {
	c, err := m.dockerClientOrErr()
	if err != nil || len(containers) == 0 {
		return nil
	}
	out := []Credential{}
	for _, ct := range containers {
		if ct.State != "running" {
			continue
		}
		// 只取尾部若干行：口令打在启动那一小段里，没必要把整份日志读进来。
		logs, err := c.containerLogs(ctx, ct.Name, 200)
		if err != nil || logs == "" {
			continue
		}
		match := generatedCredRe.FindStringSubmatch(logs)
		if len(match) != 3 {
			continue
		}
		user, pass := match[1], match[2]
		// 只提示"捞到了"，不带口令值：口令走凭据区块（结果卡片）那一条路。
		emit(ctx, tasks.LevelOK, "容器 "+ct.Name+" 的日志里有初始登录信息（用户名 "+user+"）——"+
			"口令已放进本次任务的结果里，请立刻复制保存。")
		emit(ctx, tasks.LevelWarn, "这串口令只在首次启动时生成一次，登录后请立刻改掉；"+
			"也可以在「容器」分区点该容器的「日志」看完整日志。")
		out = append(out,
			Credential{Key: "username", Value: user, Label: ct.Name + " 用户名"},
			Credential{Key: "password", Value: pass, Label: ct.Name + " 初始口令（首次启动随机生成）"},
		)
	}
	return out
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ---------- 镜像 ----------

func (m *Manager) DockerImages(ctx context.Context, all bool) ([]dockerImage, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return nil, err
	}
	return c.listImages(ctx, all)
}

// DockerPullImage 拉取镜像。耗时可能很长，超时由 Web 层的 ctx 控制。
func (m *Manager) DockerPullImage(ctx context.Context, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("必须指定镜像名")
	}
	if strings.ContainsAny(ref, " \t\n") {
		return "", fmt.Errorf("镜像名不能包含空白字符")
	}
	c, err := m.dockerClientOrErr()
	if err != nil {
		return "", err
	}
	dockerLog.Info("开始拉取镜像 %s", ref)
	msg, err := c.pullImage(ctx, ref)
	if err != nil {
		return msg, err
	}
	dockerLog.Info("镜像 %s 拉取完成：%s", ref, msg)
	return msg, nil
}

func (m *Manager) DockerRemoveImage(ctx context.Context, ref string, force bool) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("必须指定镜像")
	}
	c, err := m.dockerClientOrErr()
	if err != nil {
		return "", err
	}
	return c.removeImage(ctx, ref, force)
}

// DockerPruneImages 清理悬空镜像，返回释放字节数。
func (m *Manager) DockerPruneImages(ctx context.Context) (int64, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return 0, err
	}
	reclaimed, err := c.pruneImages(ctx)
	if err == nil {
		dockerLog.Info("清理悬空镜像，释放 %d 字节", reclaimed)
	}
	return reclaimed, err
}

// ---------- 卷 ----------

func (m *Manager) DockerVolumes(ctx context.Context) ([]dockerVolume, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return nil, err
	}
	return c.listVolumes(ctx)
}

func (m *Manager) DockerRemoveVolume(ctx context.Context, name string, force bool) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("必须指定数据卷名")
	}
	c, err := m.dockerClientOrErr()
	if err != nil {
		return err
	}
	return c.removeVolume(ctx, name, force)
}

func (m *Manager) DockerPruneVolumes(ctx context.Context) (int64, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return 0, err
	}
	reclaimed, err := c.pruneVolumes(ctx)
	if err == nil {
		dockerLog.Info("清理未使用数据卷，释放 %d 字节", reclaimed)
	}
	return reclaimed, err
}

// ---------- 网络 ----------

func (m *Manager) DockerNetworks(ctx context.Context) ([]dockerNetwork, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return nil, err
	}
	return c.listNetworks(ctx)
}

func (m *Manager) DockerRemoveNetwork(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("必须指定网络")
	}
	c, err := m.dockerClientOrErr()
	if err != nil {
		return err
	}
	// 内置网络不能删：删了会破坏默认网络行为，Docker 自己也会拒绝。
	if isBuiltinNetwork(id) {
		return fmt.Errorf("内置网络 %s 不能删除", id)
	}
	return c.removeNetwork(ctx, id)
}

func (m *Manager) DockerPruneNetworks(ctx context.Context) (int, error) {
	c, err := m.dockerClientOrErr()
	if err != nil {
		return 0, err
	}
	n, err := c.pruneNetworks(ctx)
	if err == nil && n > 0 {
		dockerLog.Info("清理未使用网络 %d 个", n)
	}
	return n, err
}

// ---------- Compose 项目 ----------

// DockerComposeProject 是面板里的一个 compose 项目（一个目录 + 一份 yml）。
type DockerComposeProject struct {
	Name        string `json:"name"`         // 目录名，也是服务标识
	Dir         string `json:"dir"`          // <work_dir>/compose/<name>
	File        string `json:"file"`         // docker-compose.yml 绝对路径
	Exists      bool   `json:"exists"`       // yml 是否已存在
	Size        int64  `json:"size"`         // yml 字节数
	UpdatedAt   string `json:"updated_at"`   // yml 最后修改时间
	Registered  bool   `json:"registered"`   // 是否已登记进「服务管理」
	ServiceName string `json:"service_name"` // 登记后的服务名（= name）
}

// ComposeRoot 返回 compose 项目根目录。
//
// 所有 compose 文件都必须落在这个目录下 —— 这是路径安全的边界：
// 项目名会被严格校验，拼出来的路径再核对一次前缀，
// 保证「编辑 compose 文件」这个能力不会变成任意文件写入。
func (m *Manager) ComposeRoot() string {
	base := m.opt.WorkDir
	if base == "" {
		base = "/opt/zizpanel/work"
	}
	return filepath.Join(base, "compose")
}

// validComposeName 校验项目名：只允许字母数字与 . _ -，且不以点开头。
//
// 刻意不允许 `/` 与 `..`：项目名会参与路径拼接与 docker compose 的项目名，
// 是这条链路上唯一的外部输入。
func validComposeName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return !strings.Contains(name, "..")
}

// ComposeProjectFile 解析项目名到 yml 路径，并做边界复核。
func (m *Manager) ComposeProjectFile(name string) (string, error) {
	if !validComposeName(name) {
		return "", fmt.Errorf("项目名不合法（只能用字母、数字、. _ -，且不能用 .. ）: %q", name)
	}
	root := m.ComposeRoot()
	file := filepath.Join(root, name, "docker-compose.yml")

	// 边界复核：Clean 之后必须仍在 compose 根目录下。
	// 名字校验已经拦住 `..`，这里是第二道防线 —— 路径校验写两遍不嫌多。
	cleanRoot := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(file), cleanRoot) {
		return "", fmt.Errorf("项目路径越界: %s", file)
	}
	return file, nil
}

// DockerComposeProjects 列出 compose 根目录下的所有项目。
func (m *Manager) DockerComposeProjects(ctx context.Context) ([]DockerComposeProject, error) {
	root := m.ComposeRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []DockerComposeProject{}, nil
		}
		return nil, fmt.Errorf("读取 compose 目录失败: %w", err)
	}

	// 已登记的服务名，用来标记 registered
	registered := map[string]bool{}
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.Kind == KindCompose {
					registered[s.Name] = true
				}
			}
		}
	}

	out := []DockerComposeProject{}
	for _, e := range entries {
		if !e.IsDir() || !validComposeName(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		file := filepath.Join(dir, "docker-compose.yml")
		p := DockerComposeProject{
			Name: e.Name(), Dir: dir, File: file,
			Registered: registered[e.Name()], ServiceName: e.Name(),
		}
		if st, err := os.Stat(file); err == nil {
			p.Exists = true
			p.Size = st.Size()
			p.UpdatedAt = st.ModTime().Format("2006-01-02 15:04:05")
		}
		out = append(out, p)
	}
	return out, nil
}

// DockerComposeRead 读取某个项目的 compose 文件内容。
func (m *Manager) DockerComposeRead(name string) (string, error) {
	file, err := m.ComposeProjectFile(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // 新项目：返回空内容，前端显示空白编辑器
		}
		return "", fmt.Errorf("读取 compose 文件失败: %w", err)
	}
	return string(b), nil
}

// DockerComposeSave 写入 compose 文件（原子替换），并可选登记为受管服务。
func (m *Manager) DockerComposeSave(ctx context.Context, name, content string, register bool) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("compose 内容不能为空")
	}
	file, err := m.ComposeProjectFile(name)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建项目目录失败: %w", err)
	}
	if err := chownTree(m.opt.UserName, dir); err != nil {
		dockerLog.Warn("设置 compose 目录归属失败: %v", err)
	}

	// 先写临时文件再 rename：避免 compose 读到半个文件
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("写入 compose 文件失败: %w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("替换 compose 文件失败: %w", err)
	}
	if err := chownTree(m.opt.UserName, file); err != nil {
		dockerLog.Warn("设置 compose 文件归属失败: %v", err)
	}

	msg := "已保存 " + file
	if register {
		if err := m.RegisterComposeProject(ctx, name, file); err != nil {
			msg += "（登记到服务管理失败：" + err.Error() + "）"
		} else {
			msg += "，并已登记到服务管理"
		}
	}
	return msg, nil
}

// DockerComposeDeleteProject 删除整个 compose 项目目录。
//
// 会先 down 再删目录：直接删目录会留下"孤儿容器"，用户以为删掉了，
// 容器还在后台跑着占端口。
func (m *Manager) DockerComposeDeleteProject(ctx context.Context, name string, removeVolumes bool) error {
	if !validComposeName(name) {
		return fmt.Errorf("项目名不合法: %q", name)
	}
	file, err := m.ComposeProjectFile(name)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(file); statErr == nil {
		if _, downErr := m.DockerComposeAction(ctx, name, "down", removeVolumes); downErr != nil {
			dockerLog.Warn("删除项目前 down 失败（继续删目录）: %v", downErr)
		}
	}
	if err := os.RemoveAll(filepath.Dir(file)); err != nil {
		return fmt.Errorf("删除项目目录失败: %w", err)
	}
	// 已经从磁盘消失，服务管理里那条记录也不该再留着
	if m.repo != nil {
		if svc, err := m.repo.Get(ctx, name); err == nil && svc.Kind == KindCompose {
			if err := m.repo.Delete(ctx, svc.Name); err != nil {
				dockerLog.Warn("移除 compose 服务记录失败: %v", err)
			}
		}
	}
	return nil
}

// DockerComposeAction 对一个已存在的项目执行 up / down / restart。
//
// 复用 composeDriver 的执行逻辑（它会处理 docker compose vs docker-compose、
// DOCKER_HOST 注入、超时），这里只是把它"借"出来给 Docker 页用。
func (m *Manager) DockerComposeAction(ctx context.Context, name, action string, removeVolumes bool) (string, error) {
	file, err := m.ComposeProjectFile(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(file); err != nil {
		return "", fmt.Errorf("compose 文件不存在: %s", file)
	}

	d := &composeDriver{opt: m.opt, svc: &Service{
		Name: name, Kind: KindCompose, ComposeFile: file,
	}}

	switch action {
	case "up":
		// --remove-orphans：yml 里删掉的服务也一起收掉，否则旧容器会一直占着端口
		return d.run(ctx, 10*time.Minute, "up", "-d", "--remove-orphans")
	case "down":
		if removeVolumes {
			return d.run(ctx, 5*time.Minute, "down", "--remove-orphans", "-v")
		}
		return d.run(ctx, 5*time.Minute, "down", "--remove-orphans")
	case "restart":
		return d.run(ctx, 5*time.Minute, "restart")
	case "ps":
		return d.run(ctx, 30*time.Second, "ps")
	default:
		return "", fmt.Errorf("不支持的 compose 操作: %s", action)
	}
}

// RegisterComposeProject 把一个 compose 项目登记进服务管理。
func (m *Manager) RegisterComposeProject(ctx context.Context, name, file string) error {
	if m.repo == nil {
		return fmt.Errorf("服务仓库不可用")
	}
	// 已存在就不重复登记（同 RegisterInstalledService 的教训：登记两次会出幽灵服务）
	if list, err := m.repo.List(ctx); err == nil {
		for _, s := range list {
			if s.Name == name {
				return nil
			}
		}
	}
	port := 0
	// 从 yml 里尽力猜一个对外端口：用于服务列表显示访问地址。
	if b, err := os.ReadFile(file); err == nil {
		port = guessComposePort(string(b))
	}
	svc := &Service{
		Name: name, DisplayName: name, Kind: KindCompose, Category: "docker",
		Icon: "🐳", Description: "由 Docker 页创建的 compose 项目",
		Port: port, ComposeFile: file, Managed: true,
	}
	return m.repo.Create(ctx, svc)
}

// guessComposePort 从 compose 内容里猜一个宿主端口。
//
// 只做**尽力而为**的文本匹配：目标是让服务列表能显示"通常在哪个端口"，
// 猜不到就返回 0（前端显示 —）。真正的状态判定始终以 compose ps 为准，
// 所以这里宁可漏也不要错。
//
// 端口写法的规律是「最后一段 = 容器端口，倒数第二段 = 宿主端口」。
// 第一版按"第一段"取，于是三段式 127.0.0.1:8000:80 整个猜不到 —— 而 Docker 页
// 创建容器的表单恰好支持三段式，两处口径不一致会让人以为面板猜错了端口。
func guessComposePort(content string) int {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-") {
			continue
		}
		spec := strings.TrimSpace(strings.TrimPrefix(line, "-"))
		spec = strings.Trim(spec, `"'`)

		// 去掉协议后缀：8080:80/udp
		if i := strings.LastIndex(spec, "/"); i >= 0 {
			spec = spec[:i]
		}
		parts := strings.Split(spec, ":")
		if len(parts) < 2 {
			continue // 只写容器端口时由 Docker 随机分配，没有"通常在哪个端口"可言
		}

		// 规律：最后一段 = 容器端口，倒数第二段 = 宿主端口。
		// 三段式 127.0.0.1:8000:80 因此取 8000 而不是 127.0.0.1。
		host := strings.TrimSpace(parts[len(parts)-2])
		if n, err := strconv.Atoi(host); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
//  给 Web 层的类型别名
//
//  这些结构体本可以在内部保持小写，但 Web 层要直接把它们序列化给前端。
//  与其在 web 包里重复声明一遍（两处字段名不一致时 JSON 会静默对不上），
//  不如在这里导出成别名，保证"面板返回的结构"与"驱动内部用的结构"永远是同一个。
// ---------------------------------------------------------------------------

type (
	// DockerContainerView 是容器列表条目。
	DockerContainerView = dockerContainer
	// DockerPortBind 是一条端口映射（创建容器时用）。
	DockerPortBind = dockerPortBind
	// DockerImageView 是镜像列表条目。
	DockerImageView = dockerImage
	// DockerVolumeView 是数据卷条目。
	DockerVolumeView = dockerVolume
	// DockerNetworkView 是网络条目。
	DockerNetworkView = dockerNetwork
)
