package web

// api_smb.go —— 「网络磁盘（SMB）」：把 NAS 的共享挂到 <安装根>/mnt/<名字>，
// 给面板应用（尤其 Jellyfin）当媒体库用。
//
// 为什么面板来挂：面板是 root LaunchDaemon，且已经拿到「完全磁盘访问」授权；
// Jellyfin 以面板子进程（jellyfin-supervise）的身份运行 ⇒ **继承面板的 TCC 授权**
// ⇒ 能直接读网络卷（网络卷同样受 TCC 保护）。这正是这条路能走通的原因。
//
// 纪律（与磁盘页同源）：
//   1. 口令只在服务端存储（settings KV）里；HTTP 只回 password_set，日志/审计/响应
//      全走 smb.Scrub（见 internal/smb/mount.go 的口令通道说明）。
//   2. 挂载成功与否**只看回读**（挂载表 + 目录真读一次），绝不信退出码。
//   3. NAS 离线**不阻塞面板启动**：启动后在后台按退避重试，状态永远是真实的。
//
// 持久化选 settings KV 而不是 config.json：与 Jellyfin 媒体目录/收藏同一处；
// 面板库里本就有 MySQL 明文口令（db_credentials 表），不新增暴露面。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/smb"
)

// smbMountsKey 是 settings 表里的键（值 = []smb.Mount 的 JSON，含口令）。
const smbMountsKey = "smb_mounts"

// smbRetryBase / smbRetryCap 是自动重挂的退避：30s → 1m → 2m → … 上限 30m。
const (
	smbRetryBase = 30 * time.Second
	smbRetryCap  = 30 * time.Minute
	smbPassEvery = 30 * time.Second
)

// smbRuntime 是进程级运行状态（不落库：重启后本来就该重新挂）。
type smbRuntime struct {
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
	NextAttempt time.Time
	Fails       int
	// Suppressed：用户**手动**卸载过 ⇒ 这一轮不再自动重挂（否则用户点完卸载，
	// 30 秒后它自己又挂回来，像是点不动）。用户再点挂载/重新挂载即解除。
	Suppressed bool
	Busy       bool
}

// smbExecFor 返回执行器。单测用路径垫片替换命令，不需要注入执行器本身。
func (s *Server) smbExecFor() *smb.Executor { return &smb.Executor{Timeout: 90 * time.Second} }

// ---------- 持久化 ----------

