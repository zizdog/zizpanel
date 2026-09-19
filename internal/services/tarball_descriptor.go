package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ============================================================================
//  tarball 轨：官方 release 预编译产物（frpc / orbien-client / ddns-go）
//
//  这个文件是第一轮"描述符化"的落点：把老 InstallReleaseBinary 那段
//  **只有参数不同、流程完全一样**的代码，变成"一份描述符 + 通用执行器"
//  （描述符类型见 descriptor.go，执行器见 steps.go）。
//
//  单一事实来源（很重要）：
//    · **参数表**（repo / tag / asset / 启动参数 / 配置模板 / 说明）留在
//      releaseBinaryApp 里，一处都不搬 —— 它被目录、镜像同步、离线计划、
//      下载点审计等多处消费，搬走会引发一大片无意义的改动；
//    · **流程**由本文件的 tarballInstallSteps 生成成 steps DSL；
//    · releaseBinaryApps 里的展示字段由描述符回填（见 init 末尾），
//      所以老代码查注册表看到的内容与新描述符永远一致 ——
//      不会出现"两套实现各说各话"。
//
//  行为契约（换轨不许变，逐条对齐老 InstallReleaseBinary）：
//    下载（镜像优先 / 官方优先 / 加速镜像兜底 / 失败用磁盘已有）
//    → sha256（frpc、ddns-go 有上游清单；orbien 上游没有，只能架构复核）
//    → tar 解压并挑成员（frpc 剥 1 层挑 frpc；ddns-go 剥 0 层挑 ddns-go）
//    → file(1) 复核 arm64 → chmod 0755
//    → 写配置（面板 marker 判据；ddns-go 是"文件存在即保留"）
//    → 递归 chown 给真实用户
//    → 写 /Library/LaunchDaemons/<label>.plist + bootstrap
//    → 登记进服务管理 → assert_ready（端口或 launchd，**失败即 error**）
// ============================================================================

// tarballDescriptors 是"官方 release 预编译产物"这一类应用的**入口列表**。
//
// 加一个新应用 = 在 releaseBinaryApps 里加一条参数 + 在下面加一行 ID。
// 不需要新写 InstallXxx 函数，也不需要动 internal/web。
//
// 顺序即注册顺序（文档/下载计划/迭代顺序都依赖它），所以刻意不用 map ——
// map 的迭代顺序每次运行都不一样。
var tarballDescriptors = []string{
	"frpc",
	"orbien-client",
	"ddns-go",
	"alist",
	"filebrowser",
}

// descriptorsByID / descriptorOrder 是描述符注册表
// （descriptor.go 的 FindDescriptor / AllDescriptors 用它）。
var (
	descriptorsByID = map[string]AppDescriptor{}
	descriptorOrder []string
)

// registerDescriptor 注册一个描述符。
//
// 重复 ID 直接 panic：那是编程错误，而"后注册的静默覆盖前一个"
// 会让加新应用时排查半天。
func registerDescriptor(d AppDescriptor) {
	if _, dup := descriptorsByID[d.ID]; dup {
		panic("services: 描述符 ID 重复注册：" + d.ID)
	}
	descriptorsByID[d.ID] = d
	descriptorOrder = append(descriptorOrder, d.ID)
}

func init() {
	InitTarballDescriptors()
	// 参数表里的展示字段以描述符为准（名称/图标/分类/端口三处写两份会漂）。
	for id, d := range descriptorsByID {
		spec, ok := releaseBinaryApps[id]
		if !ok {
			continue
		}
		spec.Name = d.Name
		spec.Icon = d.Icon
		spec.Category = d.Category
		spec.Port = d.Port
		spec.Label = d.Service.Label
		releaseBinaryApps[id] = spec
	}
}

