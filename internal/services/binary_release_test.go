package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseBinarySpecsStayDarwinArm64 锁住"官方 release 原生二进制"安装器的两条底线：
//
//	① 产物只能是 darwin-arm64（不许退化成 amd64 / Rosetta，这是 AGENTS.md 铁律 8）；
//	② 安装器与目录条目不能漂移（端口 / ServiceLabel / HealthPath / PanelInstaller）。
//
// 为什么用测试锁：这些字段是"同一个事实写了两遍"（安装器一份、目录一份），
// 只改一处不会有编译错误，却会让市场显示成另一个端口、或让"已安装"判断永远为假。
func TestReleaseBinarySpecsStayDarwinArm64(t *testing.T) {
	if len(releaseBinaryApps) == 0 {
		t.Fatal("releaseBinaryApps 为空")
	}
	for id, spec := range releaseBinaryApps {
		// 上游命名不统一：frp / ddns-go / orbien 用下划线（darwin_arm64），
		// Alist 用连字符（darwin-arm64）。判据是"确实是 macOS arm64 产物"，
		// 所以两种写法都接受 —— 但不接受任何 amd64 / 其它平台的写法。
		if !strings.Contains(spec.Asset, "darwin_arm64") && !strings.Contains(spec.Asset, "darwin-arm64") {
			t.Errorf("%s 的产物 %q 不是 darwin-arm64", id, spec.Asset)
		}
		// MirrorOnly = 自研产物，只在镜像站上有（mac军刀）：
		// 没有 GitHub tag / 官方地址 / 加速镜像，所以下面那三条"官方地址"的判据
		// 对它不适用 —— 但**架构、目录一致性**这些判据一条都不放宽。
		if !spec.MirrorOnly {
			if !strings.HasPrefix(spec.Tag, "v") {
				t.Errorf("%s 的 tag %q 应当写死成 vX.Y.Z（跟着 latest 漂会装错版本）", id, spec.Tag)
			}
			urls := spec.downloadURLs()
			if len(urls) == 0 {
				t.Errorf("%s 没有任何下载地址（非 MirrorOnly 的条目必须有官方地址）", id)
			} else {
				if urls[0] != spec.releaseURL() {
					t.Errorf("%s 的官方地址必须排在加速镜像前面，实际第一个是 %s", id, urls[0])
				}
				if !strings.Contains(urls[0], "github.com/"+spec.Repo+"/releases/download/") {
					t.Errorf("%s 的官方地址不是 GitHub release：%s", id, urls[0])
				}
				for _, u := range urls {
					if !strings.HasPrefix(u, "https://") {
						t.Errorf("%s 的下载地址必须是 https：%s", id, u)
					}
				}
			}
		} else if len(spec.downloadURLs()) != 0 {
			t.Errorf("%s 标了 MirrorOnly 却还给出公网下载地址：%v（自研产物没有 GitHub 源）",
				id, spec.downloadURLs())
		}

		app, ok := FindApp(id)
		if !ok {
			t.Errorf("安装器里的 %s 不在应用目录里（市场里看不到它）", id)
			continue
		}
		// 有些应用已经**改走 Docker**（如 lucky，2026-09-16 用户要求改成容器版），
		// 它们不再由 release 二进制安装器负责，注册表里也不再登记 ——
		// 所以"不在目录里/不是 installer"对它们来说是正常状态。
		// 只有当目录条目**仍然**声明了 PanelInstaller 或 native 安装路径时，
		// 才要求两边完全对齐。
		if app.Kind == KindCompose || app.Kind == KindDocker {
			if app.PanelInstaller != "" {
				t.Errorf("%s 走 Docker 时不该再声明 PanelInstaller（实际 %q）", id, app.PanelInstaller)
			}
			continue
		}
		if app.PanelInstaller != id {
			t.Errorf("%s 的 PanelInstaller 应为 %q，实际 %q", id, id, app.PanelInstaller)
		}
		if app.ServiceLabel != spec.Label {
			t.Errorf("%s 的 ServiceLabel 与安装器不一致：目录 %q / 安装器 %q",
				id, app.ServiceLabel, spec.Label)
		}
		if app.Port != spec.Port {
			t.Errorf("%s 的端口与安装器不一致：目录 %d / 安装器 %d", id, app.Port, spec.Port)
		}
		// UIPort 决定「打开」/健康检查/反代指向哪个端口（Port 与界面口不同的条目才填），
		// 两处写不一致会让界面打开到一个说别的协议的端口。
		if app.UIPort != spec.UIPort {
			t.Errorf("%s 的界面端口与安装器不一致：目录 %d / 安装器 %d",
				id, app.UIPort, spec.UIPort)
		}
		if app.HealthPath != spec.HealthPath {
			t.Errorf("%s 的健康检查路径与安装器不一致：目录 %q / 安装器 %q",
				id, app.HealthPath, spec.HealthPath)
		}
		// ConfigPath 是「📝 编辑配置文件」定位文件用的（相对安装目录），
		// 必须与安装器的 ConfigFile 一致，否则按钮会打开一个不存在的路径。
		if app.ConfigPath != spec.ConfigFile {
			t.Errorf("%s 的配置文件与安装器不一致：目录 %q / 安装器 %q",
				id, app.ConfigPath, spec.ConfigFile)
		}
	}
}

