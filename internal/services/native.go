package services

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// nativeDriver 管理由 launchd 托管的裸装服务。
//
// 为什么用 launchd 而不是 nohup/screen：
//   - 开机自启、崩溃自动拉起（KeepAlive）由系统保证，不依赖面板活着
//   - 面板挂掉时服务不受影响；服务挂掉时面板能看到退出码
//   - 不用自己实现进程守护与 pid 管理
//
// 关于权限：launchctl 对 LaunchDaemon（system 域）的操作需要 root，
// 因此面板必须能提权。这里复用与 nginx 相同的机制：面板以 root 运行，
// 或通过 sudoers 白名单调用 helper。
type nativeDriver struct {
	opt Options
	svc *Service
}

func newNativeDriver(opt Options, s *Service) *nativeDriver {
	return &nativeDriver{opt: opt, svc: s}
}

func (d *nativeDriver) Kind() Kind { return KindNative }

// plistPath 定位 plist 文件。
//
// 查找顺序：注册表里显式记录的路径 → 系统 LaunchDaemons → 用户 LaunchAgents。
// 显式记录优先，因为它能覆盖非标准位置。
func (d *nativeDriver) plistPath() (string, string) {
	if d.svc.PlistPath != "" {
		if _, err := os.Stat(d.svc.PlistPath); err == nil {
			if strings.Contains(d.svc.PlistPath, "/Library/LaunchDaemons/") {
				return d.svc.PlistPath, "system"
			}
			return d.svc.PlistPath, fmt.Sprintf("gui/%d", d.opt.UID)
		}
	}
	label := d.svc.LaunchLabel
	if label == "" {
		return "", ""
	}
	sys := filepath.Join("/Library/LaunchDaemons", label+".plist")
	if _, err := os.Stat(sys); err == nil {
		return sys, "system"
	}
	if d.opt.UserHome != "" {
		agent := filepath.Join(d.opt.UserHome, "Library", "LaunchAgents", label+".plist")
		if _, err := os.Stat(agent); err == nil {
			return agent, fmt.Sprintf("gui/%d", d.opt.UID)
		}
	}
	return "", ""
}

// Status 查询 launchd 报告的真实状态。
func (d *nativeDriver) Status(ctx context.Context) (State, error) {
	// 端口检查放在最前面：**端口在监听是"服务真的活着"最强的证据**，
	// 而且它不受 launchd 状态字符串、历史退出码、label 校验的影响。
	// 实测价值：php-fpm 报 `state = spawn scheduled`（没有 pid 行）
	// 且带历史退出码 78；mysql 的 label 含 @ 曾直接校验失败。
	// 这两种情况都会把好好的服务显示成"异常"，而端口检查都能救回来。
	if d.svc.Port > 0 && portListening(ctx, d.svc.Port) {
		return State{
			Status:   "running",
			Running:  true,
			Detail:   fmt.Sprintf("端口 %d 正在监听", d.svc.Port),
			Endpoint: d.endpoint(),
		}, nil
	}

	label := d.svc.LaunchLabel
	if label == "" {
		// 没有 launchd 标签：退化为按端口判断
		return d.statusByPort(ctx)
	}
	plist, domain := d.plistPath()
	if plist == "" {
		return State{Status: "not-installed", Detail: "找不到 plist 文件（服务可能已被移除）"}, nil
	}

	st, err := priv.LaunchStatus(label)
	if err != nil {
		return State{Status: "error", Detail: err.Error()}, nil
	}
	out := State{PID: st.PID, ExitCode: st.ExitCode}

	switch {
	case st.Running:
		out.Running = true
		out.Status = "running"
		out.Detail = fmt.Sprintf("由 launchd 托管（%s）", domain)
	case st.Loaded:
		// 已加载但没在跑。带退出码只说明"上一次退出过"，
		// 不代表现在出错 —— 所以报 stopped 并把退出码写进说明，
		// 不再升级成 error。真正的 error 留给"进程在但端口不通"这类明确故障。
		out.Status = "stopped"
		if st.ExitCode > 0 {
			out.Detail = fmt.Sprintf("已加载但未运行（上次退出码 %d）", st.ExitCode)
		} else {
			out.Detail = "已加载但未运行"
		}
		if d.svc.Port > 0 {
			out.Detail += fmt.Sprintf("，端口 %d 未监听", d.svc.Port)
		}
	default:
		out.Status = "stopped"
		out.Detail = "launchd 未加载该服务"
	}
	out.Endpoint = d.endpoint()
	return out, nil
}

