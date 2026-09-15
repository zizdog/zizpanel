package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  卸载"面板自己装的应用"
//
//  背景（用户反馈）：应用市场里装上应用之后**没有任何卸载入口** ——
//  而面板的通用卸载（Manager.Uninstall）只认"托管服务"，面板自研安装器装的
//  那几个（IOPaint / Qwen3 TTS / 音色接收端 / phpMyAdmin）注册时是**纳管**状态，
//  那个入口会直接拒绝（"这是纳管服务，面板不会卸载它"）。
//  于是用户装了 IOPaint，在面板里找不到任何办法把它卸掉。
//
//  这里的语义分三类，必须分清楚（否则会删掉用户自己的东西）：
//
//   1. **managed 服务**（compose 应用、面板注册的 brew 服务）
//      → 走既有的 Manager.Uninstall。
//   2. **面板自研安装器装的应用**（PanelInstaller 非空）
//      → 走本文件的 UninstallApp：卸服务 + 删 plist + 删面板记录，
//        并按用户选择删除安装产物（虚拟环境、模型、样本、任务）。
//   3. **纳管的第三方服务**（nginx / php / mysql / 用户自己注册的）
//      → **绝不卸载**。只提供「取消纳管」（把记录从面板移除，不动系统）。
//        这条是铁律：删掉用户自己装的 MySQL 等于删掉他的网站数据。
// ============================================================================

// UninstallPlan 是"卸载这个应用会做什么"的说明。
//
// 为什么要给界面：卸载是不可逆的。确认框里必须写清楚**具体会删哪些路径**、
// 以及**哪些东西会保留**（例如模型缓存、近期的合成任务），
// 而不是一句"确定卸载吗"。这也是这个项目一贯的要求：用户要知道自己按的是什么。
type UninstallPlan struct {
	// Kind: service（托管服务）/ installer（面板安装器）/ forget（只能取消纳管）
	Kind string `json:"kind"`
	// Service 是面板服务记录名（Kind=service/forget 时有意义）
	Service string `json:"service,omitempty"`
	// Steps 是卸载会做的事（人类可读，逐条列出）
	Steps []string `json:"steps"`
	// DataPaths 是"可选删除"的产物路径（remove_data=1 时才删）
	DataPaths []string `json:"data_paths,omitempty"`
	// KeepNote 说明默认会保留什么
	KeepNote string `json:"keep_note,omitempty"`
	// Blocked 非空表示现在不能卸载（例如 Docker 运行时还托着别的应用）
	Blocked string `json:"blocked,omitempty"`
}

// PlanUninstall 给出某个目录应用当前该怎么卸载（只读，不产生任何改动）。
func (m *Manager) PlanUninstall(ctx context.Context, appID string) UninstallPlan {
	app, ok := FindApp(appID)
	if !ok {
		return UninstallPlan{Kind: "none", Blocked: "目录里没有这个应用"}
	}
	return m.PlanUninstallFor(ctx, app, m.findServiceRecord(ctx, app))
}

// PlanUninstallFor 与 PlanUninstall 相同，但用调用方已经查好的记录。
//
// 为什么要这个重载：应用市场一次要给出十几个应用的计划，每个都去查一遍
// 数据库很浪费（而且列表里本来就有记录）。列表页用这个，单个应用用上面那个。
func (m *Manager) PlanUninstallFor(ctx context.Context, app App, rec *Service) UninstallPlan {
	if rec != nil && rec.Managed && app.PanelInstaller == "" {
		keep := "Homebrew 包**不会被卸载**（面板只删服务定义），需要的话请自行 brew uninstall"
		steps := []string{
			"停止并移除「" + rec.DisplayName + "」的服务定义",
			"从「服务管理」中删除这条记录",
		}
		if app.Kind == KindCompose || app.Kind == KindDocker {
			keep = "compose 应用只删容器与网络，**具名卷（数据）保留**"
			steps = []string{
				"docker compose down（删除容器与网络）",
				"从「服务管理」中删除这条记录",
			}
		}
		return UninstallPlan{Kind: "service", Service: rec.Name, Steps: steps, KeepNote: keep}
	}
	if app.PanelInstaller != "" {
		p := m.installerPlan(ctx, app)
		if rec != nil {
			p.Service = rec.Name
		}
		return p
	}
	if rec != nil {
		// 纳管的第三方服务：只允许取消纳管
		return UninstallPlan{
			Kind:    "forget",
			Service: rec.Name,
			Steps:   []string{"把「" + rec.DisplayName + "」从面板记录里移除"},
			KeepNote: "面板**不会**卸载你自己安装的软件（不跑 brew uninstall、不删文件）。" +
				"要真正删除，请在终端里自行处理。",
		}
	}
	return UninstallPlan{Kind: "none", Blocked: "没有找到可卸载的对象（可能是 brew 装的核心组件）"}
}

