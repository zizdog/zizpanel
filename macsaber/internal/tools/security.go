package tools

import (
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"

	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// 本文件全部工具**只读**：只跑 codesign/spctl/security/pkgutil/csrutil/system_profiler，
// 绝不写钥匙串、不动系统安全设置（坑 S1：负面判定不是工具失败，要如实呈现）。

// ----------------------------------------------------------------------------
// 共享解析helper（本文件内）
// ----------------------------------------------------------------------------

// kvLines 解析 "Key=Value" 与裸行 "Key: Value" 两种格式（codesign 同时用两种）。
func kvLines(out string) map[string][]string {
	m := map[string][]string{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "Executable Segment") {
			continue
		}
		// "CodeDirectory v=20400 size=… flags=0x…(name) hashes=…"：首词是键、其余是值。
		// 必须先判它，否则 "CodeDirectory v=20400" 里的 = 会把键切成 "CodeDirectory v"（坑 S3）。
		if strings.HasPrefix(ln, "CodeDirectory ") {
			f := strings.Fields(ln)
			m[f[0]] = append(m[f[0]], strings.Join(f[1:], " "))
			continue
		}
		if i := strings.IndexByte(ln, '='); i > 0 {
			k := strings.TrimSpace(ln[:i])
			v := strings.TrimSpace(ln[i+1:])
			if k != "" {
				m[k] = append(m[k], v)
			}
			continue
		}
		if i := strings.Index(ln, ": "); i > 0 {
			m[strings.TrimSpace(ln[:i])] = append(m[strings.TrimSpace(ln[:i])], strings.TrimSpace(ln[i+2:]))
			continue
		}
		// codesign 的 "CodeDirectory v=20400 size=… flags=0x…(name) hashes=…" 没有分隔符，
		// 首词是键、其余是值；不认这一行就永远读不到 flags（坑 S3）。
		if f := strings.Fields(ln); len(f) >= 2 {
			m[f[0]] = append(m[f[0]], strings.Join(f[1:], " "))
		}
	}
	return m
}

