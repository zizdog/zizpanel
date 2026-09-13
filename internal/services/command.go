package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// commandDriver 管理"没有 launchd、只有一条启动命令"的服务。
//
// 适用场景：用户手上有个脚本或可执行文件，想先在面板里跑起来看看效果，
// 还不想为它写 plist。
//
// 与 launchd 的取舍必须说清楚：
//   - launchd 由系统守护，开机自启、崩溃自动拉起，面板重启也不影响
//   - commandDriver 的进程是面板的子进程：**面板重启后这些服务会停止**
//     （面板退出时会主动优雅停止它们，避免留下孤儿进程）
//
// 因此界面上会把这类服务标注为"面板托管（随面板退出）"，
// 并建议需要长期运行的服务改用 launchd。
type commandDriver struct {
	opt Options
	svc *Service
	mu  sync.Mutex
	cmd *exec.Cmd
	pid int
	// logFile 是子进程输出重定向的文件
	logFile *os.File
}

func newCommandDriver(opt Options, s *Service) *commandDriver {
	return &commandDriver{opt: opt, svc: s}
}

func (d *commandDriver) Kind() Kind { return KindNative }

// LogPath 返回日志文件路径（用户没配时使用面板工作目录下的默认位置）。
func (d *commandDriver) LogPath() string {
	if d.svc.LogPath != "" {
		return expandHome(d.svc.LogPath, d.opt.UserHome)
	}
	base := d.opt.WorkDir
	if base == "" {
		base = "/opt/zizpanel/work"
	}
	return filepath.Join(base, "services", d.svc.Name+".log")
}

// pidFile 记录子进程 pid，使面板重启后仍能识别"这是我在管的服务"。
func (d *commandDriver) pidFile() string {
	base := d.opt.WorkDir
	if base == "" {
		base = "/opt/zizpanel/work"
	}
	return filepath.Join(base, "services", d.svc.Name+".pid")
}

func (d *commandDriver) Status(ctx context.Context) (State, error) {
	d.mu.Lock()
	pid := d.pid
	d.mu.Unlock()

	// 优先用内存里的进程；面板重启后回退到 pid 文件
	if pid == 0 {
		if b, err := os.ReadFile(d.pidFile()); err == nil {
			if n, e := parseInt(strings.TrimSpace(string(b))); e == nil {
				pid = n
			}
		}
	}

	st := State{Endpoint: d.endpoint()}
	if pid > 0 && processAlive(pid) {
		st.Running = true
		st.PID = pid
		st.Status = "running"
		st.Detail = "由面板托管的子进程运行中（面板重启后该进程会停止）"
		return st, nil
	}
	st.Status = "stopped"
	if pid > 0 {
		st.Detail = "进程已不存在（面板重启过，或进程异常退出）"
	} else {
		st.Detail = "未通过面板启动过"
	}
	return st, nil
}

func (d *commandDriver) endpoint() string {
	if d.svc.Port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", d.svc.Port)
}

// Start 启动命令。
//
// 输出（stdout+stderr）重定向到日志文件，与 launchd 的行为保持一致，
// 这样"看日志"功能对两种驱动是同一套体验。
func (d *commandDriver) Start(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.pid > 0 && processAlive(d.pid) {
		return nil // 幂等
	}
	if strings.TrimSpace(d.svc.StartCmd) == "" {
		return errors.New("该服务没有配置启动命令（StartCmd）")
	}

	logPath := d.LogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}

	// 用用户配置的 shell 执行，支持 "cd xxx && ./run.sh" 这类写法。
	// 注意：这里是用户显式配置的启动命令，属于"用户要求的执行"，
	// 与"面板拼接外部输入"是两回事（后者才是注入风险）。
	cmd := exec.CommandContext(ctx, "/bin/bash", "-lc", d.svc.StartCmd)
	if d.svc.WorkDir != "" {
		cmd.Dir = expandHome(d.svc.WorkDir, d.opt.UserHome)
	}
	if d.opt.UserHome != "" {
		cmd.Env = append(os.Environ(), "HOME="+d.opt.UserHome)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 独立进程组，便于整组停止

	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return fmt.Errorf("启动失败: %w", err)
	}
	d.cmd = cmd
	d.pid = cmd.Process.Pid
	d.logFile = f

	// 记录 pid，供面板重启后识别
	_ = os.MkdirAll(filepath.Dir(d.pidFile()), 0o755)
	_ = os.WriteFile(d.pidFile(), []byte(fmt.Sprint(d.pid)), 0o644)

	// 后台回收，避免僵尸进程
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
		d.mu.Lock()
		if d.pid == cmd.Process.Pid {
			d.pid = 0
		}
		d.mu.Unlock()
	}()
	return nil
}

// Stop 停止命令（整组停止，避免守护脚本重新拉起子进程）。
func (d *commandDriver) Stop(ctx context.Context) error {
	d.mu.Lock()
	pid := d.pid
	d.mu.Unlock()
	if pid == 0 {
		if b, err := os.ReadFile(d.pidFile()); err == nil {
			if n, e := parseInt(strings.TrimSpace(string(b))); e == nil {
				pid = n
			}
		}
	}
	if pid <= 0 || !processAlive(pid) {
		_ = os.Remove(d.pidFile())
		return nil
	}
	// 先 TERM 整个进程组，等待退出，再 KILL
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for i := 0; i < 20; i++ {
		if !processAlive(pid) {
			_ = os.Remove(d.pidFile())
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	time.Sleep(300 * time.Millisecond)
	_ = os.Remove(d.pidFile())
	if processAlive(pid) {
		return fmt.Errorf("无法停止进程 %d（可能需要更高权限）", pid)
	}
	return nil
}

func (d *commandDriver) Restart(ctx context.Context) error {
	if err := d.Stop(ctx); err != nil {
		return err
	}
	return d.Start(ctx)
}

func (d *commandDriver) Logs(ctx context.Context, lines int) (string, error) {
	path := d.LogPath()
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("日志文件尚不存在：%s", path)
	}
	if lines <= 0 {
		lines = 200
	}
	return tailFile(path, lines)
}

// LogStream 轮询文件增量（与 nativeDriver 同一策略）。
func (d *commandDriver) LogStream(ctx context.Context) (<-chan string, error) {
	path := d.LogPath()
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		var offset int64
		if st, err := os.Stat(path); err == nil {
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
					offset = 0
				}
				if st.Size() == offset {
					continue
				}
				f, err := os.Open(path)
				if err != nil {
					continue
				}
				_, _ = f.Seek(offset, 0)
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

func (d *commandDriver) Health(ctx context.Context) Health {
	return httpHealth(ctx, d.svc)
}

// Uninstall 停止进程并清理日志/pid 文件。
func (d *commandDriver) Uninstall(ctx context.Context) error {
	if err := d.Stop(ctx); err != nil {
		return err
	}
	_ = os.Remove(d.pidFile())
	return nil
}

// ---------- 小工具 ----------

func expandHome(p, home string) string {
	if strings.HasPrefix(p, "~/") && home != "" {
		return filepath.Join(home, p[2:])
	}
	return p
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// 不能用 kill(pid,0)：面板以 root 运行，判断普通用户进程没问题，
	// 但反过来（普通用户判断 root 进程）会得到 EPERM。统一用 ps 更稳。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-p", fmt.Sprint(pid), "-o", "pid=").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
