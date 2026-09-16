package acme

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"

	"github.com/zizdog/zizpanel/internal/version"
)

// Issue 申请新证书（或覆盖同名证书）。已存在同主域名证书时应正常覆盖。
//
// 覆盖策略：只有在 CA 成功签发之后才写文件，因此申请失败时旧证书原样保留，
// 线上 TLS 不会因为一次失败的重签而中断。
func (m *Manager) Issue(ctx context.Context, req IssueRequest) (*Cert, error) {
	// lego 的全局 logger 无法按 Manager 区分，这里把日志出口指向本次调用者。
	setLogf(m.logf)
	plan, err := m.prepare(req)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issue(ctx, plan, req)
}

// Renew 续期：按主域名找到现有证书，用同样的域名/验证方式重签。
//
// 为什么需要 renewal.json：面板只传主域名，而 dns-01 重签必须再次拿到 DNS
// 凭据与域名列表；这些信息在 Issue 时存到了 0600 的 renewal.json 里。
// 如果该文件缺失（例如手工拷贝了证书目录），会明确要求重新申请。
func (m *Manager) Renew(ctx context.Context, primary string) (*Cert, error) {
	setLogf(m.logf)

	primary = strings.TrimSpace(primary)
	if err := validatePrimary(primary); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.loadRenewal(primary)
	if err != nil {
		return nil, fmt.Errorf("无法续期 %s：%w（请重新申请一次）", primary, err)
	}
	plan, err := m.prepare(rec.Request)
	if err != nil {
		return nil, fmt.Errorf("无法续期 %s：%w（请重新申请一次）", primary, err)
	}
	if plan.primary != primary {
		return nil, fmt.Errorf("续期信息与目录不匹配：目录 %s 但记录里的主域名是 %s", primary, plan.primary)
	}
	m.emit("开始续期证书 %s", primary)
	return m.issue(ctx, plan, rec.Request)
}

// List 列出本机已保存的证书（读 meta.json）。
func (m *Manager) List() ([]*Cert, error) {
	if m.dataDir == "" {
		return nil, errors.New("未配置数据目录（dataDir）")
	}
	entries, err := os.ReadDir(m.certsRoot())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 还没有任何证书不是错误，返回空列表。
			return []*Cert{}, nil
		}
		return nil, fmt.Errorf("读取证书目录失败: %w", err)
	}

	out := make([]*Cert, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := m.loadMeta(e.Name())
		if err != nil {
			// 坏目录不能让整个列表失败：跳过并记录，用户仍能看到其它证书。
			m.emit("跳过无法读取的证书目录 %s：%v", e.Name(), err)
			continue
		}
		out = append(out, c)
	}
	sortCerts(out)
	return out, nil
}

// Load 读单个证书元信息。
func (m *Manager) Load(primary string) (*Cert, error) {
	return m.loadMeta(strings.TrimSpace(primary))
}

// Delete 删除证书（证书文件 + 元信息目录）。
// ACME 账户刻意保留：删掉证书不需要重新注册账户，重新申请时可以继续用。
func (m *Manager) Delete(primary string) error {
	primary = strings.TrimSpace(primary)
	if err := validatePrimary(primary); err != nil {
		return err
	}
	dir := m.certDir(primary)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("证书 %s 不存在", primary)
		}
		return fmt.Errorf("读取证书目录失败: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("删除证书目录 %s 失败: %w", primary, err)
	}
	m.emit("已删除证书 %s", primary)
	return nil
}

// issue 是 Issue/Renew 的共同实现（调用方必须已持有 m.mu）。
func (m *Manager) issue(ctx context.Context, plan issuePlan, req IssueRequest) (*Cert, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("任务已取消: %w", err)
	}
	m.emit("开始申请证书：%s（CA=%s，验证=%s）",
		strings.Join(plan.domains, ", "), plan.caName, plan.challenge)

	obtain := m.obtain
	if obtain == nil {
		obtain = m.legoObtain
	}
	res, err := obtain(ctx, plan)
	if err != nil {
		// 统一再包一层脱敏：即使某个 provider 把凭据写进了错误文本，
		// 返回给面板的 error 与写进日志的文本也都不含密钥。
		// legoObtain 已经返回过 safeError，就不重复包装，避免错误信息套娃。
		if _, ok := err.(*safeError); !ok {
			err = safeWrap(err, "申请证书失败")
		}
		m.emit("申请失败：%v", err)
		return nil, err
	}
	if len(res.fullchain) == 0 || len(res.key) == 0 {
		return nil, errors.New("CA 返回的证书或私钥为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("任务已取消: %w", err)
	}

	c, err := m.buildCert(plan, res)
	if err != nil {
		return nil, err
	}
	if err := m.saveCert(plan, c, res, planRequest(plan, req)); err != nil {
		return nil, fmt.Errorf("证书已签发但保存失败: %w", err)
	}
	m.emit("已保存证书到 %s（到期时间 %s）",
		m.certDir(c.Primary), c.NotAfter.Format("2006-01-02 15:04:05"))
	return c, nil
}