// UninstallApp 执行卸载。Kind=service/forget 由 web 层走各自的既有接口，
// 这里只处理"面板安装器"这一类。
func (m *Manager) UninstallApp(ctx context.Context, appID string, removeData bool, result *InstallResult) error {
	app, ok := FindApp(appID)
	if !ok {
		return fmt.Errorf("目录里没有这个应用: %s", appID)
	}
	switch app.PanelInstaller {
	case "qwen3tts":
		return m.uninstallQwen(ctx, removeData, result)
	case "voicereceiver":
		return m.uninstallReceiver(ctx, removeData, result)
	case "iopaint":
		return m.uninstallIOPaint(ctx, removeData, result)
	case "phpmyadmin":
		return m.UninstallPhpMyAdmin(ctx, removeData, result)
	case "docker-runtime":
		return m.uninstallDockerRuntime(ctx, removeData, result)
	}
	// "官方 release 原生二进制"类应用（Lucky / Orbien）：同一套安装器，
	// 用注册表查而不是在这里再抄一遍 switch，免得加了新应用忘记补卸载。
	if _, ok := releaseBinaryApps[app.PanelInstaller]; ok {
		return m.UninstallReleaseBinary(ctx, app.PanelInstaller, removeData, result)
	}
	return fmt.Errorf("「%s」没有对应的卸载实现（PanelInstaller=%q）", app.Name, app.PanelInstaller)
}

// installerPlan 按安装器给出卸载计划。
func (m *Manager) installerPlan(ctx context.Context, app App) UninstallPlan {
	// "官方 release 原生二进制"类应用（Lucky / Orbien 服务端与客户端 / frps / frpc）：
	// 一次注册表判断覆盖全部 —— 加了新条目不用回来补 case（漏补的后果是
	// 市场里的卸载按钮报"没有卸载实现"，而东西确实是面板装的）。
	if plan, ok := m.releaseBinaryPlan(app.PanelInstaller); ok {
		return plan
	}
	p := UninstallPlan{Kind: "installer"}
	switch app.PanelInstaller {
	case "qwen3tts":
		p.Steps = []string{"停止并删除 launchd 服务 " + qwenLabel, "从「服务管理」移除记录"}
		// 只列**我们清单里的模型目录**，不要写整个 HF hub ——
		// 那个目录里还有别的项目的模型，确认框上写它会让人以为要全删。
		// 实现（uninstallQwen）也只删这几个目录。
		p.DataPaths = []string{filepath.Join(m.opt.UserHome, "tts", "qwen3")}
		hub := filepath.Join(m.opt.UserHome, ".cache", "huggingface", "hub")
		for _, mdl := range QwenModels {
			p.DataPaths = append(p.DataPaths,
				filepath.Join(hub, "models--"+strings.ReplaceAll(mdl.Name, "/", "--")))
		}
		p.KeepNote = "默认保留虚拟环境与模型权重（重新部署时不用重新下载约 3GB）"
	case "voicereceiver":
		p.Steps = []string{"停止并删除 launchd 服务 " + receiverLabel, "从「服务管理」移除记录"}
		p.DataPaths = []string{
			filepath.Join(m.opt.UserHome, "tts", "voice-receiver"),
			filepath.Join(m.opt.UserHome, "tts", "voice-samples"),
			filepath.Join(m.opt.UserHome, "tts", "jobs"),
		}
		p.KeepNote = "默认保留音色样本与历史合成任务（它们是你的数据）"
	case "iopaint":
		p.Steps = []string{"停止并删除 launchd 服务 " + iopaintLabel, "从「服务管理」移除记录"}
		p.DataPaths = []string{filepath.Join(m.opt.UserHome, "iopaint")}
		p.KeepNote = "默认保留 ~/iopaint（虚拟环境，重新部署时不用重装依赖）"
	case "phpmyadmin":
		p.Steps = []string{
			"移除 nginx 默认站点里的 phpMyAdmin 入口并重载",
			"brew uninstall phpmyadmin",
		}
		p.DataPaths = []string{filepath.Join(m.brewPrefix(), "etc", "phpmyadmin.config.inc.php")}
		p.KeepNote = "面板自研的库表管理功能不受影响"
	case "docker-runtime":
		p.Steps = []string{
			"停止并删除 Colima 虚拟机（**其中的容器、镜像、卷都会消失**）",
			"从「服务管理」移除记录",
		}
		p.KeepNote = "Homebrew 包保留；要彻底移除请自行 brew uninstall colima docker"
		if users := m.composeUsers(ctx); len(users) > 0 {
			p.Blocked = "还有 " + fmt.Sprint(len(users)) + " 个 Docker 应用在用这个运行时（" +
				strings.Join(users, "、") + "），请先卸载它们"
		}
	default:
		p.Blocked = "这个应用没有卸载实现"
	}
	return p
}

// findServiceRecord 按应用的候选标签 / 名称找出面板记录。
func (m *Manager) findServiceRecord(ctx context.Context, app App) *Service {
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil
	}
	cand := map[string]bool{}
	for _, v := range []string{app.ServiceLabel, app.AdoptLabel, app.ID, app.Name} {
		if v != "" {
			cand[v] = true
		}
	}
	for _, s := range list {
		if cand[s.Name] || (s.LaunchLabel != "" && cand[s.LaunchLabel]) {
			return s
		}
	}
	return nil
}

