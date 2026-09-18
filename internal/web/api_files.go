package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  文件管理器
//
//  根目录白名单来自配置（FileRoots），默认给出：
//    网站根目录、面板数据/日志/工作目录
//
//  所有路径都经过 files.Manager 校验（含软链接解析），
//  因此即使前端被绕过也无法访问白名单之外的路径。
// ============================================================================

// fileManager 构造文件管理器。
//
// 每次调用重新构造（不再 sync.Once 缓存）：根目录里包含"面板安装的应用配置文件
// 所在目录"，而用户完全可能在面板运行期间**刚装完** frpc ——
// 缓存住旧根目录的后果是「📝 编辑配置文件」报"路径不在允许访问的范围内"，
// 只有重启面板才好。构造只是几次 os.Stat，代价可以忽略。
func (s *Server) fileManager() *files.Manager {
	roots := s.Cfg.FileRoots
	if len(roots) == 0 {
		roots = []string{
			s.Cfg.WWWRoot,
			s.Cfg.DataDir,
			s.Cfg.LogDir,
			s.Cfg.WorkDir,
		}
	}
	roots = append(roots, s.appConfigRoots()...)
	return files.NewManager(files.Options{
		Roots:    roots,
		UserName: s.Cfg.User,
		UserHome: s.Cfg.UserHome,
	})
}

// appConfigRoots 返回"面板安装的应用的配置文件所在目录"。
//
// 为什么需要：frpc / Orbien 客户端装在用户家目录下（~/frpc、~/orbien-client），
// 默认的文件管理器白名单（网站目录 + 面板数据/日志/工作目录）覆盖不到，
// 于是服务详情里的「📝 编辑配置文件」会被 files.Manager 正当地拒绝。
// 这里只把**这些应用的安装目录**加进白名单，不是整个家目录 ——
// 越界校验、软链接解析仍然全部由 files.Manager 负责，没有第二套读写。
func (s *Server) appConfigRoots() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range services.Catalog() {
		if a.ConfigPath == "" {
			continue
		}
		p := services.ConfigFilePath(a, s.Cfg.UserHome, s.Cfg.WorkDir)
		if p == "" {
			continue
		}
		dir := filepath.Dir(p)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	mgr := s.fileManager()
	p := r.URL.Query().Get("path")
	if p == "" {
		// 默认打开**网站根目录**（最常用的位置）。
		//
		// 不能用 mgr.Roots()[0]：roots 是按字母排序的，而白名单里还包含面板安装的
		// 应用的配置目录。真机上 /opt/homebrew/etc 排在网站目录前面，用户点开文件
		// 管理看到的是 Homebrew 的配置目录 —— 他正要传网站文件，很可能就传错地方。
		// 详见 files.Manager.DefaultDir 的注释。
		p = mgr.DefaultDir(s.Cfg.WWWRoot)
		if p == "" {
			roots := mgr.Roots()
			if len(roots) == 0 {
				fail(w, http.StatusInternalServerError, "没有可访问的目录。请检查配置中的 file_roots")
				return
			}
			p = roots[0]
		}
	}
	showHidden := r.URL.Query().Get("hidden") == "1"
	res, err := mgr.List(p, showHidden)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, res)
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	mgr := s.fileManager()
	res, err := mgr.Read(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, res)
}

type fileWriteReq struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Create 为 true 时允许新建文件
	Create bool `json:"create"`
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var req fileWriteReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	if err := mgr.Write(req.Path, req.Content, req.Create); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_write", req.Path, fmt.Sprintf("写入 %d 字节", len(req.Content)), true, "")
	ok(w, map[string]any{"msg": "已保存", "size": len(req.Content)})
}

type filePathReq struct {
	Path string `json:"path"`
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	var req filePathReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	if err := mgr.Mkdir(req.Path); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_mkdir", req.Path, "新建目录", true, "")
	ok(w, map[string]any{"msg": "目录已创建"})
}

func (s *Server) handleFileTouch(w http.ResponseWriter, r *http.Request) {
	var req filePathReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	if err := mgr.Touch(req.Path); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_touch", req.Path, "新建文件", true, "")
	ok(w, map[string]any{"msg": "文件已创建"})
}

type fileRenameReq struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	var req fileRenameReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	if err := mgr.Rename(req.From, req.To); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_rename", req.From, "重命名为 "+req.To, true, "")
	ok(w, map[string]any{"msg": "已重命名"})
}

func (s *Server) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	var req fileRenameReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	if err := mgr.Copy(req.From, req.To); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_copy", req.From, "复制到 "+req.To, true, "")
	ok(w, map[string]any{"msg": "已复制"})
}

type fileChmodReq struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	// Recursive 为 true 时递归修改
	Recursive bool `json:"recursive"`
}

