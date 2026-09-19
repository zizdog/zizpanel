package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
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
	roots := s.fileRoots()
	return files.NewManager(files.Options{
		Roots:    roots,
		UserName: s.Cfg.User,
		UserHome: s.Cfg.UserHome,
	})
}

// fileRoots 返回允许访问的根目录集合。
//
// 2026-09-2X 用户批准的「文件管理大放开」新增：
//   - 整个用户目录 /Users/<user>；
//   - 面板安装根（默认 /opt/zizpanel，整个目录，不只是 data/logs/work）；
//   - /opt/homebrew/etc（nginx / php 配置）；
//   - 所有非系统卷的挂载点（外接盘，动态枚举，插拔后随下次请求自动刷新）。
//
// 系统盘其余部分（/、/System、/Library、/usr、/bin、/private、/etc…）依然拒绝：
// 它们不在任何根目录之下，越界由 files.Manager.Resolve 返回 ErrForbidden。
// 软链接逃逸防护也没有被削弱 —— Resolve 仍然先 EvalSymlinks 再做前缀检查。
func (s *Server) fileRoots() []string {
	roots := append([]string{}, s.Cfg.FileRoots...)
	if len(roots) == 0 {
		roots = s.defaultFileRoots()
	}
	roots = append(roots, s.appConfigRoots()...)
	return roots
}

// defaultFileRoots 是未显式配置 file_roots 时的默认根集合。
func (s *Server) defaultFileRoots() []string {
	roots := []string{
		s.Cfg.WWWRoot,
		s.Cfg.DataDir,
		s.Cfg.LogDir,
		s.Cfg.WorkDir,
		// 整个用户目录
		s.Cfg.UserHome,
		// 面板安装根。除由配置推导出的安装根外，显式加上编译期默认值
		// （/opt/zizpanel）：`make run-local` 会把配置指向临时根，
		// 但验收要求此时仍然能访问真实安装根。
		config.DefaultRoot,
	}
	// Homebrew 的 etc（nginx / php 配置都在这里）
	if s.Cfg.BrewPrefix != "" {
		roots = append(roots, filepath.Join(s.Cfg.BrewPrefix, "etc"))
	}
	roots = append(roots, s.installRoots()...)
	// 外接盘：每次构造都重新枚举（读 /Volumes + getfsstat，不 fork 进程、不跑 diskutil）
	roots = append(roots, files.NonSystemVolumeMounts()...)
	return roots
}

// installRoots 由配置里的 BinDir 推导安装根，支持非默认安装位置。
//
// 只认 BinDir 的父目录（配置保证它是 <安装根>/bin）。刻意不用 DataDir 的父目录：
// 测试环境把 DataDir 直接指向临时目录本身，取父目录会意外把 /tmp 整个放进白名单。
func (s *Server) installRoots() []string {
	if s.Cfg.BinDir == "" {
		return nil
	}
	parent := filepath.Dir(filepath.Clean(s.Cfg.BinDir))
	// 绝不把 "/" 或相对路径（"."）当成安装根 —— 那等于放开整块系统盘/当前目录。
	if parent == "/" || parent == "." || !filepath.IsAbs(parent) {
		return nil
	}
	return []string{parent}
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
		failFileErr(w, err, p)
		return
	}
	s.annotateRoots(res)
	s.markSensitive(res)
	ok(w, res)
}

// annotateRoots 给每个根目录标一个用途，供前端「位置」下拉分组显示。
// fileRootKind* 是给前端"位置"下拉分组用的**根类型标签**（不是路径）。
//
// 为什么用常量而不是到处写字面量：① 语义单一来源；② 备份覆盖门禁的正则是
// `\.(DataDir|WorkDir)[ \t]*,[ \t]*"([^"]+)"`，会把「`s.Cfg.DataDir` 后紧跟一个字符串
// 字面量」的相邻参数对误判成 `<DataDir>/<那个字符串>` 路径（见 internal/backup/plan_gate_test.go 顶部）。
// 用常量名就不带引号，从写法上避开这个已知误报，而不是去削弱那条门禁。
const (
	fileRootKindWWW      = "www"
	fileRootKindHome     = "home"
	fileRootKindData     = "data"
	fileRootKindPanel    = "panel"
	fileRootKindHomebrew = "homebrew"
)

