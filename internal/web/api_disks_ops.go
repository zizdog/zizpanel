package web

// api_disks_ops.go —— 磁盘工具里"会改盘"的动作：抹盘/格式化、建卷、删卷、重命名。
//
// 2026-09 用户放开限制后的口径：
//   * 对**非系统盘**放开全部常用操作（eraseDisk / eraseVolume / addVolume /
//     deleteVolume / rename），不再要求"容器必须为空"；
//   * **系统盘相关设备一律拒绝**（沿用 computeProtected），且拒绝时**不会执行
//     任何 diskutil 写命令**（有测试断言）；
//   * 执行前**重新枚举一次**（TOCTOU 防护）：重跑 diskutil list/info，确认
//     ① 设备仍在列表里、② 仍是非系统盘、③ UUID/容量与用户确认框里看到的一致，
//     任何一项对不上就 409 拒绝（"盘可能被换过/状态变了，请刷新后重试"）；
//   * 写入前把"该设备/容器的卷与容量快照"记进审计（before_state），便于事后追溯；
//   * diskutil 一律 60s 超时 + stderr 原文；失败/超时后回读真实状态再给结论；
//   * 不依赖任何 GUI 授权弹窗（无头硬要求，源码 grep 门禁见 api_disks_test.go）。
//
// 2026-09 用户报障后的改造：抹盘/建卷/删卷/重命名都是**不可逆的分钟级动作**，
// 却挂在同步 HTTP 请求上 —— 关掉窗口就找不回进度，用户只能看"请等待"。
// 现在这四类动作统一走**任务中心**（照抄 handleSiteAppInstall / 文件压缩解压的用法）：
//   * handler 先做**同步预检**：确认不匹配 / 系统盘 / 身份（UUID/容量）对不上
//     一律当场 4xx，**不创建任务、不执行任何 diskutil 写命令**；
//   * 预检通过才 launchTask，立刻 202 + task_id，进度/日志/中断都复用任务中心；
//   * 任务体里在真正执行 diskutil 之前**再校验一次**（TOCTOU）：排队等调度期间
//     盘可能被拔掉/换掉，所以 handler 的那次结论不能信；
//   * 任务日志里的命令**从 cmd.Args 派生**（AGENTS.md 规矩），不手写标签；
//   * 完成后回读并展示：卷名 / 容量 / 挂载点与挂载状态 / 容器剩余空间；
//   * 审计仍带动作前快照：成功走任务结果的 Steps（launchTask 会写审计），
//     失败走错误文本里的 before={...}。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// diskWriteTimeout 是所有"会改盘"的 diskutil 命令超时（用户要求 30–60s）。
var diskWriteTimeout = 60 * time.Second

// ---------- 文件系统目录（界面与后端共用同一份取舍说明） ----------

type diskFSView struct {
	Key            string `json:"key"`
	Label          string `json:"label"`
	DiskutilFormat string `json:"diskutil_format"`
	VFSType        string `json:"vfstype"`
	Note           string `json:"note"`
}

var diskFilesystems = []diskFSView{
	{Key: "apfs", Label: "APFS", DiskutilFormat: "APFS", VFSType: "apfs",
		Note: "macOS 原生、快、支持快照/加密。Windows/Linux 不能原生读写 —— 本机镜像盘推荐。"},
	{Key: "hfs+", Label: "Mac OS 扩展（HFS+）", DiskutilFormat: "HFS+", VFSType: "hfs",
		Note: "旧、兼容性略好于 APFS；非 Apple 平台同样要第三方工具，不推荐新盘。"},
	{Key: "exfat", Label: "exFAT", DiskutilFormat: "ExFAT", VFSType: "exfat",
		Note: "macOS / Windows / Linux 都能读写，适合插拔硬件做备份/迁移。代价：无日志（意外拔盘更易损坏）、权限语义弱。"},
	{Key: "fat32", Label: "MS-DOS (FAT32)", DiskutilFormat: "MS-DOS FAT32", VFSType: "msdos",
		Note: "全平台兼容；但单文件 ≤ 4 GiB —— 镜像包/模型经常超过，不适合做镜像盘。"},
	{Key: "keep", Label: "保持现状（不格式化）", DiskutilFormat: "", VFSType: "",
		Note: "不执行任何格式化。要写盘请选择具体文件系统。"},
}

func diskFilesystemViews() []diskFSView {
	out := make([]diskFSView, len(diskFilesystems))
	copy(out, diskFilesystems)
	return out
}

func lookupDiskFS(key string) (diskFSView, bool) {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, f := range diskFilesystems {
		if strings.ToLower(f.Key) == k {
			return f, true
		}
	}
	return diskFSView{}, false
}

func diskFSKeys() string {
	keys := make([]string, 0, len(diskFilesystems))
	for _, f := range diskFilesystems {
		keys = append(keys, f.Key)
	}
	return strings.Join(keys, " / ")
}

// ---------- 快照辅助 ----------

func (snap *diskSnapshot) containsID(id string) bool {
	for _, x := range snap.AllIDs {
		if x == id {
			return true
		}
	}
	return false
}

