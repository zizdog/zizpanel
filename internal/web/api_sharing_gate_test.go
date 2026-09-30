package web

// api_sharing_gate_test.go —— 「文件共享（本机对外提供）」的唯一门禁。
//
// 全部走 **PATH 垫片**（假 launchctl / sharing / nfsd / dseditgroup / pgrep / ipconfig），
// 每个假命令只在临时目录维护自己的状态文件：**绝不真的开共享、绝不改 /etc/exports、
// 绝不碰 launchctl 系统服务**。NFS 的导出表由 srv.sharingExportsPath 指到临时文件。
//
// 它钉住：
//  ① 开启 SMB ⇒ 假 launchctl 收到 enable+bootstrap 参数，且**回读**（假状态）变了才报成功；
//  ② 负向对照：假命令退出码 0 但没改假状态 ⇒ 必须报失败（不许只看退出码）；
//  ③ 加/删共享 ⇒ 假 sharing 列表跟着变；非法名字 400、越界路径 403；
//  ④ NFS 导出写临时文件：保留用户原有行、只增删我们那一行、有备份、调了 nfsd update；
//     解析失败 ⇒ 原文件字节不变；
//  ⑤ 命令注入（`; rm -rf /` / `$(...)` / 换行）⇒ 拒绝且假命令一次都没被调用；
//  ⑥ 关闭 ⇒ 回读到已停止；
//  ⑦ 用户组读不到 ⇒ 如实「未复核」，不编成员。
//
// 变异验证（把回读删掉 / 把注入校验删掉 ⇒ 本门禁必须变红）见函数注释。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 假命令 ----------

const shimSharingLaunchctl = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$d/state"
printf '%s\n' "$*" >> "$d/launchctl-argv.log"
mode=$(cat "$d/launchctl-mode" 2>/dev/null || echo ok)
case "$*" in
  *com.apple.smbd*) ;;
  *) [ -f "$d/state/passthrough" ] && exec /bin/launchctl "$@"; exit 0 ;;
esac
case "$1" in
  print)
    [ "$mode" = broken ] && { echo "Operation not permitted" >&2; exit 1; }
    if [ -f "$d/state/smbd_loaded" ]; then
      echo "system/com.apple.smbd = {"
      echo "	active count = 1"
      echo "}"
      exit 0
    fi
    echo 'Could not find service "com.apple.smbd" in domain for system' >&2
    exit 113
    ;;
  enable|disable) exit 0 ;;
  bootstrap)
    [ "$mode" = silent ] && exit 0
    : > "$d/state/smbd_loaded"; : > "$d/state/smbd_running"; exit 0 ;;
  bootout)
    [ "$mode" = silent ] && exit 0
    rm -f "$d/state/smbd_loaded" "$d/state/smbd_running"; exit 0 ;;
esac
exit 0
`

const shimSharingSharing = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$d/state"
printf '%s\n' "$*" >> "$d/sharing-argv.log"
mode=$(cat "$d/sharing-mode" 2>/dev/null || echo ok)
S="$d/state/shares.txt"
TAB=$(printf '\t')
touch "$S"
json=0; prev=""
for a in "$@"; do
  [ "$prev" = "-f" ] && [ "$a" = "json" ] && json=1
  prev="$a"
done
case "$1" in
  -l)
    if [ "$json" = "1" ]; then
      printf '{'
      first=1
      while IFS="$TAB" read -r name path ro; do
        [ -z "$name" ] && continue
        [ "$first" = "1" ] || printf ','
        first=0
        printf '"%s":{"path":"%s","smb_name":"%s","smb_shared":1,"smb_read_only":%s}' "$name" "$path" "$name" "$ro"
      done < "$S"
      printf '}\n'
      exit 0
    fi
    echo "			List of Share Points"
    while IFS="$TAB" read -r name path ro; do
      [ -z "$name" ] && continue
      printf 'name:\t\t%s\n' "$name"
      printf 'path:\t\t%s\n' "$path"
      printf '\tsmb:\t{\n\t\tread-only:\t%s\n\t}\n' "$ro"
    done < "$S"
    exit 0
    ;;
  -a)
    path="$2"; name=""; ro=0; shift 2
    while [ $# -gt 0 ]; do
      case "$1" in
        -S) name="$2"; shift 2 ;;
        -s) shift 2 ;;
        -R) ro="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ "$mode" = silent ] && exit 0
    grep -v -F "$(printf '%s\t' "$name")" "$S" > "$S.tmp" 2>/dev/null || true
    mv "$S.tmp" "$S"
    printf '%s\t%s\t%s\n' "$name" "$path" "$ro" >> "$S"
    exit 0
    ;;
  -r)
    name="$2"
    [ "$mode" = silent ] && exit 0
    grep -v -F "$(printf '%s\t' "$name")" "$S" > "$S.tmp" 2>/dev/null || true
    mv "$S.tmp" "$S"
    exit 0
    ;;
esac
exit 0
`

