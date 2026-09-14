package web

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
//  操作审计：检索、分页、导出
//
//  原实现只有「取最近 N 条」（GET /api/v1/audit?limit=N），于是这个页面一直是
//  占位页 —— 而审计日志的价值全在"能不能查到某一次操作"上：谁在什么时候改了
//  哪个站点、哪次登录失败、哪次升级失败。所以这里补的是**检索**。
//
//  设计要点：
//
//  1. **游标分页而不是 OFFSET。** 审计表是只增的：翻页期间新记录插进来，
//     OFFSET 会让同一页内容漂移（用户看到重复或漏掉的记录）。
//     用 `id < before_id` 就稳定了，代价是只能"往前翻"——而审计本来就是
//     从最新往回看，正合适。
//
//  2. **全部走参数化查询。** 这是一个用户可控的搜索接口，拼字符串就是注入口。
//     另外把 LIKE 的通配符转义掉：不转义的话，用户输入 `%` 会变成"匹配一切"
//     （结果看起来像搜索失灵），`_` 会变成任意单字符。
//
//  3. **导出也要受限**。日志可能很大，导出加一个上限（exportLimit）并在
//     响应头里说明；同时导出这个动作**本身也要记一条审计**——
//     把审计日志整段带走是敏感操作。
// ============================================================================

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 500
	auditExportLimit  = 20000
)

