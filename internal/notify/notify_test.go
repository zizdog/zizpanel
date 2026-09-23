package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	name string
	err  error
	got  []Event
	mu   sync.Mutex
}

func (f *fakeSink) Name() string { return f.name }
func (f *fakeSink) Send(_ context.Context, ev Event) error {
	f.mu.Lock()
	f.got = append(f.got, ev)
	f.mu.Unlock()
	return f.err
}
func (f *fakeSink) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

// TestDisabledSendsNothing 默认关闭：不打扰用户。
func TestDisabledSendsNothing(t *testing.T) {
	sink := &fakeSink{name: "fake"}
	n := New(Options{Enabled: false}, sink)
	sent, err := n.Send(context.Background(), Event{Key: "k", Title: "t"})
	if sent || err != nil {
		t.Fatalf("未启用时不该发也不该报错：sent=%v err=%v", sent, err)
	}
	if sink.count() != 0 {
		t.Error("未启用时不该调用通道")
	}
}

// TestCooldownDedupes 同一个 key 在冷却期内只发一次（否则每几分钟一轮就是刷屏）。
func TestCooldownDedupes(t *testing.T) {
	sink := &fakeSink{name: "fake"}
	n := New(Options{Enabled: true, Cooldown: 30 * time.Minute}, sink)
	now := time.Now()
	n.Now = func() time.Time { return now }

	if sent, _ := n.Send(context.Background(), Event{Key: "service-down:aria2", Title: "挂了"}); !sent {
		t.Fatal("第一条应当发出去")
	}
	now = now.Add(5 * time.Minute)
	if sent, _ := n.Send(context.Background(), Event{Key: "service-down:aria2", Title: "挂了"}); sent {
		t.Error("冷却期内不该重复发")
	}
	now = now.Add(31 * time.Minute)
	if sent, _ := n.Send(context.Background(), Event{Key: "service-down:aria2", Title: "挂了"}); !sent {
		t.Error("冷却期过了应当再发一次（服务还在挂，用户需要知道）")
	}
	if sink.count() != 2 {
		t.Errorf("通道应当收到 2 条，实际 %d", sink.count())
	}
	// 不同 key 互不影响
	if sent, _ := n.Send(context.Background(), Event{Key: "cert-expiring:a.com", Title: "证书"}); !sent {
		t.Error("不同 key 不该被去重")
	}
}

// TestSendReportsChannelFailure 通道失败必须如实返回（"我以为通知了"比没通知更糟）。
func TestSendReportsChannelFailure(t *testing.T) {
	bad := &fakeSink{name: "bad", err: errors.New("连接被拒")}
	good := &fakeSink{name: "good"}
	n := New(Options{Enabled: true}, bad, good)
	sent, err := n.Send(context.Background(), Event{Key: "k", Title: "t"})
	if !sent {
		t.Fatal("至少一个通道在工作，应当算发出去")
	}
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("失败通道要写进错误里，实际 %v", err)
	}
	if good.count() != 1 {
		t.Error("好通道应当照常收到")
	}
}

// TestRecentKeepsNewestFirst 最近事件列表（界面用）最新在前、有条数上限。
func TestRecentKeepsNewestFirst(t *testing.T) {
	n := New(Options{Enabled: true, Cooldown: time.Nanosecond})
	for i := 0; i < 60; i++ {
		_, _ = n.Send(context.Background(), Event{Key: string(rune('a'+i%26)) + string(rune('0'+i/26)), Title: "t"})
	}
	got := n.Recent()
	if len(got) != 50 {
		t.Errorf("应当只留 50 条，实际 %d", len(got))
	}
}

