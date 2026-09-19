package web

// api_nav.go —— 「导航页」的面板接口与独立别名页。
//
// 设计取舍（为什么自己开发而不是原生部署 sun-panel / home-dash）：
// 现成项目要么需要 Node/Vue 构建链，要么自带一套运行时/数据库/鉴权/端口 ——
// 等于在面板之外再养一套「要备份、要升级、要鉴权、要单独记口令」的系统。
// 导航页的数据量极小，做进面板可以直接复用：面板鉴权、面板风格、备份/恢复、
// 日志与审计，升级时也只有一个二进制要换。
//
// 两个访问面：
//   1. 面板内页面 `#/nav`（前端 nav.js）走下面这些 /api/v1/nav/* 接口，**全部 requireAuth**；
//      所有写操作进审计。
//   2. 独立别名页 `GET /nav/`（assets/nav/index.html）注册在**安全后缀之外**，
//      未登录也能看（只读），数据来自同文件的 `GET /nav/data`（公开、只读、无敏感信息）。
//      编辑必须登录面板：页面通过 `GET /nav/whoami` 问「我登录了吗」，
//      未登录时不渲染任何编辑入口，登录后才给出回面板编辑的链接。
//
// 别名页要求登录吗：不要求 —— 它是用户的浏览器首页，必须能匿名打开。
// 但它仍受全局 accessControl（IP 白名单）约束：外层 mux 就是被它包住的，
// 所以「仅本机」模式下 /nav/ 也只能本机访问，与面板一致。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zizdog/zizpanel/internal/store"
)

// 上限刻意保守：导航页是给人看的首页，不是书签同步服务。
// 有上限才谈得上「导入了什么」可被审计、界面不会卡死。
const (
	navMaxGroups  = 200
	navMaxItems   = 3000
	navMaxNameLen = 64
	navMaxDescLen = 200
	navMaxURLLen  = 2048
	navMaxIconLen = 2048
)

// navEnsure 确保两张表存在。
//
// 面板启动时已经建过一次（见 cmd/zizpanel/main.go），这里再调用是幂等的
// 「第一次请求就能用」的兜底：单测直接构造 Server、或升级后进程还没重启完成时，
// 都可能出现「表还没建但请求已经来了」。CREATE TABLE IF NOT EXISTS 是空操作。
func (s *Server) navEnsure(ctx context.Context) error {
	return store.EnsureNavTables(ctx, s.Store.DB())
}

// navID 解析路径里的数字 ID。错误文案是导航页自己的（不要复用 pathID 的
// 「非法的任务 ID」—— 用户看到「任务」会不知道自己在哪一页）。
func navID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("非法的导航条目 ID: %s", raw)
	}
	return id, nil
}

// ---------- 校验 ----------

// reScheme 匹配「带协议头」的字符串（javascript:、data:、file: …）。
// 只用于判定「这是不是一条 URL」；真正的协议白名单在 validateNavURL 里。
var reScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*:`)

// validateNavURL 只放行 http/https。
//
// 为什么必须白名单而不是黑名单：`javascript:alert(1)` 只是一个样本，
// data:/vbscript:/file: 以及各种大小写混写都属于同一类。判据是
// 「url.Parse 出来的 scheme 必须是 http 或 https，且必须有主机名」。
func validateNavURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", errors.New("站点地址不能为空")
	}
	if len(v) > navMaxURLLen {
		return "", fmt.Errorf("站点地址过长（上限 %d 字符）", navMaxURLLen)
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", errors.New("站点地址不是合法的 URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", errors.New("站点地址只允许 http:// 或 https://（javascript: 等协议已被拒绝）")
	}
	if u.Host == "" {
		return "", errors.New("站点地址缺少主机名")
	}
	return v, nil
}

// validateNavIcon 校验图标：允许「一个 emoji / 短文本」或「http(s) 图片地址」，二选一或都空。
//
// 安全点：`javascript:...` 之类带协议头的串一律按 URL 处理并要求 http/https，
// 绝不允许它落成「短文本」被前端当图片地址用。
func validateNavIcon(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if len(v) > navMaxIconLen {
		return "", fmt.Errorf("图标内容过长（上限 %d 字符）", navMaxIconLen)
	}
	if reScheme.MatchString(v) {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return "", errors.New("图标地址不是合法的 URL")
		}
		if sc := strings.ToLower(u.Scheme); sc != "http" && sc != "https" {
			return "", errors.New("图标只支持 http:// 或 https:// 图片地址，或一个 emoji")
		}
		return v, nil
	}
	// 面板自己托管的本地图标（上传后在"已上传"里选出来的那个）。
	//
	// 只放行这一种形状：`/nav/icons/<16 位内容哈希>.<白名单扩展名>` ——
	// 它由 handleNavIconUpload 生成，读取端同样用这条判据挡路径穿越。
	// 其它任何相对路径（`//evil.com`、`../`、`/api/...`）都不在这里放行，
	// 会落到下面"必须是 emoji"的分支被拒。
	if navLocalIconRe.MatchString(v) {
		return v, nil
	}
	if utf8.RuneCountInString(v) > 8 {
		return "", errors.New("图标只支持一个 emoji、http(s) 图片地址，或面板里上传的本地图标")
	}
	return v, nil
}

