package web

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
// 所在目录"，而用户完全可能在面板运行期间**刚装完** Lucky / frps ——
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
// 为什么需要：Lucky / Orbien / frps 装在用户家目录下（~/lucky、~/orbien、~/frps），
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
		// 默认打开网站根目录（最常用的位置）
		roots := mgr.Roots()
		if len(roots) == 0 {
			fail(w, http.StatusInternalServerError, "没有可访问的目录。请检查配置中的 file_roots")
			return
		}
		p = roots[0]
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

// handleFileUpload 上传文件（multipart/form-data）。
//
// 限制单文件 512MB：再大就应该用别的方式传输（scp/rsync），
// 走浏览器上传既慢又占内存。
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 512 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(w, http.StatusBadRequest, "解析上传内容失败（文件可能超过 512MB）: "+err.Error())
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

	type uploaded struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	var results []uploaded
	var failures []string

	for field, headers := range form.File {
		for _, fh := range headers {
			f, err := fh.Open()
			if err != nil {
				failures = append(failures, fh.Filename+": "+err.Error())
				continue
			}
			path, n, err := mgr.SaveUpload(dir, fh.Filename, f)
			_ = f.Close()
			if err != nil {
				failures = append(failures, fh.Filename+": "+err.Error())
				continue
			}
			results = append(results, uploaded{Name: filepath.Base(path), Path: path, Size: n})
			_ = field
		}
	}

	if len(results) == 0 {
		fail(w, http.StatusBadRequest, "上传失败："+strings.Join(failures, "；"))
		return
	}
	s.audit(r, "file_upload", dir,
		fmt.Sprintf("上传 %d 个文件", len(results)), true, "")
	ok(w, map[string]any{
		"uploaded": results, "failed": failures,
		"msg": fmt.Sprintf("已上传 %d 个文件", len(results)),
	})
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
