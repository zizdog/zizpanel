package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  Docker 管理门面的单测
//
//  这些测试**不连 Docker**：compose 项目的文件读写、名字校验、端口猜测
//  全是纯逻辑，用真实 Docker 反而让测试变慢、变得依赖环境。
//  真正需要 Docker 的那部分由 internal/web 的假 socket 端到端测试覆盖。
// ============================================================================

func newComposeManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(nil, Options{WorkDir: t.TempDir(), UserName: "zizdog", UserHome: t.TempDir()})
}

// TestValidComposeName 名字校验是"编辑 compose 文件"这条能力的安全边界。
func TestValidComposeName(t *testing.T) {
	good := []string{"myblog", "my-blog", "my_blog", "blog1", "a", strings.Repeat("x", 64), "My.Blog-1"}
	for _, n := range good {
		if !validComposeName(n) {
			t.Errorf("应接受合法项目名 %q", n)
		}
	}
	bad := []string{
		"", "..", "../x", "a/b", "/abs", ".hidden", "a b", "a\tb",
		"中文名", "a$b", "a;b", strings.Repeat("x", 65), "a..b",
	}
	for _, n := range bad {
		if validComposeName(n) {
			t.Errorf("应拒绝非法项目名 %q", n)
		}
	}
}

// TestComposeProjectFileStaysInRoot 拼出的路径必须落在 compose 根目录下。
// 这是第二道防线：即便名字校验被绕过，路径也不能越界。
func TestComposeProjectFileStaysInRoot(t *testing.T) {
	m := newComposeManager(t)
	root := m.ComposeRoot()

	file, err := m.ComposeProjectFile("myblog")
	if err != nil {
		t.Fatalf("合法名字不该报错: %v", err)
	}
	want := filepath.Join(root, "myblog", "docker-compose.yml")
	if file != want {
		t.Errorf("路径应为 %s，实际 %s", want, file)
	}
	if !strings.HasPrefix(file, root+string(os.PathSeparator)) {
		t.Errorf("路径越出了 compose 根目录: %s", file)
	}

	for _, bad := range []string{"..", "../evil", "a/b", ".x"} {
		if _, err := m.ComposeProjectFile(bad); err == nil {
			t.Errorf("非法名字 %q 必须报错", bad)
		}
	}
}

// TestComposeSaveAndRead 保存 → 读回的往返，以及目录与文件的落地位置。
func TestComposeSaveAndRead(t *testing.T) {
	m := newComposeManager(t)
	ctx := context.Background()
	yml := "services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"8080:80\"\n"

	msg, err := m.DockerComposeSave(ctx, "myblog", yml, false)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if !strings.Contains(msg, "docker-compose.yml") {
		t.Errorf("返回信息应包含文件路径，实际: %s", msg)
	}

	file, _ := m.ComposeProjectFile("myblog")
	if b, err := os.ReadFile(file); err != nil || string(b) != yml {
		t.Fatalf("文件内容不符：err=%v 内容=%q", err, string(b))
	}

	got, err := m.DockerComposeRead("myblog")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got != yml {
		t.Errorf("读回内容不一致:\n期望 %q\n实际 %q", yml, got)
	}

	// 新项目（文件不存在）读取应返回空串而不是报错：前端要显示空白编辑器
	empty, err := m.DockerComposeRead("notyet")
	if err != nil {
		t.Fatalf("读不存在的项目不该报错: %v", err)
	}
	if empty != "" {
		t.Errorf("不存在的项目应返回空内容，实际 %q", empty)
	}

	// 空内容必须拒绝：保存成功但一部署就报错，是最坏的一种"成功"
	if _, err := m.DockerComposeSave(ctx, "myblog", "   \n  ", false); err == nil {
		t.Error("空内容必须被拒绝")
	}
}

// TestComposeProjectsList 列表要能发现目录、标出缺 yml 的项目。
func TestComposeProjectsList(t *testing.T) {
	m := newComposeManager(t)
	ctx := context.Background()
	root := m.ComposeRoot()

	// 一个完整项目 + 一个只有目录没有 yml 的项目
	if _, err := m.DockerComposeSave(ctx, "full", "services: {}\n", false); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 干扰项：非法目录名不该出现在列表里
	if err := os.MkdirAll(filepath.Join(root, "bad name"), 0o755); err != nil {
		t.Fatal(err)
	}

	list, err := m.DockerComposeProjects(ctx)
	if err != nil {
		t.Fatalf("列项目失败: %v", err)
	}
	byName := map[string]DockerComposeProject{}
	for _, p := range list {
		byName[p.Name] = p
	}
	if _, ok := byName["bad name"]; ok {
		t.Error("非法目录名不该出现在项目列表里")
	}
	if p, ok := byName["full"]; !ok || !p.Exists {
		t.Errorf("完整项目应被识别为 exists=true: %+v", p)
	}
	if p, ok := byName["empty-dir"]; !ok || p.Exists {
		t.Errorf("只有目录的项目应 exists=false: %+v", p)
	}

	// 根目录不存在时返回空列表而不是报错（全新机器上很正常）
	m2 := NewManager(nil, Options{WorkDir: filepath.Join(t.TempDir(), "nope")})
	list2, err := m2.DockerComposeProjects(ctx)
	if err != nil {
		t.Fatalf("根目录不存在时不该报错: %v", err)
	}
	if len(list2) != 0 {
		t.Errorf("应返回空列表，实际 %d 条", len(list2))
	}
}

