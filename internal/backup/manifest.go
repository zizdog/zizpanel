// Package backup 实现「网站配置 / 证书 / 反向代理」的备份与恢复。
//
// 设计要点（详见 docs/备份与恢复设计.md）：
//   - 数据库用 `VACUUM INTO` 取**一致性**快照，绝不直接 tar panel.db+wal+shm；
//   - 归档是 tar.gz + manifest.json，每个文件带 size 与 sha256，恢复前逐条校验，
//     任何一条不符 → **整包拒绝**（不做部分恢复）；
//   - 路径全部来自调用方传入的配置，包里没有任何写死的 /opt/zizpanel、
//     /opt/homebrew —— 重定位根目录或 Intel 前缀（/usr/local）下也必须正确；
//   - 归档权限 0600，因为里面含明文口令与私钥。
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Format 是归档格式标识。恢复时严格比对：格式不认识就拒绝，不做"尽力而为"。
const Format = "zizpanel-backup/1"

// ManifestName 是归档内清单的文件名。
const ManifestName = "manifest.json"

// FileEntry 是清单里的一条文件记录。
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Secrets 标记该文件是否含凭据（界面上要明确写"含明文口令"）。
	Secrets bool `json:"secrets,omitempty"`
}

// Manifest 是归档的自描述元数据。
type Manifest struct {
	Format        string `json:"format"`
	CreatedAt     string `json:"created_at"`
	PanelVersion  string `json:"panel_version"`
	Hostname      string `json:"hostname"`
	SourceDataDir string `json:"source_data_dir"` // 来源机器路径，仅供展示，恢复时不使用
	BrewPrefix    string `json:"brew_prefix,omitempty"`
	// Targets 是本次备份勾选的范围。
	Targets []string `json:"targets"`
	// SchemaTables 是快照库里的表清单（兼容性判定靠它，不能靠 schema_version ——
	// 面板虽然写 settings.schema_version，但当前代码从不读它）。
	SchemaTables    []string    `json:"schema_tables"`
	ContainsSecrets bool        `json:"contains_secrets"`
	Files           []FileEntry `json:"files"`
	// Warnings 是打包过程中"跳过了什么"的如实记录（如符号链接）。
	Warnings []string `json:"warnings,omitempty"`
}

// SecretFiles 返回含凭据的文件清单（界面二次确认用）。
func (m *Manifest) SecretFiles() []string {
	var out []string
	for _, f := range m.Files {
		if f.Secrets {
			out = append(out, f.Path)
		}
	}
	return out
}

// FileByPath 按归档内路径查一条记录。
func (m *Manifest) FileByPath(p string) (FileEntry, bool) {
	for _, f := range m.Files {
		if f.Path == p {
			return f, true
		}
	}
	return FileEntry{}, false
}

// Compat 是备份与当前程序的兼容性判定结果。
type Compat struct {
	OK bool
	// Older 为 true 表示备份比程序旧（缺表/缺列）→ 允许恢复，但必须重放 migrate()。
	Older bool
	// UnknownTables 是备份里当前程序不认识的表（备份比程序新）。
	UnknownTables []string
	// MissingTables 是当前程序有、备份没有的表（备份更旧）。
	MissingTables []string
	Reason        string
}

// CheckCompatibility 用"表清单对比"判定兼容性。
//
// 为什么不用 settings.schema_version：面板只在迁移末尾写它，代码里**从不读**，
// 老库里的值可能是空的或错的。表清单是磁盘事实。
func CheckCompatibility(m *Manifest, knownTables []string) Compat {
	if m == nil {
		return Compat{Reason: "缺少清单"}
	}
	if m.Format != Format {
		return Compat{Reason: fmt.Sprintf("归档格式 %q 不是本程序支持的 %q", m.Format, Format)}
	}
	known := map[string]bool{}
	for _, t := range knownTables {
		known[t] = true
	}
	have := map[string]bool{}
	for _, t := range m.SchemaTables {
		have[t] = true
	}
	if len(have) == 0 {
		return Compat{Reason: "清单里没有表信息（schema_tables 为空），无法判断兼容性"}
	}
	var unknown, missing []string
	for _, t := range m.SchemaTables {
		if !known[t] {
			unknown = append(unknown, t)
		}
	}
	for _, t := range knownTables {
		if !have[t] {
			missing = append(missing, t)
		}
	}
	sort.Strings(unknown)
	sort.Strings(missing)
	if len(unknown) > 0 {
		return Compat{
			UnknownTables: unknown,
			Reason: fmt.Sprintf("备份里有当前程序不认识的表：%s —— 备份比本程序新，请先升级面板",
				strings.Join(unknown, ", ")),
		}
	}
	return Compat{
		OK:            true,
		Older:         len(missing) > 0,
		MissingTables: missing,
		Reason:        fmt.Sprintf("兼容（备份 %d 张表，当前程序 %d 张表）", len(have), len(known)),
	}
}

// hashFile 计算文件 sha256（流式，不把大文件读进内存）。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// hashBytes 计算字节切片 sha256（校验归档内条目用）。
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func nowStamp() string { return time.Now().Format("2006-01-02 15:04:05") }

// stampForFile 返回文件名用的时间戳。
func stampForFile(t time.Time) string { return t.Format("20060102-150405") }

func writeManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dir+string(os.PathSeparator)+ManifestName, b, 0o600)
}
