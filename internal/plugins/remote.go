// 远端插件（B3）：从签名目录里取插件声明，落进**本地插件目录**再按本地插件走。
//
// 信任模型（刻意保守，别松）：
//
//	· 目录本身是**签名**的（Ed25519，复用升级链路的信任根，见 internal/upgrade）；
//	· 每个插件声明再按 sha256 校验一次 —— 签名只覆盖目录，不覆盖被指向的文件；
//	· 声明落盘前仍要过**同一套**严格校验（Parse：未知键一律拒绝）；
//	· **默认不联网**：调用方必须显式给出 --source；落盘的插件默认仍处于"未启用"，
//	  用户还要在面板里再点一次启用（双重确认）。
package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// VerifySignature 是"目录签名的信任根"。默认**没有**验证器 —— 没配就一律拒绝，
// 绝不出现"验签器缺失 ⇒ 直接放行"的形态。
//
// 为什么用注入而不是直接 import internal/upgrade：upgrade 反过来依赖 services，
// 而 services 依赖 plugins（目录合并），直接引会成环。注入既断开环，也让测试能换钥匙。
var VerifySignature = func(data, sig []byte) error {
	return fmt.Errorf("面板没有配置插件目录的验签公钥：拒绝使用远端插件")
}

// RemoteIndexSchema 是远端插件目录的 schema。
const RemoteIndexSchema = "zizpanel.plugins/v1"

// RemoteRecordFile 记录"这个本地插件是从哪来的"（0600，点开头 ⇒ 不会被当成插件解析）。
const RemoteRecordFile = ".remote.json"

// RemoteIndex 是签名覆盖的远端目录。
type RemoteIndex struct {
	Schema      string        `json:"schema"`
	GeneratedAt string        `json:"generated_at,omitempty"`
	Plugins     []RemoteEntry `json:"plugins"`
}

// RemoteEntry 是目录里的一条。
type RemoteEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Icon    string `json:"icon,omitempty"`
	Summary string `json:"summary,omitempty"`
	Version string `json:"version,omitempty"`
	// URL 指向插件声明本体（相对目录地址的路径也可，例如 "aria2.json"）。
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// RemoteRecord 是"这个插件从远端装的"这条事实。
type RemoteRecord struct {
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	Version     string `json:"version,omitempty"`
	IndexURL    string `json:"index_url,omitempty"`
	InstalledAt string `json:"installed_at,omitempty"`
}

// FetchRemoteIndex 取签名目录并**先验签再解析**（验签不过就当它不存在）。
func FetchRemoteIndex(ctx context.Context, indexURL string) (*RemoteIndex, error) {
	indexURL = strings.TrimSpace(indexURL)
	if indexURL == "" {
		return nil, fmt.Errorf("远端插件默认关闭：必须显式给出目录地址（例如 https://mirror.zizdog.com:8888/plugins/index.json）")
	}
	body, err := httpGet(ctx, indexURL)
	if err != nil {
		return nil, fmt.Errorf("取插件目录失败：%w", err)
	}
	sig, err := httpGet(ctx, indexURL+".sig")
	if err != nil {
		return nil, fmt.Errorf("取插件目录签名失败（没有签名一律不认）：%w", err)
	}
	if err := VerifySignature(body, sig); err != nil {
		return nil, fmt.Errorf("插件目录验签失败：%w", err)
	}
	var idx RemoteIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("插件目录解析失败：%w", err)
	}
	if idx.Schema != RemoteIndexSchema {
		return nil, fmt.Errorf("插件目录 schema 应为 %q，实际 %q", RemoteIndexSchema, idx.Schema)
	}
	if len(idx.Plugins) == 0 {
		return nil, fmt.Errorf("插件目录是空的")
	}
	return &idx, nil
}

// Find 在目录里找一条。
func (idx *RemoteIndex) Find(id string) (RemoteEntry, bool) {
	for _, e := range idx.Plugins {
		if e.ID == strings.TrimSpace(id) {
			return e, true
		}
	}
	return RemoteEntry{}, false
}

