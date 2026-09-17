package services

// catalog_ports_test.go —— 端口分配与「健康判定不许谎报成功」的静态/行为锁。
//
// 背景（2026-09-17 用户真机反馈）：
//   · MinIO 的直链/别名跳到宿主 9001，看到的是 **Portainer 的超时页** ——
//     根因是 Portainer 的 HTTP 口占着 9001，而 MinIO 控制台容器内就在 9001。
//   · Portainer 因安全超时锁定时 GET / 返回 307 → /timeout.html，
//     而通用健康规则把 3xx 判成健康 → 面板对一个已经打不开的实例报 ok=true。

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestCatalogComposeHostPortsAreUnique 是全目录级的端口护栏：
// compose 里发布的**宿主端口**不许重复（a.Port 的唯一性测试看不到 compose 内部
// 的第二、第三个端口 —— MinIO 的控制台与 Portainer 的 HTTP 口就撞在 9001）。
// 另外 9000 是本项目里 PHP-FPM 的保留端口，任何 compose 都不许往宿主 9000 上发布。
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

// TestMinIOAndPortainerPortCausality 把这次真机反馈的具体安排钉死，
// 免得以后有人"顺手"把端口改回去。
func TestMinIOAndPortainerPortCausality(t *testing.T) {
	minio, ok := FindApp("minio")
	if !ok {
		t.Fatal("目录里没有 minio")
	}
	// 健康检查必须打 9010 的 S3 API（/minio/health/live 在 9010 才是 200）
	if minio.WebPort() != 9010 {
		t.Errorf("MinIO 的健康检查端口（WebPort）应为 9010（S3 API），实际 %d", minio.WebPort())
	}
	if minio.HealthPath != "/minio/health/live" {
		t.Errorf("MinIO 健康路径应为 /minio/health/live，实际 %q", minio.HealthPath)
	}
	// 但「打开 / 直链」要给控制台 9001
	if minio.EntryPort() != 9001 {
		t.Errorf("MinIO 的入口端口（EntryPort）应为控制台 9001，实际 %d "+
			"（否则 port_url 指向 9010，浏览器只得到 AccessDenied XML）", minio.EntryPort())
	}
	minioPorts := composeHostPorts(minio.ComposeYAML)
	if !containsInt(minioPorts, 9010) || !containsInt(minioPorts, 9001) {
		t.Errorf("MinIO 的 compose 应发布宿主 9010(API) 与 9001(控制台)，实际 %v", minioPorts)
	}

	portainer, ok := FindApp("portainer")
	if !ok {
		t.Fatal("目录里没有 portainer")
	}
	if portainer.Port != 9002 {
		t.Errorf("Portainer 宿主 HTTP 口应让出 9001、改用 9002，实际 %d", portainer.Port)
	}
	if portainer.EntryPort() != 9002 {
		t.Errorf("Portainer 的入口端口应为 9002，实际 %d", portainer.EntryPort())
	}
	if containsInt(composeHostPorts(portainer.ComposeYAML), 9001) {
		t.Error("Portainer 不该再占用宿主 9001（那是 MinIO 控制台）")
	}
}

func containsInt(list []int, want int) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestHealthTreatsPortainerTimeoutLockAsUnhealthy 是"谎报成功"那条的回归：
// 307 → /timeout.html 必须判成**不健康**，并给出可操作的说明。
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
