package offsite

// 异地备份门禁：TestOffsiteGate。
//
// 全部用**进程内假服务器**（net.Listen 127.0.0.1:0 起最小 SMTP / 最小 FTP 应答器），
// 不连任何真实网络、真实邮箱、真实 FTP：
//   ① 账本判据：已发送不重发 / 新文件要发 / 大小或 mtime 变了要重发；
//   ② SMTP 真的发出一封带正确收件人与 base64 附件的邮件；超上限的文件不发送且被如实标记；
//   ③ FTP 真的登录 + STOR；ftps 真的先走 AUTH TLS；
//   ④ 口令不出现在日志与错误信息里（哪怕服务器把它回显在错误里）。
//
// 为什么此前没有任何门禁抓得到它：异地备份是这一轮才有的能力，
// 之前的备份只有"本地列表 / 恢复 / 下载 / 上传"，没有任何"向外发送"的判据。

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------- 测试证书（自签，只用于进程内假服务器） ----------------

func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "zizpanel-offsite-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// ---------------- 假 SMTP 服务器 ----------------

type smtpMsg struct {
	from   string
	rcpts  []string
	data   string
	user   string
	pass   string
	authed bool
}

type fakeSMTP struct {
	ln      net.Listener
	tlsConf *tls.Config
	pool    *x509.CertPool
	// mode: none / starttls / ssl
	mode string
	// authErr 非空时认证失败，并把它（连同口令）回给客户端。
	authErr string

	mu   sync.Mutex
	msgs []smtpMsg
}

func newFakeSMTP(t *testing.T, mode string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, mode: mode}
	if mode != "none" {
		cert, pool := testCert(t)
		f.tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}}
		f.pool = pool
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort() (string, int) {
	addr := f.ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func (f *fakeSMTP) messages() []smtpMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]smtpMsg, len(f.msgs))
	copy(out, f.msgs)
	return out
}

