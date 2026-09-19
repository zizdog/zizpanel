package services

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

// TestUpsertColimaMountsQuotesTilde 锁住 D13 踩过的那个坑：
// colima.yaml 里的挂载必须是 `location: "~"`（**带引号**）。
//
// YAML 里裸 `~` 是 null，colima 会读成空串，启动直接失败：
//
//	level=fatal msg="error starting vm: overlapping mounts not supported:
//	                  '' overlaps '/opt/zizpanel/work/'"
//
// 这是真机上实测到的报错，不是推测。
func TestUpsertColimaMountsQuotesTilde(t *testing.T) {
	orig := "cpus: 2\nmounts: []\n\nmemory: 2\n"
	got, changed := upsertColimaMounts(orig, []string{"/opt/zizpanel/work"}, "/Users/zizdog")
	if !changed {
		t.Fatal("从 mounts: [] 出发应当判定为有改动")
	}
	if !strings.Contains(got, `- location: "~"`) {
		t.Errorf("`~` 必须带引号，实际：\n%s", got)
	}
	if strings.Contains(got, "- location: ~\n") {
		t.Errorf("出现了裸 `~`（YAML 里等于 null），实际：\n%s", got)
	}
	if !strings.Contains(got, "- location: /opt/zizpanel/work") {
		t.Errorf("缺 compose 数据目录挂载：\n%s", got)
	}
	// 段外的内容必须原样保留
	for _, want := range []string{"cpus: 2", "memory: 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("改 mounts 段时弄丢了 %q：\n%s", want, got)
		}
	}
	// 结果必须可被自己解析回来（幂等）
	if _, again := upsertColimaMounts(got, []string{"/opt/zizpanel/work"}, "/Users/zizdog"); again {
		t.Errorf("第二次调用不应再有改动：\n%s", got)
	}
}

// TestUpsertColimaMountsKeepsUserMounts 保证不会把用户自己加的挂载抹掉。
func TestUpsertColimaMountsKeepsUserMounts(t *testing.T) {
	orig := "mounts:\n  - location: \"~\"\n    writable: true\n  - location: /Users/zizdog/secrets\n    writable: false\n"
	got, changed := upsertColimaMounts(orig, []string{"/opt/zizpanel/work"}, "/Users/zizdog")
	if !changed {
		t.Fatal("缺少 /opt/zizpanel/work 时应判定为有改动")
	}
	if !strings.Contains(got, "/Users/zizdog/secrets") {
		t.Errorf("用户自己配的挂载被抹掉了：\n%s", got)
	}
	if !strings.Contains(got, "/opt/zizpanel/work") {
		t.Errorf("新挂载没写进去：\n%s", got)
	}
}

// TestUpsertColimaMountsTreatsExpandedHomeAsPresent 保证 `~` 与展开后的家目录
// 被当成同一个挂载（否则会写出两个重叠的挂载，colima 直接启动失败）。
func TestUpsertColimaMountsTreatsExpandedHomeAsPresent(t *testing.T) {
	orig := "mounts:\n  - location: /Users/zizdog/\n    writable: true\n"
	got, _ := upsertColimaMounts(orig, []string{"/opt/zizpanel/work"}, "/Users/zizdog")
	if strings.Contains(got, `"~"`) {
		t.Errorf("家目录已经以展开形式挂了，不该再写一条 `~`（会重叠）：\n%s", got)
	}
}