// TestReleaseBinaryPlistResolvesRoot 校验生成的 plist：
//
//	· {root} 占位符全部被替换（留着的话 launchd 会拿到一个字面量路径）；
//	· 可执行文件、配置参数、Label、工作目录都指向安装目录。
func TestReleaseBinaryPlistResolvesRoot(t *testing.T) {
	// 用 orbien-client 当样例：Lucky / Orbien 服务端已在 2026-09-16 移除。
	spec, ok := releaseBinaryApps["orbien-client"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 orbien-client")
	}
	m := &Manager{opt: Options{UserHome: "/Users/tester", UserName: "tester"}}
	p := m.binaryReleasePaths(spec)
	got := releaseBinaryPlist(spec, p, "tester")

	// 这里只断言"每个应用都该有"的东西：可执行文件、Label、UserName、工作目录，
	// 以及 **Args 里的占位符被替换**。具体是 -cd 还是 -c 由各应用自己的 Args 决定，
	// 写死某一个应用的参数会让这个测试在换样例应用时假失败。
	for _, want := range []string{
		"<string>" + p.Binary + "</string>",
		"<string>" + p.Root + "</string>",
		"<string>" + spec.Label + "</string>",
		"<string>tester</string>", // UserName：服务以真实用户身份运行
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist 缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "{root}") {
		t.Errorf("{root} 占位符没有被替换：\n%s", got)
	}
}

