// Package services 实现网站与应用的统一服务管理。
//
// 核心抽象：
//
//	Service（一条注册记录）
//	  └── Driver（执行者）
//	        ├── NativeDriver  launchd / brew services（裸装）
//	        └── DockerDriver  Docker 容器（可选，装了 Docker 才有）
//
// 设计要点：
//
//  1. 面板把服务分成两类：**纳管（adopt）** 与 **托管（managed）**。
//     纳管 = 你原来就装好的服务（比如 com.zizdog.qwen3tts），面板只做启停/日志/健康检查；
//     托管 = 面板通过应用市场安装的服务，面板负责完整生命周期（含卸载）。
//     这个区分很重要：对纳管服务做"卸载"会把用户自己的东西删掉。
//
//  2. 「运行状态」永远以系统真实状态为准，不信任数据库里的 enabled 字段。
//     数据库只记录"这个服务是什么、怎么管"，状态每次实时查询。
//
//  3. 日志读取走 tail 语义（读末尾 N 行），而不是把整个文件加载进内存 ——
//     服务日志动辄几百 MB，全量读会拖死面板。
package services

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Kind 是服务的类型。
type Kind string

const (
	// KindNative 系统原生服务：由 launchd 托管（launchctl / brew services）。
	KindNative Kind = "native"
	// KindDocker 单个 Docker 容器。
	KindDocker Kind = "docker"
	// KindCompose 由 docker compose 管理的一组容器。
	KindCompose Kind = "compose"
	// KindColima 是容器运行时本身（Colima 起的 Linux 虚拟机）。
	// 它必须独立于 KindDocker：Docker 引擎挂掉时，其它 docker 驱动都会因为
	// "Docker 不可用"而构造失败，而这恰恰是最需要能操作运行时的时刻。
	KindColima Kind = "colima"
)