// InitTarballDescriptors 按 tarballDescriptors 生成并注册全部描述符。
//
// 刻意导出：单测要能断言"注册表与参数表一致"，也要能在加新应用时
// 一眼看到唯一的接线点（这里是唯一的，别处不许再抄一遍 ID 列表）。
func InitTarballDescriptors() {
	for _, id := range tarballDescriptors {
		spec, ok := releaseBinaryApps[id]
		if !ok {
			// 参数表里没有它 = 加新应用时漏了一步。直接在启动时暴露，
			// 而不是等用户点了安装才报"没有这个应用的安装器"。
			panic("services: tarball 描述符列表里的 " + id + " 不在 releaseBinaryApps 里")
		}
		if _, exists := descriptorsByID[id]; exists {
			continue
		}
		d, err := tarballDescriptor(id, spec)
		if err != nil {
			panic("services: 生成 " + id + " 的描述符失败：" + err.Error())
		}
		if err := d.Validate(); err != nil {
			panic("services: " + id + " 的描述符自检失败：" + err.Error())
		}
		registerDescriptor(d)
	}
}

// tarballDescriptor 把一个 releaseBinaryApp 参数表变成完整描述符。
func tarballDescriptor(id string, spec releaseBinaryApp) (AppDescriptor, error) {
	app, _ := FindApp(id)
	name, icon, category := spec.Name, spec.Icon, spec.Category
	if app.ID != "" {
		// 目录条目是给用户看的真相来源。
		if app.Name != "" {
			name = app.Name
		}
		if app.Icon != "" {
			icon = app.Icon
		}
		if app.Category != "" {
			category = app.Category
		}
	}

	// ---- 产物：主产物 +（可选）上游校验清单 ----
	main := Artifact{
		Name:            spec.Asset,
		Version:         spec.Tag,
		URLs:            spec.downloadURLs(),
		Kind:            ArtifactTarGz,
		StripComponents: spec.TarStrip,
	}
	if spec.PickBinary {
		main.ExtractMember = spec.Binary
	}
	// arm64 证据来自上游资产的写死事实（人工核对过，见 releaseBinaryApps 的注释）。
	// 这里如实带上"怎么核对的"，因为 Asset 名里有 arm64 **不等于**内容一定是 arm64。
	main.Arm64 = &Arm64Evidence{
		Evidence: "上游 release 资产 " + spec.Asset + "（Mach-O arm64）",
		Source:   "文件名 + 上游 release 资产列表；安装时由 verify_arm64 用 file(1) 再复核一次",
	}
	var checksums []Artifact
	if spec.ChecksumAsset != "" {
		main.Checksum = &ArtifactChecksum{
			Kind:  "upstream-list",
			Asset: spec.ChecksumAsset,
			URLs:  spec.checksumURLs(),
			Note: "上游清单与产物可能来自同一个第三方加速镜像 —— 那时这一步只防传输损坏，" +
				"防不住镜像作恶（日志里会把清单来源写出来）。",
		}
		checksums = append(checksums, Artifact{
			Name:    spec.ChecksumAsset,
			Version: spec.Tag,
			URLs:    spec.checksumURLs(),
			Kind:    ArtifactBinary,
		})
	} else {
		// 上游没有 checksums 文件（Orbien 客户端）：如实说明只能做架构复核。
		main.Checksum = &ArtifactChecksum{
			Kind: "",
			Note: "上游 release 里没有 checksums 文件 —— 没有内容校验，只能靠 " +
				"verify_arm64 做架构复核（如实说明，不假装校验过）。",
		}
	}

	steps := tarballInstallSteps(spec, main)
	keepNote := tarballKeepNote(spec)

	bind := spec.BindAddress
	if bind == "" {
		bind = "127.0.0.1"
	}
	if spec.Port <= 0 {
		bind = ""
	}
	d := AppDescriptor{
		ID:             id,
		Name:           name,
		Icon:           icon,
		Category:       category,
		Rail:           RailTarball,
		Kind:           KindNative,
		Port:           spec.Port,
		PanelInstaller: id,
		ServiceLabel:   spec.Label,
		Paths: DescriptorPaths{
			RootDir:    spec.RootDir,
			Binary:     spec.Binary,
			ConfigFile: spec.ConfigFile,
			OutLog:     "launchd.out.log",
			ErrLog:     "launchd.err.log",
		},
		Artifacts:         []Artifact{main},
		ChecksumArtifacts: checksums,
		Steps:             steps,
		Service: ServiceSpec{
			Manager:      "launchd-system",
			Label:        spec.Label,
			PlistPath:    "/Library/LaunchDaemons/" + spec.Label + ".plist",
			RunAs:        "user",
			RequiresSudo: true,
			RunAtLoad:    descriptorBool(true),
			KeepAlive:    descriptorBool(true),
		},
		Health: HealthSpec{
			// 用 Port（协议口）判断存活：dashboard 起得来不代表协议口绑上了，
			// 而后者才是这个应用能不能用的关键。
			Probe:   ProbePort,
			Port:    spec.Port,
			Timeout: releaseBinaryReadyTimeout,
		},
		Uninstall: UninstallSpec{
			RemoveService: descriptorBool(true),
			DataPaths:     []string{"{root}"},
			KeepNote:      keepNote,
		},
		Urls: Urls{
			BindAddress: bind,
			AdminPath:   "/",
			DocsURL:     app.DocsURL,
		},
		Notes: spec.Notes,
	}
	d.Urls.ProxyURLReason, d.Urls.ProxyURL = proxyURLFor(app, spec)
	d.Inputs = tarballInputs(spec)
	d.Settings = tarballSettings(spec)
	return d, nil
}

