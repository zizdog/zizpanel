package services

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"

	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
//  Docker 运行时的**真实**状态探测
//
//  为什么单独有这么一份探测：2026-09-19 用户实测报告
//
//	「当前 Docker socket 不存在（/var/run/docker.sock）：引擎没有在运行。
//	  因为没有安装 Docker 运行时（Colima）。但应用市场页面显示：
//	  Colima 已安装·未纳管」
//
//  根因不是显示层，而是**判据**：应用市场原来对容器运行时也用"plist 在不在
//  /Library/LaunchDaemons 里"来判定已安装。那是给 nginx/PHP/MySQL 这类**包在
//  磁盘上**的应用定的规矩（它们有 brew 前缀下的可执行文件 + 一份 plist，
//  plist 在就说明装着），可容器运行时的运行体是**一个虚拟机和它的 socket**：
//
//	· /Library/LaunchDaemons/com.zizdog.colima.plist 是旧版安装留下的**僵尸
//	  plist**（用户实际没装 colima、~/.colima 不存在、~/.colima/default 里没有
//	  docker.sock）；
//	· 于是市场卡片显示「已安装」，用户既看不到安装入口、也点不出引擎来。
//
//  所以容器运行时的"已安装"必须**看现实**：colima 二进制真的在不在。
//  残留的 plist / 配置目录只作为**残留**如实报出来，供界面给"重新安装 / 清理"。
//
//  三态（这是本文件唯一的对外语义）：
//
//	not-installed —— colima 二进制不在（哪怕磁盘上有僵尸 plist）
//	stopped       —— 二进制在，但 Docker API 连不上（引擎没跑）
//	running       —— 二进制在且 Docker API 有响应
//
//  **只做连接/stat 探测，绝不起容器、绝不跑 colima 命令。**
//  单测一律通过注入点给结论（见 services.go 的 dockerBinProbeOverride /
//  dockerVersionOverride 与 web 层的 dockerRuntimeProbeOverride）——
//  单测不碰真实服务、更不该真去跑 colima（AGENTS.md 第三节）。
// ============================================================================

// Docker 运行时三态：状态字符串（给界面与测试做稳定契约）
const (
	DockerRuntimeNotInstalled = "not-installed"
	DockerRuntimeStopped      = "stopped"
	DockerRuntimeRunning      = "running"
)

// DockerRuntimeState 是一次真实探测的结论。
//
// 字段全部**如实**：拿不到就是零值/未知，绝不猜（AGENTS.md 铁律 11）。
type DockerRuntimeState struct {
	// BinaryInstalled 是"colima 可执行文件真的在"。这是"已安装"的唯一判据。
	BinaryInstalled bool `json:"binary_installed"`
	// BinPath 是探测到的 colima 路径（没装则空）。
	BinPath string `json:"bin_path,omitempty"`
	// SocketPath 是磁盘上**存在**的 docker socket（没找到则空）。
	// 注意：socket 文件在 ≠ 引擎在跑（虚拟机崩了会留下残留 socket）。
	SocketPath string `json:"socket_path,omitempty"`
	// SocketReachable 是"这个 socket 上真的能问到 Docker API"。
	SocketReachable bool `json:"socket_reachable"`
	// EngineVersion 是 Docker API 自报的版本；拿不到就是空（未知，不是猜的）。
	EngineVersion string `json:"engine_version,omitempty"`
	// VMState 是 colima 虚拟机的状态文案（没有实例时说明"未创建"）。
	VMState string `json:"vm_state,omitempty"`
	// State 是三态之一：not-installed / stopped / running。
	State string `json:"state"`
	// PlistExists 是开机自启 plist 是否还在（/Library/LaunchDaemons/com.zizdog.colima.plist）。
	PlistExists bool `json:"plist_exists"`
	// Artifacts 表示"二进制不在、但磁盘上还有上一轮安装的残留"（僵尸 plist
	// 或 ~/.colima 配置目录）。界面据此给「重新安装 / 清理残留」，而不是"已安装"。
	Artifacts bool `json:"artifacts"`
	// Note 是一句人能看懂的状态说明（可直接显示）。
	Note string `json:"note,omitempty"`
}

