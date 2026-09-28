package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Jellyfin 的两件事
//
//  ① 安装：走面板自研安装器（镜像下载 → sha256 → 解包 → 面板二进制的
//     jellyfin-supervise → 等 /health 返回 Healthy），见 services/jellyfin.go。
//  ② 媒体目录：用户挑目录，面板用 Jellyfin 的身份（真实用户）**真的试读一次**。
//
//  为什么②必须做：Jellyfin 的库路径由它自己的界面管理，面板不写它的数据库；
//  但"Jellyfin 到底读不读得到这个目录"是面板能提前验证、且**只有面板能验证**
//  的事（外接盘/可移除宗卷的 TCC 授权属于面板的代码要求）。不验证的后果是
//  用户在 Jellyfin 里看到一个空库，却不知道是隐私保护挡住了。
// ============================================================================

// jellyfinMediaDirsKey 是 settings 表里的键（值 = []string 的 JSON）。
const jellyfinMediaDirsKey = "jellyfin_media_dirs"

// jellyfinInstallFn 是安装入口；单测注入假实现（真实安装要联网 + 动 launchd）。
var jellyfinInstallFn = func(s *Server, ctx context.Context, app services.App, res *services.InstallResult) error {
	return s.svcManager().InstallJellyfin(ctx, app, res)
}

// handleInstallJellyfin 安装应用市场里的 Jellyfin。
func (s *Server) handleInstallJellyfin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found || app.PanelInstaller != services.JellyfinAppID {
		fail(w, http.StatusBadRequest, "应用市场中找不到 Jellyfin 条目 "+id)
		return
	}
	s.launchTask(w, r, "install", id,
		"安装 "+app.Name+"（镜像 "+services.JellyfinVersion+"，面板托管守护进程）",
		"install_jellyfin", func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: app.ID, Steps: []string{}}
			if err := jellyfinInstallFn(s, ctx, app, res); err != nil {
				return res, services.AppendNetworkHint(err)
			}
			return res, nil
		})
}

// expandJellyfinDir 把用户填的 ~/… 展开成绝对路径（不认 ~user 写法，避免歧义）。
func (s *Server) expandJellyfinDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "~" {
		return strings.TrimSpace(s.Cfg.UserHome)
	}
	if strings.HasPrefix(dir, "~/") {
		home := strings.TrimSpace(s.Cfg.UserHome)
		if home == "" {
			return dir // 家目录未知时原样返回，由后续校验如实报"必须是绝对路径"
		}
		return filepath.Join(home, strings.TrimPrefix(dir, "~/"))
	}
	return dir
}

// validateJellyfinMediaDir 校验一个媒体目录：绝对路径 + 真的存在 + 是目录。
// 返回归一路径；不合法时给出**面向用户**的原因（调用方原样 400 回去）。
func (s *Server) validateJellyfinMediaDir(raw string) (string, error) {
	dir := s.expandJellyfinDir(raw)
	if dir == "" {
		return "", errors.New("媒体目录不能为空")
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("媒体目录必须是绝对路径（或 ~/ 开头），现在是「" + raw + "」")
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", errors.New("读不到这个目录：" + dir + "。确认它还在、外接盘已挂载再保存")
	}
	if !st.IsDir() {
		return "", errors.New(dir + " 不是一个目录")
	}
	return filepath.Clean(dir), nil
}

// loadJellyfinMediaDirs 读整份媒体目录表。
func (s *Server) loadJellyfinMediaDirs(r *http.Request) ([]string, error) {
	raw, err := s.Store.GetSetting(r.Context(), jellyfinMediaDirsKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var dirs []string
	if err := json.Unmarshal([]byte(raw), &dirs); err != nil {
		return nil, errors.New("媒体目录数据损坏（" + jellyfinMediaDirsKey + "）：" + err.Error())
	}
	return services.NormalizeJellyfinMediaDirs(dirs), nil
}

// saveJellyfinMediaDirs 写整份媒体目录表；空表写空串（不留一行空 JSON）。
func (s *Server) saveJellyfinMediaDirs(r *http.Request, dirs []string) error {
	if len(dirs) == 0 {
		return s.Store.SetSetting(r.Context(), jellyfinMediaDirsKey, "")
	}
	b, err := json.Marshal(dirs)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(r.Context(), jellyfinMediaDirsKey, string(b))
}

// jellyfinMediaChecks 逐个目录跑试读（前端一次拿到全部结论）。
func (s *Server) jellyfinMediaChecks(ctx context.Context, dirs []string) []services.JellyfinMediaCheck {
	mgr := s.svcManager()
	out := make([]services.JellyfinMediaCheck, 0, len(dirs))
	for _, d := range dirs {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		out = append(out, mgr.JellyfinMediaReadCheck(cctx, d))
		cancel()
	}
	return out
}

// handleJellyfinMediaGet 返回已保存的媒体目录与各自的试读结论。
func (s *Server) handleJellyfinMediaGet(w http.ResponseWriter, r *http.Request) {
	dirs, err := s.loadJellyfinMediaDirs(r)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if dirs == nil {
		dirs = []string{}
	}
	ok(w, s.jellyfinMediaPayload(r.Context(), dirs))
}

// jellyfinMediaPayload 组装媒体目录响应：目录 + 试读结论 + **已挂载的网络盘**候选。
//
// 为什么要带网络盘：网络共享挂载点（<安装根>/mnt/…）对用户不是显而易见的路径，
// 让他手打一遍纯属为难人；这里把"当前真的挂上了的"列出来，点一下就填进去，
// 之后照旧走"用 Jellyfin 身份真的试读"这一步。
func (s *Server) jellyfinMediaPayload(ctx context.Context, dirs []string) map[string]any {
	out := map[string]any{
		"dirs":   dirs,
		"checks": s.jellyfinMediaChecks(ctx, dirs),
		"note": "Jellyfin 的媒体库路径要在它自己的安装向导/后台里添加；" +
			"面板在这里验证的是「Jellyfin 那个身份读不读得到」，读不到就会是空库。",
	}
	mounts, err := s.mountedSMBMounts(ctx)
	if err != nil {
		out["smb_mounts_error"] = err.Error()
	}
	if mounts == nil {
		mounts = []map[string]any{}
	}
	out["smb_mounts"] = mounts
	return out
}

// handleJellyfinMediaSet 保存媒体目录（PUT {dirs:[...]}），并返回试读结论。
func (s *Server) handleJellyfinMediaSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dirs []string `json:"dirs"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	clean := make([]string, 0, len(req.Dirs))
	for _, raw := range req.Dirs {
		dir, err := s.validateJellyfinMediaDir(raw)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		clean = append(clean, dir)
	}
	clean = services.NormalizeJellyfinMediaDirs(clean)
	if err := s.saveJellyfinMediaDirs(r, clean); err != nil {
		fail(w, http.StatusInternalServerError, "保存媒体目录失败："+err.Error())
		return
	}
	s.audit(r, "jellyfin_media", services.JellyfinAppID, "保存媒体目录（"+strings.Join(clean, ", ")+"）", true, "")
	ok(w, s.jellyfinMediaPayload(r.Context(), clean))
}

// handleJellyfinMediaCheck 对单个目录跑一次试读（前端“试读”按钮）。
func (s *Server) handleJellyfinMediaCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := s.expandJellyfinDir(req.Dir)
	cctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	ok(w, s.svcManager().JellyfinMediaReadCheck(cctx, dir))
}
