package services

// catalog_ports_test.go —— 端口分配与「健康判定不许谎报成功」的静态/行为锁。
//
// 背景（2026-09-17 用户真机反馈）：
//   · MinIO 的直链/别名跳到宿主 9001，看到的是 **Portainer 的超时页** ——
//     根因是 Portainer 的 HTTP 口占着 9001，而 MinIO 控制台容器内就在 9001。
//     （两个条目已在同一轮按用户要求下架，端口冲突随之消失；见下面的
//     TestCatalogHasNoMinIOAndPortainer。）
//   · Portainer 因安全超时锁定时 GET / 返回 307 → /timeout.html，
//     而通用健康规则把 3xx 判成健康 → 面板对一个已经打不开的实例报 ok=true。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// composeHostPortRe 抠出 compose 里 `- "宿主:容器"` 形式的宿主端口。
var composeHostPortRe = regexp.MustCompile(`(?m)^\s*-\s*"(\d+):(\d+)"\s*$`)

// composeHostPorts 返回一个 compose 应用发布到宿主的所有端口。
func composeHostPorts(yaml string) []int {
	var out []int
	for _, m := range composeHostPortRe.FindAllStringSubmatch(yaml, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// composeUsesHostNetwork 判断一份 compose 是否用了 host 网络。
//
// 只认整行 `network_mode: host`（允许缩进与行尾注释），不认注释掉的那一行 ——
// 模板里另一种写法就是"把这行注释掉 + 写清为什么"，测试必须区分两者。
var composeHostNetRe = regexp.MustCompile(`(?m)^\s*network_mode:\s*["']?host["']?\s*(#.*)?$`)

func composeUsesHostNetwork(yaml string) bool { return composeHostNetRe.MatchString(yaml) }

// TestCatalogComposeHostPortsAreUnique 是全目录级的端口护栏：
// compose 里发布的**宿主端口**不许重复（a.Port 的唯一性测试看不到 compose 内部
// 的第二、第三个端口）。另外 9000 是本项目里 PHP-FPM 的保留端口，
// 任何 compose 都不许往宿主 9000 上发布。
func TestCatalogComposeHostPortsAreUnique(t *testing.T) {
	seen := map[int]string{}
	for _, a := range Catalog() {
		if a.Kind != KindCompose {
			continue
		}
		for _, p := range composeHostPorts(a.ComposeYAML) {
			if p == 9000 {
				t.Errorf("%s 往宿主 9000 发布了端口 —— 9000 是 PHP-FPM 的保留端口", a.ID)
			}
			if prev, ok := seen[p]; ok {
				t.Errorf("宿主端口 %d 被 %s 与 %s 同时发布（后装的那个会起不来，"+
					"或者像 MinIO 那样被用户误认成另一个应用）", p, prev, a.ID)
			}
			seen[p] = a.ID
		}
	}
}

// TestHostNetworkComposeInvariants 锁住"用 host 网络"的那几条自己的一致性：
//
//  1. host 网络下 `ports:` 会被 docker **静默忽略**（容器直接占用 VM 内端口），
//     留着它只会让人以为还能映射到别的宿主端口 —— 所以不许同时写 ports；
//  2. 端口用 App.Port 表示（host 网络下它 == VM 内端口 == Mac 宿主转发端口），
//     不许占用面板保留端口（80 nginx / 9000 PHP-FPM / 8080 IOPaint）；
//  3. 两条 host 网络条目不许撞同一个端口。
//
// 为什么要有它：2026-09-17 实测确认 Colima/Lima 会把 VM 内监听的端口转发到
// Mac 宿主，于是"无交互项目尽量用 host"这条要求成立；但它同时把"端口冲突"
// 从"映射冲突"变成了"主机端口直接冲突"，必须静态锁住。
func TestHostNetworkComposeInvariants(t *testing.T) {
	reserved := map[int]string{80: "面板自带的 nginx", 9000: "PHP-FPM", 8080: "IOPaint"}
	seen := map[int]string{}
	hostNet := 0
	for _, a := range Catalog() {
		if a.Kind != KindCompose || !composeUsesHostNetwork(a.ComposeYAML) {
			continue
		}
		hostNet++
		if apps := composeHostPorts(a.ComposeYAML); len(apps) > 0 {
			t.Errorf("%s 同时写了 network_mode: host 与 ports: %v —— host 网络下 ports 会被忽略，"+
				"容易让人误以为改的是宿主端口", a.ID, apps)
		}
		if why, bad := reserved[a.Port]; bad {
			t.Errorf("%s 用 host 网络并占用端口 %d（%s 的保留端口）—— 会直接抢不到端口", a.ID, a.Port, why)
		}
		if prev, dup := seen[a.Port]; dup {
			t.Errorf("host 网络端口 %d 被 %s 与 %s 同时占用", a.Port, prev, a.ID)
		}
		seen[a.Port] = a.ID
	}
	if hostNet == 0 {
		t.Error("目录里一条 host 网络的 compose 都没有 —— 用户明确要求「无交互项目尽量用 host」，" +
			"而 2026-09-17 已在 mini 实测 Colima/Lima 会把 VM 内端口转发到 Mac 宿主；" +
			"要么补上，要么在测试里写清为什么不适用")
	}
}

// TestHostNetworkPortMatchesComposeDoc 防止"改了端口没改说明"：
// host 网络条目的 App.Port 必须能在 compose 注释里找到（模板会写清端口号给用户看）。
func TestHostNetworkPortMatchesComposeDoc(t *testing.T) {
	for _, a := range Catalog() {
		if a.Kind != KindCompose || !composeUsesHostNetwork(a.ComposeYAML) {
			continue
		}
		if !strings.Contains(a.ComposeYAML, strconv.Itoa(a.Port)) {
			t.Errorf("%s 用 host 网络（端口 %d），但 compose 里没有任何地方写出这个端口号 —— "+
				"host 网络下用户必须知道容器内端口就是 VM 内端口", a.ID, a.Port)
		}
	}
}

// TestHealthTreatsPortainerTimeoutLockAsUnhealthy 是"谎报成功"那条的回归：
// 307 → /timeout.html 必须判成**不健康**，并给出可操作的说明。
//
// Portainer 条目已从目录下架，但机器上还装着它 —— 这个测试因此还多锁了一条：
// 健康提示不能依赖"目录里有这个条目"（见 isPortainerService）。
func TestHealthTreatsPortainerTimeoutLockAsUnhealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/timeout.html", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	h := httpHealth(context.Background(), &Service{
		Name: "portainer", DisplayName: "Portainer CE",
		HealthURL: ts.URL + "/",
	})
	if h.OK {
		t.Fatalf("Portainer 超时锁定必须判成不健康，实际 OK=true（message=%q）—— 这就是谎报成功", h.Message)
	}
	if !strings.Contains(h.Message, "锁定") || !strings.Contains(h.Message, "重启容器") {
		t.Errorf("不健康时要说清是什么问题、怎么恢复，实际：%q", h.Message)
	}
}

