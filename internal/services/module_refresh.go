package services

// 升级只更新携带位（<面板BinDir>/<name>），模块实际跑的是安装位 /opt/<name>/bin/<name>；
// 不刷新就是"升级了但新功能没生效"（坑 216）。判据贴运行体，失败一律回滚并如实报告，
// 模块数据目录（DB/config/封面/媒体）永不触碰。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// 模块刷新结果状态。后两个都表示失败：区别是二进制有没有回到旧版。
const (
	ModuleRefreshed  = "refreshed"   // 已替换并重启
	ModuleUpToDate   = "up-to-date"  // 携带位与安装位 sha256 相同，零动作
	ModuleSkipped    = "skipped"     // 未安装 / 发布包未携带
	ModuleRolledBack = "rolled-back" // 失败，已回滚旧二进制
	ModuleFailed     = "failed"      // 失败，且回滚没成功
)

// ModuleRefreshResult 是一条模块刷新的如实结果（供升级流程展示与测试断言）。
type ModuleRefreshResult struct {
	Name    string
	Status  string
	Reason  string
	FromSHA string
	ToSHA   string
}

// moduleSelfCheckRun 跑模块自己的版本自检；root 身份即可，不依赖真实用户名。
// 抽成变量：单测用假模块脚本注入，绝不真跑系统上的模块。
var moduleSelfCheckRun = func(m *Manager, ctx context.Context, bin string, args ...string) (string, error) {
	out, err := m.runRoot(ctx, 30*time.Second, bin, args...)
	if err != nil {
		return out, fmt.Errorf("执行 %s %s 失败：%w（%s）",
			bin, strings.Join(args, " "), err, tailText(strings.TrimSpace(out), 200))
	}
	return out, nil
}

// moduleSpec 描述一个"面板托管模块"的刷新位置与动作来源。
type moduleSpec struct {
	name      string
	label     string
	bundled   string // 携带位：随面板包分发的二进制（空/不存在时用 fetch 按需取件）
	installed string // 安装位：模块实际运行的二进制
	plist     string // launchd 作业定义（"装没装"的运行体判据之一）
	selfArgs  []string
	launch    func(m *Manager, ctx context.Context, label, plist string) error
	// wantVersion 是这一版面板期望的模块版本（装到位后自检输出里应含它）。
	wantVersion string
	// fetch 是"不随包分发的模块"的按需取件（从镜像站下载并核 sha256）。
	// 网络不可达时**如实失败**，绝不谎报"已是最新"。
	fetch func(m *Manager, ctx context.Context, result *InstallResult) (string, error)
}

// defaultZizvideoModuleFetch 是"不随包分发的 zizvideo"的真实取件口；
// zizvideoModuleFetch 可被单测替换（单测绝不联网），替换后用它恢复。
var defaultZizvideoModuleFetch = func(m *Manager, ctx context.Context, result *InstallResult) (string, error) {
	return m.downloadZizvideoBinary(ctx, result)
}

var zizvideoModuleFetch = defaultZizvideoModuleFetch

// managedModuleSpecs 返回要刷新的模块清单。
func (m *Manager) managedModuleSpecs(panelBinDir string) []moduleSpec {
	zp := m.ZizvideoPathsFor()
	return []moduleSpec{
		{
			name:        ZizvideoAppID,
			label:       ZizvideoLabel,
			bundled:     filepath.Join(panelBinDir, ZizvideoAppID),
			installed:   zp.Bin,
			plist:       zp.Plist,
			selfArgs:    []string{"--version"},
			wantVersion: ZizvideoVersion,
			launch:      zizvideoLaunch,
			fetch: func(m *Manager, ctx context.Context, result *InstallResult) (string, error) {
				return zizvideoModuleFetch(m, ctx, result)
			},
		},
	}
}

// RefreshInstalledModules 是升级流程调用的入口：逐个刷新面板托管模块的已安装二进制。
func RefreshInstalledModules(ctx context.Context, panelBinDir string) []ModuleRefreshResult {
	m := NewManager(nil, Options{})
	return m.RefreshInstalledModules(ctx, panelBinDir)
}

// RefreshInstalledModules 逐个刷新已安装模块；单模块失败不回滚别的模块，也不 panic。
func (m *Manager) RefreshInstalledModules(ctx context.Context, panelBinDir string) []ModuleRefreshResult {
	var out []ModuleRefreshResult
	for _, spec := range m.managedModuleSpecs(panelBinDir) {
		out = append(out, m.refreshModuleBinary(ctx, spec))
	}
	return out
}