func (f *fakeSMTP) handle(raw net.Conn) {
	defer func() { _ = raw.Close() }()
	conn := raw
	if f.mode == "ssl" {
		tc := tls.Server(raw, f.tlsConf)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
	}
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	resp := func(s string) bool {
		if _, err := bw.WriteString(s + "\r\n"); err != nil {
			return false
		}
		return bw.Flush() == nil
	}
	if !resp("220 fake ESMTP ready") {
		return
	}
	var msg *smtpMsg
	newMsg := func() *smtpMsg {
		if msg == nil {
			msg = &smtpMsg{}
		}
		return msg
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			ext := "250-fake\r\n250-AUTH PLAIN LOGIN\r\n250 SIZE 52428800"
			if f.mode == "starttls" {
				ext = "250-fake\r\n250-STARTTLS\r\n250-AUTH PLAIN LOGIN\r\n250 SIZE 52428800"
			}
			if !resp(ext) {
				return
			}
		case up == "STARTTLS":
			if f.mode != "starttls" {
				resp("502 not supported")
				continue
			}
			if !resp("220 Go ahead") {
				return
			}
			tc := tls.Server(conn, f.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			br = bufio.NewReader(conn)
			bw = bufio.NewWriter(conn)
		case strings.HasPrefix(up, "AUTH PLAIN"):
			fields := strings.Fields(line)
			var user, pass string
			if len(fields) >= 3 {
				user, pass = decodePlainAuth(fields[2])
			} else {
				resp("334 ")
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				user, pass = decodePlainAuth(strings.TrimSpace(l))
			}
			if f.authErr != "" {
				resp("535 " + f.authErr)
				continue
			}
			m := newMsg()
			m.authed, m.user, m.pass = true, user, pass
			resp("235 2.7.0 ok")
		case strings.HasPrefix(up, "AUTH LOGIN"):
			resp("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			u, err := br.ReadString('\n')
			if err != nil {
				return
			}
			resp("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			p, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if f.authErr != "" {
				resp("535 " + f.authErr)
				continue
			}
			m := newMsg()
			m.authed = true
			ub, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
			pb, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
			m.user, m.pass = string(ub), string(pb)
			resp("235 2.7.0 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			newMsg().from = strings.Trim(strings.TrimSpace(line[len("MAIL FROM:"):]), "<>")
			resp("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			m := newMsg()
			m.rcpts = append(m.rcpts, strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>"))
			resp("250 ok")
		case up == "DATA":
			resp("354 end with .")
			var sb strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				l = strings.TrimRight(l, "\r\n")
				if l == "." {
					break
				}
				if strings.HasPrefix(l, "..") {
					l = l[1:]
				}
				sb.WriteString(l)
				sb.WriteString("\r\n")
			}
			m := newMsg()
			m.data = sb.String()
			f.mu.Lock()
			f.msgs = append(f.msgs, *m)
			f.mu.Unlock()
			msg = nil
			resp("250 queued")
		case up == "QUIT":
			resp("221 bye")
			return
		case up == "RSET":
			msg = nil
			resp("250 ok")
		default:
			resp("250 ok")
		}
	}
}

func decodePlainAuth(s string) (string, string) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", ""
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) >= 3 {
		return parts[1], parts[2]
	}
	return "", ""
}

// ---------------- 假 FTP 服务器 ----------------

type fakeFTP struct {
	ln      net.Listener
	tlsConf *tls.Config
	pool    *x509.CertPool
	dir     string
	// authErr 非空时登录失败，并把它（连同口令）回给客户端。
	authErr string

	mu      sync.Mutex
	authTLS bool
	user    string
	pass    string
	stors   []string
	stored  map[string][]byte
	deleted []string
}

func newFakeFTP(t *testing.T, dir string) *fakeFTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, pool := testCert(t)
	f := &fakeFTP{
		ln: ln, tlsConf: &tls.Config{Certificates: []tls.Certificate{cert}},
		pool: pool, dir: dir, stored: map[string][]byte{},
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeFTP) hostPort() (string, int) {
	addr := f.ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

type ftpSnapshot struct {
	authTLS bool
	user    string
	pass    string
	stors   []string
	stored  map[string][]byte
	deleted []string
}

func (f *fakeFTP) snapshot() ftpSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := map[string][]byte{}
	for k, v := range f.stored {
		stored[k] = v
	}
	return ftpSnapshot{
		authTLS: f.authTLS, user: f.user, pass: f.pass,
		stors: append([]string{}, f.stors...), stored: stored,
		deleted: append([]string{}, f.deleted...),
	}
}

func (f *fakeFTP) handle(raw net.Conn) {
	defer func() { _ = raw.Close() }()
	conn := raw
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	resp := func(s string) bool {
		if _, err := bw.WriteString(s + "\r\n"); err != nil {
			return false
		}
		return bw.Flush() == nil
	}
	reset := func(c net.Conn) {
		conn = c
		br = bufio.NewReader(c)
		bw = bufio.NewWriter(c)
	}
	if !resp("220 fake FTP ready") {
		return
	}
	protected := false
	var dataLn net.Listener
	defer func() {
		if dataLn != nil {
			_ = dataLn.Close()
		}
	}()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg := line, ""
		if i := strings.IndexByte(line, ' '); i > 0 {
			cmd, arg = line[:i], line[i+1:]
		}
		switch strings.ToUpper(cmd) {
		case "AUTH":
			if !strings.EqualFold(arg, "TLS") {
				resp("504 unknown AUTH")
				continue
			}
			if !resp("234 proceed with TLS") {
				return
			}
			tc := tls.Server(conn, f.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			reset(tc)
			f.mu.Lock()
			f.authTLS = true
			f.mu.Unlock()
		case "PBSZ":
			resp("200 PBSZ=0")
		case "PROT":
			if strings.EqualFold(arg, "P") {
				protected = true
			}
			resp("200 PROT ok")
		case "USER":
			f.mu.Lock()
			f.user = arg
			f.mu.Unlock()
			resp("331 need password")
		case "PASS":
			if f.authErr != "" {
				resp("530 " + f.authErr)
				continue
			}
			f.mu.Lock()
			f.pass = arg
			f.mu.Unlock()
			resp("230 logged in")
		case "TYPE":
			resp("200 type set")
		case "CWD":
			if f.dir != "" && strings.Trim(arg, "/") != strings.Trim(f.dir, "/") {
				resp("550 no such directory")
				continue
			}
			resp("250 cwd ok")
		case "PASV":
			if dataLn != nil {
				_ = dataLn.Close()
			}
			dl, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				resp("425 cannot open data port")
				continue
			}
			dataLn = dl
			p := dl.Addr().(*net.TCPAddr).Port
			resp(fmt.Sprintf("227 Entering Passive Mode (127,0,0,1,%d,%d)", p/256, p%256))
		case "STOR":
			if dataLn == nil {
				resp("425 use PASV first")
				continue
			}
			resp("150 opening data connection")
			dc, err := dataLn.Accept()
			if err != nil {
				return
			}
			if protected {
				tc := tls.Server(dc, f.tlsConf)
				if err := tc.Handshake(); err != nil {
					_ = dc.Close()
					return
				}
				dc = tc
			}
			body := readAllConn(dc)
			_ = dc.Close()
			f.mu.Lock()
			f.stors = append(f.stors, arg)
			f.stored[arg] = body
			f.mu.Unlock()
			resp("226 transfer complete")
		case "DELE":
			f.mu.Lock()
			f.deleted = append(f.deleted, arg)
			f.mu.Unlock()
			resp("250 deleted")
		case "QUIT":
			resp("221 bye")
			return
		default:
			resp("502 not implemented")
		}
	}
}

