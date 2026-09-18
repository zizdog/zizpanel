package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  「上传与执行限制」接口
//
//  用户报障（2026-09-20）：phpMyAdmin 导入几十 MB 的 SQL 报
//  **413 Request Entity Too Large**，而面板里**找不到任何修改入口**。
//
//  这里补上的就是那个入口：
//    GET  /api/v1/settings/upload-limits   → 配置值 + **回读的生效值**
//    POST /api/v1/settings/upload-limits   → 校验（非法 400）→ 落配置 →
//                                            任务中心应用（写 vhost + conf.d →
//                                            reload nginx → 重启 php-fpm）→ 回读
//
//  设计要点：
//   1. 保存是**异步任务**：它要写多个 nginx 配置、reload、重启每个 PHP 版本的
//      fpm，几十秒到几分钟；同步请求会让用户只能看着"请等待"，而且挂上
//      r.Context() 后一刷新就把任务杀了（见 AGENTS.md 第三节的规矩）。
//   2. 非法输入必须在**创建任务之前**返回 400 + 人话：把明显错误的输入丢进
//      后台任务，用户只会在任务中心看到一条失败，体验更差。
//   3. 回读**不是**"读一遍配置就算"：nginx 侧读磁盘上的 vhost 生效指令，
//      PHP 侧真的跑一次该版本的 `php -r ini_get(...)`。这样界面能区分
//      "已保存"与"已生效"——只报"已保存"就是在谎报。
// ============================================================================

// uploadLimits 返回面板配置里的上传/执行限制（空值补默认）。
//
// 唯一映射处：config 的五个字段 ↔ sites.Limits。别在别处再拼一份。
func (s *Server) uploadLimits() sites.Limits {
	return sites.Limits{
		ClientMaxBodySize: s.Cfg.NginxClientMaxBodySize,
		UploadMaxFilesize: s.Cfg.PHPUploadMaxFilesize,
		PostMaxSize:       s.Cfg.PHPPostMaxSize,
		MemoryLimit:       s.Cfg.PHPMemoryLimit,
		MaxExecutionTime:  s.Cfg.PHPMaxExecutionTime,
	}.Normalize()
}

// setUploadLimits 把限制写进内存配置（不落盘；落盘由任务里的 Save 负责）。
func (s *Server) setUploadLimits(l sites.Limits) {
	l = l.Normalize()
	s.Cfg.NginxClientMaxBodySize = l.ClientMaxBodySize
	s.Cfg.PHPUploadMaxFilesize = l.UploadMaxFilesize
	s.Cfg.PHPPostMaxSize = l.PostMaxSize
	s.Cfg.PHPMemoryLimit = l.MemoryLimit
	s.Cfg.PHPMaxExecutionTime = l.MaxExecutionTime
}

// limitFileState 是一个 nginx 配置文件的请求体上限回读结果。
type limitFileState struct {
	File  string `json:"file"`
	Value string `json:"value"`
	Line  int    `json:"line"`
	OK    bool   `json:"ok"`
	// Managed 表示这个文件是**面板生成的**（保存时面板会重写它）。
	// 不是面板生成的文件（用户自己写的 vhost）面板**不会动**，界面上必须说清。
	Managed bool `json:"managed"`
	// Note 是这一行的人话说明（例如"全局默认值：站点 vhost 里没写时用它"）。
	Note string `json:"note,omitempty"`
	// Path 是文件的绝对路径（界面给一个「打开」按钮，用户能自己看真实内容）。
	Path string `json:"path,omitempty"`
}

// phpHardLimitState 是一个 PHP 版本的生效值回读结果。
type phpHardLimitState struct {
	Version string `json:"version"`
	Binary  string `json:"binary"`
	// Fragment 是面板写的 conf.d 片段路径（空 = 没找到该版本的 conf.d）。
	Fragment string `json:"fragment"`
	// IniPath 是 brew 的 php.ini（**面板刻意不改它**：brew 升级会覆盖，用户手改会丢）。
	// 界面要把它显示出来并写明"这一份不生效来源" —— 2026-09-18 用户报障：
	// "根本就不读真实文件！保存也不会写入真实的配置文件"（他看的是 php.ini，
	// 而真正生效的是面板的 conf.d 片段）。
	IniPath string `json:"ini_path,omitempty"`
	// IniValues 是 php.ini 里的出厂值（只读展示，用于对照）。
	IniValues map[string]string `json:"ini_values,omitempty"`
	// FragmentOK 表示片段内容与当前配置一致。
	FragmentOK bool `json:"fragment_ok"`
	// Values 是回读到的真实 ini 值（`php-cgi -f` 优先；回读不到时为空）。
	Values map[string]string `json:"values,omitempty"`
	// SAPI 是回读用的 SAPI（php-cgi / php）。CLI（php）会把 max_execution_time
	// 强制成 0，那时这一项只能标"不可复核"，不能据此判定失败。
	SAPI string `json:"sapi,omitempty"`
	// Note 是回读时的如实说明（例如 CLI 无法复核 max_execution_time）。
	Note string `json:"note,omitempty"`
	// OK 表示"片段一致 且 回读值都等于配置值"。
	OK bool `json:"ok"`
	// Error 是回读失败的原因（没装/执行失败），非空时界面显示"未复核"。
	Error string `json:"error,omitempty"`
}

