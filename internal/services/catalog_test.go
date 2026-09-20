package services

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 应用市场卡片只显示 Summary，但 Description 会出现在详情里。
// 用户两次反馈：先是"描述太多了！简单一句就行"，后又要求"只允许一两句话" ——
// 于是每个条目只留**用户做决定/使用时真正需要的那一两件事**，并用这条测试
// 防止描述再膨胀回去：
//   - Description 不超过 80 个字符（rune，中文按字算）；
//   - Summary 必须有（前端卡片就显示它，空字符串等于卡片没标题）。
//
// 上限用 rune 而不是字节：中文一个字是 3 字节，按字节算会把
// 正常的一句话误判成超长。
//
// 设计理由 / 历史事故 / 实现细节属于代码注释，不要写回 Description：
// 涨到 80 字以上就该把它移进条目的 `//` 注释里。
func TestCatalogDescriptionsStayShort(t *testing.T) {
	apps := Catalog()
	if len(apps) == 0 {
		t.Fatal("应用目录为空")
	}
	for _, a := range apps {
		if a.Summary == "" {
			t.Errorf("应用 %s 缺少 Summary（市场卡片要显示它）", a.ID)
		}
		if n := utf8.RuneCountInString(a.Description); n > 80 {
			t.Errorf("应用 %s 的 Description 有 %d 个字符，超过 80 —— "+
				"应用描述只能是 1-2 句（≤80 字），长解释请写进代码注释/文档", a.ID, n)
		}
	}
}

// TestLNMPEntriesExposeConfigFile 锁住 2026-09-18 用户的要求：
// 「面板也要有手动改 php 和 nginx 的入口啊！这是基本操作。」
//
// 判据是"这些条目必须声明 ConfigPath，且能解析成一个**绝对路径**"——
// 没有它，管理面板里连「📝 编辑配置文件」这颗按钮都不会出现（用户只能去终端）。
// 顺带锁住 {brew} 占位符的解析：Intel Mac 的前缀是 /usr/local，写死 /opt/homebrew
// 会让编辑器点开就报"文件不存在"。
func TestLNMPEntriesExposeConfigFile(t *testing.T) {
	want := map[string]string{
		"nginx":   "/nginx/nginx.conf",
		"php82":   "/php/8.2/php.ini",
		"php84":   "/php/8.4/php.ini",
		"mysql84": "/my.cnf",
		"mariadb": "/my.cnf",
	}
	for id, suffix := range want {
		app, ok := FindApp(id)
		if !ok {
			t.Fatalf("目录里找不到 %s", id)
		}
		if strings.TrimSpace(app.ConfigPath) == "" {
			t.Errorf("%s 没有声明 ConfigPath —— 面板里就没有「手动改配置文件」的入口", id)
			continue
		}
		got := ConfigFilePath(app, "/Users/someone", "/tmp/work")
		if !filepath.IsAbs(got) {
			t.Errorf("%s 的配置文件路径必须是绝对路径，实际 %q（ConfigPath=%q）", id, got, app.ConfigPath)
		}
		if !strings.HasSuffix(got, suffix) {
			t.Errorf("%s 的配置文件路径应以 %s 结尾，实际 %s", id, suffix, got)
		}
		if strings.Contains(got, "{brew}") {
			t.Errorf("%s 的 {brew} 占位符没有被展开：%s", id, got)
		}
	}
	// 占位符必须跟随真实前缀，而不是硬编码
	if got := ConfigFilePath(App{ConfigPath: "{brew}/etc/my.cnf"}, "", ""); !strings.HasSuffix(got, "/etc/my.cnf") {
		t.Errorf("{brew} 展开结果不对：%s", got)
	}
}
