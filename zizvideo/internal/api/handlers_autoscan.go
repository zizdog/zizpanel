package api

import (
	"fmt"
	"net/http"

	"github.com/zizdog/zizvideo/internal/autoscan"
	"github.com/zizdog/zizvideo/internal/domain"
)

// HandleGetAutoScan returns the settings, the event-watcher state and the
// latest round's honest outcome.
func (s *Server) HandleGetAutoScan(w http.ResponseWriter, r *http.Request) {
	st := s.Auto.Status()
	respond(w, http.StatusOK, map[string]any{
		"settings":    st.Settings,
		"events_ok":   st.EventsOK,
		"events_note": st.EventsNote,
		"last_run":    st.LastRun,
		"next_run_at": st.NextRunAt,
		"running":     st.Running,
		"summary":     autoScanSummary(st),
	}, nil)
}

// autoScanSummary is the ≤40-字 line the admin page shows.
func autoScanSummary(st autoscan.Status) string {
	if !st.Settings.Enabled {
		return "自动扫描已关闭"
	}
	events := "事件监听可用"
	if !st.EventsOK {
		events = "事件监听不可用"
	}
	return fmt.Sprintf("每 %d 分钟；%s", st.Settings.IntervalMinutes, events)
}

type autoScanReq struct {
	Enabled         *bool `json:"enabled"`
	IntervalMinutes *int  `json:"interval_minutes"`
	EventsEnabled   *bool `json:"events_enabled"`
	DebounceSeconds *int  `json:"debounce_seconds"`
}

// HandlePatchAutoScan persists the settings and answers with the read-back
// state; out-of-range numbers are refused, never silently clamped.
func (s *Server) HandlePatchAutoScan(w http.ResponseWriter, r *http.Request) {
	var req autoScanReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	cur, err := s.DB.GetAutoScanSettings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	changed := false
	if req.Enabled != nil {
		cur.Enabled, changed = *req.Enabled, true
	}
	if req.EventsEnabled != nil {
		cur.EventsEnabled, changed = *req.EventsEnabled, true
	}
	if req.IntervalMinutes != nil {
		if *req.IntervalMinutes < domain.AutoScanIntervalMin || *req.IntervalMinutes > domain.AutoScanIntervalMax {
			s.fail(w, r, domain.ErrAutoScanInterval)
			return
		}
		cur.IntervalMinutes, changed = *req.IntervalMinutes, true
	}
	if req.DebounceSeconds != nil {
		if *req.DebounceSeconds < domain.AutoScanDebounceMin || *req.DebounceSeconds > domain.AutoScanDebounceMax {
			s.fail(w, r, domain.ErrAutoScanDebounce)
			return
		}
		cur.DebounceSeconds, changed = *req.DebounceSeconds, true
	}
	if !changed {
		s.fail(w, r, domain.New("VALIDATION_SETTINGS", "没有可更新的设置项", 400))
		return
	}
	if err := s.DB.SaveAutoScanSettings(cur); err != nil {
		s.audit(r, "autoscan.update", "settings", false, errCode(err))
		s.fail(w, r, err)
		return
	}
	// 回读生效值：写不进去就不算成功（坑 164）。
	back, err := s.DB.GetAutoScanSettings()
	if err != nil {
		s.audit(r, "autoscan.update", "settings", false, "readback_failed")
		s.fail(w, r, domain.New("SETTINGS_WRITE_FAILED", "回读配置失败", 409))
		return
	}
	s.AutoScanChanged()
	s.audit(r, "autoscan.update", "settings", true,
		fmt.Sprintf("enabled=%v interval=%d events=%v", back.Enabled, back.IntervalMinutes, back.EventsEnabled))
	respond(w, http.StatusOK, map[string]any{"settings": back}, nil)
}

// HandleRunAutoScan queues one round through the task centre and answers 202
// with the scan task ids; the round is recorded when those tasks end.
func (s *Server) HandleRunAutoScan(w http.ResponseWriter, r *http.Request) {
	if s.Auto == nil {
		s.fail(w, r, domain.ErrInternal)
		return
	}
	run, ids, err := s.Auto.Trigger(domain.AutoScanTriggerManual)
	if err != nil {
		s.audit(r, "autoscan.run", "manual", false, errCode(err))
		s.fail(w, r, err)
		return
	}
	note := "已在任务中心排队"
	if run != nil && run.Note != "" {
		note = run.Note
	}
	s.audit(r, "autoscan.run", "manual", true, fmt.Sprint(len(ids)))
	body := map[string]any{"run": run, "task_ids": ids, "note": note}
	if run != nil {
		body["last_run"] = run
	}
	respond(w, http.StatusAccepted, body, nil)
}
