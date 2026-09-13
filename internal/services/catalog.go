package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// App 是应用市场里的一个可安装应用。
//
// 一个 App 描述"怎么把这个应用装起来并纳入管理"，它本身不落库；
// 安装成功后会在 services 表里生成一条 managed=true 的记录。
type App struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Category    string `json:"category"`
	// Kind 决定用哪种方式安装与管理
	Kind Kind `json:"kind"`
	// AdoptLabel 非空表示这是一个"纳管"应用：不安装，只把已存在的服务接进来
	AdoptLabel string `json:"adopt_label"`

	Port        int    `json:"port"`
	HealthPath  string `json:"health_path"`
	HealthExact string `json:"health_exact"`
	LogPath     string `json:"log_path"`

	// Native 安装：brew 包名
	BrewFormula string `json:"brew_formula"`
	// ServiceLabel 是面板自研安装器注册的 launchd label。
	// 有了它，"已安装"判断才能对这种应用生效 —— 它既不在 brew 里，
	// 面板记录里的名字也未必等于目录 ID（如记录名 com-zizdog-qwen3tts、
	// 目录 ID qwen3tts），不给出 label 就只能一直显示"未安装"。
	ServiceLabel string `json:"service_label,omitempty"`
	// PanelInstaller 非空表示这个应用由**面板自己的安装器**部署
	// （要建虚拟环境、改 nginx、注册系统级守护进程等，通用 brew/compose
	// 流程做不了），值即安装器标识，web 层据此分流。
	// 有了它，目录校验就不该再要求这类条目必须有 BrewFormula。
	PanelInstaller string `json:"panel_installer,omitempty"`
	// Compose 安装：compose 文件内容
	ComposeYAML string `json:"compose_yaml"`
	// 手工安装提示（无法自动安装时给出命令）
	ManualHint string `json:"manual_hint"`

	// Requires 是安装前置条件（用于预检查）
	Requires []Requirement `json:"requires"`
	// PostInstallHint 安装后要做的事（例如拉模型）
	PostInstallHint string `json:"post_install_hint"`
	// DocsURL 官方文档
	DocsURL string `json:"docs_url"`
}

// Requirement 是一条前置条件。
type Requirement struct {
	// Type: brew / command / docker / font / python
	Type  string `json:"type"`
	Value string `json:"value"`
	Hint  string `json:"hint"`
}

// Preflight 是一次安装前检查的结果。
type Preflight struct {
	App      string        `json:"app"`
	Ready    bool          `json:"ready"`
	Checks   []CheckResult `json:"checks"`
	PortFree bool          `json:"port_free"`
	PortNote string        `json:"port_note"`
}

// CheckResult 是单条检查结果。
type CheckResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	Fixable bool   `json:"fixable"`
	FixCmd  string `json:"fix_cmd"`
}

