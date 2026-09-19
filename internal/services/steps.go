package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
//  steps：应用安装的小步骤 DSL 与通用执行器（步骤集见 docs/应用描述符v2.md）。
//  失败语义总则：任何一步失败都**中止安装并返回 error**；唯一允许"失败但继续"的
//  是显式降级（assert_ready 的 Degrade + DegradeReason、write_file 的 Optional）。
//  "能谎报成功的功能，比没做更糟"（AGENTS.md 第一节第 10 条）。

// InstallStep 是描述符里的一个安装步骤。
type InstallStep interface {
	// Kind 返回步骤类型（用于日志、文档与"这一轨支持哪些步骤"的静态检查）。
	Kind() string
	// Describe 返回给用户看的一句话（空串表示这一步不单独写进度）。
	Describe(ec *ExecConfig) string
	// Exec 执行这一步；返回 error 即中止安装。
	Exec(ec *ExecConfig) error
}

// ---------- 步骤：chmod / chown ----------

// ChmodAction 设置权限位。解压出来的二进制权限来自归档（可能 0644），
// 必须显式 0755，否则 launchd 报 "not executable"（错误信息指不到"权限不对"）。
type ChmodAction struct {
	Path string `json:"path"`
	Mode int    `json:"mode"`
}

func (a ChmodAction) Kind() string { return "chmod" }
func (a ChmodAction) Describe(ec *ExecConfig) string {
	return "设置可执行权限 " + ec.path(a.Path)
}

func (a ChmodAction) Exec(ec *ExecConfig) error {
	path := ec.path(a.Path)
	mode := os.FileMode(a.Mode)
	if a.Mode == 0 {
		mode = 0o755
	}
	if err := ec.Runner.Chmod(path, mode); err != nil {
		return fmt.Errorf("设置可执行权限失败: %w", err)
	}
	return nil
}

// ChownAction 递归改归属（安装目录与其下全部文件交给真实用户）。
// 必须递归：只 chown 父目录时子目录仍属 root，服务以用户身份运行
// 时写不进配置 / 数据目录（真机踩过）。
type ChownAction struct {
	Path  string `json:"path"`
	Owner string `json:"owner,omitempty"`
}

func (a ChownAction) Kind() string                   { return "chown" }
func (a ChownAction) Describe(ec *ExecConfig) string { return "" }

func (a ChownAction) Exec(ec *ExecConfig) error {
	owner := a.Owner
	if owner == "" {
		owner = "user"
	}
	if owner == "root" {
		return nil
	}
	return ec.chownUser(ec.path(a.Path))
}

// ---------- 步骤：ensure_config ----------

// EnsureConfigAction 生成应用的配置文件（含面板随机凭据）。必须在**执行期**读盘：
// 构造步骤时读会在重装场景给出错误答案（用户可能刚改过配置），token/口令也只有执行期才知道。
// 判据见 configKeepsExisting：含面板 marker 即保留；PreserveExistingConfig 退化成"存在即保留"。
type EnsureConfigAction struct {
	// Path 是配置文件路径（支持 {root} 占位符）。
	Path string `json:"path"`
	// Template 是配置模板（{token} / {user} / {password} 占位符）。
	Template string `json:"template"`
	// Mode 是权限（0 = 0600：文件里是明文凭据）。
	Mode int `json:"mode,omitempty"`
	// PreserveExistingConfig 为 true 时"文件存在即保留"（见上）。
	PreserveExistingConfig bool `json:"preserve_existing_config,omitempty"`
}

func (a EnsureConfigAction) Kind() string                   { return "ensure_config" }
func (a EnsureConfigAction) Describe(ec *ExecConfig) string { return "" }

func (a EnsureConfigAction) Exec(ec *ExecConfig) error {
	path := ec.path(a.Path)
	// 判据走 configKeepsExisting：两份判据漂移会让重装冲掉用户配置。
	if b, err := ec.Runner.ReadFile(path); err == nil {
		if a.PreserveExistingConfig || strings.Contains(string(b), panelConfigMarker) {
			ec.Result.step(ec.Ctx, "已保留现有配置 "+path+"（面板不覆盖你的改动）")
			return nil
		}
	}
	secrets, err := generateConfigSecrets(a.Template, "")
	if err != nil {
		return fmt.Errorf("生成配置凭据失败: %w", err)
	}
	mode := os.FileMode(0o600)
	if a.Mode != 0 {
		mode = os.FileMode(a.Mode)
	}
	if err := ec.Runner.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", filepath.Dir(path), err)
	}
	content := expandConfigSeed(a.Template, secrets)
	if err := ec.Runner.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	ec.secrets = secrets
	if err := ec.chownUser(path); err != nil {
		return err
	}
	// 凭据区块紧跟这一步写进结果（口令/token 只允许出现在这里，
	// 任务步骤与审计日志里不许有明文）。
	if block := credentialBlock(ec.Spec.Name, path, ec.advertisedURL(), secrets); len(block) > 0 {
		ec.Result.Steps = append(ec.Result.Steps, block...)
	}
	return nil
}

// advertisedURL 按"绑定地址决定广告地址"算出给用户看的界面地址。
func (ec *ExecConfig) advertisedURL() string {
	if ec.Spec.Port <= 0 {
		return ""
	}
	return ec.Spec.Urls.AdvertisedURL(ec.primaryIP(), ec.Spec.Port)
}

// ---------- 步骤：ensure_dir ----------

// EnsureDirAction 建目录并（可选）把归属改给真实用户。
// 安装目录由面板以 root 创建、服务以真实用户运行：不递归 chown 时子目录仍属 root，
// 用户身份的 venv / 配置写入会 Permission denied（真机踩过）。
type EnsureDirAction struct {
	// Path 是目录（支持 {root} 占位符；相对路径按安装根目录解析）。
	Path string `json:"path"`
	// Mode 是权限（八进制；0 = 0755）。
	Mode int `json:"mode,omitempty"`
	// Owner 是归属："user"（默认）/ "root" / ""（不动）。
	Owner string `json:"owner,omitempty"`
}

func (a EnsureDirAction) Kind() string { return "ensure_dir" }
func (a EnsureDirAction) Describe(ec *ExecConfig) string {
	return "创建目录 " + ec.path(a.Path)
}

func (a EnsureDirAction) Exec(ec *ExecConfig) error {
	dir := ec.path(a.Path)
	mode := os.FileMode(0o755)
	if a.Mode != 0 {
		mode = os.FileMode(a.Mode)
	}
	if err := ec.Runner.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", dir, err)
	}
	switch a.Owner {
	case "", "none":
		return nil
	case "user":
		return ec.chownUser(dir)
	case "root":
		return nil
	default:
		return fmt.Errorf("ensure_dir 的 owner=%q 不支持（user / root / 空）", a.Owner)
	}
}

// ---------- 步骤：download ----------

// DownloadAction 下载一份产物。镜像优先：镜像地址插到候选第一位；超时按**来源**给
// （镜像/官方 150 秒、第三方加速 300 秒；实测官方约 46KB/s、加速镜像约 640KB/s）。
// 先下 `<dest>.part` 再改名，全部地址失败时用磁盘上已有产物继续（是否可用交给后续校验步骤）。
type DownloadAction struct {
	// Artifact 是要下载的产物（Name 是落盘文件名）。
	Artifact Artifact `json:"artifact"`
	// DestDir 是落盘目录（空 = 安装根目录）。
	DestDir string `json:"dest_dir,omitempty"`
	// MirrorPreflight 为 true 时先探镜像上有没有这个包与清单（默认 false）。
	MirrorPreflight bool `json:"mirror_preflight,omitempty"`
	// SkipWhenMirrorUsed 为 true 时：本次安装已有产物**真的从镜像站下到**就跳过这一步。
	// 上游校验清单只存在于 GitHub，而走镜像时用镜像清单（manifest.json，覆盖更全）——
	// 硬下它等于在"镜像优先"里塞回一次公网访问，GitHub 不可达时还会让安装失败。
	SkipWhenMirrorUsed bool `json:"skip_when_mirror_used,omitempty"`
	// ExpectSHA256 非空时，本步直接按它校验（镜像清单里的 sha256 就是这么来的）。
	ExpectSHA256 string `json:"expect_sha256,omitempty"`
	// ChecksumSource 是 ExpectSHA256 的来源描述（只写进日志）。
	ChecksumSource string `json:"checksum_source,omitempty"`
}

func (a DownloadAction) Kind() string { return "download" }
func (a DownloadAction) Describe(ec *ExecConfig) string {
	return "下载 " + a.Artifact.Name
}