// refreshModuleBinary 刷新一个模块：判据贴运行体，失败即回滚，绝不谎报。
func (m *Manager) refreshModuleBinary(ctx context.Context, spec moduleSpec) ModuleRefreshResult {
	r := ModuleRefreshResult{Name: spec.name}

	// ① 只处理真的装了的：安装位二进制 + 它的 launchd 作业都得在，缺一即未安装。
	if !fileExecutable(spec.installed) || strings.TrimSpace(spec.plist) == "" || !fileExists(spec.plist) {
		r.Status = ModuleSkipped
		r.Reason = "未安装 " + spec.name + "（安装位二进制或守护进程不存在）"
		return r
	}
	// ② 已装的就是这一版期望的版本 ⇒ 零动作（不下载、不替换、不重启）。
	//
	// 版本优先于 sha：机器上可能残留着旧发布包放下的携带位，光比 sha 会把
	// "已装的是旧构建、携带位也是旧的" 判成最新（坑 216 的翻版）。
	if spec.wantVersion != "" {
		if out, err := moduleSelfCheckRun(m, ctx, spec.installed, spec.selfArgs...); err == nil &&
			strings.Contains(out, spec.wantVersion) {
			r.Status = ModuleUpToDate
			r.Reason = "已安装的 " + spec.name + " 就是 " + spec.wantVersion + "，未做任何改动"
			return r
		}
	}
	// ③ 需要刷新：优先用随包携带位；不随包分发的模块从镜像站按需取件（取不到如实失败）。
	bundled := spec.bundled
	if !fileExecutable(bundled) {
		if spec.fetch == nil {
			r.Status = ModuleSkipped
			r.Reason = "发布包未携带 " + spec.name + " 二进制（" + bundled + "），它也不支持按需取件"
			return r
		}
		src, err := spec.fetch(m, ctx, nil)
		if err != nil {
			r.Status = ModuleFailed
			r.Reason = "按需取件失败，未改动已安装的 " + spec.name + "：" + err.Error()
			return r
		}
		bundled = src
	}
	from, err := fileSHA256(spec.installed)
	if err != nil {
		r.Status = ModuleFailed
		r.Reason = "读取安装位二进制失败：" + err.Error()
		return r
	}
	to, err := fileSHA256(bundled)
	if err != nil {
		r.Status = ModuleFailed
		r.Reason = "读取携带位二进制失败：" + err.Error()
		return r
	}
	r.FromSHA, r.ToSHA = from, to
	// ③ sha256 相同 ⇒ 零动作（幂等，不重启）。
	if from == to {
		r.Status = ModuleUpToDate
		r.Reason = "携带位与安装位 sha256 相同，未做任何改动"
		return r
	}

	// ④ 备份旧二进制（可回滚）→ 同目录临时文件 + rename 原子替换。
	bak := spec.installed + ".bak"
	if err := installFileExecutable(spec.installed, bak); err != nil {
		r.Status = ModuleFailed
		r.Reason = "备份旧二进制失败，未替换：" + err.Error()
		return r
	}
	if err := installFileExecutable(bundled, spec.installed); err != nil {
		r.Status = ModuleRolledBack
		r.Reason = "原子替换失败：" + err.Error()
		if rbErr := installFileExecutable(bak, spec.installed); rbErr != nil {
			r.Status = ModuleFailed
			r.Reason += "；回滚也失败：" + rbErr.Error()
		}
		return r
	}

	// ⑤ 自检：跑模块自己的版本命令；跑不起来、没有输出、或版本不是这一版期望的，都算失败。
	ver, err := moduleSelfCheckRun(m, ctx, spec.installed, spec.selfArgs...)
	if err == nil && spec.wantVersion != "" && !strings.Contains(ver, spec.wantVersion) {
		err = fmt.Errorf("自检报的版本不是 %s（%s）", spec.wantVersion, tailText(strings.TrimSpace(ver), 120))
		ver = ""
	}
	if err != nil || strings.TrimSpace(ver) == "" {
		why := "自检未通过"
		if err != nil {
			why += "：" + err.Error()
		} else {
			why += "：版本命令没有任何输出"
		}
		r.Reason = why
		if rbErr := installFileExecutable(bak, spec.installed); rbErr != nil {
			r.Status = ModuleFailed
			r.Reason = why + "；回滚失败：" + rbErr.Error()
		} else {
			r.Status = ModuleRolledBack
			r.Reason = why + "；已回滚旧二进制"
		}
		return r
	}

	// ⑥ 通过后重启守护进程（走现有的 bootstrap 路径，不手搓 launchctl）。
	if err := spec.launch(m, ctx, spec.label, spec.plist); err != nil {
		r.Reason = "重启守护进程失败：" + err.Error()
		if rbErr := installFileExecutable(bak, spec.installed); rbErr != nil {
			r.Status = ModuleFailed
			r.Reason += "；回滚失败：" + rbErr.Error()
			return r
		}
		r.Status = ModuleRolledBack
		// 旧二进制已就位，再拉一次让它回到可用状态（失败也如实说，不谎报恢复）。
		if relErr := spec.launch(m, ctx, spec.label, spec.plist); relErr != nil {
			r.Reason += "；回滚后旧版守护进程也未能启动：" + relErr.Error()
		} else {
			r.Reason += "；已回滚旧二进制并重启旧版"
		}
		return r
	}
	r.Status = ModuleRefreshed
	r.Reason = "已替换并重启，自检：" + strings.TrimSpace(ver)
	return r
}