// Catalog 返回内置应用目录。
//
// 选品原则：优先"这台机器上真的用得上、且能自动装起来"的。
// 每个条目的端口、健康检查路径、日志路径都经过实际验证，
// 而不是从文档里抄一个大概 —— 端口写错会让用户装完发现打不开。
func Catalog() []App {
	audioHealth := "/v1/models"
	return []App{
		// ---------------- 面板一键部署（自研安装器） ----------------
		//
		// 这几个项目的安装逻辑不是通用的 brew/compose 流程，而是本项目
		// 自己的安装器（见 internal/services 里的 iopaint.go / qwentts.go /
		// voicereceiver.go / phpmyadmin.go）。写进目录是为了让它们在
		// **应用市场里可见、状态可追踪**，而不是只藏在页头的按钮里 ——
		// 用户找不到的功能等于不存在。
		//
		// 真正的安装由 handleMarketInstall 按 ID 路由到对应安装器
		// （见 PanelInstaller 字段）；"已安装"则靠 ServiceLabel
		// 或 BrewFormula 判断。
		{
			ID: "iopaint", Name: "IOPaint（图片去水印）", Icon: "🖼️",
			Summary: "AI 擦除水印与杂物，支持批量与视频",
			Description: "上传图片 → 涂抹要去掉的水印 → 擦除。使用 LaMa 模型并启用 " +
				"Apple Silicon MPS 加速，单进程、模型仅约 200MB。" +
				"界面里可切换更强模型（如 PowerPaint），但 MAT / ZITS / LDM 在 M 系列上不支持 MPS。",
			Category: "tool", Kind: KindNative, PanelInstaller: "iopaint", ServiceLabel: "com.zizdog.iopaint",
			Port:    8080,
			DocsURL: "https://github.com/Sanster/IOPaint",
		},
		{
			ID: "qwen3tts", Name: "Qwen3 TTS（语音合成）", Icon: "🗣️",
			Summary: "本地语音合成，支持音色克隆",
			Description: "按 TtsVoice 插件的部署契约安装：Python 3.11 环境 + mlx-audio[server] " +
				"+ 两个模型（Base 克隆 / CustomVoice 预置音色，各约 1.9GB）。" +
				"两个模型都常驻内存，网站按请求里的 model 字段随时切换、无需重载。" +
				"可选择是否加鉴权；加了鉴权时 Qwen 只监听本机，" +
				"对外由音色接收端（8899）提供带密钥的反向代理。",
			Category: "ai", Kind: KindNative, PanelInstaller: "qwen3tts", ServiceLabel: "com.zizdog.qwen3tts",
			Port:       8880,
			HealthPath: audioHealth,
			DocsURL:    "https://github.com/Blaizzy/mlx-audio",
		},
		{
			ID: "voicereceiver", Name: "TtsVoice 音色接收端", Icon: "🔐",
			Summary: "接收音色样本 + 带鉴权的反向代理",
			Description: "给网站插件用的对外入口：接收上传的音色样本（落到本机，" +
				"因为上游只认本地文件路径），并把 /v1/* 转发给只监听本机的 8880。" +
				"支持指定共享密钥或自动生成。GET /voice/status 可查上游模型驻留状态。",
			Category: "ai", Kind: KindNative, PanelInstaller: "voicereceiver", ServiceLabel: "com.zizdog.voicereceiver",
			Port: 8899,
			// 契约来源是网站侧插件目录里的 HANDOFF-TO-MINI.md。
			// 这里原先错填成 phpmyadmin.net（复制粘贴残留），会把人引到无关文档。
			DocsURL: "https://github.com/Blaizzy/mlx-audio",
		},
		{
			ID: "phpmyadmin", Name: "phpMyAdmin", Icon: "🐬",
			Summary: "数据库管理界面（推荐入口）",
			Description: "面板自研的库表管理功能有限，日常的库/表/权限/导入导出建议用 phpMyAdmin。" +
				"装好后接入 nginx 默认站点，访问 http://<本机地址>/phpmyadmin/。" +
				"面板内置那套保留为应急入口。",
			Category: "tool", Kind: KindNative, PanelInstaller: "phpmyadmin", BrewFormula: "phpmyadmin",
			Port:       0,
			HealthPath: "/phpmyadmin/",
			DocsURL:    "https://www.phpmyadmin.net",
		},

		// ---------------- 网站环境（LNMP，原生安装） ----------------
		//
		// 为什么这三条必须存在：面板最核心的功能是"网站管理"与"数据库"，
		// 而它们依赖 nginx / PHP-FPM / MySQL。早期版本的安装脚本只提示
		// "去应用市场装"，可市场里根本没有这三条 —— 用户会在那里白找一圈，
		// 然后发现面板的主功能是空的。安装路径（brew install + brew services）
		// 早就有了，缺的只是条目。
		//
		// 顺序有意义：nginx 在最前，它是网站功能的入口。
		{
			ID: "nginx", Name: "Nginx", Icon: "🌐",
			Summary: "Web 服务器，网站管理功能的基础",
			Description: "面板的「网站管理」依赖它：新建站点、伪静态、SSL、反向代理" +
				"都是往它的 vhosts 目录写配置并 reload。" +
				"装好后面板会自动补齐 include 上下文与 WebSocket 升级映射。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.nginx",
			Port: 80, HealthPath: "/",
			BrewFormula: "nginx",
			LogPath:     "~/Library/Logs/homebrew.mxcl.nginx.log",
			DocsURL:     "https://nginx.org",
		},
		{
			ID: "php83", Name: "PHP 8.3 (FPM)", Icon: "🐘",
			Summary: "PHP FastCGI 进程管理器，供站点解析 PHP",
			Description: "以 FastCGI 方式监听 127.0.0.1:9000，由 nginx 转发 PHP 请求。" +
				"面板的站点配置默认指向这个地址；多版本 PHP 可以再装其它版本共存。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "homebrew.mxcl.php@8.3",
			Port: 9000,
			// PHP-FPM 说的是 FastCGI 协议，不是 HTTP —— 不能做 HTTP 健康检查，
			// 否则会永远显示不健康。留空表示"只按进程与端口判断"。
			HealthPath:  "",
			BrewFormula: "php@8.3",
			LogPath:     "~/Library/Logs/homebrew.mxcl.php@8.3.log",
			DocsURL:     "https://www.php.net",
		},
		{
			ID: "mysql84", Name: "MySQL 8.4", Icon: "🐬",
			Summary: "关系型数据库，供站点与面板的数据库管理使用",
			Description: "面板的「数据库管理」通过它管理库、表、账号与导入导出。" +
				"macOS 上 brew 版默认用 /tmp/mysql.sock，root 初始无密码。",
			Category: "lnmp", Kind: KindNative, ServiceLabel: "sh.brew.mysql@8.4",
			Port: 3306, HealthPath: "",
			BrewFormula: "mysql@8.4",
			LogPath:     "~/Library/Logs/homebrew.mxcl.mysql@8.4.log",
			DocsURL:     "https://dev.mysql.com",
		},

		// ---------------- AI 服务（原生，用 Metal 加速） ----------------
		{
			ID: "ollama", Name: "Ollama", Icon: "🦙",
			Summary: "本地大模型推理，支持 Metal 加速",
			Description: "一行命令跑起本地大模型（Llama / Qwen / DeepSeek 等）。" +
				"原生安装才能用 Apple Silicon 的 GPU 加速，Docker 里用不了 Metal。",
			Category: "ai", Kind: KindNative,
			Port: 11434, HealthPath: "/api/tags",
			BrewFormula: "ollama", LogPath: "~/.ollama/ollama.log",
			PostInstallHint: "安装后执行 ollama pull qwen2.5:7b 下载一个模型即可开始使用",
			Requires: []Requirement{
				{Type: "brew_formula", Value: "ollama", Hint: "brew install ollama"},
			},
			DocsURL: "https://ollama.com",
		},

		// ---------------- 容器运行时 ----------------
		//
		// 放在所有 Docker 应用之前：它是那些应用的前提。面板不把它当普通应用，
		// 而是同时登记进「服务管理」，让它像 nginx/MySQL 一样可启停、看日志。
		{
			ID: "docker-runtime", Name: "Docker 运行时（Colima）", Icon: "🐳",
			Summary: "容器引擎，Docker 类应用的前提",
			Description: "Colima 在轻量 Linux 虚拟机里跑 Docker 引擎，原生支持 Apple Silicon。" +
				"装好后开机自动启动，无需登录桌面。停止它会让所有容器一起停掉。",
			Category: "runtime", Kind: KindColima, PanelInstaller: "docker-runtime",
			// BrewFormula 用于判断"装没装"；ServiceLabel 用于判断"纳没纳管"。
			// 两者都要给，否则市场会显示"已安装·未纳管"并给出一个点了会报错的纳管按钮。
			BrewFormula: "colima", ServiceLabel: ColimaLaunchLabel,
			DocsURL: "https://github.com/abiosoft/colima",
		},
		// ---------------- 运维工具（Docker） ----------------
		{
			ID: "uptime-kuma", Name: "Uptime Kuma", Icon: "📡",
			Summary:     "自托管服务监控与告警",
			Description: "监控网站与服务的可用性，支持多种通知渠道（Telegram / Bark / 邮件等）。",
			Category:    "tool", Kind: KindCompose, Port: 3001,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker 运行时（Colima）"}},
			ComposeYAML: composeTemplate("uptime-kuma", "louislam/uptime-kuma:1", 3001, 3001, `
    volumes:
      - ./data:/app/data
    restart: unless-stopped`),
			DocsURL: "https://github.com/louislam/uptime-kuma",
		},
		{
			ID: "minio", Name: "MinIO", Icon: "🪣",
			Summary: "S3 兼容的对象存储",
			// 刻意避开 9000：那是 PHP-FPM 的固定端口（站点 vhost 都指向
			// 127.0.0.1:9000），MinIO 默认也用 9000，两者会真的抢端口 ——
			// 在装了 PHP 的机器上 MinIO 会直接起不来。
			// PHP 的 9000 是约定不能动，所以让路的是 MinIO。
			Description: "自建对象存储，适合存放图片、备份、模型文件。" +
				"API 用 9010、控制台用 9011（避开 PHP-FPM 占用的 9000）。",
			Category: "tool", Kind: KindCompose, Port: 9010,
			HealthPath: "/minio/health/live",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: `services:
  minio:
    image: quay.io/minio/minio:latest
    container_name: minio
    ports:
      # 宿主机改用 9010/9011：9000 留给 PHP-FPM
      - "9010:9000"
      - "9011:9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    command: server /data --console-address ":9001"
    volumes:
      - ./data:/data
    restart: unless-stopped
`,
			PostInstallHint: "默认账号密码均为 minioadmin，请登录后立即修改。控制台：http://127.0.0.1:9001",
			DocsURL:         "https://min.io",
		},
		{
			ID: "n8n", Name: "n8n", Icon: "🔗",
			Summary:     "可视化自动化工作流",
			Description: "用节点拖拽的方式编排自动化流程，可以对接 HTTP / 数据库 / AI 接口。",
			Category:    "tool", Kind: KindCompose, Port: 5678,
			HealthPath: "/healthz",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: composeTemplate("n8n", "docker.n8n.io/n8nio/n8n:latest", 5678, 5678, `
    environment:
      - N8N_SECURE_COOKIE=false
      - GENERIC_TIMEZONE=Asia/Shanghai
    volumes:
      - ./data:/home/node/.n8n
    restart: unless-stopped`),
			DocsURL: "https://n8n.io",
		},
		{
			ID: "gitea", Name: "Gitea", Icon: "🍵",
			Summary:     "轻量自建 Git 服务",
			Description: "资源占用极小的 Git 托管（含 Web 界面、Issue、CI 入口）。",
			Category:    "tool", Kind: KindCompose, Port: 3000,
			HealthPath: "/api/healthz",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: composeTemplate("gitea", "gitea/gitea:latest", 3000, 3000, `
    environment:
      - USER_UID=1000
      - USER_GID=1000
    volumes:
      - ./data:/data
      - /etc/timezone:/etc/timezone:ro
      - /etc/localtime:/etc/localtime:ro
    restart: unless-stopped`),
			DocsURL: "https://gitea.com",
		},
		{
			ID: "stirling-pdf", Name: "Stirling PDF", Icon: "📄",
			Summary:     "本地 PDF 工具箱",
			Description: "合并、拆分、压缩、OCR、转图片等 PDF 操作，全部在本机完成，不上传云端。",
			Category:    "tool", Kind: KindCompose, Port: 8082,
			HealthPath: "/",
			Requires:   []Requirement{{Type: "docker", Hint: "需要安装 Docker"}},
			ComposeYAML: composeTemplate("stirling-pdf", "stirlingtools/stirling-pdf:latest", 8082, 8080, `
    volumes:
      - ./data:/configs
    restart: unless-stopped`),
			DocsURL: "https://github.com/Stirling-Tools/Stirling-PDF",
		},
	}
}