// readPHPLimitsFromIni 从 php.ini（**不是**面板的 conf.d 片段）里读出这四个上限。
//
// 只用于界面**对照显示**：让用户一眼看到"brew 的 php.ini 写的是 2M，
// 而真正生效的是下面那个面板片段（512M）"—— 2026-09-18 用户报障
// "根本就不读真实文件！保存也不会写入真实的配置文件"，原因正是他打开的是 php.ini。
func readPHPLimitsFromIni(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	want := map[string]bool{
		"upload_max_filesize": true, "post_max_size": true,
		"memory_limit": true, "max_execution_time": true,
	}
	out := map[string]string{}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if !want[k] {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// uploadLimitsView 是 GET / POST 的统一响应形状。
type uploadLimitsView struct {
	Limits   sites.Limits `json:"limits"`
	Defaults sites.Limits `json:"defaults"`
	// Nginx 是每个 vhost 文件里生效的 client_max_body_size。
	Nginx    []limitFileState `json:"nginx"`
	NginxDir string           `json:"nginx_dir"`
	// PHP 是每个已安装 PHP 版本的限制片段与真实生效值。
	PHP []phpHardLimitState `json:"php"`
	// Mismatch = 有任何一处磁盘上的生效值还不是配置里的值。
	// 界面据此显示"还有 N 处未生效 → 点应用"。
	Mismatch bool `json:"mismatch"`
}

// uploadLimitsReq 是保存请求体。指针字段：只有传了的字段才改，便于将来做局部更新。
type uploadLimitsReq struct {
	ClientMaxBodySize *string `json:"client_max_body_size"`
	UploadMaxFilesize *string `json:"upload_max_filesize"`
	PostMaxSize       *string `json:"post_max_size"`
	MemoryLimit       *string `json:"memory_limit"`
	MaxExecutionTime  *int    `json:"max_execution_time"`
}

// merge 把请求合并到当前配置上。
func (r uploadLimitsReq) merge(cur sites.Limits) sites.Limits {
	out := cur.Normalize()
	if r.ClientMaxBodySize != nil {
		out.ClientMaxBodySize = strings.TrimSpace(*r.ClientMaxBodySize)
	}
	if r.UploadMaxFilesize != nil {
		out.UploadMaxFilesize = strings.TrimSpace(*r.UploadMaxFilesize)
	}
	if r.PostMaxSize != nil {
		out.PostMaxSize = strings.TrimSpace(*r.PostMaxSize)
	}
	if r.MemoryLimit != nil {
		out.MemoryLimit = strings.TrimSpace(*r.MemoryLimit)
	}
	if r.MaxExecutionTime != nil {
		out.MaxExecutionTime = *r.MaxExecutionTime
	}
	return out
}

// handleGetUploadLimits 返回配置值与回读的生效值。
func (s *Server) handleGetUploadLimits(w http.ResponseWriter, r *http.Request) {
	lim := s.uploadLimits()
	ok(w, s.collectUploadLimits(lim))
}

// handleSaveUploadLimits 校验 → 落配置 → 走任务中心应用 → 回读。
func (s *Server) handleSaveUploadLimits(w http.ResponseWriter, r *http.Request) {
	var req uploadLimitsReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	lim := req.merge(s.uploadLimits())
	// 校验必须在建任务之前：非法输入给 400 + 人话，别丢进后台再失败。
	if err := lim.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 先把值写进内存配置并落盘：即使任务在应用阶段失败，用户的设置也不会白填
	// （重试时不必重新输入）。应用结果由任务如实报告。
	s.setUploadLimits(lim)
	if err := s.Cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, "保存面板配置失败: "+err.Error())
		return
	}
	s.launchTask(w, r, "settings", "upload-limits",
		"应用上传与执行限制（nginx 请求体 + PHP 上传/执行）", "settings_upload_limits",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.applyUploadLimits(ctx, log, lim)
		})
}

