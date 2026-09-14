package upgrade

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// watchdogScriptPath 返回看门狗脚本路径。
func watchdogScriptPath(opt Options) string {
	return filepath.Join(opt.WorkDir, "upgrade", "watchdog.sh")
}

// watchdogPlistPath 返回看门狗 plist 路径。
func watchdogPlistPath(opt Options) string {
	return filepath.Join(opt.PlistDir, WatchdogLabel+".plist")
}

// startWatchdog 生成并以独立 LaunchDaemon 的形式启动看门狗。
//
// 为什么必须做成独立的 launchd 任务，而不是 面板 的子进程：
//
//	升级的最后一步是 `launchctl kickstart -k` 重启面板，
//	那会杀掉面板进程。作为子进程的看门狗很可能被一起带走
//	（launchd 会清理该 job 的进程组），于是"没人验证、没人回滚"——
//	正好在最需要它的时候消失。
//
//	注册成独立 job 后，它有自己的生命周期，即使新版面板反复崩溃重启，
//	看门狗依然能完成判定并回滚。
func startWatchdog(ctx context.Context, opt Options, st *State, from, to string) error {
	script := watchdogScript(opt, st.RunID, from, to)
	sp := watchdogScriptPath(opt)
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(sp, []byte(script), 0o755); err != nil {
		return fmt.Errorf("写入看门狗脚本失败: %w", err)
	}

	plist := watchdogPlist(opt, sp)
	pp := watchdogPlistPath(opt)
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	tmp := pp + ".tmp"
	if err := os.WriteFile(tmp, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入看门狗 plist 失败: %w", err)
	}
	// plist 权限必须是 root:wheel 0644，否则 launchd 会拒绝加载
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, pp); err != nil {
		return fmt.Errorf("安装看门狗 plist 失败: %w", err)
	}

	// 先清掉可能残留的同名 job，再加载
	_, _ = opt.Run(ctx, "launchctl", "bootout", "system/"+WatchdogLabel)
	if out, err := opt.Run(ctx, "launchctl", "bootstrap", "system", pp); err != nil {
		return fmt.Errorf("加载看门狗失败: %v（%s）", err, strings.TrimSpace(string(out)))
	}
	_ = st
	return nil
}

// watchdogPlist 生成看门狗的 LaunchDaemon 定义。
func watchdogPlist(opt Options, scriptPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <!-- 看门狗只跑一次；失败重试交给脚本内部的循环 -->
    <key>KeepAlive</key>
    <false/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, WatchdogLabel, scriptPath,
		filepath.Join(opt.WorkDir, "upgrade", "watchdog.out.log"),
		filepath.Join(opt.WorkDir, "upgrade", "watchdog.err.log"))
}