func firstKV(m map[string][]string, key string) string {
	if v := m[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// flagsHas 判断 codesign flags=0x…(name) 是否含某个位（如 0x10000 公证运行时标志）。
func flagsHas(flags string, bit uint64) bool {
	// 入参可能是 "flags=0x10000(runtime)"，也可能是 CodeDirectory 值里的 flags=… 片段。
	if i := strings.Index(flags, "flags="); i >= 0 {
		flags = flags[i+len("flags="):]
	}
	if i := strings.Index(flags, "="); i >= 0 && !strings.HasPrefix(strings.TrimSpace(flags), "0x") {
		flags = flags[i+1:]
	}
	open := strings.IndexByte(flags, '(')
	if open < 0 {
		open = len(flags)
	}
	hexPart := strings.TrimSpace(flags[:open])
	hexPart = strings.TrimPrefix(hexPart, "0x")
	hexPart = strings.TrimPrefix(hexPart, "0X")
	n, err := strconv.ParseUint(hexPart, 16, 64)
	if err != nil {
		return false
	}
	return n&bit != 0
}

// isUnsignedCodesign 判断 codesign 输出是否就是"根本没签名"（这是结果，不是失败）。
func isUnsignedCodesign(res *execx.Result) bool {
	blob := strings.ToLower(res.Stdout + res.Stderr)
	return strings.Contains(blob, "code object is not signed at all") ||
		strings.Contains(blob, "code object is not signed")
}

var spctlVerdictRe = regexp.MustCompile(`:\s*(accepted|rejected|accepted \(source|disabled)`)
var spctlSourceRe = regexp.MustCompile(`(?m)^\s*source=(.+)$`)
var spctlOriginRe = regexp.MustCompile(`(?m)^\s*origin=(.+)$`)

// spctlVerdict 从 spctl 输出取判定词；取不到就如实说"无法判定"。
func spctlVerdict(res *execx.Result) string {
	blob := res.Stdout + "\n" + res.Stderr
	if m := spctlVerdictRe.FindStringSubmatch(blob); len(m) == 2 {
		v := strings.TrimSpace(m[1])
		if strings.HasPrefix(v, "accepted") {
			return "accepted"
		}
		return v
	}
	if res.ExitCode == 0 {
		return "accepted"
	}
	if res.ExitCode == 3 {
		return "rejected"
	}
	return "unknown"
}

func spctlField(re *regexp.Regexp, res *execx.Result) string {
	blob := res.Stdout + "\n" + res.Stderr
	if m := re.FindStringSubmatch(blob); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ============================================================================
//  sec.verify —— 验签 + Gatekeeper 评估（只读）
// ============================================================================

type secVerify struct{}

func init() { Add(secVerify{}) }

func (secVerify) Meta() tool.Meta {
	_, hasCodesign := execx.LookPath("codesign")
	_, hasSpctl := execx.LookPath("spctl")
	ok := hasCodesign && hasSpctl
	reason := ""
	if !ok {
		reason = "缺少系统命令：codesign/spctl（需安装命令行工具）"
	}
	return tool.Meta{
		ID: "sec.verify", Name: "验签与 Gatekeeper", Category: "sec", Icon: "shield-check",
		Summary:   "查代码签名与 Gatekeeper 判定，未签名或被拒如实呈现。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "path", Label: "目标文件", Type: tool.TypePath, Required: true,
				Placeholder: "/Applications/某App.app", Help: "只能读允许的读根内的文件或 App。"},
		},
	}
}

func (secVerify) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	target := in.Path("path")
	c.Logf("验签：%s", redactHome(c, target))
	c.Progress(20, "读取代码签名")

	sign := c.Exec.Run(ctx, 30*time.Second, "codesign", "-dv", "--verbose=4", target)
	if sign.TimedOut {
		return nil, fmt.Errorf("codesign 超过 30 秒未返回，已终止")
	}
	if sign.ExitCode != 0 && !isUnsignedCodesign(sign) {
		return nil, fmt.Errorf("codesign 执行失败（退出码 %d）：%s", sign.ExitCode, redactHome(c, failureReason(sign)))
	}

	kv := kvLines(sign.Stdout + "\n" + sign.Stderr)
	signed := !isUnsignedCodesign(sign) && sign.ExitCode == 0

	// spctl 的非 0 是**正常判定**（未签名/未公证会被拒），不是工具失败（坑 S1）。
	c.Progress(60, "Gatekeeper 评估")
	sp := c.Exec.Run(ctx, 30*time.Second, "spctl", "-a", "-vv", target)
	if sp.TimedOut {
		return nil, fmt.Errorf("spctl 超过 30 秒未返回，已终止")
	}
	verdict := spctlVerdict(sp)

	authorities := kv["Authority"]
	if authorities == nil {
		authorities = []string{}
	}
	teamID := firstKV(kv, "TeamIdentifier")
	flags := firstKV(kv, "CodeDirectory")
	flagField := firstKV(kv, "flags")
	adhoc := strings.Contains(flagField, "adhoc") || strings.Contains(flags, "adhoc")
	// 公证信号：codesign 的公证票据字段，或 Mach-O 公证运行时标志 0x10000。
	notarized := false
	notarizeNote := ""
	if len(kv["Notarization Ticket"]) > 0 || len(kv["notarized"]) > 0 {
		notarized = true
	} else if flagsHas(flags, 0x10000) {
		notarized = true
	} else {
		notarizeNote = "codesign 详情里没有公证票据字段，本结果不声称已公证"
	}

	data := map[string]any{
		"path":          target,
		"signed":        signed,
		"signer":        strings.Join(authorities, " / "),
		"authorities":   authorities,
		"team_id":       teamID,
		"identifier":    firstKV(kv, "Identifier"),
		"format":        firstKV(kv, "Format"),
		"cdhash":        firstKV(kv, "CDHash"),
		"flags":         flags,
		"ad_hoc":        adhoc,
		"notarized":     notarized,
		"notarize_hint": notarizeNote,
		"gatekeeper": map[string]any{
			"verdict": verdict,
			"source":  spctlField(spctlSourceRe, sp),
			"origin":  spctlField(spctlOriginRe, sp),
			"exit":    sp.ExitCode,
			"note":    "spctl 非 0 是正常判定（未签名/未公证会被拒），不是工具失败",
		},
		"codesign_exit": sign.ExitCode,
	}
	if sign.TruncOut || sign.TruncErr {
		data["truncated"] = "codesign 输出过长已截断"
	}

	switch {
	case !signed:
		return &tool.Result{OK: true, Msg: "该目标没有代码签名，Gatekeeper 判定：" + verdict, Data: data}, nil
	case verdict == "rejected":
		return &tool.Result{OK: true, Msg: "签名者：" + firstSigner(authorities) + "；Gatekeeper 拒绝", Data: data}, nil
	default:
		return &tool.Result{OK: true, Msg: "签名者：" + firstSigner(authorities) + "；Gatekeeper " + verdict, Data: data}, nil
	}
}