func (snap *diskSnapshot) findInfo(id string) (diskInfo, bool) {
	for _, g := range snap.Groups {
		if g.Disk.ID == id {
			return g.Disk, true
		}
		for _, p := range g.Partitions {
			if p.ID == id {
				return p, true
			}
		}
	}
	return diskInfo{}, false
}

// containerRefFor 找出某设备所属/本身代表的 APFS 容器引用。
func (snap *diskSnapshot) containerRefFor(id string) string {
	if i, okk := snap.findInfo(id); okk && i.ContainerRef != "" {
		return i.ContainerRef
	}
	for _, g := range snap.Groups {
		if g.Disk.ID == id {
			if g.Disk.Content == "Apple_APFS_Container" {
				return id
			}
			for _, p := range g.Partitions {
				if p.ContainerRef != "" {
					return p.ContainerRef
				}
			}
			return ""
		}
		for _, p := range g.Partitions {
			if p.ID == id {
				return p.ContainerRef
			}
		}
	}
	return ""
}

func (snap *diskSnapshot) findVolumeInContainer(ref, name string) (diskInfo, bool) {
	for _, g := range snap.Groups {
		if g.Disk.ID != ref {
			continue
		}
		for _, p := range g.Partitions {
			if p.VolumeName == name {
				return p, true
			}
		}
	}
	return diskInfo{}, false
}

// diskStateSummary 是"动作前/后状态"的人话摘要，写进审计也回给前端。
func diskStateSummary(snap *diskSnapshot, id string) string {
	for _, g := range snap.Groups {
		if g.Disk.ID == id {
			d := g.Disk
			parts := []string{fmt.Sprintf("%s(name=%q fs=%q size=%d mounted=%v)", d.ID, d.VolumeName, d.Filesystem, d.SizeBytes, d.Mounted)}
			if g.Init != nil && g.Init.ContainerRef != "" {
				parts = append(parts, fmt.Sprintf("container=%s free=%d volumes=[%s]",
					g.Init.ContainerRef, g.Init.ContainerFree, strings.Join(g.Init.VolumeNames, ",")))
			} else {
				names := make([]string, 0, len(g.Partitions))
				for _, p := range g.Partitions {
					names = append(names, p.ID+":"+p.VolumeName)
				}
				parts = append(parts, "partitions=["+strings.Join(names, ",")+"]")
			}
			return strings.Join(parts, " ")
		}
		for _, p := range g.Partitions {
			if p.ID == id {
				cp := p
				s := fmt.Sprintf("%s(name=%q fs=%q size=%d mounted=%v uuid=%s)", cp.ID, cp.VolumeName, cp.Filesystem, cp.SizeBytes, cp.Mounted, cp.UUID)
				if g.Init != nil && g.Init.ContainerRef != "" {
					s += fmt.Sprintf(" container=%s volumes=[%s]", g.Init.ContainerRef, strings.Join(g.Init.VolumeNames, ","))
				}
				return s
			}
		}
	}
	return id
}

// ---------- TOCTOU：执行前重新枚举 ----------

type diskWriteCtx struct {
	snap   *diskSnapshot
	info   diskInfo // 命令执行前最后一次单设备 info 的结论
	before string   // 审计用的"动作前状态"
}

// beginDiskWrite 是所有会改盘动作的统一入口：
//   - 手输标识必须与 URL 里的设备标识完全一致；
//   - 重新枚举（diskutil list + 全设备 info）确认设备仍在、仍非系统盘；
//   - 再单独 info 一次，比较 UUID/容量（防"页面看到的盘"与"现在操作的盘"不是同一块）；
//   - 返回 (nil, code, msg) 表示拒绝（调用方必须**不执行**任何 diskutil 写命令）。
func (s *Server) beginDiskWrite(ctx context.Context, id, confirm, expectUUID string, expectSize int64) (*diskWriteCtx, int, string) {
	if !validDiskID(id) {
		return nil, http.StatusBadRequest, "设备标识不合法：" + id + "（应形如 disk9 / disk9s2 / disk4s1）"
	}
	if strings.TrimSpace(confirm) != id {
		return nil, http.StatusBadRequest,
			"二次确认失败：请手动输入磁盘标识 " + id + "（原样输入）后才能执行"
	}
	// 重新枚举：不看页面数据，重跑 list + info。
	snap, err := collectDiskSnapshot(ctx, diskScopeAll)
	if err != nil {
		return nil, http.StatusInternalServerError, "执行前重新枚举磁盘失败：" + err.Error()
	}
	if _, code, msg := snap.diskLookup(id); msg != "" {
		return nil, code, msg
	}
	if why := snap.guardWrite(id); why != "" {
		return nil, http.StatusForbidden, why
	}
	// 再贴近命令一次单设备 info，比较身份。
	fresh, err := queryDiskInfo(ctx, id)
	if err != nil {
		return nil, http.StatusConflict,
			"设备 " + id + " 在重新读取时消失或读不到（" + err.Error() + "）：盘可能被拔出/换过，请刷新后重试"
	}
	if expectUUID != "" && fresh.UUID != "" && !strings.EqualFold(fresh.UUID, expectUUID) {
		return nil, http.StatusConflict,
			"磁盘标识一致但卷 UUID 变了（页面 " + expectUUID + " / 实际 " + fresh.UUID +
				"）：盘可能被换过或已被重新格式化，请刷新后重试"
	}
	if expectSize > 0 && fresh.SizeBytes > 0 && fresh.SizeBytes != expectSize {
		return nil, http.StatusConflict,
			fmt.Sprintf("容量与页面不一致（页面 %d / 实际 %d）：盘可能被换过，请刷新后重试", expectSize, fresh.SizeBytes)
	}
	// 用最新一次 info 再确认一次系统盘判据（防止枚举与单读之间的窗口）。
	if fresh.Internal || fresh.MountPoint == "/" || strings.HasPrefix(fresh.MountPoint, "/System/Volumes/") {
		return nil, http.StatusForbidden,
			"拒绝操作 " + id + "：重新枚举时它被判定为系统盘相关设备（Internal=" +
				fmt.Sprintf("%v", fresh.Internal) + "，挂载点=" + fresh.MountPoint + "）"
	}
	return &diskWriteCtx{snap: snap, info: fresh, before: diskStateSummary(snap, id)}, 0, ""
}

