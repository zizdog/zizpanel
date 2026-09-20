package web

import (
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  「面板自己单次上传请求体上限」的唯一来源
//
//  用户报障（真机）：用「⬆ 上传文件夹」传一个 4.16 GB 的网站被拒，提示却说
//  "超过单次上传上限"，还建议他用**同一个功能**按子目录分批 —— 自相矛盾。
//  根因是判据用错了层级：拿**整批总大小**去比**单次请求上限**。
//
//  正确判据只有两条：
//    1. 单个文件 > 上限 → 拒绝（必须点名文件）；
//    2. 总大小任意大 → 前端按"每次请求 ≤ 上限"自动分批。
//
//  所以上限本身必须是**可配/可注入**的一个值（config.json 的 panel_upload_limit），
//  而不是前后端各写一份的常量：真机/测试把它调小（如 4m）就能走完整条链路。
// ============================================================================

// panelUploadLimitView 是给前端的上限回读结果。
type panelUploadLimitView struct {
	LimitBytes int64  `json:"limit_bytes"`
	LimitText  string `json:"limit_text"`
	Source     string `json:"source"`
	// Verified=false 表示配置里的值读不到/不合法：界面必须如实说"未复核"，
	// 不许拿内置默认值冒充用户配置（"保存成功"≠"已生效"的同类纪律）。
	Verified bool   `json:"verified"`
	Note     string `json:"note,omitempty"`
}

// panelUploadLimit 解析**面板自己**的单次上传上限（全仓库唯一解析处）。
//
// 配置值不合法时退回内置默认值并把 Verified 置 false —— 上限仍然存在
// （绝不能因为读不到配置就放开成无限大），但界面与文案必须标注"未复核"。
func (s *Server) panelUploadLimit() panelUploadLimitView {
	raw := ""
	if s.Cfg != nil {
		raw = strings.TrimSpace(s.Cfg.PanelUploadLimit)
	}
	if n, ok := sites.ParseSizeBytes(raw); ok && n > 0 {
		return panelUploadLimitView{
			LimitBytes: n, LimitText: raw,
			Source: "面板配置（config.json）", Verified: true,
		}
	}
	n, _ := sites.ParseSizeBytes(config.DefaultPanelUploadLimit)
	v := panelUploadLimitView{
		LimitBytes: n, LimitText: config.DefaultPanelUploadLimit,
		Source: "内置默认值", Verified: false,
	}
	if raw == "" {
		v.Note = "面板配置里没有 panel_upload_limit，用的是内置默认值（未复核）"
	} else {
		v.Note = "面板配置里的 panel_upload_limit=" + raw + " 无法解析，用的是内置默认值（未复核）"
	}
	return v
}

// handleGetPanelUploadLimit 把真实生效的上限回读给前端。
//
// 为什么要有这个接口：前端过去把 4 GiB 写死在 files.js 里（还靠测试锁两个数字
// 相等）。写死就必然出现"提示里的数字不是真实生效的上限"，而且上限一旦可配，
// 写死的数字立刻说谎。前端的本地预检与分流都必须用这个接口回读到的值。
func (s *Server) handleGetPanelUploadLimit(w http.ResponseWriter, r *http.Request) {
	ok(w, s.panelUploadLimit())
}
