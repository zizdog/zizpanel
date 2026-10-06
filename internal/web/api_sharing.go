package web

// api_sharing.go —— 「文件共享（本机对外提供）」：SMB 共享 + NFS 导出。
//
// 这是「网络磁盘（挂载别人的共享）」的反向能力：把本机目录共享出去给别的设备挂。
// 命令构造与回读在 internal/sharing；这里只做 HTTP、参数校验、审计与视图。
//
// 纪律：
//  1. 写操作前严格校验（共享名 / 路径在白名单根内），越界 403、非法 400。
//  2. 成功只认回读（sharing 包），退出码不算数；读不到状态一律「未复核」。
//  3. 命令一律数组参数、绝不 shell 拼接（sharing 包内统一 exec.Command）。
//  4. 每个写操作都写审计（成功/失败都写）。

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/sharing"
)

// sharingExec 构造执行器。门禁把 s.sharingExportsPath 指到临时文件（绝不动 /etc/exports）；
// 隔离实例用 ZP_SHARING_EXPORTS 指向临时文件（否则以非 root 跑会去碰真 /etc/exports）。
func (s *Server) sharingExec() *sharing.Executor {
	p := strings.TrimSpace(s.sharingExportsPath)
	if p == "" {
		p = strings.TrimSpace(os.Getenv("ZP_SHARING_EXPORTS"))
	}
	if p == "" {
		p = sharing.DefaultExportsPath
	}
	return &sharing.Executor{Timeout: 25 * time.Second, ExportsPath: p, User: config.PanelUser()}
}

// ---------- 参数校验 ----------

// validShareName：只允许中英文、数字、- _ .，≤40 字符，不含 /（也就不用担心路径拼接）。
func validShareName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("共享名不能为空")
	}
	if len([]rune(name)) > 40 {
		return errors.New("共享名最多 40 个字符")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		case unicode.Is(unicode.Han, r):
		default:
			return fmt.Errorf("共享名只能含中英文、数字与 - _ .（非法字符 %q）", string(r))
		}
	}
	return nil
}

// invalidPathReason：路径本身不可能合法时的原因（返回空串 = 形态上没问题）。
func invalidPathReason(p string) string {
	switch {
	case strings.TrimSpace(p) == "":
		return "路径不能为空"
	case !filepath.IsAbs(p):
		return "路径必须是绝对路径"
	case strings.ContainsAny(p, "\n\r\t\x00"):
		return "路径不能含换行/制表/控制字符"
	}
	return ""
}

// checkSharePath 校验一个共享路径：形态合法 + 真的存在 + 是目录 + 在白名单根内。
// 返回 (真实路径, HTTP 状态码, 错误)。状态码 0 = 通过。
func (s *Server) checkSharePath(p string) (string, int, error) {
	if reason := invalidPathReason(p); reason != "" {
		return "", http.StatusBadRequest, errors.New(reason)
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", http.StatusBadRequest, errors.New("路径不存在或读不到：" + p)
	}
	if !st.IsDir() {
		return "", http.StatusBadRequest, errors.New("路径不是目录：" + p)
	}
	resolved, err := s.fileManager().Resolve(p, false)
	if err != nil {
		if errors.Is(err, files.ErrForbidden) {
			return "", http.StatusForbidden, err
		}
		return "", http.StatusBadRequest, err
	}
	return resolved, 0, nil
}

// checkNFSExportPath：NFS 的导出路径**写进文本文件** /etc/exports，
// 所以比 SMB 更严：额外拒绝空白、引号与 shell 元字符（macOS exports 里空白要转义，
// 本面板不重写带引号/空白的行进，如实拒绝比偷偷写坏强）。
func (s *Server) checkNFSExportPath(p string) (string, int, error) {
	resolved, code, err := s.checkSharePath(p)
	if err != nil {
		return "", code, err
	}
	if strings.ContainsAny(resolved, " \t\"'`$;|&<>()\\*?[]{}") {
		return "", http.StatusBadRequest, errors.New("NFS 导出路径不能含空白、引号或 shell 特殊字符")
	}
	return resolved, 0, nil
}

// ---------- 视图 ----------

type sharingShareView struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	ReadOnly bool     `json:"read_only"`
	Managed  bool     `json:"managed"`
	Status   string   `json:"status"` // running / stopped / unknown
	URL      string   `json:"url,omitempty"`
	Line     string   `json:"line,omitempty"`
	Options  []string `json:"options,omitempty"`
}

