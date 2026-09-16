package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  "底层动作没生效 → HTTP 绝不许 2xx" 的端到端回归
//
//  真机缺陷（Mac mini，面板 0.12.9）：
//    POST /api/v1/services/sh-brew-syncthing/stop → HTTP 200 ok:true（cost≈8s），
//    而进程仍以同一 PID 监听 *:8384。
//
//  这里把 ZIZPANEL_LAUNCHCTL 指向一个假 launchctl，让整条
//  "web handler → services.Manager → nativeDriver → priv" 通路真的跑起来：
//  只替换最外层的系统命令，域解析、bootout 后的终态复核与错误分类
//  都是生产代码。单测不碰真实 launchd、不碰真实服务。
// ============================================================================

const webFakeLaunchLabel = "zz.fake.launchd.regression"

// fakeLaunchctlScript 是一个**域感知**的假 launchctl，复刻真机上的域差异：
//
//   - 作业只存在 `user/<uid>` 域（面板的 `sudo -n -u <user> brew ...` 就落在这里）；
//   - `system` 与 `gui/<uid>` 域里都查不到它（真机上 brew 服务正是如此）；
//   - bootout 只对 `user/<uid>` 域有意义：bootout-fail 模式下它报
//     125 Domain does not support specified action，而作业**依然在**。
//
// 这样旧实现（只按 plist 猜一个域、且把查询失败当未加载）会 bootout 打偏、
// 复核时把"查不到"当"没加载"，最终谎报成功；新实现会探测到 user/<uid>、
// bootout 失败后复核仍加载，如实报错。
const fakeLaunchctlScript = `#!/bin/sh
printf '%s\n' "$*" >> "$ZP_FAKE_LAUNCHCTL_LOG"
case "$1" in
  print)
    case "$2" in
      user/*)
        if [ "$ZP_FAKE_LAUNCHCTL_MODE" = "bootout-ok" ] && [ -f "$ZP_FAKE_LAUNCHCTL_MARKER" ]; then
          echo "Bad request."
          echo "Could not find service $2 in domain" >&2
          exit 113
        fi
        echo "state = running"
        echo "pid = 4242"
        echo "last exit code = 0"
        exit 0
        ;;
      *)
        echo "Bad request."
        echo "Could not find service $2 in domain" >&2
        exit 113
        ;;
    esac
    ;;
  bootout)
    case "$2" in
      user/*)
        if [ "$ZP_FAKE_LAUNCHCTL_MODE" = "bootout-ok" ]; then
          : > "$ZP_FAKE_LAUNCHCTL_MARKER"
          exit 0
        fi
        echo "Bootout failed: 125: Domain does not support specified action" >&2
        exit 1
        ;;
      *)
        echo "Could not find service $2 in domain" >&2
        exit 113
        ;;
    esac
    ;;
  kickstart|bootstrap)
    exit 0
    ;;
esac
exit 0
`

// installFakeLaunchctl 造一个假 launchctl 并把它注入到 priv 层。返回调用日志路径。
func installFakeLaunchctl(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "launchctl")
	if err := os.WriteFile(script, []byte(fakeLaunchctlScript), 0o755); err != nil {
		t.Fatalf("写假 launchctl 失败: %v", err)
	}
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("ZIZPANEL_LAUNCHCTL", script)
	t.Setenv("ZP_FAKE_LAUNCHCTL_MODE", mode)
	t.Setenv("ZP_FAKE_LAUNCHCTL_LOG", logPath)
	t.Setenv("ZP_FAKE_LAUNCHCTL_MARKER", filepath.Join(dir, "booted"))
	return logPath
}

func registerFakeNativeService(t *testing.T, srv *Server, name string) {
	t.Helper()
	err := srv.serviceRepo.Create(context.Background(), &services.Service{
		Name: name, DisplayName: name, Kind: services.KindNative,
		Category: "custom", LaunchLabel: webFakeLaunchLabel,
		Enabled: true, Managed: true,
	})
	if err != nil {
		t.Fatalf("登记测试服务失败: %v", err)
	}
}

func TestServiceStopMustNotReportSuccessWhenBootoutDidNotTakeEffect(t *testing.T) {
	srv, ts := newTestServer(t)
	logPath := installFakeLaunchctl(t, "bootout-fail")
	cookies := loginTestPanel(t, ts)
	registerFakeNativeService(t, srv, "fake-launchd-svc")

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/services/fake-launchd-svc/stop", nil, cookies)
	if res.StatusCode < 400 {
		t.Fatalf("bootout 没生效却返回 %d：%v", res.StatusCode, out)
	}
	msg, _ := out["msg"].(string)
	if !strings.Contains(msg, webFakeLaunchLabel) {
		t.Fatalf("错误信息应指明是哪个服务，实际: %q（%v）", msg, out)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "bootout user/") {
		t.Fatalf("应当定位到作业真正所在的 user/<uid> 域再 bootout，实际调用：%s", calls)
	}
	if !strings.Contains(msg, "仍加载在 user/") {
		t.Fatalf("错误信息应如实指出作业仍在 user/<uid> 域里，实际: %q", msg)
	}
}

func TestServiceStopReportsSuccessOnlyAfterJobIsGone(t *testing.T) {
	srv, ts := newTestServer(t)
	logPath := installFakeLaunchctl(t, "bootout-ok")
	cookies := loginTestPanel(t, ts)
	registerFakeNativeService(t, srv, "fake-launchd-svc")

	res, out, _ := doJSON(t, ts, "POST", "/api/v1/services/fake-launchd-svc/stop", nil, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bootout 生效后应当 200，实际 %d：%v", res.StatusCode, out)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "bootout user/") {
		t.Fatalf("应当从作业真正所在的 user/<uid> 域卸载，实际调用：%s", calls)
	}
}
