package web

// api_disks_test.go —— 磁盘工具的后端门禁。
//
// 这里**绝不碰真机磁盘**：全部走 diskExec / diskFstabPath 两个包级测试钩子，
// 用一个内存里的假 `diskutil` 世界回答 list/info/mount/unmount/apfs。
//
// 覆盖的重点是"安全"而不是"功能好看"：
//   · 系统盘（内置/`/`所在卷/它们的父子链）挂载/卸载/自动挂载/建卷一律 403；
//   · 设备 id 必须来自 diskutil list 的白名单（不存在的 → 404，格式非法 → 400）；
//   · 挂载/卸载不看退出码，只看回读的挂载点（回读不一致 → 500）；
//   · 超时 → 500，且必须回读真实状态并把结论带上；
//   · 只在空 APFS 容器里建卷：非空/系统盘/名字非法/没手打确认 → 4xx；
//   · 源码里不得出现任何会弹 GUI 授权窗的调用（无头机器的硬要求）。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// diskForbiddenGUIs 是"会在无头机器上永久挂起"的调用黑名单。
// 放在测试文件里而不是被测源码里 —— 否则黑名单字面量本身就会被下面的 grep 命中。
var diskForbiddenGUIs = []string{"osascript", "administrator privileges", "AuthorizationExecuteWithPrivileges"}

// ============================================================================
//  假 diskutil 世界
// ============================================================================

type fakeVol struct {
	id   string
	name string
	uuid string
}

type eraseState struct {
	name string
	fs   string
	part string // eraseDisk 后新生成的分区（整盘）
}

type fakeWorld struct {
	mu             sync.Mutex
	containerVols  map[string][]fakeVol  // 容器 ref → 卷
	mounted        map[string]string     // 设备 id → 挂载点（覆盖静态信息）
	calls          []string              // 记录所有被执行的 diskutil 子命令
	failMount      bool                  // mount 返回非 0（状态不变）
	mountNoVerify  bool                  // mount 返回 0，但回读仍显示未挂载
	timeoutUnmount bool                  // unmount 返回超时哨兵
	addVolumeFails bool                  // apfs addVolume 返回失败
	timeoutErase   bool                  // erase* 返回超时哨兵
	erasedDisk     map[string]eraseState // 整盘 eraseDisk 的结果
	erasedVol      map[string]eraseState // 分区/卷 eraseVolume 的结果
	renamed        map[string]string     // 卷重命名
	deleted        map[string]bool       // 已删除的 APFS 卷
}

func newFakeWorld() *fakeWorld {
	return &fakeWorld{
		containerVols: map[string][]fakeVol{
			"disk1": {{id: "disk1s1", name: "Macintosh HD", uuid: "AAAA1111-0000-0000-0000-000000000001"}},
			"disk2": {{id: "disk2s1", name: "Macintosh HD - Data", uuid: "AAAA2222-0000-0000-0000-000000000002"}},
			"disk4": {{id: "disk4s1", name: "ZPMirror", uuid: "CCCC1111-0000-0000-0000-000000000011"}},
		},
		mounted: map[string]string{
			"disk1s1": "/",
			"disk2s1": "/System/Volumes/Data",
			"disk4s1": "/Volumes/ZPMirror",
		},
		erasedDisk: map[string]eraseState{},
		erasedVol:  map[string]eraseState{},
		renamed:    map[string]string{},
		deleted:    map[string]bool{},
	}
}

func (w *fakeWorld) record(args ...string) {
	w.mu.Lock()
	w.calls = append(w.calls, strings.Join(args, " "))
	w.mu.Unlock()
}

func (w *fakeWorld) called(prefix string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (w *fakeWorld) callList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.calls...)
}

func (w *fakeWorld) volumeList(ref string) []fakeVol {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]fakeVol(nil), w.containerVols[ref]...)
}

const (
	uuidDisk9s1 = "BBBB1111-0000-0000-0000-000000000021"
	uuidDisk9s2 = "DDDD1111-0000-0000-0000-000000000022"
	// 整盘 disk9 的 DiskUUID（beginDiskWrite 的 TOCTOU 期望值会用它）。
	targetDisk9UUID = "FFFF1111-0000-0000-0000-0000000000FF"
	// ZPMirror 卷的 VolumeUUID（disk4s1）。
	zpmirrorUUID = "CCCC1111-0000-0000-0000-000000000011"
)

// fakeFSKey 把 diskutil 的 format 参数折算成 diskutil info 的 FilesystemType。
func fakeFSKey(format string) string {
	switch strings.ToLower(format) {
	case "apfs":
		return "apfs"
	case "hfs+":
		return "hfs"
	case "exfat":
		return "exfat"
	case "ms-dos fat32", "ms-dos", "fat32":
		return "msdos"
	}
	return strings.ToLower(format)
}

// staticInfo 是每台设备的静态 `diskutil info` 字段（挂载点会被 world.mounted 覆盖）。
func staticInfo(id string) map[string]any {
	switch id {
	case "disk0":
		return map[string]any{"DeviceIdentifier": id, "WholeDisk": true, "Internal": true,
			"Content": "GUID_partition_scheme", "Size": int64(251000193024),
			"BusProtocol": "Apple Fabric", "MediaName": "APPLE SSD AP0256Z", "SMARTStatus": "Verified"}
	case "disk0s1":
		return map[string]any{"DeviceIdentifier": id, "Internal": true, "Content": "Apple_APFS",
			"Size": int64(524288000), "ParentWholeDisk": "disk0", "BusProtocol": "Apple Fabric", "SMARTStatus": "Verified"}
	case "disk0s2":
		return map[string]any{"DeviceIdentifier": id, "Internal": true, "Content": "Apple_APFS",
			"Size": int64(245107195904), "ParentWholeDisk": "disk0", "BusProtocol": "Apple Fabric", "SMARTStatus": "Verified"}
	case "disk1", "disk2":
		return map[string]any{"DeviceIdentifier": id, "WholeDisk": true, "Internal": true,
			"Content": "Apple_APFS_Container", "Size": int64(245107195904),
			"BusProtocol": "Apple Fabric", "MediaName": "APPLE SSD AP0256Z", "SMARTStatus": "Verified"}
	case "disk9":
		return map[string]any{"DeviceIdentifier": id, "WholeDisk": true, "Internal": false,
			"RemovableMediaOrExternalDevice": true, "Content": "GUID_partition_scheme",
			"Size": int64(1024209543168), "BusProtocol": "USB", "MediaName": "RTL9210",
			"IORegistryEntryName": "Realtek RTL9210 Media", "SMARTStatus": "Not Supported",
			"DiskUUID": targetDisk9UUID}
	case "disk9s1":
		return map[string]any{"DeviceIdentifier": id, "Internal": false, "RemovableMediaOrExternalDevice": true,
			"Content": "EFI", "Size": int64(209715200), "ParentWholeDisk": "disk9",
			"VolumeName": "EFI", "FilesystemName": "MS-DOS FAT32", "FilesystemType": "msdos",
			"VolumeUUID": uuidDisk9s1, "BusProtocol": "USB", "SMARTStatus": "Not Supported"}
	case "disk9s2":
		return map[string]any{"DeviceIdentifier": id, "Internal": false, "RemovableMediaOrExternalDevice": true,
			"Content": "Apple_APFS", "Size": int64(1023999787008), "ParentWholeDisk": "disk9",
			"APFSContainerReference": "disk4", "BusProtocol": "USB", "SMARTStatus": "Not Supported"}
	case "disk4":
		return map[string]any{"DeviceIdentifier": id, "WholeDisk": true, "Internal": false,
			"RemovableMediaOrExternalDevice": true, "Content": "Apple_APFS_Container",
			"Size": int64(1023999787008), "BusProtocol": "USB", "MediaName": "RTL9210",
			"APFSPhysicalStores": []any{map[string]any{"APFSPhysicalStore": "disk9s2"}},
			"SMARTStatus":        "Not Supported"}
	}
	return nil
}

