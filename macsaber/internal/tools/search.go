package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/fsroot"
	"github.com/zizdog/macsaber/internal/tool"
)

// ============================================================================
//  search.spotlight
// ============================================================================

type searchSpotlight struct{}

func init() { Add(searchSpotlight{}) }

// kindPredicates 是"类型"下拉到 Spotlight 谓词的固定映射（不接受自由文本）。
var kindPredicates = map[string]string{
	"video":    `kMDItemContentTypeTree == "public.movie"`,
	"image":    `kMDItemContentTypeTree == "public.image"`,
	"audio":    `kMDItemContentTypeTree == "public.audio"`,
	"document": `(kMDItemContentTypeTree == "public.text" || kMDItemContentTypeTree == "public.composite-content" || kMDItemContentType == "com.adobe.pdf")`,
}

func (searchSpotlight) Meta() tool.Meta {
	_, ok := execx.LookPath("mdfind")
	reason := ""
	if !ok {
		reason = "缺少系统命令 mdfind"
	}
	return tool.Meta{
		ID: "search.spotlight", Name: "Spotlight 搜索", Category: "search", Icon: "search",
		Summary:   "全文/文件名搜索，越界结果自动丢弃并计数。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 40,
		Params: []tool.Param{
			{Name: "query", Label: "关键词", Type: tool.TypeText, Required: true,
				Placeholder: "文件名或内容关键词", Help: "按文件名或内容匹配，不区分大小写。"},
			{Name: "scope", Label: "范围", Type: tool.TypeSelect, Required: true, Default: "all",
				Options: []tool.Option{
					{Value: "all", Label: "整个索引（再按读根过滤）"},
					{Value: "subdir", Label: "指定目录"},
				}, Help: "指定目录时必须填下面的路径。"},
			{Name: "path", Label: "搜索目录", Type: tool.TypePath,
				Help: "只能读允许的读根内的目录。"},
			{Name: "kind", Label: "类型", Type: tool.TypeSelect, Default: "any",
				Options: []tool.Option{
					{Value: "any", Label: "不限"},
					{Value: "video", Label: "视频"},
					{Value: "image", Label: "图片"},
					{Value: "document", Label: "文档"},
					{Value: "audio", Label: "音频"},
				}, Help: "按 Spotlight 内容类型过滤。"},
			{Name: "limit", Label: "返回条数", Type: tool.TypeNumber, Default: 50,
				Min: Num(1), Max: Num(200), Help: "最多返回 N 条，其余只计数。"},
		},
	}
}

const spotlightMaxScan = 2000

func (searchSpotlight) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	query := strings.TrimSpace(in.Str("query"))
	scope := in.Str("scope")
	kind := in.Str("kind")
	limit := in.Int("limit", 50)

	args := []string{}
	if scope == "subdir" {
		dir := in.Path("path")
		if dir == "" {
			return nil, fmt.Errorf("范围选了指定目录，请填写搜索目录")
		}
		args = append(args, "-onlyin", dir)
	}
	// 关键词要包成正规谓词：裸词与 && 组合会让 mdfind 报 Failed to create query。
	kw := spotlightQuote(query)
	pred := fmt.Sprintf(`(kMDItemFSName == "*%s*"c || kMDItemTextContent == "*%s*"c)`, kw, kw)
	if k := kindPredicates[kind]; k != "" {
		pred = fmt.Sprintf("(%s) && (%s)", pred, k)
	}
	args = append(args, pred)

	res := c.Exec.Run(ctx, 30*time.Second, "mdfind", args...)
	if res.TimedOut {
		return nil, fmt.Errorf("mdfind 超过 30 秒未返回，已终止")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("mdfind 失败（退出码 %d）：%s", res.ExitCode, failureReason(res))
	}

	scanned, filtered, skipped := 0, 0, 0
	items := []map[string]any{}
	for _, ln := range splitOutputLines(res.Stdout) {
		if scanned >= spotlightMaxScan {
			break
		}
		scanned++
		// mdfind 会给出索引里任意位置的路径：逐条重过读根与敏感目录闸门。
		real, gerr := c.Guard.Resolve(fsroot.Read, ln, false)
		if gerr != nil {
			filtered++
			continue
		}
		st, serr := os.Stat(real)
		if serr != nil || st.IsDir() {
			skipped++
			continue
		}
		if len(items) >= limit {
			continue
		}
		items = append(items, map[string]any{
			"path": real, "name": st.Name(), "size": st.Size(),
			"modified": st.ModTime().Format(time.RFC3339),
		})
	}
	data := map[string]any{
		"query": query, "predicate": pred, "scope": scope, "kind": kind,
		"scanned": scanned, "returned": len(items), "filtered": filtered,
		"skipped": skipped, "truncated": res.TruncOut,
		"items": items,
	}
	if res.TruncOut {
		data["note"] = "命令输出超限被截断，统计不完整"
	}
	msg := fmt.Sprintf("返回 %d 条", len(items))
	if filtered > 0 {
		msg = fmt.Sprintf("返回 %d 条，已过滤 %d 条越界结果", len(items), filtered)
	}
	return &tool.Result{OK: true, Msg: msg, Data: data}, nil
}

