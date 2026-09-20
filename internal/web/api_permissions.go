// api_permissions.go —— 「权限」页：逐项申请 macOS 授权。
//
// 纪律（坑 191）：GET 只用**不碰受保护路径**的判据（控制台用户 / 是否远程 /
// 上次申请的真实结果），一个字节都不读；读一次只发生在用户点「申请」并确认之后，
// 且固定顺序预检：没人在机器前 → 缺 confirm → 才执行。
//
// 「外部应用条件授权入口」是**注册表驱动**（internal/permissions/registry.go）：
// zizvideo 只是其中一条（已关闭）。加一个独立软件 = 加一条，不改这里的判定逻辑。
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/permissions"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// 同步预检的拒绝理由（用户可见文案）。
const (
	permNoConsoleReason = "现在没人在机器前，弹窗没人点；请到真机操作，或按下面的路径手动授权"
	permConfirmReason   = "请先确认：接下来需要你在这台机器的屏幕上点『允许』"
)

// 机器可读的拒绝原因（响应 reason 字段），测试按它断言，不靠中文文案。
const (
	permReasonNoConsole       = "no_console"
	permReasonConfirmRequired = "confirm_required"
	permReasonUnknownItem     = "unknown_item"
	permReasonNotInstalled    = "not_installed"
	permReasonPathRequired    = "path_required"
	permReasonDisabled        = "disabled"
)

// 单测注入点：默认实现全部贴着真机，测试逐项换成假实现（绝不碰真实受保护路径）。
var (
	permConsoleUserFn  = files.ConsoleUser
	permVolumeMountsFn = files.NonSystemVolumeMounts
	permUserHomeFn     = func(name string) (string, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		return u.HomeDir, nil
	}
	permProbeFn permissions.Probe = permissions.ReadProtected
	// permRegistryFn 返回本机要显示入口的外部应用（默认只有启用条目）。
	permRegistryFn = permissions.EnabledRegistry
	// permLookupAppFn 按 id 查注册表（不过滤 Enabled：apply 要能分辨"已关闭"与"未知项"）。
	permLookupAppFn = permissions.LookupExternalApp
	// permExternalDetectFn 是便宜探测（回环健康 + 端口反查），不碰受保护路径。
	permExternalDetectFn = func(ctx context.Context, spec permissions.ExternalApp) permissions.DetectedApp {
		return permissions.DetectApp(ctx, permissions.DefaultEnv(), spec)
	}
	// permExternalCheckFn 以真实用户身份调用应用自己的 CLI；面板不读那个目录。
	permExternalCheckFn = func(ctx context.Context, d permissions.DetectedApp, path string) (permissions.CheckResult, error) {
		return permissions.CheckAppAccess(ctx, permissions.DefaultEnv(), d.Owner, d.ExecPath, d.Spec.CheckVerb, path)
	}
)

// permHistoryPath 是历史落盘位置（挂在 DataDir 下，与一次性标记同级）。
func permHistoryPath(dataDir string) string {
	return filepath.Join(filepath.Clean(dataDir), "permissions-history.json")
}

// permClientIsRemote：面板请求不是从本机发出的，就是"你在远程操作"。
func permClientIsRemote(s *Server, r *http.Request) bool {
	ip := strings.TrimSpace(s.clientIP(r))
	if ip == "" {
		return true
	}
	if ip == "localhost" {
		return false
	}
	parsed := net.ParseIP(ip)
	return parsed == nil || !parsed.IsLoopback()
}

// permItemTitle 是任务标题用的展示名（面板自己的两项 + 注册表里的外部应用）。
func permItemTitle(id string) string {
	switch id {
	case permissions.ItemFullDisk:
		return "完全磁盘访问权限"
	case permissions.ItemRemovable:
		return "可移除宗卷"
	}
	if spec, ok := permLookupAppFn(id); ok {
		if spec.Name != "" {
			return spec.Name
		}
		return spec.Title
	}
	return id
}

// permItemStatus 只按控制台会话 + 上次申请的真实结果给状态；读不到就 unknown。
func permItemStatus(h *permissions.History, id, consoleUser string) (status, hint, at, result string) {
	if !permissions.ConsoleOK(consoleUser) {
		return permissions.StatusNeedsConsole, "需要你在机器前点一次才能确认", "", ""
	}
	if e, ok := h.Last(id); ok && e.Status != "" {
		return e.Status, "", e.At.Format(time.RFC3339), e.Result
	}
	return permissions.StatusUnknown, "需要你在机器前点一次才能确认", "", ""
}

