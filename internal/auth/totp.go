package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP 实现（RFC 6238，SHA-1 / 6 位 / 30 秒），与 Google Authenticator、
// 1Password、Microsoft Authenticator 等主流客户端兼容。
//
// 自己实现的原因：少一个第三方依赖，这个算法本身只有几十行，
// 且必须理解其正确性（时间步长、时钟漂移容差）。

const (
	totpDigits = 6
	totpPeriod = 30 // 秒
	// totpSkew 允许前后各 1 个时间步，容忍客户端与服务器的时钟漂移。
	totpSkew = 1
)

// VerifyTOTP 校验一次性验证码。secret 为 base32（可带填充）。
func VerifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
	if len(code) != totpDigits {
		return false
	}
	key, err := decodeBase32(secret)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	for i := -totpSkew; i <= totpSkew; i++ {
		if subtle.ConstantTimeCompare([]byte(totpAt(key, now+int64(i*totpPeriod))), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// TOTPCodeAt 生成指定时间的验证码，供测试使用。
func TOTPCodeAt(secret string, t time.Time) string {
	key, err := decodeBase32(secret)
	if err != nil {
		return ""
	}
	return totpAt(key, t.Unix())
}

func totpAt(key []byte, unix int64) string {
	counter := uint64(unix / totpPeriod)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// 动态截断（RFC 4226 §5.3）
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, value%mod)
}

// decodeBase32 容忍大小写、空格、缺失填充 —— 用户手抄密钥时常见。
func decodeBase32(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(s, " ", "")))
	s = strings.TrimRight(s, "=")
	switch len(s) % 8 {
	case 2, 4, 5, 7:
		s += strings.Repeat("=", 8-len(s)%8)
	case 0:
		// 无需填充
	default:
		return nil, fmt.Errorf("密钥长度非法")
	}
	return base32.StdEncoding.DecodeString(s)
}

// TOTPProvisioningURI 生成 otpauth:// 链接，前端渲染成二维码给客户端扫。
func TOTPProvisioningURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