// ---------- 通用执行 + 回读 ----------

type diskOpResult struct {
	ID            string         `json:"id"`
	Action        string         `json:"action"`
	Command       string         `json:"command"`
	Stdout        string         `json:"stdout"`
	Stderr        string         `json:"stderr"`
	Verified      bool           `json:"verified"`
	Created       bool           `json:"created"`
	BeforeState   string         `json:"before_state"`
	AfterState    string         `json:"after_state"`
	Message       string         `json:"message"`
	VolumeName    string         `json:"volume_name,omitempty"`
	NewDevice     string         `json:"new_device,omitempty"`
	MountPoint    string         `json:"mount_point,omitempty"`
	Mounted       bool           `json:"mounted"`
	SizeBytes     int64          `json:"size_bytes,omitempty"`
	Filesystem    string         `json:"filesystem,omitempty"`
	FSType        string         `json:"fs_type,omitempty"`
	ContainerRef  string         `json:"container_ref,omitempty"`
	ContainerFree int64          `json:"container_free,omitempty"`
	ContainerSize int64          `json:"container_size,omitempty"`
	VolumeCount   int            `json:"volume_count,omitempty"`
	VolumeNames   []string       `json:"volume_names,omitempty"`
	Detail        map[string]any `json:"detail,omitempty"`
}

// containerInfo 回读某个容器的卷列表与容量（"完成后返回容器剩余容量"用）。
func containerInfo(snap *diskSnapshot, ref string) (vols []string, free, size int64) {
	for _, g := range snap.Groups {
		if g.Init != nil && g.Init.ContainerRef == ref {
			return append([]string(nil), g.Init.VolumeNames...), g.Init.ContainerFree, g.Init.ContainerSize
		}
	}
	return nil, 0, 0
}

// executeDiskWrite 跑一条写命令并回读整个磁盘状态；区分"命令失败"与"回读失败"。
func (s *Server) executeDiskWrite(ctx context.Context, action, id, cmdLine string, args []string, before string) (*diskOpResult, *diskSnapshot, error, error) {
	out, errb, runErr := runDiskutil(ctx, diskWriteTimeout, args...)
	res := &diskOpResult{
		ID:          id,
		Action:      action,
		Command:     cmdLine,
		Stdout:      strings.TrimSpace(out),
		Stderr:      strings.TrimSpace(errb),
		BeforeState: before,
	}
	after, afterErr := collectDiskSnapshot(ctx, diskScopeAll)
	if after != nil {
		res.AfterState = diskStateSummary(after, id)
	}
	return res, after, runErr, afterErr
}

// emitDiskCmd 把"真正要执行的命令"写进任务日志。
//
// 命令标签**从 cmd.Args 派生**（AGENTS.md 规矩）：手写的标签很容易和实际执行的
// 不一致。diskutilBin 是绝对路径，这里构造 *exec.Cmd 只为拿它规范化后的 Args。
func emitDiskCmd(ctx context.Context, args []string) {
	cmd := exec.Command(diskutilBin, args...)
	services.EmitProgress(ctx, tasks.LevelCmd, "$ "+strings.Join(cmd.Args, " "))
}

// diskWriteFailure 把"命令失败/超时"与"回读失败"翻成任务错误文本。
// 失败也要把**动作前的卷/容量快照**带上（before=），便于事后追溯审计。
func diskWriteFailure(res *diskOpResult, runErr, afterErr error) error {
	detail := func(msg string) string {
		if res.BeforeState != "" {
			return "before={" + res.BeforeState + "} " + msg
		}
		return msg
	}
	if runErr != nil {
		base := res.Command + " 失败：" + runErr.Error()
		if errors.Is(runErr, errDiskTimeout) {
			base = res.Command + " 超时（" + diskWriteTimeout.String() + "）：设备可能已被改到一半"
		}
		if afterErr != nil {
			base += "。回读失败（未复核）：" + afterErr.Error()
		} else if res.AfterState != "" {
			base += "。回读状态：" + res.AfterState
		}
		return errors.New(detail(base))
	}
	if afterErr != nil {
		return errors.New(detail(res.Command + " 已执行，但回读磁盘状态失败，**未复核**结果：" + afterErr.Error()))
	}
	// 正常情况下不会到这里；留一个兜底，避免"什么都没发生却返回成功"。
	return errors.New(detail(res.Command + " 结果未知（未复核）"))
}