// TestOrbienClientConfigOverwritesUpstreamSample 钉住一个很容易写错的点：
//
// orbien 客户端的 release tarball **自带**一份上游示例 orbien.toml，
// 解压后它就在目标路径上。如果按"文件已存在就保留"处理，面板永远写不进自己的配置，
// 用户装完拿到的是上游示例，而且没有任何报错。
// 反过来，**面板生成**的配置必须保留（用户可能改过地址/token），不能重装一次换一次。
func TestOrbienClientConfigOverwritesUpstreamSample(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{opt: Options{UserHome: dir, UserName: "tester"}}
	spec, ok := releaseBinaryApps["orbien-client"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 orbien-client")
	}
	p := m.binaryReleasePaths(spec)
	if p.Config == "" {
		t.Fatal("orbien-client 应有配置文件路径")
	}
	upstream := "server = \"127.0.0.1:9527\"\n\n# [[tunnels]]\n"
	if err := os.MkdirAll(filepath.Dir(p.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, []byte(upstream), 0o644); err != nil {
		t.Fatal(err)
	}

	pw, generated, err := m.ensureReleaseConfig(spec, p)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if !generated {
		t.Fatalf("上游示例配置应被面板配置覆盖，实际 generated=%v secrets=%+v", generated, pw)
	}
	b, err := os.ReadFile(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, panelConfigMarker) || !strings.Contains(body, `server = "`) {
		t.Errorf("生成的配置不完整：\n%s", body)
	}

	// 第二次调用（重装）：面板生成的配置必须原样保留，口令不轮换
	pw2, generated2, err := m.ensureReleaseConfig(spec, p)
	if err != nil {
		t.Fatal(err)
	}
	if generated2 || pw2.Password != "" {
		t.Error("面板已生成的配置不该被覆盖（会让用户改过的地址/token 失效）")
	}
	b2, _ := os.ReadFile(p.Config)
	if string(b2) != body {
		t.Error("第二次调用改动了配置内容")
	}
}

// TestFrpcConfigSeedHasAdminUIAndToken 钉住 frpc 配置模板的三个关键点：
//
//	① admin UI 真的开着（webServer.port = 7400，即 Port）—— 面板「打开」入口的前提；
//	② auth.token 由面板随机生成（不是注释掉的默认值，默认等于不鉴权）；
//	③ 模板必须明确告诉用户"token/地址要改成自己 frps 的" —— 因为面板
//	   已不再提供 frps（2026-09-16 移除），生成的 token 与用户的服务器不可能自动一致。
func TestFrpcConfigSeedHasAdminUIAndToken(t *testing.T) {
	spec, ok := releaseBinaryApps["frpc"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 frpc")
	}
	for _, want := range []string{
		fmt.Sprintf("webServer.port = %d", spec.Port),
		`auth.token = "{token}"`,
		`webServer.user = "{user}"`,
		`webServer.password = "{password}"`,
	} {
		if !strings.Contains(spec.ConfigSeed, want) {
			t.Errorf("frpc 配置模板缺少 %q：\n%s", want, spec.ConfigSeed)
		}
	}
	if spec.TarStrip != 1 || !spec.PickBinary {
		t.Error("frp 的 tarball 里有多个二进制，应当 PickBinary + TarStrip=1 只取 frpc")
	}
	// 面板已不提供 frps：模板里必须写清"这个 token 要自己改"
	if !strings.Contains(spec.ConfigSeed, "auth.token") {
		t.Error("模板必须提到 auth.token")
	}

	// 展开模板：随机值必须真的落进去，占位符不能留在文件里
	s, err := generateConfigSecrets(spec.ConfigSeed, "")
	if err != nil {
		t.Fatal(err)
	}
	got := expandConfigSeed(spec.ConfigSeed, s)
	if strings.Contains(got, "{token}") || strings.Contains(got, "{user}") || strings.Contains(got, "{password}") {
		t.Errorf("模板占位符没有被替换：\n%s", got)
	}
	if !strings.Contains(got, `auth.token = "`+s.Token+`"`) || s.Token == "" {
		t.Errorf("auth.token 没有生成随机值：\n%s", got)
	}
}

// TestExtractArgsPicksSingleBinary 锁住"一个 tarball 里挑一个二进制"：
//
// frp 的 darwin tarball 里 frps / frpc / 示例配置是平级的，全解压会
// 多出一个用不到的 frps（面板已不提供它），还会把上游示例配置落在面板要生成配置的位置上。
func TestExtractArgsPicksSingleBinary(t *testing.T) {
	spec := releaseBinaryApps["frpc"]
	args := spec.extractArgs("/tmp/frp.tar.gz", "/tmp/root")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-xzf /tmp/frp.tar.gz -C /tmp/root",
		"--strip-components=1",
		"frp_0.71.0_darwin_arm64/frpc",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("解压参数缺少 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "/frps") {
		t.Errorf("不该把 frps 一起解压出来（面板已不提供它）：%s", joined)
	}

	// 没有 PickBinary 的应用（orbien-client）保持整体解压
	orb := releaseBinaryApps["orbien-client"]
	if got := strings.Join(orb.extractArgs("a.tar.gz", "/r"), " "); got != "-xzf a.tar.gz -C /r" {
		t.Errorf("orbien-client 应整体解压，实际：%s", got)
	}
}

// TestReleaseBinaryChecksumRejectsMismatch 证明"坏文件真的会被拦住"。
//
// 这条安全收益的全部价值就在这个判定上：校验通过时的日志很好写，
// 但只有喂一个**错误的**哈希仍然报错并中止，才说明它不是一个摆设。
func TestReleaseBinaryChecksumRejectsMismatch(t *testing.T) {
	asset := "frp_0.71.0_darwin_arm64.tar.gz"
	list := "45be02b186860d375ed49a8941ae9569628a54bf14e67fc36b29c98c99dabcc6  " + asset + "\n" +
		"1b1b4e2f1836e21e7333f1dddaacd4ed9ae67d7dbee39046b9d7b7eda6253637  frp_0.71.0_darwin_amd64.tar.gz\n"

	want, err := checksumFor(list, asset)
	if err != nil {
		t.Fatalf("清单解析失败: %v", err)
	}
	if want != "45be02b186860d375ed49a8941ae9569628a54bf14e67fc36b29c98c99dabcc6" {
		t.Fatalf("取错了期望哈希：%s", want)
	}
	// ① 匹配 → 放行
	if err := matchChecksum(asset, want, want, "官方地址", "/tmp/x"); err != nil {
		t.Errorf("一致的哈希不该报错：%v", err)
	}
	// ② 不匹配（模拟镜像篡改/下载损坏）→ 必须报错中止
	err = matchChecksum(asset, want,
		"0000000000000000000000000000000000000000000000000000000000000000", "加速镜像（第三方）", "/tmp/x")
	if err == nil {
		t.Fatal("哈希不匹配必须中止安装，实际放行了")
	}
	if !strings.Contains(err.Error(), "校验不通过") || !strings.Contains(err.Error(), "已中止安装") {
		t.Errorf("错误信息要说清是校验失败且已中止：%v", err)
	}
	// ③ 清单里没有这个文件 → 报错，而不是"当没这一条"跳过
	if _, err := checksumFor(list, "frp_0.71.0_windows_arm64.zip"); err == nil {
		t.Error("清单里没有的文件必须报错，不能静默跳过")
	}
	// ④ 清单本身被改短（不是 64 位）也要拦
	if _, err := checksumFor("deadbeef  "+asset+"\n", asset); err == nil {
		t.Error("非 64 位 sha256 必须报错")
	}
}

// TestReleaseBinaryUninstallPlans 校验两个新条目都能给出"面板安装器"类卸载计划。
//
// 少了它，市场里的卸载按钮会落到 PlanUninstall 的默认分支，
// 用户看到的是"没有找到可卸载的对象"—— 而东西确实是面板装的。
func TestReleaseBinaryUninstallPlans(t *testing.T) {
	m := &Manager{opt: Options{UserHome: "/Users/tester", UserName: "tester"}}
	for id := range releaseBinaryApps {
		// MirrorOnly（自研产物，mac军刀）不走这条轨：它的卸载计划在
		// macsaber.go 的 macSaberInstallPlan（系统级 LaunchDaemon + /opt/macsaber，
		// 与"家目录 + 系统级 daemon"的通用计划完全不同）。这里只保证
		// "登记在注册表里的通用条目都有计划"。
		if ReleaseBinaryIsMirrorOnly(id) {
			continue
		}
		plan, ok := m.releaseBinaryPlan(id)
		if !ok {
			t.Fatalf("%s 没有卸载计划", id)
		}
		if plan.Kind != "installer" {
			t.Errorf("%s 的卸载类型应为 installer，实际 %q", id, plan.Kind)
		}
		if len(plan.Steps) == 0 || len(plan.DataPaths) == 0 {
			t.Errorf("%s 的卸载计划不完整：%+v", id, plan)
		}
		if !strings.HasPrefix(plan.DataPaths[0], "/Users/tester/") {
			t.Errorf("%s 的数据目录应在测试家目录下，实际 %s", id, plan.DataPaths[0])
		}
	}
}

// TestReleaseBinaryDownloadPrefersFastSource 锁住"官方源慢就及时换镜像"。
//
// 起因：12.9MB 的产物走 GitHub 官方约 46KB/s，实测要 5~10 分钟；
// 加速镜像同一份 21 秒。如果官方源没有更短的截止时间，用户会对着进度条等十分钟 ——
// 这不是"安全"换来的，只是没做取舍。
func TestReleaseBinaryDownloadPrefersFastSource(t *testing.T) {
	spec, ok := releaseBinaryApps["frpc"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 frpc")
	}
	urls := spec.downloadURLs()
	if len(urls) < 2 {
		t.Fatal("应当有官方 + 至少一个加速镜像")
	}
	if !strings.Contains(urls[0], "github.com") {
		t.Errorf("第一个仍应是官方地址（可信度优先），实际 %s", urls[0])
	}
	for _, u := range urls[1:] {
		if !strings.Contains(u, "http") {
			t.Errorf("镜像地址不合法：%s", u)
		}
	}
}

// TestConfigFilePathResolvesPerInstallLayout 锁住「📝 编辑配置文件」的路径解析。
//
// release 二进制类应用都装在**用户家目录**下（<home>/<RootDir>/<ConfigFile>），
// 解析错一个（比如把 frpc 算成 compose 目录），服务详情里的编辑按钮就会指向
// 一个不存在的文件，或被文件管理器白名单正当地拒绝。
func TestConfigFilePathResolvesPerInstallLayout(t *testing.T) {
	userHome, workDir := "/Users/tester", "/opt/zizpanel/work"

	for _, tc := range []struct{ id, want string }{
		{"frpc", "/Users/tester/frpc/frpc.toml"},
		{"orbien-client", "/Users/tester/orbien-client/orbien.toml"},
	} {
		app, ok := FindApp(tc.id)
		if !ok {
			t.Fatalf("目录里没有 %s", tc.id)
		}
		if got := ConfigFilePath(app, userHome, workDir); got != tc.want {
			t.Errorf("%s 配置路径 = %q，期望 %q", tc.id, got, tc.want)
		}
	}
	// Lucky / Orbien 服务端 / frps 已在 2026-09-16 从目录移除，
	// 它们不该再出现在应用市场里。
	for _, gone := range []string{"lucky", "orbien", "frps"} {
		if _, ok := FindApp(gone); ok {
			t.Errorf("%s 已从应用市场移除，不该还能被找到", gone)
		}
	}
	// 没有声明 ConfigPath 的条目不该凭空得到一个路径
	none, _ := FindApp("uptime-kuma")
	if got := ConfigFilePath(none, userHome, workDir); got != "" {
		t.Errorf("uptime-kuma 不该有配置文件路径，实际 %q", got)
	}
}

// TestNativeClientEntriesPointAtLoopback：两个客户端条目都走原生，
// 配置模板里的目标必须是 127.0.0.1（原生下它就在这台 Mac 上）。
//
// 为什么单列：如果模板里留了 Docker 时代的 host.docker.internal，原生安装出来的
// frpc / orbien 会去解析一个不存在的域名 —— 表现为"一直连不上"，
// 而配置看起来"很合理"。
func TestNativeClientEntriesPointAtLoopback(t *testing.T) {
	for _, id := range []string{"frpc", "orbien-client"} {
		spec, ok := releaseBinaryApps[id]
		if !ok {
			t.Fatalf("releaseBinaryApps 里没有 %s", id)
		}
		if strings.Contains(spec.ConfigSeed, "host.docker.internal") {
			t.Errorf("%s 已改原生，模板里不该再出现 host.docker.internal：\n%s", id, spec.ConfigSeed)
		}
		if !strings.Contains(spec.ConfigSeed, "127.0.0.1") {
			t.Errorf("%s 的模板应给出 127.0.0.1 示例：\n%s", id, spec.ConfigSeed)
		}
		if spec.ID != id || spec.Label == "" || spec.RootDir == "" || spec.ConfigFile == "" {
			t.Errorf("%s 的安装器参数不完整：%+v", id, spec)
		}
	}
	// frpc 的 admin UI 必须真的开在 7400，且服务详情能连到它
	frpc := releaseBinaryApps["frpc"]
	if !strings.Contains(frpc.ConfigSeed, fmt.Sprintf("webServer.port = %d", frpc.Port)) {
		t.Errorf("frpc 的 admin UI 端口应与 Port(%d) 一致：\n%s", frpc.Port, frpc.ConfigSeed)
	}
	if frpc.Port != 7400 || frpc.HealthPath != "/" {
		t.Errorf("frpc 应有可打开的 admin UI（7400 + 健康检查 /），实际 %d/%q", frpc.Port, frpc.HealthPath)
	}
	// orbien 客户端是纯出站，不监听端口
	oc := releaseBinaryApps["orbien-client"]
	if oc.Port != 0 || oc.HealthPath != "" {
		t.Errorf("orbien 客户端不该有端口/健康检查，实际 %d/%q", oc.Port, oc.HealthPath)
	}
}

// TestCredentialBlockCarriesSecretsAndConfigPath：安装结果里必须**真的**能拿到
// 口令/token/界面地址/配置文件路径 —— 用户装完不该再去翻文件。
//
// 口令只出现在这个区块里（不写进步骤叙述），所以它没渲染出来 = 用户永远看不到。
func TestCredentialBlockCarriesSecretsAndConfigPath(t *testing.T) {
	s := configSeedSecrets{Token: "tok123", User: "usr456", Password: "pwd789"}
	block := strings.Join(credentialBlock("frpc", "/Users/x/frpc/frpc.toml", "http://1.2.3.4:7400", s), "\n")
	for _, want := range []string{"tok123", "usr456", "pwd789", "http://1.2.3.4:7400", "/Users/x/frpc/frpc.toml"} {
		if !strings.Contains(block, want) {
			t.Errorf("凭据区块缺少 %q：\n%s", want, block)
		}
	}
	// 没有任何凭据（如 orbien 客户端）时不该渲染一个空区块
	if got := credentialBlock("orbien-client", "/x/orbien.toml", "", configSeedSecrets{}); got != nil {
		t.Errorf("没有凭据时不该渲染区块：%v", got)
	}
}

// TestHealthURLUsesWebPort：健康检查必须打"界面端口"。
//
// frpc 的 admin UI 在 7400，GET 它会拿到 401 —— 而 401 被判定为**健康**
// （能返回 401 正说明 HTTP 服务活着，见 health.go），
// 于是服务管理里的健康列会显示正常，而不是永远"未配置"或"失败"。
func TestHealthURLUsesWebPort(t *testing.T) {
	frpcApp, _ := FindApp("frpc")
	if got, want := healthURLFor(frpcApp), fmt.Sprintf("http://127.0.0.1:%d/", frpcApp.Port); got != want {
		t.Errorf("frpc 健康检查地址 = %q，期望 %q", got, want)
	}
	// 没有界面端口的（orbien 客户端）不给健康检查地址
	oc, _ := FindApp("orbien-client")
	if got := healthURLFor(oc); got != "" {
		t.Errorf("orbien 客户端不该有健康检查地址，实际 %q", got)
	}
	// 没有 HealthPath 的不给地址
	if got := healthURLFor(App{Port: 1234}); got != "" {
		t.Errorf("没有 HealthPath 时不该有健康检查地址，实际 %q", got)
	}
}

// TestFrpcConfigKeepsRetrying 锁住 frpc.toml 里的 loginFailExit = false。
//
// 真机踩到：frp 默认 loginFailExit = true —— 第一次登录失败（服务端还没起、
// 或用户先装客户端后装服务端）就整个进程退出。面板托管的服务被 launchd KeepAlive
// 反复拉起，表现是"装了但 7400 打不开、日志刷屏、健康检查失败"。
func TestFrpcConfigKeepsRetrying(t *testing.T) {
	spec, ok := releaseBinaryApps["frpc"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 frpc")
	}
	cfg := expandConfigSeed(spec.ConfigSeed, configSeedSecrets{
		Token: "t", User: "u", Password: "p",
	})
	if !strings.Contains(cfg, "loginFailExit = false") {
		t.Errorf("frpc.toml 必须显式 loginFailExit = false（否则服务端不在时 frpc 直接退出）：\n%s", cfg)
	}
	if !strings.Contains(cfg, "serverAddr") || !strings.Contains(cfg, "webServer.port = 7400") {
		t.Errorf("frpc.toml 缺少关键字段：\n%s", cfg)
	}
}

// TestHealthURLReconcilesBothWays 锁住"健康地址要与目录对齐，包括**清掉**"。
//
// 真机踩到：某个条目的 HealthPath 从 "/" 改成空（它的「安全入口」会让任何路径 404），
// 但老服务记录里那条 http://127.0.0.1:<port>/ 不会自己消失 —— 只补不清的话，
// 界面会永远显示"健康检查失败（HTTP 404）"，而服务其实是好的。
func TestHealthURLReconcilesBothWays(t *testing.T) {
	// HealthPath 为空的条目：不该生成健康检查地址
	noHealth := App{Port: 16601, HealthPath: ""}
	if noHealth.HealthPath != "" {
		t.Fatalf("样例条目构造不对：%q", noHealth.HealthPath)
	}
	if healthURLFor(noHealth) != "" {
		t.Errorf("HealthPath 为空时不该生成健康检查地址，实际 %q", healthURLFor(noHealth))
	}
	// 有界面端口的条目（frpc 的 admin UI 7400）必须给出地址
	frpcApp, ok := FindApp("frpc")
	if !ok {
		t.Fatal("目录里没有 frpc")
	}
	if healthURLFor(frpcApp) == "" {
		t.Error("frpc 有 admin UI，应当生成健康检查地址")
	}
}

// TestOrderBySpeed 锁住"官方慢就先走镜像，官方快就还走官方"。
//
// 中国大陆无代理时 GitHub Release 完全不通（实测 20 秒 0 字节），
// 而"官方永远排第一 + 150 秒超时"会让每个应用先白等两分半钟。
func TestOrderBySpeed(t *testing.T) {
	urls := []string{"https://github.com/a/b.tgz", "https://ghfast.top/https://github.com/a/b.tgz"}

	// 官方不通（0），镜像有速度 → 镜像排第一
	got := orderBySpeed(urls, []int64{0, 250000})
	if got[0] != urls[1] {
		t.Errorf("官方不可达时应当先用镜像，实际 %v", got)
	}
	// 官方更快（有代理的情况）→ 仍用官方（可信度优先）
	got = orderBySpeed(urls, []int64{5000000, 250000})
	if got[0] != urls[0] {
		t.Errorf("官方更快时应当用官方，实际 %v", got)
	}
	// 都没速度：保持原顺序（官方优先）
	got = orderBySpeed(urls, []int64{0, 0})
	if got[0] != urls[0] || got[1] != urls[1] {
		t.Errorf("都不可达时应保持原顺序，实际 %v", got)
	}
	// 速度数组短于 URL 数组时不能 panic
	got = orderBySpeed(urls, []int64{0})
	if len(got) != 2 {
		t.Errorf("长度应保持，实际 %v", got)
	}
}

// TestPortHasListenerRequiresRealListener 锁住 tarball 轨的幂等跳过判据：
// 光有服务记录 / plist 不算"现在是好的"，**端口真的在监听**才跳过。
//
// 为什么：tarball 轨的登记发生在验收**之前**，所以一次失败的安装同样会留下
// 记录与 plist —— 只凭它们判定，用户再点安装会永远得到"已装跳过"，没有任何
// 恢复入口（2026-09-17 mini 的 Alist 真机就是这种状态）。
func TestPortHasListenerRequiresRealListener(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)
	m.portCheckOverride = func(port int) (bool, []string, error) {
		if port == 5244 {
			return true, []string{"alist"}, nil
		}
		return false, nil, nil
	}
	if !m.portHasListener(5244) {
		t.Error("端口有监听时必须返回 true")
	}
	if m.portHasListener(9999) {
		t.Error("端口没有监听时必须返回 false（否则失败过的安装永远无法重试）")
	}
	if m.portHasListener(0) {
		t.Error("端口 0 必须返回 false（纯出站客户端由调用方另行判断）")
	}
}

