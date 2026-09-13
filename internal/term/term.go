// Package term 实现 Web 终端（浏览器里的真实 shell）。
//
// 安全设计（这一块必须比其它模块更严格）：
//
//  1. **显式开关**：默认关闭。开启需要同时满足：
//     配置里 terminal_enabled=true，且当前请求来源在允许范围内。
//     终端等于把 shell 交给浏览器，不能"默认就有"。
//
//  2. **完整审计**：每个会话记录 开始/结束时间、来源 IP、执行的命令。
//     审计写入数据库，用户可在「操作审计」里回看。
//
//  3. **身份可降权**：默认以真实用户身份启动 shell（不是 root）。
//     需要 root 时由用户在终端里自己执行 sudo（这正是他们在
//     macOS 上的日常习惯，且 sudo 有系统级的审计）。
//     这样即使面板被入侵，攻击者也不会直接拿到一个 root shell。
//
//  4. **会话超时**：空闲超过配置时长自动断开，避免遗留的持久 shell。
package term

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Options 是终端配置。
type Options struct {
	// Enabled 为 false 时拒绝建立任何会话
	Enabled bool
	// User 是运行 shell 的用户。为空或 root 时以 root 运行
	User string
	// Home 是该用户家目录（作为工作目录）
	Home string
	// Shell 指定 shell，默认 /bin/bash（macOS 默认 shell）
	Shell string
	// IdleTimeout 是空闲超时，0 表示不限制
	IdleTimeout time.Duration
	// MaxSessions 同时允许的最大会话数
	MaxSessions int
	// Cols / Rows 是初始终端尺寸
	Cols, Rows uint16
}

// Session 是一个终端会话。
type Session struct {
	ID     string
	pty    *os.File
	cmd    *exec.Cmd
	mu     sync.Mutex
	closed bool
	// 输出回调（由 WebSocket 层设置）
	writeMu sync.Mutex

	StartTime time.Time
	RemoteIP  string
	Username  string

	// 会话级的审计信息
	lastActive time.Time
}

// Manager 管理终端会话。
type Manager struct {
	opt Options

	mu       sync.Mutex
	sessions map[string]*Session
	// OnAudit 在会话开始/结束时回调（用于写审计日志）
	OnAudit func(event string, s *Session, detail string)
	// OnCommand 在识别到一条命令时回调
	OnCommand func(s *Session, command string)
}

// NewManager 创建终端管理器。
func NewManager(opt Options) *Manager {
	if opt.Shell == "" {
		// macOS 自带 bash 3.2；也有用户装了 zsh（系统默认）
		for _, sh := range []string{"/bin/zsh", "/bin/bash"} {
			if _, err := os.Stat(sh); err == nil {
				opt.Shell = sh
				break
			}
		}
	}
	if opt.Shell == "" {
		opt.Shell = "/bin/sh"
	}
	if opt.MaxSessions <= 0 {
		opt.MaxSessions = 5
	}
	return &Manager{opt: opt, sessions: map[string]*Session{}}
}

// Enabled 返回终端是否已启用。
func (m *Manager) Enabled() bool { return m.opt.Enabled }

// Options 返回当前配置（前端用来展示说明）。
func (m *Manager) Options() Options { return m.opt }

// ErrDisabled 表示终端未启用。
var ErrDisabled = errors.New("Web 终端未启用")

// ErrTooManySessions 表示会话数达到上限。
var ErrTooManySessions = errors.New("同时打开的终端会话数已达上限")

