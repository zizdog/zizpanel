package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zizdog/zizvideo/internal/auth"
	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
)

// ============================================================================
//  条目 8：管理员开关注册（allow_register，默认关）
// ============================================================================

var (
	registerMu       sync.Mutex
	registerSwitches = map[string]*config.RegisterSwitch{}
)

// registerSwitch returns the process-wide live switch for this config file, so
// every request sees the same value the scanner/CLI would.
func (s *Server) registerSwitch() *config.RegisterSwitch {
	key := s.Roots.Path()
	if key == "" {
		key = "\x00nofile:" + s.Cfg.DataDir
	}
	registerMu.Lock()
	defer registerMu.Unlock()
	if sw, ok := registerSwitches[key]; ok {
		return sw
	}
	sw := config.NewRegisterSwitch(s.Roots.Path(), s.Cfg.AllowRegister)
	registerSwitches[key] = sw
	return sw
}

// HandleGetAdminSettings reads the live settings back from config.json; an
// unreadable file makes Verified=false instead of guessing.
func (s *Server) HandleGetAdminSettings(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, s.registerSwitch().State(), nil)
}

type adminSettingsReq struct {
	AllowRegister *bool `json:"allow_register"`
}

// HandlePatchAdminSettings persists allow_register and answers with the value
// read back from disk.
func (s *Server) HandlePatchAdminSettings(w http.ResponseWriter, r *http.Request) {
	var req adminSettingsReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.AllowRegister == nil {
		s.fail(w, r, domain.New("VALIDATION_SETTINGS", "缺少 allow_register", 400))
		return
	}
	if err := s.registerSwitch().Set(*req.AllowRegister); err != nil {
		s.audit(r, "settings.update", "allow_register", false, err.Error())
		s.fail(w, r, domain.New("SETTINGS_WRITE_FAILED", err.Error(), 409))
		return
	}
	s.audit(r, "settings.update", "allow_register", true, fmt.Sprintf("%v", *req.AllowRegister))
	respond(w, http.StatusOK, s.registerSwitch().State(), nil)
}

type registerReq struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

