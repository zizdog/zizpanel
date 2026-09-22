package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// 门禁：产品路径里不得再出现"局域网镜像地址"（2026-09-20 用户明确要求）。
//
// 背景：面板要发布给所有人用，作者家里的 NAS 只在作者自己的局域网里 ——
// 产品里任何指向内网镜像的默认值/候选/文案都只会误导用户，在别人的网络里永远失败。
//
// 规则：扫描产品、开发者工具与文档路径（internal/、cmd/、tools/、docs/、AGENTS.md、
// install.sh、uninstall.sh、README.md、Makefile）里的 RFC1918 字面量。
// **不在白名单里的文件一旦出现就红**；白名单里的每个条目都必须写清"为什么它可以留"。
//
// 2026-09-20 起 tools/ 与 docs/ 也纳入扫描：开发者脚本里的内网默认值会把作者家里的
// NAS 带进发布流程（例如 `make mirror-nas` 生成的签名清单），文档里的历史内网地址
// 会让下一个会话照抄。**过时的东西直接删，不靠横幅保留**。
// 文档白名单**只有 AGENTS.md** 一个例外：它记录的是"作者自己家里的 NAS 仍是他的
// 存储/备份机、AI 不许重启它的容器"这条**运维规则**，不是产品默认路径。
func TestProductPathHasNoLANMirrorAddress(t *testing.T) {
	root := repoRoot(t)
	privateIP := regexp.MustCompile(
		`\b(?:192\.168\.\d{1,3}\.\d{1,3}|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b`)

	// 白名单：只允许"测试夹具 / 与镜像无关的反代、端口夹具"；
	// 文档类只允许 AGENTS.md（运维规则，不是产品默认路径）。
	allow := map[string]string{
		"AGENTS.md":                                   "运维规则：作者自己家里的 NAS（192.168.1.8）仍是存储/备份机、不许重启它的容器；不是产品默认路径，不含任何产品地址",
		"internal/proxies/proxies.go":                 "反向代理功能的示例目标地址（用户自己局域网里的服务，不是镜像）",
		"internal/proxies/proxies_test.go":            "反向代理功能的测试夹具",
		"internal/proxies/proxies_ssl_test.go":        "反向代理功能的测试夹具",
		"internal/proxies/proxies_upstream_test.go":   "反向代理功能的测试夹具",
		"internal/proxies/cache_test.go":              "反向代理缓存功能的测试夹具",
		"internal/proxies/forwarder_test.go":          "反向代理转发器的测试夹具",
		"internal/proxies/lanforward_test.go":         "反向代理「局域网转发」功能的测试夹具（私有网段判定用）",
		"internal/web/api_appproxy.go":                "同事负责的应用子路径反代文件；注释里是用户本机地址示例",
		"internal/web/assets/js/reverseproxy.js":      "同事负责的反向代理前端；示例目标地址",
		"internal/web/api_proxies_lan_test.go":        "反向代理接口的测试夹具",
		"internal/web/api_systemsettings_lan_test.go": "「免授权访问内网段」接口的测试夹具（用户自己的网段）",
		"internal/sysconfig/lan_preauth_test.go":      "「免授权访问内网段」的测试夹具（用户自己的网段）",
		"internal/services/netfail_test.go":           "网络失败判定的测试夹具",
		"internal/services/descriptor_test.go":        "应用描述符的测试夹具",
		"internal/services/miniflux_test.go":          "应用安装器的测试夹具",
		"internal/services/compose_reference_test.go": "compose 推荐项目的测试夹具",
		"internal/services/pypi_mirror_test.go":       "pip 镜像参数的测试夹具",
		"internal/services/syncthing_test.go":         "应用安装器的测试夹具",
		"internal/web/api_audit_test.go":              "操作审计的测试夹具（来源 IP）",
		"internal/web/api_docker_test.go":             "Docker 接口的测试夹具",
		"internal/web/server_test.go":                 "IP 白名单/CIDR 判定的测试夹具",
		"internal/auth/auth_test.go":                  "登录审计来源 IP 的测试夹具",
		"internal/mysql/mysql_test.go":                "MySQL host 授权形式的测试夹具",
		"internal/sites/sites_test.go":                "站点反代地址的测试夹具",
		"internal/store/store_test.go":                "存储层的测试夹具",
		"cmd/zizpanel/http_test.go":                   "明文 HTTP 跳转的测试夹具（Host 头示例）",
		"tools/sandbox-install-test.sh":               "安装脚本沙箱测试夹具（SSH_CONNECTION 与免授权网段样例，不碰真机）",
	}

	var bad []string
	for _, rel := range mirrorScanFiles(t, root) {
		if rel == "internal/config/mirror_public_only_test.go" {
			continue // 门禁自身（含私网网段的正则，不该被自己判红）
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", rel, err)
		}
		for _, m := range privateIP.FindAllString(string(b), -1) {
			if _, ok := allow[rel]; ok {
				continue
			}
			bad = append(bad, rel+" → "+m)
		}
	}
	if len(bad) > 0 {
		t.Errorf("产品/工具/文档路径里出现内网地址（作者家里的 NAS 只在作者自己的局域网里，别人永远连不上）：\n  %s\n"+
			"修法：直接删掉过时段落 / 改成公网镜像（https://mirror.zizdog.com:8888、https://zizdog.com/zizpanel）/ "+
			"改成由调用者提供的变量（如 `: \"${MIRROR_HOST:?…}\"`）；\n"+
			"确属测试夹具或与镜像无关的反代/端口夹具，就把该文件加进本测试的 allow 并写明理由。",
			strings.Join(bad, "\n  "))
	}
}

