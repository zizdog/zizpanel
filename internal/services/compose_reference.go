package services

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ============================================================================
//  Docker 推荐项目：预配置 compose 参考文件
//
//  用户 2026-09-17 明确要求：Docker 容器不该放在应用中心、不该写得这么"重"。
//  Docker 类条目从"可安装应用"改成"面板建议的项目"：
//    · 面板**不代用户安装**（安装接口明确拒绝，见 web 层）；
//    · 只提供预配置好的 compose 文件，用户改完自己 `docker compose up -d`；
//    · 这些文件同时发布到 NAS 镜像站，供"从镜像拉取"的场景取用。
//
//  本文件是**单一数据源**：内容来自 catalog 里的 App.ComposeYAML，
//  由 cmd/zizpanel-assets 的 `compose` 子命令导出、
//  由 tools/sync-nas-compose.sh 发布到 <镜像站>/compose/<id>/。
//  绝不在这里手抄第二份 compose —— 两份一定会漂移，而漂移的后果是
//  "面板展示的 compose"与"镜像站下载的 compose"不是同一份。
//
//  镜像站布局（与 /apps、/sites 同风格）：
//    /compose/README.md                          总索引（项目、端口、网络方式）
//    /compose/<id>/docker-compose.yml            预配置 compose（含 ${VAR} 占位符）
//    /compose/<id>/.env.example                  变量样例（复制成 .env 再改）
// ============================================================================

// mirrorComposeDir 是镜像站上"推荐 Docker 项目"的目录名。
//
// 对外路径：<base>/compose/<id>/docker-compose.yml。
const mirrorComposeDir = "compose"

// composeFileName / composeEnvExampleName 是每个项目目录下的两个固定文件名。
const (
	composeFileName      = "docker-compose.yml"
	composeEnvName       = ".env.example"
	composeIndexFileName = "README.md"
)

// ComposeReference 是一个推荐 Docker 项目要发布的全部参考内容。
type ComposeReference struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ComposeYAML 是预配置 compose 文件的完整内容（含 ${VAR} 占位符）。
	ComposeYAML string `json:"compose_yaml"`
	// EnvExample 是可以复制成 .env 的变量样例（只有占位符，绝无真实密钥）。
	EnvExample string `json:"env_example"`
	// NetworkMode 是 "host" 或 "bridge"（从 compose 内容里如实读出来）。
	NetworkMode string `json:"network_mode"`
	// Ports 是人类可读的端口说明（"8081:80" / "host 网络 · 容器内 3001"）。
	Ports []string `json:"ports,omitempty"`
	// Images 是这个项目需要的镜像（按出现顺序去重）。
	Images []string `json:"images,omitempty"`
	// DocsURL / Description 给索引 README 用。
	DocsURL     string `json:"docs_url,omitempty"`
	Description string `json:"description,omitempty"`
}

// ComposeReferences 返回目录里全部"推荐 Docker 项目"的参考文件（按目录顺序）。
//
// 判据是 App.DockerReference（不是 Kind）：两者语义不同 —— 见该字段的注释。
func ComposeReferences() []ComposeReference {
	out := []ComposeReference{}
	for _, a := range Catalog() {
		if !a.DockerReference {
			continue
		}
		out = append(out, ComposeReference{
			ID:          a.ID,
			Name:        a.Name,
			ComposeYAML: a.ComposeYAML,
			EnvExample:  ComposeEnvExample(a),
			NetworkMode: composeNetworkMode(a.ComposeYAML),
			Ports:       composePortNotes(a),
			Images:      composeImagesOf(a.ComposeYAML),
			DocsURL:     a.DocsURL,
			Description: a.Summary,
		})
	}
	return out
}

// ComposeReferenceURL 拼镜像站上推荐项目的文件地址（base 为空时返回空串）。
//
// file 传 composeFileName / composeEnvName；appID 传空表示索引 README（根目录）。
func ComposeReferenceURL(base, appID, file string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || file == "" {
		return ""
	}
	if appID == "" {
		return base + "/" + mirrorComposeDir + "/" + file
	}
	return base + "/" + mirrorComposeDir + "/" + appID + "/" + file
}

// ComposeReferenceIndexURL 是索引 README 的地址（面板把 URL 给前端/用户）。
func ComposeReferenceIndexURL(base string) string {
	return ComposeReferenceURL(base, "", composeIndexFileName)
}

// ComposeYAMLURL / ComposeEnvExampleURL 是给 web 层用的便捷入口 ——
// 文件名常量不导出，避免 web 层再手抄一遍 "docker-compose.yml"。
func ComposeYAMLURL(base, appID string) string {
	return ComposeReferenceURL(base, appID, composeFileName)
}

func ComposeEnvExampleURL(base, appID string) string {
	return ComposeReferenceURL(base, appID, composeEnvName)
}

// composeNetworkMode 如实读出 compose 用的是 host 还是默认 bridge 网络。
func composeNetworkMode(yaml string) string {
	if composeHostNetLineRe.MatchString(yaml) {
		return "host"
	}
	return "bridge"
}