func firstSigner(auth []string) string {
	if len(auth) == 0 {
		return "未知"
	}
	return auth[0]
}

// ============================================================================
//  sec.certificates —— 本机钥匙串证书摘要（只读，绝不导私钥）
// ============================================================================

type secCertificates struct{}

func init() { Add(secCertificates{}) }

// maxCertificates 是单次返回的证书上限（本机常见 40+，全量返回前端会卡）。
const maxCertificates = 200

// certInfo 是一张证书的摘要（只读字段，绝不含私钥）。
type certInfo struct {
	CommonName  string `json:"common_name"`
	Subject     string `json:"subject"`
	Issuer      string `json:"issuer"`
	NotAfter    string `json:"not_after"`
	Expired     bool   `json:"expired"`
	SelfSigned  bool   `json:"self_signed"`
	IsCA        bool   `json:"is_ca"`
	CodeSigning bool   `json:"codesigning_capable"`
	SHA1        string `json:"sha1"`
	// pemCodeSigning 是扩展里带 Code Signing EKU 的结论（不是名字猜测，坑 S2）。
	pemCodeSigning bool
}

func (secCertificates) Meta() tool.Meta {
	_, ok := execx.LookPath("security")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/security"
	}
	return tool.Meta{
		ID: "sec.certificates", Name: "钥匙串证书摘要", Category: "sec", Icon: "certificate",
		Summary:   "列证书名/签发者/到期/能否签名，不读私钥。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "limit", Label: "最多返回", Type: tool.TypeNumber, Default: 40,
				Min: Num(1), Max: Num(float64(maxCertificates)),
				Help: "证书很多时只列前 N 张，避免界面卡住。"},
		},
	}
}