// TestWebhookSinkPostsJSON 通道二：真的 POST 一份 JSON 出去（用 httptest 收）。
func TestWebhookSinkPostsJSON(t *testing.T) {
	var got string
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctype = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		got = string(buf)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	sink := WebhookSink{URL: srv.URL, Client: srv.Client()}
	if err := sink.Send(context.Background(), Event{Key: "k", Title: "服务异常", Body: "细节", Level: LevelErr}); err != nil {
		t.Fatalf("webhook 应当成功：%v", err)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", ctype)
	}
	for _, want := range []string{"zizpanel", "服务异常", "细节", "error"} {
		if !strings.Contains(got, want) {
			t.Errorf("webhook 载荷里应有 %q，实际 %s", want, got)
		}
	}
	// 非 2xx 必须报错（不许把失败当成功）
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if err := (WebhookSink{URL: bad.URL, Client: bad.Client()}).Send(context.Background(), Event{Title: "t"}); err == nil {
		t.Error("webhook 返回 500 时必须报错")
	}
	// 没配 URL：什么都不做，也不算失败
	if err := (WebhookSink{}).Send(context.Background(), Event{Title: "t"}); err != nil {
		t.Errorf("没配 webhook 不该报错：%v", err)
	}
}

// TestMacOSSinkQuotesSafely 本机通知：AppleScript 字符串要转义（引号/换行会把脚本弄坏）。
func TestMacOSSinkQuotesSafely(t *testing.T) {
	var script string
	sink := MacOSSink{Run: func(_ context.Context, name string, args ...string) error {
		if name != "/usr/bin/osascript" {
			t.Errorf("应当调用 osascript，实际 %q", name)
		}
		script = strings.Join(args, " ")
		return nil
	}}
	ev := Event{Title: `服务 "X" 异常`, Body: "第一行\n第二行"}
	if err := sink.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, `"X" 异常"`) && strings.Count(script, `\"`) < 2 {
		t.Errorf("引号没有被转义：%s", script)
	}
	if strings.Contains(script, "第一行\n第二行") {
		t.Errorf("换行应当被压平（AppleScript 字面量里换行会截断）：%q", script)
	}
	if !strings.Contains(script, "display notification") {
		t.Errorf("脚本形状不对：%s", script)
	}
}