// diskReadbackLine 是"完成后回读"的用户可见摘要：
// 卷名 / 容量 / 文件系统 / 挂载点与挂载状态 / 容器剩余空间。
func diskReadbackLine(res *diskOpResult) string {
	dash := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "—"
		}
		return s
	}
	parts := []string{
		"卷名=" + dash(res.VolumeName),
		"容量=" + humanBytes(res.SizeBytes),
		"文件系统=" + dash(res.Filesystem),
	}
	if res.Mounted {
		line := "已挂载"
		if res.MountPoint != "" {
			line += "，挂载点=" + res.MountPoint
		}
		parts = append(parts, line)
	} else {
		parts = append(parts, "未挂载")
	}
	if res.ContainerRef != "" {
		parts = append(parts, fmt.Sprintf("容器=%s 剩余空间=%s（共 %s，%d 个卷：%s）",
			res.ContainerRef, humanBytes(res.ContainerFree), humanBytes(res.ContainerSize),
			res.VolumeCount, strings.Join(res.VolumeNames, "、")))
	}
	return "回读：" + strings.Join(parts, "，")
}

// diskTaskResult 把一次写动作的结果包成任务结果。
//
// 为什么用 *services.InstallResult：任务中心的审计由 launchTask 在任务结束时统一写，
// 成功时取的是 summarizeResult(result) —— 它只认 InstallResult，会把 Steps 逐条写进
// 审计。把 before/after 快照放进 Steps，审计里就自然带上了动作前状态。
func diskTaskResult(res *diskOpResult, steps []string) *services.InstallResult {
	return &services.InstallResult{
		App:     "disk",
		Name:    res.Action,
		Message: res.Message,
		Steps:   steps,
	}
}

// ---------- 任务体：真正执行一次会改盘的动作 ----------

// diskWritePlan 描述一次危险动作。extra / args / verify 都接收"任务内重新校验后的"
// 上下文，保证用到的是运行体此刻的真实结论，而不是 handler 预检时的旧快照。
type diskWritePlan struct {
	action     string
	id         string
	confirm    string
	expectUUID string
	expectSize int64
	// extra 是动作专属检查；返回 (http状态码, 拒绝理由)，理由非空即拒绝（不执行写命令）。
	extra func(w *diskWriteCtx) (int, string)
	// args 用校验后的结论构造真正执行的 diskutil 参数。
	args func(w *diskWriteCtx) []string
	// verify 在命令成功、回读快照后校验结果，返回 (结果摘要, 错误)。
	verify func(w *diskWriteCtx, after *diskSnapshot, res *diskOpResult) (string, string)
}

// launchDiskWrite 是四个危险动作 handler 的统一入口：
//  1. 先做**同步预检** —— 拒绝时当场 4xx，**不创建任务、不执行任何写命令**；
//  2. 预检通过才交给任务中心（202 + task_id），进度/日志/中断复用既有机制。
func (s *Server) launchDiskWrite(w http.ResponseWriter, r *http.Request, title string, plan *diskWritePlan) {
	wctx, code, msg := s.beginDiskWrite(r.Context(), plan.id, plan.confirm, plan.expectUUID, plan.expectSize)
	if msg != "" {
		s.audit(r, plan.action, plan.id, msg, false, "")
		fail(w, code, msg)
		return
	}
	if plan.extra != nil {
		if c, why := plan.extra(wctx); why != "" {
			if c == 0 {
				c = http.StatusBadRequest
			}
			s.audit(r, plan.action, plan.id, why, false, "")
			fail(w, c, why)
			return
		}
	}
	s.launchTask(w, r, plan.action, plan.id, title, plan.action,
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.runDiskWritePlan(ctx, log, plan)
		})
}