type sharingServiceView struct {
	Kind  string               `json:"kind"`
	State sharing.ServiceState `json:"state"`
	// Status 是人话三态：running / stopped / unknown（unknown = 未复核）。
	Status      string             `json:"status"`
	Shares      []sharingShareView `json:"shares"`
	SharesKnown bool               `json:"shares_known"`
	SharesError string             `json:"shares_error,omitempty"`
}

// stateLabel 把人话三态算出来（running / stopped / unknown）。
//
// SMB 的"开着"= launchd 里 job 已加载（smbd 是 socket 激活的，没客户端时进程可能不在）。
// NFS 的"开着"= nfsd 真的在跑（nfsd enable 只是持久化，start 才起进程）。
func stateLabel(kind string, st sharing.ServiceState) string {
	if kind == "smb" {
		if st.EnabledKnown {
			if st.Enabled || (st.RunningKnown && st.Running) {
				return "running"
			}
			return "stopped"
		}
		if st.RunningKnown && st.Running {
			return "running"
		}
		return "unknown"
	}
	if st.RunningKnown {
		if st.Running {
			return "running"
		}
		return "stopped"
	}
	return "unknown"
}

// handleSharingStatus GET /api/v1/system/sharing —— 两个服务的真实状态 + 列表 + 地址。
func (s *Server) handleSharingStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	exec := s.sharingExec()
	ip := sharing.LocalIPv4(ctx)

	smbState := exec.SMBStatus(ctx)
	nfsState := exec.NFSStatus(ctx)

	smbShares, smbKnown, smbErr := exec.ListShares(ctx)
	nfsExports, nfsKnown, nfsErr := exec.Exports(ctx)

	smbView := sharingServiceView{Kind: "smb", State: smbState, Status: stateLabel("smb", smbState),
		Shares: []sharingShareView{}, SharesKnown: smbKnown, SharesError: smbErr}
	for _, sh := range smbShares {
		smbView.Shares = append(smbView.Shares, sharingShareView{
			Kind: "smb", Name: sh.Name, Path: sh.Path, ReadOnly: sh.ReadOnly,
			Status: smbView.Status, URL: sharing.URL("smb", ip, sh.Name, sh.ReadOnly),
		})
	}
	nfsView := sharingServiceView{Kind: "nfs", State: nfsState, Status: stateLabel("nfs", nfsState),
		Shares: []sharingShareView{}, SharesKnown: nfsKnown, SharesError: nfsErr}
	for _, ex := range nfsExports {
		nfsView.Shares = append(nfsView.Shares, sharingShareView{
			Kind: "nfs", Name: ex.Path, Path: ex.Path, ReadOnly: ex.ReadOnly, Managed: ex.Managed,
			Status: nfsView.Status, URL: sharing.URL("nfs", ip, ex.Path, ex.ReadOnly),
			Line: ex.Line, Options: ex.Options,
		})
	}

	group := exec.AccessGroup(ctx, config.PanelUser())

	out := map[string]any{
		"smb":          smbView,
		"nfs":          nfsView,
		"access":       group,
		"lan_ip":       ip,
		"is_root":      os.Geteuid() == 0,
		"exports_path": exec.ExportsPath,
		"warnings": []string{
			"开启后局域网内其它设备可访问这些目录；受 macOS 隐私保护的目录可能读不到。随时可在此关闭。",
		},
		"notes": []string{
			"登录 SMB 用 Mac 的用户名与密码（登录这台 Mac 的那个账号），不是面板账号。",
			"地址里的 IP 是面板只读探测到的本机局域网地址；读不到就只显示路径，不猜。",
			"SMB 走 launchctl（enable + bootstrap），NFS 走 nfsd（enable + start）；成功后都会回读确认。",
			sharing.AccessSMBGroup + " 是 Apple 的 SMB 服务 ACL：存在时只有成员能连，不存在时本机任意有密码的用户都能连；面板只读显示，不自动改组。",
			"NFS 导出写在 " + exec.ExportsPath + "，每次改动前先备份到同目录 .zp-bak-<时间戳>。",
		},
	}
	if ip == "" {
		out["lan_ip_error"] = "读不到本机局域网 IP（只看到回环 / VPN / 容器网桥这类别的设备连不上的地址），不编地址。"
	}
	if os.Geteuid() != 0 {
		out["root_error"] = "面板不是以 root 运行：开启/关闭系统共享会失败，请用安装版面板（root LaunchDaemon）。"
	}
	ok(w, out)
}

type sharingShareReq struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
}

func sharingKindOf(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "smb":
		return "smb", true
	case "nfs":
		return "nfs", true
	}
	return "", false
}