func (s *Server) handleFileChmod(w http.ResponseWriter, r *http.Request) {
	var req fileChmodReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mode, err := files.ParseMode(req.Mode)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	if !req.Recursive {
		if err := mgr.Chmod(req.Path, mode); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		// 递归修改：逐个走 Resolve 校验，确保不会因软链接越界
		target, err := mgr.Resolve(req.Path, false)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		count := 0
		_ = filepath.Walk(target, func(p string, info os.FileInfo, werr error) error {
			if werr != nil {
				return nil
			}
			if _, err := mgr.Resolve(p, false); err != nil {
				return nil
			}
			if err := os.Chmod(p, mode); err == nil {
				count++
			}
			return nil
		})
		s.audit(r, "file_chmod", req.Path, fmt.Sprintf("递归修改权限为 %s（%d 项）", req.Mode, count), true, "")
		ok(w, map[string]any{"msg": fmt.Sprintf("已修改 %d 项的权限", count), "count": count})
		return
	}
	s.audit(r, "file_chmod", req.Path, "修改权限为 "+req.Mode, true, "")
	ok(w, map[string]any{"msg": "权限已修改"})
}

type fileDeleteReq struct {
	Paths []string `json:"paths"`
	// Recursive 必须显式传 true 才能删除非空目录
	Recursive bool `json:"recursive"`
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	var req fileDeleteReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Paths) == 0 {
		fail(w, http.StatusBadRequest, "请选择要删除的文件")
		return
	}
	mgr := s.fileManager()
	var done []string
	for _, p := range req.Paths {
		if err := mgr.Delete(p, req.Recursive); err != nil {
			// 部分失败时返回已删列表与失败原因，便于前端准确展示
			s.audit(r, "file_delete", p, "删除失败: "+err.Error(), false, "")
			ok(w, map[string]any{
				"deleted": done, "failed": p, "error": err.Error(),
				"msg": fmt.Sprintf("已删除 %d 项，%s 失败：%v", len(done), filepath.Base(p), err),
			})
			return
		}
		done = append(done, p)
	}
	s.audit(r, "file_delete", strings.Join(done, ", "),
		fmt.Sprintf("删除 %d 项（recursive=%v）", len(done), req.Recursive), true, "")
	ok(w, map[string]any{"msg": fmt.Sprintf("已删除 %d 项", len(done)), "deleted": done})
}

type fileCompressReq struct {
	Dir    string   `json:"dir"`
	Names  []string `json:"names"`
	Format string   `json:"format"`
	Output string   `json:"output"`
}

func (s *Server) handleFileCompress(w http.ResponseWriter, r *http.Request) {
	var req fileCompressReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	out, err := mgr.Compress(r.Context(), req.Dir, req.Names, req.Format, req.Output)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_compress", req.Dir,
		fmt.Sprintf("压缩 %d 项为 %s", len(req.Names), filepath.Base(out)), true, "")
	ok(w, map[string]any{"msg": "已生成 " + filepath.Base(out), "path": out})
}

type fileExtractReq struct {
	Archive string `json:"archive"`
	Dest    string `json:"dest"`
}

func (s *Server) handleFileExtract(w http.ResponseWriter, r *http.Request) {
	var req fileExtractReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	dest, err := mgr.Extract(r.Context(), req.Archive, req.Dest)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_extract", req.Archive, "解压到 "+dest, true, "")
	ok(w, map[string]any{"msg": "已解压到 " + dest, "dest": dest})
}

// handleFileDownload 下载文件。
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	mgr := s.fileManager()
	p := r.URL.Query().Get("path")
	f, st, err := mgr.OpenForRead(p)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	defer func() { _ = f.Close() }()

	name := filepath.Base(p)
	// 用 RFC 5987 的形式同时提供 ASCII 回退与 UTF-8 文件名，
	// 否则中文文件名在部分浏览器上会变成乱码
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
			asciiFallback(name), urlEncode(name)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	http.ServeContent(w, r, name, st.ModTime(), f)
	s.audit(r, "file_download", p, fmt.Sprintf("下载 %s", files.FormatSize(st.Size())), true, "")
}

// asciiFallback 把文件名转成 ASCII 安全形式（非 ASCII 用 _ 替代）。
func asciiFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && r != '"' && r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "download"
	}
	return b.String()
}

func urlEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// maxUpload 是**面板自身**允许的单次上传请求体上限（含 multipart 开销）。
//
// 为什么是 4GB：
//   - 用户报障的真实场景是「新建站点 + 传 978MB 的网站包」。旧的 512MB 太小，
//     而且超限是在**浏览器已经把 512MB 传完**之后才被服务端拒绝的 ——
//     用户白等了半天，只得到一句失败。
//   - 2GB 是很多浏览器的单次请求/文件系统的心理门槛，4GB 留出余量，
//     也覆盖"一个中等站点 + 一堆图片/视频"的常见情形。
//
// 为什么不放进设置项（internal/config）：
//   - nginx 的 client_max_body_size 与 PHP 的 upload_max_filesize/post_max_size
//     才是"用户自己网站"的 413 来源，那条链路已经另有面板入口
//     （「设置 → 上传与执行限制」，见 api_upload_limits.go）。两件事别混在一起：
//     那条链路限制的是**别的程序**（phpMyAdmin、Typecho 后台）收多大的请求，
//     这里限制的是**面板自己**收多大的请求。
//   - 面板自身这个上限的目的是"别让用户在浏览器里白传几个 GB 才被拒"，
//     不是让用户调的业务参数。做成设置项只会制造"调小了传不动、
//     调大了内存/磁盘被吃满"的坑。
//
// 单文件与总大小都受它约束（整个请求体就是一个 multipart）；「上传文件夹」的
// 总大小同样受限，但可以做**增量**：传失败的那几个在结果里逐个列出，重传即可。
const maxUpload = int64(4) << 30 // 4 GiB

// handleFileUpload 上传文件（multipart/form-data）。
//
// 支持两种形态，共用一条路由：
//   - 普通上传：多个 `files` 字段，落在目标目录里；同名文件**不覆盖**，
//     自动加 -1/-2 序号（保留旧行为）。
//   - 「上传文件夹」：额外带一个 `relpaths` 字段（JSON 数组，与 `files` 顺序一一对应），
//     后端按相对路径在目标目录下重建目录树，并且**允许覆盖**同名文件 ——
//     传整站时把 index.php 存成 index-1.php 会直接让站点跑不起来。
//     结果里每个文件都带 overwritten 标记，前端如实显示"已覆盖"。
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	// 先解除全局 30 秒读超时对这条路由的限制（否则大文件必然被掐断，
	// 而浏览器只看到"网络错误"，用户看到的就是"点了没反应"）。
	if err := allowLongUpload(w, r); err != nil {
		// 如实记录下来：延长失败时大文件会被 30 秒读超时中断，而这不是用户的错。
		if s.Log != nil {
			s.Log.Warn("延长上传读超时失败（超过 30 秒的上传可能被中断）: %v", err)
		}
	}

	// 两道判据都在 readUploadForm 里（先看 ContentLength，再用 MaxBytesReader 边收边算），
	// 超限一律 413 + 人话：含实际大小、上限、以及两条可执行的建议。
	if err := readUploadForm(r, maxUpload); err != nil {
		if errors.Is(err, errUploadTooLarge) {
			fail(w, http.StatusRequestEntityTooLarge,
				uploadLimitMessage(r.ContentLength, maxUpload, "请求体过大"))
			return
		}
		fail(w, http.StatusBadRequest, "解析上传内容失败: "+err.Error())
		return
	}
	dir := r.FormValue("dir")
	if dir == "" {
		fail(w, http.StatusBadRequest, "缺少目标目录参数")
		return
	}

	mgr := s.fileManager()
	form := r.MultipartForm
	if form == nil || len(form.File) == 0 {
		fail(w, http.StatusBadRequest, "没有收到文件")
		return
	}

	// 相对路径（仅「上传文件夹」会带）。顺序与 form.File["files"] 一致：
	// 前端在同一个循环里先 append 文件、再 append 相对路径，Go 的 multipart
	// 解析对同一个字段名保序，所以两个切片天然对齐。
	relPaths, err := parseRelPaths(form.Value["relpaths"])
	if err != nil {
		fail(w, http.StatusBadRequest, "上传的相对路径不合法："+err.Error())
		return
	}
	// 先整批校验：任一条非法（.. / 绝对路径 / 反斜杠 / 盘符）就整单拒绝。
	// 这是**攻击特征**而不是用户笔误，不该"部分成功"地把可疑请求写进磁盘。
	treeMode := len(relPaths) > 0
	if treeMode {
		for _, p := range relPaths {
			if _, err := files.CleanRelPath(p); err != nil {
				s.audit(r, "file_upload", dir, "拒绝可疑相对路径: "+p, false, "")
				fail(w, http.StatusBadRequest, "拒绝上传：相对路径不合法（"+err.Error()+"）")
				return
			}
		}
	}

	// 按**顺序**取文件。form.File 是 map，直接 range 会打乱顺序；而「上传文件夹」
	// 的相对路径是按顺序对齐的 —— 顺序错了就会把 A 目录的 index.php 写进 B 目录。
	// 前端固定用 `files` 字段，所以先按原序取它；其它字段名（手工调 API 才可能出现）
	// 按字段名排序后追加，保证同一份请求每次结果一致。
	parts := append([]*multipart.FileHeader{}, form.File["files"]...)
	if len(form.File) > 1 {
		var others []string
		for name := range form.File {
			if name != "files" {
				others = append(others, name)
			}
		}
		sort.Strings(others)
		for _, name := range others {
			parts = append(parts, form.File[name]...)
		}
	}
	if treeMode && len(relPaths) != len(parts) {
		fail(w, http.StatusBadRequest, fmt.Sprintf(
			"上传的相对路径数量（%d）与文件数量（%d）不一致，拒绝上传（可能的中途截断）",
			len(relPaths), len(parts)))
		return
	}

	var results []uploadedFile
	var failures []string

	for idx, fh := range parts {
		f, err := fh.Open()
		if err != nil {
			failures = append(failures, uploadLabel(fh.Filename, relAt(relPaths, treeMode, idx))+": "+err.Error())
			continue
		}
		rel := relAt(relPaths, treeMode, idx)
		var path string
		var n int64
		var overwritten bool
		if treeMode {
			path, n, overwritten, err = mgr.SaveUploadAs(dir, rel, f, true)
		} else {
			path, n, err = mgr.SaveUpload(dir, fh.Filename, f)
		}
		_ = f.Close()
		if err != nil {
			failures = append(failures, uploadLabel(fh.Filename, rel)+": "+err.Error())
			continue
		}
		results = append(results, uploadedFile{
			Name: filepath.Base(path), Path: path, RelPath: rel,
			Size: n, Overwritten: overwritten,
		})
	}

	if len(results) == 0 {
		fail(w, http.StatusBadRequest, "上传失败："+strings.Join(failures, "；"))
		return
	}
	action := fmt.Sprintf("上传 %d 个文件", len(results))
	if treeMode {
		action = fmt.Sprintf("上传文件夹（%d 个文件）", len(results))
	}
	s.audit(r, "file_upload", dir, action, true, "")
	msg := fmt.Sprintf("已上传 %d 个文件", len(results))
	if n := countOverwritten(results); n > 0 {
		msg += fmt.Sprintf("（其中 %d 个覆盖了同名文件）", n)
	}
	if len(failures) > 0 {
		msg += fmt.Sprintf("，%d 个失败", len(failures))
	}
	ok(w, map[string]any{
		"uploaded": results, "failed": failures, "msg": msg,
	})
}

