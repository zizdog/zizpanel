package services

import (
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
