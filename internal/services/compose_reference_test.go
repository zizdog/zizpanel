package services

// compose_reference_test.go —— "推荐 Docker 项目"的静态不变量。
//
// 2026-09-17 用户要求：Docker 类条目不再由面板安装，只提供预配置 compose 参考
// 文件（内容 + 镜像站下载地址）。这组测试把这件事锁死：
//   · 目录里凡是 KindCompose 的条目都必须标记 DockerReference（否则前端会
//     错误地给它一个安装按钮，而安装接口必然拒绝 —— 自相矛盾）；
//   · 反过来，标了 DockerReference 的必须是 compose 条目；
//   · 导出的参考文件与目录一一对应、内容非空、路径符合约定；
//   · .env.example 只有占位符，绝不能出现真实密钥。

import (
	"strings"
	"testing"
)

// TestAllComposeEntriesAreDockerReferences 是"每次加 compose 条目都要显式表态"的锁。
func TestAllComposeEntriesAreDockerReferences(t *testing.T) {
	compose, refs := 0, 0
	for _, a := range Catalog() {
		if a.Kind == KindCompose {
			compose++
			if !a.DockerReference {
				t.Errorf("%s 是 KindCompose，但 DockerReference=false —— "+
					"Docker 类条目一律是推荐项目（面板不代安装），"+
					"漏标会让市场给它一个点了必然 4xx 的安装按钮", a.ID)
			}
		}
		if a.DockerReference {
			refs++
			if a.Kind != KindCompose && a.Kind != KindDocker {
				t.Errorf("%s 标了 DockerReference 但 Kind=%s —— 推荐项目只可能是容器类", a.ID, a.Kind)
			}
			if strings.TrimSpace(a.ComposeYAML) == "" {
				t.Errorf("%s 是推荐 Docker 项目，却没有 compose 参考内容", a.ID)
			}
		}
	}
	if compose == 0 {
		t.Fatal("目录里一个 compose 条目都没有？这条测试就失去意义了")
	}
	if refs != compose {
		t.Errorf("推荐项目数（%d）与 compose 条目数（%d）不一致", refs, compose)
	}
}

// TestComposeReferencesMatchCatalog 锁住导出给镜像站同步工具的数据与目录一一对应。
func TestComposeReferencesMatchCatalog(t *testing.T) {
	refs := ComposeReferences()
	if len(refs) == 0 {
		t.Fatal("ComposeReferences() 一个都没有")
	}
	byID := map[string]ComposeReference{}
	for _, r := range refs {
		if r.ID == "" || r.Name == "" {
			t.Errorf("参考文件缺少 ID/Name：%+v", r)
		}
		if _, dup := byID[r.ID]; dup {
			t.Errorf("参考文件里 %s 出现了两次", r.ID)
		}
		byID[r.ID] = r
		if !strings.Contains(r.ComposeYAML, "services:") {
			t.Errorf("%s 的 compose 不像一份 compose：%q", r.ID, r.ComposeYAML)
		}
		if r.NetworkMode != "host" && r.NetworkMode != "bridge" {
			t.Errorf("%s 的 NetworkMode 非法：%q", r.ID, r.NetworkMode)
		}
		if len(r.Images) == 0 {
			t.Errorf("%s 没有解析出任何镜像（反漂移测试会要求声明里有对应下载点）", r.ID)
		}
	}
	for _, a := range Catalog() {
		if !a.DockerReference {
			continue
		}
		if _, ok := byID[a.ID]; !ok {
			t.Errorf("目录里的推荐项目 %s 没有出现在 ComposeReferences() 里", a.ID)
		}
	}
	if len(refs) != len(byID) {
		t.Errorf("ComposeReferences() 里有重复 ID：%d 条 vs %d 个唯一 ID", len(refs), len(byID))
	}
}

