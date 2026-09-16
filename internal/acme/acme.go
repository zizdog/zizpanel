// Package acme 实现 ACME 证书的申请与续期（http-01 / dns-01），
// 供面板的「自动申请证书」功能调用。本包只负责「签发 + 续期 + 存储」，
// 不碰面板的 HTTP 接口与前端。
//
// # 为什么用 github.com/go-acme/lego/v4 而不是自己写 ACME
//
//  1. ACME（RFC 8555）里账号注册、JWS 签名、nonce 防重放、订单/授权轮询、
//     错误分类与重试都是细节密集的活。自己写等于把最常见的安全问题
//     （nonce 复用、账号密钥管理、CSR 组装、重试幂等）重新踩一遍。
//  2. 只支持 http-01 无法申请 wildcard 证书 —— 通配符域名只能通过 dns-01
//     验证（ACME 规范如此），而 dns-01 要对接各家 DNS 服务商 API。
//  3. lego 内置 190+ DNS provider，涵盖国内常用的阿里云 / 腾讯云 / DNSPod /
//     华为云 / Cloudflare 等，自己维护这套 API 不现实。
//
// 所以本包只做「编排 + 落盘 + 可读错误 + 密钥保护」，协议部分全部交给 lego。
package acme

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/lego"
)

// ChallengeType 是 ACME 的域名验证方式。
type ChallengeType string

const (
	// ChallengeHTTP01 把挑战文件放到网站根目录，由 80 端口的 nginx 对外提供。
	// 优点：不需要 DNS API；缺点：不能申请 wildcard。
	ChallengeHTTP01 ChallengeType = "http-01"
	// ChallengeDNS01 在域名下写一条 _acme-challenge TXT 记录。
	// 优点：wildcard 的唯一途径，且不需要 80 端口可达；缺点：需要 DNS 服务商凭据。
	ChallengeDNS01 ChallengeType = "dns-01"
)

// CA 名称常量。对外只用这三个字符串。
const (
	// CALetsEncrypt 是 Let's Encrypt 生产环境（有速率限制）。
	CALetsEncrypt = "letsencrypt"
	// CALetsEncryptStaging 是 Let's Encrypt 测试环境（证书不受信任，但不占生产配额）。
	CALetsEncryptStaging = "letsencrypt-staging"
	// CAZeroSSL 是 ZeroSSL（需要 External Account Binding 凭据）。
	CAZeroSSL = "zerossl"
)

// zeroSSLDirectoryURL 是 ZeroSSL 的 ACME directory。
// lego 没有内置这个常量（lego 的 CLI 用 --server 传），所以在这里显式写死。
const zeroSSLDirectoryURL = "https://acme.zerossl.com/v2/DV90"

// 目录与文件名布局（相对于 New 的 dataDir）：
//
//	<dataDir>/certs/<primary>/fullchain.pem   0644  证书链（叶子 + 中间证书）
//	<dataDir>/certs/<primary>/privkey.pem     0600  私钥，绝不外泄
//	<dataDir>/certs/<primary>/meta.json       0644  Cert 结构，List/Load 读它
//	<dataDir>/certs/<primary>/renewal.json    0600  续期所需的原始请求（含 DNS 凭据）
//	<dataDir>/acme/accounts/<hash>.json       0600  ACME 账户私钥与账户 URL
const (
	certsDirName    = "certs"
	accountsDirName = "acme"
	accountsSubDir  = "accounts"

	fileMeta       = "meta.json"
	fileFullchain  = "fullchain.pem"
	filePrivkey    = "privkey.pem"
	fileRenewal    = "renewal.json"
	metaFileMode   = 0o644
	certFileMode   = 0o644
	keyFileMode    = 0o600
	dirMode        = 0o700
	defaultRenewal = 30
	// maxLegoLogLen 限制转发 lego 日志的单条长度，避免 CA 的错误页把任务日志刷爆。
	maxLegoLogLen = 2000
)

