package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zizdog/zizpanel/internal/upgrade"
	"github.com/zizdog/zizpanel/internal/version"
)

// ============================================================================
//  在线升级接口
//
//  设计要点（每一条都来自"升级会把面板搞挂"这个现实约束）：
//
//   1. **两条信任路径**，互不替代：
//        - 网络升级：必须通过 Ed25519 验签（公钥内嵌在二进制里）
//        - 手动上传：上传者已经是登录后的管理员（本来就等价于 root），
//          不再要求签名，但仍然做 SHA-256 与解包安全检查
//   2. **执行升级是异步的**：升级的最后一步要重启面板，
//      同步返回的 HTTP 响应根本发不出去。所以立刻返回 202 + 前端轮询，
//      而不是让浏览器挂在一个注定断开的请求上。
//   3. **状态落盘**：重启后新进程要能读到"上次升级结果如何"。
//   4. **只有 root 能升级**：面板平时以 root 通过 LaunchDaemon 运行；
//      如果不是（比如本地调试），明确拒绝，而不是做一个半吊子的升级。
// ============================================================================

const (
	maxUploadBytes = 256 << 20 // 上传包上限 256 MB

	// 单个候选源的超时预算（候选列表见 upgrade.CandidateSources）：
	//   check 外层 30s → 单候选 6s。清单只有 ~32KB，即使按 0.3MB/s 也在 1s 内；
	//     6s 足够区分"慢"与"不通"，公网候选最坏 4 个 × 6s 正好用满外层预算。
	//   stage 外层 15min → 单候选保持 20s（与 upgrade 包的 manifestTimeout 一致）。
	//     stage 的时间主要花在下载几十 MB 的升级包上（downloadTimeout=10min），
	//     找清单这一步不该、也不需要为它省时间。
	upgradeCheckPerSourceTimeout = 6 * time.Second
	upgradeStagePerSourceTimeout = 20 * time.Second
)

// upgradeManifestFetcher 是"下载清单原文 + 签名"的实现。
//
// 生产路径为 nil：upgrade.FetchManifestAny 会回落到真实 HTTP 实现。
// web 包的单测把它换成假实现以避免联网（单测不许碰真实网络），
// 而验签/解析仍由 upgrade 包在 FetchManifestAny 里统一完成 ——
// 注入的只是下载动作，注入不了"跳过验签"。
var upgradeManifestFetcher upgrade.ManifestFetcher

// resolveUpgradeSources 由"显式传入的源 / 配置里的源"得到候选列表。
//
// 显式传入优先；都没传时用配置值（可能是用户清空后的空串，那是合法的，
// 表示"按默认候选顺序自动选源"，不再是错误）。
func (s *Server) resolveUpgradeSources(requested string) []string {
	configured := strings.TrimSpace(requested)
	if configured == "" {
		configured = s.Cfg.UpgradeSource
	}
	return upgrade.CandidateSources(configured)
}

// upgradeMu 是升级线程与状态判断之间的互斥。用文件状态 + 进程内锁双保险：
// 进程内锁防并发点击，文件状态防"重启后重复升级"。
var upgradeMu sync.Mutex

// upgradeOptions 由当前配置推导出升级所需的环境。
func (s *Server) upgradeOptions() upgrade.Options {
	return upgrade.Options{
		BinDir:    s.Cfg.BinDir,
		WorkDir:   s.Cfg.WorkDir,
		Label:     "cn.zizpanel.panel",
		HealthURL: s.upgradeHealthURL(),
	}
}

// upgradeHealthURL 返回看门狗用来验证新版的健康检查地址。
//
// 必须指向**本机的 HTTPS 端口**：看门狗在面板刚刚重启、什么都还不确定的时候
// 运行，走本机回环最可靠，也避免 DNS / 反代带来的额外失败面。
func (s *Server) upgradeHealthURL() string {
	scheme := "https"
	if !s.Cfg.TLSEnable {
		scheme = "http"
	}
	return fmt.Sprintf("%s://127.0.0.1%s/api/v1/health", scheme, s.listenPortSuffix())
}