// composeTemplate 生成一个最小可用的 compose 文件。
//
// 统一用 `container_name` 固定容器名：面板按名字管理容器，
// 不固定的话 compose 会加项目前缀，重启后名字变化导致管理失效。
func composeTemplate(container, image string, hostPort, containerPort int, extra string) string {
	return fmt.Sprintf(`services:
  %s:
    image: %s
    container_name: %s
    ports:
      - "%d:%d"%s
`, container, image, container, hostPort, containerPort, extra)
}

// FindApp 按 ID 查目录。
func FindApp(id string) (App, bool) {
	for _, a := range Catalog() {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// Preflight 检查一个应用能否安装。
//
// 为什么必须做预检查：用户点了"安装"再等几分钟才发现缺 Docker / 缺 brew，
// 体验很差。这里把所有前置条件一次列清楚，并给出可直接复制执行的修复命令。
func (m *Manager) Preflight(ctx context.Context, app App) Preflight {
	pf := Preflight{App: app.ID, Checks: []CheckResult{}}

	// 纳管类应用不做端口占用检查：服务本来就在运行，
	// 端口被它自己占用是预期状态，报"冲突"会误导用户。
	if app.AdoptLabel != "" {
		pf.PortFree = true
		pf.PortNote = "纳管已有服务，无需检查端口占用"
		for _, req := range app.Requires {
			pf.Checks = append(pf.Checks, m.checkRequirement(ctx, req))
		}
		pf.Ready = true
		for _, c := range pf.Checks {
			if !c.OK {
				pf.Ready = false
			}
		}
		return pf
	}

	// 端口占用检查
	if app.Port > 0 {
		if info, err := priv.CheckPort(strconv.Itoa(app.Port)); err == nil {
			if info.InUse {
				// 已被本面板管理的服务占用不算冲突
				names, _ := m.repo.CountByPort(ctx, app.Port, "")
				if len(names) > 0 {
					pf.PortFree = false
					pf.PortNote = fmt.Sprintf("端口 %d 已被面板管理的服务占用：%s", app.Port, strings.Join(names, ", "))
				} else {
					pf.PortFree = false
					pf.PortNote = fmt.Sprintf("端口 %d 已被其它进程占用：%s",
						app.Port, strings.Join(info.Holders, ", "))
				}
			} else {
				pf.PortFree = true
				pf.PortNote = fmt.Sprintf("端口 %d 空闲", app.Port)
			}
		}
	} else {
		pf.PortFree = true
	}

	// 依赖检查
	for _, req := range app.Requires {
		pf.Checks = append(pf.Checks, m.checkRequirement(ctx, req))
	}

	pf.Ready = pf.PortFree
	for _, c := range pf.Checks {
		if !c.OK && !c.Fixable {
			pf.Ready = false
		}
	}
	// 端口冲突但冲突方是面板管理的服务时，视为可安装（用户可以改端口）
	if !pf.PortFree {
		pf.Ready = false
	}
	return pf
}

func (m *Manager) checkRequirement(ctx context.Context, req Requirement) CheckResult {
	switch req.Type {
	case "docker":
		if m.opt.DockerSocket != "" {
			return CheckResult{Name: "Docker", OK: true, Detail: "已检测到 Docker socket: " + m.opt.DockerSocket}
		}
		return CheckResult{
			Name: "Docker", OK: false, Detail: "未检测到 Docker",
			Fixable: false,
			FixCmd:  "brew install colima docker docker-compose",
		}

	case "brew_formula":
		if m.brewHas(ctx, req.Value) {
			return CheckResult{Name: "Homebrew 包 " + req.Value, OK: true, Detail: "已安装"}
		}
		if _, err := os.Stat(m.opt.BrewBin); err != nil {
			return CheckResult{
				Name: "Homebrew", OK: false, Detail: "未安装 Homebrew",
				Fixable: false,
				FixCmd:  `brew install ` + req.Value,
			}
		}
		return CheckResult{
			Name: "Homebrew 包 " + req.Value, OK: false,
			Detail:  "未安装（安装时会自动执行 brew install " + req.Value + "）",
			Fixable: true,
			FixCmd:  "brew install " + req.Value,
		}

	case "launchd":
		plist := filepath.Join("/Library/LaunchDaemons", req.Value+".plist")
		if _, err := os.Stat(plist); err == nil {
			return CheckResult{Name: "服务 " + req.Value, OK: true, Detail: "已存在 " + plist}
		}
		if m.opt.UserHome != "" {
			agent := filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", req.Value+".plist")
			if _, err := os.Stat(agent); err == nil {
				return CheckResult{Name: "服务 " + req.Value, OK: true, Detail: "已存在 " + agent}
			}
		}
		return CheckResult{Name: "服务 " + req.Value, OK: false, Detail: "未找到该 launchd 服务"}

	case "command":
		if p, err := exec.LookPath(req.Value); err == nil {
			return CheckResult{Name: "命令 " + req.Value, OK: true, Detail: p}
		}
		return CheckResult{Name: "命令 " + req.Value, OK: false, Detail: "未找到", Fixable: true, FixCmd: req.Hint}

	default:
		return CheckResult{Name: req.Type, OK: true, Detail: "无需检查"}
	}
}

// brewHas 判断某个 Homebrew 包是否已安装。
//
// homebrew 拒绝以 root 运行，而面板以 root 运行，
// 因此必须降权到真实用户执行（与安装脚本里的处理一致）。
// InstalledFormulas 一次性返回已安装的 formula 集合。
//
// 为什么要"一次性"：`brew list --versions <单个>` 和
// `brew list --versions`（全部）耗时几乎一样（实测 0.5s vs 0.58s）——
// 因为开销主要在 brew 自身启动，不在查询。所以逐个查 N 次
// 等于白付 N 次启动成本；市场列表有十几个条目，累加就是几秒。
func (m *Manager) InstalledFormulas(ctx context.Context) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	set := map[string]bool{}
	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "-u", m.opt.UserName,
			m.opt.BrewBin, "list", "--versions")
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, "list", "--versions")
	}
	out, err := cmd.Output()
	if err != nil {
		return set
	}
	// 输出形如：nginx 1.31.5 / php@8.3 8.3.33
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 {
			set[f[0]] = true
		}
	}
	return set
}