// proxyURLFor 给出"面板反向代理入口"与缺失原因。
//
// 规则：目录条目声明了子路径 UI 且**不是** ConsoleOnly / SelfConf 的，才有代理入口；
// 其余情况必须写清为什么没有 —— 空字段是一个需要解释的决定。
func proxyURLFor(app App, spec releaseBinaryApp) (reason, url string) {
	if app.UI != nil && app.UI.Slug != "" && !app.UI.ConsoleOnly && !app.UI.SelfConf {
		return "", "/" + app.UI.Slug + "/"
	}
	switch {
	case app.UI != nil && app.UI.ConsoleOnly:
		return fmt.Sprintf("%s 的 %d 端口是**应用自带的控制台**（ConsoleOnly），"+
			"不是面板的使用入口：面板只提供「📝 编辑配置文件」与「🔄 重启服务」，"+
			"不做子路径代理", spec.Name, spec.webPort()), ""
	case spec.Port <= 0:
		return spec.Name + " 不监听任何端口（纯出站连接），没有可代理的 HTTP 入口", ""
	default:
		return spec.Name + " 没有声明子路径入口（AppUI.Slug 为空），面板不生成代理", ""
	}
}

// tarballInstallSteps 生成 tarball 轨的安装步骤序列。
//
// 步骤顺序与老 InstallReleaseBinary 逐条对应，顺序本身有理由，不要重排：
//  1. 建目录 → 2. 下载（镜像预检在下载步骤内部）→ 3. 校验（**解压之前**：
//     宁可下载完立刻失败，也不要把一个校验不通过的 tarball 解压出来、
//     chmod、再交给 launchd 去执行）→ 4. 解压（重装前 bootout）→
//  5. 架构复核 + chmod → 6. 配置（存在即保留的判据在**执行时**做）→
//  7. chown → 8. plist + bootstrap → 9. 登记（**验收之前**：验收如实失败时，
//     登记留在后面会留下"任务失败、服务管理里又找不到它"的半成品）→
//  10. 收尾提示 → 11. 验收（失败即 error）。
func tarballInstallSteps(spec releaseBinaryApp, main Artifact) []InstallStep {
	binPath := "{root}/" + spec.Binary
	steps := []InstallStep{
		EnsureDirAction{Path: "{root}", Mode: 0o755, Owner: "user"},
		DownloadAction{Artifact: main, MirrorPreflight: true},
	}
	if spec.ChecksumAsset != "" {
		// 清单很小（1.6KB），单独下一步，只在**回落公网**时才会真的下载它 ——
		// 那时它是唯一的内容校验来源（见 verify_sha256 的上游清单分支）。
		//
		// SkipWhenMirrorUsed：走镜像时 sha256 以**镜像清单**为准（覆盖更全），
		// 上游这份清单用不到 —— 跳过它，否则"镜像优先"里又塞回一次公网访问，
		// GitHub 不可达时还会让整个安装失败（老实现走镜像时从不取上游清单）。
		steps = append(steps, DownloadAction{
			Artifact: Artifact{
				Name:    spec.ChecksumAsset,
				Version: spec.Tag,
				URLs:    spec.checksumURLs(),
				Kind:    ArtifactBinary,
			},
			SkipWhenMirrorUsed: true,
		})
	}
	steps = append(steps, VerifySHA256Action{
		Artifact: main,
		Source:   "官方 " + spec.ChecksumAsset,
		// 走镜像时由执行器的 VerifyMirror 钩子改用镜像清单的 sha256
		// （镜像清单覆盖全部条目，比"只有 frp 有上游清单"更严）。
		SkipIfNoChecksum: spec.ChecksumAsset == "",
	})
	steps = append(steps, ExtractAction{
		Artifact:        main,
		Dest:            "{root}",
		Member:          main.ExtractMember,
		StripComponents: main.StripComponents,
		BootoutFirst:    true,
		ExpectFile:      binPath,
	})
	steps = append(steps,
		VerifyArm64Action{Path: binPath},
		ChmodAction{Path: binPath, Mode: 0o755},
	)
	if spec.ConfigFile != "" && spec.ConfigSeed != "" {
		steps = append(steps, EnsureConfigAction{
			Path:                   "{root}/" + spec.ConfigFile,
			Template:               spec.ConfigSeed,
			Mode:                   0o600,
			PreserveExistingConfig: spec.PreserveExistingConfig,
		})
	}
	steps = append(steps,
		ChownAction{Path: "{root}", Owner: "user"},
		WritePlistAction{Args: plistArgs(spec)},
		LaunchdBootstrapAction{},
		RegisterServiceAction{},
		MessageAction{Lines: []string{"", "安装目录：{root}", "日志：{root}/launchd.out.log",
			"配置文件：{root}/" + spec.ConfigFile}},
		AssertReadyAction{},
	)
	return steps
}

