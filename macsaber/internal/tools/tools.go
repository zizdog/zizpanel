// Package tools 是具体工具包。本轮只放三个示例（别的子代理往这里加）。
//
// 注册方式：在 init() 里 Add(...)，web 层启动时调 RegisterAll(reg)。
package tools

import "github.com/zizdog/macsaber/internal/tool"

// registered 是包级登记表；init 顺序即展示顺序。
var registered []tool.Doer

// Add 登记一个工具（子代理新增工具照抄这一行）。
func Add(d tool.Doer) { registered = append(registered, d) }

// RegisterAll 把本包全部工具注册进注册表。
func RegisterAll(reg *tool.Registry) {
	for _, d := range registered {
		reg.Register(d)
	}
}

// Num 是构造 Param 边界的小工具（Min/Max 是指针）。
func Num(f float64) *float64 { return &f }

// sipsFormats 是 sips 支持的常用输出格式（与 img.convert 的 select 一致）。
var sipsFormats = []string{"jpeg", "png", "tiff", "heic", "gif", "bmp"}
