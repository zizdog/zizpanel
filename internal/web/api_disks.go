package web

// api_disks.go —— 「磁盘」页的后端：只读枚举 + 挂载/卸载 + 开机自动挂载。
// 会改盘的动作（抹盘/格式化/建卷/删卷/重命名）在 api_disks_ops.go。
//
// ============================================================================
//  这个文件的第一原则：**磁盘操作是不可逆的，宁可拒绝也不猜**
// ============================================================================
//
// 1. 所有写操作的目标必须**来自 `diskutil list -plist` 的真实输出**（白名单）。
//    用户传一个不存在/已拔出的 id → 4xx，绝不把字符串拼进命令行去试。
// 2. **一切系统盘相关设备一律拒绝写**（挂载/卸载/自动挂载/初始化）：
//    `diskutil info` 报 Internal/OSInternalMedia 的、`/` 所在卷、
//    以及它们的父盘/APFS 容器（沿 ParentWholeDisk / APFSPhysicalStores 传播到不动点）。
//    判据来自运行体，不靠设备名硬编码（`disk0`~`disk3` 只是本机的样子）。
// 3. **不看退出码当结论**：mount/unmount 之后回读 `diskutil info` 的挂载点；
//    addVolume 之后回读容器里的卷列表与挂载点。读不回来就如实说"未复核"。
// 4. **不依赖任何 GUI 授权弹窗**（用户 2026-09 明确要求）：
//    面板以 root 的 LaunchDaemon 运行时，`diskutil` 直接执行、不弹窗。
//    这里**禁止**出现任何"弹 GUI 授权窗、要人在屏幕上点一下"的脚本或提权 API
//    （具体黑名单在 api_disks_test.go 的 diskForbiddenGUIs，有门禁 grep 锁死）——
//    无头机器上它们会**永久挂起**，比失败更糟。有测试 grep 源码锁死。
//    唯一的例外是用户**主动**点的「申请授权」（api_disks_ops.go）：它读一次外接卷、
//    可能触发 macOS 的 TCC 询问，但同步预检保证"没人在屏幕前就一个字节都不读"（铁律 12）。
// 5. **需要人工介入的操作明确拒绝**：加密卷处于 Locked 时解锁要口令/钥匙串，
//    无头环境下不可用 → 409 + 人话说明，绝不静默挂起。
// 6. 每条 `diskutil` 都带超时，stderr 原文进错误；超时/失败后**一律回读真实状态**
//    再给结论（用户踩过"只报一句超时、盘已被改到一半"的坑）。
// 7. 每个写动作（含失败）都写审计：动作、设备、结果。
//
// 第一版**不做**：抹盘 / eraseDisk / 删分区表 / 改分区。
// 唯一的"创建"动作是 `diskutil apfs addVolume <空容器> APFS <卷名>`
// —— 在**已存在、且一个卷都没有**的 APFS 容器里建一个卷（见 handleDiskInitVolume）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/files"
)

// ---------- 可注入的执行器（沿用本仓库的包级测试钩子风格，不动 Server 结构体） ----------

var (
	// diskutilBin 是 diskutil 的绝对路径。可执行文件缺失时如实报错。
	diskutilBin = "/usr/sbin/diskutil"
	// diskFstabPath 是 macOS 的静态文件系统表。测试里会被指到临时文件。
	diskFstabPath = "/etc/fstab"
	// diskExec 是命令执行钩子：单测替换它，绝不碰真机磁盘。
	diskExec = defaultDiskExec

	diskListTimeout  = 25 * time.Second
	diskInfoTimeout  = 15 * time.Second
	diskMountTimeout = 60 * time.Second
	diskInitTimeout  = 60 * time.Second
	diskInfoParallel = 6
)

// 「申请授权」按钮要用的三个入口，全部做成包级变量：
//   - 前两个是**只读判据**（有没有人在屏幕前 / 有没有非系统卷），不读卷内容、不弹窗；
//   - 第三个才是"读一次外接卷"（会弹窗），只在任务体里、且同步预检通过后才调用。
//
// 做成变量是为了单测能造出"有人在/没人/有盘/没盘"四种情形，**不碰用户真实的外接盘**。
var (
	diskVolumeAuthConsoleUserFn = files.ConsoleUser
	diskVolumeAuthMountsFn      = files.NonSystemVolumeMounts
	diskVolumeAuthRequestFn     = files.RequestVolumeAuthorizationNow
)

// diskVolumeAuthView 是「申请授权」按钮看到的真实状态（下发进 GET /system/disks）。
//
// 刻意**不包含**"盘读得到吗"：那要读卷，读了就会触发系统弹窗，而铁律 12 不允许
// 面板在没人在场时碰外接卷。所以这里只给两个不碰卷的判据，前端据此决定按钮能不能点。
type diskVolumeAuthView struct {
	Mounts            []string `json:"mounts"`
	Count             int      `json:"count"`
	ConsoleUser       string   `json:"console_user"`
	HasConsoleSession bool     `json:"has_console_session"`
}

// diskVolumeAuthState 采集按钮判据（不读任何卷）。
func diskVolumeAuthState() diskVolumeAuthView {
	mounts := diskVolumeAuthMountsFn()
	if mounts == nil {
		mounts = []string{}
	}
	cu := strings.TrimSpace(diskVolumeAuthConsoleUserFn())
	return diskVolumeAuthView{
		Mounts: mounts, Count: len(mounts), ConsoleUser: cu,
		HasConsoleSession: diskConsoleSessionOK(cu),
	}
}

// diskConsoleSessionOK 判断控制台用户是不是"真有人坐在屏幕前"。
// 空串（没有登录会话）、root（登录窗口）、loginwindow 都不是 —— 此时弹窗没人点。
func diskConsoleSessionOK(user string) bool {
	switch strings.TrimSpace(user) {
	case "", "root", "loginwindow":
		return false
	}
	return true
}

// errDiskTimeout 是"命令被我们主动掐掉"的哨兵错误（区别于 diskutil 自己失败）。
var errDiskTimeout = errors.New("diskutil 命令超时")

func defaultDiskExec(ctx context.Context, timeout time.Duration, args ...string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, diskutilBin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return out.String(), errb.String(), errDiskTimeout
	}
	if errors.Is(cctx.Err(), context.Canceled) {
		return out.String(), errb.String(), context.Canceled
	}
	return out.String(), errb.String(), err
}

// runDiskutil 跑一条 diskutil 并返回 stdout/stderr/error。
// error 里优先带 stderr 原文（用户要看到系统说了什么，而不是"exit status 1"）。
func runDiskutil(ctx context.Context, timeout time.Duration, args ...string) (string, string, error) {
	out, errb, err := diskExec(ctx, timeout, args...)
	if err != nil {
		return out, errb, &diskExecError{args: args, stderr: strings.TrimSpace(errb), err: err}
	}
	return out, errb, nil
}

type diskExecError struct {
	args   []string
	stderr string
	err    error
}

func (e *diskExecError) Error() string {
	cmd := "diskutil " + strings.Join(e.args, " ")
	if errors.Is(e.err, errDiskTimeout) {
		return cmd + " 超时"
	}
	if e.stderr != "" {
		return cmd + " 失败：" + firstLine(e.stderr)
	}
	return cmd + " 失败：" + e.err.Error()
}

func (e *diskExecError) Unwrap() error { return e.err }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// ---------- 数据结构 ----------

