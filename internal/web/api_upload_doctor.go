package web

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  「大文件上传自检」（用户 2026-09-18 报障："我上传数据库，现在报 500 Internal Server Error"）
//
//  为什么需要它：413 与 500 是**两件完全不同的事**，而用户看到的都只是一句话：
//    · 413 = 请求超过 `client_max_body_size`，在 nginx 层被拒 —— 改上限就行；
//    · 500 = 请求**已经被接受**，然后某一步真的失败了：nginx 写请求体临时文件失败
//      （最常见的是**磁盘满**："No space left on device"）、PHP-FPM 进程崩了、
//      PHP 致命错误（内存不足）…… 真正的线索只在 **nginx error_log** 里。
//
//  所以这里做三件事，全部是**只读**检查：
//    ① 磁盘剩余空间（对比用户设的上传上限：nginx 会把请求体缓冲到磁盘，
//       空间不够时就是 500，而不是 413）；
//    ② nginx 的请求体/代理临时目录存在且**worker 用户可写**（权限不对也是 500）；
//    ③ 把 nginx error_log 里最近与上传相关的行**原样捞出来**给用户看。
//
//  绝不猜结论：拿不到的东西如实写"读不到"，verdict 里也只说"风险"。
// ============================================================================

// uploadDoctor 是自检结果。
type uploadDoctor struct {
	DiskPath       string   `json:"disk_path"`
	DiskFreeBytes  int64    `json:"disk_free_bytes"`
	DiskTotalBytes int64    `json:"disk_total_bytes"`
	BodyLimitBytes int64    `json:"body_limit_bytes"`
	BodyLimitText  string   `json:"body_limit_text"`
	TempDirs       []string `json:"temp_dirs"`
	Warnings       []string `json:"warnings"`
	NginxErrors    []string `json:"nginx_errors"`
	NginxLogPath   string   `json:"nginx_log_path,omitempty"`
	WorkerUser     string   `json:"worker_user,omitempty"`
	Verdict        string   `json:"verdict"`
}

// statfsFree 取某个路径所在卷的空闲/总空间（测试可注入）。
var statfsFree = func(path string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Bavail 是"非特权用户可用块数"——正是 nginx worker 能用到的量。
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}

// nginxTempDirs 返回 brew 版 nginx 的临时目录（请求体缓冲就落在这里）。
//
// 路径来自 `nginx -V` 的编译期默认（brew 固定在 <prefix>/var/run/nginx/…）；
// 面板不解析 -V 输出，是因为这些默认值在本项目支持的唯一平台上就是这几个。
func (s *Server) nginxTempDirs() []string {
	base := filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx")
	return []string{
		filepath.Join(base, "client_body_temp"),
		filepath.Join(base, "fastcgi_temp"),
		filepath.Join(base, "proxy_temp"),
	}
}

// uploadDoctorReport 现场做一次只读自检。
func (s *Server) uploadDoctorReport() uploadDoctor {
	lim := s.uploadLimits()
	limitBytes, _ := sites.ParseSizeBytes(lim.ClientMaxBodySize)
	out := uploadDoctor{
		BodyLimitText:  lim.ClientMaxBodySize,
		BodyLimitBytes: limitBytes,
		TempDirs:       s.nginxTempDirs(),
	}
	// ① 磁盘：临时目录与站点目录所在卷都要够。
	checkPath := filepath.Join(s.Cfg.WWWRoot, "localhost")
	if _, err := os.Stat(checkPath); err != nil {
		checkPath = s.Cfg.WWWRoot
	}
	out.DiskPath = checkPath
	free, total, derr := statfsFree(checkPath)
	if derr != nil {
		out.Warnings = append(out.Warnings, "读不到磁盘空间（"+derr.Error()+"）—— 无法判断空间是否够用")
	} else {
		out.DiskFreeBytes, out.DiskTotalBytes = free, total
		if out.BodyLimitBytes > 0 && free < out.BodyLimitBytes {
			out.Warnings = append(out.Warnings, "磁盘可用空间（"+humanBytes(free)+"）小于你设置的上传上限（"+
				lim.ClientMaxBodySize+"）：nginx 会把请求体缓冲到磁盘，**空间不够时直接返回 500**（不是 413）。"+
				"先腾出空间，或把上传上限调到小于可用空间")
		} else if out.BodyLimitBytes > 0 && free < out.BodyLimitBytes*3 {
			out.Warnings = append(out.Warnings, "磁盘可用空间（"+humanBytes(free)+"）不到上传上限的 3 倍 —— "+
				"大文件导入时 nginx 与 MySQL 都要临时空间，建议留足余量")
		}
	}
	// ② 临时目录：存在 + worker 用户可写。
	user := s.nginxWorkerUser()
	out.WorkerUser = user
	for _, dir := range out.TempDirs {
		st, err := os.Stat(dir)
		if err != nil {
			out.Warnings = append(out.Warnings, dir+" 不存在 —— nginx 无法缓冲上传的请求体"+
				"（大文件上传会失败；重建目录：mkdir -p "+dir+"）")
			continue
		}
		if !st.IsDir() {
			out.Warnings = append(out.Warnings, dir+" 不是目录（nginx 无法写入）")
			continue
		}
		if user != "" && !dirWritableBy(dir, user) {
			out.Warnings = append(out.Warnings, dir+" 对 nginx worker 用户（"+user+"）不可写 —— "+
				"大文件上传会失败；修：sudo chown -R "+user+" "+dir)
		}
	}
	// ③ nginx 错误日志：把最近与上传/500 相关的行原样捞出来。
	logPath, lines := s.recentUploadErrors()
	out.NginxLogPath, out.NginxErrors = logPath, lines
	if len(lines) == 0 && logPath != "" {
		out.Warnings = append(out.Warnings, "最近没有与上传相关的 nginx 错误行（"+logPath+
			"）—— 500 也可能来自 PHP-FPM / phpMyAdmin 自身，请看「日志中心 → PHP」")
	}
	if logPath == "" {
		out.Warnings = append(out.Warnings, "找不到 nginx error_log —— 无法给出服务端原因")
	}
	switch {
	case len(out.Warnings) == 0:
		out.Verdict = "没有发现明显障碍：磁盘空间、nginx 临时目录都正常。"
	default:
		out.Verdict = "发现 " + itoaSmall(len(out.Warnings)) + " 个可能让大文件上传失败的问题（见上）。"
	}
	return out
}

