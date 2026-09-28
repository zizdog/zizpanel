package web

// api_smb_gate_test.go —— 「网络磁盘（SMB）」的唯一门禁。
//
// 用**注入/垫片**验（假 mount_smbfs / mount / umount 走 PATH），全程不连真 NAS、
// 不 root、不碰 /Volumes 与用户真实外接盘。它钉四件事：
//   ① 命令构造正确（主机/共享/挂载点/选项），且口令**不在命令行明文里**；
//   ② 挂载成功与否只看回读：挂载表 + 目录真的读得到，回读失败必须报失败；
//   ③ 凭据错 / 主机不可达 ⇒ 如实错误 + 状态（last_error / 上次尝试时间）更新；
//   ④ 口令不出现在日志/审计/HTTP 响应里。
//
// 口令通道的钉法：假 mount_smbfs 与真实现一样**只从 /dev/tty 读口令**
// （Apple 源码 lib/smbclient/server.c：readpassphrase(..., RPP_REQUIRE_TTY)），
// 面板必须给它一个真 pty（/usr/bin/script 分配）并看到提示后再写入口令。
// 所以「口令被垫片读到」本身就证明了通道正确，而 argv 里没有它。
//
// 变异验证（把回读删掉 ⇒ 本门禁必须红）：见 fake-"silent" 那一步 ——
// 假 mount_smbfs 退出码 0 却什么都不做，只有回读才能判它失败。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/smb"
)

// ---------- 假命令 ----------

const shimMountSmbfs = `#!/bin/sh
# 假 mount_smbfs。与真实现对齐的两点：
#   ① 口令只从 /dev/tty 读（readpassphrase + RPP_REQUIRE_TTY），没有 tty 就读不到；
#   ② 认证成功才让挂载点变成"已挂载"（写 state/mounted，假 mount 据此输出）。
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
d=$(cd "$(dirname "$0")" && pwd)
mode=$(cat "$d/mode" 2>/dev/null || echo ok)
printf '%s\n' "$*" >> "$d/argv.log"
mp=""; url=""
for a in "$@"; do url="$mp"; mp="$a"; done
case "$mode" in
  silent) exit 0 ;;
  authfail) echo "mount_smbfs: server rejected the connection: Authentication error" >&2; exit 77 ;;
  unreachable) echo "mount_smbfs: server connection failed: No route to host" >&2; exit 68 ;;
  nodir) echo "$mp" > "$d/state/mounted"; echo "$url" >> "$d/state/mounted"; chmod 000 "$mp"; exit 0 ;;
esac
printf 'Password for %s: ' "$url" > /dev/tty
if ! IFS= read -r -t 5 pw < /dev/tty; then
  echo "mount_smbfs: no password available (no tty)" >&2
  exit 77
fi
printf '%s' "$pw" > "$d/state/password.txt"
printf '%s\n%s\n' "$mp" "$url" > "$d/state/mounted"
mkdir -p "$mp"
printf 'fake media\n' > "$mp/fake-media.mkv"
exit 0
`

const shimMount = `#!/bin/sh
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
d=$(cd "$(dirname "$0")" && pwd)
echo "/dev/disk3s1s1 on / (apfs, local, read-only, journaled)"
if [ -f "$d/state/mounted" ]; then
  mp=$(sed -n 1p "$d/state/mounted")
  src=$(sed -n 2p "$d/state/mounted")
  echo "$src on $mp (smbfs, nodev, nosuid, mounted by root)"
fi
exit 0
`

const shimUmount = `#!/bin/sh
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
d=$(cd "$(dirname "$0")" && pwd)
if [ -f "$d/state/busy" ]; then
  echo "umount: $1: Resource busy" >&2
  exit 1
fi
rm -f "$d/state/mounted"
exit 0
`

type smbShim struct {
	t   *testing.T
	dir string
}

func newSMBShim(t *testing.T) *smbShim {
	t.Helper()
	dir := t.TempDir()
	s := &smbShim{t: t, dir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"mount_smbfs": shimMountSmbfs, "mount": shimMount, "umount": shimUmount,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.setMode("ok")
	// PATH 垫片：假命令排在最前，其余保留（面板别的探测仍能用系统命令）。
	t.Setenv("PATH", dir+":/usr/bin:/bin:/usr/sbin:/sbin")
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(dir, "mnt"), 0o755)
	})
	return s
}

func (s *smbShim) setMode(mode string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "mode"), []byte(mode+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *smbShim) setBusy(on bool) {
	s.t.Helper()
	p := filepath.Join(s.dir, "state", "busy")
	if on {
		if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
			s.t.Fatal(err)
		}
		return
	}
	_ = os.Remove(p)
}

func (s *smbShim) read(file string) string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "state", file))
	return string(b)
}

