package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// aria2InstallFn 是安装入口；单测注入假实现（真实安装要跑 brew + 写 launchd）。
var aria2InstallFn = func(s *Server, ctx context.Context, app services.App, res *services.InstallResult) error {
	return s.svcManager().InstallAria2(ctx, app, res)
}

// handleInstallAria2 安装应用市场里的 aria2（下载器）。
//
// 为什么走自研安装器而不是通用 brew 流程（三条例由，见 services/aria2.go）：
// brew 的 aria2 **没有 service 块**、它本体只是 JSON-RPC（6800）、
// 界面（AriaNg）由面板托管在 /aria/。收尾真复核：拿配置里的密钥问一次
// `aria2.getVersion`，回版本号才算装好（端口在听不算）。
//
// 全程走任务中心：brew 装包是分钟级动作，同步请求会被用户刷新杀掉（AGENTS 第三节）。
func (s *Server) handleInstallAria2(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found || app.PanelInstaller != services.Aria2AppID {
		fail(w, http.StatusBadRequest, "应用市场中找不到 aria2 条目 "+id)
		return
	}
	s.launchTask(w, r, "install", id,
		"安装 "+app.Name+"（"+services.Aria2Formula+" + 面板托管的下载界面）",
		"install_aria2", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: app.ID, Steps: []string{}}
			if err := aria2InstallFn(s, ctx, app, res); err != nil {
				// brew 装瓶是联网动作：网络类失败附统一提示（见 services/netfail.go）。
				return res, services.AppendNetworkHint(err)
			}
			return res, nil
		})
}

// handleAria2ScriptToken 把"非浏览器脚本调用 RPC"要用的专用凭证交给管理员。
//
// 为什么要一个接口：这个值不是给浏览器页面用的（页面走同源代理就行），而是给
// 油猴/其它机器上的脚本用的 —— 它们需要一个能贴进脚本的字符串，而面板配置在
// /opt/zizpanel/data/config.json（要 root 才读得到）。只有登录会话能取，
// 且它单独泄漏也操纵不了 aria2（仍要 rpc-secret）。
func (s *Server) handleAria2ScriptToken(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(s.Cfg.Aria2APIToken)
	if tok == "" {
		writeErr(w, http.StatusConflict, "面板还没有生成 aria2 脚本凭证（重启面板会补上）")
		return
	}
	ok(w, map[string]any{
		"header": Aria2APITokenHeader,
		"token":  tok,
		"usage": "POST " + Aria2APITokenHeader + ": <token> 到 AriaNg 的 /jsonrpc（独立端口或反代域名）" +
			"；body 里仍要 aria2 自己的 token:<rpc-secret>",
	})
}