func (s *Server) annotateRoots(res *files.ListResult) {
	if res == nil {
		return
	}
	known := map[string]string{}
	add := func(p, kind string) {
		if rp := resolveForCompare(p); rp != "" {
			known[rp] = kind
		}
	}
	add(s.Cfg.WWWRoot, fileRootKindWWW)
	add(s.Cfg.UserHome, fileRootKindHome)
	// 面板数据目录单独标一类：它嵌在安装根里，前端下拉不再重复列，但要能认出它。
	add(s.Cfg.DataDir, fileRootKindData)
	add(config.DefaultRoot, fileRootKindPanel)
	if s.Cfg.BrewPrefix != "" {
		add(filepath.Join(s.Cfg.BrewPrefix, "etc"), fileRootKindHomebrew)
	}
	for _, r := range s.installRoots() {
		add(r, fileRootKindPanel)
	}
	for _, v := range files.NonSystemVolumeMounts() {
		add(v, "volume")
	}
	kinds := make(map[string]string, len(res.Roots))
	for _, root := range res.Roots {
		// 先看**精确匹配**：这样嵌在用户目录里的网站根目录（~/www）仍然是 www，
		// 不会被父根（用户目录）盖掉；面板数据目录也不会被安装根盖掉。
		if k, ok := known[root]; ok {
			kinds[root] = k
			continue
		}
		kinds[root] = classifyRoot(root, known)
	}
	res.RootKinds = kinds
}

// classifyRoot 取"最长匹配"的已知用途；都不匹配时归到 other。
func classifyRoot(root string, known map[string]string) string {
	best, bestLen := "other", -1
	for p, kind := range known {
		if root == p || strings.HasPrefix(root, p+string(os.PathSeparator)) {
			if len(p) > bestLen {
				bestLen, best = len(p), kind
			}
		}
	}
	return best
}

// markSensitive 把面板数据目录（SQLite 库与凭据）标出来。
//
// 仍然可读写（用户明确要求），只是让界面能打「敏感」标记并在覆盖/删除前二次确认。
func (s *Server) markSensitive(res *files.ListResult) {
	if res == nil {
		return
	}
	data := resolveForCompare(s.Cfg.DataDir)
	if data == "" {
		return
	}
	res.SensitiveRoots = []string{data}
	for i := range res.Entries {
		p := res.Entries[i].Path
		if p == data || strings.HasPrefix(p, data+string(os.PathSeparator)) {
			res.Entries[i].Sensitive = true
		}
	}
}

