package services

import (
	"strings"
	"testing"
)

// TestFilebrowserReleaseBinaryWiring 锁住 filebrowser 原生安装的关键声明。
//
// 这些字段错一个就会表现成"条目在市场里、装的却是别的东西"或"改完主目录没生效"，
// 所以逐项断言。
func TestFilebrowserReleaseBinaryWiring(t *testing.T) {
	spec, ok := releaseBinaryApps["filebrowser"]
	if !ok {
		t.Fatal("releaseBinaryApps 里必须有 filebrowser")
	}
	if spec.Repo != "filebrowser/filebrowser" || spec.Tag != "v2.63.23" ||
		spec.Asset != "darwin-arm64-filebrowser.tar.gz" {
		t.Errorf("release 三元组不对：%s/%s/%s", spec.Repo, spec.Tag, spec.Asset)
	}
	if spec.Binary != "filebrowser" || spec.TarStrip != 0 || !spec.PickBinary {
		t.Errorf("解包参数不对：binary=%q strip=%d pick=%v", spec.Binary, spec.TarStrip, spec.PickBinary)
	}
	if spec.ChecksumAsset != "filebrowser_2.63.23_checksums.txt" {
		t.Errorf("上游有 sha256 清单就必须登记：%q", spec.ChecksumAsset)
	}
	// 端口与绑定：只绑回环（家目录暴露面必须收住）。
	if spec.Port != 8081 {
		t.Errorf("端口应是 8081，实际 %d", spec.Port)
	}
	if spec.BindAddress != "127.0.0.1" {
		t.Errorf("只应绑回环 127.0.0.1，实际 %q", spec.BindAddress)
	}
	if !spec.CheckPortConflict {
		t.Error("端口语义强的应用必须开 CheckPortConflict（否则会把别人的端口当成自己装好了）")
	}
	// 启动参数：子路径、根目录=家目录、数据库在家目录之外、只绑回环。
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{"-b /filebrowser", "-a 127.0.0.1", "-r {home}", "-d {vardir}/filebrowser/filebrowser.db"} {
		if !strings.Contains(joined, want) {
			t.Errorf("启动参数里缺少 %q，实际：%s", want, joined)
		}
	}
	// 描述符必须注册（否则 web 层不会分流到安装器）。
	d, ok := FindDescriptor("filebrowser")
	if !ok || d.Rail != RailTarball {
		t.Fatalf("filebrowser 描述符缺失或不在 tarball 轨：ok=%v rail=%v", ok, d.Rail)
	}
	if d.Service.Label != FilebrowserServiceLabel {
		t.Errorf("launchd label 不一致：描述符 %q，常量 %q", d.Service.Label, FilebrowserServiceLabel)
	}
}

// TestFilebrowserPasswordRegex 用**真实日志行**锁住"用户名 + 口令都要抓出来"。
//
// 真实格式取自 v2.63.23 二进制的格式串与 2026-09-19 本机实测输出。
func TestFilebrowserPasswordRegex(t *testing.T) {
	line := "2026/09/19 14:58:06 User 'admin' initialized with randomly generated password: OMh0r3E_RmVzNFrx"
	mm := filebrowserInitialCredRe.FindStringSubmatch(line)
	if len(mm) != 3 {
		t.Fatalf("应同时抓到用户名与口令，实际 %v", mm)
	}
	if mm[1] != "admin" || mm[2] != "OMh0r3E_RmVzNFrx" {
		t.Errorf("抓错了：user=%q pass=%q", mm[1], mm[2])
	}

	// 非 admin 用户名也必须照实抓（不许把 admin 写死当默认）。
	line2 := "2026/09/19 15:00:00 User 'alice' initialized with randomly generated password: aB3_x-9Q"
	mm2 := filebrowserInitialCredRe.FindStringSubmatch(line2)
	if len(mm2) != 3 || mm2[1] != "alice" || mm2[2] != "aB3_x-9Q" {
		t.Errorf("自定义用户名/含连字符口令抓取失败：%v", mm2)
	}

	// 兜底：只有口令、没有用户名时，只抓口令，不编用户名。
	only := filebrowserPasswordOnlyRe.FindStringSubmatch("randomly generated password: zz9_KK")
	if len(only) != 2 || only[1] != "zz9_KK" {
		t.Errorf("兜底正则没抓到口令：%v", only)
	}
}

// TestPlistArgValueRoundTrip 锁住"改 -r 之后能回读到新值"这一核心闭环。
func TestPlistArgValueRoundTrip(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>ProgramArguments</key>
    <array>
        <string>/Users/me/filebrowser/filebrowser</string>
        <string>-b</string>
        <string>/filebrowser</string>
        <string>-a</string>
        <string>127.0.0.1</string>
        <string>-p</string>
        <string>8081</string>
        <string>-r</string>
        <string>/Users/me</string>
        <string>-d</string>
        <string>/opt/zizpanel/work/filebrowser/filebrowser.db</string>
    </array>
</dict>
</plist>
`
	got, ok := plistArgValue(plist, "-r")
	if !ok || got != "/Users/me" {
		t.Fatalf("读取 -r 失败：got=%q ok=%v", got, ok)
	}
	if got, _ := plistArgValue(plist, "-d"); got != "/opt/zizpanel/work/filebrowser/filebrowser.db" {
		t.Errorf("-d 读取错位：%q", got)
	}

	updated, err := replacePlistArgValue(plist, "-r", "/Users/me/www")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := plistArgValue(updated, "-r"); got != "/Users/me/www" {
		t.Errorf("替换后回读 = %q，期望 /Users/me/www", got)
	}
	// 不能误改相邻参数（-d 必须原样）。
	if got, _ := plistArgValue(updated, "-d"); got != "/opt/zizpanel/work/filebrowser/filebrowser.db" {
		t.Errorf("替换 -r 时误改了 -d：%q", got)
	}
	// 找不到参数时如实报错，不静默成功。
	if _, err := replacePlistArgValue(plist, "--nope", "/x"); err == nil {
		t.Error("找不到参数时必须报错")
	}
}
