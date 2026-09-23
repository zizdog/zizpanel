package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/notify"
)

// 主动通知（C3）：面板发现异常时主动说一声。
//
// 分层刻意做成"规则是纯函数（internal/notify）+ 这一层只负责取事实/发/存设置"：
//   - 规则可逐条单测（正反对照，见 notify 包）；
//   - 这一层能注入假通道与假事实，测试绝不弹通知、绝不碰 launchd。
//
// 一条重要的一致性约定：**这里报的问题 = 界面上标红的问题**（服务页的「需要处理」）。
// 两边判据漂移的结果是"界面说没事、通知说有事"，用户没法信任任何一个。

const (
	notifyDefaultIntervalMins = 15
	notifyMaxPerRound         = 5
	// notifyFirstDelay 是启动后第一次巡检的延迟：启动路径上有一堆自愈动作
	// （默认站点、回环转发、证书续期），别在这时候再压一轮 launchctl 查询。
	notifyFirstDelay = 60 * time.Second
)

// notifySettings 是设置页读写的那组值。
type notifySettings struct {
	Enabled      bool   `json:"enabled"`
	MacOS        bool   `json:"macos"`
	Webhook      string `json:"webhook"`
	IntervalMins int    `json:"interval_mins"`
	CertDays     int    `json:"cert_days"`
	DiskPercent  int    `json:"disk_percent"`
}

// notifyFacts 是一次巡检取到的事实快照（可注入，单测用）。
type notifyFacts struct {
	Services    []notify.ServiceStatus
	Certs       []notify.CertStatus
	DiskMount   string
	DiskPercent int
	DiskOK      bool
	// Errors 是"这一部分没读到"的如实说明：读不到就不许当成"没问题"。
	Errors []string
}

// notifyCheckResult 是"立即检查"与每轮巡检的结果。
type notifyCheckResult struct {
	Events    []notify.Event `json:"events"`
	Sent      int            `json:"sent"`
	Skipped   int            `json:"skipped"`
	Errors    []string       `json:"errors,omitempty"`
	Note      string         `json:"note,omitempty"`
	CheckedAt string         `json:"checked_at"`
}

// ---------------- 通知器（进程内单例） ----------------

// notifier 返回进程内唯一的通知器（惰性构造，配置从面板配置读）。
func (s *Server) notifier() *notify.Notifier {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.notifyMgr == nil {
		o := s.notifyOptionsLocked()
		s.notifyMgr = notify.New(o, s.notifySinksFor(o)...)
	}
	if s.notifyWake == nil {
		s.notifyWake = make(chan struct{}, 1)
	}
	return s.notifyMgr
}

// notifyOptionsLocked 由面板配置生成通知器配置（调用方不必持锁：只读 Cfg）。
func (s *Server) notifyOptionsLocked() notify.Options {
	return notify.Options{
		Enabled:     s.Cfg.NotifyEnabled,
		MacOSNotify: s.Cfg.NotifyMacOS,
		WebhookURL:  strings.TrimSpace(s.Cfg.NotifyWebhook),
		Cooldown:    30 * time.Minute,
	}
}

// reloadNotifier 把当前配置应用到通知器，并让巡检循环按新间隔重排。
func (s *Server) reloadNotifier() {
	n := s.notifier()
	o := s.notifyOptionsLocked()
	n.SetOptions(o)
	n.SetSinks(s.notifySinksFor(o)...)
	s.notifyMu.Lock()
	wake := s.notifyWake
	s.notifyMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default: // 已经有一次待处理的重排，不堆积
		}
	}
}

// notifySinksFor 按配置造通道。**注意**：这里不看 Enabled ——
// 开/关由通知器统一裁决，通道只负责"怎么发"。
func (s *Server) notifySinksFor(o notify.Options) []notify.Sink {
	if s.notifySinkMaker != nil {
		return s.notifySinkMaker(o)
	}
	var sinks []notify.Sink
	if o.MacOSNotify {
		sinks = append(sinks, notify.MacOSSink{UID: s.Cfg.UserUID})
	}
	if strings.TrimSpace(o.WebhookURL) != "" {
		sinks = append(sinks, notify.WebhookSink{URL: o.WebhookURL})
	}
	return sinks
}