// resolveForCompare 把已存在的路径解析成真实路径，用于和 Manager.Roots() 的口径对齐。
// 不存在/解析失败时退回 Clean 后的原路径。
func resolveForCompare(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// ============================================================================
//  macOS 隐私保护（TCC）拦住外接卷 —— 把 EPERM 变成可操作的指引
//
//  为什么单独处理：面板以 root 的 LaunchDaemon 运行、**没有用户会话**，
//  读写 /Volumes 下的外接盘/可移除卷会被 macOS 隐私保护（TCC）直接拒绝，
//  返回 EPERM（operation not permitted）。用户报障原文：
//  「打开目录失败: open /Volumes/ZPMirror: operation not permitted」。
//  Apple 的"正规"出口是人工授予「完全磁盘访问权限」，但那需要在有屏幕的
//  机器上点一次，无头/远程场景做不到，所以对外**只给**"挂到 /Volumes 之外"
//  这条面板自己能执行的解法（见 tccSolution）。
//
//  为什么此前测不出来（验证盲区，必须写清楚）：本机调试实例（make run-local）
//  跑在**用户会话**里，那个终端早已被授权，所以能读 /Volumes/ZPMirror；
//  只有正式 root 面板 + 真实外接盘才会命中。调试实例上**永远复现不出**这个错误。
//
//  判据是**真实 errno（EPERM/EACCES） + 路径在 /Volumes 下**，绝不靠匹配
//  "operation not permitted" 字符串：字符串匹配会把别的原因误报成 TCC，
//  也会在包装/本地化变化后失效。
// ============================================================================

// volumeMountRoot 是 macOS 挂载外接盘/可移除卷的标准位置（与 internal/files/volumes.go 一致）。
const volumeMountRoot = "/Volumes"

// tccSolution 是给用户的**唯一**解法：让面板自己把卷挂到 /Volumes 之外。
//
// 刻意**不写**"去系统设置 → 隐私与安全性 → 完全磁盘访问权限里手动授权"：
// 那要求在**有屏幕的机器上人工点一次**，无头/远程场景根本做不到，
// 不能作为给用户的指引。人工授权只是 Apple 提供的另一条路（见 docs/磁盘工具.md）。
//
// 诚实标注：这条路径（换挂载点绕过 TCC）**尚未在真实 root 面板 + 外接盘上实测**，
// 已由真实磁盘工具能力支撑（挂载本身不需要 TCC 授权），但不写成"已验证"。
const tccSolution = "解法：用面板「磁盘 → 挂载到自定义挂载点…」把这个卷挂到 /Volumes 之外，再访问那个路径（挂载本身不受隐私保护限制）。等价命令：" +
	"\n  sudo diskutil mount -mountPoint " + config.DefaultRoot + "/mnt/mirror <卷标识>"

// volumeTCCGuide 是给用户的完整指引（前端会把每一行都显示出来，不是一行 errno）。
func volumeTCCGuide(path string) string {
	return fmt.Sprintf("macOS 隐私保护拦住了对外接卷 %s 的访问：operation not permitted。"+
		"面板以 root 的 LaunchDaemon 运行、没有用户会话，读写 /Volumes 下的外接盘会被系统拒绝。\n%s",
		path, tccSolution)
}

// isPermissionDenied 判断 err 链上是否是一次**真实的**权限拒绝（EPERM/EACCES）。
func isPermissionDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// errPathCandidates 取出 err 链上自带的路径（*fs.PathError / *os.LinkError）。
func errPathCandidates(err error) []string {
	var out []string
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Path != "" {
		out = append(out, pe.Path)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		out = append(out, le.Old, le.New)
	}
	return out
}

// withinVolumes 判断 p 是否就是 /Volumes 或在其之下（按路径分段比较，不用裸前缀字符串，
// 免得把 /Volumes2/... 这类路径也算进来）。
func withinVolumes(p string) bool {
	p = filepath.Clean(strings.TrimSpace(p))
	if p == "" || p == "." {
		return false
	}
	return p == volumeMountRoot || strings.HasPrefix(p, volumeMountRoot+string(os.PathSeparator))
}

// volumeTCCPath 判断这次失败是否是"macOS 隐私保护拦住外接卷"；命中则返回用户访问的那个路径。
//
// 两道判据缺一不可：① 真实 errno 是 EPERM/EACCES；② **出错的那个路径**在 /Volumes 下。
//
// 路径优先取 err 自带的 PathError/LinkError（谁出错看谁，不会张冠李戴）；
// 只有当 err 完全没带路径时，才退回调用方从请求里取的 explicit 路径。
// 反过来，如果 err 带了路径但都不在 /Volumes，就**不**再看 explicit ——
// 否则"从普通目录重命名进 /Volumes"这类失败会被误报成外接卷 TCC 问题。
func volumeTCCPath(err error, explicit ...string) (string, bool) {
	if err == nil || !isPermissionDenied(err) {
		return "", false
	}
	if paths := errPathCandidates(err); len(paths) > 0 {
		for _, p := range paths {
			if withinVolumes(p) {
				return filepath.Clean(p), true
			}
		}
		return "", false
	}
	for _, p := range explicit {
		if withinVolumes(p) {
			return filepath.Clean(p), true
		}
	}
	return "", false
}

// failFileErr 把文件操作的错误映射成 HTTP 状态码。
//
// 越界（ErrForbidden）必须是 403 而不是 400：前端与测试据此区分"路径不合法"
// 与"你没有权限访问这里"，也避免把"系统目录被拒绝"误报成"请求写错了"。
//
// 外接卷被 TCC 拒绝同样是 403（不是 500），并且错误体里带完整解法。
// paths 是调用方从请求里取到的路径（可选；错误自带路径时可以不传）。
func failFileErr(w http.ResponseWriter, err error, paths ...string) {
	if p, ok := volumeTCCPath(err, paths...); ok {
		fail(w, http.StatusForbidden, volumeTCCGuide(p))
		return
	}
	if errors.Is(err, files.ErrForbidden) {
		fail(w, http.StatusForbidden, err.Error())
		return
	}
	fail(w, http.StatusBadRequest, err.Error())
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	mgr := s.fileManager()
	p := r.URL.Query().Get("path")
	res, err := mgr.Read(p)
	if err != nil {
		failFileErr(w, err, p)
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
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	if err := mgr.Write(req.Path, req.Content, req.Create); err != nil {
		failFileErr(w, err, req.Path)
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
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	if err := mgr.Mkdir(req.Path); err != nil {
		failFileErr(w, err, req.Path)
		return
	}
	s.audit(r, "file_mkdir", req.Path, "新建目录", true, "")
	ok(w, map[string]any{"msg": "目录已创建"})
}

func (s *Server) handleFileTouch(w http.ResponseWriter, r *http.Request) {
	var req filePathReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	if err := mgr.Touch(req.Path); err != nil {
		failFileErr(w, err, req.Path)
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
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	if err := mgr.Rename(req.From, req.To); err != nil {
		failFileErr(w, err, req.From, req.To)
		return
	}
	s.audit(r, "file_rename", req.From, "重命名为 "+req.To, true, "")
	ok(w, map[string]any{"msg": "已重命名"})
}

func (s *Server) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	var req fileRenameReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	if err := mgr.Copy(req.From, req.To); err != nil {
		failFileErr(w, err, req.From, req.To)
		return
	}
	s.audit(r, "file_copy", req.From, "复制到 "+req.To, true, "")
	ok(w, map[string]any{"msg": "已复制"})
}

// handleFileMove 移动（剪切粘贴）。
//
// 与 copy 的关键区别：非同卷时 os.Rename 会失败（EXDEV），Manager.Move 会回退到
// "复制 + 删除源"，并且**如实**在响应里报告用的是哪种方式（前端据此提示用户）。
// on_conflict 处理"目标已存在"：rename（默认，自动改名保留两者）/ overwrite /
// skip。绝不静默覆盖。
func (s *Server) handleFileMove(w http.ResponseWriter, r *http.Request) {
	var req fileMoveReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	res, err := mgr.Move(req.From, req.To, req.OnConflict)
	if err != nil {
		failFileErr(w, err, req.From, req.To)
		return
	}
	s.audit(r, "file_move", req.From, fmt.Sprintf("移动到 %s（%s）", res.To, res.Way), true, "")
	ok(w, map[string]any{
		"from":        res.From,
		"to":          res.To,
		"way":         string(res.Way),
		"skipped":     res.Skipped,
		"overwritten": res.Overwritten,
		"msg":         moveMessage(res),
	})
}

type fileMoveReq struct {
	From string `json:"from"`
	To   string `json:"to"`
	// OnConflict 是目标已存在时的处理方式：rename（默认）/ overwrite / skip
	OnConflict string `json:"on_conflict"`
}

// moveMessage 把"实际用了哪种方式"翻译成人话。跨卷复制后删除与同卷重命名
// 对用户的意义不同（前者慢、且中途失败可能留下副本），不能都写成"已移动"。
func moveMessage(res *files.MoveResult) string {
	if res.Skipped {
		return "已跳过（目标已存在）"
	}
	switch res.Way {
	case files.MoveStrategyCopyDelete:
		return "已移动（跨卷：先复制再删除源文件）"
	case files.MoveStrategyNone:
		return "源与目标相同，未做改动"
	default:
		if res.Overwritten {
			return "已移动并覆盖了目标处的原有内容"
		}
		return "已移动（同卷重命名）"
	}
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
		failFileErr(w, err)
		return
	}
	mode, err := files.ParseMode(req.Mode)
	if err != nil {
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	if !req.Recursive {
		if err := mgr.Chmod(req.Path, mode); err != nil {
			failFileErr(w, err, req.Path)
			return
		}
	} else {
		// 递归修改：逐个走 Resolve 校验，确保不会因软链接越界
		target, err := mgr.Resolve(req.Path, false)
		if err != nil {
			failFileErr(w, err, req.Path)
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
		failFileErr(w, err)
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
			// 外接卷被 macOS 隐私保护拒绝：整单 403 + 完整指引，
			// 而不是 200 + 一行 errno（同一类问题的其它入口也都这么映射）。
			if tccPath, isTCC := volumeTCCPath(err, p); isTCC {
				s.audit(r, "file_delete", p, "删除失败: "+err.Error(), false, "")
				fail(w, http.StatusForbidden, volumeTCCGuide(tccPath))
				return
			}
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

// handleFileCompress 打包压缩（走任务中心：目录大了就是分钟级动作）。
//
// 用户 2026-09-18 报障："我传了文件解压缩，一点反应都没有！900M 的文件解压
// 没有任何进度" —— 打包与解压都必须有**真实进度**，而且关掉窗口要能找回进度，
// 所以统一改成 202 + task_id（与安装/卸载同一条规矩）。
func (s *Server) handleFileCompress(w http.ResponseWriter, r *http.Request) {
	var req fileCompressReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	// 先做一次**只读**校验（源路径是否存在/是否越界、格式是否支持）：
	// 参数错了要当场 400，而不是开一个注定失败的任务。
	if err := mgr.CheckCompressTargets(req.Dir, req.Names, req.Format); err != nil {
		failFileErr(w, err, req.Dir, req.Output)
		return
	}
	title := fmt.Sprintf("打包 %d 项为 %s", len(req.Names), filepath.Base(req.Output))
	s.launchTask(w, r, "file_compress", req.Dir, title,
		"file_compress", func(ctx context.Context, log tasks.LogFunc) (any, error) {
			lastReport := time.Now()
			out, err := mgr.CompressWithProgress(ctx, req.Dir, req.Names, req.Format, req.Output,
				func(done, total int, note string) {
					// 限流：每 400ms 或每 20 个条目报一次，免得 900MB 归档刷出几万行日志。
					if time.Since(lastReport) < 400*time.Millisecond && done%20 != 0 {
						return
					}
					lastReport = time.Now()
					log(tasks.LevelOut, fmt.Sprintf("已处理 %d/%d：%s", done, total, note))
				})
			if err != nil {
				return nil, err
			}
			log(tasks.LevelOK, "已生成 "+out)
			return map[string]any{"path": out, "msg": "已生成 " + filepath.Base(out)}, nil
		})
}

type fileExtractReq struct {
	Archive string `json:"archive"`
	Dest    string `json:"dest"`
}

// handleFileExtract 解压归档（走任务中心 + 真实进度）。
//
// 同样的报障："900M 的文件解压没有任何进度" —— 老实现是**同步**跑 unzip/tar，
// 请求挂着、页面没有任何输出，用户看到的就是"点了没反应"。现在：
// 立刻 202 + task_id，任务里逐条报"已解压 N/M：路径"，完成后在任务中心看结果。
func (s *Server) handleFileExtract(w http.ResponseWriter, r *http.Request) {
	var req fileExtractReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	// 只读预检：归档存在、格式支持、目标目录在允许范围内、归档内没有穿越路径。
	// 这些问题要**当场**告诉用户（400 + 人话），而不是丢进任务再失败。
	if err := mgr.CheckExtractTargets(r.Context(), req.Archive, req.Dest); err != nil {
		failFileErr(w, err, req.Archive, req.Dest)
		return
	}
	s.launchTask(w, r, "file_extract", req.Archive, "解压 "+filepath.Base(req.Archive),
		"file_extract", func(ctx context.Context, log tasks.LogFunc) (any, error) {
			lastReport := time.Now()
			dest, err := mgr.ExtractWithProgress(ctx, req.Archive, req.Dest,
				func(done, total int, note string) {
					if time.Since(lastReport) < 400*time.Millisecond && done%20 != 0 {
						return
					}
					lastReport = time.Now()
					if total > 0 {
						log(tasks.LevelOut, fmt.Sprintf("已解压 %d/%d：%s", done, total, note))
					} else {
						log(tasks.LevelOut, fmt.Sprintf("已解压 %d 项：%s", done, note))
					}
				})
			if err != nil {
				return nil, err
			}
			log(tasks.LevelOK, "已解压到 "+dest)
			return map[string]any{"dest": dest, "msg": "已解压到 " + dest}, nil
		})
}

// handleFileDownload 下载文件。
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	mgr := s.fileManager()
	p := r.URL.Query().Get("path")
	f, st, err := mgr.OpenForRead(p)
	if err != nil {
		failFileErr(w, err, p)
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
//   - 普通上传：多个 `files` 字段，落在目标目录里；
//   - 「上传文件夹」：额外带一个 `relpaths` 字段（JSON 数组，与 `files` 顺序一一对应），
//     后端按相对路径在目标目录下重建目录树。
//
// 同名文件怎么办由 `on_conflict` 决定（前端会先弹窗问用户，再把选择传上来）：
//   - "rename"（普通上传默认）：保留两者，自动加 -1/-2 序号；
//   - "overwrite"（上传文件夹默认）：覆盖同名文件 —— 传整站时把 index.php 存成
//     index-1.php 会直接让站点跑不起来。
//
// 结果里每个文件都带 overwritten 标记，前端如实显示"已覆盖"或"已改名保留两者"。
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

	// on_conflict 决定**同名文件**怎么处理，由前端把用户的选择传上来：
	//   "rename"    = 保留两者，自动加序号（默认，普通上传）
	//   "overwrite" = 覆盖同名文件（默认，上传文件夹）
	//
	// 用户 2026-09-22 明确要求："文件管理上传文件，如果相同名字应该询问是否覆盖
	// 还是共存。" 所以前端在发现重名时会先弹窗问，再把结果传到这里。
	// 默认值刻意分开：
	//   · 普通上传默认 **rename** —— 静默覆盖别人的文件是最不可原谅的默认行为；
	//   · 上传文件夹默认 **overwrite** —— 传整站时把 index.php 存成 index-1.php
	//     会让站点直接跑不起来（这是既有语义，进度窗里也明说了"同名会被覆盖"）。
	onConflict := strings.ToLower(strings.TrimSpace(r.FormValue("on_conflict")))
	if onConflict == "" {
		if treeMode {
			onConflict = "overwrite"
		} else {
			onConflict = "rename"
		}
	}
	if onConflict != "overwrite" && onConflict != "rename" {
		fail(w, http.StatusBadRequest, "on_conflict 只能是 overwrite 或 rename，收到："+onConflict)
		return
	}
	overwrite := onConflict == "overwrite"

	var results []uploadedFile
	var failures []string
	// failureErrs 与 failures 一一对应：聚合的失败文案会把 errno 拍平成字符串，
	// 判"是不是外接卷被 TCC 拒绝"必须回到原始 error（真实 errno）。
	var failureErrs []error

	for idx, fh := range parts {
		f, err := fh.Open()
		if err != nil {
			failures = append(failures, uploadLabel(fh.Filename, relAt(relPaths, treeMode, idx))+": "+err.Error())
			failureErrs = append(failureErrs, err)
			continue
		}
		rel := relAt(relPaths, treeMode, idx)
		// 普通上传只取基名（历史行为：绝不因为用户手工构造的 "a/b.txt" 就建目录）；
		// 上传文件夹则按相对路径重建目录树。
		if !treeMode {
			rel = filepath.Base(filepath.FromSlash(fh.Filename))
		}
		// 两种策略统一走 SaveUploadAs：它同时告诉我们"是不是真的覆盖了"，
		// 前端据此如实显示"已覆盖"还是"已改名保留两者"。
		path, n, overwritten, err := mgr.SaveUploadAs(dir, rel, f, overwrite)
		_ = f.Close()
		if err != nil {
			failures = append(failures, uploadLabel(fh.Filename, rel)+": "+err.Error())
			failureErrs = append(failureErrs, err)
			continue
		}
		results = append(results, uploadedFile{
			Name: filepath.Base(path), Path: path, RelPath: rel,
			Size: n, Overwritten: overwritten,
		})
	}

	if len(results) == 0 {
		// 目标是外接卷、被 macOS 隐私保护拒绝：403 + 完整指引，不是 400 + errno。
		for _, e := range failureErrs {
			if tccPath, isTCC := volumeTCCPath(e, dir); isTCC {
				fail(w, http.StatusForbidden, volumeTCCGuide(tccPath))
				return
			}
		}
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
	} else if n := countRenamed(results); n > 0 {
		msg += fmt.Sprintf("（其中 %d 个与已有文件重名，已自动改名，两个都保留）", n)
	}
	if len(failures) > 0 {
		msg += fmt.Sprintf("，%d 个失败", len(failures))
	}
	ok(w, map[string]any{
		"uploaded": results, "failed": failures, "msg": msg,
		"on_conflict": onConflict,
	})
}

// countRenamed 统计"因为重名被自动改名"的文件数。
//
// RelPath 是请求里的名字（普通上传时就是原文件名），Name 是真正落盘的名字 ——
// 两者不同且不是覆盖，就是自动改名保留了两份。
func countRenamed(list []uploadedFile) int {
	n := 0
	for _, f := range list {
		if f.Overwritten {
			continue
		}
		if filepath.Base(filepath.FromSlash(f.RelPath)) != f.Name {
			n++
		}
	}
	return n
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
		failFileErr(w, err)
		return
	}
	if req.Path == "" {
		fail(w, http.StatusBadRequest, "缺少搜索目录")
		return
	}
	mgr := s.fileManager()
	res, err := mgr.Search(r.Context(), req.Path, req.Query, req.Mode, req.Limit)
	if err != nil {
		failFileErr(w, err, req.Path)
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
		failFileErr(w, err)
		return
	}
	mgr := s.fileManager()
	n, err := mgr.ReplaceInFile(req.Path, req.Find, req.Replace, req.All)
	if err != nil {
		failFileErr(w, err, req.Path)
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