func (a DownloadAction) Exec(ec *ExecConfig) error {
	dest := ec.path(a.destPath())
	// 走镜像时上游清单用不到：跳过它，免得"镜像优先"又退化成一次公网访问。
	if a.SkipWhenMirrorUsed && ec.mirrorDownloadedAny() {
		ec.Result.step(ec.Ctx, "主产物已从镜像站下到，跳过 "+a.Artifact.Name+
			" 的下载（sha256 以镜像清单为准，不再访问公网源）")
		return nil
	}
	// 要求了预检却没接入则**明确报错**（静默跳过会让"镜像优先"变成一句空话）。
	if a.MirrorPreflight {
		if ec.MirrorPreflight == nil {
			return fmt.Errorf("下载 %s 失败：这一步要求先做镜像预检，但执行器没有接入"+
				"（面板内部错误）", a.Artifact.Name)
		}
		// 用它 HEAD 成功的地址当候选第一位；空串 = 镜像不可用，保持自带候选顺序。
		if mirrorURL := ec.MirrorPreflight(a.Artifact); mirrorURL != "" {
			a.Artifact = a.Artifact.withMirrorFirst(mirrorURL)
		}
	}
	return ec.download(a, dest)
}

func (a DownloadAction) destPath() string {
	if a.DestDir == "" {
		return "{root}/" + a.Artifact.Name
	}
	return strings.TrimRight(a.DestDir, "/") + "/" + a.Artifact.Name
}

// withMirrorFirst 返回一份"镜像地址排候选第一位"的产物副本（去重，避免同一地址下两次）。
// 用副本而非改原描述符：描述符是注册表共享值，就地改会污染后续安装。
func (a Artifact) withMirrorFirst(mirrorURL string) Artifact {
	if mirrorURL == "" {
		return a
	}
	urls := make([]string, 0, len(a.URLs)+1)
	urls = append(urls, mirrorURL)
	for _, u := range a.URLs {
		if u != mirrorURL {
			urls = append(urls, u)
		}
	}
	a.URLs = urls
	return a
}

// ---------- 步骤：verify_sha256 ----------

// VerifySHA256Action 按给定 sha256 校验产物。**失败即中止**：
// 静默跳过等于把"有校验"变成"看运气"（镜像/网络篡改或损坏都会漏过）。
type VerifySHA256Action struct {
	// Artifact 是要校验的产物（用它的 Name 定位文件）。
	Artifact Artifact `json:"artifact"`
	// Expect 是期望的 sha256（空 = 从 Checksum.Asset 里查）。
	Expect string `json:"expect,omitempty"`
	// Source 是期望值的来源描述（如 "官方 frp_sha256_checksums.txt"）。
	Source string `json:"source"`
	// SkipIfNoChecksum 为 true 时放行"上游也没有清单"（默认必须有清单，拿不到即中止）。Orbien 是唯一放行条目。
	SkipIfNoChecksum bool `json:"skip_if_no_checksum,omitempty"`
}

func (a VerifySHA256Action) Kind() string { return "verify_sha256" }
func (a VerifySHA256Action) Describe(ec *ExecConfig) string {
	return "" // 校验过程由 Exec 自己写更具体的一行
}

func (a VerifySHA256Action) Exec(ec *ExecConfig) error {
	_ = ec
	return fmt.Errorf("verify_sha256 必须由执行器直接处理（面板内部错误）")
}

// ---------- 步骤：verify_arm64 ----------

// VerifyArm64Action 用 file(1) 复核二进制确实是 arm64。
// Asset 名里有 darwin_arm64 并**不等于**内容一定是 arm64；这一步让失败发生在安装阶段，
// 原因明确（铁律 8：不许 amd64、不许 Rosetta）。
type VerifyArm64Action struct {
	// Path 是要复核的文件（支持 {root} 占位符）。
	Path string `json:"path"`
}

func (a VerifyArm64Action) Kind() string { return "verify_arm64" }
func (a VerifyArm64Action) Describe(ec *ExecConfig) string {
	return "复核 " + filepath.Base(ec.path(a.Path)) + " 是原生 arm64"
}

func (a VerifyArm64Action) Exec(ec *ExecConfig) error {
	return ec.verifyArm64(ec.path(a.Path))
}

// ---------- 步骤：extract ----------

// ExtractAction 解包并挑成员（tar / zip）。
type ExtractAction struct {
	// Artifact 是要解的归档（用它的 Name 定位文件，用 Kind 决定解包方式）。
	Artifact Artifact `json:"artifact"`
	// Dest 是解到哪（空 = 安装根目录）。
	Dest string `json:"dest,omitempty"`
	// Member 是"只解这一个成员"（空 = 全部成员）。
	Member string `json:"member,omitempty"`
	// StripComponents 剥掉顶层目录层数。
	StripComponents int `json:"strip_components,omitempty"`
	// BootoutFirst 为 true 时先 bootout 旧实例：覆写正在执行的 Mach-O 会被系统杀掉（Killed: 9）。
	// 这一步失败无所谓（本来没装过时必然报错），后面 bootstrap 才是决定性的那一步。
	BootoutFirst bool `json:"bootout_first,omitempty"`
	// ExpectFile 是"解完之后必须存在这个文件"（空 = 不检查）。
	// 解压成功但成员名不对最隐蔽：tar 退出码 0，目录里却什么都没有。
	ExpectFile string `json:"expect_file,omitempty"`
}

func (a ExtractAction) Kind() string { return "extract" }
func (a ExtractAction) Describe(ec *ExecConfig) string {
	return "解压 " + a.Artifact.Name
}

func (a ExtractAction) Exec(ec *ExecConfig) error {
	return ec.extract(a)
}

// ---------- 步骤：copy ----------

// CopyAction 复制文件或目录树。
type CopyAction struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Mode 非 0 时对复制出来的文件 chmod。
	Mode int `json:"mode,omitempty"`
	// Optional 为 true 时源不存在不算失败（要解释为什么，见 Note）。
	Optional bool `json:"optional,omitempty"`
	// Note 说明 Optional 的用途（Optional=true 时必须写）。
	Note string `json:"note,omitempty"`
}

func (a CopyAction) Kind() string { return "copy" }
func (a CopyAction) Describe(ec *ExecConfig) string {
	return "复制 " + ec.path(a.From) + " → " + ec.path(a.To)
}

func (a CopyAction) Exec(ec *ExecConfig) error { return ec.copyTree(a) }

// ---------- 步骤：write_file ----------

// WriteFileAction 写一个文件（配置模板走这里，占位符在构造步骤时就替换完）。
type WriteFileAction struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Mode 是权限（八进制；0 = 0600，因为这里的文件大多含凭据）。
	Mode int `json:"mode,omitempty"`
	// Overwrite 为 true 时覆盖已存在的文件（默认 false，刻意如此）：
	// 配置里往往有用户填的密钥，重装时默认覆盖会冲掉用户数据（ddns-go 的 DNS 密钥丢过一次）。
	Overwrite bool `json:"overwrite,omitempty"`
	// SkipMessage 是"已存在所以跳过"时写给用户的话（默认给一句通用的）。
	SkipMessage string `json:"skip_message,omitempty"`
	// Optional 为 true 时写失败只算警告（必须写 Note）。
	Optional bool `json:"optional,omitempty"`
	// Note 说明 Optional 的用途。
	Note string `json:"note,omitempty"`
	// Owner 是归属（"user" / "root" / ""）。
	Owner string `json:"owner,omitempty"`
}

func (a WriteFileAction) Kind() string { return "write_file" }
func (a WriteFileAction) Describe(ec *ExecConfig) string {
	return "写入 " + ec.path(a.Path)
}

func (a WriteFileAction) Exec(ec *ExecConfig) error { return ec.writeFile(a) }

// ---------- 步骤：run ----------

// RunAction 执行一条命令。
type RunAction struct {
	// Cmd 是命令路径（如 /usr/bin/curl）。**不接受 shell 字符串**：命令与参数分开传，
	// 避免把路径/变量拼进 shell（注入与转义问题都在那里）。
	Cmd string `json:"cmd"`
	// Args 是参数（支持 {root} 占位符）。
	Args []string `json:"args"`
	// As 是执行身份："user"（默认，降权到真实用户）/ "root"。默认降权：Homebrew、
	// pip / venv 都拒绝 root 运行；反过来，写 /Library/LaunchDaemons 必须 root。
	As string `json:"as,omitempty"`
	// Timeout 是超时（必填；0 = 300 秒）。
	Timeout time.Duration `json:"timeout,omitempty"`
	// Env 是追加的环境变量。
	Env []string `json:"env,omitempty"`
	// Retries 是"失败后再试几次"（默认 0 = 不重试）。只对**幂等**命令开放：
	// 重试一个"装到一半"的写操作会造成比失败更糟的状态。
	Retries int `json:"retries,omitempty"`
	// AllowFailure 为 true 时失败只写一条警告（必须写 Note）。与"重装前 bootout"
	// 同类：目标状态是"没有旧实例"，本来就没有时必然报错，而那不是失败。
	AllowFailure bool   `json:"allow_failure,omitempty"`
	Note         string `json:"note,omitempty"`
}