// endpoint 拼出服务的访问地址（仅用于展示与提示）。
func (d *nativeDriver) endpoint() string {
	if d.svc.Port <= 0 {
		return ""
	}
	scheme := "http"
	if d.svc.Port == 443 {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, d.svc.Port)
}

// statusByPort 在缺少 launchd 标签时，用端口占用 + 命令匹配推断状态。
func (d *nativeDriver) statusByPort(ctx context.Context) (State, error) {
	if d.svc.Port <= 0 {
		return State{Status: "unknown", Detail: "既没有 launchd 标签，也没有端口信息，无法判断状态"}, nil
	}
	info, err := priv.CheckPort(strconv.Itoa(d.svc.Port))
	if err != nil {
		return State{Status: "unknown", Detail: err.Error()}, nil
	}
	st := State{Endpoint: d.endpoint()}
	if info.InUse {
		st.Running = true
		st.Status = "running"
		st.Detail = "端口 " + strconv.Itoa(d.svc.Port) + " 已被占用：" + strings.Join(info.Holders, ", ")
	} else {
		st.Status = "stopped"
		st.Detail = fmt.Sprintf("端口 %d 未被监听", d.svc.Port)
	}
	return st, nil
}

// Start 启动服务。
func (d *nativeDriver) Start(ctx context.Context) error {
	plist, _ := d.plistPath()
	if plist == "" {
		// Homebrew 服务：plist 由 `brew services start` 生成。
		// 只装了包、从没启动过服务时（ollama 就是这样），plist 并不存在，
		// 直接报"找不到 plist"用户完全不知道下一步该干什么（真实反馈）。
		// 这里就近修：用 brew services 启动一次，plist 就有了。
		if formula, ok := brewFormulaFromLabel(d.svc.LaunchLabel); ok {
			// nativeDriver 只带 Options（没有 Manager），而 brew 调用在 Manager 上；
			// 用一个临时 Manager 即可 —— 它只用到 opt 里的 brew 路径与用户名。
			tmp := NewManager(nil, d.opt)
			if out, err := tmp.runAsUser(ctx, 2*time.Minute, brewPath(d.opt), "services", "start", formula); err != nil {
				return fmt.Errorf("找不到 %s 的 plist，且 `brew services start %s` 也失败了：%v（%s）",
					d.svc.LaunchLabel, formula, err, tailText(out, 300))
			}
			// brew 注册完再走一次正常路径
			if p2, _ := d.plistPath(); p2 != "" {
				if err := priv.LaunchEnsureRunning(d.svc.LaunchLabel); err == nil {
					return nil
				}
			}
			// 记录里的 label 可能是旧前缀（真机上 homebrew.mxcl.* 与 sh.brew.* 并存）。
			// brew 刚刚用**当前**前缀写了 plist —— 那就看"服务到底跑起来没有"，
			// 而不是死盯着旧 label 的 plist 文件：服务在跑就是成功。
			real := BrewLabelFor(d.opt.UserHome, formula)
			if real != "" && real != d.svc.LaunchLabel {
				if _, err := os.Stat(filepath.Join("/Library/LaunchDaemons", real+".plist")); err == nil {
					return nil
				}
				if _, err := os.Stat(filepath.Join(d.opt.UserHome, "Library", "LaunchAgents", real+".plist")); err == nil {
					return nil
				}
			}
			return fmt.Errorf("已执行 `brew services start %s`，但找不到它的 plist（试过 %s 与 %s）；"+
				"请确认这个 formula 是否支持 brew services",
				formula, d.svc.LaunchLabel, real)
		}
		return fmt.Errorf("找不到 %s 的 plist，无法启动", d.svc.LaunchLabel)
	}
	// 已在跑则空操作，未加载才 bootstrap，已加载没跑才 kickstart 一次（坑 225）。
	if err := priv.LaunchEnsureRunning(d.svc.LaunchLabel); err != nil {
		return err
	}
	return nil
}

