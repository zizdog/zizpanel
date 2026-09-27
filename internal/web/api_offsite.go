package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/offsite"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  异地备份（SMTP / FTP / FTPS）
//
//  与本地备份共用同一份实现：入口在「已有备份」与「计划任务」两个按钮区，
//  打开同一个弹窗；发送走任务中心（202 + task_id + SSE 进度），可中断。
//
//  口令只写进 config.json，**绝不**进日志/审计/任务日志/HTTP 响应：
//  对外只有 password_set 这一个布尔量。
// ============================================================================

// offsiteTarget 是异地发送任务的目标名（同一时刻只允许一个在跑）。
const offsiteTarget = "offsite:send"

func (s *Server) offsiteLedgerPath() string {
	return filepath.Join(s.Cfg.WorkDir, "offsite", "ledger.json")
}

// offsiteSettings 返回当前配置（含口令，仅进程内使用）。
func (s *Server) offsiteSettings() offsite.Settings {
	if s.Cfg.Offsite == nil {
		return offsite.DefaultSettings()
	}
	st := *s.Cfg.Offsite
	st.Normalize()
	return st
}

// offsiteReady 判断"配好了、可以发"。
func offsiteReady(st offsite.Settings) bool { return st.Enabled && st.Validate() == nil }

// offsiteView 是**对外的**配置视图：没有口令，只有"是否已配置"。
type offsiteView struct {
	Enabled       bool   `json:"enabled"`
	Protocol      string `json:"protocol"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Encryption    string `json:"encryption"`
	Username      string `json:"username"`
	PasswordSet   bool   `json:"password_set"`
	From          string `json:"from"`
	To            string `json:"to"`
	SubjectPrefix string `json:"subject_prefix"`
	RemoteDir     string `json:"remote_dir"`
	MaxFileMB     int64  `json:"max_file_mb"`
	Insecure      bool   `json:"insecure_skip_verify"`
	Ready         bool   `json:"ready"`
	Reason        string `json:"reason,omitempty"`
}

func offsiteToView(st offsite.Settings) offsiteView {
	v := offsiteView{
		Enabled: st.Enabled, Protocol: st.Protocol, Host: st.Host, Port: st.Port,
		Encryption: st.Encryption, Username: st.Username, PasswordSet: st.Password != "",
		From: st.From, To: st.To, SubjectPrefix: st.SubjectPrefix, RemoteDir: st.RemoteDir,
		MaxFileMB: st.MaxFileMB, Insecure: st.Insecure,
	}
	if err := st.Validate(); err != nil {
		v.Reason = err.Error()
	} else if st.Enabled {
		// Ready 必须连启用开关一起算：只填了字段但没启用时，
		// 界面仍要如实说"还没配置异地备份"（否则用户以为配好了却没在发）。
		v.Ready = true
	}
	return v
}

// handleOffsiteGet：配置 + 已发送账本 + 待发送数量。
func (s *Server) handleOffsiteGet(w http.ResponseWriter, r *http.Request) {
	st := s.offsiteSettings()
	led, lerr := offsite.LoadLedger(s.offsiteLedgerPath())
	files, ferr := offsite.Scan(s.backupDir())
	data := map[string]any{
		"settings":    offsiteToView(st),
		"ledger":      led.List(),
		"backup_dir":  s.backupDir(),
		"ledger_path": s.offsiteLedgerPath(),
		"files_total": len(files),
		"pending":     len(offsite.Pending(files, led)),
	}
	if lerr != nil {
		data["ledger_error"] = lerr.Error()
	}
	if ferr != nil {
		data["backup_error"] = ferr.Error()
	}
	ok(w, data)
}

// offsiteSaveReq 是保存请求。Password 留空 = 不改（界面只显示"已配置"）。
type offsiteSaveReq struct {
	Enabled       *bool  `json:"enabled"`
	Protocol      string `json:"protocol"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Encryption    string `json:"encryption"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
	From          string `json:"from"`
	To            string `json:"to"`
	SubjectPrefix string `json:"subject_prefix"`
	RemoteDir     string `json:"remote_dir"`
	MaxFileMB     int64  `json:"max_file_mb"`
	Insecure      bool   `json:"insecure_skip_verify"`
}

func (s *Server) handleOffsiteSave(w http.ResponseWriter, r *http.Request) {
	var req offsiteSaveReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	st := s.offsiteSettings()
	if req.Enabled != nil {
		st.Enabled = *req.Enabled
	}
	if req.Protocol != "" {
		st.Protocol = req.Protocol
	}
	st.Host = strings.TrimSpace(req.Host)
	st.Port = req.Port
	st.Encryption = req.Encryption
	st.Username = strings.TrimSpace(req.Username)
	st.From = strings.TrimSpace(req.From)
	st.To = strings.TrimSpace(req.To)
	st.SubjectPrefix = req.SubjectPrefix
	st.RemoteDir = strings.TrimSpace(req.RemoteDir)
	st.MaxFileMB = req.MaxFileMB
	st.Insecure = req.Insecure
	switch {
	case req.ClearPassword:
		st.Password = ""
	case req.Password != "":
		st.Password = req.Password
	}
	st.Normalize()
	// 启用时必须配全，否则会出现"开关是开的、任务每次失败"。
	if st.Enabled {
		if err := st.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.Cfg.Offsite = &st
	if err := s.Cfg.Save(); err != nil {
		// 保存失败要如实说，且不把口令带进错误里（Save 的错误来自文件系统）。
		fail(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	// 审计只记协议与主机，绝不记口令。
	s.audit(r, "offsite_save", st.Host,
		fmt.Sprintf("协议=%s 加密=%s 启用=%v", st.Protocol, st.Encryption, st.Enabled), true, "")
	ok(w, map[string]any{"settings": offsiteToView(st)})
}

// handleOffsiteTest 测试连接（同步；SMTP 发无附件测试邮件 / FTP 写删 1 字节文件）。
//
// 用**已保存的**配置测试：界面点「测试连接」时会先保存，
// 避免"测的是表单、存的是另一份"这种自欺。
func (s *Server) handleOffsiteTest(w http.ResponseWriter, r *http.Request) {
	st := s.offsiteSettings()
	if err := st.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	msg, err := offsite.DefaultSender().Test(ctx, st)
	if err != nil {
		s.audit(r, "offsite_test", st.Host, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	s.audit(r, "offsite_test", st.Host, "测试连接成功", true, "")
	ok(w, map[string]any{"msg": msg})
}

// offsiteSendReq 是发送请求：Name 非空 = 只重发那一个；Force = 忽略账本。
type offsiteSendReq struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

// handleOffsiteSend 立即发送未发送的备份（长任务）。
func (s *Server) handleOffsiteSend(w http.ResponseWriter, r *http.Request) {
	var req offsiteSendReq
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	st := s.offsiteSettings()
	if !st.Enabled {
		fail(w, http.StatusBadRequest, "还没配置异地备份（先在弹窗里填写并启用）")
		return
	}
	if err := st.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name != "" {
		if _, err := s.backupPath(req.Name); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	title := "异地备份：发送未发送的备份"
	if req.Name != "" {
		title = "异地备份：重发 " + filepath.Base(req.Name)
	}
	s.launchTask(w, r, "offsite_backup", offsiteTarget, title, "offsite_send",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.runOffsiteSend(ctx, log, offsite.Options{Force: req.Force, Only: req.Name})
		})
}

// runOffsiteSend 是异地发送的完整编排（手动、自动、重发共用）。
func (s *Server) runOffsiteSend(ctx context.Context, log tasks.LogFunc, opts offsite.Options) (*offsite.Result, error) {
	st := s.offsiteSettings()
	if !st.Enabled {
		return nil, errors.New("异地备份未启用")
	}
	if err := st.Validate(); err != nil {
		return nil, err
	}
	logf := taskLogf(log)
	logf("info", "协议 %s，目标 %s:%d%s", st.Protocol, st.Host, st.Port,
		dirHint(st.RemoteDir))
	logf("info", "单文件上限 %d MB：超过上限的文件不会发送（建议改用 FTP）", st.MaxFileMB)

	progress := func(done, total int64, filesDone, filesTotal int, msg string) {
		tasks.ReportProgress(ctx, tasks.Progress{
			Phase: "offsite", Done: done, Total: total,
			FilesDone: filesDone, FilesTotal: filesTotal, Message: msg,
		})
	}
	note := func(level, text string) { logf(level, "%s", text) }

	res, err := offsite.DefaultSender().Send(ctx, st, s.backupDir(), s.offsiteLedgerPath(), opts, progress, note)
	if err != nil {
		return res, err
	}
	if res.NoNew {
		logf("info", "没有新增备份（账本里都已成功发送过）")
		return res, nil
	}
	if len(res.Failed) > 0 {
		// 本地备份不受影响：这里只对"异地发送"这件事如实报失败。
		return res, fmt.Errorf("有 %d 个文件发送失败（本地备份不受影响；原因见任务日志）", len(res.Failed))
	}
	return res, nil
}

func dirHint(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if !strings.HasPrefix(remote, "/") {
		remote = "/" + remote
	}
	return remote
}

// maybeAutoOffsite 在本地备份成功之后追加一个异地发送任务（配置启用时）。
//
// 为什么在这里触发而不是改 launchd 脚本：备份任务本身就是面板里的长任务，
// 它结束的那一刻是"新增归档已经落盘"最可靠的判据。
func (s *Server) maybeAutoOffsite(info auditInfo, trigger string) {
	st := s.offsiteSettings()
	if !offsiteReady(st) {
		return
	}
	if t := s.Tasks.RunningFor(offsiteTarget); t != nil {
		return // 已有一个在跑：不排队、不重复发
	}
	s.Tasks.StartWithTask("offsite_backup", offsiteTarget, "异地备份（"+trigger+"后自动发送）",
		func(ctx context.Context, t *tasks.Task) (any, error) {
			ctx = tasks.WithProgress(ctx, t.SetProgress)
			res, err := s.runOffsiteSend(ctx, t.LogFunc(), offsite.Options{})
			if err != nil {
				s.auditAs(info, "offsite_send", offsiteTarget, "失败: "+err.Error(), false, "")
				return res, err
			}
			s.auditAs(info, "offsite_send", offsiteTarget, summarizeOffsite(res), true, "")
			return res, nil
		})
}

// summarizeOffsite 把发送结果压成一行审计描述（不含任何凭据）。
func summarizeOffsite(res *offsite.Result) string {
	if res == nil {
		return "完成"
	}
	if res.NoNew {
		return "没有新增备份"
	}
	return fmt.Sprintf("已发送 %d 个（%.1f MB），失败 %d 个，跳过 %d 个",
		len(res.Sent), float64(res.SentBytes)/1024/1024, len(res.Failed), len(res.Skipped))
}
