package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/acme"
	"github.com/zizdog/zizpanel/internal/scheduler"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tasks"
	"github.com/zizdog/zizpanel/internal/tlsx"
)

// ============================================================================
//  ACME 证书（Let's Encrypt / ZeroSSL …）—— 面板后端接入
//
//  分工：
//    - internal/acme（另一个代理实现，接口已冻结）：真正的 ACME 协议、
//      校验、证书落盘（<DataDir>/certs/<primary>/）。
//    - 本文件：HTTP 接口、DNS 凭据保管、站点侧"直接可用"、每日自动续期，
//      以及把每一步的真实结果写进任务中心与审计。
//
//  三条不能破的底线：
//    1. **私钥内容永不进接口**。这里只暴露 cert_path / key_path 这类路径。
//    2. **DNS token 只存服务端**（DataDir 下 0600 文件），接口只回
//       "已配置/未配置"，绝不回显用户填过的值。
//    3. 长任务（签发/续期要等 CA 校验几十秒到几分钟）一律走任务中心，
//       失败必须可见 —— 不许静默、不许谎报成功。
// ============================================================================

const (
	// certRenewThresholdDays 是自动续期阈值：到期前 30 天开始续。
	// 30 天是行业惯例（Let's Encrypt 证书 90 天有效，留足重试与排障时间）。
	certRenewThresholdDays = 30

	// certRenewHour / certRenewMinute 是每日检查的时刻。
	// 刻意避开 03:00 —— 那是计划任务预设里的 daily-backup，别让续期和备份抢 IO。
	certRenewHour, certRenewMinute = 3, 17

	// certCredFileMode 是凭据文件的权限：只有面板进程自己能读。
	certCredFileMode = 0o600
)

// acmeManager 是 web 层用到的 ACME 能力子集。
//
// 为什么要接口：单测必须能在**不联网、不碰真实 CA** 的前提下验证
// handler 的校验/落库/回显逻辑，所以测试注入假实现。
// 生产实现就是 *acme.Manager —— 方法签名与冻结的接口逐字一致。
type acmeManager interface {
	Issue(ctx context.Context, req acme.IssueRequest) (*acme.Cert, error)
	Renew(ctx context.Context, primary string) (*acme.Cert, error)
	List() ([]*acme.Cert, error)
	Load(primary string) (*acme.Cert, error)
	Delete(primary string) error
}

// certManager 返回 ACME 管理器（惰性构造）。
//
// 惰性构造的原因：构造它要读 DataDir，而 web.New 之后马上就有请求来；
// 没配 ACME 的面板不该因为这一个功能在启动时就有副作用。
func (s *Server) certManager() acmeManager {
	s.acmeMu.Lock()
	defer s.acmeMu.Unlock()
	if s.acmeOverride != nil {
		return s.acmeOverride
	}
	if s.acmeMgr == nil {
		m := acme.New(s.Cfg.DataDir, s.acmeHTTP01WebRoot(), func(msg string) {
			s.Log.Info("[acme] %s", msg)
		})
		// 面板自己的续期阈值也要告诉引擎，避免两处各写一个 30 天后漂移。
		m.RenewalDays = certRenewThresholdDays
		s.acmeMgr = m
	}
	return s.acmeMgr
}

// acmeHTTP01WebRoot 是 HTTP-01 校验文件的落盘根目录。
//
// internal/acme 会把校验文件写到 <webroot>/.well-known/acme-challenge/<token>，
// 而 CA 会从 http://<域名>/.well-known/acme-challenge/<token> 取它。
// 面板站点 vhost 里的这条规则把该路径映射到**站点根目录**：
//
//	location ^~ /.well-known/acme-challenge/ { root <站点根目录>; }
//
// 所以这里给一个面板自己的共享目录，并在签发前把各站点根下的 .well-known
// 软链到它（见 prepareHTTP01WebRoot）—— 这样续期用的还是同一份路径，
// 不会往用户站点里散落校验文件。
func (s *Server) acmeHTTP01WebRoot() string {
	return filepath.Join(s.Cfg.WWWRoot, "_acme")
}

// ---------- HTTP 接口 ----------

// certView 是列表/详情里的证书视图。
//
// **绝不含私钥内容**：只有路径、元信息与统计。
type certView struct {
	Primary        string   `json:"primary"`
	Domains        []string `json:"domains"`
	Issuer         string   `json:"issuer"`
	NotBefore      string   `json:"not_before"`
	NotAfter       string   `json:"not_after"`
	DaysLeft       int      `json:"days_left"`
	Challenge      string   `json:"challenge"`
	CA             string   `json:"ca"`
	NeedsRenewal   bool     `json:"needs_renewal"`
	CertPath       string   `json:"cert_path"`
	KeyPath        string   `json:"key_path"`
	CertExists     bool     `json:"cert_exists"`
	KeyExists      bool     `json:"key_exists"`
	UpdatedAt      string   `json:"updated_at"`
	ReferencedBy   []string `json:"referenced_sites"` // 哪些站点正在用它（删除前的安全提示）
	RenewThreshold int      `json:"renew_threshold_days"`
}

// handleCertsList 列出全部证书。
func (s *Server) handleCertsList(w http.ResponseWriter, r *http.Request) {
	mgr := s.certManager()
	list, err := mgr.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取证书列表失败: "+err.Error())
		return
	}
	now := time.Now()
	views := make([]certView, 0, len(list))
	for _, c := range list {
		if c == nil {
			continue
		}
		views = append(views, s.certView(c, now))
	}
	// EAB 只回"有没有配"，**绝不回显值**（它等价于凭据）。
	eab, eabErr := s.loadEAB(acme.CAZeroSSL)
	hasEAB := eabErr == nil && eab.Kid != "" && eab.HMAC != ""
	ok(w, map[string]any{
		"list":                 views,
		"renew_threshold_days": certRenewThresholdDays,
		"renew_daily_at":       fmt.Sprintf("每天 %02d:%02d 自动检查并续期", certRenewHour, certRenewMinute),
		"http01_webroot":       s.acmeHTTP01WebRoot(),
		"certs_dir":            filepath.Join(s.Cfg.DataDir, "certs"),
		// ZeroSSL 要求 EAB；值永不出现在这里。
		"has_eab": hasEAB,
		"eab": map[string]any{
			"ca":      acme.CAZeroSSL,
			"has_eab": hasEAB,
			"hint":    "ZeroSSL 强制要求 EAB：请在 ZeroSSL 控制台生成 Key ID 与 HMAC Key 后填入申请表单",
		},
	})
}