// planRequest 从归一化后的计划还原出「用于续期」的请求。
// 存归一化后的域名（小写/去重/去尾点），保证 Renew 与 Issue 完全一致。
// CA 保留用户原始写法，这样 StagingFirst 等开关在续期时仍然生效。
func planRequest(plan issuePlan, req IssueRequest) IssueRequest {
	return IssueRequest{
		Domains:   append([]string(nil), plan.domains...),
		Email:     plan.email,
		Challenge: plan.challenge,
		DNS:       req.DNS,
		CA:        req.CA,
	}
}

// buildCert 从 CA 返回的证书链里解析出元信息。
func (m *Manager) buildCert(plan issuePlan, res *obtainedCert) (*Cert, error) {
	leaf, err := certcrypto.ParsePEMCertificate(res.fullchain)
	if err != nil {
		return nil, fmt.Errorf("解析 CA 返回的证书失败: %w", err)
	}
	issuer := leaf.Issuer.CommonName
	if issuer == "" {
		issuer = leaf.Issuer.String()
	}
	dir := m.certDir(plan.primary)
	return &Cert{
		Primary:   plan.primary,
		Domains:   append([]string(nil), plan.domains...),
		CertPath:  filepath.Join(dir, fileFullchain),
		KeyPath:   filepath.Join(dir, filePrivkey),
		Issuer:    issuer,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Challenge: plan.challenge,
		CA:        plan.caName,
		UpdatedAt: time.Now(),
	}, nil
}

// ---------------- lego 交互 ----------------

// legoObtain 是默认的 obtain 实现：注册账户 → 配置验证方式 → 向 CA 申请。
func (m *Manager) legoObtain(ctx context.Context, plan issuePlan) (*obtainedCert, error) {
	user, err := m.loadOrCreateAccount(plan.caName, plan.email)
	if err != nil {
		return nil, err
	}

	cfg := lego.NewConfig(user)
	cfg.CADirURL = plan.caURL
	cfg.UserAgent = userAgent()
	// 证书私钥算法；lego 在每次 Obtain 时生成新私钥并返回。
	cfg.Certificate.KeyType = plan.keyType
	// 把面板的 context 接到 HTTP 传输上，让「取消任务」能真正中断在途请求。
	cfg.HTTPClient = m.httpClient(ctx)

	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, safeWrap(err, "初始化 ACME 客户端失败（CA=%s，directory=%s；请检查服务器能否访问该 CA）",
			plan.caName, plan.caURL)
	}

	if plan.challenge == ChallengeDNS01 {
		prv, restore, err := m.dnsProvider(plan.dns)
		if err != nil {
			return nil, err
		}
		// 凭据以进程环境变量的形式注入（lego provider 只从环境变量读），
		// 签发结束必须还原，避免污染面板进程的其它逻辑。
		defer restore()
		// 默认不跟随 CNAME：泛解析 `* CNAME target` 会把 TXT 写到 target 上，
		// 而 Let's Encrypt 不跟随泛解析 CNAME → 必然 "No TXT record found"。
		restoreCNAME := m.applyCNAMEPolicy()
		defer restoreCNAME()
		// 预检用的递归解析器必须显式给 IPv4 字面量：lego 的默认值是
		// google-public-dns-a.google.com:53 这种主机名，解析它要走系统解析器，
		// 而路由器通告的 IPv6 DNS 一旦不可达，整条签发就挂在 i/o timeout 上
		// （2026-09-16 mini 真机故障，见 resolvers.go）。
		if err := client.Challenge.SetDNS01Provider(prv,
			dns01.AddRecursiveNameservers(m.dns01Resolvers())); err != nil {
			return nil, safeWrap(err, "配置 dns-01 验证失败（provider=%s）", plan.dns.Name)
		}
	} else {
		if err := client.Challenge.SetHTTP01Provider(&webrootProvider{
			root: m.http01WebRoot,
			emit: m.emit,
		}); err != nil {
			return nil, safeWrap(err, "配置 http-01 验证失败（网站根目录=%s）", m.http01WebRoot)
		}
	}

	// 首次使用该 (CA, 邮箱) 时注册账户。注册信息（含私钥）落 0600 文件，
	// 之后复用，避免每次签发都新建账户撞上 CA 的账户数量限制。
	if user.reg == nil {
		if client.GetExternalAccountRequired() && strings.TrimSpace(m.EABKid) == "" {
			return nil, fmt.Errorf("CA %s 要求 External Account Binding（EAB）：请在 Manager 上配置 EABKid / EABHmacKey（ZeroSSL 控制台可生成）", plan.caName)
		}
		m.emit("正在注册 ACME 账户（CA=%s）", plan.caName)
		reg, err := registerAccount(client, m.EABKid, m.EABHmacKey)
		if err != nil {
			return nil, safeWrap(err, "注册 ACME 账户失败（CA=%s）%s%s",
				plan.caName, hintFor(err, plan.challenge), emailHint(plan.email))
		}
		user.reg = reg
		if err := m.saveAccount(plan.caName, user); err != nil {
			// 账户已在 CA 侧建立，只是本地没存下；不致命，但必须让用户知道
			// 下次签发会再注册一个账户（长期会撞配额）。
			m.emit("警告：ACME 账户已注册但本地保存失败，下次签发会重新注册：%v", err)
		}
	}

	m.emit("正在向 CA 申请证书：%s", strings.Join(plan.domains, ", "))
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: plan.domains,
		// Bundle=true：Certificate 字段是「叶子 + 中间证书」的完整链，
		// 正是 nginx 的 ssl_certificate 需要的 fullchain.pem。
		Bundle: true,
	})
	if err != nil {
		return nil, safeWrap(err, "向 CA 申请证书失败（验证方式 %s）%s", plan.challenge, hintFor(err, plan.challenge))
	}
	if len(res.Certificate) == 0 || len(res.PrivateKey) == 0 {
		return nil, errors.New("CA 返回了空的证书或私钥")
	}
	return &obtainedCert{fullchain: res.Certificate, key: res.PrivateKey}, nil
}

