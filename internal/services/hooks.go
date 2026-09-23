package services

// hooks.go —— 插件表里 `run.hooks` 的**实现登记簿**。
//
// 语义（说清楚，别让它变成摆设）：`run.hooks` 是**依赖声明** —— 插件说"这个应用要靠
// 面板的某个内建补丁才能好用"（例如 Syncthing 要改 GUI 监听并设口令）。补丁本身由面板实现，
// 插件 JSON 永远不带代码（安全边界）。
//
// 这张表的作用是**把"名字"和"真实实现"对起来**，并由门禁交叉核对（见 hooks_test.go）：
//   · 白名单里出现一个没有实现的名字 ⇒ 门禁失败（否则插件作者会以为写了就生效）；
//   · 某个实现被删掉/改名 ⇒ 门禁失败（名字就成了谎话）。
//
// 为什么不做"通用 brew 轨按名字自动执行"：这四个补丁各自要读改应用自己的配置格式
// （XML / settings.json / 建库跑迁移），把它们抽成通用动词等于给插件开一个"任意改配置"
// 的口子 —— 那正是 B0 明令禁止的方向。等真要做时，正确的是把补丁做成**参数化的内建动词**，
// 而不是让表里的字符串直接驱动执行。

// hookImpl 描述一个内建补丁的实现位置（给文档/门禁/排障用）。
type hookImpl struct {
	// AppID 是这个补丁服务的应用（必须存在于应用目录里）。
	AppID string
	// Where 是实现的落点（文件 + 大致的步骤），排障时按它找。
	Where string
}

// builtinHookImpls 必须覆盖 plugins.BuiltinHookNames() 的全部名字（门禁盯着）。
var builtinHookImpls = map[string]hookImpl{
	"syncthing-gui-lan": {
		AppID: "syncthing",
		Where: "internal/services/syncthing.go：InstallSyncthing 第 5 步（等首次配置生成 → " +
			"patchSyncthingGUI 改 XML → 原子写回 → 重启复核）",
	},
	"filebrowser-root": {
		AppID: "filebrowser",
		Where: "internal/services/filebrowser*.go：安装时按 plist 的 -r 回读并锁定文件根目录",
	},
	"transmission-rpc": {
		AppID: "transmission",
		Where: "internal/services/transmission.go：InstallTransmission（停 → 等端口 → 写 settings.json → 起 → 回读）",
	},
	"miniflux-provision": {
		AppID: "miniflux",
		Where: "internal/services/miniflux.go：InstallMiniflux（建库 → 写 LISTEN_ADDR=:port → 跑迁移 → 建管理员）",
	},
}

// HookImplementation 返回某个内建补丁的实现登记（不存在返回 false）。
func HookImplementation(name string) (hookImpl, bool) {
	impl, ok := builtinHookImpls[name]
	return impl, ok
}
