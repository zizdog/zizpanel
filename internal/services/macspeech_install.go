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
//  「macOS 语音合成（say）」的安装 / 卸载
//
//  这个条目与目录里别的应用有一个本质区别：**它没有任何东西要下载**。
//  引擎 /usr/bin/say 与 /usr/bin/afconvert 都是 macOS 自带的，安装动作只有
//  两件事：
//    ① 如实复核"这台机器现在真的能合成"（say 在、`say -v '?'` 答、音色数 > 0）；
//    ② 把面板托管的网页界面注册成系统级 launchd（com.zizdog.macosspeech），
//       它跑的是面板自己的二进制 `zizpanel speech-serve`。
//
//  为什么服务就是面板自己的二进制（照抄 imgcompress 的取舍）：
//    · 不引入第二个可执行文件、不需要构建步骤、内嵌前端随二进制走；
//    · 面板升级时界面一起升级，不会出现"面板换了、应用还是旧的"。
//
//  为什么不做成 panel-installer 之外的"通用 brew 流程"：没有 formula 可装
//  （say 是系统的一部分），通用流程会去 brew install 一个不存在的东西。
// ============================================================================

// ---------------------------------------------------------------------------
//  仅测试用的注入点
//
//  安装/卸载会写 /Library/LaunchDaemons、调 launchctl bootstrap，单测既不可能
//  真等、也绝不允许碰真实 launchd（AGENTS 第三节）。做成包级变量，
//  生产路径永远是默认实现，测试替换后恢复 —— 与 imgcompress.go 同一套做法。
// ---------------------------------------------------------------------------

var (
	// macSpeechExecutable 返回面板二进制自身的路径（launchd 要重新执行它）。
	macSpeechExecutable = os.Executable
	// macSpeechPlistPath 返回服务 plist 的绝对路径。
	macSpeechPlistPath = func() string { return SystemDaemonPlistPath(MacSpeechLabel) }
	// macSpeechLaunch 装载 launchd 服务（默认 bootout + bootstrap + 等待）。
	macSpeechLaunch = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.bootstrapService(ctx, label, plist)
	}
	// macSpeechStop 停止服务、删 plist、删面板记录。
	macSpeechStop = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.removeService(ctx, label, plist)
	}
	// macSpeechHealthy 打一次 /healthz 并等待 ok:true。
	macSpeechHealthy = func(ctx context.Context, url string, timeout time.Duration) bool {
		return readyWaitJSONBool(ctx, url, "ok", true, timeout)
	}
	// macSpeechListenOverride 仅供测试/本地试运行：覆盖监听地址。
	macSpeechListenOverride = func() string { return "" }
	// macSpeechEngineOverride 仅供测试：替换引擎（不碰真实 say / afconvert）。
	macSpeechEngineOverride = func() *SpeechEngine { return nil }
)

// macSpeechPaths 是这套服务的目录约定。
type macSpeechPaths struct {
	Plist  string
	OutLog string
	ErrLog string
}

func (m *Manager) macSpeechPaths() macSpeechPaths {
	home := m.opt.UserHome
	if home == "" && m.opt.UserName != "" {
		home = "/Users/" + m.opt.UserName
	}
	logDir := filepath.Join(home, "Library", "Logs")
	return macSpeechPaths{
		Plist:  macSpeechPlistPath(),
		OutLog: filepath.Join(logDir, "zizpanel-macspeech.out.log"),
		ErrLog: filepath.Join(logDir, "zizpanel-macspeech.err.log"),
	}
}

// SpeechEngine 返回面板当前用的语音合成引擎（测试可注入）。
func (m *Manager) SpeechEngine() *SpeechEngine {
	if e := macSpeechEngineOverride(); e != nil {
		return e
	}
	return NewSpeechEngine()
}

// macSpeechListen 返回服务应当监听的地址。
//
// 只绑回环：合成要读用户提交的文本、写临时音频，对局域网没有意义，
// 也不该在用户没明确要的情况下扩大暴露面。要给别人用就走面板的 /speech/
// 别名（那条路要求先登录面板）。
func macSpeechListen(port int) string {
	if override := strings.TrimSpace(macSpeechListenOverride()); override != "" {
		return override
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// MacSpeechHealthURL 是网页界面的健康检查地址（面板的服务健康检查也打它）。
func MacSpeechHealthURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", MacSpeechPort)
}

// macSpeechPlist 生成 LaunchDaemon 定义。
//
// 以**真实用户**身份运行（不是 root）：界面要写临时音频、要以该用户的身份跑
// `say` —— 而"能看到哪些音色"与登录用户有关（增强/高级音色是用户下载的），
// 以 root 跑只会看到系统的精简集合。
func macSpeechPlist(panelBin string, port int, user, outLog, errLog string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <!-- 以真实用户运行：临时音频的属主归该用户，音色清单也按该用户解析 -->
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>speech-serve</string>
        <string>--listen</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, MacSpeechLabel, user, panelBin, macSpeechListen(port), outLog, errLog)
}

