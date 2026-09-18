package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/backup"
	"github.com/zizdog/zizpanel/internal/store"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  备份与恢复（网页面）
//
//  复用既有的「已有备份」卡片（计划任务页）与 `GET /api/v1/backups`，
//  本轮补齐：立即备份、上传恢复、逐行 恢复/下载/删除、恢复前二次确认。
//
//  恢复是长任务，一律走任务中心（202 + task_id + SSE）：
//  同步请求会让用户只能看"请等待"，关掉窗口就找不回进度。
// ============================================================================

const (
	backupManualTarget  = "backup:manual"
	backupRestoreTarget = "backup:restore"
)

// taskLogf 把任务中心的 LogFunc（level,text）包装成 printf 风格，少写一堆
// fmt.Sprintf（恢复流程有几十条进度日志）。
func taskLogf(log tasks.LogFunc) func(level, format string, args ...any) {
	return func(level, format string, args ...any) {
		log(level, fmt.Sprintf(format, args...))
	}
}

func (s *Server) backupDir() string { return filepath.Join(s.Cfg.WorkDir, "backup") }

// backupPath 把请求里的归档名限制在备份目录内（防路径穿越）。
func (s *Server) backupPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("缺少备份文件名")
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("非法的备份文件名: %s", name)
	}
	if !strings.HasSuffix(name, ".tar.gz") {
		return "", fmt.Errorf("备份文件必须是 .tar.gz: %s", name)
	}
	return filepath.Join(s.backupDir(), name), nil
}

// backupTargetOptions 返回界面上的勾选项（含"是否含凭据/是否默认不勾"）。
func backupTargetOptions() []map[string]any {
	labels := map[string]string{
		backup.TargetSites:   "网站文件（~/www，体积可能很大）",
		backup.TargetMySQL:   "MySQL 数据库（逐库导出）",
		backup.TargetNginx:   "nginx / PHP / phpMyAdmin 配置",
		backup.TargetPanel:   "面板数据库 / 配置 / 证书 / ACME 账号",
		backup.TargetCompose: "Docker Compose 项目（含 .env 凭据）",
	}
	out := []map[string]any{}
	for _, t := range backup.AllTargets() {
		label, ok := labels[t]
		if !ok {
			id := strings.TrimPrefix(t, "apps:")
			label = "应用配置：" + id + "（含第三方 token）"
		}
		optIn := strings.HasPrefix(t, "apps:")
		out = append(out, map[string]any{
			"id": t, "label": label, "secrets": true, "opt_in": optIn,
		})
	}
	return out
}

// handleBackupList 列出已有的备份文件（既有接口，本轮补清单摘要）。
func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	dir := s.backupDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			ok(w, map[string]any{"list": []any{}, "dir": dir, "targets": backupTargetOptions()})
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		ModTime string `json:"mod_time"`
		// 以下来自 manifest.json：界面据此显示"来自哪台机器、什么时候、
		// 含不含明文口令"，并在恢复前做二次确认。
		ManifestOK      bool     `json:"manifest_ok"`
		ManifestError   string   `json:"manifest_error,omitempty"`
		Format          string   `json:"format,omitempty"`
		Hostname        string   `json:"hostname,omitempty"`
		CreatedAt       string   `json:"created_at,omitempty"`
		PanelVersion    string   `json:"panel_version,omitempty"`
		Targets         []string `json:"targets,omitempty"`
		ContainsSecrets bool     `json:"contains_secrets"`
		SecretCount     int      `json:"secret_count"`
		FileCount       int      `json:"file_count"`
		IsSnapshot      bool     `json:"is_snapshot"`
	}
	var list []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		it := item{
			Name: e.Name(), Path: p, Size: info.Size(),
			ModTime:    info.ModTime().Format("2006-01-02 15:04:05"),
			IsSnapshot: strings.HasPrefix(e.Name(), "pre-restore-"),
		}
		// 只读清单（不做全包 sha256，列表要快）；坏包也要列出来并标明原因，
		// 否则用户会以为"文件不见了"。
		if m, merr := backup.ReadManifest(p); merr == nil {
			it.ManifestOK = true
			it.Format = m.Format
			it.Hostname = m.Hostname
			it.CreatedAt = m.CreatedAt
			it.PanelVersion = m.PanelVersion
			it.Targets = m.Targets
			it.ContainsSecrets = m.ContainsSecrets
			it.SecretCount = len(m.SecretFiles())
			it.FileCount = len(m.Files)
		} else {
			it.ManifestError = merr.Error()
		}
		list = append(list, it)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ModTime > list[j].ModTime })
	if list == nil {
		list = []item{}
	}
	ok(w, map[string]any{
		"list": list, "dir": dir, "targets": backupTargetOptions(),
		"snapshot_prefix": "pre-restore-",
	})
}