func (secCertificates) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	limit := in.Int("limit", 40)
	if limit <= 0 {
		limit = 40
	}
	c.Logf("读取钥匙串证书")
	c.Progress(20, "导出证书公钥")

	// -p 只导出 PEM 公钥证书；私钥绝不会出现在这里（本工具也不去读）。
	pemRes := c.Exec.Run(ctx, 30*time.Second, "security", "find-certificate", "-a", "-p")
	if pemRes.TimedOut {
		return nil, fmt.Errorf("security find-certificate 超过 30 秒未返回，已终止")
	}
	if pemRes.ExitCode != 0 {
		// 钥匙串为空时 security 会返回非 0，这是"空结果"，不是失败。
		if strings.TrimSpace(pemRes.Stdout) == "" {
			return &tool.Result{OK: true, Msg: "钥匙串里没有可读证书", Data: map[string]any{
				"certificates": []map[string]any{}, "total": 0,
				"note": "security 返回非 0 且无输出，按空钥匙串处理",
			}}, nil
		}
		return nil, fmt.Errorf("security find-certificate 失败（退出码 %d）：%s",
			pemRes.ExitCode, redactHome(c, failureReason(pemRes)))
	}

	c.Progress(55, "解析证书")
	out := []certInfo{}
	total := 0
	parseFailed := 0
	rest := []byte(pemRes.Stdout)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		total++
		crt, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			parseFailed++
			continue
		}
		sum := sha1.Sum(crt.Raw)
		ci := certInfo{
			CommonName:     crt.Subject.CommonName,
			Subject:        crt.Subject.String(),
			Issuer:         crt.Issuer.String(),
			NotAfter:       crt.NotAfter.Local().Format("2006-01-02 15:04"),
			Expired:        time.Now().After(crt.NotAfter),
			SelfSigned:     crt.CheckSignatureFrom(crt) == nil,
			IsCA:           crt.IsCA,
			SHA1:           strings.ToUpper(hex.EncodeToString(sum[:])),
			pemCodeSigning: hasCodeSigningEKU(crt),
		}
		out = append(out, ci)
	}

	// 可用签名身份 = 钥匙串里有对应私钥且未过期的身份（find-identity 的口径，实测匹配）。
	// 注意：find-identity 不带 -v 才算"全部身份"，这里只要"可用"的，故带 -v。
	ids := c.Exec.Run(ctx, 20*time.Second, "security", "find-identity", "-v", "-p", "codesigning")
	identitySHA1 := identitySet(ids.Stdout + "\n" + ids.Stderr)
	identityNote := ""
	if ids.ExitCode != 0 && len(identitySHA1) == 0 {
		identityNote = "find-identity 返回非 0，可用身份按 0 计：" + redactHome(c, firstLine(ids.Output()))
	}
	signingIdentities := len(identitySHA1)

	// code signing EKU 能签，但只有钥匙串里真有身份（含私钥）才算"可用于代码签名"。
	ekuCapable := 0
	for i := range out {
		if out[i].pemCodeSigning {
			ekuCapable++
		}
		out[i].CodeSigning = identitySHA1[out[i].SHA1]
	}

	// 按到期时间升序：快过期的排前面。
	sort.SliceStable(out, func(a, b int) bool { return out[a].NotAfter < out[b].NotAfter })

	show := out
	if len(show) > limit {
		show = show[:limit]
	}
	c.Progress(100, "完成")
	data := map[string]any{
		"certificates":        show,
		"returned":            len(show),
		"total":               total,
		"codesigning_capable": countCodeSigning(out),
		"codesigning_eku":     ekuCapable,
		"signing_identities":  signingIdentities,
		"signing_note":        "可用于代码签名 = 该证书在钥匙串里有对应身份（含私钥），本工具只报数量",
		"limit":               limit,
		"note":                "只列证书摘要（名称/签发者/到期/能否签名），不导出私钥、不读 .p12",
	}
	if parseFailed > 0 {
		data["parse_failed"] = parseFailed
	}
	if identityNote != "" {
		data["identity_note"] = identityNote
	}
	if pemRes.TruncOut {
		data["truncated"] = "证书数量超出单次输出上限，已截断"
	}
	msg := fmt.Sprintf("共 %d 张证书，返回 %d 张；可用代码签名身份 %d 个", total, len(show), signingIdentities)
	if total == 0 {
		msg = "钥匙串里没有可读证书"
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// hasCodeSigningEKU 读扩展里的用途：带 Code Signing EKU 才算可用于代码签名。
func hasCodeSigningEKU(crt *x509.Certificate) bool {
	for _, u := range crt.ExtKeyUsage {
		if u == x509.ExtKeyUsageCodeSigning {
			return true
		}
	}
	return false
}

func countCodeSigning(list []certInfo) int {
	n := 0
	for i := range list {
		if list[i].CodeSigning {
			n++
		}
	}
	return n
}

var identityLineRe = regexp.MustCompile(`^\s*\d+\)\s+([0-9A-Fa-f]+)\s+"(.*)"\s*$`)

// identitySet 解析 find-identity 输出：返回"身份 SHA1 → 真"（匹配用指纹，不用名字）。
func identitySet(blob string) map[string]bool {
	out := map[string]bool{}
	for _, ln := range strings.Split(blob, "\n") {
		if m := identityLineRe.FindStringSubmatch(ln); len(m) == 3 {
			out[strings.ToUpper(m[1])] = true
		}
	}
	return out
}

// ============================================================================
//  sec.pkg_info —— .pkg 签名与内容摘要（只读，不解包到系统位置）
// ============================================================================

type secPkgInfo struct{}

func init() { Add(secPkgInfo{}) }

func (secPkgInfo) Meta() tool.Meta {
	_, ok := execx.LookPath("pkgutil")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/sbin/pkgutil"
	}
	return tool.Meta{
		ID: "sec.pkg_info", Name: "安装包分析", Category: "sec", Icon: "package",
		Summary:   ".pkg 的签名状态、安装位置与文件条目摘要。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "path", Label: "安装包", Type: tool.TypePath, Required: true,
				Placeholder: "/Users/你/Downloads/a.pkg", Help: "只能读允许的读根内的 .pkg 文件。"},
			{Name: "preview", Label: "预览条目数", Type: tool.TypeNumber, Default: 20,
				Min: Num(1), Max: Num(200), Help: "只列前 N 条安装路径，不做解包。"},
		},
	}
}