// TestNoMirrorDockerEndpointInProductCode 公网镜像站**不提供** `/docker` 路径
// （2026-09-20 用户明确）。产品代码里不得再构造 `<镜像基址>/docker` 端点，
// 否则用户配 docker 加速时会把一个 404 端点排在第一位，拉镜像卡死/失败。
//
// 判据是字面量 `"/docker"`（带引号，正好是字符串拼接的形态）与 `/docker/v2`；
// 注释里提到 `/docker` 不算（那是说明文字，不是端点）。
func TestNoMirrorDockerEndpointInProductCode(t *testing.T) {
	root := repoRoot(t)
	forbidden := regexp.MustCompile(`"/docker(?:/|")|/docker/v2\b`)
	var bad []string
	for _, rel := range productFiles(t, root) {
		if rel == "internal/config/mirror_public_only_test.go" {
			continue
		}
		if strings.HasSuffix(rel, "_test.go") {
			continue // 只看产品代码；测试夹具里的旧 URL 不影响运行时行为
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", rel, err)
		}
		if loc := forbidden.FindString(string(b)); loc != "" {
			bad = append(bad, rel+" → "+loc)
		}
	}
	if len(bad) > 0 {
		t.Errorf("产品代码里又出现了镜像站 /docker 端点（公网镜像站不提供该路径）：\n  %s\n"+
			"docker 加速源只允许公网候选（见 services.BuiltinDockerMirrors）。", strings.Join(bad, "\n  "))
	}
}

// repoRoot 用本测试文件的位置反推仓库根（测试的工作目录是包目录，不能直接用 "."）。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败，无法定位仓库根")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("推出来的仓库根 %s 里没有 go.mod：%v", root, err)
	}
	return root
}

// productFiles 返回要扫描的路径（仓库相对路径，已排序去重）。
// internal/、cmd/、tools/ 按扩展名遍历；产品入口与文档显式列出。
func productFiles(t *testing.T, root string) []string {
	t.Helper()
	skipDir := map[string]bool{"node_modules": true, "__pycache__": true, "dist": true, ".git": true}
	wantExt := map[string]bool{".go": true, ".js": true, ".mjs": true, ".py": true, ".sh": true}
	out := []string{}
	for _, dir := range []string{"internal", "cmd", "tools"} {
		err := filepath.Walk(filepath.Join(root, dir), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if skipDir[info.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !wantExt[filepath.Ext(p)] {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			out = append(out, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s 失败：%v", dir, err)
		}
	}
	out = append(out, "install.sh", "uninstall.sh", "README.md", "Makefile")
	if len(out) < 50 {
		t.Fatalf("只扫到 %d 个文件 —— 路径猜错了，门禁等于没跑", len(out))
	}
	return out
}

// mirrorScanFiles 是"局域网镜像地址"门禁的扫描集：
// productFiles（产品代码/工具）+ docs/**.md + AGENTS.md。
//
// docker 端点门禁**不用**这个集合：docs 里会故意写「/docker/v2 → 404」这类反例，
// 把它算成"产品构造的端点"是误判（见 TestNoMirrorDockerEndpointInProductCode）。
func mirrorScanFiles(t *testing.T, root string) []string {
	t.Helper()
	out := productFiles(t, root)
	out = append(out, walkMarkdown(t, root, "docs")...)
	out = append(out, "AGENTS.md")
	return out
}

// walkMarkdown 返回 dir 下所有 .md 的仓库相对路径。
func walkMarkdown(t *testing.T, root, dir string) []string {
	t.Helper()
	out := []string{}
	err := filepath.Walk(filepath.Join(root, dir), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(p) != ".md" {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败：%v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s 下没有扫到 .md —— 路径猜错了，文档门禁等于没跑", dir)
	}
	return out
}
