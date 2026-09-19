package services

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// ============================================================================
//  应用描述符 v2（AppDescriptor）
//
//  问题（用户原话）："有没有实现流程化/模块化？以后添加的新应用可以简单利用
//  现在管理框架。一个成熟产品不能靠一个一个地针对性上架才工作。"
//
//  现状是：**下载点**已经声明式化了（market_downloads.go），**安装流程**没有 ——
//  每个新应用都要在 internal/services 里新写一个 InstallXxx 函数，再去
//  internal/web 的分流 switch 里加一个 case。本文件与 steps.go 把"安装流程"
//  也变成数据：一个应用 = 一份 AppDescriptor，通用执行器按 steps 一步步做。
//
//  设计约束（本项目的既有不变量，不能破）：
//    1. **不引入外部 DSL 解析器/模板引擎**。steps 就是 Go 结构体，编译期就有
//       类型检查；模板只做 {token}/{user}/{password} 这类**字面量占位符**替换，
//       不引入 text/template（多了执行期才暴露的解析错误，而这里没有那个必要）。
//    2. **不改变任务的对外契约**：仍然 202 + task_id + SSE，进度仍走
//       result.step/emit（见 progress.go）。
//    3. **老目录条目继续可用**：App（catalog.go）的 24 个声明式字段一个都不动，
//       描述符是"安装流程"的补充，不是它的替代。
//    4. **失败即 error**：assert_ready 步骤失败默认返回 error，不许静默降级；
//       要降级必须显式写 DegradeReason（对齐 ready.go 的 assertReady）。
//
//  与既有 releaseBinaryApp（binary_release.go）的关系：
//    那个结构是"一个能力的参数表"，描述符是"流程本身"。第一轮把 tarball 轨
//    （frpc / orbien-client / ddns-go）搬过来时，**参数表仍然只有一份**：
//    releaseBinaryApp 由描述符派生（见 tarball_descriptor.go），
//    老的 InstallReleaseBinary 变成"描述符 → 步骤序列 → 执行"的兼容壳，
//    不许出现两套各说各话的实现。
// ============================================================================

// Rail 是"这个应用怎么装"。它决定用哪个通用执行器，而不是决定用哪个函数。
//
// 为什么不叫 Kind：Kind（services.go）描述的是**装完之后谁托管它**
// （native / docker / compose / colima），Rail 描述的是**怎么把它弄到磁盘上**。
// 两者不同：tarball 轨装的是原生二进制，Rail=tarball 而 Kind=native。
type Rail string

const (
	// RailBrew 是 Homebrew formula 轨道（brew install + brew services）。
	RailBrew Rail = "brew"
	// RailTarball 是"官方 release 预编译产物"轨道（本机第一轮实现的轨道）：
	// 下载 tarball / zip → 校验 → 解压挑成员 → 落盘 → 注册 launchd。
	RailTarball Rail = "tarball"
	// RailCompose 是 docker compose 轨道。
	RailCompose Rail = "compose"
	// RailPipVenv 是 Python 虚拟环境轨道（uv/pip + venv + 模型权重）。
	RailPipVenv Rail = "pip-venv"
	// RailPanelInstaller 是面板自研安装器轨道（要改 nginx、建站点、写库的复合流程）。
	RailPanelInstaller Rail = "panel-installer"
	// RailSite 是一键建站轨道（Typecho / WordPress：下载 → 建库 → 写 vhost）。
	RailSite Rail = "site"
)

// KnownRails 是全部合法值，供校验与文档生成使用。
var KnownRails = []Rail{RailBrew, RailTarball, RailCompose, RailPipVenv, RailPanelInstaller, RailSite}

// ArtifactKind 是产物的类型（决定 extract 怎么解）。
type ArtifactKind string

const (
	// ArtifactTarGz 是 .tar.gz / .tgz
	ArtifactTarGz ArtifactKind = "tar.gz"
	// ArtifactZip 是 .zip
	ArtifactZip ArtifactKind = "zip"
	// ArtifactBinary 是"下载下来就是可执行文件"，不需要解包。
	ArtifactBinary ArtifactKind = "binary"
)

