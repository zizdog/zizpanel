package upgrade

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
//  候选顺序（CandidateSources）
// ---------------------------------------------------------------------------

// withLocalIPs 把网卡探测钉成给定的一组地址，测试结束自动恢复。
//
// 必须钉住：开发机自己就可能在 192.168.1.0/24 里，不注入的话
// "同网段"那两条断言会随跑测试的机器而变。
func withLocalIPs(t *testing.T, ips ...string) {
	t.Helper()
	parsed := make([]net.IP, 0, len(ips))
	for _, s := range ips {
		parsed = append(parsed, net.ParseIP(s))
	}
	prev := SetLocalInterfaceIPsForTest(func() []net.IP { return parsed })
	t.Cleanup(func() { SetLocalInterfaceIPsForTest(prev) })
}

func assertSourceOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("候选数量应为 %d，实际 %d：%v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("候选第 %d 位应为 %q，实际 %q（完整顺序 %v）", i+1, want[i], got[i], got)
		}
	}
}

// TestCandidateSourcesOrder 锁定候选优先级。顺序就是这段逻辑的全部意义。
func TestCandidateSourcesOrder(t *testing.T) {
	custom := "https://custom.example/zizpanel"

	t.Run("显式配置优先", func(t *testing.T) {
		withLocalIPs(t, "192.168.1.4") // 在 NAS 网段
		assertSourceOrder(t, CandidateSources(custom),
			[]string{custom, NASSource, DefaultSource, MirrorSource, GitHubSource})
	})

	t.Run("同网段时 NAS 排在公网主源之前", func(t *testing.T) {
		withLocalIPs(t, "192.168.1.4")
		assertSourceOrder(t, CandidateSources(""),
			[]string{NASSource, DefaultSource, MirrorSource, GitHubSource})
	})

	t.Run("不同网段时 NAS 不出现", func(t *testing.T) {
		withLocalIPs(t, "10.0.0.7")
		got := CandidateSources("")
		assertSourceOrder(t, got, []string{DefaultSource, MirrorSource, GitHubSource})
		for _, s := range got {
			if s == NASSource {
				t.Fatalf("不同网段不应出现 NAS 候选：%v", got)
			}
		}
	})

	t.Run("配置成默认主源时 NAS 仍优先", func(t *testing.T) {
		// install.sh / 前端会把默认主源写进 config.json。若因此把公网
		// 排到 NAS 前面，局域网机器就永远用不上快 100 倍的通道。
		withLocalIPs(t, "192.168.1.4")
		assertSourceOrder(t, CandidateSources(DefaultSource),
			[]string{NASSource, DefaultSource, MirrorSource, GitHubSource})
	})

	t.Run("去重且去掉尾部斜杠", func(t *testing.T) {
		withLocalIPs(t, "10.0.0.7")
		assertSourceOrder(t, CandidateSources(MirrorSource+"/"),
			[]string{MirrorSource, DefaultSource, GitHubSource})
	})

	t.Run("回环地址不算同网段", func(t *testing.T) {
		withLocalIPs(t, "127.0.0.1", "::1")
		assertSourceOrder(t, CandidateSources(""),
			[]string{DefaultSource, MirrorSource, GitHubSource})
	})
}

func TestOnNASSubnet(t *testing.T) {
	withLocalIPs(t, "192.168.1.8")
	if !OnNASSubnet() {
		t.Fatal("192.168.1.8 应判定为与 NAS 同网段")
	}
	withLocalIPs(t, "192.168.2.8")
	if OnNASSubnet() {
		t.Fatal("192.168.2.8 不应判定为同网段")
	}
}

// ---------------------------------------------------------------------------
//  多候选回落（FetchManifestAny）
// ---------------------------------------------------------------------------

// useTestKey 装上一把测试公钥，返回值自动恢复原公钥。
func useTestKey(t *testing.T, pub ed25519.PublicKey) {
	t.Helper()
	old := PubKeyHex
	PubKeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { PubKeyHex = old })
}

// TestFetchManifestAnyFallsBackToNextCandidate 是候选机制的核心：
// 前面的候选"不通 / 没签名 / 验签不过"时必须自动试下一个，
// 且只有**验签通过**的那个才算命中。
func TestFetchManifestAnyFallsBackToNextCandidate(t *testing.T) {
	pub, priv := testKey(t)
	useTestKey(t, pub)

	data := goodManifest()
	sig := SignManifest(priv, data)

	// 候选 1：网络不通。候选 2：有清单但签名是伪造的（必须被验签挡下）。
	// 候选 3：正确签名。
	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		switch base {
		case "https://down.test/zizpanel":
			return nil, nil, errors.New("connection refused")
		case "https://unsigned.test/zizpanel":
			return data, []byte("this-is-not-a-valid-ed25519-signature"), nil
		case "https://good.test/zizpanel":
			return data, sig, nil
		}
		return nil, nil, fmt.Errorf("测试里不该访问的候选 %s", base)
	}

	m, used, err := FetchManifestAny(context.Background(), []string{
		"https://down.test/zizpanel",
		"https://unsigned.test/zizpanel",
		"https://good.test/zizpanel",
	}, 2*time.Second, fetch)
	if err != nil {
		t.Fatalf("应当回落到第 3 个候选并成功，实际: %v", err)
	}
	if used != "https://good.test/zizpanel" {
		t.Fatalf("实际命中的源应为第 3 个候选，实际 %q", used)
	}
	if m.Version != "0.2.0" {
		t.Fatalf("清单版本应为 0.2.0，实际 %q", m.Version)
	}
}

