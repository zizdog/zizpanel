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

// bottleTags 是本机可能用到的 Homebrew 瓶 tag，按新到旧排列。
//
// 探测时必须拿它去清单里找**真实存在**的瓶 —— 不能硬编码 sha256：
// 之前那个常量是我编的，HEAD 永远 404，导致 NAS/镜像分支永不成立、
// 静默回落 ghcr.io（用户"装了 21 分钟"的真因，2026-09-16 NAS 侧实测确认）。
var bottleTags = []string{"arm64_sequoia", "arm64_tahoe", "arm64_sonoma", "arm64_ventura"}

// brewMirrorSupportsOCI 判断某家镜像**真的能取到瓶文件**。
//
// 刻意贴近 Homebrew 自身行为，而不是拍一个固定 URL：
//  1. GET <base>/api/formula/<formula>.json，按本机 tag 取真实 version 与 sha256；
//  2. 依次试 brew 实际会用的两种瓶路径：
//     legacy 平铺 <base>/<formula>-<version>.<tag>.bottle.tar.gz
//     （自定义 HOMEBREW_BOTTLE_DOMAIN 时 Homebrew 7 走的就是这条）
//     OCI 布局 <base>/v2/homebrew/core/<formula>/blobs/sha256:<hex>
//  3. 任一返回 200/206 即认为可用。
//
// 这样镜像内容一变（升级、清老瓶）探测不会失效，也不需要维护常量。
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
				Files map[string]struct {
					SHA256 string `json:"sha256"`
				} `json:"files"`
			} `json:"stable"`
		} `json:"bottle"`
	}
	if json.Unmarshal(body, &man) != nil || man.Versions.Stable == "" {
		return false
	}
	for _, tag := range bottleTags {
		f, ok := man.Bottle.Stable.Files[tag]
		if !ok || f.SHA256 == "" {
			continue
		}
		cands := []string{
			base + "/" + formula + "-" + man.Versions.Stable + "." + tag + ".bottle.tar.gz",
			base + "/v2/homebrew/core/" + formula + "/blobs/sha256:" + f.SHA256,
		}
		for _, u := range cands {
			hreq, herr := http.NewRequestWithContext(pctx, http.MethodHead, u, nil)
			if herr != nil {
				continue
			}
			hresp, herr := http.DefaultClient.Do(hreq)
			if herr != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(hresp.Body, 1<<12))
			_ = hresp.Body.Close()
			if hresp.StatusCode == http.StatusOK || hresp.StatusCode == http.StatusPartialContent {
				return true
			}
		}
	}
	return false
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
	chosen := brewMirrorCandidates[0]
	for _, c := range brewMirrorCandidates {
		if brewMirrorWorks(ctx, c.Base, probeFormula) {
			chosen = c
			break
		}
	}
	api, bottle := chosen.Base+"/api", brewBottleDomain(ctx)
	if m.mirrorProbeCache == nil {
		m.mirrorProbeCache = map[string]string{}
	}
	m.mirrorProbeCache[probeFormula] = api + "\x00" + bottle
	return api, bottle
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

	dir := filepath.Join(m.composeDir(), app.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
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

	composeFile := filepath.Join(dir, "docker-compose.yml")
	content := app.ComposeYAML
	if content == "" {
		return fmt.Errorf("「%s」缺少 compose 定义，无法自动安装", app.Name)
	}
	if err := os.WriteFile(composeFile, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 compose 文件失败: %w", err)
	}
	res.step(ctx, "已生成 "+composeFile)

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