// plistArgs 生成 ProgramArguments：可执行文件全路径 + 应用的启动参数。
func plistArgs(spec releaseBinaryApp) []string {
	args := make([]string, 0, len(spec.Args)+1)
	args = append(args, "{root}/"+spec.Binary)
	args = append(args, spec.Args...)
	return args
}

// tarballKeepNote 生成"卸载时默认保留了什么"的说明。
//
// 必须点名"这个文件里有秘密"：用户以为卸载就清干净了，结果 token /
// DNS 服务商密钥还留在磁盘上，这是真实的安全问题。
func tarballKeepNote(spec releaseBinaryApp) string {
	keep := "默认保留安装目录（二进制"
	if spec.ConfigFile == "" {
		return keep + "）"
	}
	switch spec.ID {
	case "ddns-go":
		return keep + "与 ddns-go.yaml，配置里有 DNS 服务商的 API Token/密钥）"
	case "frpc":
		return keep + "与 frpc.toml，配置里有 token）"
	case "orbien-client":
		return keep + "与 orbien.toml，配置里可能有服务端 token）"
	case "filebrowser":
		return keep + "与 filebrowser.db；filebrowser.db 里有用户、权限与设置，" +
			"删除它等于重置管理员口令）"
	default:
		return keep + "与 " + spec.ConfigFile + "，配置里可能有秘密）"
	}
}

// tarballInputs 声明这类应用需要在任务中心限时询问的秘密。
//
// 这类应用**不需要**用户输入：配置由面板生成随机值，用户随后在
// 「📝 编辑配置文件」里改成自己的（frpc 的 token / ddns-go 的 DNS 密钥）。
// 返回 nil 是有意的：把"不需要输入"表达成空，而不是让调用方去猜。
func tarballInputs(spec releaseBinaryApp) []InputSpec {
	_ = spec
	return nil
}

