package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  基础依赖（Base Dependencies）
//
//  "基础依赖" = 面板装好之后**应该一直存在**的外部命令，不属于任何单个应用，
//  但很多功能都靠它们。目前清单只有一组：
//
//    ffmpeg / ffprobe（同一个 Homebrew formula）
//      · Qwen3 TTS 用 mlx_audio 编码 mp3，**必须**有 ffmpeg；
//      · 音色接收端用 ffprobe 真解码校验上传的样本、用 ffmpeg 做转码与归一；
//      · 后续的音视频功能（转码、时长/编码探测、缩略图）也都要它。
//
//  为什么必须把它当"一等公民"管起来（2026-09-16 真机事故，代价极大）：
//    mini 在 9/15 23:32 被"抹掉所有内容和设置"，随后面板在 9/16 01:17–01:50
//    重装了 Qwen TTS —— **整条安装链里没有 ffmpeg**（qwentts.go 全文搜不到它）。
//    结果是一个最难查的半残服务：进程能启动、健康检查全绿、请求 wav 正常，
//    但一合成 mp3 就返回 **HTTP 200 + 0 字节 body**。网站的接收端逐块
//    urlopen(...).read() 因此抛 IncompleteRead(0 bytes read)，用户**所有**
//    TTS 作业全败；Qwen 日志里 "RuntimeError: ffmpeg not found!" 出现 601 次。
//    手动 brew install ffmpeg 后立刻恢复。
//    另一条线索：交付时产物还是真 mp3，说明它是**后来被弄丢的**，
//    头号嫌疑是卸载流程里的 `brew autoremove` 把作为依赖的它一起带走。
//
//  所以这里负责四件事：
//    1) 清单：哪些算基础依赖（baseDependencies）；
//    2) 只读探测：现在缺哪些（CheckBaseDependencies / BaseDependencyStatuses）；
//    3) 幂等补装：缺什么装什么，**复用既有 brewRun / brewEnv**
//       （EnsureBaseDependencies）—— 镜像选择、降权、流式进度都不重写；
//    4) 卸载后护栏：卸载完检查它们还在不在，被带走就如实报告并补回
//       （GuardBaseDependenciesAfterUninstall）。
//
//  刻意**不**放进清单的东西，以及依据：
//    · python3：由「命令行开发者工具」提供，已有 EnsureCLT 这条专门的门禁，
//      再列一遍会出现两套真相（一套说已装、一套说没装）；
//    · mkcert：可选（没有它只是浏览器提示证书不受信任），install.sh 里就是
//      按"可选"处理的，把它升级成基础依赖等于偷偷改变用户机器的安装内容；
//    · nginx / php / mysql：属于「LNMP 应用组合」，是应用而不是基础环境，
//      有各自的安装入口与卸载计划。
// ============================================================================

// BaseDependency 描述一项基础依赖。
type BaseDependency struct {
	// Command 是命令名：探测与执行都用它。
	Command string `json:"command"`
	// Formula 是提供它的 Homebrew formula（空 = 面板不负责自动安装）。
	Formula string `json:"formula,omitempty"`
	// Why 是"为什么需要它"—— 给人看的。只报"缺失"而不说后果，
	// 用户没法判断该不该现在处理（这个项目反复吃过"提示没有信息量"的亏）。
	Why string `json:"why"`
	// Hint 是缺失时的补救提示，要能直接照抄。
	Hint string `json:"hint"`
}

// baseDependencies 是基础依赖清单。
//
// ffmpeg 与 ffprobe 来自**同一个 formula**，但仍分列两条：接收端是分别探测
// 它们的，只有 ffmpeg 时"上传样本的真解码校验"会静默退化成"只认 wav"，
// 而那种退化用户在界面上是看不见的 —— 分列才能如实暴露出来。
var baseDependencies = []BaseDependency{
	{
		Command: "ffmpeg",
		Formula: "ffmpeg",
		Why: "TTS 用 mlx_audio 编码 mp3 必需（缺了会返回 HTTP 200 但 body 是 0 字节，" +
			"用户只看到所有作业全败）；音视频转码、时长探测、缩略图等后续功能也要用",
		Hint: "brew install ffmpeg（面板的 Qwen TTS / 音色接收端部署与一键 LNMP 都会自动补装）",
	},
	{
		Command: "ffprobe",
		Formula: "ffmpeg",
		Why: "音色接收端用它真解码校验上传的样本（只认 wav 会让可用格式悄悄变少）；" +
			"媒体时长/编码探测也要用；随 ffmpeg 一起安装",
		Hint: "brew install ffmpeg（ffprobe 包含在同一个包内）",
	},
}