func volumeInfo(id, name, uuid, parent string, internal bool) map[string]any {
	bus := "USB"
	if internal {
		bus = "Apple Fabric"
	}
	return map[string]any{
		"DeviceIdentifier": id, "Internal": internal, "RemovableMediaOrExternalDevice": !internal,
		"Content": "41504653-0000-11AA-AA11-00306543ECAC", "ParentWholeDisk": parent,
		"VolumeName": name, "FilesystemName": "APFS", "FilesystemUserVisibleName": "APFS",
		"FilesystemType": "apfs", "VolumeUUID": uuid, "FileVault": internal, "Encryption": internal,
		"Size": int64(1023999787008), "BusProtocol": bus, "SMARTStatus": "Not Supported",
	}
}

// infoDict 组装某台设备的 info 字段（静态字段 + 动态卷 + 挂载点覆盖 +
// erase/rename/delete 的模拟结果）。
func (w *fakeWorld) infoDict(id string) map[string]any {
	if id == "/" {
		return map[string]any{"DeviceIdentifier": "disk1s1"}
	}
	w.mu.Lock()
	deleted := w.deleted[id]
	erased, isErased := w.erasedVol[id]
	newName, isRenamed := w.renamed[id]
	for _, es := range w.erasedDisk {
		if es.part == id {
			erased = es
			isErased = true
		}
	}
	w.mu.Unlock()
	if deleted {
		return nil
	}
	m := staticInfo(id)
	if isErased && w.erasedDiskPart(id) {
		// eraseDisk 之后新建出来的分区：整体覆盖静态信息。
		m = volumeInfo(id, erased.name, "EEEE9999-0000-0000-0000-000000000099", w.erasedDiskParent(id), false)
		m["FilesystemType"] = erased.fs
		m["FilesystemName"] = erased.fs
	}
	if m == nil {
		for ref, vols := range w.containerVols {
			for _, v := range vols {
				if v.id == id {
					internal := ref == "disk1" || ref == "disk2"
					m = volumeInfo(id, v.name, v.uuid, ref, internal)
				}
			}
		}
	}
	if m == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	if isErased && !w.erasedDiskPart(id) {
		out["VolumeName"] = erased.name
		out["FilesystemType"] = erased.fs
		out["FilesystemName"] = erased.fs
	}
	if isRenamed {
		out["VolumeName"] = newName
	}
	w.mu.Lock()
	mp, mounted := w.mounted[id]
	w.mu.Unlock()
	if mounted {
		out["MountPoint"] = mp
	} else {
		out["MountPoint"] = ""
	}
	return out
}

func (w *fakeWorld) erasedDiskPart(id string) bool {
	for _, es := range w.erasedDisk {
		if es.part == id {
			return true
		}
	}
	return false
}

func (w *fakeWorld) erasedDiskParent(id string) string {
	for dev, es := range w.erasedDisk {
		if es.part == id {
			return dev
		}
	}
	return ""
}

func (w *fakeWorld) listPlist() string {
	parts := func(ids ...string) []any {
		arr := []any{}
		for _, id := range ids {
			arr = append(arr, map[string]any{"DeviceIdentifier": id})
		}
		return arr
	}
	// disk9 整盘被 eraseDisk 之后只剩一个新分区。
	disk9Parts := []string{"disk9s1", "disk9s2"}
	w.mu.Lock()
	if es, okk := w.erasedDisk["disk9"]; okk {
		disk9Parts = []string{es.part}
	}
	w.mu.Unlock()
	vols := func(ref string) []any {
		arr := []any{}
		for _, v := range w.volumeList(ref) {
			w.mu.Lock()
			dead := w.deleted[v.id]
			w.mu.Unlock()
			if dead {
				continue
			}
			arr = append(arr, map[string]any{"DeviceIdentifier": v.id, "VolumeName": v.name, "VolumeUUID": v.uuid})
		}
		return arr
	}
	top := []any{
		map[string]any{"DeviceIdentifier": "disk0", "Size": int64(251000193024),
			"Content": "GUID_partition_scheme", "Partitions": parts("disk0s1", "disk0s2")},
		map[string]any{"DeviceIdentifier": "disk1", "Size": int64(245107195904),
			"Content": "Apple_APFS_Container", "Partitions": []any{}, "APFSVolumes": vols("disk1")},
		map[string]any{"DeviceIdentifier": "disk2", "Size": int64(245107195904),
			"Content": "Apple_APFS_Container", "Partitions": []any{}, "APFSVolumes": vols("disk2")},
		map[string]any{"DeviceIdentifier": "disk4", "Size": int64(1023999787008),
			"Content": "Apple_APFS_Container", "Partitions": []any{}, "APFSVolumes": vols("disk4"),
			"APFSPhysicalStores": []any{map[string]any{"DeviceIdentifier": "disk9s2"}}},
		map[string]any{"DeviceIdentifier": "disk9", "Size": int64(1024209543168),
			"Content": "GUID_partition_scheme", "Partitions": parts(disk9Parts...)},
	}
	all := []any{"disk0", "disk0s1", "disk0s2", "disk1", "disk1s1", "disk2", "disk2s1", "disk4", "disk9", "disk9s1", "disk9s2"}
	for _, v := range w.volumeList("disk4") {
		all = append(all, v.id)
	}
	return encodePlist(map[string]any{"AllDisks": all, "AllDisksAndPartitions": top})
}

