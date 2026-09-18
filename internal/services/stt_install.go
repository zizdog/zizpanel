package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  「语音转文字（whisper.cpp）」的安装 / 卸载
//
//  这个条目与目录里其它条目一样由**面板自研安装器**部署（PanelInstaller=stt），
//  因为它有三件事通用 brew 流程做不了：
//    ① 要下**模型权重**（brew 的 caveat 里明说 "GGML model files …
//       are not downloaded by default"，不替用户下就是一个装完不能用的应用）；
//    ② 要注册**面板托管的网页界面**（`zizpanel stt-serve`，系统级 LaunchDaemon）；
//    ③ 要复核"真的能转出字"（不是"brew 退出码 0"）。
//
//  服务就是面板自己的二进制（照抄 imgcompress / macspeech 的取舍）：
//  不引入第二个可执行文件、内嵌前端随面板升级、不需要任何构建步骤。
//
//  模型放 `<真实用户家目录>/stt/models/`（与 qwentts 的 ~/tts/qwen3、
//  iopaint 的 ~/iopaint 同一套约定）。面板以 root 往用户家目录写完**必须把
//  归属交还真实用户**（AGENTS 第三节：否则以该用户身份运行的 stt-serve
//  反而写不进去 —— nginx 日志目录 / Colima 配置都栽在这条上）。
// ============================================================================

// ---------------------------------------------------------------------------
//  仅测试用的注入点
//
//  安装/卸载会写 /Library/LaunchDaemons、调 launchctl bootstrap、跑 brew，
//  单测既不可能真等、也绝不允许碰真实 launchd / 真实家目录（AGENTS 第三节）。
//  做成包级变量，生产路径永远是默认实现，测试替换后恢复。
// ---------------------------------------------------------------------------

var (
	// sttExecutable 返回面板二进制自身的路径（launchd 要重新执行它）。
	sttExecutable = os.Executable
	// sttPlistPath 返回服务 plist 的绝对路径。
	sttPlistPath = func() string { return SystemDaemonPlistPath(STTLabel) }
	// sttLaunch 装载 launchd 服务（默认 bootout + bootstrap + 等待）。
	sttLaunch = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.bootstrapService(ctx, label, plist)
	}
	// sttStop 停止服务、删 plist、删面板记录。
	sttStop = func(m *Manager, ctx context.Context, label, plist string) error {
		return m.removeService(ctx, label, plist)
	}
	// sttHealthy 打一次 /healthz 并等待 ok:true。
	sttHealthy = func(ctx context.Context, url string, timeout time.Duration) bool {
		return readyWaitJSONBool(ctx, url, "ok", true, timeout)
	}
	// sttListenOverride 仅供测试/本地试运行：覆盖监听地址。
	sttListenOverride = func() string { return "" }
	// sttEngineOverride 仅供测试：替换引擎（不碰真实 whisper-cli / ffmpeg）。
	sttEngineOverride = func() *STTEngine { return nil }
	// sttFetchOverride 仅供测试：替换模型下载实现（单测不许联网）。
	sttFetchOverride STTFetchFunc
)

// STTCurrentModelFile 是"当前档位"的落盘文件（相对 <root>）。
//
// 为什么存文件而不是进数据库：stt-serve 是**独立进程**（不读面板配置、不开
// 数据库，与 imgcompress/speech 同一个取舍），它必须能自己知道当前用哪一档。
// 一个只有一行的小文件是最不容易出错的做法 —— 面板与 stt-serve 都能读写。
const STTCurrentModelFile = "current_model"

// sttCurrentModel 读当前档位（读不到/内容不合法时返回默认档）。
func sttCurrentModel(root string) string {
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(root), STTCurrentModelFile))
	if err != nil {
		return DefaultSTTModelID()
	}
	id := strings.TrimSpace(string(b))
	if _, err := FindSTTModel(id); err != nil {
		return DefaultSTTModelID()
	}
	return id
}

// WriteSTTCurrentModel 写入当前档位（面板与 stt-serve 共用这一处实现）。
//
// 只接受清单里存在的档位名：写进一个不存在的档位等于让服务起不来，
// 这种"坏配置"必须在写入的那一刻就被拒绝。
func WriteSTTCurrentModel(root, modelID string) error {
	m, err := FindSTTModel(modelID)
	if err != nil {
		return err
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return fmt.Errorf("模型根目录为空，无法记录当前档位")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败：%w", root, err)
	}
	tmp := filepath.Join(root, STTCurrentModelFile+".tmp")
	if err := os.WriteFile(tmp, []byte(m.ID+"\n"), 0o644); err != nil {
		return fmt.Errorf("写入当前档位失败：%w", err)
	}
	return os.Rename(tmp, filepath.Join(root, STTCurrentModelFile))
}