// servicePATH 是面板装出来的服务（Qwen TTS / 音色接收端）plist 里写的 PATH。
//
// 探测与验收都必须以它为参照，而不是面板进程自己的 PATH：面板是 LaunchDaemon，
// 通常只有 /usr/bin:/bin:/usr/sbin:/sbin；而**真正调用 ffmpeg 的是服务进程**。
// 真机事故里"文件在 /opt/homebrew/bin、服务进程却找不到"正是最容易被忽略的形态。
const servicePATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// BaseDependencies 返回基础依赖清单（副本：调用方改不动包内状态）。
func BaseDependencies() []BaseDependency {
	out := make([]BaseDependency, len(baseDependencies))
	copy(out, baseDependencies)
	return out
}

// MissingDependency 是"探测到缺失"的一项基础依赖，带上补救命令。
type MissingDependency struct {
	BaseDependency
	// FixCmd 是可以直接照抄的补救命令（面板能补装时就是 brew install …）。
	FixCmd string `json:"fix_cmd"`
}

// BaseDependencyStatus 是清单里某一项此刻的状态（只读接口返回它）。
type BaseDependencyStatus struct {
	BaseDependency
	// Satisfied 表示命令现在真的可用（探测到了可执行文件）。
	Satisfied bool `json:"satisfied"`
	// Path 是探测到的绝对路径（Satisfied=false 时为空）。
	Path string `json:"path,omitempty"`
	// Fixable 表示面板能一键补装（由 Homebrew formula 提供）。
	Fixable bool `json:"fixable"`
	// FixCmd 是手工补救命令（Fixable 时与一键补装等价）。
	FixCmd string `json:"fix_cmd,omitempty"`
}

// BaseDependencyProblem 是"验收时发现不可用"的一项依赖。
type BaseDependencyProblem struct {
	BaseDependency
	// Detail 是真实的失败原因（执行输出或错误），要能让人看懂。
	Detail string `json:"detail"`
}

// commandProbe 返回"这个命令现在能不能用"的探测函数。
//
// 抽成可注入的（Options.LookPath）是为了单测：默认实现会碰真实文件系统，
// 单测里若依赖"测试机恰好装没装 ffmpeg"，结论就会随机器而变（开发机装了、
// CI 没装）—— 违反本项目"单测不碰真实环境"的纪律（AGENTS.md 第三节）。
func (m *Manager) commandProbe() func(string) (string, error) {
	if m.opt.LookPath != nil {
		return m.opt.LookPath
	}
	return m.defaultCommandProbe
}

