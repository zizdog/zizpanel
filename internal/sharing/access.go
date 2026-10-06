package sharing

import (
	"context"
	"strings"
)

// GroupInfo 是 SMB 访问控制组（com.apple.access_smb）的真实信息。
// Verified=false ⇒ 读不到，界面必须显示「未复核」，不许编成员名单。
type GroupInfo struct {
	Group  string `json:"group"`
	Exists bool   `json:"exists"`
	// Members 是 `dseditgroup -o read` 里 GroupMembership 的原文成员（短名或 GUID）。
	Members  []string `json:"members"`
	Verified bool     `json:"verified"`
	Error    string   `json:"error,omitempty"`
	// User / UserMember：本机真实用户是否已在组里（checkmember 的权威结论）。
	User            string `json:"user,omitempty"`
	UserMember      bool   `json:"user_member"`
	UserMemberKnown bool   `json:"user_member_known"`
}

// AccessGroup 只读查询 SMB 访问组（AccessGroupNamed 的固定组版本）。
func (e *Executor) AccessGroup(ctx context.Context, userName string) GroupInfo {
	return e.AccessGroupNamed(ctx, AccessSMBGroup, userName)
}

// AccessGroupNamed 只读查询任意一个 `com.apple.access_*` 服务 ACL。**不修改**任何组成员关系
// （改组成员是敏感动作，面板不自动做，只在界面给指引）。
//
// 屏幕共享（com.apple.access_screensharing）与 SMB 是同一套机制：**组存在=只允许成员，
// 组不存在=不限制**；所以这里只把组名参数化，判定逻辑一字不动。
func (e *Executor) AccessGroupNamed(ctx context.Context, group, userName string) GroupInfo {
	group = strings.TrimSpace(group)
	info := GroupInfo{Group: group, User: userName, Members: []string{}}
	r := e.run(ctx, "dseditgroup", "-o", "read", group)
	out := r.Combined()
	switch {
	case r.Err == nil:
		info.Exists, info.Verified = true, true
		info.Members = parseGroupMembers(r.Stdout)
	case strings.Contains(out, "Group not found") || r.ExitCode == 64:
		// 真机实测：组不存在时退出码 64 + "Group not found."。
		info.Exists, info.Verified = false, true
	default:
		info.Error = "读 " + group + " 失败：" + tail(out, 200)
		return info
	}
	if !info.Exists || strings.TrimSpace(userName) == "" {
		return info
	}
	// 权威成员判定：dseditgroup -o checkmember（真机输出 "yes <user> is a member of <group>"）。
	c := e.run(ctx, "dseditgroup", "-o", "checkmember", "-m", userName, group)
	low := strings.ToLower(strings.TrimSpace(c.Combined()))
	switch {
	case c.Err == nil && strings.HasPrefix(low, "yes"):
		info.UserMember, info.UserMemberKnown = true, true
	case c.Err == nil && strings.HasPrefix(low, "no"):
		info.UserMember, info.UserMemberKnown = false, true
	case strings.Contains(low, "group not found") || c.ExitCode == 64:
		info.Exists = false
	default:
		if info.Error == "" {
			info.Error = "checkmember 读不到结论：" + tail(low, 160)
		}
	}
	return info
}

// parseGroupMembers 取 `dseditgroup -o read` 输出里 GroupMembership 段落的成员行。
func parseGroupMembers(text string) []string {
	out := []string{}
	in := false
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(ln, "dsAttrTypeStandard:") {
			in = strings.HasPrefix(ln, "dsAttrTypeStandard:GroupMembership")
			continue
		}
		if !in {
			continue
		}
		t := strings.TrimSpace(ln)
		if t == "" || t == "-" {
			continue
		}
		out = append(out, t)
	}
	return out
}