// STTEngineFor 造一个与本 Manager 配置一致的引擎。
//
// 所有外部路径都从注入参数/Manager 选项来（brew 前缀走 m.brewPrefix()，
// 模型目录走 m.sttPaths()）—— **本文件与 stt.go 都不写死 /opt/homebrew、
// 也不写死用户名**，Intel Mac（/usr/local）与 Apple Silicon 都成立。
func (m *Manager) STTEngineFor() *STTEngine {
	if e := sttEngineOverride(); e != nil {
		return e
	}
	p := m.sttPaths()
	return NewSTTEngine(m.brewPrefix(), p.ModelsDir, sttCurrentModel(p.Root))
}

// STTListen 返回服务应当监听的地址。
//
// 只绑回环：转写要读用户上传的音频、要跑本机推理，对局域网没有意义，
// 也不该在用户没明确要的情况下扩大暴露面。要给别人用就走面板的 /stt/ 别名
// （那条路要求先登录面板）。
func STTListen(port int) string {
	if override := strings.TrimSpace(sttListenOverride()); override != "" {
		return override
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// STTHealthURL 是网页界面的健康检查地址（面板的服务健康检查也打它；
// 面板别名 /stt/healthz 与它是同一份实现）。
func STTHealthURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", STTPort)
}

// sttPlist 生成 LaunchDaemon 定义。
//
// 以**真实用户**身份运行（不是 root）：要写 `<家目录>/stt/` 下的模型与临时
// 文件、要 spawn 该用户 PATH 下的 whisper-cli；以 root 跑会把产物属主变成
// root，用户反而拿不走。
//
// 镜像基址作为参数带进去：stt-serve 不读面板配置（刻意的），但下载模型时
// 必须遵守面板的"镜像优先"设置，所以由 plist 把生效值传给它。
func sttPlist(panelBin string, port int, brewPrefix, root, modelID, mirrorBase, mirrorLAN, user, outLog, errLog string) string {
	var mirrorArgs string
	if strings.TrimSpace(mirrorBase) != "" {
		mirrorArgs += "        <string>--mirror-base</string>\n        <string>" + xmlEscape(mirrorBase) + "</string>\n"
	}
	if strings.TrimSpace(mirrorLAN) != "" {
		mirrorArgs += "        <string>--mirror-lan</string>\n        <string>" + xmlEscape(mirrorLAN) + "</string>\n"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <!-- 以真实用户运行：模型与临时音频的属主要归该用户 -->
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>stt-serve</string>
        <string>--listen</string>
        <string>%s</string>
        <string>--brew-prefix</string>
        <string>%s</string>
        <string>--root</string>
        <string>%s</string>
        <string>--model</string>
        <string>%s</string>
%s    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, STTLabel, user, panelBin, STTListen(port), brewPrefix, root, modelID, mirrorArgs,
		brewPrefix, outLog, errLog)
}

// ---------------------------------------------------------------------------
//  模型下载（面板安装器与 stt-serve 共用）
// ---------------------------------------------------------------------------

// STTMinPlausibleModelBytes 导出给 web 层做上传/落盘校验（与内部判据同一份）。
func STTMinPlausibleModelBytes() int64 { return sttMinPlausibleModelBytes }

// STTFetchFunc 是"把一个 URL 下载到本地文件"的注入点。
// 生产实现是 iopaint.go 里的 fetchToFile（带停滞看门狗 + .part 原子改名 + 进度回调）。
type STTFetchFunc func(ctx context.Context, url, dest string, onProgress func(got, total int64)) error

// STTModelSourceList 返回某个档位的下载候选（**不依赖 Manager**）。
//
// probe 用来判断"镜像上到底有没有这个文件"（面板侧是 m.checkMirrorURL，
// stt-serve 侧是一个普通 HEAD）。probe 为 nil 时所有镜像候选都直接放进去，
// 由下载环节自己去试 —— 那时也只是多一次失败重试，不会出错。
func STTModelSourceList(ctx context.Context, mirrorBase, mirrorLAN string, m0 STTModel,
	probe func(ctx context.Context, url string) error) []STTModelSource {

	var out []STTModelSource
	hfPath := sttHFRepo + "/resolve/main/" + m0.File
	seen := map[string]bool{}
	add := func(s STTModelSource) {
		if s.URL == "" || seen[s.URL] {
			return
		}
		seen[s.URL] = true
		out = append(out, s)
	}
	for _, base := range []struct{ name, base string }{
		{"自建镜像", strings.TrimRight(strings.TrimSpace(mirrorBase), "/")},
		{"自建镜像（局域网）", strings.TrimRight(strings.TrimSpace(mirrorLAN), "/")},
	} {
		if base.base == "" {
			continue
		}
		// ① 静态模型目录（与 iopaint 的 models/<app>/<file> 同一套布局）。
		staticURL := base.base + "/models/whisper/" + m0.File
		if probe == nil || probe(ctx, staticURL) == nil {
			add(STTModelSource{Name: base.name + "（静态模型）", URL: staticURL, Mirror: true})
		}
		// ② HF 按需缓存代理。
		hfURL := base.base + "/hf/" + hfPath
		if probe == nil || probe(ctx, hfURL) == nil {
			add(STTModelSource{Name: base.name + "（HF 缓存）", URL: hfURL, Mirror: true})
		}
	}
	add(STTModelSource{Name: "hf-mirror.com（国内公共镜像）", URL: sttHFMirror + "/" + hfPath})
	add(STTModelSource{Name: "huggingface.co（官方，国内常不可达）", URL: sttHFUpstream + "/" + hfPath})
	return out
}

// STTModelSourcesForManager 是面板侧（带设置）的候选列表。
func (m *Manager) STTModelSourcesForManager(ctx context.Context, result *InstallResult, m0 STTModel) []STTModelSource {
	if !m.MirrorEnabled() {
		return STTModelSourceList(ctx, "", "", m0, nil)
	}
	return STTModelSourceList(ctx, m.mirrorBase(), m.mirrorBaseLAN(), m0, m.checkMirrorURL)
}

// STTDownloadResult 是一次模型下载的结果。
type STTDownloadResult struct {
	Model  STTModel
	Path   string
	Source string
	Bytes  int64
	// Elapsed 是这次下载耗时（任务日志里给用户看）。
	Elapsed time.Duration
}

// DownloadSTTModelTo 把某个档位的权重下到 dst，逐个来源重试，失败如实报错。
//
// 三条硬要求（都是本仓库踩过的坑）：
//  1. **不留半份模型**：下载写 `<dst>.part`（fetchToFile 自己保证），
//     校验不过的文件**当场删掉**再换下一个来源 —— 留着它，服务会带着坏权重
//     起来，表现为"每次转写都失败或输出乱码"，而界面上一切正常；
//  2. **失败要说清试过哪些来源、各自为什么失败**，最后一并返回；
//  3. **下完必须校验**：大小与上游一致 + ggml 魔数（见 validateSTTModelFile）。
func DownloadSTTModelTo(ctx context.Context, srcs []STTModelSource, dst string, want STTModel,
	fetch STTFetchFunc, onProgress func(note string)) (*STTDownloadResult, error) {

	if fetch == nil {
		fetch = defaultSTTFetch
	}
	if len(srcs) == 0 {
		return nil, fmt.Errorf("没有可用的模型下载来源（镜像未配置且没有任何公网来源）")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, fmt.Errorf("创建模型目录 %s 失败：%w", filepath.Dir(dst), err)
	}
	started := time.Now()
	var failures []string
	for i, src := range srcs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if onProgress != nil {
			onProgress(fmt.Sprintf("第 %d/%d 个来源：%s（%s）", i+1, len(srcs), src.Name, src.URL))
		}
		// 每个来源都从 .part 重来：上一个来源的半份文件不能影响这一份。
		_ = os.Remove(dst + ".part")
		serr := fetch(ctx, src.URL, dst, func(got, total int64) {
			if onProgress == nil {
				return
			}
			if total > 0 {
				onProgress(fmt.Sprintf("下载中：%.0f%%（%s / %s，来源 %s）",
					float64(got)*100/float64(total), humanBytes(got), humanBytes(total), src.Name))
			} else {
				onProgress(fmt.Sprintf("下载中：%s（来源 %s）", humanBytes(got), src.Name))
			}
		})
		if serr != nil {
			failures = append(failures, fmt.Sprintf("%s：%v", src.Name, serr))
			if onProgress != nil {
				onProgress("该来源失败：" + serr.Error())
			}
			_ = os.Remove(dst + ".part")
			continue
		}
		if verr := validateSTTModelFile(dst, want); verr != nil {
			failures = append(failures, fmt.Sprintf("%s：%v", src.Name, verr))
			if onProgress != nil {
				onProgress("校验不通过，已删除损坏文件：" + verr.Error())
			}
			_ = os.Remove(dst)
			continue
		}
		st, _ := os.Stat(dst)
		res := &STTDownloadResult{Model: want, Path: dst, Source: src.Name, Elapsed: time.Since(started)}
		if st != nil {
			res.Bytes = st.Size()
		}
		if onProgress != nil {
			onProgress(fmt.Sprintf("模型就绪：%s（%s，来源：%s，用时 %.0f 秒）",
				dst, humanBytes(res.Bytes), src.Name, res.Elapsed.Seconds()))
		}
		return res, nil
	}
	return nil, fmt.Errorf("模型 %s（%s）下载失败，已试过 %d 个来源：\n  %s\n"+
		"请检查网络后重试；也可以在能出网的机器上手工下载 %s 放到 %s 再重试",
		want.ID, want.File, len(srcs), strings.Join(failures, "\n  "), want.File, dst)
}

