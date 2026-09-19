package services

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Docker 加速源：**多个公网候选 + 自动配 + 逐条回落**（P0-B / D18）
//
//  为什么必须自动配：`SetDockerMirrors` 过去只在用户点"保存"时调用，
//  装 Docker 运行时与装 compose 应用都不写 registry-mirrors —— 新机器上
//  守护进程一个加速源都没有，而 `registry-1.docker.io` 在国内**直连超时**
//  （实测 6 秒无响应、报错里的 IP 是 DNS 污染结果）→ 9 个 Docker 应用全拉不动。
//
//  为什么必须是**多个**：docker 只用"第一个能应答的"源；而没有任何一个源
//  是全的 —— 实测 `docker.m.daocloud.io` **拒绝 pjmeca/squoosh**（allowlist），
//  而 `docker.1panel.live` / `dockerproxy.net` 能服务它。只配一个源 =
//  换一个镜像就装不上。所以这里配一整个**有序列表**，让 docker 自己逐条回落。
//
//  ⚠️ 没有"自建镜像站 /docker"候选：公网镜像站**不提供** `/docker/` 路径
//  （2026-09-20 用户明确）。把 404 端点排在第一位更糟 —— docker 只对 manifest
//  做多源回落，层数据阶段选错源会直接卡死/失败。加速源只用公网候选。
// ============================================================================

// DockerMirrorCandidates 返回界面上要展示的候选列表（全部是公网候选）。
func (m *Manager) DockerMirrorCandidates() []DockerMirror {
	out := make([]DockerMirror, 0, len(BuiltinDockerMirrors))
	return append(out, BuiltinDockerMirrors...)
}

// preferredDockerMirrors 探测全部候选，返回**可用的有序列表**。
//
// 全部候选都是公网源（见文件头：镜像站不提供 /docker/）。
// 排序主键是**实测能力等级**，延迟只是次键 —— 见下面 sort 的注释。
func (m *Manager) preferredDockerMirrors(ctx context.Context) ([]string, []DockerMirrorProbe) {
	cands := m.DockerMirrorCandidates()
	urls := make([]string, 0, len(cands))
	for _, c := range cands {
		urls = append(urls, c.URL)
	}
	probes := m.ProbeDockerMirrors(ctx, urls)
	ok := make([]DockerMirrorProbe, 0, len(probes))
	for _, p := range probes {
		if !p.OK {
			continue
		}
		if DockerMirrorRank(p.URL) >= DockerMirrorRankUnusable {
			continue // 实测"清单能取但层数据不可靠"的源不进自动配置
		}
		ok = append(ok, p)
	}
	// 排序主键是**实测能力等级**，延迟只是次键。
	// 为什么不用延迟当主键：延迟只说明 `/v2/` 应答快，完全不说明能不能把
	// 层数据拉下来（实测 daocloud 130ms 会 403 拒绝社区镜像；dockerproxy.net
	// 690ms 却能整层拉下来）。而 docker **只对 manifest 做多源回落**，
	// 层数据一旦选错源就只会失败/卡死 —— 所以"能力"比"快"重要。
	sort.SliceStable(ok, func(i, j int) bool {
		ri, rj := DockerMirrorRank(ok[i].URL), DockerMirrorRank(ok[j].URL)
		if ri != rj {
			return ri < rj
		}
		if ok[i].LatencyMs != ok[j].LatencyMs {
			return ok[i].LatencyMs < ok[j].LatencyMs
		}
		return ok[i].URL < ok[j].URL
	})
	ordered := make([]string, 0, len(ok))
	for _, p := range ok {
		ordered = append(ordered, p.URL)
	}
	return ordered, probes
}

// DockerMirrorRank 返回某个地址的实测能力等级；不在内置列表里的（用户手填的）
// 按 1 处理 —— 既不当成"已验证最好"，也不排除。
func DockerMirrorRank(url string) int {
	u := strings.TrimRight(strings.TrimSpace(url), "/")
	for _, m := range BuiltinDockerMirrors {
		if strings.TrimRight(m.URL, "/") == u {
			return m.Rank
		}
	}
	return 1
}

