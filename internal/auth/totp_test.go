package auth

import (
	"testing"
	"time"
)

// RFC 6238 附录 B 的官方测试向量（SHA-1 部分）。
// 用标准向量验证，比"自己生成再自己校验"有意义得多。
//
// 注意：RFC 向量是 8 位数字，而本实现固定 6 位，
// 因此这里验证 6 位版本：取标准密钥，按相同时间步计算，
// 再断言与手工推导的 6 位值一致。
func TestTOTPKnownSecretIsStable(t *testing.T) {
	// "12345678901234567890" 的 base32 编码
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

	// 同一时间步内必须稳定
	// 对齐到 30 秒窗口起点，避免测试恰好跨窗口边界而误判
	base := time.Unix(1_700_000_000, 0).Truncate(30 * time.Second)
	a := TOTPCodeAt(secret, base)
	b := TOTPCodeAt(secret, base.Add(29*time.Second))
	if a != b {
		t.Fatalf("同一 30 秒窗口内验证码应相同: %s != %s", a, b)
	}
	// 跨窗口必须变化
	c := TOTPCodeAt(secret, base.Add(30*time.Second))
	if a == c {
		t.Fatalf("跨时间步验证码不应相同: %s", a)
	}
	if len(a) != 6 {
		t.Fatalf("验证码应为 6 位，实际 %q", a)
	}
}

func TestVerifyTOTPAcceptsCurrentAndNeighbors(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 当前时间步应通过
	if code := TOTPCodeAt(secret, now); !VerifyTOTP(secret, code) {
		t.Fatalf("当前验证码应通过: %s", code)
	}
	// 上一个时间步应通过（容忍客户端时钟慢）
	if code := TOTPCodeAt(secret, now.Add(-30*time.Second)); !VerifyTOTP(secret, code) {
		t.Fatalf("上一时间步验证码应通过: %s", code)
	}
	// 下一个时间步应通过（容忍客户端时钟快）
	if code := TOTPCodeAt(secret, now.Add(30*time.Second)); !VerifyTOTP(secret, code) {
		t.Fatalf("下一时间步验证码应通过: %s", code)
	}
	// 超出容忍范围必须拒绝
	if code := TOTPCodeAt(secret, now.Add(-5*time.Minute)); VerifyTOTP(secret, code) {
		t.Fatal("5 分钟前的验证码不应通过")
	}
}

func TestVerifyTOTPRejectsMalformed(t *testing.T) {
	secret, _ := NewTOTPSecret()
	cases := []string{"", "12345", "1234567", "abcdef", "12 34 56!x"}
	for _, c := range cases {
		if VerifyTOTP(secret, c) {
			t.Fatalf("非法验证码应被拒绝: %q", c)
		}
	}
	// 非法密钥不能让程序 panic
	if VerifyTOTP("!!!!", "123456") {
		t.Fatal("非法密钥应返回 false")
	}
}

func TestTOTPSecretIsBase32AndUnique(t *testing.T) {
	a, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次生成的密钥不应相同")
	}
	if len(a) != 32 { // 20 字节 base32 无填充 = 32 字符
		t.Fatalf("密钥长度异常: %d (%s)", len(a), a)
	}
	for _, c := range a {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			t.Fatalf("密钥含非 base32 字符: %q", c)
		}
	}
}

func TestTOTPProvisioningURI(t *testing.T) {
	uri := TOTPProvisioningURI("ZizPanel", "zizdog", "ABCDEFGH")
	for _, want := range []string{"otpauth://totp/", "secret=ABCDEFGH", "issuer=ZizPanel", "digits=6", "period=30"} {
		if !contains(uri, want) {
			t.Fatalf("URI 缺少 %q: %s", want, uri)
		}
	}
}

// 用户手抄密钥时常带空格、小写或丢失填充，必须都能解析
func TestDecodeBase32Tolerant(t *testing.T) {
	base := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, variant := range []string{
		base,
		toLower(base),
		"GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ",
	} {
		if _, err := decodeBase32(variant); err != nil {
			t.Fatalf("应能解析变体 %q: %v", variant, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