// listenPortSuffix 从 Listen 配置里取出端口部分，如 ":8443"。
func (s *Server) listenPortSuffix() string {
	l := strings.TrimSpace(s.Cfg.Listen)
	if l == "" {
		return ":8443"
	}
	if i := strings.LastIndex(l, ":"); i >= 0 {
		return l[i:]
	}
	return ":" + l
}

// stagingDirFor 返回升级暂存目录。
func (s *Server) stagingDirFor() string {
	return filepath.Join(s.Cfg.WorkDir, "upgrade", "staging")
}

// ---------- 状态查询 ----------

// handleUpgradeStatus 返回当前升级状态与自身能力信息。
//
// 前端靠这个接口完成两件事：
//   - 渲染"我能不能升级"（有没有配公钥、是不是 root、有没有已暂存的包）
//   - 轮询升级进度（升级期间连接会断，恢复后继续轮询同一个接口）
func (s *Server) handleUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	st := upgrade.LoadState(s.Cfg.WorkDir)
	staged, stagedVersion := s.stagedInfo()

	// plain_http 用"最近实际用过的源"判断，没有过成功记录时退回配置值。
	// 局域网 NAS 是 http 私有地址，不该被当成"明文公网"来吓唬用户。
	plainHTTP := upgrade.IsPlainHTTPToPublicHost(s.Cfg.UpgradeSource)
	if strings.TrimSpace(s.Cfg.UpgradeSource) == "" {
		plainHTTP = upgrade.IsPlainHTTPToPublicHost(st.SourceBase)
	}

	ok(w, map[string]any{
		"current_version": version.Version,
		"current_full":    version.Full(),
		"commit":          version.Commit,
		"build_time":      version.BuildTime,
		"go_version":      runtime.Version(),
		"arch":            upgrade.AssetKey(runtime.GOOS, runtime.GOARCH),
		"state":           st,
		"staged":          staged,
		"staged_version":  stagedVersion,
		// source 是**配置里保存的源**（空串 = 用户没显式配过，按候选自动选）。
		// 设置页的输入框读的就是它，所以语义必须保持"用户填过的值"。
		"source": s.Cfg.UpgradeSource,
		// effective_source 是**最近一次探测实际命中的源**（见 State.SourceBase）：
		// 候选顺序是动态的（同网段先走 NAS），前端/CLI 靠它才能知道真实用了哪个。
		"effective_source": st.SourceBase,
		// candidates 是当前机器上的候选顺序（含是否插入 NAS），供排障展示。
		"candidates": upgrade.CandidateSources(s.Cfg.UpgradeSource),
		// 能不能从网络升级
		"can_remote": upgrade.HasPublicKey(),
		"pubkey":     upgrade.PublicKeyFingerprint(),
		// 能不能升级（只有 root 才行）
		"can_apply":  os.Geteuid() == 0,
		"is_root":    os.Geteuid() == 0,
		"plain_http": plainHTTP,
		"notes":      s.Cfg.UpgradeNotes,
	})
}

// stagedInfo 报告暂存目录里是否已有完整的升级包，以及它的版本。
func (s *Server) stagedInfo() (bool, string) {
	dir := s.stagingDirFor()
	panel := filepath.Join(dir, upgrade.PanelBinary)
	helper := filepath.Join(dir, upgrade.HelperBinary)
	if _, err := os.Stat(panel); err != nil {
		return false, ""
	}
	if _, err := os.Stat(helper); err != nil {
		return false, ""
	}
	// 版本从状态里读（暂存时已经确认过），不再重复执行二进制
	st := upgrade.LoadState(s.Cfg.WorkDir)
	if st.Status == upgrade.StatusStaged {
		return true, st.To
	}
	return true, st.To
}

// ---------- 检查更新 ----------

type upgradeCheckReq struct {
	Source string `json:"source"`
}

