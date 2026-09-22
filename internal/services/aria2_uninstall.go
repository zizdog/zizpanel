package services

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// UninstallAria2 卸载 aria2：停服务 + 撤 plist + 面板记录 + brew uninstall；
// removeData 决定要不要连 ~/aria（配置与会话文件）一起删。
//
// ⚠️ **无论 removeData 是什么，都不碰 ~/Downloads**（用户下载的文件）。
// 这条写进卸载计划与确认框里，代码里也绝不出现那个路径的删除动作。
func (m *Manager) UninstallAria2(ctx context.Context, app App, removeData bool, r *InstallResult) error {
	if r == nil {
		r = &InstallResult{App: app.ID}
	}
	p := m.aria2Paths()

	// ① 停服务、撤 plist、删面板记录（幂等：本来就没注册也要走完）。
	if p.Plist != "" {
		if err := aria2Stop(m, ctx, Aria2Label, p.Plist); err != nil {
			// 已经不在 launchd 里不算失败，但要说清；停不掉才必须让用户看见。
			r.step(ctx, "停止服务 "+Aria2Label+" 时遇到问题（可能本来就没在跑）："+err.Error())
		} else {
			r.step(ctx, "已停止并移除服务 "+Aria2Label)
		}
	}

	// ② brew 包总是卸（引擎留着没用；重装很快）。
	if m.brewHas(ctx, Aria2Formula) {
		if err := m.brewUninstall(ctx, Aria2Formula, true, r); err != nil {
			return fmt.Errorf("brew uninstall %s 失败：%w\n%s", Aria2Formula, err, m.aria2LogExcerpt())
		}
	} else {
		r.step(ctx, "Homebrew 里没有 "+Aria2Formula+"（跳过）")
	}

	// ③ 数据：默认保留（密钥与会话都在里面），勾了才删。下载目录永不动。
	if removeData {
		for _, path := range []string{p.Conf, p.Session} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("删除 %s 失败：%w", path, err)
			}
		}
		// 目录清了才算干净；里面只剩这两样东西，但不要 RemoveAll 整个 Root 之外的东西。
		_ = os.Remove(p.Root)
		r.step(ctx, "已删除配置与会话文件（"+p.Root+"）")
	} else {
		r.step(ctx, "保留配置与会话文件 "+p.Root+"（重装后 RPC 密钥不变、队列还能续上）")
	}
	r.step(ctx, "下载目录 "+p.DownloadDir+" 未做任何改动")

	// ④ 归属：root 建的目录里如果留下文件，交还给用户（卸载后用户可能还想看）。
	if m.opt.UserName != "" && dirExists(p.Root) {
		_ = chownTree(m.opt.UserName, p.Root)
	}
	r.Message = fmt.Sprintf("「%s」已卸载（下载目录里的文件保留）", app.Name)
	if strings.TrimSpace(app.Name) == "" {
		r.Message = "aria2 已卸载（下载目录里的文件保留）"
	}
	return nil
}

// aria2LogExcerpt 给卸载失败信息附上日志尾部（没有日志就如实说没有）。
func (m *Manager) aria2LogExcerpt() string {
	p := m.aria2Paths().ErrLog
	b, err := os.ReadFile(p)
	if err != nil {
		return "（没有日志：" + p + "）"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n := len(lines); n > 8 {
		lines = lines[n-8:]
	}
	return "日志尾部 " + p + "：\n" + strings.Join(lines, "\n")
}