// ensureColimaConfigFile 保证 colima.yaml 存在（不存在时写一份最小可用的）。
//
// 为什么要自己创建：`colima.yaml` 是 `colima start` 第一次跑时才生成的，
// 而加速源与挂载都必须在**首次 start 之前**就写进去，否则第一次拉取
// 依然没有加速源、第一次启动依然没有挂载表。
// 只写必须的几项，其余交给 colima 的默认值与 CLI 默认值。
func (m *Manager) ensureColimaConfigFile() (string, error) {
	cfg := m.colimaYAMLPath()
	if cfg == "" {
		return "", fmt.Errorf("未知用户家目录，无法定位 colima.yaml")
	}
	if _, err := os.Stat(cfg); err == nil {
		return cfg, nil
	}
	dir := filepath.Dir(cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败：%w", dir, err)
	}
	// 面板以 root 运行，MkdirAll 建出来的是 root 属主目录，而以真实用户身份
	// 运行的 colima 随后要在里面写 default/ 子目录 —— 必须把属主改回去。
	// （与 writeColimaPlist 里 .colima 目录同一个坑。）
	if m.opt.UserName != "" {
		if err := chownPath(m.opt.UserName, filepath.Dir(dir)); err != nil {
			return "", fmt.Errorf("设置 %s 归属失败：%w", filepath.Dir(dir), err)
		}
		if err := chownPath(m.opt.UserName, dir); err != nil {
			return "", fmt.Errorf("设置 %s 归属失败：%w", dir, err)
		}
	}
	if err := m.writeColimaConfig(cfg, []byte("# 由 ZizPanel 预置（只写必须项，其余用 Colima 默认值）\n")); err != nil {
		return "", fmt.Errorf("创建 %s 失败：%w", cfg, err)
	}
	return cfg, nil
}

// stripYAMLListKey 从一段 YAML 里删掉 `key:` 及其列表项/子块，保留其余行。
func stripYAMLListKey(block []string, key string) []string {
	out := make([]string, 0, len(block))
	skipping := false
	skipIndent := -1
	for _, ln := range block {
		t := strings.TrimSpace(ln)
		ind := len(ln) - len(strings.TrimLeft(ln, " "))
		if skipping {
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if ind > skipIndent || (ind == skipIndent && strings.HasPrefix(t, "-")) {
				continue
			}
			skipping = false
		}
		if strings.HasPrefix(t, key+":") {
			skipping = true
			skipIndent = ind
			continue
		}
		out = append(out, ln)
	}
	return out
}

// setColimaDockerOption 在 colima.yaml 的 `docker:` 段里 upsert 一个列表键，
// **并保留该段里的其它键**（insecure-registries、features…）。
//
// 为什么不复用 upsertColimaMirrors：那个函数会把整个 `docker:` 段重写成
// 只剩 registry-mirrors —— 用户自己配的 insecure-registries/features 会被
// 无声抹掉。这里只在需要时新增，不改别人。
func setColimaDockerOption(text, key string, values []string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines)+len(values)+4)
	replaced := false
	i := 0
	for i < len(lines) {
		ln := lines[i]
		trimmed := strings.TrimSpace(ln)
		if !replaced && strings.HasPrefix(trimmed, "docker:") {
			indent := ln[:len(ln)-len(strings.TrimLeft(ln, " "))]
			j := i + 1
			for j < len(lines) {
				t := strings.TrimSpace(lines[j])
				ind := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
				if t != "" && ind <= len(indent) {
					break
				}
				j++
			}
			kept := stripYAMLListKey(lines[i+1:j], key)
			out = append(out, indent+"docker:")
			out = append(out, kept...)
			if len(values) > 0 {
				out = append(out, indent+"  "+key+":")
				for _, v := range values {
					out = append(out, indent+"  - "+v)
				}
			}
			i = j
			replaced = true
			continue
		}
		out = append(out, ln)
		i++
	}
	if !replaced && len(values) > 0 {
		out = append(out, "docker:", "  "+key+":")
		for _, v := range values {
			out = append(out, "  - "+v)
		}
	}
	res := strings.Join(out, "\n")
	if !strings.HasSuffix(res, "\n") {
		res += "\n"
	}
	return res
}