// handleUpgradeCheck 拉取远端清单、验签、比较版本。
//
// 源的选择分两种情形（都不再报 400「尚未配置升级源地址」）：
//   - 调用方**显式传了** source：用它，并且写回配置（用户的选择要记住）；
//   - 没传：用配置里保存的源（可能是空串 = 没配过），交给
//     upgrade.CandidateSources 按优先级依次试。
//
// 关键：没有显式传源时**绝不写回配置**。若把候选/默认源存进去，每台机器
// 就被钉死在一个源上，同网段的 NAS 快通道再也排不到前面（见 upgrade/source.go）。
func (s *Server) handleUpgradeCheck(w http.ResponseWriter, r *http.Request) {
	var req upgradeCheckReq
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	requested := strings.TrimSpace(req.Source)
	if requested != "" && requested != s.Cfg.UpgradeSource {
		// 只有用户显式给的地址才写回配置
		s.Cfg.UpgradeSource = requested
		if err := s.Cfg.Save(); err != nil {
			s.Log.Warn("保存升级源失败: %v", err)
		}
	}
	sources := s.resolveUpgradeSources(requested)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// fail closed + 多候选认证：每个候选都必须通过验签才会被采用
	m, usedBase, err := upgrade.FetchManifestAny(ctx, sources, upgradeCheckPerSourceTimeout, upgradeManifestFetcher)
	if err != nil {
		s.Log.Warn("升级检查失败（候选顺序：%v）：%v", sources, err)
		s.audit(r, "upgrade_check", strings.Join(sources, " "), "失败: "+err.Error(), false, "")
		if errors.Is(err, upgrade.ErrNoPublicKey) {
			fail(w, http.StatusConflict,
				"面板没有内嵌发布公钥，无法验证升级包签名，因此拒绝从网络升级。"+
					"请使用「上传升级包」这条离线路径，或重新安装带公钥的版本。")
			return
		}
		fail(w, http.StatusBadGateway, "检查更新失败："+err.Error())
		return
	}
	s.Log.Info("升级检查命中源 %s（候选顺序：%v）", usedBase, sources)

	newer, err := upgrade.IsNewer(m.Version, version.Version)
	if err != nil {
		fail(w, http.StatusBadGateway, "版本号无法比较："+err.Error())
		return
	}

	ref, refErr := m.Pick(runtime.GOOS, runtime.GOARCH)
	s.Cfg.UpgradeNotes = m.Notes
	_ = s.Cfg.Save()

	// 记录"这次实际命中了哪个源"。候选是动态排序的，不落盘的话
	// 刷新一次页面就再也说不清刚才到底走的 NAS 还是公网。
	st := upgrade.LoadState(s.Cfg.WorkDir)
	st.SourceBase = usedBase
	_ = upgrade.SaveState(s.Cfg.WorkDir, st)

	s.audit(r, "upgrade_check", usedBase,
		fmt.Sprintf("命中 %s；当前 v%s，远端 v%s", usedBase, version.Version, m.Version), true, "")

	resp := map[string]any{
		"current":    version.Version,
		"latest":     m.Version,
		"has_update": newer,
		"notes":      m.Notes,
		"published":  m.PublishedAt,
		// source 保留原键名，语义 = 这次**实际命中**的源（向后兼容）
		"source": usedBase,
		// effective_source 与 source 同值，用统一命名暴露"实际用了哪个源"
		"effective_source": usedBase,
		// configured_source = 配置里保存的值（空串表示没显式配过，走默认候选）
		"configured_source": s.Cfg.UpgradeSource,
		"plain_http":        upgrade.IsPlainHTTPToPublicHost(usedBase),
	}
	if refErr == nil {
		resp["asset"] = ref
	} else {
		// 远端没有本架构的包：明确说出来，而不是让用户点升级后才发现
		resp["asset_error"] = refErr.Error()
	}
	ok(w, resp)
}

// ---------- 下载并暂存 ----------

type upgradeStageReq struct {
	Source string `json:"source"`
}

