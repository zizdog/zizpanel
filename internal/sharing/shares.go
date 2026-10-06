package sharing

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// Share 是一条 SMB 共享点（来自 `sharing -l`）。
type Share struct {
	// Name 是给客户端看的 SMB 名（`smb_name`）：面板列表与 smb:// 地址用它。
	Name string `json:"name"`
	// RecordName 是共享点**记录名**（`sharing -l` 的顶层 name）——`sharing -e/-r`
	// 认的是它。面板加共享时只给了 -S，记录名默认取目录名，两者可能不一样
	// （真机：/Volumes/ZPMirror 的记录名是 ZPMirror、SMB 名是 zpmirror）。
	// 所以删/改一律用记录名：按 SMB 名去删会找不到那条记录。
	RecordName string `json:"record_name,omitempty"`
	Path       string `json:"path"`
	ReadOnly   bool   `json:"read_only"`
	// Managed 表示这一条看起来是本面板建的（名字/路径都合法且路径在共享目录里）；
	// 只用于展示提示，不参与任何写判定。
	Managed bool `json:"managed,omitempty"`
}

// sharingJSONShare 是 `sharing -l -f json` 的值结构（真机实测字段名）。
type sharingJSONShare struct {
	Path        string `json:"path"`
	SMBName     string `json:"smb_name"`
	SMBShared   int    `json:"smb_shared"`
	SMBReadOnly int    `json:"smb_read_only"`
}

// ListShares 读一次共享点列表。known=false 表示读不到（界面标「未复核」）。
func (e *Executor) ListShares(ctx context.Context) (shares []Share, known bool, errMsg string) {
	c := e.run(ctx, "sharing", "-l", "-f", "json")
	if c.Err == nil {
		if list, ok := parseSharingJSON(c.Stdout); ok {
			return list, true, ""
		}
	}
	// JSON 形态不可用（老系统 / 异常输出）时退回文本形态。
	t := e.run(ctx, "sharing", "-l")
	if t.Err == nil {
		if list, ok := parseSharingText(t.Stdout); ok {
			return list, true, ""
		}
	}
	msg := c.Combined()
	if msg == "" {
		msg = t.Combined()
	}
	if c.Err != nil && t.Err != nil {
		return nil, false, "sharing -l 读不到：" + tail(msg, 300)
	}
	return nil, false, "sharing -l 的输出解析不了：" + tail(msg, 300)
}

func parseSharingJSON(text string) ([]Share, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, true // 没有共享点：sharing 会打空/花括号
	}
	if !strings.HasPrefix(text, "{") {
		return nil, false
	}
	var raw map[string]sharingJSONShare
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, false
	}
	out := []Share{}
	for record, v := range raw {
		if v.SMBShared != 1 {
			continue
		}
		name := record
		if strings.TrimSpace(v.SMBName) != "" {
			name = v.SMBName
		}
		out = append(out, Share{Name: name, RecordName: record, Path: v.Path, ReadOnly: v.SMBReadOnly != 0})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, true
}

// parseSharingText 解析 `sharing -l` 的文本形态（真机形状见 api_sharing_gate_test.go 注释）。
func parseSharingText(text string) ([]Share, bool) {
	var out []Share
	var cur Share
	inSMB, havePath := false, false
	flush := func() {
		if havePath && inSMB {
			out = append(out, cur)
		}
		cur, inSMB, havePath = Share{}, false, false
	}
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		switch {
		case strings.HasPrefix(t, "name:"):
			// 顶层 name = 记录名；smb: { … name: } 里那个 = SMB 显示名（两者可能不同）。
			v := strings.TrimSpace(strings.TrimPrefix(t, "name:"))
			if inSMB {
				cur.Name = v
				continue
			}
			cur.RecordName = v
			cur.Name = v
		case strings.HasPrefix(t, "path:"):
			cur.Path = strings.TrimSpace(strings.TrimPrefix(t, "path:"))
			havePath = true
		case strings.HasPrefix(t, "smb:"):
			inSMB = true
		case strings.HasPrefix(t, "shared:"):
			if strings.TrimSpace(strings.TrimPrefix(t, "shared:")) != "1" {
				inSMB = false
			}
		case strings.HasPrefix(t, "read-only:"):
			cur.ReadOnly = strings.TrimSpace(strings.TrimPrefix(t, "read-only:")) == "1"
		case t == "}":
			flush()
		}
	}
	flush()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, true
}

