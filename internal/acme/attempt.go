package acme

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 本文件是「失败申请条目」的落盘层。
//
// 背景（用户痛点）：签发只有成功时才在 certs/<primary>/ 留下条目，失败时什么都
// 不留 —— 用户下次重试必须重新打开对话框、重新填域名 / 校验方式 / DNS 服务商，
// 而 DNS 凭据其实已经存在服务端。这里把「失败的那次申请」原样留存，
// 列表里显示为 status=failed 并可一键重试。
//
// 与 renewal.json 的关键区别：
//   - renewal.json 是**成功**的产物，续期必须拿到 DNS 凭据，所以它 0600 且内容里
//     可能有 token；
//   - attempt.json 是**失败**的记录，**绝不写任何凭据值**（只写服务商名字），
//     重试时由接入层从服务端的 credentials.json 取真实凭据。
//
// 即便不含凭据，仍然用 0600：错误文本经脱敏后通常无害，但"少暴露一点"没有代价。
//
// 为什么不放进 certs/<primary>/：那个目录以 meta.json 为「已签发」的提交标记，
// List() 会跳过没有 meta 的目录；把失败记录混进去会让每次失败都在证书目录里
// 留下一个"坏目录"。失败记录放在 acme/attempts/ 下，与证书存储彻底分开。

const (
	attemptsDirName = "attempts"
	attemptFileExt  = ".json"
	attemptFileMode = 0o600

	// maxAttemptErrorLen 限制条目里错误文本的长度：CA 可能返回整页 HTML，
	// 原样落盘既没意义又会让列表接口回一个巨大的字段。
	maxAttemptErrorLen = 2000
)

// Attempt 是一次签发尝试（未成功）的持久化记录，序列化进
// <dataDir>/acme/attempts/<primary>.json。
//
// 它是**独立类型**，不塞进 Cert：Cert 是契约固定的结构（见 acme.go），
// 不能为了内部需要加字段。列表接口用「证书视图 + status/last_error」的包装，
// 而不是改 Cert 的语义。
//
// 安全约束：DNSProvider 只存服务商**名字**（cloudflare / alidns …），
// token / SecretKey 一律不写；LastError 必须经过本包的 redact。
type Attempt struct {
	Primary     string        `json:"primary"`      // 主域名（也是文件名）
	Domains     []string      `json:"domains"`      //
	Email       string        `json:"email"`        //
	Challenge   ChallengeType `json:"challenge"`    // http-01 / dns-01
	CA          string        `json:"ca"`           //
	DNSProvider string        `json:"dns_provider"` // 只有名字，绝无凭据值
	CreatedAt   time.Time     `json:"created_at"`   //
	UpdatedAt   time.Time     `json:"updated_at"`   // 最后一次失败时间
	Failures    int           `json:"failures"`     // 累计失败次数（只更新同一条，不堆积）
	LastError   string        `json:"last_error"`   // 已脱敏
}

// attemptsRoot 返回失败申请条目的根目录。
func (m *Manager) attemptsRoot() string {
	return filepath.Join(m.dataDir, accountsDirName, attemptsDirName)
}

// attemptPath 返回某主域名条目的路径。
// 主域名已过 validatePrimary（不允许路径分隔符），所以拼文件名是安全的；
// 通配符 *.example.com 里的 "*" 在文件名里是合法字符。
func (m *Manager) attemptPath(primary string) string {
	return filepath.Join(m.attemptsRoot(), primary+attemptFileExt)
}

// Attempts 列出全部失败申请条目（按主域名排序）。
//
// 目录不存在（从未失败过）不是错误，返回空列表。
func (m *Manager) Attempts() ([]*Attempt, error) {
	if m.dataDir == "" {
		return nil, errors.New("未配置数据目录（dataDir）")
	}
	entries, err := os.ReadDir(m.attemptsRoot())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*Attempt{}, nil
		}
		return nil, fmt.Errorf("读取申请记录目录失败: %w", err)
	}
	out := make([]*Attempt, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), attemptFileExt) {
			continue
		}
		a, err := m.loadAttempt(strings.TrimSuffix(e.Name(), attemptFileExt))
		if err != nil {
			// 坏条目不能让整个列表失败：跳过并如实记录（与 certs 列表一致）。
			m.emit("跳过无法读取的申请记录 %s：%v", e.Name(), err)
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Primary < out[j].Primary })
	return out, nil
}

// LoadAttempt 读单个失败申请条目。
func (m *Manager) LoadAttempt(primary string) (*Attempt, error) {
	return m.loadAttempt(strings.TrimSpace(primary))
}