// defaultCommandProbe 先看 PATH，再补看 Homebrew 的标准前缀。
//
// 为什么要补看前缀：面板由 LaunchDaemon 以 root 启动，它继承的 PATH 里没有
// /opt/homebrew/bin。只调 exec.LookPath 的话，明明装好的 ffmpeg 也会被判成
// "缺失"，于是每次安装都白装一遍（更糟的是：只读接口永远显示缺依赖，
// 用户以为坏了）。而服务 plist 里显式给了 Homebrew 前缀，服务是用得到的，
// 所以"服务能用"才是判据 —— 探测必须与它对齐。
func (m *Manager) defaultCommandProbe(command string) (string, error) {
	if p, err := exec.LookPath(command); err == nil {
		return p, nil
	}
	var dirs []string
	if prefix := m.brewPrefix(); prefix != "" && prefix != "." {
		dirs = append(dirs, filepath.Join(prefix, "bin"))
	}
	dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
	for _, d := range dirs {
		p := filepath.Join(d, command)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// fixCmdFor 给出某项依赖可照抄的补救命令。
func fixCmdFor(dep BaseDependency) string {
	if dep.Formula != "" {
		return "brew install " + dep.Formula
	}
	return ""
}

// baseDependencyFor 按 formula 找清单里对应的基础依赖。
func baseDependencyFor(formula string) (BaseDependency, bool) {
	for _, d := range baseDependencies {
		if d.Formula == formula {
			return d, true
		}
	}
	return BaseDependency{}, false
}

// AnnounceAppDependencies 把"这个应用声明的基础依赖"如实写进任务步骤。
//
// 用户明确要求："所有要用到他的软件安装时要提示对 ffmpeg 的依赖，最好同时安装。"
// 提示内容从**目录里的 Requires** 读（那是依赖关系的唯一真相），而不是在安装器里
// 手写一句"需要 ffmpeg" —— 手写的那句迟早会和目录里的声明对不上。
// 只提示本文件清单里的基础依赖；其它 brew 前置（如 python@3.11）由各自安装器处理。
//
// 注意：这是**提示**，不负责安装；安装由紧随其后的 EnsureBaseDependencies 完成。
// 拆开是为了让任务日志里"为什么需要它"出现在"正在安装它"之前，读起来顺。
func (m *Manager) AnnounceAppDependencies(ctx context.Context, appID string, result *InstallResult) {
	if result == nil {
		return
	}
	app, ok := FindApp(appID)
	if !ok {
		return
	}
	for _, req := range app.Requires {
		switch req.Type {
		case "brew_formula":
			if dep, isBase := baseDependencyFor(req.Value); isBase {
				result.step(ctx, fmt.Sprintf("依赖提示：「%s」需要基础依赖 %s，将一并安装/确认。%s",
					app.Name, dep.Command, req.Hint))
				continue
			}
			// 非"基础依赖"的 brew 包（例如 python@3.11）同样要**事先说清楚**：
			// 用户 2026-09-18 报障"Python 3.11 也是 tts 等的依赖，也没有一并安装"，
			// 其中的一半是"没人告诉他会被一并装上哪些东西"。
			result.step(ctx, fmt.Sprintf("依赖提示：「%s」需要 %s，将一并安装/确认。%s",
				app.Name, req.Value, req.Hint))
		case "docker":
			result.step(ctx, fmt.Sprintf("依赖提示：「%s」需要 Docker 运行时。%s",
				app.Name, req.Hint))
		default:
			if req.Hint != "" {
				result.step(ctx, fmt.Sprintf("依赖提示：「%s」需要 %s：%s",
					app.Name, req.Value, req.Hint))
			}
		}
	}
}

// CheckBaseDependencies 只读探测：返回当前**缺**的基础依赖，不装任何东西。
//
// 只读是刻意的：它既给只读接口用（GET /api/v1/system/deps），也是安装流程开头
// "要不要补装"的判据。带副作用的探测会让"看一眼状态"变成一次安装。
func (m *Manager) CheckBaseDependencies(ctx context.Context) []MissingDependency {
	probe := m.commandProbe()
	var out []MissingDependency
	for _, dep := range baseDependencies {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		if _, err := probe(dep.Command); err == nil {
			continue
		}
		out = append(out, MissingDependency{BaseDependency: dep, FixCmd: fixCmdFor(dep)})
	}
	return out
}

// BaseDependencyStatuses 返回清单里**每一项**的状态（含已满足的），给只读接口用。
func (m *Manager) BaseDependencyStatuses(ctx context.Context) []BaseDependencyStatus {
	probe := m.commandProbe()
	out := make([]BaseDependencyStatus, 0, len(baseDependencies))
	for _, dep := range baseDependencies {
		st := BaseDependencyStatus{
			BaseDependency: dep,
			Fixable:        dep.Formula != "",
			FixCmd:         fixCmdFor(dep),
		}
		if p, err := probe(dep.Command); err == nil {
			st.Satisfied = true
			st.Path = p
		}
		out = append(out, st)
	}
	return out
}

// EnsureBaseDependencies 幂等地补齐缺失的基础依赖。
//
// 复用既有的 brewRun / brewEnv（**不另写一套 brew 调用**）：镜像选择、降权到
// 真实用户、逐行进度流式输出都由它们负责。每个动作都写进 result.Steps ——
// 用户要能在任务中心看到到底装了什么。
//
// 失败一律**如实返回错误**，绝不把"没装上"写成"已就绪"：2026-09-16 的事故
// 就是"看起来装好了"造成的（服务能启动，编码 mp3 时才发现 ffmpeg 不在），
// 所以这里宁可让整个安装任务失败。
func (m *Manager) EnsureBaseDependencies(ctx context.Context, result *InstallResult) error {
	if result == nil {
		result = &InstallResult{Steps: []string{}}
	}
	missing := m.CheckBaseDependencies(ctx)
	if len(missing) == 0 {
		result.step(ctx, "基础依赖已就绪（"+baseDependencyNames()+"）")
		return nil
	}

	// 没有 formula 的依赖面板装不了（例如系统自带的命令被删掉）：必须明确报出来。
	for _, dep := range missing {
		if dep.Formula == "" {
			return fmt.Errorf("基础依赖 %s 缺失，且面板没有可用的自动安装方式。%s",
				dep.Command, dep.Hint)
		}
	}

	// 同一个 formula 可能提供多个命令（ffmpeg/ffprobe）：只装一次。
	done := map[string]bool{}
	for _, dep := range missing {
		if done[dep.Formula] {
			continue
		}
		done[dep.Formula] = true
		if err := m.ensureBaseFormula(ctx, dep.BaseDependency, result); err != nil {
			return err
		}
	}

	// 装完必须**复验**：brew 返回 0 不等于命令真的能用（PATH / 链接 / 权限都可能
	// 出问题，而"brew 说装好了"正是上次事故里最误导人的那句话）。
	if still := m.CheckBaseDependencies(ctx); len(still) > 0 {
		return fmt.Errorf("基础依赖安装后仍不可用：%s。%s",
			missingCommands(still), still[0].Hint)
	}
	result.step(ctx, "基础依赖已全部就绪："+missingCommands(missing))
	return nil
}

// ensureBaseFormula 装一个提供基础依赖的 formula（已在清单里，不在这里解析参数）。
func (m *Manager) ensureBaseFormula(ctx context.Context, dep BaseDependency, result *InstallResult) error {
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		return fmt.Errorf("缺少基础依赖 %s（%s），但没有 Homebrew 可以自动安装。"+
			"请先到「应用市场」安装 Homebrew 后重试，或手工执行：%s",
			dep.Command, dep.Why, fixCmdFor(dep))
	}
	if m.brewHas(ctx, dep.Formula) {
		// brew 说装了、命令却探测不到：多半是链接被删/没链接上（手工清理过 bin）。
		// 这不是"没装"，盲目再 install 会被 brew 拒绝，所以走 link。
		result.step(ctx, "Homebrew 里已有 "+dep.Formula+"，正在重新链接（"+dep.Command+" 探测不到）")
		if _, err := m.brewRun(ctx, 3*time.Minute, "link", dep.Formula); err != nil {
			return fmt.Errorf("基础依赖 %s 已安装但命令不可用，且 brew link 失败：%w", dep.Command, err)
		}
		return nil
	}
	result.step(ctx, "正在安装基础依赖 "+dep.Formula+"（"+dep.Command+"："+dep.Why+"）")
	// brewInstall：失败即换源（自建镜像 → 清华 → 官方 ghcr.io），
	// 换源前清掉相关下载缓存，见 install.go 顶部那段事故说明。
	if _, err := m.brewInstall(ctx, result, 30*time.Minute, dep.Formula); err != nil {
		return fmt.Errorf("安装基础依赖 %s 失败：%w。"+
			"它缺失会让 TTS 合成 mp3 返回 200 + 空 body（用户只看到作业全败），"+
			"请修好后重试，或手工执行：%s", dep.Formula, err, fixCmdFor(dep))
	}
	result.step(ctx, dep.Formula+" 安装完成（提供 "+dep.Command+"）")
	return nil
}

// VerifyBaseDependencies 用**服务进程的 PATH** 真的把每个基础依赖执行一次
// （`<命令> -version`），返回没法用的那些。
//
// 为什么不能只靠 CheckBaseDependencies（它只看文件在不在）：真机事故里最贵的
// 教训就是"看起来装了"不等于"能用" —— 二进制可能在、但动态库坏了、没有执行
// 权限、或者服务进程的 PATH 里没有它。mlx_audio 是在**服务进程里**调 ffmpeg 的，
// 所以验收必须按那个环境实跑一次。
//
// 不直接返回 error 是刻意的：调用方（安装器）要自己决定"是致命失败还是警告"，
// 而且需要把 Detail 原样展示给用户。
func (m *Manager) VerifyBaseDependencies(ctx context.Context) []BaseDependencyProblem {
	probe := m.commandProbe()
	var out []BaseDependencyProblem
	for _, dep := range baseDependencies {
		path, err := probe(dep.Command)
		if err != nil {
			out = append(out, BaseDependencyProblem{BaseDependency: dep, Detail: "命令不存在或不可执行"})
			continue
		}
		if detail := m.runDependencyVersion(ctx, path); detail != "" {
			out = append(out, BaseDependencyProblem{BaseDependency: dep, Detail: detail})
		}
	}
	return out
}

// runDependencyVersion 用与服务 plist 相同的 PATH 跑一次 `<命令> -version`。
//
// 返回空串 = 跑通了；非空 = 给用户看的失败原因。
func (m *Manager) runDependencyVersion(ctx context.Context, path string) string {
	if m.depExecOverride != nil {
		return m.depExecOverride(path)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, "-version")
	// 面板自己的 PATH 里没有 Homebrew，必须显式给：ffmpeg 依赖同目录下的
	// 动态库/编解码器，用错误的 PATH 跑出来的失败是**假**失败。
	cmd.Env = append(os.Environ(), "PATH="+servicePATH)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("执行 `%s -version` 失败：%v（输出：%s）",
			path, err, truncate(strings.TrimSpace(string(out)), 300))
	}
	return ""
}

