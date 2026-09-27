package offsite

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// smtp.go —— 最小 SMTP 发送实现（标准库 net/smtp + 自建 MIME 邮件）。
//
// 加密三档：none（仅内网）/ starttls / ssl（隐式 TLS，465）。
// 附件用 MIME base64（76 列折行），中文文件名按 RFC 2231 + RFC 2047 合法编码。

// sendSMTP 把单个归档作为附件发一封邮件。
func (s *Sender) sendSMTP(ctx context.Context, st Settings, f BackupFile, progress func(done, total int64)) error {
	c, closeFn, err := s.smtpClient(ctx, st)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := smtpEnvelope(c, st); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA 被拒绝: %w", err)
	}
	if err := writeAttachmentMessage(w, st, f, ctx, progress); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("发送邮件失败: %w", err)
	}
	return c.Quit()
}

// testSMTP 发一封**无附件**的测试邮件（用户要求：测试连接不许假装发过）。
func (s *Sender) testSMTP(ctx context.Context, st Settings) (string, error) {
	c, closeFn, err := s.smtpClient(ctx, st)
	if err != nil {
		return "", err
	}
	defer closeFn()
	if err := smtpEnvelope(c, st); err != nil {
		return "", err
	}
	w, err := c.Data()
	if err != nil {
		return "", fmt.Errorf("SMTP DATA 被拒绝: %w", err)
	}
	body := fmt.Sprintf("ZizPanel 异地备份测试邮件\n\n时间：%s\n这是一封没有附件的测试邮件；收到它说明 SMTP 配置可用。\n",
		time.Now().Format("2006-01-02 15:04:05"))
	head := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\nDate: %s\nMessage-ID: %s\n"+
		"MIME-Version: 1.0\nContent-Type: text/plain; charset=UTF-8\nContent-Transfer-Encoding: base64\n\n%s\n",
		st.From, st.To, encodeHeader(st.SubjectPrefix+"测试邮件"),
		time.Now().Format(time.RFC1123Z), messageID(st.Host), base64Wrap([]byte(body)))
	if _, err := io.WriteString(w, head); err != nil {
		_ = w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("发送测试邮件失败: %w", err)
	}
	if err := c.Quit(); err != nil {
		return "", fmt.Errorf("服务器在收下测试邮件后断开: %w", err)
	}
	return "测试邮件已发送到 " + st.To + "（无附件）", nil
}

// smtpClient 建立并登录一条 SMTP 连接。
func (s *Sender) smtpClient(ctx context.Context, st Settings) (*smtp.Client, func(), error) {
	addr := net.JoinHostPort(st.Host, strconv.Itoa(st.Port))
	conn, err := s.dial(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	setDeadline(conn, ctx)

	if st.Encryption == EncSSL {
		tconn := tls.Client(conn, s.tlsConfig(st))
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("SMTP TLS 握手失败: %w", err)
		}
		conn = tconn
	}
	c, err := smtp.NewClient(conn, st.Host)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("SMTP 握手失败: %w", err)
	}
	closeFn := func() { _ = c.Close() }

	if st.Encryption == EncSTARTTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			closeFn()
			return nil, nil, errors.New("服务器不支持 STARTTLS（请改用「ssl」或「none」）")
		}
		if err := c.StartTLS(s.tlsConfig(st)); err != nil {
			closeFn()
			return nil, nil, fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}
	if st.Username != "" {
		if err := c.Auth(plainAuth{user: st.Username, pass: st.Password}); err != nil {
			closeFn()
			return nil, nil, fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	return c, closeFn, nil
}

// plainAuth 是 AUTH PLAIN。
//
// 为什么不用 smtp.PlainAuth：它拒绝在非 TLS 连接上发口令，而内网 SMTP（加密=none）
// 是明确支持的场景。口令只发给用户自己填的服务器。
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if a.user == "" {
		return "", nil, errors.New("缺少用户名")
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("服务器要求额外的认证数据（不支持的认证机制）")
	}
	return nil, nil
}

// smtpEnvelope 下发 MAIL FROM / RCPT TO。
func smtpEnvelope(c *smtp.Client, st Settings) error {
	if err := c.Mail(addrOf(st.From)); err != nil {
		return fmt.Errorf("发件人被拒绝: %w", err)
	}
	rcpts := splitRecipients(st.To)
	if len(rcpts) == 0 {
		return errors.New("收件人为空")
	}
	for _, rcpt := range rcpts {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("收件人 %s 被拒绝: %w", rcpt, err)
		}
	}
	return nil
}

