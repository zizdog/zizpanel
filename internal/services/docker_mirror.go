package services

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Docker 镜像加速源（registry mirror）
//
//  为什么要做（用户原话："现在的拉取太慢了，没法用"）：这台机器上
//  `registry-1.docker.io` **直连超时**（实测 6 秒无响应），所以从 Docker Hub
//  拉任何镜像都可能卡到超时。解决方式是给 docker 守护进程配 registry-mirrors。
//
//  两条纪律：
//   1. **内置列表必须是实测过的**。国内公开镜像站这几年死了很多
//      （ustc / 163 / 百度 / kubesre / dockerpull 实测全部超时），
//      把死链接列进"常用"等于让用户白等 —— 所以每条都标注实测结论，
//      并且提供"检测可用性"按钮让用户在**自己的网络**上再验一次。
//   2. **判定要看真实回包**。registry 的 `/v2/` 返回 **401 才是正常的**
//      （表示"我是 registry，需要鉴权"），200 也可以；403/302/超时都不是好事。
//      只看"能不能连上"会把死站判成可用。
//
//  配置写在哪：Colima 的 `~/.colima/<profile>/colima.yaml` 里的 `docker:`
//  段 —— 那是**权威来源**，colima 每次 start 都会用它重新生成 VM 内的
//  /etc/docker/daemon.json。只改 VM 内的 daemon.json 会被下次启动覆盖掉。
// ============================================================================

// DockerMirror 是一个候选加速源。
type DockerMirror struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	// Note 是实测结论/注意事项（内置列表里逐条写清楚，别让用户猜）
	Note string `json:"note,omitempty"`
	// Rank 是**实测能力等级**，越小越优先；它是自动配置时的排序主键，
	// 延迟只是次键。为什么不能只看延迟：延迟只反映 `/v2/` 应答快慢，
	// **完全不能说明它能不能把层数据拉下来**（2026-09-16 实测）：
	//
	//	0 = 清单 + 层数据都能拉（含社区镜像）→ 可以放在第一位
	//	1 = 只服务白名单/官方镜像：社区镜像会 403，dockerd 会换下一个源
	//	2 = **不能写进自动配置**：清单能取，但层数据要么 404 要么直接挂住。
	//	    关键原因：docker **只对 manifest 做多源回落**，层数据一旦选定
	//	    这个源就只会失败/卡死，不会自动换源 —— 把它写进去等于给每次
	//	    拉取埋一个坑。
	Rank int `json:"rank,omitempty"`
}

// DockerMirrorRankUnusable 是"实测不能进自动配置"的 Rank。
const DockerMirrorRankUnusable = 2

// BuiltinDockerMirrors 是内置候选。
//
// 顺序 = 界面顺序，也是**自动配置时的优先顺序**（Rank 相同时再按延迟排）。
// 注释里的结论来自 2026-09-16 两台机器的实测（curl `<url>/v2/` +
// 从 Colima 虚拟机里真拉镜像）；**每条都标了 Rank，改之前请先复测**。
var BuiltinDockerMirrors = []DockerMirror{
	{URL: "https://dockerproxy.net", Name: "dockerproxy.net", Rank: 0, Note: "实测可用作镜像：社区镜像也整层拉得下来（pjmeca/squoosh:1.1.0 40 秒），uptime-kuma 145.8MiB 50 秒 —— 当前唯一验证过「零鉴权 + 层数据可下」的源"},
	{URL: "https://docker.m.daocloud.io", Name: "DaoCloud", Rank: 1, Note: "两台机器实测 401/约 130ms、官方镜像很快（alpine 10 秒）；但**按白名单拒绝部分社区镜像**（实测 pjmeca/squoosh 返回 403 DENIED），此时 docker 会换下一个源"},
	{URL: "https://docker.1panel.live", Name: "1Panel", Rank: DockerMirrorRankUnusable, Note: "实测 200/约 0.8-2.4s，但**docker 客户端会挂住**（清单之后层数据 300 秒 0 进度）—— 不进自动配置，仅作手工备用"},
	{URL: "https://docker.1ms.run", Name: "1ms.run", Rank: DockerMirrorRankUnusable, Note: "实测 401/约 200ms 能取清单，但**层数据取不到**（registry 侧一律 BLOB_UNKNOWN/404）—— 不进自动配置"},
	{URL: "https://docker.aityp.com", Name: "aityp", Rank: 1, Note: "实测 401，但延迟波动大（0.25-2s）"},
	{URL: "https://hub.rat.dev", Name: "rat.dev", Rank: 1, Note: "实测 302 跳转，能不能拉取决于跳转目标"},
	{URL: "https://docker.nju.edu.cn", Name: "南京大学", Rank: 1, Note: "实测 403（限制来源），校园网外基本不可用"},
	{URL: "https://docker.mirrors.ustc.edu.cn", Name: "中科大", Rank: 1, Note: "实测超时（该站已停止对外服务）"},
	{URL: "https://hub-mirror.c.163.com", Name: "网易 163", Rank: 1, Note: "实测超时（已停止服务）"},
	{URL: "https://mirror.baidubce.com", Name: "百度云", Rank: 1, Note: "实测超时（已停止服务）"},
	{URL: "https://docker.kubesre.xyz", Name: "kubesre", Rank: 1, Note: "实测超时"},
	{URL: "https://dockerpull.org", Name: "dockerpull.org", Rank: 1, Note: "实测超时"},
	{URL: "https://registry.dockermirror.com", Name: "dockermirror", Rank: 1, Note: "实测 525（TLS 握手失败）"},
}

