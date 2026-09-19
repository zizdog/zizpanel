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

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
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
	snap, err := collectDiskSnapshot(ctx)
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
	after, afterErr := collectDiskSnapshot(ctx)
	if after != nil {
		res.AfterState = diskStateSummary(after, id)
	}
	return res, after, runErr, afterErr
}

// failWrite 统一处理"命令失败/超时"与"回读失败"，返回给前端人话。
func (s *Server) failDiskWrite(w http.ResponseWriter, r *http.Request, action, id string, res *diskOpResult, runErr, afterErr error) {
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
		s.audit(r, action, id, base, false, "")
		fail(w, http.StatusInternalServerError, base)
		return
	}
	if afterErr != nil {
		msg := res.Command + " 已执行，但回读磁盘状态失败，**未复核**结果：" + afterErr.Error()
		s.audit(r, action, id, msg, false, "")
		fail(w, http.StatusInternalServerError, msg)
		return
	}
	// 正常情况下不会到这里；留一个兜底，避免"什么都没发生却返回成功"。
	msg := res.Command + " 结果未知（未复核）"
	s.audit(r, action, id, msg, false, "")
	fail(w, http.StatusInternalServerError, msg)
}

func (s *Server) auditWriteOK(r *http.Request, action, id string, res *diskOpResult, msg string) {
	detail := "before={" + res.BeforeState + "} after={" + res.AfterState + "} " + msg
	s.audit(r, action, id, detail, true, "")
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
	wctx, code, msg := s.beginDiskWrite(r.Context(), id, req.Confirm, req.ExpectUUID, req.ExpectSize)
	if msg != "" {
		s.audit(r, "disk_erase", id, msg, false, "")
		fail(w, code, msg)
		return
	}
	// APFS 容器分区不能直接 eraseVolume（实测 diskutil 会报
	// "in use by APFS as a Physical Store"）——提前给可行动的错误，而不是让用户看天书。
	if !wctx.info.WholeDisk && wctx.info.Content == "Apple_APFS" {
		m := "设备 " + id + " 是 APFS 容器（Apple_APFS 物理存储），不能直接格式化。" +
			"请改用「格式化 / 抹盘这块盘…」（整盘会重建它），或先用 diskutil apfs deleteContainer 删容器。"
		s.audit(r, "disk_erase", id, m, false, "")
		fail(w, http.StatusBadRequest, m)
		return
	}
	var args []string
	if wctx.info.WholeDisk {
		args = []string{"eraseDisk", fs.DiskutilFormat, name, id}
	} else {
		args = []string{"eraseVolume", fs.DiskutilFormat, name, id}
	}
	cmdLine := "diskutil " + strings.Join(args, " ")
	res, after, runErr, afterErr := s.executeDiskWrite(r.Context(), "disk_erase", id, cmdLine, args, wctx.before)
	if runErr != nil || afterErr != nil {
		s.failDiskWrite(w, r, "disk_erase", id, res, runErr, afterErr)
		return
	}
	vol, verr := verifyErased(after, id, name, fs.VFSType, wctx.info.WholeDisk)
	if verr != "" {
		m := res.Command + " 已执行，但回读校验不通过：" + verr + "。回读状态：" + res.AfterState
		s.audit(r, "disk_erase", id, m, false, "")
		fail(w, http.StatusInternalServerError, m)
		return
	}
	res.Verified = true
	res.VolumeName = vol.VolumeName
	res.NewDevice = vol.ID
	res.MountPoint = vol.MountPoint
	res.Mounted = vol.Mounted
	res.Message = fmt.Sprintf("已把 %s 格式化为 %s（卷名 %q，设备 %s）", id, fs.Label, vol.VolumeName, vol.ID)
	if vol.Mounted {
		res.Message += "，挂载于 " + vol.MountPoint
	}
	s.auditWriteOK(r, "disk_erase", id, res, res.Message)
	ok(w, res)
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
	wctx, code, msg := s.beginDiskWrite(r.Context(), id, req.Confirm, req.ExpectUUID, req.ExpectSize)
	if msg != "" {
		s.audit(r, "disk_volume_create", id, msg, false, "")
		fail(w, code, msg)
		return
	}
	ref := wctx.snap.containerRefFor(id)
	if ref == "" {
		m := "设备 " + id + " 上没有 APFS 容器。要新建卷请先用「格式化」把它做成 APFS。"
		s.audit(r, "disk_volume_create", id, m, false, "")
		fail(w, http.StatusBadRequest, m)
		return
	}
	if why := wctx.snap.guardWrite(ref); why != "" {
		m := "拒绝新建卷：容器 " + ref + " 属于系统盘（" + why + "）"
		s.audit(r, "disk_volume_create", id, m, false, "")
		fail(w, http.StatusForbidden, m)
		return
	}
	args := []string{"apfs", "addVolume", ref, "APFS", name}
	cmdLine := "diskutil " + strings.Join(args, " ")
	res, after, runErr, afterErr := s.executeDiskWrite(r.Context(), "disk_volume_create", id, cmdLine, args, wctx.before)
	if runErr != nil || afterErr != nil {
		s.failDiskWrite(w, r, "disk_volume_create", id, res, runErr, afterErr)
		return
	}
	vol, okk := after.findVolumeInContainer(ref, name)
	if !okk {
		m := res.Command + " 退出正常，但回读容器 " + ref + " 后没有找到名为 " + name + " 的卷。回读状态：" + res.AfterState
		s.audit(r, "disk_volume_create", id, m, false, "")
		fail(w, http.StatusInternalServerError, m)
		return
	}
	res.Verified = true
	res.Created = true
	res.VolumeName = name
	res.NewDevice = vol.ID
	res.MountPoint = vol.MountPoint
	res.Mounted = vol.Mounted
	res.ContainerRef = ref
	names, free, size := containerInfo(after, ref)
	res.VolumeNames = names
	res.VolumeCount = len(names)
	res.ContainerFree = free
	res.ContainerSize = size
	res.Message = fmt.Sprintf("已在容器 %s 新建卷 %q（%s）", ref, name, vol.ID)
	if vol.Mounted {
		res.Message += "，挂载于 " + vol.MountPoint
	}
	s.auditWriteOK(r, "disk_volume_create", id, res, res.Message)
	ok(w, res)
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
	wctx, code, msg := s.beginDiskWrite(r.Context(), id, req.Confirm, req.ExpectUUID, req.ExpectSize)
	if msg != "" {
		s.audit(r, "disk_volume_delete", id, msg, false, "")
		fail(w, code, msg)
		return
	}
	if wctx.info.WholeDisk || !wctx.info.APFSVolume {
		m := "设备 " + id + " 不是 APFS 卷，不能删除。只有 APFS 卷支持本操作。"
		s.audit(r, "disk_volume_delete", id, m, false, "")
		fail(w, http.StatusBadRequest, m)
		return
	}
	args := []string{"apfs", "deleteVolume", id}
	cmdLine := "diskutil " + strings.Join(args, " ")
	res, after, runErr, afterErr := s.executeDiskWrite(r.Context(), "disk_volume_delete", id, cmdLine, args, wctx.before)
	if runErr != nil || afterErr != nil {
		s.failDiskWrite(w, r, "disk_volume_delete", id, res, runErr, afterErr)
		return
	}
	if _, still := after.findInfo(id); still {
		m := res.Command + " 退出正常，但回读发现设备 " + id + " 仍然存在。回读状态：" + res.AfterState
		s.audit(r, "disk_volume_delete", id, m, false, "")
		fail(w, http.StatusInternalServerError, m)
		return
	}
	res.Verified = true
	res.VolumeName = wctx.info.VolumeName
	res.Message = fmt.Sprintf("已删除 APFS 卷 %s（%q）及其数据", id, wctx.info.VolumeName)
	s.auditWriteOK(r, "disk_volume_delete", id, res, res.Message)
	ok(w, res)
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
	wctx, code, msg := s.beginDiskWrite(r.Context(), id, req.Confirm, req.ExpectUUID, req.ExpectSize)
	if msg != "" {
		s.audit(r, "disk_volume_rename", id, msg, false, "")
		fail(w, code, msg)
		return
	}
	if wctx.info.WholeDisk {
		m := "设备 " + id + " 是整盘，不能重命名；请选择它的某个卷。"
		s.audit(r, "disk_volume_rename", id, m, false, "")
		fail(w, http.StatusBadRequest, m)
		return
	}
	args := []string{"rename", id, name}
	cmdLine := "diskutil " + strings.Join(args, " ")
	res, after, runErr, afterErr := s.executeDiskWrite(r.Context(), "disk_volume_rename", id, cmdLine, args, wctx.before)
	if runErr != nil || afterErr != nil {
		s.failDiskWrite(w, r, "disk_volume_rename", id, res, runErr, afterErr)
		return
	}
	ni, okk := after.findInfo(id)
	if !okk || ni.VolumeName != name {
		got := ""
		if okk {
			got = ni.VolumeName
		}
		m := res.Command + " 退出正常，但回读卷名不一致：期望 " + name + "，实际 " + got + "。回读状态：" + res.AfterState
		s.audit(r, "disk_volume_rename", id, m, false, "")
		fail(w, http.StatusInternalServerError, m)
		return
	}
	res.Verified = true
	res.VolumeName = name
	res.MountPoint = ni.MountPoint
	res.Mounted = ni.Mounted
	res.Message = fmt.Sprintf("已把 %s 重命名为 %q", id, name)
	s.auditWriteOK(r, "disk_volume_rename", id, res, res.Message)
	ok(w, res)
}