// handleBackupInfo 读取单个归档的清单（恢复前二次确认用）。
func (s *Server) handleBackupInfo(w http.ResponseWriter, r *http.Request) {
	p, err := s.backupPath(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(p); err != nil {
		fail(w, http.StatusNotFound, "备份文件不存在: "+filepath.Base(p))
		return
	}
	m, err := backup.ReadManifest(p)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	compat := backup.CheckCompatibility(m, store.KnownTables())
	ok(w, map[string]any{
		"name":             filepath.Base(p),
		"format":           m.Format,
		"hostname":         m.Hostname,
		"created_at":       m.CreatedAt,
		"panel_version":    m.PanelVersion,
		"source_data_dir":  m.SourceDataDir,
		"targets":          m.Targets,
		"contains_secrets": m.ContainsSecrets,
		"secret_files":     m.SecretFiles(),
		"file_count":       len(m.Files),
		"schema_tables":    m.SchemaTables,
		"warnings":         m.Warnings,
		"compatible":       compat.OK,
		"compat_reason":    compat.Reason,
		"older":            compat.Older,
		"missing_tables":   compat.MissingTables,
		"is_snapshot":      strings.HasPrefix(filepath.Base(p), "pre-restore-"),
	})
}

type backupCreateReq struct {
	Targets  []string `json:"targets"`
	KeepDays int      `json:"keep_days"`
	Out      string   `json:"out"`
}

// handleBackupCreate 立即备份（长任务）。
func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	var req backupCreateReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Targets) == 0 {
		req.Targets = []string{backup.TargetNginx, backup.TargetPanel}
	}
	for _, t := range req.Targets {
		if !backup.IsKnownTarget(t) {
			fail(w, http.StatusBadRequest, "未知的备份范围: "+t)
			return
		}
	}
	targets := backup.NormalizeTargets(req.Targets)
	outDir := s.backupDir()
	if req.Out != "" {
		// 只允许落在备份目录内，避免"立即备份"变成一个任意写文件的接口。
		if clean := filepath.Clean(req.Out); clean != filepath.Clean(outDir) {
			fail(w, http.StatusBadRequest, "输出目录只能是 "+outDir)
			return
		}
	}
	title := "立即备份（" + strings.Join(targets, ",") + "）"
	s.launchTask(w, r, "backup", backupManualTarget, title, "backup_create",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			logf := taskLogf(log)
			logf("info", "备份范围：%s", strings.Join(targets, ","))
			breq := backup.NewRequest(s.Cfg, s.Store, targets, outDir, req.KeepDays)
			res, err := backup.Create(ctx, s.Store, breq)
			if err != nil {
				return nil, err
			}
			logf("info", "已生成：%s", res.Path)
			logf("info", "文件 %d 个，大小 %.1f MB", len(res.Manifest.Files), float64(res.Size)/1024/1024)
			if res.Manifest.ContainsSecrets {
				logf("warn", "归档含明文口令/私钥（权限 0600），请当机密文件保管")
			}
			for _, wrn := range res.Manifest.Warnings {
				logf("warn", "%s", wrn)
			}
			return map[string]any{
				"path": res.Path, "file_name": res.FileName, "size": res.Size,
				"contains_secrets": res.Manifest.ContainsSecrets,
				"file_count":       len(res.Manifest.Files),
			}, nil
		})
}