func (w *fakeWorld) apfsPlist() string {
	containers := []any{}
	for _, ref := range []string{"disk1", "disk2", "disk4"} {
		vols := []any{}
		for _, v := range w.volumeList(ref) {
			vols = append(vols, map[string]any{
				"DeviceIdentifier": v.id, "Name": v.name, "APFSVolumeUUID": v.uuid,
				"Encryption": false, "Locked": false, "CapacityInUse": int64(860160),
			})
		}
		containers = append(containers, map[string]any{
			"ContainerReference": ref, "CapacityCeiling": int64(1023999787008),
			"CapacityFree": int64(1023789551616), "Volumes": vols,
		})
	}
	return encodePlist(map[string]any{"Containers": containers})
}

// exec 是假 diskutil：按 args 分派。
func (w *fakeWorld) exec(_ context.Context, _ time.Duration, args ...string) (string, string, error) {
	w.record(args...)
	if len(args) == 0 {
		return "", "", fmt.Errorf("no args")
	}
	switch args[0] {
	case "list":
		return w.listPlist(), "", nil
	case "apfs":
		if len(args) >= 2 && args[1] == "list" {
			return w.apfsPlist(), "", nil
		}
		if len(args) >= 5 && args[1] == "addVolume" {
			if w.addVolumeFails {
				return "", "Could not add volume: simulated failure", fmt.Errorf("exit status 1")
			}
			ref, name := args[2], args[4]
			w.mu.Lock()
			idx := len(w.containerVols[ref]) + 1
			nv := fakeVol{id: fmt.Sprintf("%ss%d", ref, idx), name: name,
				uuid: fmt.Sprintf("EEEE%04d-0000-0000-0000-000000000000", idx)}
			w.containerVols[ref] = append(w.containerVols[ref], nv)
			w.mu.Unlock()
			return "Volume " + nv.id + " added", "", nil
		}
		if len(args) >= 3 && args[1] == "deleteVolume" {
			w.mu.Lock()
			w.deleted[args[2]] = true
			for ref, vols := range w.containerVols {
				kept := vols[:0]
				for _, v := range vols {
					if v.id != args[2] {
						kept = append(kept, v)
					}
				}
				w.containerVols[ref] = kept
			}
			w.mu.Unlock()
			return "Deleted APFS Volume " + args[2], "", nil
		}
		return "", "", fmt.Errorf("unsupported apfs args %v", args)
	case "eraseDisk":
		if w.timeoutErase {
			return "", "", errDiskTimeout
		}
		if len(args) < 4 {
			return "", "", fmt.Errorf("eraseDisk needs format name device")
		}
		fs, name, device := args[1], args[2], args[3]
		w.mu.Lock()
		w.erasedDisk[device] = eraseState{name: name, fs: fakeFSKey(fs), part: device + "s1"}
		w.mu.Unlock()
		return "Finished erase on " + device, "", nil
	case "eraseVolume":
		if w.timeoutErase {
			return "", "", errDiskTimeout
		}
		if len(args) < 4 {
			return "", "", fmt.Errorf("eraseVolume needs format name device")
		}
		fs, name, device := args[1], args[2], args[3]
		w.mu.Lock()
		w.erasedVol[device] = eraseState{name: name, fs: fakeFSKey(fs)}
		w.mu.Unlock()
		return "Finished erase on " + device, "", nil
	case "rename":
		if len(args) < 3 {
			return "", "", fmt.Errorf("rename needs device name")
		}
		w.mu.Lock()
		w.renamed[args[1]] = args[2]
		w.mu.Unlock()
		return "Volume " + args[1] + " renamed", "", nil
	case "info":
		if len(args) < 3 {
			return "", "", fmt.Errorf("info needs device")
		}
		m := w.infoDict(args[2])
		if m == nil {
			return "", "Could not find disk: " + args[2], fmt.Errorf("exit status 1")
		}
		return encodePlist(m), "", nil
	case "mount":
		id := args[1]
		if w.failMount {
			return "", "Could not mount disk " + id, fmt.Errorf("exit status 1")
		}
		if !w.mountNoVerify {
			w.mu.Lock()
			if _, okk := w.mounted[id]; !okk {
				name := "Untitled"
				for _, vols := range w.containerVols {
					for _, v := range vols {
						if v.id == id {
							name = v.name
						}
					}
				}
				w.mounted[id] = "/Volumes/" + name
			}
			w.mu.Unlock()
		}
		return "Volume " + id + " mounted", "", nil
	case "unmount":
		id := args[1]
		if w.timeoutUnmount {
			return "", "", errDiskTimeout
		}
		w.mu.Lock()
		delete(w.mounted, id)
		w.mu.Unlock()
		return "Volume " + id + " unmounted", "", nil
	}
	return "", "", fmt.Errorf("unsupported diskutil %v", args)
}

// stubDiskWorld 安装假 diskutil 与临时 fstab，并保证测试后恢复。
func stubDiskWorld(t *testing.T, w *fakeWorld) string {
	t.Helper()
	prevExec := diskExec
	diskExec = w.exec
	prevFstab := diskFstabPath
	diskFstabPath = filepath.Join(t.TempDir(), "fstab")
	t.Cleanup(func() {
		diskExec = prevExec
		diskFstabPath = prevFstab
	})
	return diskFstabPath
}

// newDiskServer 起一个用假磁盘世界武装起来的测试服务器，并完成初始化登录。
func newDiskServer(t *testing.T, w *fakeWorld) (*Server, *httptest.Server, []*http.Cookie) {
	t.Helper()
	stubDiskWorld(t, w)
	srv, ts := newTestServer(t)
	res, out, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("初始化失败 %d: %v", res.StatusCode, out)
	}
	return srv, ts, cookies
}

// ============================================================================
//  危险操作 = 任务中心任务
// ============================================================================

// runDiskTask 提交一个危险操作并等任务结束：先断言 202 + task_id（走任务中心），
// 再等任务结束返回任务对象。
func runDiskTask(t *testing.T, srv *Server, ts *httptest.Server, cookies []*http.Cookie, path string, body any) *tasks.Task {
	t.Helper()
	res, out, _ := doJSON(t, ts, "POST", path, body, cookies)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%s 必须走任务中心（202 + task_id），实际 %d: %v", path, res.StatusCode, out["msg"])
	}
	return waitTaskDone(t, srv, taskIDFrom(t, out))
}