// TestMacOSSinkUsesUserSessionWhenRoot 面板是 root LaunchDaemon 时，直接 osascript
// 弹不出通知（不在用户的 Aqua 会话里）——必须 `launchctl asuser <uid>`。
func TestMacOSSinkUsesUserSessionWhenRoot(t *testing.T) {
	var gotBin string
	var gotArgs []string
	sink := MacOSSink{
		UID:    501,
		IsRoot: func() bool { return true },
		Run: func(_ context.Context, name string, args ...string) error {
			gotBin, gotArgs = name, args
			return nil
		},
	}
	if err := sink.Send(context.Background(), Event{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if gotBin != "/bin/launchctl" || len(gotArgs) < 2 || gotArgs[0] != "asuser" || gotArgs[1] != "501" {
		t.Errorf("root 下应当走 launchctl asuser 501，实际 %s %v", gotBin, gotArgs)
	}
	// 非 root（本地调试直接跑面板）时不该套 launchctl —— 那会直接失败。
	plain := MacOSSink{UID: 501, IsRoot: func() bool { return false },
		Run: func(_ context.Context, name string, args ...string) error { gotBin = name; return nil }}
	if err := plain.Send(context.Background(), Event{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if gotBin != "/usr/bin/osascript" {
		t.Errorf("非 root 应当直接用 osascript，实际 %s", gotBin)
	}
}

// TestSetSinksKeepsCooldown 换通道不该让用户被同一件事再刷一遍。
func TestSetSinksKeepsCooldown(t *testing.T) {
	a := &fakeSink{name: "a"}
	n := New(Options{Enabled: true, Cooldown: time.Hour}, a)
	if sent, _ := n.Send(context.Background(), Event{Key: "k", Title: "t"}); !sent {
		t.Fatal("第一条应当发出去")
	}
	b := &fakeSink{name: "b"}
	n.SetSinks(b)
	if sent, _ := n.Send(context.Background(), Event{Key: "k", Title: "t"}); sent {
		t.Error("换通道后仍应在冷却期内去重")
	}
	if a.count() != 1 || b.count() != 0 {
		t.Errorf("新旧通道调用次数不对：a=%d b=%d", a.count(), b.count())
	}
}

// TestCapEventsSummarizesOverflow 溢出合并：宁可少发，也不能把用户刷到关掉通知。
func TestCapEventsSummarizesOverflow(t *testing.T) {
	mk := func(n int) []Event {
		out := make([]Event, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Event{Key: string(rune('a' + i)), Title: "t"})
		}
		return out
	}
	if got := CapEvents(mk(3), 5); len(got) != 3 {
		t.Errorf("没到上限不该动，实际 %d", len(got))
	}
	got := CapEvents(mk(8), 5)
	if len(got) != 6 {
		t.Fatalf("8 条 + 上限 5 应当是 5 条 + 1 条汇总，实际 %d", len(got))
	}
	last := got[len(got)-1]
	if last.Key != "notify-overflow" || last.Level != LevelWarn || !strings.Contains(last.Title, "3") {
		t.Errorf("汇总条不对：%+v", last)
	}
}

// TestRulesServiceDown 规则：只报"该跑没跑好"，不报用户自己停的。
func TestRulesServiceDown(t *testing.T) {
	evs := RulesServiceDown([]ServiceStatus{
		{Name: "a", Display: "A", Status: "running", Checked: true, HealthOK: true},
		{Name: "b", Display: "B", Status: "running", Checked: true, HealthOK: false, Detail: "RPC 无应答\n更多细节"},
		{Name: "c", Display: "C", Status: "error", Detail: "launchd 说它退出了"},
		{Name: "d", Display: "D", Status: "stopped"},
		{Name: "e", Display: "E", Status: "unknown"},
		{Name: "f", Display: "F", Status: "unavailable"},
	})
	if len(evs) != 3 {
		t.Fatalf("应当报 3 条（b/c/f），实际 %d：%+v", len(evs), evs)
	}
	if evs[0].Key != "service-unhealthy:b" || evs[0].Level != LevelErr {
		t.Errorf("第一条应当是「在跑但不健康」，实际 %+v", evs[0])
	}
	if strings.Contains(evs[0].Body, "\n") {
		t.Error("正文只留第一行（细节进日志，不进通知）")
	}
	for _, e := range evs {
		if e.Key == "service-down:d" || e.Key == "service-down:e" {
			t.Error("用户自己停掉的/还没探测的服务不该报")
		}
	}
}

// TestRulesCertAndDisk 证书与磁盘规则（含边界）。
func TestRulesCertAndDisk(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.Local)
	evs := RulesCertExpiring([]CertStatus{
		{Domain: "soon.test", NotAfter: now.Add(10 * 24 * time.Hour)},
		{Domain: "far.test", NotAfter: now.Add(200 * 24 * time.Hour)},
		{Domain: "expired.test", NotAfter: now.Add(-2 * time.Hour)},
		{Domain: "zero.test"},
	}, 14, now)
	if len(evs) != 2 {
		t.Fatalf("应当报 2 条（soon + expired），实际 %d：%+v", len(evs), evs)
	}
	if evs[0].Level != LevelWarn || evs[1].Level != LevelErr {
		t.Errorf("级别不对：%+v", evs)
	}
	if evs[0].Key != "cert-expiring:soon.test" {
		t.Errorf("去重键不对：%+v", evs[0])
	}
	if got := RulesDiskUsage("/", 89, 90); got != nil {
		t.Errorf("没到阈值不该报：%+v", got)
	}
	if got := RulesDiskUsage("/", 90, 90); len(got) != 1 || got[0].Key != "disk-high:/" {
		t.Errorf("到阈值要报：%+v", got)
	}
	if got := RulesDiskUsage("/Data", 95, 0); len(got) != 1 {
		t.Errorf("threshold=0 时用默认 90：%+v", got)
	}
}