const shimSharingNfsd = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$d/state"
printf '%s\n' "$*" >> "$d/nfsd-argv.log"
mode=$(cat "$d/nfsd-mode" 2>/dev/null || echo ok)
case "$1" in
  status)
    if [ -f "$d/state/nfsd_enabled" ]; then echo "nfsd service is enabled"; else echo "nfsd service is disabled"; fi
    if [ -f "$d/state/nfsd_running" ]; then echo "nfsd is running"; exit 0; fi
    echo "nfsd is not running"; exit 1 ;;
  enable) : > "$d/state/nfsd_enabled"; exit 0 ;;
  disable) rm -f "$d/state/nfsd_enabled"; exit 0 ;;
  start) [ "$mode" = silent ] || : > "$d/state/nfsd_running"; exit 0 ;;
  stop) [ "$mode" = silent ] || rm -f "$d/state/nfsd_running"; exit 0 ;;
  update) exit 0 ;;
esac
exit 0
`

const shimSharingPgrep = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
[ -f "$d/state/pgrep_broken" ] && { echo "pgrep: error" >&2; exit 2; }
case "$*" in
  "-x smbd") [ -f "$d/state/smbd_running" ] && { echo 4242; exit 0; }; exit 1 ;;
  "-x nfsd") [ -f "$d/state/nfsd_running" ] && { echo 4343; exit 0; }; exit 1 ;;
esac
exit 2
`

const shimSharingDseditgroup = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$d/state"
printf '%s\n' "$*" >> "$d/dseditgroup-argv.log"
mode=$(cat "$d/dseditgroup-mode" 2>/dev/null || echo ok)
[ "$mode" = fail ] && { echo "DS Error: -14009 eDSUnknownNodeName" >&2; exit 1; }
G="$d/state/access_smb"
if [ "$1 $2" = "-o read" ]; then
  [ "$3" = "com.apple.access_smb" ] || { echo "Group not found." >&2; exit 64; }
  [ -f "$G" ] || { echo "Group not found." >&2; exit 64; }
  echo "dsAttrTypeStandard:GroupMembership -"
  while IFS= read -r m; do [ -n "$m" ] && printf '\t\t%s\n' "$m"; done < "$G"
  echo "dsAttrTypeStandard:GeneratedUID -"
  printf '\t\tFAKE-GUID\n'
  exit 0
fi
if [ "$1 $2" = "-o checkmember" ]; then
  user="$4"; group="$5"
  { [ "$group" = "com.apple.access_smb" ] && [ -f "$G" ]; } || { echo "Group not found." >&2; exit 64; }
  if grep -qxF "$user" "$G"; then echo "yes $user is a member of $group"; exit 0; fi
  echo "no $user is NOT a member of $group"; exit 0
fi
echo "Group not found." >&2
exit 64
`

const shimSharingIpconfig = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
case "$*" in
  "getifaddr en0") [ -f "$d/state/no_ip" ] && exit 1; echo "192.0.2.7"; exit 0 ;;
esac
exit 1
`

type sharingShim struct {
	t   *testing.T
	dir string
}

func newSharingShim(t *testing.T) *sharingShim {
	t.Helper()
	dir := t.TempDir()
	s := &sharingShim{t: t, dir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"launchctl": shimSharingLaunchctl, "sharing": shimSharingSharing,
		"nfsd": shimSharingNfsd, "pgrep": shimSharingPgrep,
		"dseditgroup": shimSharingDseditgroup, "ipconfig": shimSharingIpconfig,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.setMode("launchctl-mode", "ok")
	s.setMode("nfsd-mode", "ok")
	s.setMode("sharing-mode", "ok")
	s.setMode("dseditgroup-mode", "ok")
	t.Setenv("PATH", dir+":/usr/bin:/bin:/usr/sbin:/sbin")
	return s
}

func (s *sharingShim) setMode(file, mode string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, file), []byte(mode+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sharingShim) state(file string) string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "state", file))
	return string(b)
}