type diskInfo struct {
	ID             string `json:"id"`
	Node           string `json:"device_node"`
	WholeDisk      bool   `json:"whole_disk"`
	Parent         string `json:"parent"`
	Content        string `json:"content"`
	SizeBytes      int64  `json:"size_bytes"`
	Filesystem     string `json:"filesystem"`
	FSType         string `json:"fs_type"`
	VolumeName     string `json:"volume_name"`
	MountPoint     string `json:"mount_point"`
	Mounted        bool   `json:"mounted"`
	Internal       bool   `json:"internal"`
	External       bool   `json:"external"`
	Removable      bool   `json:"removable"`
	Ejectable      bool   `json:"ejectable"`
	Encrypted      bool   `json:"encrypted"`
	EncryptedKnown bool   `json:"encrypted_known"`
	Locked         bool   `json:"locked"`
	SMARTStatus    string `json:"smart_status"`
	SMARTNote      string `json:"smart_note"`
	// Virtual 表示这是**合成设备**（diskutil 的 VirtualOrPhysical=Virtual），
	// 典型就是 APFS 容器：物理盘是另一台设备，卷建在容器里。
	// 页面据此把"同一块盘出现两次"讲清楚（用户 2026-09-19 报过这个困惑）。
	Virtual        bool     `json:"virtual"`
	BusProtocol    string   `json:"bus_protocol"`
	Model          string   `json:"model"`
	UUID           string   `json:"uuid"`
	DiskUUID       string   `json:"disk_uuid"`
	ContainerRef   string   `json:"container_ref"`
	PhysicalStores []string `json:"physical_stores"`

	SystemDisk    bool   `json:"system_disk"`
	ProtectReason string `json:"protect_reason"`
	AutoMount     bool   `json:"auto_mount"`
	HasFilesystem bool   `json:"has_filesystem"`
	Mountable     bool   `json:"mountable"`
	// APFSVolume 表示"这是一个 APFS 卷"（可以删/重命名/单独格式化）。
	APFSVolume bool `json:"apfs_volume"`

	InfoError string `json:"info_error,omitempty"`
	HasInfo   bool   `json:"-"`
}

type diskGroup struct {
	Disk       diskInfo      `json:"disk"`
	Partitions []diskInfo    `json:"partitions"`
	Init       *diskInitInfo `json:"init,omitempty"`
}

// diskInitInfo 是「初始化镜像盘」在一块盘卡片上的可行性结论（前端据此禁用按钮并写原因）。
type diskInitInfo struct {
	Eligible       bool     `json:"eligible"`
	ContainerRef   string   `json:"container_ref"`
	VolumeCount    int      `json:"volume_count"`
	VolumeNames    []string `json:"volume_names"`
	ContainerFree  int64    `json:"container_free"`
	ContainerSize  int64    `json:"container_size"`
	BackendCommand string   `json:"backend_command"`
	Reason         string   `json:"reason"`
}

type diskProtected struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type diskSnapshot struct {
	Groups    []diskGroup
	AllIDs    []string
	Protected map[string]string
	RootDev   string
	FstabText string
	FstabRead bool
	FstabErr  string
	Notes     []string
	IsRoot    bool
}

// ---------- 从 plist 构造 diskInfo ----------

func infoFromPlist(id string, m map[string]any) diskInfo {
	d := diskInfo{ID: id, HasInfo: true}
	d.Node = plistStr(m, "DeviceNode")
	if d.Node == "" {
		d.Node = "/dev/" + id
	}
	d.WholeDisk = plistBool(m, "WholeDisk")
	d.Parent = plistStr(m, "ParentWholeDisk")
	d.Content = plistStr(m, "Content")
	d.SizeBytes = plistInt(m, "Size")
	if d.SizeBytes == 0 {
		d.SizeBytes = plistInt(m, "TotalSize")
	}
	d.FSType = plistStr(m, "FilesystemType")
	d.Filesystem = firstNonEmpty(
		plistStr(m, "FilesystemUserVisibleName"),
		plistStr(m, "FilesystemName"),
		plistStr(m, "FilesystemType"),
	)
	d.VolumeName = firstNonEmpty(plistStr(m, "VolumeName"), plistStr(m, "IORegistryEntryName"))
	d.MountPoint = plistStr(m, "MountPoint")
	d.Mounted = d.MountPoint != ""
	d.Internal = plistBool(m, "Internal") || plistBool(m, "OSInternalMedia")
	d.External = plistBool(m, "RemovableMediaOrExternalDevice")
	d.Removable = plistBool(m, "Removable") || plistBool(m, "RemovableMedia")
	d.Ejectable = plistBool(m, "Ejectable")
	d.Locked = plistBool(m, "Locked")
	d.BusProtocol = plistStr(m, "BusProtocol")
	d.Virtual = strings.EqualFold(plistStr(m, "VirtualOrPhysical"), "Virtual")
	d.Model = firstNonEmpty(plistStr(m, "MediaName"), plistStr(m, "IORegistryEntryName"))
	d.DiskUUID = plistStr(m, "DiskUUID")
	d.UUID = firstNonEmpty(plistStr(m, "VolumeUUID"), d.DiskUUID)
	d.ContainerRef = plistStr(m, "APFSContainerReference")

	fv, fvOK := plistBoolField(m, "FileVault")
	enc, encOK := plistBoolField(m, "Encryption")
	encProp, encPropOK := plistBoolField(m, "EncryptionThisVolumeProper")
	d.EncryptedKnown = fvOK || encOK || encPropOK
	d.Encrypted = fv || enc || encProp

	rawSMART := plistStr(m, "SMARTStatus")
	switch strings.ToLower(rawSMART) {
	case "":
		d.SMARTStatus = "unknown"
		d.SMARTNote = "diskutil 未返回 SMART 状态（该总线的桥接芯片通常不透传，属正常）"
	case "verified":
		d.SMARTStatus = "verified"
		d.SMARTNote = "diskutil 报 Verified"
	case "failing":
		d.SMARTStatus = "failing"
		d.SMARTNote = "diskutil 报 Failing —— 这块盘可能即将故障，尽快备份"
	case "not supported":
		d.SMARTStatus = "not_supported"
		d.SMARTNote = "diskutil 报 Not Supported（USB/SATA 桥接常见，不是失败）"
	default:
		d.SMARTStatus = "unknown"
		d.SMARTNote = "diskutil 返回了无法归类的 SMART 状态：" + rawSMART
	}

	for _, it := range plistArray(m["APFSPhysicalStores"]) {
		if sub := plistMap(it); sub != nil {
			if s := plistStr(sub, "APFSPhysicalStore"); s != "" {
				d.PhysicalStores = append(d.PhysicalStores, s)
			}
		}
	}
	d.HasFilesystem = d.Filesystem != ""
	d.Mountable = d.HasFilesystem
	d.APFSVolume = d.FSType == "apfs" && !d.WholeDisk
	return d
}

// infoFromListNode 在 `diskutil info` 失败时用 `diskutil list` 的节点兜底：
// 至少让用户看见"盘在、容量多少"，同时带 InfoError 说明为什么没有更多信息。
func infoFromListNode(id string, node map[string]any, parent string, whole bool) diskInfo {
	d := diskInfo{ID: id, Node: "/dev/" + id}
	d.Content = plistStr(node, "Content")
	d.SizeBytes = plistInt(node, "Size")
	d.VolumeName = plistStr(node, "VolumeName")
	d.MountPoint = plistStr(node, "MountPoint")
	d.Mounted = d.MountPoint != ""
	d.DiskUUID = plistStr(node, "DiskUUID")
	d.UUID = firstNonEmpty(plistStr(node, "VolumeUUID"), d.DiskUUID)
	d.Parent = parent
	if d.Parent == "" {
		d.Parent = plistStr(node, "ParentWholeDisk")
	}
	d.WholeDisk = whole
	d.InfoError = "diskutil info 读不到这台设备的详情"
	return d
}

// ---------- 快照采集 ----------

