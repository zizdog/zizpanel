// Package offsite 把本地备份发到异地：SMTP 邮件附件，或 FTP / FTPS 上传。
//
// 只依赖标准库。所有"拨号"都走 Sender.DialContext 注入点，
// 门禁用进程内假服务器即可覆盖，不碰真实网络与真实服务。
//
// 口令只存在于 config.json：这里既不打印也不写进任何错误文本（Send/Test
// 的出口统一过 scrub）。
package offsite

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 协议与加密方式。
const (
	ProtocolSMTP = "smtp"
	ProtocolFTP  = "ftp"
	ProtocolFTPS = "ftps"

	EncNone        = "none"
	EncSTARTTLS    = "starttls"
	EncSSL         = "ssl"
	EncExplicitTLS = "explicit_tls"
)

// 账本条目状态。只有 sent 才算"已发送"，failed/skipped 下次仍会重试。
const (
	StatusSent    = "sent"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// 日志级别（与任务中心的字符串一致，便于直接透传）。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelOK    = "ok"
)

// DefaultMaxFileMB 是单文件大小上限的默认值（MB）。
const DefaultMaxFileMB = 20

const defaultTimeout = 90 * time.Second

// Settings 是异地备份配置（**含口令**，因此只在 config.json 里落地）。
type Settings struct {
	Enabled  bool   `json:"enabled"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	// Encryption：smtp 用 none/starttls/ssl；ftp/ftps 用 none/explicit_tls。
	Encryption string `json:"encryption"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	// SMTP 专用。
	From          string `json:"from"`
	To            string `json:"to"`
	SubjectPrefix string `json:"subject_prefix"`
	// FTP 专用。
	RemoteDir string `json:"remote_dir"`
	// MaxFileMB 是单文件大小上限（SMTP 必填）：超过的文件不发送、绝不截断。
	MaxFileMB int64 `json:"max_file_mb"`
	// Insecure 允许自签名证书（家用 NAS 的 FTPS / 自建 SMTP 常见）。
	Insecure bool `json:"insecure_skip_verify"`
}

// DefaultSettings 返回一份默认配置。
func DefaultSettings() Settings {
	return Settings{
		Protocol: ProtocolSMTP, Encryption: EncNone, Port: 25,
		SubjectPrefix: "[ZizPanel 备份] ", MaxFileMB: DefaultMaxFileMB,
	}
}

// Normalize 补齐默认值并规范化（不校验必填项）。
func (st *Settings) Normalize() {
	st.Protocol = strings.ToLower(strings.TrimSpace(st.Protocol))
	st.Encryption = strings.ToLower(strings.TrimSpace(st.Encryption))
	st.Host = strings.TrimSpace(st.Host)
	st.Username = strings.TrimSpace(st.Username)
	st.From = strings.TrimSpace(st.From)
	st.To = strings.TrimSpace(st.To)
	st.SubjectPrefix = strings.TrimSpace(st.SubjectPrefix)
	st.RemoteDir = strings.TrimSpace(st.RemoteDir)
	if st.Protocol == "" {
		st.Protocol = ProtocolSMTP
	}
	if st.Encryption == "" {
		st.Encryption = defaultEncryption(st.Protocol)
	}
	// ftps 的加密方式只有一种；写成 none 的（老配置/手改）按显式 TLS 处理。
	if st.Protocol == ProtocolFTPS {
		st.Encryption = EncExplicitTLS
	}
	if st.Port <= 0 {
		st.Port = defaultPort(st.Protocol, st.Encryption)
	}
	if st.MaxFileMB <= 0 {
		st.MaxFileMB = DefaultMaxFileMB
	}
	if st.Protocol == ProtocolSMTP {
		if st.From == "" {
			st.From = st.Username
		}
	}
}