// insecureRegistryHostFor 判断某个镜像地址是否必须登记进 insecure-registries。
//
// 只有 **http://** 的镜像源需要：dockerd 默认只对 HTTPS 的 registry 放行，
// 明文 HTTP 的必须在 insecure-registries 里点名。内置公网候选都是 https，
// 所以通常什么都不用加；用户手填一个 `http://<自建机>:8090` 时才需要。
func insecureRegistryHostFor(raw string) string {
	if !strings.HasPrefix(strings.ToLower(raw), "http://") {
		return ""
	}
	rest := raw[len("http://"):]
	if i := strings.IndexAny(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// writeColimaDockerMirrors 把加速源写进 colima.yaml（不重启运行时）。
//
// 调用方负责保证 colima.yaml 已存在（见 ensureColimaConfigFile），以及
// 写完之后的 `colima start`/restart 来让配置生效。
func (m *Manager) writeColimaDockerMirrors(mirrors []string) (string, error) {
	cfg, err := m.ensureColimaConfigFile()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败：%w", cfg, err)
	}
	updated := setColimaDockerOption(string(b), configuredMirrorsKey, mirrors)
	// 明文 HTTP 的源要同时登记 insecure-registries，否则守护进程直接拒绝。
	var insecure []string
	for _, mir := range mirrors {
		if h := insecureRegistryHostFor(mir); h != "" {
			insecure = append(insecure, h)
		}
	}
	if len(insecure) > 0 {
		updated = setColimaDockerOption(updated, "insecure-registries", insecure)
	}
	if err := m.writeColimaConfig(cfg, []byte(updated)); err != nil {
		return "", fmt.Errorf("写入 %s 失败：%w", cfg, err)
	}
	return cfg, nil
}

// EnsureDockerMirrorsForRuntime 自动配置加速源（自建镜像站优先 + 探通的公共源回落）。
//
// 在 `colima start` **之前**调用：写进去的列表会被这次 start 带进 VM 里的
// daemon.json。探测有超时上限（4 秒），不会拖慢正常安装。
// 返回实际写入的列表（探测全失败时可能为空，此时**不改动**已有配置）。
func (m *Manager) EnsureDockerMirrorsForRuntime(ctx context.Context, result *InstallResult) []string {
	step := func(msg string) {
		if result != nil {
			result.step(ctx, msg)
		} else {
			emit(ctx, tasks.LevelStep, msg)
		}
	}
	// 已经配过就不重复探测/改写：避免每次装应用都把用户的配置推倒重来。
	if cfg := m.colimaYAMLPath(); cfg != "" {
		if b, err := os.ReadFile(cfg); err == nil {
			if have := parseColimaMirrors(string(b)); len(have) > 0 {
				step("Docker 加速源已配置（" + strings.Join(have, "、") + "），保持不动")
				return have
			}
		}
	}
	step(fmt.Sprintf("正在探测 Docker 加速源（共 %d 个，限时 %ds）…",
		len(m.DockerMirrorCandidates()), m.mirrorProbeTimeout()/time.Second))
	ordered, probes := m.preferredDockerMirrors(ctx)
	for _, p := range probes {
		mark := "不可用"
		if p.OK {
			mark = "可用"
		}
		step(fmt.Sprintf("  %s  %s  %dms  %s", p.URL, mark, p.LatencyMs, p.Detail))
	}
	if len(ordered) == 0 {
		step("⚠️ 所有加速源都探测失败，本次不改动 Docker 加速源配置；" +
			"拉取镜像可能超时，可到「Docker → 加速源」手动重试")
		return nil
	}
	cfg, err := m.writeColimaDockerMirrors(ordered)
	if err != nil {
		step("写入 Docker 加速源失败：" + err.Error())
		return nil
	}
	step(fmt.Sprintf("已写入 %d 个加速源到 %s（顺序即 docker 的尝试顺序；第一个不通就自动换下一个）",
		len(ordered), cfg))
	return ordered
}

// DockerMirrorRuntimeNote 返回"当前守护进程实际在用的加速源"的一句话说明。
//
// 给 compose 安装的步骤用（D18：必须让用户看见走的哪条路）。
func (m *Manager) DockerMirrorRuntimeNote(ctx context.Context) string {
	eff := m.effectiveMirrors(ctx)
	if len(eff) == 0 {
		return "守护进程当前**没有**生效的加速源（docker info 里 Registry Mirrors 为空）" +
			"：直连 registry-1.docker.io 在国内通常超时，建议先到「Docker → 加速源」配置"
	}
	return "守护进程生效的加速源（按顺序回落）：" + strings.Join(eff, " → ")
}

// ProbeComposeImageSources 逐条回答"这个镜像会从哪个源拉"。
//
// 为什么必须逐条、而且必须**按守护进程真实生效的顺序**探：
//
//	· registry-mirrors 只对 **Docker Hub** 生效 —— quay.io / ghcr.io 上的镜像
//	  根本不经过任何加速源，加多少源都没用；
//	· 而没有任何一个加速源是全的：实测 `docker.m.daocloud.io` 对
//	  `pjmeca/squoosh:1.1.0` 返回 **403 DENIED（not in the allowlist）**，
//	  而 `docker.1panel.live` 返回 200。所以"这个镜像到底会不会被拒绝"
//	  只能一个源一个源地问过去 —— 这也正是 docker 自己做的事。
//
// 返回给任务日志的行；调用方逐行 step 出去。
func (m *Manager) ProbeComposeImageSources(ctx context.Context, images []string) []string {
	if len(images) == 0 {
		return nil
	}
	mirrors := m.effectiveMirrors(ctx)
	client := &http.Client{Timeout: 8 * time.Second}
	out := make([]string, 0, len(images))
	for _, img := range images {
		host, repo, tag := splitImageRef(img)
		if host != "" {
			out = append(out, fmt.Sprintf("  %s：不是 Docker Hub 镜像（registry=%s）—— 加速源不覆盖，由守护进程直连该 registry", img, host))
			continue
		}
		if len(mirrors) == 0 {
			out = append(out, fmt.Sprintf("  %s：没有生效的加速源，将直连 registry-1.docker.io（国内通常超时）", img))
			continue
		}
		// 按守护进程的顺序逐个问：哪个先答 200 就是它。
		answered := ""
		var denied []string
		var unreachable int
		for _, mir := range mirrors {
			u := strings.TrimRight(mir, "/") + "/v2/" + repo + "/manifests/" + tag
			req, err := newRequestWithAccept(ctx, u)
			if err != nil {
				continue
			}
			start := time.Now()
			resp, err := client.Do(req)
			lat := time.Since(start).Milliseconds()
			if err != nil {
				unreachable++
				continue
			}
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == 200, resp.StatusCode == 304:
				answered = fmt.Sprintf("%s（%dms，第 %d 个源）", mir, lat, indexOfString(mirrors, mir)+1)
			case resp.StatusCode == 401:
				// 401 = registry 要求鉴权，是**正常**的（拉取时 docker 会去换 token）
				answered = fmt.Sprintf("%s（%dms，第 %d 个源）", mir, lat, indexOfString(mirrors, mir)+1)
			case resp.StatusCode == 403:
				denied = append(denied, mir)
			}
			if answered != "" {
				break
			}
		}
		switch {
		case answered != "":
			out = append(out, fmt.Sprintf("  %s：将走 %s", img, answered))
		case len(denied) > 0:
			out = append(out, fmt.Sprintf("  %s：⚠️ 加速源都拒绝了这个镜像（%s），将直连 registry-1.docker.io —— 很可能拉不下来",
				img, strings.Join(denied, "、")))
		case unreachable == len(mirrors):
			out = append(out, fmt.Sprintf("  %s：所有加速源都探测失败，将直连 registry-1.docker.io", img))
		default:
			out = append(out, fmt.Sprintf("  %s：加速源上没有这个 tag（可能只有 amd64），将由守护进程回落直连", img))
		}
	}
	return out
}

