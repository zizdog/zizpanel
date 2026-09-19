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
//  面板自建 Python 运行时的**唯一版本来源**
//
//  背景（2026-09-18 用户反馈）：装 Qwen3 TTS 时 `brew install python@3.11`
//  在用户机器上反复失败，用户看到的现象是"预置的 python 版本装不上"。
//  真实原因有两层（见 DEVELOPMENT.md 坑 157）：镜像站按需缓存把上游一次
//  `200 + 空 body` 永久缓存成了 0 字节文件；而 Housebrew 的 python@3.11
//  当前补丁（3.11.16）又是**镜像侧最新同步**的那一版，最容易被"同步延迟"命中。
//
//  所以这里把"面板用哪个 Python"集中成一个常量，并有两条硬要求：
//    1. 换版本时只改这一处 —— 在此之前 "python@3.11" 与 "python3.11" 散落在
//       qwentts.go / iopaint.go 的安装器里（解释器路径、site-packages 路径、
//       venv 创建命令），靠 grep 找全，漏一处的表现是"venv 用一个版本建、
//       包装进另一个版本的 site-packages"，而且**当时不会报错**。
//    2. 装完必须**如实报告真正装上的补丁版本**（`brew list --versions`），
//       因为 brew 装的是 formula 当前指向的补丁，不由我们决定 —— 用户有权
//       在任务日志里看到 "python@3.11 3.11.16" 这样的真实结果（见
//       InstalledBrewVersion），而不是我们预设的那个数字。
// ============================================================================

// panelPythonFormula 是面板自建 Python 运行时（Qwen3 TTS / IOPaint）的预置版本。
//
// 为什么是 python@3.11：
//   - mlx-audio（Qwen3 TTS 的推理后端）在 3.11 上有预编译 wheel，且本机
//     生产环境验证过整条链路（真机跑的是 3.11.12 的 venv，见
//     ZizPanel-当前状态.md 的部署记录）；
//   - IOPaint 的 PyTorch 依赖在 3.11 上同样是稳妥组合；
//   - 对应的 bottle 在**国内镜像与镜像站**上都能取到（含依赖闭包，
//     已用 tools/seed-nas-brew.sh 预置到镜像站，见该脚本的说明与实测输出）。
//
// 改这个常量之前请先做两件事：
//
//	① 用 `bash tools/seed-nas-brew.sh <新 formula>` 把新版本的瓶（含依赖）
//	   预置到镜像站，并确认每个文件都 sha256 一致 + `X-Cache: HIT`；
//	② 确认 mlx-audio / iopaint 在新 Python 上有可用的 wheel（PyPI 上的
//	   cp3xx-macosx_arm64 轮子），否则装到一半才失败。
const panelPythonFormula = "python@3.11"

// pythonFormulaVersion 把 "python@3.11" 变成 "3.11"（解释器后缀与
// site-packages 目录名都用它）。无 @ 的 formula 返回空串。
func pythonFormulaVersion(formula string) string {
	_, rest, ok := strings.Cut(formula, "@")
	if !ok {
		return ""
	}
	return strings.TrimSpace(rest)
}

// panelPythonInterpreter 返回某个 brew 前缀下该 formula 的解释器绝对路径。
//
// 为什么要拼而不是 `brew --prefix python@3.11`：安装器在**创建 venv 之前**
// 就要用这个路径，多跑一次 brew 只为了拿一个可推导的路径是浪费
// （而且 brew 命令在 root 身份下还要 sudo -u 包装，见 brewCommand）。
// 拼错的表现是 venv 建不出来 —— 会立刻失败，不会静默错。
func panelPythonInterpreter(brewPrefix, formula string) string {
	v := pythonFormulaVersion(formula)
	if v == "" {
		return ""
	}
	return filepath.Join(brewPrefix, "opt", formula, "bin", "python"+v)
}

// panelPythonSitePackages 返回某个 venv 里该 Python 版本的 site-packages 目录。
//
// macOS 的 venv 布局是 <venv>/lib/python<版本>/site-packages；版本号写错就会把
// sitecustomize.py 之类的补丁文件写到不存在的目录里（历史坑：IPv4 优先补丁
// 就是靠这个路径生效的，见 qwenIPv4Sitecustomize）。
func panelPythonSitePackages(venvDir, formula string) string {
	v := pythonFormulaVersion(formula)
	if v == "" {
		return ""
	}
	return filepath.Join(venvDir, "lib", "python"+v, "site-packages")
}

// InstalledBrewVersion 读某个 formula **当前真正装着的版本**（"3.11.16" 这样）。
//
// 存在的意义是"不许谎报"：面板能决定的只是 formula 名（python@3.11），
// 补丁版本由 Homebrew 决定，而且会随镜像同步情况变。任务日志里给出的必须是
// 磁盘上的事实，而不是我们写死的期望值 —— 用户排查"镜像上有没有这一版"时
// 第一眼要看的正是这个字符串。
func (m *Manager) InstalledBrewVersion(ctx context.Context, formula string) (string, error) {
	if strings.TrimSpace(formula) == "" {
		return "", fmt.Errorf("formula 为空")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := m.brewRun(ctx, 30*time.Second, "list", "--versions", formula)
	if err != nil {
		return "", err
	}
	// 输出形如 "python@3.11 3.11.16"（可能有多个版本，取最后一个 = 当前链接的）
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("`brew list --versions %s` 输出里没有版本号：%q", formula, strings.TrimSpace(out))
	}
	return fields[len(fields)-1], nil
}

