package web

// api_disks_plist.go —— 给磁盘工具用的极简 XML plist 解析器。
//
// 为什么手写而不是引三方库：
//   · 面板的硬约束是"不引入依赖、无构建步骤"；
//   · `diskutil list/info/apfs list -plist` 输出的 plist 只是 Property List 的
//     一个很小子集（dict / array / string / integer / real / true / false / date / data），
//     没有 base64 二进制、没有嵌套实体、没有外部引用；
//   · 手写这 150 行的好处是**解析失败时能给出人话错误**，而不是把畸形输出
//     静默吞成一个空结构，最后在界面上显示成"这台机器没有磁盘"。
//
// 重要纪律：解析失败一律返回 error，调用方必须如实报错，**绝不降级成空列表**。
// 磁盘列表是破坏性操作的前置判据（"这块盘在不在、是不是系统盘"），
// 一份空列表会被误读成"没有可操作的盘"，比报错更危险。

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// parsePlist 把 diskutil 的 XML plist 解析成 Go 值：
// string → string，integer → int64，real → float64，true/false → bool，
// dict → map[string]any，array → []any，date/data → string（原样）。
func parsePlist(data []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// diskutil 的 plist 带 DOCTYPE，Strict 默认即可；关掉 HTML 实体解析的意外行为。
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("plist 解析失败：文档里没有找到值")
		}
		if err != nil {
			return nil, fmt.Errorf("plist 解析失败：%w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if strings.EqualFold(se.Name.Local, "plist") {
			continue // 进入 <plist>，下一个元素才是根值
		}
		v, err := parsePlistValue(dec, se)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
}

// nextPlistStart 读出下一个开始元素，跳过空白文本（plist 里 key 与 value 之间有换行/缩进）。
func nextPlistStart(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t, nil
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return xml.StartElement{}, fmt.Errorf("plist 解析失败：值前面出现意外文本 %q", string(t))
			}
		}
	}
}

func parsePlistValue(dec *xml.Decoder, start xml.StartElement) (any, error) {
	switch strings.ToLower(start.Name.Local) {
	case "dict":
		m := map[string]any{}
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("plist 解析失败：<dict> 未闭合：%w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if !strings.EqualFold(t.Name.Local, "key") {
					return nil, fmt.Errorf("plist 解析失败：<dict> 里出现 <%s>（应为 <key>）", t.Name.Local)
				}
				var key string
				if err := dec.DecodeElement(&key, &t); err != nil {
					return nil, fmt.Errorf("plist 解析失败：读取 <key> 失败：%w", err)
				}
				vs, err := nextPlistStart(dec)
				if err != nil {
					return nil, fmt.Errorf("plist 解析失败：键 %q 没有对应的值：%w", key, err)
				}
				val, err := parsePlistValue(dec, vs)
				if err != nil {
					return nil, err
				}
				m[key] = val
			case xml.EndElement:
				if strings.EqualFold(t.Name.Local, "dict") {
					return m, nil
				}
			}
		}
	case "array":
		arr := []any{}
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("plist 解析失败：<array> 未闭合：%w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				val, err := parsePlistValue(dec, t)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			case xml.EndElement:
				if strings.EqualFold(t.Name.Local, "array") {
					return arr, nil
				}
			}
		}
	case "string", "date", "data":
		var s string
		if err := dec.DecodeElement(&s, &start); err != nil {
			return nil, fmt.Errorf("plist 解析失败：读取 <%s> 失败：%w", start.Name.Local, err)
		}
		return s, nil
	case "integer":
		var s string
		if err := dec.DecodeElement(&s, &start); err != nil {
			return nil, fmt.Errorf("plist 解析失败：读取 <integer> 失败：%w", err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("plist 解析失败：整数 %q 无法解析", strings.TrimSpace(s))
		}
		return n, nil
	case "real":
		var s string
		if err := dec.DecodeElement(&s, &start); err != nil {
			return nil, fmt.Errorf("plist 解析失败：读取 <real> 失败：%w", err)
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("plist 解析失败：实数 %q 无法解析", strings.TrimSpace(s))
		}
		return f, nil
	case "true", "false":
		if err := dec.Skip(); err != nil {
			return nil, fmt.Errorf("plist 解析失败：读取 <%s> 失败：%w", start.Name.Local, err)
		}
		return strings.EqualFold(start.Name.Local, "true"), nil
	}
	return nil, fmt.Errorf("plist 解析失败：不认识的元素 <%s>", start.Name.Local)
}

// ---------- 取值助手（都对缺失/类型不符保持沉默，由调用方决定默认值） ----------

func plistMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func plistArray(v any) []any {
	a, _ := v.([]any)
	return a
}

func plistStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "true"
		}
		return "false"
	}
	return ""
}

// plistBool 把 bool 与 0/1 整数都折算成 bool。
func plistBool(m map[string]any, key string) bool {
	v, _ := plistBoolField(m, key)
	return v
}

// plistBoolField 额外返回"这个键到底在不在"——加密状态必须区分
// "系统说没加密"和"系统压根没给这个字段"，后者要如实显示"未复核"。
func plistBoolField(m map[string]any, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[key]
	if !ok {
		return false, false
	}
	switch t := v.(type) {
	case bool:
		return t, true
	case int64:
		return t != 0, true
	case float64:
		return t != 0, true
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		if s == "true" || s == "yes" || s == "1" {
			return true, true
		}
		if s == "false" || s == "no" || s == "0" {
			return false, true
		}
	}
	return false, false
}

func plistInt(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return 0
}

func plistStrSlice(m map[string]any, key string) []string {
	out := []string{}
	for _, it := range plistArray(m[key]) {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