// defaultSTTFetch 是生产实现：复用 iopaint 那条"停滞看门狗 + .part"的下载器。
//
// 为什么不各写一份：那条实现里有本仓库最贵的教训（"不动了就失败"必须能被
// 单测注入验证；失败绝不留下半个文件冒充好文件），复制一份就是让它漂移。
func defaultSTTFetch(ctx context.Context, url, dest string, onProgress func(got, total int64)) error {
	return fetchToFile(ctx, &http.Client{}, url, dest, iopaintStallTimeout, func(got, total int64) {
		if onProgress != nil {
			onProgress(got, total)
		}
	})
}

// DownloadSTTModel 是面板侧的"下载某一档"（安装流程与"补下另一档"共用）。
func (m *Manager) DownloadSTTModel(ctx context.Context, modelID string, result *InstallResult) (*STTDownloadResult, error) {
	p := m.sttPaths()
	dst, want, err := sttModelPath(p.ModelsDir, modelID)
	if err != nil {
		return nil, err
	}
	if STTModelFileExists(p.ModelsDir, want.ID) {
		if result != nil {
			result.step(ctx, fmt.Sprintf("档位 %s 的模型已在（%s），跳过下载", want.ID, dst))
		}
		return &STTDownloadResult{Model: want, Path: dst, Source: "（已在本地）"}, nil
	}
	// 先清掉可疑的残留（大小不对的旧文件），否则下载器会以为已经下好了。
	if st, serr := os.Stat(dst); serr == nil && st.Size() != want.Bytes {
		if result != nil {
			result.step(ctx, fmt.Sprintf("发现不完整的旧模型文件（%d 字节，应为 %d），先删除：%s",
				st.Size(), want.Bytes, dst))
		}
		_ = os.Remove(dst)
	}
	// 离线模式（仅走 NAS）：缺件必须明确失败，绝不偷偷出网。
	if m.MirrorOfflineOnly(ctx) {
		if err := m.MirrorOfflinePreflight(ctx, "whisper 模型 "+want.File,
			m.mirrorSubPath("models")+"/whisper/"+want.File); err != nil {
			// 静态目录没有时，退一步看 HF 代理上有没有（那也算"仅走 NAS"）。
			if err2 := m.MirrorOfflinePreflight(ctx, "whisper 模型 "+want.File,
				m.mirrorSubPath("hf")+"/"+sttHFRepo+"/resolve/main/"+want.File); err2 != nil {
				return nil, err
			}
		}
	}
	srcs := m.STTModelSourcesForManager(ctx, result, want)
	if result != nil {
		result.step(ctx, fmt.Sprintf("模型 %s：%s，共 %d 个候选来源（镜像优先，逐个回落）",
			want.ID, humanBytes(want.Bytes), len(srcs)))
	}
	res, derr := DownloadSTTModelTo(ctx, srcs, dst, want, m.sttFetch(), func(note string) {
		if result != nil {
			result.step(ctx, note)
		}
	})
	if derr != nil {
		return nil, derr
	}
	// 以 root 下载的文件属主是 root，必须交还真实用户 —— 否则以该用户身份
	// 运行的 stt-serve 读不了自己的模型（AGENTS 第三节的坑 156/163）。
	if m.opt.UserName != "" {
		if cerr := chownTree(m.opt.UserName, p.ModelsDir); cerr != nil {
			if result != nil {
				result.step(ctx, "警告：模型已就绪，但把属主交还用户失败："+cerr.Error()+
					"（服务可能读不到模型，可在终端执行 chown -R "+m.opt.UserName+" "+p.ModelsDir+" 后重启服务）")
			}
		}
	}
	return res, nil
}