// Validate 校验必填项；失败原因是给用户看的一句话。
func (st Settings) Validate() error {
	switch st.Protocol {
	case ProtocolSMTP:
		if st.Host == "" {
			return errors.New("请填写 SMTP 服务器地址")
		}
		if st.From == "" {
			return errors.New("请填写发件人地址")
		}
		if st.To == "" {
			return errors.New("请填写收件人地址")
		}
		switch st.Encryption {
		case EncNone, EncSTARTTLS, EncSSL:
		default:
			return fmt.Errorf("SMTP 加密方式不支持: %s", st.Encryption)
		}
	case ProtocolFTP, ProtocolFTPS:
		if st.Host == "" {
			return errors.New("请填写 FTP 服务器地址")
		}
		switch st.Encryption {
		case EncNone, EncExplicitTLS:
		default:
			return fmt.Errorf("FTP 加密方式不支持: %s", st.Encryption)
		}
	default:
		return fmt.Errorf("不支持的协议: %s", st.Protocol)
	}
	if st.Port <= 0 || st.Port > 65535 {
		return fmt.Errorf("端口不合法: %d", st.Port)
	}
	if st.MaxFileMB <= 0 {
		return errors.New("单文件大小上限必须大于 0（超过上限的文件不会发送）")
	}
	return nil
}

func defaultEncryption(protocol string) string {
	if protocol == ProtocolFTPS {
		return EncExplicitTLS
	}
	return EncNone
}

func defaultPort(protocol, encryption string) int {
	switch protocol {
	case ProtocolSMTP:
		switch encryption {
		case EncSSL:
			return 465
		case EncSTARTTLS:
			return 587
		}
		return 25
	default:
		return 21
	}
}

// ---------- 候选文件与账本 ----------

// BackupFile 是一个待发送的本地备份。
type BackupFile struct {
	Name    string
	Path    string
	Size    int64
	ModTime int64 // UnixNano
}

// Scan 列出备份目录里的 .tar.gz（按 mtime 从旧到新）。
func Scan(dir string) ([]BackupFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BackupFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupFile{
			Name: e.Name(), Path: filepath.Join(dir, e.Name()),
			Size: info.Size(), ModTime: info.ModTime().UnixNano(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime < out[j].ModTime })
	return out, nil
}

