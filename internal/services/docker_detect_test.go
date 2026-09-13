package services

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// shortHome 造一个**路径很短**的家目录。
//
// macOS 的 unix socket 路径上限是 104 字节（sun_path），
// 而 t.TempDir() 生成的 /var/folders/... 长路径再加上
// "/.orbstack/run/docker.sock" 会超限，直接 bind 失败。
func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zpdock")
	if err != nil {
		t.Fatalf("创建临时家目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeDockerSocket 在一个 unix socket 上起一个最小的 Docker API，
// 只实现 /version。用来验证 DetectDocker 的探测逻辑，
// 不需要真的安装 Docker，也不需要 root。
func fakeDockerSocket(t *testing.T, sock string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatalf("创建 socket 目录失败: %v", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("监听 unix socket 失败: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"27.3.1","ApiVersion":"1.47"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// TestDetectDockerUsesGivenUserHome 锁定一个真机上踩过的坑。
//
// 面板以 root 通过 LaunchDaemon 运行，进程的 HOME 是 /var/root。
// 如果探测 Docker 时用 os.UserHomeDir() 拼路径，就会去找
// /var/root/.orbstack/run/docker.sock —— 而 OrbStack 的 socket 在
// /Users/<真实用户>/.orbstack/run/docker.sock。
// 结果：OrbStack 明明在正常运行，面板却显示"未安装 Docker"。
//
// 所以这里故意让 socket 只存在于**传入的**家目录下，
// 并要求 DetectDocker 必须能找到它。
func TestDetectDockerUsesGivenUserHome(t *testing.T) {
	realHome := shortHome(t)
	sock := filepath.Join(realHome, ".orbstack", "run", "docker.sock")
	fakeDockerSocket(t, sock)

	// 关键：进程自己的 HOME 指向别处（模拟 root 的 /var/root）
	t.Setenv("HOME", t.TempDir())

	got, ver := DetectDocker("", realHome)
	if got != sock {
		t.Fatalf("应按传入的家目录找到 OrbStack socket\n  期望: %s\n  实际: %s", sock, got)
	}
	if ver != "27.3.1" {
		t.Fatalf("版本解析错误：期望 27.3.1，实际 %q", ver)
	}
}

// TestDetectDockerWithoutUserHomeMissesOrbStack 是上一个测试的反面：
// 不传真实家目录时（等价于用 os.UserHomeDir()）就应该找不到。
// 这条断言保证上面那个测试真的在验证"传入家目录"这件事，
// 而不是碰巧因为别的原因通过。
func TestDetectDockerWithoutUserHomeMissesOrbStack(t *testing.T) {
	realHome := shortHome(t)
	sock := filepath.Join(realHome, ".orbstack", "run", "docker.sock")
	fakeDockerSocket(t, sock)

	// 进程 HOME 与 OrbStack socket 所在目录无关，且没有 /var/run/docker.sock 时
	// 探测必然失败（本机若真有 /var/run/docker.sock，则跳过这条反向断言）
	t.Setenv("HOME", t.TempDir())
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		t.Skip("本机存在 /var/run/docker.sock，无法构造「探测失败」的场景")
	}
	if got, _ := DetectDocker("", ""); got != "" {
		t.Fatalf("未传真实家目录时不应探测到 OrbStack socket，却返回了 %s", got)
	}
}

// TestDetectDockerHonoursConfiguredSocket 确认显式配置的路径优先级最高。
func TestDetectDockerHonoursConfiguredSocket(t *testing.T) {
	home := shortHome(t)
	sock := filepath.Join(home, "custom", "docker.sock")
	fakeDockerSocket(t, sock)

	got, ver := DetectDocker(sock, home)
	if got != sock {
		t.Fatalf("应优先使用显式配置的 socket：期望 %s，实际 %s", sock, got)
	}
	if ver != "27.3.1" {
		t.Fatalf("版本解析错误：期望 27.3.1，实际 %q", ver)
	}
}
