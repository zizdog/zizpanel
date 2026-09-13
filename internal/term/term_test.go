package term

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestDisabledByDefault(t *testing.T) {
	m := NewManager(Options{})
	if m.Enabled() {
		t.Fatal("终端默认必须是关闭的 —— 它等于把 shell 交给浏览器")
	}
	if _, err := m.NewSession("127.0.0.1"); err == nil {
		t.Fatal("未启用时不允许建立会话")
	}
}

func TestSessionLifecycle(t *testing.T) {
	m := NewManager(Options{
		Enabled: true,
		User:    os.Getenv("USER"),
		Home:    os.Getenv("HOME"),
		Shell:   "/bin/sh",
		Cols:    80, Rows: 24,
	})
	sess, err := m.NewSession("127.0.0.1")
	if err != nil {
		t.Fatalf("建立会话失败: %v", err)
	}
	defer m.Close(sess.ID)

	if m.Count() != 1 {
		t.Fatalf("会话数应为 1，实际 %d", m.Count())
	}
	if len(m.List()) != 1 {
		t.Fatal("会话列表应包含 1 项")
	}

	// 让 shell 输出一段可识别的内容
	if err := sess.Write([]byte("echo TERM_OK_MARKER\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 读输出（带超时，避免测试卡死）
	deadline := time.Now().Add(5 * time.Second)
	var out strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, err := sess.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			if strings.Contains(out.String(), "TERM_OK_MARKER") {
				break
			}
		}
		if err != nil {
			break
		}
	}
	got := out.String()
	if !strings.Contains(got, "TERM_OK_MARKER") {
		t.Fatalf("未从终端读到预期输出。实际输出：%q", got)
	}
	// 输出应当包含提示符或 echo 的回显，说明是真实交互式 shell
	if len(got) < 5 {
		t.Fatalf("终端输出过短，可能不是真实的交互式 shell: %q", got)
	}

	// 调整窗口大小不应报错
	if err := sess.Resize(120, 40); err != nil {
		t.Fatalf("调整窗口大小失败: %v", err)
	}

	m.Close(sess.ID)
	if m.Count() != 0 {
		t.Fatal("关闭后会话数应为 0")
	}
	// 关闭后写入应失败
	if err := sess.Write([]byte("x")); err == nil {
		t.Fatal("已关闭的会话不应接受写入")
	}
}

func TestSessionLimit(t *testing.T) {
	m := NewManager(Options{
		Enabled: true, Shell: "/bin/sh", MaxSessions: 2,
		User: os.Getenv("USER"), Home: os.Getenv("HOME"),
	})
	var ids []string
	for i := 0; i < 2; i++ {
		s, err := m.NewSession("127.0.0.1")
		if err != nil {
			t.Fatalf("第 %d 个会话应能建立: %v", i+1, err)
		}
		ids = append(ids, s.ID)
	}
	if _, err := m.NewSession("127.0.0.1"); err == nil {
		t.Fatal("超过上限后应拒绝建立新会话")
	}
	for _, id := range ids {
		m.Close(id)
	}
	// 关掉后可以再建
	if s, err := m.NewSession("127.0.0.1"); err != nil {
		t.Fatalf("释放后应能再建会话: %v", err)
	} else {
		m.Close(s.ID)
	}
}

func TestCloseAll(t *testing.T) {
	m := NewManager(Options{
		Enabled: true, Shell: "/bin/sh",
		User: os.Getenv("USER"), Home: os.Getenv("HOME"),
	})
	for i := 0; i < 3; i++ {
		if _, err := m.NewSession("127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	m.CloseAll()
	if m.Count() != 0 {
		t.Fatalf("CloseAll 后不应有残留会话，实际 %d", m.Count())
	}
}

// 空闲回收：把超时设成 0 以外的极小值，验证能自动断开。
func TestReapIdleSessions(t *testing.T) {
	m := NewManager(Options{
		Enabled: true, Shell: "/bin/sh", IdleTimeout: time.Second,
		User: os.Getenv("USER"), Home: os.Getenv("HOME"),
	})
	sess, err := m.NewSession("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	// 会话刚建立时不应被回收
	if n := m.ReapIdle(); n != 0 {
		t.Fatalf("刚建立的会话不应被回收，实际回收 %d 个", n)
	}
	// 等待超过空闲阈值
	time.Sleep(1200 * time.Millisecond)
	if n := m.ReapIdle(); n != 1 {
		t.Fatalf("空闲超时的会话应被回收，实际回收 %d 个", n)
	}
	if !sess.Closed() {
		t.Fatal("被回收的会话应处于关闭状态")
	}
}

func TestMessageCodec(t *testing.T) {
	orig := Message{Type: TypeInput, Data: "ls -la\r"}
	b := EncodeMessage(orig)
	got, err := DecodeMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != orig.Type || got.Data != orig.Data {
		t.Fatalf("编解码不一致: %+v", got)
	}
	// 尺寸消息
	b2 := EncodeMessage(Message{Type: TypeResize, Cols: 120, Rows: 40})
	got2, _ := DecodeMessage(b2)
	if got2.Cols != 120 || got2.Rows != 40 {
		t.Fatalf("尺寸编解码不一致: %+v", got2)
	}
	// 非法 JSON 应报错而不是 panic
	if _, err := DecodeMessage([]byte("not json")); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
}

func TestBuildEnvHasRequiredVars(t *testing.T) {
	m := NewManager(Options{Shell: "/bin/zsh", User: "tester"})
	env := m.buildEnv("/Users/tester")
	joined := strings.Join(env, "\n")
	// 面板由 launchd 启动，环境极简，这些必须补上，否则终端里中文乱码/TERM 异常
	for _, want := range []string{"TERM=", "PATH=", "HOME=", "LANG=", "SHELL="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("环境变量缺少 %s：%v", want, env)
		}
	}
	if !strings.Contains(joined, "/opt/homebrew/bin") {
		t.Fatal("PATH 应包含 Homebrew 目录，否则终端里找不到 brew 安装的命令")
	}
}

func TestSessionIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newID()
		if id == "" {
			t.Fatal("会话 ID 不应为空")
		}
		if seen[id] {
			t.Fatalf("会话 ID 重复: %s", id)
		}
		seen[id] = true
	}
}

// 非 root 环境下不应尝试切换用户（否则会启动失败）。
func TestCredentialWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("当前是 root，跳过非 root 分支")
	}
	m := NewManager(Options{User: "someoneelse"})
	if _, _, ok := m.credential(); ok {
		t.Fatal("非 root 运行时不应设置凭据（会造成权限错误）")
	}
}
