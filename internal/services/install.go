package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/logx"
	"github.com/zizdog/zizpanel/internal/priv"
)

// reconcileLog 记录"服务记录 ↔ 应用目录"对齐动作（改了什么、为什么清空）。
//
// 为什么必须留日志：这类对齐会**改用户看得见的端口与健康地址**，出问题时
// （例如把某个应用的端口对错了）只能靠日志回溯是哪一次列表请求改的。
var reconcileLog = logx.New("services")

// InstallResult 描述一次安装的结果。
type InstallResult struct {
	App     string   `json:"app"`
	Name    string   `json:"name"`
	Message string   `json:"message"`
	Steps   []string `json:"steps"`
	Service *Service `json:"service"`
	Warning string   `json:"warning,omitempty"`
	// Token / Address：部署结果里需要用户**记录下来**的关键信息
	// （共享密钥、本机地址）。放在结构体里而不是只拼进 Steps，
	// 是为了让前端能把它做成可复制的独立区块，而不是埋在日志里。
	Token   string `json:"token,omitempty"`
	Address string `json:"address,omitempty"`
	// Credentials 是"装完必须让用户看一眼"的凭据（例如面板刚给 MySQL 设的
	// root 口令）。复用服务凭据弹窗的同一份结构（Credential），前端只需一套渲染。
	//
	// 安全约定（硬要求）：这是**唯一**允许出现口令的地方。
	// 任务步骤、实时日志、审计里都不许出现 —— 那些地方会被长期保存/转发。
	Credentials []Credential `json:"credentials,omitempty"`

	// mysqlFreshInit 记录"这次是不是我们把 MySQL 数据目录初始化出来的"
	// （--initialize-insecure ⇒ root 必定是空口令）。
	//
	// 为什么必须是本次初始化才敢改 root 口令：数据目录本来就存在的机器上，
	// root 的口令可能正被别的程序使用，替用户改它属于破坏性操作。
	// 不导出：这是内部判断依据，不该出现在 API 响应里。
	mysqlFreshInit bool
}

// Install 安装一个应用市场里的应用并纳入管理。
//
// 支持两种安装方式：
//   - KindNative + BrewFormula：brew install 后交给 brew services 托管
//   - 面板自研安装器（PanelInstaller 非空）：web 层分流到各自的安装器
//
// **KindCompose 不再走这里**：Docker 类条目自 2026-09-17 起都是
// DockerReference（推荐项目），面板只提供预配置 compose 参考文件、不代安装；
// 误调到这里会立刻返回明确错误（见下面的 DockerReference 判断）。
//
// 纳管类应用（AdoptLabel 非空）不走这里，见 AdoptApp。
func (m *Manager) Install(ctx context.Context, appID string) (*InstallResult, error) {
	app, ok := FindApp(appID)
	if !ok {
		return nil, fmt.Errorf("应用市场中找不到 %s", appID)
	}
	// 推荐 Docker 项目不安装（用户 2026-09-17 的要求）。
	//
	// web 层已经在 handleMarketInstall 里返回 4xx 拒绝，这里再挡一次是**纵深防御**：
	// Install() 是导出方法，任务中心、未来的调用方、甚至测试都可能直接调它 ——
	// 只在 HTTP 层挡，等于把这条产品决策挂在"调用方一定会走那个 handler"上。
	if app.DockerReference {
		return nil, fmt.Errorf("「%s」是面板推荐的 Docker 项目，面板不再代你安装；"+
			"请取用预配置的 compose 文件（应用 → docker 页可复制）自行运行", app.Name)
	}
	if app.AdoptLabel != "" {
		return m.AdoptApp(ctx, appID)
	}

	// 幂等：已经装过的应用再点一次「安装」，必须是"已安装（跳过）"这种良性终态，
	// 而不是 failed。真机（2026-09-16）的 8 个失败场景（nginx / mysql84 / ollama /
	// uptime-kuma 的"端口被自己占用"、php81-84 的"服务名已存在"）都是这一条。
	// 判定与提示见 install_idempotent.go。
	//
	// 只对**这个函数真正会安装的那两类**生效（brew 原生 / compose）：
	// 纳管类、面板安装器类、一键建站类在 web 层已经分流，不该被这里的判定
	// 改变它们的语义（例如 WordPress 走到这里是"暂不支持自动安装"）。
	if (app.Kind == KindNative && app.BrewFormula != "") || app.Kind == KindCompose {
		if res, done := m.installedSkipResult(ctx, app); done {
			return res, nil
		}
	}

	// 先做预检查：避免装到一半才发现缺依赖
	pf := m.Preflight(ctx, app)
	if !pf.Ready {
		var reasons []string
		if !pf.PortFree {
			reasons = append(reasons, pf.PortNote)
		}
		for _, c := range pf.Checks {
			if !c.OK {
				reasons = append(reasons, c.Name+": "+c.Detail)
			}
		}
		return nil, fmt.Errorf("安装前检查未通过：%s", strings.Join(reasons, "；"))
	}

	res := &InstallResult{App: app.ID, Name: app.Name}

	switch {
	case app.Kind == KindNative && app.BrewFormula != "":
		if err := m.installViaBrew(ctx, app, res); err != nil {
			return nil, err
		}
	case app.Kind == KindCompose:
		if err := m.installViaCompose(ctx, app, res); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("「%s」暂不支持自动安装。%s", app.Name, app.ManualHint)
	}

	// 写入注册表（managed=true 表示面板负责其生命周期）
	port := app.Port
	health := ""
	if app.HealthPath != "" && port > 0 {
		health = fmt.Sprintf("http://127.0.0.1:%d%s", port, app.HealthPath)
	}
	svc := &Service{
		Name:        app.ID,
		DisplayName: app.Name,
		Kind:        app.Kind,
		Category:    app.Category,
		Icon:        app.Icon,
		Description: app.Summary,
		Port:        port,
		HealthURL:   health,
		LogPath:     res.Service.LogPath,
		Enabled:     true,
		Managed:     true,
		Autostart:   true,
	}
	switch app.Kind {
	case KindNative:
		svc.LaunchLabel = res.Service.LaunchLabel
		svc.PlistPath = res.Service.PlistPath
		svc.WorkDir = res.Service.WorkDir
	case KindCompose:
		svc.ComposeFile = res.Service.ComposeFile
	}

	// 写入注册表。同名记录已存在且就是这个应用时按幂等成功处理 ——
	// 不再报"服务已安装但写入注册表失败: 服务名 php83 已存在"（真机 4 个 PHP 版本）。
	stored, err := m.registerAppService(ctx, app, svc)
	if err != nil {
		return nil, fmt.Errorf("服务已安装但写入注册表失败: %w", err)
	}
	res.Service = stored

	// MySQL 的 root 凭据闭环（限时询问 → 随机 → 写回面板配置 → 自检）。
	//
	// 为什么放在**登记服务之后**：万一这一步失败，MySQL 也已经出现在
	// 「服务管理」里，用户有地方看日志、重启、改配置 —— 否则会变成
	// "装好了但面板里找不到"的僵尸状态（真机上踩过这个坑）。
	// 失败时返回 res（而不是 nil）：任务结果里的"一次性凭据区块"必须能到达
	// 用户，写配置失败时那是口令唯一的记录。
	if isMySQLFormula(app.BrewFormula) {
		if err := m.ensureMySQLRootCredential(ctx, res); err != nil {
			return res, err
		}
	}

	res.Message = fmt.Sprintf("「%s」已安装并纳入管理", app.Name)
	if app.PostInstallHint != "" {
		res.Message += "。" + app.PostInstallHint
	}
	return res, nil
}

// InstalledFormulaVersions 一次性返回**已安装 formula → 版本串**，
// 第二个返回值表示这次探测**真的成功了**。
//
// 为什么不复用 catalog.go 的 InstalledFormulas（map[string]bool）：卸载计划要知道
// 版本，才能把 `php@8.4` 这种"目录写法"对到机器上真实装的 `php 8.4.7`
// （见 uninstall_app.go 的 ResolveBrewFormula）。两者是同一条 `brew list --versions`
// 的两种投影，分开查会白付一次 brew 启动成本。
//
// ⚠️ **必须把"探测失败"与"什么都没装"分开**（2026-09-23 那一类缺陷的根因之一）：
// 过去失败时返回空集合，市场列表就把它当成"这台机器上一个 brew 包都没有"，
// 于是所有只靠 brew 证据的条目（纯 CLI 应用）一起显示「安装」—— 用户眼里
// 就是"装了却显示未装"。正确的含义是"**未复核**"，调用方要如实降级。
//
// 降权规则与 Homebrew 拒绝 root 运行一致。
func (m *Manager) InstalledFormulaVersions(ctx context.Context) (map[string]string, bool) {
	// 单测注入点：不碰真实 brew（理由同 brewUsesProbe）。
	if m.brewInstalledProbe != nil {
		vers, ok := m.brewInstalledProbe(ctx)
		if vers == nil {
			vers = map[string]string{}
		}
		return vers, ok
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "-u", m.opt.UserName,
			m.opt.BrewBin, "list", "--versions")
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, "list", "--versions")
	}
	out, err := cmd.Output()
	if err != nil {
		return map[string]string{}, false
	}
	// 输出形如：nginx 1.31.5 / php@8.2 8.2.33 / mysql@8.4 8.4.11_4
	res := map[string]string{}
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 {
			res[f[0]] = strings.Join(f[1:], " ")
		}
	}
	return res, true
}

// brewRun 以真实用户身份执行 brew 命令。
//
// 必须降权：Homebrew 明确拒绝 root 运行
// （"Running Homebrew as root is extremely dangerous"）。
// 面板以 root 运行，所以这里统一通过 sudo -u 切到真实用户。
// sudoers 里已授权该用户免密使用 sudo，因此不需要密码。
func (m *Manager) brewRun(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	// 只读 / 服务类命令（list、info、services …）沿用"当前镜像"这一条路：
	// 它们不下载瓶文件，换源没有意义。**会下载瓶的 install 走 brewInstall**
	// （带"失败即换源"兜底，见下面的长注释）。
	src := brewInstallSource{
		Name: "当前镜像",
		Env:  m.brewEnv(ctx, firstFormulaOf(args)),
	}
	text, err := m.brewRunSource(ctx, timeout, src, args...)
	if err != nil {
		return text, fmt.Errorf("brew %s 失败: %s", strings.Join(args, " "),
			truncate(strings.TrimSpace(text), 500))
	}
	return text, nil
}

// brewRunSource 用指定源跑一条 brew 命令，返回**完整输出**（不截断）。
//
// 为什么不复用 brewRun：brewRun 会把输出截断到 500 字符再塞进 error，
// 而"瓶校验失败 / 下到的是 0 字节文件"的判据出现在输出中后段，截断后就判断不出来
// —— 那恰恰是 2026-09-20 python@3.11 事故里唯一该换源的信号。
func (m *Manager) brewRunSource(ctx context.Context, timeout time.Duration, src brewInstallSource, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 逐行流式：brew install 的下载/解压进度因此能实时出现在任务中心，
	// 而不是等命令跑完才一次性看到。
	return streamCmd(ctx, m.brewCommand(ctx, src, args...))
}

// brewCommand 构造一条带**指定源**环境变量的 brew 命令（只构造，不执行）。
//
// 抽成独立方法有两个理由：
//  1. 官方源必须**显式清掉** HOMEBREW_API_DOMAIN / HOMEBREW_BOTTLE_DOMAIN，
//     只"不设"是不够的 —— 面板进程或用户 shell 里残留的值会被子进程继承，
//     brew 仍会去撞那个坏镜像。这条约束必须能在不执行 brew 的前提下被验证；
//  2. 降权规则不能走样：Homebrew 拒绝 root，面板是 LaunchDaemon（root），
//     sudo 默认 env_reset，所以镜像变量只能靠 `/usr/bin/env` 显式带进去。
func (m *Manager) brewCommand(ctx context.Context, src brewInstallSource, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		full := []string{"-n", "-u", m.opt.UserName, "/usr/bin/env"}
		full = append(full, brewEnvArgs(src, m.opt.BrewBin, args)...)
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", full...)
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, args...)
		cmd.Env = append(envWithout(os.Environ(), src.Unset), src.Env...)
	}
	// brew 需要正确的 HOME 才能找到 Cellar 与缓存
	if m.opt.UserHome != "" {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, "HOME="+m.opt.UserHome)
	}
	return cmd
}