// DNSProviderSpec 描述一个 DNS 服务商凭据（走 lego 的 provider）。
type DNSProviderSpec struct {
	// Name 是 lego 的 provider code，例如 cloudflare / alidns / tencentcloud / dnspod。
	Name string
	// Env 是 lego 文档里的环境变量键值（如 CF_DNS_API_TOKEN / ALICLOUD_ACCESS_KEY ...）。
	// 这些值都是密钥，任何情况下都不写进日志；落盘时只进 0600 的 renewal.json。
	Env map[string]string
}

// IssueRequest 是一次签发请求。
type IssueRequest struct {
	Domains   []string         // 第一个必须是主域名；wildcard 用 *.example.com
	Email     string           // ACME 账户邮箱（可为空，但收不到到期提醒）
	Challenge ChallengeType    // 空 = http-01
	DNS       *DNSProviderSpec // ChallengeDNS01 时必填
	CA        string           // "letsencrypt" / "letsencrypt-staging" / "zerossl"，空 = letsencrypt
}

// Cert 是已保存证书的元信息，序列化进 meta.json。
type Cert struct {
	Primary   string        `json:"primary"`    // 主域名（也是存储目录名）
	Domains   []string      `json:"domains"`    //
	CertPath  string        `json:"cert_path"`  // fullchain.pem 绝对路径
	KeyPath   string        `json:"key_path"`   // privkey.pem 绝对路径
	Issuer    string        `json:"issuer"`     //
	NotBefore time.Time     `json:"not_before"` //
	NotAfter  time.Time     `json:"not_after"`  //
	Challenge ChallengeType `json:"challenge"`  //
	CA        string        `json:"ca"`         //
	UpdatedAt time.Time     `json:"updated_at"` //
}

// Manager 是证书引擎。零值不可用，必须用 New 创建。
//
// 除 New 的参数外，下面这些导出字段是「可选开关」，必须在调用 Issue/Renew
// 之前设置（面板启动时设置一次即可）。契约里的 New 签名保持不变。
type Manager struct {
	dataDir       string
	http01WebRoot string
	logf          func(string)

	// StagingFirst 为 true 时，只有「CA 留空」的请求会被送到 Let's Encrypt staging。
	// 用户显式选了 letsencrypt 时不会被覆盖：静默把生产请求降级成 staging，会让用户
	// 以为拿到正式证书而实际拿到浏览器不信任的测试证书，必须避免。
	// 为什么需要它：生产环境每个注册域名每周只有 5 次签发配额，真机第一次调试
	// （端口 / DNS 配置对不对）用 staging 验证完再切生产，不浪费配额。
	StagingFirst bool
	// RenewalDays 覆盖默认续期阈值（30 天），<=0 时按 30 天。
	RenewalDays int
	// KeyType 是证书私钥算法："rsa2048"(默认) / "rsa3072" / "rsa4096" / "ec256" / "ec384"。
	// 默认 RSA2048 是为了兼容老旧客户端；新部署可以选 ec256，握手更快、证书更小。
	KeyType string
	// DNSTimeout / DNSPolling 覆盖 dns-01 的传播等待（默认等 lego 的 60s / 2s）。
	// 国内 DNS 有时要几分钟才生效，可以调大 DNSTimeout。
	DNSTimeout time.Duration
	DNSPolling time.Duration
	// DNS01Resolvers 覆盖 dns-01 预检用的递归解析器，**必须是 IP 字面量**。
	// 留空时用 DefaultDNS01Resolvers（见 resolvers.go）。
	// 为什么不许写主机名：lego 默认值就是主机名，解析它要走系统解析器，
	// 在"路由器通告了不可达 IPv6 DNS"的网络里会让签发直接 i/o timeout（真机踩过）。
	DNS01Resolvers []string
	// DNS01FollowCNAME 为 true 时按 lego 默认行为**跟随** CNAME 委派
	// （把 TXT 写到 CNAME 目标上）。默认 false：泛解析 CNAME 会让 TXT 写到
	// CA 不看的地方，必然以 "No TXT record found" 失败（真机踩过，见 challenge.go）。
	DNS01FollowCNAME bool
	// EABKid / EABHmacKey 是 External Account Binding 凭据。
	// ZeroSSL 这类 CA 强制要求 EAB，没有它注册账户会被拒。
	EABKid     string
	EABHmacKey string
	// HTTPClient 可选：注入自定义 HTTP 客户端（代理 / 测试）。nil 表示用默认。
	HTTPClient *http.Client

	mu sync.Mutex
	// obtain 是真正与 CA 交互的钩子。默认走 lego；
	// 单测把它替换成假实现，从而在完全不联网的前提下验证存储 / 日志 / 覆盖逻辑。
	obtain obtainFunc
}