func readAllConn(c net.Conn) []byte {
	var out []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			return out
		}
	}
}

// ---------------- 测试辅助 ----------------

func writeBackup(t *testing.T, dir, name string, body []byte, at time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func smtpSettings(host string, port int, mode string) Settings {
	st := DefaultSettings()
	st.Enabled = true
	st.Protocol = ProtocolSMTP
	st.Host = host
	st.Port = port
	st.Encryption = mode
	st.Username = "mailer"
	st.Password = secretForTest
	st.From = "panel@example.com"
	st.To = "offsite@example.com"
	st.MaxFileMB = 20
	return st
}

func ftpSettings(host string, port int, protocol string) Settings {
	st := DefaultSettings()
	st.Enabled = true
	st.Protocol = protocol
	st.Host = host
	st.Port = port
	st.Username = "backupuser"
	st.Password = secretForTest
	st.RemoteDir = "/remote-backups"
	if protocol == ProtocolFTPS {
		st.Encryption = EncExplicitTLS
	} else {
		st.Encryption = EncNone
	}
	st.MaxFileMB = 20
	return st
}

const secretForTest = "s3cr3t-passw0rd"

// trustTestCert 让 Sender 信任假服务器的自签证书（生产默认严格校验）。
func trustTestCert(pool *x509.CertPool) *Sender {
	return &Sender{TLSConfig: func(st Settings) *tls.Config {
		return &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	}}
}

func extractAttachment(t *testing.T, raw string) (string, []byte) {
	t.Helper()
	raw = strings.ReplaceAll(raw, "\r\n", "\n") // SMTP 线路上是 CRLF
	bi := strings.Index(raw, `boundary="`)
	if bi < 0 {
		t.Fatalf("邮件里没有 multipart boundary:\n%s", raw)
	}
	rest := raw[bi+len(`boundary="`):]
	boundary := rest[:strings.Index(rest, `"`)]

	gi := strings.Index(raw, "application/gzip")
	if gi < 0 {
		t.Fatalf("邮件里没有附件段:\n%s", raw)
	}
	part := raw[gi:]
	hend := strings.Index(part, "\n\n")
	if hend < 0 {
		t.Fatalf("附件段没有头部结束空行:\n%s", part)
	}
	head := part[:hend]
	body := part[hend+2:]
	if j := strings.Index(body, "--"+boundary); j >= 0 {
		body = body[:j]
	}
	clean := strings.Join(strings.Fields(body), "")
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		t.Fatalf("附件不是合法 base64: %v", err)
	}
	return head, data
}

