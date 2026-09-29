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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
if [ -f "$d/state/nfs" ]; then
  mp=$(sed -n 1p "$d/state/nfs")
  src=$(sed -n 2p "$d/state/nfs")
  echo "$src on $mp (nfs, nodev, nosuid, read-only, mounted by $(id -un))"
fi
exit 0
`

// 假 mount_nfs：真实实现无凭据、不需要 tty、不读 /dev/tty，也绝不经过 script。
const shimMountNFS = `#!/bin/sh
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
d=$(cd "$(dirname "$0")" && pwd)
printf '%s\n' "$*" >> "$d/nfs-argv.log"
mode=$(cat "$d/nfs-mode" 2>/dev/null || echo ok)
mp=""; src=""
for a in "$@"; do src="$mp"; mp="$a"; done
case "$mode" in
  silent) exit 0 ;;
  noexport) echo "mount_nfs: can't access /volume1/media: No such file or directory" >&2; exit 1 ;;
  unreachable) echo "mount_nfs: can't contact server: RPC: Port mapper failure - RPC: Timed out" >&2; exit 1 ;;
  denied) echo "mount_nfs: Permission denied" >&2; exit 1 ;;
  nodir) echo "$mp" > "$d/state/nfs"; echo "$src" >> "$d/state/nfs"; chmod 000 "$mp"; exit 0 ;;
esac
printf '%s\n%s\n' "$mp" "$src" > "$d/state/nfs"
mkdir -p "$mp"
printf 'fake media\n' > "$mp/fake-media.mkv"
exit 0
`

// 假 script：只记一笔调用，再原样交给真 /usr/bin/script（SMB 的 pty 通道靠它）。
// NFS 门禁据此断言"从不调用 script"。
const shimScript = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
printf '%s\n' "$*" >> "$d/script-argv.log"
exec /usr/bin/script "$@"
`

const shimUmount = `#!/bin/sh
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
d=$(cd "$(dirname "$0")" && pwd)
if [ -f "$d/state/busy" ]; then
  echo "umount: $1: Resource busy" >&2
  exit 1
fi
rm -f "$d/state/mounted" "$d/state/nfs"
exit 0
`