// navCleanText 折叠空白并裁剪长度。
//
// 为什么要折叠：名称/描述会直接渲染成一行的标题与说明，用户从别处
// 复制来的内容常带换行与多余空格，不处理会让网格高度参差不齐。
func navCleanText(raw string, max int, what string) (string, error) {
	v := strings.Join(strings.Fields(raw), " ")
	if v == "" {
		return "", fmt.Errorf("%s不能为空", what)
	}
	if utf8.RuneCountInString(v) > max {
		return "", fmt.Errorf("%s过长（上限 %d 个字符）", what, max)
	}
	return v, nil
}

// navCleanOptional 与 navCleanText 相同，但允许空串（描述可以为空）。
func navCleanOptional(raw string, max int, what string) (string, error) {
	v := strings.Join(strings.Fields(raw), " ")
	if utf8.RuneCountInString(v) > max {
		return "", fmt.Errorf("%s过长（上限 %d 个字符）", what, max)
	}
	return v, nil
}

// ---------- 分组接口 ----------

func (s *Server) handleNavGroupsList(w http.ResponseWriter, r *http.Request) {
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, err := s.Store.ListNavGroups(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取导航分组失败: "+err.Error())
		return
	}
	ok(w, map[string]any{"list": list})
}

type navGroupReq struct {
	Name string `json:"name"`
	// Sort 用指针区分「没传」与「传了 0」：0 是合法的排序值。
	Sort *int `json:"sort"`
}

func (s *Server) handleNavGroupCreate(w http.ResponseWriter, r *http.Request) {
	var req navGroupReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := navCleanText(req.Name, navMaxNameLen, "分组名称")
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, err := s.navGroupCount(r.Context()); err == nil && n >= navMaxGroups {
		fail(w, http.StatusBadRequest, fmt.Sprintf("分组数量已达上限（%d 个）", navMaxGroups))
		return
	}
	sort := -1
	if req.Sort != nil {
		sort = *req.Sort
	}
	g, err := s.Store.CreateNavGroup(r.Context(), name, sort)
	if err != nil {
		s.audit(r, "nav_group_create", name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "创建分组失败: "+err.Error())
		return
	}
	s.audit(r, "nav_group_create", name, fmt.Sprintf("id=%d", g.ID), true, "")
	ok(w, g)
}

func (s *Server) handleNavGroupUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := navID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var req navGroupReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := navCleanText(req.Name, navMaxNameLen, "分组名称")
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sort := -1
	if req.Sort != nil {
		sort = *req.Sort
	}
	if err := s.Store.UpdateNavGroup(r.Context(), id, name, sort); err != nil {
		if errors.Is(err, store.ErrNavNotFound) {
			fail(w, http.StatusNotFound, "分组不存在（可能已被删除，请刷新）")
			return
		}
		s.audit(r, "nav_group_update", name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "更新分组失败: "+err.Error())
		return
	}
	s.audit(r, "nav_group_update", name, fmt.Sprintf("id=%d", id), true, "")
	g, _ := s.Store.GetNavGroup(r.Context(), id)
	ok(w, g)
}

func (s *Server) handleNavGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, err := navID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	g, _ := s.Store.GetNavGroup(r.Context(), id)
	if err := s.Store.DeleteNavGroup(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNavNotFound) {
			fail(w, http.StatusNotFound, "分组不存在（可能已被删除，请刷新）")
			return
		}
		fail(w, http.StatusInternalServerError, "删除分组失败: "+err.Error())
		return
	}
	target := fmt.Sprintf("id=%d", id)
	if g != nil {
		target = g.Name
	}
	s.audit(r, "nav_group_delete", target, "删除分组及其下所有站点", true, "")
	ok(w, map[string]any{"msg": "分组已删除"})
}

type navReorderReq struct {
	IDs []int64 `json:"ids"`
}

