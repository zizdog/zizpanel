package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
)

// ============================================================================
//  升级源候选
//
//  背景：过去升级源是**单一地址**，没有默认值也没有回落 —— 没配就是
//  400「尚未配置升级源地址」。用户要求改成"以后升级探测以公网 zizdog.com
//  为主"，同时局域网里从 NAS 拉包快约 100 倍（实测 38–81 MB/s 对
//  0.3–0.65 MB/s），所以候选顺序必须把"同网段的 NAS"插到公网源前面。
//
//  安全模型不变：候选只决定"去哪里下载"。**每一个候选的清单都必须通过
//  Ed25519 验签**才会被采用（见 FetchManifestAny），换源换不掉签名，
//  也没有任何"跳过验证"的开关。
// ============================================================================

const (
	// DefaultSource 是公网主源。直接引用 config 里的常量，避免两处各写一份以后漂移。
	DefaultSource = config.DefaultUpgradeSource

	// NASSource 是局域网镜像上的升级源。只有本机与 NAS 同网段时才会成为候选。
	//
	// 用明文 HTTP 是可以接受的：升级的信任根是清单签名，不是传输通道
	// （见 Source.Normalize 的注释）。但它只出现在私有网段，不会在公网裸奔。
	NASSource = "http://192.168.1.8:8090/zizpanel"

	// MirrorSource 是备用公网镜像（面板其余下载也用它，见 DefaultMirrorBase）。
	MirrorSource = "https://mirror.zizdog.com:8888/zizpanel"

	// GitHubSource 是最后的兜底：GitHub Releases 的 latest/download 固定地址。
	// DNS 被污染时它可能失败，这属于预期 —— 前面还有两个公网候选。
	GitHubSource = "https://github.com/zizdog/zizpanel/releases/latest/download"

	// NASSubnet 是 NAS 所在网段。本机有网卡落在里面才把 NAS 排进候选。
	NASSubnet = "192.168.1.0/24"
)

// localInterfaceIPs 返回本机所有网卡 IP。
//
// 变量而不是函数：单测必须能注入"本机在 192.168.1.x / 不在"两种情形，
// 否则候选顺序只能靠真机验证 —— 而顺序恰恰是这段逻辑的全部意义。
var localInterfaceIPs = localInterfaceIPsReal

// localInterfaceIPsReal 是生产实现：枚举所有网卡地址（含 IPv6，交给上层过滤）。
func localInterfaceIPsReal() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			out = append(out, v.IP)
		case *net.IPAddr:
			out = append(out, v.IP)
		}
	}
	return out
}

// SetLocalInterfaceIPsForTest 替换网卡地址探测，返回值供测试恢复原实现。
//
// **只给测试用。** 正式路径永远以真实网卡为准；测试也不能真的去相信
// 跑测试那台机器的网络环境（开发机就在 192.168.1.0/24 里）。
func SetLocalInterfaceIPsForTest(fn func() []net.IP) func() []net.IP {
	prev := localInterfaceIPs
	if fn != nil {
		localInterfaceIPs = fn
	}
	return prev
}

// OnNASSubnet 报告本机是否有非回环网卡落在 NAS 网段内。
func OnNASSubnet() bool {
	_, subnet, err := net.ParseCIDR(NASSubnet)
	if err != nil {
		return false
	}
	for _, ip := range localInterfaceIPs() {
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if subnet.Contains(ip) {
			return true
		}
	}
	return false
}

// CandidateSources 返回按优先级排列的升级源候选（已规范化尾部斜杠、已去重）。
//
// 顺序（用户要"公网为主"，但同网段时 NAS 插到最前）：
//
//  1. 用户显式配置的源（若有）
//  2. 本机在与 NAS 同网段时的 NAS 地址
//  3. 公网主源 DefaultSource
//  4. 备用镜像 MirrorSource
//  5. GitHub Releases 兜底
//
// 为什么同网段时 NAS 排在公网前面：升级包几十 MB，从 NAS 走局域网实测
// 38–81 MB/s，走公网 zizdog.com 只有 0.3–0.65 MB/s（差约 100 倍）。
// NAS 试不通时下一个候选立刻回落公网，代价只是一次极短的本地请求。
//
// 特例：configured 恰好等于默认主源时按"没配过"处理（放到第 3 位）。
// 因为无法区分"用户手填了默认地址"与"配置里根本没有这个字段"，而把它排到
// NAS 前面会让 install.sh 写入默认公网源的机器永远用不上局域网快通道 ——
// 那不是用户要的。要强制公网优先，填一个等价的其它地址即可。
func CandidateSources(configured string) []string {
	configured = strings.TrimRight(strings.TrimSpace(configured), "/")

	out := make([]string, 0, 5)
	seen := make(map[string]bool, 5)
	add := func(raw string) {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, raw)
	}

	if configured != "" && configured != DefaultSource {
		add(configured)
	}
	if OnNASSubnet() {
		add(NASSource)
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
	return nil, "", fmt.Errorf("所有升级源都失败（共尝试 %d 个候选）：\n  - %s",
		len(failures), strings.Join(failures, "\n  - "))
}