// Stop 停止服务。
func (d *nativeDriver) Stop(ctx context.Context) error {
	if err := priv.LaunchUnload(d.svc.LaunchLabel); err != nil {
		return err
	}
	return nil
}

// Restart 重启服务。
//
// 只调一次 LaunchKickstart：LaunchLoad 对已加载的作业也会 kick，再叠一次就会落进
// launchd 的 10s 节流窗口 —— 实测把面板重启拖成 10.0s（坑 225）。
func (d *nativeDriver) Restart(ctx context.Context) error {
	return priv.LaunchKickstart(d.svc.LaunchLabel)
}

// logPaths 返回该服务的日志文件路径（可能多个）。
//
// launchd 的日志由 plist 里的 StandardOutPath / StandardErrorPath 指定，
// 所以优先从 plist 读取 —— 这样即使用户把日志写到别处也能正确读到。
func (d *nativeDriver) logPaths() []string {
	var paths []string
	if d.svc.LogPath != "" {
		paths = append(paths, d.svc.LogPath)
	}
	plist, _ := d.plistPath()
	if plist != "" {
		if out := plistString(plist, "StandardOutPath"); out != "" {
			paths = append(paths, out)
		}
		if errp := plistString(plist, "StandardErrorPath"); errp != "" && errp != d.svc.LogPath {
			paths = append(paths, errp)
		}
	}
	// 去重
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// plistString 从 plist 里读取一个字符串键。
// 用 PlistBuddy 而不是解析 XML：这是 macOS 自带的工具，行为稳定。
func plistString(path, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :"+key, path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Logs 读取日志末尾若干行。
//
// 多个日志文件时按"最后修改时间"取最新的那个作为主日志，
// 并在内容前标注来源 —— 用户最关心的是刚刚发生了什么。
func (d *nativeDriver) Logs(ctx context.Context, lines int) (string, error) {
	paths := d.logPaths()
	if len(paths) == 0 {
		return "", fmt.Errorf("该服务未配置日志路径，也没有从 plist 中读到 StandardOutPath/StandardErrorPath。"+
			"可以在服务编辑里手工填写日志文件路径（服务名：%s）", d.svc.Name)
	}
	if lines <= 0 {
		lines = 200
	}
	var parts []string
	for _, p := range paths {
		content, err := tailFile(p, lines)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			parts = append(parts, fmt.Sprintf("（读取 %s 失败：%v）", p, err))
			continue
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		if len(paths) > 1 {
			parts = append(parts, "===== "+p+" =====")
		}
		parts = append(parts, content)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n"), nil
}

// LogStream 持续输出日志（SSE 用）。
//
// 实现方式：每 500ms 检查文件是否增长，只推送新增内容。
// 为什么不用 `tail -f` 子进程：面板可能需要同时跟踪多个服务，
// 每个都起一个进程既浪费又容易留下孤儿进程；
// 轮询实现简单、可取消、天然支持日志轮转（文件被替换时从头读）。
func (d *nativeDriver) LogStream(ctx context.Context) (<-chan string, error) {
	paths := d.logPaths()
	if len(paths) == 0 {
		return nil, fmt.Errorf("该服务没有可跟踪的日志文件")
	}
	path := paths[0]
	// 取最后修改时间最新的那个文件
	var newest time.Time
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.ModTime().After(newest) {
			newest = st.ModTime()
			path = p
		}
	}

	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		var offset int64
		if st, err := os.Stat(path); err == nil {
			// 从末尾往前 8KB 开始，先给用户一点上下文
			offset = st.Size() - 8192
			if offset < 0 {
				offset = 0
			}
		}
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				st, err := os.Stat(path)
				if err != nil {
					continue
				}
				if st.Size() < offset {
					// 文件被截断或轮转，从头读
					offset = 0
				}
				if st.Size() == offset {
					continue
				}
				f, err := os.Open(path)
				if err != nil {
					continue
				}
				if _, err := f.Seek(offset, 0); err != nil {
					_ = f.Close()
					continue
				}
				buf := make([]byte, st.Size()-offset)
				n, _ := f.Read(buf)
				_ = f.Close()
				offset += int64(n)
				if n > 0 {
					select {
					case ch <- string(buf[:n]):
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch, nil
}

// Health 做一次 HTTP 健康检查。
func (d *nativeDriver) Health(ctx context.Context) Health {
	return httpHealth(ctx, d.svc)
}

// Uninstall 移除 launchd 服务。
//
// 只对托管服务调用（Manager 层已校验）。
// 顺序：先卸载再删 plist —— 反过来会留下一个无法卸载的孤儿服务。
func (d *nativeDriver) Uninstall(ctx context.Context) error {
	label := d.svc.LaunchLabel
	plist, _ := d.plistPath()
	if label != "" {
		// 卸载失败必须上报，不能吞掉：作业还挂在 launchd 里就把 plist 删了，
		// 结果是"服务还在跑、面板里却查不到它"，而且再也没法用 launchctl
		// 卸载（plist 已不存在）—— 只会留下一个无法管理的孤儿服务。
		if uerr := priv.LaunchUnload(label); uerr != nil {
			return fmt.Errorf("卸载 %s 失败: %w", label, uerr)
		}
	}
	if plist != "" {
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 plist 失败: %w", err)
		}
	}
	return nil
}

// ---------- 通用日志工具 ----------

// tailFile 读取文件末尾 n 行。
//
// 大文件优化：如果文件超过 1MB，只从末尾 512KB 处开始读。
// 服务日志经常涨到几百 MB，全量读会一次性占用大量内存。
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	const maxRead = 512 * 1024
	start := int64(0)
	if st.Size() > maxRead {
		start = st.Size() - maxRead
	}
	if _, err := f.Seek(start, 0); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		return "", err
	}
	lines := strings.Split(buf.String(), "\n")
	// 从中间开始读时，第一行可能是半截
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// portListening 判断本机端口是否有人在监听。
//
// 用它而不是 HTTP 健康检查：PHP-FPM 说的是 FastCGI、MySQL 说的是
// MySQL 二进制协议，它们**永远不可能通过 HTTP 检查**。
// 对这类服务，"端口可连"才是正确的存活证据。
func portListening(ctx context.Context, port int) bool {
	d := net.Dialer{Timeout: 1200 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// brewPath 返回 brew 可执行文件路径（macOS 上两种前缀都可能）。
func brewPath(opt Options) string {
	if opt.BrewBin != "" {
		return opt.BrewBin
	}
	for _, p := range []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "brew"
}

// brewFormulaFromLabel 从 launchd 标签推出 Homebrew formula 名。
//
// brew services 生成的标签形如 `homebrew.mxcl.ollama` / `sh.brew.mysql@8.4`，
// 前缀随 brew 版本与安装方式变化（真机上两种前缀并存过），所以按后缀取。
func brewFormulaFromLabel(label string) (string, bool) {
	for _, prefix := range []string{"homebrew.mxcl.", "sh.brew."} {
		if strings.HasPrefix(label, prefix) {
			f := strings.TrimPrefix(label, prefix)
			if f != "" {
				return f, true
			}
		}
	}
	return "", false
}