// TestGuessComposePort 端口猜测是"尽力而为"的显示辅助，宁漏勿错。
func TestGuessComposePort(t *testing.T) {
	good := map[string]int{
		"services:\n  web:\n    ports:\n      - \"8080:80\"\n": 8080,
		"services:\n  web:\n    ports:\n      - 3001:3001\n":   3001,
		"ports:\n  - '9000:9000'\n":                            9000,
		"ports:\n  - 127.0.0.1:8000:80\n":                      8000,
	}
	for yml, want := range good {
		if got := guessComposePort(yml); got != want {
			t.Errorf("猜测端口应为 %d，实际 %d（内容 %q）", want, got, yml)
		}
	}

	// 猜不到时必须返回 0（显示 "—"），不能瞎猜一个
	bad := []string{
		"",
		"services:\n  web:\n    image: nginx\n",
		"ports:\n  - \"${PORT}:80\"\n",
		"ports:\n  - \"80\"\n",
	}
	for _, yml := range bad {
		if got := guessComposePort(yml); got != 0 {
			t.Errorf("猜不到时应返回 0，实际 %d（内容 %q）", got, yml)
		}
	}
}

// TestIsBuiltinNetwork 内置网络不能删。
func TestIsBuiltinNetwork(t *testing.T) {
	for _, n := range []string{"bridge", "host", "none"} {
		if !isBuiltinNetwork(n) {
			t.Errorf("%s 应被判为内置网络", n)
		}
	}
	for _, n := range []string{"my-net", "bridge2", "", "Bridge"} {
		if isBuiltinNetwork(n) {
			t.Errorf("%s 不该被判为内置网络", n)
		}
	}
}

// TestNormalizeRestartPolicy 重启策略必须收敛到 Docker 认识的四值，
// 否则 Docker 会报一个"invalid restart policy"的错误。
func TestNormalizeRestartPolicy(t *testing.T) {
	cases := map[string]string{
		"":                 "no",
		"no":               "no",
		"always":           "always",
		"unless-stopped":   "unless-stopped",
		"on-failure":       "on-failure",
		"ALWAYS":           "always",
		"  always  ":       "always",
		"bogus":            "no",
		"always; rm -rf /": "no",
	}
	for in, want := range cases {
		if got := normalizeRestartPolicy(in); got != want {
			t.Errorf("normalizeRestartPolicy(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestDockerClientUnavailableMessage Docker 不可用时的报错必须可读且可执行。
func TestDockerClientUnavailableMessage(t *testing.T) {
	// 空 socket
	m := NewManager(nil, Options{})
	if _, err := m.dockerClientOrErr(); err == nil {
		t.Fatal("空 socket 应报错")
	} else if !strings.Contains(err.Error(), "应用市场") {
		t.Errorf("应告诉用户去哪里装 Docker，实际: %v", err)
	}

	// socket 路径存在但文件不存在
	m2 := NewManager(nil, Options{DockerSocket: "/nonexistent/docker.sock"})
	if _, err := m2.dockerClientOrErr(); err == nil {
		t.Fatal("socket 不存在时应报错")
	} else if !strings.Contains(err.Error(), "服务管理") {
		t.Errorf("应告诉用户去哪里启动运行时，实际: %v", err)
	}

	// DockerInfo 不返回 error：不可用本身就是要显示给用户的信息
	info := m.DockerInfo(context.Background())
	if info.Available {
		t.Error("没有 socket 时不该报告 available")
	}
	if strings.TrimSpace(info.Error) == "" {
		t.Error("不可用时应给出原因")
	}
}

// TestDockerContainerActionRejectsUnknown 未知动作必须被拒，不能原样发给 Docker。
func TestDockerContainerActionRejectsUnknown(t *testing.T) {
	m := NewManager(nil, Options{DockerSocket: "/nonexistent/docker.sock"})
	// 动作校验发生在连 Docker 之前，所以这里不需要真实环境
	err := m.DockerContainerAction(context.Background(), "c1", "rm -rf /", 0)
	if err == nil {
		t.Fatal("未知动作必须被拒")
	}
	if !strings.Contains(err.Error(), "不支持的操作") {
		t.Errorf("应说明动作不支持，实际: %v", err)
	}
}