// ---------------- 门禁本体 ----------------

func TestOffsiteGate(t *testing.T) {
	// ---------- ① 账本判据 ----------
	t.Run("①账本判定", func(t *testing.T) {
		dir := t.TempDir()
		ledgerPath := filepath.Join(dir, "ledger.json")
		base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
		writeBackup(t, dir, "a.tar.gz", []byte("AAAA"), base)
		writeBackup(t, dir, "b.tar.gz", []byte("BBBBBB"), base.Add(time.Minute))

		sm := newFakeSMTP(t, "none")
		host, port := sm.hostPort()
		st := smtpSettings(host, port, EncNone)
		sender := DefaultSender()

		files, err := Scan(dir)
		if err != nil || len(files) != 2 {
			t.Fatalf("扫描备份目录失败: %v %v", files, err)
		}
		if n := len(Pending(files, NewLedger())); n != 2 {
			t.Fatalf("空账本时两个文件都算新增，实际 %d", n)
		}

		res, err := sender.Send(context.Background(), st, dir, ledgerPath, Options{}, nil, nil)
		if err != nil {
			t.Fatalf("首次发送失败: %v", err)
		}
		if len(res.Sent) != 2 {
			t.Fatalf("首次应发送 2 个，实际 %v（失败 %v）", res.Sent, res.Failed)
		}
		if got := len(sm.messages()); got != 2 {
			t.Fatalf("假服务器应收到 2 封邮件，实际 %d", got)
		}

		// 再发一次：账本里都发过了
		res, err = sender.Send(context.Background(), st, dir, ledgerPath, Options{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !res.NoNew || len(res.Sent) != 0 {
			t.Fatalf("已发送的不许重发，实际 NoNew=%v sent=%v", res.NoNew, res.Sent)
		}
		if got := len(sm.messages()); got != 2 {
			t.Fatalf("没有新增时不该再发邮件，假服务器收到 %d 封", got)
		}

		// 同名但 mtime 变了 ⇒ 视为新版本、要重发
		//（负向对照：删掉 mtime 判据这条必须红）
		later := base.Add(2 * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, "a.tar.gz"), later, later); err != nil {
			t.Fatal(err)
		}
		res, err = sender.Send(context.Background(), st, dir, ledgerPath, Options{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Sent) != 1 || res.Sent[0] != "a.tar.gz" {
			t.Fatalf("mtime 变了必须重发那一个（且只发那一个），实际 %v", res.Sent)
		}

		// 同名但大小变了 ⇒ 也要重发
		writeBackup(t, dir, "b.tar.gz", []byte("BBBBBBBBBB"), base.Add(time.Minute))
		res, err = sender.Send(context.Background(), st, dir, ledgerPath, Options{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Sent) != 1 || res.Sent[0] != "b.tar.gz" {
			t.Fatalf("大小变了必须重发那一个，实际 %v", res.Sent)
		}

		// 直接锁死"已发送"判据本身（大小与 mtime 都必须参与比较）
		led, err := LoadLedger(ledgerPath)
		if err != nil {
			t.Fatal(err)
		}
		e := led.Entries["a.tar.gz"]
		if !led.IsSent("a.tar.gz", e.Size, e.ModTime) {
			t.Fatal("同名+同大小+同 mtime 且已成功，必须判定为已发送")
		}
		if led.IsSent("a.tar.gz", e.Size, e.ModTime+1) {
			t.Fatal("mtime 变了就不算已发送（否则被覆盖过的同名归档永远不会重发）")
		}
		if led.IsSent("a.tar.gz", e.Size+1, e.ModTime) {
			t.Fatal("大小变了就不算已发送")
		}

		// 重启不丢：账本文件还在磁盘上，重新读出来记得每一个已发送的文件
		led2, err := LoadLedger(ledgerPath)
		if err != nil {
			t.Fatal(err)
		}
		files2, _ := Scan(dir)
		if len(files2) != 2 {
			t.Fatalf("备份目录应有两个文件，实际 %d", len(files2))
		}
		for _, f := range files2 {
			if !led2.IsSent(f.Name, f.Size, f.ModTime) {
				t.Fatalf("账本必须落盘且重启不丢：%s 读回来却是未发送", f.Name)
			}
		}

		// 取消不记账：任务被中断时一个文件都不发，也不写账本（下次仍会发）。
		cancelDir := t.TempDir()
		writeBackup(t, cancelDir, "c.tar.gz", []byte("CCCC"), base)
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		cLedger := filepath.Join(cancelDir, "ledger.json")
		if _, err := sender.Send(cctx, st, cancelDir, cLedger, Options{}, nil, nil); err == nil {
			t.Fatal("ctx 已取消时发送必须返回中断错误")
		}
		if _, err := os.Stat(cLedger); err == nil {
			t.Fatal("取消后不许写账本（写了下次就不会再发）")
		}
	})

	// ---------- ② SMTP 附件 + 大小上限 ----------
	t.Run("②SMTP附件与上限", func(t *testing.T) {
		dir := t.TempDir()
		ledgerPath := filepath.Join(dir, "ledger.json")
		base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
		body := []byte("ZIZPANEL-BACKUP-BYTES-0123456789")
		writeBackup(t, dir, "中文备份-01.tar.gz", body, base)

		sm := newFakeSMTP(t, "none")
		host, port := sm.hostPort()
		st := smtpSettings(host, port, EncNone)
		res, err := DefaultSender().Send(context.Background(), st, dir, ledgerPath, Options{}, nil, nil)
		if err != nil || len(res.Sent) != 1 {
			t.Fatalf("发送失败: %v / %+v", err, res)
		}
		msgs := sm.messages()
		if len(msgs) != 1 {
			t.Fatalf("假服务器应收到 1 封邮件，实际 %d", len(msgs))
		}
		m := msgs[0]
		if len(m.rcpts) != 1 || m.rcpts[0] != "offsite@example.com" {
			t.Fatalf("收件人不对: %v", m.rcpts)
		}
		if m.from != "panel@example.com" {
			t.Fatalf("发件人不对: %s", m.from)
		}
		if !m.authed || m.user != "mailer" || m.pass != secretForTest {
			t.Fatalf("SMTP 应该用配置的用户名/口令登录，实际 authed=%v user=%q", m.authed, m.user)
		}
		head, data := extractAttachment(t, m.data)
		if string(data) != string(body) {
			t.Fatalf("附件内容与归档不一致: %q", string(data))
		}
		if !strings.Contains(head, "filename*=UTF-8''") {
			t.Fatalf("中文文件名必须按 RFC 2231 编码，实际头部:\n%s", head)
		}
		if !strings.Contains(m.data, "=?UTF-8?B?") {
			t.Fatalf("中文主题必须按 RFC 2047 编码:\n%s", firstLines(m.data, 6))
		}

		// STARTTLS 与 SSL（隐式 TLS）两条加密路径也必须真的能把附件发出去。
		for _, mode := range []string{"starttls", "ssl"} {
			d := t.TempDir()
			want := "BODY-" + mode
			writeBackup(t, d, mode+".tar.gz", []byte(want), base)
			smX := newFakeSMTP(t, mode)
			hx, px := smX.hostPort()
			stX := smtpSettings(hx, px, mode)
			resX, err := trustTestCert(smX.pool).Send(context.Background(), stX, d,
				filepath.Join(d, "ledger.json"), Options{}, nil, nil)
			if err != nil || len(resX.Sent) != 1 {
				t.Fatalf("%s 发送失败: %v / %+v", mode, err, resX)
			}
			ms := smX.messages()
			if len(ms) != 1 {
				t.Fatalf("%s 应收到 1 封邮件，实际 %d", mode, len(ms))
			}
			if _, data := extractAttachment(t, ms[0].data); string(data) != want {
				t.Fatalf("%s 附件内容不对: %q", mode, string(data))
			}
		}

		// 测试连接：一封**无附件**的测试邮件
		smTest := newFakeSMTP(t, "none")
		ht, pt := smTest.hostPort()
		stTest := smtpSettings(ht, pt, EncNone)
		if _, err := DefaultSender().Test(context.Background(), stTest); err != nil {
			t.Fatalf("SMTP 测试连接失败: %v", err)
		}
		tm := smTest.messages()
		if len(tm) != 1 {
			t.Fatalf("测试连接应发出 1 封邮件，实际 %d", len(tm))
		}
		if strings.Contains(tm[0].data, "application/gzip") {
			t.Fatal("测试连接必须是**无附件**的邮件")
		}
		if len(tm[0].rcpts) != 1 || tm[0].rcpts[0] != "offsite@example.com" {
			t.Fatalf("测试邮件收件人不对: %v", tm[0].rcpts)
		}

		// 超过上限的文件：不发送、如实标记（负向对照：去掉上限判据这条就红）
		dir2 := t.TempDir()
		big := make([]byte, 1*1024*1024+1)
		writeBackup(t, dir2, "big.tar.gz", big, base)
		sm2 := newFakeSMTP(t, "none")
		h2, p2 := sm2.hostPort()
		st2 := smtpSettings(h2, p2, EncNone)
		st2.MaxFileMB = 1
		res2, err := DefaultSender().Send(context.Background(), st2, dir2,
			filepath.Join(dir2, "ledger.json"), Options{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res2.Sent) != 0 || len(res2.Skipped) != 1 || res2.Skipped[0].Name != "big.tar.gz" {
			t.Fatalf("超过上限的文件不许发送，且必须如实标记：sent=%v skipped=%v", res2.Sent, res2.Skipped)
		}
		if len(sm2.messages()) != 0 {
			t.Fatal("超过上限的文件一个字节都不许发出去（绝不截断）")
		}
		led, _ := LoadLedger(filepath.Join(dir2, "ledger.json"))
		if e := led.Entries["big.tar.gz"]; e.Status != StatusSkipped {
			t.Fatalf("账本要如实记下跳过状态，实际 %+v", e)
		}
	})

	// ---------- ③ FTP 登录 + STOR；FTPS 先 AUTH TLS ----------
	t.Run("③FTP与FTPS", func(t *testing.T) {
		// 明文 FTP
		dir := t.TempDir()
		writeBackup(t, dir, "ftp.tar.gz", []byte("FTP-BODY"), time.Now())
		ff := newFakeFTP(t, "/remote-backups")
		host, port := ff.hostPort()
		st := ftpSettings(host, port, ProtocolFTP)
		res, err := DefaultSender().Send(context.Background(), st, dir,
			filepath.Join(dir, "ledger.json"), Options{}, nil, nil)
		if err != nil || len(res.Sent) != 1 {
			t.Fatalf("FTP 发送失败: %v / %+v", err, res)
		}
		s1 := ff.snapshot()
		if s1.authTLS {
			t.Fatal("ftp（不加密）不该走 AUTH TLS")
		}
		if len(s1.stors) != 1 || s1.stors[0] != "ftp.tar.gz" {
			t.Fatalf("假 FTP 应收到 STOR ftp.tar.gz，实际 %v", s1.stors)
		}
		if string(s1.stored["ftp.tar.gz"]) != "FTP-BODY" {
			t.Fatalf("STOR 上来的内容不对: %q", s1.stored["ftp.tar.gz"])
		}
		if s1.user != "backupuser" || s1.pass != secretForTest {
			t.Fatalf("FTP 登录用的用户名/口令不对: %q", s1.user)
		}

		// 显式 FTPS：必须先 AUTH TLS
		//（负向对照：把加密关掉仍走明文 ⇒ 这条必须红）
		dir2 := t.TempDir()
		writeBackup(t, dir2, "ftps.tar.gz", []byte("FTPS-BODY"), time.Now())
		ff2 := newFakeFTP(t, "/remote-backups")
		h2, p2 := ff2.hostPort()
		st2 := ftpSettings(h2, p2, ProtocolFTPS)
		res2, err := trustTestCert(ff2.pool).Send(context.Background(), st2, dir2,
			filepath.Join(dir2, "ledger.json"), Options{}, nil, nil)
		if err != nil || len(res2.Sent) != 1 {
			t.Fatalf("FTPS 发送失败: %v / %+v", err, res2)
		}
		s2 := ff2.snapshot()
		if !s2.authTLS {
			t.Fatal("ftps 必须先发 AUTH TLS（否则就是明文上传）")
		}
		if len(s2.stors) != 1 || s2.stors[0] != "ftps.tar.gz" {
			t.Fatalf("FTPS 应收到 STOR ftps.tar.gz，实际 %v", s2.stors)
		}
		if string(s2.stored["ftps.tar.gz"]) != "FTPS-BODY" {
			t.Fatalf("FTPS STOR 内容不对: %q", s2.stored["ftps.tar.gz"])
		}

		// 自签名证书：默认严格校验要如实报"证书"原因；允许自签名时才通
		ff3 := newFakeFTP(t, "/remote-backups")
		h3, p3 := ff3.hostPort()
		bad := ftpSettings(h3, p3, ProtocolFTPS)
		dir3 := t.TempDir()
		writeBackup(t, dir3, "selfsigned.tar.gz", []byte("X"), time.Now())
		resBad, err := DefaultSender().Send(context.Background(), bad, dir3,
			filepath.Join(dir3, "ledger.json"), Options{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(resBad.Failed) != 1 || !strings.Contains(strings.ToLower(resBad.Failed[0].Reason), "tls") {
			t.Fatalf("自签名证书必须被拒绝并如实报原因，实际 %+v", resBad)
		}
		okSt := ftpSettings(h3, p3, ProtocolFTPS)
		okSt.Insecure = true
		dir4 := t.TempDir()
		writeBackup(t, dir4, "selfsigned.tar.gz", []byte("Y"), time.Now())
		res4, err := DefaultSender().Send(context.Background(), okSt, dir4,
			filepath.Join(dir4, "ledger.json"), Options{}, nil, nil)
		if err != nil || len(res4.Sent) != 1 {
			t.Fatalf("勾选「允许自签名证书」后应能连上: %v / %+v", err, res4)
		}

		// 远程目录不存在：如实报目录原因
		ff5 := newFakeFTP(t, "/only-here")
		h5, p5 := ff5.hostPort()
		bad5 := ftpSettings(h5, p5, ProtocolFTP)
		bad5.RemoteDir = "/nope"
		if _, err := DefaultSender().Test(context.Background(), bad5); err == nil {
			t.Fatal("远程目录不存在必须报错")
		} else if !strings.Contains(err.Error(), "远程目录") {
			t.Fatalf("失败原因要指明是远程目录问题，实际: %v", err)
		}

		// 测试连接：FTP 写一个 1 字节临时文件再删掉
		ff6 := newFakeFTP(t, "")
		h6, p6 := ff6.hostPort()
		st6 := ftpSettings(h6, p6, ProtocolFTP)
		st6.RemoteDir = ""
		msg, err := DefaultSender().Test(context.Background(), st6)
		if err != nil {
			t.Fatalf("FTP 测试连接失败: %v", err)
		}
		s6 := ff6.snapshot()
		if len(s6.stors) != 1 || len(s6.deleted) != 1 || s6.stors[0] != s6.deleted[0] {
			t.Fatalf("测试连接要写一个临时文件再删掉，实际 stor=%v del=%v", s6.stors, s6.deleted)
		}
		if len(s6.stored[s6.stors[0]]) != 1 {
			t.Fatalf("测试文件必须是 1 字节，实际 %d", len(s6.stored[s6.stors[0]]))
		}
		if !strings.Contains(msg, "删除测试文件成功") {
			t.Fatalf("测试连接的成功说明要如实: %q", msg)
		}
	})

	// ---------- ④ 口令绝不进日志/错误 ----------
	t.Run("④口令不落日志", func(t *testing.T) {
		// 服务器把口令回显在错误信息里：这是最容易泄漏的一条路径。
		echo := "530 Login incorrect (password=" + secretForTest + ")"
		dir := t.TempDir()
		writeBackup(t, dir, "x.tar.gz", []byte("X"), time.Now())
		ff := newFakeFTP(t, "")
		ff.authErr = echo
		host, port := ff.hostPort()
		st := ftpSettings(host, port, ProtocolFTP)
		st.RemoteDir = ""

		var logs []string
		logf := func(level, text string) { logs = append(logs, level+": "+text) }
		res, err := DefaultSender().Send(context.Background(), st, dir,
			filepath.Join(dir, "ledger.json"), Options{}, nil, logf)
		if err != nil {
			t.Fatalf("单个文件失败应逐条如实报，而不是整批报错: %v", err)
		}
		if len(res.Failed) != 1 {
			t.Fatalf("登录失败必须记一条失败，实际 %+v", res)
		}
		all := strings.Join(logs, "\n") + "\n" + res.Failed[0].Reason
		if strings.Contains(all, secretForTest) {
			t.Fatalf("口令出现在了日志/错误信息里：\n%s", all)
		}
		// 账本里也不许留口令（它是长期保存且界面可见的）
		led, _ := LoadLedger(filepath.Join(dir, "ledger.json"))
		if e := led.Entries["x.tar.gz"]; strings.Contains(e.Error, secretForTest) {
			t.Fatalf("账本里不许出现口令: %q", e.Error)
		}
		// Test() 的返回错误同样不许带口令
		if _, terr := DefaultSender().Test(context.Background(), st); terr == nil {
			t.Fatal("登录失败时测试连接必须报错")
		} else if strings.Contains(terr.Error(), secretForTest) {
			t.Fatalf("测试连接的错误里不许出现口令: %v", terr)
		}

		// SMTP 认证失败时同理
		sm := newFakeSMTP(t, "none")
		sm.authErr = "535 auth failed for " + secretForTest
		h2, p2 := sm.hostPort()
		st2 := smtpSettings(h2, p2, EncNone)
		dir2 := t.TempDir()
		writeBackup(t, dir2, "y.tar.gz", []byte("Y"), time.Now())
		var logs2 []string
		res2, err := DefaultSender().Send(context.Background(), st2, dir2,
			filepath.Join(dir2, "ledger.json"), Options{}, nil,
			func(level, text string) { logs2 = append(logs2, level+": "+text) })
		if err != nil {
			t.Fatal(err)
		}
		all2 := strings.Join(logs2, "\n") + "\n" + res2.Failed[0].Reason
		if strings.Contains(all2, secretForTest) {
			t.Fatalf("SMTP 口令出现在了日志/错误信息里：\n%s", all2)
		}
	})
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
