package upgrade

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Manifest 是发布清单元数据，由 `make release` 生成并签名。
//
// 为什么中间要加一层"清单"而不是直接签名 tar 包：
// 签名文件是**逐个版本**产出的，而面板在检查更新时还不知道该下载哪个包。
// 清单里一次性说明"最新版本是什么、每个架构的包在哪、SHA-256 是多少"，
// 我们只签这一份小文件（几百字节），既省流量又只需要校验一个签名。
type Manifest struct {
	Version     string                 `json:"version"`
	Notes       string                 `json:"notes,omitempty"`
	PublishedAt string                 `json:"published_at,omitempty"`
	Assets      map[string]ManifestRef `json:"assets"`
}

// ManifestRef 描述某个架构对应的发布包。
type ManifestRef struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

// AssetKey 返回当前机器对应的资源键，如 darwin_arm64。
func AssetKey(goos, goarch string) string {
	return goos + "_" + goarch
}

// PubKeyHex 是内嵌的发布公钥（Ed25519，32 字节的十六进制）。
//
// 由 `make keys` 生成、`make release` 用它对应的私钥签名。
// 为空表示"尚未配置发布公钥"，此时**拒绝**从网络升级（fail closed）：
// 宁可让功能不可用，也不能在没有验证手段的情况下远程替换 root 二进制。
//
// 这里刻意只存公钥。私钥在 .release-key/ 下（已 gitignore），
// 绝不能进仓库、也不能出现在发布包里。
var PubKeyHex = ""

// SetPublicKey 允许从构建变量注入公钥（-ldflags -X）。
func SetPublicKey(hexKey string) { PubKeyHex = strings.TrimSpace(hexKey) }

// PublicKeyHex 返回当前生效的公钥（空串表示未配置）。
//
// 这个访问器存在的首要目的不是"给外面看"，而是**让这个变量真的被引用**。
//
// 踩过的坑：`go build -trimpath` 会把不可达的符号做死代码消除，
// 而 `-X` 在找不到符号时**静默忽略**（退出码依然是 0、构建日志毫无异常），
// 于是发布出来的二进制根本没带上公钥 —— 后果是面板永远拒绝网络升级，
// 而且极难从表面看出原因。main 直接引用它即可避免被剪掉，
// 同时发布流程必须**运行产物**去核对，而不是相信"构建成功"。
func PublicKeyHex() string { return strings.TrimSpace(PubKeyHex) }

// PublicKeyFingerprint 返回公钥短指纹，用于界面展示与发布校验。
func PublicKeyFingerprint() string {
	raw := PublicKeyHex()
	if len(raw) < 16 {
		return raw
	}
	return raw[:16]
}

// ErrNoPublicKey 表示没有配置发布公钥，无法验证远端清单。
var ErrNoPublicKey = errors.New("未配置发布公钥，无法验证远端升级包的签名")

// ParseManifest 解析清单 JSON。
//
// 严格模式：拒绝未知字段。这样如果将来清单格式升级、
// 而面板还是旧版本，会明确报错，而不是静默忽略掉新增的安全相关字段
// （比如将来加的"最低可升级版本"约束）。
func ParseManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("清单格式不正确: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("清单缺少 version 字段")
	}
	if _, _, err := parseSemver(m.Version); err != nil {
		return fmt.Errorf("清单里的版本号不合法: %w", err)
	}
	if len(m.Assets) == 0 {
		return errors.New("清单里没有任何可下载的资源（assets 为空）")
	}
	for key, ref := range m.Assets {
		if strings.TrimSpace(ref.URL) == "" {
			return fmt.Errorf("资源 %s 缺少 url", key)
		}
		if err := validateSHA256(ref.SHA256); err != nil {
			return fmt.Errorf("资源 %s 的 sha256 不合法: %w", key, err)
		}
	}
	return nil
}

// Pick 取出指定架构的资源，不存在时给出人话错误。
func (m *Manifest) Pick(goos, goarch string) (ManifestRef, error) {
	key := AssetKey(goos, goarch)
	ref, ok := m.Assets[key]
	if !ok {
		keys := make([]string, 0, len(m.Assets))
		for k := range m.Assets {
			keys = append(keys, k)
		}
		return ManifestRef{}, fmt.Errorf("这个版本没有提供 %s 的安装包（清单里只有 %s）", key, strings.Join(keys, ", "))
	}
	return ref, nil
}

// VerifyManifest 用内嵌公钥验证清单签名。
//
// 这是整个升级流程里唯一一次"信任根"判断：只要这一关过了，
// 后面 tar 包的 SHA-256 就是可信的，不需要再验签。
func VerifyManifest(data, sig []byte) error {
	pub, err := publicKey()
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("签名长度不对（%d 字节，应为 %d）", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, data, sig) {
		return errors.New("清单签名验证失败：升级包不是用配对的私钥签的，已拒绝")
	}
	return nil
}

// publicKey 解析内嵌公钥。
func publicKey() (ed25519.PublicKey, error) {
	raw := strings.TrimSpace(PubKeyHex)
	if raw == "" {
		return nil, ErrNoPublicKey
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("内嵌公钥不是合法十六进制: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("内嵌公钥长度不对（%d 字节，应为 %d）", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// HasPublicKey 报告是否已经配置了发布公钥（前端据此决定是否显示"从网络升级"）。
func HasPublicKey() bool {
	_, err := publicKey()
	return err == nil
}

// SignManifest 用给定私钥签名清单（发布流程使用，面板运行时不会调用）。
func SignManifest(priv ed25519.PrivateKey, data []byte) []byte {
	return ed25519.Sign(priv, data)
}

// LoadPrivateKey 从文件读取私钥（发布流程使用）。
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("私钥不是合法十六进制: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	}
	return nil, fmt.Errorf("私钥长度不对（%d 字节）", len(raw))
}

// FileSHA256 计算文件摘要，返回小写十六进制。
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateSHA256(s string) error {
	s = strings.TrimSpace(s)
	if len(s) != 64 {
		return fmt.Errorf("长度应为 64 个十六进制字符，实际 %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return errors.New("含有非十六进制字符")
	}
	return nil
}

// EqualSHA256 做大小写无关的摘要比较。
func EqualSHA256(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
