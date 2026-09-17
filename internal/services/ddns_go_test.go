package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDDNSGoEntryIsWired 锁住 ddns-go 条目与「官方 release 原生二进制」安装器的对齐。
//
// 为什么值得单测（而不是靠人工点一次）：
//   - 目录条目与安装器是**同一个事实写了两遍**（端口 / Label / ConfigPath / HealthPath），
//     只改一处不会有编译错误，却会让「打开」指到错端口、或让「已安装」判断永远为假；
//   - 方案 A 的日常路径全靠「📝 编辑配置文件」，它要求 ConfigPath 非空且能被
//     ConfigFilePath 解析到真实文件 —— 这一条必须在无网络、无 root 的单测里就能锁住。
func TestDDNSGoEntryIsWired(t *testing.T) {
	app, ok := FindApp("ddns-go")
	if !ok {
		t.Fatal("应用市场里没有 ddns-go")
	}
	spec, ok := releaseBinaryApps["ddns-go"]
	if !ok {
		t.Fatal("releaseBinaryApps 里没有 ddns-go（市场点了安装会走不进安装器）")
	}
	if !IsReleaseBinaryApp("ddns-go") {
		t.Error("IsReleaseBinaryApp(ddns-go) 应为 true，否则 web 层不会分流到 release 安装器")
	}

	want := struct {
		port       int
		health     string
		configPath string
		label      string
	}{9876, "/", "ddns-go.yaml", "com.zizdog.ddns-go"}

	if app.Kind != KindNative {
		t.Errorf("ddns-go 应为 KindNative（原生 brew/release 路径），实际 %s", app.Kind)
	}
	if app.PanelInstaller != "ddns-go" {
		t.Errorf("PanelInstaller 应为 ddns-go，实际 %q", app.PanelInstaller)
	}
	if app.ServiceLabel != want.label || spec.Label != want.label {
		t.Errorf("ServiceLabel 不一致：目录 %q / 安装器 %q / 期望 %q",
			app.ServiceLabel, spec.Label, want.label)
	}
	if app.Port != want.port || spec.Port != want.port {
		t.Errorf("端口不一致：目录 %d / 安装器 %d / 期望 %d", app.Port, spec.Port, want.port)
	}
	if app.HealthPath != want.health || spec.HealthPath != want.health {
		t.Errorf("健康检查路径不一致：目录 %q / 安装器 %q / 期望 %q",
			app.HealthPath, spec.HealthPath, want.health)
	}
	if app.ConfigPath != want.configPath || spec.ConfigFile != want.configPath {
		t.Errorf("配置文件名不一致：目录 %q / 安装器 %q / 期望 %q",
			app.ConfigPath, spec.ConfigFile, want.configPath)
	}
	if spec.RootDir != "ddns-go" || spec.Binary != "ddns-go" {
		t.Errorf("安装目录/可执行文件名不对：RootDir=%q Binary=%q", spec.RootDir, spec.Binary)
	}

	// 它监听 9876 且有守护进程：绝不能标 NoDaemon（标了市场会把"launchd 里找不到"
	// 当正常，装完不跑也不报警）。
	if app.NoDaemon {
		t.Error("ddns-go 有常驻守护进程并监听 9876，不该标 NoDaemon")
	}
	// homebrew 的 ddns-go formula 没有 service 块（2026-09-16 真机实测：
	// brew services start ddns-go 直接报 "has not implemented #plist, #service"），
	// 所以这一条**不能**声明 BrewFormula —— 声明了会让人以为 brew services 能托管它。
	if app.BrewFormula != "" {
		t.Errorf("ddns-go 的 formula 无 service 块，不该声明 BrewFormula（实际 %q）", app.BrewFormula)
	}
	// 它不需要 ffmpeg 之类的额外依赖
	for _, r := range app.Requires {
		if strings.Contains(strings.ToLower(r.Type+r.Value+r.Hint), "ffmpeg") {
			t.Errorf("ddns-go 不该依赖 ffmpeg：%+v", r)
		}
	}

	// 方案 A：必须保留「打开」（首次配置要用它自己的网页界面），
	// 所以不能是 ConsoleOnly。
	//
	// PreferDirect **不设**（2026-09-17 修正）：只读排查证实它的子路径其实是好的 ——
	// 未登录 GET / 返回 307，Location 已被面板正确改写；前端资源走相对路径。
	// 原先标 PreferDirect 属于过度保守（会在按钮上挂一个没有依据的 ⚠️）。
	if app.UI == nil {
		t.Fatal("ddns-go 必须有 UI 入口（方案 A 的首次配置要在 :9876 完成）")
	}
	if app.UI.ConsoleOnly {
		t.Error("方案 A 要保留一次「打开」，不能设 ConsoleOnly: true")
	}
	if app.UI.PreferDirect {
		t.Error("ddns-go 的子路径已验证可用，不该再标 PreferDirect（过度保守会给按钮挂无依据的警告）")
	}
	if app.UI.Slug != "ddns-go" {
		t.Errorf("UI slug 应为 ddns-go，实际 %q", app.UI.Slug)
	}

	// 安装后提示必须写出方案 A 的两步，缺一步用户就不知道该开网页还是该改文件。
	for _, needle := range []string{"打开", "9876", "编辑配置文件", "重启服务"} {
		if !strings.Contains(app.PostInstallHint, needle) {
			t.Errorf("PostInstallHint 缺少 %q：\n%s", needle, app.PostInstallHint)
		}
	}
	if app.DocsURL != "https://github.com/jeessy2/ddns-go" {
		t.Errorf("DocsURL 应为上游仓库，实际 %q", app.DocsURL)
	}
	if app.Summary == "" || app.Description == "" {
		t.Error("ddns-go 缺少 Summary/Description")
	}
}

