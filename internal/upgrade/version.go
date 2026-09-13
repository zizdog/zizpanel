// Package upgrade 实现面板的「在线升级」：检查更新、下载校验、原子替换、失败回滚。
//
// 为什么单独一个包，而不是塞进 web 层：
//
//	升级会**替换正在运行的二进制并重启服务**，一旦出错用户就失去了唯一的
//	远程管理入口。所以这里的每一步都必须可单独测试：
//	版本比较、清单验签、校验和、解包安全、回滚判定。
//	web 层只负责编排与鉴权，不做任何安全判定。
//
// 安全模型（按重要性排序）：
//
//  1. **签名优先于一切**。远端下载的升级包会用内嵌的 Ed25519 公钥验签，
//     校验的是「清单」，清单里再带 tar 包的 SHA-256。
//     这样即使下载源被完全控制、或者走了明文 HTTP，
//     攻击者也无法让我们安装一个他没签过名的二进制。
//  2. 手动上传的升级包走另一条信任路径：上传者已经是登录后的管理员
//     （面板管理员本来就等价于 root），所以不再要求签名，
//     但**仍然**做 SHA-256 自校验与解包安全检查。
//  3. 任何一步失败都必须在**换掉二进制之前**失败。换掉之后才发现问题，
//     就只能靠看门狗回滚了，那是最后一道防线而不是第一道。
package upgrade

import (
	"fmt"
	"strconv"
	"strings"
)

// CompareVersions 比较两个语义化版本号，返回 -1 / 0 / 1。
//
// 只实现我们真正需要的子集，刻意不引入第三方依赖：
//
//	"0.2.0" > "0.1.9"
//	"1.0.0" > "0.9.9"
//	"1.0.0" > "1.0.0-rc1"   预发布版本小于正式版本
//	"1.0.0-rc2" > "1.0.0-rc1"
//	"1.2"   == "1.2.0"      缺省的段按 0 处理
//
// 遇到无法解析的版本串时**不做猜测**：直接返回错误，
// 让上层明确报"版本号格式无法识别"，而不是悄悄判定成"有更新"，
// 那会导致用户被反复提示升级到一个装不上的包。
func CompareVersions(a, b string) (int, error) {
	av, apre, err := parseSemver(a)
	if err != nil {
		return 0, fmt.Errorf("版本号 %q 无法解析: %w", a, err)
	}
	bv, bpre, err := parseSemver(b)
	if err != nil {
		return 0, fmt.Errorf("版本号 %q 无法解析: %w", b, err)
	}

	for i := 0; i < 3; i++ {
		switch {
		case av[i] < bv[i]:
			return -1, nil
		case av[i] > bv[i]:
			return 1, nil
		}
	}

	// 主版本相同：有预发布后缀的更小（1.0.0-rc1 < 1.0.0）
	switch {
	case apre == "" && bpre == "":
		return 0, nil
	case apre == "":
		return 1, nil
	case bpre == "":
		return -1, nil
	case apre == bpre:
		return 0, nil
	}
	// 逐段比较预发布串：数字段按数值比，其余按字典序
	return comparePrerelease(apre, bpre), nil
}

func parseSemver(v string) ([3]int, string, error) {
	var out [3]int
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v") // 允许 tag 写成 v0.2.0
	if s == "" {
		return out, "", fmt.Errorf("空字符串")
	}

	pre := ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return out, "", fmt.Errorf("段数超过 3")
	}
	for i, p := range parts {
		if p == "" {
			return out, "", fmt.Errorf("第 %d 段为空", i+1)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, "", fmt.Errorf("第 %d 段 %q 不是数字", i+1, p)
		}
		if n < 0 {
			return out, "", fmt.Errorf("第 %d 段为负数", i+1)
		}
		out[i] = n
	}
	return out, pre, nil
}

func comparePrerelease(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aerr == nil:
			// 数字标识符优先级低于字母标识符
			return -1
		case berr == nil:
			return 1
		default:
			if as[i] != bs[i] {
				if as[i] < bs[i] {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// IsNewer 判断 candidate 是否比 current 新。
// 版本串无法解析时返回 false 并带出错误 —— 宁可"不提示更新"，也不误报。
func IsNewer(candidate, current string) (bool, error) {
	c, err := CompareVersions(candidate, current)
	if err != nil {
		return false, err
	}
	return c > 0, nil
}