// Arm64Evidence 是"这份产物确实是 darwin-arm64"的**证据**（铁律 8：不许 amd64、
// 不许 Rosetta）。
//
// 为什么必须是结构化的、而不是一句注释：Asset 名里有 darwin_arm64 **不等于**
// 内容一定是 arm64 —— 上游改过命名、挂错产物都真的发生过，而症状是 launchd 报
// "Bad CPU type"，完全指不到"下错架构"。所以每条产物都要留下取数来源，
// 并且安装时**运行期再复核一次**（verify_arm64 步骤，见 steps.go）：
//   - Evidence：人工核对时的观测（"file(1) 报 Mach-O 64-bit executable arm64"）；
//   - Source：怎么核对出来的（"2026-09-16 实下解压后 file -b"）；
//   - SHA256：有就写上游清单里的值（能核对内容，不只是架构）。
type Arm64Evidence struct {
	Evidence string `json:"evidence"`
	Source   string `json:"source"`
	SHA256   string `json:"sha256,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// ArtifactChecksum 描述"这份产物的 sha256 从哪来"。
//
// Source 的三种取值对应三种**不同强度**的校验，必须如实区分（不许含糊）：
//   - 上游清单（<repo>/releases/download/<tag>/<asset>）：能防传输损坏，
//     但清单与产物可能来自同一个第三方镜像 —— 那时它防不住镜像作恶；
//   - 镜像清单（镜像站的 manifest.json，由同步工具生成）：镜像模式下唯一的内容校验；
//   - 空：上游没有清单（例如 Orbien 客户端），只能靠 verify_arm64 做架构复核。
type ArtifactChecksum struct {
	// Kind 是校验方式标识，用于文档与日志（"upstream-list" / "mirror-manifest" / ""）。
	Kind string `json:"kind"`
	// Asset 是清单文件名（如 frp_sha256_checksums.txt）。
	Asset string `json:"asset,omitempty"`
	// URLs 是清单的下载地址（官方优先，第三方加速镜像兜底）。
	URLs []string `json:"urls,omitempty"`
	// Note 如实写清这次校验能防什么、防不住什么。
	Note string `json:"note,omitempty"`
}

// Artifact 是一份要落盘的产物。一个应用可以有多份（二进制 + 校验清单 + 模型）。
type Artifact struct {
	// Name 是产物在安装目录里的文件名（也是镜像布局里 <base>/apps/<id>/<version>/<name>）。
	Name string `json:"name"`
	// Version 是写死的版本（不许跟 latest 漂：上游改名/换架构时会静默装错东西）。
	Version string `json:"version"`
	// URLs 是按优先级排列的**公网**下载地址（官方优先、加速镜像兜底）。
	//
	// 镜像站地址不写死在这里：镜像基址是执行期设置，由 MirrorPreflight 钩子
	// 在执行时注入到候选列表第一位（见 steps.go 的 DownloadAction）。
	// 写死会让"设置改了、描述符还是旧的"，也会让探测到的地址与下载用的地址漂移。
	URLs []string `json:"urls"`
	// Kind 决定 extract 怎么解。
	Kind ArtifactKind `json:"kind"`
	// ExtractMember 是"一个归档里只挑这一个成员"（空 = 全部成员）。
	//
	// 为什么需要：frp 的 tarball 里同时有 frps / frpc / 示例配置，全解压会让
	// 安装目录多出一个用不到的二进制，还会把上游示例配置盖在我们生成配置的位置上。
	ExtractMember string `json:"extract_member,omitempty"`
	// StripComponents 剥掉归档的顶层目录（frp 是 1，ddns-go 是 0）。
	//
	// 这条区分是**真机踩出来的**：ddns-go 的产物没有顶层目录，沿用
	// "成员名一定带顶层目录"的老写法，tar 会去找一个不存在的路径，解压直接失败。
	StripComponents int `json:"strip_components,omitempty"`
	// Arm64 是这份产物是 arm64 的证据（空 = 不适用，例如校验清单 / 模型权重）。
	Arm64 *Arm64Evidence `json:"arm64,omitempty"`
	// Checksum 是内容校验的来源。
	Checksum *ArtifactChecksum `json:"checksum,omitempty"`
	// Required 表示"没有它就不能继续"（默认 true）。
	Required *bool `json:"required,omitempty"`
}

// IsRequired 报告这份产物是不是必需的（默认必需）。
func (a Artifact) IsRequired() bool { return a.Required == nil || *a.Required }

// EnvVar 是注册守护进程时要注入的环境变量。
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ServiceSpec 描述"装完之后谁来托管它、以谁的身份、开机自启吗"。
type ServiceSpec struct {
	// Manager 是托管方式："launchd-system"（系统级 LaunchDaemon，无人登录也在跑）
	// 或 "launchd-user"（用户级 LaunchAgent）。
	Manager string `json:"manager"`
	// Label 是 launchd 标签，例如 com.zizdog.frpc。
	Label string `json:"label"`
	// PlistPath 是 plist 的绝对落盘位置。
	PlistPath string `json:"plist_path"`
	// Template 是 plist 模板（用 {key} 占位符，见 steps.go 的 PlistTemplate）。
	Template *PlistTemplate `json:"template,omitempty"`
	// WorkDir 是工作目录（相对安装根目录，空 = 安装根目录本身）。
	WorkDir string `json:"work_dir,omitempty"`
	// Env 是要注入的环境变量（PATH 已经有了，这里只写应用额外需要的）。
	Env []EnvVar `json:"env,omitempty"`
	// RunAs 是"以谁的身份运行"："user"（真实用户，默认）或 "root"。
	RunAs string `json:"run_as,omitempty"`
	// RequiresSudo 表示注册这一步需要 root（写 /Library/LaunchDaemons 必然需要）。
	//
	// 为什么单独一个字段而不是让执行器推断：它与 PlistPath 的位置是两件事 ——
	// 以后支持"装到 /opt 但不注册服务"时，推断会给出错误答案。
	RequiresSudo bool `json:"requires_sudo"`
	// RunAtLoad / KeepAlive 对应 launchd 的同名字段（默认都 true）。
	RunAtLoad *bool `json:"run_at_load,omitempty"`
	KeepAlive *bool `json:"keep_alive,omitempty"`
	// OutLog / ErrLog 是标准输出/错误日志（相对安装根目录；空 = launchd.out.log / launchd.err.log）。
	OutLog string `json:"out_log,omitempty"`
	ErrLog string `json:"err_log,omitempty"`
	// StdoutTail 是失败时给用户看的日志（相对安装根目录；空 = 用 ErrLog）。
	StdoutTail string `json:"stdout_tail,omitempty"`
}

// RunAtLoadOrDefault 报告 RunAtLoad 的最终取值（默认 true）。
func (s ServiceSpec) RunAtLoadOrDefault() bool { return s.RunAtLoad == nil || *s.RunAtLoad }

// KeepAliveOrDefault 报告 KeepAlive 的最终取值（默认 true）。
func (s ServiceSpec) KeepAliveOrDefault() bool { return s.KeepAlive == nil || *s.KeepAlive }

// PlistTemplate 是 plist 的模板。
//
// 只支持两种占位符，且都是**字面量替换**（不引入模板引擎）：
//
//	{args}    —— ProgramArguments 数组里的 <string> 项（每项一行，已缩进）
//	{key}     —— ServiceSpec 的字段（label / user / workdir / outlog / errlog / envxml）
//
// 绝大多数应用用默认模板就够了（见 DefaultPlistTemplate）。
type PlistTemplate struct {
	// Body 是模板正文；空 = 用 DefaultPlistTemplate。
	Body string `json:"body,omitempty"`
	// ExtraKeys 是追加在 </dict> 之前的额外 key（原样 XML 片段，如 Sockets）。
	ExtraKeys string `json:"extra_keys,omitempty"`
}

// HealthProbeKind 是就绪判定的方式。
type HealthProbeKind string

const (
	// ProbePort 只按"端口开始监听"判断（比 launchd 状态更可信，默认）。
	ProbePort HealthProbeKind = "port"
	// ProbeHTTP 按 HTTP 响应判断。
	ProbeHTTP HealthProbeKind = "http"
	// ProbeLaunchd 只按"launchd 把这个作业拉起来了"判断。
	//
	// 这是给**不监听任何端口**的应用用的（orbien 客户端是纯出站连接）：
	// 对它们 waitPort(0, 60s) 必然超时，每次安装都会误报一次。
	ProbeLaunchd HealthProbeKind = "launchd"
)

// HealthSpec 描述"怎么判断它真的活了"。
//
// 失败语义（默认，不许改）：超时即 **error**，错误信息里写清
// 期望什么 / 实际什么 / 等了多久 / 已经完成了什么 / 还差什么 / 怎么办 / 日志尾部
// （由 ready.go 的 assertReady 生成）。要降级必须显式写 Degrade + DegradeReason。
type HealthSpec struct {
	// Probe 是判定方式（默认 ProbePort；Port<=0 时自动退到 ProbeLaunchd）。
	Probe HealthProbeKind `json:"probe,omitempty"`
	// Port 是要等的 TCP 端口（0 = 这个应用不监听端口）。
	//
	// ⚠️ 用 ProtocolPort 而不是 UIPort：dashboard 起得来不代表协议口绑上了，
	// 而后者才是"这个应用能不能用"的关键（frps 的 7000 常被隔空播放接收器占着）。
	Port int `json:"port,omitempty"`
	// Path 是 HTTP 探针路径（仅 Probe=ProbeHTTP 时用）。
	Path string `json:"path,omitempty"`
	// ExpectStatus 是 HTTP 探针可接受的 2xx/3xx 以外的状态码（默认 2xx/3xx 都算健康，
	// 与 health.go 的既有判定一致）。
	ExpectStatus []int `json:"expect_status,omitempty"`
	// Timeout 是等待上限（默认 60 秒）。
	Timeout time.Duration `json:"timeout,omitempty"`
	// Degrade 为 true 时失败只算警告；**必须**同时写 DegradeReason。
	Degrade bool `json:"degrade,omitempty"`
	// DegradeReason 解释这次降级为什么可接受。
	DegradeReason string `json:"degrade_reason,omitempty"`
}

// TimeoutOr 返回等待上限（未配置时 60 秒）。
func (h HealthSpec) TimeoutOr() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return 60 * time.Second
}

// InputSpec 描述"这个应用需要用户提供什么秘密"。
//
// 语义（复用任务中心的限时输入，见 input.go）：任务跑到需要它的那一步时，
// 通过 tasks.InputRequest 问用户，超时自动生成/用默认值继续 ——
// **绝不阻塞**，也绝不把秘密写进任务步骤（只进 InstallResult.Credentials）。
//
// 为什么放在描述符里而不是安装函数里：装 MySQL 时必须拿到 root 口令，
// 而"装完再对齐"注定留下不一致的窗口（2026-09-16 mini 就是这么把面板锁在门外的）。
// 把"需要什么输入"声明出来，任何新应用都能复用同一条闭环。
type InputSpec struct {
	// Key 是输入标识（前端按它回填）。
	Key string `json:"key"`
	// Label 是给用户看的一句话。
	Label string `json:"label"`
	// Secret 为 true 时前端用密码框，且值只进 Credentials、不进日志。
	Secret bool `json:"secret"`
	// Required 为 true 时用户不填就拒绝继续；false 时超时走默认值。
	Required bool `json:"required,omitempty"`
	// Default 是超时/未提供时的取值（空 = 由执行器生成随机值）。
	Default string `json:"default,omitempty"`
	// Generate 是"没有默认值也没有用户输入时"的生成方式（"random-hex-16" 等）。
	Generate string `json:"generate,omitempty"`
	// Timeout 是最长等待时间（默认 60 秒，与 MySQLInputTimeout 一致）。
	Timeout time.Duration `json:"timeout,omitempty"`
	// BindTo 是"这个值替换进哪个占位符"（如 "{token}"）。
	BindTo string `json:"bind_to,omitempty"`
}

// SettingKind 是设置项的类型。
type SettingKind string

const (
	SettingString SettingKind = "string"
	SettingInt    SettingKind = "int"
	SettingBool   SettingKind = "bool"
	SettingSecret SettingKind = "secret"
	SettingEnum   SettingKind = "enum"
)

// SettingSpec 是设置界面**由 schema 生成**的一项。
//
// 现状缺口（本轮只声明、不改前端）：应用的设置界面不是 schema 生成的，
// 每个应用都在 servicePanel.js 里手写一份表单。声明出来后，
// 服务详情面板可以零应用 ID 分支地渲染（constraints 里的 enum/min/max
// 也用于**安装前的输入校验**，见 ValidateSettings）。
type SettingSpec struct {
	Key      string      `json:"key"`
	Label    string      `json:"label"`
	Kind     SettingKind `json:"kind"`
	Default  any         `json:"default,omitempty"`
	Required bool        `json:"required,omitempty"`
	// Options 是 enum 的候选值。
	Options []string `json:"options,omitempty"`
	// Min / Max 是 int 的取值范围（0 表示不限制）。
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
	// Validate 是额外校验规则："port" / "path" / "url" / "nonempty" / ""。
	Validate string `json:"validate,omitempty"`
	// BindTo 是"这个设置项替换进配置模板的哪个占位符"（如 "{port}"）。
	BindTo string `json:"bind_to,omitempty"`
	// Advanced 表示折叠在"高级"里（默认值足够好的项）。
	Advanced bool `json:"advanced,omitempty"`
	// Note 是给用户看的说明。
	Note string `json:"note,omitempty"`
}

// SettingsSchema 是一个应用的设置界面 schema。
type SettingsSchema struct {
	Fields []SettingSpec `json:"fields"`
}

// Field 按 key 取设置项。
func (s SettingsSchema) Field(key string) (SettingSpec, bool) {
	for _, f := range s.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return SettingSpec{}, false
}

// Defaults 返回"只填了默认值"的配置。
func (s SettingsSchema) Defaults() map[string]any {
	out := map[string]any{}
	for _, f := range s.Fields {
		if f.Default != nil {
			out[f.Key] = f.Default
		}
	}
	return out
}

// ValidateSettings 按 schema 校验一组设置值，返回补全默认值后的结果。
//
// **失败即 error**：非法端口、非法路径一律拒绝，不"尽力而为"——
// 把 0 或 70000 写进配置文件只会让服务起来后立刻退出，而那时错误信息
// 完全指不到"是设置项填错了"。
func (s SettingsSchema) ValidateSettings(in map[string]any) (map[string]any, error) {
	out := s.Defaults()
	for k, v := range in {
		out[k] = v
	}
	for _, f := range s.Fields {
		v, ok := out[f.Key]
		if !ok || v == nil {
			if f.Required {
				return nil, fmt.Errorf("设置项 %s（%s）是必填的", f.Key, f.Label)
			}
			continue
		}
		switch f.Kind {
		case SettingInt:
			n, ok := asInt(v)
			if !ok {
				return nil, fmt.Errorf("设置项 %s（%s）必须是整数，实际 %v", f.Key, f.Label, v)
			}
			if f.Min != 0 && n < f.Min {
				return nil, fmt.Errorf("设置项 %s（%s）不能小于 %d，实际 %d", f.Key, f.Label, f.Min, n)
			}
			if f.Max != 0 && n > f.Max {
				return nil, fmt.Errorf("设置项 %s（%s）不能大于 %d，实际 %d", f.Key, f.Label, f.Max, n)
			}
			if f.Validate == "port" && (n < 1 || n > 65535) {
				return nil, fmt.Errorf("设置项 %s（%s）不是合法端口：%d", f.Key, f.Label, n)
			}
		case SettingString, SettingSecret, SettingEnum:
			sv, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("设置项 %s（%s）必须是字符串，实际 %v", f.Key, f.Label, v)
			}
			if f.Required && strings.TrimSpace(sv) == "" {
				return nil, fmt.Errorf("设置项 %s（%s）不能为空", f.Key, f.Label)
			}
			if f.Kind == SettingEnum && len(f.Options) > 0 && !containsString(f.Options, sv) {
				return nil, fmt.Errorf("设置项 %s（%s）只能是 %s 之一，实际 %q",
					f.Key, f.Label, strings.Join(f.Options, " / "), sv)
			}
		case SettingBool:
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("设置项 %s（%s）必须是布尔值，实际 %v", f.Key, f.Label, v)
			}
		}
	}
	return out, nil
}

// asInt 把 JSON 反序列化常见的数字写法都收进来（int / int64 / float64）。
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// UninstallSpec 描述卸载时的保留/清理策略。
type UninstallSpec struct {
	// RemoveService 表示要停止并删除 launchd 服务（默认 true）。
	RemoveService *bool `json:"remove_service,omitempty"`
	// DataPaths 是"勾选删除数据时"要删的目录（相对安装根目录，"." = 根目录本身）。
	DataPaths []string `json:"data_paths"`
	// KeepNote 是默认（不勾选删除数据）时对用户说的话。
	//
	// 必须写清"保留了哪些**有秘密的文件**"：frpc 的 frpc.toml 里有 token、
	// ddns-go.yaml 里有 DNS 服务商的 API 密钥 —— 用户以为卸载就清干净了，
	// 结果密钥还留在磁盘上，这是真实的安全问题。
	KeepNote string `json:"keep_note"`
}

// Urls 是地址约定。**核心规则：绑定地址决定广告地址。**
//
// 只绑 127.0.0.1 的服务**不得**广告成 LAN 地址（http://192.168.x.x:port）——
// 点了打不开，而用户会以为是面板坏了。反过来，绑 0.0.0.0 的服务必须广告
// LAN 地址，否则用户看到 127.0.0.1 以为只能本机用。
type Urls struct {
	// BindAddress 是这个应用真正监听的地址（"127.0.0.1" / "0.0.0.0" / "::"）。
	BindAddress string `json:"bind_address"`
	// AdminPath 是管理/界面入口路径（如 "/"）。
	AdminPath string `json:"admin_path,omitempty"`
	// ProxyURL 是"通过面板反向代理访问"的地址模板（{slug} 会被替换）。
	//
	// ⚠️ 为空**必须**写 ProxyURLReason 说明为什么没有 —— 这一条是硬要求：
	// 子路径反代不是万能的（Uptime Kuma 的 Vue 路由不认前缀、MinIO 要设
	// MINIO_BROWSER_REDIRECT_URL、纯 CLI 根本没有界面），"没有代理地址"
	// 是一个需要解释的决定，而不是一个可以静默留空的字段。
	ProxyURL string `json:"proxy_url,omitempty"`
	// ProxyURLReason 解释 ProxyURL 为空的原因（非空 ProxyURL 时留空）。
	ProxyURLReason string `json:"proxy_url_reason,omitempty"`
	// DocsURL 是上游文档。
	DocsURL string `json:"docs_url,omitempty"`
}

// AdvertisedURL 按"绑定地址决定广告地址"算出给用户看的地址。
//
//	host 是面板探测到的主机地址（通常是主网卡 IP）。
//	返回空串表示"这个服务不对外提供 HTTP 入口"（不监听端口 / 绑定地址无法
//	解析）—— 空串是**结论**，调用方必须如实显示"无网页入口"，不许拿 host 顶上。
//
// 三种绑定地址对应三种广告方式：
//   - 只绑回环 127.0.0.1：只能本机访问，广告 **127.0.0.1**（广告 LAN 地址
//     会让用户点开一个连不上的链接）；
//   - 绑通配 0.0.0.0 / ::：广告面板探测到的主机地址（用户从别的机器访问）；
//   - 绑某个具体地址：广告**那个地址**（不是面板探测到的那个 —— 服务根本
//     没在面板探测到的地址上监听）。
func (u Urls) AdvertisedURL(host string, port int) string {
	if port <= 0 {
		return ""
	}
	bind, ok := advertisedHost(u.BindAddress, host)
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s:%d%s", bind, port, u.AdminPath)
}

// advertisedHost 由绑定地址算出广告用的主机名；ok=false 表示"无法确定，
// 宁可不广告"。
//
// 判据刻意保守：只有回环地址、通配地址、或一个能解析的具体 IP 才给出答案。
// 看不懂的写法（主机名、空串）一律**不广告** —— 宁可让用户自己填地址，
// 也不要给一个可能打不开的链接。
func advertisedHost(bind, host string) (string, bool) {
	b := strings.TrimSpace(bind)
	if b == "" {
		return "", false
	}
	if b == "0.0.0.0" || b == "::" || b == "*" {
		if strings.TrimSpace(host) == "" {
			return "127.0.0.1", true
		}
		return host, true
	}
	ip := net.ParseIP(b)
	if ip == nil {
		return "", false
	}
	if ip.IsLoopback() {
		return "127.0.0.1", true
	}
	if ip.IsUnspecified() {
		if strings.TrimSpace(host) == "" {
			return "127.0.0.1", true
		}
		return host, true
	}
	return b, true
}

// AppDescriptor 是一个应用的完整安装/管理描述。
type AppDescriptor struct {
	// ID 与目录条目（catalog.go 的 App.ID）一致。**必须**一致：
	// 市场点击安装时传的是目录 ID，描述符按它查。
	ID string `json:"id"`
	// Name / Icon / Category 是给任务日志与服务记录用的展示信息。
	Name     string `json:"name"`
	Icon     string `json:"icon"`
	Category string `json:"category"`
	// Rail 决定用哪个通用执行器。
	Rail Rail `json:"rail"`
	// Kind 是装完之后谁托管它（写进服务注册表，与 App.Kind 保持一致）。
	Kind Kind `json:"kind"`
	// Port 是**协议口**（安装前的端口冲突检查用它）。
	Port int `json:"port"`
	// Paths 是安装根目录与其中的具体文件。
	Paths DescriptorPaths `json:"paths"`
	// Artifacts 是要落盘的产物清单。
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Steps 是安装流程本身（小步骤 DSL）。
	Steps []InstallStep `json:"steps"`
	// Service 是装完之后的服务定义。
	Service ServiceSpec `json:"service"`
	// Health 是就绪判定。
	Health HealthSpec `json:"health"`
	// Inputs 是需要用户输入的秘密。
	Inputs []InputSpec `json:"inputs,omitempty"`
	// Settings 是设置界面 schema（也用于安装前的输入校验）。
	Settings SettingsSchema `json:"settings,omitempty"`
	// Uninstall 是卸载策略。
	Uninstall UninstallSpec `json:"uninstall"`
	// Urls 是地址约定（绑定地址决定广告地址）。
	Urls Urls `json:"urls"`
	// Notes 是安装成功后要额外告诉用户的话（原样展示）。
	Notes []string `json:"notes,omitempty"`
	// PanelInstaller 与目录条目的同名字段对齐（市场"已安装"判定与卸载分流用它）。
	PanelInstaller string `json:"panel_installer,omitempty"`
	// ServiceLabel 与目录条目的同名字段对齐。
	ServiceLabel string `json:"service_label,omitempty"`
	// ChecksumArtifacts 是"这份描述符引用的校验清单"（镜像模式下由镜像站的
	// manifest.json 提供，公网模式下从上游下载）。
	ChecksumArtifacts []Artifact `json:"checksum_artifacts,omitempty"`
}

// Paths 是安装根目录与其中的文件（相对家目录 / 绝对路径两种情况都表达得了）。
type DescriptorPaths struct {
	// RootDir 是安装根目录，**相对真实用户家目录**（如 "frpc"）。
	RootDir string `json:"root_dir"`
	// Binary 是解压后的可执行文件名（如 "frpc"）。
	Binary string `json:"binary,omitempty"`
	// ConfigFile 是配置文件名（相对安装根目录；空 = 这个应用没有独立配置文件）。
	ConfigFile string `json:"config_file,omitempty"`
	// OutLog / ErrLog 是 launchd 日志文件名（相对安装根目录）。
	OutLog string `json:"out_log,omitempty"`
	ErrLog string `json:"err_log,omitempty"`
}

// FindDescriptor 按 ID 查描述符。
func FindDescriptor(id string) (AppDescriptor, bool) {
	d, ok := descriptorsByID[id]
	return d, ok
}

// Validate 对描述符做静态自检，供测试与"新增应用"的预检使用。
//
// 为什么要在**构造期**就检查：描述符是数据，写错了不会有编译错误 ——
// 少一个 Label 会让 plist 的 Label 变成空串（launchd 拒绝加载），
// 少一个 Artifact 会让安装"成功"但目录里什么都没有。这些都要在
// 单测里立刻失败，而不是等真机上点了安装才发现。
func (d AppDescriptor) Validate() error {
	if strings.TrimSpace(d.ID) == "" {
		return fmt.Errorf("描述符缺少 ID")
	}
	if !containsRail(d.Rail) {
		return fmt.Errorf("描述符 %s 的 rail=%q 不是合法值（%s）",
			d.ID, d.Rail, railsText())
	}
	if strings.TrimSpace(d.Paths.RootDir) == "" {
		return fmt.Errorf("描述符 %s 缺少 Paths.RootDir", d.ID)
	}
	if d.Service.Label == "" {
		return fmt.Errorf("描述符 %s 缺少 Service.Label（没有它 launchd 无从加载）", d.ID)
	}
	if d.Service.RequiresSudo && strings.TrimSpace(d.Service.PlistPath) == "" {
		return fmt.Errorf("描述符 %s 需要 sudo 注册但没写 Service.PlistPath", d.ID)
	}
	if d.Health.Degrade && strings.TrimSpace(d.Health.DegradeReason) == "" {
		return fmt.Errorf("描述符 %s 声明了降级但没写 Health.DegradeReason"+
			"（显式降级必须说清为什么可接受，否则等于静默吞掉失败）", d.ID)
	}
	if d.Urls.ProxyURL == "" && strings.TrimSpace(d.Urls.ProxyURLReason) == "" {
		return fmt.Errorf("描述符 %s 没有 proxy_url，必须写 Urls.ProxyURLReason 说明为什么"+
			"（留空是一个需要解释的决定，不是可以省掉的字段）", d.ID)
	}
	if d.Urls.ProxyURL != "" && strings.TrimSpace(d.Urls.ProxyURLReason) != "" {
		return fmt.Errorf("描述符 %s 同时写了 proxy_url 与 proxy_url_reason（二者互斥）", d.ID)
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("描述符 %s 没有任何 steps（那就等于没有安装流程）", d.ID)
	}
	for i, a := range d.Artifacts {
		if a.Name == "" {
			return fmt.Errorf("描述符 %s 的第 %d 个产物缺少 Name", d.ID, i+1)
		}
		if a.Version == "" {
			return fmt.Errorf("描述符 %s 的产物 %s 没有写死 Version（跟 latest 漂会装错版本）", d.ID, a.Name)
		}
		if len(a.URLs) == 0 {
			return fmt.Errorf("描述符 %s 的产物 %s 没有任何下载地址", d.ID, a.Name)
		}
		if a.Kind == ArtifactTarGz && a.ExtractMember != "" && a.Arm64 != nil {
			if strings.TrimSpace(a.Arm64.Evidence) == "" {
				return fmt.Errorf("描述符 %s 的产物 %s 声明了 arm64 证据但内容为空", d.ID, a.Name)
			}
		}
	}
	for _, f := range d.Settings.Fields {
		if f.Key == "" {
			return fmt.Errorf("描述符 %s 的设置项缺少 Key", d.ID)
		}
		if f.Kind == SettingEnum && len(f.Options) == 0 {
			return fmt.Errorf("描述符 %s 的设置项 %s 是 enum 但没给 Options", d.ID, f.Key)
		}
	}
	for _, in := range d.Inputs {
		if in.Key == "" {
			return fmt.Errorf("描述符 %s 的输入项缺少 Key", d.ID)
		}
		if in.Secret && len(in.BindTo) == 0 && in.Generate == "" && in.Default == "" {
			return fmt.Errorf("描述符 %s 的秘密输入 %s 既没有 BindTo 也没有默认值/生成方式"+
				"（那它到底被用在哪？）", d.ID, in.Key)
		}
	}
	return nil
}

func containsRail(r Rail) bool {
	for _, k := range KnownRails {
		if k == r {
			return true
		}
	}
	return false
}

func railsText() string {
	parts := make([]string, 0, len(KnownRails))
	for _, r := range KnownRails {
		parts = append(parts, string(r))
	}
	return strings.Join(parts, " / ")
}

// DefaultPlistTemplate 是 launchd plist 的缺省模板。
//
// 字段与 releaseBinaryPlist（老实现）逐字一致 —— 换轨不能改行为：
// 系统级 LaunchDaemon、以真实用户身份运行、RunAtLoad + KeepAlive、
// 标准输出/错误分别落日志、注入 Homebrew PATH。
//
// 两个刻意的决定：
//   - **以真实用户身份运行**（UserName=user）：服务要写它自己的配置与数据目录，
//     用 root 跑会让这些文件变成 root 所有，用户改不动。
//   - **PATH 注入 /opt/homebrew/bin**：LaunchDaemon 启动的环境里没有 Homebrew，
//     应用启动时要找 ffmpeg / python 之类会找不到（真机上踩过）。
const DefaultPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{label}</string>
    <key>UserName</key>
    <string>{user}</string>
    <key>ProgramArguments</key>
    <array>
{args}    </array>
    <key>WorkingDirectory</key>
    <string>{workdir}</string>
    <key>RunAtLoad</key>
    <{runatload}/>
    <key>KeepAlive</key>
    <{keepalive}/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>{path}</string>
{envxml}    </dict>
    <key>StandardOutPath</key>
    <string>{outlog}</string>
    <key>StandardErrorPath</key>
    <string>{errlog}</string>
{extrakeys}</dict>
</plist>
`

// DefaultPATH 是注入 LaunchDaemon 的 PATH（与老实现逐字一致）。
const DefaultPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