// notifyMacOSReady 如实回答"本机通知现在能不能发"。
//
// 只检查 osascript 在不在；面板以 root 跑时用 launchctl asuser 送进用户会话
// （见 notify.MacOSSink），否则 root 进程弹不出任何东西。
func (s *Server) notifyMacOSReady() (bool, string) {
	if _, err := os.Stat("/usr/bin/osascript"); err != nil {
		return false, "本机找不到 /usr/bin/osascript，无法发系统通知"
	}
	if os.Geteuid() == 0 && s.Cfg.UserUID > 0 {
		return true, "面板以 root 运行：通知通过 launchctl asuser 送进你的用户会话"
	}
	if os.Geteuid() == 0 {
		return true, "面板以 root 运行，但不知道目标用户：系统通知可能弹不出来"
	}
	return true, ""
}

// ---------------- 事实采集 ----------------

// collectNotifyFacts 取三份事实：服务、证书、磁盘。
//
// 每一部分失败都只记进 Errors，不让整轮作废 —— 但绝不把"没读到"当成"没问题"。
func (s *Server) collectNotifyFacts(ctx context.Context) notifyFacts {
	if s.notifyFactsFn != nil {
		return s.notifyFactsFn(ctx)
	}
	var f notifyFacts

	views, err := s.svcManager().List(ctx, true)
	if err != nil {
		f.Errors = append(f.Errors, "读取服务列表失败："+err.Error())
	}
	for _, v := range views {
		if v == nil || v.Service == nil {
			continue
		}
		// 判据与前端 problemOf 一致：启动失败 / 运行时不可用 / 健康检查没过；
		// 但**用户主动停掉的**不算问题（2026-09-18 报障：那是我不想让它跑）。
		st, h := v.State, v.Health
		problem := st.Status == "error" || st.Status == "unavailable" || (h.Checked && !h.OK)
		if v.StoppedByUser && !st.Running {
			problem = false
		}
		if !problem {
			continue
		}
		detail := st.Detail
		if detail == "" && h.Checked && !h.OK {
			detail = h.Message
		}
		f.Services = append(f.Services, notify.ServiceStatus{
			Name: v.Name, Display: v.DisplayName, Status: st.Status,
			HealthOK: h.OK, Checked: h.Checked, Detail: detail,
		})
	}

	certs, err := s.certManager().List()
	if err != nil {
		f.Errors = append(f.Errors, "读取证书列表失败："+err.Error())
	}
	for _, c := range certs {
		if c == nil {
			continue
		}
		f.Certs = append(f.Certs, notify.CertStatus{Domain: c.Primary, NotAfter: c.NotAfter})
	}

	// 磁盘看根卷：macOS 的系统卷与数据卷同属一个 APFS 容器，可用空间是同一个数。
	if pct, ok := notifyDiskUsage("/"); ok {
		f.DiskMount, f.DiskPercent, f.DiskOK = "/", pct, true
	} else {
		f.Errors = append(f.Errors, "读取磁盘占用失败（statfs /）")
	}
	return f
}

// notifyDiskUsage 返回路径所在卷的已用百分比。
func notifyDiskUsage(path string) (int, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 || free > total {
		return 0, false
	}
	return int((total - free) * 100 / total), true
}

// ---------------- 巡检 ----------------