// registerAccount 注册 ACME 账户。配置了 EAB 时走带 EAB 的注册流程。
//
// TermsOfServiceAgreed=true 是自动化场景的必要妥协：面板无法在 CLI 里等用户
// 点「同意」。用户在使用「自动申请证书」时即视为同意 CA 的服务条款，
// 因此前端必须把 CA 的 ToS 链接展示出来（接入层负责）。
func registerAccount(c *lego.Client, kid, hmac string) (*registration.Resource, error) {
	if strings.TrimSpace(kid) != "" && strings.TrimSpace(hmac) != "" {
		return c.Registration.RegisterWithExternalAccountBinding(registration.RegisterEABOptions{
			TermsOfServiceAgreed: true,
			Kid:                  kid,
			HmacEncoded:          hmac,
		})
	}
	return c.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
}

// emailHint 把「没填邮箱」变成一句可读的提醒（邮箱本身不算敏感信息）。
func emailHint(email string) string {
	if strings.TrimSpace(email) == "" {
		return "；未填写邮箱，将无法收到 CA 的到期提醒"
	}
	return ""
}

// httpClient 构造带 context 的 HTTP 客户端。
//
// 为什么不用 lego 默认客户端：lego 的 API 不接收 context，取消任务时在途的
// ACME 请求会继续跑到超时，面板上表现为「点了取消但任务还在跑」。
// 这里包一层 RoundTripper，把 ctx 嫁接到每个请求上。
func (m *Manager) httpClient(ctx context.Context) *http.Client {
	base := http.DefaultTransport
	timeout := 2 * time.Minute
	if m.HTTPClient != nil {
		if m.HTTPClient.Transport != nil {
			base = m.HTTPClient.Transport
		}
		if m.HTTPClient.Timeout > 0 {
			timeout = m.HTTPClient.Timeout
		}
	}
	return &http.Client{Transport: &ctxTransport{base: base, ctx: ctx}, Timeout: timeout}
}

// ctxTransport 给每个请求绑定调用方的 context。
type ctxTransport struct {
	base http.RoundTripper
	ctx  context.Context
}

func (t *ctxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ctx == nil {
		return t.base.RoundTrip(req)
	}
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req.Clone(t.ctx))
}

