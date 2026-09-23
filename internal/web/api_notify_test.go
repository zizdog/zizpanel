package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/notify"
)

// fakeNotifySink 记账用的假通道：单测绝不许真的弹系统通知或发网络请求。
type fakeNotifySink struct {
	name string
	err  error
	got  []notify.Event
}

func (f *fakeNotifySink) Name() string { return f.name }
func (f *fakeNotifySink) Send(_ context.Context, ev notify.Event) error {
	f.got = append(f.got, ev)
	return f.err
}

// notifyTestFacts 造成"一个服务崩了 + 一张证书快到期 + 磁盘 95%"的现场。
func notifyTestFacts() notifyFacts {
	return notifyFacts{
		Services: []notify.ServiceStatus{
			{Name: "aria2", Display: "Aria2", Status: "error", Detail: "launchd 说它退出了\n第二行"},
			{Name: "nginx", Display: "Nginx", Status: "running", Checked: true, HealthOK: true},
			{Name: "idle", Display: "用户停掉的", Status: "stopped"},
		},
		DiskMount:   "/",
		DiskPercent: 95,
		DiskOK:      true,
	}
}

func postJSON(t *testing.T, h http.HandlerFunc, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON（%d）：%s", rec.Code, rec.Body.String())
	}
	return rec, out
}

