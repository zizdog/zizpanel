package upgrade

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
)

// TestFetchManifestAnyAppendsNetworkHint 锁住"候选全失败 + 网络原因"这条用户可见
// 路径：升级源候选全失败时，错误里必须带上统一网络提示（判据见 services/netfail.go），
// 而且要保留每个候选各自的原文（排障起点不能丢）。
func TestFetchManifestAnyAppendsNetworkHint(t *testing.T) {
	pub, _ := testKey(t)
	useTestKey(t, pub)

	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		return nil, nil, errors.New(`Get "https://zizdog.com/zizpanel/manifest.json": dial tcp: lookup zizdog.com: no such host`)
	}
	_, _, err := FetchManifestAny(context.Background(),
		[]string{"https://a.test/zizpanel", "https://b.test/zizpanel"}, time.Second, fetch)
	if err == nil {
		t.Fatal("全部候选失败时应当返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, services.NetworkHintMarker) {
		t.Errorf("网络原因导致的候选全失败必须附上统一提示，实际:\n%s", msg)
	}
	if !strings.Contains(msg, "no such host") || !strings.Contains(msg, "a.test") || !strings.Contains(msg, "b.test") {
		t.Errorf("候选原文（各自的原因与地址）不能丢，实际:\n%s", msg)
	}
}

// TestFetchManifestAnyNoHintForNonNetworkFailure 反例：候选全失败但**不是**网络问题
// （这里是签名不对）时，绝不能给用户加"网络问题/自备代理"的提示 —— 那是误导。
func TestFetchManifestAnyNoHintForNonNetworkFailure(t *testing.T) {
	pub, _ := testKey(t)
	useTestKey(t, pub)

	// 清单能下到，但签名是伪造的 —— 这是信任问题，不是网络问题。
	fetch := func(ctx context.Context, base string) ([]byte, []byte, error) {
		return goodManifest(), []byte(strings.Repeat("x", 64)), nil
	}
	_, _, err := FetchManifestAny(context.Background(),
		[]string{"https://a.test/zizpanel", "https://b.test/zizpanel"}, time.Second, fetch)
	if err == nil {
		t.Fatal("签名不对时应当失败")
	}
	if strings.Contains(err.Error(), services.NetworkHintMarker) {
		t.Errorf("签名失败不是网络问题，不该附网络提示，实际:\n%s", err.Error())
	}
}