// certView 组装证书视图（含"哪些站点在用"）。
func (s *Server) certView(c *acme.Cert, now time.Time) certView {
	v := certView{
		Primary:        c.Primary,
		Domains:        certDomainList(c),
		Issuer:         c.Issuer,
		NotBefore:      fmtCertTime(c.NotBefore),
		NotAfter:       fmtCertTime(c.NotAfter),
		DaysLeft:       certDaysLeft(c, now),
		Challenge:      string(c.Challenge),
		CA:             c.CA,
		NeedsRenewal:   acme.NeedsRenewal(c, now, certRenewThresholdDays),
		CertPath:       c.CertPath,
		KeyPath:        c.KeyPath,
		UpdatedAt:      fmtCertTime(c.UpdatedAt),
		ReferencedBy:   s.sitesUsingCert(c),
		RenewThreshold: certRenewThresholdDays,
	}
	if c.CertPath != "" {
		if _, err := os.Stat(c.CertPath); err == nil {
			v.CertExists = true
		}
	}
	if c.KeyPath != "" {
		if _, err := os.Stat(c.KeyPath); err == nil {
			v.KeyExists = true
		}
	}
	return v
}

type certDNSReq struct {
	Name string            `json:"name"`
	Env  map[string]string `json:"env"`
}

type certIssueReq struct {
	Domains []string `json:"domains"`
	// Domain 是单域名的容错写法（前端可能只填一个）
	Domain    string      `json:"domain"`
	Email     string      `json:"email"`
	Challenge string      `json:"challenge"`
	CA        string      `json:"ca"`
	DNS       *certDNSReq `json:"dns"`

	// EAB（External Account Binding）：ZeroSSL 强制要求，其它 CA 忽略。
	// 与 DNS 凭据同等敏感 —— 只落盘（0600），绝不回显、绝不进日志/审计。
	EABKid  string `json:"eab_kid"`
	EABHmac string `json:"eab_hmac"`
}

// handleCertsIssue 申请证书。
//
// 走任务中心：签发要等 CA 完成 HTTP/DNS 校验，几十秒到几分钟，
// 同步请求会让用户卡在"请等待"，刷新还会把请求连根拔掉（见 SPEC-任务中心.md）。
func (s *Server) handleCertsIssue(w http.ResponseWriter, r *http.Request) {
	var req certIssueReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	domains, err := normalizeCertDomains(append(req.Domains, req.Domain))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	challenge, err := normalizeChallenge(req.Challenge)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if hasWildcard(domains) && challenge != acme.ChallengeDNS01 {
		fail(w, http.StatusBadRequest, "通配符域名（*.example.com）只能用 DNS-01 校验")
		return
	}

	issue := acme.IssueRequest{
		Domains:   domains,
		Email:     strings.TrimSpace(req.Email),
		Challenge: challenge,
		CA:        strings.TrimSpace(req.CA),
	}

	// DNS-01：凭据要么这次带上，要么之前已经存过；两种情况都只留在服务端。
	if challenge == acme.ChallengeDNS01 {
		spec, err := s.resolveDNSChallenge(req.DNS)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		issue.DNS = spec
	} else if req.DNS != nil {
		// HTTP-01 也允许提前存凭据（用户可能在页面上先填好），但申请本身不用它。
		if err := s.validateAndStoreDNS(req.DNS); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// EAB（External Account Binding）：只有 ZeroSSL 需要。
	//
	// 为什么只在 zerossl 时读：前端可能对所有 CA 统一带这两个字段，
	// 别的 CA 传了就忽略（报错只会让用户困惑）；而 ZeroSSL 强制要求 EAB，
	// 缺了它由**引擎**给出明确错误（不许这里悄悄回落到别的 CA）。
	var eab eabCreds
	if isZeroSSL(issue.CA) {
		var err error
		eab, err = s.resolveEAB(req)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	primary := domains[0]
	target := "cert:" + primary

	// HTTP-01 的落盘目录必须在任务开始前准备好（含站点根下的软链）。
	var prepNotes []string
	if challenge == acme.ChallengeHTTP01 {
		prepNotes = s.prepareHTTP01WebRoot(domains)
	}

	issueReq := issue // 闭包捕获副本
	eabKid, eabHMAC := eab.Kid, eab.HMAC
	s.launchTask(w, r, "cert-issue", target,
		"申请证书 "+strings.Join(domains, ", "), "cert_issue",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			for _, n := range prepNotes {
				log(tasks.LevelWarn, n)
			}
			log(tasks.LevelStep, fmt.Sprintf("校验方式 %s，CA %s", issueReq.Challenge,
				orDefault(issueReq.CA, "由 ACME 引擎决定（默认 Let's Encrypt）")))
			if isZeroSSL(issueReq.CA) {
				if eabKid == "" || eabHMAC == "" {
					log(tasks.LevelWarn, "没有可用的 ZeroSSL EAB 凭据：ZeroSSL 要求 External Account Binding，"+
						"请在 ZeroSSL 控制台生成 Key ID 与 HMAC Key 后填入")
				} else {
					// 只说"有凭据"，绝不打印 Key ID / HMAC。
					log(tasks.LevelOut, "已使用服务端保存的 ZeroSSL EAB 凭据")
				}
			}
			cert, err := s.certManagerWithCreds(eabKid, eabHMAC).Issue(ctx, issueReq)
			if err != nil {
				log(tasks.LevelErr, "申请失败: "+err.Error())
				return nil, err
			}
			log(tasks.LevelOK, "证书已签发: "+cert.CertPath+"（到期 "+fmtCertTime(cert.NotAfter)+"）")
			// 重新申请同一个 primary 时，文件路径不变 —— 引用它的站点
			// 需要重建 vhost 并重载 nginx，否则新证书不会被加载。
			if rerr := s.reloadSitesUsingCert(ctx, cert, log); rerr != nil {
				log(tasks.LevelWarn, "证书已签发，但有站点重载失败: "+rerr.Error())
			}
			return certSummary(cert), nil
		})
}

// isZeroSSL 判断这次申请是否走 ZeroSSL（EAB 只对它有意义）。
func isZeroSSL(ca string) bool {
	return strings.EqualFold(strings.TrimSpace(ca), acme.CAZeroSSL)
}

// resolveEAB 组装 ZeroSSL 需要的 EAB 凭据。
//
// 规则：
//   - 本次请求带了 EAB → 落盘（0600）后使用，几天后的自动续期才有得用；
//   - 只带了一半 → 明确报错（半套凭据一定注册失败，不如当场说清）；
//   - 什么都没带 → 用面板里已保存的；一个都没有就返回空凭据，
//     **不在这里报错**，让引擎按它自己的契约给出"ZeroSSL 要求 EAB"的错误。
func (s *Server) resolveEAB(req certIssueReq) (eabCreds, error) {
	kid := strings.TrimSpace(req.EABKid)
	hmac := strings.TrimSpace(req.EABHmac)

	if (kid == "") != (hmac == "") {
		return eabCreds{}, fmt.Errorf("ZeroSSL 的 EAB 需要同时填写 Key ID 与 HMAC Key（当前只填了一个）")
	}
	if kid != "" {
		if err := s.storeEAB(acme.CAZeroSSL, kid, hmac); err != nil {
			return eabCreds{}, fmt.Errorf("保存 EAB 凭据失败: %w", err)
		}
		return eabCreds{Kid: kid, HMAC: hmac}, nil
	}
	return s.loadEAB(acme.CAZeroSSL)
}

// certManagerWithCreds 返回一个带 EAB 配置的引擎实例。
//
// 为什么不把 EAB 设到共享的那个 Manager 上：EAB 是**引擎级**字段，
// 共享实例可能同时被另一个申请/续期使用，直接改字段是数据竞争。
// 引擎是无状态的（配置 + dataDir），按需新建一个实例成本极低。
func (s *Server) certManagerWithCreds(eabKid, eabHMAC string) acmeManager {
	if s.acmeOverride != nil {
		return s.acmeOverride
	}
	if eabKid == "" && eabHMAC == "" {
		return s.certManager()
	}
	m := acme.New(s.Cfg.DataDir, s.acmeHTTP01WebRoot(), func(msg string) {
		s.Log.Info("[acme] %s", msg)
	})
	m.RenewalDays = certRenewThresholdDays
	m.EABKid, m.EABHmacKey = eabKid, eabHMAC
	return m
}

// certManagerForRenew 返回续期某张证书应当使用的引擎。
//
// 为什么续期也要 EAB：EAB 是"注册 ACME 账户"用的，而引擎把它放在
// Manager 配置里（不在 renewal.json 里），所以每次续期都要重新注入。
// ZeroSSL 的证书如果没有保存 EAB，续期必然失败 —— 这里提前给出可读的
// 指引，而不是等引擎报一句难懂的注册错误。
func (s *Server) certManagerForRenew(c *acme.Cert) (acmeManager, error) {
	if c == nil {
		return s.certManager(), nil
	}
	if !strings.EqualFold(c.CA, acme.CAZeroSSL) {
		return s.certManager(), nil
	}
	if s.acmeOverride != nil {
		return s.acmeOverride, nil
	}
	eab, err := s.loadEAB(acme.CAZeroSSL)
	if err != nil {
		return nil, err
	}
	if eab.Kid == "" || eab.HMAC == "" {
		return nil, fmt.Errorf("证书 %s 由 ZeroSSL 签发，续期必须带 EAB 凭据，但面板里没有保存："+
			"请到「证书」页重新申请一次（选择 ZeroSSL 并填入 EAB Key ID / HMAC Key）", c.Primary)
	}
	return s.certManagerWithCreds(eab.Kid, eab.HMAC), nil
}

// handleCertRenew 续期一张证书（任务中心）。
func (s *Server) handleCertRenew(w http.ResponseWriter, r *http.Request) {
	primary := r.PathValue("primary")
	mgr := s.certManager()
	cert, err := mgr.Load(primary)
	if err != nil {
		fail(w, http.StatusNotFound, "证书不存在: "+err.Error())
		return
	}
	primary = cert.Primary // 用引擎里的规范名，避免大小写/别名差异

	// 续期用的引擎必须在启动任务前确定：ZeroSSL 缺 EAB 这类问题
	// 要当场以 400 说清，而不是让用户在任务中心里看到一个更含糊的失败。
	renewMgr, err := s.certManagerForRenew(cert)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	s.launchTask(w, r, "cert-renew", "cert:"+primary, "续期证书 "+primary, "cert_renew",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			log(tasks.LevelStep, "开始续期 "+primary+"（当前到期 "+fmtCertTime(cert.NotAfter)+"）")
			renewed, err := renewMgr.Renew(ctx, primary)
			if err != nil {
				log(tasks.LevelErr, "续期失败: "+err.Error())
				return nil, err
			}
			log(tasks.LevelOK, "续期成功，新到期时间 "+fmtCertTime(renewed.NotAfter))
			if rerr := s.reloadSitesUsingCert(ctx, renewed, log); rerr != nil {
				return certSummary(renewed), rerr
			}
			return certSummary(renewed), nil
		})
}