func (s *smbShim) argvLog() string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "argv.log"))
	return string(b)
}

// reset 把垫片侧的"已挂载"清掉（相当于系统上什么都没挂），
// 用来给"退出码 0 但没挂上"这类负向对照造初始状态。
func (s *smbShim) reset() {
	s.t.Helper()
	_ = os.Remove(filepath.Join(s.dir, "state", "mounted"))
	_ = os.Remove(filepath.Join(s.dir, "state", "password.txt"))
}

func (s *smbShim) mountedState() string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "state", "mounted"))
	return string(b)
}

// ---------- 测试用 HTTP ----------

// smbDo 与 doJSON 同源，但**保留响应原文**（口令泄漏断言必须看原文，
// 只看解析后的结构会漏掉藏在字符串里的东西）。
func smbDo(t *testing.T, ts *httptest.Server, method, path string, body any, cookies []*http.Cookie) (*http.Response, map[string]any, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, ts.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == "zp_csrf" {
			req.Header.Set("X-CSRF-Token", c.Value)
		}
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res, out, string(raw)
}

// ============================================================================
//  门禁本体
// ============================================================================

func TestSMBNetworkMountGate(t *testing.T) {
	shim := newSMBShim(t)
	_, ts := newTestServer(t)
	res, out, _ := smbDo(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("[前置] 初始化失败 %d: %v", res.StatusCode, out)
	}
	cookies := res.Cookies()

	// 垫片真的生效：面板将执行的是假 mount_smbfs，而不是真机 /sbin/mount_smbfs
	// （下面所有断言都建立在这上面，先钉死它）。
	if got, want := smb.LookBin("mount_smbfs"), filepath.Join(shim.dir, "mount_smbfs"); got != want {
		t.Fatalf("PATH 垫片没生效：解析到 %q，期望 %q", got, want)
	}

	const secret = "S3cr3t-Pass-123"
	const name = "nasmedia"

	// ---- ① 新增：校验 + 口令只回 password_set ----
	res, out, raw := smbDo(t, ts, "POST", "/api/v1/system/smb", map[string]any{
		"name": name, "host": "nas.local", "share": "Media",
		"user": "mediauser", "password": secret, "read_only": true,
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[①新增] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	d := mapGet(out, "data")
	id := asString(mapGet(d, "id"))
	if id == "" {
		t.Fatalf("[①新增] 没有返回 id：%s", raw)
	}
	if mapGet(d, "password_set") != true {
		t.Errorf("[①新增] password_set 应为 true，实际 %v", mapGet(d, "password_set"))
	}
	if strings.Contains(raw, secret) {
		t.Errorf("[④] 新增响应里出现了口令明文：%s", raw)
	}
	// 挂载点必须在面板自己的 mnt 下（不碰 /Volumes）。
	mountPoint := asString(mapGet(d, "mount_point"))
	if !strings.HasSuffix(mountPoint, "/mnt/"+name) || !filepath.IsAbs(mountPoint) {
		t.Fatalf("[①新增] 挂载点应在 <安装根>/mnt/%s 下，实际 %q", name, mountPoint)
	}

	// 空口令（非 guest）必须当场拒绝：留着空口令去挂只会得到一句"认证失败"。
	res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb", map[string]any{
		"name": "noPass", "host": "nas.local", "share": "Media", "user": "mediauser",
	}, cookies)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(asString(mapGet(out, "msg")), "口令") {
		t.Errorf("[①新增] 空口令应 400 并说明，实际 %d：%v", res.StatusCode, out["msg"])
	}

	// ---- ② 挂载：命令构造 + 口令不进 argv + 回读到内容才算成功 ----
	res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[②挂载] 应 200，实际 %d：%v\n%s", res.StatusCode, out["msg"], raw)
	}
	md := mapGet(out, "data")
	if mapGet(md, "mounted") != true || mapGet(md, "verified") != true {
		t.Fatalf("[②挂载] 回读必须确认已挂载，实际 mounted=%v verified=%v：%v",
			mapGet(md, "mounted"), mapGet(md, "verified"), out["msg"])
	}
	if got := asSlice(mapGet(md, "entries")); len(got) == 0 || asString(got[0]) != "fake-media.mkv" {
		t.Errorf("[②挂载] 回读到的目录内容不对：%v", got)
	}
	// 命令行（下发/审计里的那份）必须体现主机/共享/挂载点/选项，且没有口令。
	cmd := asString(mapGet(md, "command"))
	for _, want := range []string{"mount_smbfs", "-o nodev,nosuid,rdonly", "//mediauser@nas.local/Media", mountPoint} {
		if !strings.Contains(cmd, want) {
			t.Errorf("[①命令构造] 命令行里缺少 %q：%s", want, cmd)
		}
	}
	if strings.Contains(cmd, secret) {
		t.Errorf("[①命令构造] 命令行里出现了口令明文：%s", cmd)
	}
	// 垫片（=真正的 mount_smbfs 看到的 argv）里也不能有口令；主机/共享/挂载点/选项要在。
	argv := shim.argvLog()
	if strings.TrimSpace(argv) == "" {
		t.Fatal("[①命令构造] 假 mount_smbfs 一次都没被调用")
	}
	if strings.Contains(argv, secret) {
		t.Errorf("[①命令构造] 子进程 argv 里出现了口令明文：%s", argv)
	}
	if !strings.Contains(argv, "//mediauser@nas.local/Media") ||
		!strings.Contains(argv, "-o nodev,nosuid,rdonly") || !strings.Contains(argv, mountPoint) {
		t.Errorf("[①命令构造] 子进程 argv 不对：%s", argv)
	}
	// 口令通道：垫片只能从 /dev/tty 读到 —— 读到就说明 pty 通道真的通了。
	if got := shim.read("password.txt"); got != secret {
		t.Fatalf("[①口令通道] 假 mount_smbfs 应从 /dev/tty 读到的口令，实际 %q（错误 %q）", got, shim.read("password.txt"))
	}

	// ---- ②负向对照 C：挂载点已被**别的**卷占着 ⇒ 必须拒绝，绝不盖上去 ----
	// （<安装根>/mnt 下可能已经有卷，比如镜像盘；盖上去会把原来的挂载藏掉。）
	beforeArgv := shim.argvLog()
	shim.reset()
	if err := os.WriteFile(filepath.Join(shim.dir, "state", "mounted"),
		[]byte(mountPoint+"\n//other@nas.local/Other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[②占用] 挂载点已被别的卷占着，接口却去挂了：%s", raw)
	}
	if msg := asString(mapGet(out, "msg")); !strings.Contains(msg, "占着") {
		t.Errorf("[②占用] 应说明挂载点已被占用，实际：%v", msg)
	}
	if shim.argvLog() != beforeArgv {
		t.Errorf("[②占用] 被占用时不该再调用 mount_smbfs，实际：%s", shim.argvLog())
	}

	// ---- ②负向对照 A：退出码 0 但什么都没挂 ⇒ 必须报失败（删掉回读这条就会变红） ----
	shim.reset()
	shim.setMode("silent")
	res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[②回读] 假 mount_smbfs 退出码 0 但没有任何挂载，接口却报了成功（回读被绕过）：%s", raw)
	}
	if msg := asString(mapGet(out, "msg")); !strings.Contains(msg, "不算挂载成功") || !strings.Contains(msg, "挂载表") {
		t.Errorf("[②回读] 失败原因应说明是回读不一致（挂载表里没有），实际：%v", msg)
	}

	// ---- ②负向对照 B：挂载表说挂上了、目录却读不到 ⇒ 也必须报失败 ----
	if os.Geteuid() != 0 { // root 不受 000 权限限制，这条例外跳过
		shim.reset()
		shim.setMode("nodir")
		res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
		if res.StatusCode == 200 {
			t.Fatalf("[②回读] 目录读不到却报成功：%s", raw)
		}
		if !strings.Contains(asString(mapGet(out, "msg")), "读不到目录内容") {
			t.Errorf("[②回读] 失败原因应说明读不到目录，实际：%v", out["msg"])
		}
		_ = os.Chmod(mountPoint, 0o755)
		_ = os.Remove(filepath.Join(shim.dir, "state", "mounted"))
	}

	// ---- ③ 凭据错 / 主机不可达 ⇒ 如实错误 + 状态更新 ----
	for _, c := range []struct{ mode, want string }{
		{"authfail", "认证失败"},
		{"unreachable", "连不上 NAS"},
	} {
		shim.reset()
		shim.setMode(c.mode)
		res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
		if res.StatusCode == 200 {
			t.Fatalf("[③%s] 失败场景却报成功：%s", c.mode, raw)
		}
		msg := asString(mapGet(out, "msg"))
		if !strings.Contains(msg, c.want) {
			t.Errorf("[③%s] 错误应是「%s」，实际 %q", c.mode, c.want, msg)
		}
		if strings.Contains(raw, secret) {
			t.Errorf("[④] 失败响应里出现了口令明文：%s", raw)
		}
		// 状态必须更新：未挂载 + 上次失败原因 + 上次尝试时间。
		_, lout, lraw := smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
		row := smbRowByID(t, lout, id)
		if mapGet(row, "mounted") != false {
			t.Errorf("[③%s] 失败后 mounted 应为 false，实际 %v", c.mode, mapGet(row, "mounted"))
		}
		if !strings.Contains(asString(mapGet(row, "last_error")), c.want) {
			t.Errorf("[③%s] last_error 应带真实原因，实际 %q", c.mode, mapGet(row, "last_error"))
		}
		if asString(mapGet(row, "last_attempt_at")) == "" {
			t.Errorf("[③%s] last_attempt_at 应被记下", c.mode)
		}
		if strings.Contains(lraw, secret) {
			t.Errorf("[④] 列表响应里出现了口令明文：%s", lraw)
		}
	}

	// ---- ②（回到绿灯）+ 卸载：状态跟着真实挂载表走 ----
	shim.setMode("ok")
	res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[②卸载前] 重新挂载应成功，实际 %d：%v", res.StatusCode, out["msg"])
	}
	res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/unmount", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[②卸载] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	if got := shim.mountedState(); strings.TrimSpace(got) != "" {
		t.Errorf("[②卸载] 垫片侧仍认为已挂载：%q", got)
	}
	_, lout, _ := smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	if mapGet(smbRowByID(t, lout, id), "mounted") != false {
		t.Errorf("[②卸载] 卸载后 mounted 应为 false")
	}

	// 忙：有进程在用 ⇒ 如实报"资源忙"，绝不 -f 强卸。
	shim.setMode("ok")
	if res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies); res.StatusCode != 200 {
		t.Fatalf("[②忙] 前置挂载失败：%v", out["msg"])
	}
	shim.setBusy(true)
	res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/unmount", map[string]any{}, cookies)
	if res.StatusCode != http.StatusConflict || !strings.Contains(asString(mapGet(out, "msg")), "有进程正在使用") {
		t.Errorf("[②忙] 应 409 + 「有进程正在使用」，实际 %d：%v", res.StatusCode, out["msg"])
	}
	shim.setBusy(false)
	if res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/unmount", map[string]any{}, cookies); res.StatusCode != 200 {
		t.Fatalf("[②忙] 恢复后卸载应成功：%v", out["msg"])
	}

	// ---- ④ 口令不出现在审计里 ----
	_, _, auditRaw := smbDo(t, ts, "GET", "/api/v1/audit?limit=200", nil, cookies)
	if strings.Contains(auditRaw, secret) {
		t.Errorf("[④] 审计里出现了口令明文")
	}
	if !strings.Contains(auditRaw, "smb_mount") || !strings.Contains(auditRaw, "smb_unmount") {
		t.Errorf("[④] 挂载/卸载应写审计（smb_mount / smb_unmount）")
	}

	// ---- 删除：先卸载再删配置；共享里的文件一个都不动 ----
	shim.setMode("ok")
	if res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies); res.StatusCode != 200 {
		t.Fatalf("[删除] 前置挂载失败：%v", out["msg"])
	}
	if res, out, _ = smbDo(t, ts, "DELETE", "/api/v1/system/smb/"+id, nil, cookies); res.StatusCode != 200 {
		t.Fatalf("[删除] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	_, lout, _ = smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	if list := asSlice(mapGet(mapGet(lout, "data"), "mounts")); len(list) != 0 {
		t.Errorf("[删除] 删除后列表应为空，实际 %d 条", len(list))
	}
}

