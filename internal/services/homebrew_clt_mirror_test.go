package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMacMajorVersionFromDarwin 锁住 Darwin 主版本与 macOS 主版本的映射。
//
// 真机：macOS 15.7.9 的 `sysctl -n kern.osrelease` = 24.x。
// 这个映射决定"挑哪份 CLT 包"，算错就是把不兼容的工具链装到机器上。
func TestMacMajorVersionFromDarwin(t *testing.T) {
	cases := []struct{ darwin, want int }{
		{0, 0}, // 非 darwin / 拿不到版本 → 调用方跳过镜像路径
		{20, 11},
		{23, 14},
		{24, 15},
		{25, 16},
	}
	for _, c := range cases {
		if got := macMajorVersion(c.darwin); got != c.want {
			t.Errorf("macMajorVersion(%d) = %d，期望 %d", c.darwin, got, c.want)
		}
	}
}

// TestPickCLTItemSkipsIncompatible 锁住"清单里挑第一份适用的包集合"。
//
// 清单是运营数据（镜像上随时可能改），但选错会导致装不上/装错版本，
// 而且真机上极难复盘，所以用单测把规则钉死：
//   - MaxOS 是**上限**（CLT 只要求 macOS 不高于它编译时对应的版本）；
//   - MaxOS=0 表示不限制，永远可用（兜底条目）；
//   - 空目录 / 空包列表的条目直接跳过。
func TestPickCLTItemSkipsIncompatible(t *testing.T) {
	items := []cltIndexItem{
		{Name: "坏条目", Dir: "", Pkgs: []string{"CLTools_Executables.pkg"}},
		{Name: "16.2", Path: "clt/072-44426-A", MaxOS: 15, Pkgs: []string{"CLTools_Executables.pkg"}},
		{Name: "更高版本", Dir: "16.4", MaxOS: 16, Pkgs: []string{"CLTools_Executables.pkg"}},
		{Name: "兜底", Dir: "generic", MaxOS: 0, Pkgs: []string{"CLTools_Executables.pkg"}},
	}

	// 顺序即优先级：清单由运营维护，"上次在这台机器上成功过的那条排最前"
	got, ok := pickCLTItem(items, 15)
	if !ok || got.Name != "16.2" {
		t.Fatalf("macOS 15 应挑到排最前的 16.2，实际 %+v ok=%v", got, ok)
	}
	// 空目录条目必须被跳过（否则会拼出 <mirror>/clt//CLTools_Executables.pkg）
	if strings.Contains(got.filesBase("https://m"), "//CLTools") {
		t.Errorf("包地址拼错了：%q", got.filesBase("https://m"))
	}

	got, ok = pickCLTItem(items, 26)
	if !ok || got.Name != "兜底" {
		t.Fatalf("macOS 26 应落到兜底条目，实际 %+v ok=%v", got, ok)
	}

	if _, ok := pickCLTItem(nil, 15); ok {
		t.Error("空清单不该挑出任何东西")
	}
}

// TestCLTNeededPkgsOnlyWhitelist 锁住"真的只下需要的那两个包"。
//
// 苹果的 CLT 产品里还有 SwiftBackDeploy / LMOS_SDK / DevSDK_Remove_* 一类组件，
// 全下要多 105 MB。国内链路上这 105 MB 是实打实的等待时间，所以只装：
// CLTools_Executables.pkg（真工具链）+ CLTools_macOSNMOS_SDK.pkg（macOS SDK）。
func TestCLTNeededPkgsOnlyWhitelist(t *testing.T) {
	item := cltIndexItem{
		Dir: "16.2",
		Pkgs: []string{
			"CLTools_macOS_DevSDK_Remove_macOS13.pkg", // 5390 字节的清理包
			"CLTools_SwiftBackDeploy.pkg",
			"CLTools_macOSNMOS_SDK.pkg",
			"CLTools_Executables.pkg",
			"CLTools_macOSLMOS_SDK.pkg",
		},
	}
	got := strings.Join(cltNeededPkgs(item), ",")
	want := "CLTools_Executables.pkg,CLTools_macOSNMOS_SDK.pkg" // 顺序固定，Executables 先
	if got != want {
		t.Errorf("cltNeededPkgs = %q，期望 %q", got, want)
	}
	// 清单里缺组件不算致命（有的版本没有 NMOS_SDK），但不能返回空
	if got := cltNeededPkgs(cltIndexItem{Dir: "x", Pkgs: []string{"CLTools_macOSLMOS_SDK.pkg"}}); len(got) != 0 {
		t.Errorf("没有白名单内的包时应返回空，实际 %v", got)
	}
}