// HasBrewFormula 报告某个 formula 是否已由 Homebrew 安装。
//
// 导出它是因为 web 层要判断"应用市场里这个应用是否已经装过" ——
// 只看面板自己的服务记录是不够的（用户可能早就用 brew 装好了），
// 真机上就出现过"装好的 nginx 在市场里还显示安装按钮"。
func (m *Manager) HasBrewFormula(ctx context.Context, formula string) bool {
	return m.brewHas(ctx, formula)
}

func (m *Manager) brewHas(ctx context.Context, formula string) bool {
	if formula == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "-u", m.opt.UserName,
			m.opt.BrewBin, "list", "--versions", formula)
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, "list", "--versions", formula)
	}
	return cmd.Run() == nil
}

// ScanAdoptable 扫描本机上"可以纳管"但还没纳入的服务。
//
// 扫描范围：/Library/LaunchDaemons 与用户的 LaunchAgents，
// 排除系统自带（com.apple.*）与已被纳管的。
func (m *Manager) ScanAdoptable(ctx context.Context) ([]AdoptCandidate, error) {
	known := map[string]bool{}
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		if s.LaunchLabel != "" {
			known[s.LaunchLabel] = true
		}
	}

	candidates := []AdoptCandidate{}
	dirs := []string{"/Library/LaunchDaemons"}
	if m.opt.UserHome != "" {
		dirs = append(dirs, filepath.Join(m.opt.UserHome, "Library", "LaunchAgents"))
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".plist") {
				continue
			}
			label := strings.TrimSuffix(name, ".plist")
			if known[label] {
				continue
			}
			if isSystemLabel(label) || isSelfLabel(label) {
				continue
			}
			path := filepath.Join(dir, name)
			c := AdoptCandidate{Label: label, PlistPath: path}
			c.Program = plistString(path, "Program")
			if c.Program == "" {
				// ProgramArguments 的第一项就是可执行文件
				c.Program = plistString(path, "ProgramArguments:0")
			}
			c.DisplayName = label
			c.LogPath = plistString(path, "StandardOutPath")
			if c.LogPath == "" {
				c.LogPath = plistString(path, "StandardErrorPath")
			}
			// 读运行状态
			if st, err := priv.LaunchStatus(label); err == nil {
				c.Running = st.Running
				c.PID = st.PID
			}
			candidates = append(candidates, c)
		}
	}

	// 补充：**已加载但没有 plist 的作业**。
	//
	// 只看 plist 文件会漏掉一类真实存在的服务：作业还挂在 launchd 里跑着，
	// 但 /Library/LaunchDaemons 下已经没有它的 plist 了（用户手工注册、
	// 或清理过 plist）。真机上就是这样（com.vix.cron 等）——
	// 服务管理里看不到、可纳管列表里也找不到，用户完全无从下手。
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.Label] = true
	}
	for _, label := range m.loadedLabels(ctx) {
		if seen[label] || known[label] || isSystemLabel(label) || isSelfLabel(label) {
			continue
		}
		c := AdoptCandidate{Label: label, DisplayName: label}
		if st, err := priv.LaunchStatus(label); err == nil {
			c.Running = st.Running
			c.PID = st.PID
		}
		candidates = append(candidates, c)
	}

	// 按可执行文件去重：同一个程序可能有多套 label
	// （例如 brew 的 homebrew.mxcl.php 与 sh.brew.php@8.3 指向同一个 php-fpm），
	// 只保留"正在运行"的那个，否则用户会在列表里看到重复且互相矛盾的条目。
	byProgram := map[string]int{}
	deduped := make([]AdoptCandidate, 0, len(candidates))
	for _, c := range candidates {
		key := c.Program
		if key == "" {
			deduped = append(deduped, c)
			continue
		}
		if idx, seen := byProgram[key]; seen {
			// 已有同程序的条目：运行中的优先
			if c.Running && !deduped[idx].Running {
				deduped[idx] = c
			}
			continue
		}
		byProgram[key] = len(deduped)
		deduped = append(deduped, c)
	}
	return deduped, nil
}