// userAgent 让 CA 侧能识别请求来自面板，便于排查。
func userAgent() string {
	v := version.Version
	if v == "" {
		v = "dev"
	}
	return "ZizPanel/" + v + " (lego)"
}

// ---------------- DNS provider ----------------

// dnsProvider 按名称构造 lego 的 DNS provider，并注入凭据环境变量。
func (m *Manager) dnsProvider(spec *DNSProviderSpec) (challenge.Provider, func(), error) {
	if spec == nil || strings.TrimSpace(spec.Name) == "" {
		return nil, func() {}, errors.New("dns-01 缺少 DNS 服务商配置")
	}
	restore, err := applyDNSEnv(spec)
	if err != nil {
		return nil, func() {}, err
	}
	// NewDNSChallengeProviderByName 是 lego 的 provider 注册表入口，
	// provider 自己从环境变量读凭据（lego 文档里的 CF_DNS_API_TOKEN 等）。
	raw, err := dns.NewDNSChallengeProviderByName(strings.TrimSpace(spec.Name))
	if err != nil {
		restore()
		return nil, func() {}, fmt.Errorf(
			"不支持的 DNS 服务商 %q（应填 lego 的 provider code，例如 cloudflare / alidns / tencentcloud / dnspod）: %w",
			spec.Name, err)
	}
	timeout, interval := m.dnsTiming(raw)
	return &loggingDNSProvider{inner: raw, emit: m.emit, timeout: timeout, interval: interval}, restore, nil
}

// dnsTiming 决定 dns-01 的传播等待时间。
// 面板可显式覆盖（国内 DNS 常要几分钟），否则沿用 provider 自己的建议值。
func (m *Manager) dnsTiming(p challenge.Provider) (time.Duration, time.Duration) {
	if m.DNSTimeout > 0 || m.DNSPolling > 0 {
		timeout, interval := m.DNSTimeout, m.DNSPolling
		if timeout <= 0 {
			timeout = dns01.DefaultPropagationTimeout
		}
		if interval <= 0 {
			interval = dns01.DefaultPollingInterval
		}
		return timeout, interval
	}
	if pt, ok := p.(challenge.ProviderTimeout); ok {
		return pt.Timeout()
	}
	return dns01.DefaultPropagationTimeout, dns01.DefaultPollingInterval
}

// ---------------- ACME 账户 ----------------

// acmeUser 实现 lego 的 registration.User。
type acmeUser struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.reg }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// loadOrCreateAccount 读取或新建 ACME 账户。
// 账户私钥只落 0600 文件，绝不进日志与错误信息。
func (m *Manager) loadOrCreateAccount(caName, email string) (*acmeUser, error) {
	root := m.accountsRoot()
	if err := ensureDir(root); err != nil {
		return nil, err
	}
	path := accountPath(root, caName, email)

	if data, err := os.ReadFile(path); err == nil {
		var rec accountRecord
		switch {
		case json.Unmarshal(data, &rec) != nil:
			m.emit("ACME 账户文件无法解析，将重新注册账户（CA=%s）", caName)
		default:
			key, kerr := certcrypto.ParsePEMPrivateKey([]byte(rec.KeyPEM))
			if kerr != nil {
				// 这里不把 kerr 打印出来：它可能包含密钥材料的片段。
				m.emit("ACME 账户私钥无法解析，将重新注册账户（CA=%s）", caName)
				break
			}
			u := &acmeUser{email: rec.Email, key: key}
			if u.email == "" {
				u.email = email
			}
			if rec.URI != "" {
				u.reg = &registration.Resource{URI: rec.URI}
			}
			return u, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 ACME 账户失败: %w", err)
	}

	key, err := certcrypto.GeneratePrivateKey(certcrypto.RSA2048)
	if err != nil {
		return nil, fmt.Errorf("生成 ACME 账户密钥失败: %w", err)
	}
	return &acmeUser{email: email, key: key}, nil
}

// saveAccount 持久化 ACME 账户（0600）。
func (m *Manager) saveAccount(caName string, u *acmeUser) error {
	root := m.accountsRoot()
	if err := ensureDir(root); err != nil {
		return err
	}
	rec := accountRecord{Email: u.email, CA: caName, CreatedAt: time.Now()}
	if u.reg != nil {
		rec.URI = u.reg.URI
	}
	rec.KeyPEM = string(certcrypto.PEMEncode(u.key))

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 ACME 账户失败: %w", err)
	}
	return writeFileAtomic(accountPath(root, caName, u.email), data, keyFileMode)
}