// diskScope 决定枚举范围。
//
// 为什么要有"只看外置"这个范围（2026-09-19）：
//
//	· **产品口径**：磁盘页只服务"外接硬盘"这件事，系统盘不显示（用户明确要求）；
//	· **性能**：每次 `diskutil info -plist <id>` 是真实硬件查询，机器上 25 台设备要 3.3 秒；
//	  `diskutil list -plist external` 只要 17ms，外置设备通常只有 5 台 ⇒ 一次约 0.5 秒。
//
// 写操作仍然用 all（保护判据必须看得到系统盘，安全不能因为"界面不显示"而放松）。
type diskScope string

const (
	diskScopeExternal diskScope = "external"
	diskScopeAll      diskScope = "all"
)

// collectDiskSnapshot 采集一次真实磁盘状态：list + 每台设备 info + apfs list + fstab。
func collectDiskSnapshot(ctx context.Context, scope diskScope) (*diskSnapshot, error) {
	// `diskutil list -plist external` 只列外接设备（macOS 原生支持，比全量快得多）。
	listArgs := []string{"list", "-plist"}
	if scope == diskScopeExternal {
		listArgs = append(listArgs, "external")
	}
	listOut, listErr, err := runDiskutil(ctx, diskListTimeout, listArgs...)
	if err != nil {
		return nil, fmt.Errorf("读取磁盘列表失败：%w", err)
	}
	_ = listErr
	root, perr := parsePlist([]byte(listOut))
	if perr != nil {
		return nil, fmt.Errorf("解析 diskutil list 输出失败：%w", perr)
	}
	rootMap := plistMap(root)
	if rootMap == nil {
		return nil, errors.New("解析 diskutil list 输出失败：顶层不是字典")
	}

	allIDs := plistStrSlice(rootMap, "AllDisks")
	if len(allIDs) == 0 {
		return nil, errors.New("diskutil list 没有返回任何设备（AllDisks 为空）")
	}

	// 并发跑每台设备的 info：25 台串行要 2~3 秒，并发后约 0.5 秒。
	infos := map[string]diskInfo{}
	var mu sync.Mutex
	sem := make(chan struct{}, diskInfoParallel)
	var wg sync.WaitGroup
	for _, id := range allIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, _, err := runDiskutil(ctx, diskInfoTimeout, "info", "-plist", id)
			var d diskInfo
			if err != nil {
				d = diskInfo{ID: id, Node: "/dev/" + id, InfoError: err.Error()}
			} else {
				v, pe := parsePlist([]byte(out))
				if pe != nil {
					d = diskInfo{ID: id, Node: "/dev/" + id, InfoError: "解析 diskutil info 输出失败：" + pe.Error()}
				} else if m := plistMap(v); m != nil {
					d = infoFromPlist(id, m)
				} else {
					d = diskInfo{ID: id, Node: "/dev/" + id, InfoError: "diskutil info 返回的不是字典"}
				}
			}
			mu.Lock()
			infos[id] = d
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	// 根卷：用它确定 `/` 到底挂在哪台设备上（系统盘保护的第一判据）。
	rootDev := ""
	if out, _, err := runDiskutil(ctx, diskInfoTimeout, "info", "-plist", "/"); err == nil {
		if v, pe := parsePlist([]byte(out)); pe == nil {
			rootDev = plistStr(plistMap(v), "DeviceIdentifier")
		}
	}

	// APFS 容器：一个调用拿到全部容器（容量/卷列表），用于"空容器"判定。
	apfsByRef := map[string]*apfsContainer{}
	if out, _, err := runDiskutil(ctx, diskListTimeout, "apfs", "list", "-plist"); err == nil {
		if v, pe := parsePlist([]byte(out)); pe == nil {
			apfsByRef = parseAPFSContainers(plistMap(v))
		}
	}

	protected := computeProtected(infos, rootDev)

	snap := &diskSnapshot{
		AllIDs:    allIDs,
		Protected: protected,
		RootDev:   rootDev,
		IsRoot:    os.Geteuid() == 0,
	}
	snap.Groups = buildGroups(rootMap, infos, apfsByRef, protected)
	// 把"是不是系统盘 + 为什么"落到每一条设备上，前端据此禁用按钮并写明原因。
	applyProtectionFlags(snap.Groups, protected)

	// fstab：读不到就如实说明（例如非 root 也可能读得到，读不到要能看见原因）。
	if text, _, err := readFstabFile(); err != nil {
		snap.FstabErr = err.Error()
		snap.FstabText = ""
		snap.FstabRead = false
	} else {
		snap.FstabRead = true
		snap.FstabText = text
	}
	entries := parseFstabEntries(snap.FstabText)
	for i := range snap.Groups {
		markAutoMount(&snap.Groups[i].Disk, entries)
		for j := range snap.Groups[i].Partitions {
			markAutoMount(&snap.Groups[i].Partitions[j], entries)
		}
	}

	snap.Notes = append(snap.Notes,
		"本版只提供：查看磁盘信息、挂载、卸载、开机自动挂载、在空 APFS 容器里新建一个卷。",
		"抹盘 / 格式化 / 分区 / 删分区表 本版不提供 —— 请用 macOS 自带的「磁盘工具.app」。",
	)
	if !snap.IsRoot {
		snap.Notes = append(snap.Notes,
			"当前面板进程不是 root：只读枚举照常，挂载/卸载/写 fstab/建卷会失败（正式面板以 root 的 launchd 运行时可执行）。")
	}
	if snap.FstabErr != "" {
		snap.Notes = append(snap.Notes, "读取 "+diskFstabPath+" 失败："+snap.FstabErr)
	}
	return snap, nil
}

// applyProtectionFlags 把保护集合落到每条设备记录上。
func applyProtectionFlags(groups []diskGroup, protected map[string]string) {
	set := func(d *diskInfo) {
		if why, okk := protected[d.ID]; okk {
			d.SystemDisk = true
			d.ProtectReason = why
		}
	}
	for i := range groups {
		set(&groups[i].Disk)
		for j := range groups[i].Partitions {
			set(&groups[i].Partitions[j])
		}
	}
}

func markAutoMount(d *diskInfo, entries []fstabEntry) {
	if d.UUID == "" {
		return
	}
	for _, e := range entries {
		if strings.EqualFold(e.UUID, d.UUID) {
			d.AutoMount = true
			return
		}
	}
}

// buildGroups 把 `diskutil list -plist` 的层级摊成"整盘 → 分区/卷"两层的卡片。
func buildGroups(rootMap map[string]any, infos map[string]diskInfo, apfs map[string]*apfsContainer, protected map[string]string) []diskGroup {
	var groups []diskGroup
	for _, it := range plistArray(rootMap["AllDisksAndPartitions"]) {
		node := plistMap(it)
		if node == nil {
			continue
		}
		id := plistStr(node, "DeviceIdentifier")
		if id == "" {
			continue
		}
		g := diskGroup{}
		if d, okk := infos[id]; okk {
			g.Disk = d
		} else {
			g.Disk = infoFromListNode(id, node, "", true)
		}
		if g.Disk.InfoError == "" && !g.Disk.HasInfo {
			g.Disk.InfoError = "diskutil info 读不到这台设备的详情"
		}
		for _, pit := range plistArray(node["Partitions"]) {
			pn := plistMap(pit)
			if pn == nil {
				continue
			}
			pid := plistStr(pn, "DeviceIdentifier")
			if pid == "" {
				continue
			}
			d, okk := infos[pid]
			if !okk {
				d = infoFromListNode(pid, pn, id, false)
			}
			if d.Parent == "" {
				d.Parent = id
			}
			g.Partitions = append(g.Partitions, d)
		}
		for _, vit := range plistArray(node["APFSVolumes"]) {
			vn := plistMap(vit)
			if vn == nil {
				continue
			}
			vid := plistStr(vn, "DeviceIdentifier")
			if vid == "" {
				continue
			}
			d, okk := infos[vid]
			if !okk {
				d = infoFromListNode(vid, vn, id, false)
			}
			if d.Parent == "" {
				d.Parent = id
			}
			g.Partitions = append(g.Partitions, d)
		}
		// 容器信息（容量/卷数）挂到"整盘"卡片上：物理盘看它自己的 APFS 容器，
		// 合成的容器盘（如 disk4）就直接看自己。
		g.Init = buildInitInfo(id, g, infos, apfs, protected)
		groups = append(groups, g)
	}
	return groups
}