// composeUsers 返回当前登记在面板里的 Docker/compose 应用名。
func (m *Manager) composeUsers(ctx context.Context) []string {
	list, err := m.repo.List(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range list {
		if s.Kind == KindCompose || s.Kind == KindDocker {
			out = append(out, s.DisplayName)
		}
	}
	return out
}

// ---------- 各安装器的卸载实现 ----------

func (m *Manager) uninstallQwen(ctx context.Context, removeData bool, result *InstallResult) error {
	if result != nil {
		result.step(ctx, "停止 Qwen3 TTS 服务")
	}
	p := m.qwenPaths()
	if err := m.removeService(ctx, qwenLabel, p.Plist); err != nil {
		return err
	}
	if removeData {
		if err := m.removeTree(ctx, p.Root, result); err != nil {
			return err
		}
		// 模型权重在 HF 缓存里，按目录名逐个删（只删我们清单里的那些）
		hub := filepath.Join(m.opt.UserHome, ".cache", "huggingface", "hub")
		for _, mdl := range QwenModels {
			dir := filepath.Join(hub, "models--"+strings.ReplaceAll(mdl.Name, "/", "--"))
			if err := m.removeTree(ctx, dir, result); err != nil {
				return err
			}
		}
	} else if result != nil {
		result.step(ctx, "保留 "+p.Root+"（虚拟环境与模型；需要彻底清理请勾选删除数据）")
	}
	return nil
}

func (m *Manager) uninstallReceiver(ctx context.Context, removeData bool, result *InstallResult) error {
	if result != nil {
		result.step(ctx, "停止音色接收端")
	}
	p := m.receiverPaths()
	if err := m.removeService(ctx, receiverLabel, p.Plist); err != nil {
		return err
	}
	if removeData {
		for _, dir := range []string{p.Dir, p.Samples, p.Jobs} {
			if err := m.removeTree(ctx, dir, result); err != nil {
				return err
			}
		}
	} else if result != nil {
		result.step(ctx, "保留音色样本与历史合成任务（"+p.Samples+"、"+p.Jobs+"）")
	}
	return nil
}

func (m *Manager) uninstallIOPaint(ctx context.Context, removeData bool, result *InstallResult) error {
	if result != nil {
		result.step(ctx, "停止 IOPaint 服务")
	}
	p := m.iopaintPaths()
	if err := m.removeService(ctx, iopaintLabel, p.Plist); err != nil {
		return err
	}
	if removeData {
		if err := m.removeTree(ctx, p.Root, result); err != nil {
			return err
		}
	} else if result != nil {
		result.step(ctx, "保留 "+p.Root+"（虚拟环境；需要彻底清理请勾选删除数据）")
	}
	return nil
}

func (m *Manager) uninstallDockerRuntime(ctx context.Context, removeData bool, result *InstallResult) error {
	if users := m.composeUsers(ctx); len(users) > 0 {
		return fmt.Errorf("还有 %d 个 Docker 应用在用这个运行时（%s），请先卸载它们",
			len(users), strings.Join(users, "、"))
	}
	if result != nil {
		result.step(ctx, "删除 Colima 虚拟机（容器/镜像/卷会一起消失）")
	}
	if out, err := m.runAsUser(ctx, 3*time.Minute, m.colimaBin(), "delete", "-f"); err != nil {
		return fmt.Errorf("colima delete 失败: %v（%s）", err, tailText(out, 300))
	}
	return m.removeService(ctx, ColimaLaunchLabel, ColimaPlistPath)
}

// ---------- 通用小工具 ----------

// removeService 停止并删除 launchd 服务，再从面板记录里移除。
//
// 顺序：先 unload/删 plist，再删记录 —— 反了的话进程还在跑、面板却已经没有它了。
func (m *Manager) removeService(ctx context.Context, label, plist string) error {
	if err := m.stopLaunchdService(ctx, label, plist); err != nil {
		return err
	}
	if list, err := m.repo.List(ctx); err == nil {
		for _, s := range list {
			if s.LaunchLabel == label || s.Name == label {
				if err := m.repo.Delete(ctx, s.Name); err != nil {
					return fmt.Errorf("删除面板记录失败: %w", err)
				}
			}
		}
	}
	return nil
}

// stopLaunchdService 停服务并删 plist（幂等：本来就不存在也算成功）。
func (m *Manager) stopLaunchdService(ctx context.Context, label, plist string) error {
	// 先走正常停止（服务可能正在跑）；失败也继续 —— 目标状态是"没有它"，
	// 后面删 plist 才是决定性的那一步。
	_, _ = m.runRoot(ctx, 30*time.Second, "/bin/launchctl", "bootout", "system/"+label)
	if plist != "" {
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败: %w", plist, err)
		}
	}
	return nil
}

func (m *Manager) removeTree(ctx context.Context, path string, result *InstallResult) error {
	if path == "" || path == "/" || path == m.opt.UserHome {
		return fmt.Errorf("拒绝删除危险路径: %q", path)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	if result != nil {
		result.step(ctx, "删除 "+path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}