// FetchSpec 取插件声明本体：先按 sha256 核对，再走严格校验。
func (e RemoteEntry) FetchSpec(ctx context.Context, indexURL string) (*Spec, error) {
	raw, err := httpGet(ctx, resolveURL(indexURL, e.URL))
	if err != nil {
		return nil, fmt.Errorf("取插件声明失败：%w", err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(strings.TrimSpace(e.SHA256), got) {
		return nil, fmt.Errorf("插件声明 sha256 不一致（目录说 %s，实际 %s）：拒绝安装",
			strings.TrimSpace(e.SHA256), got)
	}
	spec, err := Parse(raw, e.URL)
	if err != nil {
		return nil, fmt.Errorf("插件声明没通过校验：%w", err)
	}
	if e.ID != "" && spec.ID != e.ID {
		return nil, fmt.Errorf("目录里的 id（%s）与声明里的（%s）不一致：拒绝安装", e.ID, spec.ID)
	}
	return spec, nil
}

// InstallOptions 是 InstallRemote 需要的环境信息。
type InstallOptions struct {
	// Reserved 报告某个 id 是否已被占用（内建应用 / 已存在的本地插件）——
	// 由调用方注入，因为 plugins 包不能反向依赖 services。
	Reserved func(id string) bool
	IndexURL string
	Version  string
}

// InstallRemote 把一条远端插件落进本地插件目录（**默认未启用**）。
//
// 落盘顺序：先 FetchSpec 校验通过，再写 `<id>.json` 与来源记录；任何一步失败都不留半成品。
func InstallRemote(ctx context.Context, dir string, entry RemoteEntry, opts InstallOptions) (*Spec, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("插件目录为空")
	}
	if opts.Reserved != nil && opts.Reserved(entry.ID) {
		return nil, fmt.Errorf("id %q 已被面板自带应用或已存在的插件占用：拒绝安装（远端插件不许覆盖本机的东西）", entry.ID)
	}
	spec, err := entry.FetchSpec(ctx, opts.IndexURL)
	if err != nil {
		return nil, err
	}
	if opts.Reserved != nil && opts.Reserved(spec.ID) {
		return nil, fmt.Errorf("id %q 已被占用：拒绝安装", spec.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// 声明本身由 FetchSpec 校验过，这里重新序列化落盘（0600：它是可执行能力的声明）
	blob, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	target := filepath.Join(dir, spec.ID+".json")
	if err := os.WriteFile(target, append(blob, '\n'), 0o600); err != nil {
		return nil, err
	}
	rec := map[string]RemoteRecord{}
	if old := RemoteRecords(dir); old != nil {
		rec = old
	}
	rec[spec.ID] = RemoteRecord{URL: entry.URL, SHA256: entry.SHA256, Version: opts.Version,
		IndexURL: opts.IndexURL, InstalledAt: time.Now().Format(time.RFC3339)}
	if err := writeRecords(dir, rec); err != nil {
		_ = os.Remove(target) // 记录写不进去就把声明也撤掉，不留半成品
		return nil, err
	}
	return spec, nil
}

// RemoteRecords 读"哪些插件来自远端"。
func RemoteRecords(dir string) map[string]RemoteRecord {
	raw, err := os.ReadFile(filepath.Join(dir, RemoteRecordFile))
	if err != nil {
		return nil
	}
	out := map[string]RemoteRecord{}
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// RemoveRemote 删除一个远端插件（声明 + 记录）。**不碰**已装好的应用。
func RemoveRemote(dir, id string) error {
	id = strings.TrimSpace(id)
	rec := RemoteRecords(dir)
	if rec == nil || rec[id].URL == "" {
		return fmt.Errorf("%s 不是从远端装的（本地插件请直接改/删文件）", id)
	}
	delete(rec, id)
	if err := os.Remove(filepath.Join(dir, id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeRecords(dir, rec)
}

func writeRecords(dir string, rec map[string]RemoteRecord) error {
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, RemoteRecordFile), append(blob, '\n'), 0o600)
}

// resolveURL 把相对路径拼到目录地址上。
func resolveURL(indexURL, ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	base := indexURL
	if i := strings.LastIndex(base, "/"); i > 0 {
		base = base[:i]
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(ref, "/")
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d（%s）", res.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(res.Body, 4<<20))
}