// buildInitInfo 判断"在这块盘上新建 APFS 卷"可不可行，并给出人话原因。
//
// 2026-09 用户放开限制后：**不再要求容器为空** —— 只要盘不是系统盘、
// 且上面有一个能读到的 APFS 容器，就允许新建卷（addVolume 不会动已有卷）。
// 系统盘/容器属于系统盘 → Eligible=false，前端禁用并写明原因。
func buildInitInfo(id string, g diskGroup, infos map[string]diskInfo, apfs map[string]*apfsContainer, protected map[string]string) *diskInitInfo {
	if why, isProtected := protected[id]; isProtected {
		return &diskInitInfo{
			Eligible: false,
			Reason:   "系统盘相关设备（" + why + "），面板拒绝对它做任何写操作",
		}
	}
	// 找这块盘上的 APFS 容器：自己就是容器，或者某个分区的 APFSContainerReference。
	ref := ""
	if d, okk := infos[id]; okk && d.Content == "Apple_APFS_Container" {
		ref = id
	}
	if ref == "" {
		for _, p := range g.Partitions {
			if p.ContainerRef != "" {
				ref = p.ContainerRef
				break
			}
		}
	}
	if ref == "" {
		return &diskInitInfo{
			Eligible: false,
			Reason:   "这块盘上没有 APFS 容器。要新建卷请先用「格式化/抹盘」把它做成 APFS（或改用「磁盘工具.app」）",
		}
	}
	c := apfs[ref]
	if c == nil {
		return &diskInitInfo{
			Eligible:     false,
			ContainerRef: ref,
			Reason:       "读不到 APFS 容器 " + ref + " 的信息（diskutil apfs list 没有返回它），为保证安全拒绝执行",
		}
	}
	info := &diskInitInfo{
		ContainerRef:   ref,
		VolumeCount:    len(c.Volumes),
		ContainerFree:  c.CapacityFree,
		ContainerSize:  c.CapacityCeiling,
		BackendCommand: "diskutil apfs addVolume " + ref + " APFS <卷名>",
	}
	for _, v := range c.Volumes {
		info.VolumeNames = append(info.VolumeNames, v.Name)
	}
	if why, isProtected := protected[ref]; isProtected {
		info.Eligible = false
		info.Reason = "容器的物理存储属于系统盘（" + why + "）"
		return info
	}
	info.Eligible = true
	if len(c.Volumes) == 0 {
		info.Reason = "空容器，可新建一个卷（不会删除分区表）"
	} else {
		info.Reason = fmt.Sprintf("容器 %s 里已有 %d 个卷（%s）；仍可新建卷（addVolume 不会动已有卷）",
			ref, len(c.Volumes), strings.Join(info.VolumeNames, "、"))
	}
	return info
}

// computeProtected 计算"绝不允许写"的设备集合，沿父子/APFS 物理存储传播到不动点。
//
// 判据（全部来自 diskutil 的真实输出）：
//   - Internal / OSInternalMedia 为真 → 内置盘；
//   - 挂载点 / 或 /System/Volumes/* → macOS 系统卷；
//   - `/` 所在卷；
//   - 以上设备的子分区、以及以它们为物理存储的 APFS 容器（及其卷）。
func computeProtected(infos map[string]diskInfo, rootDev string) map[string]string {
	protected := map[string]string{}
	mark := func(id, why string) {
		if id == "" {
			return
		}
		if _, okk := protected[id]; !okk {
			protected[id] = why
		}
	}
	for id, d := range infos {
		if d.Internal {
			mark(id, "内置磁盘（diskutil 报 Internal）")
		}
		if d.MountPoint == "/" {
			mark(id, "根文件系统 / 所在卷")
		} else if strings.HasPrefix(d.MountPoint, "/System/Volumes/") {
			mark(id, "macOS 系统卷 "+d.VolumeName)
		}
	}
	mark(rootDev, "根文件系统 / 所在卷")
	for changed := true; changed; {
		changed = false
		for id, d := range infos {
			_, idProtected := protected[id]
			if idProtected {
				for cid, cd := range infos {
					if cd.Parent == id {
						if _, okk := protected[cid]; !okk {
							protected[cid] = "属于系统盘 " + id
							changed = true
						}
					}
				}
			}
			for _, ps := range d.PhysicalStores {
				if _, okk := protected[ps]; okk {
					if _, okk2 := protected[id]; !okk2 {
						protected[id] = "APFS 容器，物理存储 " + ps + " 属于系统盘"
						changed = true
					}
				}
			}
			if d.Parent != "" {
				if _, okk := protected[d.Parent]; okk {
					if _, okk2 := protected[id]; !okk2 {
						protected[id] = "属于系统盘 " + d.Parent
						changed = true
					}
				}
			}
		}
	}
	return protected
}

// ---------- APFS 容器 ----------

type apfsVolume struct {
	ID            string
	Name          string
	UUID          string
	Encrypted     bool
	Locked        bool
	CapacityInUse int64
}

type apfsContainer struct {
	Ref             string
	CapacityCeiling int64
	CapacityFree    int64
	Volumes         []apfsVolume
}

func parseAPFSContainers(m map[string]any) map[string]*apfsContainer {
	out := map[string]*apfsContainer{}
	for _, it := range plistArray(m["Containers"]) {
		cm := plistMap(it)
		if cm == nil {
			continue
		}
		ref := plistStr(cm, "ContainerReference")
		if ref == "" {
			continue
		}
		c := &apfsContainer{
			Ref:             ref,
			CapacityCeiling: plistInt(cm, "CapacityCeiling"),
			CapacityFree:    plistInt(cm, "CapacityFree"),
		}
		for _, vit := range plistArray(cm["Volumes"]) {
			vm := plistMap(vit)
			if vm == nil {
				continue
			}
			c.Volumes = append(c.Volumes, apfsVolume{
				ID:            plistStr(vm, "DeviceIdentifier"),
				Name:          plistStr(vm, "Name"),
				UUID:          plistStr(vm, "APFSVolumeUUID"),
				Encrypted:     plistBool(vm, "Encryption") || plistBool(vm, "FileVault"),
				Locked:        plistBool(vm, "Locked"),
				CapacityInUse: plistInt(vm, "CapacityInUse"),
			})
		}
		out[ref] = c
	}
	return out
}

// ---------- 设备 id 校验 / 白名单 ----------

var diskIDRe = regexp.MustCompile(`^disk[0-9]+(s[0-9]+)*$`)

func validDiskID(id string) bool { return diskIDRe.MatchString(id) }