// diskTaskInstallResult 取任务结果。后端包成 *services.InstallResult：
// Steps 里带 before/after 快照与回读摘要（审计也来自它）。
func diskTaskInstallResult(t *testing.T, tk *tasks.Task) *services.InstallResult {
	t.Helper()
	r, _ := tk.Meta().Result.(*services.InstallResult)
	if r == nil {
		t.Fatalf("任务结果应为 *services.InstallResult，实际 %#v（error=%q）", tk.Meta().Result, tk.Meta().Error)
	}
	return r
}

// noDiskTaskCreated 断言拒绝路径**没有创建任务**（验收：不建任务、不执行写命令）。
func noDiskTaskCreated(t *testing.T, srv *Server, why string) {
	t.Helper()
	if list := srv.Tasks.List(); len(list) != 0 {
		t.Errorf("%s：拒绝时不应创建任何任务，实际 %d 个：%v", why, len(list), list)
	}
}

func stepsContain(steps []string, sub string) bool {
	for _, s := range steps {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ============================================================================
//  plist 编码器（只给测试用：把 Go 值编成 plist，喂给假 diskutil）
// ============================================================================

func encodePlist(v any) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	encodePlistValue(&b, v, 0)
	b.WriteString("</plist>\n")
	return b.String()
}

func encodePlistValue(b *strings.Builder, v any, depth int) {
	ind := strings.Repeat("\t", depth)
	switch t := v.(type) {
	case map[string]any:
		b.WriteString(ind + "<dict>\n")
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(ind + "\t<key>" + xmlEscape(k) + "</key>\n")
			encodePlistValue(b, t[k], depth+1)
		}
		b.WriteString(ind + "</dict>\n")
	case []any:
		b.WriteString(ind + "<array>\n")
		for _, it := range t {
			encodePlistValue(b, it, depth+1)
		}
		b.WriteString(ind + "</array>\n")
	case string:
		b.WriteString(ind + "<string>" + xmlEscape(t) + "</string>\n")
	case bool:
		if t {
			b.WriteString(ind + "<true/>\n")
		} else {
			b.WriteString(ind + "<false/>\n")
		}
	case int:
		b.WriteString(ind + fmt.Sprintf("<integer>%d</integer>\n", t))
	case int64:
		b.WriteString(ind + fmt.Sprintf("<integer>%d</integer>\n", t))
	case float64:
		b.WriteString(ind + fmt.Sprintf("<real>%v</real>\n", t))
	case nil:
		b.WriteString(ind + "<string></string>\n")
	default:
		b.WriteString(ind + "<string>" + xmlEscape(fmt.Sprint(t)) + "</string>\n")
	}
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func asSlice(v any) []any { a, _ := v.([]any); return a }

func mapGet(v any, key string) any {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	return m[key]
}

// findGroup 在列表响应里找某个整盘卡片。
func findGroup(data any, id string) map[string]any {
	for _, it := range asSlice(mapGet(data, "list")) {
		g, _ := it.(map[string]any)
		if asString(mapGet(g["disk"], "id")) == id {
			return g
		}
	}
	return nil
}

// ============================================================================
//  plist 解析器
// ============================================================================

func TestParsePlistRoundTrip(t *testing.T) {
	src := map[string]any{
		"AllDisks": []any{"disk0", "disk9"},
		"Size":     int64(42),
		"Flag":     true,
		"Nested":   map[string]any{"Name": "RTL9210"},
	}
	got, err := parsePlist([]byte(encodePlist(src)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	m := plistMap(got)
	if m == nil {
		t.Fatal("顶层不是 dict")
	}
	if n := plistInt(m, "Size"); n != 42 {
		t.Errorf("Size = %d, 期望 42", n)
	}
	if s := plistStr(plistMap(m["Nested"]), "Name"); s != "RTL9210" {
		t.Errorf("Nested.Name = %q", s)
	}
	if ids := plistStrSlice(m, "AllDisks"); len(ids) != 2 || ids[1] != "disk9" {
		t.Errorf("AllDisks = %v", ids)
	}
	if !plistBool(m, "Flag") {
		t.Error("Flag 应为 true")
	}
}

func TestParsePlistRejectsGarbage(t *testing.T) {
	if _, err := parsePlist([]byte("not a plist at all")); err == nil {
		t.Fatal("垃圾输入必须返回 error，不能当成空结构")
	}
	if _, err := parsePlist([]byte(`<plist version="1.0"><dict><key>a</key></dict></plist>`)); err == nil {
		t.Fatal("key 没有值必须报错")
	}
}

// ============================================================================
//  只读接口
// ============================================================================

func TestDiskListReturnsExternalDiskAndPartitions(t *testing.T) {
	_, ts, cookies := newDiskServer(t, newFakeWorld())
	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/disks", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("磁盘列表应 200，实际 %d: %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatal("缺 data")
	}
	g9 := findGroup(data, "disk9")
	if g9 == nil {
		t.Fatal("列表里必须能看到 disk9")
	}
	if mapGet(g9["disk"], "system_disk") == true {
		t.Error("disk9 是外接盘，不应被标成系统盘")
	}
	var ids []string
	for _, p := range asSlice(g9["partitions"]) {
		ids = append(ids, asString(mapGet(p, "id")))
	}
	if strings.Join(ids, ",") != "disk9s1,disk9s2" {
		t.Fatalf("disk9 的分区应为 disk9s1,disk9s2，实际 %v", ids)
	}
	if mapGet(findGroup(data, "disk0")["disk"], "system_disk") != true {
		t.Error("disk0 必须被标成系统盘")
	}
	// disk4s1 ZPMirror 应带挂载点与"已挂载"、SMART 如实
	var zd map[string]any
	for _, p := range asSlice(findGroup(data, "disk4")["partitions"]) {
		if asString(mapGet(p, "id")) == "disk4s1" {
			zd, _ = p.(map[string]any)
		}
	}
	if zd == nil {
		t.Fatal("找不到 disk4s1（ZPMirror）")
	}
	if zd["mounted"] != true || asString(zd["mount_point"]) != "/Volumes/ZPMirror" {
		t.Errorf("disk4s1 应显示已挂载于 /Volumes/ZPMirror，实际 %v", zd)
	}
	if asString(zd["smart_status"]) != "not_supported" {
		t.Errorf("SMART 应如实报 not_supported，实际 %v", zd["smart_status"])
	}
	if asString(zd["smart_note"]) == "" {
		t.Error("SMART 未复核时必须带说明")
	}
	if len(asSlice(data["protected"])) == 0 {
		t.Fatal("protected 不能为空（至少要有系统盘）")
	}
}

// ============================================================================
//  系统盘保护
// ============================================================================

func TestDiskWriteOpsRefuseSystemDisk(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)

	cases := []struct {
		name string
		path string
		body any
	}{
		{"挂载系统盘", "/api/v1/system/disks/disk1s1/mount", map[string]any{}},
		{"卸载系统盘", "/api/v1/system/disks/disk1s1/unmount", map[string]any{}},
		{"卸载数据卷", "/api/v1/system/disks/disk2s1/unmount", map[string]any{}},
		{"系统盘自动挂载", "/api/v1/system/disks/disk1s1/auto-mount", map[string]any{"enabled": true}},
		{"系统盘初始化", "/api/v1/system/disks/disk0/init-volume", map[string]any{"name": "x", "confirm": "disk0"}},
		{"系统盘抹盘", "/api/v1/system/disks/disk0/erase", map[string]any{"filesystem": "exfat", "name": "x", "confirm": "disk0"}},
		{"系统盘建卷", "/api/v1/system/disks/disk0/volume-create", map[string]any{"name": "x", "confirm": "disk0"}},
		{"系统盘删卷", "/api/v1/system/disks/disk1s1/volume-delete", map[string]any{"confirm": "disk1s1"}},
		{"系统盘重命名", "/api/v1/system/disks/disk1s1/volume-rename", map[string]any{"name": "x", "confirm": "disk1s1"}},
		{"根卷抹盘", "/api/v1/system/disks/disk1s1/erase", map[string]any{"filesystem": "apfs", "name": "x", "confirm": "disk1s1"}},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", c.path, c.body, cookies)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s：应 403，实际 %d (%v)", c.name, res.StatusCode, out["msg"])
		}
		if !strings.Contains(asString(out["msg"]), "系统盘") {
			t.Errorf("%s：拒绝理由必须提到系统盘，实际 %q", c.name, asString(out["msg"]))
		}
	}
	// 关键：被拒的请求**根本没有执行过**任何写命令。
	for _, prefix := range []string{
		"mount disk1s1", "unmount disk1s1", "unmount disk2s1", "apfs addVolume",
		"eraseDisk", "eraseVolume", "apfs deleteVolume", "rename",
	} {
		if w.called(prefix) {
			t.Errorf("系统盘保护失效：真的执行了 %q", prefix)
		}
	}
	// 验收：系统盘被拒时**不创建任务**（不建任务 + 不执行写命令，二者都要）。
	noDiskTaskCreated(t, srv, "系统盘拒绝")
}