// handleCertDelete 删除一张证书。
//
// 同步执行（就是删目录）。**被站点引用时拒绝删除**：证书文件被删掉后，
// nginx 下一次 reload 会直接失败（ssl_certificate 找不到），
// 那会让站点在用户毫无察觉的情况下打不开 —— 宁可在这里明确拦住。
func (s *Server) handleCertDelete(w http.ResponseWriter, r *http.Request) {
	primary := r.PathValue("primary")
	mgr := s.certManager()
	cert, err := mgr.Load(primary)
	if err != nil {
		fail(w, http.StatusNotFound, "证书不存在: "+err.Error())
		return
	}
	if refs := s.sitesUsingCert(cert); len(refs) > 0 {
		fail(w, http.StatusConflict,
			"证书 "+cert.Primary+" 正被站点使用："+strings.Join(refs, "、")+
				"；请先在这些站点里改用其它证书或关闭 SSL，再删除")
		return
	}
	if err := mgr.Delete(cert.Primary); err != nil {
		s.audit(r, "cert_delete", cert.Primary, "删除失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "删除证书失败: "+err.Error())
		return
	}
	s.audit(r, "cert_delete", cert.Primary, "删除证书（不影响站点：无站点引用）", true, "")
	ok(w, map[string]any{"msg": "证书已删除", "primary": cert.Primary})
}

// handleCertDNSProviders 返回可选的 DNS 服务商清单。
//
// 只回**名称 + 需要的环境变量键名 + 是否已配置**，绝不回显用户填过的值。
// 形状按前端约定的最自然形式：`{list:[{name,label,fields:[{key,label,required}]}]}`
// （同时给一份 `providers` 别名，兼容早期取值）。
func (s *Server) handleCertDNSProviders(w http.ResponseWriter, r *http.Request) {
	stored, err := s.loadACMECreds()
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取 DNS 凭据失败: "+err.Error())
		return
	}
	items := make([]map[string]any, 0, len(dnsProviderCatalog))
	for _, p := range dnsProviderCatalog {
		missing := make([]string, 0)
		configured := false
		for _, f := range p.Env {
			has := strings.TrimSpace(stored[p.Name][f.Key]) != ""
			if has {
				configured = true
			} else if f.Required {
				missing = append(missing, f.Key)
			}
		}
		items = append(items, map[string]any{
			"name":       p.Name,
			"label":      p.Label,
			"note":       p.Note,
			"fields":     p.Env,
			"env":        p.Env, // 别名：前端也接受 env 这个键名
			"configured": configured,
			"missing":    missing,
		})
	}
	ok(w, map[string]any{
		"list":      items,
		"providers": items,
		// 告诉前端凭据存在哪里、权限是什么，方便用户自己核对；
		// 路径不含任何密钥内容。
		"credential_file": s.acmeCredsPath(),
		"credential_mode": "0600（仅面板进程可读；接口永不回显）",
	})
}

