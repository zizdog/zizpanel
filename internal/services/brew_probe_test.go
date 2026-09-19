package services

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================================
//  镜像探测的判据（按真机实测重写）
//
//  这一组测试锁住三件在真机上真的出过错的事：
//    1. brew 7 在自定义 HOMEBREW_BOTTLE_DOMAIN 下走**旧式平铺**文件名，
//       且 formula 重打包过（rebuild>0）时文件名必须带 `.bottle.<N>` ——
//       漏掉它，python@3.12（rebuild=1）永远 404、整家镜像被误判为不可用；
//    2. OCI 路径里 `@` 是**目录分隔**：python@3.11 → python/3.11；
//    3. 只看 HTTP 200 会把"200 + 0 字节"的坏源判成可用 ——
//       中科大在 IPv4 上对所有瓶就是 200 + 0 字节（同一时刻 IPv6 正常），
//       这正是 2026-09-18 python@3.11 事故的表象。
// ============================================================================

func TestBrewBottleFilenameMatchesBrewRules(t *testing.T) {
	cases := []struct {
		formula, version, tag string
		rebuild               int
		want                  string
	}{
		{"nginx", "1.29.0", "arm64_sequoia", 0, "nginx-1.29.0.arm64_sequoia.bottle.tar.gz"},
		{"python@3.11", "3.11.16", "arm64_sequoia", 0, "python%403.11-3.11.16.arm64_sequoia.bottle.tar.gz"},
		// rebuild>0：真机实测 python@3.12 是 rebuild=1，没有 `.bottle.1` 就是 404。
		{"python@3.12", "3.12.14", "arm64_sequoia", 1, "python%403.12-3.12.14.arm64_sequoia.bottle.1.tar.gz"},
		{"ca-certificates", "2026-08-13", "all", 1, "ca-certificates-2026-08-13.all.bottle.1.tar.gz"},
	}
	for _, c := range cases {
		if got := brewBottleFilename(c.formula, c.version, c.tag, c.rebuild); got != c.want {
			t.Errorf("brewBottleFilename(%q,%q,%q,%d) = %q，期望 %q",
				c.formula, c.version, c.tag, c.rebuild, got, c.want)
		}
	}
}

func TestBrewOCIPathSplitsFormulaAtSign(t *testing.T) {
	got := brewOCIPath("python@3.11", "a5dd571f")
	want := "/v2/homebrew/core/python/3.11/blobs/sha256:a5dd571f"
	if got != want {
		t.Errorf("brewOCIPath = %q，期望 %q（`@` 在 OCI 路径里是目录分隔）", got, want)
	}
	if got := brewOCIPath("nginx", "abc"); got != "/v2/homebrew/core/nginx/blobs/sha256:abc" {
		t.Errorf("无 @ 的 formula 不该多出目录：%q", got)
	}
}

