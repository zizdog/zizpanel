package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// api_market_update.go —— 市场卡片的"装了但镜像上有更新吗"按需接口。
//
// 单独成文件：handleMarketList 在 api_services.go，门禁据此断言那个文件里不出现探测调用。
// 探测很贵（镜像索引 + `--version` 子进程），只在按需请求时做，结果在 Server 里按 TTL 缓存。

// marketUpdateCheckTTL 是进程内缓存时长。
//
// 刻意**略短于**前端 sessionStorage 的 10 分钟：否则前端到期再来问时后端也快到期，
// 会把旧的 checked_at 发回去、前端立刻又判过期而反复请求。后端先过期最稳。
const marketUpdateCheckTTL = 9 * time.Minute

// cachedUpdateCheck 是一条缓存结果 + 写入时刻（TTL 用）。
type cachedUpdateCheck struct {
	check services.ZizvideoUpdateCheck
	at    time.Time
}

// handleMarketUpdateCheck 处理 GET /api/v1/market/{id}/update-check。
//
// 返回：{"app","installed","latest","update_available","unknown","checked_at","error"}。
// 未安装 ⇒ installed 为空；索引不可达/拿不到期望值 ⇒ unknown=true + error 写人话，
// **绝不假装**有更新或已是最新。
func (s *Server) handleMarketUpdateCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !services.SupportsUpdateCheck(id) {
		fail(w, http.StatusBadRequest,
			"「"+id+"」不支持更新检查：它的版本不由镜像索引决定，没有"+
				"「装了但镜像上有新版」这回事")
		return
	}
	// 目前只有 zizvideo 是动态版本条目（services.SupportsUpdateCheck 静态派生的事实，
	// 有测试锁住）。将来新增动态条目时在这里接线，而不是把 zizvideo 的探测套上去。
	if id != services.ZizvideoAppID {
		fail(w, http.StatusBadRequest, "「"+id+"」的更新检查还没接线")
		return
	}
	// fresh=1 只给用户手动点的「检查更新」用：绕过进程内缓存重探一次
	//（否则一次瞬时故障的 unknown 会顶到 TTL 结束）。自动路径不带它。
	if r.URL.Query().Get("fresh") == "1" {
		s.forgetUpdateCheck(id)
	}
	ok(w, s.appUpdateCheck(r.Context(), id))
}

// forgetMarketUpdates 让批量结论立即失效（安装/升级/卸载完成后调用；
// 否则刚更新完的卡片上"有新版"徽标会一直挂着，直到 TTL 到期）。
func (s *Server) forgetMarketUpdates() {
	s.updateMu.Lock()
	s.updateBatch = marketUpdatesResponse{}
	s.updateBatchAt = time.Time{}
	s.updateMu.Unlock()
}

// handleMarketUpgrade 处理 POST /api/v1/market/{id}/upgrade：把已装的应用更新到最新版。
//
// 为什么 brew 条目必须走 `brew upgrade`：`brew install <已装的包>` 是幂等跳过，
// 拿它当"更新"就是点了没反应却报成功（谎报）。动态索引条目（zizvideo）的更新
// 本来就是"按最新版重装"（同一套安装流程会复核 sha256 与 --version），直接复用。
func (s *Server) handleMarketUpgrade(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, found := services.FindApp(id)
	if !found {
		fail(w, http.StatusBadRequest, "应用市场中找不到 "+id)
		return
	}
	if strings.TrimSpace(app.BrewFormula) == "" {
		s.handleMarketInstall(w, r)
		return
	}
	s.launchInstallTask(w, r, "upgrade", id, "更新 "+app.Name, "market_upgrade",
		func(ctx context.Context, _ tasks.LogFunc) (any, error) {
			res := &services.InstallResult{App: app.ID, Name: app.Name}
			if err := s.svcManager().UpgradeBrewApp(ctx, app, res); err != nil {
				return nil, err
			}
			return res, nil
		})
}

// appUpdateCheck 先查缓存，miss 才真的探测（探测期间串行化，避免重复打镜像）。
func (s *Server) appUpdateCheck(ctx context.Context, id string) services.ZizvideoUpdateCheck {
	if chk, hit := s.cachedUpdateCheck(id); hit {
		return chk
	}
	s.updateProbeMu.Lock()
	defer s.updateProbeMu.Unlock()
	// 等锁期间别人可能已经探测完并写进缓存（double-check）。
	if chk, hit := s.cachedUpdateCheck(id); hit {
		return chk
	}
	chk := s.probeAppUpdate(ctx, id)
	s.storeUpdateCheck(id, chk)
	return chk
}

// storeUpdateCheck 写进程内缓存（调用方决定要不要持 updateProbeMu）。
func (s *Server) storeUpdateCheck(id string, chk services.ZizvideoUpdateCheck) {
	s.updateMu.Lock()
	if s.updateChecks == nil {
		s.updateChecks = map[string]cachedUpdateCheck{}
	}
	s.updateChecks[id] = cachedUpdateCheck{check: chk, at: time.Now()}
	s.updateMu.Unlock()
}

// ---------------------------------------------------------------------------
//  批量：一次问 brew，两个子 Tab 共用一份结论
// ---------------------------------------------------------------------------

