package priv

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
)

// ============================================================================
//  nginx worker 属主判定：必须有"运行体优先、读不到就不猜"这条纪律
//
//  2026-09-22 用户报障（推音色样本 500，nginx 自己的 HTML 页）暴露的正是这块：
//  面板 web 层的兜底以前写的是 `worker == "" → "nobody"` —— 在 nginx.conf 没有
//  user 指令的机器上，把 client_body_temp chown 给 nobody，而 worker 跑在别人
//  名下，等于"修过了但没修好"，而且更难查。
// ============================================================================

func TestNginxWorkerOwnerPrefersRunningWorkerProcess(t *testing.T) {
	prev := nginxWorkerProbe
	nginxWorkerProbe = func() (int, int, int, bool) { return 501, 20, 4242, true }
	t.Cleanup(func() { nginxWorkerProbe = prev })

	name, uid, gid, how, ok := NginxWorkerOwner("/nonexistent/nginx.conf")
	if !ok || uid != 501 || gid != 20 {
		t.Fatalf("运行中的 worker 进程是权威判据，应当直接采信：ok=%v uid=%d gid=%d", ok, uid, gid)
	}
	if name == "" || how == "" {
		t.Errorf("必须给出用户名与判据来源（日志要用）：name=%q how=%q", name, how)
	}
}

func TestNginxWorkerOwnerFallsBackToConfThenRefusesToGuess(t *testing.T) {
	prev := nginxWorkerProbe
	nginxWorkerProbe = func() (int, int, int, bool) { return 0, 0, 0, false } // 没有 worker 在跑
	t.Cleanup(func() { nginxWorkerProbe = prev })

	me, err := user.Current()
	if err != nil {
		t.Skipf("拿不到当前用户：%v", err)
	}
	dir := t.TempDir()

	// ① nginx.conf 里有 user 指令 → 退回配置（用当前用户，保证 lookup 成功）
	conf := filepath.Join(dir, "nginx.conf")
	if werr := os.WriteFile(conf, []byte("user  "+me.Username+" staff;\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	name, uid, _, how, ok := NginxWorkerOwner(conf)
	if !ok || name != me.Username {
		t.Fatalf("没有 worker 时应退回 nginx.conf 的 user 指令：ok=%v name=%q how=%q", ok, name, how)
	}
	wantUID, _ := strconv.Atoi(me.Uid)
	if uid != wantUID {
		t.Errorf("uid 应当来自该用户：got %d want %d", uid, wantUID)
	}

	// ② 注释掉的 user 不算；没有 user → **拒绝猜**（ok=false）
	noUser := filepath.Join(dir, "nouser.conf")
	if werr := os.WriteFile(noUser, []byte("# user nobody;\nworker_processes auto;\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if _, _, _, how, ok := NginxWorkerOwner(noUser); ok {
		t.Errorf("读不到 worker 用户时必须返回 ok=false（绝不猜 nobody），实际 how=%q", how)
	} else if how == "" {
		t.Error("拒绝时要说明原因（给人看的）")
	}
}

func TestNginxTempDirsAreUnderHomebrewPrefix(t *testing.T) {
	t.Setenv("ZIZPANEL_BREW_PREFIX", "/tmp/zp-brew-test")
	dirs := nginxTempDirs()
	if len(dirs) != 6 {
		t.Fatalf("应当是父目录 + 5 个临时子目录，实际 %d 个：%v", len(dirs), dirs)
	}
	for _, d := range dirs[:1] {
		if d != filepath.Join("/tmp/zp-brew-test", "var", "run", "nginx") {
			t.Errorf("父目录不对：%s", d)
		}
	}
	for _, sub := range []string{"client_body_temp", "proxy_temp", "fastcgi_temp", "uwsgi_temp", "scgi_temp"} {
		found := false
		for _, d := range dirs {
			if filepath.Base(d) == sub {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少 %s（大请求体落盘要用它，缺了 nginx 会回自己的 500 页）", sub)
		}
	}
}
