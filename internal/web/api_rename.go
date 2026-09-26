package web

// api_rename.go —— 批量改名（规则引擎在 internal/rename，前后端共用同一份）。
//
// 两个接口：
//   POST /api/v1/files/rename-plan  —— 只读预览（逐条计划 + 汇总 + 指纹）
//   POST /api/v1/files/rename-apply —— 后端**按同一份规则重算**（不信任前端的新名），
//                                      带指纹核对；有 conflict/invalid 就整批 400、一个都不改。
//
// 目录白名单与文件列表同一套（fileResolveDir → files.Manager.Resolve）：
// 越界 403，名字不许含 `/`、不许为空（rename.Plan 判 invalid）。

import (
	"fmt"
	"net/http"
	"os"

	"github.com/zizdog/zizpanel/internal/rename"
)

// fileRenamePlanReq 是预览与执行共用的请求体。
type fileRenamePlanReq struct {
	Dir         string         `json:"dir"`
	Names       []string       `json:"names"`
	Rules       []rename.Rule  `json:"rules"`
	Options     rename.Options `json:"options"`
	Fingerprint string         `json:"fingerprint"`
}

// readDirNames 读目录里现有的名字（冲突判据的输入）。
func readDirNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

// buildRenamePlan 解析目录 + 读现有名字 + 算计划（预览与执行共用，保证同一套判据）。
func (s *Server) buildRenamePlan(req fileRenamePlanReq) (string, rename.Result, error) {
	if len(req.Names) == 0 {
		return "", rename.Result{}, fmt.Errorf("请先选择要改名的项")
	}
	dir, err := s.fileResolveDir(req.Dir, false)
	if err != nil {
		return "", rename.Result{}, err
	}
	existing, err := readDirNames(dir)
	if err != nil {
		return "", rename.Result{}, err
	}
	return dir, rename.Plan(req.Names, existing, req.Rules, req.Options), nil
}

func (s *Server) handleFileRenamePlan(w http.ResponseWriter, r *http.Request) {
	var req fileRenamePlanReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	dir, plan, err := s.buildRenamePlan(req)
	if err != nil {
		failFileErr(w, err, req.Dir)
		return
	}
	ok(w, map[string]any{
		"dir":         dir,
		"items":       plan.Items,
		"ok":          plan.OK,
		"unchanged":   plan.Unchanged,
		"conflict":    plan.Conflict,
		"invalid":     plan.Invalid,
		"fingerprint": plan.Fingerprint(),
	})
}

func (s *Server) handleFileRenameApply(w http.ResponseWriter, r *http.Request) {
	var req fileRenamePlanReq
	if err := decode(r, &req); err != nil {
		failFileErr(w, err)
		return
	}
	dir, plan, err := s.buildRenamePlan(req)
	if err != nil {
		failFileErr(w, err, req.Dir)
		return
	}
	// 指纹不一致 = 目录内容/规则在这期间变了：拒绝一份过期计划，绝不照旧改。
	if req.Fingerprint != "" && req.Fingerprint != plan.Fingerprint() {
		fail(w, http.StatusConflict, "计划已变化（目录内容或规则变了），请重新预览后再应用")
		return
	}
	if plan.Conflict > 0 || plan.Invalid > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":  false,
			"msg": fmt.Sprintf("整批拒绝：%d 项冲突、%d 项不合法，一个都没改", plan.Conflict, plan.Invalid),
			"data": map[string]any{
				"items":    plan.Items,
				"conflict": plan.Conflict,
				"invalid":  plan.Invalid,
			},
		})
		return
	}
	// 长任务中心不参与：改名是毫秒级操作（与单文件改名一致）。
	res, err := rename.Apply(dir, plan)
	if err != nil {
		failFileErr(w, err, dir)
		return
	}
	s.audit(r, "file_rename_batch", dir, res.Summary, res.Failed == 0, "")
	ok(w, map[string]any{"dir": dir, "result": res})
}