func canonPath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// shareNamed 在列表里按名字找一条（名字大小写不敏感，macOS 共享名不区分）。
func shareNamed(list []Share, name string) (Share, bool) {
	for _, s := range list {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return Share{}, false
}

// shareByRecord 按**记录名**找一条（`sharing -e/-r` 用的就是它）。
func shareByRecord(list []Share, record string) (Share, bool) {
	for _, s := range list {
		if strings.EqualFold(s.RecordName, record) {
			return s, true
		}
	}
	return Share{}, false
}

// AddShare 新增/更新一条 SMB 共享点，成功只认 `sharing -l` 回读。
func (e *Executor) AddShare(ctx context.Context, path, name string, readOnly bool) Result {
	res := Result{Action: "share_add"}
	args := []string{"-a", path, "-S", name, "-s", "001"}
	if readOnly {
		args = append(args, "-R", "1")
	}
	e.execSeq(ctx, &res, "sharing", args...)
	res.Command = strings.Join(res.Commands, " && ")

	list, known, msg := e.ListShares(ctx)
	if !known {
		res.Error = "命令已执行，但回读不到共享列表（未复核，不敢报成功）：" + msg
		return res
	}
	got, found := shareNamed(list, name)
	if !found {
		res.Error = "命令退出码 0，但回读 `sharing -l` 里没有「" + name + "」（退出码不算数）"
		return res
	}
	if canonPath(got.Path) != canonPath(path) {
		res.Error = "回读到的共享路径不一致：期望 " + path + "，实际 " + got.Path
		return res
	}
	if got.ReadOnly != readOnly {
		res.Error = "回读到的只读设置不一致：期望 " + boolText(readOnly) + "，实际 " + boolText(got.ReadOnly)
		return res
	}
	res.Verified, res.OK = true, true
	return res
}

// RemoveShare 删除一条 SMB 共享点，成功只认回读。
//
// recordName 是**记录名**（Share.RecordName）：`sharing -r` 认这个，不认 SMB 显示名。
func (e *Executor) RemoveShare(ctx context.Context, recordName string) Result {
	res := Result{Action: "share_remove"}
	// 先回读一次：本来就没有的共享点直接如实说"没有"，不跑删除命令。
	before, knownBefore, msgBefore := e.ListShares(ctx)
	if !knownBefore {
		res.Error = "回读不到共享列表（未复核，不敢动手）：" + msgBefore
		return res
	}
	if _, found := shareByRecord(before, recordName); !found {
		res.Missing = true
		res.Error = "没有找到共享记录「" + recordName + "」"
		return res
	}
	e.execSeq(ctx, &res, "sharing", "-r", recordName)
	res.Command = strings.Join(res.Commands, " && ")

	list, known, msg := e.ListShares(ctx)
	if !known {
		res.Error = "命令已执行，但回读不到共享列表（未复核，不敢报成功）：" + msg
		return res
	}
	if _, found := shareByRecord(list, recordName); found {
		res.Error = "命令退出码 0，但回读 `sharing -l` 里「" + recordName + "」还在（退出码不算数）"
		return res
	}
	res.Verified, res.OK = true, true
	return res
}

// UpdateShare 改一条已有 SMB 共享点：SMB 显示名（smbName 空 = 不改）与只读
// （readOnly 为 nil = 不改）。recordName 是记录名（Share.RecordName）。
//
// 用 `sharing -e <记录名>`（上游就为"编辑已有共享点"提供了这个动词，见 man sharing）：
// `-S` 改 SMB 名、`-R 0/1` 改只读、`-s 001` 保证 SMB 共享仍是开着的（面板列出来的
// 都是已共享的，这一步是幂等的）。改完必须回读对上才算成功。
func (e *Executor) UpdateShare(ctx context.Context, recordName, smbName string, readOnly *bool) Result {
	res := Result{Action: "share_update"}
	before, knownBefore, msgBefore := e.ListShares(ctx)
	if !knownBefore {
		res.Error = "回读不到共享列表（未复核，不敢动手）：" + msgBefore
		return res
	}
	cur, found := shareByRecord(before, recordName)
	if !found {
		res.Missing = true
		res.Error = "没有找到共享记录「" + recordName + "」"
		return res
	}
	wantName := cur.Name
	if strings.TrimSpace(smbName) != "" {
		wantName = strings.TrimSpace(smbName)
	}
	wantRO := cur.ReadOnly
	if readOnly != nil {
		wantRO = *readOnly
	}
	if wantName == cur.Name && wantRO == cur.ReadOnly {
		res.Verified, res.OK = true, true
		res.Command = "（目标值与现状一致，未执行命令）"
		return res
	}
	args := []string{"-e", recordName, "-s", "001"}
	if wantName != cur.Name {
		args = append(args, "-S", wantName)
	}
	if wantRO != cur.ReadOnly {
		args = append(args, "-R", boolNum(wantRO))
	}
	e.execSeq(ctx, &res, "sharing", args...)
	res.Command = strings.Join(res.Commands, " && ")

	list, known, msg := e.ListShares(ctx)
	if !known {
		res.Error = "命令已执行，但回读不到共享列表（未复核，不敢报成功）：" + msg
		return res
	}
	got, found := shareByRecord(list, recordName)
	if !found {
		res.Error = "命令退出码 0，但回读 `sharing -l` 里记录「" + recordName + "」不见了"
		return res
	}
	if got.Name != wantName {
		res.Error = "回读到的 SMB 名不一致：期望 " + wantName + "，实际 " + got.Name
		return res
	}
	if got.ReadOnly != wantRO {
		res.Error = "回读到的只读设置不一致：期望 " + boolText(wantRO) + "，实际 " + boolText(got.ReadOnly)
		return res
	}
	res.Verified, res.OK = true, true
	return res
}

// boolNum 把布尔转成 sharing 的 0/1 参数。
func boolNum(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func boolText(b bool) string {
	if b {
		return "只读"
	}
	return "读写"
}