// MacSpeechPrerequisites 复核"这台机器现在真的能合成语音"。
//
// 这是安装的第一个动作，也是**唯一的硬前置**：say 不在就如实失败，
// 绝不在一个合成不了语音的机器上把服务和界面装起来（那就是谎报成功）。
// 返回音色清单供调用方写进安装步骤。
func (m *Manager) MacSpeechPrerequisites(ctx context.Context, result *InstallResult, eng *SpeechEngine) ([]SpeechVoice, error) {
	if ok, reason := eng.Available(); !ok {
		return nil, fmt.Errorf("macOS 语音合成引擎不可用：%s。"+
			"这个应用依赖 macOS 自带的 /usr/bin/say，无法用 Homebrew / Docker 补上；"+
			"请确认这是 macOS 且系统未被改动过", reason)
	}
	voices, err := eng.Voices(ctx)
	if err != nil {
		return nil, fmt.Errorf("`say -v '?'` 取不到音色清单：%w。"+
			"服务与界面**未安装**（装了也合成不出声音）", err)
	}
	chinese := 0
	for _, v := range voices {
		if v.Chinese {
			chinese++
		}
	}
	if result != nil {
		result.step(ctx, fmt.Sprintf("引擎已复核：%s 可执行，`say -v '?'` 报出 %d 个音色（其中中文 %d 个）",
			eng.SayBin, len(voices), chinese))
		if chinese == 0 {
			result.step(ctx, "警告：这台机器上没有中文音色（可在「系统设置 → 辅助功能 → 朗读内容 → 系统声音」里添加）；"+
				"中文文本会用系统默认音色朗读，效果可能不对")
		}
	}
	return voices, nil
}