// ---------- DNS 凭据（只存服务端） ----------

// dnsProviderField 是某个 DNS 服务商需要的一个环境变量。
type dnsProviderField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	Secret   bool   `json:"secret"`
}

// dnsProviderDef 是一个 DNS 服务商定义。
type dnsProviderDef struct {
	Name  string
	Label string
	Env   []dnsProviderField
	Note  string
}

// dnsProviderCatalog 是界面可选的 DNS 服务商清单。
//
// 为什么这份清单在面板这边、而不是问 acme 包要：
// 冻结的接口里没有"列出支持的服务商"这个方法，而接口必须给出清单。
// **变量名逐个核对过 lego v4.35.2 的 providers/dns 源码**（acme 引擎就是用
// lego 的 provider 注册表，且只按环境变量取凭据，见 internal/acme/challenge.go）。
// 如果将来升级 lego，这里要一起核对；写错键名的表现是引擎报"缺少凭据"，
// 会如实出现在任务日志里，不会被吞掉。
//
// Required 只作为界面提示（前端把它画成必填），校验时只要求"至少填了一项" ——
// 因为像 Cloudflare 既有 API Token 也有 Email+Global Key 两套方案，
// 用一刀切的"全部必填"会把合法的另一套方案挡在门外。
var dnsProviderCatalog = []dnsProviderDef{
	{
		Name: "cloudflare", Label: "Cloudflare",
		Env: []dnsProviderField{
			{Key: "CLOUDFLARE_DNS_API_TOKEN", Label: "API Token（推荐，权限 Zone:DNS:Edit）", Required: true, Secret: true},
			{Key: "CLOUDFLARE_EMAIL", Label: "账号邮箱（用 Global API Key 时才需要）", Secret: false},
			{Key: "CLOUDFLARE_API_KEY", Label: "Global API Key（可选，与邮箱配对）", Secret: true},
		},
		Note: "推荐 API Token：只授权 DNS 编辑，泄露影响面最小；也可用 邮箱 + Global API Key。",
	},
	{
		Name: "alidns", Label: "阿里云 DNS",
		Env: []dnsProviderField{
			{Key: "ALICLOUD_ACCESS_KEY", Label: "AccessKey ID", Required: true, Secret: true},
			{Key: "ALICLOUD_SECRET_KEY", Label: "AccessKey Secret", Required: true, Secret: true},
			{Key: "ALICLOUD_REGION_ID", Label: "区域（可选，默认 cn-hangzhou）", Secret: false},
		},
	},
	{
		Name: "tencentcloud", Label: "腾讯云 DNSPod（新版 API）",
		Env: []dnsProviderField{
			{Key: "TENCENTCLOUD_SECRET_ID", Label: "SecretId", Required: true, Secret: true},
			{Key: "TENCENTCLOUD_SECRET_KEY", Label: "SecretKey", Required: true, Secret: true},
			{Key: "TENCENTCLOUD_REGION", Label: "区域（可选，默认 ap-guangzhou）", Secret: false},
		},
	},
	{
		Name: "dnspod", Label: "DNSPod（旧版 API）",
		Env: []dnsProviderField{
			{Key: "DNSPOD_API_KEY", Label: "API Key", Required: true, Secret: true},
		},
	},
	{
		Name: "route53", Label: "AWS Route 53",
		Env: []dnsProviderField{
			{Key: "AWS_ACCESS_KEY_ID", Label: "Access Key ID", Required: true, Secret: true},
			{Key: "AWS_SECRET_ACCESS_KEY", Label: "Secret Access Key", Required: true, Secret: true},
			{Key: "AWS_REGION", Label: "区域（如 us-east-1）", Required: true, Secret: false},
			{Key: "AWS_HOSTED_ZONE_ID", Label: "Hosted Zone ID（可选，给了可跳过自动查找）", Secret: false},
		},
	},
	{
		Name: "godaddy", Label: "GoDaddy",
		Env: []dnsProviderField{
			{Key: "GODADDY_API_KEY", Label: "API Key", Required: true, Secret: true},
			{Key: "GODADDY_API_SECRET", Label: "API Secret", Required: true, Secret: true},
		},
	},
	{
		Name: "namesilo", Label: "NameSilo",
		Env: []dnsProviderField{
			{Key: "NAMESILO_API_KEY", Label: "API Key", Required: true, Secret: true},
		},
	},
	{
		Name: "huaweicloud", Label: "华为云 DNS",
		Env: []dnsProviderField{
			{Key: "HUAWEICLOUD_ACCESS_KEY_ID", Label: "Access Key ID", Required: true, Secret: true},
			{Key: "HUAWEICLOUD_SECRET_ACCESS_KEY", Label: "Secret Access Key", Required: true, Secret: true},
			{Key: "HUAWEICLOUD_REGION", Label: "区域（如 cn-north-1）", Required: true, Secret: false},
		},
	},
}

// acmeCredsPath 返回证书凭据（DNS token + EAB）的落盘位置。
//
// 放在 DataDir 下的 0600 文件里（与面板其它密钥同级），而不是数据库：
// 数据库会进备份、进导出，凭据不该跟着到处跑。
func (s *Server) acmeCredsPath() string {
	return filepath.Join(s.Cfg.DataDir, "acme", "credentials.json")
}

// eabCreds 是某个 CA 的 External Account Binding 凭据。
//
// 为什么单独一份：EAB 不是"域名验证"用的，而是**向 CA 注册账户**用的
// （ZeroSSL 强制要求）。它与 DNS token 同等敏感 —— 拿到就能以你的名义
// 向 CA 注册账户，所以和 DNS 凭据放同一个文件、同样 0600、同样绝不回显。
type eabCreds struct {
	Kid  string `json:"kid"`
	HMAC string `json:"hmac"`
}

// acmeCredsFile 是凭据文件的 JSON 形状。
//
// 值只在这里落盘，永不出现在任何接口响应、日志、审计或任务步骤里。
type acmeCredsFile struct {
	Providers map[string]map[string]string `json:"providers"`
	// EAB 按 CA 名称索引（当前只有 zerossl）。
	EAB map[string]eabCreds `json:"eab,omitempty"`
}

// empty 保证反序列化后的空字段可用。
func (f *acmeCredsFile) normalize() {
	if f.Providers == nil {
		f.Providers = map[string]map[string]string{}
	}
	if f.EAB == nil {
		f.EAB = map[string]eabCreds{}
	}
}