// TestParseMountLocationsReadsLimaYAML 用**真机上真实生成的** lima.yaml 片段做样本。
func TestParseMountLocationsReadsLimaYAML(t *testing.T) {
	sample := `vmType: vz
arch: aarch64
mounts:
    - location: /Users/zizdog/
      writable: true
    - location: /opt/zizpanel/work/
      writable: true
mountType: virtiofs
`
	got := parseMountLocations(sample)
	want := []string{"/Users/zizdog/", "/opt/zizpanel/work/"}
	if len(got) != len(want) {
		t.Fatalf("解析结果 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestSetColimaDockerOptionKeepsSiblings 锁住 D29/P0-B 的教训：
// 写 registry-mirrors 时**不能**把 docker: 段里的 insecure-registries / features 抹掉。
// 旧的 upsertColimaMirrors 会把整段重写成只剩 registry-mirrors。
func TestSetColimaDockerOptionKeepsSiblings(t *testing.T) {
	orig := `cpu: 2
docker:
  insecure-registries:
    - myregistry.com:5000
  features:
    buildkit: false
  registry-mirrors:
    - https://old.example.com
`
	got := setColimaDockerOption(orig, "registry-mirrors", []string{"https://new.example.com"})
	if strings.Contains(got, "old.example.com") {
		t.Errorf("旧的 registry-mirrors 没被替换：\n%s", got)
	}
	if !strings.Contains(got, "https://new.example.com") {
		t.Errorf("新的 registry-mirrors 没写进去：\n%s", got)
	}
	if !strings.Contains(got, "myregistry.com:5000") {
		t.Errorf("insecure-registries 被抹掉了：\n%s", got)
	}
	if !strings.Contains(got, "buildkit: false") {
		t.Errorf("features 被抹掉了：\n%s", got)
	}
	if !strings.Contains(got, "cpu: 2") {
		t.Errorf("docker 段外的内容被改动了：\n%s", got)
	}
}

// TestSetColimaDockerOptionOnEmptyDockerSection 处理 `docker: {}` 这种空段写法。
func TestSetColimaDockerOptionOnEmptyDockerSection(t *testing.T) {
	got := setColimaDockerOption("docker: {}\n", "registry-mirrors", []string{"https://a.example.com"})
	if !strings.Contains(got, "registry-mirrors:") || !strings.Contains(got, "https://a.example.com") {
		t.Errorf("空 docker 段没写入成功：\n%s", got)
	}
	if strings.Contains(got, "{}") {
		t.Errorf("`docker: {}` 后面不能直接跟列表项，必须展开：\n%s", got)
	}
}

// TestInsecureRegistryHostFor 只对明文 HTTP 的源要求登记 insecure-registries。
func TestInsecureRegistryHostFor(t *testing.T) {
	cases := map[string]string{
		"http://registry.example:5000/v2": "registry.example:5000",
		"http://mirror.local":             "mirror.local",
		"https://mirror.zizdog.com:8888":  "",
		"https://docker.1ms.run":          "",
	}
	for in, want := range cases {
		if got := insecureRegistryHostFor(in); got != want {
			t.Errorf("insecureRegistryHostFor(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestSplitImageRef 覆盖 compose 里真实出现的几种写法。
func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		in              string
		host, repo, tag string
	}{
		{"louislam/uptime-kuma:1", "", "louislam/uptime-kuma", "1"},
		{"gitea/gitea:latest", "", "gitea/gitea", "latest"},
		{"n8nio/n8n", "", "n8nio/n8n", "latest"},
		{"alpine", "", "library/alpine", "latest"},
		{"quay.io/minio/minio:latest", "quay.io", "minio/minio", "latest"},
		{"ghcr.io/corentinth/it-tools:latest", "ghcr.io", "corentinth/it-tools", "latest"},
		{"docker.n8n.io/n8nio/n8n:latest", "docker.n8n.io", "n8nio/n8n", "latest"},
		{"localhost:5000/foo/bar:v1", "localhost:5000", "foo/bar", "v1"},
	}
	for _, c := range cases {
		h, r, tag := splitImageRef(c.in)
		if h != c.host || r != c.repo || tag != c.tag {
			t.Errorf("splitImageRef(%q) = (%q,%q,%q)，期望 (%q,%q,%q)", c.in, h, r, tag, c.host, c.repo, c.tag)
		}
	}
}

// TestColimaGuestCachePathMatchesColimaRule 用**真实的那一对** URL/文件名做断言。
//
// 这是 P0-A 的核心机制：Colima 把 guest 镜像按"URL 的 sha256"缓存。
// 真机证据：本机 ~/Library/Caches/colima/caches/b0992ab8… 的内容 sha512 与
// 官方发布的 sha512 一致，而 b0992ab8… = sha256(下面这条 URL)。
// 这条单测把它钉死，防止有人把命名规则改成"文件名"或"版本号"。
func TestColimaGuestCachePathMatchesColimaRule(t *testing.T) {
	const url = "https://github.com/abiosoft/colima-core/releases/download/v0.10.4/" +
		"ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz"
	const wantName = "b0992ab88f5a3c0c436bbb3065c01466f20dc1dd0eb0a60299d410176f21a1c3"
	sum := sha256.Sum256([]byte(url))
	if got := hex.EncodeToString(sum[:]); got != wantName {
		t.Fatalf("缓存名 = %s，真机上观察到的名字是 %s", got, wantName)
	}
	m := &Manager{opt: Options{UserHome: "/Users/zizdog"}}
	if got := m.colimaGuestCachePath(url); got != "/Users/zizdog/Library/Caches/colima/caches/"+wantName {
		t.Errorf("缓存路径 = %s", got)
	}
}

// TestColimaGuestMirrorURLs 锁住镜像站上的两个布局（同一个 inode 的硬链接）。
func TestColimaGuestMirrorURLs(t *testing.T) {
	m := &Manager{opt: Options{MirrorBase: "https://mirror.zizdog.com:8888/"}}
	got := m.colimaGuestMirrorURLs(
		"https://github.com/abiosoft/colima-core/releases/download/v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz")
	want := []string{
		"https://mirror.zizdog.com:8888/apps/colima-core/v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz",
		"https://mirror.zizdog.com:8888/colima/v0.10.4/ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz",
	}
	if len(got) != len(want) {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestDockerMirrorCandidatesArePublicOnly 公网镜像站**不提供** `/docker/` 路径
// （2026-09-20 用户明确）：候选里绝不能出现任何以镜像基址拼出来的 /docker 端点，
// 否则 docker 会把一个 404 端点排在第一位，层数据阶段卡死/失败。
func TestDockerMirrorCandidatesArePublicOnly(t *testing.T) {
	m := &Manager{opt: Options{MirrorBase: "https://mirror.zizdog.com:8888"}}
	got := m.DockerMirrorCandidates()
	if len(got) != len(BuiltinDockerMirrors) {
		t.Fatalf("候选数 = %d，期望只有内置公网候选 %d 个", len(got), len(BuiltinDockerMirrors))
	}
	// 只匹配 URL 的**路径**段 `/docker`，不能误伤主机名（如 https://dockerproxy.net）。
	mirrorDockerPath := regexp.MustCompile(`^https?://[^/]+/docker(?:/|$)`)
	for i, c := range got {
		if c.URL != BuiltinDockerMirrors[i].URL {
			t.Errorf("第 %d 位 = %q，期望内置公网候选 %q", i, c.URL, BuiltinDockerMirrors[i].URL)
		}
		if mirrorDockerPath.MatchString(c.URL) {
			t.Errorf("候选里出现镜像站 /docker 端点（公网镜像站不提供它）：%q", c.URL)
		}
	}
	// 没配镜像站时同样只有公网候选（数量与内容都不随 MirrorBase 变）。
	m2 := &Manager{opt: Options{}}
	if got2 := m2.DockerMirrorCandidates(); len(got2) != len(BuiltinDockerMirrors) {
		t.Errorf("未配置镜像站时候选数 = %d，期望 %d", len(got2), len(BuiltinDockerMirrors))
	}
}

// TestColimaStartTimeoutIsGenerous 锁住 P0-A 的超时：guest 镜像公网约 317MiB，
// 实测 ~77KB/s（≈71 分钟），旧的 5 分钟必然掐断。
func TestColimaStartTimeoutIsGenerous(t *testing.T) {
	if colimaStartTimeout < 90*60*1e9 {
		t.Errorf("colimaStartTimeout = %v，对 317MiB 的 guest 镜像太短（公网实测约 71 分钟）", colimaStartTimeout)
	}
}

// TestShellQuote 覆盖单引号里的单引号（迁移脚本要靠它把路径安全拼进 sh -c）。
func TestShellQuote(t *testing.T) {
	if got := shellQuote("/opt/zizpanel/work"); got != "'/opt/zizpanel/work'" {
		t.Errorf("shellQuote = %s", got)
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shellQuote 转义错误：%s", got)
	}
}

// TestYamlQuoteValue 只有需要时才加引号，且 `~` 一定会被加。
func TestYamlQuoteValue(t *testing.T) {
	if got := yamlQuoteValue("~"); got != `"~"` {
		t.Errorf("yamlQuoteValue(~) = %s，期望带引号", got)
	}
	if got := yamlQuoteValue("/opt/zizpanel/work"); got != "/opt/zizpanel/work" {
		t.Errorf("普通绝对路径不需要加引号，得到 %s", got)
	}
	if got := yamlQuoteValue("/Users/a b/c"); !strings.HasPrefix(got, `"`) {
		t.Errorf("含空格的路径必须加引号，得到 %s", got)
	}
}

// TestBuiltinMirrorRanksAreNonDecreasing 锁住"候选顺序 = 自动配置顺序"：
// 声明顺序里 Rank 不允许回退（否则排在后面的"更能干"的源永远轮不到）。
func TestBuiltinMirrorRanksAreNonDecreasing(t *testing.T) {
	prev := -1
	for _, m := range BuiltinDockerMirrors {
		if m.Rank >= DockerMirrorRankUnusable {
			continue // 不可用于自动配置的条目只是说明，不参与顺序
		}
		if m.Rank < prev {
			t.Errorf("%s 的 Rank=%d 小于前一个可用源的 %d —— 可用源的声明顺序必须与能力顺序一致",
				m.URL, m.Rank, prev)
		}
		prev = m.Rank
	}
	if BuiltinDockerMirrors[0].Rank != 0 {
		t.Errorf("第一位应当是实测最能干的源（Rank 0），实际是 %s（Rank %d）",
			BuiltinDockerMirrors[0].URL, BuiltinDockerMirrors[0].Rank)
	}
}

// TestUnusableMirrorsMarked 锁住 2026-09-16 实测的两条"清单能取但没有层数据"
// 的源 —— 它们**必须**被标为不可用于自动配置，否则 docker 只对 manifest 做
// 多源回落，层数据会在这些源上失败/卡死。
func TestUnusableMirrorsMarked(t *testing.T) {
	want := map[string]bool{
		"https://docker.1ms.run":     true, // 层数据 BLOB_UNKNOWN/404
		"https://docker.1panel.live": true, // docker 客户端层数据 300 秒 0 进度
	}
	for _, m := range BuiltinDockerMirrors {
		if want[m.URL] && m.Rank < DockerMirrorRankUnusable {
			t.Errorf("%s 实测层数据不可用，Rank=%d 必须 >= %d", m.URL, m.Rank, DockerMirrorRankUnusable)
		}
	}
}

// TestDockerMirrorRankDefaultsToOne 用户手填的未知地址既不当"最好"也不排除。
func TestDockerMirrorRankDefaultsToOne(t *testing.T) {
	if got := DockerMirrorRank("https://unknown-mirror.example/v2"); got != 1 {
		t.Errorf("未知地址 Rank = %d，期望 1", got)
	}
	if got := DockerMirrorRank("https://dockerproxy.net/"); got != 0 {
		t.Errorf("带尾斜杠的已知地址应解析出 Rank 0，实际 %d", got)
	}
}