// TestHealthStillLikesNormalRedirect 反面保证：
// 普通的 3xx（例如 ddns-go 未登录 307 → /login）仍然是健康的 ——
// 不能为了修 Portainer 把"能正常跳转"也叫成故障。
func TestHealthStillLikesNormalRedirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	h := httpHealth(context.Background(), &Service{Name: "ddns-go", HealthURL: ts.URL + "/"})
	if !h.OK {
		t.Errorf("普通 307 跳转应仍是健康的，实际 OK=false（message=%q）", h.Message)
	}
}

// TestCatalogHasNoN8n 锁住下架（反漂移门禁是双向的：目录与声明都要删干净）。
func TestCatalogHasNoN8n(t *testing.T) {
	if _, ok := FindApp("n8n"); ok {
		t.Error("n8n 已按用户要求从市场下架，目录里不该再有它")
	}
	if _, ok := MarketAppFor("n8n"); ok {
		t.Error("n8n 的市场下载点声明也要一起删（否则反漂移测试会报声明里多了一条）")
	}
	// 但"已下架条目仍要清理得掉"：镜像名必须留在卸载计划的知识里。
	if imgs := legacyComposeImages["n8n"]; len(imgs) == 0 {
		t.Error("n8n 下架后必须把镜像名留在 legacyComposeImages 里，" +
			"否则用户删完 compose 目录，镜像还占着磁盘而计划里不提")
	}
}

// TestCatalogHasNoMinIOAndPortainer 锁住这一轮的下架（用户要求删掉这两个条目）。
//
// 与 n8n 那次**关键区别**：mini 上这两个应用还**装着**（用户明确说只删条目、
// 不要动容器/数据）。所以除了"目录与声明都删干净"，还必须保证：
//
//	· 镜像名留在 legacyComposeImages 里（卸载计划说得出镜像）；
//	· 目录里没有它们时，卸载计划仍然给得出来（走 PlanUninstall 的残留分支）。
func TestCatalogHasNoMinIOAndPortainer(t *testing.T) {
	for _, id := range []string{"minio", "portainer"} {
		if _, ok := FindApp(id); ok {
			t.Errorf("%s 已按用户要求从市场删除，目录里不该再有它", id)
		}
		if _, ok := MarketAppFor(id); ok {
			t.Errorf("%s 的市场下载点声明也要一起删（反漂移门禁是双向的）", id)
		}
		if imgs := legacyComposeImages[id]; len(imgs) == 0 {
			t.Errorf("%s 下架后必须把镜像名留在 legacyComposeImages 里 —— "+
				"这台机器上它还装着，卸载计划要说得出该删哪个镜像", id)
		}
	}
}

// TestDelistedEntriesStillCleanable 锁住"条目下架 ≠ 卸载得掉"：
// 目录里没有 minio/portainer 了，但磁盘上的 compose 目录还在时必须给得出计划，
// 且计划里要点名该删哪个镜像。
//
// 这是 2026-09-16 那次事故（Lucky 从目录移除后"根本没被卸载掉"）的同类回归：
// 卸载计划必须**以磁盘状态为准**，不能只看代码注册表。
func TestDelistedEntriesStillCleanable(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"minio", "portainer"} {
		t.Run(id, func(t *testing.T) {
			m, _ := sandboxIdempotentManager(t)
			dir := filepath.Join(m.opt.WorkDir, "compose", id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			plan := m.PlanUninstall(ctx, id)
			if plan.Kind != "installer" {
				t.Fatalf("下架条目在磁盘上还有 compose 目录时，必须给 installer 残留计划；"+
					"实际 kind=%q blocked=%q", plan.Kind, plan.Blocked)
			}
			if len(plan.DataPaths) == 0 {
				t.Error("计划要列出残留目录路径，用户才知道会删什么")
			}
			imgs := legacyComposeImages[id]
			joined := strings.Join(plan.Steps, "\n")
			if len(imgs) == 0 || !strings.Contains(joined, imgs[0]) {
				t.Errorf("计划里必须点名镜像 %v（面板不自动 docker rmi，但用户有权知道）——实际步骤：\n%s",
					imgs, joined)
			}
		})
	}
}