// permissionItems 组装「权限」页的条目。
//
// 外部应用条目：**未安装 ⇒ 整块不出现**（前端因此不渲染灰色占位）；已安装才带
// 版本/可执行文件/签名身份。关闭的条目默认不在注册表启用列表里。
func (s *Server) permissionItems(ctx context.Context, r *http.Request, consoleUser string) []permissions.Item {
	hasConsole := permissions.ConsoleOK(consoleUser)
	remote := permClientIsRemote(s, r)
	h := permissions.HistoryFor(permHistoryPath(s.Cfg.DataDir))

	home := ""
	if hasConsole {
		if hm, err := permUserHomeFn(consoleUser); err == nil {
			home = hm
		}
	}

	items := make([]permissions.Item, 0, 3)

	st, hint, at, res := permItemStatus(h, permissions.ItemFullDisk, consoleUser)
	items = append(items, permissions.Item{
		ID: permissions.ItemFullDisk, Title: "完全磁盘访问权限",
		Why:    "让面板能读桌面、文稿、下载等受保护目录",
		Status: st, StatusHint: hint, ConsoleUser: consoleUser, IsRemote: remote,
		CanApply: hasConsole, LastCheckedAt: at, LastResult: res,
		Targets:    permissions.ProtectedDirCandidates(home),
		ManualPath: diskVolumeAuthManualPath(),
	})

	st, hint, at, res = permItemStatus(h, permissions.ItemRemovable, consoleUser)
	mounts := permVolumeMountsFn()
	items = append(items, permissions.Item{
		ID: permissions.ItemRemovable, Title: "可移除宗卷",
		Why:    "让面板能读 /Volumes 下的外接盘",
		Status: st, StatusHint: hint, ConsoleUser: consoleUser, IsRemote: remote,
		CanApply: hasConsole, LastCheckedAt: at, LastResult: res,
		Targets:    mounts,
		ManualPath: diskVolumeAuthManualPath(),
	})

	for _, spec := range permRegistryFn() {
		d := permExternalDetectFn(ctx, spec)
		if !d.Installed {
			continue
		}
		st, hint, at, res = permItemStatus(h, spec.ID, consoleUser)
		items = append(items, permissions.Item{
			ID: spec.ID, Title: spec.Title, Why: spec.Why,
			Status: st, StatusHint: hint, ConsoleUser: consoleUser, IsRemote: remote,
			CanApply: hasConsole, LastCheckedAt: at, LastResult: res,
			Targets: d.Roots, AcceptsPath: true,
			Version: d.Version, ExecPath: d.ExecPath, SigningID: d.SigningID,
			Port: d.Port, Roots: d.Roots,
			ManualPath: permExternalManualPath(spec, d),
		})
	}
	return items
}

// permExternalManualPath 失败时点名"给哪个二进制 + 签名身份"。
func permExternalManualPath(spec permissions.ExternalApp, d permissions.DetectedApp) string {
	bin := d.ExecPath
	if bin == "" {
		bin = spec.InstallPath
	}
	return "手动授权：系统设置 → 隐私与安全性 → 完全磁盘访问权限 / 可移除宗卷 → 打开 " + bin +
		"（签名身份 " + spec.SigningID + "）"
}

// handlePermissionsList 只读列表：不读受保护路径、不建任务、不弹窗。
func (s *Server) handlePermissionsList(w http.ResponseWriter, r *http.Request) {
	consoleUser := strings.TrimSpace(permConsoleUserFn())
	ok(w, map[string]any{
		"console_user":        consoleUser,
		"has_console_session": permissions.ConsoleOK(consoleUser),
		"is_remote":           permClientIsRemote(s, r),
		"items":               s.permissionItems(r.Context(), r, consoleUser),
	})
}

// permErrView 是拒绝响应的正文：msg 给人看，reason 给机器断言。
type permErrView struct {
	OK     bool   `json:"ok"`
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
}

func failPermission(w http.ResponseWriter, code int, msg, reason string) {
	writeJSON(w, code, permErrView{Msg: msg, Reason: reason})
}