// composeHostNetLineRe 与 catalog_ports_test.go 里的测试用正则同义，
// 但**不能共用一个变量**：测试文件里的那个是本包编译期的一部分，
// 生产代码依赖测试文件会在 `go build`（不带 _test.go）时失败。
var composeHostNetLineRe = regexp.MustCompile(`(?m)^\s*network_mode:\s*["']?host["']?\s*(#.*)?$`)

// refPortPairRe 抠 compose 里 `- "宿主:容器"` 端口对。
var refPortPairRe = regexp.MustCompile(`(?m)^\s*-\s*"(\d+):(\d+)"\s*$`)

// composePortNotes 生成给索引/README 看的端口说明。
func composePortNotes(a App) []string {
	var out []string
	for _, m := range refPortPairRe.FindAllStringSubmatch(a.ComposeYAML, -1) {
		out = append(out, m[1]+":"+m[2])
	}
	if composeNetworkMode(a.ComposeYAML) == "host" {
		// host 网络下容器内端口就是 VM 内端口；App.Port 是面板记录的端口，
		// 对这两个条目而言它等于容器内默认端口。
		out = []string{fmt.Sprintf("host 网络 · 容器内 %d（Colima 自动转发到 Mac 宿主同一端口）", a.Port)}
	}
	return out
}

// composeVarRe 抠出 `${VAR}` / `${VAR:-默认值}` 里的变量名与默认值。
var composeVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// composeEnvSkipVars 是 compose 里引用、但**不该写进 .env.example** 的变量：
// 它们由运行 compose 的那个 shell 提供（HOME）。写进 .env 反而危险 ——
// compose 读 .env 并**覆盖**同名环境变量，一个空的 `HOME=` 会把
// `${HOME}/Documents` 变成 `/Documents`。
//
// 为什么用 ${HOME} 而不是 `~`：2026-09-17 实测 `~` 的展开随 compose 版本变化 ——
// mini 上 /opt/homebrew/bin/docker-compose（Compose 5.5.1）会把 compose 文件里
// 的 `~/Documents` 展开成家目录，而更早的版本（以及最初部署 filebrowser 的那次）
// 把它当成相对路径，挂载点变成 <项目目录>/~/Documents（磁盘上真有一个叫 `~` 的
// 目录，容器里看到的是空的）。${HOME} 是标准变量替换，各版本行为一致。
var composeEnvSkipVars = map[string]bool{"HOME": true}