// tarballSettings 声明设置界面 schema（与配置模板里的字段一一对应）。
//
// 这些应用的日常改配置走「📝 编辑配置文件」（用户明确要的路径，
// 见 ddns_go_test.go 的 TestDDNSGoEntryIsWired）。schema 先声明出来，
// 让服务详情面板以后能零应用 ID 分支地渲染，并让端口这类取值先可校验。
func tarballSettings(spec releaseBinaryApp) SettingsSchema {
	fields := []SettingSpec{
		{
			Key: "config_file", Label: "配置文件", Kind: SettingString,
			Default: spec.ConfigFile, Required: spec.ConfigFile != "",
			Validate: "path", Advanced: true,
			Note: "相对安装目录；服务详情里的「📝 编辑配置文件」改的就是它",
		},
		{
			Key: "ui_port", Label: "界面端口", Kind: SettingInt,
			Default: spec.webPort(), Min: 1, Max: 65535, Validate: "port",
			Note: "改完要同时改配置文件与 launchd 启动参数，面板不会自动同步",
		},
	}
	if spec.ID == "frpc" {
		fields = append(fields,
			SettingSpec{Key: "auth_token", Label: "auth.token", Kind: SettingSecret,
				BindTo: "{token}", Note: "必须与 frps 的 auth.token 完全一致"},
			SettingSpec{Key: "web_user", Label: "admin UI 用户名", Kind: SettingString,
				BindTo: "{user}", Advanced: true},
			SettingSpec{Key: "web_password", Label: "admin UI 口令", Kind: SettingSecret,
				BindTo: "{password}", Advanced: true},
		)
	}
	if spec.ID == "ddns-go" {
		fields = append(fields,
			SettingSpec{Key: "listen", Label: "监听地址", Kind: SettingString,
				Default: ":9876", Validate: "nonempty", Advanced: true,
				Note: "启动参数 -l；改它要改 launchd 的 plist"},
			SettingSpec{Key: "check_interval", Label: "检查间隔（秒）", Kind: SettingInt,
				Default: 300, Min: 30, Max: 86400, Advanced: true,
				Note: "启动参数 -f 的默认值是 300；改它要改 launchd 的 plist"},
		)
	}
	return SettingsSchema{Fields: fields}
}

// ---------- 兼容层：老 API 背后的唯一实现 ----------
//
// 这些函数的名字与签名与改造前**完全一致**，但内部已经不再有第二份流程：
// 它们要么查描述符，要么把描述符的执行结果翻译成老的返回值。
// 老调用方（internal/web、uninstall_app.go、mirror.go、offline_plan.go）
// 因此不需要任何改动。

// descriptorBool 是 *bool 的取值助手（描述符里的默认值都是 true）。
//
// 名字刻意避开测试文件里已有的 boolPtr（同包不同文件的同名函数会编译冲突）。
func descriptorBool(v bool) *bool { return &v }

