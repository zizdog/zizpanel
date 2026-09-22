package api_test

import (
	"net/http"
	"testing"
)

// TestEmptySeriesReturnsNextStepGuide 锁住"新建剧场不是空页面"：详情接口必须
// 给出放文件的位置、命名写法、允许根与两条后续路径（前端空状态卡片的依据）。
func TestEmptySeriesReturnsNextStepGuide(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	created := e.createSeries("空剧场")

	res, env, raw := e.do(http.MethodGet, "/api/v1/series/"+created.ID, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("读取剧场失败 %d: %s", res.StatusCode, raw)
	}
	var body struct {
		List  []any `json:"list"`
		Guide struct {
			Title       string   `json:"title"`
			Where       string   `json:"where"`
			Naming      string   `json:"naming"`
			Roots       []string `json:"roots"`
			UsableRoots []string `json:"usable_roots"`
			RootsURL    string   `json:"roots_url"`
			Steps       []struct {
				Key  string `json:"key"`
				Text string `json:"text"`
			} `json:"steps"`
		} `json:"guide"`
	}
	decodeInto(t, env.Data, &body)

	if len(body.List) != 0 {
		t.Fatalf("新剧场不应有剧集: %v", body.List)
	}
	if body.Guide.Title == "" || body.Guide.Where == "" || body.Guide.Naming == "" {
		t.Fatalf("空状态提示缺字段: %+v", body.Guide)
	}
	if len(body.Guide.Roots) != 1 || body.Guide.Roots[0] != e.Root {
		t.Fatalf("guide 必须带上允许根: %v", body.Guide.Roots)
	}
	if len(body.Guide.UsableRoots) != 1 || body.Guide.UsableRoots[0] != e.Root {
		t.Fatalf("该根可读，usable_roots = %v", body.Guide.UsableRoots)
	}
	keys := map[string]bool{}
	for _, step := range body.Guide.Steps {
		keys[step.Key] = true
	}
	for _, want := range []string{"import_dir", "batch"} {
		if !keys[want] {
			t.Fatalf("缺少后续路径 %q: %+v", want, body.Guide.Steps)
		}
	}
	if body.Guide.RootsURL == "" {
		t.Fatal("必须给出「去加允许根」的入口")
	}
}