// auditEntry 是一条审计记录（也是 JSON 响应的元素形状）。
type auditEntry struct {
	ID     int64  `json:"id"`
	Ts     string `json:"ts"`
	Actor  string `json:"actor"`
	IP     string `json:"ip"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	OK     bool   `json:"ok"`
	Msg    string `json:"message"`
}

// auditFilter 是从查询参数解析出来的筛选条件。
type auditFilter struct {
	Q      string
	Action string
	Target string
	Actor  string
	OK     *bool
	From   string
	To     string
}

// parseAuditFilter 解析并校验查询参数。
//
// 时间参数接受 `YYYY-MM-DD` 或 `YYYY-MM-DD HH:MM:SS`：
// 只给日期时，"到"要补齐到当天 23:59:59 —— 否则用户选"今天到明天"
// 会漏掉明天整天的记录（这是日期筛选最常见的坑）。
func parseAuditFilter(q map[string][]string) (auditFilter, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	f := auditFilter{
		Q:      get("q"),
		Action: get("action"),
		Target: get("target"),
		Actor:  get("actor"),
	}
	if v := get("ok"); v != "" {
		switch v {
		case "1", "true":
			b := true
			f.OK = &b
		case "0", "false":
			b := false
			f.OK = &b
		default:
			return f, fmt.Errorf("ok 只能是 1 或 0")
		}
	}
	if v := get("from"); v != "" {
		norm, err := normalizeAuditTime(v, false)
		if err != nil {
			return f, err
		}
		f.From = norm
	}
	if v := get("to"); v != "" {
		norm, err := normalizeAuditTime(v, true)
		if err != nil {
			return f, err
		}
		f.To = norm
	}
	return f, nil
}

// normalizeAuditTime 校验时间格式；endOfDay=true 时把纯日期补齐到当天末尾。
func normalizeAuditTime(v string, endOfDay bool) (string, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			if layout == "2006-01-02" {
				if endOfDay {
					t = t.Add(24*time.Hour - time.Second)
				}
				return t.Format("2006-01-02 15:04:05"), nil
			}
			return t.Format("2006-01-02 15:04:05"), nil
		}
	}
	return "", fmt.Errorf("时间格式不对（应为 YYYY-MM-DD 或 YYYY-MM-DD HH:MM:SS）: %s", v)
}

// likeEscape 转义 LIKE 的通配符。
//
// 不转义的话用户输入 `%` 等于"匹配一切"，看起来像搜索坏了；
// `_` 会匹配任意单字符。反斜杠本身要先转义，否则 `\%` 会被误解析。
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// where 生成 WHERE 子句与参数。全部参数化，绝不拼用户输入。
func (f auditFilter) where() (string, []any) {
	var conds []string
	var args []any

	if f.Q != "" {
		pat := "%" + likeEscape(f.Q) + "%"
		conds = append(conds, `(action LIKE ? ESCAPE '\' OR target LIKE ? ESCAPE '\'`+
			` OR detail LIKE ? ESCAPE '\' OR message LIKE ? ESCAPE '\'`+
			` OR actor LIKE ? ESCAPE '\' OR ip LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat, pat, pat, pat)
	}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, f.Action)
	}
	if f.Target != "" {
		conds = append(conds, `target LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(f.Target)+"%")
	}
	if f.Actor != "" {
		conds = append(conds, "actor = ?")
		args = append(args, f.Actor)
	}
	if f.OK != nil {
		v := 0
		if *f.OK {
			v = 1
		}
		conds = append(conds, "ok = ?")
		args = append(args, v)
	}
	if f.From != "" {
		conds = append(conds, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "ts <= ?")
		args = append(args, f.To)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// queryAudit 按条件查询，返回记录与"是否还有更多"。
//
// beforeID > 0 时只取 id 更小的（游标分页）。多查一条用来判断 has_more，
// 这样前端不需要额外请求就能决定要不要显示"加载更多"。
func (s *Server) queryAudit(r *http.Request, f auditFilter, limit int, beforeID int64) ([]auditEntry, bool, error) {
	where, args := f.where()
	q := `SELECT id,ts,actor,ip,action,target,detail,ok,message FROM audit_logs` + where
	if beforeID > 0 {
		if where == "" {
			q += " WHERE id < ?"
		} else {
			q += " AND id < ?"
		}
		args = append(args, beforeID)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.Store.DB().QueryContext(r.Context(), q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []auditEntry
	for rows.Next() {
		var e auditEntry
		var okInt int
		if err := rows.Scan(&e.ID, &e.Ts, &e.Actor, &e.IP, &e.Action, &e.Target,
			&e.Detail, &okInt, &e.Msg); err != nil {
			continue
		}
		e.OK = okInt == 1
		out = append(out, e)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// auditCounts 返回当前筛选下的总数与失败数（页头那句"共 N 条，失败 M 条"）。
func (s *Server) auditCounts(r *http.Request, f auditFilter) (total, failed int) {
	where, args := f.where()
	row := s.Store.DB().QueryRowContext(r.Context(),
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN ok = 0 THEN 1 ELSE 0 END), 0) FROM audit_logs`+where,
		args...)
	_ = row.Scan(&total, &failed)
	return total, failed
}

// handleAuditList 是审计页的主接口：检索 + 游标分页。
//
// 响应同时带上 total/failed 与 has_more/next_before_id，让前端一次请求就能
// 渲染出"结果概要 + 列表 + 是否还能翻"。分页靠 next_before_id（不是 offset）。
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := parseAuditFilter(q)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	limit := auditDefaultLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}

	var beforeID int64
	if v := q.Get("before_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			beforeID = n
		}
	}

	list, hasMore, err := s.queryAudit(r, f, limit, beforeID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取审计日志失败: "+err.Error())
		return
	}
	if list == nil {
		list = []auditEntry{}
	}

	total, failed := s.auditCounts(r, f)

	next := int64(0)
	if hasMore && len(list) > 0 {
		next = list[len(list)-1].ID
	}

	ok(w, map[string]any{
		"list":           list,
		"total":          total,
		"failed":         failed,
		"has_more":       hasMore,
		"next_before_id": next,
		"limit":          limit,
	})
}

// handleAuditFacets 返回可用的筛选项（动作 / 操作者 / 来源 IP）。
//
// 为什么要有它：动作名是代码里的字符串（如 site_create、upgrade_stage），
// 让用户在一个空输入框里手敲这些名字等于没有筛选。这里把库里真实出现过的
// 值列出来给下拉框用，顺带给出每个值的条数（便于判断哪些是噪音）。
func (s *Server) handleAuditFacets(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DB().QueryContext(r.Context(),
		`SELECT action, COUNT(*) c FROM audit_logs GROUP BY action ORDER BY c DESC, action`)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取动作列表失败: "+err.Error())
		return
	}
	defer rows.Close()

	type facet struct {
		Value string `json:"value"`
		Count int    `json:"count"`
	}
	actions := []facet{}
	for rows.Next() {
		var v string
		var c int
		if err := rows.Scan(&v, &c); err == nil {
			actions = append(actions, facet{Value: v, Count: c})
		}
	}

	actors := []facet{}
	if rows2, err := s.Store.DB().QueryContext(r.Context(),
		`SELECT actor, COUNT(*) c FROM audit_logs WHERE actor <> '' GROUP BY actor ORDER BY c DESC`); err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var v string
			var c int
			if err := rows2.Scan(&v, &c); err == nil {
				actors = append(actors, facet{Value: v, Count: c})
			}
		}
	}

	ok(w, map[string]any{"actions": actions, "actors": actors})
}

// handleAuditExport 按当前筛选导出 CSV 或 JSON（附带下载头）。
//
// 为什么要有：审计的价值在于"把某段时间的操作记录拿出去看/存档"，
// 只在网页上翻页是做不到的。导出用同一套筛选条件，所以页面上看到什么就导出什么。
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := parseAuditFilter(q)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		fail(w, http.StatusBadRequest, "format 只能是 csv 或 json")
		return
	}

	// 导出上限：日志可能很大，不能让它把内存和响应都撑爆。
	list, _, err := s.queryAudit(r, f, auditExportLimit, 0)
	if err != nil {
		fail(w, http.StatusInternalServerError, "导出失败: "+err.Error())
		return
	}

	stamp := time.Now().Format("20060102-150405")
	name := fmt.Sprintf("zizpanel-audit-%s.%s", stamp, format)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"exported_at": time.Now().Format("2006-01-02 15:04:05"),
			"count":       len(list),
			"limit":       auditExportLimit,
			"list":        list,
		})
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		// Excel 对无 BOM 的 UTF-8 CSV 会按本地编码解，中文变乱码。
		_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"时间", "操作者", "来源IP", "动作", "目标", "结果", "说明"})
		for _, e := range list {
			res := "成功"
			if !e.OK {
				res = "失败"
			}
			detail := e.Detail
			if e.Msg != "" {
				if detail != "" {
					detail += " | "
				}
				detail += e.Msg
			}
			_ = cw.Write([]string{e.Ts, e.Actor, e.IP, e.Action, e.Target, res, detail})
		}
		cw.Flush()
	}

	// 把审计日志整段带走是敏感操作，本身要留痕。
	s.audit(r, "audit_export", "",
		fmt.Sprintf("格式=%s 条数=%d 筛选=%s", format, len(list), f.describe()), true, "")
}

// describe 把筛选条件描述成一句人话（写进审计记录，便于事后理解那次导出拿走了什么）。
func (f auditFilter) describe() string {
	var parts []string
	if f.Q != "" {
		parts = append(parts, "关键词="+f.Q)
	}
	if f.Action != "" {
		parts = append(parts, "动作="+f.Action)
	}
	if f.Target != "" {
		parts = append(parts, "目标="+f.Target)
	}
	if f.Actor != "" {
		parts = append(parts, "操作者="+f.Actor)
	}
	if f.OK != nil {
		if *f.OK {
			parts = append(parts, "结果=成功")
		} else {
			parts = append(parts, "结果=失败")
		}
	}
	if f.From != "" {
		parts = append(parts, "从="+f.From)
	}
	if f.To != "" {
		parts = append(parts, "到="+f.To)
	}
	if len(parts) == 0 {
		return "无（全部）"
	}
	return strings.Join(parts, " ")
}