// DockerRuntimeStatus 真实探测本机 Docker 运行时（Colima）的状态。
func (m *Manager) DockerRuntimeStatus(ctx context.Context) DockerRuntimeState {
	probe := m.commandProbe()
	if m.dockerBinProbeOverride != nil { // 单测注入点，见 services.go 的字段说明
		probe = m.dockerBinProbeOverride
	}
	return DiagnoseDockerRuntime(ctx, DockerRuntimeProbe{
		CommandProbe:     probe,
		DialVersion:      m.dockerVersionOverride,
		UserHome:         m.colimaHome(),
		ConfiguredSocket: m.opt.DockerSocket,
	})
}

// colimaHome 返回 Colima 家目录（socket 与 _lima 实例目录都在它下面）。
//
// 优先用配置里的真实用户家目录；它为空时不要直接退回 os.UserHomeDir() ——
// 面板以 root 运行时那是 /var/root，于是 ~/.colima/default/docker.sock 永远
// 找不到（表现为"没装 Colima"，其实只是找错了家目录）。currentUserHome 会
// 显式排除 /var/root，让这种情况如实退化成"未知"，而不是给一个错的结论。
func (m *Manager) colimaHome() string {
	if m.opt.UserHome != "" {
		return m.opt.UserHome
	}
	return currentUserHome()
}

// DockerRuntimeProbe 是探测所需的**全部**外部能力（可注入，便于单测）。
//
// 默认实现只读文件系统 + 连一次本地 unix socket，不执行任何命令。
type DockerRuntimeProbe struct {
	// CommandProbe 判断"某个命令在不在"（默认复用 basedep 的 commandProbe：
	// 先 PATH、再补看 Homebrew 前缀 —— 面板由 LaunchDaemon 以 root 启动，
	// PATH 里没有 /opt/homebrew/bin，不补看就会把装好的 colima 判成没装）。
	CommandProbe func(command string) (string, error)
	// DialVersion 从 socket 问一次 Docker 引擎版本。
	// nil = 用真实实现；返回 "" 表示连不上/拿不到（如实当未知）。
	DialVersion func(ctx context.Context, sock string) string
	// UserHome 是真实用户家目录（Colima 的 socket 在 <home>/.colima/default/）。
	// 为空时退回 os.UserHomeDir()（测试里家目录是隔离的临时目录）。
	UserHome string
	// ConfiguredSocket 是面板配置里显式指定的 socket（Cfg.DockerSocket，可空）。
	// 非空时它是**首选**候选 —— 部署时改过 socket 位置的机器不能被忽略。
	ConfiguredSocket string
}

// DiagnoseDockerRuntime 是 DockerRuntimeStatus 的可注入实现。
func DiagnoseDockerRuntime(ctx context.Context, p DockerRuntimeProbe) DockerRuntimeState {
	home := p.UserHome
	if home == "" {
		home = currentUserHome()
	}

	state := DockerRuntimeState{}

	// ---- 1. 二进制：唯一的"已安装"判据 ----
	if p.CommandProbe != nil {
		if bin, err := p.CommandProbe("colima"); err == nil && bin != "" {
			state.BinaryInstalled = true
			state.BinPath = bin
		}
	}

	// ---- 2. socket：先看磁盘上有没有，再问它能不能应答 ----
	state.SocketPath = firstExistingSocket(home, p.ConfiguredSocket)

	// ---- 3. 虚拟机状态：读 Lima 实例目录（毫秒级，不跑 colima） ----
	// LIMA_HOME 可覆盖；Colima 默认用 ~/.colima/_lima。
	base := os.Getenv("LIMA_HOME")
	if base == "" && home != "" {
		base = filepath.Join(home, ".colima", "_lima")
	}
	if base != "" {
		if _, detail, found := readColimaFastState(base); found {
			state.VMState = detail
		} else {
			state.VMState = "未创建虚拟机实例"
		}
	} else {
		state.VMState = "未知（拿不到家目录）"
	}

	// ---- 4. 引擎版本：只有真的问到了才给，问到才叫"在跑" ----
	if state.SocketPath != "" {
		dial := p.DialVersion
		if dial == nil {
			dial = dockerVersionViaSocket
		}
		if ver := dial(ctx, state.SocketPath); ver != "" {
			state.EngineVersion = ver
			state.SocketReachable = true
		}
	}

	// ---- 5. 三态 ----
	switch {
	case !state.BinaryInstalled:
		state.State = DockerRuntimeNotInstalled
		if state.SocketReachable {
			// 装了别的运行时（Docker Desktop / OrbStack / 自己起的 dockerd）：
			// 二进制不在不等于引擎不在，如实说出来，但运行时装没装仍是"没装"。
			state.Note = "未安装 Colima（但检测到可用的 Docker socket：引擎可能是别的运行时提供的）"
		} else {
			state.Note = "未安装 Docker 运行时（Colima）"
		}
	case state.SocketReachable:
		state.State = DockerRuntimeRunning
		state.Note = "Docker 运行时已就绪"
	default:
		state.State = DockerRuntimeStopped
		state.Note = "已安装但引擎没在运行（可在 Docker 页点启动）"
	}

	// ---- 6. 残留：二进制不在，但磁盘上留着上一轮安装的东西 ----
	plist := fileExists(ColimaPlistPath)
	colimaDir := home != "" && dirExists(filepath.Join(home, ".colima"))
	state.PlistExists = plist
	state.Artifacts = !state.BinaryInstalled && (plist || colimaDir)
	if state.Artifacts {
		state.Note += "；检测到上一次安装的残留（开机自启配置或配置目录），可重新安装覆盖"
	}
	return state
}