// TestCLTIndexIsValidJSON 锁住内置清单的解析（防止以后写错字段名）。
func TestCLTIndexIsValidJSON(t *testing.T) {
	// 与镜像上 clt/index.json 同构，字段名一旦漂移，真机上就是"清单不是合法 JSON"
	const sample = `{"updated":"2026-09-16","items":[{"name":"Command Line Tools 16.2",
	  "dir":"16.2","max_os":15,"bytes":661802053,
	  "pkgs":["CLTools_Executables.pkg","CLTools_macOSNMOS_SDK.pkg"],
	  "sha256":["CLTools_Executables.pkg=aa"]}]}`
	var idx cltIndex
	if err := json.Unmarshal([]byte(sample), &idx); err != nil {
		t.Fatalf("内置清单样例解析失败：%v", err)
	}
	if len(idx.Items) != 1 || idx.Items[0].MaxOS != 15 || idx.Items[0].Bytes != 661802053 {
		t.Fatalf("解析结果不对：%+v", idx.Items)
	}
	if got, ok := pickCLTItem(idx.Items, 15); !ok || len(cltNeededPkgs(got)) != 2 {
		t.Fatalf("从样例清单里挑不出两个包：%+v", got)
	}
}

// TestCLTMirrorIsTriedFirst 锁住三条路的**顺序**。
//
// 这是这一轮改动里最重要的行为：真机（抹机后的 mini，无代理）实测
// `softwareupdate -i "Command Line Tools for Xcode-16.2"` 15 分钟只下 1 MB
// 然后停住，最后回退到会弹图形对话框的 xcode-select —— 全新安装就卡在那里。
// 所以镜像整包必须是第一条路，而且失败要**如实记录苹果那侧的输出**。
func TestCLTMirrorIsTriedFirst(t *testing.T) {
	b, err := os.ReadFile("homebrew.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	iMirror := strings.Index(src, "m.installCLTFromMirror(ctx, result)")
	iSU := strings.Index(src, `"/usr/sbin/softwareupdate", "-i", label`)
	iPop := strings.Index(src, `"/usr/bin/xcode-select", "--install"`)
	if iMirror < 0 || iSU < 0 || iPop < 0 {
		t.Fatalf("三条路必须都在 installCLT 里：mirror=%d su=%d popup=%d", iMirror, iSU, iPop)
	}
	if !(iMirror < iSU && iSU < iPop) {
		t.Errorf("顺序应为 镜像(%d) → softwareupdate(%d) → 弹窗(%d)", iMirror, iSU, iPop)
	}
	// softwareupdate 必须以 root 直接跑（runAsUser 会插 sudo -u，工具链命令会失败）
	if strings.Contains(src, `m.runAsUser(ctx, 40*time.Minute, "/usr/sbin/softwareupdate"`) {
		t.Error("softwareupdate 必须用 runRoot；runAsUser 会插 `sudo -n -u <user>` 导致失败")
	}
	// 失败原因必须写进任务步骤（历史上只写"没成功"，真机上无从排查）
	if !strings.Contains(src, `result.step(ctx, "softwareupdate 失败："+lastLines(iout, 4))`) {
		t.Error("softwareupdate 失败时必须把真实输出带进任务步骤")
	}
	// 装完必须核对 python3 真的能用，不能只看 xcode-select -p
	if !strings.Contains(src, "m.python3Works(ctx)") {
		t.Error("CLT 安装成功判定必须包含 python3Works（占位 python3 会打印提示但不报错）")
	}
}

// TestCLTMirrorFetchesIndexOverHTTP 走一遍真实的 HTTP 抓取（本地假镜像）。
//
// 用假镜像而不是 mock 掉网络调用：这里要验证的是"地址怎么拼、返回怎么解"，
// 以及**失败时错误信息里有没有地址**（真机上只看日志，没有地址就没法排查）。
func TestCLTMirrorFetchesIndexOverHTTP(t *testing.T) {
	body, _ := json.Marshal(cltIndex{Updated: "2026-09-16", Items: []cltIndexItem{
		{Name: "Command Line Tools 16.2", Dir: "16.2", MaxOS: 15,
			Pkgs: []string{"CLTools_Executables.pkg", "CLTools_macOSNMOS_SDK.pkg"}},
	}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clt/index.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	t.Setenv("ZIZPANEL_CLT_MIRROR", srv.URL)
	if got := cltMirrorBase(); got != srv.URL {
		t.Fatalf("cltMirrorBase 应尊重 ZIZPANEL_CLT_MIRROR，实际 %q", got)
	}

	m := &Manager{}
	result := &InstallResult{Steps: []string{}}
	raw, err := m.fetchSmallText(context.Background(), 20*time.Second, cltMirrorBase()+"/clt/index.json")
	if err != nil {
		t.Fatalf("抓取假镜像清单失败：%v", err)
	}
	var idx cltIndex
	if err := json.Unmarshal([]byte(raw), &idx); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(idx.Items) != 1 || idx.Items[0].Dir != "16.2" {
		t.Fatalf("清单内容不对：%+v", idx.Items)
	}
	_ = result

	// 404 时错误信息里必须带地址，否则真机上无从排查
	_, err = m.fetchSmallText(context.Background(), 20*time.Second, srv.URL+"/clt/missing.json")
	if err == nil {
		t.Fatal("404 应当返回错误")
	}
	if !strings.Contains(err.Error(), "/clt/missing.json") {
		t.Errorf("错误信息里应包含地址，实际：%v", err)
	}
}

// TestCLTDownloadToFileVerifiesContent 走一遍真实的下载落盘（本地假镜像）。
//
// 锁住的是：下载必须是 root 直接跑 curl（不用 sudo -u），失败时删掉半成品，
// 且产物内容逐字节正确 —— 安装包下坏了 installer 会以看不懂的方式失败。
func TestCLTDownloadToFileVerifiesContent(t *testing.T) {
	// 这里不跳过非 root：runRoot 只是"不经 sudo 直接执行"，非特权用户也能跑 curl。
	payload := []byte("fake-pkg-content-" + strings.Repeat("x", 1024))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clt/16.2/CLTools_Executables.pkg" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "CLTools_Executables.pkg")
	m := &Manager{}
	result := &InstallResult{Steps: []string{}}
	n, err := m.downloadToFile(context.Background(), srv.URL+"/clt/16.2/CLTools_Executables.pkg",
		dst, 60*time.Second, result, "下载 CLTools_Executables.pkg")
	if err != nil {
		t.Fatalf("下载失败：%v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("返回大小 %d，期望 %d", n, len(payload))
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil || string(got) != string(payload) {
		t.Fatalf("落盘内容不对：err=%v len=%d", rerr, len(got))
	}
	if len(result.Steps) == 0 {
		t.Error("下载完成应写一条任务步骤（进度可见）")
	}

	// 失败路径：必须删掉半成品，否则下次重试会拿坏文件去 installer
	bad := filepath.Join(t.TempDir(), "CLTools_macOSNMOS_SDK.pkg")
	_, err = m.downloadToFile(context.Background(), srv.URL+"/clt/16.2/nope.pkg", bad, 60*time.Second, result, "下载")
	if err == nil {
		t.Fatal("404 应当返回错误")
	}
	if _, serr := os.Stat(bad); !os.IsNotExist(serr) {
		t.Error("下载失败后必须删掉半成品文件")
	}
}

// TestCLTIndexFilesBase 锁住"包地址怎么拼"。
//
// 镜像上按苹果产品号归档（clt/072-44426-A），所以清单里用 path；
// 但也允许更简单的 dir（相对 clt/）—— 两种写法都要拼对，否则真机上
// 就是 404，而 404 会被当成"这条路走不通"直接回退到苹果的慢路径。
func TestCLTIndexFilesBase(t *testing.T) {
	cases := []struct {
		item         cltIndexItem
		mirror, want string
	}{
		{cltIndexItem{Path: "clt/072-44426-A"}, "https://zizdog.com/zizpanel", "https://zizdog.com/zizpanel/clt/072-44426-A"},
		{cltIndexItem{Dir: "16.2"}, "https://zizdog.com/zizpanel", "https://zizdog.com/zizpanel/clt/16.2"},
		// 结尾带 / 的基址不能拼出 //
		{cltIndexItem{Path: "/clt/x/"}, "https://zizdog.com/zizpanel/", "https://zizdog.com/zizpanel/clt/x"},
	}
	for i, c := range cases {
		if got := c.item.filesBase(c.mirror); got != c.want {
			t.Errorf("第 %d 例 filesBase = %q，期望 %q", i, got, c.want)
		}
	}
}

// TestConcatPartsUsesManifestOrder 锁住"分片按清单顺序拼接"。
//
// 拼接顺序错了整体 sha256 必然不匹配，但**报错会指向"校验不一致"**，
// 排查时很难想到是顺序问题：split 的默认后缀是字母序、-d 是数字序，
// 一旦有人改成 sort.Strings 就会在 10 片以上时悄悄错位（a, aa, ab…）。
func TestConcatPartsUsesManifestOrder(t *testing.T) {
	dir := t.TempDir()
	partDir := filepath.Join(dir, "parts")
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 故意让文件名顺序与清单顺序相反
	content := map[string]string{"p0": "AAA", "p1": "BB", "p2": "C"}
	for name, body := range content {
		if err := os.WriteFile(filepath.Join(partDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parts := []cltPart{{File: "p2", Size: 1}, {File: "p1", Size: 2}, {File: "p0", Size: 3}}
	dst := filepath.Join(dir, "out.pkg")
	n, err := concatParts(partDir, parts, dst)
	if err != nil {
		t.Fatalf("拼接失败：%v", err)
	}
	if n != 6 {
		t.Errorf("总大小 %d，期望 6", n)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "CBBAAA" {
		t.Errorf("拼接结果 %q，期望 %q（必须按清单顺序）", string(got), "CBBAAA")
	}

	// 缺片：必须报错并删掉半成品，否则 installer 会拿到一个不完整的包
	bad := filepath.Join(dir, "bad.pkg")
	if _, err := concatParts(partDir, []cltPart{{File: "nope", Size: 1}}, bad); err == nil {
		t.Fatal("缺分片应当报错")
	}
	if _, serr := os.Stat(bad); !os.IsNotExist(serr) {
		t.Error("缺分片时必须删掉半成品")
	}

	// 大小与清单不符：同样必须报错（能查出"下了一半"）
	if _, err := concatParts(partDir, []cltPart{{File: "p0", Size: 99}}, filepath.Join(dir, "size.pkg")); err == nil {
		t.Error("分片大小与清单不符应当报错")
	}
}

// TestCLTPartsAreOptional 锁住"清单不给分片时退回单文件下载"。
//
// 镜像可以分阶段升级：先放整包（0.8.8 就是这样上线的），之后再加分片。
// 面板必须两种布局都能装，否则"镜像更新"会变成一次必须同步的发布。
func TestCLTPartsAreOptional(t *testing.T) {
	item := cltIndexItem{Dir: "16.2", Pkgs: []string{"CLTools_Executables.pkg"}}
	if len(item.Parts) != 0 {
		t.Fatal("没写 parts 时应为空（走单文件）")
	}
	withParts := cltIndexItem{
		Pkgs: []string{"CLTools_Executables.pkg", "CLTools_macOSNMOS_SDK.pkg"},
		Parts: []cltPkgParts{{
			Pkg:   "CLTools_Executables.pkg",
			Parts: []cltPart{{File: "CLTools_Executables.pkg.part-000", Size: 10}},
		}},
	}
	if got := withParts.partsFor("CLTools_Executables.pkg"); len(got) != 1 || got[0].Size != 10 {
		t.Fatalf("parts 解析不对：%+v", withParts.Parts)
	}
	// 清单里没给分片的包必须返回 nil → 走单文件下载（镜像可以分阶段升级）
	if got := withParts.partsFor("CLTools_macOSNMOS_SDK.pkg"); got != nil {
		t.Errorf("没配置分片的包应返回 nil，实际 %+v", got)
	}
}

// TestFetchPartsOverHTTP 走一遍真实的分片下载（本地假镜像，小分片）。
//
// 这是 0.8.9 的核心新路径：576 MB 的 CLT 包在国内链路上单连接会抖动
// （真机实测偶发 `Connection reset by peer`），单文件下载断一次就要从 0 开始。
// 这里锁住四件事：
//   - 分片能并行下全；
//   - 大小不符要报错（"下了一半"不能被当成成功）；
//   - 已存在且大小正确的分片**不重下**（断点续传的实质）；
//   - 拼回来的字节与原文件完全一致。
func TestFetchPartsOverHTTP(t *testing.T) {
	payload := []byte(strings.Repeat("zizpanel-clt-part-", 64))
	third := len(payload) / 3
	chunks := [][]byte{payload[:third], payload[third : 2*third], payload[2*third:]}
	names := []string{"pkg.part-000", "pkg.part-001", "pkg.part-002"}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i, n := range names {
			if r.URL.Path == "/clt/"+n {
				atomic.AddInt32(&hits, 1)
				_, _ = w.Write(chunks[i])
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	partDir := t.TempDir()
	parts := []cltPart{
		{File: names[0], Size: int64(len(chunks[0]))},
		{File: names[1], Size: int64(len(chunks[1]))},
		{File: names[2], Size: int64(len(chunks[2]))},
	}
	m := &Manager{}
	result := &InstallResult{Steps: []string{}}
	if err := m.fetchParts(context.Background(), result, srv.URL+"/clt", "pkg", partDir, parts); err != nil {
		t.Fatalf("分片下载失败：%v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 个分片，实际 %d", got)
	}

	dst := filepath.Join(t.TempDir(), "pkg")
	n, err := concatParts(partDir, parts, dst)
	if err != nil {
		t.Fatalf("拼接失败：%v", err)
	}
	got, _ := os.ReadFile(dst)
	if n != int64(len(payload)) || string(got) != string(payload) {
		t.Fatalf("拼回来的内容不对：n=%d len=%d", n, len(got))
	}

	// 续传：三个分片都已存在且大小正确 → 一个请求都不该发
	atomic.StoreInt32(&hits, 0)
	if err := m.fetchParts(context.Background(), result, srv.URL+"/clt", "pkg", partDir, parts); err != nil {
		t.Fatalf("续传路径失败：%v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("分片都在本地时不该再请求，实际请求 %d 次", got)
	}

	// 大小不符：必须报错，且不能把明显的半成品当成成功
	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, names[0]), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	badParts := []cltPart{{File: names[0], Size: int64(len(chunks[0]))}}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("still-short"))
	}))
	defer srv2.Close()
	if err := m.fetchParts(context.Background(), result, srv2.URL, "pkg", badDir, badParts); err == nil {
		t.Error("分片大小不符时必须报错")
	}
}

// TestBrewSudoShimRewritesSudo 锁住"降权安装 Homebrew 时那几步 root 操作"。
//
// 真机（抹机后的 mini）挂在这里：CLT 装好了，Homebrew 安装脚本走到
// `/usr/bin/sudo /usr/bin/install -d -o root -g wheel /opt/homebrew` 就死，
// 报 "sudo: a terminal is required to read the password"。
// 垫片的作用是：**有 -u 就拿掉并原样执行**（安装脚本本意就是降权），
// **没 -u 就清掉 HOME 后以面板用户执行**（建目录这类操作换谁做结果一样）。
// 关键是不能因为它而谎报成功：真的失败时退出码与输出必须原样传上来。
func TestBrewSudoShimRewritesSudo(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("没有 /bin/bash")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "sudo")
	if err := os.WriteFile(shim, []byte(brewShimSudo), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"-u 形式：拿掉 -u 与用户名", []string{"-u", "someone", "/bin/echo", "hello"}, "hello\n"},
		{"-n 形式：拿掉 -n", []string{"-n", "/bin/echo", "hello"}, "hello\n"},
		{"裸形式：原样执行", []string{"/bin/echo", "hello"}, "hello\n"},
	}
	for _, c := range cases {
		cmd := exec.Command("/bin/bash", append([]string{shim}, c.args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s：执行失败 %v（输出 %q）", c.name, err, out)
			continue
		}
		if string(out) != c.want {
			t.Errorf("%s：输出 %q，期望 %q", c.name, out, c.want)
		}
	}

	// HOME 必须被清掉：不清的话 `sudo` 语义下 brew 会把文件写进 root 的家目录
	cmd := exec.Command("/bin/bash", shim, "/bin/sh", "-c", "echo ${HOME:-EMPTY}")
	cmd.Env = append(os.Environ(), "HOME=/Users/someone")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("HOME 用例执行失败：%v", err)
	}
	if strings.TrimSpace(string(out)) != "EMPTY" {
		t.Errorf("垫片必须清掉 HOME，实际 %q", out)
	}

	// 失败必须原样传上来（退出码），不能把失败说成成功
	cmd = exec.Command("/bin/bash", shim, "/bin/sh", "-c", "exit 7")
	err = cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Errorf("失败退出码必须原样传递，实际 err=%v", err)
	}
}

// TestBrewInstallUsesShimAndIsHonest 锁住 EnsureHomebrew 的两个要点：
// 安装时确实用了垫片；失败信息里带**真实输出**（而不是只说"安装失败"）。
func TestBrewInstallUsesShimAndIsHonest(t *testing.T) {
	b, err := os.ReadFile("homebrew.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"m.brewSudoShim()",
		// Homebrew 官方安装脚本硬编码 /usr/bin/sudo，PATH 垫片拦不住它 ——
		// 必须有"真的免密 sudo"，而且必须装完撤销
		"m.brewSudoGrant(ctx, result)",
		"defer revoke()",
		"HOMEBREW_ALLOW_ROOT=1",
		`"PATH="+shimDir+":/usr/bin:/bin:/usr/sbin:/sbin"`,
		"输出末尾",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("homebrew.go 缺少 %q", want)
		}
	}
	// 垫片本身必须是合法的 bash（用 bash -n 静态检查）
	cmd := exec.Command("/bin/bash", "-n", "-c", brewShimSudo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("垫片脚本语法不合法：%v %s", err, out)
	}
}

// TestBrewSudoGrantIsTemporaryAndSafe 锁住临时 sudoers 授权的**边界**。
//
// 这是整个项目里"离危险最近"的一处代码：给面板用户开免密 sudo。
// 所以边界必须被测试钉住：
//   - 文件必须写在 /etc/sudoers.d/ 下的**专用文件名**（不能覆盖用户的 sudoers）；
//   - 权限 0440 + root 拥有（sudo 自己会拒绝加载权限过宽的文件）；
//   - **必须先 visudo -c 校验再启用**（写坏 sudoers 会让整个系统的 sudo 失效）；
//   - 必须有撤销路径，且撤销时用 `sudo -k` 清掉已缓存的时间戳；
//   - 非 root 运行时必须拒绝（否则写不出 root 拥有的 sudoers）。
func TestBrewSudoGrantIsTemporaryAndSafe(t *testing.T) {
	b, err := os.ReadFile("homebrew.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		`"/etc/sudoers.d"`,
		`"zizpanel-homebrew-install"`,
		"0o440",
		"os.Chown(tmp, 0, 0)",
		`"/usr/sbin/visudo", "-c", "-f", tmp`,
		"os.Remove(path)",
		`"/usr/bin/sudo", "-k"`,
		"if os.Geteuid() != 0 {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("homebrew.go 的临时 sudo 授权缺少 %q", want)
		}
	}
	// 撤销必须是"关掉任务就会发生"的 defer，而不是只写在成功分支里
	if !strings.Contains(src, "defer revoke()") {
		t.Error("临时授权必须 defer 撤销，失败路径也不能留")
	}
	// 授权内容必须限定到安装用户，不能写成"所有人 NOPASSWD: ALL"
	if !strings.Contains(src, `m.opt.UserName + " ALL=(root) NOPASSWD: ALL\n"`) {
		t.Error("免密授权必须绑定到具体安装用户")
	}
}