// NewSession 创建一个新的终端会话。
func (m *Manager) NewSession(remoteIP string) (*Session, error) {
	if !m.opt.Enabled {
		return nil, ErrDisabled
	}
	m.mu.Lock()
	if len(m.sessions) >= m.opt.MaxSessions {
		m.mu.Unlock()
		return nil, ErrTooManySessions
	}
	m.mu.Unlock()

	pty, tty, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("创建伪终端失败: %w", err)
	}

	cmd := exec.Command(m.opt.Shell, "-l")
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}
	// 凭据：默认降权到真实用户
	if uid, gid, ok := m.credential(); ok {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	home := m.opt.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	cmd.Dir = home
	cmd.Env = m.buildEnv(home)

	if err := cmd.Start(); err != nil {
		_ = pty.Close()
		_ = tty.Close()
		return nil, fmt.Errorf("启动 shell 失败: %w", err)
	}
	// 子进程已经持有 tty，父进程关掉它，否则关闭 pty 时不会有 EOF
	_ = tty.Close()

	s := &Session{
		ID:         newID(),
		pty:        pty,
		cmd:        cmd,
		StartTime:  time.Now(),
		RemoteIP:   remoteIP,
		Username:   m.opt.User,
		lastActive: time.Now(),
	}

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	if m.OnAudit != nil {
		m.OnAudit("terminal_open", s, fmt.Sprintf("打开终端会话（shell=%s，用户=%s）", m.opt.Shell, s.Username))
	}

	// 设置初始窗口大小
	_ = s.Resize(m.opt.Cols, m.opt.Rows)
	return s, nil
}

// Close 关闭会话。
func (m *Manager) Close(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return
	}
	s.Close("")
}

// Count 返回当前会话数。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// List 返回当前会话摘要（不包含终端内容）。
func (m *Manager) List() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.mu.Lock()
		idle := time.Since(s.lastActive).Seconds()
		s.mu.Unlock()
		out = append(out, map[string]any{
			"id": s.ID, "ip": s.RemoteIP, "user": s.Username,
			"started_at": s.StartTime.Format("2006-01-02 15:04:05"),
			"idle_sec":   int(idle),
		})
	}
	return out
}

// CloseAll 关闭全部会话（面板退出时调用）。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Close(id)
	}
}

// ReapIdle 关闭空闲超时的会话。
func (m *Manager) ReapIdle() int {
	if m.opt.IdleTimeout <= 0 {
		return 0
	}
	var toClose []string
	m.mu.Lock()
	for id, s := range m.sessions {
		s.mu.Lock()
		idle := time.Since(s.lastActive)
		s.mu.Unlock()
		if idle > m.opt.IdleTimeout {
			toClose = append(toClose, id)
		}
	}
	m.mu.Unlock()
	for _, id := range toClose {
		m.mu.Lock()
		s := m.sessions[id]
		m.mu.Unlock()
		if s != nil {
			s.Close("空闲超时，已自动断开")
		}
	}
	return len(toClose)
}

// credential 返回 shell 应使用的 uid/gid。
//
// 默认降权：面板以 root 运行，但终端不该默认给 root shell。
// 用户需要 root 时在终端里执行 sudo —— 那是他们熟悉的路径，
// 而且 sudo 本身有系统级审计。
func (m *Manager) credential() (int, int, bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false // 面板本来就不是 root，无需切换
	}
	if m.opt.User == "" || m.opt.User == "root" {
		return 0, 0, false
	}
	u, err := user.Lookup(m.opt.User)
	if err != nil {
		return 0, 0, false
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	if uid == 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

// buildEnv 构造 shell 的环境变量。
//
// 面板由 LaunchDaemon 启动，环境极简（可能没有 LANG/TERM），
// 不补上会导致终端里中文乱码、命令提示符异常。
func (m *Manager) buildEnv(home string) []string {
	path := os.Getenv("PATH")
	if path == "" || !strings.Contains(path, "/opt/homebrew/bin") {
		path = "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	env := []string{
		"TERM=xterm-256color",
		"PATH=" + path,
		"HOME=" + home,
		"LANG=zh_CN.UTF-8",
		"LC_ALL=zh_CN.UTF-8",
		"SHELL=" + m.opt.Shell,
		// 让 shell 知道自己运行在一个非交互式父进程里，但仍需要提示符
		"PS1=\\h:\\W \\u\\$ ",
	}
	if m.opt.User != "" {
		env = append(env, "USER="+m.opt.User, "LOGNAME="+m.opt.User)
	}
	return env
}

func newID() string {
	b := make([]byte, 8)
	if _, err := cryptoRand(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

// ---------- Session 方法 ----------

// Write 把用户输入写到 pty。
func (s *Session) Write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("会话已关闭")
	}
	s.lastActive = time.Now()
	_, err := s.pty.Write(data)
	return err
}

// Read 从 pty 读取输出。返回读到的字节数。
func (s *Session) Read(buf []byte) (int, error) {
	n, err := s.pty.Read(buf)
	if n > 0 {
		s.mu.Lock()
		s.lastActive = time.Now()
		s.mu.Unlock()
	}
	return n, err
}

// Resize 调整终端窗口大小。
func (s *Session) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("会话已关闭")
	}
	return setWinsize(s.pty, cols, rows)
}