// handleBackupUpload 上传备份文件（multipart，字段名 file）。
//
// 上传完**立刻整包校验**：坏包当场拒绝并删掉，不留一个"看起来能恢复"的文件。
func (s *Server) handleBackupUpload(w http.ResponseWriter, r *http.Request) {
	// 备份归档可能很大，必须解除全局 30 秒读超时（否则大归档必然被掐断，
	// 而浏览器只看到"网络错误"）。
	if err := allowLongUpload(w, r); err != nil && s.Log != nil {
		s.Log.Warn("延长备份上传读超时失败（超过 30 秒的上传可能被中断）: %v", err)
	}
	// 归档可能含 sites，给到 16 GiB 上限；再大请用文件管理器放进备份目录。
	r.Body = http.MaxBytesReader(w, r.Body, 16<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(w, http.StatusBadRequest, "解析上传内容失败: "+err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "缺少上传文件（表单字段名应为 file）: "+err.Error())
		return
	}
	defer func() { _ = f.Close() }()

	name := filepath.Base(hdr.Filename)
	if !strings.HasSuffix(name, ".tar.gz") {
		fail(w, http.StatusBadRequest, "只接受 .tar.gz 备份归档: "+name)
		return
	}
	if err := os.MkdirAll(s.backupDir(), 0o700); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	dest := filepath.Join(s.backupDir(), name)
	if _, err := os.Stat(dest); err == nil {
		// 不覆盖已有归档：换一个名字，用户自己能看出来是"上传的副本"。
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		dest = filepath.Join(s.backupDir(), fmt.Sprintf("%s-upload-%s%s", base, time.Now().Format("20060102-150405"), ext))
	}
	tmp := dest + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := copyStream(out, f); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, "写入上传文件失败: "+err.Error())
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, err := backup.Verify(dest)
	if err != nil {
		_ = os.Remove(dest) // 坏包不留在备份目录里
		s.audit(r, "backup_upload", name, "校验失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, "上传的归档未通过校验，已删除：\n"+err.Error())
		return
	}
	s.audit(r, "backup_upload", filepath.Base(dest),
		fmt.Sprintf("来源 %s，%d 个文件", m.Hostname, len(m.Files)), true, "")
	ok(w, map[string]any{
		"name":             filepath.Base(dest),
		"hostname":         m.Hostname,
		"created_at":       m.CreatedAt,
		"panel_version":    m.PanelVersion,
		"contains_secrets": m.ContainsSecrets,
		"secret_count":     len(m.SecretFiles()),
		"targets":          m.Targets,
	})
}

// handleBackupDelete 删除一个备份（含恢复快照）。
func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	p, err := s.backupPath(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			fail(w, http.StatusNotFound, "备份文件不存在")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "backup_delete", filepath.Base(p), "删除备份归档", true, "")
	ok(w, map[string]any{"msg": "已删除 " + filepath.Base(p)})
}

// handleBackupDownload 下载一个备份。
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	p, err := s.backupPath(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fail(w, http.StatusNotFound, "备份文件不存在")
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := filepath.Base(p)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
			asciiFallback(name), urlEncode(name)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	http.ServeContent(w, r, name, st.ModTime(), f)
	s.audit(r, "backup_download", name, "下载备份归档", true, "")
}

// restoreRequest 是恢复请求体。
type restoreRequest struct {
	Name string `json:"name"`
	// RestoreConfig 为 true 时连 data/config.json 一起恢复。
	//
	// 默认 false（保持本机身份：面板后缀/监听端口/证书都不动）。
	// 打开它会把后缀/端口/升级源一起换掉，**必须重启面板**，网页会断几秒。
	RestoreConfig bool `json:"restore_config"`
}

// handleBackupRestore 从归档恢复（长任务）。
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.backupPath(req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(p); err != nil {
		fail(w, http.StatusNotFound, "备份文件不存在: "+req.Name)
		return
	}
	title := "恢复备份 " + filepath.Base(p)
	if req.RestoreConfig {
		title += "（含面板配置，将重启面板）"
	}
	target := backupRestoreTarget
	s.launchTask(w, r, "restore", target, title, "backup_restore",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.runRestore(ctx, log, p, req.RestoreConfig)
		})
}

