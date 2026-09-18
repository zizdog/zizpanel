package web

// api_nav_icons.go —— 「导航页」的本地图标：上传、列举、删除、公开读取。
//
// 用户 2026-09-18 要求：「导航页要求可以上传本地图标！也可以在已经上传的图标中选择！」
//
// 设计取舍：
//   · 文件放**面板数据目录** `<data>/nav-icons/`（随面板的数据一起备份/迁移），
//     不进数据库 —— 图片进 SQLite 会让库体积与每次备份暴涨，而它们本来就是文件；
//   · 文件名 = **内容 sha256 前 16 位 + 扩展名**（内容寻址）：天然去重、
//     不可能路径穿越、不受中文/空格/大小写/重名影响，也永远不会互相覆盖；
//   · 读取走**公开**路径 `/nav/icons/<name>`（与 `/nav/` 同层、免登录）：
//     导航页是匿名首页，图标必须匿名可读；写操作全部 requireAuth 并进审计；
//   · 只吃白名单图片类型 + 前缀魔数校验 + 512 KiB 上限。SVG 也允许（图标常用），
//     但响应带 `Content-Security-Policy: default-src 'none'; sandbox`，
//     即使有人直接在浏览器里打开它，脚本也执行不了（同源 XSS 的入口被堵死）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// navIconMaxBytes 是单个图标的上限。
	//
	// 512 KiB：足够放一张 512×512 的 PNG，又小到"上传 300 个也不过 150 MB"。
	// 导航页是给人看的首页，不是图床。
	navIconMaxBytes = 512 << 10
	// navIconDirName 是图标目录名（在面板数据目录下）。
	navIconDirName = "nav-icons"
	// navIconURLBase 是图标对外的公开路径前缀。
	navIconURLBase = "/nav/icons/"
	// navIconListMax 是列表返回的上限（防止目录被塞爆后列表拖死界面）。
	navIconListMax = 300
)

// navIconExts 是允许的扩展名 → Content-Type。
//
// 为什么按**扩展名**而不是用户给的 MIME：浏览器/系统给的 Content-Type 不可信，
// 而我们落盘的文件名本来就是自己按内容算出来的，扩展名是唯一可信来源。
var navIconExts = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"svg":  "image/svg+xml",
	"ico":  "image/x-icon",
}

// navIconNameRe 是**唯一**合法的图标文件名形状（内容寻址名 + 白名单扩展名）。
//
// 它同时是路径穿越的判据：任何含 `/`、`..`、空格、中文的名字都不匹配。
var navIconNameRe = regexp.MustCompile(`^[0-9a-f]{16}\.(png|jpg|jpeg|gif|webp|svg|ico)$`)

// navLocalIconRe 在 validateNavIcon 里放行"面板自己托管的图标"这一种相对路径。
//
// 只放行这一种形状：其它相对路径（`//evil.com`、`../x`、`javascript:` 变体）
// 一律按原有规则拒绝。
var navLocalIconRe = regexp.MustCompile(`^/nav/icons/[0-9a-f]{16}\.(png|jpg|jpeg|gif|webp|svg|ico)$`)

// navIconInfo 是列表/上传返回的一项。
type navIconInfo struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Bytes    int64  `json:"bytes"`
	Modified string `json:"modified"`
}

// navIconsDir 返回图标目录（面板数据目录下的 nav-icons/）。
func (s *Server) navIconsDir() string {
	if s.Cfg == nil || strings.TrimSpace(s.Cfg.DataDir) == "" {
		return ""
	}
	return filepath.Join(s.Cfg.DataDir, navIconDirName)
}

// handleNavIconsList 列出已经上传的图标（新的在前）。
func (s *Server) handleNavIconsList(w http.ResponseWriter, r *http.Request) {
	dir := s.navIconsDir()
	if dir == "" {
		fail(w, http.StatusInternalServerError, "面板数据目录未配置，无法列出图标")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 一个都没传过：如实返回空列表，不是错误。
			ok(w, map[string]any{"icons": []navIconInfo{}, "max_bytes": navIconMaxBytes, "background_max_bytes": navBackgroundMaxBytes})
			return
		}
		fail(w, http.StatusInternalServerError, "读取图标目录失败: "+err.Error())
		return
	}
	out := make([]navIconInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !navIconNameRe.MatchString(name) {
			continue
		}
		st, serr := e.Info()
		if serr != nil {
			continue
		}
		out = append(out, navIconInfo{
			Name:     name,
			URL:      navIconURLBase + name,
			Bytes:    st.Size(),
			Modified: st.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified > out[j].Modified })
	if len(out) > navIconListMax {
		out = out[:navIconListMax]
	}
	ok(w, map[string]any{"icons": out, "max_bytes": navIconMaxBytes, "background_max_bytes": navBackgroundMaxBytes})
}