// runDiskWritePlan 是任务体：在**真正执行 diskutil 之前**再校验一次（TOCTOU），
// 命令标签从 cmd.Args 派生，执行后回读并展示结果。
func (s *Server) runDiskWritePlan(ctx context.Context, log tasks.LogFunc, plan *diskWritePlan) (any, error) {
	log(tasks.LevelStep, "执行前二次校验：设备仍在 / 仍非系统盘 / 标识（UUID）与容量一致")
	// ★ 排队等任务中心调度期间盘可能被换掉，handler 的预检结论不能信，这里必须重做。
	wctx, _, msg := s.beginDiskWrite(ctx, plan.id, plan.confirm, plan.expectUUID, plan.expectSize)
	if msg != "" {
		return nil, errors.New(msg)
	}
	if plan.extra != nil {
		if _, why := plan.extra(wctx); why != "" {
			return nil, errors.New(why)
		}
	}
	args := plan.args(wctx)
	cmdLine := "diskutil " + strings.Join(args, " ")
	emitDiskCmd(ctx, args)
	log(tasks.LevelStep, "开始执行："+cmdLine)

	res, after, runErr, afterErr := s.executeDiskWrite(ctx, plan.action, plan.id, cmdLine, args, wctx.before)
	// 命令输出（含 stderr 原文）逐行进任务日志。
	for _, ln := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(ln) != "" {
			log(tasks.LevelOut, ln)
		}
	}
	for _, ln := range strings.Split(res.Stderr, "\n") {
		if strings.TrimSpace(ln) != "" {
			log(tasks.LevelErr, ln)
		}
	}
	if runErr != nil || afterErr != nil {
		return nil, diskWriteFailure(res, runErr, afterErr)
	}
	summary, verr := plan.verify(wctx, after, res)
	if verr != "" {
		return nil, fmt.Errorf("%s 已执行，但回读校验不通过：%s。回读状态：%s", res.Command, verr, res.AfterState)
	}
	res.Verified = true
	res.Message = summary
	// 写操作已经落到真实设备上：让磁盘快照缓存失效，任务完成后的刷新一定拿到新状态。
	invalidateDiskSnapshotCache()
	steps := []string{
		"before={" + res.BeforeState + "}",
		"after={" + res.AfterState + "}",
		summary,
		diskReadbackLine(res),
	}
	for _, ln := range steps {
		log(tasks.LevelOK, ln)
	}
	return diskTaskResult(res, steps), nil
}

// ---------- 抹盘 / 格式化 ----------

type diskEraseReq struct {
	Filesystem string `json:"filesystem"`
	Name       string `json:"name"`
	Confirm    string `json:"confirm"`
	ExpectUUID string `json:"expect_uuid"`
	ExpectSize int64  `json:"expect_size"`
}