// restoreReport 是恢复任务结束时的**如实回读**。
type restoreReport struct {
	Archive         string   `json:"archive"`
	FromHost        string   `json:"from_host"`
	CreatedAt       string   `json:"created_at"`
	Snapshot        string   `json:"snapshot"`
	DBTables        int      `json:"db_tables"`
	DBRows          int64    `json:"db_rows"`
	SessionsCleared int64    `json:"sessions_cleared"`
	SkippedTables   []string `json:"skipped_tables,omitempty"`
	Sites           int      `json:"sites"`
	Proxies         int      `json:"proxies"`
	Certs           int      `json:"certs"`
	CronJobs        int      `json:"cron_jobs"`
	NginxTest       string   `json:"nginx_test"`
	ForeignKeyCheck string   `json:"foreign_key_check"`
	// Unapplied 是**真正失败**、没能恢复上的条目（会让任务报"部分完成"）。
	Unapplied []string `json:"unapplied"`
	// Skipped 是"按策略或环境明确不做"的条目（例如默认不恢复 config.json、
	// 非特权实例不重建 nginx）。它们是**如实说明**，不算失败，也不谎报成已恢复。
	Skipped  []string `json:"skipped"`
	Warnings []string `json:"warnings"`
	// Partial 为 true 表示数据库/文件已应用，但有部分项没成功。
	Partial bool `json:"partial"`
	// ConfigRestored / RestartPlanned 描述 config.json 与重启状态。
	ConfigRestored bool `json:"config_restored"`
	RestartPlanned bool `json:"restart_planned"`
	// SnapshotUsable 是回滚凭据是否可用（已生成即 true）。
	SnapshotUsable bool `json:"snapshot_usable"`
}