func TestDiskUnknownAndInvalidID(t *testing.T) {
	w := newFakeWorld()
	_, ts, cookies := newDiskServer(t, w)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk99/mount", map[string]any{}, cookies)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的设备应 404，实际 %d (%v)", res.StatusCode, out["msg"])
	}
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/disks/disk9s1%3Brm/mount", map[string]any{}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 id 应 400，实际 %d (%v)", res.StatusCode, out["msg"])
	}
	if w.called("mount disk99") {
		t.Error("白名单校验失效：执行了不存在的设备")
	}
}

// ============================================================================
//  挂载 / 卸载：不看退出码，看回读
// ============================================================================

func TestDiskUnmountThenMountVerifiesByReadback(t *testing.T) {
	_, ts, cookies := newDiskServer(t, newFakeWorld())

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/unmount", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("卸载外接卷应 200，实际 %d: %v", res.StatusCode, out["msg"])
	}
	data, _ := out["data"].(map[string]any)
	if data["verified"] != true || data["mounted"] != false {
		t.Errorf("卸载后回读应 verified=true mounted=false，实际 %v", data)
	}

	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/mount", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("挂载外接卷应 200，实际 %d: %v", res.StatusCode, out["msg"])
	}
	data, _ = out["data"].(map[string]any)
	if data["verified"] != true || data["mounted"] != true {
		t.Errorf("挂载后回读应 verified=true mounted=true，实际 %v", data)
	}
	if asString(data["mount_point"]) == "" {
		t.Error("挂载成功必须带真实挂载点")
	}
}

func TestDiskMountExitZeroButNotMountedIsFailure(t *testing.T) {
	w := newFakeWorld()
	w.mountNoVerify = true // 命令说成功，状态没变
	_, ts, cookies := newDiskServer(t, w)

	doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/unmount", map[string]any{}, cookies)
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/mount", map[string]any{}, cookies)
	if res.StatusCode < 400 {
		t.Fatalf("退出码 0 但回读未挂载必须判失败，实际 %d: %v", res.StatusCode, out)
	}
	if !strings.Contains(asString(out["msg"]), "回读") {
		t.Errorf("错误信息里必须说明回读不一致，实际 %q", asString(out["msg"]))
	}
}

func TestDiskUnmountTimeoutReadsBackState(t *testing.T) {
	w := newFakeWorld()
	w.timeoutUnmount = true
	_, ts, cookies := newDiskServer(t, w)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/unmount", map[string]any{}, cookies)
	if res.StatusCode < 400 {
		t.Fatalf("超时必须报失败，实际 %d", res.StatusCode)
	}
	msg := asString(out["msg"])
	if !strings.Contains(msg, "超时") {
		t.Errorf("必须说明超时，实际 %q", msg)
	}
	if !strings.Contains(msg, "回读") {
		t.Errorf("超时后必须回读真实状态，实际 %q", msg)
	}
	if !w.called("info -plist disk4s1") {
		t.Error("超时后没有回读 diskutil info")
	}
}

// ============================================================================
//  开机自动挂载
// ============================================================================

