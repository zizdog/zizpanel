package services

import (
	"context"
	"fmt"
	"os"
)

// ============================================================================
//  基础环境（Base Environment）= 运行依赖层
//
//  与「网站环境（LNMP）」的分工（产品负责人 2026-09-19 拍板）：
//
//    · 运行依赖层（本文件）：命令行开发者工具(CLT) → Homebrew → ffmpeg。
//      python3 随 CLT 一起来，不单独列（避免两套真相，见 basedep.go 的说明）。
//      **所有** brew 类应用都需要它，跟网站无关。
//    · 网站环境层（lnmp.go / catalog.go）：nginx + PHP + MySQL + phpMyAdmin，
//      只有「网站管理 / 数据库 / 一键建站」需要。
//
//  为什么要拆：首页横幅那颗按钮过去叫「一键 LNMP」，实际却把两层一起装了 ——
//  只想装个 ffmpeg 的用户被顺带装上一个 Web 服务器，而"到底缺哪一层"也说不清。
//  现在探测（BaseEnvStatus）与安装（EnsureBaseEnvironment）都只覆盖运行依赖层，
//  网站环境仍走 POST /api/v1/market/install-lnmp（InstallLNMP）。
//
//  探测口径以**现实**为准，绝不读面板数据库/服务记录：
//    · CLT      —— 现成的 cltInstalled（xcode-select -p + stat，识破"装过又删掉"）；
//    · Homebrew —— <brew 前缀>/bin/brew 是否存在（Apple Silicon /opt/homebrew、
//                  Intel /usr/local，见 brewBinCandidates / reconcileBrewBin）；
//    · ffmpeg   —— 与 EnsureBaseDependencies 同源的 BaseDependencyStatuses。
// ============================================================================

// BaseEnvStatus 是「运行依赖层」此刻的状态（GET /api/v1/system/base-env 的 data）。
//
// 字段名是**与前端约定的契约**，不要改名。
type BaseEnvStatus struct {
	// CLTOK 表示命令行开发者工具可用。
	CLTOK bool `json:"clt_ok"`
	// BrewOK 表示 Homebrew 已安装（<前缀>/bin/brew 存在）。
	BrewOK bool `json:"brew_ok"`
	// DepsOK 表示基础依赖（ffmpeg / ffprobe）全部就绪。
	DepsOK bool `json:"deps_ok"`
	// Ready 三者皆真才为真。
	Ready bool `json:"ready"`
	// Missing 是缺失项的人类可读名（CLT → Homebrew → ffmpeg/ffprobe 的顺序）。
	Missing []string `json:"missing"`
}

// BaseEnvStatus 只读探测「运行依赖层」缺什么。不装任何东西。
func (m *Manager) BaseEnvStatus(ctx context.Context) BaseEnvStatus {
	st := BaseEnvStatus{Missing: []string{}}

	st.CLTOK = m.cltReady(ctx)
	if !st.CLTOK {
		st.Missing = append(st.Missing, "命令行开发者工具")
	}

	st.BrewOK = m.brewInstalled()
	if !st.BrewOK {
		st.Missing = append(st.Missing, "Homebrew")
	}

	// ffmpeg / ffprobe 逐项如实列出（只报一句"缺依赖"用户没法判断缺的是哪个）。
	st.DepsOK = true
	for _, dep := range m.BaseDependencyStatuses(ctx) {
		if dep.Satisfied {
			continue
		}
		st.DepsOK = false
		st.Missing = append(st.Missing, dep.Command)
	}

	st.Ready = st.CLTOK && st.BrewOK && st.DepsOK
	return st
}

// cltReady 是 BaseEnvStatus 用的 CLT 探测。
//
// 单独包一层只为可注入：cltInstalled 会执行 /usr/bin/xcode-select -p，
// 而"这台机器装没装 CLT"不该决定单测结论（开发机装了、CI 没装）。
func (m *Manager) cltReady(ctx context.Context) bool {
	if m.baseEnvCLTProbe != nil {
		return m.baseEnvCLTProbe(ctx)
	}
	return m.cltInstalled(ctx)
}

// brewInstalled 报告 Homebrew 此刻是否真的装了 —— 只看 brew 可执行文件在不在。
//
// 先 reconcileBrewBin 一次：配置里可能写着另一个架构的路径（Apple Silicon 上
// 却是 /usr/local/bin/brew，2026-09-17 真机事故），口径必须与 EnsureHomebrew 一致，
// 否则会出现"安装任务说装好了、只读接口说没装"这种自相矛盾。
func (m *Manager) brewInstalled() bool {
	m.reconcileBrewBin()
	if p := m.opt.BrewBin; p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return true
		}
	}
	// reconcileBrewBin 在"一个候选都没有"时会保留原值，所以这里再按候选列表兜一次：
	// 只要标准前缀里真的有 brew，就如实报"装了"。
	for _, c := range brewBinCandidates() {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// EnsureBaseEnvironment 幂等地补齐「运行依赖层」：Homebrew → ffmpeg。
//
// 刻意**只做这两步**：nginx / PHP / MySQL 属于「网站环境」（EnsureLNMP），
// 这个任务绝不碰它们 —— 用户装 ffmpeg 不该被顺带装上一个 Web 服务器。
// CLT 是 Homebrew 的前置依赖，由 EnsureHomebrew 在需要时调用 EnsureCLT 处理。
//
// 失败一律如实返回错误（与 EnsureBaseDependencies 同一条纪律）：
// "能谎报成功的功能，比没做更糟"（AGENTS.md 铁律 11）。
func (m *Manager) EnsureBaseEnvironment(ctx context.Context, result *InstallResult) error {
	if result == nil {
		result = &InstallResult{Steps: []string{}}
	}
	if err := m.EnsureHomebrew(ctx, result); err != nil {
		return fmt.Errorf("基础环境安装中断在 Homebrew 这一步：%w", err)
	}
	if err := m.EnsureBaseDependencies(ctx, result); err != nil {
		return fmt.Errorf("基础环境安装中断在 ffmpeg 这一步：%w", err)
	}
	result.step(ctx, "基础环境（运行依赖层）已就绪")
	return nil
}
