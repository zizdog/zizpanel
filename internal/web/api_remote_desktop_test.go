package web

// 远程桌面（网页 noVNC 中继）的门禁。
//
// 抓的是"面板能不能真的连上本机屏幕共享"这条链上最容易谎报成功的几段：
//   ① RFB 握手解析：安全类型列表 / 服务器拒绝（count=0 + 原因）/ 根本不是 RFB 服务；
//   ② 状态接口：5900 在听 + 提供 Apple ARD(30) 才算"能连"；提供别的（如只有 Tight）
//      必须如实报 unsupported，而不是显示"已就绪"；
//   ③ 开关屏幕共享的 argv 表（enable = enable + bootstrap，disable = bootout + disable）——
//      这条是纯函数，真机执行与回读在 local 实例上验。

import (
	"context"
	"net"
	"strings"
	"testing"
)

// fakeRFBServer 起一个假屏幕共享：按真实握手形状答复（版本 + 安全类型列表）。
// refuseReason 非空时回 count=0 + 原因字符串（真实 macOS 拒绝时就是这个形状）。
func fakeRFBServer(t *testing.T, version string, types []byte, refuseReason string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假 RFB 服务失败：%v", err)
	}
	// 注意：状态探测会**多次**连这个端口（portListening 先拨一次再关，
	// 然后是 RFB 自检），所以必须循环 Accept —— 只接一次会让第二次连接干等超时。
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if _, werr := c.Write([]byte(version)); werr != nil {
					return
				}
				buf := make([]byte, 12)
				if _, rerr := c.Read(buf); rerr != nil { // 客户端版本（内容不校验）
					return
				}
				if refuseReason != "" {
					_, _ = c.Write([]byte{0})
					l := len(refuseReason)
					_, _ = c.Write([]byte{byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)})
					_, _ = c.Write([]byte(refuseReason))
					return
				}
				_, _ = c.Write(append([]byte{byte(len(types))}, types...))
				select {} // 保持连接，等客户端自己断
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestRFBSecurityTypesParsesRealShapes(t *testing.T) {
	// ① 真实 macOS：RFB 003.889 + 安全类型 30（Apple ARD）—— noVNC 支持它。
	addr, stop := fakeRFBServer(t, "RFB 003.889\n", []byte{30}, "")
	defer stop()
	types, err := rfbSecurityTypes(context.Background(), addr)
	if err != nil {
		t.Fatalf("解析 ARD 类型失败：%v", err)
	}
	if len(types) != 1 || types[0] != rfbSecurityARD {
		t.Fatalf("应读到 [30]，实际 %v", types)
	}

	// ② 服务器拒绝（count=0 + 原因）必须把原因带出来，不能只报"连不上"。
	addr2, stop2 := fakeRFBServer(t, "RFB 003.889\n", nil, "The connection was rejected")
	defer stop2()
	if _, err := rfbSecurityTypes(context.Background(), addr2); err == nil ||
		!strings.Contains(err.Error(), "拒绝了握手") || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("被拒时应带出服务器给的原因，实际 err=%v", err)
	}

	// ③ 不是 RFB 服务（比如把别的端口填错了）必须明确说出来。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		_ = conn.Close()
	}()
	if _, err := rfbSecurityTypes(context.Background(), ln.Addr().String()); err == nil ||
		!strings.Contains(err.Error(), "不是 RFB 服务") {
		t.Fatalf("非 RFB 服务应明确报错，实际 err=%v", err)
	}
}

// TestRemoteDesktopStatusOnlyClaimsSupportedWhenARDOrVNCAuth 是"不许谎报能连"的判据：
// 只有提供 30（ARD）或 2（VNC 口令）才算能连；提供别的类型必须 unsupported + 说清原因。
func TestRemoteDesktopStatusOnlyClaimsSupportedWhenARDOrVNCAuth(t *testing.T) {
	oldAddr := remoteDesktopAddr
	t.Cleanup(func() { remoteDesktopAddr = oldAddr })

	addr, stop := fakeRFBServer(t, "RFB 003.889\n", []byte{30}, "")
	defer stop()
	remoteDesktopAddr = addr
	srv, _ := newTestServer(t)
	st := srv.remoteDesktopStatus(context.Background())
	if !st.Listening || !st.AuthSupported {
		t.Fatalf("提供 ARD(30) 时应判成可连：%+v", st)
	}

	addr2, stop2 := fakeRFBServer(t, "RFB 003.008\n", []byte{16}, "") // 只有 Tight
	defer stop2()
	remoteDesktopAddr = addr2
	st2 := srv.remoteDesktopStatus(context.Background())
	if st2.AuthSupported {
		t.Fatalf("只有 Tight(16) 时不该说能连：%+v", st2)
	}
	if st2.CheckError == "" {
		t.Fatal("不支持时必须给出原因（界面要显示为什么不能连）")
	}
}

// TestRemoteDesktopCommandPlan 锁住开/关屏幕共享的命令（真机执行与回读在 local 实例上验；
// 这里防止有人把 enable 写成 disable、或漏掉 bootstrap/bootout 那一步）。
func TestRemoteDesktopCommandPlan(t *testing.T) {
	en := remoteDesktopCommandPlan("enable")
	if len(en) != 2 || en[0][0] != "enable" || en[1][0] != "bootstrap" {
		t.Fatalf("enable 应是 enable + bootstrap，实际 %v", en)
	}
	if en[1][2] != screensharingPlist {
		t.Fatalf("bootstrap 必须用屏幕共享的 plist，实际 %v", en[1])
	}
	dis := remoteDesktopCommandPlan("disable")
	if len(dis) != 2 || dis[0][0] != "bootout" || dis[1][0] != "disable" {
		t.Fatalf("disable 应是 bootout + disable，实际 %v", dis)
	}
}