// runNotifyCheck 跑规则 → 发送 → 汇总。send=false 时只列出问题（通知没启用时用）。
func (s *Server) runNotifyCheck(ctx context.Context, f notifyFacts, send bool) notifyCheckResult {
	res := notifyCheckResult{CheckedAt: time.Now().Format(time.RFC3339), Errors: append([]string{}, f.Errors...)}

	var events []notify.Event
	events = append(events, notify.RulesServiceDown(f.Services)...)
	events = append(events, notify.RulesCertExpiring(f.Certs, s.Cfg.NotifyCertDays, time.Now())...)
	if f.DiskOK {
		events = append(events, notify.RulesDiskUsage(f.DiskMount, f.DiskPercent, s.Cfg.NotifyDiskPercent)...)
	}
	events = notify.CapEvents(events, notifyMaxPerRound)

	if !send || !s.Cfg.NotifyEnabled {
		res.Events = events
		if !send {
			res.Note = "只检查，没有发送"
		} else {
			res.Note = "通知未启用：只列出问题，没有发送"
		}
		return res
	}
	n := s.notifier()
	for _, ev := range events {
		sent, err := n.Send(ctx, ev)
		switch {
		case err != nil:
			res.Errors = append(res.Errors, err.Error())
			res.Sent++
		case sent:
			res.Sent++
		default:
			res.Skipped++
		}
	}
	// 发出去的事件（含冷却跳过的）都要在结果里如实列出：跳过的要能解释为什么没收到通知。
	res.Events = events
	return res
}

// watchNotifications 是巡检循环。间隔可在设置里改，改完立刻按新间隔重排。
func (s *Server) watchNotifications(ctx context.Context) {
	first := true
	for {
		d := notifyFirstDelay
		if !first {
			d = time.Duration(s.Cfg.NotifyIntervalMins) * time.Minute
			if d <= 0 {
				d = notifyDefaultIntervalMins * time.Minute
			}
		}
		first = false
		s.notifier() // 确保 notifyWake 已就绪
		s.notifyMu.Lock()
		wake := s.notifyWake
		s.notifyMu.Unlock()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
			continue // 设置变了：按新间隔重排，不立刻巡检
		case <-timer.C:
		}
		if !s.Cfg.NotifyEnabled {
			continue
		}
		f := s.collectNotifyFacts(ctx)
		res := s.runNotifyCheck(ctx, f, true)
		s.setNotifyLast(res)
		if len(res.Errors) > 0 {
			s.Log.Warn("主动通知：本轮有 %d 处失败：%s", len(res.Errors), strings.Join(res.Errors, "；"))
		}
		if res.Sent > 0 {
			s.Log.Info("主动通知：发现 %d 个问题，发出 %d 条（%d 条在冷却期内未重复）",
				len(res.Events), res.Sent, res.Skipped)
		}
	}
}

// setNotifyLast / notifyLast 记录最近一次巡检（设置页显示用）。
func (s *Server) setNotifyLast(res notifyCheckResult) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.notifyLastAt = res.CheckedAt
	s.notifyLastNote = fmt.Sprintf("发现 %d 个问题，发出 %d 条", len(res.Events), res.Sent)
}

func (s *Server) notifyLast() (string, string) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifyLastAt, s.notifyLastNote
}

// ---------------- HTTP ----------------

func (s *Server) handleNotifyGet(w http.ResponseWriter, r *http.Request) {
	ready, note := s.notifyMacOSReady()
	lastAt, lastNote := s.notifyLast()
	ok(w, map[string]any{
		"settings":   s.notifySettingsView(),
		"macos":      map[string]any{"ready": ready, "note": note},
		"last_check": lastAt,
		"last_note":  lastNote,
		"recent":     s.notifier().Recent(),
	})
}

func (s *Server) notifySettingsView() notifySettings {
	return notifySettings{
		Enabled:      s.Cfg.NotifyEnabled,
		MacOS:        s.Cfg.NotifyMacOS,
		Webhook:      s.Cfg.NotifyWebhook,
		IntervalMins: s.Cfg.NotifyIntervalMins,
		CertDays:     s.Cfg.NotifyCertDays,
		DiskPercent:  s.Cfg.NotifyDiskPercent,
	}
}

func (s *Server) handleNotifySettings(w http.ResponseWriter, r *http.Request) {
	var req notifySettings
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	clean, err := validateNotifySettings(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Cfg.NotifyEnabled = clean.Enabled
	s.Cfg.NotifyMacOS = clean.MacOS
	s.Cfg.NotifyWebhook = clean.Webhook
	s.Cfg.NotifyIntervalMins = clean.IntervalMins
	s.Cfg.NotifyCertDays = clean.CertDays
	s.Cfg.NotifyDiskPercent = clean.DiskPercent
	if err := s.Cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, "保存面板配置失败: "+err.Error())
		return
	}
	s.reloadNotifier()
	s.audit(r, "notify_settings", "notify", notifyAuditDetail(clean), true, "")
	ok(w, s.notifySettingsView())
}