// ComposeEnvExample 从 compose 内容里提取变量，生成可以复制成 .env 的样例。
//
// 三条硬要求（用户明确提过）：
//  1. **只有占位符，绝不写真密钥** —— 密钥类变量给 CHANGE_ME 提示；
//  2. DATA_ROOT 放在最前面并解释"默认 . = compose 文件旁边，想统一放就指到
//     ~/docker/<项目>"（用户原话："也许，用户会想统一把目录挂载到 user/.../docker 下"）；
//  3. 与 ComposeYAML 一一对应：compose 里引用什么变量，这里就列什么变量。
func ComposeEnvExample(app App) string {
	type vinfo struct {
		name string
		def  string
	}
	seen := map[string]bool{}
	var vars []vinfo
	for _, m := range composeVarRe.FindAllStringSubmatch(app.ComposeYAML, -1) {
		name := m[1]
		if seen[name] || composeEnvSkipVars[name] {
			continue
		}
		seen[name] = true
		vars = append(vars, vinfo{name: name, def: m[3]})
	}
	// DATA_ROOT 永远排第一（用户最先要改的就是它）。
	sort.SliceStable(vars, func(i, j int) bool {
		return vars[i].name == "DATA_ROOT" && vars[j].name != "DATA_ROOT"
	})

	secrets := map[string]string{} // env -> label
	for _, s := range app.ComposeSecrets {
		label := s.Label
		if label == "" {
			label = s.Env
		}
		secrets[s.Env] = label
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s（%s）的变量样例 —— 复制成 .env 后按需修改，docker compose 会自动读同目录 .env。\n", app.Name, app.ID)
	b.WriteString("# ⚠️ 这里**没有**任何真实密钥：带 CHANGE_ME 的值请自己生成后再用。\n")
	b.WriteString("# 用法：cp .env.example .env && 编辑 .env && docker compose up -d\n")
	if len(vars) == 0 {
		b.WriteString("#\n# 这个项目没有任何可配置变量（不需要 .env）。\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, v := range vars {
		switch {
		case v.name == "DATA_ROOT":
			b.WriteString("# 数据根目录：默认 . = compose 文件所在目录。\n")
			b.WriteString("# 想把各项目的数据统一收在一处，就改成你自己的目录。推荐写成\n")
			b.WriteString("#   DATA_ROOT=${HOME}/docker/" + app.ID + "\n")
			b.WriteString("# ⚠️ 别用 `~/docker/...`：`~` 在不同 compose 版本里展开行为不一致\n")
			b.WriteString("#    （老版本会把它当相对路径，生成一个字面叫 ~ 的目录）。\n")
			def := v.def
			if def == "" {
				def = "."
			}
			fmt.Fprintf(&b, "DATA_ROOT=%s\n\n", def)
		case secrets[v.name] != "":
			fmt.Fprintf(&b, "# %s —— **必填**，不要用这个占位符。生成一个强随机值：\n", secrets[v.name])
			b.WriteString("#   openssl rand -hex 32\n")
			fmt.Fprintf(&b, "%s=CHANGE_ME\n\n", v.name)
		default:
			if v.def != "" {
				fmt.Fprintf(&b, "%s=%s\n", v.name, v.def)
			} else {
				b.WriteString("# 留空表示用 compose 里写的默认值；需要时再填。\n")
				fmt.Fprintf(&b, "%s=\n", v.name)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// ComposeReferenceReadme 是单个项目目录里的 README（把"怎么用"写清楚）。
func ComposeReferenceReadme(ref ComposeReference) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s（%s）—— 面板推荐的 Docker 项目\n\n", ref.Name, ref.ID)
	if ref.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", ref.Description)
	}
	b.WriteString("这份 compose 由 ZizPanel 预配置，**面板不会替你安装** —— 下载后自己改、自己跑。\n\n")
	b.WriteString("## 怎么用\n\n")
	b.WriteString("```sh\n")
	b.WriteString("# 1. 建目录并取文件\n")
	fmt.Fprintf(&b, "mkdir -p ~/docker/%s && cd ~/docker/%s\n", ref.ID, ref.ID)
	b.WriteString("curl -fsSLO <镜像站>/compose/" + ref.ID + "/docker-compose.yml\n")
	b.WriteString("curl -fsSL  <镜像站>/compose/" + ref.ID + "/.env.example -o .env.example\n\n")
	b.WriteString("# 2. 按需改 .env（数据目录、密钥）；然后把 .env.example 复制成 .env\n")
	b.WriteString("cp .env.example .env && vi .env\n\n")
	b.WriteString("# 3. 起容器\n")
	b.WriteString("docker compose up -d\n```\n\n")
	b.WriteString("## 访问\n\n")
	if len(ref.Ports) == 0 {
		b.WriteString("- 没有对外端口。\n")
	} else {
		for _, p := range ref.Ports {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	if ref.NetworkMode == "host" {
		b.WriteString("\n> host 网络：容器内监听哪个端口，Mac 宿主上就是哪个端口" +
			"（Colima/Lima 会把 VM 内监听的端口自动转发到宿主；已实测）。\n")
	} else {
		b.WriteString("\n> 端口映射（bridge）网络：冒号左边是 Mac 宿主端口，右边是容器内端口。\n")
	}
	b.WriteString("\n## 数据目录\n\n")
	b.WriteString("模板里的挂载都写成 `${DATA_ROOT:-.}/...`：默认就在 compose 文件旁边，" +
		"改 `.env` 里的 `DATA_ROOT` 就能把数据统一收到别处 —— " +
		"推荐写成 `DATA_ROOT=${HOME}/docker/" + ref.ID + "`（**别用 `~`**：" +
		"`~` 的展开随 compose 版本变化，老版本会生成一个字面叫 `~` 的目录）。\n")
	if len(ref.Images) > 0 {
		b.WriteString("\n## 用到的镜像\n\n")
		for _, img := range ref.Images {
			fmt.Fprintf(&b, "- `%s`\n", img)
		}
	}
	if ref.DocsURL != "" {
		fmt.Fprintf(&b, "\n官方文档：%s\n", ref.DocsURL)
	}
	return b.String()
}

// ComposeReferenceIndexMarkdown 是镜像站 /compose/README.md 的内容：
// 一份列出全部推荐项目与端口的总索引。
func ComposeReferenceIndexMarkdown(host string) string {
	var b strings.Builder
	b.WriteString("# ZizPanel 推荐的 Docker 项目（预配置 compose）\n\n")
	b.WriteString("这些是面板建议的 Docker 项目。**面板不代你安装** —— 这里给出预配置好的\n")
	b.WriteString("`docker-compose.yml` 与 `.env.example`，改完自己 `docker compose up -d`。\n\n")
	b.WriteString("每个项目一个目录：`compose/<id>/docker-compose.yml` + `compose/<id>/.env.example`。\n\n")
	b.WriteString("数据目录统一用 `${DATA_ROOT:-.}` 变量：默认放在 compose 文件旁边，\n")
	b.WriteString("把 `.env` 里的 `DATA_ROOT` 指到 `${HOME}/docker/<项目>` 就能集中管理\n")
	b.WriteString("（**别写 `~`**：`~` 的展开随 compose 版本变化，老版本会生成一个字面叫 `~` 的目录）。\n\n")
	b.WriteString("| 项目 | 说明 | 网络 | 端口 | compose |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, ref := range ComposeReferences() {
		ports := strings.Join(ref.Ports, "、")
		if ports == "" {
			ports = "—"
		}
		fmt.Fprintf(&b, "| [%s](%s/) | %s | %s | %s | [yml](%s/%s) |\n",
			ref.Name, ref.ID, ref.Description, ref.NetworkMode, ports, ref.ID, composeFileName)
	}
	if host != "" {
		b.WriteString("\n基址：" + strings.TrimRight(host, "/") + "/compose/\n")
	}
	return b.String()
}