// TestNotifySettingsValidation 非法设置必须给 400 + 人话（不许默默钳值）。
func TestNotifySettingsValidation(t *testing.T) {
	cases := []struct {
		name string
		in   notifySettings
		bad  bool
		want string // bad 时错误里必须出现的词
	}{
		{"开启但没通道", notifySettings{Enabled: true}, true, "通道"},
		{"webhook 少协议", notifySettings{Enabled: true, Webhook: "example.com/hook"}, true, "http"},
		{"webhook 是文件路径", notifySettings{Enabled: true, Webhook: "file:///etc/passwd"}, true, "http"},
		{"间隔太长", notifySettings{Enabled: true, MacOS: true, IntervalMins: 2000}, true, "间隔"},
		{"磁盘阈值太松", notifySettings{Enabled: true, MacOS: true, DiskPercent: 20}, true, "50~99"},
		{"合法", notifySettings{Enabled: true, MacOS: true}, false, ""},
		{"只 webhook", notifySettings{Enabled: true, Webhook: "https://example.com/hook"}, false, ""},
		{"关闭时不要求通道", notifySettings{Enabled: false}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := validateNotifySettings(c.in)
			if c.bad {
				if err == nil {
					t.Fatalf("应当拒绝，却通过了：%+v", out)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("错误里应有 %q，实际 %q", c.want, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("不该被拒绝：%v", err)
			}
			// 缺省值必须补上，否则存进配置的就是 0（下一轮巡检立刻跑、且阈值全 0）
			if out.IntervalMins <= 0 || out.CertDays <= 0 || out.DiskPercent <= 0 {
				t.Errorf("缺省值没补：%+v", out)
			}
		})
	}
}

// TestNotifySettingsSavedAndReloaded 保存后：落配置、生效到通知器、GET 回读一致。
func TestNotifySettingsSavedAndReloaded(t *testing.T) {
	srv, _ := newTestServer(t)
	sink := &fakeNotifySink{name: "fake"}
	srv.notifySinkMaker = func(notify.Options) []notify.Sink { return []notify.Sink{sink} }

	rec, out := postJSON(t, srv.handleNotifySettings,
		`{"enabled":true,"macos":false,"webhook":"https://example.com/hook","interval_mins":30,"cert_days":7,"disk_percent":80}`)
	if rec.Code != 200 {
		t.Fatalf("保存应当成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if okv, _ := out["ok"].(bool); !okv {
		t.Fatalf("响应应当是 ok：%s", rec.Body.String())
	}
	if !srv.Cfg.NotifyEnabled || srv.Cfg.NotifyWebhook != "https://example.com/hook" {
		t.Errorf("配置没写进去：%+v", srv.notifySettingsView())
	}
	if srv.Cfg.NotifyIntervalMins != 30 || srv.Cfg.NotifyCertDays != 7 || srv.Cfg.NotifyDiskPercent != 80 {
		t.Errorf("数值没写进去：%+v", srv.notifySettingsView())
	}
	// 落盘：重新 Load 也要能读回来（否则重启就丢）
	reloaded, err := config.Load(srv.Cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.NotifyWebhook != "https://example.com/hook" || !reloaded.NotifyEnabled {
		t.Errorf("没有落盘，重启会丢：%+v", reloaded)
	}
	// 通知器已经按新配置生效
	if o := srv.notifier().Options(); !o.Enabled || o.WebhookURL != "https://example.com/hook" {
		t.Errorf("通知器没跟着换配置：%+v", o)
	}
	// 非法请求不许改已有配置
	rec, _ = postJSON(t, srv.handleNotifySettings, `{"enabled":true,"macos":false,"webhook":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法设置应当 400，实际 %d", rec.Code)
	}
	if !srv.Cfg.NotifyEnabled || srv.Cfg.NotifyWebhook == "" {
		t.Error("400 之后旧配置被改坏了")
	}
}

// TestNotifyCheckSendsDiscoveredProblems 巡检 → 规则 → 发送的整条链路（假事实 + 假通道）。
func TestNotifyCheckSendsDiscoveredProblems(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Cfg.NotifyEnabled = true
	srv.Cfg.NotifyMacOS = true
	srv.Cfg.NotifyDiskPercent = 90
	srv.Cfg.NotifyCertDays = 14
	sink := &fakeNotifySink{name: "fake"}
	srv.notifySinkMaker = func(notify.Options) []notify.Sink { return []notify.Sink{sink} }
	srv.notifyFactsFn = func(context.Context) notifyFacts { return notifyTestFacts() }

	rec, out := postJSON(t, srv.handleNotifyCheck, `{}`)
	if rec.Code != 200 {
		t.Fatalf("巡检应当成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应缺少 data：%s", rec.Body.String())
	}
	if sent, _ := data["sent"].(float64); sent != 2 {
		t.Errorf("应当发出 2 条（服务崩了 + 磁盘 95%%），实际 %v：%s", data["sent"], rec.Body.String())
	}
	if len(sink.got) != 2 {
		t.Fatalf("假通道应当收到 2 条，实际 %d", len(sink.got))
	}
	if sink.got[0].Key != "service-down:aria2" {
		t.Errorf("第一条应当是服务异常，实际 %+v", sink.got[0])
	}
	if strings.Contains(sink.got[0].Body, "\n") {
		t.Errorf("通知正文只留第一行：%q", sink.got[0].Body)
	}
	if sink.got[1].Key != "disk-high:/" || sink.got[1].Level != notify.LevelWarn {
		t.Errorf("第二条应当是磁盘告警，实际 %+v", sink.got[1])
	}
	// 同一个现场再巡检一次：冷却期内不重复打扰
	_, out2 := postJSON(t, srv.handleNotifyCheck, `{}`)
	d2, _ := out2["data"].(map[string]any)
	if sent, _ := d2["sent"].(float64); sent != 0 {
		t.Errorf("冷却期内不该重复发，实际 %v", d2["sent"])
	}
	if skipped, _ := d2["skipped"].(float64); skipped != 2 {
		t.Errorf("应当如实报 2 条被冷却跳过，实际 %v", d2["skipped"])
	}
	if len(sink.got) != 2 {
		t.Errorf("冷却期内通道不该再被调用，实际 %d 次", len(sink.got))
	}
}

// TestNotifyDisabledListsButDoesNotSend 通知没开时，"立即检查"仍要如实列出问题（有用），
// 但一条都不许发。
func TestNotifyDisabledListsButDoesNotSend(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Cfg.NotifyEnabled = false
	sink := &fakeNotifySink{name: "fake"}
	srv.notifySinkMaker = func(notify.Options) []notify.Sink { return []notify.Sink{sink} }
	srv.notifyFactsFn = func(context.Context) notifyFacts { return notifyTestFacts() }

	_, out := postJSON(t, srv.handleNotifyCheck, `{}`)
	data, _ := out["data"].(map[string]any)
	events, _ := data["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("关闭时也要列出问题，实际 %v", data["events"])
	}
	if note, _ := data["note"].(string); !strings.Contains(note, "没有发送") {
		t.Errorf("必须如实说明没有发送，实际 %q", note)
	}
	if len(sink.got) != 0 {
		t.Error("通知关闭时一条都不许发")
	}
}

// TestNotifyTestReportsChannelFailure 测试通知要逐通道如实报结果（"发成功了"不许瞎报）。
func TestNotifyTestReportsChannelFailure(t *testing.T) {
	srv, _ := newTestServer(t)
	sink := &fakeNotifySink{name: "macos", err: errors.New("osascript 失败：没有用户会话")}
	srv.notifySinkMaker = func(notify.Options) []notify.Sink { return []notify.Sink{sink} }

	rec, out := postJSON(t, srv.handleNotifyTest, `{"enabled":true,"macos":true}`)
	if rec.Code != 200 {
		t.Fatalf("应当返回 200（接口本身成功，通道失败在结果里），实际 %d", rec.Code)
	}
	data, _ := out["data"].(map[string]any)
	results, _ := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("应当有 1 条通道结果：%s", rec.Body.String())
	}
	first, _ := results[0].(map[string]any)
	if okv, _ := first["ok"].(bool); okv {
		t.Errorf("通道失败不许报成功：%+v", first)
	}
	if msg, _ := first["error"].(string); !strings.Contains(msg, "osascript") {
		t.Errorf("失败原因要如实带回来，实际 %q", msg)
	}

	// 一个通道都没有：400 + 人话
	srv.notifySinkMaker = func(notify.Options) []notify.Sink { return nil }
	rec, _ = postJSON(t, srv.handleNotifyTest, `{"enabled":true,"macos":false,"webhook":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("没有通道应当 400，实际 %d", rec.Code)
	}
}

// TestNotifyGetExposesSettingsAndRecent 设置页要拿到设置、通道可用性、最近事件。
func TestNotifyGetExposesSettingsAndRecent(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Cfg.NotifyEnabled = true
	srv.Cfg.NotifyIntervalMins = 20
	srv.notifySinkMaker = func(notify.Options) []notify.Sink { return nil }
	if _, err := srv.notifier().Send(context.Background(), notify.Event{Key: "k", Title: "磁盘快满了"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.handleNotifyGet(rec, httptest.NewRequest(http.MethodGet, "/api/v1/notify", nil))
	if rec.Code != 200 {
		t.Fatalf("应当成功，实际 %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	data, _ := out["data"].(map[string]any)
	st, _ := data["settings"].(map[string]any)
	if en, _ := st["enabled"].(bool); !en {
		t.Errorf("设置没带出来：%+v", st)
	}
	if mins, _ := st["interval_mins"].(float64); mins != 20 {
		t.Errorf("间隔没带出来：%+v", st)
	}
	recent, _ := data["recent"].([]any)
	if len(recent) != 1 {
		t.Errorf("最近事件应当有 1 条：%+v", data["recent"])
	}
	mac, _ := data["macos"].(map[string]any)
	if _, has := mac["ready"]; !has {
		t.Errorf("通道可用性没带出来：%+v", mac)
	}
}