// firstExistingSocket 返回磁盘上**存在**的第一个 docker socket 路径。
//
// 候选顺序（与 detectDocker 的口径一致）：
//
//  1. 面板配置里的 Cfg.DockerSocket（部署时可改；空则跳过）
//  2. ~/.colima/default/docker.sock（Colima 默认 profile）
//  3. /var/run/docker.sock（系统级 socket 的标准位置）
//
// 只 stat，不连接 —— 连接由调用方决定（单测用注入的 DialVersion 替代）。
func firstExistingSocket(home string, configured ...string) string {
	var cands []string
	for _, c := range configured {
		if strings.TrimSpace(c) != "" {
			cands = append(cands, c)
		}
	}
	if home != "" {
		cands = append(cands, filepath.Join(home, ".colima", "default", "docker.sock"))
	}
	cands = append(cands, "/var/run/docker.sock")
	for _, c := range cands {
		if isSocketFile(c) {
			return c
		}
	}
	return ""
}

// isSocketFile 判断路径是不是一个真实存在的 unix socket。
//
// 必须查 ModeSocket：残留的普通文件/目录不能冒充 socket。
func isSocketFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// dockerVersionViaSocket 连一次 Docker API 问引擎版本。
//
// 只发一个 `GET /version`：不列容器、不动任何状态。超时给 1.5 秒 ——
// 本地 unix socket 是毫秒级的，1.5 秒还问不到就说明它其实没在跑。
// 返回空字符串表示**未知**（调用方据此判"引擎没在跑"），不返回假版本号。
func dockerVersionViaSocket(ctx context.Context, sock string) string {
	if sock == "" {
		return ""
	}
	client := &http.Client{
		Timeout: 1500 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return ""
	}
	var v struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ""
	}
	return strings.TrimSpace(v.Version)
}

// readColimaFastState 是 colimaFastState 的纯函数版本（给定 Lima 实例根目录）。
//
// 抽出来是为了让 DockerRuntimeStatus 也能复用同一套判据（pid 活着 **且**
// ssh.sock 在），而不是把 ~/.colima/_lima 的目录布局再抄一遍 ——
// 抄两份判据必然有一天会分叉。
func readColimaFastState(base string) (running bool, detail string, found bool) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return false, "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() != "colima" && !strings.HasPrefix(e.Name(), "colima-") {
			continue
		}
		dir := filepath.Join(base, e.Name())
		alive := false
		if b, err := os.ReadFile(filepath.Join(dir, "ha.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				alive = processAlive(pid)
			}
		}
		sockOK := isSocketFile(filepath.Join(dir, "ssh.sock"))
		if alive && sockOK {
			return true, "虚拟机运行中（实例 " + e.Name() + "）", true
		}
		return false, "虚拟机未运行（实例 " + e.Name() + "）", true
	}
	return false, "", false
}

// currentUserHome 解析真实用户家目录（拿不到就是空，调用方按"未知"处理）。
func currentUserHome() string {
	if h := os.Getenv("HOME"); h != "" && h != "/var/root" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" && u.HomeDir != "/var/root" {
		return u.HomeDir
	}
	return ""
}

// 目录判据用 docker_mirror.go 里已有的 dirExists（同一个包，避免两份定义分叉）。