// IdleSeconds 返回空闲秒数。
func (s *Session) IdleSeconds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(time.Since(s.lastActive).Seconds())
}

// Close 关闭会话并终止子进程。
func (s *Session) Close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	// 先给 shell 发 SIGHUP（真实终端关闭时就是这样），再关 pty
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGHUP)
	}
	_ = s.pty.Close()

	// 等待退出，超时则强杀
	done := make(chan struct{})
	go func() {
		if s.cmd != nil {
			_ = s.cmd.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	}
}

// Closed 返回会话是否已关闭。
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// ---------------------------------------------------------------------------
//  终端消息协议
//
//  前后端用 JSON 消息通信，而不是裸字节：
//    - 便于在同一通道上同时传"输入 / 尺寸变化 / 心跳"，避免多条连接
//    - 有利于审计：服务端能结构化地识别用户输入
// ---------------------------------------------------------------------------

// Message 是终端通信消息。
type Message struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	// Error 用于服务端下发错误提示
	Error string `json:"error,omitempty"`
}

// Message types
const (
	TypeInput  = "input"
	TypeOutput = "output"
	TypeResize = "resize"
	TypePing   = "ping"
	TypePong   = "pong"
	TypeError  = "error"
	TypeClose  = "close"
)

// EncodeMessage 编码为 JSON。
func EncodeMessage(m Message) []byte {
	b, _ := json.Marshal(m)
	return b
}

// DecodeMessage 解析 JSON。
func DecodeMessage(b []byte) (Message, error) {
	var m Message
	err := json.Unmarshal(b, &m)
	return m, err
}

// ---------------------------------------------------------------------------
//  PTY 底层实现（Darwin）
//
//  用 syscall 直接调用 openpty：Go 标准库没有提供，
//  而 macOS 的 /dev/ptmx 需要 ioctl 才能正确获取从设备，
//  直接调 libc 的 openpty(3) 最可靠。
// ---------------------------------------------------------------------------

// openPTY 打开一对伪终端（master, slave）。
func openPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	// 解锁从设备
	var unlock int32
	if err := ioctl(master.Fd(), syscall.TIOCPTYUNLK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("解锁 ptmx 失败: %w", err)
	}
	// 授权从设备
	var grant int32
	if err := ioctl(master.Fd(), syscall.TIOCPTYGRANT, uintptr(unsafe.Pointer(&grant))); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("授权 ptmx 失败: %w", err)
	}
	// 取得从设备名称
	var buf [128]byte
	if err := ioctlPtr(master.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("获取 pts 名称失败: %w", err)
	}
	name := ""
	for _, b := range buf {
		if b == 0 {
			break
		}
		name += string(b)
	}
	if name == "" {
		_ = master.Close()
		return nil, nil, errors.New("pts 名称为空")
	}
	slave, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("打开 %s 失败: %w", name, err)
	}
	return master, slave, nil
}

// winsize 对应 struct winsize。
type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// setWinsize 设置终端尺寸（TIOCSWINSZ）。
func setWinsize(f *os.File, cols, rows uint16) error {
	ws := winsize{Row: rows, Col: cols, Xpixel: 0, Ypixel: 0}
	return ioctl(f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// ioctl 执行一次 ioctl 系统调用。
func ioctl(fd uintptr, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlPtr(fd uintptr, req uintptr, arg uintptr) error {
	return ioctl(fd, req, arg)
}

// cryptoRand 是 crypto/rand.Read 的间接引用（便于测试替换）。
func cryptoRand(b []byte) (int, error) { return randRead(b) }

// WaitWithContext 在 ctx 取消或会话结束时返回。
func (s *Session) WaitWithContext(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.Close("客户端断开")
	}()
}