// navBackgroundMaxBytes 是**背景图**的上限。
//
// 比图标（512 KiB）大得多：背景图是整屏图。8 MiB 足够放一张 2560×1440 的
// 高质量 WebP/JPEG，又小到"不会把面板数据目录撑爆"。
const navBackgroundMaxBytes = 8 << 20

// navBackgroundExts 是背景图允许的格式。
//
// 刻意**不含 SVG**：背景图是以 CSS background-image 加载的，虽然这样加载的 SVG
// 不执行脚本，但"能给整页换背景"的东西没必要开放脚本型格式。
var navBackgroundExts = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "webp": "image/webp",
}

// handleNavIconUpload 上传一个**图标**（≤512 KiB，见 navIconMaxBytes）。
func (s *Server) handleNavIconUpload(w http.ResponseWriter, r *http.Request) {
	s.navStoreImageUpload(w, r, navIconMaxBytes, navIconExts, "图标", "nav_icon_upload")
}

// handleNavBackgroundUpload 上传一张**背景图**（≤8 MiB）。
//
// 存进同一个 nav-icons 目录：文件名是内容哈希，天然不会与图标撞名，
// 读取也复用同一个公开入口 /nav/icons/<name>（带 immutable 缓存头）——
// 背景图必须匿名可读，因为导航页本身就是匿名首页。
func (s *Server) handleNavBackgroundUpload(w http.ResponseWriter, r *http.Request) {
	s.navStoreImageUpload(w, r, navBackgroundMaxBytes, navBackgroundExts, "背景图", "nav_background_upload")
}

// navStoreImageUpload 是图标与背景图**共用**的落盘逻辑（只有上限/格式/文案不同）。
//
// 幂等：同一张图再传一次得到同一个名字（内容一样），不会堆重复文件。
func (s *Server) navStoreImageUpload(w http.ResponseWriter, r *http.Request,
	maxBytes int64, exts map[string]string, what string, auditAction string) {
	dir := s.navIconsDir()
	if dir == "" {
		fail(w, http.StatusInternalServerError, "面板数据目录未配置，无法保存"+what)
		return
	}
	limitText := fmt.Sprintf("%d KiB", maxBytes>>10)
	if maxBytes >= 1<<20 {
		limitText = fmt.Sprintf("%d MiB", maxBytes>>20)
	}
	// 先给整个请求体设上限（multipart 的边界/头部也占字节，所以留一点余量）。
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(64<<10))
	file, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "没有收到"+what+"文件（表单字段名必须是 file；单个文件不超过 "+
			limitText+"）: "+err.Error())
		return
	}
	defer func() { _ = file.Close() }()

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(hdr.Filename), "."))
	if _, allowed := exts[ext]; !allowed {
		names := make([]string, 0, len(exts))
		for k := range exts {
			names = append(names, k)
		}
		sort.Strings(names)
		fail(w, http.StatusBadRequest,
			"这个"+what+"格式不支持："+strings.Join(names, " / ")+"（收到的是 ."+ext+"）")
		return
	}
	data, rerr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if rerr != nil {
		fail(w, http.StatusBadRequest, "读取上传内容失败: "+rerr.Error())
		return
	}
	if len(data) == 0 {
		fail(w, http.StatusBadRequest, "上传的"+what+"是空文件（0 字节）")
		return
	}
	if int64(len(data)) > maxBytes {
		fail(w, http.StatusBadRequest,
			fmt.Sprintf("%s太大：%d 字节，上限 %d 字节（%s）。请先压小再上传",
				what, len(data), maxBytes, limitText))
		return
	}
	if err := navIconCheckMagic(ext, data); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:8]) + "." + ext
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, "创建图片目录失败: "+err.Error())
		return
	}
	dst := filepath.Join(dir, name)
	if st, serr := os.Stat(dst); serr == nil && st.Size() == int64(len(data)) {
		// 内容一样 → 同一个文件。如实告诉用户"这张图之前就传过"，不制造副本。
		s.audit(r, auditAction, name,
			fmt.Sprintf("%s（%d 字节，内容已存在，复用）", hdr.Filename, len(data)), true, "")
		ok(w, navIconInfo{Name: name, URL: navIconURLBase + name, Bytes: st.Size(),
			Modified: st.ModTime().Format(time.RFC3339)})
		return
	}
	tmp := dst + ".tmp"
	if werr := os.WriteFile(tmp, data, 0o644); werr != nil {
		fail(w, http.StatusInternalServerError, "写入"+what+"失败: "+werr.Error())
		return
	}
	if rerr := os.Rename(tmp, dst); rerr != nil {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, "保存"+what+"失败: "+rerr.Error())
		return
	}
	s.audit(r, auditAction, name,
		fmt.Sprintf("%s（%d 字节）", hdr.Filename, len(data)), true, "")
	ok(w, navIconInfo{Name: name, URL: navIconURLBase + name, Bytes: int64(len(data)),
		Modified: time.Now().Format(time.RFC3339)})
}