func (s *Server) handleDiskErase(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req diskEraseReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	fs, okk := lookupDiskFS(req.Filesystem)
	if !okk {
		fail(w, http.StatusBadRequest, "未知的文件系统 "+req.Filesystem+"；可选："+diskFSKeys())
		return
	}
	if fs.Key == "keep" {
		fail(w, http.StatusBadRequest, "你选择的是「保持现状（不格式化）」—— 没有可执行的格式化动作。请选择具体文件系统。")
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := validateVolumeName(name); err != nil {
		s.audit(r, "disk_erase", id, "卷名不合法："+err.Error(), false, "")
		fail(w, http.StatusBadRequest, "卷名不合法："+err.Error())
		return
	}
	plan := &diskWritePlan{
		action: "disk_erase", id: id, confirm: req.Confirm,
		expectUUID: req.ExpectUUID, expectSize: req.ExpectSize,
		extra: func(ww *diskWriteCtx) (int, string) {
			// APFS 容器分区不能直接 eraseVolume（实测 diskutil 会报
			// "in use by APFS as a Physical Store"）——提前给可行动的错误。
			if !ww.info.WholeDisk && ww.info.Content == "Apple_APFS" {
				return http.StatusBadRequest, "设备 " + id + " 是 APFS 容器（Apple_APFS 物理存储），不能直接格式化。" +
					"请改用「格式化 / 抹盘这块盘…」（整盘会重建它），或先用 diskutil apfs deleteContainer 删容器。"
			}
			return 0, ""
		},
		args: func(ww *diskWriteCtx) []string {
			if ww.info.WholeDisk {
				return []string{"eraseDisk", fs.DiskutilFormat, name, id}
			}
			return []string{"eraseVolume", fs.DiskutilFormat, name, id}
		},
		verify: func(ww *diskWriteCtx, after *diskSnapshot, res *diskOpResult) (string, string) {
			vol, verr := verifyErased(after, id, name, fs.VFSType, ww.info.WholeDisk)
			if verr != "" {
				return "", verr
			}
			res.VolumeName = vol.VolumeName
			res.NewDevice = vol.ID
			res.MountPoint = vol.MountPoint
			res.Mounted = vol.Mounted
			res.SizeBytes = vol.SizeBytes
			res.Filesystem = vol.Filesystem
			res.FSType = vol.FSType
			if ref := after.containerRefFor(vol.ID); ref != "" {
				res.ContainerRef = ref
				names, free, size := containerInfo(after, ref)
				res.VolumeNames = names
				res.VolumeCount = len(names)
				res.ContainerFree = free
				res.ContainerSize = size
			}
			msg := fmt.Sprintf("已把 %s 格式化为 %s（卷名 %q，设备 %s）", id, fs.Label, vol.VolumeName, vol.ID)
			if vol.Mounted {
				msg += "，挂载于 " + vol.MountPoint
			}
			return msg, ""
		},
	}
	s.launchDiskWrite(w, r, "抹盘/格式化 "+id, plan)
}

// verifyErased 回读确认格式化结果（整盘与分区两条路径）。
func verifyErased(snap *diskSnapshot, device, name, vfsType string, wasWhole bool) (diskInfo, string) {
	if !wasWhole {
		ni, okk := snap.findInfo(device)
		if !okk {
			return diskInfo{}, "回读找不到设备 " + device
		}
		if ni.FSType != vfsType {
			return diskInfo{}, fmt.Sprintf("文件系统不一致：期望 %s，实际 %q", vfsType, ni.FSType)
		}
		if ni.VolumeName != name {
			return diskInfo{}, fmt.Sprintf("卷名不一致：期望 %q，实际 %q", name, ni.VolumeName)
		}
		return ni, ""
	}
	for _, g := range snap.Groups {
		if g.Disk.ID != device {
			continue
		}
		for _, p := range g.Partitions {
			if p.ContainerRef != "" {
				if v, okk := snap.findVolumeInContainer(p.ContainerRef, name); okk {
					if v.FSType != vfsType {
						return diskInfo{}, fmt.Sprintf("文件系统不一致：期望 %s，实际 %q", vfsType, v.FSType)
					}
					return v, ""
				}
				continue
			}
			if p.VolumeName == name {
				if p.FSType != vfsType {
					return diskInfo{}, fmt.Sprintf("文件系统不一致：期望 %s，实际 %q", vfsType, p.FSType)
				}
				return p, ""
			}
		}
	}
	return diskInfo{}, "回读后没有在 " + device + " 上找到名为 " + name + " 的卷"
}

// ---------- 新建 APFS 卷 ----------

type diskVolumeCreateReq struct {
	Name       string `json:"name"`
	Confirm    string `json:"confirm"`
	ExpectUUID string `json:"expect_uuid"`
	ExpectSize int64  `json:"expect_size"`
}

func (s *Server) handleDiskVolumeCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req diskVolumeCreateReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := validateVolumeName(name); err != nil {
		s.audit(r, "disk_volume_create", id, "卷名不合法："+err.Error(), false, "")
		fail(w, http.StatusBadRequest, "卷名不合法："+err.Error())
		return
	}
	plan := &diskWritePlan{
		action: "disk_volume_create", id: id, confirm: req.Confirm,
		expectUUID: req.ExpectUUID, expectSize: req.ExpectSize,
		extra: func(ww *diskWriteCtx) (int, string) {
			ref := ww.snap.containerRefFor(id)
			if ref == "" {
				return http.StatusBadRequest, "设备 " + id + " 上没有 APFS 容器。要新建卷请先用「格式化」把它做成 APFS。"
			}
			if why := ww.snap.guardWrite(ref); why != "" {
				return http.StatusForbidden, "拒绝新建卷：容器 " + ref + " 属于系统盘（" + why + "）"
			}
			return 0, ""
		},
		args: func(ww *diskWriteCtx) []string {
			return []string{"apfs", "addVolume", ww.snap.containerRefFor(id), "APFS", name}
		},
		verify: func(ww *diskWriteCtx, after *diskSnapshot, res *diskOpResult) (string, string) {
			ref := ww.snap.containerRefFor(id)
			vol, okk := after.findVolumeInContainer(ref, name)
			if !okk {
				return "", "回读容器 " + ref + " 后没有找到名为 " + name + " 的卷"
			}
			res.Created = true
			res.VolumeName = name
			res.NewDevice = vol.ID
			res.MountPoint = vol.MountPoint
			res.Mounted = vol.Mounted
			res.SizeBytes = vol.SizeBytes
			res.Filesystem = vol.Filesystem
			res.FSType = vol.FSType
			res.ContainerRef = ref
			names, free, size := containerInfo(after, ref)
			res.VolumeNames = names
			res.VolumeCount = len(names)
			res.ContainerFree = free
			res.ContainerSize = size
			msg := fmt.Sprintf("已在容器 %s 新建卷 %q（%s）", ref, name, vol.ID)
			if vol.Mounted {
				msg += "，挂载于 " + vol.MountPoint
			}
			return msg, ""
		},
	}
	s.launchDiskWrite(w, r, "新建 APFS 卷 "+id, plan)
}

// ---------- 删除 APFS 卷 ----------

type diskVolumeDeleteReq struct {
	Confirm    string `json:"confirm"`
	ExpectUUID string `json:"expect_uuid"`
	ExpectSize int64  `json:"expect_size"`
}