// GuardBaseDependenciesAfterUninstall 在卸载任何应用之后检查基础依赖是否还在。
//
// intentionalFormula 是"用户这次**主动**卸载的那个 formula"（没有就传空串）：
// 如果缺的正好是它，说明基础依赖是被用户有意删掉的（例如在应用市场里卸载
// FFmpeg 这个条目），这时**绝不能自动装回去** —— 那等于对着用户干。
// 只如实报告"它是基础依赖、删了会有什么后果"。
//
// 为什么要挂在卸载之后（这是"ffmpeg 静默消失"的直接护栏）：
// 头号嫌疑就是卸载流程里的 `brew autoremove` 把作为共享依赖的 ffmpeg 一起带走，
// 而这件事**当时没有任何提示** —— 直到用户所有 TTS 作业全败才被发现。
// 卸载是唯一会"顺带删掉共享依赖"的操作，所以在这里设卡：
//
//	· 没缺 → 安静通过（不往任务日志里塞没用的行）；
//	· 缺了（非用户主动）→ 如实报告少了哪个（并点明 autoremove 这种可能），
//	  然后**自动补回**（用户明确要求"ffmpeg 是基础环境"），补装过程与结果都
//	  写进任务步骤；补不回来就写进 Warning 并给出可照抄的手工命令，绝不谎报；
//	· 缺了（用户主动卸载）→ 只报告后果，不补装（用户是故意的）。
func (m *Manager) GuardBaseDependenciesAfterUninstall(
	ctx context.Context, result *InstallResult, intentionalFormula string) {

	if result == nil {
		return
	}
	missing := m.CheckBaseDependencies(ctx)
	if len(missing) == 0 {
		return
	}

	var intentional, accidental []MissingDependency
	for _, dep := range missing {
		if intentionalFormula != "" && dep.Formula == intentionalFormula {
			intentional = append(intentional, dep)
			continue
		}
		accidental = append(accidental, dep)
	}

	if len(intentional) > 0 {
		msg := "「" + intentionalFormula + "」是面板的基础依赖：" +
			intentional[0].Why + "。卸载后依赖它的功能会失败" +
			"（TTS 合成 mp3 会返回 HTTP 200 + 空 body）；需要时请随时重新安装：" +
			intentional[0].FixCmd
		result.Warning = appendBaseDepWarning(result.Warning, msg)
		result.step(ctx, "⚠️ "+msg)
	}
	if len(accidental) == 0 {
		return
	}

	result.step(ctx, "⚠️ 卸载后发现基础依赖不见了："+missingCommands(accidental)+
		"（很可能是 `brew autoremove` 之类把共享依赖一起带走了）")
	for _, dep := range accidental {
		result.step(ctx, "   · "+dep.Command+"："+dep.Why)
	}
	result.step(ctx, "正在自动补回基础依赖（它们是面板的基础环境，TTS 等功能的公共前提）")
	if err := m.EnsureBaseDependencies(ctx, result); err != nil {
		msg := "基础依赖被卸载流程带走且自动补装失败：" + err.Error() +
			"。请手工执行 `" + accidental[0].FixCmd + "`，否则 TTS 合成 mp3 会返回 200 + 空 body"
		result.Warning = appendBaseDepWarning(result.Warning, msg)
		result.step(ctx, "❌ "+msg)
		return
	}
	result.step(ctx, "基础依赖已补回："+missingCommands(accidental))
}