// diskLookup 从快照里按 id 找设备。返回的 (code, msg) 非空表示应直接拒绝。
func (snap *diskSnapshot) diskLookup(id string) (diskInfo, int, string) {
	if !validDiskID(id) {
		return diskInfo{}, http.StatusBadRequest,
			"设备标识不合法：" + id + "（应形如 disk9 / disk9s2 / disk4s1）"
	}
	found := false
	for _, cand := range snap.AllIDs {
		if cand == id {
			found = true
			break
		}
	}
	if !found {
		return diskInfo{}, http.StatusNotFound,
			"找不到设备 " + id + "：它不在 diskutil 当前列表里（可能已拔出，或被重新编号）"
	}
	for _, g := range snap.Groups {
		if g.Disk.ID == id {
			return g.Disk, 0, ""
		}
		for _, p := range g.Partitions {
			if p.ID == id {
				return p, 0, ""
			}
		}
	}
	return diskInfo{}, http.StatusInternalServerError, "设备 " + id + " 在列表里但读不到详情"
}

// guardWrite 是"拒绝对系统盘写"的唯一入口。返回空串表示放行。
func (snap *diskSnapshot) guardWrite(id string) string {
	if why, okk := snap.Protected[id]; okk {
		return "拒绝操作 " + id + "：它是系统盘相关设备（" + why + "）。面板不会对系统盘做挂载/卸载/自动挂载/抹盘/格式化/建卷/删卷/重命名。"
	}
	return ""
}

// ---------- fstab ----------

const fstabMarkerPrefix = "# zizpanel auto-mount "

type fstabEntry struct {
	UUID       string `json:"uuid"`
	MountPoint string `json:"mount_point"`
	VFSType    string `json:"vfstype"`
	Options    string `json:"options"`
	Raw        string `json:"raw"`
}

func readFstabFile() (string, bool, error) {
	b, err := os.ReadFile(diskFstabPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(b), true, nil
}

func parseFstabEntries(content string) []fstabEntry {
	out := []fstabEntry{}
	for _, ln := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		f := strings.Fields(trimmed)
		if len(f) < 4 || !strings.HasPrefix(strings.ToUpper(f[0]), "UUID=") {
			continue
		}
		out = append(out, fstabEntry{
			UUID:       strings.TrimSpace(f[0][len("UUID="):]),
			MountPoint: f[1],
			VFSType:    f[2],
			Options:    f[3],
			Raw:        trimmed,
		})
	}
	return out
}

func fstabLineUUID(line string) string {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) == 0 || strings.HasPrefix(f[0], "#") {
		return ""
	}
	if !strings.HasPrefix(strings.ToUpper(f[0]), "UUID=") {
		return ""
	}
	return strings.TrimSpace(f[0][len("UUID="):])
}

// fstabEscape 按 fstab(5) 的规矩转义空格/制表符（挂载点里的空格要写成 \040）。
func fstabEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, " ", `\040`)
	s = strings.ReplaceAll(s, "\t", `\011`)
	return s
}

// applyFstabEntry 在 content 里"加入"或"删除"某个卷的自动挂载条目。
// 返回新内容；其它行一字不动。删除时连它上面的 zizpanel 标记行一起删。
func applyFstabEntry(content string, enable bool, spec fstabSpec) string {
	lines := []string{}
	if strings.TrimSpace(content) != "" {
		lines = strings.Split(strings.TrimRight(content, "\n"), "\n")
	}
	out := make([]string, 0, len(lines)+2)
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if strings.EqualFold(strings.TrimSpace(ln), fstabMarkerPrefix+spec.UUID) {
			if i+1 < len(lines) && strings.EqualFold(fstabLineUUID(lines[i+1]), spec.UUID) {
				i++
			}
			continue
		}
		if strings.EqualFold(fstabLineUUID(ln), spec.UUID) {
			continue
		}
		out = append(out, ln)
	}
	if enable {
		entry := fmt.Sprintf("UUID=%s %s %s rw 0 2", spec.UUID, fstabEscape(spec.MountPoint), spec.VFSType)
		out = append(out, fstabMarkerPrefix+spec.UUID, entry)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

type fstabSpec struct {
	UUID       string
	MountPoint string
	VFSType    string
}

// writeFileAtomic：同目录临时文件 + fsync + rename，避免半截文件让开机挂载失败。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".zizpanel-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// fstabWriteHint 把"写不进去"翻译成用户能行动的一句话。
func fstabWriteHint(err error) string {
	if err == nil {
		return ""
	}
	if os.IsPermission(err) && os.Geteuid() != 0 {
		return "（当前面板进程不是 root，无法修改 " + diskFstabPath +
			"；正式面板以 root 的 launchd 运行时可以写入）"
	}
	return "（写入 " + diskFstabPath + " 失败：" + err.Error() + "）"
}

// ---------- 卷名校验 ----------

// validateVolumeName 保守白名单：1~32 个字符，只允许字母、数字、中文、空格、-_.
// 不放行 / : * ? " < > | \ 等会破坏 fstab/路径的字符，也不放行控制字符。
func validateVolumeName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("卷名不能为空")
	}
	if n := len([]rune(name)); n > 32 {
		return fmt.Errorf("卷名过长（%d 个字符，上限 32）", n)
	}
	for _, r := range name {
		if r == ' ' || r == '-' || r == '_' || r == '.' {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		if unicode.Is(unicode.Han, r) {
			continue
		}
		return fmt.Errorf("卷名里有不允许的字符 %q（只允许中英文、数字、空格与 - _ .）", string(r))
	}
	if strings.Trim(name, ".") == "" {
		return errors.New("卷名不能只由点组成")
	}
	return nil
}

// defaultMountPoint 给一个还没挂载的卷推导默认挂载点（与 macOS 的 /Volumes/<名字> 一致）。
func defaultMountPoint(volumeName, id string) string {
	name := strings.TrimSpace(volumeName)
	if name == "" {
		name = id
	}
	name = strings.ReplaceAll(name, "/", "-")
	return "/Volumes/" + name
}

// ---------- 自定义挂载点（把卷挂到 <安装根>/mnt 下的 API 能力） ----------
//
// 🚨 这个功能**不是** macOS 隐私保护（TCC）的解法 —— 2026-09-19 实测证伪：
//   · `diskutil mount -mountPoint /opt/zizpanel/mnt/ZPMirror disk4s1` → 挂载成功；
//   · 面板（root LaunchDaemon）读那个新路径 → 仍然 operation not permitted；
//   · 系统日志：kTCCServiceSystemPolicyRemovableVolumes + 「Background Session …
//     record_denial」—— 这条保护按**卷**判定，与挂载路径无关，后台进程连询问
//     窗口都不会弹。所以"挂到 /Volumes 之外"绕不过去（见 api_files.go 顶部）。
// 因此前端的「挂载到自定义挂载点…」入口已删除；这里只保留 API 能力本身
// （把卷挂到指定目录是有效的磁盘操作），调用方要清楚它不解决 TCC。

// diskMountReq 是挂载动作的**可选**请求体。MountPoint 为空时行为不变（挂到 /Volumes）。
type diskMountReq struct {
	MountPoint string `json:"mount_point"`
}

// decodeOptional 解析可选 JSON body：空 body 不算错误（老前端/脚本不带 body）。
func decodeOptional(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return decode(r, v)
}

// customMountBase 返回自定义挂载点唯一允许的前缀：<面板安装根>/mnt。
//
// 为什么必须限制前缀：面板以 root 运行，若"用户给什么绝对路径就挂什么"，
// 等于允许把任意卷挂到 /etc、/usr 之上 —— 是提权级别的事故。
func (s *Server) customMountBase() string {
	root := config.DefaultRoot
	if s.Cfg != nil && s.Cfg.BinDir != "" {
		if parent := filepath.Dir(filepath.Clean(s.Cfg.BinDir)); parent != "/" && parent != "." && filepath.IsAbs(parent) {
			root = parent
		}
	}
	return filepath.Join(root, "mnt")
}