// obtainFunc 是 Issue/Renew 与 CA 交互的唯一入口，便于单测替换。
type obtainFunc func(ctx context.Context, plan issuePlan) (*obtainedCert, error)

// obtainedCert 是 CA 返回的原始 PEM 材料。
type obtainedCert struct {
	fullchain []byte // 证书链 PEM
	key       []byte // 私钥 PEM
}

// issuePlan 是校验 / 归一化之后的签发计划。
type issuePlan struct {
	domains   []string
	primary   string
	email     string
	challenge ChallengeType
	dns       *DNSProviderSpec
	caName    string // 归一化后的 CA 名称（写进 Cert.CA）
	caURL     string // ACME directory URL
	keyType   certcrypto.KeyType
}

// New 创建引擎。dataDir 是证书存储根（面板会传 Cfg.DataDir）；http01WebRoot 是
// http-01 挑战文件的落盘目录（面板会传它的网站根）；logf 用于进度输出。
func New(dataDir, http01WebRoot string, logf func(string)) *Manager {
	if logf == nil {
		logf = func(string) {}
	}
	m := &Manager{
		dataDir:       dataDir,
		http01WebRoot: http01WebRoot,
		logf:          logf,
	}
	m.obtain = m.legoObtain
	// lego 库内部有自己的日志（写 stderr）。把它接到本包的日志出口，
	// 这样进度能进面板任务中心，而且会经过我们的脱敏（见 logging.go）。
	// 只装一次，进程内所有 Manager 共用。
	installLegoLogger()
	return m
}

// CADirURL 返回 CA 名称对应的 ACME directory URL；未知名称返回空串。
// 单测用它断言 CA 选择逻辑，不需要真的发请求。
func CADirURL(ca string) string {
	_, url, err := resolveCA(ca, false)
	if err != nil {
		return ""
	}
	return url
}

// resolveCA 把用户可读的 CA 名称解析成 (标准名, directory URL)。
//
// stagingFirst 的语义被刻意限制为「只影响空 CA」：如果用户显式选了 letsencrypt，
// 就必须真的走生产。静默降级到 staging 会让用户以为拿到正式证书、实际拿到浏览器
// 不信任的测试证书 —— 这属于「没有如实执行并如实报告」，比报错更糟。
// 用户想先验证配置时，应该显式选 letsencrypt-staging（面板下拉里有这一项）。
func resolveCA(ca string, stagingFirst bool) (name, url string, err error) {
	switch strings.ToLower(strings.TrimSpace(ca)) {
	case "":
		// 用户没选，才允许 staging 优先。
		if stagingFirst {
			return CALetsEncryptStaging, lego.LEDirectoryStaging, nil
		}
		return CALetsEncrypt, lego.LEDirectoryProduction, nil
	case "letsencrypt", "le":
		return CALetsEncrypt, lego.LEDirectoryProduction, nil
	case "letsencrypt-staging", "staging":
		return CALetsEncryptStaging, lego.LEDirectoryStaging, nil
	case "zerossl":
		return CAZeroSSL, zeroSSLDirectoryURL, nil
	default:
		return "", "", fmt.Errorf("未知的 CA %q：只支持 %s / %s / %s",
			ca, CALetsEncrypt, CALetsEncryptStaging, CAZeroSSL)
	}
}

