package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  音视频播放器门禁：vendor 资源齐全 + files.js 真的接上了 + 运行时绝不联网
//
//  用户："文件管理要能打开视频和音频文件"，且"最好引入现有项目"。
//  这类内嵌资源的坏法都很隐蔽：
//    · 少 plyr.svg → 控件图标全是空白（Plyr 不报错）；
//    · iconUrl 退回默认值 → 运行时去 cdn.plyr.io 取图标（内网/断网直接没图标）；
//    · files.js 只把媒体塞进文本编辑器 → 用户看到的是"乱码"，不是播放器。
//  所以用测试把"资源清单 / 接入点 / 无 CDN"钉住。
// ============================================================================

func vendoredFile(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join("assets", "vendor", rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("内嵌播放器资源缺失：%s（%v）—— 播放器会静默少图标/打不开", p, err)
	}
	if len(b) == 0 {
		t.Fatalf("内嵌播放器资源是空文件：%s", p)
	}
	return string(b)
}

func TestMediaPlayerVendorAssetsPresent(t *testing.T) {
	for _, rel := range []string{
		"plyr/plyr.min.js", "plyr/plyr.css", "plyr/plyr.svg",
		"plyr/LICENSE.md", "plyr/README.md",
	} {
		vendoredFile(t, rel)
	}
	// 真的是 Plyr，不是被换成了占位内容
	if js := vendoredFile(t, "plyr/plyr.min.js"); !strings.Contains(js, "Plyr") {
		t.Error("plyr/plyr.min.js 里找不到 Plyr —— vendor 内容不对")
	}
	// 图标 sprite 里必须有控件图标：空 sprite 不会报错，只会让按钮变空白方块
	svg := vendoredFile(t, "plyr/plyr.svg")
	for _, icon := range []string{"plyr-play", "plyr-pause", "plyr-muted", "plyr-volume", "plyr-settings"} {
		if !strings.Contains(svg, `id="`+icon+`"`) {
			t.Errorf("plyr.svg 里缺图标 %s —— 控件会渲染成空白", icon)
		}
	}
	// 版本与许可证必须写死在 README（升级时同步这里）
	readme := vendoredFile(t, "plyr/README.md")
	for _, want := range []string{"3.8.4", "MIT", "registry.npmjs.org"} {
		if !strings.Contains(readme, want) {
			t.Errorf("vendor/plyr/README.md 里缺 %q —— 来源/版本/许可证必须可追溯", want)
		}
	}
}

func TestMediaPlayerWiredIntoFilesJS(t *testing.T) {
	js := readAssetJS(t, "files.js")
	for _, want := range []string{
		"vendor/plyr/",       // 资源指到内嵌目录
		"ensurePlyr(",        // 按需加载入口
		"window.Plyr",        // UMD 全局
		"new Plyr(",          // 真的构造播放器
		"'video'", "'audio'", // 视频/音频两条路径
		"preload: 'metadata'", // 大文件不许整份进内存
		"iconUrl:",            // 覆盖默认 CDN 图标
		"plyr.svg",
		"mediaKind(",    // 类型判定
		"previewMedia(", // 播放器入口
	} {
		if !strings.Contains(js, want) {
			t.Errorf("files.js 里缺 %q —— 音视频要么没接上播放器，要么被改回了文本编辑器", want)
		}
	}
	// 双击/回车/右键都走 openAny，openAny 必须能到 previewMedia
	if !strings.Contains(js, "previewMedia(entry, kind)") {
		t.Error("files.js 的 openAny 没有把音视频分派给 previewMedia")
	}
	// 放不了的容器清单与用户可见文案（诚实标注能力边界）
	for _, want := range []string{"mkv", "avi", "wmv", "flv", "rmvb", "浏览器不支持这个格式，请下载后用本地播放器"} {
		if !strings.Contains(js, want) {
			t.Errorf("files.js 里缺 %q —— 放不了的格式必须明确告知，不许假装能播", want)
		}
	}
}

// TestMediaPlayerNoCDN 运行时绝不联网：vendor 引用上不许出现 http(s)，也不许出现 CDN 主机名。
func TestMediaPlayerNoCDN(t *testing.T) {
	js := readAssetJS(t, "files.js")
	for _, bad := range []string{"cdn.plyr.io", "jsdelivr", "unpkg.com"} {
		if strings.Contains(js, bad) {
			t.Errorf("files.js 里出现了 CDN 主机名 %q —— 面板运行时不许联网取资源", bad)
		}
	}
	for i, line := range strings.Split(js, "\n") {
		if !strings.Contains(line, "vendor/") {
			continue
		}
		if strings.Contains(line, "http://") || strings.Contains(line, "https://") {
			t.Errorf("files.js:%d 的 vendor 引用带了 http(s) 链接：%s", i+1, strings.TrimSpace(line))
		}
	}
	// 播放器不许写进 index.html（必须按需加载，不拖慢首屏）
	idx, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("读不到 index.html: %v", err)
	}
	if strings.Contains(string(idx), "plyr") {
		t.Error("index.html 里出现了 plyr —— 播放器必须与 CodeMirror 一样按需加载")
	}
}
