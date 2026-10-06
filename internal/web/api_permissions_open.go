package web

// api_permissions_open.go —— 「打开系统设置」：像别的软件那样，把用户要点的那一页直接打开。
//
// 🚨 为什么需要它（2026-10-06 真机实测，坑 242）：macOS 对「完全磁盘访问权限」**不弹窗** ——
// 进程去读受保护的东西时系统静默拒绝，只把应用加进「系统设置 → 隐私与安全性 → 完全磁盘访问权限」
// 列表、开关默认关。用户唯一能做的是**手动打开那个开关**。只给一行"请到 系统设置 → … → …"
// 的路径，等于让人自己翻三层菜单；别的软件的做法是给一个按钮直接把那一页弹出来。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/permissions"
)

// macOS 系统设置的深链（Privacy_AllFiles = 完全磁盘访问权限那一页）。
const settingsURLFullDisk = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"

// settingsURLForItem 返回这一项对应的系统设置页；没有对应页面的返回空串（前端就不显示按钮）。
//
// 只有"确实有一页可以点"的项才给按钮：可移除宗卷在 macOS 里**没有独立列项**，
// 给它一个按钮只会打开一个找不到目标的页面（那才是骗人）。
func settingsURLForItem(id string) string {
	if strings.TrimSpace(id) == permissions.ItemFullDisk {
		return settingsURLFullDisk
	}
	return ""
}

// permOpenSettingsFn 是"打开设置页"的注入点：单测绝不许真去开窗口。
var permOpenSettingsFn = openSettingsForConsoleUser

// openSettingsForConsoleUser 以**控制台登录用户**的身份打开一个 URL。
//
// 为什么不能直接在面板进程里 open：面板是 root LaunchDaemon，不在用户的 Aqua 图会话里，
// 直接 open 要么报错、要么窗口根本不出现。root 时用 `launchctl asuser <uid> open <url>`
// 把命令送进该用户的图形会话（macOS 上这是标准做法）；调试实例（非 root，前台跑）直接 open。
func openSettingsForConsoleUser(ctx context.Context, userName, rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return errors.New("没有可打开的系统设置页")
	}
	u, err := user.Lookup(strings.TrimSpace(userName))
	if err != nil {
		return fmt.Errorf("找不到控制台用户 %s：%w", userName, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("控制台用户的 uid 读不出来：%w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if os.Geteuid() == 0 {
		return exec.CommandContext(cctx, "/bin/launchctl", "asuser", strconv.Itoa(uid),
			"/usr/bin/open", rawURL).Run()
	}
	return exec.CommandContext(cctx, "/usr/bin/open", rawURL).Run()
}

// handlePermissionOpenSettings 打开某一项对应的系统设置页（仅管理员，且在人在机器前时）。
//
// 预检与「申请」同一套：没人在图形会话里就拒绝 —— 开了也没人看见，等于什么都没做。
func (s *Server) handlePermissionOpenSettings(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	consoleUser := strings.TrimSpace(permConsoleUserFn())
	if !permissions.ConsoleOK(consoleUser) {
		s.audit(r, "permission_open_settings", id, "拒绝：没人在机器前（reason="+permReasonNoConsole+"）", false, "")
		failPermission(w, http.StatusConflict, permNoConsoleReason, permReasonNoConsole)
		return
	}
	url := settingsURLForItem(id)
	if url == "" {
		failPermission(w, http.StatusNotFound, "这一项没有可打开的系统设置页，请按「手动授权路径」操作", "no_settings_page")
		return
	}
	if err := permOpenSettingsFn(r.Context(), consoleUser, url); err != nil {
		s.audit(r, "permission_open_settings", id, "打开系统设置失败："+err.Error(), false, "")
		failPermission(w, http.StatusInternalServerError, "打不开系统设置（可手动打开）："+err.Error(), "open_failed")
		return
	}
	// 审计里记 URL 与动作，不记任何凭据。
	s.audit(r, "permission_open_settings", id, "已打开系统设置页："+url, true, "")
	ok(w, map[string]any{
		"opened": true,
		"url":    url,
		"hint":   "已在屏幕上打开「完全磁盘访问权限」：打开 zizpanel 的开关，然后回到这里点「刷新」",
	})
}