func (m *Manager) loadAttempt(primary string) (*Attempt, error) {
	if err := validatePrimary(primary); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(m.attemptPath(primary))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("申请记录 %s 不存在", primary)
		}
		return nil, fmt.Errorf("读取申请记录失败: %w", err)
	}
	var a Attempt
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("申请记录 %s 已损坏: %w", primary, err)
	}
	if a.Primary == "" {
		a.Primary = primary
	}
	return &a, nil
}

// DeleteAttempt 删除一条失败申请条目（列表里的「删除」用它）。
func (m *Manager) DeleteAttempt(primary string) error {
	primary = strings.TrimSpace(primary)
	if err := validatePrimary(primary); err != nil {
		return err
	}
	if err := os.Remove(m.attemptPath(primary)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("申请记录 %s 不存在", primary)
		}
		return fmt.Errorf("删除申请记录失败: %w", err)
	}
	m.emit("已删除失败申请记录 %s", primary)
	return nil
}

// recordIssueFailure 在签发失败后保存 / 更新条目。
//
// best-effort：保存失败只告警、不改返回值 —— 用户看到的主错误必须是 CA 的失败原因，
// 而不是"顺便存条目也失败了"。但绝不静默：告警会进日志。
func (m *Manager) recordIssueFailure(plan issuePlan, cause error) {
	if cause == nil || m.dataDir == "" || plan.primary == "" {
		return
	}
	m.saveFailure(&Attempt{
		Primary:     plan.primary,
		Domains:     append([]string(nil), plan.domains...),
		Email:       plan.email,
		Challenge:   plan.challenge,
		CA:          plan.caName,
		DNSProvider: dnsProviderName(plan.dns),
	}, cause)
}

// recordRequestFailure 是 prepare 失败时的兜底：没有归一化的 plan，尽力从原始请求
// 还原出可重试的信息（CA 保留用户原始写法，让 StagingFirst 等开关在重试时仍生效）。
func (m *Manager) recordRequestFailure(req IssueRequest, cause error) {
	if cause == nil || m.dataDir == "" || len(req.Domains) == 0 {
		return
	}
	primary := strings.ToLower(strings.TrimSpace(req.Domains[0]))
	if primary == "" || validatePrimary(primary) != nil {
		// 连存储名都定不下来（例如域名非法），不要造出半个条目。
		return
	}
	challenge := req.Challenge
	if challenge == "" {
		challenge = ChallengeHTTP01
	}
	m.saveFailure(&Attempt{
		Primary:     primary,
		Domains:     append([]string(nil), req.Domains...),
		Email:       strings.TrimSpace(req.Email),
		Challenge:   challenge,
		CA:          strings.TrimSpace(req.CA),
		DNSProvider: dnsProviderName(req.DNS),
	}, cause)
}

// dnsProviderName 只取服务商名字 —— 凭据值（spec.Env）绝不进条目文件。
func dnsProviderName(spec *DNSProviderSpec) string {
	if spec == nil {
		return ""
	}
	return strings.TrimSpace(spec.Name)
}

// saveFailure 合并写入条目：同一主域名反复失败只更新同一条
// （CreatedAt 保留首次、Failures 递增），不会堆积同主域名的多份记录。
func (m *Manager) saveFailure(fresh *Attempt, cause error) {
	now := time.Now()
	a := fresh
	if old, err := m.loadAttempt(fresh.Primary); err == nil {
		a.CreatedAt = old.CreatedAt
		a.Failures = old.Failures
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	a.Failures++
	// 双保险：错误文本先经 safeError（errors.go）脱敏，这里再走一遍 redact。
	a.LastError = truncateAttemptError(redact(cause.Error()))

	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		m.emit("警告：序列化失败申请记录出错（%v）", err)
		return
	}
	if err := ensureDir(m.attemptsRoot()); err != nil {
		m.emit("警告：创建申请记录目录失败（%v）", err)
		return
	}
	if err := writeFileAtomic(m.attemptPath(a.Primary), data, attemptFileMode); err != nil {
		m.emit("警告：保存失败申请记录出错（%v），下次需要重新填写申请信息", err)
		return
	}
	m.emit("已保存本次失败的申请记录 %s（累计失败 %d 次）：可在证书页一键重试", a.Primary, a.Failures)
}

// clearAttempt 删除某主域名的失败条目（签发成功或证书被删除时调用）。
// 删除失败只告警：它不该把一个已经成功的签发判成失败。
func (m *Manager) clearAttempt(primary string) {
	if m.dataDir == "" || primary == "" {
		return
	}
	if err := os.Remove(m.attemptPath(primary)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.emit("警告：清理失败申请记录 %s 出错（%v）", primary, err)
	}
}

// truncateAttemptError 按字符（不是字节）截断，避免把多字节字符切成半个。
func truncateAttemptError(s string) string {
	r := []rune(s)
	if len(r) > maxAttemptErrorLen {
		return string(r[:maxAttemptErrorLen]) + "…(截断)"
	}
	return s
}