func (a RunAction) Kind() string { return "run" }
func (a RunAction) Describe(ec *ExecConfig) string {
	if a.AllowFailure {
		return ""
	}
	return "执行 " + filepath.Base(a.Cmd)
}

func (a RunAction) Exec(ec *ExecConfig) error { return ec.run(a) }

// ---------- 步骤：wait_http ----------

// WaitHTTPAction 等一个 HTTP 端点就绪。
type WaitHTTPAction struct {
	URL          string        `json:"url"`
	ExpectStatus []int         `json:"expect_status,omitempty"`
	Timeout      time.Duration `json:"timeout,omitempty"`
	// Degrade / DegradeReason 语义同 HealthSpec（默认失败即 error）。
	Degrade       bool   `json:"degrade,omitempty"`
	DegradeReason string `json:"degrade_reason,omitempty"`
}

func (a WaitHTTPAction) Kind() string { return "wait_http" }
func (a WaitHTTPAction) Describe(ec *ExecConfig) string {
	return "等待 " + a.URL + " 可访问"
}

func (a WaitHTTPAction) Exec(ec *ExecConfig) error { return ec.waitHTTP(a) }

// ---------- 步骤：pip_install ----------

// PipInstallAction 在虚拟环境里安装 Python 包。
type PipInstallAction struct {
	// Python 是 venv 里的 python 解释器（支持 {root} 占位符）。
	Python string `json:"python"`
	// Packages 是要安装的包（如 "mlx-audio[server]"）。
	Packages []string `json:"packages"`
	// IndexURL 是 pip 索引（空 = 用执行器算出来的：镜像站优先、回落清华）。
	IndexURL string `json:"index_url,omitempty"`
	// ExtraArgs 是额外参数（如 --upgrade）。
	ExtraArgs []string      `json:"extra_args,omitempty"`
	Timeout   time.Duration `json:"timeout,omitempty"`
	// Verify 是"装完之后用什么命令验证真的可用"（空 = 不验证）。不可省：装上了但
	// import 不了在 pip 退出码上仍是 0（缺 wheel / 依赖冲突 / 缺 extra）。
	Verify     []string `json:"verify,omitempty"`
	VerifyNote string   `json:"verify_note,omitempty"`
}

func (a PipInstallAction) Kind() string { return "pip_install" }
func (a PipInstallAction) Describe(ec *ExecConfig) string {
	return "安装 Python 包 " + strings.Join(a.Packages, " ")
}

func (a PipInstallAction) Exec(ec *ExecConfig) error { return ec.pipInstall(a) }

// ---------- 步骤：pull_model ----------