func TestDiskAutoMountWritesAndRemovesFstab(t *testing.T) {
	w := newFakeWorld()
	stubDiskWorld(t, w)
	srv, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	_ = srv
	fstab := diskFstabPath
	if !strings.HasPrefix(fstab, os.TempDir()) && !strings.Contains(fstab, "zpt") {
		t.Fatalf("fstab 未沙箱化：%s", fstab)
	}

	// 预置一条别人的条目，确认不会被我们删掉。
	if err := os.WriteFile(fstab, []byte("LABEL=Other /Volumes/Other apfs rw,noauto 0 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/auto-mount", map[string]any{"enabled": true}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("写自动挂载应 200，实际 %d: %v", res.StatusCode, out["msg"])
	}
	data, _ := out["data"].(map[string]any)
	if data["fstab_written"] != true || data["enabled"] != true {
		t.Errorf("应写入 fstab，实际 %v", data)
	}
	got, _ := os.ReadFile(fstab)
	content := string(got)
	if !strings.Contains(content, "UUID=CCCC1111-0000-0000-0000-000000000011") {
		t.Errorf("fstab 里应有该卷 UUID，实际:\n%s", content)
	}
	if !strings.Contains(content, "/Volumes/ZPMirror apfs rw 0 2") {
		t.Errorf("fstab 条目格式不对，实际:\n%s", content)
	}
	if !strings.Contains(content, "LABEL=Other") {
		t.Errorf("不能删掉别人的条目，实际:\n%s", content)
	}
	if _, err := os.Stat(fstab + ".zizpanel.bak"); err != nil {
		t.Errorf("应留下备份文件: %v", err)
	}
	found := false
	for _, e := range parseFstabEntries(content) {
		if strings.EqualFold(e.UUID, "CCCC1111-0000-0000-0000-000000000011") {
			found = true
			if e.VFSType != "apfs" || !strings.Contains(e.Options, "rw") {
				t.Errorf("fstab 条目字段不对: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("解析 fstab 时找不到刚写入的条目")
	}

	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/auto-mount", map[string]any{"enabled": false}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("删自动挂载应 200，实际 %d: %v", res.StatusCode, out["msg"])
	}
	got, _ = os.ReadFile(fstab)
	content = string(got)
	if strings.Contains(content, "CCCC1111") {
		t.Errorf("关闭后不该还有该卷条目，实际:\n%s", content)
	}
	if !strings.Contains(content, "LABEL=Other") {
		t.Errorf("别人的条目不能被删，实际:\n%s", content)
	}
}

func TestDiskAutoMountIdempotentAndRequiresEnabledField(t *testing.T) {
	w := newFakeWorld()
	stubDiskWorld(t, w)
	_, ts := newTestServer(t)
	_, _, cookies := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/auto-mount", map[string]any{}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 enabled 应 400，实际 %d (%v)", res.StatusCode, out["msg"])
	}
	doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/auto-mount", map[string]any{"enabled": true}, cookies)
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/auto-mount", map[string]any{"enabled": true}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("重复写应幂等成功，实际 %d: %v", res.StatusCode, out["msg"])
	}
	data, _ := out["data"].(map[string]any)
	if data["changed"] == true {
		t.Errorf("第二次不该再改动，实际 %v", data)
	}
}

func TestApplyFstabEntryFormatAndPreserve(t *testing.T) {
	spec := fstabSpec{UUID: "X", MountPoint: "/Volumes/My Disk", VFSType: "apfs"}
	out := applyFstabEntry("", true, spec)
	if !strings.Contains(out, `UUID=X /Volumes/My\040Disk apfs rw 0 2`) {
		t.Fatalf("空格应按 fstab(5) 转义为 \\040，实际:\n%s", out)
	}
	out2 := applyFstabEntry(out, false, spec)
	if strings.TrimSpace(out2) != "" {
		t.Fatalf("删除后应为空文件，实际:\n%s", out2)
	}
	out3 := applyFstabEntry("UUID=Y /Volumes/Y apfs rw 0 2\n", true, spec)
	if !strings.Contains(out3, "UUID=Y") {
		t.Fatalf("误删了别的 UUID:\n%s", out3)
	}
}

// ============================================================================
//  初始化镜像盘
// ============================================================================

func TestValidateVolumeName(t *testing.T) {
	ok := []string{"ZPMirror", "镜像盘", "My Disk", "a-b_c.d", "备份 2026"}
	for _, n := range ok {
		if err := validateVolumeName(n); err != nil {
			t.Errorf("%q 应合法: %v", n, err)
		}
	}
	bad := []string{"", "a/b", "a:b", "a*b", `a"b`, "a<b", "a|b", "a\\b", "a\nb", strings.Repeat("x", 33), "..."}
	for _, n := range bad {
		if err := validateVolumeName(n); err == nil {
			t.Errorf("%q 应被拒绝", n)
		}
	}
}

// 2026-09 用户放开限制后：非空容器**也允许**新建卷（addVolume 不动已有卷）。
func TestDiskVolumeCreateAllowsNonEmptyContainer(t *testing.T) {
	w := newFakeWorld() // disk4 里有 ZPMirror
	srv, ts, cookies := newDiskServer(t, w)

	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk9/init-volume",
		map[string]any{"name": "Mirror1", "confirm": "disk9"})
	if tk.Meta().Status != tasks.StatusSucceeded {
		t.Fatalf("非空容器现在应允许建卷，实际 %s: %v", tk.Meta().Status, tk.Meta().Error)
	}
	r := diskTaskInstallResult(t, tk)
	if !stepsContain(r.Steps, "新建卷") || !stepsContain(r.Steps, "回读：") {
		t.Errorf("结果要含建卷摘要与回读，实际 %v", r.Steps)
	}
	if !w.called("apfs addVolume disk4 APFS Mirror1") {
		t.Errorf("命令构造不对：%v", w.callList())
	}
	// 原有卷不受影响
	if len(w.volumeList("disk4")) != 2 {
		t.Errorf("原有 ZPMirror 不能被删掉，实际 %v", w.volumeList("disk4"))
	}
}

// confirm 不匹配 / 卷名非法时必须拒绝，且**不创建任务、不执行命令**。
func TestDiskInitRequiresConfirmAndValidName(t *testing.T) {
	w := newFakeWorld()
	w.containerVols["disk4"] = nil // 造一个空容器
	srv, ts, cookies := newDiskServer(t, w)

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk9/init-volume",
		map[string]any{"name": "Mirror1", "confirm": "disk4"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("手打标识不匹配应 400，实际 %d: %v", res.StatusCode, out["msg"])
	}
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/disks/disk9/init-volume",
		map[string]any{"name": "bad/name", "confirm": "disk9"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法卷名应 400，实际 %d: %v", res.StatusCode, out["msg"])
	}
	if w.called("apfs addVolume") {
		t.Error("校验失败时绝不能真的执行 addVolume")
	}
	noDiskTaskCreated(t, srv, "确认不匹配/卷名非法")
}

func TestDiskInitEmptyContainerCreatesVolume(t *testing.T) {
	w := newFakeWorld()
	w.containerVols["disk4"] = nil // 空 APFS 容器（本机那块 1T 盘的目标形态）
	srv, ts, cookies := newDiskServer(t, w)

	res, out, _ := doJSON(t, ts, "GET", "/api/v1/system/disks", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("列表 %d", res.StatusCode)
	}
	listData, _ := out["data"].(map[string]any)
	init9, _ := findGroup(listData, "disk9")["init"].(map[string]any)
	if init9 == nil || init9["eligible"] != true {
		t.Fatalf("空容器下 disk9 应可初始化，实际 %v", init9)
	}
	if asString(init9["container_ref"]) != "disk4" {
		t.Errorf("容器引用应为 disk4，实际 %v", init9["container_ref"])
	}
	if asString(init9["backend_command"]) == "" {
		t.Error("必须把将要执行的命令告诉用户")
	}

	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk9/init-volume",
		map[string]any{"name": "ZPMirror", "confirm": "disk9"})
	if tk.Meta().Status != tasks.StatusSucceeded {
		t.Fatalf("空容器初始化应成功，实际 %s: %v", tk.Meta().Status, tk.Meta().Error)
	}
	r := diskTaskInstallResult(t, tk)
	if !stepsContain(r.Steps, "ZPMirror") || !stepsContain(r.Steps, "回读：") {
		t.Errorf("结果要带卷名与回读摘要，实际 %v", r.Steps)
	}
	if !w.called("apfs addVolume disk4 APFS ZPMirror") {
		t.Errorf("命令构造不对，实际调用：%v", w.callList())
	}
}

