package web

import (
	"context"
	"net/http"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
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
	s.updateMu.Lock()
	if s.updateChecks == nil {
		s.updateChecks = map[string]cachedUpdateCheck{}
	}
	s.updateChecks[id] = cachedUpdateCheck{check: chk, at: time.Now()}
	s.updateMu.Unlock()
	return chk
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