// descriptorIDsForRail 返回某条轨上的全部应用 ID（按 ID 排序）。
//
// 给文档与测试用：迁移清单里"哪条轨上有哪些应用"必须是**代码算出来的**，
// 而不是文档里手抄一份会过期的列表。
func descriptorIDsForRail(rail Rail) []string {
	var out []string
	for _, id := range descriptorOrder {
		if d, ok := descriptorsByID[id]; ok && d.Rail == rail {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// descriptorsForRail 返回某条轨上的全部描述符（按 ID 排序）。
func descriptorsForRail(rail Rail) []AppDescriptor {
	ids := descriptorIDsForRail(rail)
	out := make([]AppDescriptor, 0, len(ids))
	for _, id := range ids {
		out = append(out, descriptorsByID[id])
	}
	return out
}

// OrchestrateTarballInstall 执行一次 tarball 轨安装。
//
// 这是 InstallReleaseBinary 的唯一实现主体：装配执行器（含镜像预检、
// 清单校验、服务登记三条钩子）→ 执行描述符的 steps → 把生成的凭据
// 放进 InstallResult 的凭据区块。
func (m *Manager) OrchestrateTarballInstall(ctx context.Context, d AppDescriptor,
	result *InstallResult) error {

	spec, ok := releaseBinaryApps[d.ID]
	if !ok {
		return fmt.Errorf("没有 %s 的参数表（描述符与参数表不一致，面板内部错误）", d.ID)
	}
	p := m.binaryReleasePathsFor(d)

	ec := &ExecConfig{
		Ctx:    ctx,
		Spec:   d,
		Result: result,
		Runner: newSysRunner(m),
		pathVars: map[string]string{
			"{root}":   p.Root,
			"{home}":   m.opt.UserHome,
			"{user}":   m.opt.UserName,
			"{vardir}": m.opt.WorkDir,
		},
		// 镜像是执行期才知道配没配的（设置项），所以候选地址在**执行期**注入，
		// 而不是把镜像地址写死进描述符（写死就无法跟随设置变更，也会让
		// "探测到的地址"与"下载用的地址"有机会漂移）。
		MirrorBase: m.mirrorBase(),
	}

	// ---- 钩子：镜像预检 + 内容校验 + 服务登记 ----
	ec.MirrorPreflight = func(a Artifact) string {
		// 预检探的是 spec.Asset（主产物），返回的也是它 HEAD 成功的地址；
		// 把它交给执行器插到候选第一位 —— 这样"日志说走镜像"与"curl 实际
		// 打的地址"由同一个字符串决定，不可能再各说各话。
		//
		// 只为注册表里的主产物接线（tarballInstallSteps 只给主产物加了
		// MirrorPreflight）。别的产物真要求了预检就回落公网候选，
		// 而不是拿主产物的地址去顶替（那会下载到错的包）。
		if a.Name != spec.Asset {
			return ""
		}
		return m.preflightMirrorAsset(ctx, spec, result)
	}
	ec.VerifyMirror = func(a Artifact) (string, string, error) {
		sha, err := m.mirrorChecksumFor(ctx, spec)
		if err != nil {
			return "", "", err
		}
		return sha, "镜像清单 " + m.appManifestURL(spec.ID, spec.Tag), nil
	}
	ec.FetchChecksumList = func(a Artifact) (string, string, error) {
		list, source, err := m.fetchChecksumList(ctx, spec, p.Root)
		if err != nil {
			// 下不到清单就**中止**：静默跳过等于把"有校验"变成"看运气"。
			return "", "", fmt.Errorf("无法取得官方校验清单 %s: %w。校验失败，已中止安装"+
				"（可稍后网络正常时重试）", spec.ChecksumAsset, err)
		}
		want, err := checksumFor(list, spec.Asset)
		if err != nil {
			return "", "", fmt.Errorf("%v。已中止安装（清单来源：%s）", err, source)
		}
		return want, source, nil
	}
	ec.RegisterService = func(label, name, icon, category string, port int) error {
		return m.RegisterInstalledService(ctx, label, name, icon, category, port)
	}

	if err := ExecuteInstall(ec); err != nil {
		return err
	}

	result.Address = m.primaryIP()
	// 凭据区块：口令/token **只**出现在这里（任务步骤与审计里不许有明文）。
	// 步骤执行期间会在已有步骤后面插入自己的行（见 EnsureConfigAction），
	// 所以这里只在还没有凭据区块时补一个。
	if len(ec.secrets.Token) > 0 || len(ec.secrets.Password) > 0 {
		if !hasCredentialBlock(result.Steps) {
			block := credentialBlock(d.Name, p.Config,
				fmt.Sprintf("http://%s:%d", result.Address, spec.webPort()), ec.secrets)
			result.Steps = append(result.Steps, block...)
		}
	}
	if len(d.Notes) > 0 {
		result.Steps = append(result.Steps, "")
		result.step(ctx, d.Notes...)
	}
	return nil
}

// hasCredentialBlock 判断结果步骤里是否已经有凭据区块。
func hasCredentialBlock(steps []string) bool {
	for _, s := range steps {
		if strings.Contains(s, "凭据（面板随机生成，请自行保存）") {
			return true
		}
	}
	return false
}

// UninstallByDescriptor 按描述符卸载（停止并删除服务，按需删除数据目录）。
func (m *Manager) UninstallByDescriptor(ctx context.Context, d AppDescriptor,
	removeData bool, result *InstallResult) error {

	p := m.binaryReleasePathsFor(d)
	if result != nil {
		result.step(ctx, "停止并删除服务 "+d.Service.Label)
	}
	if err := m.removeService(ctx, d.Service.Label, p.Plist); err != nil {
		return err
	}
	if removeData {
		for _, dp := range d.Uninstall.DataPaths {
			path := expandVars(dp, map[string]string{"{root}": p.Root})
			if err := m.removeTree(ctx, path, result); err != nil {
				return err
			}
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "保留 "+p.Root+"（二进制与配置；需要彻底清理请勾选删除数据）")
	}
	return nil
}

// uninstallPlanForDescriptor 由描述符生成卸载计划（给确认框用）。
func (m *Manager) uninstallPlanForDescriptor(d AppDescriptor) (UninstallPlan, bool) {
	p := m.binaryReleasePathsFor(d)
	plan := UninstallPlan{
		Kind:      "installer",
		Steps:     []string{"停止并删除 launchd 服务 " + d.Service.Label, "从「服务管理」移除记录"},
		DataPaths: []string{p.Root},
		KeepNote:  d.Uninstall.KeepNote,
	}
	return plan, true
}