// AdoptCandidate 是一个可纳管候选项。
type AdoptCandidate struct {
	Label       string `json:"label"`
	DisplayName string `json:"display_name"`
	PlistPath   string `json:"plist_path"`
	Program     string `json:"program"`
	LogPath     string `json:"log_path"`
	Running     bool   `json:"running"`
	PID         int    `json:"pid"`
}

// selfLabels 是不允许纳管的服务：面板自身与关键基础设施。
//
// 为什么必须排除面板自己：如果用户在面板里"停止面板服务"，
// 会立刻失去访问入口，这是一个不可恢复的操作（除非有 SSH）。
// nginx 同理 —— 停止它会让所有站点离线，属于应该由专门入口管理的对象。
var selfLabels = map[string]bool{
	"cn.zizpanel.panel": true,
	"cn.zizdog.nginx":   true,
}

// isSelfLabel 判断是否为面板自身或关键基础设施。
func isSelfLabel(label string) bool {
	return selfLabels[label]
}

// isSystemLabel 判断是否为系统自带服务（不应出现在纳管列表里）。
func isSystemLabel(label string) bool {
	systemPrefixes := []string{
		"com.apple.", "com.openssh.", "org.cups.", "com.microsoft.",
		"com.google.", "com.adobe.", "com.docker.vmnetd", "com.openssh.sshd",
		// 网络扩展（Tailscale / VPN 等）由系统网络栈托管，
		// 面板没有能力也没有必要去启停它 —— 列出来只会让用户误点。
		"NetworkExtension.", "com.tailscale.", "io.tailscale.",
	}
	for _, p := range systemPrefixes {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return false
}

// AutoRegisterKnown 把「面板已知且本机确实存在」的服务自动登记进服务管理。
//
// 为什么需要它：服务管理应该是"本机装了什么"的如实反映，而不是
// "用户手工登记过什么"。早期只有在安装那一刻才登记，于是：
//
//	· 换台机器/重装面板后，已经在跑的服务全都不见了
//	· 用户以为面板坏了，其实只是没登记
//
// 现在面板启动时自动对齐一次 —— 幂等，已登记的会跳过。
//
// 只自动登记**目录里已知 label** 的服务（nginx/php/mysql/Qwen/接收端/IOPaint…）：
// 这些面板知道该怎么管（端口、健康检查、日志位置）。
// 其它任意第三方服务仍然走手动纳管 —— 自动接管一个我们不了解的服务，
// 会在面板里显示成一条信息不全、状态不准的记录，反而更误导。
func (m *Manager) AutoRegisterKnown(ctx context.Context) int {
	n := 0
	for _, a := range Catalog() {
		if a.ServiceLabel == "" {
			continue
		}
		// Docker 运行时走 EnsureColimaRuntime 的专用登记路径（kind=colima）。
		// 如果在这里也登记一遍，同一个东西会在「服务管理」里出现两次：
		// 一次是运行中的运行时，另一次是 kind=native 且永远显示"已加载但未运行"
		// —— 因为 colima 的开机任务是"起来就退出"的一次性作业。
		if a.Kind == KindColima {
			continue
		}
		if !m.launchServicePresent(ctx, a.ServiceLabel) {
			continue
		}
		icon := a.Icon
		if icon == "" {
			icon = "🧩"
		}
		if err := m.RegisterInstalledService(ctx, a.ServiceLabel, a.Name, icon, a.Category, a.Port); err == nil {
			n++
		}
	}
	return n
}

// launchServicePresent 判断某个 launchd 服务在本机是否存在。
//
// 两个来源都要看：plist 文件（常规），以及 launchd 已加载的作业。
// 实测有服务**没有 plist 但作业在跑**（用户手工注册、或 plist 被清掉了），
// 只看文件会把它们判成"不存在"。
func (m *Manager) launchServicePresent(ctx context.Context, label string) bool {
	if label == "" {
		return false
	}
	if fileExists(filepath.Join("/Library/LaunchDaemons", label+".plist")) {
		return true
	}
	if m.opt.UserHome != "" &&
		fileExists(filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")) {
		return true
	}
	// 系统域或用户域里已加载也算存在
	if _, err := m.runRoot(ctx, 8*time.Second, "/bin/launchctl", "print", "system/"+label); err == nil {
		return true
	}
	if m.opt.UID > 0 {
		if _, err := m.runRoot(ctx, 8*time.Second, "/bin/launchctl", "print",
			fmt.Sprintf("gui/%d/%s", m.opt.UID, label)); err == nil {
			return true
		}
	}
	return false
}

// loadedLabels 列出 launchd 里已加载的作业 label。
//
// 用 `launchctl list` 而不是 `launchctl print <domain>`：
// 后者的服务列表格式随系统版本变化，用正则去抠很脆
// （我第一版就是那么写的，实测一条都没匹配上，于是"已加载但无 plist"
//
//	的服务在可纳管列表里依然看不见）。
//
// `launchctl list` 是稳定的三列格式：PID  Status  Label。
func (m *Manager) loadedLabels(ctx context.Context) []string {
	res, err := m.runRoot(ctx, 15*time.Second, "/bin/launchctl", "list")
	if err != nil {
		return nil
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)
	out := []string{}
	for _, ln := range strings.Split(string(res), "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		label := f[len(f)-1]
		// 跳过表头：launchctl list 的第一行是 "PID  Status  Label"，
		// 不排除的话会多出一条名叫 "Label" 的假服务（实测踩到过）。
		if strings.EqualFold(label, "Label") || strings.EqualFold(f[0], "PID") {
			continue
		}
		if !valid.MatchString(label) {
			continue
		}
		out = append(out, label)
	}
	return out
}