func (secPkgInfo) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	pkg := in.Path("path")
	preview := in.Int("preview", 20)
	if preview <= 0 {
		preview = 20
	}
	data := map[string]any{"path": pkg}
	problems := []string{}

	c.Progress(15, "检查签名")
	sig := c.Exec.Run(ctx, 60*time.Second, "pkgutil", "--check-signature", pkg)
	if sig.TimedOut {
		problems = append(problems, "pkgutil --check-signature 超时被终止")
	} else {
		blob := sig.Stdout + "\n" + sig.Stderr
		status := ""
		if m := regexp.MustCompile(`(?m)^\s*Status:\s*(.+)$`).FindStringSubmatch(blob); len(m) == 2 {
			status = strings.TrimSpace(m[1])
		}
		data["signature"] = map[string]any{
			"status": status,
			"exit":   sig.ExitCode,
			"note":   "pkgutil 对未签名 pkg 返回非 0 是正常判定，不是工具失败",
		}
		if chains := regexp.MustCompile(`(?m)^\s*\d+\.\s+(.+)$`).FindAllStringSubmatch(blob, 8); len(chains) > 0 {
			names := []string{}
			for _, m := range chains {
				names = append(names, strings.TrimSpace(m[1]))
			}
			data["certificate_chain"] = names
		}
		if status == "" && sig.ExitCode != 0 {
			problems = append(problems, "签名状态读取失败："+redactHome(c, firstLine(sig.Output())))
		}
	}

	c.Progress(45, "读取安装信息")
	info := c.Exec.Run(ctx, 60*time.Second, "pkgutil", "--pkg-info", pkg)
	if info.TimedOut {
		problems = append(problems, "pkgutil --pkg-info 超时被终止")
	} else if info.ExitCode != 0 {
		problems = append(problems, "安装信息读取失败："+redactHome(c, firstLine(info.Output())))
	} else {
		kv := map[string]string{}
		for _, ln := range strings.Split(info.Stdout, "\n") {
			if i := strings.Index(ln, ": "); i > 0 {
				kv[strings.TrimSpace(ln[:i])] = strings.TrimSpace(ln[i+2:])
			}
		}
		data["package"] = map[string]any{
			"identifier":       kv["package-id"],
			"version":          kv["version"],
			"install_location": kv["install-location"],
			"volume":           kv["volume"],
		}
	}

	c.Progress(75, "统计文件条目")
	files := c.Exec.Run(ctx, 120*time.Second, "pkgutil", "--payload-files", pkg)
	entryCount := 0
	previewList := []string{}
	if files.TimedOut {
		problems = append(problems, "pkgutil --payload-files 超时被终止")
	} else if files.ExitCode != 0 {
		problems = append(problems, "文件条目读取失败："+redactHome(c, firstLine(files.Output())))
	} else {
		for _, ln := range strings.Split(files.Stdout, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			entryCount++
			if len(previewList) < preview {
				previewList = append(previewList, ln)
			}
		}
		if entryCount == 0 {
			data["payload_note"] = "该 pkg 没有 payload 条目（可能是分发脚本包）"
		}
	}
	data["payload"] = map[string]any{
		"entry_count": entryCount,
		"preview":     previewList,
		"preview_n":   len(previewList),
	}
	if len(problems) > 0 {
		data["unavailable"] = problems
	}
	msg := fmt.Sprintf("已读取：%d 个文件条目", entryCount)
	if len(problems) > 0 {
		msg = fmt.Sprintf("部分信息不可用（%d 项）", len(problems))
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// ============================================================================
//  sec.gatekeeper_status —— 本机安全开关一览（只读）
// ============================================================================

type secGatekeeperStatus struct{}

func init() { Add(secGatekeeperStatus{}) }

func (secGatekeeperStatus) Meta() tool.Meta {
	_, hasSpctl := execx.LookPath("spctl")
	ok := hasSpctl
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/sbin/spctl"
	}
	return tool.Meta{
		ID: "sec.gatekeeper_status", Name: "安全开关状态", Category: "sec", Icon: "lock",
		Summary:   "Gatekeeper、SIP 与开发工具的开关摘要。",
		Async:     true,
		Available: ok, UnavailableReason: reason,
		Params: []tool.Param{},
	}
}