// DockerMirrorProbe 是一条探测结果。
type DockerMirrorProbe struct {
	DockerMirror
	// OK 表示"这个地址现在像个能用的 registry"
	OK bool `json:"ok"`
	// Status 是 /v2/ 的 HTTP 状态码（0 = 没拿到响应）
	Status int `json:"status"`
	// LatencyMs 是到首个响应的毫秒数
	LatencyMs int `json:"latency_ms"`
	// Detail 是人话结论（"可用"/"需要鉴权（正常）"/"被拒绝（403）"/"超时"…）
	Detail string `json:"detail"`
}

// DockerMirrorState 是加速源的完整现状，直接喂给界面。
type DockerMirrorState struct {
	// Runtime 是识别出来的运行时（colima / docker-desktop / 其它）
	Runtime string `json:"runtime"`
	// ConfigPath 是加速源真正写在哪
	ConfigPath string `json:"config_path"`
	// Supported 表示面板能不能替用户改（不支持的运行时只读展示）
	Supported bool `json:"supported"`
	// Configured 是配置文件里写的列表
	Configured []string `json:"configured"`
	// Effective 是**守护进程当前实际生效**的列表（来自 docker info）
	Effective []string `json:"effective"`
	// NeedRestart 为真表示"配置与生效不一致，需要重启运行时"
	NeedRestart bool `json:"need_restart"`
	// HubDirect 是 Docker Hub 官方地址的探测结果（用户抱怨的"慢"就是它）
	HubDirect *DockerMirrorProbe `json:"hub_direct,omitempty"`
	// Note 是给用户的说明（例如"这台机器的运行时面板改不了"）
	Note string `json:"note,omitempty"`
}

// ---------- 探测 ----------

// ProbeDockerMirrors 并发探测一组地址。
//
// 判定依据（真机实测得出）：
//
//	200        → 可用（有的镜像站直接放行 /v2/）
//	401        → **可用**：registry 要求鉴权，这是它"活着且是 registry"的证据
//	403        → 被拒绝：站点在，但不给这个来源用（校园网限制等）
//	3xx        → 跳转：能不能用取决于跳到哪里，如实标出来
//	其它/超时  → 不可用
func (m *Manager) ProbeDockerMirrors(ctx context.Context, urls []string) []DockerMirrorProbe {
	out := make([]DockerMirrorProbe, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		out[i] = DockerMirrorProbe{DockerMirror: DockerMirror{URL: u}}
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			out[i] = probeOneMirror(ctx, u)
		}(i, u)
	}
	wg.Wait()
	return out
}