// resolveCustomMountPoint 校验自定义挂载点并**建好目录**（不存在则先建）。
//
// 只允许 <安装根>/mnt/<单个目录名>，且解析软链接后仍在这个前缀内；
// 落在 /Volumes 下的挂载点直接拒绝：那不叫「自定义」位置（默认挂载点就是它）。
// 注意这**不是** TCC 的解法 —— 实测证明换挂载点后读卷仍被拒（见本节顶部结论）。
func (s *Server) resolveCustomMountPoint(raw string) (string, error) {
	p := filepath.Clean(strings.TrimSpace(raw))
	if !filepath.IsAbs(p) {
		return "", errors.New("自定义挂载点必须是绝对路径")
	}
	base := s.customMountBase()
	if p == base || !strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("自定义挂载点必须是 %s 下的子目录（例如 %s）", base, filepath.Join(base, "mirror"))
	}
	leaf := filepath.Base(p)
	if leaf == "" || leaf == "." || leaf == ".." || strings.ContainsAny(leaf, `/\`) {
		return "", errors.New("自定义挂载点的目录名不合法")
	}
	inVolumes := func(x string) bool {
		return x == "/Volumes" || strings.HasPrefix(x, "/Volumes"+string(os.PathSeparator))
	}
	if inVolumes(p) {
		return "", errors.New("自定义挂载点不能落在 /Volumes 下（那不叫自定义挂载点）")
	}
	// 目录不存在则先建：`diskutil mount -mountPoint` 要求挂载点目录已存在。
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("创建挂载点目录失败：%w", err)
	}
	// 解析软链接后再确认没越界（面板是 root，这里不能只做字符串前缀检查）。
	realBase := base
	if r, err := filepath.EvalSymlinks(base); err == nil {
		realBase = r
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("解析挂载点失败：%w", err)
	}
	if real != realBase && !strings.HasPrefix(real, realBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("自定义挂载点解析后越出 %s，拒绝", base)
	}
	if inVolumes(real) {
		return "", errors.New("自定义挂载点不能落在 /Volumes 下（那不叫自定义挂载点）")
	}
	return real, nil
}

// ---------- 视图 / 响应 ----------

type diskListView struct {
	List          []diskGroup     `json:"list"`
	Protected     []diskProtected `json:"protected"`
	Filesystems   []diskFSView    `json:"filesystems"`
	RootDevice    string          `json:"root_device"`
	IsRoot        bool            `json:"is_root"`
	FstabPath     string          `json:"fstab_path"`
	FstabEntries  []fstabEntry    `json:"fstab_entries"`
	FstabReadable bool            `json:"fstab_readable"`
	FstabError    string          `json:"fstab_error,omitempty"`
	Notes         []string        `json:"notes"`
	// CustomMountBase 是「挂载到自定义挂载点」允许的前缀（<安装根>/mnt），
	// 供 API 调用方预填路径。放在响应里而不是写死 /opt/zizpanel，非默认安装才正确。
	CustomMountBase string `json:"custom_mount_base"`
	// PanelBinary 是面板自己的可执行文件路径。给前端显示"去系统设置里给谁授权"用：
	// 外接盘被 macOS 隐私保护拒绝时，只有人工给这个二进制授权才能放行
	// （换挂载点没用 —— 见 api_files.go 顶部 2026-09-19 的实测结论）。
	PanelBinary string `json:"panel_binary"`
	// VolumeAuth 是「申请授权」按钮的同步预检结论（不读盘：有没有外接卷、有没有人在屏幕前）。
	VolumeAuth  diskVolumeAuthView `json:"volume_auth"`
	CollectedAt string             `json:"collected_at"`
}

func (s *diskSnapshot) view() diskListView {
	v := diskListView{
		List:          s.Groups,
		Protected:     []diskProtected{},
		Filesystems:   diskFilesystemViews(),
		RootDevice:    s.RootDev,
		IsRoot:        s.IsRoot,
		FstabPath:     diskFstabPath,
		FstabEntries:  parseFstabEntries(s.FstabText),
		FstabReadable: s.FstabRead,
		FstabError:    s.FstabErr,
		Notes:         s.Notes,
		CollectedAt:   time.Now().Format(time.RFC3339),
	}
	if v.List == nil {
		v.List = []diskGroup{}
	}
	ids := make([]string, 0, len(s.Protected))
	for id := range s.Protected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v.Protected = append(v.Protected, diskProtected{ID: id, Reason: s.Protected[id]})
	}
	return v
}

// 磁盘快照的短缓存。
//
// 为什么要缓存：一次真实采集要跑 diskutil（外置范围约 0.5 秒，全量 3 秒+），
// 而磁盘状态**秒级不会变**，用户来回切页面时没必要每次重跑硬件查询。
// 为什么只有 15 秒：面板的纪律是"看真实状态" —— 缓存必须短，且
// **任何写操作（挂载/卸载/抹盘/建卷/删卷/重命名）成功后立即失效**，
// 前端"刷新"按钮传 `?fresh=1` 也会绕过缓存。
const diskSnapshotTTL = 15 * time.Second

var diskSnapshotCache struct {
	mu    sync.Mutex
	at    time.Time
	scope diskScope
	snap  *diskSnapshot
}

// invalidateDiskSnapshotCache 让下一次读取重新采集（写操作后必须调用）。
func invalidateDiskSnapshotCache() {
	diskSnapshotCache.mu.Lock()
	diskSnapshotCache.snap = nil
	diskSnapshotCache.mu.Unlock()
}

func collectDiskSnapshotCached(ctx context.Context, scope diskScope, fresh bool) (*diskSnapshot, error) {
	if !fresh {
		diskSnapshotCache.mu.Lock()
		if diskSnapshotCache.snap != nil && diskSnapshotCache.scope == scope &&
			time.Since(diskSnapshotCache.at) < diskSnapshotTTL {
			snap := diskSnapshotCache.snap
			diskSnapshotCache.mu.Unlock()
			return snap, nil
		}
		diskSnapshotCache.mu.Unlock()
	}
	snap, err := collectDiskSnapshot(ctx, scope)
	if err != nil {
		return nil, err
	}
	diskSnapshotCache.mu.Lock()
	diskSnapshotCache.at = time.Now()
	diskSnapshotCache.scope = scope
	diskSnapshotCache.snap = snap
	diskSnapshotCache.mu.Unlock()
	return snap, nil
}

// ---------- HTTP handlers ----------

// handleDiskList 枚举磁盘与分区（只读）。见 GET /api/v1/system/disks。
func (s *Server) handleDiskList(w http.ResponseWriter, r *http.Request) {
	// 默认只看外置盘（用户口径）；`?scope=all` 给排障/将来用。
	scope := diskScopeExternal
	if strings.EqualFold(r.URL.Query().Get("scope"), "all") {
		scope = diskScopeAll
	}
	snap, err := collectDiskSnapshotCached(r.Context(), scope, r.URL.Query().Get("fresh") == "1")
	if err != nil {
		s.audit(r, "disk_list", "", err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "读取磁盘列表失败："+err.Error())
		return
	}
	s.audit(r, "disk_list", "", fmt.Sprintf("%d 组设备", len(snap.Groups)), true, "")
	v := snap.view()
	v.CustomMountBase = s.customMountBase()
	// 与文件管理里的 TCC 指引用**同一个**来源（见 api_files.go 的 panelBinaryForGuide）：
	// 两处各算一次是"同一事实两个来源"，非默认安装时必然写出两个不同路径。
	v.PanelBinary = panelBinaryForGuide
	// 按钮状态每次都重新采（不进 15 秒快照缓存）：人可能刚走开/刚插上盘。
	v.VolumeAuth = diskVolumeAuthState()
	ok(w, v)
}

func (s *Server) handleDiskMount(w http.ResponseWriter, r *http.Request) {
	// body 可选：mount_point 非空时走「挂载到自定义挂载点」（见 diskMountReq）。
	var req diskMountReq
	if err := decodeOptional(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.diskMountAction(w, r, "mount", req.MountPoint)
}

func (s *Server) handleDiskUnmount(w http.ResponseWriter, r *http.Request) {
	s.diskMountAction(w, r, "unmount", "")
}

type diskActionResult struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Verified    bool   `json:"verified"`
	Mounted     bool   `json:"mounted"`
	MountPoint  string `json:"mount_point"`
	Command     string `json:"command"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	Message     string `json:"message"`
	VerifyError string `json:"verify_error,omitempty"`
	RebootNote  string `json:"reboot_note,omitempty"`
}