// handlePermissionApply 是「申请」入口：**同步预检 → 任务中心**。
//
// 预检顺序固定：① 没人在机器前 → 4xx no_console；② 缺 confirm:true → 400
// confirm_required；③ 才执行。前两步**不建任务、一个字节都不读**。
func (s *Server) handlePermissionApply(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req struct {
		Confirm bool   `json:"confirm"`
		Path    string `json:"path"`
	}
	// 空 body 合法：等于「没确认」，交给预检第②条给 confirm_required。
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	consoleUser := strings.TrimSpace(permConsoleUserFn())
	if !permissions.ConsoleOK(consoleUser) {
		s.audit(r, "permission_apply", id, "拒绝：没人在机器前（reason="+permReasonNoConsole+"）", false, "")
		failPermission(w, http.StatusConflict, permNoConsoleReason, permReasonNoConsole)
		return
	}
	if !req.Confirm {
		s.audit(r, "permission_apply", id, "拒绝：缺显式确认（reason="+permReasonConfirmRequired+"）", false, "")
		failPermission(w, http.StatusBadRequest, permConfirmReason, permReasonConfirmRequired)
		return
	}

	if id != permissions.ItemFullDisk && id != permissions.ItemRemovable {
		spec, found := permLookupAppFn(id)
		if !found {
			s.audit(r, "permission_apply", id, "拒绝：没有这一项权限", false, "")
			failPermission(w, http.StatusNotFound, "没有这一项权限", permReasonUnknownItem)
			return
		}
		if !spec.Enabled {
			// 机制与代码全留，但本机已不再提供这个入口（如 zizvideo 改为面板托管）。
			s.audit(r, "permission_apply", id, "拒绝：该外部应用条目已关闭（reason="+permReasonDisabled+"）", false, "")
			failPermission(w, http.StatusNotFound, spec.DisabledNote, permReasonDisabled)
			return
		}
		d := permExternalDetectFn(r.Context(), spec)
		if !d.Installed {
			s.audit(r, "permission_apply", id, "拒绝："+spec.Name+" 未安装", false, "")
			failPermission(w, http.StatusNotFound, "没有检测到运行中的 "+spec.Name, permReasonNotInstalled)
			return
		}
		// 目录取自该实例的允许根；一个都拿不到才要求用户填（面板不猜目录）。
		if strings.TrimSpace(req.Path) == "" && len(d.Roots) == 0 {
			s.audit(r, "permission_apply", id, "拒绝：没有指定要申请的目标目录", false, "")
			failPermission(w, http.StatusBadRequest, "请选择或填写要申请的目标目录（面板不猜目录）", permReasonPathRequired)
			return
		}
	}

	title := "申请权限：" + permItemTitle(id)
	s.launchTask(w, r, "permission_apply", "permission-"+id, title, "permission_apply",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.runPermissionApply(ctx, log, id, req.Path)
		})
}

// runPermissionApply 是任务体：再预检一次（排队期间人可能走了）→ 才读一次。
func (s *Server) runPermissionApply(ctx context.Context, log tasks.LogFunc, id, reqPath string) (any, error) {
	consoleUser := strings.TrimSpace(permConsoleUserFn())
	if !permissions.ConsoleOK(consoleUser) {
		return nil, errors.New(permNoConsoleReason)
	}
	switch id {
	case permissions.ItemFullDisk, permissions.ItemRemovable:
		return s.runPermissionPanelRead(ctx, log, id, consoleUser)
	}
	spec, found := permLookupAppFn(id)
	if !found || !spec.Enabled {
		return nil, errors.New("没有这一项权限")
	}
	return s.runPermissionExternal(ctx, log, spec, reqPath)
}