func (secGatekeeperStatus) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	data := map[string]any{}
	problems := []string{}

	c.Progress(20, "读 Gatekeeper")
	sp := c.Exec.Run(ctx, 20*time.Second, "spctl", "--status")
	if sp.TimedOut {
		problems = append(problems, "spctl --status 超时被终止")
	} else {
		out := strings.TrimSpace(sp.Stdout + "\n" + sp.Stderr)
		data["gatekeeper"] = map[string]any{
			"raw": out, "enabled": strings.Contains(strings.ToLower(out), "enabled"),
			"exit": sp.ExitCode,
		}
	}

	c.Progress(45, "读 SIP")
	csr := c.Exec.Run(ctx, 20*time.Second, "csrutil", "status")
	if csr.TimedOut {
		problems = append(problems, "csrutil status 超时被终止")
	} else if csr.ExitCode != 0 && strings.TrimSpace(csr.Stdout) == "" {
		problems = append(problems, "csrutil status 失败："+redactHome(c, firstLine(csr.Output())))
	} else {
		out := strings.TrimSpace(csr.Stdout + "\n" + csr.Stderr)
		data["sip"] = map[string]any{
			"raw": out,
			"enabled": strings.Contains(out, "enabled") &&
				!strings.Contains(out, "disabled"),
		}
	}

	c.Progress(80, "读开发工具")
	dev := c.Exec.Run(ctx, 60*time.Second, "system_profiler", "SPDeveloperToolsDataType")
	if dev.TimedOut {
		problems = append(problems, "system_profiler 超时被终止")
	} else {
		lines := []string{}
		for _, ln := range strings.Split(dev.Stdout, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			lines = append(lines, ln)
			if len(lines) >= 12 {
				break
			}
		}
		entry := map[string]any{"exit": dev.ExitCode, "detail": lines}
		if len(lines) == 0 {
			entry["note"] = "system_profiler 没有返回开发工具信息（可能未安装命令行工具）"
		}
		data["developer_tools"] = entry
	}

	if len(problems) > 0 {
		data["unavailable"] = problems
	}
	msg := "已读取 Gatekeeper / SIP / 开发工具状态"
	if len(problems) > 0 {
		msg = fmt.Sprintf("%d 项未能读取", len(problems))
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}