// brewEnvArgs 构造 `/usr/bin/env` 的参数：先显式清除镜像变量（-u），再注入本次要用的源。
//
// 顺序有意为之：`env` 是"先 unset、再赋值"，-u 必须排在 KEY=VAL 之前，
// 否则残留值会赢。
func brewEnvArgs(src brewInstallSource, brewBin string, args []string) []string {
	out := make([]string, 0, len(src.Unset)*2+len(src.Env)+1+len(args))
	for _, k := range src.Unset {
		out = append(out, "-u", k)
	}
	out = append(out, src.Env...)
	out = append(out, brewBin)
	return append(out, args...)
}

// envWithout 从环境变量列表里去掉指定键。
func envWithout(env []string, keys []string) []string {
	if len(keys) == 0 {
		return append([]string(nil), env...)
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// ============================================================================
//  失败即换源（brew install 的兜底）
//
//  2026-09-20 真机事故：面板在装 Qwen3 TTS 时执行 `brew install python@3.11`
//  失败，报
//      Error: Bottle reports different checksum:   a5dd571f…
//      SHA-256 checksum of downloaded file: e3b0c442…
//  而 e3b0c442… 正是**空文件**的 SHA-256 —— 镜像侧把上游的 403 缓存成了 0 字节
//  文件，brew 拿到空文件后校验失败，整个任务失败。
//
//  三个必须记住的事实（它们决定了下面这段代码的形状）：
//   1. 面板**强制**把 HOMEBREW_BOTTLE_DOMAIN / HOMEBREW_API_DOMAIN 指向自建镜像
//      （install.sh 写 rc、brewEnv 注入），这是为了国内速度，不能因为一次坏缓存就取消；
//   2. python@3.11 的 3.11.16 arm64 bottle 在 USTC/清华/阿里云/腾讯**都没有**
//      （实测 USTC 403、其余 404），只有官方 ghcr.io 有。所以"镜像全挂"是常态，
//      必须有**官方源**这条兜底，而且必须能自动走到；
//   3. brew 的下载缓存按 sha256 命名、命中即**复用**。换了源却不清掉那个空文件，
//      只会拿着同一个坏包在下一个源上再撞一次校验失败 —— 换源等于白换。
//
//  所以：任何一次 brew install 失败都不能直接判死刑，按
//  自建镜像 → 其它国内镜像（清华）→ 官方源(ghcr.io) 逐条重试，
//  每次 result.step 说清"用了哪个源、结果如何"，最后把各源的**真实错误**逐条列出。
// ============================================================================

// brewInstallSource 是一次 brew install 尝试使用的"源"（API 域 + 瓶域）。
type brewInstallSource struct {
	// Name 是人类可读的源名，直接进任务日志与最终错误文案。
	Name string
	// Env 是这次要注入的环境变量（KEY=VAL）。
	Env []string
	// Unset 是这次要**显式清除**的环境变量名。
	// 官方源靠它兜底：只"不设"不够，进程/用户 shell 里残留的值会被子进程继承。
	Unset []string
}

// brewCommonEnv 与源无关的 brew 开关。
//
// 自动更新会在每次 brew 命令前拉一遍仓库元数据（国内很慢，面板自己管安装）；
// NO_INSTALL_CLEANUP 防止 brew 在我们想保留旧版本时自行清理。
//
// NO_AUTOREMOVE（2026-09-21 用户真机）：`brew uninstall php` 结束时会顺手
// `Autoremoving 2 unneeded formulae: net-snmp rtmpdump` —— 用户只是卸载 PHP，
// 面板却把两个**跟他这次操作无关**的包一起删了。所以所有 brew 命令（尤其
// uninstall）都必须带上它：卸载只删用户点名的那一个包。
var brewCommonEnv = []string{
	"HOMEBREW_NO_AUTO_UPDATE=1",
	"HOMEBREW_NO_INSTALL_CLEANUP=1",
	"HOMEBREW_NO_AUTOREMOVE=1",
}

// brewTUNABase 是"另一个国内镜像"（清华 TUNA）的 Homebrew 瓶基址。
//
// 为什么在自建镜像与官方源之间插这一家：自建镜像与面板探测选中的那家可能恰好
// 缺某个 bottle 的某个版本，多一家多一条路；它挂了也只是多一次快速失败，
// 后面还有官方源兜底。
const brewTUNABase = "https://mirrors.tuna.tsinghua.edu.cn/homebrew-bottles"

// brewInstallSources 返回这次 brew install 的**换源顺序**。
//
// 顺序（顺序即优先级，全部写成返回值而不是散在重试循环里，是为了可测）：
//  1. 当前镜像 —— brewEnv 的结果：自建镜像优先，否则是中科大/清华/阿里云里探测到的那家；
//  2. 清华大学镜像 —— 另一家国内镜像（可选项，见 brewTUNABase）；
//  3. 官方源 —— **不设** HOMEBREW_BOTTLE_DOMAIN（brew 回落 ghcr.io），
//     API 也回落 formulae.brew.sh。这是 python@3.11 这类"国内镜像全都没有"的
//     瓶的唯一出路，也是本次事故的根因修复点。
//
// 离线模式（仅走 NAS）下**只保留第 1 条**：项目硬规则是离线模式禁止任何外网回落
// （见 Options.OfflineOnly），宁可明确失败也不能偷偷出网。
func (m *Manager) brewInstallSources(ctx context.Context, formula string) []brewInstallSource {
	curEnv := m.brewEnv(ctx, formula)
	curHasMirror := brewEnvValue(curEnv, "HOMEBREW_BOTTLE_DOMAIN") != "" ||
		brewEnvValue(curEnv, "HOMEBREW_API_DOMAIN") != ""

	// 离线模式（仅走 NAS）：只允许那一个源，绝不回落公网。
	//
	// 2026-09-20 补的诚实性要求：如果**连自建镜像都没探到**，这里不能返回"官方源"
	// 那一条（brewEnv 此时是空的 = brew 走官方默认），那等于"离线模式偷偷出网"。
	// 返回空列表，让 brewInstall 明确报"离线模式下没有可用镜像"。
	if m.opt.OfflineOnly {
		if !curHasMirror {
			return nil
		}
		return []brewInstallSource{{Name: m.brewSourceName(curEnv), Env: curEnv}}
	}

	srcs := make([]brewInstallSource, 0, 3)
	// 第 1 条只在**真的探到镜像**时才排。探不到时它其实就是官方源，
	// 排在第一位意味着"先花几分钟撞官方源、再回头试国内镜像" —— 与
	// "国内镜像优先、官方兜底"正好相反（2026-09-20 修）。
	if curHasMirror {
		srcs = append(srcs, brewInstallSource{Name: m.brewSourceName(curEnv), Env: curEnv})
	}
	if !brewEnvMentions(curEnv, brewTUNABase) {
		srcs = append(srcs, brewInstallSource{
			Name: "清华大学镜像（tuna）",
			Env: append([]string{
				"HOMEBREW_API_DOMAIN=" + brewTUNABase + "/api",
				"HOMEBREW_BOTTLE_DOMAIN=" + brewTUNABase,
			}, brewCommonEnv...),
		})
	}
	// 官方源永远**最后**兜底（显式清掉镜像变量：残留值会让 brew 仍去撞坏镜像）。
	srcs = append(srcs, brewInstallSource{
		Name:  "官方源（formulae.brew.sh / ghcr.io）",
		Env:   append([]string(nil), brewCommonEnv...),
		Unset: []string{"HOMEBREW_API_DOMAIN", "HOMEBREW_BOTTLE_DOMAIN"},
	})
	return srcs
}

// brewSourceName 把一份 brew 环境变量翻译成"人话"的源名。
//
// 日志里"用了哪个源"必须是真的：直接写死"自建镜像"会在探测回落到中科大时骗人
// （而这个项目最贵的教训就是"日志说 A、实际做 B"）。
func (m *Manager) brewSourceName(env []string) string {
	api := brewEnvValue(env, "HOMEBREW_API_DOMAIN")
	bottle := brewEnvValue(env, "HOMEBREW_BOTTLE_DOMAIN")
	switch {
	case bottle == "" && api == "":
		return "官方源（formulae.brew.sh / ghcr.io）"
	case bottle == "":
		return "仅有 API 源 " + api + "（瓶回落官方 ghcr.io）"
	}
	if base := strings.TrimRight(strings.TrimSpace(m.opt.MirrorBase), "/"); base != "" &&
		strings.HasPrefix(bottle, base+"/brew") {
		return "自建镜像 " + bottle
	}
	for _, c := range brewMirrorCandidates {
		if strings.HasPrefix(bottle, c.Base) {
			return c.Name + "镜像 " + bottle
		}
	}
	return "镜像 " + bottle
}

// brewEnvValue 从 KEY=VAL 列表里取一个键的值（取不到返回 ""）。
func brewEnvValue(env []string, key string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"=")
		}
	}
	return ""
}

// brewEnvMentions 判断这份环境变量里是不是已经在用某个镜像基址。
func brewEnvMentions(env []string, base string) bool {
	if base == "" {
		return false
	}
	return strings.HasPrefix(brewEnvValue(env, "HOMEBREW_BOTTLE_DOMAIN"), base) ||
		strings.HasPrefix(brewEnvValue(env, "HOMEBREW_API_DOMAIN"), base)
}

// sha256OfEmptyFile 是空文件的 SHA-256。
//
// 镜像侧把上游 403 缓存成 0 字节文件时，brew 报出来的"实际校验值"就是这个常量。
// 见到它等价于"下到的是个空文件"，必须当作**该源不可用**去换源，而不是直接失败。
const sha256OfEmptyFile = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// brewBottleDownloadBroken 判断一次失败是不是"这个源上的瓶文件是坏的/空的"。
//
// 判据是 brew 自己的原文：校验值不符，或实际校验值等于空文件的 sha256。
// 这类失败**不代表这个包装不上**，只代表这个源不能用了 —— 必须换源重试。
func brewBottleDownloadBroken(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "bottle reports different checksum") ||
		strings.Contains(low, "sha-256 checksum of downloaded file") ||
		strings.Contains(low, sha256OfEmptyFile)
}

// brewFailureLine 从一次失败的输出里挑出**真正的那几句错误**。
//
// 为什么不直接贴输出尾部：brew 失败时最后几行常常是清理/提示，真正的原因
// （例如 "Bottle reports different checksum"）在中间，而"为什么下不动"往往在
// 紧邻的上一行（`curl: (28) timed out`）。只贴尾部等于把最该看的那一行丢掉；
// 只贴一行又会丢掉"校验失败 → 下到的是空文件"这层因果。
// 所以取**最后两行像错误的行**（保持原顺序），既短又不丢线索。
func brewFailureLine(text string, err error) string {
	lines := strings.Split(text, "\n")
	picked := make([]string, 0, 2)
	for i := len(lines) - 1; i >= 0 && len(picked) < 2; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || !brewErrorLineish(l) {
			continue
		}
		picked = append([]string{l}, picked...)
	}
	if len(picked) > 0 {
		return truncate(strings.Join(picked, " / "), 300)
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return truncate(l, 300)
		}
	}
	if err != nil {
		return truncate(err.Error(), 300)
	}
	return "未知错误（brew 没有任何输出）"
}