// applyUploadLimits 是任务体：写配置 → reload nginx → 重启 php-fpm → 回读。
//
// 失败策略（刻意区分"致命"与"尽力而为"，且都必须如实说明）：
//   - PHP 限制片段写入失败：致命（这正是报障的一半，写不进去等于没修）；
//   - 某个站点 vhost 重写失败：不致命（可能它的 PHP 端点没在跑），
//     进日志如实列出，用户可单独去修；
//   - 默认站点 / phpMyAdmin 入口失败：致命（用户的 phpMyAdmin 导入路径就在这里）；
//   - 某个 php-fpm 重启失败：致命（配置写了不重启＝没生效），但会**先**把
//     回读结果与补救动作写进日志，再返回错误。
func (s *Server) applyUploadLimits(ctx context.Context, log tasks.LogFunc, lim sites.Limits) (any, error) {
	lim = lim.Normalize()
	if err := lim.Validate(); err != nil {
		return nil, err
	}
	log(tasks.LevelStep, "目标值：nginx client_max_body_size="+lim.ClientMaxBodySize+
		"；PHP upload="+lim.UploadMaxFilesize+" post="+lim.PostMaxSize+
		" memory="+lim.MemoryLimit+" max_execution_time="+strconv.Itoa(lim.MaxExecutionTime)+"s")

	// ---- 1. 落盘面板配置（重启后仍在）----
	if err := s.Cfg.Save(); err != nil {
		return nil, fmt.Errorf("保存面板配置失败: %w", err)
	}
	log(tasks.LevelOK, "面板配置已保存（面板重启后仍然生效）")

	// ---- 2. PHP conf.d 片段 ----
	versions := sites.DiscoverPHPVersions(s.Cfg.BrewPrefix)
	var restartVersions []string
	if len(versions) == 0 {
		log(tasks.LevelWarn, "本机没有发现已安装的 PHP 版本：跳过 PHP 限制片段（nginx 侧仍会应用）")
	}
	for _, v := range versions {
		lr, err := sites.EnsurePHPLimits(s.Cfg.BrewPrefix, v.Version, lim, false)
		if err != nil {
			log(tasks.LevelErr, "PHP "+v.Version+" 限制片段写入失败："+err.Error())
			return nil, fmt.Errorf("PHP %s 的上传/执行限制片段写入失败: %w", v.Version, err)
		}
		if lr.Changed {
			log(tasks.LevelOK, "已写入 "+lr.Path)
			restartVersions = append(restartVersions, v.Version)
		} else {
			log(tasks.LevelOut, "PHP "+v.Version+" 的片段已是最新（未改写）")
		}
	}

	// ---- 3. phpMyAdmin 的 ExecTimeLimit 与实际 max_execution_time 对齐 ----
	// 338 秒的 PHP 上限 + phpMyAdmin 自己 300 秒的硬限制，会让"改大了却还是超时"
	// 变成必然。这里保证两者一致（没装 phpMyAdmin 时是无操作）。
	pmaConf := services.PhpMyAdminConfPath(s.Cfg.BrewPrefix)
	if changed, err := services.SyncPhpMyAdminExecTimeLimit(pmaConf, lim.MaxExecutionTime, s.Cfg.User); err != nil {
		log(tasks.LevelWarn, "phpMyAdmin 的 ExecTimeLimit 同步失败（不影响 nginx/PHP 生效）："+err.Error())
	} else if changed {
		log(tasks.LevelOK, "已把 phpMyAdmin 的 $cfg['ExecTimeLimit'] 对齐到 "+
			strconv.Itoa(lim.MaxExecutionTime)+" 秒")
	} else {
		log(tasks.LevelOut, "phpMyAdmin 的 ExecTimeLimit 已是最新（或未安装 phpMyAdmin）")
	}

	// ---- 3b. 把**全局**请求体上限写进 nginx.conf 的 http 块 ----
	//
	// 为什么必须写这一份（2026-09-18 用户报障："保存也不会写入真实的配置文件"）：
	// 面板过去只改自己生成的站点 vhost 与默认站点，nginx.conf 里那一行保持出厂值。
	// 对用户来说"我改了设置，配置文件却没变"就是没生效 —— 而且**没被面板管理的
	// vhost**（用户自己写的站）根本没有 vhost 级设置，用的正是这个全局值。
	// 这里复用「性能调整」那套写入器（备份 → 改写 → nginx -t → 失败回滚 → 回读），
	// 只改 client_max_body_size 一项，其余参数原样保留。
	if mb, ok := sites.ParseSizeBytes(lim.ClientMaxBodySize); ok && mb > 0 {
		mbInt := int(mb / (1024 * 1024))
		if mbInt < 1 {
			mbInt = 1
		}
		if cur, err := s.readNginxTuning(ctx); err != nil {
			log(tasks.LevelWarn, "读不到 nginx 当前参数，跳过全局值写入（站点 vhost 的值仍然生效）："+err.Error())
		} else {
			cur.ClientMaxBodySizeMB = mbInt
			enc, _ := priv.TuningEncode(cur)
			if _, werr := s.callHelper(ctx, "nginx-tuning-write", "-values", enc); werr != nil {
				// 不致命：站点 vhost 与默认站点已经带上新值（用户真正要修的 phpMyAdmin 路径在那里）。
				// 但必须如实说，并给出下一步。
				log(tasks.LevelErr, "全局 client_max_body_size 写入 nginx.conf 失败："+werr.Error())
				log(tasks.LevelWarn, "（站点 vhost 与默认站点的值已生效；要在 nginx.conf 里也写全局值，"+
					"可到「网站管理 → ⚙️ Nginx 管理 → 性能调整」改 client_max_body_size）")
			} else {
				log(tasks.LevelOK, "已写入全局值：nginx.conf 的 http 块 client_max_body_size "+
					strconv.Itoa(mbInt)+"m（其余 nginx 参数未动）")
			}
		}
	}

	// ---- 4. 重新生成站点 vhost（带上新的 client_max_body_size）----
	// 这是"尽力而为"的一段：某个站点可能因为 PHP 端点没在跑而暂时应用不了，
	// 那不该挡住其它站点与默认站点（用户真正要修的 phpMyAdmin 路径）。
	siteList, err := s.siteMgr().List(ctx)
	if err != nil {
		log(tasks.LevelWarn, "读取站点列表失败，跳过站点 vhost 重写："+err.Error())
	} else {
		var failed []string
		for _, st := range siteList {
			if st == nil {
				continue
			}
			if err := s.applySite(ctx, st); err != nil {
				failed = append(failed, st.Domain)
				log(tasks.LevelErr, "站点 "+st.Domain+" 的配置未能应用："+err.Error())
				continue
			}
			log(tasks.LevelOut, "站点 "+st.Domain+" 已写入 client_max_body_size "+lim.ClientMaxBodySize)
		}
		if len(failed) > 0 {
			log(tasks.LevelWarn, "以下站点的配置没改成（其它站点与默认站点不受影响）："+
				strings.Join(failed, "、")+"。修好它们（常见原因：PHP 版本没在运行）后再点一次应用")
		}
	}

	// ---- 5. 默认站点（phpMyAdmin 的导入路径）----
	// 默认站点里既有 server 级上限，也有 phpMyAdmin 那个 location 自己的上限；
	// 面板会整份重写它并做请求级复核（见 applyDefaultVhost）。
	if err := s.applyDefaultVhost(ctx); err != nil {
		log(tasks.LevelErr, "默认站点 / phpMyAdmin 入口应用失败："+err.Error())
		return nil, fmt.Errorf("默认站点 / phpMyAdmin 入口应用失败: %w", err)
	}
	log(tasks.LevelOK, "默认站点与 phpMyAdmin 入口已应用（含 client_max_body_size "+
		lim.ClientMaxBodySize+"）")

	// ---- 6. 重启对应 php-fpm（改了 conf.d 必须重启才生效）----
	var restartErrs []string
	for _, version := range restartVersions {
		svcName := s.phpServiceName(version)
		if svcName == "" {
			msg := "PHP " + version + " 的限制片段已写入，但面板里没有它的服务记录，" +
				"无法重启：该版本下次启动时自动生效（也可到「网站管理 → PHP 环境」修复端点并重启）"
			log(tasks.LevelWarn, msg)
			continue
		}
		if _, err := s.svcManager().Action(ctx, svcName, "restart"); err != nil {
			restartErrs = append(restartErrs, version)
			log(tasks.LevelErr, "重启 "+svcName+" 失败："+err.Error())
			continue
		}
		endpoint, _ := sites.PreferredEndpoint(s.Cfg.BrewPrefix, version)
		if endpoint != "" && !waitEndpointLive(endpoint, 15*time.Second) {
			log(tasks.LevelWarn, "已重启 "+svcName+"，但 "+endpoint+
				" 上暂时没探测到监听（PHP 可能仍在启动；导入仍失败请回来看这里的回读值）")
		}
		log(tasks.LevelOK, "已重启 "+svcName)
	}

	// ---- 7. 回读生效值（不能只报"已保存"）----
	view := s.collectUploadLimits(lim)
	log(tasks.LevelStep, "回读生效值：")
	for _, f := range view.Nginx {
		if f.OK {
			log(tasks.LevelOK, "nginx "+f.File+" → client_max_body_size "+f.Value)
		} else {
			log(tasks.LevelWarn, "nginx "+f.File+" → "+
				describeLimitValue(f.Value)+"（期望 "+lim.ClientMaxBodySize+"）")
		}
	}
	for _, p := range view.PHP {
		if p.Error != "" {
			log(tasks.LevelWarn, "PHP "+p.Version+" 生效值未能回读："+p.Error+"（如实标为未复核）")
			continue
		}
		log(tasks.LevelOut, fmt.Sprintf("PHP %s → upload_max_filesize=%s post_max_size=%s memory_limit=%s max_execution_time=%s（%s 回读；片段 %s）",
			p.Version, p.Values["upload_max_filesize"], p.Values["post_max_size"],
			p.Values["memory_limit"], p.Values["max_execution_time"], p.SAPI, p.Fragment))
		if p.Note != "" {
			log(tasks.LevelWarn, "PHP "+p.Version+"："+p.Note)
		}
	}
	if view.Mismatch {
		log(tasks.LevelWarn, "有配置项还没变成目标值（见上面的回读行）——"+
			"常见原因：php-fpm 没重启成功、或某个站点暂时应用不了")
	} else {
		log(tasks.LevelOK, "回读通过：磁盘上的生效值都等于目标值")
	}

	if len(restartErrs) > 0 {
		return view, fmt.Errorf("以下 PHP 版本的 php-fpm 重启失败，新限制尚未生效：%s。"+
			"请到「服务管理」查看它们的日志后重试", strings.Join(restartErrs, "、"))
	}
	if view.Mismatch {
		return view, fmt.Errorf("配置已写入，但回读发现仍有生效值不是目标值（详见任务日志）：" +
			"请确认 nginx 与 php-fpm 的状态后重试")
	}
	return view, nil
}