// TestDDNSGoConfigFilePathResolves 锁住方案 A 的日常路径：
// 「📝 编辑配置文件」必须指到 <家目录>/ddns-go/ddns-go.yaml。
//
// 为什么这是关键：纯 release 二进制类应用的配置路径由 ConfigFilePath 在运行期拼出，
// 拼错一个目录名（比如漏掉 RootDir、或按 compose 布局算）按钮就会打开一个不存在的文件，
// 而那正是用户"之后日常"唯一要用的入口。
func TestDDNSGoConfigFilePathResolves(t *testing.T) {
	app, ok := FindApp("ddns-go")
	if !ok {
		t.Fatal("应用市场里没有 ddns-go")
	}
	home, work := "/Users/tester", "/opt/zizpanel/work"
	got := ConfigFilePath(app, home, work)
	if got != "/Users/tester/ddns-go/ddns-go.yaml" {
		t.Errorf("ddns-go 配置路径 = %q，期望 %q", got, "/Users/tester/ddns-go/ddns-go.yaml")
	}

	// 与安装器算出来的真实安装路径必须一致（否则编辑按钮指向的是另一份文件）
	m := &Manager{opt: Options{UserHome: home, UserName: "tester"}}
	spec := releaseBinaryApps["ddns-go"]
	if p := m.binaryReleasePaths(spec); p.Config != got {
		t.Errorf("安装器写入的配置路径 %q 与目录条目解析出的 %q 不一致", p.Config, got)
	}

	// 家目录未知时不猜一个路径（宁可不给入口，也不要给一个错的）
	if got := ConfigFilePath(app, "", work); got != "" {
		t.Errorf("拿不到家目录时应返回空串，实际 %q", got)
	}
}

// TestConfigFilePathStillResolvesOtherLayouts 是回归测试：
// 给 ConfigFilePath 加"brew/release 类应用的配置路径"这件事，绝不能改变
// compose / docker / release / 站点类应用的解析结果。
func TestConfigFilePathStillResolvesOtherLayouts(t *testing.T) {
	home, work := "/Users/tester", "/opt/zizpanel/work"

	// compose / docker 类：装到 <work>/compose/<id>/<文件>
	for _, kind := range []Kind{KindCompose, KindDocker} {
		a := App{ID: "demo", Kind: kind, ConfigPath: "app.conf"}
		if got, want := ConfigFilePath(a, home, work), "/opt/zizpanel/work/compose/demo/app.conf"; got != want {
			t.Errorf("%s 配置路径 = %q，期望 %q", kind, got, want)
		}
		if got := ConfigFilePath(a, home, ""); got != "" {
			t.Errorf("%s 在没有 workDir 时应返回空串，实际 %q", kind, got)
		}
	}

	// release 二进制类（既有条目）不受影响
	for id, want := range map[string]string{
		"frpc":          "/Users/tester/frpc/frpc.toml",
		"orbien-client": "/Users/tester/orbien-client/orbien.toml",
	} {
		app, ok := FindApp(id)
		if !ok {
			t.Fatalf("目录里没有 %s", id)
		}
		if got := ConfigFilePath(app, home, work); got != want {
			t.Errorf("%s 配置路径 = %q，期望 %q", id, got, want)
		}
	}

	// 站点类（没有 ConfigPath）不该凭空得到一个路径
	site, ok := FindApp("typecho")
	if !ok {
		t.Fatal("目录里没有 typecho")
	}
	if site.ConfigPath != "" {
		t.Fatalf("样例取错了：typecho 现在声明了 ConfigPath=%q", site.ConfigPath)
	}
	if got := ConfigFilePath(site, home, work); got != "" {
		t.Errorf("没有 ConfigPath 的条目应返回空串，实际 %q", got)
	}
}