// brewErrorLineish 判断一行是不是"值得进错误摘要"的那类行。
func brewErrorLineish(l string) bool {
	return strings.HasPrefix(l, "Error:") ||
		strings.HasPrefix(l, "curl:") ||
		strings.Contains(l, "reports different checksum") ||
		strings.Contains(l, "SHA-256 checksum") ||
		strings.Contains(l, "Failed to download")
}

// brewInstall 用"失败即换源"的方式执行一次 brew install，返回成功源的完整输出。
//
// 它是**所有** brew 安装路径的唯一入口（普通市场应用、LNMP、基础依赖、phpMyAdmin、
// Miniflux、Syncthing、Colima、Qwen/IOPaint 的 python@3.11 …）—— 各写一遍必然走样，
// 而这次事故要修的正是"同一条 brew install 少了一层兜底"。
//
// 关于 brewRun（只读命令）与它的分工见 brewRun 的注释。这里的注释不重复
// 顶部那一大段教训，只讲循环本身：
//   - 每次尝试前（除第一次）先清**与本次公式相关**的坏包缓存；
//   - 每次尝试都写 result.step（用了哪个源、成功还是失败、失败的真实原因）；
//   - 全部失败时把每个源的真实错误**逐条**列进 error，并给出可行动的下一步。
func (m *Manager) brewInstall(ctx context.Context, res *InstallResult, timeout time.Duration, formulas ...string) (string, error) {
	formulaList := strings.Join(formulas, "、")
	if len(formulas) == 0 {
		return "", fmt.Errorf("brewInstall 没有指定任何 formula")
	}
	args := append([]string{"install"}, formulas...)
	srcs := m.brewInstallSources(ctx, formulas[0])

	var (
		failures []string
		lastText string
	)
	for i, src := range srcs {
		if i > 0 {
			// 换源之前先清坏包：缓存按 sha256 命名、命中即复用，
			// 不清就会拿着同一个空文件在下一个源上再撞一次校验失败。
			m.cleanBrewDownloadCache(ctx, res, formulas, lastText)
		}
		if res != nil {
			res.step(ctx, fmt.Sprintf("第 %d/%d 个源：%s", i+1, len(srcs), src.Name))
		}
		text, err := m.brewInstallRun(ctx, timeout, src, args...)
		lastText = text
		if err == nil {
			if res != nil {
				res.step(ctx, "安装成功（源："+src.Name+"）："+formulaList)
			}
			return text, nil
		}
		line := brewFailureLine(text, err)
		failures = append(failures, src.Name+" → "+line)
		if res != nil {
			res.step(ctx, "失败（源："+src.Name+"）："+line)
			if brewBottleDownloadBroken(text) {
				// 重点：这不是"包装不上"，而是"这个源是坏的"。必须明说并继续换源。
				res.step(ctx, "  ↑ 这个源上的瓶文件是坏的（0 字节 / 校验值不符），"+
					"按「该源不可用」处理，继续换下一个源")
			}
		}
	}

	detail := strings.Join(failures, "\n  ")
	if m.opt.OfflineOnly {
		return lastText, fmt.Errorf(
			"安装 %s 失败：离线模式（仅走 NAS 镜像）下不允许回落公网/官方源，因此只试了 1 个源。\n  %s\n"+
				"请确认 NAS 镜像已同步该瓶，或关闭「仅走 NAS（离线）」后重试。",
			formulaList, detail)
	}
	return lastText, fmt.Errorf(
		"安装 %s 失败：面板试过的 %d 个源都不可用，各源的真实错误如下：\n  %s\n"+
			"若错误里出现 `Bottle reports different checksum` / 0 字节（e3b0c442…），"+
			"说明这些镜像上还没有这个版本的 arm64 bottle（实测 USTC 403、清华/阿里云/腾讯 404），"+
			"而官方源也不通；建议稍后重试（镜像侧同步有延迟），"+
			"或先清掉 brew 下载缓存再手工执行 `brew install %s`。",
		formulaList, len(srcs), detail, strings.Join(formulas, " "))
}

// brewInstallRun 跑一次尝试。测试通过 brewSourceRunOverride 注入假执行器，
// 这样"换源顺序 / 校验失败要换源"能在不执行真实 brew 的前提下被验证。
func (m *Manager) brewInstallRun(ctx context.Context, timeout time.Duration, src brewInstallSource, args ...string) (string, error) {
	if m.brewSourceRunOverride != nil {
		return m.brewSourceRunOverride(ctx, timeout, src, args...)
	}
	return m.brewRunSource(ctx, timeout, src, args...)
}

// brewSHA256Pattern 从失败输出里抓 64 位十六进制校验值。
//
// 为什么值得抓：缓存条目一律以 `<sha256>--` 开头，而 brew 会在错误里同时给出
// "期望的 sha"和"实际下到的 sha"。用它定位坏包，连**坏在依赖上**（例如
// python@3.11 的 readline 瓶）也能精确删对，而不是只按 formula 名去猜。
var brewSHA256Pattern = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