// uploadedFile 是一个成功落盘的文件。
type uploadedFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// RelPath 是「上传文件夹」时的相对路径（普通上传为空）
	RelPath string `json:"rel_path,omitempty"`
	Size    int64  `json:"size"`
	// Overwritten 表示这次覆盖了一个已存在的同名文件
	Overwritten bool `json:"overwritten,omitempty"`
}

// relAt 取第 idx 个文件的相对路径（非文件夹上传时为 ""）。
func relAt(relPaths []string, treeMode bool, idx int) string {
	if !treeMode || idx < 0 || idx >= len(relPaths) {
		return ""
	}
	return relPaths[idx]
}

// uploadLabel 是结果里给用户看的文件名：有相对路径就用它（更能说明是哪个文件）。
func uploadLabel(name, rel string) string {
	if rel != "" {
		return rel
	}
	return name
}

// parseRelPaths 解析「上传文件夹」带的相对路径字段（JSON 数组）。
//
// 空值返回 nil，表示这是普通上传。
func parseRelPaths(vals []string) ([]string, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	raw := vals[0]
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("relpaths 不是合法的 JSON 数组: %w", err)
	}
	return out, nil
}

func countOverwritten(list []uploadedFile) int {
	n := 0
	for _, it := range list {
		if it.Overwritten {
			n++
		}
	}
	return n
}

type fileSearchReq struct {
	Path  string `json:"path"`
	Query string `json:"query"`
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	var req fileSearchReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Path == "" {
		fail(w, http.StatusBadRequest, "缺少搜索目录")
		return
	}
	mgr := s.fileManager()
	res, err := mgr.Search(r.Context(), req.Path, req.Query, req.Mode, req.Limit)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, res)
}

type fileReplaceReq struct {
	Path    string `json:"path"`
	Find    string `json:"find"`
	Replace string `json:"replace"`
	All     bool   `json:"all"`
}

func (s *Server) handleFileReplace(w http.ResponseWriter, r *http.Request) {
	var req fileReplaceReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.fileManager()
	n, err := mgr.ReplaceInFile(req.Path, req.Find, req.Replace, req.All)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "file_replace", req.Path,
		fmt.Sprintf("替换 %q → %q（%d 处）", req.Find, req.Replace, n), true, "")
	ok(w, map[string]any{"count": n, "msg": fmt.Sprintf("已替换 %d 处", n)})
}

// 保留：便于将来支持流式解压
var _ = bytes.MinRead
var _ = io.Discard
var _ = time.Now