// TestDDNSGoReleaseSpecIsFetchable 锁住下载参数：
// 产物必须是 darwin-arm64、成员在 tarball 顶层（无顶层目录）、参数指向安装目录下的配置。
func TestDDNSGoReleaseSpecIsFetchable(t *testing.T) {
	spec := releaseBinaryApps["ddns-go"]
	if !strings.Contains(spec.Asset, "darwin_arm64") {
		t.Errorf("产物必须是 darwin-arm64：%s", spec.Asset)
	}
	if spec.Tag != "v6.17.7" {
		t.Errorf("tag 应写死成 v6.17.7（跟 latest 漂会装错版本），实际 %q", spec.Tag)
	}
	// 上游 release 里有 checksums.txt（2026-09-16 实下核对过，里面有本产物的 sha256）：
	// 有清单就必须核对，回落到第三方加速镜像时它是唯一的内容校验。
	if spec.ChecksumAsset != "checksums.txt" {
		t.Errorf("应使用上游 checksums.txt 做 sha256 校验，实际 %q", spec.ChecksumAsset)
	}
	// ddns-go 的 tarball **没有**顶层目录：4 个成员（ddns-go / README.md /
	// README_EN.md / LICENSE）平级。TarStrip 写 1 会让 --strip-components 把
	// 成员名算错，解压直接失败（真机核对过目录结构）。
	if spec.TarStrip != 0 || !spec.PickBinary {
		t.Errorf("ddns-go 的 tarball 无顶层目录，应为 TarStrip=0 + PickBinary=true，实际 %d/%v",
			spec.TarStrip, spec.PickBinary)
	}
	joined := strings.Join(spec.extractArgs("/tmp/x/"+spec.Asset, "/root"), " ")
	if !strings.Contains(joined, "-xzf /tmp/x/"+spec.Asset+" -C /root") {
		t.Errorf("解压参数前缀不对：%s", joined)
	}
	if strings.Contains(joined, "--strip-components") {
		t.Errorf("无顶层目录时不该带 --strip-components：%s", joined)
	}
	if !strings.HasSuffix(joined, " ddns-go") {
		t.Errorf("成员名应是在归档顶层直接取 ddns-go，实际：%s", joined)
	}
	// 配置必须落在安装目录里（面板按 <home>/ddns-go/ddns-go.yaml 定位），
	// 不能留 ddns-go 的默认 $HOME/.ddns_go_config.yaml。
	argsJoined := strings.Join(spec.Args, " ")
	if !strings.Contains(argsJoined, "-c {root}/ddns-go.yaml") {
		t.Errorf("启动参数必须把配置写死到安装目录：%s", argsJoined)
	}
	if !strings.Contains(argsJoined, "-l :9876") {
		t.Errorf("启动参数应显式写监听端口 :9876（上游默认值也是它，写出来免得上游改默认）：%s", argsJoined)
	}
	// ddns-go 的网页界面保存时会整体重写 YAML，把面板写的 marker 抹掉 ——
	// 重装判据必须是"文件存在就保留"，否则会把用户的 DNS 密钥当上游示例覆盖。
	if !spec.PreserveExistingConfig {
		t.Error("ddns-go 必须标 PreserveExistingConfig（它的配置由自己的网页界面重写，marker 活不过第一次保存）")
	}
}

