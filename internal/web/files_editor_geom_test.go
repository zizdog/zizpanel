package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  文件编辑器窗口几何门禁（坑 220）
//
//  用户报障：编辑器打开时"上半截看不到、按钮点不到"，刷新无效，改窗口大小才自适应。
//  根因：窗口几何持久化在 localStorage，在**大视口**存的 left/top/尺寸被在**小视口**
//  原样恢复 ⇒ 顶边落在视口外；刷新恢复同一份坏几何 ⇒ 永远好不了。
//
//  修法对应的判据（三条，全部用 node 跑 files.js 里抽出的纯函数 clampEditorGeom）：
//    1. 越界（尺寸合格、位置在视口外）⇒ 平移进视口，尺寸不变；
//    2. 超大（尺寸超过可用区）⇒ 回退到居中的默认几何（占可用区 80%~90%），不保留坏几何；
//    3. 字段缺失/极小视口 ⇒ 同样回退且绝不溢出可用区。
//
//  另按函数体断言接线（打开/resize 都夹 + 写回夹过的值 + 最大化/最小化分支不变），
//  否则纯函数正确也只是死代码。
// ============================================================================

type editorGeomSize struct {
	Left   int `json:"left"`
	Top    int `json:"top"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type editorGeomBox struct {
	Left     int  `json:"left"`
	Top      int  `json:"top"`
	Width    int  `json:"width"`
	Height   int  `json:"height"`
	FellBack bool `json:"fellBack"`
}

type editorGeomProbe struct {
	View     editorGeomSize `json:"view"`
	TinyView editorGeomSize `json:"tinyView"`
	Boundary editorGeomBox  `json:"boundary"`
	Negative editorGeomBox  `json:"negative"`
	Oversize editorGeomBox  `json:"oversize"`
	Missing  editorGeomBox  `json:"missing"`
	Undef    editorGeomBox  `json:"undef"`
	Tiny     editorGeomBox  `json:"tiny"`
	Inside   map[string]bool
}

// editorGeomHarness 是喂给 node 的探针；%s 处填抽出来的纯函数体（含花括号）。
const editorGeomHarness = `
function clampEditorGeom(g, view)%s

const VIEW = { left: 0, top: 0, width: 900, height: 600 };
const TINY = { left: 0, top: 0, width: 200, height: 120 };
const inside = (g, v) => g.left >= v.left && g.top >= v.top &&
  g.left + g.width <= v.left + v.width && g.top + g.height <= v.top + v.height;
const boundary = clampEditorGeom({ left: 2400, top: 1800, width: 800, height: 500 }, VIEW);
const negative = clampEditorGeom({ left: -600, top: -400, width: 800, height: 500 }, VIEW);
const oversize = clampEditorGeom({ left: 100, top: 100, width: 2000, height: 1500 }, VIEW);
const missing  = clampEditorGeom(null, VIEW);
const undef    = clampEditorGeom(undefined, VIEW);
const tiny     = clampEditorGeom({ left: 0, top: 0, width: 400, height: 300 }, TINY);
console.log(JSON.stringify({
  view: VIEW, tinyView: TINY,
  boundary, negative, oversize, missing, undef, tiny,
  inside: {
    boundary: inside(boundary, VIEW), negative: inside(negative, VIEW),
    oversize: inside(oversize, VIEW), missing: inside(missing, VIEW),
    undef: inside(undef, VIEW), tiny: inside(tiny, TINY),
  },
}));
`

func runEditorGeomProbe(t *testing.T) editorGeomProbe {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("找不到 node，跳过编辑器几何纯函数门禁（make check 的 acorn 检查也需要它）")
	}
	js := readAssetJS(t, "files.js")
	body := jsFuncBody(t, js, "clampEditorGeom")
	script := strings.Replace(editorGeomHarness, "%s", body, 1)

	harness := filepath.Join(t.TempDir(), "probe.mjs")
	if err := os.WriteFile(harness, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harness).CombinedOutput()
	if err != nil {
		t.Fatalf("node 跑 clampEditorGeom 失败: %v\n%s", err, out)
	}
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	var p editorGeomProbe
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("解析探针输出失败: %v\n输出: %s", err, out)
	}
	return p
}

func (b editorGeomBox) ratioOf(v int) float64 { return float64(b.Width) / float64(v) }

func TestEditorWindowGeomClampsIntoViewport(t *testing.T) {
	p := runEditorGeomProbe(t)
	if !p.Inside["boundary"] || !p.Inside["negative"] {
		t.Fatalf("位置越界的几何没被平移进视口：%+v / %+v（视口 %+v）", p.Boundary, p.Negative, p.View)
	}
	for name, box := range map[string]editorGeomBox{"boundary": p.Boundary, "negative": p.Negative} {
		if box.FellBack {
			t.Errorf("%s：尺寸是合格的（800x500 ≤ 900x600），只该平移、不该回退默认几何", name)
		}
		if box.Width != 800 || box.Height != 500 {
			t.Errorf("%s：越界平移不该改尺寸，实际 %dx%d", name, box.Width, box.Height)
		}
		if box.Left < 0 || box.Top < 0 {
			t.Errorf("%s：夹后 left/top 仍为负：%+v", name, box)
		}
	}
	t.Logf("越界 %+v → %+v（未回退，尺寸保留）", editorGeomBox{Left: 2400, Top: 1800, Width: 800, Height: 500}, p.Boundary)
}

func TestEditorWindowGeomFallsBackWhenItDoesNotFit(t *testing.T) {
	p := runEditorGeomProbe(t)
	if !p.Inside["oversize"] || !p.Inside["missing"] || !p.Inside["undef"] {
		t.Fatalf("超大/缺失几何没回退到视口内：oversize=%+v missing=%+v undef=%+v",
			p.Oversize, p.Missing, p.Undef)
	}
	for name, box := range map[string]editorGeomBox{"oversize": p.Oversize, "missing": p.Missing, "undef": p.Undef} {
		if !box.FellBack {
			t.Errorf("%s：装不下/缺字段必须回退默认几何（不保留坏几何）", name)
		}
		if box.Width > p.View.Width || box.Height > p.View.Height {
			t.Errorf("%s：回退后的尺寸 %dx%d 超过可用区 %dx%d", name, box.Width, box.Height, p.View.Width, p.View.Height)
		}
		if box.Width < 320 || box.Height < 200 {
			t.Errorf("%s：回退尺寸 %dx%d 小于 CSS 最小尺寸 320x200", name, box.Width, box.Height)
		}
		if r := box.ratioOf(p.View.Width); r < 0.79 || r > 0.91 {
			t.Errorf("%s：默认宽度占可用区 %.3f，期望 80%%~90%%", name, r)
		}
		// 居中：默认几何的中心必须落在可用区中心附近（≤2px）。
		cx := box.Left + box.Width/2
		cy := box.Top + box.Height/2
		if absInt(cx-p.View.Width/2) > 2 || absInt(cy-p.View.Height/2) > 2 {
			t.Errorf("%s：回退几何没有居中：中心 (%d,%d)，可用区中心 (%d,%d)", name, cx, cy, p.View.Width/2, p.View.Height/2)
		}
	}
	t.Logf("坏几何 → 回退默认：oversize=%+v missing=%+v（视口 %+v）", p.Oversize, p.Missing, p.View)
}

func TestEditorWindowGeomNeverOverflowsTinyViewport(t *testing.T) {
	p := runEditorGeomProbe(t)
	if !p.Inside["tiny"] {
		t.Fatalf("极小视口（%dx%d）下窗口溢出：%+v", p.TinyView.Width, p.TinyView.Height, p.Tiny)
	}
	if p.Tiny.Width > p.TinyView.Width || p.Tiny.Height > p.TinyView.Height {
		t.Errorf("极小视口下窗口 %dx%d 超过可用区 %dx%d（CSS min-width 不该把窗口顶出视口）",
			p.Tiny.Width, p.Tiny.Height, p.TinyView.Width, p.TinyView.Height)
	}
	t.Logf("极小视口 %dx%d → 窗口 %+v", p.TinyView.Width, p.TinyView.Height, p.Tiny)
}

// TestEditorWindowGeomWiring 锁"打开/resize 都夹 + 写回 + 最大化/最小化不破坏"。
func TestEditorWindowGeomWiring(t *testing.T) {
	js := readAssetJS(t, "files.js")

	apply := jsFuncBody(t, js, "applyGeometry")
	for _, want := range []string{
		"clampEditorGeom(",                  // 真的走纯函数
		"layerViewRect()",                   // 可用区来自 .zpf-layer，不是 window.innerWidth 猜的
		"persistGeom()",                     // 夹过的值写回 localStorage
		"if (disposed || minimized) return", // 最小化态交给 CSS（zpf-win-min 的 !important）
		"if (maximized)",                    // 最大化语义单独一条分支
		"contentRect()",                     // 最大化仍逐像素贴 content
	} {
		if !strings.Contains(apply, want) {
			t.Errorf("applyGeometry 缺少 %q —— 关闭/恢复几何的某一环没接上", want)
		}
	}
	if !strings.Contains(js, "window.addEventListener('resize', applyGeometry)") {
		t.Error("resize 不再重夹几何 —— 拖大→存→拖小会原样复发")
	}
	persist := jsFuncBody(t, js, "persistGeom")
	if !strings.Contains(persist, "ZPF_POS_KEY") || !strings.Contains(persist, "JSON.stringify") {
		t.Errorf("persistGeom 没有把几何写回 %s：%s", "ZPF_POS_KEY", strings.TrimSpace(persist))
	}
	if !strings.Contains(persist, "maximized") || !strings.Contains(persist, "minimized") {
		t.Error("persistGeom 没排除最大化/最小化 —— 会把铺满态当成还原态存下来")
	}
	drag := jsFuncBody(t, js, "startDrag")
	for _, want := range []string{"layerViewRect()", "persistGeom()"} {
		if !strings.Contains(drag, want) {
			t.Errorf("startDrag 缺少 %q —— 拖动路径没接同一套可用区/落盘", want)
		}
	}
	read := jsFuncBody(t, js, "readStoredGeom")
	if !strings.Contains(read, "ZPF_POS_KEY") {
		t.Error("readStoredGeom 没读持久化几何")
	}
	// 旧实现（写死的 content 内缩偏移）必须彻底消失，否则两套几何打架。
	if strings.Contains(js, "ZPF_WIN_INSET") || strings.Contains(js, "readStoredPos") {
		t.Error("files.js 里还留着旧的 ZPF_WIN_INSET/readStoredPos 几何 —— 两套并存会互相覆盖")
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