// handleSharingShareCreate POST /api/v1/system/sharing/shares
func (s *Server) handleSharingShareCreate(w http.ResponseWriter, r *http.Request) {
	var req sharingShareReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	kind, okKind := sharingKindOf(req.Kind)
	if !okKind {
		fail(w, http.StatusBadRequest, "类型只能是 smb 或 nfs")
		return
	}
	exec := s.sharingExec()

	if kind == "nfs" {
		// NFS 的身份就是导出路径（exports 里没有"名字"字段），name 只是展示用。
		path, code, err := s.checkNFSExportPath(req.Path)
		if err != nil {
			fail(w, code, err.Error())
			return
		}
		res := exec.AddExport(r.Context(), path, req.ReadOnly)
		s.audit(r, "sharing_export_add", path, "新增 NFS 导出 "+path, res.OK, firstLine(res.Error))
		if !res.OK {
			fail(w, http.StatusBadGateway, res.Error)
			return
		}
		ok(w, map[string]any{"kind": "nfs", "name": path, "path": path, "read_only": req.ReadOnly, "result": res})
		return
	}

	name := strings.TrimSpace(req.Name)
	if err := validShareName(name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	path, code, err := s.checkSharePath(req.Path)
	if err != nil {
		fail(w, code, err.Error())
		return
	}
	res := exec.AddShare(r.Context(), path, name, req.ReadOnly)
	s.audit(r, "sharing_share_add", name, "新增 SMB 共享 "+name+" → "+path, res.OK, firstLine(res.Error))
	if !res.OK {
		fail(w, http.StatusBadGateway, res.Error)
		return
	}
	ok(w, map[string]any{"kind": "smb", "name": name, "path": path, "read_only": req.ReadOnly, "result": res})
}

// handleSharingShareDelete DELETE /api/v1/system/sharing/shares —— body {kind,name}
func (s *Server) handleSharingShareDelete(w http.ResponseWriter, r *http.Request) {
	var req sharingShareReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	kind, okKind := sharingKindOf(req.Kind)
	if !okKind {
		fail(w, http.StatusBadRequest, "类型只能是 smb 或 nfs")
		return
	}
	exec := s.sharingExec()
	if kind == "nfs" {
		// 删除时目录可能已经不在了 ⇒ 只校验形态，不要求存在。
		name := strings.TrimSpace(req.Name)
		if reason := invalidPathReason(name); reason != "" {
			fail(w, http.StatusBadRequest, reason)
			return
		}
		res := exec.RemoveExport(r.Context(), name)
		s.audit(r, "sharing_export_remove", name, "删除 NFS 导出 "+name, res.OK, firstLine(res.Error))
		if !res.OK {
			code := http.StatusBadGateway
			if res.Missing {
				code = http.StatusNotFound
			}
			fail(w, code, res.Error)
			return
		}
		ok(w, map[string]any{"kind": "nfs", "name": name, "result": res})
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := validShareName(name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	res := exec.RemoveShare(r.Context(), name)
	s.audit(r, "sharing_share_remove", name, "删除 SMB 共享 "+name, res.OK, firstLine(res.Error))
	if !res.OK {
		code := http.StatusBadGateway
		if res.Missing {
			code = http.StatusNotFound
		}
		fail(w, code, res.Error)
		return
	}
	ok(w, map[string]any{"kind": "smb", "name": name, "result": res})
}

// handleSharingServiceAction POST /api/v1/system/sharing/{smb|nfs}/{enable|disable}
func (s *Server) handleSharingServiceAction(w http.ResponseWriter, r *http.Request) {
	kind, okKind := sharingKindOf(r.PathValue("kind"))
	if !okKind {
		fail(w, http.StatusBadRequest, "类型只能是 smb 或 nfs")
		return
	}
	action := strings.ToLower(strings.TrimSpace(r.PathValue("action")))
	if action != "enable" && action != "disable" {
		fail(w, http.StatusBadRequest, "动作只能是 enable 或 disable")
		return
	}
	exec := s.sharingExec()
	var res sharing.Result
	if kind == "smb" {
		res = exec.SMBAction(r.Context(), action == "enable")
	} else {
		res = exec.NFSAction(r.Context(), action == "enable")
	}
	verb := "开启"
	if action == "disable" {
		verb = "关闭"
	}
	detail := verb + strings.ToUpper(kind) + "共享"
	s.audit(r, "sharing_"+kind+"_"+action, kind, detail, res.OK, firstLine(res.Error))
	if !res.OK {
		fail(w, http.StatusBadGateway, res.Error)
		return
	}
	ok(w, map[string]any{"kind": kind, "action": action, "result": res})
}