// handleUpgradeStage 下载升级包并暂存（下载、校验、解包、试运行）。
//
// 把"下载"和"应用"分开，是为了让最危险的"替换二进制"这一步
// 发生在一个已经被完整验证过的包上，而不是边下边换。
func (s *Server) handleUpgradeStage(w http.ResponseWriter, r *http.Request) {
	var req upgradeStageReq
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !upgradeMu.TryLock() {
		fail(w, http.StatusConflict, "已有一个升级任务在进行中，请等待它结束")
		return
	}
	defer upgradeMu.Unlock()

	// 与 check 相同：显式传源优先，否则按候选顺序自动选。
	// 这里**不写回配置** —— stage 只是"这一次用哪个源下载"，
	// 持久化用户选择是 check/设置页的职责。
	sources := s.resolveUpgradeSources(strings.TrimSpace(req.Source))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	// 保留上一次"实际命中的源"：万一这次探测失败，status 里仍能显示
	// 最近一次真正用过的地址，而不是被清空。
	st := &upgrade.State{
		Status:     upgrade.StatusChecking,
		Source:     "remote",
		SourceBase: upgrade.LoadState(s.Cfg.WorkDir).SourceBase,
		From:       version.Version,
		StartedAt:  time.Now(),
		Stage:      "正在获取发布清单",
	}
	_ = upgrade.SaveState(s.Cfg.WorkDir, st)

	m, usedBase, err := upgrade.FetchManifestAny(ctx, sources, upgradeStagePerSourceTimeout, upgradeManifestFetcher)
	if err != nil {
		s.Log.Warn("升级暂存获取清单失败（候选顺序：%v）：%v", sources, err)
		s.stageFailed(st, "获取清单失败: "+err.Error())
		fail(w, http.StatusBadGateway, "获取发布清单失败："+err.Error())
		return
	}
	st.SourceBase = usedBase
	s.Log.Info("升级暂存命中源 %s（候选顺序：%v）", usedBase, sources)
	newer, err := upgrade.IsNewer(m.Version, version.Version)
	if err != nil {
		s.stageFailed(st, err.Error())
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if !newer {
		st.Status = upgrade.StatusIdle
		st.Stage = ""
		st.Message = fmt.Sprintf("当前已是最新版本 v%s", version.Version)
		st.FinishedAt = time.Now()
		_ = upgrade.SaveState(s.Cfg.WorkDir, st)
		ok(w, map[string]any{"staged": false, "message": st.Message})
		return
	}

	ref, err := m.Pick(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		s.stageFailed(st, err.Error())
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	st.To = m.Version
	st.Status = upgrade.StatusDownloading
	st.Stage = fmt.Sprintf("正在下载 v%s 安装包", m.Version)
	_ = upgrade.SaveState(s.Cfg.WorkDir, st)

	tarPath := filepath.Join(s.Cfg.WorkDir, "upgrade", "download",
		fmt.Sprintf("zizpanel_%s_%s.tar.gz", m.Version, upgrade.AssetKey(runtime.GOOS, runtime.GOARCH)))
	// 局域网抓包：清单来自权威源（保证"有没有更新"判断正确），但**包**优先从 NAS 取 ——
	// 实测快两个数量级（92 MB/s vs ～0.5 MB/s）。DownloadTarball 会按清单里的 SHA-256 校验，
	// NAS 上是旧包/坏包时校验必然失败 → 自动回落到清单里的原始地址。
	tryURLs := []string{ref.URL}
	if upgrade.OnNASSubnet() {
		if lan := upgrade.LANAssetURL(ref.URL); lan != "" {
			tryURLs = append([]string{lan}, tryURLs...)
			st.Stage = fmt.Sprintf("正在下载 v%s 安装包（优先局域网镜像）", m.Version)
			_ = upgrade.SaveState(s.Cfg.WorkDir, st)
		}
	}
	// 下载进度写进 state（节流 700ms 一次）：用户 2026-09-20 要求"升级过程要有详细的
	// 内容展示"，而升级下载是这一步里唯一耗时的地方 —— 只有写了进度，界面上才看得到
	// "已下载 12.3 MB / 24.4 MB（50%）"，而不是一句静止的"正在下载"。
	//
	// 为什么节流：回调是同步的，每次都要写一次 state.json（含 fsync 语义的原子写）。
	// 不节流的话，24MB 的包会写上百次盘，白白拖慢下载本身。
	var lastProgressAt time.Time
	progress := func(written, total int64) {
		if time.Since(lastProgressAt) < 700*time.Millisecond {
			return
		}
		lastProgressAt = time.Now()
		if total > 0 {
			st.Message = fmt.Sprintf("已下载 %s / %s（%d%%）",
				upgradeHumanBytes(written), upgradeHumanBytes(total), written*100/total)
		} else {
			st.Message = "已下载 " + upgradeHumanBytes(written)
		}
		_ = upgrade.SaveState(s.Cfg.WorkDir, st)
	}

	var lastErr error
	for _, u := range tryURLs {
		if _, err := upgrade.DownloadTarballWithProgress(ctx, u, tarPath, ref.SHA256, progress); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
			if len(tryURLs) > 1 {
				st.Stage = fmt.Sprintf("局域网镜像那份没通过校验或不可达（%v），回落到原地址", err)
				_ = upgrade.SaveState(s.Cfg.WorkDir, st)
			}
		}
	}
	if lastErr != nil {
		s.stageFailed(st, lastErr.Error())
		fail(w, http.StatusBadGateway, "下载升级包失败："+lastErr.Error())
		return
	}

	if err := s.stageFromTarball(tarPath, st); err != nil {
		s.stageFailed(st, err.Error())
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(r, "upgrade_stage", "v"+m.Version,
		fmt.Sprintf("命中源 %s；已下载并暂存，等待应用", usedBase), true, "")
	ok(w, map[string]any{
		"staged":           true,
		"version":          st.To,
		"state":            st,
		"effective_source": usedBase,
	})
}

// upgradeHumanBytes 把字节数变成人读形式（只用于升级进度文案）。
//
// 面板的服务层另有一个 humanBytes，但它在 internal/services 包里；这里只有
// 升级接口用得到，为了几个字符去跨包导出并不划算 —— 而写死 "12.3 MB" 这种
// 格式化逻辑在界面文案上必须一致，所以单独放一个同规则的小函数。
func upgradeHumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}

func (s *Server) stageFailed(st *upgrade.State, msg string) {
	st.Status = upgrade.StatusFailed
	st.Error = msg
	st.Stage = ""
	st.FinishedAt = time.Now()
	_ = upgrade.SaveState(s.Cfg.WorkDir, st)
}

// stageFromTarball 解包到暂存目录，并验证新二进制真的能跑、版本可读。
//
// 这一步是"早失败"的关键：在**换掉任何东西之前**就确认包是好的。
func (s *Server) stageFromTarball(tarPath string, st *upgrade.State) error {
	dir := s.stagingDirFor()
	// 清掉上一次的残留，避免新旧文件混在一起
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := upgrade.ExtractBinaries(tarPath, dir, []string{
		upgrade.PanelBinary, upgrade.HelperBinary,
	}); err != nil {
		return err
	}

	st.Status = upgrade.StatusStaged
	st.Stage = "安装包已校验并暂存"
	st.Message = fmt.Sprintf("已准备好升级到 v%s，可以点击「立即升级」", st.To)
	st.Error = ""
	st.FinishedAt = time.Time{}
	return upgrade.SaveState(s.Cfg.WorkDir, st)
}

// ---------- 手动上传（离线路径）----------

// handleUpgradeUpload 接收管理员上传的发布包。
//
// 这条路径不需要签名：上传者已经通过面板鉴权，而面板管理员本身就等价于 root
// （面板以 root 运行、能改系统配置）。真正的风险是"包损坏/不是这个架构"，
// 那由解包后的试运行来挡。
func (s *Server) handleUpgradeUpload(w http.ResponseWriter, r *http.Request) {
	if !upgradeMu.TryLock() {
		fail(w, http.StatusConflict, "已有一个升级任务在进行中，请等待它结束")
		return
	}
	defer upgradeMu.Unlock()

	st := upgrade.LoadState(s.Cfg.WorkDir)
	if st.Status == upgrade.StatusApplying || st.Status == upgrade.StatusRestarting {
		fail(w, http.StatusConflict, "已有升级正在进行中")
		return
	}

	// 手动上传升级包（离线路径）同样可能传很慢：256MB 在慢速公网上超过 30 秒是常态，
	// 而全局 ReadTimeout=30s（cmd/zizpanel/http.go）会把它直接掐断 —— 表现与
	// 文件管理里传大文件一样（"点了没反应"）。所以这条路由也要解除读超时。
	if err := allowLongUpload(w, r); err != nil && s.Log != nil {
		s.Log.Warn("延长升级包上传读超时失败（超过 30 秒的上传可能被中断）: %v", err)
	}
	// 在接收 body 之前先按声明长度判一次，避免用户白传 256MB 才被拒。
	if r.ContentLength > maxUploadBytes {
		fail(w, http.StatusRequestEntityTooLarge,
			uploadLimitMessage(r.ContentLength, maxUploadBytes, "升级包过大"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if isBodyTooLarge(err) {
			fail(w, http.StatusRequestEntityTooLarge,
				uploadLimitMessage(r.ContentLength, maxUploadBytes, "升级包过大"))
			return
		}
		fail(w, http.StatusBadRequest, "上传内容无法解析："+err.Error())
		return
	}
	file, hdr, err := r.FormFile("package")
	if err != nil {
		fail(w, http.StatusBadRequest, "没有收到升级包文件（字段名应为 package）")
		return
	}
	defer func() { _ = file.Close() }()

	if !strings.HasSuffix(strings.ToLower(hdr.Filename), ".tar.gz") &&
		!strings.HasSuffix(strings.ToLower(hdr.Filename), ".tgz") {
		fail(w, http.StatusBadRequest, "升级包必须是 .tar.gz（发布产物里的 zizpanel_x.y.z_darwin_arm64.tar.gz）")
		return
	}

	upDir := filepath.Join(s.Cfg.WorkDir, "upgrade", "upload")
	if err := os.MkdirAll(upDir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 不用用户提供的文件名拼路径（避免路径穿越）；只用它做展示。
	tarPath := filepath.Join(upDir, "uploaded.tar.gz")
	dst, err := os.Create(tarPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, copyErr := io.Copy(dst, file)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(tarPath)
		fail(w, http.StatusBadRequest, "写入上传文件失败："+copyErr.Error())
		return
	}
	if closeErr != nil {
		_ = os.Remove(tarPath)
		fail(w, http.StatusInternalServerError, closeErr.Error())
		return
	}

	sum, err := upgrade.FileSHA256(tarPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 版本号从包里的二进制读出来 —— 这样用户上传完立刻能确认
	// "我传的确实是哪个版本"，而不是等到升级完才发现传错文件。
	dir := s.stagingDirFor()
	if err := os.RemoveAll(dir); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := upgrade.ExtractBinaries(tarPath, dir, []string{
		upgrade.PanelBinary, upgrade.HelperBinary,
	}); err != nil {
		s.audit(r, "upgrade_upload", hdr.Filename, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, "升级包不可用："+err.Error())
		return
	}
	ver, err := upgrade.ProbeVersion(r.Context(), filepath.Join(dir, upgrade.PanelBinary))
	if err != nil {
		s.audit(r, "upgrade_upload", hdr.Filename, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest,
			"升级包里的主程序无法执行，可能是架构不对或文件损坏："+err.Error())
		return
	}

	newState := &upgrade.State{
		Status:    upgrade.StatusStaged,
		Source:    "upload",
		From:      version.Version,
		To:        ver,
		Stage:     "已接收上传的安装包",
		Message:   fmt.Sprintf("已准备好升级到 v%s，可以点击「立即升级」", ver),
		StartedAt: time.Now(),
	}
	_ = upgrade.SaveState(s.Cfg.WorkDir, newState)

	s.audit(r, "upgrade_upload", hdr.Filename,
		fmt.Sprintf("v%s，%d 字节，sha256=%s", ver, n, sum[:12]), true, "")

	ok(w, map[string]any{
		"staged":  true,
		"version": ver,
		"size":    n,
		"sha256":  sum,
		"state":   newState,
	})
}

// ---------- 执行升级 ----------

// handleUpgradeApply 启动升级。
//
// 立刻返回 202，真正的替换与重启在后台进行 ——
// 因为升级的最后一步会 kickstart 掉我们自己，同步响应根本发不出去。
func (s *Server) handleUpgradeApply(w http.ResponseWriter, r *http.Request) {
	if os.Geteuid() != 0 {
		fail(w, http.StatusForbidden,
			"面板当前不是以 root 运行，无法自我升级。"+
				"这通常出现在本地调试实例上；正式安装的面板由 LaunchDaemon 以 root 运行。")
		return
	}
	upgradeMu.Lock()
	defer upgradeMu.Unlock()

	st := upgrade.LoadState(s.Cfg.WorkDir)
	if st.Status == upgrade.StatusApplying || st.Status == upgrade.StatusRestarting {
		fail(w, http.StatusConflict, "已有升级正在进行中")
		return
	}
	if st.Status != upgrade.StatusStaged {
		fail(w, http.StatusBadRequest, "还没有已准备好的升级包，请先「检查更新」或「上传升级包」")
		return
	}
	staged, stagedVer := s.stagedInfo()
	if !staged {
		fail(w, http.StatusBadRequest, "暂存目录里没有完整的升级包，请重新下载或上传")
		return
	}
	if stagedVer == "" {
		stagedVer = st.To
	}
	if stagedVer == "" {
		fail(w, http.StatusBadRequest, "无法确定升级包版本，拒绝执行")
		return
	}
	if sameVersion(stagedVer, version.Version) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("升级包版本 v%s 与当前版本相同，无需升级", stagedVer))
		return
	}

	opt := s.upgradeOptions()
	stagedFiles := map[string]string{
		upgrade.PanelBinary:  filepath.Join(s.stagingDirFor(), upgrade.PanelBinary),
		upgrade.HelperBinary: filepath.Join(s.stagingDirFor(), upgrade.HelperBinary),
	}
	from, to, source := version.Version, stagedVer, st.Source
	log := s.Log

	s.audit(r, "upgrade_apply", "v"+to,
		fmt.Sprintf("从 v%s 升级，来源 %s", from, source), true, "")

	// 后台执行：先给前端一点时间把 202 收完，再动手替换与重启。
	go func() {
		time.Sleep(700 * time.Millisecond)
		upgradeMu.Lock()
		defer upgradeMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := upgrade.Apply(ctx, opt, stagedFiles, from, to, source); err != nil {
			log.Error("升级失败: %v", err)
			return
		}
		log.Info("升级已提交，看门狗将验证 v%s 并在失败时回滚", to)
	}()

	w.WriteHeader(http.StatusAccepted)
	ok(w, map[string]any{
		"accepted": true,
		"from":     from,
		"to":       to,
		"message":  "升级已开始，面板即将重启。页面会自动重连并显示结果。",
	})
}

func sameVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") ==
		strings.TrimPrefix(strings.TrimSpace(b), "v")
}

// ---------- 清除结果 ----------

// handleUpgradeDismiss 清除已经结束的升级状态（让界面回到干净状态）。
func (s *Server) handleUpgradeDismiss(w http.ResponseWriter, r *http.Request) {
	st := upgrade.LoadState(s.Cfg.WorkDir)
	if st.Status == upgrade.StatusApplying || st.Status == upgrade.StatusRestarting {
		fail(w, http.StatusConflict, "升级正在进行中，不能清除状态")
		return
	}
	// 暂存目录一起清掉：避免用户"清除了提示但包还在"，
	// 过一会儿又被一个陈旧的 staged 状态误导。
	_ = os.RemoveAll(s.stagingDirFor())
	_ = upgrade.SaveState(s.Cfg.WorkDir, &upgrade.State{Status: upgrade.StatusIdle})
	s.audit(r, "upgrade_dismiss", "", "清除升级状态", true, "")
	ok(w, map[string]any{"ok": true})
}