func (s *Server) handleDiskVolumeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req diskVolumeDeleteReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	plan := &diskWritePlan{
		action: "disk_volume_delete", id: id, confirm: req.Confirm,
		expectUUID: req.ExpectUUID, expectSize: req.ExpectSize,
		extra: func(ww *diskWriteCtx) (int, string) {
			if ww.info.WholeDisk || !ww.info.APFSVolume {
				return http.StatusBadRequest, "设备 " + id + " 不是 APFS 卷，不能删除。只有 APFS 卷支持本操作。"
			}
			return 0, ""
		},
		args: func(ww *diskWriteCtx) []string {
			return []string{"apfs", "deleteVolume", id}
		},
		verify: func(ww *diskWriteCtx, after *diskSnapshot, res *diskOpResult) (string, string) {
			if _, still := after.findInfo(id); still {
				return "", "回读发现设备 " + id + " 仍然存在"
			}
			res.VolumeName = ww.info.VolumeName
			res.SizeBytes = ww.info.SizeBytes
			res.Filesystem = ww.info.Filesystem
			res.FSType = ww.info.FSType
			res.Mounted = false
			if ref := ww.snap.containerRefFor(id); ref != "" {
				res.ContainerRef = ref
				names, free, size := containerInfo(after, ref)
				res.VolumeNames = names
				res.VolumeCount = len(names)
				res.ContainerFree = free
				res.ContainerSize = size
			}
			return fmt.Sprintf("已删除 APFS 卷 %s（%q）及其数据", id, ww.info.VolumeName), ""
		},
	}
	s.launchDiskWrite(w, r, "删除 APFS 卷 "+id, plan)
}

// ---------- 卷重命名 ----------

type diskVolumeRenameReq struct {
	Name       string `json:"name"`
	Confirm    string `json:"confirm"`
	ExpectUUID string `json:"expect_uuid"`
	ExpectSize int64  `json:"expect_size"`
}

func (s *Server) handleDiskVolumeRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req diskVolumeRenameReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := validateVolumeName(name); err != nil {
		s.audit(r, "disk_volume_rename", id, "新卷名不合法："+err.Error(), false, "")
		fail(w, http.StatusBadRequest, "新卷名不合法："+err.Error())
		return
	}
	plan := &diskWritePlan{
		action: "disk_volume_rename", id: id, confirm: req.Confirm,
		expectUUID: req.ExpectUUID, expectSize: req.ExpectSize,
		extra: func(ww *diskWriteCtx) (int, string) {
			if ww.info.WholeDisk {
				return http.StatusBadRequest, "设备 " + id + " 是整盘，不能重命名；请选择它的某个卷。"
			}
			return 0, ""
		},
		args: func(ww *diskWriteCtx) []string {
			return []string{"rename", id, name}
		},
		verify: func(ww *diskWriteCtx, after *diskSnapshot, res *diskOpResult) (string, string) {
			ni, okk := after.findInfo(id)
			if !okk || ni.VolumeName != name {
				got := ""
				if okk {
					got = ni.VolumeName
				}
				return "", fmt.Sprintf("回读卷名不一致：期望 %s，实际 %q", name, got)
			}
			res.VolumeName = name
			res.MountPoint = ni.MountPoint
			res.Mounted = ni.Mounted
			res.SizeBytes = ni.SizeBytes
			res.Filesystem = ni.Filesystem
			res.FSType = ni.FSType
			if ref := after.containerRefFor(id); ref != "" {
				res.ContainerRef = ref
				names, free, size := containerInfo(after, ref)
				res.VolumeNames = names
				res.VolumeCount = len(names)
				res.ContainerFree = free
				res.ContainerSize = size
			}
			return fmt.Sprintf("已把 %s 重命名为 %q", id, name), ""
		},
	}
	s.launchDiskWrite(w, r, "重命名卷 "+id, plan)
}

// ---------- 申请外接盘授权（用户点按钮；读了才会弹窗） ----------

// diskVolumeAuthWait 是"等用户在弹窗上点「允许」"的上限：超时就如实报仍被拒。
var diskVolumeAuthWait = 90 * time.Second

// 同步预检的拒绝理由（用户可见文案；前端按钮禁用时也用同一份前两条）。
const (
	diskVolumeAuthNoVolumeReason  = "先接上外接硬盘"
	diskVolumeAuthNoSessionReason = "现在没人在机器前，弹窗没人点；请到真机操作，或按下面的路径手动授权"
	// diskVolumeAuthConfirmRequiredReason 与前端确认框是同一件事：只点按钮不算，
	// 必须让用户先确认"人就在屏幕前、准备好点弹窗了"。
	diskVolumeAuthConfirmRequiredReason = "请先确认：接下来需要你在这台机器的屏幕上点『允许』"
)

// 机器可读的拒绝原因（响应里的 reason 字段），测试按它断言，不靠中文文案。
const (
	diskVolumeAuthReasonNoVolume        = "no_volume"
	diskVolumeAuthReasonNoConsole       = "no_console"
	diskVolumeAuthReasonConfirmRequired = "confirm_required"
)

// diskVolumeAuthErrView 是拒绝响应的正文：msg 给人看，reason 给机器断言。
type diskVolumeAuthErrView struct {
	OK     bool   `json:"ok"`
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
}

func failDiskVolumeAuth(w http.ResponseWriter, code int, msg, reason string) {
	writeJSON(w, code, diskVolumeAuthErrView{Msg: msg, Reason: reason})
}

// diskVolumeAuthManualPath 是"仍被拒"时的手动授权路径（与文件管理里的 TCC 指引同一份事实）。
func diskVolumeAuthManualPath() string {
	return "手动授权：系统设置 → 隐私与安全性 → 完全磁盘访问权限 → 打开 " + panelBinaryForGuide +
		"（这块盘还要给站点/镜像站用，就把 nginx 也打开）。" +
		"授权跟二进制绑定：面板升级后可能要再授一次（用固定证书签名的正式包通常不用）。"
}

