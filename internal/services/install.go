package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
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

	var cmd *exec.Cmd
	if os.Geteuid() == 0 && m.opt.UserName != "" {
		full := append([]string{"-n", "-u", m.opt.UserName, m.opt.BrewBin}, args...)
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", full...)
	} else {
		cmd = exec.CommandContext(ctx, m.opt.BrewBin, args...)
	}
	// brew 需要正确的 HOME 才能找到 Cellar 与缓存
	if m.opt.UserHome != "" {
		cmd.Env = append(os.Environ(), "HOME="+m.opt.UserHome)
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
	return nil
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
	for _, a := range Catalog() {
		if a.AdoptLabel != label {
			continue
		}
		port = a.Port
		if a.HealthPath != "" && a.Port > 0 {
			healthURL = fmt.Sprintf("http://127.0.0.1:%d%s", a.Port, a.HealthPath)
		}
		category = a.Category
		if a.Name != "" {
			description = a.Summary
		}
		break
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
// 幂等：已登记过（同 label）就直接返回，重复部署不会产生重复条目。
func (m *Manager) RegisterInstalledService(ctx context.Context, label, displayName, icon, category string, port int) error {
	if label == "" {
		return fmt.Errorf("缺少服务 label")
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