// TestFetchManifestAnyDoesNotAcceptUnsignedCandidate 单独锁一条：
// 未签名的候选即使排在前面、即使下载成功，也绝不能被采用。
func TestFetchManifestAnyDoesNotAcceptUnsignedCandidate(t *testing.T) {
	pub, _ := testKey(t)
	useTestKey(t, pub)

	called := 0
	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		called++
		return goodManifest(), nil, nil // 没有签名
	}
	_, _, err := FetchManifestAny(context.Background(),
		[]string{"https://unsigned.test/zizpanel"}, time.Second, fetch)
	if err == nil {
		t.Fatal("未签名的清单竟然被采用")
	}
	if !strings.Contains(err.Error(), "签名") {
		t.Errorf("错误信息应说明是签名问题，实际: %v", err)
	}
	if called != 1 {
		t.Errorf("fetcher 应恰好被调用一次，实际 %d", called)
	}
}

// TestFetchManifestAnyReportsEveryCandidateReason 全部失败时，
// 错误里必须逐个带出每个候选**各自**的原因（只回最后一个没法排障）。
func TestFetchManifestAnyReportsEveryCandidateReason(t *testing.T) {
	pub, _ := testKey(t)
	useTestKey(t, pub)

	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		switch base {
		case "https://refused.test/zizpanel":
			return nil, nil, errors.New("连接被拒绝")
		case "https://badsig.test/zizpanel":
			return goodManifest(), []byte(strings.Repeat("x", ed25519.SignatureSize)), nil
		}
		return nil, nil, errors.New("未知候选")
	}

	_, _, err := FetchManifestAny(context.Background(), []string{
		"https://refused.test/zizpanel",
		"https://badsig.test/zizpanel",
	}, time.Second, fetch)
	if err == nil {
		t.Fatal("全部候选失败时应当返回错误")
	}
	msg := err.Error()
	for _, want := range []string{
		"refused.test", "连接被拒绝",
		"badsig.test", "签名", // 必须说明是验签失败，而不是笼统的"下载失败"
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("汇总错误里缺少 %q，实际:\n%s", want, msg)
		}
	}
}

// TestFetchManifestAnyFailsClosedWithoutKey 没有公钥时一个请求都不该发。
func TestFetchManifestAnyFailsClosedWithoutKey(t *testing.T) {
	old := PubKeyHex
	PubKeyHex = ""
	t.Cleanup(func() { PubKeyHex = old })

	called := 0
	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		called++
		return nil, nil, nil
	}
	_, _, err := FetchManifestAny(context.Background(),
		[]string{"https://a.test", "https://b.test"}, time.Second, fetch)
	if !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("没有公钥时应返回 ErrNoPublicKey，实际: %v", err)
	}
	if called != 0 {
		t.Fatalf("没有公钥时不应发起任何下载，实际调用 %d 次", called)
	}
}

// TestFetchManifestAnyHonoursPerTryTimeout 单个候选的慢不能拖垮后面的候选。
func TestFetchManifestAnyHonoursPerTryTimeout(t *testing.T) {
	pub, priv := testKey(t)
	useTestKey(t, pub)
	data := goodManifest()
	sig := SignManifest(priv, data)

	var secondSawDeadline bool
	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		if strings.Contains(base, "slow") {
			<-ctx.Done() // 模拟一个卡住的源，只能靠 perTry 超时打断
			return nil, nil, ctx.Err()
		}
		_, secondSawDeadline = ctx.Deadline()
		return data, sig, nil
	}

	_, used, err := FetchManifestAny(context.Background(),
		[]string{"https://slow.test/zizpanel", "https://good.test/zizpanel"},
		50*time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("慢候选超时后应回落到下一个候选: %v", err)
	}
	if used != "https://good.test/zizpanel" {
		t.Fatalf("应命中第二个候选，实际 %q", used)
	}
	if !secondSawDeadline {
		t.Error("每个候选都应拿到自己的超时预算")
	}
}

// TestConfiguredSourceUsedFirstWhenReachable 是"显式配置了源的机器行为不变"
// 这条回归：只要配置的源可用，它就是第一个也是唯一被访问的候选。
func TestConfiguredSourceUsedFirstWhenReachable(t *testing.T) {
	pub, priv := testKey(t)
	useTestKey(t, pub)
	withLocalIPs(t, "192.168.1.4") // 即便同网段，显式配置也排在最前

	data := goodManifest()
	sig := SignManifest(priv, data)

	configured := "http://192.168.1.8:8090/zizpanel"
	sources := CandidateSources(configured)
	if len(sources) == 0 || sources[0] != configured {
		t.Fatalf("显式配置的源应排在候选第一位，实际 %v", sources)
	}

	var tried []string
	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		tried = append(tried, base)
		return data, sig, nil
	}
	_, used, err := FetchManifestAny(context.Background(), sources, time.Second, fetch)
	if err != nil {
		t.Fatalf("配置的源可用时不应失败: %v", err)
	}
	if used != configured {
		t.Fatalf("应命中显式配置的源 %q，实际 %q", configured, used)
	}
	if len(tried) != 1 {
		t.Fatalf("配置的源可用时不该再试其它候选，实际访问了 %v", tried)
	}
}