// diskMountAction 执行挂载/卸载，并**回读真实状态**确认结果。
//
// customMountPoint 非空（仅 mount）时改用 `diskutil mount -mountPoint <dir> <id>`：
// 把外接盘挂到 <安装根>/mnt 下的指定目录（见 resolveCustomMountPoint）。
// 这不是 TCC 的解法：实测证明换挂载点后读卷仍被拒（见本节顶部结论）。
func (s *Server) diskMountAction(w http.ResponseWriter, r *http.Request, action, customMountPoint string) {
	id := r.PathValue("id")
	snap, err := collectDiskSnapshot(r.Context(), diskScopeAll)
	if err != nil {
		s.audit(r, "disk_"+action, id, "读取磁盘状态失败："+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "读取磁盘状态失败："+err.Error())
		return
	}
	info, code, msg := snap.diskLookup(id)
	if msg != "" {
		s.audit(r, "disk_"+action, id, msg, false, "")
		fail(w, code, msg)
		return
	}
	if why := snap.guardWrite(id); why != "" {
		s.audit(r, "disk_"+action, id, why, false, "")
		fail(w, http.StatusForbidden, why)
		return
	}
	if action == "mount" && info.Locked {
		msg := "设备 " + id + " 当前处于锁定状态（加密卷未解锁）。解锁需要用户口令/钥匙串，" +
			"无头环境下面板无法自动完成、也不提供会弹窗授权的路径 —— 请人工解锁后再挂载。"
		s.audit(r, "disk_"+action, id, msg, false, "")
		fail(w, http.StatusConflict, msg)
		return
	}
	if action == "mount" && !info.HasFilesystem && info.Content != "Apple_APFS" && info.Content != "Apple_APFS_Container" {
		// EFI 之类有时 info 里没有 FilesystemName，但不该直接拦住 —— 让 diskutil
		// 给真实错误。只有明显没有文件系统的整盘才提前拦。
		if info.WholeDisk {
			msg := "设备 " + id + " 是整盘（没有文件系统），不能直接挂载。请选择它的某个分区/卷。"
			s.audit(r, "disk_"+action, id, msg, false, "")
			fail(w, http.StatusBadRequest, msg)
			return
		}
	}

	// 自定义挂载点（仅挂载）：校验 + 建目录都在这里做，命令标签与实际参数一致。
	args := []string{action, id}
	command := "diskutil " + action + " " + id
	if action == "mount" && strings.TrimSpace(customMountPoint) != "" {
		mp, mpErr := s.resolveCustomMountPoint(customMountPoint)
		if mpErr != nil {
			s.audit(r, "disk_mount", id, "自定义挂载点不合法："+mpErr.Error(), false, "")
			fail(w, http.StatusBadRequest, mpErr.Error())
			return
		}
		args = []string{"mount", "-mountPoint", mp, id}
		command = "diskutil mount -mountPoint " + mp + " " + id
	}

	out, errb, runErr := runDiskutil(r.Context(), diskMountTimeout, args...)
	res := diskActionResult{
		ID:      id,
		Action:  action,
		Command: command,
		Stdout:  strings.TrimSpace(out),
		Stderr:  strings.TrimSpace(errb),
	}

	// 不管退出码是什么，都回读一次真实状态 —— 退出码不是结论。
	after, verr := queryDiskInfo(r.Context(), id)
	if verr == nil {
		res.Mounted = after.Mounted
		res.MountPoint = after.MountPoint
		res.Verified = true
	} else {
		res.VerifyError = "回读 diskutil info 失败：" + verr.Error()
	}

	if runErr != nil {
		// 超时要特别说清"设备可能处于中间态，请复查"，并附上回读到的真实状态。
		base := fmt.Sprintf("diskutil %s %s 失败：%s", action, id, runErr.Error())
		if isTimeout(runErr) {
			base = fmt.Sprintf("diskutil %s %s 超时（%s）：设备状态可能处于中间态。"+
				"回读结果：%s", action, id, diskMountTimeout, describeMountState(res))
		} else if res.Verified {
			base += "。回读结果：" + describeMountState(res)
		}
		res.Message = base
		s.audit(r, "disk_"+action, id, base, false, "")
		fail(w, http.StatusInternalServerError, base)
		return
	}

	wantMounted := action == "mount"
	if !res.Verified {
		res.Message = "命令已执行，但回读状态失败，**未复核**结果。"
		s.audit(r, "disk_"+action, id, res.Message, false, "")
		fail(w, http.StatusInternalServerError, res.Message)
		return
	}
	if res.Mounted != wantMounted {
		res.Message = fmt.Sprintf("命令退出码为 0，但回读不一致：期望 mounted=%v，实际 mounted=%v（挂载点 %q）。以真实状态为准。",
			wantMounted, res.Mounted, res.MountPoint)
		s.audit(r, "disk_"+action, id, res.Message, false, "")
		fail(w, http.StatusInternalServerError, res.Message)
		return
	}
	if wantMounted {
		res.Message = "已挂载到 " + res.MountPoint
	} else {
		res.Message = "已卸载（回读挂载点为空）"
	}
	s.audit(r, "disk_"+action, id, res.Message, true, "")
	// 挂载状态变了：让磁盘快照缓存立刻失效，后续读取必须重新采集（看真实状态）。
	invalidateDiskSnapshotCache()
	ok(w, res)
}

func describeMountState(res diskActionResult) string {
	if !res.Verified {
		return "回读失败（" + res.VerifyError + "）"
	}
	if res.Mounted {
		return "当前已挂载于 " + res.MountPoint
	}
	return "当前未挂载"
}

func isTimeout(err error) bool { return errors.Is(err, errDiskTimeout) }

// queryDiskInfo 单独回读一台设备的 info。
func queryDiskInfo(ctx context.Context, id string) (diskInfo, error) {
	out, _, err := runDiskutil(ctx, diskInfoTimeout, "info", "-plist", id)
	if err != nil {
		return diskInfo{ID: id}, err
	}
	v, perr := parsePlist([]byte(out))
	if perr != nil {
		return diskInfo{ID: id}, perr
	}
	m := plistMap(v)
	if m == nil {
		return diskInfo{ID: id}, errors.New("diskutil info 返回的不是字典")
	}
	return infoFromPlist(id, m), nil
}

// ---------- 开机自动挂载 ----------

type diskAutoMountReq struct {
	Enabled *bool `json:"enabled"`
}

type diskAutoMountResult struct {
	ID             string `json:"id"`
	UUID           string `json:"uuid"`
	Enabled        bool   `json:"enabled"`
	FstabPath      string `json:"fstab_path"`
	Entry          string `json:"entry"`
	Changed        bool   `json:"changed"`
	FstabWritten   bool   `json:"fstab_written"`
	FstabContent   string `json:"fstab_content"`
	BackupPath     string `json:"backup_path"`
	MountPoint     string `json:"mount_point"`
	MountAttempted bool   `json:"mount_attempted"`
	Mounted        bool   `json:"mounted"`
	MountPointNow  string `json:"mount_point_now"`
	MountError     string `json:"mount_error,omitempty"`
	RebootRequired bool   `json:"reboot_required"`
	RebootNote     string `json:"reboot_note"`
	Message        string `json:"message"`
}