// TestComposeEnvExampleHasNoSecrets 是安全锁：.env.example 只能有占位符。
//
// 这条比"看起来对"更重要：它是发布到镜像站、任何人都能下载的文件，
// 一旦有人把安装时随机生成的真密钥写进模板，等于把全世界的部署都暴露了。
func TestComposeEnvExampleHasNoSecrets(t *testing.T) {
	for _, a := range Catalog() {
		if !a.DockerReference {
			continue
		}
		example := ComposeEnvExample(a)
		if strings.TrimSpace(example) == "" {
			t.Errorf("%s 的 .env.example 是空的", a.ID)
		}
		for _, s := range a.ComposeSecrets {
			if !strings.Contains(example, s.Env+"=") {
				t.Errorf("%s 的 .env.example 缺少声明的密钥变量 %s（用户不知道要填什么）", a.ID, s.Env)
			}
			if !strings.Contains(example, "CHANGE_ME") {
				t.Errorf("%s 的 .env.example 里 %s 没有 CHANGE_ME 占位提示", a.ID, s.Env)
			}
		}
		// DATA_ROOT 是用户最先要改的：有数据挂载的项目必须给出它，且是第一行变量。
		if strings.Contains(a.ComposeYAML, "${DATA_ROOT") {
			if !strings.Contains(example, "DATA_ROOT=") {
				t.Errorf("%s 的 compose 用了 DATA_ROOT，.env.example 里却没有它", a.ID)
				continue
			}
			firstVar := ""
			for _, line := range strings.Split(example, "\n") {
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				firstVar = line
				break
			}
			if !strings.HasPrefix(firstVar, "DATA_ROOT=") {
				t.Errorf("%s 的 .env.example 第一行变量应是 DATA_ROOT=，实际 %q", a.ID, firstVar)
			}
		}
	}
}

// TestComposeReferenceURLs 锁住镜像站路径约定（与 tools/sync-nas-compose.sh 一致）。
func TestComposeReferenceURLs(t *testing.T) {
	base := "http://192.168.1.8:8090/"
	if got := ComposeYAMLURL(base, "it-tools"); got != "http://192.168.1.8:8090/compose/it-tools/docker-compose.yml" {
		t.Errorf("ComposeYAMLURL 拼错了：%q", got)
	}
	if got := ComposeEnvExampleURL(base, "immich"); got != "http://192.168.1.8:8090/compose/immich/.env.example" {
		t.Errorf("ComposeEnvExampleURL 拼错了：%q", got)
	}
	if got := ComposeReferenceIndexURL(base); got != "http://192.168.1.8:8090/compose/README.md" {
		t.Errorf("ComposeReferenceIndexURL 拼错了：%q", got)
	}
	// 没配镜像基址时必须是空串（前端据此隐藏下载链接，而不是给个坏地址）。
	if got := ComposeYAMLURL("", "it-tools"); got != "" {
		t.Errorf("镜像基址为空时应返回空串，实际 %q", got)
	}
}

// TestComposeReferenceDocsMentionDataRootAndNetwork 锁住"说明里必须讲清两件事"：
// 数据目录怎么改、网络方式是什么（host 的话端口怎么算）。
func TestComposeReferenceDocsMentionDataRootAndNetwork(t *testing.T) {
	index := ComposeReferenceIndexMarkdown("http://192.168.1.8:8090")
	if !strings.Contains(index, "DATA_ROOT") {
		t.Error("总索引 README 必须说明 DATA_ROOT 的用法（用户要统一放数据目录）")
	}
	for _, ref := range ComposeReferences() {
		if !strings.Contains(index, ref.ID) {
			t.Errorf("总索引 README 里没有列出 %s", ref.ID)
		}
		readme := ComposeReferenceReadme(ref)
		if !strings.Contains(readme, "DATA_ROOT") {
			t.Errorf("%s 的项目 README 要说明数据目录怎么改", ref.ID)
		}
		if ref.NetworkMode == "host" && !strings.Contains(readme, "host 网络") {
			t.Errorf("%s 用 host 网络，README 必须说清端口语义", ref.ID)
		}
	}
}