// runRestore 是恢复的完整编排。
//
// 每一步都先记日志再做，失败时明确区分"已回滚 / 部分应用"：
//   - 校验/兼容性/快照阶段失败 → 什么都没改，直接失败；
//   - 数据库替换失败 → 事务回滚，目标库一字未变；
//   - 数据库已成功、后续落盘/nginx 有失败 → 不谎报成功，Partial=true 并逐条列出。
func (s *Server) runRestore(ctx context.Context, log tasks.LogFunc, archive string, restoreConfig bool) (*restoreReport, error) {
	logf := taskLogf(log)
	name := filepath.Base(archive)
	logf("info", "开始恢复：%s", name)

	// ① 整包校验 + 兼容性
	logf("info", "① 校验归档（逐文件 sha256 + 表兼容性）…")
	m, err := backup.Verify(archive)
	if err != nil {
		return nil, err
	}
	compat := backup.CheckCompatibility(m, store.KnownTables())
	if !compat.OK {
		return nil, errors.New(compat.Reason)
	}
	if compat.Older {
		logf("warn", "备份比本程序旧（缺表：%s），恢复后会自动重放迁移补齐",
			strings.Join(compat.MissingTables, ", "))
	}
	rep := &restoreReport{
		Archive: name, FromHost: m.Hostname, CreatedAt: m.CreatedAt,
		SkippedTables: compat.MissingTables,
	}
	if m.ContainsSecrets {
		logf("warn", "归档含 %d 个明文口令/私钥文件（来源 %s）", len(m.SecretFiles()), m.Hostname)
	}

	// ② 恢复前自动快照（回滚唯一凭据）。快照失败 → 中止，不冒险。
	logf("info", "② 生成恢复前快照（回滚凭据）…")
	snapReq := backup.NewRequest(s.Cfg, s.Store, s.snapshotTargets(m.Targets), "", 0)
	snapReq.FileName = backup.SnapshotName(time.Now())
	snapRes, serr := backup.Create(ctx, s.Store, snapReq)
	if serr != nil {
		return nil, fmt.Errorf("恢复前快照失败，已中止（不冒险恢复）: %w", serr)
	}
	rep.Snapshot = snapRes.Path
	rep.SnapshotUsable = true
	logf("info", "   快照：%s", snapRes.Path)

	// ③ 解包（Verify 已在 Extract 里再跑一次，确保解出来的就是校验过的那份）
	tmp, err := os.MkdirTemp("", "zp-restore-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	logf("info", "③ 解包归档…")
	if _, err := backup.Extract(archive, tmp); err != nil {
		return nil, err
	}

	// ④ 数据库：ATTACH + 单事务逐表复制（不替换 panel.db 文件）
	logf("info", "④ 替换数据库（单事务逐表复制，失败整事务回滚）…")
	rrep, err := s.Store.ReplaceFromBackup(ctx, filepath.Join(tmp, "db", "panel.db"))
	if err != nil {
		// 数据库这一步失败 = 事务回滚，只有启动时快照/解包可能已落盘，
		// 但那两者都不改业务状态 → 对用户而言就是"没恢复成"。
		return nil, fmt.Errorf("数据库恢复失败（已回滚，未改动现有数据）: %w", err)
	}
	rep.DBTables = len(rrep.Tables)
	for _, t := range rrep.Tables {
		rep.DBRows += t.Rows
	}
	rep.SessionsCleared = rrep.SessionsCleared
	logf("info", "   已恢复 %d 张表；旧会话已清空 %d 条（需重新登录）", len(rrep.Tables), rrep.SessionsCleared)

	// ⑤ 证书 / ACME / 配置落盘
	logf("info", "⑤ 写回证书、ACME 状态与配置文件…")
	items := backup.Plan(s.planOptions())
	for _, it := range items {
		if it.Apply != backup.ApplyReplace || !backup.Selected(it.Targets, m.Targets) {
			continue
		}
		if it.ArchivePath == "data/config.json" {
			if !restoreConfig {
				rep.Skipped = append(rep.Skipped,
					"data/config.json：按默认策略未恢复（保持本机面板后缀/端口/证书身份）")
			}
			// 勾选了恢复时由第 ⑨ 步统一处理（写盘 + 重启），这里不重复写：
			// config.json 一落盘就要重启才生效，放在一处才不会出现"写了一半"。
			continue
		}
		// 非特权实例（make run-local / 前台调试）只允许写面板自己的数据目录与工作目录。
		//
		// 为什么必须有这道闸：调试实例的 BrewPrefix 仍是真机的 /opt/homebrew
		// （config 只在安装时确定前缀，见 DEVELOPMENT 坑 162），不拦的话
		// "在调试实例上点恢复"会去写真实机器的 PHP 片段/phpMyAdmin 配置 ——
		// 权限挡住时任务会莫名其妙地"部分失败"，权限没挡住时就是真机被改。
		if os.Geteuid() != 0 && !underAny(it.SourcePath, s.Cfg.DataDir, s.Cfg.WorkDir) {
			rep.Skipped = append(rep.Skipped,
				it.ArchivePath+"：当前面板未以 root 运行，且该路径不在面板数据目录内，已跳过")
			continue
		}
		if err := backup.RestoreItem(tmp, it); err != nil {
			rep.Unapplied = append(rep.Unapplied, it.ArchivePath+"："+err.Error())
			continue
		}
	}
	// 归档里带、但**本次恢复不映射到任何真实路径**的内容必须如实列出来，
	// 不能默默丢掉：`sites/www.tar.gz`（网站文件）与 `mysql/*.sql`（数据库导出）
	// 都是用户勾选后才进归档的，恢复时不做自动覆盖 —— 不说清楚，用户会以为
	// "恢复完了怎么网站没变"。
	planned := map[string]bool{"db/panel.db": true}
	for _, it := range items {
		planned[it.ArchivePath] = true
	}
	for _, f := range m.Files {
		if planned[f.Path] {
			continue
		}
		rep.Skipped = append(rep.Skipped,
			f.Path+"：归档里有，但本次恢复不自动覆盖（网站文件/数据库导出请在面板里手工还原）")
	}
	// 恢复出来的证书要能被以真实用户运行的 nginx 读到。
	if os.Geteuid() == 0 && s.Cfg.User != "" {
		for _, d := range []string{
			filepath.Join(s.Cfg.DataDir, "certs"),
			filepath.Join(s.Cfg.DataDir, "site-certs"),
			filepath.Join(s.Cfg.DataDir, "proxy-certs"),
			filepath.Join(s.Cfg.DataDir, "acme"),
			filepath.Join(s.Cfg.DataDir, "tls"),
		} {
			if _, err := os.Stat(d); err == nil {
				_ = chownTreeTo(d, s.Cfg.User)
			}
		}
	}

	// ⑥ nginx：按库/配置**重建**（不整份覆盖 nginx.conf）
	if os.Geteuid() != 0 {
		// 非特权实例（make run-local / 手工前台跑）：绝不触碰真机 nginx 配置，
		// 如实标注跳过。数据库与证书文件已经恢复，用户可在正式面板里重跑。
		rep.Skipped = append(rep.Skipped,
			"nginx 配置重建：当前面板未以 root 运行，已跳过（数据库与证书已恢复）")
		rep.Warnings = append(rep.Warnings,
			"非特权实例不重建 nginx：请以正式面板（root）重新执行一次恢复，或在面板里逐个保存站点")
	} else {
		logf("info", "⑥ 重建 nginx 环境（conf.d / includes）…")
		if _, err := s.callHelper(ctx, "nginx-ensure-env"); err != nil {
			rep.Unapplied = append(rep.Unapplied, "nginx 环境自愈失败："+err.Error())
		}
		logf("info", "⑦ 按数据库重建站点与反向代理配置…")
		if list, err := s.siteMgr().List(ctx); err != nil {
			rep.Unapplied = append(rep.Unapplied, "读取站点列表失败："+err.Error())
		} else {
			for _, site := range list {
				if err := s.applySite(ctx, site); err != nil {
					rep.Unapplied = append(rep.Unapplied, "站点 "+site.Domain+"："+err.Error())
				}
			}
		}
		if rules, err := s.proxyRepo().List(ctx); err != nil {
			rep.Unapplied = append(rep.Unapplied, "读取反向代理规则失败："+err.Error())
		} else {
			for _, rule := range rules {
				if rule.Enabled {
					if err := s.syncForwarder(rule); err != nil {
						rep.Unapplied = append(rep.Unapplied,
							"反向代理 "+rule.Name+" 的回环转发器："+err.Error())
						continue
					}
				}
				if err := s.applyProxy(ctx, rule); err != nil {
					rep.Unapplied = append(rep.Unapplied, "反向代理 "+rule.Name+"："+err.Error())
				}
			}
		}
		if _, err := s.callHelper(ctx, "nginx-test"); err != nil {
			rep.NginxTest = "失败：" + err.Error()
		} else {
			rep.NginxTest = "通过"
		}
		if rep.NginxTest == "通过" {
			if _, err := s.callHelper(ctx, "nginx-reload"); err != nil {
				rep.NginxTest = "校验通过但重载失败：" + err.Error()
			}
		}
		if rep.NginxTest != "通过" {
			// 配置坏了：用恢复前快照把 nginx 侧文件整份还原，再校验一次。
			logf("warn", "nginx 校验未通过，用恢复前快照回滚 nginx 配置…")
			if rerr := s.rollbackNginxFromSnapshot(ctx, snapRes.Path); rerr != nil {
				rep.Warnings = append(rep.Warnings,
					"nginx 回滚也失败，需要人工介入："+rerr.Error())
			} else {
				rep.Warnings = append(rep.Warnings, "nginx 配置已回滚到恢复前状态（数据库仍是备份里的数据）")
				rep.Partial = true
			}
		}
	}

	// ⑧ 计划任务：按恢复后的 cron_jobs 重放 launchd 注册
	logf("info", "⑧ 重新注册计划任务到系统…")
	if os.Geteuid() != 0 {
		rep.Skipped = append(rep.Skipped, "计划任务重放：当前面板未以 root 运行，已跳过")
	} else {
		applied, failed, err := s.cronManager().SyncAll(ctx)
		if err != nil {
			rep.Unapplied = append(rep.Unapplied, "计划任务重放失败："+err.Error())
		}
		for _, f := range failed {
			rep.Unapplied = append(rep.Unapplied, "计划任务 "+f)
		}
		logf("info", "   已注册 %d 个任务，失败 %d 个", applied, len(failed))
	}

	// ⑨ config.json（可选）+ 重启面板（走既有 launchd 通道，不自己 kill 进程）
	if restoreConfig {
		cfgSrc := filepath.Join(tmp, "data", "config.json")
		if _, err := os.Stat(cfgSrc); err != nil {
			rep.Unapplied = append(rep.Unapplied, "data/config.json：归档里没有这份文件，未恢复")
		} else if err := backup.CopyFile(cfgSrc, s.Cfg.Path()); err != nil {
			rep.Unapplied = append(rep.Unapplied, "data/config.json："+err.Error())
		} else {
			rep.ConfigRestored = true
			// 延迟几秒再重启：先让本次任务的响应与日志落库，否则页面只会看到断连。
			s.schedulePanelRestart(log)
			rep.RestartPlanned = true
			logf("warn", "面板配置已恢复：面板后缀/监听端口/升级源已改变，几秒后面板将重启，网页会断开")
		}
	}

	// ⑩ 如实回读
	rep.Sites, rep.Proxies, rep.CronJobs = s.readbackCounts(ctx)
	rep.Certs = countCertDirs(
		filepath.Join(s.Cfg.DataDir, "certs"),
		filepath.Join(s.Cfg.DataDir, "site-certs"),
		filepath.Join(s.Cfg.DataDir, "proxy-certs"),
	)
	if rep.NginxTest == "" {
		rep.NginxTest = "未执行（非特权实例）"
	}
	if err := s.Store.CheckForeignKeys(ctx); err != nil {
		rep.ForeignKeyCheck = "失败：" + err.Error()
	} else {
		rep.ForeignKeyCheck = "通过（无违规）"
	}
	if len(rep.Unapplied) > 0 {
		rep.Partial = true
	}
	logf("info", "回读：站点 %d 个 / 反代 %d 条 / 证书 %d 份 / 计划任务 %d 个；nginx 校验：%s；外键自检：%s",
		rep.Sites, rep.Proxies, rep.Certs, rep.CronJobs, rep.NginxTest, rep.ForeignKeyCheck)
	for _, u := range rep.Skipped {
		logf("info", "按策略跳过：%s", u)
	}
	for _, u := range rep.Unapplied {
		logf("warn", "未恢复：%s", u)
	}
	if rep.Partial {
		return rep, fmt.Errorf("恢复部分完成（数据库与已落盘文件已应用，但有条目未成功）—— "+
			"详见任务日志里的「未恢复」清单；如需回到恢复前状态，可用快照 %s 再恢复一次",
			filepath.Base(rep.Snapshot))
	}
	logf("info", "恢复完成 ✅")
	return rep, nil
}

// snapshotTargets 决定恢复前快照要包含什么：凡是我们会改动的都算。
//
// sites/mysql 内容恢复时并不应用，也不快照（可能几 GB，没必要）。
func (s *Server) snapshotTargets(backupTargets []string) []string {
	out := []string{backup.TargetPanel, backup.TargetNginx}
	for _, t := range backupTargets {
		if strings.HasPrefix(t, "apps:") && backup.IsKnownTarget(t) {
			out = append(out, t)
		}
	}
	return backup.NormalizeTargets(out)
}

// planOptions 用**当前机器**的配置重建"归档路径 → 真实路径"的映射。
func (s *Server) planOptions() backup.PlanOptions {
	return backup.PlanOptions{
		DataDir:    s.Cfg.DataDir,
		WorkDir:    s.Cfg.WorkDir,
		BrewPrefix: s.Cfg.BrewPrefix,
		UserHome:   s.Cfg.UserHome,
	}
}

// rollbackNginxFromSnapshot 直接用快照里的 nginx 文件覆盖回去（回滚唯一通道）。
//
// 为什么这里可以直接写文件：回滚是"把已知能用的旧配置放回去"，
// 写坏的概率远低于"按新库重建"；写完立刻 nginx -t + reload 复核，
// 复核不过就如实报告"回滚也失败"。
func (s *Server) rollbackNginxFromSnapshot(ctx context.Context, snapPath string) error {
	tmp, err := os.MkdirTemp("", "zp-nxrollback-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := backup.Extract(snapPath, tmp); err != nil {
		return err
	}
	items := backup.Plan(s.planOptions())
	restored := 0
	for _, it := range items {
		if it.Apply != backup.ApplyRegenerate {
			continue
		}
		if err := backup.RestoreItem(tmp, it); err != nil {
			return fmt.Errorf("还原 %s 失败: %w", it.ArchivePath, err)
		}
		// RestoreItem 一律写成 0600（归档含私钥），但 nginx 配置文件要能被
		// 以真实用户运行的 nginx 读到 —— 放宽到 0644/0755 并交还属主。
		relaxPerms(it.SourcePath, 0o644, 0o755)
		if os.Geteuid() == 0 && s.Cfg.User != "" {
			_ = chownTreeTo(it.SourcePath, s.Cfg.User)
		}
		restored++
	}
	if restored == 0 {
		return errors.New("快照里没有可还原的 nginx 配置")
	}
	if _, err := s.callHelper(ctx, "nginx-test"); err != nil {
		return err
	}
	_, err = s.callHelper(ctx, "nginx-reload")
	return err
}

// schedulePanelRestart 通过既有 launchd 通道重启面板（绝不自己 kill 进程）。
//
// 为什么延迟执行：任务中心要把这次恢复的结果与日志先落库/推给前端，
// 立刻重启会让页面只看到一次断连，任务状态永远停在"进行中"。
func (s *Server) schedulePanelRestart(log tasks.LogFunc) {
	logf := taskLogf(log)
	label := "cn.zizpanel.panel"
	script := fmt.Sprintf("sleep 3; /bin/launchctl kickstart -k system/%s", label)
	cmd := execCommand(context.Background(), "/bin/sh", "-c", script)
	out, err := cmd.StderrPipe()
	if err != nil {
		logf("error", "重启命令准备失败：%v（配置文件已写入，请手动重启面板）", err)
		return
	}
	if err := cmd.Start(); err != nil {
		logf("error", "启动重启命令失败：%v（配置文件已写入，请手动重启面板）", err)
		return
	}
	go func() {
		b, _ := io.ReadAll(out)
		if werr := cmd.Wait(); werr != nil {
			// 如实记录：配置已落盘但面板没重启 → 新配置还没生效，不能谎报"已生效"。
			msg := fmt.Sprintf("面板重启命令失败（配置已写入但尚未生效，请手动重启面板）: %v %s",
				werr, strings.TrimSpace(string(b)))
			if s.Log != nil {
				s.Log.Warn("%s", msg)
			}
		}
	}()
}

// readbackCounts 回读站点/反代/计划任务数量（读不到就是 0，不编）。
func (s *Server) readbackCounts(ctx context.Context) (sites, proxies, cronJobs int) {
	if list, err := s.siteMgr().List(ctx); err == nil {
		sites = len(list)
	}
	if list, err := s.proxyRepo().List(ctx); err == nil {
		proxies = len(list)
	}
	if list, err := s.cronManager().List(ctx); err == nil {
		cronJobs = len(list)
	}
	return
}

// underAny 判断 path 是否位于 dirs 中任意一个目录之下（含相等）。
//
// 用于"非特权实例只允许写面板自己的目录"这道闸：只做前缀比较时必须在目录
// 边界上截断（/data 不能匹配 /database），否则会把闸开错地方。
func underAny(path string, dirs ...string) bool {
	p := filepath.Clean(path)
	for _, d := range dirs {
		d = filepath.Clean(d)
		if d == "" || d == "." {
			continue
		}
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// countCertDirs 数证书目录（面板证书库 / 站点证书 / 反代证书各一套）。
//
// 为什么不数 certificates 表：那是死表（全仓只有 CREATE、无读写），
// 证书的真相在磁盘（<DataDir>/certs/<primary>/meta.json 等，见
// docs/备份与恢复设计.md 第一节 A）。
func countCertDirs(dirs ...string) int {
	markers := []string{"meta.json", "fullchain.pem", "privkey.pem"}
	n := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			for _, m := range markers {
				if _, err := os.Stat(filepath.Join(dir, e.Name(), m)); err == nil {
					n++
					break
				}
			}
		}
	}
	return n
}

// copyStream 是 io.Copy 的薄封装（便于测试替换/统计）。
func copyStream(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// relaxPerms 把一个文件/目录树的权限放宽到给定模式（目录 0755、文件 0644）。
//
// 只用于 nginx 配置文件：它们必须能被以真实用户运行的 nginx 读到，
// 而归档解出来的文件一律是 0600（因为归档里还有私钥）。
func relaxPerms(path string, fileMode, dirMode os.FileMode) {
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if !info.IsDir() {
		_ = os.Chmod(path, fileMode)
		return
	}
	_ = filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			_ = os.Chmod(p, dirMode)
		} else {
			_ = os.Chmod(p, fileMode)
		}
		return nil
	})
}