func probeOneMirror(ctx context.Context, raw string) DockerMirrorProbe {
	res := DockerMirrorProbe{DockerMirror: DockerMirror{URL: raw}}
	u := strings.TrimRight(raw, "/") + "/v2/"
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		res.Detail = "地址不合法：" + err.Error()
		return res
	}
	// 有些镜像站对没有 UA 的请求直接拒绝
	req.Header.Set("User-Agent", "zizpanel-mirror-probe/1")
	// 不要跟随跳转：302 本身就是判定依据之一
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Detail = "连不上：" + shortErr(err)
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	res.Status = resp.StatusCode
	switch {
	case resp.StatusCode == http.StatusOK:
		res.OK = true
		res.Detail = "可用"
	case resp.StatusCode == http.StatusUnauthorized:
		res.OK = true
		res.Detail = "可用（需要鉴权，属正常）"
	case resp.StatusCode == http.StatusForbidden:
		res.Detail = "被拒绝（403）：站点在，但不给这个来源用"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		res.Detail = fmt.Sprintf("跳转（%d）到 %s", resp.StatusCode, resp.Header.Get("Location"))
	default:
		res.Detail = fmt.Sprintf("异常状态 %d", resp.StatusCode)
	}
	return res
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && len(s)-i > 3 {
		return s[i+2:]
	}
	return s
}

// ---------- 现状 ----------

// DockerMirrorStatus 汇总"配置里写的 / 守护进程实际生效的 / 运行时是谁"。
func (m *Manager) DockerMirrorStatus(ctx context.Context) DockerMirrorState {
	st := DockerMirrorState{Runtime: "unknown"}
	runtime, cfgPath, supported, note := m.dockerMirrorTarget()
	st.Runtime, st.ConfigPath, st.Supported, st.Note = runtime, cfgPath, supported, note
	if cfgPath != "" {
		st.Configured = m.readConfiguredMirrors(cfgPath, runtime)
	}
	st.Effective = m.effectiveMirrors(ctx)
	// 顺序无关，按集合比较 —— 否则"只是顺序不同"也会被说成需要重启
	st.NeedRestart = !sameStringSet(st.Configured, st.Effective)
	if p := probeOneMirror(ctx, "https://registry-1.docker.io"); true {
		st.HubDirect = &p
	}
	return st
}

// dockerMirrorTarget 判断这台机器用的是哪个 Docker 运行时，以及配置该写在哪。
//
// 只对**能验证**的运行时动手：Colima 是面板自己装的（两台机器都在用），
// 它的配置格式与重启方式都实测过。其它运行时如实告诉用户去哪改，
// 而不是瞎写一个我们没验过的文件。
func (m *Manager) dockerMirrorTarget() (runtime, cfgPath string, supported bool, note string) {
	home := m.opt.UserHome
	if home == "" {
		home = "/Users/" + m.opt.UserName
	}
	sock := m.opt.DockerSocket
	switch {
	case strings.Contains(sock, ".colima") || dirExists(filepath.Join(home, ".colima")):
		profile := "default"
		// socket 形如 ~/.colima/<profile>/docker.sock
		parts := strings.Split(sock, string(filepath.Separator))
		for i, p := range parts {
			if p == ".colima" && i+1 < len(parts) {
				profile = parts[i+1]
				break
			}
		}
		p := filepath.Join(home, ".colima", profile, "colima.yaml")
		if _, err := os.Stat(p); err == nil {
			return "colima", p, true, "Colima（面板自己装的那个）：改 colima.yaml 并重启运行时即可生效"
		}
		return "colima", p, false, "找到 Colima 目录但读不到 colima.yaml（" + p + "），请检查是否安装完整"
	case strings.Contains(sock, ".orbstack") || dirExists(filepath.Join(home, ".orbstack")):
		return "orbstack", filepath.Join(home, ".orbstack", "config", "docker.json"), false,
			"这台机器用的是 OrbStack：面板暂不支持自动改写它的配置，请在 OrbStack 设置里加 registry mirror"
	case dirExists(filepath.Join(home, ".docker")):
		return "docker-desktop", filepath.Join(home, ".docker", "daemon.json"), false,
			"这台机器用的是 Docker Desktop：面板暂不支持自动改写它的配置，请在 Docker Desktop → Settings → Docker Engine 里加 registry-mirrors"
	default:
		return "unknown", "", false, "没识别出 Docker 运行时；请手工给 docker 守护进程配置 registry-mirrors"
	}
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// configuredMirrorsKey 是 colima.yaml 里 registry mirror 的键名（docker 守护进程配置格式）
const configuredMirrorsKey = "registry-mirrors"

// readConfiguredMirrors 从配置文件里读出已写的 mirror 列表（尽力而为的解析）。
func (m *Manager) readConfiguredMirrors(path, runtime string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if runtime != "colima" {
		return nil
	}
	return parseColimaMirrors(string(b))
}

// parseColimaMirrors 解析 colima.yaml 里 `docker:` 段的 registry-mirrors。
//
// 为什么手写解析而不是引 YAML 库：这个文件是 Colima 自己生成的、结构极浅
// （顶层键 + 缩进子键），为了读一个字符串列表把 YAML 依赖引进面板不划算。
// 解析规则与写回规则（upsertColimaMirrors）严格配对，并由单测锁住来回一致。
func parseColimaMirrors(text string) []string {
	lines := strings.Split(text, "\n")
	inDocker := false
	inList := false
	baseIndent := -1
	var out []string
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		if !inDocker {
			if strings.HasPrefix(trimmed, "docker:") {
				inDocker = true
				baseIndent = indent
				// `docker: {}` 这种写法表示空配置
				if strings.Contains(trimmed, "{}") {
					return nil
				}
			}
			continue
		}
		// docker 段结束：出现不更深缩进的顶层键
		if trimmed != "" && indent <= baseIndent && !strings.HasPrefix(trimmed, "#") {
			break
		}
		if strings.HasPrefix(trimmed, configuredMirrorsKey+":") {
			inList = true
			// 也支持 `registry-mirrors: ["https://a"]` 这种行内写法
			if i := strings.Index(trimmed, "["); i >= 0 {
				for _, part := range strings.Split(strings.Trim(trimmed[i:], "[]"), ",") {
					v := strings.Trim(strings.TrimSpace(part), `"'`)
					if v != "" {
						out = append(out, v)
					}
				}
				inList = false
			}
			continue
		}
		if inList {
			if strings.HasPrefix(trimmed, "-") {
				v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), `"'`)
				if v != "" {
					out = append(out, v)
				}
				continue
			}
			// 列表结束（遇到同级或更浅的键，或空行后跟别的键）
			if trimmed != "" {
				inList = false
			}
		}
	}
	return out
}

