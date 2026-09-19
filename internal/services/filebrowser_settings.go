package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  File Browser「主目录」设置
//
//  用户要求（2026-09-19）：面板里能改 File Browser 的文件根目录，改完重启服务生效，
//  并且要**回读生效值**（读不到就说"未复核"）。
//
//  为什么不能只改它数据库里的 config.root（filebrowser CLI `config set --root`）：
//  实测（2026-09-19，v2.63.23）——
//    · 它读取配置的优先级是 **启动参数 > 环境变量 > 配置文件 > 数据库值 > 默认值**，
//      而我们的 plist 里始终带 `-r`，所以数据库里的 root 永远被 `-r` 盖住；
//    · `config set` 在实例运行时会因 Bolt 库被锁而 `Error: timeout`，必须先停服务。
//  所以面板改的是 **launchd plist 里的 -r 参数**，改完重新 bootstrap，再读回来核对。
//  ============================================================================

// FilebrowserServiceLabel 是 File Browser 的 launchd label（与目录 ServiceLabel 同源）。
const FilebrowserServiceLabel = "com.zizdog.filebrowser"

// FilebrowserRootInfo 是一次「主目录」读取/修改的结果。
type FilebrowserRootInfo struct {
	AppID       string `json:"app_id"`
	Installed   bool   `json:"installed"`
	Root        string `json:"root"`
	DefaultRoot string `json:"default_root"`
	Plist       string `json:"plist"`
	// RootSource 说明这个值是从哪读回来的（只有 plist 一条真实来源）。
	RootSource string `json:"root_source"`
	// Verified=true 表示改完之后**真的回读到**新值且端口在监听；
	// false 且 Changed 时要在 Note 里说清"未复核"。
	Verified bool   `json:"verified"`
	Changed  bool   `json:"changed,omitempty"`
	Note     string `json:"note,omitempty"`
}

// FilebrowserDescriptor 返回 File Browser 的描述符（找不到时 ok=false）。
func FilebrowserDescriptor() (AppDescriptor, bool) { return FindDescriptor("filebrowser") }

// FilebrowserRootInfo 读取当前「主目录」：真实来源是 launchd plist 里的 `-r` 参数。
func (m *Manager) FilebrowserRootInfo() (FilebrowserRootInfo, error) {
	d, ok := FilebrowserDescriptor()
	if !ok {
		return FilebrowserRootInfo{}, fmt.Errorf("面板内部错误：找不到 filebrowser 的描述符")
	}
	p := m.binaryReleasePathsFor(d)
	info := FilebrowserRootInfo{
		AppID: "filebrowser", Plist: p.Plist,
		DefaultRoot: m.opt.UserHome, RootSource: "launchd plist 里的 -r 参数（" + p.Plist + "）",
	}
	data, err := os.ReadFile(p.Plist)
	if err != nil {
		return info, nil // 没装/没 plist：Installed=false，Root 为空
	}
	info.Installed = true
	root, ok := plistArgValue(string(data), "-r")
	if !ok {
		info.Note = "plist 里没有找到 -r 参数（可能是旧版本安装的），请重新安装 File Browser 后再改主目录"
		return info, nil
	}
	info.Root = root
	return info, nil
}

// SetFilebrowserRoot 改 plist 里的 `-r`、重启服务、并**回读核对**。
//
// 校验：新目录必须是绝对路径、存在、且是目录（不存在就拒绝，避免把一个打不开的
// 根目录写进去 —— filebrowser 起得来但页面 404/403，用户会以为面板坏了）。
func (m *Manager) SetFilebrowserRoot(ctx context.Context, newRoot string) (FilebrowserRootInfo, error) {
	d, ok := FilebrowserDescriptor()
	if !ok {
		return FilebrowserRootInfo{}, fmt.Errorf("面板内部错误：找不到 filebrowser 的描述符")
	}
	root := strings.TrimSpace(newRoot)
	if root == "" {
		return FilebrowserRootInfo{}, fmt.Errorf("主目录不能为空")
	}
	if !filepath.IsAbs(root) {
		return FilebrowserRootInfo{}, fmt.Errorf("主目录必须是绝对路径，例如 %s", filepath.Join(m.opt.UserHome, "www"))
	}
	st, err := os.Stat(root)
	if err != nil {
		return FilebrowserRootInfo{}, fmt.Errorf("目录 %s 不存在或不可访问：%v", root, err)
	}
	if !st.IsDir() {
		return FilebrowserRootInfo{}, fmt.Errorf("%s 不是目录（File Browser 的 -r 只能指向目录）", root)
	}
	p := m.binaryReleasePathsFor(d)
	data, err := os.ReadFile(p.Plist)
	if err != nil {
		return FilebrowserRootInfo{}, fmt.Errorf("读不到 launchd 配置 %s（File Browser 可能没装好）：%v", p.Plist, err)
	}
	updated, err := replacePlistArgValue(string(data), "-r", root)
	if err != nil {
		return FilebrowserRootInfo{}, err
	}
	if err := os.WriteFile(p.Plist, []byte(updated), 0o644); err != nil {
		return FilebrowserRootInfo{}, fmt.Errorf("写入 launchd 配置 %s 失败：%w", p.Plist, err)
	}
	// 重新装载（bootout + bootstrap + 重试），让它读新的 plist。
	if err := m.bootstrapService(ctx, d.Service.Label, p.Plist); err != nil {
		return FilebrowserRootInfo{}, fmt.Errorf("已改配置但重启服务失败：%w", err)
	}
	// 等端口真的起来（最多 ~10 秒），再回读。
	portUp := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.portHasListener(d.Port) {
			portUp = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	info, _ := m.FilebrowserRootInfo()
	info.Changed = true
	switch {
	case info.Root == root && portUp:
		info.Verified = true
		info.Note = fmt.Sprintf("已回读：plist 的 -r = %s，且端口 %d 在监听。", info.Root, d.Port)
	case info.Root == root && !portUp:
		info.Verified = false
		info.Note = fmt.Sprintf("配置已写成 %s 并已请求重启，但端口 %d 没在 10 秒内监听 —— "+
			"**未复核**，请到「服务管理 → File Browser → 日志」看它有没有起来。", root, d.Port)
	default:
		info.Verified = false
		info.Note = fmt.Sprintf("写入后回读到的 -r 是 %q，与期望的 %q 不一致 —— **未复核**，请检查 %s",
			info.Root, root, p.Plist)
	}
	return info, nil
}

// plistArgValue 从 plist 文本里取出 `<string>-r</string>` 后面那个 <string> 的值。
func plistArgValue(plist, flag string) (string, bool) {
	lines := strings.Split(plist, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "<string>"+flag+"</string>" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			v, ok := plistStringValue(lines[j])
			if ok {
				return v, true
			}
		}
	}
	return "", false
}

// replacePlistArgValue 把 `<string>flag</string>` 后面那个 <string> 的值换掉。
func replacePlistArgValue(plist, flag, value string) (string, error) {
	lines := strings.Split(plist, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "<string>"+flag+"</string>" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if !strings.HasPrefix(t, "<string>") || !strings.HasSuffix(t, "</string>") {
				continue
			}
			indent := lines[j][:len(lines[j])-len(strings.TrimLeft(lines[j], " \t"))]
			lines[j] = indent + "<string>" + xmlEscape(value) + "</string>"
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", fmt.Errorf("plist 里找不到 %s 参数，无法修改（请重新安装 File Browser）", flag)
}

// plistStringValue 解析一行 `<string>值</string>`。
func plistStringValue(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "<string>") || !strings.HasSuffix(t, "</string>") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(t, "<string>"), "</string>"), true
}
