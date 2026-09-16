package acme

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 本文件是证书/账户的落盘层。关键约束：
//
//   - 私钥 privkey.pem 权限 0600：它不是「面板数据」，而是任何能读到它的进程
//     都能冒充该站点 TLS 身份的最高敏感材料。0600 + 目录 0700 保证只有运行
//     面板的用户（root 安装场景下就是 root）能读。
//   - 私钥内容绝不进日志、绝不进错误信息：日志会被任务中心持久化并展示给所有
//     面板用户，错误信息也可能被原样回显；一旦写进去等于把密钥公开。
//   - 写入用「临时文件 + rename」：nginx 可能在任意时刻读取 fullchain.pem，
//     直接覆盖会产生「读到半个文件」的窗口，导致 reload 失败或线上握手失败。

// certsRoot 返回证书根目录。
func (m *Manager) certsRoot() string { return filepath.Join(m.dataDir, certsDirName) }

// certDir 返回某个证书的目录。
func (m *Manager) certDir(primary string) string {
	return filepath.Join(m.certsRoot(), primary)
}

// accountsRoot 返回 ACME 账户目录。账户私钥也在这里，所以同样 0700。
func (m *Manager) accountsRoot() string {
	return filepath.Join(m.dataDir, accountsDirName, accountsSubDir)
}

// writeFileAtomic 原子写入文件并设置权限。
// os.CreateTemp 默认 0600，这里显式 chmod 到目标权限后再 rename。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	// 任何中途失败都要清掉临时文件，避免证书目录里堆垃圾（其中可能含私钥）。
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("刷新文件到磁盘失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换文件 %s 失败: %w", filepath.Base(path), err)
	}
	tmp = "" // 已 rename，defer 不再删除
	// rename 保留临时文件的权限，但某些文件系统（或 umask 交互）下再兜一次底。
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	return nil
}

// ensureDir 创建目录并强制权限（目录必须 0700）。
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("设置目录权限失败: %w", err)
	}
	return nil
}

// saveCert 把一次成功签发的材料落盘。
// 顺序：先证书/私钥，最后 meta.json —— meta 是 List/Load 的「提交标记」，
// 先写 meta 再写文件会在崩溃后留下「meta 指向不存在的证书」的坏状态。
func (m *Manager) saveCert(plan issuePlan, c *Cert, res *obtainedCert, req IssueRequest) error {
	dir := m.certDir(c.Primary)
	if err := ensureDir(dir); err != nil {
		return err
	}

	if err := writeFileAtomic(filepath.Join(dir, fileFullchain), res.fullchain, certFileMode); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, filePrivkey), res.key, keyFileMode); err != nil {
		return err
	}

	meta, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化证书元信息失败: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, fileMeta), meta, metaFileMode); err != nil {
		return err
	}

	// renewal.json 供 Renew 使用：Renew 只拿到主域名，必须能从磁盘还原出
	// 「域名列表 + 验证方式 + CA + DNS 凭据」。它是 0600，因为里面可能有 DNS API token。
	rec := renewalRecord{Request: req, Challenge: plan.challenge, CA: plan.caName}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化续期信息失败: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, fileRenewal), data, keyFileMode); err != nil {
		return err
	}
	return nil
}

// renewalRecord 是续期所需的原始请求。
// 单独存文件而不是塞进 Cert：Cert 是契约固定的结构，不能为了内部需要加字段。
type renewalRecord struct {
	Request   IssueRequest  `json:"request"`
	Challenge ChallengeType `json:"challenge"`
	CA        string        `json:"ca"`
}

// loadMeta 读取单个证书的 meta.json。
func (m *Manager) loadMeta(primary string) (*Cert, error) {
	if err := validatePrimary(primary); err != nil {
		return nil, err
	}
	path := filepath.Join(m.certDir(primary), fileMeta)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("证书 %s 不存在（缺少 %s）", primary, fileMeta)
		}
		return nil, fmt.Errorf("读取证书元信息失败: %w", err)
	}
	var c Cert
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("证书元信息 %s 已损坏: %w", fileMeta, err)
	}
	if c.Primary == "" {
		c.Primary = primary
	}
	// 老 meta 里可能没写路径，补全，避免面板拿到空路径。
	if c.CertPath == "" {
		c.CertPath = filepath.Join(m.certDir(primary), fileFullchain)
	}
	if c.KeyPath == "" {
		c.KeyPath = filepath.Join(m.certDir(primary), filePrivkey)
	}
	return &c, nil
}

// loadRenewal 读取续期记录。
func (m *Manager) loadRenewal(primary string) (renewalRecord, error) {
	var rec renewalRecord
	data, err := os.ReadFile(filepath.Join(m.certDir(primary), fileRenewal))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, fmt.Errorf("缺少 %s，无法自动续期", fileRenewal)
		}
		return rec, fmt.Errorf("读取续期信息失败: %w", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("续期信息已损坏: %w", err)
	}
	if len(rec.Request.Domains) == 0 {
		return rec, errors.New("续期信息里没有域名")
	}
	// 兜底：早期记录可能没写这两个字段，用 meta 里的值补齐。
	if rec.Challenge == "" {
		if c, err := m.loadMeta(primary); err == nil {
			rec.Challenge = c.Challenge
		}
	}
	if rec.CA == "" {
		if c, err := m.loadMeta(primary); err == nil {
			rec.CA = c.CA
		}
	}
	if rec.Request.Challenge == "" {
		rec.Request.Challenge = rec.Challenge
	}
	if rec.Request.CA == "" {
		rec.Request.CA = rec.CA
	}
	return rec, nil
}

// accountRecord 是持久化的 ACME 账户。
//
// 为什么要持久化账户私钥：ACME 账户由私钥唯一标识。如果不保存，每次签发都会
// 新建一个账户，很快撞上 CA 的「每 IP 每 3 小时 10 个账户」限制，而且历史证书
// 无法再用同一账户管理。KeyPEM 是私钥，文件 0600、绝不进日志。
type accountRecord struct {
	Email     string    `json:"email"`
	CA        string    `json:"ca"`
	URI       string    `json:"uri"`
	KeyPEM    string    `json:"key_pem"`
	CreatedAt time.Time `json:"created_at"`
}

// accountPath 由 (CA, 邮箱) 派生账户文件名。
// 用哈希而不是邮箱原文做文件名：邮箱可能含 "/" 等字符，直接拼接会有路径穿越风险。
func accountPath(root, caName, email string) string {
	sum := sha256.Sum256([]byte(caName + "\x00" + email))
	return filepath.Join(root, hex.EncodeToString(sum[:])[:24]+".json")
}