// watchdogScript 生成看门狗脚本。
//
// 脚本刻意写得非常"笨"：不依赖面板、不依赖 Go 运行时、只用 /bin/bash 与 curl，
// 因为它的运行前提就是"新版面板可能是坏的"。
// 它做的判断也只有一件事：HTTP 健康检查能不能返回**期望的新版本号**。
func watchdogScript(opt Options, runID, from, to string) string {
	// 健康检查里带 version 字段，所以可以直接确认"跑起来的确实是新版"，
	// 而不是"有个什么东西在 8443 上应答"。
	return fmt.Sprintf(`#!/bin/bash
# 由 ZizPanel 自动生成 —— 升级看门狗。请勿手工编辑（会被下一次升级覆盖）。
#
# 职责：新版面板重启后，确认它真的能提供服务。
#   - 能：写下 success，清理自己
#   - 不能：把 *.bak 还原回去、重启面板、写下 rolled_back
#
# 为什么需要它：验证新版是否可用只能在新版起来之后做，
# 而那时发起升级的旧进程已经被杀掉，无法自己回滚。

set -u

BIN="%s"
WORK="%s"
# 这两个 label 必须分开！
# 踩过的真实事故：清理时用了面板的 label 去 bootout，
# 结果**升级成功后把面板自己卸载了** —— 服务直接消失、网站入口 502。
# 单元测试当时只断言了"调用过 bootout"，没断言 bootout 的是谁，所以没拦住。
PANEL_LABEL="%s"
SELF_LABEL="%s"
HEALTH="%s"
WANT="%s"
FROM="%s"
RUN_ID="%s"
HEALTH_TRIES=%d
ROLLBACK_TRIES=%d
RESULT="%s"
PLIST="%s"
PANEL_PLIST="%s"

mkdir -p "$(dirname "$RESULT")"

log() { echo "[$(date '+%%Y-%%m-%%d %%H:%%M:%%S')] $*"; }

# recover_panel 把面板 job 重新装一遍。
#
# 为什么需要它（2026-09-14 真机事故）：mini 从 0.4.10 升 0.5.0 时，
# 看门狗 90 秒都等不到新版，回滚之后连**旧版**也起不来，result 写着
# "需要人工介入"；launchd 那边只有一句 last exit code = 78: EX_CONFIG，
# 日志一个字都没有。而同一个二进制、同一份 config、同样的环境变量，
# 手工执行却能正常起来并返回健康页 —— 也就是说**二进制没问题，是 launchd
# 里那个 job 僵住了**。现场用 launchctl bootout + bootstrap 重新装一次
# 立刻就恢复了，随后重试升级一次通过。
# 光靠 kickstart 救不回来，所以这里在等待期间主动做一次重装。
recover_panel() {
  if [ ! -f "$PANEL_PLIST" ]; then
    log "面板 plist 不存在（${PANEL_PLIST}），跳过重装"
    return
  fi
  log "健康检查仍未通过，尝试重装面板 job（bootout + bootstrap）"
  launchctl bootout "system/$PANEL_LABEL" >/dev/null 2>&1 || true
  sleep 1
  launchctl bootstrap system "$PANEL_PLIST" >/dev/null 2>&1 || log "bootstrap 失败，继续等待"
  launchctl kickstart -k "system/$PANEL_LABEL" >/dev/null 2>&1 || log "kickstart 失败，继续等待"
}

# 等新版通过健康检查。最多 90 秒：
# 太短会把"启动稍慢"误判成失败并白白回滚；太长则用户盯着一个坏掉的面板干等。
#
# 匹配版本时必须同时接受「纯版本号」与「版本号+commit 后缀」两种形式：
#   /api/v1/health 返回的是 version.Full()，正式发布出来的二进制带 git commit，
#   形如 "0.3.1+9a304af"；而看门狗拿到的是清单里的纯版本号 "0.3.1"。
#   早期只精确匹配 "0.3.1"，于是**新版明明起来并且正常服务**，看门狗却 90 秒都
#   匹配不上，判定"启动失败"并把面板回滚掉 —— 真机上连续回滚了三次才发现。
#   （之所以以前没暴露：那些二进制是 commit=dev 的本地构建，Full() 不带后缀。）
ok=0
for i in $(seq 1 "$HEALTH_TRIES"); do
  body="$(curl -fsSk --max-time 3 "$HEALTH" 2>/dev/null || true)"
  case "$body" in
    *"\"version\":\"$WANT\""*|*"\"version\":\"$WANT+"*) ok=1; break ;;
  esac
  # 三分之一、三分之二处各试一次"重装 job"（见 recover_panel 的说明）：
  # 先给新版留出正常启动的时间，再介入，避免把慢启动误当成 job 僵死。
  if [ "$i" = "$((HEALTH_TRIES / 3))" ] || [ "$i" = "$((HEALTH_TRIES * 2 / 3))" ]; then
    recover_panel
  fi
  sleep 1
done

# 只摘掉看门狗自己。这里**绝不能**碰 PANEL_LABEL。
#
# 顺序很关键：launchctl bootout 摘掉自己时会当场杀掉本进程，
# 所以它后面的语句永远不会执行。必须先删 plist、再 bootout ——
# 反过来写的结果是 plist 永久残留在 /Library/LaunchDaemons 里
# （真机上观察到的现象），下次升级时加载会撞上旧 job。
cleanup_self() {
  rm -f "$PLIST"
  launchctl bootout "system/$SELF_LABEL" >/dev/null 2>&1 || true
}

if [ "$ok" = "1" ]; then
  log "新版 $WANT 已通过健康检查"
  printf '%%s %%s %%s %%s 新版已通过健康检查\n' "success" "$WANT" "$(date +%%s)" "$RUN_ID" > "$RESULT"
  cleanup_self
  exit 0
fi

log "新版 $WANT 在 $HEALTH_TRIES 次尝试内未通过健康检查，开始回滚到 $FROM"

rolled=0
for name in zizpanel zizpanel-helper; do
  if [ -f "$BIN/$name.bak" ]; then
    cp -f "$BIN/$name.bak" "$BIN/$name" && chmod 0755 "$BIN/$name" || log "还原 $name 失败"
  else
    log "找不到 $BIN/$name.bak，无法还原 $name"
  fi
done

launchctl kickstart -k "system/$PANEL_LABEL" >/dev/null 2>&1 || log "重启面板失败"

# 再等旧版回来说明自己活着 —— 回滚也要被验证，不能"以为回滚成功了"。
# 同样要接受 "+commit" 后缀（旧版也可能是带 commit 的构建）。
# 中途同样允许一次"重装 job"：如果刚才新版起不来是 launchd job 僵住造成的，
# 那么回滚后照样起不来（真机上就是这么演的），必须把 job 重装一遍。
for i in $(seq 1 "$ROLLBACK_TRIES"); do
  body="$(curl -fsSk --max-time 3 "$HEALTH" 2>/dev/null || true)"
  case "$body" in
    *"\"version\":\"$FROM\""*|*"\"version\":\"$FROM+"*) rolled=1; break ;;
  esac
  if [ "$i" = "$((ROLLBACK_TRIES / 3))" ] || [ "$i" = "$((ROLLBACK_TRIES * 2 / 3))" ]; then
    recover_panel
  fi
  sleep 1
done

if [ "$rolled" = "1" ]; then
  log "已回滚到 $FROM 并确认可用"
  printf '%%s %%s %%s %%s 新版未通过健康检查，已回滚\n' "rolled_back" "$FROM" "$(date +%%s)" "$RUN_ID" > "$RESULT"
else
  log "回滚后旧版仍未通过健康检查，需要人工介入"
  printf '%%s %%s %%s %%s 回滚后旧版仍未通过健康检查，需要人工介入\n' "rolled_back" "$FROM" "$(date +%%s)" "$RUN_ID" > "$RESULT"
fi

cleanup_self
exit 0
`, opt.BinDir, opt.WorkDir, opt.Label, WatchdogLabel, opt.HealthURL, to, from, runID,
		opt.HealthTries, opt.RollbackTries,
		resultPath(opt.WorkDir), watchdogPlistPath(opt),
		filepath.Join(opt.PlistDir, opt.Label+".plist"))
}

// CleanupStaleWatchdog 清掉可能残留的看门狗任务与 plist。
//
// 由面板启动时调用。看门狗理论上会自我清理，但它运行在
// "新版面板可能已经崩溃"的环境里，完全可能被强杀、机器断电、
// 或者因为上面那个"bootout 杀掉自己"的顺序问题留下残骸。
// 残留的 job 会让下一次升级的 bootstrap 撞上旧注册，
// 所以每次启动都顺手扫一遍，比事后排查便宜得多。
func CleanupStaleWatchdog(ctx context.Context, opt Options) {
	opt.withDefaults()
	// 只有在没有升级在进行中时才清理，避免打断正在工作的看门狗
	st := LoadState(opt.WorkDir)
	if st.Status == StatusApplying || st.Status == StatusRestarting {
		return
	}
	if _, err := os.Stat(watchdogPlistPath(opt)); err != nil {
		// plist 不在，但 job 可能还注册着
		_, _ = opt.Run(ctx, "launchctl", "bootout", "system/"+WatchdogLabel)
		return
	}
	_ = os.Remove(watchdogPlistPath(opt))
	if opt.Run != nil {
		_, _ = opt.Run(ctx, "launchctl", "bootout", "system/"+WatchdogLabel)
	}
}
