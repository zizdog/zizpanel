package web

// api_systemsettings_lan.go —— 「允许免授权访问内网段」的 HTTP 层。
//
// 与「系统设置」页其他动作的区别：
//   - 其他动作（pmset / hosts / 偏好）是多命令、要十几秒的**长任务**，走任务中心；
//   - 这一个只有几条 `defaults write/delete`，秒级返回，所以是同步 POST，
//     直接把「写完之后的真实状态」返回给前端（前端据此渲染状态行，不靠猜）。
//
// 路由放在 /api/v1/system/settings/lan-preauth：比 `{action}` 通配更具体，
// Go 1.22 的 ServeMux 会优先命中它；CSRF 校验由和其他写接口相同的中间件负责。

import (
	"context"
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/sysconfig"
)

// 包级 fn 变量是本包既有的单测注入约定（见 server.go 的 proxyLookupHostFn）。
// 单测把它们换成假实现：绝不真的跑 defaults、绝不读写真实偏好域。
var (
	lanPreauthProbeFn    = sysconfig.DetectLANPreauth
	lanPreauthApplyFn    = sysconfig.ApplyLANPreauth
	lanPreauthRollbackFn = sysconfig.RollbackLANPreauth
)

// lanPreauthRequest 是写入/撤销的请求体。
type lanPreauthRequest struct {
	// Enabled 省略时按 true（应用）处理；显式 false = 撤销。
	Enabled *bool  `json:"enabled"`
	CIDRs   string `json:"cidrs"`
}

// withLANWarning 保证响应里**始终**带着"对所有程序生效"的代价说明。
//
// 探针正常都会带上它；这里兜底是为了防止以后有人改探针时把代价弄丢 ——
// 少一句警告，用户就会以为这只影响 nginx。
func withLANWarning(st sysconfig.LANPreauthState) sysconfig.LANPreauthState {
	if st.Warning == "" {
		st.Warning = sysconfig.LANPreauthWarning
	}
	return st
}

// handleLANPreauth 应用（enabled=true）或撤销（enabled=false）内网段预授权。
//
// 成功返回的是**重新探测到的**状态；写入失败一律返回真实错误 —— 这个功能会
// 削弱隐私门，绝不能谎报成功。
func (s *Server) handleLANPreauth(w http.ResponseWriter, r *http.Request) {
	var req lanPreauthRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 面板以 root 运行时，用户域写入要降权到真实用户；这个探测也是只读的。
	ctx := r.Context()

	if req.Enabled != nil && !*req.Enabled {
		s.rollbackLANPreauth(w, r, ctx)
		return
	}

	cidrs, err := sysconfig.ParseLANCIDRs(req.CIDRs)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(cidrs) == 0 {
		fail(w, http.StatusBadRequest, "请至少填一个网段（CIDR），例如 192.168.1.0/24")
		return
	}
	if err := lanPreauthApplyFn(ctx, strings.Join(cidrs, ","), nil); err != nil {
		s.audit(r, "sysconfig_lan_preauth", "lan", "写入失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "写入失败："+err.Error())
		return
	}
	st := withLANWarning(lanPreauthProbeFn(ctx))
	// 写入是刚刚发生的：无论 mtime 判断如何，这次改动一定还没生效。
	st.RebootRequired = true
	st.RebootNote = "已写入；这次改动只有重启后才生效（重启前一切照旧）。"
	s.audit(r, "sysconfig_lan_preauth", "lan", "已写入 "+strings.Join(cidrs, ", "), true, "")
	ok(w, st)
}

// rollbackLANPreauth 撤销预授权。只有之前确实写过，才存在"等重启恢复隐私门"。
func (s *Server) rollbackLANPreauth(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	before := withLANWarning(lanPreauthProbeFn(ctx))
	if err := lanPreauthRollbackFn(ctx, nil); err != nil {
		s.audit(r, "sysconfig_lan_preauth_rollback", "lan", "撤销失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "撤销失败："+err.Error())
		return
	}
	st := withLANWarning(lanPreauthProbeFn(ctx))
	if before.SystemSet || before.UserSet {
		st.RebootRequired = true
		st.RebootNote = "撤销已写入；重启后该网段不再豁免。注意：系统里已经登记或授权过的程序" +
			"（例如手动点过「允许」的）不会被这次撤销清除，仍可访问该网段。"
	}
	s.audit(r, "sysconfig_lan_preauth_rollback", "lan",
		"已撤销（"+strings.Join(before.CIDRs, ", ")+"）", true, "")
	ok(w, st)
}