// brewChecksumsInText 返回文本里出现的所有 sha256（去重、小写）。
func brewChecksumsInText(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range brewSHA256Pattern.FindAllString(strings.ToLower(text), -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// brewCacheMatches 判断一个下载缓存条目是否属于"本次安装相关的坏包"。
//
// 两级判据，都要求名字里出现**明确分隔符**，宁可漏删也绝不误删：
//  1. sha256：缓存条目一律以 `<sha256>--` 开头（Homebrew 用下载内容的 sha 命名），
//     所以从失败输出里抓到的校验值能精确定位那一个坏文件 —— 即使坏的是**依赖**
//     而不是目标公式（例如 python@3.11 依赖的 readline 下成了空文件）也能删对；
//  2. formula：真实布局是 `<sha256>--<formula>-<version>…`（本机缓存实测，
//     例如 `08b1afef…--libssh2-1.11.1_5.bottle_manifest.json`）。
//     分隔符后**必须紧跟版本号（数字）**，这样 `go` 不会连 `go-task-3.40.0`
//     一起删、`php` 不会碰 `phpmyadmin`、`python@3.11` 不会碰 `python@3.12`、
//     `python` 也不会碰 `python@3.11`。
//
// `.incomplete`（下到一半的断点文件）先剥掉再判断 —— 那个同样必须清，
// 否则 brew 会从断点续传继续拼出一个坏文件。
func brewCacheMatches(name string, formulas, sha256s []string) bool {
	base := strings.TrimSuffix(name, ".incomplete")
	for _, sha := range sha256s {
		if sha != "" && strings.HasPrefix(base, sha+"--") {
			return true
		}
	}
	for _, f := range formulas {
		if f == "" {
			continue
		}
		if brewCacheNameHasFormula(base, f) {
			return true
		}
	}
	return false
}

// brewCacheNameHasFormula 判断 `<sha>--<formula>-<version>…` 这类缓存名里的
// formula 是不是**这一个**（而不是恰好以它开头的另一个公式）。
//
// 判据：分隔符（`--<formula>` 之后的一个或多个 `-`）后面必须直接跟版本号的第一位数字。
// 允许 `-` 与 `--` 两种分隔，是为了同时覆盖 `<sha>--xz-5.8.4.tar.gz` 与
// `<sha>--xz--5.8.4.tar.gz` 两种历史布局。
func brewCacheNameHasFormula(base, formula string) bool {
	seg := "--" + formula
	for i := strings.Index(base, seg); i >= 0; {
		rest := strings.TrimLeft(base[i+len(seg):], "-")
		if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			return true
		}
		next := strings.Index(base[i+1:], seg)
		if next < 0 {
			return false
		}
		i = i + 1 + next
	}
	return false
}

// planBrewCacheClean 从缓存目录的文件名列表里挑出要删的那些（纯函数，可单测）。
//
// 只返回与本次公式/本次错误里的校验值相关的条目；目录里的其它东西一律不动 ——
// 调用方也**不会** rm -rf 整个缓存目录。
func planBrewCacheClean(names []string, formulas, sha256s []string) []string {
	var out []string
	for _, n := range names {
		if brewCacheMatches(n, formulas, sha256s) {
			out = append(out, n)
		}
	}
	return out
}

// brewCacheDir 返回 brew 的下载缓存目录。
//
// 缓存属于**真实用户**（Homebrew 的 `<brew> --cache` 就在用户家目录下），
// 所以只能从 m.opt.UserHome 推导，不能自己拼 /var/root 之类。
func (m *Manager) brewCacheDir() string {
	if m.brewCacheDirOverride != "" {
		return m.brewCacheDirOverride
	}
	if strings.TrimSpace(m.opt.UserHome) == "" {
		return ""
	}
	return filepath.Join(m.opt.UserHome, "Library", "Caches", "Homebrew", "downloads")
}

// cleanBrewDownloadCache 在换源重试之前清掉与本次安装相关的坏包缓存条目。
//
// 只删相关条目（判据见 brewCacheMatches），并**把删了什么写进任务日志** ——
// "我删了你的文件"这件事必须可追溯，否则用户没法判断是不是面板误删了什么。
// 目录不存在/不可读一律不是错误（还没下过东西的机器就是这样），如实说一句即可。
func (m *Manager) cleanBrewDownloadCache(ctx context.Context, res *InstallResult, formulas []string, failedText string) {
	dir := m.brewCacheDir()
	if dir == "" {
		if res != nil {
			res.step(ctx, "跳过 brew 下载缓存清理：面板不知道真实用户的家目录")
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if res != nil {
			res.step(ctx, "brew 下载缓存目录不可读（"+dir+"），没有可清理的坏包："+err.Error())
		}
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	doomed := planBrewCacheClean(names, formulas, brewChecksumsInText(failedText))
	if len(doomed) == 0 {
		if res != nil {
			res.step(ctx, "brew 下载缓存里没有与 "+strings.Join(formulas, "、")+
				" 相关的条目（未删任何文件；目录 "+dir+"）")
		}
		return
	}
	paths := make([]string, 0, len(doomed))
	for _, n := range doomed {
		paths = append(paths, filepath.Join(dir, n))
	}
	if err := m.removeBrewCacheEntries(ctx, paths); err != nil {
		if res != nil {
			res.step(ctx, "清理 brew 下载缓存失败（换源重试可能会复用坏包）："+err.Error())
		}
		return
	}
	if res != nil {
		res.step(ctx, "已清除 "+strconv.Itoa(len(doomed))+" 个相关下载缓存条目"+
			"（只删与本次公式相关的，未动整个缓存）："+strings.Join(doomed, "、"))
	}
}

// removeBrewCacheEntries 以**真实用户身份**删除这些缓存条目，绝不以 root 删。
//
// 为什么必须降权：缓存文件归用户所有，root 直接删会在用户下次 brew 时留下
// 归属/权限不一致的目录（Homebrew 自己就明确拒绝 root 运行）。
// 非 root（本地调试实例）时用 os.Remove，等价且不需要 sudo。
func (m *Manager) removeBrewCacheEntries(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if m.brewCacheRemoveOverride != nil {
		return m.brewCacheRemoveOverride(ctx, paths)
	}
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		args := append([]string{"-n", "-u", m.opt.UserName, "/bin/rm", "-f", "--"}, paths...)
		out, err := exec.CommandContext(ctx, "/usr/bin/sudo", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("以用户 %s 删除缓存失败: %v: %s",
				m.opt.UserName, err, truncate(strings.TrimSpace(string(out)), 200))
		}
		return nil
	}
	var firstErr error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// brewMirrorBase 是一个 Homebrew 二进制瓶镜像的基址（API 与瓶文件共用基址）。
type brewMirrorBase struct {
	Name string
	Base string
}

// brewMirrorCandidates 是国内可用的 Homebrew 瓶镜像，**顺序即优先级**。
//
// 为什么不是写死某一家：各家镜像的**路径布局、限速与可用性都会变**，
// 2026-09-16 实测（同一台 Mac、同一个 php@8.3 瓶，走 8MB 分片测速）：
//
//	中科大   <base>/v2/homebrew/core/…/blobs/sha256:<hex>   206   5.1 MB/s
//	阿里云   同一路径 404（不提供 OCI 布局）
//	清华     同一路径 404
//	ghcr.io  官方直连                                       约 91 KB/s（实测 21 分钟没装完 php@8.3）
//
// 所以顺序改成 中科大 → 清华 → 阿里云：中科大同时提供 API 清单与 OCI 瓶路径，
// 实测比官方快约 56 倍；阿里云留着是因为它的 API 稳定（清单可用），
// 只是瓶路径不通 —— 那种情况 brew 会自行回落官方域，不会比不设更差。
var brewMirrorCandidates = []brewMirrorBase{
	{Name: "中科大", Base: "https://mirrors.ustc.edu.cn/homebrew-bottles"},
	{Name: "清华大学", Base: "https://mirrors.tuna.tsinghua.edu.cn/homebrew-bottles"},
	{Name: "阿里云", Base: "https://mirrors.aliyun.com/homebrew/homebrew-bottles"},
}

// brewMirrorProbeTimeout 是探测单个镜像的超时。
//
// 短是刻意的：探测只是"换条路"的准备工作，不该让用户为它等太久；
// 国内镜像正常在 1 秒内应答。
const brewMirrorProbeTimeout = 4 * time.Second

// brewMirrorWorks 判断某个镜像的 API 是否能在 4 秒内给出指定 formula 的清单。
//
// 只确认"这家能给出清单"；真正的瓶是否可下由 brewMirrorSupportsOCI 判断。
func brewMirrorWorks(ctx context.Context, base, formula string) bool {
	if base == "" || formula == "" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, brewMirrorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet,
		strings.TrimRight(base, "/")+"/api/formula/"+formula+".json", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusOK
}

// bottleTagsForArch 返回**本机架构**可能用到的 Homebrew 瓶 tag，按新到旧排列。
//
// 探测时必须拿它去清单里找**真实存在**的瓶 —— 不能硬编码 sha256：
// 之前那个常量是编的，HEAD 永远 404，导致 NAS/镜像分支永不成立、
// 静默回落 ghcr.io（用户"装了 21 分钟"的真因，2026-09-16 NAS 侧实测确认）。
//
// 为什么必须按架构分（2026-09-20 实测补的）：arm64 的 tag 在 Intel 机器上永远
// 匹配不到，于是**所有镜像都会被判为不可用**、静默回落 ghcr.io —— 而 Intel 机器
// 恰恰最依赖国内镜像（官方源更慢）。Intel 侧只有 sonoma / ventura / monterey；
// 上游已经删掉 ventura（实测 python@3.10/3.11/3.12/3.13 都没有 ventura 瓶），
// 但留着它的代价只是一次快速 404，不作为判据去除。
func bottleTagsForArch() []string {
	if runtime.GOARCH == "amd64" {
		return []string{"sonoma", "ventura", "monterey", "tahoe"}
	}
	return []string{"arm64_sequoia", "arm64_tahoe", "arm64_sonoma", "arm64_ventura"}
}

// brewBottleFilename 拼出 brew 7 在**自定义 HOMEBREW_BOTTLE_DOMAIN** 下真正请求的平铺文件名。
//
// 为什么必须完全照抄 brew 的规则（2026-09-20 读了 Homebrew 7.0.3 的
// utils/bottles.rb + bottle.rb 才确认，之前面板的候选是错的）：
//
//	· brew 只在清单的 root_url 形如 https://ghcr.io/v2/… 时才走 OCI 路径
//	  （<base>/v2/homebrew/core/<name>/blobs/sha256:<digest>）；镜像清单里的
//	  root_url 仍写着 ghcr.io，但域被 HOMEBREW_BOTTLE_DOMAIN 换掉了 —— 于是
//	  brew 走的是**旧式平铺**：<domain>/<name>-<version>.<tag>.bottle[.<rebuild>].tar.gz
//	· `<name>` 里的 `@` 会被 URL 编码成 `%40`；
//	· formula 重打包过（rebuild>0）时文件名里必须带 `.bottle.<N>` —— 少了它，
//	  python@3.12（rebuild=1）这种包**永远 404**，探测会误判整家镜像不可用。
func brewBottleFilename(formula, version, tag string, rebuild int) string {
	name := strings.ReplaceAll(formula, "@", "%40")
	if rebuild > 0 {
		return name + "-" + version + "." + tag + ".bottle." + strconv.Itoa(rebuild) + ".tar.gz"
	}
	return name + "-" + version + "." + tag + ".bottle.tar.gz"
}

// brewOCIPath 拼出 OCI 布局路径（老 brew / 部分镜像用）。
//
// formula 里的 `@` 在 OCI 路径里是**目录分隔**：python@3.11 → python/3.11
// （Homebrew 的 GitHubPackages.image_formula_name）。写成 `python@3.11` 会 404，
// 之前的候选就是这么写的 —— 它只在镜像恰好把 `@` 也当路径时才对。
func brewOCIPath(formula, sha string) string {
	return "/v2/homebrew/core/" + strings.ReplaceAll(formula, "@", "/") + "/blobs/sha256:" + sha
}

// brewRemoteNonEmpty 确认一个瓶 URL **真的有内容**（不是 200 + 0 字节）。
//
// 为什么单看状态码不够（2026-09-20 实测）：中科大镜像在 **IPv4** 上对所有 bottle
// 返回 `200` + **0 字节 body**（IPv6 正常，同一时刻同一 URL）。于是"只看 200"的
// 探测会在只有 IPv4 的机器/网络上把中科大选为可用源，随后 brew 拿到空文件、
// 校验失败 —— 正是 2026-09-18 python@3.11 事故的表象。判据必须看内容：
//
//	· HEAD 拿到 Content-Length > 0 即算通过；
//	· 拿不到长度（很多镜像 HEAD 不给）时，用 Range 取 1 个字节，收到 ≥1 字节才算通过。
func brewRemoteNonEmpty(ctx context.Context, url string) bool {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err == nil {
		if hresp, herr := http.DefaultClient.Do(hreq); herr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(hresp.Body, 1<<12))
			_ = hresp.Body.Close()
			if hresp.StatusCode == http.StatusOK || hresp.StatusCode == http.StatusPartialContent {
				if hresp.ContentLength > 0 {
					return true
				}
			}
		}
	}
	// HEAD 不可靠（无 Content-Length / 405）→ 真取一个字节。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return false
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	return n > 0
}

// brewMirrorSupportsOCI 判断某家镜像**真的能取到瓶文件**。
//
// 名字里的 OCI 是历史遗留（最初只探 /v2 布局），现在探的是 brew 真正会用的两条路。
// 刻意贴近 Homebrew 自身行为，而不是拍一个固定 URL：
//  1. GET <base>/api/formula/<formula>.json，按本机 tag 取真实 version / sha256 / rebuild；
//  2. 依次试 brew 实际会用的两种瓶路径（见 brewBottleFilename / brewOCIPath 的说明）；
//  3. 必须**有内容**（见 brewRemoteNonEmpty），只看 200 会把"200 + 0 字节"的坏镜像当成好源。
func brewMirrorSupportsOCI(ctx context.Context, base string) bool {
	return brewMirrorSupportsOCIFor(ctx, base, "nginx")
}

func brewMirrorSupportsOCIFor(ctx context.Context, base, formula string) bool {
	if base == "" || formula == "" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, brewMirrorProbeTimeout)
	defer cancel()
	base = strings.TrimRight(base, "/")

	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/formula/"+formula+".json", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var man struct {
		Versions struct {
			Stable string `json:"stable"`
		} `json:"versions"`
		Bottle struct {
			Stable struct {
				Rebuild int `json:"rebuild"`
				Files   map[string]struct {
					SHA256 string `json:"sha256"`
				} `json:"files"`
			} `json:"stable"`
		} `json:"bottle"`
	}
	if json.Unmarshal(body, &man) != nil || man.Versions.Stable == "" {
		return false
	}
	for _, tag := range bottleTagsForArch() {
		f, ok := man.Bottle.Stable.Files[tag]
		if !ok || f.SHA256 == "" {
			continue
		}
		fname := brewBottleFilename(formula, man.Versions.Stable, tag, man.Bottle.Stable.Rebuild)
		cands := []string{
			base + "/" + fname,
			// 有的镜像/服务端把 `@` 原样保留而不是 %40，两种都试一次（NAS 实测两种都通）。
			base + "/" + strings.ReplaceAll(fname, "%40", "@"),
			base + brewOCIPath(formula, f.SHA256),
		}
		for _, u := range cands {
			if brewRemoteNonEmpty(pctx, u) {
				return true
			}
		}
	}
	return false
}

// probeBrewMirrors 选出这次 brew 要用的 API 域与瓶域。
//
// 抽成独立方法是为了**可测试**：默认实现会发真实网络请求，而单测不该碰真实服务
// （AGENTS.md 第三节）。测试通过 m.mirrorProbeOverride 注入一个假实现。
//
// 优先级：自建 NAS 镜像（<mirror>/brew）→ 公共镜像按序探测 → 都不行则不设瓶域。
func (m *Manager) probeBrewMirrors(ctx context.Context, probeFormula string) (apiDomain, bottleDomain string) {
	if m.mirrorProbeOverride != nil {
		return m.mirrorProbeOverride(ctx, probeFormula)
	}
	// 一次会话只探一次：探测是"选条路"，不该在每次 brew 调用上都重做一遍。
	// key 用 probeFormula（不同包的清单可用性可能不同，但同一轮安装里
	// 主要就是那几个包，命中率很高）。
	if m.mirrorProbeCache != nil {
		if v, ok := m.mirrorProbeCache[probeFormula]; ok {
			parts := strings.SplitN(v, "\x00", 2)
			if len(parts) == 2 {
				return parts[0], parts[1]
			}
		}
	}
	// 自建镜像优先：布局是 <mirror>/brew（api 在 <mirror>/brew/api，瓶在 <mirror>/brew/v2/…），
	// 由 NAS 那一侧负责同步上游瓶文件。
	if base := strings.TrimRight(strings.TrimSpace(m.opt.MirrorBase), "/"); base != "" {
		nasBase := base + "/brew"
		if brewMirrorSupportsOCI(ctx, nasBase) {
			if m.mirrorProbeCache == nil {
				m.mirrorProbeCache = map[string]string{}
			}
			m.mirrorProbeCache[probeFormula] = nasBase + "/api" + "\x00" + nasBase
			return nasBase + "/api", nasBase
		}
	}
	// 选"清单对、而且**瓶真的下得动**"的那一家，API 域与瓶域用同一家。
	//
	// 为什么不再"先按清单 200 选中一家、再单独挑瓶域"（2026-09-20 修）：
	//   · 阿里云的清单是**陈旧快照**（实测 python@3.11 清单写 3.11.12，而瓶
	//     一个都没有）—— 只按清单 200 可能选中它，brew 随后拿着过期版本号去要瓶，
	//     必然失败（多绕一圈才回落到官方源）；
	//   · 中科大在 IPv4 上对所有瓶返回 `200 + 0 字节`，只按状态码探测会把它选中。
	// 所以判据统一成"这家镜像 + 这个 formula 的瓶能取到非空内容"。
	var chosen string
	for _, c := range brewMirrorCandidates {
		if brewMirrorSupportsOCIFor(ctx, c.Base, probeFormula) {
			chosen = c.Base
			break
		}
	}
	if chosen == "" {
		// 都不行：返回空，让 brewEnv **不设**这两个变量（回落官方）。
		// 之前这里会退回 brewMirrorCandidates[0] 的 API 域 + 单独挑一个瓶域，
		// 于是"没有可用镜像"这件事被掩盖成"用了一家还行的镜像"。
		if m.mirrorProbeCache == nil {
			m.mirrorProbeCache = map[string]string{}
		}
		m.mirrorProbeCache[probeFormula] = "\x00"
		return "", ""
	}
	if m.mirrorProbeCache == nil {
		m.mirrorProbeCache = map[string]string{}
	}
	m.mirrorProbeCache[probeFormula] = chosen + "/api" + "\x00" + chosen
	return chosen + "/api", chosen
}

