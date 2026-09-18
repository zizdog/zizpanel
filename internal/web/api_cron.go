package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/zizdog/zizpanel/internal/scheduler"
)

// ============================================================================
//  计划任务
//
//  与文件管理器/终端不同，这个模块没有"路径白名单"问题，
//  但有两件事必须做好：
//    1. 任务内容会作为 root 执行 —— 因此只能由已登录的管理员创建，
//       且所有创建/修改/删除都写审计
//    2. 任务状态以 launchd 为准（数据库只存"计划是什么"），
//       否则会出现"界面显示启用了但实际没在跑"
// ============================================================================

func (s *Server) cronManager() *scheduler.Manager {
	return scheduler.NewManager(s.Store, scheduler.Options{
		LogDir: filepath.Join(s.Cfg.LogDir, "cron"),
		// 备份输出目录与"用哪个二进制/哪份配置执行备份"都从面板配置取：
		// 脚本里写死 /opt/zizpanel 会在 ZIZPANEL_ROOT 重定位或 Intel 前缀下备错东西。
		BackupDir:  filepath.Join(s.Cfg.WorkDir, "backup"),
		BinaryPath: s.Cfg.ServicePath("zizpanel"),
		ConfigPath: s.Cfg.Path(),
	})
}

func (s *Server) handleCronList(w http.ResponseWriter, r *http.Request) {
	mgr := s.cronManager()
	if err := mgr.EnsureLogDir(); err != nil {
		s.Log.Warn("创建任务日志目录失败: %v", err)
	}
	list, err := mgr.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取计划任务失败: "+err.Error())
		return
	}
	ok(w, map[string]any{
		"list":       list,
		"presets":    scheduler.CommonSchedules(),
		"log_dir":    filepath.Join(s.Cfg.LogDir, "cron"),
		"backup_dir": filepath.Join(s.Cfg.WorkDir, "backup"),
	})
}

func (s *Server) handleCronGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.cronManager()
	j, err := mgr.Get(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	ok(w, j)
}

type cronReq struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Schedule      string   `json:"schedule"`
	Command       string   `json:"command"`
	WorkDir       string   `json:"work_dir"`
	Enabled       *bool    `json:"enabled"`
	BackupTargets []string `json:"backup_targets"`
	BackupDir     string   `json:"backup_dir"`
	KeepDays      int      `json:"keep_days"`
}