// sttFetch 返回下载实现（测试可注入；默认走 iopaint 那条带看门狗的下载器）。
func (m *Manager) sttFetch() STTFetchFunc {
	if sttFetchOverride != nil {
		return sttFetchOverride
	}
	return defaultSTTFetch
}

// ---------------------------------------------------------------------------
//  安装
// ---------------------------------------------------------------------------

// InstallSTT 安装「语音转文字（whisper.cpp）」。
//
// 顺序（每一步都真的复核，不靠"上一步退出码 0"）：
//  1. 提示并补齐基础依赖 —— ffmpeg（转码链路要用它，见 Requires）；
//  2. `brew install whisper.cpp`（走**多源兜底**：镜像 → 清华 → 官方）；
//  3. 复核 whisper-cli 真的能跑（--help 有输出）；
//  4. 下载**默认档**模型（small）并校验（大小 + ggml 魔数）；
//  5. 写系统级 plist、装载、登记进「服务管理」、等 /healthz 真的报 ok:true
//     （那一刻 = 模型在 + 进程在 + **真的跑过一次极短音频转写**）。
func (m *Manager) InstallSTT(ctx context.Context, app App, result *InstallResult) error {
	if result != nil {
		result.App = app.ID
	}
	// ① 基础依赖：从目录的 Requires 读出来如实提示，再幂等补齐。
	m.AnnounceAppDependencies(ctx, app.ID, result)
	if err := m.EnsureBaseDependencies(ctx, result); err != nil {
		return fmt.Errorf("补齐基础依赖失败：%w（语音转文字要用 ffmpeg 把上传的音频转成 16 kHz WAV）", err)
	}

	// ② 引擎本体。
	formula := strings.TrimSpace(app.BrewFormula)
	if formula == "" {
		formula = STTBrewFormula
	}
	if m.brewHas(ctx, formula) {
		if result != nil {
			result.step(ctx, formula+" 已经装好了（Homebrew 里已有），跳过安装")
		}
	} else {
		if result != nil {
			result.step(ctx, "正在 brew install "+formula+
				"（原生 arm64 瓶、Metal 加速；不需要 Docker / Python / GUI）")
		}
		// 必须走 brewInstall（多源兜底）：单源撞上镜像缺件就会整个失败。
		if _, err := m.brewInstall(ctx, result, 30*time.Minute, formula); err != nil {
			return fmt.Errorf("安装 %s 失败：%w", formula, err)
		}
	}

	// ③ 复核引擎真的能跑。
	eng := m.STTEngineFor()
	if verr := m.sttVerifyCLI(ctx, eng, result); verr != nil {
		return verr
	}

	// ④ 下载默认档模型。
	modelID := DefaultSTTModelID()
	p := m.sttPaths()
	if err := WriteSTTCurrentModel(p.Root, modelID); err != nil {
		return err
	}
	if _, derr := m.DownloadSTTModel(ctx, modelID, result); derr != nil {
		return derr
	}

	// ⑤ 网页界面服务。
	if err := m.installSTTService(ctx, app, result); err != nil {
		return err
	}
	port := app.WebPort()
	if port <= 0 {
		port = STTPort
	}
	if result != nil {
		state := STTModelsStateFor(p.ModelsDir, modelID)
		var ready []string
		for _, ms := range state.Models {
			if ms.Installed {
				ready = append(ready, ms.ID)
			}
		}
		result.Steps = append(result.Steps,
			"网页界面：直连 http://127.0.0.1:"+fmt.Sprint(port)+
				"/ ，或从「应用市场 → 语音转文字」点「打开」走面板别名 /"+STTSlug+"/（要求先登录面板）",
			"OpenAI 兼容接口：POST http://127.0.0.1:"+fmt.Sprint(port)+"/v1/audio/transcriptions",
			`  例：curl -sS http://127.0.0.1:`+fmt.Sprint(port)+`/v1/audio/transcriptions \`,
			`        -F file=@/tmp/test.wav -F model=`+modelID+` -F language=zh`,
			"已就绪的档位："+strings.Join(ready, "、")+"（其余档位可在网页界面上按需下载，界面会显示各自体积）",
			"模型目录："+p.ModelsDir+"（卸载时可在确认框里选择是否一并删除）",
		)
	}
	return nil
}

// sttVerifyCLI 复核 whisper-cli 真的能跑。
//
// 只 os.Stat 是不够的：brew 的"已安装"可能是悬空软链（Cellar 已删），
// 也可能缺了动态库直接 dyld 报错。所以真跑一次 `--help` 并看输出里
// 有没有我们依赖的那几个开关。
func (m *Manager) sttVerifyCLI(ctx context.Context, eng *STTEngine, result *InstallResult) error {
	out, err := eng.run()(ctx, 60*time.Second, eng.CLIBin, []string{"--help"}, nil)
	if err != nil {
		return fmt.Errorf("引擎装好了但跑不起来（%s --help 失败）：%w。"+
			"通常是 Homebrew 下载/链接失败，请重试或在终端跑 `brew install %s`",
			eng.CLIBin, err, STTBrewFormula)
	}
	// 这几个开关是转写流程真正用到的（模型是每次调用的参数、JSON 输出、
	// 进度行、语言）。缺任何一个说明装的不是我们预期的版本 —— 如实失败，
	// 而不是等用户点转写时才报错。
	for _, want := range []string{"--model", "--output-json", "--print-progress", "--language"} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("引擎 %s 不认识 %s（装到的是不兼容的版本？）。"+
				"请执行 `brew reinstall %s` 后重试", eng.CLIBin, want, STTBrewFormula)
		}
	}
	if result != nil {
		ver := sttCLIVersionLine(out)
		if ver != "" {
			result.step(ctx, "引擎已就绪："+eng.CLIBin+"（"+ver+"）")
		} else {
			result.step(ctx, "引擎已就绪："+eng.CLIBin+"（版本行没解析出来，但 --help 的关键开关都在）")
		}
		if fileExecutable(eng.ServerBin) {
			// 如实说明包里还有什么：用户可能想直接用官方服务端。
			result.step(ctx, "提示：同一个包里还有官方 OpenAI 兼容服务端 "+eng.ServerBin+
				"（面板自己的界面用的是 "+filepath.Base(eng.CLIBin)+"，因为它能给长音频真实进度、能按请求切档）")
		}
	}
	return nil
}

// sttCLIVersionLine 从 `whisper-cli --help` 的输出里挑一行版本/构建信息。
func sttCLIVersionLine(help string) string {
	for _, line := range strings.Split(help, "\n") {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "whisper.cpp") && (strings.Contains(l, "commit") || strings.Contains(l, "version")) {
			return tailText(l, 160)
		}
	}
	return ""
}

// installSTTService 写系统级 plist、装载、登记、等健康。
func (m *Manager) installSTTService(ctx context.Context, app App, result *InstallResult) error {
	if strings.TrimSpace(m.opt.UserName) == "" {
		return fmt.Errorf("无法确定运行语音转文字网页界面的真实用户（UserName 为空）")
	}
	panelBin, err := sttExecutable()
	if err != nil {
		return fmt.Errorf("找不到面板自身的可执行文件路径（launchd 要用它启动界面）：%w", err)
	}
	if strings.TrimSpace(panelBin) == "" {
		return fmt.Errorf("面板自身的可执行文件路径为空，无法注册语音转文字网页界面")
	}
	p := m.sttPaths()
	if strings.TrimSpace(p.Plist) == "" {
		return fmt.Errorf("语音转文字网页界面的 plist 路径为空")
	}
	port := app.WebPort()
	if port <= 0 {
		port = STTPort
	}
	// 日志目录必须存在（launchd 会直接打开 StandardOutPath，目录不在会让作业起不来）。
	if err := os.MkdirAll(filepath.Dir(p.OutLog), 0o755); err != nil {
		return fmt.Errorf("创建日志目录 %s 失败：%w", filepath.Dir(p.OutLog), err)
	}
	// 服务以真实用户跑，它要读模型目录、写临时文件 —— 先把目录建好并把属主交还用户。
	if err := os.MkdirAll(p.ModelsDir, 0o755); err != nil {
		return fmt.Errorf("创建模型目录 %s 失败：%w", p.ModelsDir, err)
	}
	if m.opt.UserName != "" {
		_ = chownTree(m.opt.UserName, p.Root)
	}
	plist := sttPlist(panelBin, port, m.brewPrefix(), p.Root, sttCurrentModel(p.Root),
		m.mirrorBase(), m.mirrorBaseLAN(), m.opt.UserName, p.OutLog, p.ErrLog)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist %s 失败（面板需要以 root 运行）：%w", p.Plist, err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist %s 失败：%w", p.Plist, err)
	}
	if result != nil {
		result.step(ctx, "正在注册并启动网页界面服务 "+STTLabel+"（"+STTListen(port)+"）")
	}
	if err := sttLaunch(m, ctx, STTLabel, p.Plist); err != nil {
		return fmt.Errorf("启动语音转文字网页界面失败：%w", err)
	}
	if err := m.RegisterInstalledService(ctx, STTLabel, app.Name, app.Icon, app.Category, port); err != nil {
		// 登记失败不该把"界面已经起来"报成安装失败，但必须如实留下警告。
		if result != nil {
			result.step(ctx, "警告：界面已启动，但登记进「服务管理」失败："+err.Error()+
				"（可在「应用 → 已安装」里点「+ 注册服务」手动加入）")
		}
	}
	if result != nil {
		result.Steps = append(result.Steps, "已注册为系统级后台服务（开机自启、不依赖用户登录）")
	}
	return m.waitSTTReady(ctx, p, port, result)
}

// waitSTTReady 等到 /healthz 真的报 ok:true。
//
// 为什么不是"端口在听就算成功"（AGENTS 第三节）：服务可以起来而模型不在、
// 或 ffmpeg 缺失、或权重损坏 —— 这时界面能打开但每次转写都失败，
// 端口探测会把它报成健康。/healthz 的 ok:true 意味着**真的跑过一次转写**。
func (m *Manager) waitSTTReady(ctx context.Context, p STTPaths, port int, result *InstallResult) error {
	const timeout = 180 * time.Second
	url := STTHealthURL()
	if port > 0 && port != STTPort {
		url = fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	}
	wait := sttHealthy
	return assertReady(ctx, readySpec{
		What:    "语音转文字网页界面",
		Expect:  url + " 在 " + timeout.String() + " 内返回 {\"ok\":true}（模型在 + 引擎在 + 真的跑通一次极短音频转写）",
		Timeout: timeout,
		Probe: func(ctx context.Context) readyVerdict {
			if wait(ctx, url, timeout) {
				return readyVerdict{OK: true, Actual: "已就绪，健康检查确认能真的转出结果"}
			}
			return readyVerdict{Actual: url + " 没有返回 ok:true（服务没起来，或模型/ffmpeg 不可用、权重损坏）"}
		},
		LogPath: p.ErrLog,
		State:   "引擎 " + filepath.Base(m.STTEngineFor().CLIBin) + " 已复核可执行，模型已下载并校验，服务已登记进「服务管理」",
		Missing: "但网页界面不可用，市场里的「打开」与直连端口都会打不开",
		Remedy: "在「服务管理 → 语音转文字」里点「重启服务」再试；" +
			"仍然失败请看下面的日志尾部（常见原因：模型没下完 / 没有 ffmpeg）",
		Result: result,
	})
}

// ---------------------------------------------------------------------------
//  卸载
// ---------------------------------------------------------------------------

// STTModelOnDisk 是磁盘上一个模型文件的现状（卸载确认框要逐条列出来）。
type STTModelOnDisk struct {
	ID    string `json:"id"`
	File  string `json:"file"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	// Installed 表示"文件在，且大小与上游一致"。
	Installed bool `json:"installed"`
}

// InstalledSTTModels 返回磁盘上真实存在的模型文件（**判据贴着运行体**：
// 只报真的在的，不报"清单里有但没下"的）。
//
// 卸载流程用它列出"将要删除哪些路径、各多大"—— 用户按下去之前就看得见。
func (m *Manager) InstalledSTTModels() []STTModelOnDisk {
	p := m.sttPaths()
	var out []STTModelOnDisk
	for _, mdl := range STTModels {
		path := filepath.Join(p.ModelsDir, mdl.File)
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		out = append(out, STTModelOnDisk{
			ID: mdl.ID, File: mdl.File, Path: path, Bytes: st.Size(),
			Installed: st.Size() == mdl.Bytes,
		})
	}
	return out
}

// STTModelBytes 返回磁盘上全部模型文件的总字节数（卸载确认框显示"共多大"）。
func (m *Manager) STTModelBytes() int64 {
	var total int64
	for _, f := range m.InstalledSTTModels() {
		total += f.Bytes
	}
	return total
}

// UninstallSTT 卸载：停服务 + 撤 plist + (按用户选择)删模型 + brew uninstall 引擎。
//
// 三件事的归属必须分清（写在确认框里，别让用户以为"卸载"会自动删掉一切）：
//   - 服务与 plist：**总是**摘掉（否则留下一个 KeepAlive 复活却找不到引擎的僵尸）；
//   - 模型权重（几百 MB ~ 1.5 GB）：只有用户显式选择"同时删除数据"时才删；
//   - brew 包：总是卸（这就是这个应用的引擎本体）。
//
// removeData 为 false 时**保留模型**并如实说明"它们在磁盘上占多少、下次装回来
// 不用重下"—— model 文件是可复用的下载产物，删掉就得重新下几百 MB。
func (m *Manager) UninstallSTT(ctx context.Context, app App, removeData, force bool, result *InstallResult) error {
	if err := m.removeSTTService(ctx, result); err != nil {
		return err
	}
	// 模型：先如实报告，再按用户选择动手。
	models := m.InstalledSTTModels()
	if len(models) > 0 {
		if result != nil {
			for _, f := range models {
				result.step(ctx, fmt.Sprintf("磁盘上的模型：%s（%s，%s）", f.Path, f.ID, humanBytes(f.Bytes)))
			}
		}
		if removeData {
			p := m.sttPaths()
			if result != nil {
				result.step(ctx, fmt.Sprintf("按你的选择删除模型目录 %s（共 %s）", p.ModelsDir, humanBytes(m.STTModelBytes())))
			}
			if err := os.RemoveAll(p.ModelsDir); err != nil {
				return fmt.Errorf("删除模型目录 %s 失败：%w", p.ModelsDir, err)
			}
			if result != nil {
				result.step(ctx, "模型已删除（下次安装要重新下载）")
			}
		} else {
			if result != nil {
				result.step(ctx, fmt.Sprintf("按你的选择**保留**模型（共 %s）—— "+
					"它们只是下载产物，下次装回来不用重下；要清理请在弹出的选项里勾选"+
					"「同时删除数据」，或手工删除 %s", humanBytes(m.STTModelBytes()), m.sttPaths().ModelsDir))
			}
		}
	}
	// 引擎包。
	formula := strings.TrimSpace(app.BrewFormula)
	if formula == "" {
		formula = STTBrewFormula
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
	if _, err := m.brewRun(ctx, 10*time.Minute, "uninstall", formula); err != nil {
		return fmt.Errorf("卸载 %s 失败：%w", formula, err)
	}
	if result != nil {
		result.step(ctx, "已卸载 "+formula+"：网页界面与面板别名 /"+STTSlug+"/ 都会不可用，直到重新安装")
	}
	return nil
}

// removeSTTService 停止并删除网页界面服务（幂等）。
func (m *Manager) removeSTTService(ctx context.Context, result *InstallResult) error {
	p := m.sttPaths()
	hasPlist := fileExists(p.Plist)
	hasRecord := false
	if m.repo != nil {
		if list, err := m.repo.List(ctx); err == nil {
			for _, s := range list {
				if s.LaunchLabel == STTLabel {
					hasRecord = true
					break
				}
			}
		}
	}
	if !hasPlist && !hasRecord {
		if result != nil {
			result.step(ctx, "语音转文字网页界面服务本来就没有注册，跳过停止")
		}
		return nil
	}
	if result != nil {
		result.step(ctx, "停止并删除 launchd 服务 "+STTLabel+"（语音转文字网页界面）")
	}
	if err := sttStop(m, ctx, STTLabel, p.Plist); err != nil {
		return fmt.Errorf("停止语音转文字网页界面失败：%w", err)
	}
	return nil
}

// STTProbeMirrorFile 判断镜像上一个模型文件在不在（HEAD，4 秒超时）。
//
// 给 stt-serve 用：那个进程**不读面板配置**（刻意的），所以它拿不到
// Manager 的 checkMirrorURL，只能自己探一次。
func STTProbeMirrorFile(ctx context.Context, url string) error {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<12))
	if res.StatusCode >= 200 && res.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("HTTP %d", res.StatusCode)
}

// STTModelSourcesForServe 是 stt-serve 侧的候选列表（镜像基址由 plist 参数传进来）。
func STTModelSourcesForServe(ctx context.Context, mirrorBase, mirrorLAN string, m0 STTModel) []STTModelSource {
	return STTModelSourceList(ctx, mirrorBase, mirrorLAN, m0, STTProbeMirrorFile)
}

// STTDefaultFetch 是模型下载的生产实现（导出给 stt-serve 用）。
func STTDefaultFetch(ctx context.Context, url, dest string, onProgress func(got, total int64)) error {
	return defaultSTTFetch(ctx, url, dest, onProgress)
}
