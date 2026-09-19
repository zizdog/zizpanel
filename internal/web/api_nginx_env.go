package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/zizdog/zizpanel/internal/priv"
)

// ============================================================================
//  「修复 Nginx 环境」——把手动自愈做成一颗按钮
//
//  用户报障："反向代理用不了了！规则已保存但 nginx 配置应用失败：
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

// nginxEnvSnapshot 是"nginx 基础环境到底铺好了没有"的只读快照。
//
// RuntimeDirsBad 里的每一项都是**真的会让请求失败**的东西：nginx 把超过
// client_body_buffer_size 的请求体落盘到 client_body_temp；那个目录缺失或属主
// 不是 worker 用户时，nginx 直接回**自己的 500 HTML 页** —— 用户看到的就是
// "返回的不是 JSON（HTTP 500，Content-Type: text/html）… nginx/1.31.6"
// （TtsVoice 推音色样本、phpMyAdmin 导入大 SQL 都栽在这里）。
type nginxEnvSnapshot struct {
	ConfDIncluded  bool   `json:"conf_d_included"`
	VhostsIncluded bool   `json:"vhosts_included"`
	UpgradeMap     bool   `json:"upgrade_map"`
	ConfDPath      string `json:"conf_d_path"`
	MapPath        string `json:"map_path"`
	// RuntimeDir     是这些临时目录的父目录；BadRuntimeDirs 列出缺失/属主不对的子目录
	//（空 = 全部就绪）。名字里刻意带 Bad：读接口的人不会把"有列表"当成好事。
	RuntimeDir     string   `json:"runtime_dir"`
	BadRuntimeDirs []string `json:"bad_runtime_dirs,omitempty"`
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
	// 运行时目录：存在 + 属主是 nginx worker 用户才算就绪
	snap.RuntimeDir, snap.BadRuntimeDirs = s.nginxRuntimeDirsState()
	return snap
}

// nginxRuntimeDirsState 检查 nginx 的临时目录（返回父目录与"有问题"的子目录列表）。
//
// 判据不只是"目录在不在"，还包括**属主是不是 worker 用户**：brew 安装/升级/迁移
// 机器后这些目录常常存在但属主是别人（真机见过 nobody:admin 0700，而 worker 跑在
// 另一个用户下），那时 nginx 依然写不进去 —— 只判存在会漏掉一半故障。
func (s *Server) nginxRuntimeDirsState() (string, []string) {
	base := filepath.Join(priv.HomebrewPrefix(), "var", "run", "nginx")
	// 期望属主同样用运行体判据（运行中的 worker 进程 → nginx.conf 的 user 指令）。
	// 判不出来就只看"目录在不在"，**不谎报"属主不对"**（不知道对的是谁）。
	worker, wantUID, wantGID, _, ownerKnown := priv.NginxWorkerOwner(s.Cfg.NginxConf)
	if !ownerKnown {
		worker, wantUID, wantGID = "", -1, -1
	}
	var bad []string
	for _, sub := range []string{"", "client_body_temp", "proxy_temp", "fastcgi_temp", "uwsgi_temp", "scgi_temp"} {
		dir := filepath.Join(base, sub)
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			bad = append(bad, sub+"/（不存在）")
			continue
		}
		if wantUID >= 0 {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok && (int(sys.Uid) != wantUID || int(sys.Gid) != wantGID) {
				bad = append(bad, sub+"/（属主不是 "+worker+"）")
			}
		}
	}
	return base, bad
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
	if len(before.BadRuntimeDirs) > 0 && len(after.BadRuntimeDirs) == 0 {
		fixed = append(fixed, "已补齐 nginx 运行时临时目录 "+after.RuntimeDir+
			"（"+strconv.Itoa(len(before.BadRuntimeDirs))+" 项：client_body_temp 等）——"+
			"大请求体（音色样本、大 SQL 导入）落盘要用它，缺了 nginx 会直接回自己的 500 页面")
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