// effectiveMirrors 问守护进程当前真正生效的 mirror（dokcer info）。
func (m *Manager) effectiveMirrors(ctx context.Context) []string {
	out, err := m.DockerInfoCmd(ctx) // docker info --format '{{json .RegistryConfig.Mirrors}}'
	if err != nil {
		return nil
	}
	return parseJSONStringArray(out)
}

func parseJSONStringArray(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		v := strings.Trim(strings.TrimSpace(part), `"'`)
		if v != "" && v != "null" {
			out = append(out, v)
		}
	}
	return out
}

// sameStringSet 比较两个列表是否是同一个集合（忽略顺序与重复）。
func sameStringSet(a, b []string) bool {
	set := func(in []string) map[string]bool {
		m := map[string]bool{}
		for _, v := range in {
			m[strings.TrimRight(strings.TrimSpace(v), "/")] = true
		}
		return m
	}
	ma, mb := set(a), set(b)
	if len(ma) != len(mb) {
		return false
	}
	for k := range ma {
		if !mb[k] {
			return false
		}
	}
	return true
}

// ---------- 写入 ----------

// ValidateMirrorURL 校验用户填/内置的 mirror 地址。
func ValidateMirrorURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("地址不合法：%v", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return "", fmt.Errorf("缺少主机名")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("地址里不该带查询参数或片段")
	}
	return strings.TrimRight(raw, "/"), nil
}