// HandleRegister is the only public self-registration entry. It is closed by
// default; the first-run setup flow never consults this switch.
func (s *Server) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.registerSwitch().On() {
		s.fail(w, r, domain.New("AUTH_REGISTER_DISABLED", "管理员已关闭注册，请联系管理员开通", 403))
		return
	}
	var req registerReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !validUsername(req.Username) {
		s.fail(w, r, domain.New("VALIDATION_USERNAME", "用户名需 3-32 位字母数字或 _-.", 400))
		return
	}
	if len(req.Password) < 8 {
		s.fail(w, r, domain.New("VALIDATION_PASSWORD", "口令至少 8 位", 400))
		return
	}
	if _, err := s.DB.GetUserByUsername(req.Username); err == nil {
		s.fail(w, r, domain.ErrConflict)
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := &domain.User{
		ID: domain.NewID("usr"), Username: req.Username,
		DisplayName: strings.TrimSpace(req.DisplayName), PasswordHash: hash,
		Role: domain.RoleUser, Status: domain.StatusActive,
	}
	if u.DisplayName == "" {
		u.DisplayName = u.Username
	}
	if err := s.DB.CreateUserWithDefaults(u); err != nil {
		s.audit(r, "auth.register", "user:"+req.Username, false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "auth.register", "user:"+u.ID, true, "")
	respond(w, http.StatusCreated, u, nil)
}

// ============================================================================
//  条目 11：媒体去重（判据仅为 size_bytes + duration_ms ⇒ 疑似重复）
// ============================================================================

// duplicateJudgement 必须原样出现在接口里：判据只是"疑似"，不是内容一致。
const duplicateJudgement = "判据：size_bytes 与 duration_ms 相同 ⇒ 疑似重复，不代表内容相同"

// deleteFilesConfirm is the phrase an admin must type to unlock file deletion.
const deleteFilesConfirm = "删除文件"

type duplicateMemberView struct {
	storage.DuplicateMember
	LibraryName string `json:"library_name"`
	FileExists  bool   `json:"file_exists"`
}

// HandleListDuplicates lists suspected-duplicate groups by the stated key only.
func (s *Server) HandleListDuplicates(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100, 1, 500)
	groups, err := s.DB.DuplicateGroups(limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	names := map[string]string{}
	if libs, err := s.DB.ListLibraries(); err == nil {
		for _, l := range libs {
			names[l.ID] = l.Name
		}
	}
	out := make([]map[string]any, 0, len(groups))
	members := 0
	for _, g := range groups {
		views := make([]duplicateMemberView, 0, len(g.Members))
		for _, m := range g.Members {
			_, statErr := os.Stat(m.Path)
			views = append(views, duplicateMemberView{
				DuplicateMember: m, LibraryName: names[m.LibraryID], FileExists: statErr == nil,
			})
		}
		members += len(views)
		out = append(out, map[string]any{
			"size_bytes": g.Size, "duration_ms": g.DurationMS, "members": views,
		})
	}
	respond(w, http.StatusOK, map[string]any{
		"judgement":    duplicateJudgement,
		"group_count":  len(out),
		"member_count": members,
		"groups":       out,
	}, nil)
}

type duplicateIDsReq struct {
	IDs     []string `json:"ids"`
	Confirm string   `json:"confirm"`
}

func cleanIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// HandleDeleteDuplicateRecords deletes panel rows only. It never touches disk.
func (s *Server) HandleDeleteDuplicateRecords(w http.ResponseWriter, r *http.Request) {
	var req duplicateIDsReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ids := cleanIDs(req.IDs)
	if len(ids) == 0 {
		s.fail(w, r, domain.New("VALIDATION_IDS", "请先勾选要删除的记录", 400))
		return
	}
	found, missing, err := s.DB.MediaByIDs(ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(missing) > 0 {
		s.fail(w, r, domain.New("VALIDATION_IDS", "有记录已不存在，请刷新后重试", 400))
		return
	}
	n, err := s.DB.SoftDeleteMedia(ids)
	if err != nil {
		s.audit(r, "dedupe.delete_records", "count:"+fmt.Sprint(len(found)), false, errCode(err))
		s.fail(w, r, err)
		return
	}
	s.audit(r, "dedupe.delete_records", "count:"+fmt.Sprint(n), true, "")
	respond(w, http.StatusOK, map[string]any{
		"deleted": n, "files_untouched": true, "checked": len(found),
		"note": "仅删除面板记录，磁盘文件未动",
	}, nil)
}

// HandleDeleteDuplicateFiles deletes chosen files plus their rows. It requires
// the typed confirmation, refuses while a library is busy, and runs through the
// task centre so progress and failures are visible.
func (s *Server) HandleDeleteDuplicateFiles(w http.ResponseWriter, r *http.Request) {
	var req duplicateIDsReq
	if err := s.decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ids := cleanIDs(req.IDs)
	if len(ids) == 0 {
		s.fail(w, r, domain.New("VALIDATION_IDS", "请先勾选要删除的文件", 400))
		return
	}
	if strings.TrimSpace(req.Confirm) != deleteFilesConfirm {
		s.audit(r, "dedupe.delete_files", "confirm", false, "missing_confirm")
		s.fail(w, r, domain.New("DEDUPE_CONFIRM_REQUIRED",
			"危险操作：请手动输入「删除文件」确认", 400))
		return
	}
	found, missing, err := s.DB.MediaByIDs(ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(missing) > 0 || len(found) == 0 {
		s.fail(w, r, domain.New("VALIDATION_IDS", "有记录已不存在，请刷新后重试", 400))
		return
	}
	for _, m := range found {
		if s.Tasks.Busy(m.LibraryID) {
			s.fail(w, r, domain.New("DEDUPE_LIBRARY_BUSY", "该媒体库正在扫描，请稍后再试", 409))
			return
		}
		if active, aerr := s.DB.ActiveScanTask(m.LibraryID); aerr == nil && active != nil {
			s.fail(w, r, domain.New("DEDUPE_LIBRARY_BUSY", "该媒体库已有任务在进行，请稍后再试", 409))
			return
		}
	}
	t := &domain.ScanTask{ID: domain.NewID("scn"), LibraryID: found[0].LibraryID, Kind: "dedupe_delete"}
	if err := s.DB.CreateScanTask(t); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.DB.UpdateScanTaskProgress(t.ID, len(found), 0, 0, 0); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "dedupe.delete_files", "task:"+t.ID, true, "count:"+fmt.Sprint(len(found)))
	go s.runDeleteDuplicateFiles(t.ID, found)
	respond(w, http.StatusAccepted, map[string]any{
		"task_id": t.ID, "total": len(found),
		"note": "只删除勾选的文件，逐个回读核对；失败会如实报告",
	}, nil)
}

// runDeleteDuplicateFiles removes each file, re-checks the disk, and only then
// soft-deletes the row. A file that is still there is reported as a failure.
func (s *Server) runDeleteDuplicateFiles(taskID string, items []domain.Media) {
	roots := map[string]string{}
	if libs, err := s.DB.ListLibraries(); err == nil {
		for _, l := range libs {
			roots[l.ID] = l.RootPath
		}
	}
	total := len(items)
	processed, deleted, failed := 0, 0, 0
	problems := []string{}
	gone := []string{}
	for _, m := range items {
		processed++
		reason := ""
		canonical, err := media.ValidateMediaFile(s.Roots.List(), roots[m.LibraryID], m.Path)
		switch {
		case err != nil:
			reason = "路径校验未通过: " + pathReason(err)
		default:
			if rerr := os.Remove(canonical); rerr != nil {
				reason = "删除失败: " + rerr.Error()
				break
			}
			if _, serr := os.Stat(canonical); serr == nil {
				reason = "删除后文件仍在，未确认成功"
			} else if !os.IsNotExist(serr) {
				reason = "删除后核对失败: " + serr.Error()
			}
		}
		if reason == "" {
			gone = append(gone, m.ID)
			deleted++
		} else {
			failed++
			if len(problems) < 5 {
				problems = append(problems, filepath.Base(m.Path)+": "+reason)
			}
		}
		if err := s.DB.UpdateScanTaskProgress(taskID, total, processed, deleted, failed); err != nil {
			s.Log.Error("更新去重任务进度失败", "task_id", taskID, "error", err.Error())
		}
	}
	if len(gone) > 0 {
		if _, err := s.DB.SoftDeleteMedia(gone); err != nil {
			failed += len(gone)
			deleted -= len(gone)
			problems = append(problems, "记录删除失败: "+err.Error())
		}
	}
	status, errMsg := domain.TaskSuccess, ""
	if failed > 0 {
		status = domain.TaskFailed
		errMsg = fmt.Sprintf("已删 %d，失败 %d：%s", deleted, failed, strings.Join(problems, "；"))
	}
	if err := s.DB.FinishScanTask(taskID, status, errMsg, 0, 0); err != nil {
		s.Log.Error("结束去重任务失败", "task_id", taskID, "error", err.Error())
	}
}

// pathReason turns a coded path error into one short human sentence.
func pathReason(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return err.Error()
}
