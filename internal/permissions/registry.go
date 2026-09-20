// registry.go —— 「外部应用条件授权入口」注册表。
//
// 每个独立软件都是**平权的一条**：面板只做转发 —— 探测不碰受保护路径（坑 191），
// 用户点「申请」后才以该应用的真实用户身份调用**它自己的 CLI**，让弹窗由它的签名身份触发。
// 加一个外部应用 = 加一条，不改判定逻辑。
//
// zizvideo 已回仓成为面板模块、由面板托管并与面板共用文件权限，故其条目 Enabled=false：
// 机制与代码全留，只是本机不再显示这个会误导用户的授权入口。
package permissions

import "strings"

// 权限项 id（GET 下发的机器可读值）。
const (
	ItemFullDisk  = "full_disk"
	ItemRemovable = "removable"
	ItemZizvideo  = "zizvideo"
)

// zizvideo 的冻结契约（注册表条目的取值来源）。
const (
	ZizvideoInstallPath = "/opt/zizvideo/bin/zizvideo"
	ZizvideoLabel       = "cn.zizvideo.serve"
	ZizvideoPort        = 7766
	ZizvideoSigningID   = "cn.zizvideo.serve"
	ZizvideoCheckVerb   = "check-access"
	zizvideoDataSubdir  = "Library/Application Support/zizvideo"
)

// ExternalApp 是注册表里的一条外部应用。
//
// RootsArgs 为 nil 表示该应用不提供"列出允许根"；此时申请必须由用户填目标路径（面板不猜目录）。
type ExternalApp struct {
	ID           string // 权限项 id
	Name         string // 短名（日志/任务标题用）
	Title        string
	Why          string
	Enabled      bool   // false ⇒ 本机不显示入口（机制与代码全留）
	DisabledNote string // 关闭原因（申请被拒时如实告知，不谎报"未知项"）
	// InstallPath 是冻结安装路径：拿不到实际可执行文件时用于手动授权指引。
	InstallPath string
	Label       string // launchd 标签
	Port        int
	SigningID   string
	CheckVerb   string // 该应用自己的自检动词
	DataSubdir  string // 相对家目录的数据目录（config.json 在其中）
	HealthPath  string // 回环健康端点路径（不含前导 /）
	// RootsArgs 返回"列出允许根"的参数（nil ⇒ 不支持列出允许根）。
	RootsArgs func(configPath string) []string
}

// Registry 是全部外部应用条目（含已关闭的）。
func Registry() []ExternalApp { return []ExternalApp{ZizvideoApp()} }

// EnabledRegistry 只返回本机要显示入口的条目（GET 列表用它）。
func EnabledRegistry() []ExternalApp {
	all := Registry()
	out := make([]ExternalApp, 0, len(all))
	for _, a := range all {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

// LookupExternalApp 按 id 查条目（**不过滤 Enabled**：apply 要能分辨"已关闭"与"未知项"）。
func LookupExternalApp(id string) (ExternalApp, bool) {
	id = strings.TrimSpace(id)
	for _, a := range Registry() {
		if a.ID == id {
			return a, true
		}
	}
	return ExternalApp{}, false
}

// ZizvideoApp 是 zizvideo 条目：回仓为面板模块后由面板托管、与面板共用权限，故默认关闭。
func ZizvideoApp() ExternalApp {
	return ExternalApp{
		ID: ItemZizvideo, Name: "zizvideo", Title: "zizvideo 媒体目录",
		Why:          "让 zizvideo 自己读取媒体目录",
		Enabled:      false,
		DisabledNote: "已改为面板托管、与面板共用权限，无需单独授权",
		InstallPath:  ZizvideoInstallPath,
		Label:        ZizvideoLabel,
		Port:         ZizvideoPort,
		SigningID:    ZizvideoSigningID,
		CheckVerb:    ZizvideoCheckVerb,
		DataSubdir:   zizvideoDataSubdir,
		HealthPath:   "healthz",
		RootsArgs:    func(cfg string) []string { return []string{"roots", "list", "--config", cfg} },
	}
}