func (s *Server) handleCronCreate(w http.ResponseWriter, r *http.Request) {
	var req cronReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	j := &scheduler.Job{
		Name: req.Name, Kind: orDefault(req.Kind, "shell"), Schedule: req.Schedule,
		Command: req.Command, WorkDir: req.WorkDir,
		BackupTargets: req.BackupTargets, BackupDir: req.BackupDir, KeepDays: req.KeepDays,
		Enabled: true,
	}
	if req.Enabled != nil {
		j.Enabled = *req.Enabled
	}
	mgr := s.cronManager()
	if err := mgr.EnsureLogDir(); err != nil {
		s.Log.Warn("创建任务日志目录失败: %v", err)
	}
	if err := mgr.Create(r.Context(), j); err != nil {
		s.audit(r, "cron_create", j.Name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "cron_create", j.Name,
		fmt.Sprintf("类型=%s 计划=%s", j.Kind, j.Schedule), true, "")
	ok(w, j)
}

func (s *Server) handleCronUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.cronManager()
	j, err := mgr.Get(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var req cronReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name != "" {
		j.Name = req.Name
	}
	if req.Kind != "" {
		j.Kind = req.Kind
	}
	if req.Schedule != "" {
		j.Schedule = req.Schedule
	}
	j.Command = req.Command
	j.WorkDir = req.WorkDir
	if req.Enabled != nil {
		j.Enabled = *req.Enabled
	}
	if len(req.BackupTargets) > 0 {
		j.BackupTargets = req.BackupTargets
	}
	if req.BackupDir != "" {
		j.BackupDir = req.BackupDir
	}
	if req.KeepDays > 0 {
		j.KeepDays = req.KeepDays
	}

	if err := mgr.Update(r.Context(), j); err != nil {
		s.audit(r, "cron_update", j.Name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "cron_update", j.Name, "更新计划任务", true, "")
	ok(w, j)
}

func (s *Server) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.cronManager()
	j, _ := mgr.Get(r.Context(), id)
	if err := mgr.Delete(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := strconv.FormatInt(id, 10)
	if j != nil {
		name = j.Name
	}
	s.audit(r, "cron_delete", name, "删除计划任务", true, "")
	ok(w, map[string]any{"msg": "任务已删除"})
}

// handleCronToggle 启用/停用任务。
func (s *Server) handleCronToggle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.cronManager()
	if err := mgr.EnsureLogDir(); err != nil {
		s.Log.Warn("创建任务日志目录失败: %v", err)
	}
	if err := mgr.Toggle(r.Context(), id, req.Enabled); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	word := "停用"
	if req.Enabled {
		word = "启用"
	}
	s.audit(r, "cron_toggle", strconv.FormatInt(id, 10), word+"计划任务", true, "")
	ok(w, map[string]any{"msg": "已" + word})
}

// handleCronRun 立即执行一次。
func (s *Server) handleCronRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	mgr := s.cronManager()
	if err := mgr.EnsureLogDir(); err != nil {
		s.Log.Warn("创建任务日志目录失败: %v", err)
	}
	msg, err := mgr.RunNow(r.Context(), id)
	if err != nil {
		s.audit(r, "cron_run", strconv.FormatInt(id, 10), "失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "cron_run", strconv.FormatInt(id, 10), "手动触发执行", true, "")
	ok(w, map[string]any{"msg": msg})
}

// handleCronLog 读取任务运行日志。
func (s *Server) handleCronLog(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}
	mgr := s.cronManager()
	path, err := mgr.LogPath(id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	content, total, err := tailFile(path, lines)
	if err != nil {
		if os.IsNotExist(err) {
			ok(w, map[string]any{
				"content": "", "path": path, "lines": 0,
				"msg": "该任务还没有运行记录（首次执行后才会产生日志）",
			})
			return
		}
		fail(w, http.StatusInternalServerError, "读取日志失败: "+err.Error())
		return
	}
	ok(w, map[string]any{"content": content, "path": path, "lines": total})
}

// handleCronSync 把数据库里的任务全部重新注册到 launchd（修复不同步）。
func (s *Server) handleCronSync(w http.ResponseWriter, r *http.Request) {
	mgr := s.cronManager()
	if err := mgr.EnsureLogDir(); err != nil {
		s.Log.Warn("创建任务日志目录失败: %v", err)
	}
	applied, failed, err := mgr.SyncAll(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "cron_sync", "all",
		fmt.Sprintf("重新注册 %d 个任务，失败 %d 个", applied, len(failed)), len(failed) == 0, "")
	ok(w, map[string]any{"applied": applied, "failed": failed})
}

// handleCronPreview 预览 cron 描述与接下来的执行时间（表单实时反馈）。
func (s *Server) handleCronPreview(w http.ResponseWriter, r *http.Request) {
	expr := r.URL.Query().Get("schedule")
	if expr == "" {
		fail(w, http.StatusBadRequest, "缺少 schedule 参数")
		return
	}
	c, err := scheduler.ParseCron(expr)
	if err != nil {
		ok(w, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	runs := scheduler.NextRuns(c, time.Now(), 5)
	var times []string
	for _, t := range runs {
		times = append(times, t.Format("2006-01-02 15:04"))
	}
	ok(w, map[string]any{
		"valid": true, "describe": c.Describe(), "next_runs": times,
	})
}

// handleBackupList 已移到 api_backup.go（本轮补齐"恢复/上传/删除/下载"后，
// 备份相关接口集中在一个文件里）。

// pathID 解析路径里的数字 ID。
func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("非法的任务 ID: %s", raw)
	}
	return id, nil
}

// 保留：便于将来支持"任务执行历史"表
var _ = context.Background