// UninstallBaseDependency 卸载一个"作为独立软件安装"的基础依赖（目前只有 ffmpeg）。
//
// 为什么允许卸载（用户要求"不要禁止，但要明确提示"）：
// 它是基础环境，但用户可能确实想换成别的方式管理（例如自编译版）。
// 面板能做的正确的事是**把后果说清楚**，而不是替用户锁死。
// 所以这里先把"卸载会有什么后果"写进任务步骤，再真的 brew uninstall。
func (m *Manager) UninstallBaseDependency(ctx context.Context, app App, result *InstallResult) error {
	formula := app.BrewFormula
	if formula == "" {
		formula = app.ID
	}
	if result != nil {
		result.step(ctx, "⚠️ "+app.Name+" 是面板的基础依赖：卸载后 TTS 编码 mp3 会返回 "+
			"HTTP 200 + 0 字节 body（接收端只看到 IncompleteRead），音色样本校验与"+
			"后续音视频功能也会失效。需要时请随时从应用市场重新安装。")
	}
	if !m.brewHas(ctx, formula) {
		if result != nil {
			result.step(ctx, formula+" 未安装（Homebrew 里没有它），无需卸载")
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "正在 brew uninstall "+formula)
	}
	if _, err := m.brewRun(ctx, 5*time.Minute, "uninstall", formula); err != nil {
		return fmt.Errorf("卸载 %s 失败: %w", formula, err)
	}
	if result != nil {
		result.step(ctx, formula+" 已卸载（TTS 的 mp3 合成会因此失效，直到重新安装）")
	}
	return nil
}

// appendBaseDepWarning 把新警告并进已有警告（不覆盖前一条）。
func appendBaseDepWarning(cur, add string) string {
	if cur == "" {
		return add
	}
	return cur + "；" + add
}

// baseDependencyNames 返回清单里的命令名（用于步骤文案）。
func baseDependencyNames() string {
	names := make([]string, 0, len(baseDependencies))
	for _, d := range baseDependencies {
		names = append(names, d.Command)
	}
	return strings.Join(names, "、")
}

// missingCommands 把缺失项压成"ffmpeg、ffprobe"。
func missingCommands(deps []MissingDependency) string {
	names := make([]string, 0, len(deps))
	for _, d := range deps {
		names = append(names, d.Command)
	}
	return strings.Join(names, "、")
}

// describeDependencyProblems 把验收失败项压成一句给用户看的话。
func describeDependencyProblems(probs []BaseDependencyProblem) string {
	parts := make([]string, 0, len(probs))
	for _, p := range probs {
		parts = append(parts, p.Command+"（"+p.Detail+"）")
	}
	return strings.Join(parts, "；")
}
