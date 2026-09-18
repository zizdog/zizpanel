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
//  图片压缩（libvips）的安装 / 卸载
//
//  这是一个**能力型应用**：装的是 libvips 的 `vips` 命令行，没有守护进程、
//  没有端口、没有网页界面（界面是面板自己的「文件管理 → 🖼️ 图片压缩」）。
//  所以它既不该走通用 brew 流程（`brew services start vips` 会去启动一个
//  根本不存在的 service 定义，留下"已安装但启动失败"的假警告），
//  也不需要 launchd —— 与 ffmpeg 完全同形。
//
//  为什么不用 govips（用户参考文档给的 CGO 绑定）：见 catalog.go 里这一条的注释。
//  一句话：面板是纯 Go 单二进制，运行期不能把 libvips 的动态库绑到自己身上。
// ============================================================================

// ImgCompressFormula 是图片压缩引擎的 Homebrew formula。
//
// 只在这里定义一次：安装、卸载、引擎探测（web 层）都引用它 ——
// 三处各写一遍 strings 必然漂移（本项目为这类漂移踩过坑）。
const ImgCompressFormula = "vips"

// ImgCompressBinName 是引擎的命令名。
const ImgCompressBinName = "vips"

// ImgCompressBin 返回这台机器上引擎二进制的路径（按 brew 前缀推导）。
func (m *Manager) ImgCompressBin() string {
	return filepath.Join(m.brewPrefix(), "bin", ImgCompressBinName)
}

// InstallImageCompressor 安装 libvips（幂等）。
//
// 装完**必须复核**：二进制存在 + `vips --version` 真的能报出版本。
// 只跑完 brew 就报成功是不够的（brew 退出码 0 而库缺依赖的情况真的存在），
// 用户点「图片压缩」时才发现不能用，那就是谎报。
func (m *Manager) InstallImageCompressor(ctx context.Context, app App, result *InstallResult) error {
	formula := app.BrewFormula
	if formula == "" {
		formula = ImgCompressFormula
	}
	if result != nil {
		result.App = app.ID
	}
	if m.brewHas(ctx, formula) {
		if result != nil {
			result.step(ctx, formula+" 已经装好了（Homebrew 里已有），跳过安装")
		}
	} else {
		if result != nil {
			result.step(ctx, "正在 brew install "+formula+"（原生 arm64 包，不需要 Docker/Node）")
		}
		// 必须走 brewInstall（**多源兜底**）而不是 brewRun：
		// brewRun 只用"当前镜像"这一个源，而镜像站经常缺某个瓶文件
		//（2026-09-18 实测：aliyun 镜像上 gcc-16.2.0.arm64_sequoia.bottle.1.tar.gz 是 404，
		//  而 vips 依赖 gcc 的 OpenMP 运行时 → 单源直接失败）。
		// brewInstall 会清掉坏缓存并依次换源，最后回落到官方源。
		if _, err := m.brewInstall(ctx, result, 30*time.Minute, formula); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", formula, err)
		}
	}
	bin := m.ImgCompressBin()
	ver, err := m.imgCompressVersion(ctx, bin)
	if err != nil {
		return err
	}
	if result != nil {
		result.step(ctx, "引擎已就绪："+bin+"（"+ver+"）")
		result.Steps = append(result.Steps,
			"用法：到「文件管理」选中目录 → 点工具条上的「🖼️ 图片压缩」"+
				"（可调质量 / 最长边 / 输出格式；默认另存为 xxx.min.<ext>，不动原文件）")
	}
	return nil
}

// UninstallImageCompressor 卸载引擎（brew uninstall vips）。
//
// 如实说明后果：卸载后「图片压缩」功能会不可用（那时点它会被明确挡住并指回这里），
// 但**不会**动用户任何一张图片 —— 压缩是就地读、输出到目标路径的操作，
// 没有"面板的数据目录"要清理。
func (m *Manager) UninstallImageCompressor(ctx context.Context, app App, result *InstallResult) error {
	formula := app.BrewFormula
	if formula == "" {
		formula = ImgCompressFormula
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
		return fmt.Errorf("卸载 %s 失败: %w", formula, err)
	}
	if result != nil {
		result.step(ctx, "已卸载 "+formula+
			"：「文件管理 → 图片压缩」会显示引擎不可用（图片本身没有任何改动）")
	}
	return nil
}

// imgCompressVersion 跑一次 `vips --version` 复核引擎真的能用。
func (m *Manager) imgCompressVersion(ctx context.Context, bin string) (string, error) {
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("安装似乎完成了，但 %s 不存在：%w"+
			"（这通常是 Homebrew 下载/链接失败，请重试或在终端跑 brew install %s）",
			bin, err, ImgCompressFormula)
	}
	// 直接执行（不需要降权：`vips --version` 只是打印版本，任何人可跑）。
	// 用 sudo -u 反而会把"用户不存在"这种测试/环境问题混进来。
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	outRaw, err := exec.CommandContext(cctx, bin, "--version").CombinedOutput()
	out := string(outRaw)
	if err != nil {
		return "", fmt.Errorf("引擎装好了但跑不起来（%s --version 失败）：%v\n%s",
			bin, err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}
