// 本地插件（P2）的接口：列出 / 启用 / 停用 / 看计划。
//
// 设计取舍：
//
//	· 只读文件 + 一个 0600 的启用状态文件，**不**把插件状态写进面板配置（两份真源必漂）；
//	· 启用是**显式动作**：本地插件等于让面板以 root 去装/跑东西，默认关闭；
//	· 不能启用的（来源/运行方式还没支持的）如实返回原因，绝不让用户以为"点了就会装"。
package web

import (
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/plugins"
	"github.com/zizdog/zizpanel/internal/services"
)

// handlePluginList 列出本地插件目录里的全部声明（含解析失败的）。
func (s *Server) handlePluginList(w http.ResponseWriter, r *http.Request) {
	dir := services.LocalPluginDir()
	ok(w, map[string]any{
		"dir":   dir,
		"items": services.LocalPluginStatus(services.LocalPluginEnabledState()),
		"note": "放一份 JSON 到上面这个目录即可新增应用（格式见 docs/插件规范.md，" +
			"或跑 `zizpanel plugin init` 生成模板）。启用后才出现在应用市场里。",
	})
}

type pluginToggleReq struct {
	Enabled *bool `json:"enabled"`
}

// handlePluginToggle 启用/停用一个本地插件。
func (s *Server) handlePluginToggle(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req pluginToggleReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Enabled == nil {
		fail(w, http.StatusBadRequest, "缺少 enabled 字段")
		return
	}
	dir := services.LocalPluginDir()
	if dir == "" {
		fail(w, http.StatusConflict, "面板没有启用本地插件目录（这一版还没接上）")
		return
	}
	// 只有**合法且可安装**的声明才允许启用：不合法的启用等于把坏表塞进市场目录。
	var found bool
	for _, v := range services.LocalPluginStatus(services.LocalPluginEnabledState()) {
		if v.ID != id {
			continue
		}
		found = true
		if *req.Enabled && !v.Installable {
			fail(w, http.StatusBadRequest, "不能启用「"+id+"」："+v.Error)
			return
		}
	}
	if !found {
		fail(w, http.StatusNotFound, "插件目录里没有 id="+id+" 的声明")
		return
	}
	if err := plugins.SetEnabled(dir, id, *req.Enabled); err != nil {
		fail(w, http.StatusInternalServerError, "写入启用状态失败："+err.Error())
		return
	}
	action := "停用"
	if *req.Enabled {
		action = "启用"
	}
	s.audit(r, "plugin", id, action+"本地插件", true, "")
	ok(w, map[string]any{"id": id, "enabled": *req.Enabled,
		"hint": "已" + action + "；刷新「应用」页即可看到变化（已装的东西不会因为它被删掉）"})
}