// describeLimitValue 把回读到的值写成人话（空 = 该文件里没有这条指令）。
func describeLimitValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return "文件里没有 client_max_body_size（会用 nginx 出厂默认 1m）"
	}
	return "client_max_body_size " + v
}

// collectUploadLimits 回读当前**实际生效**的上传/执行限制。
//
// 两路证据：
//   - nginx：读 vhost 目录里每个 .conf 的 client_max_body_size（跳过注释行）；
//   - PHP：读面板片段内容 + 真的执行该版本的 php 回读 ini 值。
func (s *Server) collectUploadLimits(lim sites.Limits) uploadLimitsView {
	lim = lim.Normalize()
	view := uploadLimitsView{
		Limits: lim, Defaults: sites.DefaultLimits(),
		Nginx: []limitFileState{}, PHP: []phpHardLimitState{},
		NginxDir: s.Cfg.VhostDir,
	}
	// ---- nginx 侧 ----
	//
	// 先加**全局**那一行（nginx.conf 的 http 块）：站点 vhost 里没写
	// client_max_body_size 时，用的就是它 —— 这是"真实文件"里最容易被忽略的一份，
	// 也是 2026-09-18 用户说"不读真实文件"时真正想看的那一份。
	if b, err := os.ReadFile(s.Cfg.NginxConf); err == nil {
		val, line := sites.FindClientMaxBodySize(string(b))
		valTrim := strings.TrimSpace(val)
		st := limitFileState{
			File: "nginx.conf（http 全局）", Value: val, Line: line, Path: s.Cfg.NginxConf,
			Managed: true,
			OK:      valTrim == strings.TrimSpace(lim.ClientMaxBodySize),
		}
		switch {
		case st.OK:
			st.Note = "全局默认值：站点 vhost 里没写 client_max_body_size 时用它（这一行与目标值一致）"
		case valTrim == "":
			// 没写这一行是**正常状态**（nginx 出厂 1m，站点 vhost 各自带值）：
			// 不算"未生效"，但提示保存会把它写进来。
			st.Note = "这一行还没有（用 nginx 出厂值 1m；点「保存并应用」会把全局值写进这里）"
		default:
			st.Note = "这一行的值与目标值不一致 —— 点「保存并应用」会改写它"
			view.Mismatch = true
		}
		view.Nginx = append(view.Nginx, st)
	}
	entries, _ := os.ReadDir(s.Cfg.VhostDir)
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join(s.Cfg.VhostDir, name))
		if err != nil {
			continue
		}
		val, line := sites.FindClientMaxBodySize(string(b))
		managed := strings.Contains(string(b), services.DefaultVhostMarker) ||
			strings.Contains(string(b), "由 ZizPanel 生成")
		st := limitFileState{
			File: name, Value: val, Line: line, OK: val == lim.ClientMaxBodySize,
			Managed: managed, Path: filepath.Join(s.Cfg.VhostDir, name),
		}
		if !managed {
			st.Note = "这个 vhost 不是面板生成的：面板**不会改它**（保存时跳过）。" +
				"它里面的值会覆盖全局值 —— 要改它请用「网站管理 → ⚙️ 配置文件」手工改，" +
				"或把它交给面板管理（重建配置）"
			// 非面板管理的文件**不参与** mismatch 判定：面板改不了它，
			// 把它算成"未生效"等于让用户永远点不绿（那是假故障）。
		} else if !st.OK {
			view.Mismatch = true
		}
		view.Nginx = append(view.Nginx, st)
	}
	// ---- PHP 侧 ----
	for _, v := range sites.DiscoverPHPVersions(s.Cfg.BrewPrefix) {
		st := phpHardLimitState{Version: v.Version, Binary: v.Binary}
		// php.ini 现状（**面板不改它**，只用于对照说明"真正生效的是下面的片段"）
		if ini := filepath.Join(s.Cfg.BrewPrefix, "etc", "php", v.Version, "php.ini"); fileExists(ini) {
			st.IniPath = ini
			st.IniValues = readPHPLimitsFromIni(ini)
		}
		frag := sites.PHPConfDPath(s.Cfg.BrewPrefix, v.Version)
		st.Fragment = frag
		if b, err := os.ReadFile(frag); err == nil {
			st.FragmentOK = string(b) == sites.PHPIniFragment(lim)
		}
		vals, err := sites.ProbePHPLimits(v.Binary)
		if err != nil {
			// 回读不到**不等于没生效**：可能该版本的 php 二进制不在/不可执行。
			// 这种时候只能如实标"未复核"（界面与任务日志都会显示原因），
			// 不能凭"读不到"就判定失败（那是另一种谎报）。
			st.Error = err.Error()
		} else {
			st.SAPI = vals["_sapi"]
			delete(vals, "_sapi")
			st.Values = vals
			execOK := true
			if st.SAPI == "php" {
				// CLI SAPI 强制 max_execution_time=0，这一项在 CLI 下永远读成 0
				// （实测：php -r → 0，php-cgi -f → 300）。无法复核 ≠ 没生效，
				// 所以如实标注，并且**不**据此判定失败。
				st.Note = "回读用的是 PHP CLI：它会把 max_execution_time 强制为 0，这一项无法复核" +
					"（该版本没有 php-cgi 时才会走到这里）"
			} else {
				execOK = strings.TrimSpace(vals["max_execution_time"]) == strconv.Itoa(lim.MaxExecutionTime)
			}
			st.OK = st.FragmentOK && phpSizeValuesMatch(vals, lim) && execOK
			if !st.OK {
				view.Mismatch = true
			}
		}
		view.PHP = append(view.PHP, st)
	}
	return view
}

// phpSizeValuesMatch 判断 php 回读到的三个尺寸是否等于目标（按字节比较，
// 免得 "512M" 与 "512m" 这种大小写差异被误判成没生效）。
// max_execution_time 不在这里比：它取决于回读用的 SAPI（CLI 恒为 0）。
func phpSizeValuesMatch(vals map[string]string, lim sites.Limits) bool {
	sizeEq := func(got, want string) bool {
		g, ok1 := sites.ParseSizeBytes(got)
		w, ok2 := sites.ParseSizeBytes(want)
		return ok1 && ok2 && g == w
	}
	return sizeEq(vals["upload_max_filesize"], lim.UploadMaxFilesize) &&
		sizeEq(vals["post_max_size"], lim.PostMaxSize) &&
		sizeEq(vals["memory_limit"], lim.MemoryLimit)
}