// runPermissionPanelRead 用**面板自己的身份**读一次受保护路径（这一步才弹窗）。
func (s *Server) runPermissionPanelRead(ctx context.Context, log tasks.LogFunc, id, consoleUser string) (any, error) {
	var targets []string
	switch id {
	case permissions.ItemFullDisk:
		home := ""
		if hm, err := permUserHomeFn(consoleUser); err == nil {
			home = hm
		}
		targets = permissions.ProtectedDirCandidates(home)
	case permissions.ItemRemovable:
		targets = permVolumeMountsFn()
	}
	if len(targets) == 0 {
		return nil, errors.New("当前没有可申请的目标（完全磁盘访问：找不到受保护目录；可移除宗卷：没接外接盘）")
	}

	log(tasks.LevelStep, "已向系统发起授权请求，请在这台机器的屏幕上点『允许』")
	results := permProbeFn(ctx, targets)

	readable, denied := 0, 0
	lines := make([]string, 0, len(results))
	for i, pr := range results {
		// 标签用通用名（桌面/文稿/下载/可移除宗卷 N）：Steps 会进审计，路径片段不进日志。
		label := permTargetLabel(id, i)
		switch {
		case pr.Readable:
			readable++
			lines = append(lines, label+"：已可访问")
			log(tasks.LevelOK, label+"：已可访问")
		case pr.Denied:
			denied++
			lines = append(lines, label+"：仍被拒")
			log(tasks.LevelErr, label+"：仍被拒")
		default:
			lines = append(lines, label+"：未读到")
			log(tasks.LevelOut, label+"：未读到（"+pr.Reason+"）")
		}
	}

	// 审计/结果摘要**不带路径**（路径片段不进日志）：只报计数。
	summary := fmt.Sprintf("%s：%d/%d 已可访问", permItemTitle(id), readable, len(results))
	status := permissions.StatusUnknown
	switch {
	case denied > 0:
		status = permissions.StatusDenied
	case readable > 0:
		status = permissions.StatusGranted
	}
	s.recordPermissionResult(id, status, summary)

	if status != permissions.StatusGranted {
		log(tasks.LevelErr, "结果："+summary)
		if status == permissions.StatusDenied {
			return nil, errors.New(summary + "。有目标仍被拒。" + diskVolumeAuthManualPath())
		}
		return nil, errors.New(summary + "。没有读到任何目标（目录可能不存在或盘已卸载），授权未确认")
	}
	log(tasks.LevelOK, "结果："+summary)
	return permInstallResult(id, summary, lines), nil
}

// permTargetLabel 是结果行/日志里的通用标签：**不带路径**（审计与日志都不留路径片段）。
func permTargetLabel(id string, index int) string {
	if id == permissions.ItemFullDisk {
		switch index {
		case 0:
			return "桌面"
		case 1:
			return "文稿"
		case 2:
			return "下载"
		}
		return "受保护目录"
	}
	return fmt.Sprintf("可移除宗卷 %d", index+1)
}

// runPermissionExternal 以外部应用自己的签名身份调用它的 CLI；面板不读那个目录。
func (s *Server) runPermissionExternal(ctx context.Context, log tasks.LogFunc, spec permissions.ExternalApp, reqPath string) (any, error) {
	d := permExternalDetectFn(ctx, spec)
	if !d.Installed {
		return nil, errors.New("没有检测到运行中的 " + spec.Name)
	}
	path := strings.TrimSpace(reqPath)
	if path == "" && len(d.Roots) > 0 {
		path = d.Roots[0]
	}
	if path == "" {
		return nil, errors.New("请选择或填写要申请的目标目录（面板不猜目录）")
	}

	log(tasks.LevelStep, "以 "+d.Owner+" 身份调用 "+spec.Name+" 自检（弹窗由它的签名身份触发）")
	res, err := permExternalCheckFn(ctx, d, path)
	if err != nil {
		s.recordPermissionResult(spec.ID, permissions.StatusUnknown, "调用失败")
		return nil, err
	}
	if !res.Supported {
		// 旧版本没有这个动词：如实报，**不许**退化由面板去读。
		s.recordPermissionResult(spec.ID, permissions.StatusUnknown, res.Reason)
		log(tasks.LevelErr, res.Reason)
		return nil, errors.New(res.Reason)
	}

	if res.Readable {
		summary := spec.Name + " 已可读目标目录"
		s.recordPermissionResult(spec.ID, permissions.StatusGranted, summary)
		log(tasks.LevelOK, summary)
		return permInstallResult(spec.ID, summary, []string{summary}), nil
	}
	summary := spec.Name + " 仍读不到目标目录"
	s.recordPermissionResult(spec.ID, permissions.StatusDenied, summary)
	log(tasks.LevelErr, summary)
	return nil, errors.New(summary + "。" + permExternalManualPath(spec, d))
}

// recordPermissionResult 写历史（失败只记日志，不因此谎报申请结果）。
func (s *Server) recordPermissionResult(id, status, result string) {
	h := permissions.HistoryFor(permHistoryPath(s.Cfg.DataDir))
	if err := h.Record(permissions.Entry{ID: id, Status: status, Result: result}); err != nil {
		s.Log.Warn("写权限申请历史失败: %v", err)
	}
}

// permInstallResult 把结果包成任务中心认识的结构（Steps **不含路径**，审计只取它）。
func permInstallResult(id, summary string, lines []string) *services.InstallResult {
	steps := append([]string{summary}, lines...)
	return &services.InstallResult{App: "permissions", Name: id, Message: summary, Steps: steps}
}