// handleNavIconDelete 删除一个已上传的图标。
//
// 为什么必须有它：只能传不能删，攒下的图标就再也清不掉 —— 与
// 「能装不能卸」是同一类问题（AGENTS 第三节）。
func (s *Server) handleNavIconDelete(w http.ResponseWriter, r *http.Request) {
	dir := s.navIconsDir()
	name := strings.TrimSpace(r.PathValue("name"))
	if dir == "" {
		fail(w, http.StatusInternalServerError, "面板数据目录未配置，无法删除图标")
		return
	}
	if !navIconNameRe.MatchString(name) {
		fail(w, http.StatusBadRequest, "图标名不合法（只能是面板自己生成的 <hash>.<ext>）")
		return
	}
	p := filepath.Join(dir, name)
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.audit(r, "nav_icon_delete", name, "失败: 文件不存在", false, "")
			fail(w, http.StatusNotFound, "这个图标已经不在磁盘上了（可能已被删除）")
			return
		}
		s.audit(r, "nav_icon_delete", name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "删除图标失败: "+err.Error())
		return
	}
	s.audit(r, "nav_icon_delete", name, "已删除图标文件", true, "")
	ok(w, map[string]any{"name": name, "deleted": true})
}

// handleNavIconFile 是**公开**的图标读取（与 /nav/ 同层，免登录）。
//
// 安全要点：
//
//	· 名字必须完全匹配 navIconNameRe（内容寻址名）—— 这一个判据同时挡住
//	  路径穿越、绝对路径、以及任何"读面板数据目录里别的文件"的尝试；
//	· 只回白名单类型，且带 nosniff；
//	· SVG 带 CSP sandbox（default-src 'none'），直接打开也执行不了脚本；
//	· 内容寻址 → 名字即内容，可以长缓存（immutable）。
func (s *Server) handleNavIconFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	dir := s.navIconsDir()
	name := strings.TrimPrefix(r.URL.Path, navIconURLBase)
	if dir == "" || !navIconNameRe.MatchString(name) {
		writeErr(w, http.StatusNotFound, "图标不存在")
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	ctype, allowed := navIconExts[ext]
	if !allowed {
		writeErr(w, http.StatusNotFound, "图标不存在")
		return
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, "图标不存在")
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, "图标不存在")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	// 内容寻址：文件名就是内容哈希，永远可以长缓存。
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// navIconCheckMagic 检查内容**看起来真的是**这个扩展名对应的图片。
//
// 为什么不能只看扩展名：用户把 .txt 改名成 .png 也能传上来，
// 于是导航页上出现一堆碎图；更糟的是把 HTML/JS 改名成 .svg
// （所以 SVG 还额外拒绝脚本与事件属性）。
func navIconCheckMagic(ext string, b []byte) error {
	switch ext {
	case "png":
		if len(b) < 8 || !bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
			return errors.New("这个文件不是 PNG（文件头不对）—— 是不是把别的文件改名了？")
		}
	case "jpg", "jpeg":
		if len(b) < 3 || b[0] != 0xFF || b[1] != 0xD8 || b[2] != 0xFF {
			return errors.New("这个文件不是 JPEG（文件头不对）—— 是不是把别的文件改名了？")
		}
	case "gif":
		if len(b) < 6 || (!bytes.HasPrefix(b, []byte("GIF87a")) && !bytes.HasPrefix(b, []byte("GIF89a"))) {
			return errors.New("这个文件不是 GIF（文件头不对）—— 是不是把别的文件改名了？")
		}
	case "webp":
		if len(b) < 12 || !bytes.HasPrefix(b, []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WEBP")) {
			return errors.New("这个文件不是 WebP（文件头不对）—— 是不是把别的文件改名了？")
		}
	case "ico":
		if len(b) < 4 || b[0] != 0x00 || b[1] != 0x00 || b[2] != 0x01 || b[3] != 0x00 {
			return errors.New("这个文件不是 ICO（文件头不对）—— 是不是把别的文件改名了？")
		}
	case "svg":
		head := strings.ToLower(string(b))
		if i := strings.IndexByte(head, '<'); i >= 0 {
			head = head[i:]
		}
		if !strings.HasPrefix(head, "<svg") && !strings.Contains(head, "<svg") {
			return errors.New("这个文件不是 SVG（找不到 <svg> 标签）")
		}
		for _, bad := range []string{"<script", "javascript:", "onload=", "onerror=", "<iframe"} {
			if strings.Contains(head, bad) {
				return fmt.Errorf("SVG 里含有可执行的 %s —— 图标里不允许脚本（防止同源 XSS）", bad)
			}
		}
	}
	return nil
}
