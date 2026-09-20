package services

import (
	"strings"
	"testing"
)

// ============================================================================
//  File Browser 口令重置：解析与脱敏的门禁
//
//  夹具是 2026-09-20 在本机 v2.63.23 上实测输出的原样拷贝（含两行 stderr 日志、
//  带空格的表头、以及 Admin 列在数据行里的实际位置）。
// ============================================================================

const fbUsersLSReal = `2026/09/20 21:32:08 No config file used
2026/09/20 21:32:08 Using database: /tmp/zp-fbprobe.HtKT1r/probe.db
ID  Username  Scope  Locale  V. Mode  S.Click  Red. After C/M  Admin  Execute  Create  Rename  Modify  Delete  Share  Download  Pwd Lock
1   zizdog    /      en      list     false    false           true   true     true    true    true    true    true   true      false
2   bob       /      en      list     false    false           false  false    false   true    true    false   true   true      false`

// TestParseFilebrowserUsernamePicksAdmin 用户名不许写死，且多用户时要挑管理员。
func TestParseFilebrowserUsernamePicksAdmin(t *testing.T) {
	got, err := parseFilebrowserUsername(fbUsersLSReal)
	if err != nil {
		t.Fatalf("解析真实输出失败：%v", err)
	}
	if got != "zizdog" {
		t.Errorf("应取管理员 zizdog（不写死用户名），实际 %q", got)
	}
}

// 只有一个用户时（真机现状）也必须能取到，且不是靠"第一行第一列"的巧合。
func TestParseFilebrowserUsernameSingleUser(t *testing.T) {
	out := `2026/09/20 21:32:08 No config file used
ID  Username  Scope  Locale  V. Mode  S.Click  Red. After C/M  Admin  Execute  Create  Rename  Modify  Delete  Share  Download  Pwd Lock
1   alice     /      en      list     false    false           false  false    false   true    true    true    true   true      false`
	got, err := parseFilebrowserUsername(out)
	if err != nil || got != "alice" {
		t.Fatalf("单用户应取到 alice，实际 %q err=%v", got, err)
	}
}

// 多个非管理员用户时不许猜：如实报错让用户自己决定。
func TestParseFilebrowserUsernameRefusesAmbiguous(t *testing.T) {
	out := `ID  Username  Scope  Locale  V. Mode  S.Click  Red. After C/M  Admin  Execute  Create  Rename  Modify  Delete  Share  Download  Pwd Lock
1   alice     /      en      list     false    false           false  false    false   true    true    true    true   true      false
2   bob       /      en      list     false    false           false  false    false   true    true    false   true   true      false`
	if _, err := parseFilebrowserUsername(out); err == nil {
		t.Fatal("多个非管理员用户时必须报错，绝不猜一个去改口令")
	}
}

func TestParseFilebrowserConfigRoot(t *testing.T) {
	out := "Sign up:                  false\n\nServer:\n  Log:                       stdout\n  Root:                      /Users/me/filebrowser\n\nTUS:\n"
	if got := parseFilebrowserConfigRoot(out); got != "/Users/me/filebrowser" {
		t.Errorf("config cat 的 Root 解析错：%q", got)
	}
}

// 口令绝不能出现在命令标签或命令输出里（那两处会进任务日志与审计）。
func TestFilebrowserRedactHidesPassword(t *testing.T) {
	const pw = "Zz_Custom_Pass_1234"
	label := strings.Join([]string{"/Users/me/filebrowser/filebrowser", "-d", "/x/filebrowser.db",
		"users", "update", "zizdog", "--password", pw, "--perm.execute=false"}, " ")
	if got := filebrowserRedact(label, pw); strings.Contains(got, pw) {
		t.Fatalf("命令标签里仍然有口令：%s", got)
	} else if !strings.Contains(got, "****") {
		t.Errorf("口令应被替换成 ****，实际：%s", got)
	}
	out := "user zizdog updated with password " + pw
	if got := filebrowserRedact(out, pw); strings.Contains(got, pw) {
		t.Fatalf("命令输出里仍然有口令：%s", got)
	}
	// 没有口令时原样返回（别把正常输出改坏）。
	if got := filebrowserRedact("plain output", ""); got != "plain output" {
		t.Errorf("空口令不该改动文本：%q", got)
	}
}

// plist 是三条路径（二进制 / 库 / 根目录）的唯一真实来源。
func TestFilebrowserPlistArgs(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>ProgramArguments</key>
    <array>
        <string>/Users/me/filebrowser/filebrowser</string>
        <string>-b</string>
        <string>/filebrowser</string>
        <string>-r</string>
        <string>/Users/me/filebrowser</string>
        <string>-d</string>
        <string>/opt/zizpanel/work/filebrowser/filebrowser.db</string>
    </array>
</dict>
</plist>`
	bin, db, root := filebrowserPlistArgs(plist)
	if bin != "/Users/me/filebrowser/filebrowser" {
		t.Errorf("二进制解析错：%q", bin)
	}
	if db != "/opt/zizpanel/work/filebrowser/filebrowser.db" {
		t.Errorf("-d 解析错：%q", db)
	}
	if root != "/Users/me/filebrowser" {
		t.Errorf("-r 解析错：%q", root)
	}
}

func TestArgFlagValue(t *testing.T) {
	args := []string{"-b", "/filebrowser", "-r", "/Users/me", "-d", "/x/fb.db"}
	if got := argFlagValue(args, "-d"); got != "/x/fb.db" {
		t.Errorf("-d = %q", got)
	}
	if got := argFlagValue(args, "-p"); got != "" {
		t.Errorf("不存在的 flag 应返回空，实际 %q", got)
	}
}