// 假 smbutil：与真实现一样**只从 /dev/tty** 读口令，再打印一张共享表
// （含一个 Pipe，用来断言"只留 Disk"）。「列出共享」走的就是这条路。
const shimSmbutil = `#!/bin/sh
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
d=$(cd "$(dirname "$0")" && pwd)
printf '%s\n' "$*" >> "$d/smbutil-argv.log"
printf 'Password for 192.0.2.10: ' > /dev/tty
if ! IFS= read -r pw < /dev/tty; then echo "smbutil: no password available" >&2; exit 77; fi
printf '%s' "$pw" > "$d/state/smbutil-password.txt"
cat <<'EOF'
Share                                           Type    Comments
-------------------------------
Media                                           Disk    
Photos                                          Disk    
IPC$                                            Pipe    IPC Service (fake nas)

3 shares listed
EOF
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
		"mount_smbfs": shimMountSmbfs, "mount_nfs": shimMountNFS,
		"mount": shimMount, "umount": shimUmount, "script": shimScript,
		"smbutil": shimSmbutil,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.setMode("ok")
	s.setNFSMode("ok")
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

func (s *smbShim) setNFSMode(mode string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "nfs-mode"), []byte(mode+"\n"), 0o644); err != nil {
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

func (s *smbShim) nfsArgvLog() string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "nfs-argv.log"))
	return string(b)
}

func (s *smbShim) smbutilArgv() string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "smbutil-argv.log"))
	return string(b)
}

func (s *smbShim) scriptLog() string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "script-argv.log"))
	return string(b)
}

// reset 把垫片侧的"已挂载"清掉（相当于系统上什么都没挂），
// 用来给"退出码 0 但没挂上"这类负向对照造初始状态。
func (s *smbShim) reset() {
	s.t.Helper()
	_ = os.Remove(filepath.Join(s.dir, "state", "mounted"))
	_ = os.Remove(filepath.Join(s.dir, "state", "password.txt"))
	_ = os.Remove(filepath.Join(s.dir, "state", "nfs"))
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
	for _, want := range []string{"mount_smbfs", "-o nodev,nosuid,soft,nomdatacache,rdonly", "//mediauser@nas.local/Media", mountPoint} {
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
		!strings.Contains(argv, "-o nodev,nosuid,soft,nomdatacache,rdonly") || !strings.Contains(argv, mountPoint) {
		t.Errorf("[①命令构造] 子进程 argv 不对：%s", argv)
	}
	// NAS 会关机：SMB 也必须带 soft（I/O 失败而不是永久挂起）。
	if !strings.Contains(argv, "soft") {
		t.Errorf("[①离线安全] SMB argv 必须带 soft：%s", argv)
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

// ============================================================================
//  NFS 门禁（与 SMB 共用同一套 PATH 垫片与 settings 键 smb_mounts）
//
//  它钉四件事：
//    ① 导出路径校验（不以 / 开头 / 含 .. / 含空白 ⇒ 400）；
//    ② NFS 新建+挂载：mount_nfs 直接跑，argv 有 -o…ro… / host:/path / 挂载点，
//       且**从不**调用 script / mount_smbfs，全新无凭据；
//    ③ 退出码 0 但挂载表里没有 NFS 行 ⇒ 必须报失败；
//    ④ 挂载表里有 NFS 行但目录读不到 ⇒ 必须报失败。
// ============================================================================

func TestNFSNetworkMountGate(t *testing.T) {
	shim := newSMBShim(t)
	_, ts := newTestServer(t)
	res, out, _ := smbDo(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("[前置] 初始化失败 %d: %v", res.StatusCode, out)
	}
	cookies := res.Cookies()

	if got, want := smb.LookBin("mount_nfs"), filepath.Join(shim.dir, "mount_nfs"); got != want {
		t.Fatalf("PATH 垫片没生效：mount_nfs 解析到 %q，期望 %q", got, want)
	}

	// ---- ① 非法导出路径必须当场 400 ----
	for _, bad := range []struct{ share, want string }{
		{"volume1/media", "以 / 开头"}, // 不以 / 开头
		{"/volume1/../etc", ".."},   // 目录穿越
		{"/volume1/my media", "空白"}, // 空白字符
	} {
		res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb", map[string]any{
			"kind": "nfs", "name": "badnfs", "host": "nas.local", "share": bad.share,
		}, cookies)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("[①校验] 导出路径 %q 应 400，实际 %d：%v", bad.share, res.StatusCode, out["msg"])
			continue
		}
		if msg := asString(mapGet(out, "msg")); !strings.Contains(msg, bad.want) {
			t.Errorf("[①校验] %q 的报错应含 %q，实际 %q", bad.share, bad.want, msg)
		}
	}

	// ---- 新增：kind=nfs，账号/域/口令一律丢弃（NFS 无凭据） ----
	const nfsHost = "nas.local"
	const nfsExport = "/volume1/media"
	const name = "nfsmedia"
	res, out, raw := smbDo(t, ts, "POST", "/api/v1/system/smb", map[string]any{
		"kind": "nfs", "name": name, "host": nfsHost, "share": nfsExport,
		"user": "someone", "domain": "DOM", "password": "should-be-dropped", "read_only": true,
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[新增] NFS 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	d := mapGet(out, "data")
	id := asString(mapGet(d, "id"))
	if id == "" {
		t.Fatalf("[新增] 没返回 id：%s", raw)
	}
	if mapGet(d, "kind") != "nfs" {
		t.Errorf("[新增] kind 应为 nfs，实际 %v", mapGet(d, "kind"))
	}
	if mapGet(d, "password_set") != false {
		t.Errorf("[新增] NFS 不该有口令，password_set=%v", mapGet(d, "password_set"))
	}
	if asString(mapGet(d, "user")) != "" {
		t.Errorf("[新增] NFS 不该持久化账号，实际 %q", mapGet(d, "user"))
	}
	if strings.Contains(raw, "should-be-dropped") {
		t.Errorf("[新增] NFS 响应里出现了被丢弃的口令：%s", raw)
	}
	mountPoint := asString(mapGet(d, "mount_point"))
	if !strings.HasSuffix(mountPoint, "/mnt/"+name) || !filepath.IsAbs(mountPoint) {
		t.Fatalf("[新增] 挂载点应在 <安装根>/mnt/%s 下，实际 %q", name, mountPoint)
	}
	_, lout, lraw := smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	row := smbRowByID(t, lout, id)
	if mapGet(row, "kind") != "nfs" || mapGet(row, "user") != "" {
		t.Errorf("[新增] 回读应为 kind=nfs / 空账号，实际 kind=%v user=%v", mapGet(row, "kind"), mapGet(row, "user"))
	}
	if strings.Contains(lraw, "should-be-dropped") {
		t.Errorf("[新增] 列表里出现了被丢弃的口令")
	}

	// ---- ② 挂载：mount_nfs 直接跑（无 script / 无 mount_smbfs / 无口令） ----
	scriptBefore, smbfsBefore := shim.scriptLog(), shim.argvLog()
	res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[②挂载] NFS 应 200，实际 %d：%v\n%s", res.StatusCode, out["msg"], raw)
	}
	md := mapGet(out, "data")
	if mapGet(md, "mounted") != true || mapGet(md, "verified") != true {
		t.Fatalf("[②挂载] 回读必须确认已挂载，实际 mounted=%v verified=%v：%v",
			mapGet(md, "mounted"), mapGet(md, "verified"), out["msg"])
	}
	if got := asSlice(mapGet(md, "entries")); len(got) == 0 || asString(got[0]) != "fake-media.mkv" {
		t.Errorf("[②挂载] 回读到的目录内容不对：%v", got)
	}
	cmd := asString(mapGet(md, "command"))
	for _, want := range []string{"mount_nfs", "-o nosuid,nodev,soft,timeo=10,retrans=2,vers=3,rsize=1048576,wsize=1048576,ro", nfsHost + ":" + nfsExport, mountPoint} {
		if !strings.Contains(cmd, want) {
			t.Errorf("[②命令构造] 命令行里缺少 %q：%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "script") || strings.Contains(cmd, "mount_smbfs") {
		t.Errorf("[②命令构造] NFS 命令行不该出现 script/mount_smbfs：%s", cmd)
	}
	argv := shim.nfsArgvLog()
	if strings.TrimSpace(argv) == "" {
		t.Fatal("[②命令构造] 假 mount_nfs 一次都没被调用")
	}
	for _, want := range []string{"-o nosuid,nodev,soft,timeo=10,retrans=2,vers=3,rsize=1048576,wsize=1048576,ro", nfsHost + ":" + nfsExport, mountPoint} {
		if !strings.Contains(argv, want) {
			t.Errorf("[②命令构造] 假 mount_nfs 的 argv 缺少 %q：%s", want, argv)
		}
	}
	// NAS 会关机：NFS 必须 soft + timeo + retrans（有界失败，不永久挂起）。
	for _, opt := range []string{"soft", "timeo=", "retrans="} {
		if !strings.Contains(argv, opt) {
			t.Errorf("[②离线安全] NFS argv 必须带 %q：%s", opt, argv)
		}
	}
	if shim.scriptLog() != scriptBefore {
		t.Errorf("[②命令构造] NFS 挂载调用了 script：%s", shim.scriptLog())
	}
	if shim.argvLog() != smbfsBefore {
		t.Errorf("[②命令构造] NFS 挂载调用了 mount_smbfs：%s", shim.argvLog())
	}

	// ---- ③ 退出码 0 但挂载表里没有 NFS 行 ⇒ 必须报失败 ----
	shim.reset()
	shim.setNFSMode("silent")
	res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[③回读] mount_nfs 退出码 0 但没挂上，接口却报成功：%s", raw)
	}
	msg := asString(mapGet(out, "msg"))
	if !strings.Contains(msg, "不算挂载成功") || !strings.Contains(msg, "挂载表") {
		t.Errorf("[③回读] 应说明回读不一致（挂载表里没有），实际：%v", msg)
	}
	if !strings.Contains(msg, "mount_nfs") {
		t.Errorf("[③回读] NFS 的结论不该说 mount_smbfs，实际：%v", msg)
	}

	// ---- ④ 挂载表里有 NFS 行、目录却读不到 ⇒ 必须报失败 ----
	if os.Geteuid() != 0 { // root 不受 000 权限限制
		shim.reset()
		shim.setNFSMode("nodir")
		res, out, raw = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
		if res.StatusCode == 200 {
			t.Fatalf("[④回读] 目录读不到却报成功：%s", raw)
		}
		if !strings.Contains(asString(mapGet(out, "msg")), "读不到目录内容") {
			t.Errorf("[④回读] 应说明读不到目录，实际：%v", out["msg"])
		}
		_ = os.Chmod(mountPoint, 0o755)
		_ = os.Remove(filepath.Join(shim.dir, "state", "nfs"))
	}

	// ---- NFS 错误分类是人话，且不套用 SMB 的结论 ----
	for _, c := range []struct{ mode, want, notWant string }{
		{"noexport", "导出路径不存在", "认证失败"},
		{"unreachable", "连不上 NFS 服务器", "连不上 NAS"},
		{"denied", "拒绝访问", "认证失败"},
	} {
		shim.reset()
		shim.setNFSMode(c.mode)
		res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
		if res.StatusCode == 200 {
			t.Fatalf("[分类%s] 失败场景却报成功", c.mode)
		}
		msg := asString(mapGet(out, "msg"))
		if !strings.Contains(msg, c.want) {
			t.Errorf("[分类%s] 结论应含「%s」，实际 %q", c.mode, c.want, msg)
		}
		if c.notWant != "" && strings.Contains(msg, c.notWant) {
			t.Errorf("[分类%s] 不该套用 SMB 的结论「%s」：%q", c.mode, c.notWant, msg)
		}
		row := smbRowByID(t, loutOr(t, ts, cookies), id)
		if mapGet(row, "mounted") != false {
			t.Errorf("[分类%s] 失败后 mounted 应为 false，实际 %v", c.mode, mapGet(row, "mounted"))
		}
	}

	// ---- 卸载：状态跟真实挂载表走 ----
	shim.reset()
	shim.setNFSMode("ok")
	if res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies); res.StatusCode != 200 {
		t.Fatalf("[卸载前] NFS 重新挂载应成功：%v", out["msg"])
	}
	if res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/unmount", map[string]any{}, cookies); res.StatusCode != 200 {
		t.Fatalf("[卸载] NFS 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	_, lout, _ = smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	if mapGet(smbRowByID(t, lout, id), "mounted") != false {
		t.Errorf("[卸载] 卸载后 mounted 应为 false")
	}
}

// ============================================================================
//  离线韧性门禁：NAS 关机不能让面板挂住
//
//  它钉三件事（都能失败）：
//    ① 有界读：注入故意阻塞的读 + 很短超时 ⇒ 必须快速返回超时错误，而不是一直等；
//    ② GET /system/smb 列表视图**不读挂载点内容**（注入读函数调用次数为 0）；
//    ③ 挂着但已死 ⇒ 自动那一轮 remount（unmount+mount），NAS 回来能自愈。
// ============================================================================

func TestSMBOfflineResilienceGate(t *testing.T) {
	shim := newSMBShim(t)
	srv, ts := newTestServer(t)
	res, out, _ := smbDo(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("[前置] 初始化失败 %d: %v", res.StatusCode, out)
	}
	cookies := res.Cookies()

	res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb", map[string]any{
		"kind": "nfs", "name": "nfsmedia", "host": "nas.local", "share": "/volume1/media", "read_only": true,
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[前置] 新增 NFS 失败 %d: %v", res.StatusCode, out["msg"])
	}
	id := asString(mapGet(mapGet(out, "data"), "id"))

	// ---- ① 有界读：阻塞读 + 150ms 超时 ⇒ 必须快速失败 ----
	restoreRead := smb.SetReadDirForTest(func(string) ([]os.DirEntry, error) {
		time.Sleep(10 * time.Second)
		return nil, nil
	})
	restoreTimeout := smb.SetReadDirTimeoutForTest(150 * time.Millisecond)
	start := time.Now()
	res, out, raw := smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies)
	elapsed := time.Since(start)
	restoreTimeout()
	restoreRead()
	if res.StatusCode == 200 {
		t.Fatalf("[①有界读] 目录读不到却报成功：%s", raw)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("[①有界读] 读挂载点把请求挂住了 %v（有界读没生效）", elapsed)
	}
	if msg := asString(mapGet(out, "msg")); !strings.Contains(msg, "挂载点无响应") {
		t.Errorf("[①有界读] 应返回人话超时错误，实际：%v", msg)
	}
	_ = os.Remove(filepath.Join(shim.dir, "state", "nfs")) // 清掉假 mount_nfs 写下的状态

	// ---- 前置：真正挂上一次，给 ③ 造"挂着但已死" ----
	shim.setNFSMode("ok")
	if res, out, _ = smbDo(t, ts, "POST", "/api/v1/system/smb/"+id+"/mount", map[string]any{}, cookies); res.StatusCode != 200 {
		t.Fatalf("[前置] 正常挂载应成功：%v", out["msg"])
	}

	// ---- ② 列表视图不读挂载点内容 ----
	var reads atomic.Int32
	restoreCount := smb.SetReadDirForTest(func(p string) ([]os.DirEntry, error) {
		reads.Add(1)
		return os.ReadDir(p)
	})
	reads.Store(0)
	_, lout, _ := smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	if n := reads.Load(); n != 0 {
		t.Errorf("[②列表不读] GET /system/smb 读了挂载点 %d 次（NAS 离线时会把列表拖死）", n)
	}
	if mapGet(smbRowByID(t, lout, id), "mounted") != true {
		t.Errorf("[②列表不读] 列表应如实显示已挂载")
	}

	// ---- ③ 挂着但已死 ⇒ 自动那一轮 remount ----
	// 探测那一次读失败（死挂载的典型 EIO，模拟 NAS 掉线），后续读恢复 ⇒ remount 应成功。
	var calls atomic.Int32
	restoreDead := smb.SetReadDirForTest(func(p string) ([]os.DirEntry, error) {
		if calls.Add(1) == 1 {
			return nil, syscall.EIO
		}
		return os.ReadDir(p)
	})
	before := strings.Count(shim.nfsArgvLog(), "\n")
	srv.smbAutoMountPass(context.Background())
	after := strings.Count(shim.nfsArgvLog(), "\n")
	restoreDead()
	restoreCount()
	if after <= before {
		t.Errorf("[③自愈] 挂着但读不到时应 remount，mount_nfs 调用次数没变（%d → %d）", before, after)
	}
	_, lout, _ = smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	row := smbRowByID(t, lout, id)
	if mapGet(row, "mounted") != true {
		t.Errorf("[③自愈] remount 后应已挂载，实际 %v", mapGet(row, "mounted"))
	}
	if le := asString(mapGet(row, "last_error")); le != "" {
		t.Errorf("[③自愈] remount 成功后 last_error 应清空，实际 %q", le)
	}

	// ---- ④ 负向对照：不是"失去响应"的读错误**不许**触发 remount ----
	// 权限类错误重挂一百次也没用，还会把正在扫库的 Jellyfin 打断。
	restoreDenied := smb.SetReadDirForTest(func(string) ([]os.DirEntry, error) {
		return nil, syscall.EACCES
	})
	before = strings.Count(shim.nfsArgvLog(), "\n")
	srv.smbAutoMountPass(context.Background())
	after = strings.Count(shim.nfsArgvLog(), "\n")
	restoreDenied()
	restoreCount()
	if after != before {
		t.Errorf("[④不重挂] 权限类读错误不该 remount，mount_nfs 调用次数 %d → %d", before, after)
	}
	_, lout, _ = smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	if le := asString(mapGet(smbRowByID(t, lout, id), "last_error")); !strings.Contains(le, "读不到内容") {
		t.Errorf("[④不重挂] 应如实记下非超时原因，实际 %q", le)
	}
}

// loutOr 拉一次网络盘列表（分类断言里要回读状态）。
func loutOr(t *testing.T, ts *httptest.Server, cookies []*http.Cookie) map[string]any {
	t.Helper()
	_, out, _ := smbDo(t, ts, "GET", "/api/v1/system/smb", nil, cookies)
	return out
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

// TestNFSConfigPure 钉纯函数：老配置（无 kind）仍是 SMB；NFS 校验 / 凭据清空 / argv 构造。
func TestNFSConfigPure(t *testing.T) {
	// 老配置没有 kind ⇒ 必须继续当 SMB（原 SMB 校验一字不改）。
	old := smb.Mount{Name: "nasmedia", Host: "nas.local", Share: "Media", User: "alice", Password: "x"}
	if err := smb.Normalize(&old); err != nil {
		t.Fatalf("老配置（无 kind）应通过校验：%v", err)
	}
	if old.KindOrDefault() != smb.KindSMB || old.Kind != smb.KindSMB {
		t.Fatalf("老配置应归一化为 smb，实际 kind=%q", old.Kind)
	}
	if got, want := (smb.Mount{Host: "h", Share: "s"}).Source(), "//h/s"; got != want {
		t.Fatalf("空 kind 的 Source 应按 SMB 构造，实际 %q", got)
	}

	// NFS：合法导出路径通过，账号/域/口令被清空。
	nfs := smb.Mount{
		Kind: smb.KindNFS, Name: "nfsmedia", Host: "nas.local", Share: "/volume1/media",
		User: "someone", Domain: "DOM", Password: "p", ReadOnly: true,
	}
	if err := smb.Normalize(&nfs); err != nil {
		t.Fatalf("合法 NFS 应通过校验：%v", err)
	}
	if nfs.User != "" || nfs.Domain != "" || nfs.Password != "" {
		t.Fatalf("NFS 凭据必须清空，实际 user=%q domain=%q password_set=%v", nfs.User, nfs.Domain, nfs.Password != "")
	}
	if got, want := nfs.Source(), "nas.local:/volume1/media"; got != want {
		t.Fatalf("NFS Source 不对：%q", got)
	}
	wantArgv := strings.Join([]string{smb.LookBin("mount_nfs"), "-o", "nosuid,nodev,soft,timeo=10,retrans=2,vers=3,ro", "nas.local:/volume1/media", "/mnt/nfsmedia"}, " ")
	if got := strings.Join(smb.MountArgs(nfs, "/mnt/nfsmedia"), " "); got != wantArgv {
		t.Fatalf("NFS argv 不对：\n got %q\nwant %q", got, wantArgv)
	}
	// 未知类型必须拒绝（不能默默当 SMB）。
	if err := smb.Normalize(&smb.Mount{Kind: "afp", Name: "x", Host: "h", Share: "s"}); err == nil {
		t.Fatal("未知类型应被拒绝")
	}
}

// TestSMBListSharesGate —— 「列出共享」的唯一门禁。
//
// 真机事故（2026-09-29）：用户把「名字（挂载点）」当共享名填，服务端只回
// "Unknown error: -1073741275"（0xC0000225 ⇒ NAS 上没这个共享），用户完全无从下手。
// 这条门禁钉三件事：只回 Disk、口令照样只走 pty 且不进 argv/响应、通道真的通。
func TestSMBListSharesGate(t *testing.T) {
	shim := newSMBShim(t)
	_, ts := newTestServer(t)
	res, _, _ := smbDo(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("[前置] 初始化失败 %d", res.StatusCode)
	}
	cookies := res.Cookies()
	const secret = "S3cr3t-Pass-123"

	res, out, raw := smbDo(t, ts, "POST", "/api/v1/system/smb/shares",
		map[string]any{"host": "192.0.2.10", "user": "alice", "password": secret}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[列出共享] 应 200，实际 %d：%v\n%s", res.StatusCode, out["msg"], raw)
	}
	var names []string
	for _, it := range asSlice(mapGet(mapGet(out, "data"), "shares")) {
		names = append(names, asString(mapGet(it, "name")))
	}
	if len(names) != 2 || names[0] != "Media" || names[1] != "Photos" {
		t.Errorf("[列出共享] 只该回 Disk 共享且保序，实际 %v", names)
	}
	if strings.Contains(strings.Join(names, ","), "IPC$") {
		t.Errorf("[列出共享] IPC$ 这类 Pipe 不该进 shares 列表：%v", names)
	}
	if !strings.Contains(raw, "********") {
		t.Errorf("[口令] 原样输出里的口令必须被 scrub：%s", raw)
	}
	// 口令：既不在响应里，也不在 argv 里；但它必须真的通过 pty 送到了 smbutil。
	if strings.Contains(raw, secret) {
		t.Errorf("[口令] 响应里出现了口令明文")
	}
	if strings.Contains(shim.smbutilArgv(), secret) {
		t.Errorf("[口令] smbutil 的 argv 里有口令明文：%s", shim.smbutilArgv())
	}
	if got := shim.read("smbutil-password.txt"); got != secret {
		t.Fatalf("[口令通道] 假 smbutil 应从 /dev/tty 读到口令，实际 %q", got)
	}
	if !strings.Contains(shim.smbutilArgv(), "view") || !strings.Contains(shim.smbutilArgv(), "//alice@192.0.2.10") {
		t.Errorf("[命令构造] smbutil 调用形态不对：%s", shim.smbutilArgv())
	}
}
