package services

import (
	"testing"
	"unicode/utf8"
)

// 应用市场卡片只显示 Summary，但 Description 会出现在详情里。
// 2026-09 用户反馈"应用市场里的软件有些描述太多了！简单一句就行" ——
// 于是每个条目压缩成 1~2 句，并用这条测试防止描述再膨胀回去：
//   - Description 不超过 240 个字符（rune，中文按字算）；
//   - Summary 必须有（前端卡片就显示它，空字符串等于卡片没标题）。
//
// 上限用 rune 而不是字节：中文一个字是 3 字节，按字节算会把
// 正常的一句话误判成超长。
func TestCatalogDescriptionsStayShort(t *testing.T) {
	apps := Catalog()
	if len(apps) == 0 {
		t.Fatal("应用目录为空")
	}
	for _, a := range apps {
		if a.Summary == "" {
			t.Errorf("应用 %s 缺少 Summary（市场卡片要显示它）", a.ID)
		}
		if n := utf8.RuneCountInString(a.Description); n > 240 {
			t.Errorf("应用 %s 的 Description 有 %d 个字符，超过 240 —— "+
				"市场卡片会被大段文字淹没，请压到 1~2 句", a.ID, n)
		}
	}
}
