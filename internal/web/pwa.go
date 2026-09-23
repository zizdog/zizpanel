package web

// pwa.go —— PWA（可加到手机主屏）与它的图标/manifest（C4）。
//
// 三条取舍：
//   · **manifest 由后端生成**：面板可能挂在 `/<安全后缀>/` 下，start_url / scope /
//     图标地址都必须带这个前缀，写死成静态文件就会指向 404（加主屏后打不开）。
//   · **图标用代码画**（不引入二进制资源、不加构建步骤）：一张品牌蓝圆角方块 + 白色 Z，
//     任何尺寸都能生成，改配色只改一处。
//   · 离线壳交给 `assets/sw.js`（浏览器直接加载的静态文件，进 acorn 语法门禁）；
//     这里只提供 manifest 与图标。

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// pwaThemeColor 是品牌色（与 index.html 里的 favicon/theme-color 同一色值）。
const pwaThemeColor = "#2563eb"

func (s *Server) handlePWAManifest(w http.ResponseWriter, r *http.Request) {
	entry := s.PanelEntryPath() // "/" 或 "/<后缀>/"
	icon := func(size int, purpose string) map[string]any {
		return map[string]any{
			"src":     entry + "pwa/icon-" + strconv.Itoa(size) + ".png",
			"sizes":   strconv.Itoa(size) + "x" + strconv.Itoa(size),
			"type":    "image/png",
			"purpose": purpose,
		}
	}
	m := map[string]any{
		"name":             "ZizPanel 管理面板",
		"short_name":       "ZizPanel",
		"description":      "macOS 服务器管理面板",
		"lang":             "zh-CN",
		"start_url":        entry,
		"scope":            entry,
		"display":          "standalone",
		"orientation":      "any",
		"background_color": "#0b1220",
		"theme_color":      pwaThemeColor,
		"icons": []map[string]any{
			icon(192, "any"),
			icon(512, "any"),
			// maskable：安卓会把图标裁成圆形/方形，留白版本才不会把 Z 切掉。
			icon(512, "maskable"),
		},
	}
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m)
}

// handlePWAIcon 生成指定尺寸的 PNG 图标。只接受 192 / 512（够用，且避免被当成图片代理）。
func (s *Server) handlePWAIcon(w http.ResponseWriter, r *http.Request) {
	// 路由是两条固定路径（icon-192.png / icon-512.png）：Go 的 ServeMux 不允许
	// 段中通配（`icon-{size}.png` 直接 panic），所以尺寸从路径名解析。
	name := strings.TrimSuffix(strings.TrimPrefix(path.Base(r.URL.Path), "icon-"), ".png")
	size, err := strconv.Atoi(name)
	if err != nil || (size != 192 && size != 512) {
		writeErr(w, http.StatusNotFound, "图标尺寸不存在")
		return
	}
	b, err := pwaIconPNG(size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成图标失败")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

var pwaIconCache sync.Map // size → []byte

// pwaIconPNG 画一张 size×size 的品牌图标（圆角蓝底 + 白色 Z）。
func pwaIconPNG(size int) ([]byte, error) {
	if v, ok := pwaIconCache.Load(size); ok {
		return v.([]byte), nil
	}
	f := float64(size)
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 0x25, G: 0x63, B: 0xeb, A: 0xff}
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	radius := 0.22 * f // 圆角半径：与 favicon 的 rx=7/32 视觉一致
	// 三条笔画构成 Z：上横、斜线、下横（不依赖字体，任何机器上画出来都一样）。
	inBar := func(x, y, x0, x1, y0, y1 float64) bool {
		return x >= x0*f && x <= x1*f && y >= y0*f && y <= y1*f
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			// 圆角矩形：四角按圆心距离裁剪，其余区域填满。
			cx := math.Min(math.Max(px, radius), f-radius)
			cy := math.Min(math.Max(py, radius), f-radius)
			if math.Hypot(px-cx, py-cy) > radius {
				continue // 角外留透明（安卓 maskable 与 iOS 都好看）
			}
			img.Set(x, y, bg)
			if inBar(px, py, 0.28, 0.72, 0.26, 0.345) || inBar(px, py, 0.28, 0.72, 0.655, 0.74) {
				img.Set(x, y, white)
				continue
			}
			// 斜线：y 从 0.30 到 0.70 时，中心 x 从 0.70 线性走到 0.30。
			if py >= 0.30*f && py <= 0.70*f {
				t := (py - 0.30*f) / (0.40 * f)
				cxm := (0.70 - 0.40*t) * f
				if math.Abs(px-cxm) <= 0.055*f {
					img.Set(x, y, white)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	pwaIconCache.Store(size, out)
	return out, nil
}
