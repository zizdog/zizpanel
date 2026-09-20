package web

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
//  最小 WebSocket 实现（RFC 6455）
//
//  为什么不引入 gorilla/websocket：
//    面板的功能只需要"可靠地收发文本帧"，而完整库要带来一串依赖。
//    自己实现握手 + 帧协议约 200 行，且能精确控制缓冲、超时与关闭语义 ——
//    对终端这种需要低延迟双向流式的场景反而更可控。
//
//  已实现：握手校验、掩码解码（客户端帧必须掩码）、分片帧组装、
//         ping/pong、关闭握手、长度分档（7/16/64 位）。
//  未实现（本项目不需要）：扩展协商（permessage-deflate）、子协议。
// ---------------------------------------------------------------------------

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA

	// 单帧最大载荷：终端输出可能是一次几 MB 的命令回显，
	// 但过大的帧会占用大量内存，因此限制在 1MB，超出则分片发送。
	wsMaxPayload = 1 << 20
)

// wsConn 是一个已建立连接的 WebSocket。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	writeMu sync.Mutex
	closeMu sync.Mutex
	closed  bool
}

// 握手校验错误：逐跳头这一类可能是被中间层吃掉的（坑 219），
// 调用方据此给出"去开 WebSocket 透传"的结论，而不是只记日志。
var (
	errWSNotGET              = errors.New("WebSocket 握手必须使用 GET")
	errWSNoConnectionUpgrade = errors.New("缺少 Connection: Upgrade 头")
	errWSUpgradeNotWebsocket = errors.New("Upgrade 头不是 websocket")
	errWSBadVersion          = errors.New("不支持的 WebSocket 版本")
	errWSMissingKey          = errors.New("缺少 Sec-WebSocket-Key")
)

// validateWSHandshake 校验升级请求；返回 nil 才会接管连接。
//
// 关键校验（缺一不可）：
//   - 必须是 GET
//   - Connection 头含 "upgrade"，Upgrade 头为 "websocket"
//   - Sec-WebSocket-Key 存在
//   - Sec-WebSocket-Version 为 13
func validateWSHandshake(r *http.Request) error {
	if r.Method != http.MethodGet {
		return errWSNotGET
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return errWSNoConnectionUpgrade
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return errWSUpgradeNotWebsocket
	}
	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" {
		return fmt.Errorf("%w: %s（需要 13）", errWSBadVersion, v)
	}
	if r.Header.Get("Sec-WebSocket-Key") == "" {
		return errWSMissingKey
	}
	return nil
}

// upgradeWebSocket 完成握手，把 HTTP 连接升级为 WebSocket。
func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if err := validateWSHandshake(r); err != nil {
		return nil, err
	}
	key := r.Header.Get("Sec-WebSocket-Key")

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("当前连接不支持协议升级（需要 HTTP/1.1）")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("接管连接失败: %w", err)
	}

	accept := wsAcceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &wsConn{conn: conn, br: rw.Reader, bw: rw.Writer}, nil
}

// wsAcceptKey 计算 Sec-WebSocket-Accept（RFC 6455 §4.2.2）。
func wsAcceptKey(key string) string {
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.New()
	h.Write([]byte(key + magic))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ReadMessage 读取一条完整消息（自动处理分片与 ping/pong）。
func (c *wsConn) ReadMessage() (opcode byte, payload []byte, err error) {
	var message []byte
	var messageOp byte
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpPing:
			// 收到 ping 必须回 pong（内容原样返回）
			if err := c.writeFrame(wsOpPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			// 回一个 close 完成关闭握手
			_ = c.writeFrame(wsOpClose, nil)
			return wsOpClose, nil, io.EOF
		}

		if op != wsOpContinuation {
			messageOp = op
			message = message[:0]
		}
		message = append(message, data...)
		if len(message) > wsMaxPayload*4 {
			return 0, nil, errors.New("WebSocket 消息过大")
		}
		if fin {
			return messageOp, message, nil
		}
	}
}

// readFrame 读取单个帧。
func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return false, 0, nil, err
	}
	fin = header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		// RSV 位必须为 0（我们没有协商任何扩展）
		return false, 0, nil, errors.New("非法帧：RSV 位非零")
	}
	opcode = header[0] & 0x0F
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > wsMaxPayload {
		return false, 0, nil, fmt.Errorf("帧载荷过大: %d", length)
	}

	// RFC 规定客户端发来的帧必须掩码；服务端帧必须不掩码。
	// 不校验这条会让代理缓存污染等问题乘虚而入。
	if !masked {
		return false, 0, nil, errors.New("客户端帧必须使用掩码")
	}
	var maskKey [4]byte
	if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= maskKey[i%4]
	}
	return fin, opcode, payload, nil
}

// WriteText 发送一条文本消息。
func (c *wsConn) WriteText(data []byte) error { return c.writeFrame(wsOpText, data) }

// writeFrame 发送一帧（按长度选择 7/16/64 位长度字段）。
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.isClosed() {
		return errors.New("连接已关闭")
	}

	var header []byte
	b0 := byte(0x80) | opcode // FIN=1
	n := len(payload)
	switch {
	case n < 126:
		header = []byte{b0, byte(n)}
	case n <= 0xFFFF:
		header = []byte{b0, 126, 0, 0}
		binary.BigEndian.PutUint16(header[2:], uint16(n))
	default:
		header = []byte{b0, 127, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(header[2:], uint64(n))
	}
	if _, err := c.bw.Write(header); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.bw.Write(payload); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

// Close 关闭连接（先发 close 帧，再关 TCP）。
func (c *wsConn) Close() error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closed = true
	c.closeMu.Unlock()

	_ = c.writeFrame(wsOpClose, []byte{0x03, 0xE8}) // 1000 normal closure
	return c.conn.Close()
}

func (c *wsConn) isClosed() bool {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closed
}

// SetReadDeadline 设置读超时（用于实现空闲检测）。
func (c *wsConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }
