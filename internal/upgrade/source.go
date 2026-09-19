package upgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  升级源候选
//
//  候选顺序＝用户显式配置的源 → 公网主源 zizdog.com → 备用公网镜像 → GitHub 兜底。
//  **没有局域网候选**：面板发布给所有人用，作者自己的局域网镜像入口已删除，
//  别的用户的网络也永远进不去（2026-09-20 用户明确要求）。
//
//  安全模型不变：候选只决定"去哪里下载"。**每一个候选的清单都必须通过
//  Ed25519 验签**才会被采用（见 FetchManifestAny），换源换不掉签名，
//  也没有任何"跳过验证"的开关。
// ============================================================================

const (
	// DefaultSource 是公网主源。直接引用 config 里的常量，避免两处各写一份以后漂移。
	DefaultSource = config.DefaultUpgradeSource

	// MirrorSource 是备用公网镜像（面板其余下载也用它，见 DefaultMirrorBase）。
	MirrorSource = "https://mirror.zizdog.com:8888/zizpanel"

	// GitHubSource 是最后的兜底：GitHub Releases 的 latest/download 固定地址。
	// DNS 被污染时它可能失败，这属于预期 —— 前面还有两个公网候选。
	GitHubSource = "https://github.com/zizdog/zizpanel/releases/latest/download"
)

// CandidateSources 返回按优先级排列的升级源候选（已规范化尾部斜杠、已去重）。
//
// 顺序：
//
//  1. 用户显式配置的源（若有）
//  2. 公网主源 DefaultSource
//  3. 备用公网镜像 MirrorSource
//  4. GitHub Releases 兜底
//
// 特例：configured 恰好等于默认主源时会去重，只留一份。
func CandidateSources(configured string) []string {
	configured = strings.TrimRight(strings.TrimSpace(configured), "/")

	out := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	add := func(raw string) {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, raw)
	}

	// 用户/安装脚本配置的源**永远排第一**。
	//
	// 为什么清单一律取自权威源（2026-09-17，坑 149）：局域网镜像的清单可能滞后，
	// "某次只推公网、忘了同步镜像"会让机器静默停在旧版本。取消局域网候选后
	// 这条纪律更简单：所有候选都是公开源。
	if configured != "" {
		add(configured)
	}
	add(DefaultSource)
	add(MirrorSource)
	add(GitHubSource)
	return out
}

// ManifestFetcher 负责"把清单原文与签名拉下来"。
//
// 单独抽成函数类型是为了让上层（web）能注入假实现：单测**不许联网**，
// 而候选回落逻辑又必须被真实测到。注意它只负责下载 ——
// 验签与解析统一由 FetchManifestAny 完成，任何候选都绕不过去。
type ManifestFetcher func(ctx context.Context, baseURL string) (data, sig []byte, err error)

// FetchManifestAny 依次尝试候选源，返回第一个**验签通过**的清单及其来源。
//
// 每个候选有独立的 perTry 超时预算，互不拖累；任何一个候选失败
// （网络不通、缺清单/签名、验签失败、清单格式不对）都继续试下一个。
//
// 全部失败时返回的 error 会**逐个列出每个候选各自的失败原因** ——
// 只回最后一个会让人误以为问题只出在最后一个源上，真机排障时
// 根本看不出"到底试过哪些地址、各自为什么不行"。
//
// fetch 传 nil 表示用真实 HTTP 实现（FetchManifestRaw）；单测传假实现。
func FetchManifestAny(ctx context.Context, sources []string, perTry time.Duration, fetch ManifestFetcher) (*Manifest, string, error) {
	if fetch == nil {
		fetch = FetchManifestRaw
	}
	// fail closed：没有公钥就一个请求都不发。候选有多个，逐个试只会把
	// 同一个原因重复 N 遍，还会白白发 N 组请求。
	if !HasPublicKey() {
		return nil, "", ErrNoPublicKey
	}
	if len(sources) == 0 {
		return nil, "", errors.New("没有可用的升级源候选")
	}
	if perTry <= 0 {
		perTry = manifestTimeout
	}

	failures := make([]string, 0, len(sources))
	for _, base := range sources {
		if err := ctx.Err(); err != nil {
			// 外层预算用完：不必再试（后续候选也只会立刻失败）。
			failures = append(failures, fmt.Sprintf("%s: 未尝试（外层超时/已取消: %v）", base, err))
			break
		}
		tryCtx, cancel := context.WithTimeout(ctx, perTry)
		data, sig, err := fetch(tryCtx, base)
		cancel()
		if err == nil {
			// 验签放在这里、而不是交给 fetcher：候选只解决"去哪下"，
			// "信不信"必须复用升级流程唯一的那份验签实现。
			if verr := VerifyManifest(data, sig); verr != nil {
				err = verr
			} else if m, perr := ParseManifest(data); perr != nil {
				err = perr
			} else {
				return m, base, nil
			}
		}
		failures = append(failures, fmt.Sprintf("%s: %v", base, err))
	}
	// 全部候选失败：逐个列出原因，有网络证据就再附统一提示（判据见 services/netfail.go）。
	return nil, "", services.AppendNetworkHint(fmt.Errorf("所有升级源都失败（共尝试 %d 个候选）：\n  - %s",
		len(failures), strings.Join(failures, "\n  - ")))
}