// nginxWorkerUser 从 nginx.conf 里读 `user` 指令（worker 以谁的身份跑）。
//
// 读不到就返回空 —— 此时**不做**可写性判断（不猜），也不拿 root 的身份去"验证"。
func (s *Server) nginxWorkerUser() string {
	b, err := os.ReadFile(s.Cfg.NginxConf)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(stripInlineComment(ln))
		if !strings.HasPrefix(line, "user") {
			continue
		}
		f := strings.Fields(strings.TrimSuffix(line, ";"))
		if len(f) >= 2 {
			return f[1]
		}
	}
	return ""
}

// stripInlineComment 去掉行尾 # 注释（不处理引号里的 # —— nginx.conf 里没有那种写法）。
func stripInlineComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

// dirWritableBy 粗略判断"某个用户能不能写这个目录"：owner 是该用户且属主有写位。
//
// 为什么不做"以该用户身份真写一次"：那要提权 fork，而且会在目录里留下探针文件；
// 而这里要回答的是"权限看起来对不对"这个更弱、但**不会误报**的问题——
// owner 与写位都对时，剩下唯一的例外是更上层目录的执行位，那种情况罕见且会在
// error_log 里留下明确的 permission denied（用户下一步就能看到）。
func dirWritableBy(dir, user string) bool {
	st, err := os.Stat(dir)
	if err != nil {
		return false
	}
	if st.Mode().Perm()&0o200 == 0 {
		return false
	}
	uid := lookupUID(user)
	if uid < 0 {
		return true // 查不到这个用户（例如 nginx.conf 写的是别的机器上的名字）：不判断
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return int(sys.Uid) == uid
}

// lookupUID 用 id -u 查用户 id（查不到返回 -1）。
func lookupUID(user string) int {
	cmd := exec.Command("/usr/bin/id", "-u", user)
	out, err := cmd.Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, ch := range strings.TrimSpace(string(out)) {
		if ch < '0' || ch > '9' {
			return -1
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// recentUploadErrors 找 nginx error_log 并返回与"上传失败"相关的最近若干行。
//
// 只看**尾部**（默认 300 行里挑），因为用户要的是"刚才那次 500 的原因"。
func (s *Server) recentUploadErrors() (string, []string) {
	cands := []string{
		filepath.Join(s.Cfg.WWWRoot, "_logs", "nginx-error.log"),
		filepath.Join(s.Cfg.WWWRoot, "_logs", "localhost.error.log"),
		filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log"),
	}
	for _, p := range cands {
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > 300 {
			lines = lines[len(lines)-300:]
		}
		hits := []string{}
		for _, ln := range lines {
			low := strings.ToLower(ln)
			for _, key := range []string{"no space left", "client intended to send too large",
				"permission denied", "fastcgi sent in stderr", "upstream prematurely",
				"worker process", "500", "request body", "client_body_temp"} {
				if strings.Contains(low, key) {
					hits = append(hits, ln)
					break
				}
			}
		}
		if len(hits) > 12 {
			hits = hits[len(hits)-12:]
		}
		return p, hits
	}
	return "", nil
}

// itoaSmall 是个小工具（避免为一个数字引 strconv）。
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	buf := []byte{}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// handleUploadDoctor 返回自检结果（只读）。
func (s *Server) handleUploadDoctor(w http.ResponseWriter, r *http.Request) {
	ok(w, s.uploadDoctorReport())
}

// sortStrings 供测试与日志用。
func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