func TestDiskInitFailureReadsBackState(t *testing.T) {
	w := newFakeWorld()
	w.containerVols["disk4"] = nil
	w.addVolumeFails = true
	srv, ts, cookies := newDiskServer(t, w)

	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk9/init-volume",
		map[string]any{"name": "ZPMirror", "confirm": "disk9"})
	if tk.Meta().Status != tasks.StatusFailed {
		t.Fatalf("命令失败必须让任务失败，实际 %s", tk.Meta().Status)
	}
	msg := tk.Meta().Error
	if !strings.Contains(msg, "simulated failure") {
		t.Errorf("必须带 stderr 原文，实际 %q", msg)
	}
	if !strings.Contains(msg, "回读") {
		t.Errorf("失败后必须回读真实状态，实际 %q", msg)
	}
	if !strings.Contains(msg, "before=") {
		t.Errorf("失败审计要带动作前快照，实际 %q", msg)
	}
}

// ============================================================================
//  无头安全 / 鉴权
// ============================================================================

// ============================================================================
//  放开限制后的写动作：抹盘/格式化/建卷/删卷/重命名
// ============================================================================

// 文件系统目录必须随列表下发（界面据此展示取舍）。
func TestDiskListExposesFilesystemCatalog(t *testing.T) {
	_, ts, cookies := newDiskServer(t, newFakeWorld())
	_, out, _ := doJSON(t, ts, "GET", "/api/v1/system/disks", nil, cookies)
	fss := asSlice(mapGet(out["data"], "filesystems"))
	if len(fss) < 5 {
		t.Fatalf("文件系统目录至少要有 5 项，实际 %v", fss)
	}
	keys := map[string]bool{}
	for _, f := range fss {
		keys[asString(mapGet(f, "key"))] = true
		if asString(mapGet(f, "note")) == "" {
			t.Errorf("%v 缺少取舍说明（note）", mapGet(f, "key"))
		}
	}
	for _, k := range []string{"apfs", "hfs+", "exfat", "fat32", "keep"} {
		if !keys[k] {
			t.Errorf("文件系统目录缺少 %q", k)
		}
	}
}

func TestDiskEraseVolumeSuccess(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk4s1/erase",
		map[string]any{"filesystem": "exfat", "name": "BackupDisk", "confirm": "disk4s1",
			"expect_uuid": zpmirrorUUID, "expect_size": int64(1023999787008)})
	if tk.Meta().Status != tasks.StatusSucceeded {
		t.Fatalf("对外接卷 eraseVolume 应成功，实际 %s: %v", tk.Meta().Status, tk.Meta().Error)
	}
	r := diskTaskInstallResult(t, tk)
	if !strings.Contains(r.Message, "BackupDisk") {
		t.Errorf("结果摘要应含新卷名，实际 %q", r.Message)
	}
	if !stepsContain(r.Steps, "卷名=BackupDisk") {
		t.Errorf("回读摘要必须给出卷名，实际 %v", r.Steps)
	}
	if !w.called("eraseVolume ExFAT BackupDisk disk4s1") {
		t.Errorf("命令构造不对：%v", w.callList())
	}
	if !stepsContain(r.Steps, "ZPMirror") {
		t.Errorf("before_state 必须记录动作前的卷快照，实际 %v", r.Steps)
	}
}

func TestDiskEraseWholeDiskSuccessAndCommandShape(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk9/erase",
		map[string]any{"filesystem": "exfat", "name": "BigDisk", "confirm": "disk9",
			"expect_uuid": targetDisk9UUID, "expect_size": int64(1024209543168)})
	if tk.Meta().Status != tasks.StatusSucceeded {
		t.Fatalf("整盘 eraseDisk 应成功，实际 %s: %v", tk.Meta().Status, tk.Meta().Error)
	}
	r := diskTaskInstallResult(t, tk)
	if !stepsContain(r.Steps, "卷名=BigDisk") {
		t.Errorf("应回读到新卷名 BigDisk，实际 %v", r.Steps)
	}
	if !w.called("eraseDisk ExFAT BigDisk disk9") {
		t.Errorf("整盘应走 eraseDisk，实际：%v", w.callList())
	}
}

func TestDiskEraseRejectsKeepBadFSAndBadConfirm(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"keep 不格式化", map[string]any{"filesystem": "keep", "name": "x", "confirm": "disk4s1"}},
		{"未知文件系统", map[string]any{"filesystem": "zfs", "name": "x", "confirm": "disk4s1"}},
		{"手输标识不匹配", map[string]any{"filesystem": "exfat", "name": "x", "confirm": "disk4"}},
		{"卷名非法", map[string]any{"filesystem": "exfat", "name": "bad/name", "confirm": "disk4s1"}},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/erase", c.body, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s：应 400，实际 %d (%v)", c.name, res.StatusCode, out["msg"])
		}
	}
	if w.called("eraseVolume") || w.called("eraseDisk") {
		t.Errorf("校验失败时绝不能执行任何 erase，实际：%v", w.callList())
	}

	// APFS 容器分区不能直接 eraseVolume（实测 diskutil 会拒绝）——提前 400 给可行动的错误。
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk9s2/erase",
		map[string]any{"filesystem": "hfs+", "name": "x", "confirm": "disk9s2"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("APFS 容器分区应 400，实际 %d (%v)", res.StatusCode, out["msg"])
	}
	if !strings.Contains(asString(out["msg"]), "APFS 容器") {
		t.Errorf("错误应解释这是 APFS 容器，实际 %q", asString(out["msg"]))
	}
	if w.called("eraseVolume") {
		t.Errorf("APFS 容器分区绝不能真的 eraseVolume：%v", w.callList())
	}
	// 验收：这些拒绝路径**不创建任务**。
	noDiskTaskCreated(t, srv, "抹盘校验失败")
}