// BrewCapture 以真实用户身份跑一条只读 brew 命令并把输出原样返回。
//
// 为什么需要导出：面板要**直接渲染 brew 的真实状态**（`brew services list`、
// `brew outdated`…），而不是继续维护自己那张会漂移的服务表 ——
// "日志说装了、市场看不到、服务管理也没有"就是两套真相来源造成的。
// 只读命令，不做任何写操作；写操作仍走各自的安装/卸载流程。
func (m *Manager) BrewCapture(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return m.brewRun(ctx, timeout, args...)
}

// brewEnv 返回跑 brew 时要注入的环境变量。
//
// 为什么必须显式注入：install.sh 只把镜像写进用户 shell 的 rc 文件，而**面板是
// LaunchDaemon（root）**，那里面读不到用户的 rc —— 于是面板装 nginx/PHP/MySQL 时
// brew 仍然走官方源（formulae.brew.sh / ghcr.io）。国内无代理时那条路基本不通，
// 表现是"点安装后长时间没进度"，而且看起来像面板卡死。
//
// 镜像优先级（用户自己设过的**一律以用户为准**，不覆盖）：
//  1. **自建 NAS 镜像**（Cfg.MirrorBase，如 https://mirror.zizdog.com:8888）的
//     `/brew` 子路径 —— 用户明确要求"LNMP 的包也留一份在 NAS 上、优先调用"；
//  2. 中科大 / 清华 / 阿里云（按实测速度排序，见 brewMirrorCandidates）；
//  3. 都不行就不设 bottle 域，让 brew 回落官方（不会比不设更差）。
//
// probeFormula 是这次要装的第一个包（用它的清单做探针）；空表示不探测。
//
// 注意：这里选出的只是**第一个源**。真正会下载瓶的 brew install 走 brewInstall，
// 它在这次失败之后还会按 自建镜像 → 清华 → 官方源 继续换源重试
// （见文件上方"失败即换源"那一段事故说明）—— 所以"探测没选中镜像"不等于没救。
func (m *Manager) brewEnv(ctx context.Context, probeFormula string) []string {
	pick := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}

	apiDomain, bottleDomain := m.probeBrewMirrors(ctx, probeFormula)

	// 域为空 = 没有可用镜像 → **不设**这两个变量，让 brew 用官方默认。
	//
	// 为什么不能写成 `HOMEBREW_API_DOMAIN=`（空值）：Homebrew 是 Ruby 写的，
	// ENV 里存在但为空串不等于"未设置"（`ENV["X"]` 会返回 ""，仍然被当成已配置），
	// brew 会拿着空域去拼 URL。历史上的注释说"不设"，代码却在设空值 —— 这次一起修掉。
	// 公共开关（NO_AUTO_UPDATE / NO_INSTALL_CLEANUP / **NO_AUTOREMOVE**）一律走
	// brewCommonEnv：卸载绝不能顺手删掉用户没点名的包（2026-09-21 真机：卸 PHP 时
	// 带走了 net-snmp / rtmpdump）。写在**两个列表里**是刻意的 —— 这里与
	// brewCommand 各一份兜底，将来漏改一处也不会让 uninstall 退回自动清理。
	env := append([]string(nil), brewCommonEnv...)
	if v := pick("HOMEBREW_API_DOMAIN", apiDomain); v != "" {
		env = append(env, "HOMEBREW_API_DOMAIN="+v)
	}
	if v := pick("HOMEBREW_BOTTLE_DOMAIN", bottleDomain); v != "" {
		env = append(env, "HOMEBREW_BOTTLE_DOMAIN="+v)
	}
	return env
}

// installViaBrew 用 Homebrew 安装原生服务。
func (m *Manager) installViaBrew(ctx context.Context, app App, res *InstallResult) error {
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		// 全新 macOS 上 brew 与 CLT 都没有：安装脚本**刻意不装它们**
		// （CLT 的官方安装路径会弹「命令行开发者工具」GUI 对话框，且 Apple 源在国内很慢）。
		// 所以这里不能再报"请先安装 Homebrew"把用户挡在门外 —— 直接**复用面板自己的镜像链路**：
		// EnsureHomebrew 会先装 CLT（632MB 镜像分片、不弹窗）再装 brew（镜像），
		// 缺什么装什么、已就绪就秒过。qwentts / 音色接收端早就是这条路（见坑 152）。
		res.step(ctx, "未检测到 Homebrew：先自动安装「命令行开发者工具 + Homebrew」（走国内镜像，可能需要几分钟）")
		if err := m.EnsureHomebrew(ctx, res); err != nil {
			return fmt.Errorf("自动安装 Homebrew 失败：%w（也可以在面板「基础环境」里重试）", err)
		}
		if _, err := os.Stat(m.opt.BrewBin); err != nil {
			return fmt.Errorf("已尝试安装 Homebrew 但仍找不到 %s", m.opt.BrewBin)
		}
	}

	// 1) 确保包已安装
	if !m.brewHas(ctx, app.BrewFormula) {
		res.step(ctx, "正在 brew install "+app.BrewFormula+"（首次可能需要几分钟）")
		// 走 brewInstall（而不是裸 brewRun）：失败会自动换源重试，
		// 详见文件上方"失败即换源"那一段。
		if _, err := m.brewInstall(ctx, res, 30*time.Minute, app.BrewFormula); err != nil {
			return err
		}
	}
	res.step(ctx, app.BrewFormula+" 已安装")

	// 2) 交给 brew services 托管（它会写 LaunchAgent 并启动）
	//    先停再起，避免"已运行但不在 brew 管理下"的状态导致 start 报错
	//
	// 2a) MySQL 特有：数据目录为空时先初始化。
	//     不做这一步的话，brew services start 出来的 mysqld 会因为数据目录为空
	//     直接退出（tools/system-services.sh 里 prepare_mysql 就是干这个的）。
	//     initMySQLDataDir 自身幂等：数据目录非空就直接返回。
	if isMySQLFormula(app.BrewFormula) {
		if err := m.initMySQLDataDir(ctx, res); err != nil {
			return err
		}
	}
	res.step(ctx, "注册为后台服务并启动")
	if _, err := m.brewRun(ctx, 3*time.Minute, "services", "start", app.BrewFormula); err != nil {
		// 部分 formula 不支持 services（没有 service 定义），这时给出提示但不当作致命错误
		res.Warning = fmt.Sprintf("已安装，但 brew services 启动失败：%v。"+
			"可以手工前台运行，或在面板里补充启动方式", err)
	}

	// 2b) PHP-FPM 特有：装完立刻把它配置成**只监听自己专属的端点**，然后重启生效。
	//
	// 这一步与一键 LNMP 共用同一份实现（php_endpoint.go 的 ensurePHPListenEndpoint），
	// 因为"两条安装路径各自实现一遍"正是本项目踩过的坑：通用路径改了 www.conf，
	// 一键 LNMP 却还在让 fpm 听 9000，用户随后装第二个版本就互相抢端口。
	// 失败**如实报**（进 Warning + Steps），不谎报成功。
	// 端点没配好**不该**让"应用已装上"这件事变成失败：包已经装好，用户可以在
	// 「🐘 PHP 环境」里重试，Warning 里已经写清了怎么补；流程必须继续往下走
	// （登记服务），否则用户连重启它的入口都找不到。
	_ = m.ensurePHPListenEndpoint(ctx, app.BrewFormula, res)

	// 3) 读取真实的 launchd label 与日志路径
	label, plist, logPath := m.brewServiceInfo(ctx, app.BrewFormula)
	if label == "" {
		label = "homebrew.mxcl." + app.BrewFormula
	}
	if plist == "" && m.opt.UserHome != "" {
		plist = filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
	}

	// 4) 必须常驻的应用：改造成系统级 LaunchDaemon（UserName=真实用户）。
	//
	// 为什么安装路径上就要做，而不是等启动迁移：用户装完立刻重启机器是最常见的
	// 验证动作，而用户级 agent 在无头机器上开机根本不会加载（坑 130）——
	// "装完就好了"不能依赖"下次面板启动时再搬"。
	if systemDaemonNeeded(app) {
		sysLabel, sysPlist, err := systemDaemonEnsureFn(m, ctx, app, res)
		if err != nil {
			// 包已经装好、服务在用户域也能跑：**如实降级**并写清代价，绝不谎报成功。
			res.Warning = appendWarning(res.Warning,
				"已安装，但没能装成系统级服务（重启后不会自动起来）："+err.Error())
		} else {
			label, plist = sysLabel, sysPlist
		}
	}

	res.Service = &Service{
		LaunchLabel: label,
		PlistPath:   plist,
		LogPath:     logPath,
	}
	return nil
}

// brewServiceInfo 查询 brew 服务的真实 label / plist / 日志路径。
//
// 这些信息优先从 `brew services info --json` 读（权威），
// 读不到时回退到约定：label = homebrew.mxcl.<formula>，
// 日志 = ~/Library/Logs/<formula>.log（brew 的默认位置）。
func (m *Manager) brewServiceInfo(ctx context.Context, formula string) (label, plist, logPath string) {
	out, err := m.brewRun(ctx, 30*time.Second, "services", "info", formula, "--json")
	if err == nil {
		var info []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			File   string `json:"file"`
		}
		if e := json.Unmarshal([]byte(out), &info); e == nil && len(info) > 0 {
			plist = info[0].File
			if plist != "" {
				base := filepath.Base(plist)
				label = strings.TrimSuffix(base, ".plist")
			}
		}
	}
	// brew services info 拿不到时（老版本 brew、服务未启动、输出为空），
	// 按磁盘上的 plist 反推真实标签 —— 否则就会用错标签去纳管。
	if label == "" {
		label = BrewLabelFor(m.opt.UserHome, formula)
		if label != "" {
			for _, d := range []string{filepath.Join(m.opt.UserHome, "Library", "LaunchAgents"),
				"/Library/LaunchDaemons"} {
				if p := filepath.Join(d, label+".plist"); fileExists(p) {
					plist = p
					break
				}
			}
		}
	}
	if m.opt.UserHome != "" {
		logPath = filepath.Join(m.opt.UserHome, "Library", "Logs", formula+".log")
		// 有的 formula 把日志写到 .out.log / .err.log
		if _, err := os.Stat(logPath); err != nil {
			for _, suffix := range []string{".out.log", ".err.log"} {
				if p := filepath.Join(m.opt.UserHome, "Library", "Logs", formula+suffix); fileExists(p) {
					logPath = p
					break
				}
			}
		}
		// 仍然不存在时留空，让驱动回退到 plist 里的 StandardOutPath
		if !fileExists(logPath) {
			logPath = ""
		}
	}
	return label, plist, logPath
}