// handleDiskAutoMount 写/删 /etc/fstab 里的自动挂载条目（见 fstab(5)）。
//
// 已经查证并实测的口径：
//   - 第一字段用 `UUID=<卷 UUID>`（man 明确要求 APFS 不能用块设备名）；
//   - 第三字段是文件系统类型（apfs / hfs / msdos）；
//   - 第四字段 `rw`（含 `auto` 的默认语义；`noauto` 才是"不自动挂载"）；
//   - 末尾 `0 2`（dump 0、fsck 顺序 2）。
//
// 写文件用"原子写 + 先备份"（/etc/fstab.zizpanel.bak）。
func (s *Server) handleDiskAutoMount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req diskAutoMountReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Enabled == nil {
		fail(w, http.StatusBadRequest, `缺少 enabled 字段（true = 写入开机自动挂载，false = 删除）`)
		return
	}
	enable := *req.Enabled

	snap, err := collectDiskSnapshot(r.Context(), diskScopeAll)
	if err != nil {
		s.audit(r, "disk_auto_mount", id, "读取磁盘状态失败："+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, "读取磁盘状态失败："+err.Error())
		return
	}
	info, code, msg := snap.diskLookup(id)
	if msg != "" {
		s.audit(r, "disk_auto_mount", id, msg, false, "")
		fail(w, code, msg)
		return
	}
	if why := snap.guardWrite(id); why != "" {
		s.audit(r, "disk_auto_mount", id, why, false, "")
		fail(w, http.StatusForbidden, why)
		return
	}
	if info.WholeDisk {
		fail(w, http.StatusBadRequest, "设备 "+id+" 是整盘，不能设置开机自动挂载；请选择它的某个卷。")
		return
	}
	if info.UUID == "" {
		fail(w, http.StatusBadRequest, "设备 "+id+" 没有卷 UUID（diskutil 没有返回 VolumeUUID/DiskUUID），无法用 UUID 稳定引用它。")
		return
	}
	if !info.HasFilesystem {
		fail(w, http.StatusBadRequest, "设备 "+id+" 没有可挂载的文件系统（例如 APFS 容器/无文件系统分区），无法设置开机自动挂载。")
		return
	}
	if enable && info.Encrypted {
		m := "设备 " + id + " 是加密卷：开机时自动挂载需要在启动阶段人工输入口令/解锁（无头环境下不可用），" +
			"面板不提供也不会弹窗要授权。请先人工确认解锁方式，本版拒绝为它写自动挂载条目。"
		s.audit(r, "disk_auto_mount", id, m, false, "")
		fail(w, http.StatusConflict, m)
		return
	}

	mountPoint := info.MountPoint
	if mountPoint == "" {
		mountPoint = defaultMountPoint(info.VolumeName, info.ID)
	}
	fsType := info.FSType
	if fsType == "" {
		fsType = "apfs"
	}
	spec := fstabSpec{UUID: info.UUID, MountPoint: mountPoint, VFSType: fsType}

	before, existed, rerr := readFstabFile()
	if rerr != nil {
		msg := "读取 " + diskFstabPath + " 失败：" + rerr.Error() + fstabWriteHint(rerr)
		s.audit(r, "disk_auto_mount", id, msg, false, "")
		fail(w, http.StatusInternalServerError, msg)
		return
	}
	after := applyFstabEntry(before, enable, spec)
	entryLine := ""
	if enable {
		entryLine = fmt.Sprintf("UUID=%s %s %s rw 0 2", spec.UUID, fstabEscape(spec.MountPoint), spec.VFSType)
	}

	res := diskAutoMountResult{
		ID:             id,
		UUID:           spec.UUID,
		Enabled:        enable,
		FstabPath:      diskFstabPath,
		Entry:          entryLine,
		Changed:        after != before,
		MountPoint:     mountPoint,
		RebootRequired: true,
		RebootNote: "「开机自动挂载」是否真正生效，必须重启后才能最终确认；" +
			"本面板不做重启，也不会把这一步标成已验证。",
	}

	if !res.Changed {
		res.FstabWritten = false
		res.FstabContent = after
		res.Mounted = info.Mounted
		res.MountPointNow = info.MountPoint
		if enable {
			res.Message = "条目已存在，未做改动。"
		} else {
			res.Message = "条目本来就不存在，未做改动。"
		}
		s.audit(r, "disk_auto_mount", id, res.Message+" ("+spec.UUID+")", true, "")
		ok(w, res)
		return
	}

	// 先备份原文件，再原子写入新内容。
	if existed {
		if err := writeFileAtomic(diskFstabPath+".zizpanel.bak", []byte(before), 0o644); err != nil {
			msg := "备份 " + diskFstabPath + " 失败，未做任何修改：" + err.Error() + fstabWriteHint(err)
			s.audit(r, "disk_auto_mount", id, msg, false, "")
			fail(w, http.StatusInternalServerError, msg)
			return
		}
		res.BackupPath = diskFstabPath + ".zizpanel.bak"
	}
	if err := writeFileAtomic(diskFstabPath, []byte(after), 0o644); err != nil {
		msg := "写入 " + diskFstabPath + " 失败：" + err.Error() + fstabWriteHint(err)
		s.audit(r, "disk_auto_mount", id, msg, false, "")
		fail(w, http.StatusInternalServerError, msg)
		return
	}

	// 回读文件确认写对了（不看返回值，看磁盘上的内容）。
	written, _, verr := readFstabFile()
	if verr != nil {
		msg := "写入后回读 " + diskFstabPath + " 失败：" + verr.Error()
		s.audit(r, "disk_auto_mount", id, msg, false, "")
		fail(w, http.StatusInternalServerError, msg)
		return
	}
	entries := parseFstabEntries(written)
	has := false
	for _, e := range entries {
		if strings.EqualFold(e.UUID, spec.UUID) {
			has = true
			break
		}
	}
	if has != enable {
		msg := fmt.Sprintf("文件已写入，但回读校验不通过：期望条目存在=%v，实际=%v", enable, has)
		s.audit(r, "disk_auto_mount", id, msg, false, "")
		fail(w, http.StatusInternalServerError, msg)
		return
	}
	res.FstabWritten = true
	res.FstabContent = written

	// 立即挂载（仅在打开且当前未挂载时）：让用户现在就能用上，而不是等重启。
	if enable && !info.Mounted {
		res.MountAttempted = true
		if _, _, err := runDiskutil(r.Context(), diskMountTimeout, "mount", id); err != nil {
			res.MountError = err.Error()
		}
		if a, err := queryDiskInfo(r.Context(), id); err == nil {
			res.Mounted = a.Mounted
			res.MountPointNow = a.MountPoint
		}
	} else {
		res.Mounted = info.Mounted
		res.MountPointNow = info.MountPoint
	}

	if enable {
		res.Message = "已写入开机自动挂载条目（" + diskFstabPath + "），并已尝试立即挂载"
		if res.MountAttempted {
			if res.Mounted {
				res.Message += "，当前挂载于 " + res.MountPointNow
			} else if res.MountError != "" {
				res.Message += "；立即挂载失败：" + res.MountError
			}
		}
	} else {
		res.Message = "已删除开机自动挂载条目（" + diskFstabPath + "）"
	}
	s.audit(r, "disk_auto_mount", id, res.Message+" ("+spec.UUID+")", true, "")
	ok(w, res)
}