// PullModelAction 拉一份模型权重（只实现"执行器注入的下载器"这一层）。
type PullModelAction struct {
	// Repo 是模型仓库（如 "Sanster/lama-cleaner-lama"）。
	Repo string `json:"repo"`
	// Dest 是落盘目录（支持 {root} 占位符）。
	Dest string `json:"dest"`
	// Files 是要拉的文件（空 = 整个仓库）。
	Files []string `json:"files,omitempty"`
	// SHA256 是期望的 sha256（有就校验）。
	SHA256 string `json:"sha256,omitempty"`
	// MD5 是期望的 md5（IOPaint 的权重上游只给 md5）。
	MD5 string `json:"md5,omitempty"`
	// SizeBytes 是期望大小（只用于日志与"下完是不是空的"判断）。
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

func (a PullModelAction) Kind() string { return "pull_model" }
func (a PullModelAction) Describe(ec *ExecConfig) string {
	return "下载模型 " + a.Repo
}

func (a PullModelAction) Exec(ec *ExecConfig) error { return ec.pullModel(a) }

// ---------- 步骤：create_db ----------

// CreateDBAction 建库建账号。
type CreateDBAction struct {
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	Charset  string `json:"charset,omitempty"`
}

func (a CreateDBAction) Kind() string { return "create_db" }
func (a CreateDBAction) Describe(ec *ExecConfig) string {
	return "创建数据库 " + a.Name
}

func (a CreateDBAction) Exec(ec *ExecConfig) error { return ec.createDB(a) }

// ---------- 步骤：seed_site ----------

// SeedSiteAction 铺站点文件（Typecho / WordPress 这类"下载整包 → 铺到站点目录"）。
type SeedSiteAction struct {
	// Source 是源目录（支持 {root} 占位符）。
	Source string `json:"source"`
	// Dest 是站点根目录（通常是 nginx vhost 的 root）。
	Dest string `json:"dest"`
	// Overwrite 为 true 时覆盖已存在的文件（默认 false：不冲掉用户的站点）。
	Overwrite bool `json:"overwrite,omitempty"`
}

func (a SeedSiteAction) Kind() string { return "seed_site" }
func (a SeedSiteAction) Describe(ec *ExecConfig) string {
	return "铺站点文件到 " + ec.path(a.Dest)
}

func (a SeedSiteAction) Exec(ec *ExecConfig) error { return ec.seedSite(a) }

// ---------- 步骤：register_service ----------

// RegisterServiceAction 把服务登记进「服务管理」，必须排在 assert_ready **之前**
// （否则验收如实失败时会留下"任务失败、服务管理里又找不到它"的半成品）。
// 登记失败**不**让整个部署失败（刻意降级）：launchd 服务本身是好的、软件能用。
type RegisterServiceAction struct {
	// Label / Name / Icon / Category 是登记信息（空 = 用描述符里的）。
	Label    string `json:"label,omitempty"`
	Name     string `json:"name,omitempty"`
	Icon     string `json:"icon,omitempty"`
	Category string `json:"category,omitempty"`
	// Port 是登记进服务记录的端口（0 = 用描述符的 Port）。
	Port int `json:"port,omitempty"`
}

func (a RegisterServiceAction) Kind() string                   { return "register_service" }
func (a RegisterServiceAction) Describe(ec *ExecConfig) string { return "" }

func (a RegisterServiceAction) Exec(ec *ExecConfig) error { return ec.registerService(a) }

// ---------- 步骤：write_plist ----------

// WritePlistAction 渲染并写入 launchd plist（先写 .tmp 再 rename，避免半个文件）。
type WritePlistAction struct {
	// Args 是 ProgramArguments（第一项通常是 {root}/<binary>；支持 {root} 占位符）。
	Args []string `json:"args"`
	// Template 覆盖默认模板（空 = DefaultPlistTemplate）。
	Template *PlistTemplate `json:"template,omitempty"`
	// Optional 为 true 时失败只算警告（必须写 Note）。
	Optional bool   `json:"optional,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (a WritePlistAction) Kind() string { return "write_plist" }
func (a WritePlistAction) Describe(ec *ExecConfig) string {
	return "生成 launchd 服务定义 " + ec.Spec.Service.PlistPath
}

func (a WritePlistAction) Exec(ec *ExecConfig) error { return ec.writePlist(a) }

// ---------- 步骤：launchd_bootout ----------

// LaunchdBootoutAction 卸载旧实例（幂等：本来没有也算成功）。
type LaunchdBootoutAction struct {
	// Label 为空时用描述符的 Service.Label。
	Label string `json:"label,omitempty"`
}

func (a LaunchdBootoutAction) Kind() string { return "launchd_bootout" }
func (a LaunchdBootoutAction) Describe(ec *ExecConfig) string {
	return "停止旧实例（如果它在跑）"
}

func (a LaunchdBootoutAction) Exec(ec *ExecConfig) error {
	return ec.bootout(a.Label)
}

// ---------- 步骤：launchd_bootstrap ----------

// LaunchdBootstrapAction 装载服务（等旧实例真的消失再装，见 bootstrapService）。
type LaunchdBootstrapAction struct {
	Label string `json:"label,omitempty"`
	Plist string `json:"plist,omitempty"`
	// Message 是成功后写给用户的一行（空 = 默认那句"已注册为系统级后台服务…"）。
	Message string `json:"message,omitempty"`
}

func (a LaunchdBootstrapAction) Kind() string { return "launchd_bootstrap" }
func (a LaunchdBootstrapAction) Describe(ec *ExecConfig) string {
	return "装载并启动服务"
}

func (a LaunchdBootstrapAction) Exec(ec *ExecConfig) error { return ec.bootstrap(a) }

// ---------- 步骤：assert_ready ----------

// AssertReadyAction 就绪判定。**失败即 error**（要降级必须显式写理由）。
type AssertReadyAction struct {
	// What 是被判定的对象（默认用描述符的 Name）。
	What string `json:"what,omitempty"`
	// State / Missing / Remedy 覆盖默认的失败叙述（空 = 用执行器按 HealthSpec 生成的）。
	State   string `json:"state,omitempty"`
	Missing string `json:"missing,omitempty"`
	Remedy  string `json:"remedy,omitempty"`
	// LogPath 覆盖日志路径（空 = 用 Service.ErrLog）。
	LogPath string `json:"log_path,omitempty"`
}

func (a AssertReadyAction) Kind() string { return "assert_ready" }
func (a AssertReadyAction) Describe(ec *ExecConfig) string {
	return "等待服务就绪（" + ec.readyExpect() + "）"
}

func (a AssertReadyAction) Exec(ec *ExecConfig) error { return ec.assertReady(a) }

// ---------- 步骤：message ----------

// MessageAction 只写一条进度（不产生任何副作用）。
// 用途：把**给用户看的话**（安装目录/日志、Notes、凭据区块）原样保留 ——
// 它们往往就是用户唯一的操作指引。
type MessageAction struct {
	// Lines 是要追加的步骤文本，逐条写（空串会写成一个空行，与老实现一致）。
	Lines []string `json:"lines"`
	// Secret 为 true 时这些文本只进 Credentials 区块，不进任务步骤。
	// 口令/token **只允许出现在 InstallResult.Credentials 里** —— 任务步骤会进审计日志。
	Secret bool `json:"secret,omitempty"`
}

func (a MessageAction) Kind() string                   { return "message" }
func (a MessageAction) Describe(ec *ExecConfig) string { return "" }

func (a MessageAction) Exec(ec *ExecConfig) error {
	if a.Secret {
		return fmt.Errorf("message 的 secret 文本必须由执行器直接处理（面板内部错误）")
	}
	ec.Result.step(ec.Ctx, a.Lines...)
	return nil
}

//  Runner：执行器唯一的副作用出口

// Runner 是描述符执行器需要的系统能力。生产实现是 sysRunner（os/* + launchctl），
// 单测注入假实现，于是"整个安装编排"能在不联网、不碰 launchd、不动真实家目录的前提下被验证。
type Runner interface {
	Stat(path string) (exists bool, isDir bool, err error)
	MkdirAll(path string, mode os.FileMode) error
	WriteFile(path string, data []byte, mode os.FileMode) error
	ReadFile(path string) ([]byte, error)
	Size(path string) int64
	Chmod(path string, mode os.FileMode) error
	Rename(from, to string) error
	Remove(path string) error
	RemoveAll(path string) error
	SHA256(path string) (string, error)
	ChownTree(user, path string) error
	CopyTree(from, to string) error
	// FileType 返回 `file -b` 的输出（架构复核用）。
	FileType(ctx context.Context, path string) (string, error)
	// RunAsUser 以真实用户身份执行（brew / pip / curl 都走这条）。
	RunAsUser(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)
	RunAsUserEnv(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) (string, error)
	RunRoot(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)
	// Bootout 卸载 launchd 作业（幂等）。
	Bootout(ctx context.Context, label string) error
	// Bootstrap 装载 launchd 作业（带重试，等旧实例真的消失）。
	Bootstrap(ctx context.Context, label, plist string) error
	// LaunchRunning 报告 launchd 作业此刻是否真的在跑。
	LaunchRunning(label string) (bool, string)
	WaitPort(ctx context.Context, port int, timeout time.Duration) bool
	// HTTPGet 发一次 HTTP GET（wait_http 用），返回状态码。
	HTTPGet(ctx context.Context, url string, timeout time.Duration) (int, error)
	PrimaryIP(ctx context.Context) string
}

// sysRunner 是 Runner 的生产实现。
type sysRunner struct{ m *Manager }

func newSysRunner(m *Manager) Runner { return &sysRunner{m: m} }

func (r *sysRunner) Stat(path string) (bool, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, fi.IsDir(), nil
}

func (r *sysRunner) MkdirAll(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}

func (r *sysRunner) WriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func (r *sysRunner) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (r *sysRunner) Size(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

func (r *sysRunner) Chmod(path string, mode os.FileMode) error { return os.Chmod(path, mode) }

func (r *sysRunner) Rename(from, to string) error { return os.Rename(from, to) }

func (r *sysRunner) Remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (r *sysRunner) RemoveAll(path string) error { return os.RemoveAll(path) }

func (r *sysRunner) SHA256(path string) (string, error) { return fileSHA256(path) }

func (r *sysRunner) ChownTree(user, path string) error { return chownTree(user, path) }

func (r *sysRunner) CopyTree(from, to string) error { return copyTreeNative(from, to) }

func (r *sysRunner) FileType(ctx context.Context, path string) (string, error) {
	return r.m.runRoot(ctx, 10*time.Second, "/usr/bin/file", "-b", path)
}

func (r *sysRunner) RunAsUser(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return r.m.runAsUser(ctx, timeout, name, args...)
}

func (r *sysRunner) RunAsUserEnv(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) (string, error) {
	return r.m.runAsUserEnv(ctx, timeout, env, name, args...)
}

func (r *sysRunner) RunRoot(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return r.m.runRoot(ctx, timeout, name, args...)
}

func (r *sysRunner) Bootout(ctx context.Context, label string) error {
	_, err := r.m.runRoot(ctx, 20*time.Second, "/bin/launchctl", "bootout", "system/"+label)
	return err
}

func (r *sysRunner) Bootstrap(ctx context.Context, label, plist string) error {
	return r.m.bootstrapService(ctx, label, plist)
}

func (r *sysRunner) LaunchRunning(label string) (bool, string) { return readyLaunchRunning(label) }

func (r *sysRunner) WaitPort(ctx context.Context, port int, timeout time.Duration) bool {
	return readyWaitPort(ctx, port, timeout)
}

func (r *sysRunner) HTTPGet(ctx context.Context, url string, timeout time.Duration) (int, error) {
	return httpProbeStatus(ctx, url, timeout)
}

func (r *sysRunner) PrimaryIP(ctx context.Context) string {
	_ = ctx
	return r.m.primaryIP()
}

//  执行器

// ExecConfig 是一次安装执行的现场。
type ExecConfig struct {
	Ctx    context.Context
	Spec   AppDescriptor
	Result *InstallResult
	Runner Runner

	// 执行期状态（由执行器维护，步骤实现只读）
	secrets     configSeedSecrets
	secretLines []string
	registered  bool
	// mirrorDownloaded 记录"哪些产物这次是真的从镜像站下到的"（按产物 Name）。
	// 用集合而非一个 bool：一次安装有多个 download 步骤，bool 会被后一步清掉，
	// 导致主产物明明来自镜像、校验却去取上游清单（0.12.4 的隐蔽错位）。
	mirrorDownloaded map[string]bool
	// pathVars 是 {root} 这类占位符的取值。
	pathVars map[string]string

	// ---- 依赖注入：需要外部能力的步骤走这些钩子（钩子为 nil 时明确报错，不静默跳过）----
	// MirrorPreflight 必须返回**它实际 HEAD 成功的地址**而不是 bool：只回 bool 时地址
	// 永远进不了候选列表，就会出现"日志说走镜像、curl 打 GitHub"（2026-09-17 真机 P0）。
	MirrorPreflight   func(a Artifact) string
	VerifyMirror      func(a Artifact) (sha string, source string, err error)
	FetchChecksumList func(a Artifact) (sha string, source string, err error)
	RegisterService   func(label, name, icon, category string, port int) error
	UnregisterService func(label string) error
	PipIndexURL       func() string
	PullModel         func(a PullModelAction) error
	CreateDB          func(a CreateDBAction) error
	// MirrorBase 是镜像站基址（判断"某个候选地址是不是镜像上的那一份"）。
	MirrorBase string
}

// primaryIP 返回面板探测到的主机地址（只读探针，失败返回空串）。
func (ec *ExecConfig) primaryIP() string {
	if ec.Runner == nil {
		return ""
	}
	return ec.Runner.PrimaryIP(ec.Ctx)
}

// path 展开占位符（{root} 等），并把相对路径按安装根目录解析 ——
// "{root}/frpc.toml" 与 "frpc.toml" 必须得到同一个结果。
func (ec *ExecConfig) path(p string) string {
	p = expandVars(p, ec.pathVars)
	if p == "" {
		return ec.pathVars["{root}"]
	}
	if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~") {
		p = filepath.Join(ec.pathVars["{root}"], p)
	}
	if strings.HasPrefix(p, "~/") {
		if home := ec.pathVars["{home}"]; home != "" {
			p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}

// expandArg 只把 {root} / {home} / {user} 占位符换成字面量，**不做路径拼接**：
// plist 参数里既有路径也有非路径（`server`、`--data`），path() 会把后者拼到 {root} 前面。
// 真机复现（2026-09-17，Alist，这个坑）：launchd 把 `<root>/server` 当子命令，进程立刻退出。
func (ec *ExecConfig) expandArg(s string) string { return expandVars(s, ec.pathVars) }

// expandVars 做字面量替换（不引入模板引擎：值里带 {root} 也不会被二次展开）。
func expandVars(s string, vars map[string]string) string {
	if s == "" || len(vars) == 0 {
		return s
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	// 长 key 优先（当前无前缀冲突，但排序后行为与 key 的书写顺序无关）。
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	repl := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		repl = append(repl, k, vars[k])
	}
	return strings.NewReplacer(repl...).Replace(s)
}

// InstallOptions 是执行一次描述符安装的可选项。
type InstallOptions struct {
	// RemoveData 只对 uninstall_steps 有意义。
	RemoveData bool
}

// ExecuteInstall 按描述符执行安装：任何一步失败即中止。
// 返回的 error 已是面向用户的（说清期望/实际/怎么办），不会再包一层 "install failed"。
func ExecuteInstall(ec *ExecConfig) error {
	if ec.Runner == nil {
		return fmt.Errorf("描述符执行器缺少 Runner（面板内部错误）")
	}
	if ec.Result == nil {
		ec.Result = &InstallResult{App: ec.Spec.ID, Name: ec.Spec.Name}
	}
	if ec.pathVars == nil {
		ec.pathVars = map[string]string{}
	}
	if len(ec.Spec.Steps) == 0 {
		return fmt.Errorf("描述符 %s 没有任何安装步骤", ec.Spec.ID)
	}
	for i, st := range ec.Spec.Steps {
		if st == nil {
			return fmt.Errorf("描述符 %s 的第 %d 个步骤是空的（面板内部错误）", ec.Spec.ID, i+1)
		}
		if d := st.Describe(ec); d != "" {
			ec.Result.step(ec.Ctx, d)
		}
		if err := execStep(ec, st); err != nil {
			return fmt.Errorf("「%s」安装失败（第 %d 步 %s）：%w",
				ec.Spec.Name, i+1, st.Kind(), err)
		}
	}
	return nil
}

// execStep 分发一个步骤。verify_sha256 / message 需要执行器的额外上下文
// （校验清单的取法、秘密文本的去向），所以在这里显式分派。
func execStep(ec *ExecConfig, st InstallStep) error {
	switch a := st.(type) {
	case VerifySHA256Action:
		expect, source := a.Expect, a.Source
		if expect == "" {
			art := a.Artifact
			kind := ""
			if art.Checksum != nil {
				kind = art.Checksum.Kind
			}
			switch {
			case ec.mirrorUsed(art.Name) && ec.VerifyMirror != nil:
				// 走镜像时用镜像清单的 sha256（覆盖全部条目，比上游清单更严）。
				var err error
				if expect, source, err = ec.VerifyMirror(art); err != nil {
					return err
				}
			case kind == "mirror-manifest":
				if ec.VerifyMirror == nil {
					return fmt.Errorf("需要镜像清单校验但执行器没有接入（面板内部错误）")
				}
				var err error
				if expect, source, err = ec.VerifyMirror(art); err != nil {
					return err
				}
			case kind == "upstream-list":
				if ec.FetchChecksumList == nil {
					return fmt.Errorf("需要上游校验清单但执行器没有接入（面板内部错误）")
				}
				var err error
				if expect, source, err = ec.FetchChecksumList(art); err != nil {
					return err
				}
			case a.SkipIfNoChecksum:
				// 上游确实没有清单：如实写一条，靠 verify_arm64 做架构复核。
				ec.Result.step(ec.Ctx, "上游没有提供 sha256 清单，跳过内容校验"+
					"（只能做架构复核，见下一步）")
				return nil
			default:
				return fmt.Errorf("没有可用的 sha256 校验清单（产物 %s）：静默跳过等于"+
					"把\"有校验\"变成\"看运气\"，已中止安装", art.Name)
			}
		}
		return ec.verifySHA256(a.Artifact, expect, source)
	case MessageAction:
		if a.Secret {
			ec.secretLines = append(ec.secretLines, a.Lines...)
			return nil
		}
		ec.Result.step(ec.Ctx, a.Lines...)
		return nil
	default:
		return st.Exec(ec)
	}
}

// UserName 返回真实用户名（由调用方在 pathVars 里提供）。
func (ec *ExecConfig) UserName() string { return ec.pathVars["{user}"] }

// isMirrorURL 判断候选地址是否镜像站上的那一份（镜像基址前缀）。
// 不另存镜像地址：另存会出现"改了镜像基址、判断还用旧值"的错位。
func (ec *ExecConfig) isMirrorURL(u string) bool {
	base := ec.MirrorBase
	if base == "" {
		return false
	}
	return strings.HasPrefix(u, strings.TrimRight(base, "/")+"/")
}

// markMirrorDownload 记下"这个产物这次是不是真的从镜像站下到的"。
// 由**实际用的候选地址**判定：镜像在候选里但下载失败回落官方时不许记成走了镜像。
func (ec *ExecConfig) markMirrorDownload(asset string, fromMirror bool) {
	if ec.mirrorDownloaded == nil {
		ec.mirrorDownloaded = map[string]bool{}
	}
	if fromMirror {
		ec.mirrorDownloaded[asset] = true
		return
	}
	delete(ec.mirrorDownloaded, asset)
}

// mirrorUsed 报告某个产物这次是不是真的从镜像站下到的（决定用哪套 sha256）。
func (ec *ExecConfig) mirrorUsed(asset string) bool { return ec.mirrorDownloaded[asset] }

// mirrorDownloadedAny 报告本次安装是否已经有产物真的从镜像站下到。
func (ec *ExecConfig) mirrorDownloadedAny() bool { return len(ec.mirrorDownloaded) > 0 }

// mirrorChecksumFor 从镜像清单里取某个产物的 sha256。
// 只返回"期望值 + 来源"：日志由步骤系统统一负责，两处各写一遍必然不一致。
func (m *Manager) mirrorChecksumFor(ctx context.Context, spec releaseBinaryApp) (string, error) {
	url := m.appManifestURL(spec.ID, spec.Tag)
	mm, err := m.fetchMirrorManifest(ctx, spec.ID, spec.Tag)
	if err != nil {
		return "", fmt.Errorf("校验失败，已中止安装：%w"+
			"（清单由同步工具生成：在能访问上游的机器上执行 `make sync-apps`）", err)
	}
	want := mm.sha256For(spec.Asset)
	if want == "" {
		return "", fmt.Errorf("镜像清单里没有 %s 的 sha256（%s）。已中止安装；"+
			"请重新执行 `make sync-apps` 让清单与包一致", spec.Asset, url)
	}
	return want, nil
}

// ---------- 步骤实现的内部方法 ----------

func (ec *ExecConfig) chownUser(path string) error {
	if ec.Spec.Service.RunAs == "root" {
		return nil
	}
	user := ec.UserName()
	if user == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户（不能把 %s 的归属改错）", path)
	}
	return ec.Runner.ChownTree(user, path)
}

// download 执行下载（含镜像优先、超时阶梯、磁盘已有产物的退路）。
func (ec *ExecConfig) download(a DownloadAction, dest string) error {
	urls := a.Artifact.URLs
	if len(urls) == 0 {
		return fmt.Errorf("产物 %s 没有下载地址", a.Artifact.Name)
	}
	part := dest + ".part"
	var lastErr error
	for _, u := range urls {
		label, maxTime := ec.downloadSource(u)
		ec.Result.step(ec.Ctx, fmt.Sprintf("下载 %s（%s %s，上限 %d 秒）",
			a.Artifact.Name, label, hostOf(u), maxTime))
		fromMirror := ec.isMirrorURL(u)
		started := time.Now()
		args := []string{
			"-fL", "--retry", "1", "--connect-timeout", "20",
			"--max-time", strconv.Itoa(maxTime),
			// --speed-limit/--speed-time：防的是"连上了但完全不走数据"的停滞
			"--speed-limit", "1024", "--speed-time", "30",
			"-o", part, u,
		}
		out, err := ec.Runner.RunAsUser(ec.Ctx, 15*time.Minute, "/usr/bin/curl", args...)
		elapsed := time.Since(started).Seconds()
		if err == nil {
			if rerr := ec.Runner.Rename(part, dest); rerr != nil {
				return fmt.Errorf("下载完成但保存到 %s 失败: %w", dest, rerr)
			}
			// 只有真的从镜像下到才算走了镜像（回落官方不许记成走了镜像）。
			ec.markMirrorDownload(a.Artifact.Name, fromMirror)
			size := ec.Runner.Size(dest)
			// 把实际耗时/速度写进任务日志，便于事后判断该不该调超时。
			ec.Result.step(ec.Ctx, fmt.Sprintf("下载完成：%s（%.1f 秒，约 %s/s）",
				humanBytes(size), elapsed,
				humanBytes(int64(float64(size)/max64(elapsed, 0.1)))))
			return nil
		}
		lastErr = fmt.Errorf("%s 失败: %v（%s）", u, err, tailText(out, 200))
		ec.Result.step(ec.Ctx, "下载失败，换下一个地址："+tailText(out, 200))
		_ = ec.Runner.Remove(part)
	}
	// 自动下载全失败：磁盘上已有该产物就用它继续 —— 否则"手动下载放到 <root>"就是空话。
	if ok, _, _ := ec.Runner.Stat(dest); ok {
		ec.Result.step(ec.Ctx, "所有自动下载地址都失败，改用磁盘上已有的 "+dest)
		return nil
	}
	return fmt.Errorf("下载 %s 失败（%d 个地址都试过）：%v。"+
		"可在网络可达时手动下载该文件放到 %s，再重新点安装",
		a.Artifact.Name, len(urls), lastErr, filepath.Dir(dest))
}

// downloadSource 给出候选地址的来源标签与截止时间：镜像站/官方 150 秒、
// 其余（第三方加速镜像）300 秒 —— 实测官方约 46KB/s、加速镜像约 640KB/s。
// 按**来源**而非候选位置判定：按位置时镜像排第一会让官方地址也拿到 300 秒。
func (ec *ExecConfig) downloadSource(u string) (label string, maxTime int) {
	switch {
	case ec.isMirrorURL(u):
		return "镜像站", 150
	case hostOf(u) == "github.com":
		return "官方地址", 150
	default:
		return "加速镜像（第三方）", 300
	}
}

// verifySHA256 按给定期望值校验产物内容。
func (ec *ExecConfig) verifySHA256(art Artifact, expect, source string) error {
	path := ec.path("{root}/" + art.Name)
	ok, _, err := ec.Runner.Stat(path)
	if err != nil || !ok {
		return fmt.Errorf("找不到要校验的产物 %s", path)
	}
	got, err := ec.Runner.SHA256(path)
	if err != nil {
		return fmt.Errorf("计算 %s 的 sha256 失败: %w", path, err)
	}
	if err := matchChecksum(art.Name, expect, got, source, ec.path("{root}")); err != nil {
		return err
	}
	ec.Result.step(ec.Ctx, fmt.Sprintf("SHA-256 校验通过：%s 与 %s 一致（来源：%s）",
		art.Name, source, source))
	return nil
}

// verifyArm64 用 file(1) 复核二进制是 arm64。
func (ec *ExecConfig) verifyArm64(path string) error {
	if ok, isDir, err := ec.Runner.Stat(path); err != nil || !ok || isDir {
		return fmt.Errorf("解压后没有找到可执行文件 %s", path)
	}
	out, err := ec.Runner.FileType(ec.Ctx, path)
	if err != nil {
		return fmt.Errorf("复核二进制架构失败: %v（%s）", err, tailText(out, 200))
	}
	if !strings.Contains(out, "arm64") {
		return fmt.Errorf("下载到的 %s 不是 arm64 原生二进制（file 报告：%s）。"+
			"面板只允许原生 arm64（不跑 Rosetta 转译），已中止安装",
			filepath.Base(path), strings.TrimSpace(out))
	}
	return nil
}

// extract 解包并挑成员。
func (ec *ExecConfig) extract(a ExtractAction) error {
	asset := ec.path("{root}/" + a.Artifact.Name)
	dest := a.Dest
	if dest == "" {
		dest = "{root}"
	}
	dest = ec.path(dest)
	if a.BootoutFirst {
		// 重装先停旧实例（覆写正在执行的 Mach-O 会被系统杀掉）；失败无所谓。
		_ = ec.Runner.Bootout(ec.Ctx, ec.Spec.Service.Label)
	}
	switch a.Artifact.Kind {
	case ArtifactTarGz, "":
		if out, err := ec.Runner.RunAsUser(ec.Ctx, 3*time.Minute,
			"/usr/bin/tar", tarExtractArgs(asset, dest, a.Member, a.StripComponents)...); err != nil {
			return fmt.Errorf("解压失败: %v（%s）", err, tailText(out, 300))
		}
	case ArtifactZip:
		// zip 无 --strip-components，这里只做整体解压（需要挑成员的 zip 目前没有）。
		if a.StripComponents != 0 || a.Member != "" {
			return fmt.Errorf("zip 产物暂不支持 StripComponents/Member（产物 %s）", a.Artifact.Name)
		}
		if out, err := ec.Runner.RunAsUser(ec.Ctx, 3*time.Minute,
			"/usr/bin/unzip", "-o", "-q", asset, "-d", dest); err != nil {
			return fmt.Errorf("解压失败: %v（%s）", err, tailText(out, 300))
		}
	case ArtifactBinary:
		target := filepath.Join(dest, filepath.Base(asset))
		if target != asset {
			if err := ec.copyTree(CopyAction{From: asset, To: target}); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("产物 %s 的类型 %q 不支持解压", a.Artifact.Name, a.Artifact.Kind)
	}
	if a.ExpectFile != "" {
		want := ec.path(a.ExpectFile)
		if ok, isDir, err := ec.Runner.Stat(want); err != nil || !ok || isDir {
			return fmt.Errorf("解压后没有找到 %s —— 归档成员路径与描述符不一致"+
				"（产物 %s，成员 %q，剥 %d 层）；tar 退出码是 0 也可能什么都没解出来",
				want, a.Artifact.Name, a.Member, a.StripComponents)
		}
	}
	return nil
}

// tarExtractArgs 组装 tar 的解压参数（选项必须在成员名前，所以返回全部参数）。
// 成员名是否带顶层目录取决于 StripComponents：>0 是 <顶层目录>/<binary>，==0 就是 <binary>。
// 真机踩过：ddns-go 产物没有顶层目录，按 asset 名猜路径会让 tar 找一个不存在的成员而失败。
func tarExtractArgs(asset, dest, member string, strip int) []string {
	args := []string{"-xzf", asset, "-C", dest}
	if member == "" {
		if strip > 0 {
			args = append(args, fmt.Sprintf("--strip-components=%d", strip))
		}
		return args
	}
	if strip > 0 {
		args = append(args, fmt.Sprintf("--strip-components=%d", strip))
		top := strings.TrimSuffix(filepath.Base(asset), ".tar.gz")
		if top != filepath.Base(asset) && top != "" {
			member = top + "/" + member
		}
	}
	return append(args, member)
}

// copyTree 复制文件或目录树。
func (ec *ExecConfig) copyTree(a CopyAction) error {
	from, to := ec.path(a.From), ec.path(a.To)
	if ok, _, err := ec.Runner.Stat(from); err != nil || !ok {
		if a.Optional {
			ec.Result.step(ec.Ctx, "（可选文件不存在，跳过："+from+"；"+a.Note+"）")
			return nil
		}
		return fmt.Errorf("找不到源文件 %s", from)
	}
	if err := ec.Runner.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", filepath.Dir(to), err)
	}
	if err := ec.Runner.CopyTree(from, to); err != nil {
		return fmt.Errorf("复制 %s → %s 失败: %w", from, to, err)
	}
	if a.Mode != 0 {
		if err := ec.Runner.Chmod(to, os.FileMode(a.Mode)); err != nil {
			return fmt.Errorf("设置 %s 权限失败: %w", to, err)
		}
	}
	return nil
}

// writeFile 写文件。
func (ec *ExecConfig) writeFile(a WriteFileAction) error {
	path := ec.path(a.Path)
	mode := os.FileMode(0o600)
	if a.Mode != 0 {
		mode = os.FileMode(a.Mode)
	}
	if exists, _, _ := ec.Runner.Stat(path); exists {
		if !a.Overwrite {
			msg := a.SkipMessage
			if msg == "" {
				msg = "已保留现有文件 " + path + "（面板不覆盖你的改动）"
			}
			ec.Result.step(ec.Ctx, msg)
			return nil
		}
	}
	if err := ec.Runner.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", filepath.Dir(path), err)
	}
	if err := ec.Runner.WriteFile(path, []byte(a.Content), mode); err != nil {
		if a.Optional {
			ec.Result.step(ec.Ctx, "警告：写入 "+path+" 失败（"+a.Note+"）："+err.Error())
			return nil
		}
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	switch a.Owner {
	case "", "none", "root":
	case "user":
		if err := ec.chownUser(path); err != nil {
			return err
		}
	}
	return nil
}

// run 执行一条命令。
func (ec *ExecConfig) run(a RunAction) error {
	if a.Cmd == "" {
		return fmt.Errorf("run 步骤缺少命令")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	args := make([]string, 0, len(a.Args))
	for _, x := range a.Args {
		args = append(args, ec.path(x))
	}
	runOnce := func() (string, error) {
		switch a.As {
		case "root":
			return ec.Runner.RunRoot(ec.Ctx, timeout, a.Cmd, args...)
		case "", "user":
			if len(a.Env) > 0 {
				return ec.Runner.RunAsUserEnv(ec.Ctx, timeout, a.Env, a.Cmd, args...)
			}
			return ec.Runner.RunAsUser(ec.Ctx, timeout, a.Cmd, args...)
		default:
			return "", fmt.Errorf("run 的 as=%q 不支持（user / root）", a.As)
		}
	}
	var out string
	var err error
	for attempt := 0; attempt <= a.Retries; attempt++ {
		out, err = runOnce()
		if err == nil {
			return nil
		}
		if attempt < a.Retries {
			ec.Result.step(ec.Ctx, fmt.Sprintf("命令失败，重试（第 %d 次）：%s",
				attempt+2, tailText(out, 160)))
		}
	}
	if a.AllowFailure {
		ec.Result.step(ec.Ctx, "（这一步失败是可接受的："+a.Note+"；"+tailText(out, 200)+"）")
		return nil
	}
	note := ""
	if a.Note != "" {
		note = "（" + a.Note + "）"
	}
	return fmt.Errorf("%s 失败%s: %v（%s）", a.Cmd, note, err, tailText(out, 300))
}

// waitHTTP 等一个 HTTP 端点就绪。
func (ec *ExecConfig) waitHTTP(a WaitHTTPAction) error {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last string
waitLoop:
	for {
		code, err := ec.Runner.HTTPGet(ec.Ctx, a.URL, 5*time.Second)
		if err == nil && httpStatusAcceptable(code, a.ExpectStatus) {
			ec.Result.step(ec.Ctx, fmt.Sprintf("%s 已就绪（HTTP %d）", a.URL, code))
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("HTTP %d", code)
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ec.Ctx.Done():
			last += "（等待被取消）"
			break waitLoop
		case <-time.After(500 * time.Millisecond):
		}
	}
	msg := fmt.Sprintf("%s 未就绪：期望返回 2xx/3xx，但实际 %s（已等待 %s）",
		a.URL, last, timeout)
	if a.Degrade {
		if strings.TrimSpace(a.DegradeReason) == "" {
			return fmt.Errorf("%s：显式降级必须给出 DegradeReason", msg)
		}
		msg += "（这是**可接受的降级**：" + a.DegradeReason + "）"
		ec.Result.Warning = msg
		ec.Result.step(ec.Ctx, "警告："+msg)
		return nil
	}
	ec.Result.Warning = msg
	return fmt.Errorf("%s", msg)
}

// pipInstall 在 venv 里装 Python 包。
func (ec *ExecConfig) pipInstall(a PipInstallAction) error {
	if len(a.Packages) == 0 {
		return fmt.Errorf("pip_install 没有写要装哪些包")
	}
	python := ec.path(a.Python)
	index := a.IndexURL
	if index == "" && ec.PipIndexURL != nil {
		index = ec.PipIndexURL()
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	args := []string{"-m", "pip", "install"}
	if index != "" {
		args = append(args, "-i", index)
		// 索引不可达时要能回落到官方源（与既有 pip 实现同一策略）。
		args = append(args, "--extra-index-url", "https://pypi.org/simple")
	}
	args = append(args, a.ExtraArgs...)
	args = append(args, a.Packages...)
	if out, err := ec.Runner.RunAsUser(ec.Ctx, timeout, python, args...); err != nil {
		return fmt.Errorf("pip 安装 %s 失败: %v（%s）",
			strings.Join(a.Packages, " "), err, tailText(out, 400))
	}
	if len(a.Verify) > 0 {
		vargs := make([]string, 0, len(a.Verify))
		for _, x := range a.Verify {
			vargs = append(vargs, ec.path(x))
		}
		if out, err := ec.Runner.RunAsUser(ec.Ctx, 2*time.Minute, python, vargs...); err != nil {
			return fmt.Errorf("包装上了但验证不通过（%s）：%v（%s）",
				a.VerifyNote, err, tailText(out, 300))
		}
	}
	ec.Result.step(ec.Ctx, "已安装 "+strings.Join(a.Packages, " "))
	return nil
}

// pullModel 拉模型权重（需要外部下载器，未注入即明确失败）。
func (ec *ExecConfig) pullModel(a PullModelAction) error {
	if ec.PullModel == nil {
		return fmt.Errorf("这一步需要模型下载器，但当前执行器没有接入"+
			"（pull_model：%s）。这是**如实失败**，不是跳过 —— "+
			"没有权重却报安装成功，用户会得到一个用不了的服务", a.Repo)
	}
	return ec.PullModel(a)
}

// createDB 建库（需要数据库管理员能力，未注入即明确失败）。
func (ec *ExecConfig) createDB(a CreateDBAction) error {
	if ec.CreateDB == nil {
		return fmt.Errorf("这一步需要数据库管理员能力，但当前执行器没有接入"+
			"（create_db：%s）。这是**如实失败**，不是跳过", a.Name)
	}
	return ec.CreateDB(a)
}

// seedSite 铺站点文件。
func (ec *ExecConfig) seedSite(a SeedSiteAction) error {
	src, dest := ec.path(a.Source), ec.path(a.Dest)
	if ok, _, err := ec.Runner.Stat(src); err != nil || !ok {
		return fmt.Errorf("找不到要铺的站点源目录 %s", src)
	}
	if exists, isDir, _ := ec.Runner.Stat(dest); exists && !a.Overwrite {
		if isDir {
			ec.Result.step(ec.Ctx, "站点目录已存在，保留现有文件："+dest)
			return nil
		}
		return fmt.Errorf("站点目标 %s 已存在且不是目录（不会覆盖它）", dest)
	}
	if err := ec.Runner.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("创建站点目录 %s 失败: %w", dest, err)
	}
	if err := ec.Runner.CopyTree(src, dest); err != nil {
		return fmt.Errorf("铺站点文件失败: %w", err)
	}
	return nil
}

// registerService 登记进「服务管理」（失败只写一条说明，不让整个部署失败）。
func (ec *ExecConfig) registerService(a RegisterServiceAction) error {
	label := a.Label
	if label == "" {
		label = ec.Spec.Service.Label
	}
	name := a.Name
	if name == "" {
		name = ec.Spec.Name
	}
	icon := a.Icon
	if icon == "" {
		icon = ec.Spec.Icon
	}
	category := a.Category
	if category == "" {
		category = ec.Spec.Category
	}
	port := a.Port
	if port == 0 {
		port = ec.Spec.Port
	}
	if ec.RegisterService == nil {
		ec.Result.step(ec.Ctx, "（没有接入服务登记：跳过登记，服务本身已就位）")
		return nil
	}
	if err := ec.RegisterService(label, name, icon, category, port); err != nil {
		ec.Result.step(ec.Ctx, "（自动登记到服务管理失败："+err.Error()+"）")
		return nil
	}
	ec.registered = true
	return nil
}

// writePlist 渲染并写入 launchd plist。
func (ec *ExecConfig) writePlist(a WritePlistAction) error {
	plistPath := ec.Spec.Service.PlistPath
	if plistPath == "" {
		return fmt.Errorf("描述符 %s 没有写 Service.PlistPath", ec.Spec.ID)
	}
	body, err := ec.renderPlist(a)
	if err != nil {
		return err
	}
	if err := ec.Runner.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", filepath.Dir(plistPath), err)
	}
	tmp := plistPath + ".tmp"
	if err := ec.Runner.WriteFile(tmp, []byte(body), 0o644); err != nil {
		if a.Optional {
			ec.Result.step(ec.Ctx, "警告：写入 plist 失败（"+a.Note+"）："+err.Error())
			return nil
		}
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	if err := ec.Runner.Rename(tmp, plistPath); err != nil {
		return fmt.Errorf("安装 plist 失败: %w", err)
	}
	return nil
}

// renderPlist 用描述符渲染 plist。
func (ec *ExecConfig) renderPlist(a WritePlistAction) (string, error) {
	tpl := DefaultPlistTemplate
	extra := ""
	if a.Template != nil {
		if a.Template.Body != "" {
			tpl = a.Template.Body
		}
		extra = a.Template.ExtraKeys
	}
	if ec.Spec.Service.Template != nil {
		if ec.Spec.Service.Template.Body != "" && (a.Template == nil || a.Template.Body == "") {
			tpl = ec.Spec.Service.Template.Body
		}
		if extra == "" {
			extra = ec.Spec.Service.Template.ExtraKeys
		}
	}
	var argLines strings.Builder
	for _, arg := range a.Args {
		argLines.WriteString("        <string>")
		argLines.WriteString(xmlEscape(ec.expandArg(arg)))
		argLines.WriteString("</string>\n")
	}
	var envXML strings.Builder
	for _, e := range ec.Spec.Service.Env {
		envXML.WriteString("        <key>")
		envXML.WriteString(xmlEscape(e.Name))
		envXML.WriteString("</key>\n        <string>")
		envXML.WriteString(xmlEscape(ec.expandArg(e.Value)))
		envXML.WriteString("</string>\n")
	}
	out := strings.NewReplacer(
		"{label}", xmlEscape(ec.Spec.Service.Label),
		"{user}", xmlEscape(ec.serviceUser()),
		"{args}", argLines.String(),
		"{workdir}", xmlEscape(ec.workDir()),
		"{runatload}", xmlBool(ec.Spec.Service.RunAtLoadOrDefault()),
		"{keepalive}", xmlBool(ec.Spec.Service.KeepAliveOrDefault()),
		"{path}", DefaultPATH,
		"{envxml}", envXML.String(),
		"{outlog}", xmlEscape(ec.logPath("out")),
		"{errlog}", xmlEscape(ec.logPath("err")),
		"{extrakeys}", extra,
	).Replace(tpl)
	return out, nil
}

// serviceUser 返回 plist 里 UserName 的取值。
func (ec *ExecConfig) serviceUser() string {
	if ec.Spec.Service.RunAs == "root" {
		return "root"
	}
	return ec.UserName()
}

// workDir 返回 plist 里 WorkingDirectory 的取值。
func (ec *ExecConfig) workDir() string {
	if ec.Spec.Service.WorkDir == "" {
		return ec.path("{root}")
	}
	return ec.path(ec.Spec.Service.WorkDir)
}

// logPath 返回 out/err 日志路径。
func (ec *ExecConfig) logPath(which string) string {
	name := ec.Spec.Service.OutLog
	if which == "err" {
		name = ec.Spec.Service.ErrLog
	}
	if name == "" {
		if which == "err" {
			name = "launchd.err.log"
		} else {
			name = "launchd.out.log"
		}
	}
	return ec.path(name)
}

// bootout 卸载旧实例。
func (ec *ExecConfig) bootout(label string) error {
	if label == "" {
		label = ec.Spec.Service.Label
	}
	// 幂等：本来没有这个作业时 bootout 必然报错，那不是失败。
	_ = ec.Runner.Bootout(ec.Ctx, label)
	return nil
}

// bootstrap 装载服务。
func (ec *ExecConfig) bootstrap(a LaunchdBootstrapAction) error {
	label := a.Label
	if label == "" {
		label = ec.Spec.Service.Label
	}
	plist := a.Plist
	if plist == "" {
		plist = ec.Spec.Service.PlistPath
	}
	if err := ec.Runner.Bootstrap(ec.Ctx, label, plist); err != nil {
		return err
	}
	msg := a.Message
	if msg == "" {
		msg = "已注册为系统级后台服务（开机自启、不依赖用户登录）"
	}
	ec.Result.step(ec.Ctx, msg)
	return nil
}

// readyExpect 生成"期望什么"的描述（与老实现逐字一致）。
func (ec *ExecConfig) readyExpect() string {
	if ec.Spec.Health.Port > 0 {
		return fmt.Sprintf("TCP 端口 %d 开始监听", ec.Spec.Health.Port)
	}
	return "launchd 把这个作业真正拉起来（该应用不监听任何端口）"
}

// assertReady 就绪判定：探针 + assertReady（失败即 error）。
func (ec *ExecConfig) assertReady(a AssertReadyAction) error {
	what := a.What
	if what == "" {
		what = ec.Spec.Name
	}
	port := ec.Spec.Health.Port
	expect := ec.readyExpect()
	state := a.State
	if state == "" {
		state = "二进制、配置与 launchd 服务都已就位，服务也已登记进服务管理"
		if !ec.registered {
			state = "二进制、配置与 launchd 服务都已就位（但登记进服务管理失败）"
		}
	}
	missing := a.Missing
	if missing == "" {
		missing = "但服务实际上不可用，管理界面与它自己的功能现在都连不上"
		if port <= 0 {
			missing = "但 launchd 也没能把它跑起来，它实际上不可用"
		}
	}
	logPath := a.LogPath
	if logPath == "" {
		logPath = ec.logPath("err")
		if ec.Spec.Service.StdoutTail != "" {
			logPath = ec.path(ec.Spec.Service.StdoutTail)
		}
	}
	timeout := ec.Spec.Health.TimeoutOr()
	probe := ec.Spec.Health.Probe
	// 没声明探针时按端口自动选：不监听端口的应用只能看 launchd，等端口必然误报超时。
	if probe == ProbePort && port <= 0 {
		probe = ProbeLaunchd
	}
	if probe == "" {
		if port > 0 {
			probe = ProbePort
		} else {
			probe = ProbeLaunchd
		}
	}
	remedy := a.Remedy
	if remedy == "" {
		remedy = "按下面的日志尾部里的报错修好后重新部署；" +
			"也可以在「服务管理」里点「重启服务」再试"
	}
	return assertReady(ec.Ctx, readySpec{
		What:    what,
		Expect:  expect,
		Timeout: timeout,
		Probe: func(ctx context.Context) readyVerdict {
			switch probe {
			case ProbeLaunchd:
				// 不监听端口的应用（纯出站连接）只能看 launchd 有没有真的把它拉起来。
				ok, detail := ec.Runner.LaunchRunning(ec.Spec.Service.Label)
				if ok {
					return readyVerdict{OK: true, Actual: "已就绪，" + detail}
				}
				return readyVerdict{Actual: detail}
			case ProbeHTTP:
				url := ec.path(ec.Spec.Health.Path)
				if !strings.HasPrefix(url, "http") {
					url = fmt.Sprintf("http://127.0.0.1:%d%s", port, ec.Spec.Health.Path)
				}
				code, err := ec.Runner.HTTPGet(ctx, url, timeout)
				if err != nil {
					return readyVerdict{Actual: fmt.Sprintf("连 %s 一直失败：%v", url, err)}
				}
				if httpStatusAcceptable(code, ec.Spec.Health.ExpectStatus) {
					return readyVerdict{OK: true, Actual: fmt.Sprintf("已就绪，%s 返回 HTTP %d", url, code)}
				}
				return readyVerdict{Actual: fmt.Sprintf("%s 返回 HTTP %d", url, code)}
			default:
				if ec.Runner.WaitPort(ctx, port, timeout) {
					return readyVerdict{OK: true,
						Actual: fmt.Sprintf("已就绪，监听 %d 端口", port)}
				}
				return readyVerdict{Actual: fmt.Sprintf(
					"连 127.0.0.1:%d 一直失败，端口始终没有监听", port)}
			}
		},
		LogPath:       logPath,
		State:         state,
		Missing:       missing,
		Remedy:        remedy,
		Degrade:       ec.Spec.Health.Degrade,
		DegradeReason: ec.Spec.Health.DegradeReason,
		Result:        ec.Result,
	})
}

// ---------- 小工具 ----------

// httpProbeStatus 发一次 HTTP GET 并返回状态码（网络错误时返回 0 与原因）。
func httpProbeStatus(ctx context.Context, url string, timeout time.Duration) (int, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// 不跟随重定向：3xx 本身就是"服务活着"的证据。
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	return res.StatusCode, nil
}

// copyTreeNative 递归复制文件或目录树（保留权限位）。
func copyTreeNative(from, to string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, fi.Mode().Perm())
	}
	if err := os.MkdirAll(to, fi.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTreeNative(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func xmlBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// httpStatusAcceptable 判定 HTTP 状态码是否算"健康"（判据与 health.go 一致）：
// 2xx/3xx 都算（ddns-go 未登录时 GET / 返回 307 → /login）。
func httpStatusAcceptable(code int, extra []int) bool {
	for _, s := range extra {
		if code == s {
			return true
		}
	}
	return (code >= 200 && code < 400)
}