// SetDockerMirrors 写入加速源并重启运行时（这是**长任务**：colima restart 约 30-60 秒）。
//
// 顺序：备份配置 → 改写 → 重启 → 复核守护进程真的读到了。
// 复核不过就报错（把备份路径告诉用户），而不是"写进去了就算成功"。
func (m *Manager) SetDockerMirrors(ctx context.Context, mirrors []string) error {
	_, cfgPath, supported, note := m.dockerMirrorTarget()
	if !supported {
		return fmt.Errorf("这台机器不能自动配置加速源：%s", note)
	}
	clean := make([]string, 0, len(mirrors))
	for _, raw := range mirrors {
		v, err := ValidateMirrorURL(raw)
		if err != nil {
			return fmt.Errorf("%s：%v", raw, err)
		}
		clean = append(clean, v)
	}

	// **顺序很重要**：docker 只用**第一个能应答**的镜像源，不会因为某个源慢
	// 而自动换下一个。所以这里先实测一遍，按（可用优先、延迟升序）排序再写。
	// 真机依据（2026-09-14）：按字母序把 docker.1ms.run 排在第一个时，
	// 拉 alpine/busybox/redis 分别要 30s/18s/22s；换成实测最快的源打头后明显更快。
	emit(ctx, tasks.LevelStep, fmt.Sprintf("正在实测这 %d 个源，按延迟排序（顺序决定 docker 用哪个）…", len(clean)))
	probes := m.ProbeDockerMirrors(ctx, clean)
	byURL := map[string]DockerMirrorProbe{}
	for _, p := range probes {
		byURL[p.URL] = p
	}
	sort.SliceStable(clean, func(i, j int) bool {
		a, b := byURL[clean[i]], byURL[clean[j]]
		if a.OK != b.OK {
			return a.OK // 可用的排前面
		}
		// 主键是实测能力等级（见 DockerMirror.Rank）：延迟只说明 /v2/ 应答快，
		// 不代表层数据拉得下来。用户手填的地址按 Rank 1 参与排序。
		ra, rb := DockerMirrorRank(clean[i]), DockerMirrorRank(clean[j])
		if ra != rb {
			return ra < rb
		}
		if a.LatencyMs != b.LatencyMs {
			return a.LatencyMs < b.LatencyMs
		}
		return clean[i] < clean[j]
	})
	for _, u := range clean {
		p := byURL[u]
		emit(ctx, tasks.LevelOut, fmt.Sprintf("  %s  %s  %dms", u, map[bool]string{true: "可用", false: "不可用"}[p.OK], p.LatencyMs))
	}

	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%w", cfgPath, err)
	}
	backup := cfgPath + ".zizpanel.bak"
	if _, err := os.Stat(backup); err != nil {
		if err := os.WriteFile(backup, b, 0o644); err != nil {
			return fmt.Errorf("写备份失败：%w", err)
		}
		emit(ctx, tasks.LevelStep, "已备份原配置到 "+backup)
	}
	// 用 setColimaDockerOption 而不是 upsertColimaMirrors：后者会把整个
	// `docker:` 段重写成只剩 registry-mirrors，把用户自己配的
	// insecure-registries / features 无声抹掉（见 docker_mirror_nas.go 的说明）。
	updated := setColimaDockerOption(string(b), configuredMirrorsKey, clean)
	var insecure []string
	for _, mir := range clean {
		if h := insecureRegistryHostFor(mir); h != "" {
			insecure = append(insecure, h)
		}
	}
	if len(insecure) > 0 {
		updated = setColimaDockerOption(updated, "insecure-registries", insecure)
	}
	if err := os.WriteFile(cfgPath, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", cfgPath, err)
	}
	if len(clean) == 0 {
		emit(ctx, tasks.LevelStep, "已清空加速源列表")
	} else {
		emit(ctx, tasks.LevelStep, fmt.Sprintf("已写入 %d 个加速源到 %s", len(clean), cfgPath))
	}

	// 重启运行时让配置生效（Colima 会在 start 时把 docker: 段写进 VM 内的 daemon.json）
	emit(ctx, tasks.LevelStep, "正在重启 Docker 运行时（colima restart，约 30-60 秒）…")
	if out, err := m.runAsUser(ctx, 3*time.Minute, m.colimaBin(), "restart"); err != nil {
		return fmt.Errorf("colima restart 失败：%v（%s）—— 配置已写入，可执行 `colima restart` 手工生效；"+
			"要回退请把 %s 覆盖回 %s", err, tailText(out, 300), cfgPath, backup)
	}

	// 复核：问守护进程实际生效的列表
	emit(ctx, tasks.LevelStep, "复核守护进程实际生效的列表…")
	deadline := time.Now().Add(60 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = m.effectiveMirrors(ctx); len(got) > 0 || len(clean) == 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !sameStringSet(got, clean) {
		return fmt.Errorf("重启后守护进程生效的列表与配置不一致：期望 %v，实际 %v。"+
			"（配置已写入 %s；要回退请把 %s 覆盖回去）", clean, got, cfgPath, backup)
	}
	if len(got) == 0 {
		emit(ctx, tasks.LevelOK, "已清空加速源（docker info 里 Registry Mirrors 为空）")
	} else {
		emit(ctx, tasks.LevelOK, "加速源已生效："+strings.Join(got, "、"))
	}
	return m.verifyPull(ctx)
}