// TestDDNSGoConfigSeedStaysZeroValueYAML 锁住配置骨架的一个关键性质：
// 它**只有注释**（外加空行）—— 这样的 YAML 文档是合法的，yaml.Unmarshal 得到全零
// Config，ddns-go 会照常启动（真机实测：正常监听 :9876，GET / 307 跳 /login）。
//
// 为什么不预填 dnsconf/webhook 字段：那部分是 ddns-go 网页界面保存时生成的，
// 面板手抄字段名一旦写错（比如 notallowwanaccess 这种），ddns-go 要么解析失败、
// 要么带着半截配置启动 —— 而零值配置有一个额外好处：上游在"没有用户名口令时"
// 会强制 NotAllowWanAccess（只允许本机/内网访问），第一次配置更安全。
func TestDDNSGoConfigSeedStaysZeroValueYAML(t *testing.T) {
	seed := releaseBinaryApps["ddns-go"].ConfigSeed
	if seed == "" {
		t.Fatal("ddns-go 必须有 ConfigSeed，否则装完配置文件不存在，「📝 编辑配置文件」点开就是读文件失败")
	}
	// 必须带面板标记：重装时 ensureReleaseConfig 靠它判断"这份配置是面板生成的、
	// 可以覆盖骨架"，也靠它区别于用户自己改过的文件（有标记且已被用户改过时保留）。
	if !strings.Contains(seed, panelConfigMarker) {
		t.Errorf("配置骨架必须包含面板标记 %q", panelConfigMarker)
	}
	for i, line := range strings.Split(seed, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		t.Errorf("第 %d 行不是注释/空行，配置骨架必须保持零值 YAML：%q", i+1, line)
	}
	// 骨架要告诉用户"首次去网页、之后改文件"，否则这份文件读起来像一份坏配置
	for _, needle := range []string{"9876", "编辑配置文件", "重启服务"} {
		if !strings.Contains(seed, needle) {
			t.Errorf("配置骨架缺少 %q 的说明", needle)
		}
	}
}

// TestDDNSGoReinstallKeepsUserConfig 是这条目最容易造成真实损失的一步：
// ddns-go 的网页界面点"保存"时会**整体重写** ddns-go.yaml —— 面板写在骨架里的
// marker 注释会被抹掉。如果重装还按"没有 marker 就当我生成/上游示例覆盖"处理，
// 用户第一次保存后只要重装一次，填好的 DNS 服务商密钥与域名就被冲没了。
//
// 所以 ddns-go 的判据是"文件存在就一律保留"，这里用一个模拟的 ddns-go 保存结果
// （没有 marker、带真实密钥）把这条钉死。
func TestDDNSGoReinstallKeepsUserConfig(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{opt: Options{UserHome: dir, UserName: "tester"}}
	spec := releaseBinaryApps["ddns-go"]
	p := m.binaryReleasePaths(spec)
	if p.Config == "" {
		t.Fatal("ddns-go 应有配置文件路径")
	}
	// 真实安装时 InstallReleaseBinary 会先建好安装目录（os.MkdirAll(p.Root)），
	// 这里照做，否则写配置会因为目录不存在而失败，测的就不是"是否覆盖"了。
	if err := os.MkdirAll(filepath.Dir(p.Config), 0o755); err != nil {
		t.Fatal(err)
	}

	// ① 全新安装：必须写出骨架（文件存在，「📝 编辑配置文件」才点得开）
	_, generated, err := m.ensureReleaseConfig(spec, p)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if !generated {
		t.Fatal("全新安装应写出配置骨架")
	}
	seedBody, err := os.ReadFile(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seedBody), panelConfigMarker) {
		t.Error("骨架里应有面板标记（让人一眼看出这还不是用户的配置）")
	}

	// ② 模拟 ddns-go 网页界面保存后的文件：它自己 marshal 出来的 YAML，
	//    没有面板 marker，但有用户的真实 DNS 密钥。
	userCfg := "dnsconf:\n" +
		"    - ipv4:\n        enable: true\n        domains:\n            - home.example.com\n" +
		"      dns:\n        name: cloudflare\n        id: user@example.com\n" +
		"        secret: super-secret-token\n" +
		"user: admin\npassword: $2a$10$abcdefghijklmnop\n"
	if err := os.WriteFile(p.Config, []byte(userCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// ③ 重装：面板绝不能覆盖它
	_, generated2, err := m.ensureReleaseConfig(spec, p)
	if err != nil {
		t.Fatalf("重装生成配置失败: %v", err)
	}
	if generated2 {
		t.Error("用户已保存过的配置（marker 已被 ddns-go 抹掉）不该被骨架覆盖")
	}
	after, err := os.ReadFile(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != userCfg {
		t.Errorf("重装改动了用户配置（DNS 密钥会被冲掉）：\n%s", after)
	}
	if !strings.Contains(string(after), "super-secret-token") {
		t.Error("用户的 DNS 服务商密钥被覆盖了")
	}
}
