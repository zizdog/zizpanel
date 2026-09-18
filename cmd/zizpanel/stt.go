// 语音转文字 Web UI 的独立服务进程（`zizpanel stt-serve`）。
//
// 它是应用「语音转文字（whisper.cpp）」的组成部分：由 internal/services 的
// 安装器注册成系统级 launchd（com.zizdog.stt），默认只听 127.0.0.1:8892。
// 面板的 internal/appproxy 会把 /stt/ 反代到这里，所以"端口"与"别名"
// 两条路都能用（见 internal/web/stt_web.go 顶部）。
//
// 刻意**不读面板配置、不开数据库、不需要 root**：它只做一件事 ——
// 用 Homebrew 的 whisper.cpp 把音频转成文字并提供界面与 OpenAI 兼容接口。
// 这样即使面板主进程在重启，已经打开的这个页面也不会跟着挂掉。
//
// 面板设置里的镜像基址由 plist 通过 --mirror-base / --mirror-lan 传进来
// （它不读配置，但下载模型时必须遵守"镜像优先"）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/version"
	"github.com/zizdog/zizpanel/internal/web"
)

func cmdSTTServe(args []string) error {
	fs := flag.NewFlagSet("stt-serve", flag.ContinueOnError)
	listen := fs.String("listen", fmt.Sprintf("127.0.0.1:%d", services.STTPort),
		"监听地址（默认只绑回环）")
	brewPrefix := fs.String("brew-prefix", "", "Homebrew 前缀（留空则自动探测；whisper-cli 与 ffmpeg 都在它下面）")
	root := fs.String("root", "", "模型根目录（默认 <当前用户家目录>/stt）")
	modelID := fs.String("model", "", "当前档位（留空则读 <root>/current_model，再空则用默认档）")
	mirrorBase := fs.String("mirror-base", "", "镜像基址（面板设置里的值，下载模型时优先走它）")
	mirrorLAN := fs.String("mirror-lan", "", "镜像的局域网入口（可选）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prefix := strings.TrimRight(strings.TrimSpace(*brewPrefix), "/")
	if prefix == "" {
		prefix = guessBrewPrefix()
	}
	modelRoot := strings.TrimSpace(*root)
	if modelRoot == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return fmt.Errorf("无法确定用户家目录（launchd 应当以真实用户运行）：%w", herr)
		}
		modelRoot = filepath.Join(home, "stt")
	}

	srv := web.NewSTTServer(web.STTOptions{
		Listen:     *listen,
		BrewPrefix: prefix,
		Root:       modelRoot,
		Model:      strings.TrimSpace(*modelID),
		MirrorBase: strings.TrimSpace(*mirrorBase),
		MirrorLAN:  strings.TrimSpace(*mirrorLAN),
	})
	httpSrv := &http.Server{
		Addr:    srv.Listen(),
		Handler: srv.Handler(),
		// 只设读头超时：转写是"上传音频 + 长任务"，进度流是长连接（SSE），
		// 写超时会把 SSE 掐断（那正是"进度走到一半停住"的成因）。
		ReadHeaderTimeout: 20 * time.Second,
	}

	// 启动时如实报一次引擎状态：引擎不可用也要把服务起起来 ——
	// 界面本身要能打开并明确告诉用户"差什么、怎么补"，
	// 而不是让端口连不上、用户只能猜。
	eng := srv.Engine()
	h := eng.Health(context.Background())
	if h.OK {
		fmt.Fprintf(os.Stderr, "zizpanel %s：语音转文字网页界面 http://%s/（%s，当前档 %s，已装 %d/%d 档）\n",
			version.Full(), srv.Listen(), eng.CLIBin, h.Current, h.Ready, len(h.Models))
	} else {
		fmt.Fprintf(os.Stderr, "zizpanel %s：语音转文字网页界面 http://%s/（不可用：%s）\n",
			version.Full(), srv.Listen(), h.Reason)
	}
	if !h.FfmpegPresent {
		fmt.Fprintf(os.Stderr, "提示：没有找到 ffmpeg → 转码链路断了，转写会如实失败（到「应用市场 → FFmpeg」安装）\n")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case s := <-sig:
		fmt.Fprintf(os.Stderr, "收到信号 %s，正在关闭语音转文字界面…\n", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		return nil
	}
}

// guessBrewPrefix 找 Homebrew 前缀。
//
// **不写死 /opt/homebrew**：Intel Mac 上是 /usr/local。顺序与
// services.brewPrefixDefault 一致，但这里不能调它（它是 services 的内部约定），
// 所以按"磁盘上真实存在的那个"判断，推不出来才退回 Apple Silicon 默认值。
func guessBrewPrefix() string {
	for _, p := range []string{"/opt/homebrew", "/usr/local"} {
		if st, err := os.Stat(filepath.Join(p, "bin", "brew")); err == nil && !st.IsDir() {
			return p
		}
	}
	return "/opt/homebrew"
}