// installViaCompose 用 docker compose 安装服务。
func (m *Manager) installViaCompose(ctx context.Context, app App, res *InstallResult) error {
	if m.opt.DockerSocket == "" {
		return fmt.Errorf("未检测到 Docker。请先安装 OrbStack（推荐）或 Colima，然后重试")
	}
	if _, _, ok := composeBin(); !ok {
		return fmt.Errorf("未找到 docker compose 命令。请确认 Docker 安装完整（OrbStack 自带 compose）")
	}

	composeFile, err := m.prepareComposeProject(ctx, app, res)
	if err != nil {
		return err
	}
	dir := filepath.Dir(composeFile)

	// 让用户看得见"数据落在哪、镜像走哪条路"（D13 的教训就是面板说一个路径、
	// 数据却在另一个路径；D18 是"装了但不知道走的哪个源"）。
	res.step(ctx, "数据目录："+m.ColimaDataPathHint(app.ID))
	res.step(ctx, m.DockerMirrorRuntimeNote(ctx))
	if warn := m.colimaWorkDirMountWarning(); warn != "" {
		res.step(ctx, "⚠️ "+warn)
	}
	// 新机器上守护进程**一个加速源都没有**（registry-mirrors 过去只在用户点
	// "保存"时才写），而 registry-1.docker.io 在国内直连超时 —— 这时拉取必然
	// 失败。所以在真正拉取之前先自动配上；只在"一个源都没有"时才动运行时，
	// 免得每次装应用都重启一遍容器。
	if len(m.effectiveMirrors(ctx)) == 0 && m.ColimaInstalled() {
		res.step(ctx, "检测到 Docker 守护进程还没有加速源（新机器的默认状态），正在自动配置…")
		if len(m.EnsureDockerMirrorsForRuntime(ctx, res)) > 0 {
			res.step(ctx, "正在重启 Docker 运行时让加速源生效（会短暂中断已有容器，约 30-60 秒）…")
			if _, err := m.runColima(ctx, colimaStartTimeout, "restart"); err != nil {
				res.step(ctx, "⚠️ 重启运行时失败："+firstMeaningfulLine(err.Error())+
					"；仍会继续拉取，但可能超时")
			} else {
				res.step(ctx, m.DockerMirrorRuntimeNote(ctx))
			}
		}
	}
	if imgs := composeImagesOf(app.ComposeYAML); len(imgs) > 0 {
		res.step(ctx, "镜像来源逐条核对（加速源只覆盖 Docker Hub；非 Hub 镜像由守护进程直连该 registry）：")
		for _, ln := range m.ProbeComposeImageSources(ctx, imgs) {
			res.step(ctx, ln)
		}
	}

	// 启动（首次会拉镜像，给足超时）
	res.step(ctx, "正在拉取镜像并启动容器（首次可能需要几分钟）")
	drv := newComposeDriver(m.opt, &Service{ComposeFile: composeFile, Name: app.ID})
	if _, err := drv.run(ctx, 20*time.Minute, "up", "-d"); err != nil {
		return err
	}
	// 容器启动时 docker 会在项目目录下创建 ./data 这类目录（root 属主），
	// 再递归改一次归属，让 uid 1000 的容器真能写进去（D29）。
	if m.opt.UserName != "" {
		_ = chownTree(m.opt.UserName, dir)
	}
	res.step(ctx, "容器已启动")
	res.Service = &Service{ComposeFile: composeFile}

	// 有些容器镜像会在**首次启动时随机生成**管理员密码并只打在日志里
	// （File Browser 就是）。用户要在"安装完成"这一屏就看到它，
	// 而不是去「服务管理 → 日志」里翻 —— 那对不熟悉的人太难了。
	m.scrapeGeneratedCredentials(ctx, app, res)
	return nil
}

// composeEnvFileName 是 compose 项目目录里的密钥文件名。
//
// 用 docker compose 的默认约定 `.env`：compose 会自动读**项目目录**
// （即 compose 文件所在目录，也是 composeDriver 设置的 cmd.Dir）下的它，
// 所以 ComposeYAML 里的 ${VAR} 不需要额外 --env-file 参数。
const composeEnvFileName = ".env"

// prepareComposeProject 为一次 compose 安装准备磁盘内容：
// 建项目目录 → 生成/复用 .env（0600）→ 写 docker-compose.yml，返回 compose 文件路径。
//
// 为什么从 installViaCompose 里抽出来单独一个函数：这一半是**确定性、可单测**的
// （不碰 docker），另一半才是真的把容器拉起来。密钥的幂等与脱敏是本项目最容易
// 伤到用户的一条（Immich 的 DB 口令一变、数据库立刻连不上），必须有单测
// 直接锁住"这一半"，而不是靠一次真机安装去碰运气。
func (m *Manager) prepareComposeProject(ctx context.Context, app App, res *InstallResult) (string, error) {
	if strings.TrimSpace(app.ComposeYAML) == "" {
		return "", fmt.Errorf("「%s」缺少 compose 定义，无法自动安装", app.Name)
	}
	dir := filepath.Join(m.composeDir(), app.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}
	// compose 项目目录归属真实用户，方便用户自己维护。
	// **必须递归**（D29）：非 root 的容器（n8n 用 uid 1000 挂 ./data）要在
	// 项目目录下建目录、写文件，只 chown 顶层会让它的数据目录变成 root 属主，
	// 容器直接写不进去。顶层仍单独 chown 一次，保证 chownTree 遇到异常条目时
	// 顶层归属不会漏。
	if m.opt.UserName != "" {
		if uid, gid, err := lookupUser(m.opt.UserName); err == nil {
			_ = os.Chown(dir, uid, gid)
			_ = chownTree(m.opt.UserName, dir)
		}
	}

	// 1) 随机密钥：先读已有 .env，有值就复用；缺的才生成。
	creds, envChanged, err := ensureComposeEnv(dir, app)
	if err != nil {
		return "", err
	}
	if len(creds) > 0 {
		// 明文**只**进 Credentials（安装结果里那一次展示）。
		// 安全约定（硬要求）：步骤、日志、审计里都不许出现，见 InstallResult.Credentials。
		res.Credentials = append(res.Credentials, creds...)
		envPath := filepath.Join(dir, composeEnvFileName)
		if envChanged {
			res.step(ctx, "已生成随机密钥并写入 "+envPath+
				"（权限 0600；明文只在下方凭据区块里出现，不进日志）")
		} else {
			res.step(ctx, "复用已有的密钥文件 "+envPath+
				"（重装/升级不会重新生成 —— 否则数据库口令一变就连不上）")
		}
	}

	// 2) compose 文件（里面的密钥都是 ${VAR} 引用，不落明文）
	composeFile := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte(app.ComposeYAML), 0o644); err != nil {
		return "", fmt.Errorf("写入 compose 文件失败: %w", err)
	}
	res.step(ctx, "已生成 "+composeFile)
	return composeFile, nil
}

// ensureComposeEnv 保证 <dir>/.env 里每个声明的密钥都有值，返回**全部**声明的
// 当前值（新生成的与复用的都算）+ "这次有没有写盘"。
//
// 幂等契约（必须有单测锁住）：已有非空值一律复用，绝不重新生成 ——
// Immich 的 DB_PASSWORD 一变，已初始化的数据库立刻连不上。
func ensureComposeEnv(dir string, app App) (creds []Credential, changed bool, err error) {
	if len(app.ComposeSecrets) == 0 {
		return nil, false, nil
	}
	envPath := filepath.Join(dir, composeEnvFileName)
	raw := ""
	if b, rerr := os.ReadFile(envPath); rerr == nil {
		raw = string(b)
	} else if !os.IsNotExist(rerr) {
		return nil, false, fmt.Errorf("读取 %s 失败: %w", envPath, rerr)
	}
	existing := parseComposeEnv(raw)

	fresh := map[string]string{}
	for _, s := range app.ComposeSecrets {
		env := strings.TrimSpace(s.Env)
		if env == "" {
			continue
		}
		if v, ok := existing[env]; ok && strings.TrimSpace(v) != "" {
			// 复用旧值：这是"重装不能换口令"的唯一实现点。
			creds = append(creds, composeSecretCredential(s, v))
			continue
		}
		v, gerr := generateSecretValue(s)
		if gerr != nil {
			return nil, false, fmt.Errorf("为 %s 生成随机值失败: %w", env, gerr)
		}
		fresh[env] = v
		creds = append(creds, composeSecretCredential(s, v))
	}

	out, changed := mergeComposeEnv(raw, app.ComposeSecrets, fresh)
	if !changed {
		// 内容没变就别写盘（保住 mtime，也让"重装 .env 一字未动"可断言）；
		// 但权限仍要纠正一次：老版本/用户手工建的文件可能是 0644。
		_ = os.Chmod(envPath, 0o600)
		return creds, false, nil
	}
	if err := os.WriteFile(envPath, []byte(out), 0o600); err != nil {
		return nil, false, fmt.Errorf("写入 %s 失败: %w", envPath, err)
	}
	// WriteFile 只在**新建**时应用 0600，已存在的文件权限不变 → 显式 chmod。
	if err := os.Chmod(envPath, 0o600); err != nil {
		return nil, false, fmt.Errorf("设置 %s 权限失败: %w", envPath, err)
	}
	return creds, true, nil
}

// parseComposeEnv 解析 .env（KEY=VALUE，忽略空行、# 注释与可选的 export 前缀）。
func parseComposeEnv(text string) map[string]string {
	out := map[string]string{}
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimPrefix(t, "export ")
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

// mergeComposeEnv 把新生成的密钥并进已有 .env 文本：
//   - 已存在的同名行**原地替换**（保住用户的排版与其它未被面板管理的键）；
//   - 不存在的键**追加**在末尾；
//   - 没有任何要写的值时返回 changed=false（调用方据此不写盘）。
func mergeComposeEnv(raw string, secrets []ComposeSecret, add map[string]string) (string, bool) {
	if len(add) == 0 {
		return raw, false
	}
	managed := map[string]bool{}
	for _, s := range secrets {
		if env := strings.TrimSpace(s.Env); env != "" {
			managed[env] = true
		}
	}
	lines := strings.Split(raw, "\n")
	handled := map[string]bool{}
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimPrefix(t, "export ")
		k, _, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v, want := add[k]
		if !want || !managed[k] {
			continue
		}
		lines[i] = k + "=" + v
		handled[k] = true
	}
	var extra []string
	for _, s := range secrets {
		env := strings.TrimSpace(s.Env)
		if v, ok := add[env]; ok && !handled[env] {
			extra = append(extra, env+"="+v)
		}
	}
	out := strings.Join(lines, "\n")
	if len(extra) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += strings.Join(extra, "\n") + "\n"
	}
	return out, true
}

// generateSecretValue 按声明生成一个随机值。
func generateSecretValue(s ComposeSecret) (string, error) {
	n := s.Bytes
	if n < composeSecretMinBytes {
		n = composeSecretMinBytes
	}
	switch s.Encoding {
	case SecretHex:
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	case SecretBase64:
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		// RawURLEncoding：不含 + / =，能原样进 .env 与 URL
		return base64.RawURLEncoding.EncodeToString(b), nil
	case SecretPassword:
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		out := make([]byte, n)
		buf := make([]byte, n)
		// 拒绝采样，避免取模偏置。limit 用 int：byte 装不下 256，
		// 一旦字母表长度整除 256 就会溢出成 0 → 死循环。
		limit := 256 - (256 % len(alphabet))
		for i := 0; i < n; {
			if _, err := rand.Read(buf); err != nil {
				return "", err
			}
			for _, c := range buf {
				if int(c) >= limit {
					continue
				}
				out[i] = alphabet[int(c)%len(alphabet)]
				i++
				if i == n {
					break
				}
			}
		}
		return string(out), nil
	default:
		return "", fmt.Errorf("未知的密钥生成方式 %q（必须是 hex / base64 / password）", s.Encoding)
	}
}

// composeSecretCredential 把一个密钥值包成给用户看的凭据条目。
func composeSecretCredential(s ComposeSecret, value string) Credential {
	label := strings.TrimSpace(s.Label)
	if label == "" {
		label = s.Env
	}
	return Credential{Key: s.Env, Value: value, Label: label}
}

