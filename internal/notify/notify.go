// Package notify 是很小的"主动通知"层：面板发现异常时**主动说一声**，
// 而不是等用户自己点进来看。
//
// 设计取舍：
//
//	· 通道只有两个（够用且都不引入第三方服务）：macOS 本机通知（osascript）、Webhook（POST JSON）；
//	· 默认**关闭**：会打扰用户的东西不许默认打开；
//	· **去重 + 冷却**：同一个 key（例如"服务 X 掉线"）在冷却期内只发一次 ——
//	  面板每几分钟检查一轮，不去重就会变成刷屏（用户第一件事就是把它关掉）；
//	· 发送失败**如实返回**，由调用方记日志；绝不吞掉（"我以为通知了"比没通知更糟）。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level 决定通知的措辞与 Webhook 里的 level 字段。
type Level string

const (
	LevelInfo Level = "info"
	LevelWarn Level = "warn"
	LevelErr  Level = "error"
)

// Event 是一条通知。
type Event struct {
	// Key 是去重键（例如 "service-down:com.zizdog.aria2"、"cert-expiring:example.com"）。
	Key string `json:"key"`
	// Title 是给人看的一句话（网易云式短句：中文、不超过 40 字）。
	Title string `json:"title"`
	// Body 是细节（可为空）。
	Body  string `json:"body,omitempty"`
	Level Level  `json:"level"`
	At    string `json:"at"`
}

// Options 是通知器的配置（由面板设置页写入）。
type Options struct {
	Enabled bool
	// macOS 本机通知：走 osascript，不需要任何凭据。
	MacOSNotify bool
	// WebhookURL 非空就 POST 一份 JSON 过去（企业微信/飞书/Slack 的中转、自建脚本都行）。
	WebhookURL string
	// Cooldown 是同一个 key 的冷却时间（默认 30 分钟）。
	Cooldown time.Duration
}

// Sink 是发送通道。做成接口是为了单测能注入假通道（绝不在单测里弹通知/发网络请求）。
type Sink interface {
	Name() string
	Send(ctx context.Context, ev Event) error
}

// Notifier 串起"去重 + 多通道发送 + 最近事件"。
type Notifier struct {
	mu     sync.Mutex
	opts   Options
	sinks  []Sink
	lastAt map[string]time.Time
	recent []Event
	Now    func() time.Time // 注入口，单测用来控制时间
}

// New 造一个通知器。
func New(opts Options, sinks ...Sink) *Notifier {
	if opts.Cooldown <= 0 {
		opts.Cooldown = 30 * time.Minute
	}
	return &Notifier{opts: opts, sinks: sinks, lastAt: map[string]time.Time{}, Now: time.Now}
}

// Options 返回当前配置（只读用途）。
func (n *Notifier) Options() Options {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.opts
}

// SetOptions 更新配置（面板设置保存后调用）。
func (n *Notifier) SetOptions(o Options) {
	if o.Cooldown <= 0 {
		o.Cooldown = 30 * time.Minute
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.opts = o
}

// SetSinks 换掉发送通道（设置页改了 Webhook 之后调用）。
// 去重状态与最近事件**保留** —— 换个通道不该让用户再被同一件事刷一遍。
func (n *Notifier) SetSinks(sinks ...Sink) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sinks = sinks
}

// Recent 返回最近的事件（最新在前，最多 50 条）——界面里"通知记录"用。
func (n *Notifier) Recent() []Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Event, len(n.recent))
	copy(out, n.recent)
	return out
}

// Send 发一条通知。返回 (sent, err)：
//   - sent=false 表示**被去重跳过**或"通知未启用"（都不算失败）；
//   - err 非空表示至少一个通道发失败（如实返回，绝不假装成功）。
func (n *Notifier) Send(ctx context.Context, ev Event) (bool, error) {
	n.mu.Lock()
	if !n.opts.Enabled {
		n.mu.Unlock()
		return false, nil
	}
	if ev.Key != "" {
		if last, ok := n.lastAt[ev.Key]; ok && n.Now().Sub(last) < n.opts.Cooldown {
			n.mu.Unlock()
			return false, nil
		}
		n.lastAt[ev.Key] = n.Now()
	}
	if ev.At == "" {
		ev.At = n.Now().Format(time.RFC3339)
	}
	n.recent = append([]Event{ev}, n.recent...)
	if len(n.recent) > 50 {
		n.recent = n.recent[:50]
	}
	sinks := n.sinks
	n.mu.Unlock()

	var errs []string
	for _, s := range sinks {
		if err := s.Send(ctx, ev); err != nil {
			errs = append(errs, s.Name()+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return true, fmt.Errorf("%s", strings.Join(errs, "；"))
	}
	return true, nil
}

// ---------------- 通道一：macOS 本机通知 ----------------

// MacOSSink 用 osascript 弹一条系统通知（无凭据、无第三方）。
type MacOSSink struct {
	// UID > 0 且面板以 root 跑时，改走 `launchctl asuser <uid>`：面板是 root
	// LaunchDaemon，**不在用户的 Aqua 会话里**，直接 osascript 弹不出任何东西
	// （坑：通知"发出成功"而屏幕上什么都没有）。
	UID int
	// Run 是注入口：单测替换成假命令，绝不在测试里真的弹窗。
	Run func(ctx context.Context, name string, args ...string) error
	// IsRoot 也是注入口（单测要能扮演 root）。
	IsRoot func() bool
}

func (s MacOSSink) Name() string { return "macos" }

func (s MacOSSink) Send(ctx context.Context, ev Event) error {
	script := fmt.Sprintf("display notification %s with title %s", applescriptQuote(ev.Body), applescriptQuote(ev.Title))
	run := s.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		}
	}
	isRoot := s.IsRoot
	if isRoot == nil {
		isRoot = func() bool { return os.Geteuid() == 0 }
	}
	bin, args := "/usr/bin/osascript", []string{"-e", script}
	if s.UID > 0 && isRoot() {
		bin = "/bin/launchctl"
		args = []string{"asuser", strconv.Itoa(s.UID), "/usr/bin/osascript", "-e", script}
	}
	if err := run(ctx, bin, args...); err != nil {
		return fmt.Errorf("%s 失败：%w", bin, err)
	}
	return nil
}

// applescriptQuote 把字符串安全塞进 AppleScript 字符串字面量。
func applescriptQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", " ")
	return "\"" + s + "\""
}

// ---------------- 通道二：Webhook ----------------

// WebhookSink 把事件 POST 成 JSON。
type WebhookSink struct {
	URL string
	// Client 注入口（单测用 httptest 的 client）。
	Client *http.Client
	// Timeout 默认 8 秒。
	Timeout time.Duration
}

func (s WebhookSink) Name() string { return "webhook" }

func (s WebhookSink) Send(ctx context.Context, ev Event) error {
	if strings.TrimSpace(s.URL) == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"source": "zizpanel",
		"event":  ev,
		"text":   ev.Title + textSuffix(ev.Body),
	})
	if err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("webhook 返回 HTTP %d", res.StatusCode)
	}
	return nil
}

func textSuffix(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return "：" + body
}