// indexOfString 返回 s 在 list 中的下标（找不到返回 -1）。
func indexOfString(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// imageAcceptHeader 是查 manifest 时要带的 Accept（多种 media type 都要收）。
const imageAcceptHeader = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// newRequestWithAccept 构造一个带 Accept 与 UA 的 GET 请求。
func newRequestWithAccept(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", imageAcceptHeader)
	req.Header.Set("User-Agent", "zizpanel-mirror-probe/1")
	return req, nil
}

// splitImageRef 把 `image:` 那一行的值拆成 (registry host, repo, tag)。
//
// 只按 Docker 的官方规则做最小拆分：
//
//	· 第一段含 `.` / `:` 或等于 localhost → 它是 registry 主机名；
//	· 没有主机名 = Docker Hub（此时单段名要补 `library/` 前缀）；
//	· tag 是**最后一个 `/` 之后**的 `:` 后面的部分，没有就默认 latest
//	  （不能按"最后一个冒号"切：`host:5000/repo` 里的冒号是端口）。
func splitImageRef(ref string) (host, repo, tag string) {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 { // 带 digest 的引用，取 digest 当 tag 用
		return splitImageRef(ref[:i])
	}
	tag = "latest"
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		tag = ref[i+1:]
		ref = ref[:i]
	}
	parts := strings.Split(ref, "/")
	if len(parts) > 1 {
		first := parts[0]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host = first
			repo = strings.Join(parts[1:], "/")
			return host, repo, tag
		}
	}
	repo = ref
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return "", repo, tag
}