// loadACMECreds 读取全部已保存的 DNS 凭据（文件不存在时返回空表）。
func (s *Server) loadACMECreds() (map[string]map[string]string, error) {
	s.acmeCredMu.Lock()
	defer s.acmeCredMu.Unlock()
	f, err := s.readACMECredsLocked()
	if err != nil {
		return nil, err
	}
	return f.Providers, nil
}

// loadEAB 读取某个 CA 已保存的 EAB 凭据。
func (s *Server) loadEAB(ca string) (eabCreds, error) {
	s.acmeCredMu.Lock()
	defer s.acmeCredMu.Unlock()
	f, err := s.readACMECredsLocked()
	if err != nil {
		return eabCreds{}, err
	}
	return f.EAB[strings.ToLower(strings.TrimSpace(ca))], nil
}

// readACMECredsLocked 读凭据文件；调用方必须已持有 acmeCredMu。
func (s *Server) readACMECredsLocked() (*acmeCredsFile, error) {
	path := s.acmeCredsPath()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			f := &acmeCredsFile{}
			f.normalize()
			return f, nil
		}
		return nil, err
	}
	// 权限必须锁死：DNS token / EAB 泄露 = 别人可以改你的解析记录、
	// 或以你的名义向 CA 注册账户。历史遗留的宽松权限先收紧再读，并告警。
	if fi.Mode().Perm() != certCredFileMode {
		if cerr := os.Chmod(path, certCredFileMode); cerr != nil {
			s.Log.Warn("证书凭据文件权限不是 0600 且收紧失败: %s: %v", path, cerr)
		} else {
			s.Log.Warn("证书凭据文件权限曾被放宽，已收紧为 0600: %s", path)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f acmeCredsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("凭据文件格式损坏（%s）: %w", path, err)
	}
	f.normalize()
	return &f, nil
}

// updateACMECreds 是凭据文件的唯一写入口：加锁 → 读 → 改 → 原子落盘（0600）。
//
// 所有凭据（DNS / EAB）都走这一条路，避免出现第二份存储与第二套权限处理。
func (s *Server) updateACMECreds(mutate func(*acmeCredsFile) error) error {
	s.acmeCredMu.Lock()
	defer s.acmeCredMu.Unlock()

	f, err := s.readACMECredsLocked()
	if err != nil {
		return err
	}
	if err := mutate(f); err != nil {
		return err
	}
	return s.writeACMECredsLocked(f)
}