func smbRowByID(t *testing.T, out map[string]any, id string) map[string]any {
	t.Helper()
	for _, it := range asSlice(mapGet(mapGet(out, "data"), "mounts")) {
		row, _ := it.(map[string]any)
		if asString(mapGet(row, "id")) == id {
			return row
		}
	}
	t.Fatalf("列表里找不到网络盘 %s：%v", id, mapGet(out, "data"))
	return nil
}

// TestSMBCorePure 是核心纯函数的负向对照（URL 构造 / 口令 scrub），
// 不碰系统：命令构造与口令安全这两条最要命的规则在这里再钉一遍。
func TestSMBCorePure(t *testing.T) {
	m := smb.Mount{Name: "nasmedia", Host: "nas.local", Share: "Media", User: "alice", Domain: "DOMAIN", Password: "x"}
	if got, want := m.URL(), "//DOMAIN;alice@nas.local/Media"; got != want {
		t.Fatalf("URL 构造不对：%s", got)
	}
	// 中文/空格共享名必须百分号转义（真机实测：原文会被 mount_smbfs 判 URL 非法）。
	m.Share = "我的 媒体"
	if got := m.URL(); strings.ContainsAny(got, " ") || strings.Contains(got, "我") {
		t.Fatalf("共享名必须转义，实际 %s", got)
	}
	if got := smb.Scrub("err: abcabc", "abc"); strings.Contains(got, "abc") {
		t.Fatalf("scrub 必须抹掉口令：%s", got)
	}
}