// Service 是一条服务注册记录。
type Service struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        Kind   `json:"kind"`
	Category    string `json:"category"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Port        int    `json:"port"`

	// native
	LaunchLabel string `json:"launch_label"`
	PlistPath   string `json:"plist_path"`
	WorkDir     string `json:"work_dir"`
	StartCmd    string `json:"start_cmd"`

	// docker / compose
	Container   string `json:"container"`
	ComposeFile string `json:"compose_file"`
	Image       string `json:"image"`

	// 通用
	HealthURL    string `json:"health_url"`
	HealthExpect string `json:"health_expect"`
	LogPath      string `json:"log_path"`
	Autostart    bool   `json:"autostart"`
	Enabled      bool   `json:"enabled"`
	Managed      bool   `json:"managed"` // true=面板安装（可卸载），false=仅纳管

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// State 是服务的实时运行状态。
type State struct {
	Running bool   `json:"running"`
	Status  string `json:"status"` // running / stopped / error / unknown / not-installed
	PID     int    `json:"pid"`
	Detail  string `json:"detail"`
	// ExitCode 仅在 launchd 报告异常时有效
	ExitCode int `json:"exit_code"`
	// Endpoint 实际访问地址（若已知）
	Endpoint string `json:"endpoint"`
}

// Health 是健康检查结果。
type Health struct {
	Checked   bool   `json:"checked"`
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	Code      int    `json:"code"`
	Latency   int64  `json:"latency_ms"`
	Message   string `json:"message"`
	CheckedAt string `json:"checked_at"`
}

// Driver 是服务操作接口。所有实现都必须满足：
//   - 幂等：重复 start 已运行的服务不报错
//   - 不阻塞：调用方给出 ctx，实现必须尊重超时
//   - 状态真实：Status 必须查询系统，不读缓存
type Driver interface {
	// Kind 返回驱动类型。
	Kind() Kind
	// Status 查询真实运行状态。
	Status(ctx context.Context) (State, error)
	// Start 启动服务。
	Start(ctx context.Context) error
	// Stop 停止服务。
	Stop(ctx context.Context) error
	// Restart 重启服务。
	Restart(ctx context.Context) error
	// Logs 读取末尾 lines 行日志。follow 为 true 时返回一个持续输出的通道。
	Logs(ctx context.Context, lines int) (string, error)
	// LogStream 返回持续输出日志的通道（用于 SSE）。
	LogStream(ctx context.Context) (<-chan string, error)
	// Health 做一次健康检查。
	Health(ctx context.Context) Health
	// Uninstall 移除服务（仅对托管服务调用）。
	Uninstall(ctx context.Context) error
}

// Manager 是服务管理器。
type Manager struct {
	repo *Repository
	opt  Options
	// qwenPortOverride 仅供测试：把模型状态查询指向一个指定端口。
	// 没有它的话，单元测试会打到本机真实运行的 Qwen 服务上（8880）——
	// 实测就这么发生过：断言"服务不可用时应降级"的测试，因为本机恰好
	// 有另一个项目在跑 Qwen 而失败，测出来的根本不是被测代码的行为。
	qwenPortOverride int
	// qwenMemGBOverride 仅供测试：伪造物理内存 GB（0 = 用真机值）。
	// 守温策略是内存感知的（16GB 只常驻一个 1.7B 模型），而真机内存是固定的，
	// 没有它就没法在同一个测试进程里同时断言"宽裕"和"吃紧"两种情形。
	qwenMemGBOverride int
}

// Options 是管理器需要的环境信息。
type Options struct {
	// HelperBin 是提权助手路径（用于需要 root 的操作）
	HelperBin string
	// BrewBin 是 brew 路径（brew services 类服务需要）
	BrewBin string
	// DockerSocket 是 Docker socket 路径，为空表示 Docker 不可用
	DockerSocket string
	// UserHome 是真实用户家目录（用于找 LaunchAgents 与日志）
	UserHome string
	// UserName 是真实用户名
	UserName string
	// UID 是真实用户 uid（launchd gui/<uid> 域需要）
	UID int
	// WorkDir 是面板工作目录（compose 文件放在 <WorkDir>/compose 下）
	WorkDir string
}

// NewManager 创建服务管理器。
func NewManager(repo *Repository, opt Options) *Manager {
	return &Manager{repo: repo, opt: opt}
}

// DriverFor 为一条服务记录构造对应的驱动。
func (m *Manager) DriverFor(s *Service) (Driver, error) {
	switch s.Kind {
	case KindNative:
		// 有 launchd 标签/plist 的用 launchd 驱动；
		// 只有启动命令的用命令驱动（进程随面板退出，界面上会标注）
		if s.LaunchLabel != "" || s.PlistPath != "" {
			return newNativeDriver(m.opt, s), nil
		}
		if strings.TrimSpace(s.StartCmd) != "" {
			return newCommandDriver(m.opt, s), nil
		}
		return newNativeDriver(m.opt, s), nil
	case KindDocker:
		if m.opt.DockerSocket == "" {
			return nil, fmt.Errorf("Docker 不可用：未检测到 %s", "/var/run/docker.sock")
		}
		return newDockerDriver(m.opt, s), nil
	case KindCompose:
		if m.opt.DockerSocket == "" {
			return nil, fmt.Errorf("Docker 不可用，无法管理 compose 项目")
		}
		return newComposeDriver(m.opt, s), nil
	case KindColima:
		// 刻意不检查 DockerSocket：运行时停着的时候 socket 自然不存在，
		// 而这正是用户最需要把它启动起来的场景。
		return newColimaDriver(m.opt, s), nil
	default:
		return nil, fmt.Errorf("未知的服务类型: %s", s.Kind)
	}
}

// View 是返回给前端的服务视图（注册信息 + 实时状态 + 健康）。
type View struct {
	*Service
	State  State  `json:"state"`
	Health Health `json:"health"`
	// DriverReady 表示当前环境能否管理该服务（如 Docker 未装时为 false）
	DriverReady bool   `json:"driver_ready"`
	DriverError string `json:"driver_error,omitempty"`
}

// List 返回所有服务及其实时状态。
//
// 状态查询是并发进行的：每个服务都要 fork 一次 launchctl/ps，
// 串行查询在服务多时会让页面明显变慢。
func (m *Manager) List(ctx context.Context, withHealth bool) ([]*View, error) {
	all, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]*View, len(all))
	type result struct {
		idx    int
		state  State
		health Health
		ready  bool
		derr   string
	}
	ch := make(chan result, len(all))

	for i, s := range all {
		go func(i int, s *Service) {
			r := result{idx: i, state: State{Status: "unknown"}}
			drv, err := m.DriverFor(s)
			if err != nil {
				r.derr = err.Error()
				r.state.Status = "unavailable"
				ch <- r
				return
			}
			r.ready = true
			// 状态查询单独限时，避免某个服务卡住整个列表
			sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			st, err := drv.Status(sctx)
			if err != nil {
				r.state = State{Status: "error", Detail: err.Error()}
			} else {
				r.state = st
			}
			if withHealth && s.HealthURL != "" {
				hctx, hcancel := context.WithTimeout(ctx, 8*time.Second)
				defer hcancel()
				r.health = drv.Health(hctx)
			}
			ch <- r
		}(i, s)
	}

	for range all {
		r := <-ch
		views[r.idx] = &View{
			Service:     all[r.idx],
			State:       r.state,
			Health:      r.health,
			DriverReady: r.ready,
			DriverError: r.derr,
		}
	}
	return views, nil
}

// Get 返回单个服务的视图。
func (m *Manager) Get(ctx context.Context, name string) (*View, error) {
	s, err := m.repo.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	v := &View{Service: s, State: State{Status: "unknown"}}
	drv, err := m.DriverFor(s)
	if err != nil {
		v.DriverError = err.Error()
		v.State.Status = "unavailable"
		return v, nil
	}
	v.DriverReady = true
	st, err := drv.Status(ctx)
	if err != nil {
		v.State = State{Status: "error", Detail: err.Error()}
	} else {
		v.State = st
	}
	if s.HealthURL != "" {
		v.Health = drv.Health(ctx)
	}
	return v, nil
}

// driver 是内部辅助：取驱动或返回错误。
func (m *Manager) driver(ctx context.Context, name string) (Driver, *Service, error) {
	s, err := m.repo.Get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	drv, err := m.DriverFor(s)
	if err != nil {
		return nil, s, err
	}
	return drv, s, nil
}

// Action 执行一次服务操作（start/stop/restart）。
//
// 操作后不立刻返回状态：服务启动需要时间，
// 这里等待一小段并轮询，尽量让前端拿到"操作后"的真实状态。
func (m *Manager) Action(ctx context.Context, name, action string) (State, error) {
	drv, s, err := m.driver(ctx, name)
	if err != nil {
		return State{Status: "error", Detail: err.Error()}, err
	}
	switch action {
	case "start":
		err = drv.Start(ctx)
	case "stop":
		err = drv.Stop(ctx)
	case "restart":
		err = drv.Restart(ctx)
	default:
		return State{}, fmt.Errorf("不支持的操作: %s", action)
	}
	if err != nil {
		return State{Status: "error", Detail: err.Error()}, err
	}
	// 等待状态稳定（最多 8 秒）
	want := action != "stop"
	for i := 0; i < 16; i++ {
		time.Sleep(500 * time.Millisecond)
		st, serr := drv.Status(ctx)
		if serr == nil && st.Running == want {
			return st, nil
		}
	}
	st, _ := drv.Status(ctx)
	_ = s
	return st, nil
}

// Logs 读取服务日志。
func (m *Manager) Logs(ctx context.Context, name string, lines int) (string, error) {
	drv, _, err := m.driver(ctx, name)
	if err != nil {
		return "", err
	}
	return drv.Logs(ctx, lines)
}

// LogStream 订阅服务日志。
func (m *Manager) LogStream(ctx context.Context, name string) (<-chan string, error) {
	drv, _, err := m.driver(ctx, name)
	if err != nil {
		return nil, err
	}
	return drv.LogStream(ctx)
}

// Uninstall 卸载一个托管服务。纳管服务拒绝卸载。
func (m *Manager) Uninstall(ctx context.Context, name string) error {
	drv, s, err := m.driver(ctx, name)
	if err != nil {
		return err
	}
	if !s.Managed {
		return fmt.Errorf("「%s」是纳管服务（由你自己安装），面板不会卸载它。"+
			"如需停止请用「停止」，如需从列表移除请用「取消纳管」", s.DisplayName)
	}
	if err := drv.Uninstall(ctx); err != nil {
		return err
	}
	return m.repo.Delete(ctx, name)
}

// Adopt 纳管一个已存在的服务（写入注册表，不做任何系统改动）。
func (m *Manager) Adopt(ctx context.Context, s *Service) error {
	s.Managed = false
	return m.repo.Create(ctx, s)
}

// ForgetName 从注册表移除（不触碰系统）。
func (m *Manager) Forget(ctx context.Context, name string) error {
	return m.repo.Delete(ctx, name)
}

// ---------- 工具 ----------

// NormalizeName 把展示名规范成服务标识（小写字母数字与连字符）。
func NormalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '.':
			b.WriteRune('-')
		case r == '-':
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}