// diskVolumeAuthPrecheck 按固定顺序回答"现在能不能弹窗"：① 有没有非系统卷、
// ② 有没有人在屏幕前、③ 用户有没有显式确认"我准备好点弹窗了"。每条拒绝都有
// **独立可断言**的 reason（no_volume / no_console / confirm_required）。
// 三条判据都**不读卷内容**；mounts 只用于日志与结果。
func diskVolumeAuthPrecheck(confirm bool) (mounts []string, consoleUser string, code int, msg, reason string) {
	mounts = diskVolumeAuthMountsFn()
	consoleUser = strings.TrimSpace(diskVolumeAuthConsoleUserFn())
	if len(mounts) == 0 {
		return nil, consoleUser, http.StatusConflict, diskVolumeAuthNoVolumeReason, diskVolumeAuthReasonNoVolume
	}
	if !diskConsoleSessionOK(consoleUser) {
		return mounts, consoleUser, http.StatusConflict, diskVolumeAuthNoSessionReason, diskVolumeAuthReasonNoConsole
	}
	if !confirm {
		return mounts, consoleUser, http.StatusBadRequest,
			diskVolumeAuthConfirmRequiredReason, diskVolumeAuthReasonConfirmRequired
	}
	return mounts, consoleUser, 0, "", ""
}

// handleDiskVolumeAuthRequest 是「申请授权」的入口：**同步预检 → 任务中心**。
//
// 为什么必须预检（铁律 12）：读外接卷会让 macOS 弹授权询问；没人在屏幕前时弹了也没人点，
// 系统只会把它记成 denial。所以没有人 / 没有盘 / 没有显式确认 → 当场 4xx，
// **不创建任务、一个字节都不读**。确认不能只靠前端自觉（坑 192）。
func (s *Server) handleDiskVolumeAuthRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	// 空 body 合法：等于「没确认」，交给预检第③条给 confirm_required（不是解码错误）。
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	_, consoleUser, code, msg, reason := diskVolumeAuthPrecheck(req.Confirm)
	if msg != "" {
		s.audit(r, "disk_volume_auth", "", "拒绝："+msg+"（控制台用户 "+consoleUser+"，reason="+reason+"）", false, "")
		failDiskVolumeAuth(w, code, msg, reason)
		return
	}
	s.launchTask(w, r, "disk_volume_auth", "disk-volume-auth", "申请访问外接盘授权", "disk_volume_auth",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			return s.runDiskVolumeAuth(ctx, log)
		})
}

// runDiskVolumeAuth 是任务体：再预检一次（排队期间人可能注销、盘可能被拔）→
// 读一次外接卷（这一步才会弹窗）→ 最多等 diskVolumeAuthWait → 如实报告每个卷的结果。
func (s *Server) runDiskVolumeAuth(ctx context.Context, log tasks.LogFunc) (any, error) {
	// 任务体复检的 confirm 恒为 true：用户已在 handler 那次显式确认过。
	mounts, _, _, reason, _ := diskVolumeAuthPrecheck(true)
	if reason != "" {
		return nil, errors.New(reason)
	}
	log(tasks.LevelStep, "已向系统发起授权请求，请在这台机器的屏幕上点『允许』（最多等 "+
		diskVolumeAuthWait.Round(time.Second).String()+"，不点就如实报仍被拒）")
	res := diskVolumeAuthRequestFn(ctx, diskVolumeAuthWait, func(f string, a ...any) {
		log(tasks.LevelStep, fmt.Sprintf(f, a...))
	})
	if res.Skipped {
		return nil, errors.New("未发起授权请求：" + res.Reason)
	}

	var lines, denied []string
	for _, m := range mounts {
		switch {
		case slices.Contains(res.Okay, m):
			lines = append(lines, m+"：已可访问")
			log(tasks.LevelOK, m+"：已可访问")
		case slices.Contains(res.Attempted, m):
			lines = append(lines, m+"：仍被拒（operation not permitted）")
			denied = append(denied, m)
			log(tasks.LevelErr, m+"：仍被拒")
		default:
			lines = append(lines, m+"：未读到（可能已卸载，非授权问题）")
			log(tasks.LevelOut, m+"：未读到（可能已卸载，非授权问题）")
		}
	}
	if len(res.Okay) == 0 {
		return nil, errors.New("外接盘仍不可访问：" + strings.Join(lines, "；") + "。" + diskVolumeAuthManualPath())
	}
	if len(denied) > 0 {
		return nil, errors.New(strings.Join(denied, "、") + " 仍被拒（已可访问：" +
			strings.Join(res.Okay, "、") + "）。" + diskVolumeAuthManualPath())
	}
	msg := "已可访问：" + strings.Join(res.Okay, "、")
	steps := append(lines, "结果："+msg)
	log(tasks.LevelOK, "结果："+msg)
	return &services.InstallResult{App: "disk", Name: "disk_volume_auth", Message: msg, Steps: steps}, nil
}