// NeedsRenewal 报告证书是否应在 now 之前续期（默认阈值 30 天）。
//
// 判定用「剩余时间 <= 阈值」而不是「<」：剩余正好 30 天时应当续期，
// 这样阈值是一个闭区间，边界行为对使用者更好解释。
func NeedsRenewal(c *Cert, now time.Time, days int) bool {
	if days <= 0 {
		days = defaultRenewal
	}
	if c == nil || c.NotAfter.IsZero() {
		// 没有可信的到期时间，保守认为需要续期（重新签一次总比过期强）。
		return true
	}
	return !c.NotAfter.After(now.Add(time.Duration(days) * 24 * time.Hour))
}

// ShouldRenew 是 NeedsRenewal 的便捷版本：用 Manager.RenewalDays（默认 30 天）
// 作为阈值。面板的定时任务可以直接用它，不必自己记阈值。
func (m *Manager) ShouldRenew(c *Cert, now time.Time) bool {
	return NeedsRenewal(c, now, m.RenewalDays)
}

// keyTypeOrDefault 把可读的算法名映射成 lego 的 KeyType。
func keyTypeOrDefault(s string) (certcrypto.KeyType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "rsa", "rsa2048":
		return certcrypto.RSA2048, nil
	case "rsa3072":
		return certcrypto.RSA3072, nil
	case "rsa4096":
		return certcrypto.RSA4096, nil
	case "ec", "ecdsa", "ec256", "ecdsa256":
		return certcrypto.EC256, nil
	case "ec384", "ecdsa384":
		return certcrypto.EC384, nil
	default:
		return "", fmt.Errorf("不支持的私钥算法 %q：可选 rsa2048 / rsa3072 / rsa4096 / ec256 / ec384", s)
	}
}

// wildcardRe 只允许一个 "*." 前缀。
var wildcardRe = regexp.MustCompile(`^\*\.`)

// normalizeDomain 校验并归一化域名（小写、去尾部点）。
// allowWildcard=false 时，`*.example.com` 会被明确拒绝。
func normalizeDomain(raw string, allowWildcard bool) (string, error) {
	d := strings.ToLower(strings.TrimSpace(raw))
	d = strings.TrimSuffix(d, ".")

	if d == "" {
		return "", errors.New("域名不能为空")
	}
	if strings.ContainsAny(d, "/\\ \t\r\n") {
		return "", fmt.Errorf("域名 %q 含非法字符", raw)
	}
	// 只允许一个 "*." 前缀，且必须开启 wildcard 才接受。
	if strings.HasPrefix(d, "*") {
		if !allowWildcard {
			return "", fmt.Errorf("域名 %q 是通配符，http-01 无法验证；请改用 dns-01", raw)
		}
		if !wildcardRe.MatchString(d) || strings.Count(d, "*") != 1 {
			return "", fmt.Errorf("通配符域名 %q 非法：只支持 *.example.com 形式", raw)
		}
	}
	if d == "" || len(d) > 253 {
		return "", fmt.Errorf("域名 %q 长度非法", raw)
	}

	body := strings.TrimPrefix(d, "*.")
	labels := strings.Split(body, ".")
	if len(labels) < 2 {
		// ACME 只能给公开后缀下的 FQDN 签发，单标签主机名没有意义。
		return "", fmt.Errorf("域名 %q 不是完整域名（至少要 example.com 这样两段）", raw)
	}
	for _, label := range labels {
		if err := validateLabel(label); err != nil {
			return "", fmt.Errorf("域名 %q: %w", raw, err)
		}
	}
	return d, nil
}

// validateLabel 校验单个 DNS 标签。只接受 ASCII 字母数字与连字符，
// 明确拒绝 IDN（中文域名）——面板与 nginx 都按 ASCII 处理，静默接受
// 只会在签发阶段以更难懂的方式失败。
func validateLabel(label string) error {
	if label == "" {
		return errors.New("存在空的域名段（连续的点）")
	}
	if len(label) > 63 {
		return errors.New("域名段超过 63 字节")
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return fmt.Errorf("域名段 %q 不能以连字符开头或结尾", label)
	}
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return fmt.Errorf("域名段 %q 含非法字符 %q（不支持中文等非 ASCII 域名）", label, r)
	}
	return nil
}