func (s *sharingShim) setState(file, v string) {
	s.t.Helper()
	p := filepath.Join(s.dir, "state", file)
	if v == "" {
		_ = os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// hasState 判断某个"标志文件"是否存在（假命令用文件存在性当状态，文件内容是空的）。
func (s *sharingShim) hasState(file string) bool {
	_, err := os.Stat(filepath.Join(s.dir, "state", file))
	return err == nil
}

func (s *sharingShim) log(name string) string {
	b, _ := os.ReadFile(filepath.Join(s.dir, name))
	return string(b)
}

// resetAll 把垫片侧状态清成"什么都没开、没有任何共享"。
func (s *sharingShim) resetAll() {
	s.t.Helper()
	s.setState("smbd_loaded", "")
	s.setState("smbd_running", "")
	s.setState("nfsd_running", "")
	s.setState("nfsd_enabled", "")
	s.setState("shares.txt", "")
	s.setState("pgrep_broken", "")
	s.setState("no_ip", "")
}

// exportsPath 是门禁里替代 /etc/exports 的临时文件。
func (s *sharingShim) exportsPath() string { return filepath.Join(s.dir, "etc-exports") }

func (s *sharingShim) writeExports(t *testing.T, content string) []byte {
	t.Helper()
	if err := os.WriteFile(s.exportsPath(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return []byte(content)
}

func (s *sharingShim) readExports(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(s.exportsPath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// backupsOf 找 <exports>.zp-bak-* 备份文件。
func (s *sharingShim) backupsOf(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(s.exportsPath() + ".zp-bak-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sharingState(t *testing.T, out map[string]any, kind string) map[string]any {
	t.Helper()
	d, _ := mapGet(out, "data").(map[string]any)
	svc, _ := mapGet(d, kind).(map[string]any)
	if svc == nil {
		t.Fatalf("响应里没有 %s 段：%v", kind, out)
	}
	return svc
}

func sharingShares(t *testing.T, out map[string]any, kind string) []map[string]any {
	t.Helper()
	var res []map[string]any
	for _, it := range asSlice(mapGet(sharingState(t, out, kind), "shares")) {
		row, _ := it.(map[string]any)
		if row != nil {
			res = append(res, row)
		}
	}
	return res
}

func sharingFindShare(t *testing.T, out map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, r := range sharingShares(t, out, kind) {
		if asString(mapGet(r, "name")) == name {
			return r
		}
	}
	t.Fatalf("%s 列表里找不到 %q：%v", kind, name, sharingShares(t, out, kind))
	return nil
}

// ============================================================================
//  门禁本体
// ============================================================================

func TestFileSharingGate(t *testing.T) {
	shim := newSharingShim(t)
	srv, ts := newTestServer(t)
	srv.sharingExportsPath = shim.exportsPath()

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/setup",
		map[string]string{"username": "admin", "password": "zizpanel-test-fixture-pass"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("[前置] 初始化失败 %d: %v", res.StatusCode, out)
	}
	cookies := res.Cookies()

	// 共享目录必须在白名单根（家目录）里。
	shareDir := filepath.Join(srv.Cfg.UserHome, "shareme")
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shareDirReal, err := filepath.EvalSymlinks(shareDir)
	if err != nil {
		t.Fatal(err)
	}

	// ---- ① 初始：两个服务都是「已停止」（探针拿得到确定结论） ----
	res, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[①初始] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	if got := asString(mapGet(sharingState(t, out, "smb"), "status")); got != "stopped" {
		t.Errorf("[①初始] SMB 应为 stopped，实际 %q", got)
	}
	if got := asString(mapGet(sharingState(t, out, "nfs"), "status")); got != "stopped" {
		t.Errorf("[①初始] NFS 应为 stopped，实际 %q", got)
	}
	if got := asString(mapGet(mapGet(out, "data"), "lan_ip")); got != "192.0.2.7" {
		t.Errorf("[①初始] lan_ip 应来自只读探测，实际 %q", got)
	}

	// ---- ② 开启 SMB：假 launchctl 收到预期参数 + 回读确实变了才报成功 ----
	res, out, raw := doJSON(t, ts, "POST", "/api/v1/system/sharing/smb/enable", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[②开启] 应 200，实际 %d：%v\n%s", res.StatusCode, out["msg"], raw)
	}
	argv := shim.log("launchctl-argv.log")
	for _, want := range []string{
		"enable system/com.apple.smbd",
		"bootstrap system /System/Library/LaunchDaemons/com.apple.smbd.plist",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("[②开启] 假 launchctl 没收到 %q：%s", want, argv)
		}
	}
	if !shim.hasState("smbd_loaded") {
		t.Errorf("[②开启] 回读状态没变（垫片侧仍未加载），接口却报成功")
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	if got := asString(mapGet(sharingState(t, out, "smb"), "status")); got != "running" {
		t.Errorf("[②开启] 回读 SMB 应为 running，实际 %q", got)
	}

	// launchd 已加载但 smbd 进程还没起来（socket 激活，真机常态）：仍应显示运行中。
	shim.setState("smbd_running", "")
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	if got := asString(mapGet(sharingState(t, out, "smb"), "status")); got != "running" {
		t.Errorf("[②socket 激活] launchd 已加载但进程未起时仍应 running，实际 %q", got)
	}
	shim.setState("smbd_running", "1")

	// ---- ②负向对照：退出码 0 但没改状态 ⇒ 必须报失败（删掉回读这条就变红） ----
	shim.setState("smbd_loaded", "")
	shim.setState("smbd_running", "")
	shim.setMode("launchctl-mode", "silent")
	res, out, raw = doJSON(t, ts, "POST", "/api/v1/system/sharing/smb/enable", nil, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[②回读] 退出码 0 但状态没变，接口却报成功：%s", raw)
	}
	msg := asString(mapGet(out, "msg"))
	if !strings.Contains(msg, "回读") || !strings.Contains(msg, "退出码不算数") {
		t.Errorf("[②回读] 失败原因应说明是回读对不上，实际：%v", msg)
	}
	shim.setMode("launchctl-mode", "ok")

	// ---- ⑥ 关闭 SMB：回读回到已停止 ----
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/smb/disable", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[⑥关闭] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	argv = shim.log("launchctl-argv.log")
	for _, want := range []string{
		"bootout system /System/Library/LaunchDaemons/com.apple.smbd.plist",
		"disable system/com.apple.smbd",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("[⑥关闭] 假 launchctl 没收到 %q：%s", want, argv)
		}
	}
	if shim.hasState("smbd_loaded") || shim.hasState("smbd_running") {
		t.Errorf("[⑥关闭] 垫片侧状态没清干净：loaded=%v running=%v", shim.hasState("smbd_loaded"), shim.hasState("smbd_running"))
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	if got := asString(mapGet(sharingState(t, out, "smb"), "status")); got != "stopped" {
		t.Errorf("[⑥关闭] 回读 SMB 应为 stopped，实际 %q", got)
	}

	// ---- ③ 加共享：sharing -a 参数正确 + 回读列表跟着变 ----
	res, out, raw = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
		"kind": "smb", "path": shareDir, "name": "zp-test", "read_only": true,
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[③加共享] 应 200，实际 %d：%v\n%s", res.StatusCode, out["msg"], raw)
	}
	sargv := shim.log("sharing-argv.log")
	if !strings.Contains(sargv, "-a "+shareDirReal+" -S zp-test -s 001 -R 1") {
		t.Errorf("[③加共享] 假 sharing 的 argv 不对：%s", sargv)
	}
	if !strings.Contains(shim.state("shares.txt"), "zp-test\t"+shareDirReal+"\t1") {
		t.Errorf("[③加共享] 垫片侧列表没变：%q", shim.state("shares.txt"))
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	row := sharingFindShare(t, out, "smb", "zp-test")
	if mapGet(row, "read_only") != true {
		t.Errorf("[③加共享] 回读只读标记错：%v", mapGet(row, "read_only"))
	}
	if got := asString(mapGet(row, "url")); got != "smb://192.0.2.7/zp-test" {
		t.Errorf("[③加共享] 地址建议应是 smb://192.0.2.7/zp-test，实际 %q", got)
	}

	// ---- ③负向对照：sharing 退出码 0 但没写列表 ⇒ 报失败 ----
	before := shim.log("sharing-argv.log")
	shim.setMode("sharing-mode", "silent")
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
		"kind": "smb", "path": shareDir, "name": "zp-silent", "read_only": false,
	}, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[③回读] 退出码 0 但列表没变，接口却报成功")
	}
	if !strings.Contains(asString(mapGet(out, "msg")), "回读") {
		t.Errorf("[③回读] 应说明回读对不上，实际 %v", out["msg"])
	}
	if shim.log("sharing-argv.log") == before {
		t.Errorf("[③回读] 负向对照应至少调用过一次假 sharing")
	}
	shim.setMode("sharing-mode", "ok")

	// ---- ③ 非法名字 ⇒ 400；越界路径 ⇒ 403；且一律不调用假命令 ----
	cmdBefore := shim.log("sharing-argv.log")
	for _, bad := range []struct {
		name, path string
		code       int
	}{
		{"bad/name", shareDir, 400},              // 含 /
		{strings.Repeat("a", 41), shareDir, 400}, // 超长
		{"", shareDir, 400},                      // 空
		{"bad name", shareDir, 400},              // 含空格
		{"ok", "/etc", 403},                      // 越界（存在、是目录，但不在白名单根内）
	} {
		res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
			"kind": "smb", "path": bad.path, "name": bad.name, "read_only": false,
		}, cookies)
		if res.StatusCode != bad.code {
			t.Errorf("[③校验] 名字 %q / 路径 %q 应 %d，实际 %d：%v", bad.name, bad.path, bad.code, res.StatusCode, out["msg"])
		}
	}
	if shim.log("sharing-argv.log") != cmdBefore {
		t.Errorf("[③校验] 非法参数时不该调用任何共享命令：%s", shim.log("sharing-argv.log")[len(cmdBefore):])
	}

	// ---- ⑤ 命令注入：必须拒不执行，假命令一次都没被调用 ----
	injBefore := shim.log("sharing-argv.log")
	nfsInjBefore := shim.log("nfsd-argv.log")
	for _, badPath := range []string{
		shareDir + "; rm -rf /",
		filepath.Join(srv.Cfg.UserHome, "$(touch pwned)"),
		filepath.Join(srv.Cfg.UserHome, "x\ny"),
		shareDir + "`id`",
	} {
		res, _, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
			"kind": "smb", "path": badPath, "name": "inj", "read_only": false,
		}, cookies)
		if res.StatusCode == 200 {
			t.Errorf("[⑤注入] 恶意路径 %q 竟被接受", badPath)
		}
		res, _, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
			"kind": "nfs", "path": badPath, "name": badPath, "read_only": false,
		}, cookies)
		if res.StatusCode == 200 {
			t.Errorf("[⑤注入] NFS 恶意路径 %q 竟被接受", badPath)
		}
	}
	if shim.log("sharing-argv.log") != injBefore {
		t.Errorf("[⑤注入] 被拒的注入路径不该调用共享命令")
	}
	if shim.log("nfsd-argv.log") != nfsInjBefore {
		t.Errorf("[⑤注入] 被拒的注入路径不该调用 nfsd：%s", shim.log("nfsd-argv.log")[len(nfsInjBefore):])
	}
	if _, err := os.Stat(filepath.Join(srv.Cfg.UserHome, "pwned")); err == nil {
		t.Errorf("[⑤注入] 注入路径被执行了（出现了 pwned 文件）")
	}

	// ---- ③ 删共享：列表跟着变 + 回读到没了 ----
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/system/sharing/shares",
		map[string]any{"kind": "smb", "name": "zp-test"}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[③删共享] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	if strings.Contains(shim.state("shares.txt"), "zp-test") {
		t.Errorf("[③删共享] 垫片侧列表没删掉：%q", shim.state("shares.txt"))
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	if got := len(sharingShares(t, out, "smb")); got != 0 {
		t.Errorf("[③删共享] 回读列表应为空，实际 %d 条", got)
	}

	// ---- ④ NFS 开关：开启→回读 running；关闭→回读 stopped ----
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/nfs/enable", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[④NFS开启] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	if !strings.Contains(shim.log("nfsd-argv.log"), "enable") || !strings.Contains(shim.log("nfsd-argv.log"), "start") {
		t.Errorf("[④NFS开启] 假 nfsd 没收到 enable/start：%s", shim.log("nfsd-argv.log"))
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	if got := asString(mapGet(sharingState(t, out, "nfs"), "status")); got != "running" {
		t.Errorf("[④NFS开启] 回读应为 running，实际 %q", got)
	}
	shim.setState("nfsd_running", "")
	shim.setMode("nfsd-mode", "silent")
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/nfs/enable", nil, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[④NFS回读] 退出码 0 但状态没变，接口却报成功")
	}
	shim.setMode("nfsd-mode", "ok")
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/nfs/disable", nil, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[④NFS关闭] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	if got := asString(mapGet(sharingState(t, out, "nfs"), "status")); got != "stopped" {
		t.Errorf("[④NFS关闭] 回读应为 stopped，实际 %q", got)
	}

	// ---- ④ NFS 导出：写临时文件，保留用户原有行、只增删我们那一行、有备份、调 nfsd update ----
	userContent := "# 用户自己的导出\n/Users/someone/Movies -ro -mapall=nobody\n\n/Users/someone/Public -mapall=guest\n"
	shim.writeExports(t, userContent)
	updBefore := shim.log("nfsd-argv.log")
	res, out, raw = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
		"kind": "nfs", "path": shareDir, "name": shareDir, "read_only": true,
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[④导出新增] 应 200，实际 %d：%v\n%s", res.StatusCode, out["msg"], raw)
	}
	got := string(shim.readExports(t))
	for _, keep := range []string{"# 用户自己的导出", "/Users/someone/Movies -ro -mapall=nobody", "/Users/someone/Public -mapall=guest"} {
		if !strings.Contains(got, keep) {
			t.Errorf("[④导出新增] 用户原有行丢了 %q：\n%s", keep, got)
		}
	}
	wantLine := shareDirReal + " -ro -mapall=" + srv.sharingExec().User
	if !strings.Contains(got, wantLine) {
		t.Errorf("[④导出新增] 应写入 %q：\n%s", wantLine, got)
	}
	if n := strings.Count(got, shareDirReal); n != 1 {
		t.Errorf("[④导出新增] 我们那一行应只出现 1 次，实际 %d 次：\n%s", n, got)
	}
	backs := shim.backupsOf(t)
	if len(backs) == 0 {
		t.Fatalf("[④导出新增] 没有生成备份文件")
	}
	if b, _ := os.ReadFile(backs[0]); string(b) != userContent {
		t.Errorf("[④导出新增] 备份内容不是写入前的原文：\n%s", string(b))
	}
	if shim.log("nfsd-argv.log") == updBefore || !strings.Contains(shim.log("nfsd-argv.log")[len(updBefore):], "update") {
		t.Errorf("[④导出新增] 应调用 nfsd update：%s", shim.log("nfsd-argv.log"))
	}
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	nrow := sharingFindShare(t, out, "nfs", shareDirReal)
	if got := asString(mapGet(nrow, "url")); got != "nfs://192.0.2.7"+shareDirReal {
		t.Errorf("[④导出新增] 地址建议应是 nfs://192.0.2.7%s，实际 %q", shareDirReal, got)
	}

	// 再改一次（只读 → 读写）：仍然只保留我们那一行。
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
		"kind": "nfs", "path": shareDir, "name": shareDir, "read_only": false,
	}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[④导出改] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	got = string(shim.readExports(t))
	if strings.Contains(got, wantLine) {
		t.Errorf("[④导出改] 旧的只读行没被替换：\n%s", got)
	}
	if !strings.Contains(got, shareDirReal+" -mapall="+srv.sharingExec().User) {
		t.Errorf("[④导出改] 新的读写行没写进去：\n%s", got)
	}

	// 删除：用户原有行仍在，我们那一行没了。
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/system/sharing/shares",
		map[string]any{"kind": "nfs", "name": shareDirReal}, cookies)
	if res.StatusCode != 200 {
		t.Fatalf("[④导出删除] 应 200，实际 %d：%v", res.StatusCode, out["msg"])
	}
	got = string(shim.readExports(t))
	if strings.Contains(got, shareDirReal) {
		t.Errorf("[④导出删除] 我们那一行还在：\n%s", got)
	}
	if !strings.Contains(got, "/Users/someone/Movies -ro -mapall=nobody") || !strings.Contains(got, "/Users/someone/Public -mapall=guest") {
		t.Errorf("[④导出删除] 用户原有行被删了：\n%s", got)
	}

	// ---- ④ 解析失败 ⇒ 原文件字节不变、不执行任何写 ----
	badContent := userContent + "relative/path -ro\n"
	orig := shim.writeExports(t, badContent)
	nfsArgvBefore := shim.log("nfsd-argv.log")
	res, out, _ = doJSON(t, ts, "POST", "/api/v1/system/sharing/shares", map[string]any{
		"kind": "nfs", "path": shareDir, "name": shareDir, "read_only": false,
	}, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[④解析失败] 竟报成功")
	}
	if !strings.Contains(asString(mapGet(out, "msg")), "解析失败") {
		t.Errorf("[④解析失败] 应说明解析失败，实际 %v", out["msg"])
	}
	if after := shim.readExports(t); string(after) != string(orig) {
		t.Errorf("[④解析失败] 原文件被改了：\n之前：%s\n之后：%s", string(orig), string(after))
	}
	if shim.log("nfsd-argv.log") != nfsArgvBefore {
		t.Errorf("[④解析失败] 不该调用 nfsd（更不该 update）")
	}
	// 删除同样不许动原文件。
	res, out, _ = doJSON(t, ts, "DELETE", "/api/v1/system/sharing/shares",
		map[string]any{"kind": "nfs", "name": shareDirReal}, cookies)
	if res.StatusCode == 200 {
		t.Fatalf("[④解析失败] 删除竟报成功")
	}
	if after := shim.readExports(t); string(after) != string(orig) {
		t.Errorf("[④解析失败] 删除把原文件改了")
	}

	// ---- ⑦ 组信息：读失败 ⇒ 未复核；不存在 ⇒ 如实说不存在；存在 ⇒ 解析成员 ----
	shim.setMode("dseditgroup-mode", "fail")
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	acc, _ := mapGet(mapGet(out, "data"), "access").(map[string]any)
	if mapGet(acc, "verified") != false {
		t.Errorf("[⑦组] 读失败时必须 verified=false（未复核），实际 %v", mapGet(acc, "verified"))
	}
	if asString(mapGet(acc, "error")) == "" {
		t.Errorf("[⑦组] 读失败时应给出原因")
	}
	if len(asSlice(mapGet(acc, "members"))) != 0 {
		t.Errorf("[⑦组] 读不到成员时不许编成员：%v", mapGet(acc, "members"))
	}
	shim.setMode("dseditgroup-mode", "ok")
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	acc, _ = mapGet(mapGet(out, "data"), "access").(map[string]any)
	if mapGet(acc, "verified") != true || mapGet(acc, "exists") != false {
		t.Errorf("[⑦组] 组不存在时应如实 exists=false/verified=true，实际 exists=%v verified=%v",
			mapGet(acc, "exists"), mapGet(acc, "verified"))
	}
	shim.setState("access_smb", "alice\nbob\n")
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	acc, _ = mapGet(mapGet(out, "data"), "access").(map[string]any)
	if mapGet(acc, "exists") != true {
		t.Errorf("[⑦组] 组存在时应 exists=true")
	}
	members := asSlice(mapGet(acc, "members"))
	if len(members) != 2 || asString(members[0]) != "alice" {
		t.Errorf("[⑦组] 成员解析不对：%v", members)
	}

	// ---- ⑧ 状态读不到 ⇒ 「未复核」（不许猜已停止） ----
	shim.setState("pgrep_broken", "1")
	shim.setMode("launchctl-mode", "broken")
	_, out, _ = doJSON(t, ts, "GET", "/api/v1/system/sharing", nil, cookies)
	smb := sharingState(t, out, "smb")
	if got := asString(mapGet(smb, "status")); got != "unknown" {
		t.Errorf("[⑧未复核] 探针读不到时应显示 unknown，实际 %q", got)
	}
	st, _ := mapGet(smb, "state").(map[string]any)
	if mapGet(st, "running_known") != false {
		t.Errorf("[⑧未复核] running_known 应为 false")
	}
	if asString(mapGet(st, "error")) == "" {
		t.Errorf("[⑧未复核] 应给出探针失败原因")
	}
	shim.setState("pgrep_broken", "")
	shim.setMode("launchctl-mode", "ok")

	// ---- ⑨ 每个写操作都写审计（成功/失败都写） ----
	_, _, auditRaw := smbDo(t, ts, "GET", "/api/v1/audit?limit=300", nil, cookies)
	for _, want := range []string{"sharing_smb_enable", "sharing_smb_disable", "sharing_share_add", "sharing_share_remove", "sharing_nfs_enable", "sharing_export_add", "sharing_export_remove"} {
		if !strings.Contains(auditRaw, want) {
			t.Errorf("[⑨审计] 缺少 %s", want)
		}
	}
}
