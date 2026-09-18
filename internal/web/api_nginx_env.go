package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zizdog/zizpanel/internal/priv"
)

// ============================================================================
//  「修复 Nginx 环境」——把手动自愈做成一颗按钮
//
//  用户 2026-09-22 报障："反向代理用不了了！规则已保存但 nginx 配置应用失败：
//  配置语法错误，已回滚：nginx: [emerg] unknown "connection_upgrade" variable"
//
//  根因不是用户的规则写错了，而是面板自己的 nginx 片段没被加载：
//  brew 重装/升级 nginx 会把 nginx.conf **还原成出厂版**，里面的
//  `include conf.d/*.conf` 与 upgrade map 一起消失 —— 于是任何用了
//  $connection_upgrade 的反向代理规则都过不了 `nginx -t`。
//
//  面板本来就会修（EnsureNginxEnv），但只有启动时那一次。自动自愈（1.3.6 起的
//  巡检 + 写 vhost 前的预检）已经能兜住，可用户当下就想把规则保存成功 ——
//  给他一颗按钮，点了立刻补齐并**复核 nginx -t**，而不是让他去重装 nginx 或改配置。
// ============================================================================

// nginxEnvSnapshot 是"nginx 基础片段到底加载了没有"的只读快照。
type nginxEnvSnapshot struct {
	ConfDIncluded  bool   `json:"conf_d_included"`
	VhostsIncluded bool   `json:"vhosts_included"`
	UpgradeMap     bool   `json:"upgrade_map"`
	ConfDPath      string `json:"conf_d_path"`
	MapPath        string `json:"map_path"`
}

// nginxEnvSnapshotNow 读当前状态（不写任何东西）。
func (s *Server) nginxEnvSnapshotNow() nginxEnvSnapshot {
	snap := nginxEnvSnapshot{
		ConfDPath: priv.NginxConfD(),
		MapPath:   filepath.Join(priv.NginxConfD(), "upgrade-map.conf"),
	}
	if inc, _, err := priv.ConfDIncluded(); err == nil {
		snap.ConfDIncluded = inc
	}
	if inc, _, err := priv.VhostsIncluded(); err == nil {
		snap.VhostsIncluded = inc
	}
	if _, err := os.Stat(snap.MapPath); err == nil {
		snap.UpgradeMap = true
	}
	return snap
}

// handleNginxEnsureEnv 手动触发一次 nginx 环境自愈，并复核 nginx -t。
//
// 幂等：环境已经是好的就什么都不改（只回报现状 + 校验通过）。
func (s *Server) handleNginxEnsureEnv(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	before := s.nginxEnvSnapshotNow()

	s.healWebEnv(ctx)

	after := s.nginxEnvSnapshotNow()

	// 复核：让 nginx 自己说话（助手跑 nginx -t），不拿"我写完了"当成功。
	testOK := false
	testOut := ""
	if res, err := s.callHelper(ctx, "nginx-test"); err != nil {
		testOut = err.Error()
	} else {
		testOK = true
		if msg, _ := res["message"].(string); msg != "" {
			testOut = msg
		}
	}

	fixed := []string{}
	if !before.ConfDIncluded && after.ConfDIncluded {
		fixed = append(fixed, "nginx.conf 已加上 include "+after.ConfDPath+"/*.conf（反向代理的 WebSocket map 才会被加载）")
	}
	if !before.VhostsIncluded && after.VhostsIncluded {
		fixed = append(fixed, "nginx.conf 已加上 include "+s.Cfg.VhostDir+"/*.conf（站点配置才会被加载）")
	}
	if !before.UpgradeMap && after.UpgradeMap {
		fixed = append(fixed, "已写入 "+after.MapPath+"（定义 $connection_upgrade）")
	}
	s.audit(r, "nginx_ensure_env", "nginx", "手动修复 nginx 环境（改动 "+strconv.Itoa(len(fixed))+" 项）", true, "")
	ok(w, map[string]any{
		"before":        before,
		"after":         after,
		"fixed":         fixed,
		"nginx_test_ok": testOK,
		"nginx_test":    testOut,
	})
}