// verifyPull 用一次真实的小镜像拉取证明"现在真的能拉动了"。
//
// 为什么要这一步：配置生效 ≠ 拉得动（镜像站可能只代理一部分镜像）。
// 用户抱怨的正是"拉取太慢/没法用"，所以要用**真实拉取**来收尾，
// 并把耗时写进日志 —— 这才是用户能感知的证据。
// 失败只记为警告，不让整个任务失败：有些加速源不代理 hello-world 这类镜像，
// 而那不代表它不能用。
func (m *Manager) verifyPull(ctx context.Context) error {
	emit(ctx, tasks.LevelStep, "实测拉一个小镜像（hello-world）看是否真的拉得动…")
	start := time.Now()
	out, err := m.DockerRun(ctx, 90*time.Second, "pull", "hello-world")
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		{
			emit(ctx, tasks.LevelWarn, fmt.Sprintf("拉取测试没成功（用时 %s）：%s。"+
				"这不代表加速源不能用（有些镜像站不代理 hello-world），但建议你用真正要用的镜像再试一次",
				elapsed, tailText(out, 200)))
		}
		return nil
	}
	emit(ctx, tasks.LevelOK, fmt.Sprintf("拉取测试通过：hello-world 用时 %s", elapsed))
	return nil
}

// upsertColimaMirrors 把 registry-mirrors 写进 colima.yaml 的 docker: 段。
//
// 保留文件里**其它所有内容**（注释、架构、cpu、memory…）：这个文件是
// Colima 自己生成的，重写整个文件等于把它的默认值也一起改掉。
// 与 parseColimaMirrors 严格配对，来回一致由单测锁住。
func upsertColimaMirrors(text string, mirrors []string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines)+len(mirrors)+4)
	replaced := false
	i := 0
	for i < len(lines) {
		ln := lines[i]
		trimmed := strings.TrimSpace(ln)
		if !replaced && strings.HasPrefix(trimmed, "docker:") {
			indent := ln[:len(ln)-len(strings.TrimLeft(ln, " "))]
			// 收集并跳过 docker 段的原有内容
			j := i + 1
			for j < len(lines) {
				t := strings.TrimSpace(lines[j])
				ind := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
				if t != "" && ind <= len(indent) {
					break
				}
				j++
			}
			// 写新块
			if len(mirrors) == 0 {
				out = append(out, indent+"docker: {}")
			} else {
				out = append(out, indent+"docker:")
				out = append(out, indent+"  "+configuredMirrorsKey+":")
				for _, mir := range mirrors {
					out = append(out, indent+"  - "+mir)
				}
			}
			i = j
			replaced = true
			continue
		}
		out = append(out, ln)
		i++
	}
	if !replaced {
		// 文件里没有 docker: 段（极老/极新的 Colima 版本）→ 追加到末尾
		if len(mirrors) > 0 {
			out = append(out, "docker:")
			out = append(out, "  "+configuredMirrorsKey+":")
			for _, mir := range mirrors {
				out = append(out, "  - "+mir)
			}
		}
	}
	res := strings.Join(out, "\n")
	if !strings.HasSuffix(res, "\n") {
		res += "\n"
	}
	return res
}

// DockerRun 调 docker CLI。
//
// 面板以 root 运行时，docker CLI 需要显式拿到 socket 与 PATH
// （macOS 上 docker 在 /opt/homebrew/bin，且 socket 可能不是默认路径）。
func (m *Manager) DockerRun(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	env := []string{"PATH=" + m.dockerPath()}
	if m.opt.DockerSocket != "" {
		env = append(env, "DOCKER_HOST=unix://"+m.opt.DockerSocket)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.dockerBin(), args...)
	cmd.Env = append(os.Environ(), env...)
	return streamCmd(ctx, cmd)
}

// dockerBin 找 docker CLI（与其它地方一样显式用绝对路径，
// 因为 launchd 起的进程 PATH 很短）。
func (m *Manager) dockerBin() string {
	for _, p := range []string{"/opt/homebrew/bin/docker", "/usr/local/bin/docker", "/usr/bin/docker"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "docker"
}

func (m *Manager) dockerPath() string {
	return "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

// DockerInfoCmd 取守护进程的 Registry Mirrors（JSON 数组字符串）。
func (m *Manager) DockerInfoCmd(ctx context.Context) (string, error) {
	return m.DockerRun(ctx, 20*time.Second, "info", "--format", "{{json .RegistryConfig.Mirrors}}")
}
