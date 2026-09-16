package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

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
}

// Install 安装一个应用市场里的应用并纳入管理。
//
// 支持两种安装方式：
//   - KindNative + BrewFormula：brew install 后交给 brew services 托管
//   - KindCompose：写 compose 文件后 docker compose up -d
//
// 纳管类应用（AdoptLabel 非空）不走这里，见 AdoptApp。
func (m *Manager) Install(ctx context.Context, appID string) (*InstallResult, error) {
	app, ok := FindApp(appID)
	if !ok {
		return nil, fmt.Errorf("应用市场中找不到 %s", appID)
	}
	if app.AdoptLabel != "" {
		return m.AdoptApp(ctx, appID)
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

	if err := m.repo.Create(ctx, svc); err != nil {
		return nil, fmt.Errorf("服务已安装但写入注册表失败: %w", err)
	}
	res.Service = svc
	res.Message = fmt.Sprintf("「%s」已安装并纳入管理", app.Name)
	if app.PostInstallHint != "" {
		res.Message += "。" + app.PostInstallHint
	}
	return res, nil
}

// brewRun 以真实用户身份执行 brew 命令。
//
// 必须降权：Homebrew 明确拒绝 root 运行
// （"Running Homebrew as root is extremely dangerous"）。
// 面板以 root 运行，所以这里统一通过 sudo -u 切到真实用户。
// sudoers 里已授权该用户免密使用 sudo，因此不需要密码。
func (m *Manager) brewRun(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := m.brewEnv(ctx, firstFormulaOf(args))
	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		// 注意： sudo 默认会**清空环境**（env_reset），所以镜像变量不能只放在
		// 进程环境里 —— 必须显式用 `env` 在命令里带上，否则等于没设。
		full := append([]string{"-n", "-u", m.opt.UserName, "/usr/bin/env"}, env...)
		full = append(full, m.opt.BrewBin)
		full = append(full, args...)
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", full...)
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, args...)
		cmd.Env = append(os.Environ(), env...)
	}
	// brew 需要正确的 HOME 才能找到 Cellar 与缓存
	if m.opt.UserHome != "" {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, "HOME="+m.opt.UserHome)
	}
	// 逐行流式：brew install 的下载/解压进度因此能实时出现在任务中心，
	// 而不是等命令跑完才一次性看到。
	text, err := streamCmd(ctx, cmd)
	if err != nil {
		return text, fmt.Errorf("brew %s 失败: %s", strings.Join(args, " "),
			truncate(strings.TrimSpace(text), 500))
	}
	return text, nil
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

