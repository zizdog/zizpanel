package priv

// listen_scope_gate_test.go —— 端口**真实监听范围**分类的唯一门禁。
//
// 为什么现有门禁抓不到：此前没有任何"监听地址分类"的判据 —— PortInfo 只保留
// `command (pid N)`，lsof 的 NAME 列（`127.0.0.1:9876` / `*:9876`）被直接丢掉；
// 前端那句「本机直连 http://127.0.0.1:9876/（仅本机可用）」是**写死的常量**，
// 与真实监听地址无关，所以对在 `*` 上监听的 ddns-go 说错了话（用户 2026-09-27 报障）。
//
// 样本全部取自本机 `lsof -nP -iTCP:<port> -sTCP:LISTEN` 的真实输出（逐字保留列距），
// 只做纯字符串解析 + 注入，不碰真实端口/进程。
//
// 负向对照：把 ClassifyListen 里的 `*` / `0.0.0.0` 也算成 loopback ⇒ 用例①必须红
//（用例①断言 `*:9876` 是 all 且局域网提示可用）。

import "testing"

func TestListenScopeGate(t *testing.T) {
	// 真实机器上的 lsof 表头（NAME 列是第 8 个字段；STATE `(LISTEN)` 在 NAME 之后）。
	const header = "COMMAND   PID   USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME"

	cases := []struct {
		name     string
		out      string
		wantUse  bool
		wantKind string
		wantAddr string
	}{
		{
			// ① ddns-go 真实输出：`*` 表示所有网卡 ⇒ 局域网能连，不能报"仅本机可用"。
			name:     "① 所有网卡 *（ddns-go 真实样本）",
			out:      header + "\nddns-go 96491 zizdog    8u  IPv6 0x41745562371c1dc1      0t0  TCP *:9876 (LISTEN)\n",
			wantUse:  true,
			wantKind: ListenAll,
		},
		{
			// ①b 另一种 null 写法（Linux lsof / 部分版本）：同样是所有网卡。
			name:     "①b 所有网卡 0.0.0.0",
			out:      header + "\nnode    1234 user   1u  IPv4 0x0      0t0  TCP 0.0.0.0:8080 (LISTEN)\n",
			wantUse:  true,
			wantKind: ListenAll,
		},
		{
			// ② 只听回环（mariadbd 真实样本）：保留"仅本机可用"。
			name:     "② 回环 127.0.0.1（mariadbd 真实样本）",
			out:      header + "\nmariadbd 939 zizdog   44u  IPv4 0xe81865daec5cfbb5      0t0  TCP 127.0.0.1:3306 (LISTEN)\n",
			wantUse:  true,
			wantKind: ListenLoopback,
		},
		{
			// ②b IPv6 回环（qemu 真实样本）：同样是"仅本机"。
			name:     "②b 回环 [::1]（qemu 真实样本）",
			out:      header + "\nqemu-syst 65855 zizdog    8u  IPv6 0x3405f36c76d39803      0t0  TCP [::1]:59842 (LISTEN)\n",
			wantUse:  true,
			wantKind: ListenLoopback,
		},
		{
			// ③ 绑某个具体地址 ⇒ lan，并给出该地址。
			name:     "③ 具体地址 192.168.1.181",
			out:      header + "\napp 4321 user   1u  IPv4 0x1      0t0  TCP 192.168.1.181:9876 (LISTEN)\n",
			wantUse:  true,
			wantKind: ListenLAN,
			wantAddr: "192.168.1.181",
		},
		{
			// ③b 同一进程同时听 * 与回环 ⇒ 按最宽算（all）。
			name: "③b 同时听 * 与回环 ⇒ all",
			out: header +
				"\napp 1 user   1u  IPv4 0x1      0t0  TCP *:80 (LISTEN)" +
				"\napp 1 user   2u  IPv4 0x2      0t0  TCP 127.0.0.1:80 (LISTEN)\n",
			wantUse:  true,
			wantKind: ListenAll,
		},
		{
			// ④ 端口没在听：lsof 无匹配（stdout 空）⇒ unknown，前端一句话都不显示。
			name:     "④ 端口空闲（stdout 空）",
			out:      "",
			wantUse:  false,
			wantKind: ListenUnknown,
		},
		{
			// ④b 权限不够：lsof 的报错在 stderr，stdout 为空 ⇒ unknown（宁可不提示）。
			name:     "④b 权限不足 / 探测失败（stdout 空）",
			out:      "",
			wantUse:  false,
			wantKind: ListenUnknown,
		},
		{
			// ④c 只有表头、没有数据行 ⇒ unknown。
			name:     "④c 只有表头",
			out:      header + "\n",
			wantUse:  false,
			wantKind: ListenUnknown,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := portInfoFromLsof(c.out, 9876)
			if info.InUse != c.wantUse {
				t.Fatalf("InUse = %v，期望 %v（holders=%v）", info.InUse, c.wantUse, info.Holders)
			}
			if info.Listen != c.wantKind {
				t.Fatalf("Listen = %q，期望 %q", info.Listen, c.wantKind)
			}
			if info.ListenAddr != c.wantAddr {
				t.Fatalf("ListenAddr = %q，期望 %q", info.ListenAddr, c.wantAddr)
			}
		})
	}

	t.Run("⑤ holders 仍是 `command (pid N)`（不许因为加解析而回归）", func(t *testing.T) {
		info := portInfoFromLsof(
			header+"\nnginx 581 zizdog   10u  IPv4 0xd5a0549fe8cec5a1      0t0  TCP *:80 (LISTEN)\n", 80)
		if len(info.Holders) != 1 || info.Holders[0] != "nginx (pid 581)" {
			t.Fatalf("holders = %v，期望 [nginx (pid 581)]", info.Holders)
		}
	})

	t.Run("⑥ 认不出来的地址 ⇒ unknown（绝不猜）", func(t *testing.T) {
		if k, a := ClassifyListen([]string{"weird-name:80"}); k != ListenUnknown || a != "" {
			t.Fatalf("不可解析地址应 unknown，得到 %q/%q", k, a)
		}
		if k, _ := ClassifyListen(nil); k != ListenUnknown {
			t.Fatalf("空集合应 unknown，得到 %q", k)
		}
	})
}