// ComposeSecretProblems 校验目录里的 compose 密钥声明是否自洽（空 = 没问题）。
//
// 静态门禁用：声明了密钥却不在 compose 里 ${} 引用、Encoding 写错、
// 长度小于下限，都会让"随机密钥"这个能力静默失效（生成了却没人用，
// 或生成一个空口令），必须在测试里拦住。
func ComposeSecretProblems(app App) []string {
	var problems []string
	seen := map[string]bool{}
	for i, s := range app.ComposeSecrets {
		where := fmt.Sprintf("%s.compose_secrets[%d]", app.ID, i)
		env := strings.TrimSpace(s.Env)
		if env == "" {
			problems = append(problems, where+": Env 为空")
			continue
		}
		if seen[env] {
			problems = append(problems, where+": Env "+env+" 重复声明")
		}
		seen[env] = true
		switch s.Encoding {
		case SecretHex, SecretBase64, SecretPassword:
		default:
			problems = append(problems, where+": Encoding 必须是 hex / base64 / password，实际 "+s.Encoding)
		}
		if s.Bytes < composeSecretMinBytes {
			problems = append(problems,
				fmt.Sprintf("%s: Bytes=%d 太小（至少 %d）", where, s.Bytes, composeSecretMinBytes))
		}
		if !strings.Contains(app.ComposeYAML, "${"+env+"}") &&
			!strings.Contains(app.ComposeYAML, "${"+env+":") {
			problems = append(problems, where+": ComposeYAML 里没有 ${"+env+"} 引用 —— "+
				"生成了也没有容器读它")
		}
	}
	if len(app.ComposeSecrets) > 0 && app.Kind != KindCompose && app.Kind != KindDocker {
		problems = append(problems, app.ID+": 只有 compose/docker 应用能用 ComposeSecrets")
	}
	return problems
}

// generatedCredRe 匹配"首次启动随机生成的管理员密码"这类日志。
//
// 目前已知的：File Browser
//
//	User 'admin' initialized with randomly generated password: zNhlM0V4cDQCsuD2
var generatedCredRe = regexp.MustCompile(`(?i)(?:user|username)[^\n]*?['"]([^'"]+)['"][^\n]*?password[:\s]+([A-Za-z0-9!@#$%^&*_+\-]{6,})`)

// scrapeGeneratedCredentials 从刚启动的容器日志里捞"随机生成的账号密码"。
//
// 为什么值得做：这些密码**只出现一次**（日志里），用户没看到就只能删库重来。
// 捞不到也不算失败 —— 有些应用不生成随机密码，那条提示就是多余的。
func (m *Manager) scrapeGeneratedCredentials(ctx context.Context, app App, res *InstallResult) {
	if app.UI == nil {
		return
	}
	// 给容器一点时间把首启日志打出来
	select {
	case <-ctx.Done():
		return
	case <-time.After(4 * time.Second):
	}
	drv := newComposeDriver(m.opt, &Service{ComposeFile: filepath.Join(m.composeDir(), app.ID, "docker-compose.yml"), Name: app.ID})
	logs, err := drv.Logs(ctx, 120)
	if err != nil || logs == "" {
		return
	}
	match := generatedCredRe.FindStringSubmatch(logs)
	if len(match) != 3 {
		return
	}
	user, pass := match[1], match[2]
	res.Steps = append(res.Steps,
		"",
		"┌─────────────────────────────────────────────┐",
		"│  "+app.Name+" 的登录账号（首次启动随机生成）  │",
		"└─────────────────────────────────────────────┘",
		"  用户名 = "+user,
		"  密  码 = "+pass,
		"",
		"  这串密码只在容器首次启动时生成一次，请立刻记下来；",
		"  登录后到设置里改成自己的密码（改完这条记录就不重要了）。",
	)
	// 同时把密码写进安装提示，安装结果与市场卡片都能看到
	res.Warning = ""
	res.Steps = append(res.Steps, "  （同一份信息也记录在服务日志里）")
}

// composeDir 返回 compose 文件存放目录。
func (m *Manager) composeDir() string {
	// 与安装脚本创建的目录保持一致：<root>/work/compose
	if m.opt.WorkDir != "" {
		return filepath.Join(m.opt.WorkDir, "compose")
	}
	return "/opt/zizpanel/work/compose"
}

// AdoptApp 把一个"纳管类"应用接入面板（不安装任何东西）。
func (m *Manager) AdoptApp(ctx context.Context, appID string) (*InstallResult, error) {
	app, ok := FindApp(appID)
	if !ok {
		return nil, fmt.Errorf("应用市场中找不到 %s", appID)
	}
	if app.AdoptLabel == "" {
		return nil, fmt.Errorf("「%s」不是「只登记已有服务」类应用", app.Name)
	}

	label := app.AdoptLabel
	plist := filepath.Join("/Library/LaunchDaemons", label+".plist")
	if !fileExists(plist) && m.opt.UserHome != "" {
		plist = filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
	}
	if !fileExists(plist) {
		return nil, fmt.Errorf("未找到 %s 对应的 launchd 服务（%s）", app.Name, label)
	}

	logPath := ""
	if plist != "" {
		logPath = plistString(plist, "StandardOutPath")
		if logPath == "" {
			logPath = plistString(plist, "StandardErrorPath")
		}
	}
	workDir := plistString(plist, "WorkingDirectory")

	port := app.Port
	health := ""
	if app.HealthPath != "" && port > 0 {
		health = fmt.Sprintf("http://127.0.0.1:%d%s", port, app.HealthPath)
	}
	svc := &Service{
		Name:        app.ID,
		DisplayName: app.Name,
		Kind:        KindNative,
		Category:    app.Category,
		Icon:        app.Icon,
		Description: app.Summary,
		Port:        port,
		LaunchLabel: label,
		PlistPath:   plist,
		WorkDir:     workDir,
		LogPath:     logPath,
		HealthURL:   health,
		Enabled:     true,
		Managed:     false, // 纳管：面板不会卸载它
		Autostart:   true,
	}
	if err := m.repo.Create(ctx, svc); err != nil {
		return nil, err
	}
	return &InstallResult{
		App: app.ID, Name: app.Name, Service: svc,
		Steps:   []string{"已接管 " + label, "日志：" + logPath},
		Message: fmt.Sprintf("「%s」已纳入面板管理（面板只做启停与查看，不会卸载它）", app.Name),
	}, nil
}

// Adopt 手工纳管一个 launchd 服务（用于"扫描到可纳管服务"后的操作）。
func (m *Manager) AdoptCandidate(ctx context.Context, label, displayName, icon string) (*Service, error) {
	if strings.TrimSpace(label) == "" {
		return nil, fmt.Errorf("缺少服务 label")
	}
	if isSystemLabel(label) {
		return nil, fmt.Errorf("系统自带服务不允许登记到面板：%s", label)
	}
	plist := filepath.Join("/Library/LaunchDaemons", label+".plist")
	if !fileExists(plist) && m.opt.UserHome != "" {
		plist = filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
	}
	// 没有 plist 也允许纳管：作业可能仍挂在 launchd 里跑着，
	// 只是 plist 文件被清掉了（实测存在这种情况）。
	// 此时状态查询走 launchctl，仍然能启停与看状态，只是没有 plist 路径。
	if !fileExists(plist) {
		// 没有 plist 时，**必须确认作业真的已被 launchd 加载**才允许纳管。
		// 否则随便传一个不存在的 label 也会"纳管成功"，
		// 在服务管理里留下一条永远查不到状态的假记录 ——
		// 这一条是测试 `TestAdoptCandidateRejectsMissing` 逼出来的。
		if !m.launchServicePresent(ctx, label) {
			return nil, fmt.Errorf("找不到 %s 的 plist，且该服务未在 launchd 中加载", label)
		}
		plist = ""
	}
	if displayName == "" {
		displayName = label
	}
	if icon == "" {
		icon = "🧩"
	}

	name := NormalizeName(label)
	// 服务名冲突时加后缀
	for i := 2; ; i++ {
		exists, err := m.repo.Exists(ctx, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			break
		}
		name = fmt.Sprintf("%s-%d", NormalizeName(label), i)
	}

	logPath := ""
	if plist != "" {
		logPath = plistString(plist, "StandardOutPath")
		if logPath == "" {
			logPath = plistString(plist, "StandardErrorPath")
		}
	}

	// 如果这个 label 在应用目录里有对应条目，继承它的端口与健康检查地址 ——
	// 否则纳管后状态显示为"运行中"但健康列永远是"未检查"，价值大打折扣。
	port, healthURL, category, description := 0, "", "custom", "由面板登记的本机服务（"+label+"）"
	if a, ok := catalogEntryForLabel(label); ok {
		port = a.WebPort()
		healthURL = healthURLFor(a)
		category = a.Category
		if a.Name != "" {
			description = a.Summary
		}
	}

	svc := &Service{
		Name:        name,
		DisplayName: displayName,
		Kind:        KindNative,
		Category:    category,
		Icon:        icon,
		Description: description,
		Port:        port,
		HealthURL:   healthURL,
		LaunchLabel: label,
		PlistPath:   plist,
		WorkDir:     plistString(plist, "WorkingDirectory"),
		LogPath:     logPath,
		Enabled:     true,
		Managed:     false,
	}
	if err := m.repo.Create(ctx, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

// PortCandidates 为新建服务推荐一个可用端口。
func (m *Manager) PortCandidates(ctx context.Context, from int, count int) []int {
	if from <= 0 {
		from = 9000
	}
	var out []int
	for p := from; p < from+200 && len(out) < count; p++ {
		info, err := priv.CheckPort(strconv.Itoa(p))
		if err != nil {
			continue
		}
		if !info.InUse {
			out = append(out, p)
		}
	}
	return out
}

// lookupUser 把用户名解析为 uid/gid。
func lookupUser(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// catalogEntryForLabel 按 launchd label 在应用目录里找条目。
//
// 两个字段都认：AdoptLabel（纯纳管类，如 ollama）与 ServiceLabel（面板自研安装器
// 注册的，如 com.zizdog.lucky / com.zizdog.frps）。
// 只认 AdoptLabel 是历史遗留 —— 后果是面板自己装出来的服务纳管后健康列永远
// "未配置"，而目录里其实写着 HealthPath（真机上 Lucky 就这么显示过）。
func catalogEntryForLabel(label string) (App, bool) {
	if label == "" {
		return App{}, false
	}
	for _, a := range Catalog() {
		if a.AdoptLabel == label || a.ServiceLabel == label {
			return a, true
		}
	}
	// 兜底：按 brew formula 后缀反查。
	//
	// 目录条目只能写**一种** ServiceLabel，但磁盘上同一台机器会混用两套 Homebrew
	// 前缀（`sh.brew.php@8.3` 与 `homebrew.mxcl.nginx` 并存），系统级改造还可能
	// 换成自研前缀（本机 nginx 的 LaunchDaemon 就是 `cn.zizdog.nginx`）。
	// 只认精确匹配的后果：按真实标签登记/纳管回来的记录丢掉端口、分类与健康地址，
	// 界面显示"未配置"—— 这就是历史上反复出现的那类问题。
	// 后缀匹配是安全的：`sh.brew.php@8.3` 只会命中 BrewFormula=php@8.3 的那条。
	for _, a := range Catalog() {
		if a.BrewFormula != "" && strings.HasSuffix(label, "."+a.BrewFormula) {
			return a, true
		}
	}
	return App{}, false
}

// FindAppByService 按服务记录找出对应的目录条目（label / 名称 / ID 三种写法都认）。
//
// 给 web 层用：服务详情要知道"这个服务在目录里声明的配置文件是哪个"，
// 才能给出「📝 编辑配置文件」入口。找不到目录条目时返回 false（纯自定义服务）。
func FindAppByService(svc *Service) (App, bool) {
	if svc == nil {
		return App{}, false
	}
	keys := []string{svc.LaunchLabel, svc.Name, svc.DisplayName}
	for _, a := range Catalog() {
		for _, k := range keys {
			if k == "" {
				continue
			}
			if a.ServiceLabel == k || a.AdoptLabel == k || a.ID == k || a.Name == k {
				return a, true
			}
		}
	}
	return App{}, false
}

// catalogAppForRecord 按**一条服务记录**在应用目录里找对应条目。
//
// 为什么单靠 catalogEntryForLabel 不够：那个函数只认 launchd 标签，而 **compose
// 记录从不写标签**（见 Install 的登记分支：KindCompose 只写 ComposeFile）。
// 结果是 compose 应用永远不参与健康地址对齐 —— 真机（mini，0.14.2）表现是
// portainer 记录停在 port=9001 / health_url=http://127.0.0.1:9001/，而 9001 当时
// 已经是 MinIO 的控制台：面板健康列照样回 code=200，检查打的是**别的应用**
// （AGENTS.md 铁律 10 的"能谎报成功"）。
//
// 判据复用仓库里已有的匹配工具，不另写一套：
//  1. FindAppByService —— label / name / display_name 三种写法都认，
//     面板安装器与纳管类都由它覆盖（compose 记录登记时 Name=目录 ID、
//     DisplayName=目录展示名，所以命中的就是同一个应用）；
//  2. compose 项目目录兜底 —— <composeDir>/<目录 ID>/docker-compose.yml。
//     记录被用户改过名、前一条匹配不上时，仍能按 compose_file 找回目录条目。
//
// 刻意**不**按 container / image 去猜：目录条目里根本没有这两个字段
// （App 只有 ComposeYAML），按镜像名反推等于自造一份与目录无关的判据，
// 正是任务里说的"完全不同的判据"。
func catalogAppForRecord(rec *Service) (App, bool) {
	if rec == nil {
		return App{}, false
	}
	if app, ok := FindAppByService(rec); ok {
		return app, true
	}
	if id := composeProjectIDFromFile(rec.ComposeFile); id != "" {
		for _, a := range Catalog() {
			if a.ID == id {
				return a, true
			}
		}
	}
	return App{}, false
}

// composeProjectIDFromFile 从 compose 文件路径里取出项目目录名（= 目录 ID）。
//
// <composeDir>/<id>/docker-compose.yml → <id>；路径为空/退化（"." 之类）时返回空，
// 绝不返回一个会到处乱匹配的伪 ID。
func composeProjectIDFromFile(file string) string {
	if strings.TrimSpace(file) == "" {
		return ""
	}
	dir := filepath.Base(filepath.Dir(file))
	if dir == "" || dir == "." || dir == ".." || dir == string(filepath.Separator) {
		return ""
	}
	return dir
}

// healthURLFor 由目录条目拼出健康检查地址（没有健康路径/端口时返回空）。
//
// 用 WebPort() 而不是 Port：有些条目的协议口与界面口不同，健康检查必须打界面口。
func healthURLFor(a App) string {
	if a.HealthPath == "" || a.WebPort() <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", a.WebPort(), a.HealthPath)
}

// RegisterInstalledService 把面板刚装好的服务登记进「服务管理」。
//
// 为什么必须自动做：用户点一次"部署"，期望的是**装完就能在服务管理里看到它**
// 并能启停/看日志。如果还要再去"可纳管"列表里手工纳管一次，那是把
// 实现的内部步骤暴露给了用户 —— 而且很容易漏（真机上就是这么反馈的）。
//
// 这里复用 AdoptCandidate（它已经从 plist 里读出路径、日志、label，
// 是经过验证的代码），而不是另写一套入库逻辑：
// 两份实现迟早会不一致，而服务记录不一致会让状态显示对不上。
//
// 幂等：已登记过（同 label）就直接返回，重复部署不会产生重复条目；
// 但"已登记却缺健康检查地址"会在这里补上 —— 否则旧版本装出来的记录
// （health.url 为空、界面显示"未配置"）永远修不好，只能让用户手工删了重装。
func (m *Manager) RegisterInstalledService(ctx context.Context, label, displayName, icon, category string, port int) error {
	if label == "" {
		return fmt.Errorf("缺少服务 label")
	}
	if app, ok := catalogEntryForLabel(label); ok {
		if hp := app.WebPort(); hp > 0 {
			port = hp
		}
		// 已存在就不再登记，但要把健康检查地址与目录**对齐**：
		//   · 目录说该有（healthURLFor 非空）→ 补上/更新；
		//   · 目录说**不该有**（HealthPath 为空，例如 Lucky 的「安全入口」会让任何
		//     路径都 404）→ 把旧记录里那条会误导的健康地址**清掉**。
		// 只补不清的话，策略一改，老记录会永远带着一条假红灯。
		hu := healthURLFor(app)
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.LaunchLabel != label {
					continue
				}
				if s.HealthURL == hu && (port <= 0 || s.Port == port) {
					return nil
				}
				s.HealthURL = hu
				if port > 0 {
					s.Port = port
				}
				return m.repo.Update(ctx, s)
			}
		}
	}
	// 已存在就不再登记 —— 这是"两个 Qwen3 TTS"那次事故的直接教训
	if list, err := m.repo.List(ctx); err == nil {
		for _, s := range list {
			if s.LaunchLabel == label {
				return nil
			}
		}
	}

	svc, err := m.AdoptCandidate(ctx, label, displayName, icon)
	if err != nil {
		return err
	}
	changed := false
	if port > 0 && svc.Port != port {
		svc.Port = port
		changed = true
	}
	if category != "" && svc.Category != category {
		svc.Category = category
		changed = true
	}
	if changed {
		return m.repo.Update(ctx, svc)
	}
	return nil
}

// firstFormulaOf 从 brew 的参数里取"首个 formula 名"，用于镜像探测。
//
// 只做最小解析：跳过 install/reinstall/upgrade 这类动作词与 -开头 的选项，
// 取第一个普通参数。取不到就返回空（brewEnv 会跳过探测、直接用第一家镜像）。
//
// 为什么不用它做别的：brew 的参数组合很多，这里只需要一个"探针包名"，
// 真正的参数解析交给 brew 自己 —— 多写一份解析只会多一份会走样的实现。
func firstFormulaOf(args []string) string {
	for _, a := range args {
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "install", "reinstall", "upgrade", "uninstall", "list", "info", "services", "start", "stop", "restart":
			continue
		}
		return a
	}
	return ""
}