// validatePrimary 校验存储目录名。它就是主域名，允许 wildcard，
// 但不允许任何路径分隔符，防止 Delete/Load 被 "../" 逃出证书目录。
func validatePrimary(primary string) error {
	if strings.ContainsAny(primary, "/\\") || primary == "." || primary == ".." {
		return fmt.Errorf("证书名 %q 非法：不允许路径分隔符", primary)
	}
	if _, err := normalizeDomain(primary, true); err != nil {
		return fmt.Errorf("证书名 %q 非法: %w", primary, err)
	}
	return nil
}

// prepare 校验并归一化一次签发请求。
func (m *Manager) prepare(req IssueRequest) (issuePlan, error) {
	challenge := req.Challenge
	if challenge == "" {
		// 空值按最常用的 http-01 处理，避免面板忘记设置时报出难懂的错。
		challenge = ChallengeHTTP01
	}
	if challenge != ChallengeHTTP01 && challenge != ChallengeDNS01 {
		return issuePlan{}, fmt.Errorf("未知的验证方式 %q：只支持 %s / %s", req.Challenge, ChallengeHTTP01, ChallengeDNS01)
	}
	allowWildcard := challenge == ChallengeDNS01

	if len(req.Domains) == 0 {
		return issuePlan{}, errors.New("至少需要一个域名")
	}

	// 去重但保持顺序：第一个域名是主域名，也是证书目录名。
	seen := make(map[string]bool, len(req.Domains))
	domains := make([]string, 0, len(req.Domains))
	for _, raw := range req.Domains {
		d, err := normalizeDomain(raw, allowWildcard)
		if err != nil {
			return issuePlan{}, err
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		domains = append(domains, d)
	}
	if len(domains) == 0 {
		return issuePlan{}, errors.New("至少需要一个域名")
	}
	// wildcard 必须 dns-01：normalizeDomain 已按 allowWildcard 拦过一遍，
	// 这里再兜一次底，防止以后有人改坏上面的分支。
	if challenge == ChallengeHTTP01 {
		for _, d := range domains {
			if strings.HasPrefix(d, "*.") {
				return issuePlan{}, fmt.Errorf("域名 %q 是通配符，http-01 无法验证；通配符只能用 dns-01", d)
			}
		}
	}

	if challenge == ChallengeDNS01 {
		if req.DNS == nil || strings.TrimSpace(req.DNS.Name) == "" {
			return issuePlan{}, errors.New("dns-01 必须提供 DNS 服务商（DNS.Name 为空）")
		}
	}

	if m.dataDir == "" {
		return issuePlan{}, errors.New("未配置数据目录（dataDir），无法保存证书")
	}
	if challenge == ChallengeHTTP01 && strings.TrimSpace(m.http01WebRoot) == "" {
		return issuePlan{}, errors.New("http-01 需要网站根目录（http01WebRoot）来放挑战文件；如需通配符请改用 dns-01")
	}

	keyType, err := keyTypeOrDefault(m.KeyType)
	if err != nil {
		return issuePlan{}, err
	}
	caName, caURL, err := resolveCA(req.CA, m.StagingFirst)
	if err != nil {
		return issuePlan{}, err
	}

	email := strings.TrimSpace(req.Email)
	if email == "" {
		m.emit("未填写邮箱：证书到期前的提醒邮件将收不到（不影响签发）")
	}

	return issuePlan{
		domains:   domains,
		primary:   domains[0],
		email:     email,
		challenge: challenge,
		dns:       req.DNS,
		caName:    caName,
		caURL:     caURL,
		keyType:   keyType,
	}, nil
}

// sortCerts 让 List 的输出稳定（按主域名排序），便于前端与测试比对。
func sortCerts(list []*Cert) {
	sort.Slice(list, func(i, j int) bool { return list[i].Primary < list[j].Primary })
}
