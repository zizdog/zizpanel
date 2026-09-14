package services

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpsertAndParseColimaMirrorsRoundTrip 是本功能最重要的断言：
// 改写 colima.yaml 时**不能碰其它内容**（那是 Colima 自己生成的文件，
// 重写整个文件等于把它的默认值一起改掉），而且写进去要能读回来。
func TestUpsertAndParseColimaMirrorsRoundTrip(t *testing.T) {
	orig := `# Colima 生成的配置，请勿手改（除了 docker: 段）
arch: aarch64
cpu: 4
memory: 8
disk: 60
runtime: docker
docker: {}
forwardAgent: false
`
	got := upsertColimaMirrors(orig, []string{"https://docker.m.daocloud.io", "https://docker.1ms.run"})
	// 其它行必须一字不动
	for _, keep := range []string{"arch: aarch64", "cpu: 4", "memory: 8", "runtime: docker", "forwardAgent: false", "# Colima 生成的配置"} {
		if !strings.Contains(got, keep) {
			t.Errorf("改写后丢了原有内容：%q\n---\n%s", keep, got)
		}
	}
	list := parseColimaMirrors(got)
	if len(list) != 2 {
		t.Fatalf("应解析出 2 个加速源，实际 %v（\n%s）", list, got)
	}
	if list[0] != "https://docker.m.daocloud.io" || list[1] != "https://docker.1ms.run" {
		t.Errorf("解析结果不对：%v", list)
	}

	// 再写一次（幂等）：内容必须一致
	twice := upsertColimaMirrors(got, []string{"https://docker.m.daocloud.io", "https://docker.1ms.run"})
	if twice != got {
		t.Errorf("重复写入结果不同：\n--- once ---\n%s\n--- twice ---\n%s", got, twice)
	}

	// 清空：回到 docker: {}，并且解析为空
	cleared := upsertColimaMirrors(got, nil)
	if !strings.Contains(cleared, "docker: {}") {
		t.Errorf("清空后应写回 docker: {}：\n%s", cleared)
	}
	if l := parseColimaMirrors(cleared); len(l) != 0 {
		t.Errorf("清空后不该解析出加速源：%v", l)
	}
	if !strings.Contains(cleared, "forwardAgent: false") {
		t.Error("清空时把后面的配置弄丢了")
	}
}

// TestParseColimaMirrorsHandlesInline 兼容行内写法与已有多条的情况。
func TestParseColimaMirrorsHandlesInline(t *testing.T) {
	inline := "docker:\n  registry-mirrors: [\"https://a.example\", \"https://b.example\"]\n"
	if got := parseColimaMirrors(inline); len(got) != 2 || got[0] != "https://a.example" {
		t.Errorf("行内写法解析失败：%v", got)
	}
	// 没有 docker 段 → 空
	if got := parseColimaMirrors("arch: aarch64\n"); len(got) != 0 {
		t.Errorf("没有 docker 段时应为空：%v", got)
	}
	// 老版本可能用 <<: *docker 之类，这里只需保证不 panic、不误读别的段
	other := "docker: {}\ncontainerd:\n  registry-mirrors:\n  - https://should-not-be-read\n"
	if got := parseColimaMirrors(other); len(got) != 0 {
		t.Errorf("不该把 containerd 段的镜像源读成 docker 的：%v", got)
	}
}

// TestValidateMirrorURL 拦住明显写错的地址（这类错要在提交任务**之前**说清楚）。
func TestValidateMirrorURL(t *testing.T) {
	ok := map[string]string{
		"https://docker.m.daocloud.io": "https://docker.m.daocloud.io",
		"https://docker.1ms.run/":      "https://docker.1ms.run",
		"http://mirror.internal:5000":  "http://mirror.internal:5000",
	}
	for in, want := range ok {
		got, err := ValidateMirrorURL(in)
		if err != nil || got != want {
			t.Errorf("%q 应通过并规范化为 %q，实际 %q err=%v", in, want, got, err)
		}
	}
	bad := []string{"", "docker.m.daocloud.io", "ftp://x.example", "https://", "https://a.example/?x=1"}
	for _, in := range bad {
		if _, err := ValidateMirrorURL(in); err == nil {
			t.Errorf("%q 应被拒绝", in)
		}
	}
}

// TestSameStringSet 决定"要不要提示重启"：顺序不同不该被当成不一致，
// 否则用户每次进页面都会看到一条假的"需要重启"。
func TestSameStringSet(t *testing.T) {
	if !sameStringSet([]string{"https://a", "https://b"}, []string{"https://b/", "https://a/"}) {
		t.Error("同一集合（顺序不同、有无尾斜杠）应判为相同")
	}
	if sameStringSet([]string{"https://a"}, []string{"https://a", "https://b"}) {
		t.Error("不同集合应判为不同")
	}
}

// TestProbeOneMirrorJudgement 判定必须区分"可用"和"连线成功但站坏了"。
//
// 真实依据（2026-09-14 两台机器实测）：
//
//	200 → 可用；401 → **可用**（registry 要求鉴权，是它在正常工作的证据）
//	403 → 被拒绝；302 → 跳转；000/超时 → 连不上
func TestProbeOneMirrorJudgement(t *testing.T) {
	cases := []struct {
		status  int
		wantOK  bool
		wantSub string
	}{
		{http.StatusOK, true, "可用"},
		{http.StatusUnauthorized, true, "可用"},
		{http.StatusForbidden, false, "被拒绝"},
		{http.StatusFound, false, "跳转"},
		{http.StatusBadGateway, false, "异常状态"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		}))
		got := probeOneMirror(t.Context(), srv.URL)
		srv.Close()
		if got.OK != c.wantOK {
			t.Errorf("状态 %d：OK 应为 %v，实际 %v（%s）", c.status, c.wantOK, got.OK, got.Detail)
		}
		if !strings.Contains(got.Detail, c.wantSub) {
			t.Errorf("状态 %d：说明应含 %q，实际 %q", c.status, c.wantSub, got.Detail)
		}
	}

	// 连不上（没人监听的端口）
	dead := probeOneMirror(t.Context(), "http://127.0.0.1:1")
	if dead.OK {
		t.Error("连不上时不该判为可用")
	}
	if !strings.Contains(dead.Detail, "连不上") {
		t.Errorf("应说明连不上，实际 %q", dead.Detail)
	}
}

// TestBuiltinMirrorsAreWellFormed 内置列表里每条都必须是合法地址，
// 而且**都要写实测结论**（死站要让用户一眼看出来，而不是白等）。
func TestBuiltinMirrorsAreWellFormed(t *testing.T) {
	if len(BuiltinDockerMirrors) < 5 {
		t.Fatalf("内置候选太少（%d 条），用户没法比较", len(BuiltinDockerMirrors))
	}
	for _, m := range BuiltinDockerMirrors {
		if _, err := ValidateMirrorURL(m.URL); err != nil {
			t.Errorf("内置地址不合法 %q：%v", m.URL, err)
		}
		if m.Name == "" || m.Note == "" {
			t.Errorf("内置地址 %s 缺少名称或实测结论", m.URL)
		}
	}
}