// spotlightQuote 转义引号与反斜杠：关键词只当字符串值嵌进谓词。
func spotlightQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func splitOutputLines(out string) []string {
	lines := []string{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lines = append(lines, ln)
	}
	return lines
}

// ============================================================================
//  search.metadata
// ============================================================================

type searchMetadata struct{}

func init() { Add(searchMetadata{}) }

// mdlsKeys 是白名单：kMDItemTextContent 这类超长字段一律不回显。
var mdlsKeys = map[string]bool{
	"kMDItemFSName": true, "kMDItemDisplayName": true, "kMDItemKind": true,
	"kMDItemFSSize": true, "kMDItemFSCreationDate": true,
	"kMDItemFSContentChangeDate": true, "kMDItemContentCreationDate": true,
	"kMDItemContentModificationDate": true, "kMDItemContentType": true,
	"kMDItemContentTypeTree": true, "kMDItemFSOwnerUserID": true,
	"kMDItemFSOwnerGroupID": true, "kMDItemFSLabel": true,
	"kMDItemWhereFroms": true, "kMDItemTitle": true, "kMDItemAuthors": true,
	"kMDItemPixelHeight": true, "kMDItemPixelWidth": true,
	"kMDItemDurationSeconds": true, "kMDItemNumberOfPages": true,
}

func (searchMetadata) Meta() tool.Meta {
	_, ok := execx.LookPath("mdls")
	reason := ""
	if !ok {
		reason = "缺少系统命令 mdls"
	}
	return tool.Meta{
		ID: "search.metadata", Name: "文件元数据", Category: "search", Icon: "info",
		Summary:   "读一个文件的 Spotlight 元数据（白名单键）。",
		Available: ok, UnavailableReason: reason, TimeoutSeconds: 25,
		Params: []tool.Param{
			{Name: "path", Label: "文件", Type: tool.TypePath, Required: true,
				Help: "只能读允许的读根内的文件。"},
		},
	}
}

func (searchMetadata) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	src := in.Path("path")
	res, err := execTool(ctx, c, 20*time.Second, "mdls", src)
	if err != nil {
		return nil, err
	}
	vals := parseMDLS(res.Stdout)
	return &tool.Result{
		OK: true, Msg: fmt.Sprintf("读到 %d 个白名单字段", len(vals)),
		Data: map[string]any{"path": src, "count": len(vals), "values": vals},
	}, nil
}

func parseMDLS(out string) map[string]any {
	vals := map[string]any{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		i := strings.Index(ln, "=")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(ln[:i])
		if !mdlsKeys[key] {
			continue
		}
		v := strings.TrimSpace(ln[i+1:])
		if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
			v = v[1 : len(v)-1]
		}
		if len(v) > 300 {
			v = v[:300] + "…"
		}
		vals[key] = v
	}
	return vals
}
