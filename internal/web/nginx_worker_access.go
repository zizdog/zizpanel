package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
	"github.com/zizdog/zizpanel/internal/sites"
)

// ============================================================================
//  写 vhost 前的硬预检：目录必须能被 **nginx worker 用户**真的读出来（坑 212）
//
//  2026-09-20 事故：站点 root 在无 TCC 授权的外置盘上，nginx 唯一 worker 卡死
//  ⇒ 全站超时。面板（root）能读不等于 nginx 能读 —— 所以判据必须换成 worker 的
//  uid/gid，而且**读不到就拒绝写入**，不能再把事故写进生产。
// ============================================================================

// nginxReadProbeTimeout 是"以 worker 身份读一次目录"的上限：超时按读不到处理。
var nginxReadProbeTimeout = 3 * time.Second

// nginxWorkerReadDirFn 以 nginx worker 身份读一次目录；测试注入。
var nginxWorkerReadDirFn = readDirAsNginxWorker

// readDirAsNginxWorker 真正降权读目录：root 直接用 Credential setuid/setgid（不经 sudo）。
// 非 root（调试实例）只能退回模式位判据 —— 如实说清，不冒充"已经以 worker 读过"。
func readDirAsNginxWorker(ctx context.Context, dir string, uid, gid int) error {
	if os.Geteuid() != 0 {
		return sites.DirReadableBy(dir, uid, gid)
	}
	ctx, cancel := context.WithTimeout(ctx, nginxReadProbeTimeout)
	defer cancel()
	cmd := execCommand(ctx, "/bin/ls", "-a", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(uid), Gid: uint32(gid),
	}}
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("读超时（%s）—— 很像这块盘把进程卡住了", nginxReadProbeTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// ensureNginxCanReadDir 是写盘前的预检：nginx worker 读不到就**拒绝写入**。
// 路径在外接卷上时叠加"非系统卷"判据，并给出可执行的授权出路。
func (s *Server) ensureNginxCanReadDir(ctx context.Context, dir, what string) error {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	// 只判"能不能读"：目录不存在/不是目录是调用方该先拦的事，交给既有校验，
	// 这道预检不冒充"读到了"，也不为此改变旧行为。
	if st, serr := os.Stat(dir); os.IsNotExist(serr) {
		return nil
	} else if serr == nil && !st.IsDir() {
		return nil
	}
	onVolume := onNonSystemVolume(dir)
	worker, uid, gid, _, known := priv.NginxWorkerOwner(s.Cfg.NginxConf)
	if !known {
		if onVolume {
			// 身份定不了 + 外接卷：风险不可评估，绝不写盘
			return fmt.Errorf("%s读不到 %s，已拒绝写入\n%s 在外接盘上，而面板确认不了 nginx 的运行用户。"+
				"先在「磁盘」页申请外接盘授权，或给 nginx 打开「完全磁盘访问权限」后重试。", what, dir, dir)
		}
		return nil // 内建盘 + 拿不到 worker 身份：维持旧行为，只记日志
	}
	if err := nginxWorkerReadDirFn(ctx, dir, uid, gid); err != nil {
		return nginxDirUnreadableError(what, dir, worker, err, onVolume)
	}
	return nil
}

// nginxDirUnreadableError 首行是 ≤40 字的结论，细节给路径/身份/出路。
func nginxDirUnreadableError(what, dir, worker string, cause error, onVolume bool) error {
	head := "nginx 用户读不到" + what + "，已拒绝写入"
	if onVolume {
		return fmt.Errorf("%s\n%s（%s）在外接盘上，nginx 以 %s 运行却读不进去：%v。\n"+
			"先在「磁盘」页申请外接盘授权，或给 nginx 打开「完全磁盘访问权限」后重试。",
			head, dir, what, worker, cause)
	}
	return fmt.Errorf("%s\n%s（%s）对 nginx 用户 %s 不可读：%v。\n"+
		"请改成 chmod 755，或把归属交给该用户（sudo chown -R %s %s）后重试。",
		head, dir, what, worker, cause, worker, dir)
}