func (s *Server) handleNavGroupReorder(w http.ResponseWriter, r *http.Request) {
	var req navReorderReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.IDs) == 0 {
		fail(w, http.StatusBadRequest, "缺少要排序的分组 id 列表")
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.ReorderNavGroups(r.Context(), req.IDs); err != nil {
		s.audit(r, "nav_group_reorder", "groups", "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "保存分组顺序失败: "+err.Error())
		return
	}
	s.audit(r, "nav_group_reorder", "groups", fmt.Sprintf("%d 个分组", len(req.IDs)), true, "")
	ok(w, map[string]any{"msg": "顺序已保存"})
}

// ---------- 站点接口 ----------

func (s *Server) handleNavItemsList(w http.ResponseWriter, r *http.Request) {
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, err := s.Store.ListNavItems(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取导航站点失败: "+err.Error())
		return
	}
	// ?group_id=N 是给 API 使用者的便利过滤（面板前端用整棵树，不依赖它）。
	if raw := r.URL.Query().Get("group_id"); raw != "" {
		gid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || gid <= 0 {
			fail(w, http.StatusBadRequest, "group_id 必须是正整数")
			return
		}
		filtered := make([]store.NavItem, 0, len(list))
		for _, it := range list {
			if it.GroupID == gid {
				filtered = append(filtered, it)
			}
		}
		list = filtered
	}
	ok(w, map[string]any{"list": list})
}

type navItemReq struct {
	GroupID     int64  `json:"group_id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	// OpenNewTab 用指针：不传时默认「新标签打开」（sun-panel 的默认手感）。
	OpenNewTab *bool `json:"open_new_tab"`
	Sort       *int  `json:"sort"`
}

// buildNavItem 把请求体校验成一条可写入的记录。
func (s *Server) buildNavItem(ctx context.Context, req navItemReq) (store.NavItem, error) {
	it := store.NavItem{}
	if req.GroupID <= 0 {
		return it, errors.New("必须指定所属分组")
	}
	if _, err := s.Store.GetNavGroup(ctx, req.GroupID); err != nil {
		if errors.Is(err, store.ErrNavNotFound) {
			return it, errors.New("所属分组不存在（可能已被删除，请刷新）")
		}
		return it, err
	}
	name, err := navCleanText(req.Name, navMaxNameLen, "站点名称")
	if err != nil {
		return it, err
	}
	link, err := validateNavURL(req.URL)
	if err != nil {
		return it, err
	}
	icon, err := validateNavIcon(req.Icon)
	if err != nil {
		return it, err
	}
	desc, err := navCleanOptional(req.Description, navMaxDescLen, "站点描述")
	if err != nil {
		return it, err
	}
	it.GroupID = req.GroupID
	it.Name = name
	it.URL = link
	it.Icon = icon
	it.Description = desc
	it.OpenNewTab = true
	if req.OpenNewTab != nil {
		it.OpenNewTab = *req.OpenNewTab
	}
	it.Sort = -1
	if req.Sort != nil {
		it.Sort = *req.Sort
	}
	return it, nil
}

func (s *Server) handleNavItemCreate(w http.ResponseWriter, r *http.Request) {
	var req navItemReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	it, err := s.buildNavItem(r.Context(), req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if n, err := s.navItemCount(r.Context()); err == nil && n >= navMaxItems {
		fail(w, http.StatusBadRequest, fmt.Sprintf("站点数量已达上限（%d 个）", navMaxItems))
		return
	}
	created, err := s.Store.CreateNavItem(r.Context(), it)
	if err != nil {
		s.audit(r, "nav_item_create", it.Name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "创建站点失败: "+err.Error())
		return
	}
	s.audit(r, "nav_item_create", it.Name, "地址="+it.URL, true, "")
	ok(w, created)
}

func (s *Server) handleNavItemUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := navID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var req navItemReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur, err := s.Store.GetNavItem(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNavNotFound) {
			fail(w, http.StatusNotFound, "站点不存在（可能已被删除，请刷新）")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 允许部分更新：没传 group_id 就沿用原分组，没传 sort 就沿用原顺序。
	if req.GroupID == 0 {
		req.GroupID = cur.GroupID
	}
	it, err := s.buildNavItem(r.Context(), req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	it.ID = id
	if err := s.Store.UpdateNavItem(r.Context(), it); err != nil {
		if errors.Is(err, store.ErrNavNotFound) {
			fail(w, http.StatusNotFound, "站点不存在（可能已被删除，请刷新）")
			return
		}
		s.audit(r, "nav_item_update", it.Name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "更新站点失败: "+err.Error())
		return
	}
	s.audit(r, "nav_item_update", it.Name, "地址="+it.URL, true, "")
	updated, _ := s.Store.GetNavItem(r.Context(), id)
	ok(w, updated)
}

func (s *Server) handleNavItemDelete(w http.ResponseWriter, r *http.Request) {
	id, err := navID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	it, _ := s.Store.GetNavItem(r.Context(), id)
	if err := s.Store.DeleteNavItem(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNavNotFound) {
			fail(w, http.StatusNotFound, "站点不存在（可能已被删除，请刷新）")
			return
		}
		fail(w, http.StatusInternalServerError, "删除站点失败: "+err.Error())
		return
	}
	target := fmt.Sprintf("id=%d", id)
	if it != nil {
		target = it.Name
	}
	s.audit(r, "nav_item_delete", target, "删除导航站点", true, "")
	ok(w, map[string]any{"msg": "站点已删除"})
}

func (s *Server) handleNavItemReorder(w http.ResponseWriter, r *http.Request) {
	var req navReorderReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.IDs) == 0 {
		fail(w, http.StatusBadRequest, "缺少要排序的站点 id 列表")
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.ReorderNavItems(r.Context(), req.IDs); err != nil {
		s.audit(r, "nav_item_reorder", "items", "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "保存站点顺序失败: "+err.Error())
		return
	}
	s.audit(r, "nav_item_reorder", "items", fmt.Sprintf("%d 个站点", len(req.IDs)), true, "")
	ok(w, map[string]any{"msg": "顺序已保存"})
}

// ---------- 整棵树 ----------

// navTree 返回分组 + 站点（面板页与独立页共用同一形状）。
func (s *Server) navTree(ctx context.Context) (map[string]any, error) {
	groups, err := s.Store.ListNavGroups(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.Store.ListNavItems(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := s.Store.NavSettings(ctx)
	if err != nil {
		return nil, err
	}
	// 外观设置随数据一起返回：**公开别名页（未登录）也必须能拿到** ——
	// 标题 / 主题色 / 背景图 / 三件套都是给访问者看的，不含任何敏感信息。
	// themes 是 8 套色系表：前端据此渲染"主题色卡"与渐变/光晕，服务端是唯一真相源。
	return map[string]any{"groups": groups, "items": items, "settings": settings, "themes": navThemes}, nil
}

func (s *Server) handleNavTree(w http.ResponseWriter, r *http.Request) {
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	tree, err := s.navTree(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取导航数据失败: "+err.Error())
		return
	}
	ok(w, tree)
}

// ---------- 外观设置（标题 / 副标题 / 主题色 / 背景图）----------
//
// 用户 2026-09-18 要求：「标题要可以改！要可以自定义背景图！要可以指定主题色！」
// 数据存导航页自己的 nav_settings 表（跟分组/站点一起进备份，见 store/nav.go）。

// 上限与面板其它一行文案一致：标题是标题，不是正文。
const (
	navMaxTitleLen    = 40
	navMaxSubtitleLen = 60
)

// navAccentRe 是主题色的**唯一**合法形状：#rrggbb（六个十六进制位）。
//
// 为什么卡这么死：主题色会直接拼进 CSS 自定义属性。命名色、rgb()/hsl()、
// 8 位带透明度、以及任何含分号/括号的串都可能被拿来注入（例如
// `#fff; background-image:url(...)`）。判据是"能安全拼进 CSS 的最小形状"，
// 不是"看起来像颜色"。
var navAccentRe = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// ---------- 外观色系（8 套）----------
//
// 用户 2026-09-18 要求：照抄参考首页的观感 —— 外观模式（自动/亮色/暗色）、
// 8 套主题色系（每套含 light/dark 的渐变与光晕色）、卡片尺寸（小/中/大）。
//
// 为什么颜色表放在**服务端**（而不是两份前端各抄一遍）：
//
//	· 公开别名页的 nav.js 必须自包含（不能 import 面板资源），面板内页面又是另一个
//	  文件 —— 颜色表抄两份，迟早只改一份（这正是"离散真相源"那一类坑）；
//	· 校验用的白名单也要与颜色表的 id 一致，放在一起才不会"能存不能用"。
//
// 前端从 /nav/data（公开）或 /api/v1/nav/tree 里的 `themes` 字段读取。
type navThemeVariant struct {
	// Grad 是背景渐变的 2~3 个色停（左→右）。
	Grad []string `json:"grad"`
	// Orb 是两个漂移光晕的颜色。
	Orb []string `json:"orb"`
	// Accent 是这套色系自带的强调色（用户没自定义 accent 时用它）。
	Accent string `json:"accent"`
}

type navTheme struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Light navThemeVariant `json:"light"`
	Dark  navThemeVariant `json:"dark"`
}

// navThemes 是照抄参考页的 8 套色系（颜色值逐字取自考参文件）。
var navThemes = []navTheme{
	{ID: "neon", Name: "霓虹",
		Light: navThemeVariant{Grad: []string{"#6a7ef0", "#8f5fe8", "#e96fc0"}, Orb: []string{"#7c5cff", "#ff5cc8"}, Accent: "#7c5cff"},
		Dark:  navThemeVariant{Grad: []string{"#150a2e", "#2a1250", "#3d1147"}, Orb: []string{"#8b5cf6", "#22d3ee"}, Accent: "#8b5cf6"}},
	{ID: "aurora", Name: "极光",
		Light: navThemeVariant{Grad: []string{"#5ee7df", "#7f9cf5", "#c8a2e0"}, Orb: []string{"#34d399", "#60a5fa"}, Accent: "#14b8a6"},
		Dark:  navThemeVariant{Grad: []string{"#08131f", "#0d2b3e", "#123044"}, Orb: []string{"#10b981", "#0ea5e9"}, Accent: "#10b981"}},
	{ID: "sunset", Name: "日落",
		Light: navThemeVariant{Grad: []string{"#ffb26b", "#ff7e5f", "#f76b8a"}, Orb: []string{"#fb923c", "#f43f5e"}, Accent: "#fb7185"},
		Dark:  navThemeVariant{Grad: []string{"#2a1216", "#43181f", "#2b1424"}, Orb: []string{"#f97316", "#e11d48"}, Accent: "#f97316"}},
	{ID: "ocean", Name: "海洋",
		Light: navThemeVariant{Grad: []string{"#4fc3f7", "#2193b0", "#2b5876"}, Orb: []string{"#38bdf8", "#0e7490"}, Accent: "#0ea5e9"},
		Dark:  navThemeVariant{Grad: []string{"#061a2e", "#0a2c48", "#0e3a52"}, Orb: []string{"#0ea5e9", "#155e75"}, Accent: "#0ea5e9"}},
	{ID: "forest", Name: "森林",
		Light: navThemeVariant{Grad: []string{"#8fd694", "#43a047", "#1b6b4a"}, Orb: []string{"#4ade80", "#15803d"}, Accent: "#43a047"},
		Dark:  navThemeVariant{Grad: []string{"#071a12", "#0d2f20", "#123a26"}, Orb: []string{"#22c55e", "#0f766e"}, Accent: "#22c55e"}},
	{ID: "ink", Name: "水墨",
		Light: navThemeVariant{Grad: []string{"#cfd9df", "#8e9eab", "#5c6b73"}, Orb: []string{"#94a3b8", "#64748b"}, Accent: "#64748b"},
		Dark:  navThemeVariant{Grad: []string{"#0a0a0f", "#161622", "#1f1f2e"}, Orb: []string{"#64748b", "#334155"}, Accent: "#94a3b8"}},
	{ID: "violet", Name: "罗兰",
		Light: navThemeVariant{Grad: []string{"#c9a7f5", "#9b6dff", "#6d5efc"}, Orb: []string{"#a78bfa", "#6366f1"}, Accent: "#9b6dff"},
		Dark:  navThemeVariant{Grad: []string{"#120a26", "#221043", "#2e1550"}, Orb: []string{"#a855f7", "#4f46e5"}, Accent: "#a855f7"}},
	{ID: "ember", Name: "炭火",
		Light: navThemeVariant{Grad: []string{"#f6d365", "#e8a87c", "#c98b6b"}, Orb: []string{"#fbbf24", "#ea580c"}, Accent: "#ea580c"},
		Dark:  navThemeVariant{Grad: []string{"#1c1208", "#2e1c0f", "#231510"}, Orb: []string{"#f59e0b", "#b45309"}, Accent: "#f59e0b"}},
}

// navThemeIDs 由 navThemes 派生（不要另抄一份）。
func navThemeIDs() []string {
	out := make([]string, 0, len(navThemes))
	for _, t := range navThemes {
		out = append(out, t.ID)
	}
	return out
}

// 外观模式 / 卡片尺寸的白名单（值少且稳定，直接列出来更好读）。
var (
	navModeIDs = []string{"auto", "light", "dark"}
	navSizeIDs = []string{"s", "m", "l"}
)

// navOneOf 返回 v 是否在 allowed 里。
func navOneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// navSettingsReq 是 POST /api/v1/nav/settings 的请求体。
type navSettingsReq struct {
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle"`
	Accent     string `json:"accent"`
	Background string `json:"background"`
	// Mode / Theme / Size 为空时回落到默认值（老前端不带这三个字段也能保存）。
	Mode  string `json:"mode"`
	Theme string `json:"theme"`
	Size  string `json:"size"`
}

func (s *Server) handleNavSettingsGet(w http.ResponseWriter, r *http.Request) {
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	set, err := s.Store.NavSettings(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取导航页外观设置失败: "+err.Error())
		return
	}
	ok(w, set)
}

func (s *Server) handleNavSettingsSave(w http.ResponseWriter, r *http.Request) {
	var req navSettingsReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	set, err := validateNavSettings(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.SaveNavSettings(r.Context(), set); err != nil {
		s.audit(r, "nav_settings_save", "外观设置", "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "保存导航页外观设置失败: "+err.Error())
		return
	}
	s.audit(r, "nav_settings_save", "外观设置",
		fmt.Sprintf("标题=%q 主题色=%q 背景=%q 模式=%s 色系=%s 尺寸=%s",
			set.Title, set.Accent, set.Background, set.Mode, set.Theme, set.Size), true, "")
	ok(w, set)
}

// validateNavSettings 校验并规范化外观设置（返回人话错误）。
//
// 三件套（mode/theme/size）是**白名单**：只接受列出的取值；空串回落到默认值
// （兼容老前端/老库）。非法值一律 400 并说明合法取值 —— 不静默改写成默认，
// 否则用户会以为"选了却没生效"。
func validateNavSettings(req navSettingsReq) (store.NavSettings, error) {
	var out store.NavSettings
	title, err := navCleanOptional(req.Title, navMaxTitleLen, "标题")
	if err != nil {
		return out, err
	}
	sub, err := navCleanOptional(req.Subtitle, navMaxSubtitleLen, "副标题")
	if err != nil {
		return out, err
	}
	accent := strings.ToLower(strings.TrimSpace(req.Accent))
	if accent != "" && !navAccentRe.MatchString(accent) {
		return out, errors.New("主题色只接受 #rrggbb 形式的十六进制颜色（例如 #3b82f6）")
	}
	bg, err := validateNavBackground(req.Background)
	if err != nil {
		return out, err
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = store.NavDefaultMode
	} else if !navOneOf(mode, navModeIDs) {
		return out, fmt.Errorf("外观模式只支持 %s（收到 %q）", strings.Join(navModeIDs, " / "), mode)
	}
	theme := strings.ToLower(strings.TrimSpace(req.Theme))
	if theme == "" {
		theme = store.NavDefaultTheme
	} else if !navOneOf(theme, navThemeIDs()) {
		return out, fmt.Errorf("主题色系只支持 %s（收到 %q）", strings.Join(navThemeIDs(), " / "), theme)
	}
	size := strings.ToLower(strings.TrimSpace(req.Size))
	if size == "" {
		size = store.NavDefaultSize
	} else if !navOneOf(size, navSizeIDs) {
		return out, fmt.Errorf("卡片尺寸只支持 %s（收到 %q）", strings.Join(navSizeIDs, " / "), size)
	}
	out.Title, out.Subtitle, out.Accent, out.Background = title, sub, accent, bg
	out.Mode, out.Theme, out.Size = mode, theme, size
	return out, nil
}

// validateNavBackground 校验背景图：空 / 面板自己托管的图片 / http(s) 直链。
//
// 与图标同一条思路（见 validateNavIcon）：只放行"面板生成的内容哈希名"这一种
// 相对路径，其余相对路径（`//evil.com`、`../`、`javascript:`）一律拒绝。
func validateNavBackground(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if len(v) > navMaxIconLen {
		return "", fmt.Errorf("背景图地址过长（上限 %d 字符）", navMaxIconLen)
	}
	if navLocalIconRe.MatchString(v) {
		return v, nil
	}
	if reScheme.MatchString(v) {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return "", errors.New("背景图地址不是合法的 URL")
		}
		if sc := strings.ToLower(u.Scheme); sc != "http" && sc != "https" {
			return "", errors.New("背景图只支持 http:// 或 https:// 直链，或在面板里上传一张")
		}
		return v, nil
	}
	return "", errors.New("背景图只支持面板里上传的图片或 http(s) 直链（不能是相对路径）")
}

// ---------- 导入 / 导出 ----------

// navExportDoc 是导出文件的形状，也是导入接受的形状（往返一致）。
type navExportDoc struct {
	App        string           `json:"app"`
	Kind       string           `json:"kind"`
	Version    int              `json:"version"`
	ExportedAt string           `json:"exported_at"`
	Groups     []store.NavGroup `json:"groups"`
	Items      []store.NavItem  `json:"items"`
}

func (s *Server) handleNavExport(w http.ResponseWriter, r *http.Request) {
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := s.Store.ListNavGroups(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取导航分组失败: "+err.Error())
		return
	}
	items, err := s.Store.ListNavItems(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取导航站点失败: "+err.Error())
		return
	}
	doc := navExportDoc{
		App: "zizpanel", Kind: "nav", Version: 1,
		ExportedAt: time.Now().Format(time.RFC3339),
		Groups:     groups, Items: items,
	}
	// 导出是**可重新导入的纯 JSON 文档**（不是 {ok,data} 包装）：
	// 用户拿到文件后可以直接在「导入」里贴回来，不需要手工剥一层。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="zizpanel-nav-%s.json"`, time.Now().Format("20060102-150405")))
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}

// navImportReq 用指针接收数组：区分「字段没写」与「字段是空数组」。
// 两者语义完全不同 —— 前者是坏请求，后者是「清空导航」。
type navImportReq struct {
	Groups *[]store.NavGroup `json:"groups"`
	Items  *[]store.NavItem  `json:"items"`
}

// decodeNavImport 给导入单独放宽体积上限。
//
// 通用 decode() 的 1MB 上限对单条表单足够，但一次最多允许 3000 个站点，
// 每条地址+描述可达 2KB+，整份 JSON 可能超过 1MB —— 用通用上限会让
// 「合法的整份导入」以"请求格式错误"这种完全指错方向的提示失败。
func decodeNavImport(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求格式错误: %w", err)
	}
	return nil
}

func (s *Server) handleNavImport(w http.ResponseWriter, r *http.Request) {
	var req navImportReq
	if err := decodeNavImport(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Groups == nil && req.Items == nil {
		fail(w, http.StatusBadRequest, "导入内容缺少 groups/items 字段（请选择由本面板导出的 JSON）")
		return
	}
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups := []store.NavGroup{}
	if req.Groups != nil {
		groups = *req.Groups
	}
	items := []store.NavItem{}
	if req.Items != nil {
		items = *req.Items
	}
	if len(groups) > navMaxGroups {
		fail(w, http.StatusBadRequest, fmt.Sprintf("导入的分组过多（%d 个，上限 %d）", len(groups), navMaxGroups))
		return
	}
	if len(items) > navMaxItems {
		fail(w, http.StatusBadRequest, fmt.Sprintf("导入的站点过多（%d 个，上限 %d）", len(items), navMaxItems))
		return
	}

	// 逐条复用与单条写接口完全相同的校验：导入不能成为绕过校验的后门。
	cleanGroups := make([]store.NavGroup, 0, len(groups))
	known := map[int64]bool{}
	for i, g := range groups {
		name, err := navCleanText(g.Name, navMaxNameLen, fmt.Sprintf("第 %d 个分组的名称", i+1))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if g.ID > 0 {
			if known[g.ID] {
				fail(w, http.StatusBadRequest, fmt.Sprintf("导入内容里有重复的分组 id %d", g.ID))
				return
			}
			known[g.ID] = true
		}
		sort := g.Sort
		if sort < 0 {
			sort = i
		}
		cleanGroups = append(cleanGroups, store.NavGroup{ID: g.ID, Name: name, Sort: sort})
	}
	cleanItems := make([]store.NavItem, 0, len(items))
	for i, it := range items {
		if it.GroupID <= 0 || !known[it.GroupID] {
			fail(w, http.StatusBadRequest,
				fmt.Sprintf("第 %d 个站点引用了不存在的分组（group_id=%d）", i+1, it.GroupID))
			return
		}
		name, err := navCleanText(it.Name, navMaxNameLen, fmt.Sprintf("第 %d 个站点的名称", i+1))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		link, err := validateNavURL(it.URL)
		if err != nil {
			fail(w, http.StatusBadRequest, fmt.Sprintf("第 %d 个站点：%s", i+1, err.Error()))
			return
		}
		icon, err := validateNavIcon(it.Icon)
		if err != nil {
			fail(w, http.StatusBadRequest, fmt.Sprintf("第 %d 个站点：%s", i+1, err.Error()))
			return
		}
		desc, err := navCleanOptional(it.Description, navMaxDescLen, fmt.Sprintf("第 %d 个站点的描述", i+1))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		sort := it.Sort
		if sort < 0 {
			sort = i
		}
		cleanItems = append(cleanItems, store.NavItem{
			ID: it.ID, GroupID: it.GroupID, Name: name, URL: link,
			Icon: icon, Description: desc, OpenNewTab: it.OpenNewTab, Sort: sort,
		})
	}

	if err := s.Store.ReplaceNav(r.Context(), cleanGroups, cleanItems); err != nil {
		s.audit(r, "nav_import", "nav", "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "导入失败（已回滚，原数据未变）: "+err.Error())
		return
	}
	s.audit(r, "nav_import", "nav",
		fmt.Sprintf("整份替换：%d 个分组、%d 个站点", len(cleanGroups), len(cleanItems)), true, "")
	ok(w, map[string]any{
		"msg":    fmt.Sprintf("已导入 %d 个分组、%d 个站点", len(cleanGroups), len(cleanItems)),
		"groups": len(cleanGroups),
		"items":  len(cleanItems),
	})
}

// ---------- 计数（上限判定用） ----------

func (s *Server) navGroupCount(ctx context.Context) (int, error) {
	var n int
	err := s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM nav_groups`).Scan(&n)
	return n, err
}

func (s *Server) navItemCount(ctx context.Context) (int, error) {
	var n int
	err := s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM nav_items`).Scan(&n)
	return n, err
}

// ============================================================================
//  独立别名页（浏览器首页）：GET /nav/
//
//  为什么注册在**外层 mux**（不经 panelGate）：别名页的价值就是
//  「http://<主机>/nav/ 直接打开」，不能要求访问者知道面板的安全后缀。
//  它也因此必须自包含 —— 不能引用面板的 /js/ui.js、/app.css（那些在被后缀
//  保护的路径下，匿名访问拿不到），所以自带一份很小的 CSS 与一段 ESM。
//
//  只读公开、编辑要登录：
//    · GET /nav/data   公开只读的导航数据（只有名称/地址/图标，无任何凭据）；
//    · GET /nav/whoami 告诉页面「你登录了吗」，未登录时**绝不回显面板后缀**。
// ============================================================================

func (s *Server) registerNavPage(root *http.ServeMux) {
	// 公开面有两个前缀：/nav/（页面与数据）与 /nav/icons/（用户上传的本地图标）。
	// ServeMux 取**最长匹配**，所以 /nav/icons/<name> 一定会命中下面这条，
	// 与注册顺序无关；写在前面只是为了让人一眼看到公开面有哪些。
	root.Handle(navIconURLBase, http.HandlerFunc(s.handleNavIconFile))
	root.Handle("/nav/", http.HandlerFunc(s.handleNavPage))
	root.Handle("/nav", http.RedirectHandler("/nav/", http.StatusMovedPermanently))
}

func (s *Server) handleNavPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, "/nav/")
	switch sub {
	case "", "index.html":
		s.serveNavAsset(w, "nav/index.html", "text/html; charset=utf-8")
		return
	case "nav.js":
		s.serveNavAsset(w, "nav/nav.js", "text/javascript; charset=utf-8")
		return
	case "data":
		s.handleNavPublicData(w, r)
		return
	case "whoami":
		s.handleNavWhoami(w, r)
		return
	default:
		writeErr(w, http.StatusNotFound, "页面不存在")
		return
	}
}

func (s *Server) serveNavAsset(w http.ResponseWriter, name, ctype string) {
	b, err := fs.ReadFile(s.static, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内置导航页资源缺失: "+err.Error())
		return
	}
	// 与面板其它静态资源一致：no-cache（每次校验 ETag），避免升级后浏览器
	// 还拿着旧版 nav.js —— 别名页是浏览器首页，缓存错的代价很高。
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(b)
}

func (s *Server) handleNavPublicData(w http.ResponseWriter, r *http.Request) {
	if err := s.navEnsure(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	tree, err := s.navTree(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取导航数据失败: "+err.Error())
		return
	}
	ok(w, tree)
}

func (s *Server) handleNavWhoami(w http.ResponseWriter, r *http.Request) {
	entry := ""
	if _, err := s.Auth.AuthSession(r.Context(), s.sessionToken(r)); err == nil {
		// 只有已登录的访问者才拿得到面板入口（安全后缀）。
		// 未登录时返回空串：别名页是公开页面，绝不能在这里泄露后缀。
		entry = s.PanelEntryPath()
	}
	ok(w, map[string]any{"authenticated": entry != "", "panel_entry": entry})
}
