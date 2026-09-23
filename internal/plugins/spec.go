// Package plugins 定义「应用插件」的声明式规范（zizpanel.app/v1）与它的校验/干跑。
//
// 为什么要有它（用户 2026-09-23 的设想）：现在每加一个应用 = 写一套安装器 + 探针 +
// 卸载计划 + 门禁，同一个坑（TCC 授权、降权、假状态、端口绑定、产物清理）被反复重踩。
// 把能力抽成"一张签名的表"之后，新增应用变成填表 + 一条校验命令，而坑由框架统一兜住。
//
// 安全边界（铁律，别松）：**只允许声明式动词，绝不允许携带任意命令**。
// 面板是 root 守护进程，插件等于代码执行权 —— 所以：
//
//	· 结构体里没有任何"跑一段命令/脚本"的字段；
//	· 解码用 DisallowUnknownFields，出现 steps/script/command 之类未知键直接报错；
//	· 需要定制的动作（比如 Syncthing 改 GUI 监听与口令）只能引用**面板内建**的补丁名，
//	  名字不在白名单里就拒绝。
package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SchemaV1 是当前唯一受支持的规范版本。
const SchemaV1 = "zizpanel.app/v1"

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Spec 是一份应用插件声明。
type Spec struct {
	Schema  string `json:"schema"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Summary string `json:"summary,omitempty"`
	// Notes 是给用户看的安装说明（每条一句话）。
	Notes []string `json:"notes,omitempty"`

	Source    Source    `json:"source"`
	Run       Run       `json:"run"`
	Config    *Config   `json:"config,omitempty"`
	Expose    *Expose   `json:"expose,omitempty"`
	Verify    Verify    `json:"verify"`
	Health    Health    `json:"health"`
	Uninstall Uninstall `json:"uninstall"`
	Update    *Update   `json:"update,omitempty"`
	Requires  *Requires `json:"requires,omitempty"`
}

// Source 说明产物从哪来、怎么验。三选一，没有第四种。
type Source struct {
	Kind string `json:"kind"` // brew | release | compose
	// brew
	Formula string `json:"formula,omitempty"`
	// release（GitHub / 镜像站的裸产物或归档）
	Repo    string `json:"repo,omitempty"`
	Tag     string `json:"tag,omitempty"` // 留空 = 由镜像索引决定（动态）
	Asset   string `json:"asset,omitempty"`
	Binary  string `json:"binary,omitempty"`
	TarGz   bool   `json:"targz,omitempty"`
	PickOne bool   `json:"pick_one,omitempty"` // 归档里只取 Binary 那一个成员
	// compose
	ComposeYAML string `json:"compose_yaml,omitempty"`
	// Checksum 是内容校验强度，必须如实声明（不许把"只做了架构复核"写成 sha256）
	Checksum string `json:"checksum,omitempty"` // sha256 | md5 | arch-only
}

// Run 说明装完之后"由谁、以什么身份、怎么跑"。
type Run struct {
	Mode string `json:"mode"` // panel-daemon | app-daemon | brew-service | compose | none
	// panel-daemon：面板二进制里的 supervisor 拉起应用（**继承面板的 TCC 授权**）
	Supervise *Supervise `json:"supervise,omitempty"`
	// app-daemon：launchd 直接跑应用自己的二进制（数据放在受隐私保护目录时会踩坑）
	Bin  string   `json:"bin,omitempty"`
	Args []string `json:"args,omitempty"`
	// brew-service：brew 的 service 定义（没有 service 块的 formula 必须选别的模式）
	// 定制的后置动作只能引用面板内建补丁名（白名单见 builtinHooks）
	Hooks []string `json:"hooks,omitempty"`
}

// Supervise 是"面板托管守护进程"的形态（与 zizvideo / aria2 同一套）。
type Supervise struct {
	Bin      string   `json:"bin"`
	Args     []string `json:"args,omitempty"`
	Subcmd   string   `json:"subcmd,omitempty"` // 默认 <id>-supervise
	LogDir   string   `json:"log_dir,omitempty"`
	WorkDir  string   `json:"work_dir,omitempty"`
	NoExpand bool     `json:"no_expand,omitempty"`
}

// Config 是"面板维护的配置文件"。
type Config struct {
	Path string `json:"path"`
	Seed string `json:"seed,omitempty"` // 相对插件目录的模板文件；空 = 应用自己创建
	Mode string `json:"mode,omitempty"` // 默认 0600
	Own  string `json:"own,omitempty"`  // user（默认）| root
	// Set 是**声明式配置补丁**：装上之后把配置里的这几个键改成这些值
	// （见 patch.go）。有了它，"只差一行配置"的应用（改监听地址/端口）才能靠填表上架，
	// 而不必为每个应用写一个面板内建补丁。
	Set map[string]string `json:"set,omitempty"`
	// Format 是配置文件格式：kv（默认）/ ini / yaml。
	Format string `json:"format,omitempty"`
	// Section 只在 ini 里有意义（限定改哪一节）。
	Section string `json:"section,omitempty"`
	// IfMissing：配置文件还不存在时怎么办：skip（默认，不动）/ create（新建最小文件）。
	IfMissing string `json:"if_missing,omitempty"`
	// Secrets 是单补丁简写：等价于第一条补丁的 secrets（随机生成、只展示一次）。
	Secrets []string `json:"secrets,omitempty"`
	// Patches 是**多条**补丁：一个文件里可能要改多个 section（couchdb 既要写 [admins]
	// 口令、又要把 [chttpd] 的监听地址改成 0.0.0.0）。写了 patches 就不能再用上面的简写。
	Patches []Patch `json:"patches,omitempty"`
}

// PatchList 返回这份配置声明**真正要执行的补丁**（简写与列表二选一）。
func (c *Config) PatchList() []Patch {
	if c == nil {
		return nil
	}
	if len(c.Patches) > 0 {
		return c.Patches
	}
	if len(c.Set) == 0 && len(c.Secrets) == 0 {
		return nil
	}
	return []Patch{{
		Format: c.Format, Section: c.Section, Set: c.Set,
		IfMissing: c.IfMissing, Secrets: c.Secrets,
	}}
}

// Expose 是"用户从哪里打开它"。**一个应用只有一个规范入口。**
type Expose struct {
	Port   int    `json:"port,omitempty"`
	Bind   string `json:"bind,omitempty"` // 默认 0.0.0.0（用户 2026-09-23：局域网直连）
	UIPort int    `json:"ui_port,omitempty"`
	UI     string `json:"ui,omitempty"` // panel-hosted（面板托管界面）| app（应用自带）
	Path   string `json:"path,omitempty"`
}

// Probe 是探针的三种形态（够覆盖 http / json-rpc / 进程 / 端口）。
type Probe struct {
	Kind string `json:"kind"` // http | rpc | process | port
	// http
	Path   string `json:"path,omitempty"`
	Expect string `json:"expect,omitempty"`
	// rpc
	URL       string `json:"url,omitempty"`
	Method    string `json:"method,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`
	ExpectKey string `json:"expect_key,omitempty"`
	// process / port
	Label   string `json:"label,omitempty"`
	Port    int    `json:"port,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// Verify 是安装/修复收尾时必须**真的跑一次**的验收（贴运行体，不许"端口在听就算好"）。
type Verify struct {
	AnyOf []Probe `json:"any_of"`
}

// Health 是列表/卡片上"运行中"的判据来源。**必填**：没有它就只能显示"未复核"。
type Health struct {
	Probe
	Interval string `json:"interval,omitempty"`
}

// Uninstall 声明"删什么、留什么"。
//
// 与面板卸载对话框的语义一一对应（别混）：
//   - Always：卸载一定删的（launchd plist、面板记录、共享二进制）；
//   - OptionalData：**勾选「删除数据」才删**的用户数据/配置目录（默认保留）；
//   - KeepNote：给用户看的一句话（纯文案，门禁不比对）。
type Uninstall struct {
	Always       []string `json:"always,omitempty"`
	OptionalData []string `json:"optional_data,omitempty"`
	Formula      string   `json:"formula,omitempty"`
	KeepNote     string   `json:"keep_note,omitempty"`
}

// Update 说明怎么判断有没有新版本。
type Update struct {
	Kind string `json:"kind"` // brew | release-index | none
}

// Requires 是安装前给用户看的"同意清单"。
type Requires struct {
	FullDisk         bool  `json:"full_disk,omitempty"`
	RemovableVolume  bool  `json:"removable_volume,omitempty"`
	LocalNetwork     bool  `json:"local_network,omitempty"`
	Ports            []int `json:"ports,omitempty"`
	SystemDaemon     bool  `json:"system_daemon,omitempty"`
	RootOwnedPaths   bool  `json:"root_owned_paths,omitempty"`
	NeedsPanelUIHost bool  `json:"needs_panel_ui_host,omitempty"`
}

// builtinHooks 是允许引用的**面板内建**补丁名（不在表里的一律拒绝）。
//
// 这是"声明式"与"可编程"的分界线：需要定制逻辑的动作由面板实现一次、所有插件复用，
// 插件本身永远不能带代码。
var builtinHooks = map[string]string{
	"syncthing-gui-lan":  "把 Syncthing 的 GUI 监听改成 0.0.0.0:<port> 并设置随机登录口令",
	"filebrowser-root":   "按 plist 里的 -r 回读并锁定 File Browser 的文件根目录",
	"transmission-rpc":   "写 settings.json：关白名单、开认证、绑 0.0.0.0（停→等端口→写→起→回读）",
	"miniflux-provision": "建库、写 LISTEN_ADDR=:port、跑迁移、建管理员",
}

// Load 读入并校验一份插件声明。**严格模式**：任何未知键都直接失败（安全边界）。
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw, path)
}

// Parse 从字节解析（path 只用于报错定位）。
func Parse(raw []byte, path string) (*Spec, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s：声明解析失败（未知字段/格式错误，插件不允许携带任意命令或未知能力）：%w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s：%w", path, err)
	}
	return &s, nil
}

// Validate 是全部不变量。每条错误都要能照着改。
func (s *Spec) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if s.Schema != SchemaV1 {
		add("schema 必须是 %q（当前只支持这一版）", SchemaV1)
	}
	if !idRe.MatchString(s.ID) {
		add("id 只能是小写字母/数字/连字符（≤32），实际 %q", s.ID)
	}
	if strings.TrimSpace(s.Name) == "" {
		add("name 不能为空")
	}
	if strings.TrimSpace(s.Icon) == "" {
		add("icon 不能为空（市场卡片要用）")
	}

	// ---- source ----
	switch s.Source.Kind {
	case "brew":
		if strings.TrimSpace(s.Source.Formula) == "" {
			add("source.kind=brew 必须给 formula")
		}
	case "release":
		if strings.TrimSpace(s.Source.Repo) == "" || strings.TrimSpace(s.Source.Asset) == "" {
			add("source.kind=release 必须给 repo 与 asset")
		}
		if strings.TrimSpace(s.Source.Binary) == "" {
			add("source.kind=release 必须给 binary（解包后要取哪个可执行文件）")
		}
	case "compose":
		if strings.TrimSpace(s.Source.ComposeYAML) == "" {
			add("source.kind=compose 必须给 compose_yaml")
		}
	default:
		add("source.kind 必须是 brew / release / compose 之一，实际 %q", s.Source.Kind)
	}
	switch s.Source.Checksum {
	case "", "sha256", "md5", "arch-only":
	default:
		add("source.checksum 只能是 sha256 / md5 / arch-only，实际 %q（必须如实声明校验强度）", s.Source.Checksum)
	}
	if s.Source.Kind == "release" && s.Source.Checksum == "" {
		add("source.kind=release 必须声明 checksum（sha256 / md5 / arch-only），不许默认「下完就信」")
	}

	// ---- run ----
	switch s.Run.Mode {
	case "panel-daemon":
		if s.Run.Supervise == nil {
			add("run.mode=panel-daemon 必须给 run.supervise")
		} else {
			if !isAbsOrHome(s.Run.Supervise.Bin) {
				add("run.supervise.bin 必须是绝对路径或用 ~/ 开头，实际 %q", s.Run.Supervise.Bin)
			}
			if sc := strings.TrimSpace(s.Run.Supervise.Subcmd); sc != "" && !idRe.MatchString(sc) {
				add("run.supervise.subcmd 形状不对：%q", sc)
			}
		}
	case "app-daemon":
		if !isAbsOrHome(s.Run.Bin) {
			add("run.mode=app-daemon 必须给绝对路径的 run.bin（launchd 直接跑它），实际 %q", s.Run.Bin)
		}
	case "brew-service":
		if s.Source.Kind != "brew" {
			add("run.mode=brew-service 要求 source.kind=brew")
		}
	case "compose":
		if s.Source.Kind != "compose" {
			add("run.mode=compose 要求 source.kind=compose")
		}
	case "none":
		// 纯网页/无守护进程（如 phpMyAdmin 那种 nginx alias）
	default:
		add("run.mode 必须是 panel-daemon / app-daemon / brew-service / compose / none 之一，实际 %q", s.Run.Mode)
	}
	for _, h := range s.Run.Hooks {
		if _, ok := builtinHooks[h]; !ok {
			add("run.hooks 里的 %q 不是面板内建补丁（插件不许带自己的代码；内建补丁见 docs/插件规范.md）", h)
		}
	}

	// ---- config ----
	if s.Config != nil {
		if !isAbsOrHome(s.Config.Path) {
			add("config.path 必须是绝对路径或用 ~/ 开头，实际 %q", s.Config.Path)
		}
		if m := s.Config.Mode; m != "" && len(m) != 4 {
			add("config.mode 形如 0600，实际 %q", m)
		}
		switch s.Config.Own {
		case "", "user", "root":
		default:
			add("config.own 只能是 user / root，实际 %q", s.Config.Own)
		}
		// 声明式配置补丁（patch.go）：格式/键名形状/值不许带换行/secret 与 set 不许撞键。
		if len(s.Config.Patches) > 0 && (len(s.Config.Set) > 0 || len(s.Config.Secrets) > 0) {
			add("config：写了 config.patches 就不要再写 config.set / config.secrets（两处会打架）")
		}
		// 注意：这里**不**强制"有 secrets 就必须 if_missing=create" —— 装完就有配置文件的
		// 应用（couchdb 的 local.ini）用默认的 skip 本来就对。文件真的不存在时的后果由
		// 运行期兜住：跳过写入 ⇒ 不生成口令、凭据区也不出现（见 services.applyConfigPatchStep）。
		for i, p := range s.Config.PatchList() {
			for _, e := range ValidatePatch(&p) {
				add("config.patches[%d]：%s", i, e)
			}
		}
	}

	// ---- expose ----
	if s.Expose != nil {
		if s.Expose.Port != 0 && (s.Expose.Port < 1 || s.Expose.Port > 65535) {
			add("expose.port 越界：%d", s.Expose.Port)
		}
		if s.Expose.UIPort != 0 && (s.Expose.UIPort < 1 || s.Expose.UIPort > 65535) {
			add("expose.ui_port 越界：%d", s.Expose.UIPort)
		}
		switch s.Expose.Bind {
		case "", "0.0.0.0", "127.0.0.1":
		default:
			add("expose.bind 只能是 0.0.0.0 或 127.0.0.1（具体地址/主机名换网络就静默失效），实际 %q", s.Expose.Bind)
		}
		switch s.Expose.UI {
		case "", "panel-hosted", "app":
		default:
			add("expose.ui 只能是 panel-hosted / app，实际 %q", s.Expose.UI)
		}
	}

	// ---- verify / health ----
	if len(s.Verify.AnyOf) == 0 {
		add("verify.any_of 至少要有一条探针（安装收尾必须真的验一次，端口在听不算）")
	}
	for i, p := range s.Verify.AnyOf {
		if err := p.validate(); err != nil {
			add("verify.any_of[%d]：%v", i, err)
		}
	}
	if err := s.Health.Probe.validate(); err != nil {
		add("health：%v", err)
	}
	if s.Health.Kind == "" {
		add("health 必填：没有可跑的探针就只能显示「未复核」，那等于把假状态留给用户")
	}

	// ---- uninstall ----
	if s.Run.Mode != "none" && len(s.Uninstall.Always) == 0 && len(s.Uninstall.OptionalData) == 0 {
		add("uninstall 至少要声明 always 或 optional_data：装了必须说得清卸的时候删什么")
	}
	for i, a := range s.Uninstall.Always {
		if !isAbsOrHome(a) {
			add("uninstall.always[%d] 必须是绝对路径或用 ~/ 开头，实际 %q", i, a)
		}
	}
	for i, a := range s.Uninstall.OptionalData {
		if !isAbsOrHome(a) {
			add("uninstall.optional_data[%d] 必须是绝对路径或用 ~/ 开头，实际 %q", i, a)
		}
	}

	// ---- update ----
	if s.Update != nil {
		switch s.Update.Kind {
		case "brew", "release-index", "none":
		default:
			add("update.kind 只能是 brew / release-index / none，实际 %q", s.Update.Kind)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "；"))
	}
	return nil
}

func (p Probe) validate() error {
	switch p.Kind {
	case "http":
		if p.Path != "" && !strings.HasPrefix(p.Path, "/") {
			return fmt.Errorf("http 探针的 path 要以 / 开头，实际 %q", p.Path)
		}
	case "rpc":
		if strings.TrimSpace(p.URL) == "" || strings.TrimSpace(p.Method) == "" {
			return errors.New("rpc 探针必须给 url 与 method")
		}
		if !strings.HasPrefix(p.URL, "http://127.0.0.1") && !strings.HasPrefix(p.URL, "http://localhost") {
			return fmt.Errorf("rpc 探针只允许打本机回环（面板自己代发），实际 %q", p.URL)
		}
	case "process":
		if strings.TrimSpace(p.Label) == "" {
			return errors.New("process 探针必须给 label（launchd 标签）")
		}
	case "port":
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("port 探针的端口越界：%d", p.Port)
		}
	case "":
		return errors.New("探针必须给 kind（http / rpc / process / port）")
	default:
		return fmt.Errorf("探针 kind 只能是 http / rpc / process / port，实际 %q", p.Kind)
	}
	return nil
}

// isAbsOrHome 允许绝对路径或 ~/ 开头（插件作者写表时更自然）。
func isAbsOrHome(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "~/") {
		return len(p) > 2
	}
	// {brew}/… 交给运行期按本机 Homebrew 前缀展开（Intel/ARM 前缀不同）。
	if strings.HasPrefix(p, "{brew}/") {
		return len(p) > len("{brew}/")
	}
	return filepath.IsAbs(p)
}

// PlanText 把声明翻译成"装这台机器会做什么"（干跑：只打印，不碰机器）。
//
// 存在的意义：审查者在真机动手前能一眼看完整个流程 —— 尤其要看**服务怎么跑**
// （panel-daemon 继承面板授权 / app-daemon 不继承）与**卸载删什么、留什么**。
func PlanText(s *Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "应用：%s %s（%s）\n", s.Icon, s.Name, s.ID)
	if s.Summary != "" {
		fmt.Fprintf(&b, "简介：%s\n", s.Summary)
	}
	b.WriteString("计划（干跑，不会改这台机器）：\n")

	// ① 来源
	switch s.Source.Kind {
	case "brew":
		fmt.Fprintf(&b, "  1. 来源：Homebrew formula %s\n", s.Source.Formula)
	case "release":
		tag := s.Source.Tag
		if tag == "" {
			tag = "由镜像索引决定"
		}
		fmt.Fprintf(&b, "  1. 来源：release %s @ %s，取 %s（校验强度：%s）\n",
			s.Source.Repo, tag, s.Source.Asset, orDefault(s.Source.Checksum, "未声明"))
	case "compose":
		fmt.Fprintf(&b, "  1. 来源：compose（%s）\n", s.Source.ComposeYAML)
	}
	fmt.Fprintf(&b, "     安装根：~/%s（面板约定）\n", s.ID)

	// ② 配置
	if s.Config != nil {
		seed := s.Config.Seed
		if seed == "" {
			seed = "应用自己创建（面板只登记位置）"
		}
		fmt.Fprintf(&b, "  2. 配置：%s（权限 %s，归属 %s，模板：%s）\n",
			s.Config.Path, orDefault(s.Config.Mode, "0600"), orDefault(s.Config.Own, "user"), seed)
		if len(s.Config.Secrets) > 0 {
			fmt.Fprintf(&b, "     随机密钥（只展示一次）：%s\n", strings.Join(s.Config.Secrets, ", "))
		}
	}

	// ③ 运行方式（这一条是重点）
	switch s.Run.Mode {
	case "panel-daemon":
		sub := s.Run.Supervise.Subcmd
		if sub == "" {
			sub = s.ID + "-supervise"
		}
		fmt.Fprintf(&b, "  3. 服务：**面板托管**（<面板二进制> %s --user <真实用户> --bin %s）\n",
			sub, s.Run.Supervise.Bin)
		b.WriteString("     以真实用户运行、fork 后降权；**继承面板的 TCC 授权**（数据目录受隐私保护时必需）\n")
	case "app-daemon":
		fmt.Fprintf(&b, "  3. 服务：系统级 LaunchDaemon 直接跑 %s（以真实用户）\n", s.Run.Bin)
		b.WriteString("     ⚠️ 不继承面板授权：数据目录若在 ~/Downloads、~/Desktop、~/Documents 或外接卷，会被隐私保护挡住\n")
	case "brew-service":
		fmt.Fprintf(&b, "  3. 服务：brew service（%s）", s.Source.Formula)
		if len(s.Run.Hooks) > 0 {
			fmt.Fprintf(&b, "，并执行内建补丁：%s", strings.Join(s.Run.Hooks, ", "))
		}
		b.WriteString("\n")
	case "compose":
		b.WriteString("  3. 服务：docker compose（容器由面板托管）\n")
	case "none":
		b.WriteString("  3. 服务：无守护进程（纯网页/由 nginx 提供）\n")
	}

	// ④ 配置补丁与暴露
	if patches := s.Config.PatchList(); len(patches) > 0 {
		fmt.Fprintf(&b, "  4. 配置补丁：改 %s\n", s.Config.Path)
		for _, p := range patches {
			fmt.Fprintf(&b, "     · %s（%s）→ %s\n", orDefault(p.Section, "默认段"),
				NormalizeFormat(p.Format), PatchText(&p))
			if len(p.Secrets) > 0 {
				fmt.Fprintf(&b, "       其中 %s 的值由面板随机生成、只展示一次（已有值一律复用，不轮换）\n",
					strings.Join(p.Secrets, ", "))
			}
			if p.IfMissing != "create" {
				b.WriteString("       文件还不存在时不动它（不少应用是首次启动才生成配置）\n")
			}
		}
	}
	if s.Expose != nil {
		port := s.Expose.Port
		if port == 0 {
			port = s.Expose.UIPort
		}
		fmt.Fprintf(&b, "  5. 入口：http://<本机IP>:%d/（绑定 %s）", port, orDefault(s.Expose.Bind, "0.0.0.0"))
		if s.Expose.UI != "" {
			fmt.Fprintf(&b, "，界面形态 %s", s.Expose.UI)
		}
		b.WriteString("\n")
	}

	// ⑤ 验收 + 健康
	var vs []string
	for _, p := range s.Verify.AnyOf {
		vs = append(vs, probeText(p))
	}
	fmt.Fprintf(&b, "  6. 安装收尾验收（真的跑一次，失败即报错）：%s\n", strings.Join(vs, " 或 "))
	fmt.Fprintf(&b, "  7. 健康判据（列表/卡片用，%s）：%s\n", orDefault(s.Health.Interval, "默认间隔"), probeText(s.Health.Probe))
	if s.Health.Kind == "process" || s.Health.Kind == "port" {
		b.WriteString("     ⚠️ process/port 只证明「进程在跑/端口在听」，不证明服务可用 —— 卡片必须如实这么写\n")
	}

	// ⑦ 登记 + 卸载
	fmt.Fprintf(&b, "  8. 登记：服务管理 + 市场卡片；已安装判据与卸载计划由面板统一生成\n")
	fmt.Fprintf(&b, "  9. 卸载：停服务")
	if len(s.Uninstall.Always) > 0 {
		fmt.Fprintf(&b, " → 删 %s", strings.Join(s.Uninstall.Always, ", "))
	}
	if s.Uninstall.Formula != "" {
		fmt.Fprintf(&b, " → brew uninstall %s", s.Uninstall.Formula)
	}
	b.WriteString("\n")
	if len(s.Uninstall.OptionalData) > 0 {
		fmt.Fprintf(&b, "     勾选「删除数据」才会删：%s（默认保留）\n", strings.Join(s.Uninstall.OptionalData, ", "))
	}
	if strings.TrimSpace(s.Uninstall.KeepNote) != "" {
		fmt.Fprintf(&b, "     说明：%s\n", s.Uninstall.KeepNote)
	}
	if s.Update != nil {
		fmt.Fprintf(&b, "  9. 更新检测：%s\n", s.Update.Kind)
	}
	if s.Requires != nil {
		var need []string
		if s.Requires.FullDisk {
			need = append(need, "完全磁盘访问权限")
		}
		if s.Requires.RemovableVolume {
			need = append(need, "可移除宗卷")
		}
		if s.Requires.LocalNetwork {
			need = append(need, "本地网络")
		}
		if s.Requires.SystemDaemon {
			need = append(need, "系统级守护进程")
		}
		if len(s.Requires.Ports) > 0 {
			need = append(need, fmt.Sprintf("端口 %v", s.Requires.Ports))
		}
		if len(need) > 0 {
			fmt.Fprintf(&b, "  ⚠️ 安装前要向用户说明的权限：%s\n", strings.Join(need, "、"))
		}
	}
	return b.String()
}

func probeText(p Probe) string {
	switch p.Kind {
	case "http":
		return fmt.Sprintf("HTTP %s%s", orDefault(p.Path, "/"), expectText(p.Expect))
	case "rpc":
		return fmt.Sprintf("JSON-RPC %s 调 %s%s", p.URL, p.Method, expectText(p.ExpectKey))
	case "process":
		return fmt.Sprintf("进程 %s 在跑", p.Label)
	case "port":
		return fmt.Sprintf("端口 %d 在听", p.Port)
	}
	return p.Kind
}

func expectText(v string) string {
	if v == "" {
		return ""
	}
	return "（期望含 " + v + "）"
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}