// writeAttachmentMessage 写一封 multipart/mixed 邮件（纯文本说明 + base64 归档附件）。
func writeAttachmentMessage(w io.Writer, st Settings, f BackupFile, ctx context.Context,
	progress func(int64, int64)) error {

	boundary := "zp-" + randomHex(12)
	subject := encodeHeader(st.SubjectPrefix + f.Name)
	note := fmt.Sprintf("ZizPanel 备份归档：%s（%s）\n发送时间：%s\n",
		f.Name, humanBytes(f.Size), time.Now().Format("2006-01-02 15:04:05"))
	head := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\nDate: %s\nMessage-ID: %s\n"+
		"MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=%q\n\n"+
		"--%s\nContent-Type: text/plain; charset=UTF-8\nContent-Transfer-Encoding: base64\n\n%s\n"+
		"--%s\nContent-Type: application/gzip\nContent-Transfer-Encoding: base64\n"+
		"Content-Disposition: attachment; filename=\"%s\"; filename*=UTF-8''%s\n\n",
		st.From, st.To, subject, time.Now().Format(time.RFC1123Z), messageID(st.Host),
		boundary, boundary, base64Wrap([]byte(note)),
		boundary, asciiFilename(f.Name), rfc2231(f.Name))
	if _, err := io.WriteString(w, head); err != nil {
		return err
	}
	if err := writeBase64File(ctx, w, f.Path, progress); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n--"+boundary+"--\n")
	return err
}

// writeBase64File 流式把文件按 base64（76 列）写入 w。
//
// 不截断：读多少写多少；ctx 取消时立刻返回（半截邮件由服务器侧丢弃）。
func writeBase64File(ctx context.Context, w io.Writer, path string, progress func(done, total int64)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	lw := &lineWrapWriter{w: w, width: 76}
	enc := base64.NewEncoder(base64.StdEncoding, lw)
	buf := make([]byte, 64*1024)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			_ = enc.Close()
			return err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := enc.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, st.Size())
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return enc.Close()
}

// lineWrapWriter 每 width 个字符插一个换行。
//
// net/smtp 的 DotWriter 会把 \n 统一转成 \r\n（并做点转义），
// 所以这里只用 \n，行宽也不会超 SMTP 的 998 字节上限。
type lineWrapWriter struct {
	w     io.Writer
	width int
	n     int
}

func (l *lineWrapWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if l.n >= l.width {
			if _, err := l.w.Write([]byte{'\n'}); err != nil {
				return 0, err
			}
			l.n = 0
		}
		if _, err := l.w.Write([]byte{b}); err != nil {
			return 0, err
		}
		l.n++
	}
	return len(p), nil
}

// base64Wrap 把一段短文本编码成 76 列 base64。
func base64Wrap(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var sb strings.Builder
	for len(s) > 76 {
		sb.WriteString(s[:76])
		sb.WriteByte('\n')
		s = s[76:]
	}
	sb.WriteString(s)
	return sb.String()
}

// setDeadline 按 ctx 的截止时刻设置连接超时（没有就用默认值）。
func setDeadline(conn net.Conn, ctx context.Context) {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(defaultTimeout))
}

// messageID 生成一个合法的 Message-ID。
func messageID(host string) string {
	if host == "" {
		host = "zizpanel.local"
	}
	return fmt.Sprintf("<%s.%s@%s>", strconv.FormatInt(time.Now().UnixNano(), 10), randomHex(6), host)
}

// encodeHeader 按 RFC 2047 编码非 ASCII 头（中文主题）。
//
// 每个编码字 ≤75 字符，超长就分段并用 CRLF+空格折叠（SMTP 头行上限 998 字节）。
func encodeHeader(s string) string {
	if isASCII(s) {
		return s
	}
	const maxBytesPerWord = 45 // 45 字节原文 → 60 字符 base64，加外壳仍 <75
	var parts []string
	var buf []byte
	flush := func() {
		if len(buf) == 0 {
			return
		}
		parts = append(parts, "=?UTF-8?B?"+base64.StdEncoding.EncodeToString(buf)+"?=")
		buf = nil
	}
	for _, r := range s {
		b := []byte(string(r))
		if len(buf)+len(b) > maxBytesPerWord {
			flush()
		}
		buf = append(buf, b...)
	}
	flush()
	return strings.Join(parts, "\n ")
}

// asciiFilename 生成 ASCII 回退文件名（老客户端只认 filename=）。
func asciiFilename(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r > 32 && r < 127 && r != '"' && r != '\\':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	out := sb.String()
	if out == "" {
		out = "backup.tar.gz"
	}
	return out
}

// rfc2231 按 RFC 2231/5987 百分号编码文件名（中文文件名的合法写法）。
func rfc2231(s string) string {
	const attrChars = "!#$&+-.^_`|~"
	var sb strings.Builder
	for _, b := range []byte(s) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
			sb.WriteByte(b)
		case strings.IndexByte(attrChars, b) >= 0:
			sb.WriteByte(b)
		default:
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}
	return sb.String()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 126 || s[i] < 32 {
			return false
		}
	}
	return true
}

// addrOf 从 "名字 <a@b>" 里取出裸地址（Mail/Rcpt 只接受裸地址）。
func addrOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return strings.TrimSpace(s[i+1 : i+j])
		}
	}
	return s
}

// splitRecipients 支持逗号/分号/空白分隔的多个收件人。
func splitRecipients(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	var out []string
	for _, f := range fields {
		if f = addrOf(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