// InstallMacSpeech 安装「macOS 语音合成」：复核引擎 + 注册并启动网页界面。
//
// 没有任何下载：不需要 Homebrew、不需要 Python/Node、不需要模型权重，
// 因此这个安装是**秒级**的（这也是它与 Qwen3 TTS 的核心区别）。
func (m *Manager) InstallMacSpeech(ctx context.Context, app App, result *InstallResult) error {
	if result != nil {
		result.App = app.ID
	}
	eng := m.SpeechEngine()
	if _, err := m.MacSpeechPrerequisites(ctx, result, eng); err != nil {
		return err
	}
	// 转换链路如实报告：aiff 由 say 直接产出；wav/m4a 要 afconvert（系统自带）；
	// mp3 只有装了 ffmpeg 才有 —— 缺了就说缺，而不是装完让用户点 mp3 才失败。
	for _, st := range eng.FormatsAvailability() {
		if st.Available {
			continue
		}
		if result != nil {
			result.step(ctx, "提示："+string(st.Format)+" 格式当前不可用 —— "+st.Reason)
		}
	}
	if result != nil {
		if eng.FfmpegPath() != "" {
			result.step(ctx, "mp3 可用（ffmpeg："+eng.FfmpegPath()+"）")
		} else {
			result.step(ctx, "mp3 不可用（这台机器没有 ffmpeg）：其它格式不受影响，"+
				"需要 mp3 就到「应用市场 → FFmpeg」安装")
		}
	}
	if err := m.installMacSpeechService(ctx, app, result); err != nil {
		return err
	}
	port := app.WebPort()
	if port <= 0 {
		port = MacSpeechPort
	}
	if result != nil {
		result.Steps = append(result.Steps,
			"网页界面：直连 http://127.0.0.1:"+fmt.Sprint(port)+
				"/ ，或从「应用市场 → macOS 语音合成」点「打开」走面板别名 /"+MacSpeechSlug+"/（要求先登录面板）",
			"OpenAI 兼容接口：POST http://127.0.0.1:"+fmt.Sprint(port)+"/v1/audio/speech",
			`  例：curl -sS -X POST http://127.0.0.1:`+fmt.Sprint(port)+`/v1/audio/speech \`,
			`        -H 'Content-Type: application/json' \`,
			`        -d '{"input":"你好，世界","voice":"Tingting","format":"m4a"}' -o /tmp/hello.m4a`,
			"音色清单：GET /v1/voices（来自 `say -v '?'`，中文音色带 chinese=true）",
		)
	}
	return nil
}

// installMacSpeechService 写系统级 plist、装载、登记、等健康。
func (m *Manager) installMacSpeechService(ctx context.Context, app App, result *InstallResult) error {
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行语音合成网页界面的真实用户（UserName 为空）")
	}
	panelBin, err := macSpeechExecutable()
	if err != nil {
		return fmt.Errorf("找不到面板自身的可执行文件路径（launchd 要用它启动界面）: %w", err)
	}
	if strings.TrimSpace(panelBin) == "" {
		return fmt.Errorf("面板自身的可执行文件路径为空，无法注册语音合成网页界面")
	}
	p := m.macSpeechPaths()
	if strings.TrimSpace(p.Plist) == "" {
		return fmt.Errorf("语音合成网页界面的 plist 路径为空")
	}
	port := app.WebPort()
	if port <= 0 {
		port = MacSpeechPort
	}
	// 日志目录必须存在（launchd 会直接打开 StandardOutPath，目录不在会让作业起不来）。
	if err := os.MkdirAll(filepath.Dir(p.OutLog), 0o755); err != nil {
		return fmt.Errorf("创建日志目录 %s 失败: %w", filepath.Dir(p.OutLog), err)
	}
	plist := macSpeechPlist(panelBin, port, m.opt.UserName, p.OutLog, p.ErrLog)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist %s 失败（面板需要以 root 运行）: %w", p.Plist, err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist %s 失败: %w", p.Plist, err)
	}
	if result != nil {
		result.step(ctx, "正在注册并启动网页界面服务 "+MacSpeechLabel+"（"+macSpeechListen(port)+"）")
	}
	if err := macSpeechLaunch(m, ctx, MacSpeechLabel, p.Plist); err != nil {
		return fmt.Errorf("启动语音合成网页界面失败: %w", err)
	}
	if err := m.RegisterInstalledService(ctx, MacSpeechLabel, app.Name, app.Icon, app.Category, port); err != nil {
		// 登记失败不该把"界面已经起来"报成安装失败，但必须如实留下警告。
		if result != nil {
			result.step(ctx, "警告：界面已启动，但登记进「服务管理」失败："+err.Error()+
				"（可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
		}
	}
	if result != nil {
		result.Steps = append(result.Steps,
			"已注册为系统级后台服务（开机自启、不依赖用户登录）")
	}
	return m.waitMacSpeechReady(ctx, p, result)
}

// waitMacSpeechReady 等到 /healthz 真的报 ok:true（且音色数 > 0）。
//
// 为什么不是"端口在听就算成功"（AGENTS 第三节）：服务进程可以起来而 say/音色
// 不可用，这时界面能打开但每次合成都失败 —— 端口探测会把它报成健康。
func (m *Manager) waitMacSpeechReady(ctx context.Context, p macSpeechPaths, result *InstallResult) error {
	const timeout = 60 * time.Second
	url := MacSpeechHealthURL()
	wait := macSpeechHealthy
	return assertReady(ctx, readySpec{
		What:    "语音合成网页界面",
		Expect:  url + " 在 " + timeout.String() + " 内返回 {\"ok\":true}（引擎可用且音色数 > 0）",
		Timeout: timeout,
		Probe: func(ctx context.Context) readyVerdict {
			if wait(ctx, url, timeout) {
				return readyVerdict{OK: true, Actual: "已就绪，健康检查确认引擎可用"}
			}
			return readyVerdict{Actual: url + " 没有返回 ok:true（服务进程没起来，或 say/音色不可用）"}
		},
		LogPath: p.ErrLog,
		State:   "引擎 /usr/bin/say 已复核可执行，服务已登记进「服务管理」",
		Missing: "但网页界面不可用，市场里的「打开」与直连端口都会打不开",
		Remedy: "在「服务管理 → macOS 语音合成」里点「重启服务」再试；" +
			"仍然失败请看下面的日志尾部",
		Result: result,
	})
}

// UninstallMacSpeech 卸载：停掉面板托管的网页界面服务，并如实说明"没动系统"。
//
// 与别的应用不同，这里**没有** brew 包要卸、没有模型/虚拟环境要删 ——
// /usr/bin/say 是 macOS 的一部分，面板不会也不能删它。
func (m *Manager) UninstallMacSpeech(ctx context.Context, app App, result *InstallResult) error {
	return m.removeMacSpeechService(ctx, result)
}

// removeMacSpeechService 停止并删除网页界面服务（幂等）。
func (m *Manager) removeMacSpeechService(ctx context.Context, result *InstallResult) error {
	p := m.macSpeechPaths()
	hasPlist := fileExists(p.Plist)
	hasRecord := false
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.LaunchLabel == MacSpeechLabel {
					hasRecord = true
					break
				}
			}
		}
	}
	if !hasPlist && !hasRecord {
		if result != nil {
			result.step(ctx, "语音合成网页界面服务本来就没有注册，跳过停止")
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "停止并删除 launchd 服务 "+MacSpeechLabel+"（语音合成网页界面）")
	}
	if err := macSpeechStop(m, ctx, MacSpeechLabel, p.Plist); err != nil {
		return fmt.Errorf("停止语音合成网页界面失败: %w", err)
	}
	if result != nil {
		result.step(ctx, "已停止并删除服务：/speech/ 别名与直连端口都会失效，直到重新安装")
		result.step(ctx, "系统自带的 /usr/bin/say 与你的音色**没有被删除或修改**（那是 macOS 的一部分，面板不会动它）")
	}
	return nil
}
