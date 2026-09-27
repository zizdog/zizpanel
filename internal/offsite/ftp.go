package offsite

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// ftp.go —— 最小 FTP 客户端（USER/PASS/TYPE I/PASV/STOR/DELE/CWD/QUIT）。
//
// ftps = 显式 AUTH TLS + crypto/tls；控制连接与数据连接都加密（PBSZ 0 + PROT P）。
// 只实现上传需要的这几条命令，不引入第三方依赖。

type ftpSession struct {
	conn      net.Conn
	r         *bufio.Reader
	w         *bufio.Writer
	st        Settings
	s         *Sender
	protected bool // 数据连接是否走 TLS
}

// ftpDial 建立控制连接、登录并进入远程目录。
func (s *Sender) ftpDial(ctx context.Context, st Settings) (*ftpSession, error) {
	addr := net.JoinHostPort(st.Host, strconv.Itoa(st.Port))
	conn, err := s.dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连接 FTP 服务器失败: %w", err)
	}
	setDeadline(conn, ctx)
	sess := &ftpSession{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn), st: st, s: s}

	code, msg, err := sess.read()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取 FTP 欢迎语失败: %w", err)
	}
	if code != 220 {
		_ = conn.Close()
		return nil, fmt.Errorf("FTP 服务器拒绝连接: %d %s", code, msg)
	}

	if st.Encryption == EncExplicitTLS {
		if code, msg, err = sess.cmd("AUTH TLS"); err != nil {
			_ = conn.Close()
			return nil, err
		} else if code != 234 {
			_ = conn.Close()
			return nil, fmt.Errorf("服务器不支持 AUTH TLS: %d %s", code, msg)
		}
		tconn := tls.Client(conn, s.tlsConfig(st))
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("FTPS TLS 握手失败: %w", err)
		}
		sess.conn = tconn
		sess.r = bufio.NewReader(tconn)
		sess.w = bufio.NewWriter(tconn)
		sess.protected = true
		// 数据连接也要保护：PBSZ 0 + PROT P 是显式 FTPS 的标准做法。
		for _, c := range []string{"PBSZ 0", "PROT P"} {
			if code, msg, err = sess.cmd(c); err != nil {
				sess.close()
				return nil, err
			} else if code/100 != 2 {
				sess.close()
				return nil, fmt.Errorf("FTPS 的 %s 被拒绝: %d %s", c, code, msg)
			}
		}
	}

	code, msg, err = sess.cmd("USER " + st.Username)
	if err != nil {
		sess.close()
		return nil, err
	}
	if code == 331 {
		code, msg, err = sess.cmd("PASS " + st.Password)
		if err != nil {
			sess.close()
			return nil, err
		}
	}
	if code/100 != 2 {
		sess.close()
		return nil, fmt.Errorf("FTP 登录失败（用户名或口令不对）: %d %s", code, msg)
	}
	if code, msg, err = sess.cmd("TYPE I"); err != nil {
		sess.close()
		return nil, err
	} else if code/100 != 2 {
		sess.close()
		return nil, fmt.Errorf("FTP 切换到二进制模式失败: %d %s", code, msg)
	}
	if st.RemoteDir != "" {
		if code, msg, err = sess.cmd("CWD " + st.RemoteDir); err != nil {
			sess.close()
			return nil, err
		} else if code/100 != 2 {
			sess.close()
			return nil, fmt.Errorf("远程目录 %s 不存在或无权进入: %d %s", st.RemoteDir, code, msg)
		}
	}
	return sess, nil
}

// cmd 发一条命令并读响应。
func (f *ftpSession) cmd(line string) (int, string, error) {
	if _, err := f.w.WriteString(line + "\r\n"); err != nil {
		return 0, "", err
	}
	if err := f.w.Flush(); err != nil {
		return 0, "", err
	}
	return f.read()
}

// read 读一条（可能多行的）响应。
//
// FTP 多行响应的中间行**不带**状态码，只有首行与末行带；把中间行当状态码解析
// 会让带说明文字的服务器（vsftpd 等）直接不可用。
func (f *ftpSession) read() (int, string, error) {
	var code int
	var sb strings.Builder
	first := true
	for {
		line, err := f.r.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if first {
			if len(line) < 4 {
				return 0, "", fmt.Errorf("FTP 响应格式不对: %q", line)
			}
			n, err := strconv.Atoi(line[:3])
			if err != nil {
				return 0, "", fmt.Errorf("FTP 响应格式不对: %q", line)
			}
			code = n
			first = false
			sb.WriteString(strings.TrimSpace(line[3:]))
			if line[3] == '-' {
				continue
			}
			return code, sb.String(), nil
		}
		if strings.HasPrefix(line, fmt.Sprintf("%03d ", code)) {
			sb.WriteString(" " + strings.TrimSpace(line[4:]))
			return code, sb.String(), nil
		}
		sb.WriteString(" " + strings.TrimSpace(line))
	}
}