// marketUpdatesResponse 是 GET /api/v1/market/updates 的响应。
type marketUpdatesResponse struct {
	CheckedAt string `json:"checked_at"`
	// BrewOK=false 表示这次**没能**读到 brew 的可升级清单（brew 不可用/超时/联网失败）：
	// 这时 brew 条目的 Items 里是 unknown=true，界面必须说"未知"，不许说"已是最新"。
	BrewOK    bool   `json:"brew_ok"`
	BrewError string `json:"brew_error,omitempty"`
	// Items 按目录应用 ID 给结论；**没有条目的应用 = 现在没有可比的版本真源**
	//（前端不给徽标，也不给"检查更新"按钮）。
	Items map[string]services.AppUpdate `json:"items"`
}

// handleMarketUpdates 批量检查更新：一次 `brew outdated` + 逐个动态索引条目。
//
// 为什么必须批量：`brew outdated` 一次就给出**全部**可升级的包，按卡片逐个查会跑
// N 次联网比对（13 个 brew 应用 = 13 次）；而且「已安装」与「应用市场」两个子 Tab
// 必须共用同一份结论（2026-09-22 zizvideo 报障的根因之一就是结论只在一个 Tab 上）。
func (s *Server) handleMarketUpdates(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh") == "1"
	ok(w, s.marketUpdates(r.Context(), fresh))
}

// marketUpdates 先查批量缓存，miss 才真探测（探测期间串行化）。
func (s *Server) marketUpdates(ctx context.Context, fresh bool) marketUpdatesResponse {
	if !fresh {
		if cached, hit := s.cachedMarketUpdates(); hit {
			return cached
		}
	}
	s.updateProbeMu.Lock()
	defer s.updateProbeMu.Unlock()
	// 等锁期间别人可能已经探测完（double-check）。
	if !fresh {
		if cached, hit := s.cachedMarketUpdates(); hit {
			return cached
		}
	}
	resp := s.probeMarketUpdates(ctx)
	s.updateMu.Lock()
	s.updateBatch, s.updateBatchAt = resp, time.Now()
	s.updateMu.Unlock()
	return resp
}

// cachedMarketUpdates 取 TTL 内的批量结论。
func (s *Server) cachedMarketUpdates() (marketUpdatesResponse, bool) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if s.updateBatch.Items == nil || time.Since(s.updateBatchAt) >= marketUpdateCheckTTL {
		return marketUpdatesResponse{}, false
	}
	return s.updateBatch, true
}

// probeMarketUpdates 是真实探测：调用方必须已持有 updateProbeMu。
func (s *Server) probeMarketUpdates(ctx context.Context) marketUpdatesResponse {
	now := time.Now().Format(time.RFC3339)
	resp := marketUpdatesResponse{CheckedAt: now, Items: map[string]services.AppUpdate{}}

	// 已装 brew 版本：先走一次批量探测（有 5 分钟缓存，列表路径本来就会做）。
	_, _ = s.installedFormulas(ctx)
	s.mktMu.Lock()
	brewVers := s.mktBrewVer
	s.mktMu.Unlock()

	outdated, outOK := s.svcManager().OutdatedFormulas(ctx)
	resp.BrewOK = outOK
	if !outOK {
		resp.BrewError = "读不到 brew 的可升级清单（brew 不可用/超时/联网失败）"
	}

	for _, a := range marketVisibleApps(services.Catalog()) {
		if a.BrewFormula != "" {
			formula, inst, installed := services.ResolveBrewFormula(a.BrewFormula, brewVers)
			if !installed {
				continue // 没装：市场显示「安装」，谈不上"更新"
			}
			u := services.AppUpdate{App: a.ID, Installed: inst, CheckedAt: now, Source: "brew"}
			if !outOK {
				u.Unknown, u.Error = true, resp.BrewError
			} else if latest := strings.TrimSpace(outdated[formula]); latest != "" {
				u.Latest, u.UpdateAvailable = latest, true
			}
			resp.Items[a.ID] = u
			continue
		}
		// 动态索引条目：复用按需接口的探测与缓存（**不要**在这里另写一套）。
		if a.ID == services.ZizvideoAppID {
			chk := s.probeAppUpdate(ctx, a.ID)
			s.storeUpdateCheck(a.ID, chk)
			resp.Items[a.ID] = chk
		}
	}
	return resp
}

// probeAppUpdate 是真实探测入口（单测用 marketUpdateCheckOverride 替换）。
func (s *Server) probeAppUpdate(ctx context.Context, id string) services.ZizvideoUpdateCheck {
	if s.marketUpdateCheckOverride != nil {
		return s.marketUpdateCheckOverride(ctx, id)
	}
	return s.svcManager().CheckZizvideoUpdate(ctx)
}

// cachedUpdateCheck 取 TTL 内的缓存结果。
func (s *Server) cachedUpdateCheck(id string) (services.ZizvideoUpdateCheck, bool) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	entry, ok := s.updateChecks[id]
	if !ok || time.Since(entry.at) >= marketUpdateCheckTTL {
		return services.ZizvideoUpdateCheck{}, false
	}
	return entry.check, true
}

// forgetUpdateCheck 让某个应用的缓存立刻失效（安装/更新完成后调用，让下次检查
// 真的重跑而不是拿旧结论）。
func (s *Server) forgetUpdateCheck(id string) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	delete(s.updateChecks, id)
}