// Entry 是账本里的一条记录。
type Entry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mod_time"`
	Status   string `json:"status"`
	Protocol string `json:"protocol,omitempty"`
	SentAt   string `json:"sent_at,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Ledger 是"已发送账本"：重启不丢，键 = 文件名 + 大小 + mtime。
type Ledger struct {
	Entries map[string]Entry `json:"entries"`
}

// NewLedger 返回一个空账本。
func NewLedger() *Ledger { return &Ledger{Entries: map[string]Entry{}} }

// LoadLedger 读取账本；文件不存在 = 空账本（不是错误）。
func LoadLedger(path string) (*Ledger, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewLedger(), nil
		}
		return nil, err
	}
	l := NewLedger()
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("账本文件损坏: %w", err)
	}
	if l.Entries == nil {
		l.Entries = map[string]Entry{}
	}
	return l, nil
}

// Save 原子写回账本（0600：里面有归档名与失败原因）。
func (l *Ledger) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Upsert 写入/覆盖一条记录。
func (l *Ledger) Upsert(e Entry) { l.Entries[e.Name] = e }

// IsSent 是"新增判据"：同名 + 同大小 + 同 mtime **且**上次成功，才算已发送。
//
// 大小或 mtime 变了 ⇒ 视为新版本，要重发（同名覆盖的归档必须重发）。
// status != sent（失败/超限）不算已发送 ⇒ 下次仍会重试。
func (l *Ledger) IsSent(name string, size, modTime int64) bool {
	if l == nil {
		return false
	}
	e, ok := l.Entries[name]
	return ok && e.Status == StatusSent && e.Size == size && e.ModTime == modTime
}

// List 返回账本条目（按时间倒序，同序按文件名）。
func (l *Ledger) List() []Entry {
	if l == nil {
		return []Entry{}
	}
	out := make([]Entry, 0, len(l.Entries))
	for _, e := range l.Entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SentAt != out[j].SentAt {
			return out[i].SentAt > out[j].SentAt
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Pending 返回账本里没有成功记录的备份（"新增"）。
func Pending(files []BackupFile, l *Ledger) []BackupFile {
	var out []BackupFile
	for _, f := range files {
		if !l.IsSent(f.Name, f.Size, f.ModTime) {
			out = append(out, f)
		}
	}
	return out
}

// ---------- 发送 ----------

// Options 控制一次发送。
type Options struct {
	// Force 忽略账本（「重发」用）。
	Force bool
	// Only 只发这一个文件名（重发单条）；空 = 全部新增。
	Only string
}

// Attempt 是一条失败/跳过的如实记录。
type Attempt struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Result 是一次发送的汇总（成功/失败/跳过逐条列清）。
type Result struct {
	Total     int       `json:"total"`
	Sent      []string  `json:"sent"`
	Failed    []Attempt `json:"failed"`
	Skipped   []Attempt `json:"skipped"`
	SentBytes int64     `json:"sent_bytes"`
	NoNew     bool      `json:"no_new"`
}

// ProgressFunc 上报结构化进度：当前文件已传 done/total 字节，整体第 filesDone/filesTotal 个。
type ProgressFunc func(done, total int64, filesDone, filesTotal int, message string)

// LogFunc 记录一条人话日志（level 用 Level* 常量）。
type LogFunc func(level, text string)

// Sender 负责真正把文件发出去。零值即可用；测试注入 DialContext / TLSConfig。
type Sender struct {
	// DialContext 建立 TCP 连接（门禁在这里塞进程内假服务器）。
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSConfig 按配置生成 TLS 参数（门禁注入信任自签证书的 RootCAs）。
	TLSConfig func(st Settings) *tls.Config
	// Now 便于测试注入时间。
	Now func() time.Time
}

// DefaultSender 是生产用发送器（真拨号 + 真证书校验）。
func DefaultSender() *Sender { return &Sender{} }

func (s *Sender) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sender) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if s != nil && s.DialContext != nil {
		return s.DialContext(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (s *Sender) tlsConfig(st Settings) *tls.Config {
	if s != nil && s.TLSConfig != nil {
		return s.TLSConfig(st)
	}
	return &tls.Config{ServerName: st.Host, InsecureSkipVerify: st.Insecure, MinVersion: tls.VersionTLS12}
}

// Send 把备份目录里"新增"的归档逐个发出。
//
// 返回 error 只表示**整批无法开始**（配置不合法/备份目录读不到/被中断）；
// 单个文件的失败与超限在 Result 里逐条如实列出，不影响本地备份。
func (s *Sender) Send(ctx context.Context, st Settings, dir, ledgerPath string,
	opts Options, progress ProgressFunc, logf LogFunc) (*Result, error) {

	st.Normalize()
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, string) {}
	}
	if progress == nil {
		progress = func(int64, int64, int, int, string) {}
	}

	files, err := Scan(dir)
	if err != nil {
		return nil, fmt.Errorf("读取备份目录失败: %w", err)
	}
	led, lerr := LoadLedger(ledgerPath)
	if lerr != nil {
		// 账本坏了不能变成"什么都不发"：按全部新增处理并如实说明。
		logf(LevelWarn, "已发送账本读取失败，本次按全部新增处理："+scrub(lerr.Error(), st.Password))
		led = NewLedger()
	}

	var jobs []BackupFile
	for _, f := range files {
		if opts.Only != "" && f.Name != opts.Only {
			continue
		}
		if !opts.Force && led.IsSent(f.Name, f.Size, f.ModTime) {
			continue
		}
		jobs = append(jobs, f)
	}
	res := &Result{Total: len(jobs), Sent: []string{}, Failed: []Attempt{}, Skipped: []Attempt{}}
	if len(jobs) == 0 {
		res.NoNew = true
		return res, nil
	}

	send1, cleanup := s.perFileSender(st)
	defer cleanup()

	finished := 0
	for _, f := range jobs {
		if err := ctx.Err(); err != nil {
			logf(LevelWarn, fmt.Sprintf("已中断：还有 %d 个文件没发（下次仍会发送）", len(jobs)-finished))
			return res, err
		}
		if st.MaxFileMB > 0 && f.Size > st.MaxFileMB*1024*1024 {
			reason := fmt.Sprintf("超过单文件上限 %d MB（该文件 %.1f MB），未发送；大文件请改用 FTP",
				st.MaxFileMB, float64(f.Size)/1024/1024)
			res.Skipped = append(res.Skipped, Attempt{Name: f.Name, Reason: reason})
			led.Upsert(Entry{Name: f.Name, Size: f.Size, ModTime: f.ModTime,
				Status: StatusSkipped, Protocol: st.Protocol, SentAt: s.now().Format(time.RFC3339), Error: reason})
			logf(LevelWarn, f.Name+"："+reason)
			_ = led.Save(ledgerPath)
			finished++
			continue
		}

		logf(LevelInfo, fmt.Sprintf("发送 %d/%d：%s（%.1f MB）", finished+1, len(jobs),
			f.Name, float64(f.Size)/1024/1024))
		perFile := func(done, total int64) {
			progress(done, total, finished, len(jobs),
				fmt.Sprintf("已发送 %d/%d · 当前：%s · 已传 %s/%s",
					finished, len(jobs), f.Name, humanBytes(done), humanBytes(total)))
		}
		if err := send1(ctx, st, f, perFile); err != nil {
			// 被中断（用户点了「中断」/任务超时）：**不记账**，下次仍会发送。
			// 把中断写成"失败"会在界面与账本里多一条假的失败记录。
			if ctx.Err() != nil {
				logf(LevelWarn, fmt.Sprintf("%s：已中断，未发送（下次仍会发送）", f.Name))
				return res, ctx.Err()
			}
			reason := scrub(err.Error(), st.Password)
			res.Failed = append(res.Failed, Attempt{Name: f.Name, Reason: reason})
			led.Upsert(Entry{Name: f.Name, Size: f.Size, ModTime: f.ModTime,
				Status: StatusFailed, Protocol: st.Protocol, SentAt: s.now().Format(time.RFC3339), Error: reason})
			logf(LevelError, fmt.Sprintf("%s：发送失败：%s", f.Name, reason))
		} else {
			res.Sent = append(res.Sent, f.Name)
			res.SentBytes += f.Size
			led.Upsert(Entry{Name: f.Name, Size: f.Size, ModTime: f.ModTime,
				Status: StatusSent, Protocol: st.Protocol, SentAt: s.now().Format(time.RFC3339), Bytes: f.Size})
			logf(LevelOK, fmt.Sprintf("已发送：%s（%.1f MB）", f.Name, float64(f.Size)/1024/1024))
		}
		_ = led.Save(ledgerPath)
		finished++
	}
	progress(0, 0, finished, len(jobs),
		fmt.Sprintf("已发送 %d/%d · 成功 %d · 失败 %d · 跳过 %d",
			finished, len(jobs), len(res.Sent), len(res.Failed), len(res.Skipped)))
	return res, nil
}

// perFileSender 返回"发一个文件"的闭包与收尾函数。
//
// FTP 复用同一条控制连接（每个文件重连一次太浪费）；SMTP 每个文件一封独立邮件。
func (s *Sender) perFileSender(st Settings) (func(context.Context, Settings, BackupFile, func(int64, int64)) error, func()) {
	if st.Protocol == ProtocolSMTP {
		return func(ctx context.Context, st Settings, f BackupFile, prog func(int64, int64)) error {
			return s.sendSMTP(ctx, st, f, prog)
		}, func() {}
	}
	var sess *ftpSession
	return func(ctx context.Context, st Settings, f BackupFile, prog func(int64, int64)) error {
			if sess == nil {
				var err error
				sess, err = s.ftpDial(ctx, st)
				if err != nil {
					return err
				}
			}
			file, err := os.Open(f.Path)
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			if err := sess.store(ctx, f.Name, file, f.Size, prog); err != nil {
				sess.close() // 连接可能已坏：下一个文件重连
				sess = nil
				return err
			}
			return nil
		}, func() {
			if sess != nil {
				sess.quit()
				sess = nil
			}
		}
}

// Test 测试连接：
//   - SMTP：发一封**没有附件**的测试邮件；
//   - FTP/FTPS：连接 + 登录 +（进远程目录）+ 写一个 1 字节临时文件再删掉。
func (s *Sender) Test(ctx context.Context, st Settings) (string, error) {
	st.Normalize()
	if err := st.Validate(); err != nil {
		return "", err
	}
	msg, err := s.testConn(ctx, st)
	if err != nil {
		return "", errors.New(scrub(err.Error(), st.Password))
	}
	return msg, nil
}

func (s *Sender) testConn(ctx context.Context, st Settings) (string, error) {
	if st.Protocol == ProtocolSMTP {
		return s.testSMTP(ctx, st)
	}
	return s.testFTP(ctx, st)
}

// scrub 把口令从任意文本里抹掉：服务器把口令回显在错误信息里也不能泄漏。
func scrub(s, secret string) string {
	if s == "" || secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "******")
}

func humanBytes(n int64) string {
	f := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// randomHex 生成随机十六进制串（Message-ID / 边界 / 临时文件名）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