// ReconcileHealthURLs 让**已存在**的服务记录的健康检查地址与目录条目对齐。
//
// 为什么需要：HealthURL 原先只在安装/登记时写一次。目录里后来补了 HealthPath
// （例如 voicereceiver 的 /health）也不会传到老记录上 —— 界面表现就是
// "面板纳管了却不监测"：服务列表里它永远 checked=false / ok=false，
// 用户看到的就是"TTS 没在管理、不知道死活"（2026-09-16 用户反馈）。
//
// 两个方向都要对齐：该补的补上，**目录里已清掉的也要清掉** ——
// 只补不清会让界面永远显示"健康检查失败"，而服务其实是好的（见
// TestHealthURLReconcilesBothWays 记录的那次真机问题）。
//
// compose 记录也必须参与（2026-09-17 修）：它们**从不写 launchd 标签**，所以
// 过去被 `LaunchLabel == ""` 整条跳过，永远不参与对齐 —— 真机（mini）的 portainer
// 记录因此停在 port=9001/http://127.0.0.1:9001/，而 9001 已经是 MinIO 的控制台，
// 面板健康列照样报绿。判据见 catalogAppForRecord；目录里已经查不到条目时
// （下架的 minio / portainer）清空 health_url，宁可显示"未配置检查地址"，
// 也不能拿一条来历不明的旧地址继续谎报成功（铁律 10）。
//
// 幂等且廉价：只在不一致时写库（值已经对就 `continue`，连 mtime 都不动），
// 每次改动都留一行日志，返回改动条数。
func (m *Manager) ReconcileHealthURLs(ctx context.Context) (int, error) {
	list, err := m.repo.List(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, s := range list {
		// ---- 原生 / 纳管类：按 launchd 标签对齐（既有行为，一字未改）----
		if s.LaunchLabel != "" {
			app, ok := catalogEntryForLabel(s.LaunchLabel)
			if !ok {
				// 标签在目录里查不到时保持原样：纳管记录的标签未必来自目录
				// （用户自己的服务也能纳管），凭"查不到"就清空会误伤。
				continue
			}
			want := healthURLFor(app)
			if s.HealthURL == want {
				continue
			}
			before := s.HealthURL
			s.HealthURL = want
			if err := m.repo.Update(ctx, s); err != nil {
				s.HealthURL = before
				continue
			}
			reconcileLog.Info("服务 %s 的健康地址按目录对齐：%q → %q", s.Name, before, want)
			changed++
			continue
		}

		// ---- compose 记录：按记录本身找目录条目（它没有标签可用）----
		if s.Kind != KindCompose {
			continue
		}
		app, ok := catalogAppForRecord(s)
		if ok && app.Kind != KindCompose {
			// 名字撞上了目录里的**原生**条目（例如用户自己用 Docker 页起了个叫
			// "nginx" 的项目）：那不是 compose 目录条目，按它对齐只会把健康检查
			// 打到真正的 nginx 上 —— 又变成"检查打的是别的应用"。当作查不到处理。
			ok = false
		}
		if !ok {
			// 目录里已经没有这个条目（例如刚下架的 minio / portainer）。
			// 旧健康地址此刻很可能正打**别的应用**，继续拿它当绿就是假成功。
			// 清空 → 面板显示"未配置检查地址"；Port 推不出来就保持不动。
			if s.HealthURL == "" {
				continue
			}
			before := s.HealthURL
			s.HealthURL = ""
			if err := m.repo.Update(ctx, s); err != nil {
				s.HealthURL = before
				continue
			}
			reconcileLog.Info("compose 记录 %s 在应用目录里已无对应条目（可能已下架）："+
				"清空健康地址 %q —— 它可能打到别的应用上，不能继续当绿", s.Name, before)
			changed++
			continue
		}

		// 目录里找到了：**不一致就覆盖**（不只是零值才补）—— 这正是本次要修的。
		// 端口取 WebPort()（界面/健康检查口），健康地址照抄 healthURLFor 的拼法。
		wantPort := app.WebPort()
		wantHealth := healthURLFor(app)
		portChanged := wantPort > 0 && s.Port != wantPort
		if !portChanged && s.HealthURL == wantHealth {
			continue // 已对齐：不写库、不搅动 mtime
		}
		beforePort, beforeHealth := s.Port, s.HealthURL
		if portChanged {
			s.Port = wantPort
		}
		s.HealthURL = wantHealth
		if err := m.repo.Update(ctx, s); err != nil {
			s.Port, s.HealthURL = beforePort, beforeHealth
			continue
		}
		if portChanged {
			reconcileLog.Info("compose 记录 %s 按目录对齐：端口 %d → %d，健康地址 %q → %q",
				s.Name, beforePort, wantPort, beforeHealth, wantHealth)
		} else {
			reconcileLog.Info("compose 记录 %s 按目录对齐：健康地址 %q → %q",
				s.Name, beforeHealth, wantHealth)
		}
		changed++
	}
	return changed, nil
}

// phpVersionFromFormula 从 brew formula 名里取出 PHP 版本号。
//
// "php@8.4" → ("8.4", true)；"php"（无版本后缀，Homebrew 的默认别名）与其它
// formula → ("", false)。刻意不认 "php"：它的实际版本随 Homebrew 漂移，
// 面板不该替用户猜一个版本号去改 www.conf。
func phpVersionFromFormula(formula string) (string, bool) {
	const p = "php@"
	if !strings.HasPrefix(formula, p) {
		return "", false
	}
	v := strings.TrimSpace(strings.TrimPrefix(formula, p))
	if v == "" {
		return "", false
	}
	return v, true
}

// appendWarning 把一条告警追加进已有的 Warning（多条用换行分隔）。
//
// 为什么需要：result.Warning 是单字符串字段，多步都可能写它；直接赋值会让
// 前一条被后一条悄悄覆盖（本项目历史上就有"登记失败被后续告警吞掉"的坑）。
func appendWarning(cur, add string) string {
	if strings.TrimSpace(cur) == "" {
		return add
	}
	return cur + "\n" + add
}