func (s *Server) loadSMBMounts(ctx context.Context) ([]smb.Mount, error) {
	raw, err := s.Store.GetSetting(ctx, smbMountsKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var list []smb.Mount
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, errors.New("网络磁盘数据损坏（" + smbMountsKey + "）：" + err.Error())
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (s *Server) saveSMBMounts(ctx context.Context, list []smb.Mount) error {
	if len(list) == 0 {
		return s.Store.SetSetting(ctx, smbMountsKey, "")
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, smbMountsKey, string(b))
}

func smbFind(list []smb.Mount, id string) (smb.Mount, bool) {
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return smb.Mount{}, false
}

// ---------- 运行状态 ----------

// smbBegin 抢占一条记录的"正在挂载"标记；已被占用返回 false。
func (s *Server) smbBegin(id string) bool {
	s.smbMu.Lock()
	defer s.smbMu.Unlock()
	if s.smbRun == nil {
		s.smbRun = map[string]*smbRuntime{}
	}
	r := s.smbRun[id]
	if r == nil {
		r = &smbRuntime{}
		s.smbRun[id] = r
	}
	if r.Busy {
		return false
	}
	r.Busy = true
	return true
}

func (s *Server) smbEnd(id string) {
	s.smbMu.Lock()
	if r := s.smbRun[id]; r != nil {
		r.Busy = false
	}
	s.smbMu.Unlock()
}

// smbRecord 记一次真实尝试的结果（挂载/卸载都走它）。
func (s *Server) smbRecord(id string, res smb.Result) {
	s.smbMu.Lock()
	defer s.smbMu.Unlock()
	if s.smbRun == nil {
		s.smbRun = map[string]*smbRuntime{}
	}
	r := s.smbRun[id]
	if r == nil {
		r = &smbRuntime{}
		s.smbRun[id] = r
	}
	r.LastAttempt = time.Now()
	if res.Action == "mount" {
		if res.Mounted && res.Error == "" {
			r.LastSuccess, r.LastError, r.Fails, r.NextAttempt, r.Suppressed = time.Now(), "", 0, time.Time{}, false
		} else {
			r.LastError = smbResultMsg(res)
			r.Fails++
			r.NextAttempt = time.Now().Add(smbBackoff(r.Fails))
		}
		return
	}
	if res.Mounted {
		r.LastError = smbResultMsg(res)
	}
}

func smbBackoff(fails int) time.Duration {
	d := smbRetryBase
	for i := 1; i < fails && d < smbRetryCap; i++ {
		d *= 2
	}
	if d > smbRetryCap {
		d = smbRetryCap
	}
	return d
}

// ---------- 视图 ----------

type smbView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Share       string `json:"share"`
	User        string `json:"user"`
	Domain      string `json:"domain,omitempty"`
	ReadOnly    bool   `json:"read_only"`
	PasswordSet bool   `json:"password_set"`
	MountPoint  string `json:"mount_point"`
	Mounted     bool   `json:"mounted"`
	// Verified 表示"状态是回读真实挂载表得到的"（读不到挂载表 ⇒ false，前端要如实标注）。
	Verified    bool     `json:"verified"`
	Source      string   `json:"source,omitempty"`
	FSType      string   `json:"fs_type,omitempty"`
	Entries     []string `json:"entries,omitempty"`
	Busy        bool     `json:"busy"`
	LastError   string   `json:"last_error,omitempty"`
	LastAttempt string   `json:"last_attempt_at,omitempty"`
	LastSuccess string   `json:"last_success_at,omitempty"`
	NextAttempt string   `json:"next_attempt_at,omitempty"`
	Command     string   `json:"command,omitempty"`
}

func (s *Server) smbViewOf(m smb.Mount, table []smb.Entry, tableErr error) smbView {
	base := s.customMountBase()
	mp := smb.MountPoint(base, m.Name)
	v := smbView{
		ID: m.ID, Name: m.Name, Host: m.Host, Share: m.Share, User: m.User,
		Domain: m.Domain, ReadOnly: m.ReadOnly, PasswordSet: m.Password != "",
		MountPoint: mp,
	}
	if tableErr == nil {
		v.Verified = true
		if e, found := smb.EntryAt(table, mp); found {
			v.Mounted, v.Source, v.FSType = true, e.Source, e.FSType
		}
	}
	s.smbMu.Lock()
	st := s.smbRun[m.ID]
	if st == nil {
		st = &smbRuntime{}
	}
	v.Busy, v.LastError = st.Busy, st.LastError
	if !st.LastAttempt.IsZero() {
		v.LastAttempt = st.LastAttempt.Format(time.RFC3339)
	}
	if !st.LastSuccess.IsZero() {
		v.LastSuccess = st.LastSuccess.Format(time.RFC3339)
	}
	if !st.NextAttempt.IsZero() && !v.Mounted {
		v.NextAttempt = st.NextAttempt.Format(time.RFC3339)
	}
	s.smbMu.Unlock()
	v.Command = strings.Join(smb.MountArgs(m, mp), " ")
	return v
}

// smbResultMsg 把执行结论拼成一句能直接给用户看的话（口令已 scrub）。
func smbResultMsg(res smb.Result) string {
	parts := []string{}
	if res.Error != "" {
		parts = append(parts, res.Error)
	}
	if res.Remedy != "" {
		parts = append(parts, "出路："+res.Remedy)
	}
	if res.Note != "" {
		parts = append(parts, res.Note)
	}
	if res.Output != "" && res.Error != "" {
		parts = append(parts, "原样输出："+smbTail(res.Output, 300))
	}
	return strings.Join(parts, "\n")
}

func smbTail(s string, n int) string {
	s = smb.Clean(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ---------- HTTP ----------

// handleSMBList GET /api/v1/system/smb —— 配置 + **真实**挂载状态。
func (s *Server) handleSMBList(w http.ResponseWriter, r *http.Request) {
	list, err := s.loadSMBMounts(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 没有配置就不去 fork `mount`（新装面板打开磁盘页不该多一个子进程）。
	var table []smb.Entry
	var terr error
	if len(list) > 0 {
		table, terr = smb.MountTable(r.Context())
	}
	views := make([]smbView, 0, len(list))
	for _, m := range list {
		views = append(views, s.smbViewOf(m, table, terr))
	}
	out := map[string]any{
		"mounts":     views,
		"mount_base": s.customMountBase(),
		"is_root":    os.Geteuid() == 0,
		"notes": []string{
			"网络盘挂在 " + s.customMountBase() + " 下（默认只读优先；Jellyfin 直接选这个目录即可）。",
			"口令只存在面板数据库里，接口只回 password_set，日志与响应里都不会出现它。",
		},
	}
	if terr != nil {
		out["table_error"] = terr.Error()
	}
	ok(w, out)
}

type smbSaveReq struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Share    string `json:"share"`
	User     string `json:"user"`
	Domain   string `json:"domain"`
	Password string `json:"password"`
	ReadOnly bool   `json:"read_only"`
}

// handleSMBCreate POST /api/v1/system/smb —— 新增一条（不自动挂载）。
func (s *Server) handleSMBCreate(w http.ResponseWriter, r *http.Request) {
	var req smbSaveReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	m := smb.Mount{
		Name: req.Name, Host: req.Host, Share: req.Share, User: req.User,
		Domain: req.Domain, Password: req.Password, ReadOnly: req.ReadOnly,
	}
	if err := smb.Normalize(&m); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	list, err := s.loadSMBMounts(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, e := range list {
		if strings.EqualFold(e.Name, m.Name) {
			fail(w, http.StatusConflict, "已经有叫「"+m.Name+"」的网络盘了（挂载点会撞车）")
			return
		}
	}
	list = append(list, m)
	if err := s.saveSMBMounts(r.Context(), list); err != nil {
		fail(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.audit(r, "smb_create", m.ID, "新增网络盘 "+m.Name+"（"+m.User+"@"+m.Host+"/"+m.Share+"）", true, "")
	table, terr := smb.MountTable(r.Context())
	ok(w, s.smbViewOf(m, table, terr))
}

// handleSMBUpdate PUT /api/v1/system/smb/{id} —— 改配置；password 空串 = 保持原口令。
func (s *Server) handleSMBUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req smbSaveReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	list, err := s.loadSMBMounts(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	old, found := smbFind(list, id)
	if !found {
		fail(w, http.StatusNotFound, "找不到这条网络盘（"+id+"）")
		return
	}
	m := old
	m.Name = req.Name
	m.Host, m.Share, m.User, m.Domain = req.Host, req.Share, req.User, req.Domain
	m.ReadOnly = req.ReadOnly
	if req.Password != "" {
		m.Password = req.Password
	}
	if strings.TrimSpace(m.Name) == "" {
		m.Name = old.Name
	}
	if err := smb.Normalize(&m); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !strings.EqualFold(m.Name, old.Name) {
		fail(w, http.StatusBadRequest, "名字就是挂载点目录名，改名请删掉重建（免得两条配置指向同一个目录）")
		return
	}
	for i := range list {
		if list[i].ID == id {
			list[i] = m
		}
	}
	if err := s.saveSMBMounts(r.Context(), list); err != nil {
		fail(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.audit(r, "smb_update", id, "修改网络盘 "+m.Name+"（"+m.User+"@"+m.Host+"/"+m.Share+"）", true, "")
	table, terr := smb.MountTable(r.Context())
	ok(w, s.smbViewOf(m, table, terr))
}

// handleSMBDelete DELETE /api/v1/system/smb/{id} —— 先卸载（忙就拒绝），再删配置。
func (s *Server) handleSMBDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	list, err := s.loadSMBMounts(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, found := smbFind(list, id)
	if !found {
		fail(w, http.StatusNotFound, "找不到这条网络盘（"+id+"）")
		return
	}
	mp := smb.MountPoint(s.customMountBase(), m.Name)
	if table, terr := smb.MountTable(r.Context()); terr == nil {
		if _, mounted := smb.EntryAt(table, mp); mounted {
			res := s.smbExecFor().Unmount(r.Context(), mp)
			if !res.Verified || res.Mounted {
				msg := "还在使用中，没有删除。"
				if res.Error != "" {
					msg = res.Error
				}
				s.audit(r, "smb_delete", id, msg, false, "")
				fail(w, http.StatusConflict, msg+"（先停掉在用它的程序，或先手动卸载）")
				return
			}
		}
	}
	out := make([]smb.Mount, 0, len(list))
	for _, e := range list {
		if e.ID != id {
			out = append(out, e)
		}
	}
	if err := s.saveSMBMounts(r.Context(), out); err != nil {
		fail(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.smbMu.Lock()
	delete(s.smbRun, id)
	s.smbMu.Unlock()
	s.audit(r, "smb_delete", id, "删除网络盘 "+m.Name, true, "")
	ok(w, map[string]any{"deleted": id})
}

// mountOne 是挂载的唯一入口（HTTP 与自动重挂共用）。
func (s *Server) mountOne(ctx context.Context, m smb.Mount) smb.Result {
	mp := smb.MountPoint(s.customMountBase(), m.Name)
	res := s.smbExecFor().Mount(ctx, m, mp)
	s.smbRecord(m.ID, res)
	return res
}

func (s *Server) handleSMBMount(w http.ResponseWriter, r *http.Request) {
	s.smbMountAction(w, r, false)
}
func (s *Server) handleSMBRemount(w http.ResponseWriter, r *http.Request) {
	s.smbMountAction(w, r, true)
}

func (s *Server) smbMountAction(w http.ResponseWriter, r *http.Request, remount bool) {
	id := r.PathValue("id")
	list, err := s.loadSMBMounts(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, found := smbFind(list, id)
	if !found {
		fail(w, http.StatusNotFound, "找不到这条网络盘（"+id+"）")
		return
	}
	if !s.smbBegin(id) {
		fail(w, http.StatusConflict, "这条网络盘正在处理中（后台自动重挂或上一次操作还没结束），稍等几秒再看状态")
		return
	}
	defer s.smbEnd(id)

	mp := smb.MountPoint(s.customMountBase(), m.Name)
	if remount {
		if table, terr := smb.MountTable(r.Context()); terr == nil {
			if _, mounted := smb.EntryAt(table, mp); mounted {
				if u := s.smbExecFor().Unmount(r.Context(), mp); u.Mounted {
					s.smbRecord(id, u)
					s.audit(r, "smb_remount", id, smbResultMsg(u), false, "")
					fail(w, http.StatusConflict, smbResultMsg(u))
					return
				}
			}
		}
	}
	// 明确的手动挂载：解除"用户卸载过"的抑制。
	s.smbMu.Lock()
	if st := s.smbRun[id]; st != nil {
		st.Suppressed = false
	}
	s.smbMu.Unlock()

	res := s.mountOne(r.Context(), m)
	table, terr := smb.MountTable(r.Context())
	v := s.smbViewOf(m, table, terr)
	v.Entries = res.Entries
	if res.Error != "" || !res.Mounted {
		msg := smbResultMsg(res)
		if msg == "" {
			msg = "挂载失败（回读显示未挂载）"
		}
		s.audit(r, "smb_mount", id, firstLine(msg), false, "")
		fail(w, http.StatusBadGateway, msg)
		return
	}
	s.audit(r, "smb_mount", id, "已挂载 "+m.Name+" → "+mp, true, "")
	ok(w, v)
}

// handleSMBUnmount POST /api/v1/system/smb/{id}/unmount
func (s *Server) handleSMBUnmount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	list, err := s.loadSMBMounts(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, found := smbFind(list, id)
	if !found {
		fail(w, http.StatusNotFound, "找不到这条网络盘（"+id+"）")
		return
	}
	if !s.smbBegin(id) {
		fail(w, http.StatusConflict, "这条网络盘正在处理中（后台自动重挂或上一次操作还没结束），稍等几秒再看状态")
		return
	}
	defer s.smbEnd(id)

	mp := smb.MountPoint(s.customMountBase(), m.Name)
	res := s.smbExecFor().Unmount(r.Context(), mp)
	s.smbRecord(id, res)
	table, terr := smb.MountTable(r.Context())
	v := s.smbViewOf(m, table, terr)
	if res.Mounted {
		s.audit(r, "smb_unmount", id, firstLine(res.Error), false, "")
		code := http.StatusBadGateway
		if res.Reason == "busy" {
			code = http.StatusConflict
		}
		fail(w, code, res.Error+func() string {
			if res.Remedy != "" {
				return "\n出路：" + res.Remedy
			}
			return ""
		}())
		return
	}
	// 手动卸载：抑制自动重挂（否则 30 秒后它自己又挂回来）。
	s.smbMu.Lock()
	if st := s.smbRun[id]; st != nil {
		st.Suppressed = true
	}
	s.smbMu.Unlock()
	s.audit(r, "smb_unmount", id, "已卸载 "+m.Name+"（"+mp+"）", true, "")
	ok(w, v)
}

// ---------- 给 Jellyfin 用：已挂载的网络盘路径 ----------

// mountedSMBMounts 返回**当前真的挂载了**的网络盘（给媒体目录选择器当候选）。
// 读不到挂载表就返回 nil + 错误，让调用方如实说明，绝不猜。
func (s *Server) mountedSMBMounts(ctx context.Context) ([]map[string]any, error) {
	list, err := s.loadSMBMounts(ctx)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	table, terr := smb.MountTable(ctx)
	if terr != nil {
		return nil, terr
	}
	out := []map[string]any{}
	for _, m := range list {
		mp := smb.MountPoint(s.customMountBase(), m.Name)
		if _, mounted := smb.EntryAt(table, mp); !mounted {
			continue
		}
		out = append(out, map[string]any{
			"id": m.ID, "name": m.Name, "mount_point": mp, "read_only": m.ReadOnly,
		})
	}
	return out, nil
}

// ---------- 启动时的自动重挂（带退避，不阻塞启动） ----------

// StartSMBAutoMount 由 cmdServe 在后台调用：NAS 离线绝不能拖住面板启动。
func (s *Server) StartSMBAutoMount(ctx context.Context) {
	s.smbLoopOnce.Do(func() { go s.smbAutoMountLoop(ctx) })
}

func (s *Server) smbAutoMountLoop(ctx context.Context) {
	// 启动后稍等：先把 nginx/市场预热这类启动动作让出去。
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	for {
		s.smbAutoMountPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(smbPassEvery):
		}
	}
}

// smbAutoMountPass 巡一遍：没挂上的按各自退避重试一次。任一条失败只记状态，不影响别人。
func (s *Server) smbAutoMountPass(ctx context.Context) {
	list, err := s.loadSMBMounts(ctx)
	if err != nil {
		return
	}
	if len(list) == 0 {
		return
	}
	table, terr := smb.MountTable(ctx)
	if terr != nil {
		return
	}
	now := time.Now()
	for _, m := range list {
		mp := smb.MountPoint(s.customMountBase(), m.Name)
		if _, mounted := smb.EntryAt(table, mp); mounted {
			s.smbMu.Lock()
			if st := s.smbRun[m.ID]; st != nil {
				st.Suppressed, st.LastError, st.Fails, st.NextAttempt = false, "", 0, time.Time{}
			}
			s.smbMu.Unlock()
			continue
		}
		s.smbMu.Lock()
		st := s.smbRun[m.ID]
		if st == nil {
			st = &smbRuntime{}
			if s.smbRun == nil {
				s.smbRun = map[string]*smbRuntime{}
			}
			s.smbRun[m.ID] = st
		}
		skip := st.Busy || st.Suppressed || (!st.NextAttempt.IsZero() && now.Before(st.NextAttempt))
		s.smbMu.Unlock()
		if skip {
			continue
		}
		if !s.smbBegin(m.ID) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res := s.mountOne(cctx, m)
		cancel()
		s.smbEnd(m.ID)
		if res.Error == "" && res.Mounted {
			s.Log.Info("[smb] 自动挂载 %s → %s", m.Name, mp)
		}
	}
}