// TOCTOU：UUID/容量与页面不一致 → 409，且不创建任务、不执行任何写命令。
func TestDiskWriteRejectsTOCTOUIdentityMismatch(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"UUID 变了", map[string]any{"filesystem": "exfat", "name": "x", "confirm": "disk4s1",
			"expect_uuid": "00000000-DEAD-BEEF-0000-000000000000", "expect_size": int64(1023999787008)}},
		{"容量变了", map[string]any{"filesystem": "exfat", "name": "x", "confirm": "disk4s1",
			"expect_uuid": zpmirrorUUID, "expect_size": int64(123)}},
	}
	for _, c := range cases {
		res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/erase", c.body, cookies)
		if res.StatusCode != http.StatusConflict {
			t.Errorf("%s：应 409，实际 %d (%v)", c.name, res.StatusCode, out["msg"])
		}
		if !strings.Contains(asString(out["msg"]), "刷新") {
			t.Errorf("%s：应提示刷新重试，实际 %q", c.name, asString(out["msg"]))
		}
	}
	if w.called("eraseVolume") {
		t.Errorf("TOCTOU 不匹配时绝不能执行写命令：%v", w.callList())
	}
	noDiskTaskCreated(t, srv, "TOCTOU 不匹配")
}

func TestDiskEraseTimeoutReadsBackState(t *testing.T) {
	w := newFakeWorld()
	w.timeoutErase = true
	srv, ts, cookies := newDiskServer(t, w)
	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk4s1/erase",
		map[string]any{"filesystem": "apfs", "name": "X", "confirm": "disk4s1",
			"expect_uuid": zpmirrorUUID, "expect_size": int64(1023999787008)})
	if tk.Meta().Status != tasks.StatusFailed {
		t.Fatalf("超时必须让任务失败，实际 %s", tk.Meta().Status)
	}
	msg := tk.Meta().Error
	if !strings.Contains(msg, "超时") || !strings.Contains(msg, "回读") {
		t.Errorf("超时后必须说明超时并回读真实状态，实际 %q", msg)
	}
}

func TestDiskVolumeDeleteSuccessAndRejectsNonAPFS(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)

	// 非 APFS 卷（EFI）不能删
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk9s1/volume-delete",
		map[string]any{"confirm": "disk9s1"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("非 APFS 卷应 400，实际 %d (%v)", res.StatusCode, out["msg"])
	}
	if w.called("apfs deleteVolume") {
		t.Error("非 APFS 卷绝不能真删")
	}
	noDiskTaskCreated(t, srv, "非 APFS 卷删除")

	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk4s1/volume-delete",
		map[string]any{"confirm": "disk4s1", "expect_uuid": zpmirrorUUID, "expect_size": int64(1023999787008)})
	if tk.Meta().Status != tasks.StatusSucceeded {
		t.Fatalf("删 APFS 卷应成功，实际 %s: %v", tk.Meta().Status, tk.Meta().Error)
	}
	if !w.called("apfs deleteVolume disk4s1") {
		t.Errorf("命令构造不对：%v", w.callList())
	}
	if len(w.volumeList("disk4")) != 0 {
		t.Errorf("卷应已从容器移除，实际 %v", w.volumeList("disk4"))
	}
}

func TestDiskVolumeRenameSuccess(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	tk := runDiskTask(t, srv, ts, cookies, "/api/v1/system/disks/disk4s1/volume-rename",
		map[string]any{"name": "ZPArchive", "confirm": "disk4s1",
			"expect_uuid": zpmirrorUUID, "expect_size": int64(1023999787008)})
	if tk.Meta().Status != tasks.StatusSucceeded {
		t.Fatalf("重命名应成功，实际 %s: %v", tk.Meta().Status, tk.Meta().Error)
	}
	r := diskTaskInstallResult(t, tk)
	if !stepsContain(r.Steps, "卷名=ZPArchive") {
		t.Errorf("回读应确认新卷名，实际 %v", r.Steps)
	}
	if !w.called("rename disk4s1 ZPArchive") {
		t.Errorf("命令构造不对：%v", w.callList())
	}
}

func TestDiskVolumeRenameRejectsWholeDisk(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk9/volume-rename",
		map[string]any{"name": "X", "confirm": "disk9",
			"expect_uuid": targetDisk9UUID, "expect_size": int64(1024209543168)}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("整盘不能重命名，应 400，实际 %d (%v)", res.StatusCode, out["msg"])
	}
	if w.called("rename disk9") {
		t.Error("整盘重命名必须被拦在命令之前")
	}
	noDiskTaskCreated(t, srv, "整盘重命名")
}

func TestDiskVolumeCreateRejectsNoContainer(t *testing.T) {
	w := newFakeWorld()
	srv, ts, cookies := newDiskServer(t, w)
	// disk0s3 在 fake 里是 EFI 之外的分区；用 disk9s1（EFI，无容器）
	res, out, _ := doJSON(t, ts, "POST", "/api/v1/system/disks/disk9s1/volume-create",
		map[string]any{"name": "X", "confirm": "disk9s1"}, cookies)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("没有 APFS 容器应 400，实际 %d (%v)", res.StatusCode, out["msg"])
	}
	if w.called("apfs addVolume") {
		t.Error("没有容器时绝不能执行 addVolume")
	}
	noDiskTaskCreated(t, srv, "无容器建卷")
}

func TestLookupDiskFSAndKeys(t *testing.T) {
	for _, k := range []string{"apfs", "APFS", "ExFAT", "hfs+", "fat32", "keep"} {
		if _, okk := lookupDiskFS(k); !okk {
			t.Errorf("%q 应能查到", k)
		}
	}
	if _, okk := lookupDiskFS("zfs"); okk {
		t.Error("zfs 不应存在")
	}
	if !strings.Contains(diskFSKeys(), "exfat") {
		t.Error("diskFSKeys 应列出 exfat")
	}
}

func TestDiskSourceHasNoGuiAuthorizationPrompt(t *testing.T) {
	for _, f := range []string{"api_disks.go", "api_disks_plist.go", "api_disks_ops.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(src))
		for _, bad := range diskForbiddenGUIs {
			if strings.Contains(lower, strings.ToLower(bad)) {
				t.Errorf("%s 里出现了会弹 GUI 授权窗的调用 %q —— 无头机器上会永久挂起", f, bad)
			}
		}
	}
}

func TestDiskRoutesRequireAuth(t *testing.T) {
	_, ts, _ := newDiskServer(t, newFakeWorld())
	res, _, _ := doJSON(t, ts, "GET", "/api/v1/system/disks", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET disks 未登录应 401，实际 %d", res.StatusCode)
	}
	res, _, _ = doJSON(t, ts, "POST", "/api/v1/system/disks/disk4s1/mount", map[string]any{}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST mount 未登录应 401，实际 %d", res.StatusCode)
	}
}