// writeACMECredsLocked 原子替换凭据文件（调用方必须已持有 acmeCredMu）。
func (s *Server) writeACMECredsLocked(f *acmeCredsFile) error {
	dir := filepath.Dir(s.acmeCredsPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建凭据目录失败: %w", err)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".creds-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 先 chmod 再写：CreateTemp 默认 0600，但显式收紧一次更稳。
	if err := tmp.Chmod(certCredFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.acmeCredsPath()); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// storeDNSEnv 保存某个服务商的凭据（合并写入，0600，原子替换）。
func (s *Server) storeDNSEnv(name string, env map[string]string) error {
	return s.updateACMECreds(func(f *acmeCredsFile) error {
		if f.Providers[name] == nil {
			f.Providers[name] = map[string]string{}
		}
		for k, v := range env {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			v = strings.TrimSpace(v)
			// 空值表示"不改动已保存的值"，而不是"清空" —— 用户改一项时
			// 前端不会把其它项回显（我们也不回显），必须避免误删。
			if v == "" {
				continue
			}
			f.Providers[name][k] = v
		}
		return nil
	})
}

// storeEAB 保存某个 CA 的 EAB 凭据（0600，与 DNS 凭据同文件）。
func (s *Server) storeEAB(ca, kid, hmac string) error {
	ca = strings.ToLower(strings.TrimSpace(ca))
	return s.updateACMECreds(func(f *acmeCredsFile) error {
		cur := f.EAB[ca]
		if strings.TrimSpace(kid) != "" {
			cur.Kid = strings.TrimSpace(kid)
		}
		if strings.TrimSpace(hmac) != "" {
			cur.HMAC = strings.TrimSpace(hmac)
		}
		f.EAB[ca] = cur
		return nil
	})
}

// resolveDNSChallenge 组装 DNS-01 所需的凭据：
// 优先用这次请求带来的值，缺的用已保存的值补齐。
func (s *Server) resolveDNSChallenge(req *certDNSReq) (*acme.DNSProviderSpec, error) {
	if req == nil || strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("DNS-01 需要选择 DNS 服务商并填写凭据")
	}
	name := strings.TrimSpace(req.Name)

	// 本次请求里带来的值先落盘（0600），供几天后的自动续期使用。
	if err := s.validateAndStoreDNS(req); err != nil {
		return nil, err
	}
	stored, err := s.loadACMECreds()
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range stored[name] {
		env[k] = v
	}
	if len(env) == 0 {
		return nil, fmt.Errorf("DNS 服务商 %s 还没有可用的凭据", name)
	}
	return &acme.DNSProviderSpec{Name: name, Env: env}, nil
}

// validateAndStoreDNS 校验并保存用户填写的 DNS 凭据。
//
// 校验规则：
//   - 服务商必须在目录里（否则引擎多半也不支持，不如当场说清）；
//   - 目录里标了 required 的键必须齐全（本次填的 + 已保存的）。
func (s *Server) validateAndStoreDNS(req *certDNSReq) error {
	if req == nil {
		return nil
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return fmt.Errorf("缺少 DNS 服务商名称")
	}
	def, okDef := findDNSProvider(name)
	if !okDef {
		names := make([]string, 0, len(dnsProviderCatalog))
		for _, p := range dnsProviderCatalog {
			names = append(names, p.Name)
		}
		return fmt.Errorf("不支持 DNS 服务商 %q；可选：%s", name, strings.Join(names, "、"))
	}
	// 本次请求里的键也必须是该服务商认识的键（防止把无关变量写进凭据文件）。
	for k := range req.Env {
		found := false
		for _, f := range def.Env {
			if f.Key == k {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("DNS 服务商 %s 不认识变量 %q", name, k)
		}
	}
	if err := s.storeDNSEnv(name, req.Env); err != nil {
		return fmt.Errorf("保存 DNS 凭据失败: %w", err)
	}
	stored, err := s.loadACMECreds()
	if err != nil {
		return err
	}
	// 只要求"至少填了一项"：同一家服务商常有多种凭据方案
	// （如 Cloudflare 的 API Token 与 邮箱+Global Key），
	// 一刀切要求全部必填会把合法的另一套方案挡在门外，
	// 而真正缺项时 lego 会在任务日志里给出明确错误。
	any := false
	for _, f := range def.Env {
		if strings.TrimSpace(stored[name][f.Key]) != "" {
			any = true
			break
		}
	}
	if !any {
		if len(def.Env) == 1 {
			return fmt.Errorf("DNS 服务商 %s 需要填写 %s", name, def.Env[0].Key)
		}
		keys := make([]string, 0, len(def.Env))
		for _, f := range def.Env {
			if f.Required {
				keys = append(keys, f.Key)
			}
		}
		return fmt.Errorf("DNS 服务商 %s 至少需要填写一项凭据（通常需要：%s）",
			name, strings.Join(keys, "、"))
	}
	return nil
}

func findDNSProvider(name string) (dnsProviderDef, bool) {
	for _, p := range dnsProviderCatalog {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return dnsProviderDef{}, false
}

// ---------- HTTP-01 目录准备 ----------

// prepareHTTP01WebRoot 准备 HTTP-01 校验目录，并在相关站点根下建立软链。
//
// 为什么要软链：站点 vhost 把 `/.well-known/acme-challenge/` 映射到**站点根目录**
// （见 internal/sites 生成的规则），而引擎只接受一个共享 webroot。
// 把 <站点根>/.well-known 指到共享目录，两边就对上了。
//
// 已知限制（必须如实说明，不假装都能用）：
//   - 未开启 SSL 的站点，vhost 里的 `location ~ /\. { deny all; }` 会**拦截**
//     `/.well-known/...`（该站点还没有 acme-challenge 的 ^~ 放行规则）。
//     所以对"还没有证书的站点"请优先用 DNS-01；HTTP-01 在已开启 SSL 的站点
//     （续期）以及没有站点接管的域名（落在默认站点）上才可用。
//
// 返回的是给用户看的提示（不是错误）：失败也不该拦住签发，
// 因为 DNS-01 路径完全不依赖这里。
func (s *Server) prepareHTTP01WebRoot(domains []string) []string {
	var notes []string
	root := s.acmeHTTP01WebRoot()
	challengeDir := filepath.Join(root, ".well-known", "acme-challenge")
	if err := os.MkdirAll(challengeDir, 0o755); err != nil {
		return []string{"创建 HTTP-01 校验目录失败: " + err.Error()}
	}
	// 根目录归属真实用户，方便用户自己查看/清理（与站点日志目录同一考虑）。
	if s.Cfg.User != "" && os.Geteuid() == 0 {
		_ = chownTreeTo(root, s.Cfg.User)
	}

	targets := map[string]bool{}
	if list, err := s.siteMgr().List(context.Background()); err == nil {
		for _, st := range list {
			if siteMatchesAnyDomain(st, domains) && st.Root != "" {
				targets[st.Root] = true
			}
		}
	}
	// 没有被任何站点接管的域名会落到默认站点，那里也要能取到校验文件。
	if def := filepath.Join(s.Cfg.WWWRoot, "localhost"); dirExists(def) {
		targets[def] = true
	}
	if len(targets) == 0 {
		notes = append(notes, "没有找到匹配的站点目录，HTTP-01 校验文件只写到了 "+challengeDir+
			"（该域名必须有 nginx 站点或默认站点承载）")
	}
	for dir := range targets {
		if note := ensureWellKnownLink(dir, filepath.Join(root, ".well-known")); note != "" {
			notes = append(notes, note)
		}
	}
	return notes
}

// ensureWellKnownLink 让 <siteRoot>/.well-known 指向共享校验目录。
//
// 只动 `.well-known` 这一个隐藏入口：
//   - 已经是正确软链 → 什么都不做（幂等）；
//   - 是一个真实目录 → **不动它**（用户可能自己在放 challenge），只给提示；
//   - 指向别处 → 重新指向共享目录（并给提示）。
func ensureWellKnownLink(siteRoot, target string) string {
	link := filepath.Join(siteRoot, ".well-known")
	fi, err := os.Lstat(link)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		if dest, derr := os.Readlink(link); derr == nil && filepath.Clean(dest) == filepath.Clean(target) {
			return ""
		}
		if rerr := os.Remove(link); rerr != nil {
			return fmt.Sprintf("%s：软链更新失败 %v", link, rerr)
		}
		if serr := os.Symlink(target, link); serr != nil {
			return fmt.Sprintf("%s：软链创建失败 %v", link, serr)
		}
		return fmt.Sprintf("%s：原软链指向别处，已改为共享校验目录", link)
	case err == nil:
		return fmt.Sprintf("%s 是一个真实目录（未改动）；HTTP-01 校验文件写在 %s，如需它生效请自行建立软链",
			link, target)
	default:
		if serr := os.Symlink(target, link); serr != nil {
			return fmt.Sprintf("%s：软链创建失败 %v", link, serr)
		}
		return ""
	}
}

// ---------- 站点侧：匹配证书 ----------

// matchCertForSite 为一个站点找到要用的 ACME 证书。
//
// 匹配顺序（先精确、后宽松）：
//  1. 请求显式指定的 cert_primary / domain；
//  2. 站点的域名（或别名）等于某证书的 primary；
//  3. 站点的域名（或别名）出现在某证书的 SAN 列表里（含 *.example.com 通配）。
//
// 匹配不到就**明确报错并指路**。这里不做"就地签发"：签发是要等 CA 校验的
// 长任务，必须走任务中心（POST /api/v1/certs）；站点 SSL 这个接口是同步的，
// 让它挂几分钟既违反"长任务必须走任务中心"的约定，用户也看不到进度。
func (s *Server) matchCertForSite(site *sites.Site, req siteSSLReq) (*acme.Cert, error) {
	list, err := s.certManager().List()
	if err != nil {
		return nil, fmt.Errorf("读取证书列表失败: %w", err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("还没有任何 ACME 证书：请先到「证书」页申请（DNS-01 或 HTTP-01）后再回来绑定")
	}

	want := strings.ToLower(strings.TrimSpace(req.CertPrimary))
	if want == "" {
		want = strings.ToLower(strings.TrimSpace(req.Domain))
	}
	if want != "" {
		for _, c := range list {
			if c != nil && strings.EqualFold(c.Primary, want) {
				return c, nil
			}
		}
	}

	names := []string{strings.ToLower(site.Domain)}
	for _, a := range site.AliasList() {
		names = append(names, strings.ToLower(a))
	}
	if want != "" {
		names = append(names, want)
	}

	// 优先 SAN 里精确命中的
	for _, c := range list {
		if c == nil {
			continue
		}
		for _, d := range certDomainList(c) {
			for _, n := range names {
				if strings.EqualFold(d, n) {
					return c, nil
				}
			}
		}
	}
	// 再退一步：通配符 SAN
	for _, c := range list {
		if c == nil {
			continue
		}
		for _, d := range certDomainList(c) {
			for _, n := range names {
				if wildcardMatches(d, n) {
					return c, nil
				}
			}
		}
	}
	if want != "" && want != strings.ToLower(site.Domain) {
		return nil, fmt.Errorf("找不到证书 %q：请到「证书」页确认 primary（或先申请）", req.CertPrimary)
	}
	return nil, fmt.Errorf("没有覆盖域名 %s 的 ACME 证书：请先到「证书」页申请，"+
		"申请成功后回到这里选择来源「ACME 自动证书」即可绑定", site.Domain)
}

// sitesUsingCert 返回正在引用该证书的站点域名列表。
//
// 判定用**证书路径**而不是域名：一个证书可以有多个域名，一个站点也可能用别名
// 匹配到同一张证书；而 acme 续期是**同路径覆盖**，只有路径能精确对应"哪些
// vhost 里写着 ssl_certificate <这份文件>"。
func (s *Server) sitesUsingCert(c *acme.Cert) []string {
	if c == nil || c.CertPath == "" {
		return []string{}
	}
	list, err := s.siteMgr().List(context.Background())
	if err != nil {
		return []string{}
	}
	var out []string
	for _, st := range list {
		if st == nil || !st.SSLEnabled || st.SSLCert == "" {
			continue
		}
		if filepath.Clean(st.SSLCert) == filepath.Clean(c.CertPath) {
			out = append(out, st.Domain)
		}
	}
	return out
}

// reloadSitesUsingCert 让引用了这张证书的站点重新生成 vhost 并重载 nginx。
//
// 为什么必须做：续期是**同路径覆盖**证书文件，而 nginx 只在 reload 时重新读盘；
// 不 reload 的话证书文件换了、线上还是旧证书（浏览器继续提示即将过期）。
func (s *Server) reloadSitesUsingCert(ctx context.Context, c *acme.Cert, log tasks.LogFunc) error {
	refs := s.sitesUsingCert(c)
	if len(refs) == 0 {
		if log != nil {
			log(tasks.LevelOut, "没有站点引用这张证书，无需重载 nginx")
		}
		return nil
	}
	if log != nil {
		log(tasks.LevelStep, "重载引用该证书的站点: "+strings.Join(refs, "、"))
	}
	mgr := s.siteMgr()
	list, err := mgr.List(ctx)
	if err != nil {
		return fmt.Errorf("读取站点列表失败: %w", err)
	}
	var failed []string
	for _, st := range list {
		if st == nil || !st.SSLEnabled || st.SSLCert == "" {
			continue
		}
		if filepath.Clean(st.SSLCert) != filepath.Clean(c.CertPath) {
			continue
		}
		// applySite 内部会写 vhost → reload → 按真实结果复核（403 探针），
		// 所以"重载成功"这句话有硬证据，不是只看命令退出码。
		if err := s.applySite(ctx, st); err != nil {
			failed = append(failed, st.Domain+": "+err.Error())
			if log != nil {
				log(tasks.LevelErr, "站点 "+st.Domain+" 重载失败: "+err.Error())
			}
			continue
		}
		if log != nil {
			log(tasks.LevelOK, "站点 "+st.Domain+" 已重载新证书")
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d 个站点重载失败: %s", len(failed), strings.Join(failed, "；"))
	}
	return nil
}

// ---------- 自动续期 ----------

// startCertRenewal 把"每日检查并续期"注册进面板既有的调度器。
//
// 位置：web.Startup（面板启动时调用一次）。循环本身在 internal/scheduler
// 的每日任务机制里，不在本文件自造定时器。
func (s *Server) startCertRenewal(ctx context.Context) {
	runner := scheduler.NewDailyRunner(certRenewHour, certRenewMinute, []scheduler.DailyTask{
		{Name: "acme-renew", Run: s.renewDueCerts},
	}, s.Log.Info)
	runner.Start(ctx)
	s.Log.Info("证书自动续期已登记：%s（阈值 %d 天）",
		runner.NextRun(time.Now()).Format("2006-01-02 15:04"), certRenewThresholdDays)
}

// renewDueCerts 检查所有证书，续期其中即将到期（<30 天）的。
//
// 失败处理：每一项续期都跑在任务中心里（进度可见），并额外写一条审计；
// 有任何一项失败就返回 error，让每日循环也留一条日志 —— 绝不静默。
func (s *Server) renewDueCerts(ctx context.Context) error {
	mgr := s.certManager()
	list, err := mgr.List()
	if err != nil {
		return fmt.Errorf("读取证书列表失败: %w", err)
	}
	now := time.Now()
	var due []*acme.Cert
	for _, c := range list {
		if c != nil && acme.NeedsRenewal(c, now, certRenewThresholdDays) {
			due = append(due, c)
		}
	}
	if len(due) == 0 {
		s.Log.Info("自动续期：%d 张证书均不需要续期（阈值 %d 天）", len(list), certRenewThresholdDays)
		return nil
	}

	var failed []string
	for _, c := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 用户可能刚好在界面上点了同一张证书的续期；同一 target 已在跑就跳过，
		// 避免两个进程同时改同一份证书文件。
		if t := s.Tasks.RunningFor("cert:" + c.Primary); t != nil {
			s.Log.Warn("自动续期跳过 %s：已有任务在跑（%s）", c.Primary, t.Meta().Title)
			continue
		}
		s.Log.Info("自动续期：%s 还有 %d 天到期，开始续期", c.Primary, certDaysLeft(c, now))
		if err := s.renewCertViaTaskCenter(ctx, c); err != nil {
			failed = append(failed, c.Primary+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d 张证书自动续期失败：%s", len(failed), strings.Join(failed, "；"))
	}
	return nil
}

// renewCertViaTaskCenter 在任务中心里跑一次自动续期，并等待它结束。
//
// 为什么同步等待：自动续期是后台循环，没有 HTTP 请求可返回，它需要逐个拿到
// 结果才能汇总失败。任务体本身仍跑在任务中心的 goroutine 里，进度在
// 「任务中心」可见；这里只是等它结束（带缓冲通道，没人等也不会泄漏）。
func (s *Server) renewCertViaTaskCenter(ctx context.Context, c *acme.Cert) error {
	type outcome struct {
		cert *acme.Cert
		err  error
	}
	ch := make(chan outcome, 1)
	// ZeroSSL 证书续期同样要注入 EAB（见 certManagerForRenew）。
	mgr, err := s.certManagerForRenew(c)
	if err != nil {
		s.auditAs(auditInfo{actor: "system:scheduler"}, "cert_renew", c.Primary,
			"自动续期无法开始", false, err.Error())
		return err
	}

	_ = s.Tasks.Start("cert-renew", "cert:"+c.Primary, "自动续期证书 "+c.Primary,
		func(tctx context.Context, log tasks.LogFunc) (any, error) {
			// 兜底：任务体无论怎么结束（含 panic 被任务中心 recover）都要给等待方
			// 一个结果，否则下面 select 会一直等到 15 分钟超时。
			sent := false
			defer func() {
				if !sent {
					ch <- outcome{nil, fmt.Errorf("续期任务异常结束（未返回结果）")}
				}
			}()
			log(tasks.LevelStep, fmt.Sprintf("自动续期 %s（当前到期 %s，剩 %d 天）",
				c.Primary, fmtCertTime(c.NotAfter), certDaysLeft(c, time.Now())))
			renewed, err := mgr.Renew(tctx, c.Primary)
			if err != nil {
				log(tasks.LevelErr, "自动续期失败: "+err.Error())
				sent = true
				ch <- outcome{nil, err}
				return nil, err
			}
			log(tasks.LevelOK, "续期成功，新到期时间 "+fmtCertTime(renewed.NotAfter))
			if rerr := s.reloadSitesUsingCert(tctx, renewed, log); rerr != nil {
				sent = true
				ch <- outcome{renewed, rerr}
				return certSummary(renewed), rerr
			}
			sent = true
			ch <- outcome{renewed, nil}
			return certSummary(renewed), nil
		})

	select {
	case o := <-ch:
		// 自动续期没有 HTTP 请求，审计要单独写：这是"失败可见"的兜底。
		s.auditAs(auditInfo{actor: "system:scheduler"}, "cert_renew", c.Primary,
			"自动续期（阈值 30 天）", o.err == nil, errString(o.err))
		return o.err
	case <-ctx.Done():
		s.auditAs(auditInfo{actor: "system:scheduler"}, "cert_renew", c.Primary,
			"自动续期被中断", false, ctx.Err().Error())
		return ctx.Err()
	}
}

// ---------- 小工具 ----------

// normalizeCertDomains 清洗域名列表：去空、转小写、去重。
func normalizeCertDomains(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if err := validateCertDomain(d); err != nil {
			return nil, err
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("请至少填写一个域名")
	}
	return out, nil
}

// validateCertDomain 校验证书域名，允许一个前导的 `*.`（通配证书）。
//
// sites.ValidateDomain 不接受通配符，所以这里剥掉前缀后再复用它 —— 不另写
// 一套正则，避免两边对"合法域名"的判断出现分歧。
func validateCertDomain(d string) error {
	base := strings.TrimPrefix(d, "*.")
	if base != d && strings.Contains(base, "*") {
		return fmt.Errorf("域名 %q 不合法：通配符只能出现在最前面的标签（*.example.com）", d)
	}
	if strings.Contains(d, "*") && !strings.HasPrefix(d, "*.") {
		return fmt.Errorf("域名 %q 不合法：通配符只能出现在最前面的标签（*.example.com）", d)
	}
	return sites.ValidateDomain(base)
}

func normalizeChallenge(s string) (acme.ChallengeType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(acme.ChallengeHTTP01), "http":
		return acme.ChallengeHTTP01, nil
	case string(acme.ChallengeDNS01), "dns":
		return acme.ChallengeDNS01, nil
	default:
		return "", fmt.Errorf("不支持的校验方式 %q（可选 http-01 / dns-01）", s)
	}
}

func hasWildcard(domains []string) bool {
	for _, d := range domains {
		if strings.HasPrefix(d, "*.") {
			return true
		}
	}
	return false
}

// certDomainList 返回证书覆盖的域名列表。
//
// 引擎的 Cert.Domains 已经是 []string；空值时退回 primary，
// 保证界面永远有一个可显示的名字。
func certDomainList(c *acme.Cert) []string {
	if c == nil {
		return []string{}
	}
	out := make([]string, 0, len(c.Domains))
	for _, d := range c.Domains {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	if len(out) == 0 && c.Primary != "" {
		return []string{c.Primary}
	}
	return out
}

// wildcardMatches 判断证书域名 d 是否覆盖站点域名 host（只支持 *.example.com 这一种）。
func wildcardMatches(d, host string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	host = strings.ToLower(strings.TrimSpace(host))
	suffix, ok := strings.CutPrefix(d, "*.")
	if !ok || suffix == "" {
		return false
	}
	// 通配只覆盖一级：a.example.com 匹配，a.b.example.com 不匹配。
	if !strings.HasSuffix(host, "."+suffix) {
		return false
	}
	return !strings.Contains(strings.TrimSuffix(host, "."+suffix), ".")
}

// siteMatchesAnyDomain 判断站点（含别名）是否覆盖给定域名之一。
func siteMatchesAnyDomain(st *sites.Site, domains []string) bool {
	if st == nil {
		return false
	}
	names := []string{strings.ToLower(st.Domain)}
	for _, a := range st.AliasList() {
		names = append(names, strings.ToLower(a))
	}
	for _, d := range domains {
		for _, n := range names {
			if strings.EqualFold(d, n) || wildcardMatches(d, n) {
				return true
			}
		}
	}
	return false
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// sslProviderLabel 把证书来源翻译成界面可读的中文。
func sslProviderLabel(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "self":
		return "自签证书"
	case "mkcert":
		return "mkcert 本地 CA"
	case "manual":
		return "手工上传"
	case "acme":
		return "ACME 自动证书（Let's Encrypt 等）"
	case "":
		return "未配置"
	default:
		return provider
	}
}

// siteSSLDaysLeft 从证书文件读剩余天数；读不到返回 -1（表示"无法判断"，不猜）。
//
// 用 tlsx.CertExpiry（纯 Go 解析）而不是 priv.CertInfo（起 openssl 子进程）：
// 详情页每次打开都要算剩余天数，不值得为它 fork 一个进程。
func siteSSLDaysLeft(certPath string) int {
	if certPath == "" {
		return -1
	}
	notAfter, err := tlsx.CertExpiry(certPath)
	if err != nil || notAfter.IsZero() {
		return -1
	}
	return int(time.Until(notAfter).Hours() / 24)
}

func fmtCertTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// certDaysLeft 返回距到期还有几天（负数=已过期，0=今天到期）。
func certDaysLeft(c *acme.Cert, now time.Time) int {
	if c == nil || c.NotAfter.IsZero() {
		return 0
	}
	return int(c.NotAfter.Sub(now).Hours() / 24)
}

// certSummary 是返回给接口/任务结果的证书摘要。
//
// 刻意手工挑字段：**不直接序列化 acme.Cert**，避免将来引擎在结构体里
// 加了任何私钥相关字段时被自动带出去。
func certSummary(c *acme.Cert) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	return map[string]any{
		"primary":    c.Primary,
		"domains":    certDomainList(c),
		"issuer":     c.Issuer,
		"cert_path":  c.CertPath,
		"key_path":   c.KeyPath,
		"not_before": fmtCertTime(c.NotBefore),
		"not_after":  fmtCertTime(c.NotAfter),
		"challenge":  string(c.Challenge),
		"ca":         c.CA,
	}
}