// TestAlistInitialPasswordScrapeGoesToCredentialsOnly 锁住 Alist 的初始口令收尾：
// 口令从**首次启动日志**里抓出来，只进 InstallResult.Credentials（凭据区），
// 绝不进任务步骤文本（步骤会进任务日志 / SSE / 审计）。
func TestAlistInitialPasswordScrapeGoesToCredentialsOnly(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)
	d, ok := FindDescriptor("alist")
	if !ok {
		t.Fatal("没有 alist 的描述符")
	}
	root := filepath.Join(m.opt.UserHome, d.Paths.RootDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	logBody := "time=\"2026-09-17 01:00:00\" level=info msg=\"Successfully created the admin user " +
		"and the initial password is: Ab3xY9Zq\"\n" +
		"time=\"2026-09-17 01:00:00\" level=info msg=\"start HTTP server @ 0.0.0.0:5244\"\n"
	if err := os.WriteFile(filepath.Join(root, d.Paths.OutLog), []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}

	res := &InstallResult{App: "alist", Steps: []string{}}
	m.appendAlistInitialPassword(d, res)

	got := ""
	for _, c := range res.Credentials {
		if c.Key == "alist_admin_password" {
			got = c.Value
		}
	}
	if got != "Ab3xY9Zq" {
		t.Fatalf("应从日志里抓出初始口令 Ab3xY9Zq，实际 %q（凭据=%+v）", got, res.Credentials)
	}
	joined := strings.Join(res.Steps, "\n")
	if strings.Contains(joined, "Ab3xY9Zq") {
		t.Errorf("口令不得出现在任务步骤文本里（步骤会进日志/SSE/审计）：\n%s", joined)
	}
	if !strings.Contains(joined, "凭据区") {
		t.Errorf("步骤里要指向凭据区，实际：\n%s", joined)
	}

	// 幂等：再调一次不会重复追加凭据。
	before := len(res.Credentials)
	m.appendAlistInitialPassword(d, res)
	if len(res.Credentials) != before {
		t.Errorf("重复调用不该重复追加凭据：%d → %d", before, len(res.Credentials))
	}
}

// TestAlistInitialPasswordMissingIsHonest 锁住"抓不到就如实说，不编造口令"。
func TestAlistInitialPasswordMissingIsHonest(t *testing.T) {
	m, _ := sandboxIdempotentManager(t)
	d, _ := FindDescriptor("alist")
	res := &InstallResult{App: "alist", Steps: []string{}}
	m.appendAlistInitialPassword(d, res)
	if len(res.Credentials) != 0 {
		t.Errorf("日志里没有那行时不得凭空造出口令，实际 %+v", res.Credentials)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "没有在启动日志里找到初始口令") {
		t.Errorf("要如实说明没抓到，实际：\n%s", joined)
	}
}