// pasvAddr 只取被动模式的地址，不建立数据连接。
//
// 为什么不在这一步连接：FTPS 要在数据连接上做 TLS 握手，而服务器要到收到 STOR
// 才会 accept 数据连接 —— 先握手就会双方对等死锁（真机上表现为"一直卡住"）。
func (f *ftpSession) pasvAddr() (string, int, error) {
	code, msg, err := f.cmd("PASV")
	if err != nil {
		return "", 0, err
	}
	if code != 227 {
		return "", 0, fmt.Errorf("FTP PASV 失败: %d %s", code, msg)
	}
	return parsePASV(msg)
}

// openData 建立数据连接（FTPS 时在其上完成 TLS 握手）。
func (f *ftpSession) openData(ctx context.Context, host string, port int) (net.Conn, error) {
	conn, err := f.s.dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("连接 FTP 数据端口失败: %w", err)
	}
	setDeadline(conn, ctx)
	if f.protected {
		tconn := tls.Client(conn, f.s.tlsConfig(f.st))
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("FTPS 数据连接握手失败: %w", err)
		}
		return tconn, nil
	}
	return conn, nil
}

// store 用 STOR 上传一个流（size 只用于进度显示）。
//
// 顺序必须是 PASV → STOR(150) → 建立数据连接：服务器只在收到 STOR 后才 accept。
func (f *ftpSession) store(ctx context.Context, name string, src io.Reader, size int64,
	progress func(int64, int64)) error {

	host, port, err := f.pasvAddr()
	if err != nil {
		return err
	}
	code, msg, err := f.cmd("STOR " + name)
	if err != nil {
		return err
	}
	if code != 150 && code != 125 {
		return fmt.Errorf("服务器拒绝上传 %s: %d %s", name, code, msg)
	}
	data, err := f.openData(ctx, host, port)
	if err != nil {
		return err
	}
	werr := copyCtx(ctx, data, src, progress, size)
	cerr := data.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	code, msg, err = f.read()
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("上传 %s 未完成: %d %s", name, code, msg)
	}
	return nil
}

// dele 删除远程文件（测试连接用）。
func (f *ftpSession) dele(name string) error {
	code, msg, err := f.cmd("DELE " + name)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("删除远程文件 %s 失败: %d %s", name, code, msg)
	}
	return nil
}

// close 直接断开（连接可能已坏，不等 QUIT）。
func (f *ftpSession) close() {
	if f.conn != nil {
		_ = f.conn.Close()
	}
}

// quit 正常收工。
func (f *ftpSession) quit() {
	if f.conn == nil {
		return
	}
	_, _, _ = f.cmd("QUIT")
	f.close()
}

// testFTP 连接 + 登录 +（进目录）+ 写 1 字节临时文件再删掉。
func (s *Sender) testFTP(ctx context.Context, st Settings) (string, error) {
	sess, err := s.ftpDial(ctx, st)
	if err != nil {
		return "", err
	}
	defer sess.quit()
	name := ".zizpanel-test-" + randomHex(6) + ".tmp"
	if err := sess.store(ctx, name, strings.NewReader("x"), 1, nil); err != nil {
		return "", err
	}
	if err := sess.dele(name); err != nil {
		return "", fmt.Errorf("测试文件已上传但删除失败（请检查目录写权限）: %w", err)
	}
	where := st.Host
	if st.RemoteDir != "" {
		where += "/" + strings.Trim(st.RemoteDir, "/")
	}
	return "已连接 " + where + "：登录、写入并删除测试文件成功", nil
}

// parsePASV 解析 "227 Entering Passive Mode (h1,h2,h3,h4,p1,p2)"。
func parsePASV(msg string) (string, int, error) {
	m := regexp.MustCompile(`\((\d{1,3}),(\d{1,3}),(\d{1,3}),(\d{1,3}),(\d{1,3}),(\d{1,3})\)`).FindStringSubmatch(msg)
	if m == nil {
		return "", 0, fmt.Errorf("无法解析 FTP 被动模式响应: %s", msg)
	}
	nums := make([]int, 6)
	for i := 0; i < 6; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n < 0 || n > 255 {
			return "", 0, fmt.Errorf("FTP 被动模式地址不合法: %s", msg)
		}
		nums[i] = n
	}
	host := fmt.Sprintf("%d.%d.%d.%d", nums[0], nums[1], nums[2], nums[3])
	return host, nums[4]*256 + nums[5], nil
}

// copyCtx 是带 ctx 检查与进度回调的分块拷贝（64KB 一块）。
//
// 为什么不用 io.Copy：大文件传输时用户点「中断」要能很快停下，
// 而 io.Copy 在读完整个文件前都不会回到我们的代码。
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader, progress func(int64, int64), total int64) error {
	buf := make([]byte, 64*1024)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