// InstallPythonRuntime 安装一个"面板自建运行时"用的 Python 解释器。
//
// 与通用 brew 安装路径的区别：Python 是**命令行工具 + 库**，没有 brew service。
// 走通用路径会 `brew services start python@3.11`，得到一个"已安装，但启动失败"
// 的假警告，还会在服务管理里留下一条永远没有状态的假记录（ffmpeg 当年就是这么
// 被误报的，见 catalog.go 里 ffmpeg 条目的说明）。所以它只做三件事：
// 确保 brew → brewInstall（失败即换源）→ 如实报告装上的版本。
func (m *Manager) InstallPythonRuntime(ctx context.Context, formula string, result *InstallResult) error {
	if strings.TrimSpace(formula) == "" {
		return fmt.Errorf("没有指定要安装的 Python 版本")
	}
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		result.step(ctx, "未检测到 Homebrew：先在面板里装上它（含命令行开发者工具）")
		if err := m.EnsureHomebrew(ctx, result); err != nil {
			return fmt.Errorf("自动安装 Homebrew 失败：%w", err)
		}
	}

	if m.brewHas(ctx, formula) {
		result.step(ctx, formula+" 已经装好了，跳过安装")
	} else {
		result.step(ctx, "正在 brew install "+formula+"（失败会自动换源重试）")
		if _, err := m.brewInstall(ctx, result, 30*time.Minute, formula); err != nil {
			return fmt.Errorf("安装 %s 失败：%w", formula, err)
		}
	}

	// 真实版本（不谎报）：拿不到就如实说拿不到，不编一个。
	if v, err := m.InstalledBrewVersion(ctx, formula); err == nil {
		result.step(ctx, fmt.Sprintf("%s 已就绪：实际版本 %s", formula, v))
	} else {
		result.step(ctx, fmt.Sprintf("%s 已安装，但读不出实际版本（%v）—— 可在终端执行 `brew list --versions %s` 核对",
			formula, err, formula))
	}
	result.step(ctx, "解释器位置："+panelPythonInterpreter(m.brewPrefix(), formula))
	return nil
}

// pythonRuntimeDependents 找出"正在用这个 Python 版本的虚拟环境"。
//
// 为什么卸载前必须查：面板自研的两个服务各自建了 venv，venv 里的 python 是**指向
// brew 那个解释器**的符号链接（pyvenv.cfg 里还记着 executable 的真实路径）。
// 把解释器卸掉，venv 不会报"依赖缺失"，而是直接起不来 —— 用户看到的是
// "Qwen3 TTS 突然打不开了"，很难联想到几分钟前卸载的 Python。所以卸载计划与
// 卸载结果里都要点名这些依赖方（**只告知，不阻止** —— 用户有权卸自己机器上的东西）。
func (m *Manager) pythonRuntimeDependents(formula string) []string {
	if m.opt.UserHome == "" || formula == "" {
		return nil
	}
	candidates := []struct{ name, venv string }{
		{"Qwen3 TTS（~/tts/qwen3/.venv）", filepath.Join(m.opt.UserHome, "tts", "qwen3", ".venv")},
		{"IOPaint（~/iopaint/.venv）", filepath.Join(m.opt.UserHome, "iopaint", ".venv")},
	}
	var out []string
	for _, c := range candidates {
		cfg := filepath.Join(c.venv, "pyvenv.cfg")
		b, err := os.ReadFile(cfg)
		if err != nil {
			continue
		}
		text := string(b)
		if strings.Contains(text, "/"+formula+"/") || strings.Contains(text, "Cellar/"+formula+"/") {
			out = append(out, c.name)
		}
	}
	return out
}

// UninstallPythonRuntime 卸载一个面板上架的 Python 解释器。
//
// 诚实要求：卸载前把"谁还在用它"写进步骤与结果里；brew uninstall 失败（例如
// 别的 formula 依赖它）时**如实报错**，不许吞掉当成卸载成功。
//
// force=true = 用户在确认框里明确选了「强制卸载」（brew uninstall
// --ignore-dependencies）。默认**绝不**加这个开关：用户真机卸载
// python@3.13 时正是 brew 因为 llvm/rust 依赖它而拒绝，面板需要先把依赖方
// 讲清楚并让用户自己选（见 BrewDependencyBlock 与 brewUninstallErrText）。
func (m *Manager) UninstallPythonRuntime(ctx context.Context, app App, force bool, result *InstallResult) error {
	formula := app.BrewFormula
	if strings.TrimSpace(formula) == "" {
		return fmt.Errorf("「%s」没有声明 BrewFormula，无法卸载", app.Name)
	}
	dependents := m.pythonRuntimeDependents(formula)
	if len(dependents) > 0 && result != nil {
		result.step(ctx, "⚠️ 以下环境正在使用 "+formula+"："+strings.Join(dependents, "、")+
			"，卸载后它们会无法启动（需要重新部署）")
	}
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		// brew 本身不在：这时 brewHas 也会返回 false，但那是"探不到"而不是
		// "没装过" —— 两者必须分开说，否则用户会以为东西已经删干净了。
		return fmt.Errorf("找不到 Homebrew（%s），无法卸载 %s", m.opt.BrewBin, formula)
	}
	if !m.brewHas(ctx, formula) {
		if result != nil {
			result.step(ctx, formula+" 当前没有安装（可能已经卸过了）")
		}
		return nil
	}
	if err := m.brewUninstall(ctx, formula, force, result); err != nil {
		return err
	}
	if result != nil {
		result.step(ctx, formula+" 已卸载")
		if len(dependents) > 0 {
			result.step(ctx, "提醒："+strings.Join(dependents, "、")+" 现在起不来了，需要重新部署它们")
		}
	}
	return nil
}