// probeServer 造一个假镜像：清单里的 tag 与 rebuild 可控，
// 瓶文件按 `serve` 决定"给内容 / 给空 body / 404"。它还能记录被请求过的路径。
func probeServer(t *testing.T, formula, version, tag string, rebuild int, serve func(path string) (int, []byte)) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/formula/"+formula+".json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"versions":{"stable":%q},"bottle":{"stable":{"rebuild":%d,"files":{%q:{"sha256":"deadbeef"}}}}}`,
			version, rebuild, tag)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		code, body := serve(r.URL.Path)
		w.WriteHeader(code)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &seen
}

// 200 + 0 字节必须被拒（否则 IPv4-only 机器会选中中科大，brew 随后拿到空文件）。
func TestBrewMirrorProbeRejectsEmptyBottleBody(t *testing.T) {
	tag := bottleTagsForArch()[0]
	ts, seen := probeServer(t, "nginx", "1.29.0", tag, 0, func(string) (int, []byte) {
		return http.StatusOK, nil // 关键：状态码 200，但一个字节都没有
	})
	if brewMirrorSupportsOCIFor(context.Background(), ts.URL, "nginx") {
		t.Errorf("200 + 0 字节的镜像不该判为可用（真机实测：中科大 IPv4 侧就是这样）\n请求过：%v", *seen)
	}

	// 同样一家镜像，只要真的给 1 个字节就应判为可用。
	ts2, _ := probeServer(t, "nginx", "1.29.0", tag, 0, func(string) (int, []byte) {
		return http.StatusOK, []byte("B")
	})
	if !brewMirrorSupportsOCIFor(context.Background(), ts2.URL, "nginx") {
		t.Error("能取到非空内容的镜像应判为可用")
	}
}

// rebuild>0 时只有 `.bottle.<N>` 那个名字存在 → 必须能找到它（python@3.12 就是这样）。
func TestBrewMirrorProbeHonoursRebuildSuffix(t *testing.T) {
	tag := bottleTagsForArch()[0]
	withRebuild := "nginx-1.29.0." + tag + ".bottle.1.tar.gz"
	ts, seen := probeServer(t, "nginx", "1.29.0", tag, 1, func(p string) (int, []byte) {
		if strings.HasSuffix(p, "/"+withRebuild) {
			return http.StatusOK, []byte("B")
		}
		return http.StatusNotFound, nil
	})
	if !brewMirrorSupportsOCIFor(context.Background(), ts.URL, "nginx") {
		t.Errorf("rebuild=1 时应去找 %s（少了 .1 会永远 404）\n请求过：%v", withRebuild, *seen)
	}

	// 反向：清单说 rebuild=0，而镜像上只有带 .1 的那个文件 → 不该判为可用
	//（否则 brew 会按 rebuild=0 的名字去要，仍然 404）。
	ts2, _ := probeServer(t, "nginx", "1.29.0", tag, 0, func(p string) (int, []byte) {
		if strings.HasSuffix(p, "/"+withRebuild) {
			return http.StatusOK, []byte("B")
		}
		return http.StatusNotFound, nil
	})
	if brewMirrorSupportsOCIFor(context.Background(), ts2.URL, "nginx") {
		t.Error("清单与文件名不一致时不该判为可用（brew 会按清单要 rebuild=0 的名字）")
	}
}

// @ 形式与 %40 形式都要试：brew 用 %40，但有的镜像/服务端只认原样的 @（镜像站两种都通）。
func TestBrewMirrorProbeAcceptsPlainAtForm(t *testing.T) {
	tag := bottleTagsForArch()[0]
	plain := "python@3.11-3.11.16." + tag + ".bottle.tar.gz"
	ts, _ := probeServer(t, "python@3.11", "3.11.16", tag, 0, func(p string) (int, []byte) {
		if strings.HasSuffix(p, "/"+plain) {
			return http.StatusOK, []byte("B")
		}
		return http.StatusNotFound, nil
	})
	if !brewMirrorSupportsOCIFor(context.Background(), ts.URL, "python@3.11") {
		t.Error("镜像只认原样 @ 时也应判为可用")
	}
}

// 探测不到镜像时，brewEnv **不能**写出空值变量（brew 是 Ruby，空串 ≠ 未设置）。
func TestBrewEnvOmitsEmptyMirrorDomains(t *testing.T) {
	m := brewFallbackTestManager(t, "")
	env := m.brewEnv(context.Background(), "nginx")
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOMEBREW_API_DOMAIN=") || strings.HasPrefix(kv, "HOMEBREW_BOTTLE_DOMAIN=") {
			t.Errorf("没有可用镜像时不该出现 %q（空值会被 brew 当成已配置）", kv)
		}
	}
	if brewEnvValue(env, "HOMEBREW_NO_AUTO_UPDATE") != "1" {
		t.Errorf("公共开关仍要在，实际 %v", env)
	}
}

// 没有可用镜像时：国内镜像（清华）排第一，官方源只出现一次且在最后。
//
// 之前的实现会把"官方源"排在第一位（因为它把"当前源"当成一条普通候选塞在最前），
// 于是用户要等官方源慢慢失败才走到国内镜像 —— 与"国内镜像优先"正好相反。
func TestBrewInstallSourcesOfficialLastWhenNoMirror(t *testing.T) {
	m := brewFallbackTestManager(t, "")
	srcs := m.brewInstallSources(context.Background(), "nginx")
	if len(srcs) < 2 {
		t.Fatalf("没有可用镜像时至少该有 清华 + 官方 两个源，实际 %+v", srcs)
	}
	if !strings.Contains(srcs[0].Name, "清华") {
		t.Errorf("第 1 个源应是国内的清华镜像，实际 %q", srcs[0].Name)
	}
	official := 0
	for i, s := range srcs {
		if strings.Contains(s.Name, "官方源") {
			official++
			if i != len(srcs)-1 {
				t.Errorf("官方源必须排在最后兜底，实际在第 %d/%d 位", i+1, len(srcs))
			}
		}
	}
	if official != 1 {
		t.Errorf("官方源应只出现 1 次，实际 %d 次：%+v", official, srcs)
	}
}

// 离线模式（仅走镜像站）连自建镜像都没探到时，必须**什么都不试**并如实报错，
// 绝不能退回官方源（那等于"离线模式偷偷出网"）。
func TestBrewInstallOfflineWithoutMirrorFailsHonestly(t *testing.T) {
	m := brewFallbackTestManager(t, "")
	m.opt.OfflineOnly = true

	if srcs := m.brewInstallSources(context.Background(), "python@3.11"); len(srcs) != 0 {
		t.Fatalf("离线模式没有可用镜像时不该有任何源，实际 %+v", srcs)
	}
	var n int
	m.brewSourceRunOverride = func(context.Context, time.Duration, brewInstallSource, ...string) (string, error) {
		n++
		return "", nil
	}
	_, err := m.brewInstall(context.Background(), &InstallResult{}, time.Minute, "python@3.11")
	if err == nil {
		t.Fatal("离线模式没有可用镜像时必须如实报错")
	}
	if n != 0 {
		t.Errorf("不许真的去跑 brew，实际跑了 %d 次", n)
	}
	if !strings.Contains(err.Error(), "离线") {
		t.Errorf("错误文案要说清「离线模式」下为什么没装，实际：%v", err)
	}
}