func notifyAuditDetail(s notifySettings) string {
	chans := make([]string, 0, 2)
	if s.MacOS {
		chans = append(chans, "本机通知")
	}
	if s.Webhook != "" {
		chans = append(chans, "Webhook")
	}
	if !s.Enabled {
		return "关闭主动通知"
	}
	return fmt.Sprintf("开启主动通知（%s），每 %d 分钟巡检一次", strings.Join(chans, "+"), s.IntervalMins)
}

// handleNotifyTest 立刻通过各通道发一条测试通知，逐通道如实报告结果。
//
// 接受请求体里的设置：用户可以在**保存之前**先测通道（否则要先开一个可能
// 根本发不出去的通道）。不写配置、不进去重表。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	req := s.notifySettingsView()
	if r.ContentLength > 0 {
		var body notifySettings
		if err := decode(r, &body); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		body.IntervalMins = req.IntervalMins
		body.CertDays = req.CertDays
		body.DiskPercent = req.DiskPercent
		req = body
	}
	clean, err := validateNotifySettings(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	sinks := s.notifySinksFor(notify.Options{
		Enabled: true, MacOSNotify: clean.MacOS, WebhookURL: clean.Webhook,
	})
	if len(sinks) == 0 {
		fail(w, http.StatusBadRequest, "没有可用的通知通道：请勾选「本机通知」或填一个 Webhook 地址")
		return
	}
	ev := notify.Event{
		Key: "test", Title: "ZizPanel 测试通知", Level: notify.LevelInfo,
		Body: "看到这条说明通道配置可用。",
	}
	type chanResult struct {
		Channel string `json:"channel"`
		OK      bool   `json:"ok"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]chanResult, 0, len(sinks))
	for _, sk := range sinks {
		if err := sk.Send(r.Context(), ev); err != nil {
			results = append(results, chanResult{Channel: sk.Name(), Error: err.Error()})
			continue
		}
		results = append(results, chanResult{Channel: sk.Name(), OK: true})
	}
	s.audit(r, "notify_test", "notify", "发送测试通知", true, "")
	ok(w, map[string]any{"results": results})
}

// handleNotifyCheck 立刻巡检一次（并把结果如实返回；通知启用时同时发送）。
func (s *Server) handleNotifyCheck(w http.ResponseWriter, r *http.Request) {
	f := s.collectNotifyFacts(r.Context())
	res := s.runNotifyCheck(r.Context(), f, true)
	s.setNotifyLast(res)
	ok(w, res)
}

// validateNotifySettings 校验并归一化设置。非法输入给 400 + 人话（别默默钳值）。
func validateNotifySettings(in notifySettings) (notifySettings, error) {
	out := in
	out.Webhook = strings.TrimSpace(in.Webhook)
	if out.Enabled && !out.MacOS && out.Webhook == "" {
		return out, fmt.Errorf("请至少选一个通知通道：本机通知或 Webhook 地址")
	}
	if out.Webhook != "" {
		u, err := url.Parse(out.Webhook)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return out, fmt.Errorf("Webhook 地址必须是完整的 http:// 或 https:// 地址")
		}
	}
	if out.IntervalMins <= 0 {
		out.IntervalMins = notifyDefaultIntervalMins
	}
	if out.IntervalMins > 1440 {
		return out, fmt.Errorf("巡检间隔最长 1440 分钟（1 天）")
	}
	if out.CertDays <= 0 {
		out.CertDays = 14
	}
	if out.CertDays > 90 {
		return out, fmt.Errorf("证书提醒天数最长 90 天")
	}
	if out.DiskPercent <= 0 {
		out.DiskPercent = 90
	}
	if out.DiskPercent < 50 || out.DiskPercent > 99 {
		return out, fmt.Errorf("磁盘提醒阈值请在 50~99 之间（百分比）")
	}
	return out, nil
}