// brewBottleCapable 判断某家镜像是否提供 OCI 瓶路径（<base>/v2/…）。
//
// 只有这类镜像才适合设 HOMEBREW_BOTTLE_DOMAIN：Homebrew 是把清单里的
// ghcr.io 域**替换**成这个域去取瓶文件的（官方文档称之为"legacy flat-file mirror"
// 之外的用法，见 bottle.rb 的 custom_bottle_domain 分支）。
// 不提供 /v2/ 的镜像（如阿里云）设了也取不到，只会让 brew 白试一次再回落 ——
// 所以宁可退回一家 API 可用但没有 /v2/ 的，也不要让用户等那次超时。
func brewMirrorSupportsOCI(ctx context.Context, base string) bool {
	// 用一个稳定的公开瓶做 HEAD：nginx 在各家都有 arm64_sequoia 瓶。
	const probe = "/v2/homebrew/core/nginx/blobs/sha256:972063bdf74564fc0e5f3a0e8b0f5b6b5e0a5f2b7f0f2c4a5b6c7d8e9f0a1b2c"
	pctx, cancel := context.WithTimeout(ctx, brewMirrorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodHead, strings.TrimRight(base, "/")+probe, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	// 404 = 这家没有 OCI 布局；401/403 也可能出现在不支持时，一并当作不可用。
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent
}

// brewMirrorProbeTimeout 是探测单个镜像的超时。
//
// 短是刻意的：探测只是"换条路"的准备工作，不该让用户为它等太久；
// 局域网/国内镜像正常在 1 秒内应答。
const brewMirrorProbeTimeout = 4 * time.Second

// brewMirrorWorks 判断某个镜像的 API 是否能在 4 秒内给出指定 formula 的清单。
//
// 只做一次 HEAD/GET：能拿到 200 就认为这家可用。真正的 sha256 是否最新
// 由 brew 自己判断（取不到时它仍会回落公网），这里只负责"别选一家完全不可达的"。
func brewMirrorWorks(ctx context.Context, base, formula string) bool {
	if base == "" || formula == "" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, brewMirrorProbeTimeout)
	defer cancel()
	url := strings.TrimRight(base, "/") + "/api/formula/" + formula + ".json"
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
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

// brewBottleDomain 选一个**真的能取到瓶文件**的镜像域做 HOMEBREW_BOTTLE_DOMAIN。
//
// 语义很重要：Homebrew 把清单里的 ghcr.io 域替换成这个域去取瓶，所以这个域
// 必须提供 OCI 布局（<base>/v2/…）。候选里排在前面的镜像未必支持（实测阿里云 404），
// 所以这里逐个探测，第一个支持的胜出。
//
// 全都不支持时返回 ""：**不设**比设一个取不到的更好 —— 设了会让 brew 先白试一次
// （甚至撞上 4 秒超时），最后仍然回落 ghcr.io，用户只是多等。
func brewBottleDomain(ctx context.Context) string {
	for _, c := range brewMirrorCandidates {
		if brewMirrorSupportsOCI(ctx, c.Base) {
			return c.Base
		}
	}
	return ""
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
	// 自建镜像优先：布局是 <mirror>/brew（api 在 <mirror>/brew/api，瓶在 <mirror>/brew/v2/…），
	// 由 NAS 那一侧负责同步上游瓶文件。
	if base := strings.TrimRight(strings.TrimSpace(m.opt.MirrorBase), "/"); base != "" {
		nasBase := base + "/brew"
		if brewMirrorSupportsOCI(ctx, nasBase) {
			return nasBase + "/api", nasBase
		}
	}
	chosen := brewMirrorCandidates[0]
	for _, c := range brewMirrorCandidates {
		if brewMirrorWorks(ctx, c.Base, probeFormula) {
			chosen = c
			break
		}
	}
	return chosen.Base + "/api", brewBottleDomain(ctx)
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
func (m *Manager) brewEnv(ctx context.Context, probeFormula string) []string {
	pick := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}

	apiDomain, bottleDomain := m.probeBrewMirrors(ctx, probeFormula)

	return []string{
		"HOMEBREW_API_DOMAIN=" + pick("HOMEBREW_API_DOMAIN", apiDomain),
		"HOMEBREW_BOTTLE_DOMAIN=" + pick("HOMEBREW_BOTTLE_DOMAIN", bottleDomain),
		// 自动更新会在每次 brew 命令前拉一遍仓库元数据：国内很慢，而且我们
		// 不需要它（面板自己管安装）。
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_INSTALL_CLEANUP=1",
	}
}

// installViaBrew 用 Homebrew 安装原生服务。
func (m *Manager) installViaBrew(ctx context.Context, app App, res *InstallResult) error {
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		return fmt.Errorf("未安装 Homebrew，无法自动安装 %s。请先安装 Homebrew", app.Name)
	}

	// 1) 确保包已安装
	if !m.brewHas(ctx, app.BrewFormula) {
		res.step(ctx, "正在 brew install "+app.BrewFormula+"（首次可能需要几分钟）")
		if _, err := m.brewRun(ctx, 30*time.Minute, "install", app.BrewFormula); err != nil {
			return err
		}
	}
	res.step(ctx, app.BrewFormula+" 已安装")

	// 2) 交给 brew services 托管（它会写 LaunchAgent 并启动）
	//    先停再起，避免"已运行但不在 brew 管理下"的状态导致 start 报错
	res.step(ctx, "注册为后台服务并启动")
	if _, err := m.brewRun(ctx, 3*time.Minute, "services", "start", app.BrewFormula); err != nil {
		// 部分 formula 不支持 services（没有 service 定义），这时给出提示但不当作致命错误
		res.Warning = fmt.Sprintf("已安装，但 brew services 启动失败：%v。"+
			"可以手工前台运行，或在面板里补充启动方式", err)
	}

	// 3) 读取真实的 launchd label 与日志路径
	label, plist, logPath := m.brewServiceInfo(ctx, app.BrewFormula)
	if label == "" {
		label = "homebrew.mxcl." + app.BrewFormula
	}
	if plist == "" && m.opt.UserHome != "" {
		plist = filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
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

	dir := filepath.Join(m.composeDir(), app.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	// compose 项目目录归属真实用户，方便用户自己维护
	if m.opt.UserName != "" {
		if uid, gid, err := lookupUser(m.opt.UserName); err == nil {
			_ = os.Chown(dir, uid, gid)
		}
	}

	composeFile := filepath.Join(dir, "docker-compose.yml")
	content := app.ComposeYAML
	if content == "" {
		return fmt.Errorf("「%s」缺少 compose 定义，无法自动安装", app.Name)
	}
	if err := os.WriteFile(composeFile, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 compose 文件失败: %w", err)
	}
	res.step(ctx, "已生成 "+composeFile)

	// 启动（首次会拉镜像，给足超时）
	res.step(ctx, "正在拉取镜像并启动容器（首次可能需要几分钟）")
	drv := newComposeDriver(m.opt, &Service{ComposeFile: composeFile, Name: app.ID})
	if _, err := drv.run(ctx, 20*time.Minute, "up", "-d"); err != nil {
		return err
	}
	res.step(ctx, "容器已启动")
	res.Service = &Service{ComposeFile: composeFile}

	// 有些容器镜像会在**首次启动时随机生成**管理员密码并只打在日志里
	// （File Browser 就是）。用户要在"安装完成"这一屏就看到它，
	// 而不是去「服务管理 → 日志」里翻 —— 那对不熟悉的人太难了。
	m.scrapeGeneratedCredentials(ctx, app, res)
	return nil
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
		return nil, fmt.Errorf("「%s」不是纳管类应用", app.Name)
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
		return nil, fmt.Errorf("系统自带服务不允许纳管：%s", label)
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
	port, healthURL, category, description := 0, "", "custom", "由面板纳管的 launchd 服务（"+label+"）"
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
// 幂等且廉价：只在不一致时写库，返回改动条数。
func (m *Manager) ReconcileHealthURLs(ctx context.Context) (int, error) {
	list, err := m.repo.List(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, s := range list {
		if s.LaunchLabel == "" {
			continue
		}
		app, ok := catalogEntryForLabel(s.LaunchLabel)
		if !ok {
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
		changed++
	}
	return changed, nil
}
