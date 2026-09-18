// 语音合成 Web UI 的独立服务进程（`zizpanel speech-serve`）。
//
// 它是应用「macOS 语音合成（say）」的组成部分：由 internal/services 的安装器
// 注册成系统级 launchd（com.zizdog.macosspeech），默认只听 127.0.0.1:8891。
// 面板的 internal/appproxy 会把 /speech/ 反代到这里，所以"端口"与"别名"
// 两条路都能用（见 internal/web/speech_web.go 顶部）。
//
// 刻意**不读面板配置、不开数据库、不需要 root**：它只做一件事 ——
// 用系统自带的 /usr/bin/say 合成语音并提供界面与 OpenAI 兼容接口。
// 这样即使面板主进程在重启，已经打开的这个页面也不会跟着挂掉。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/version"
	"github.com/zizdog/zizpanel/internal/web"
)

func cmdSpeechServe(args []string) error {
	fs := flag.NewFlagSet("speech-serve", flag.ContinueOnError)
	listen := fs.String("listen", fmt.Sprintf("127.0.0.1:%d", services.MacSpeechPort),
		"监听地址（默认只绑回环）")
	sayBin := fs.String("say", services.MacSpeechSayBin, "say 可执行文件路径")
	ffmpeg := fs.String("ffmpeg", "", "ffmpeg 路径（留空则自动探测；只有 mp3 需要它）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	eng := services.NewSpeechEngine()
	if v := *sayBin; v != "" {
		eng.SayBin = v
	}
	if *ffmpeg != "" {
		eng.FfmpegBin = *ffmpeg
	}

	srv := web.NewSpeechServer(web.SpeechOptions{
		Listen: *listen,
		Engine: eng,
	})
	httpSrv := &http.Server{
		Addr:    srv.Listen(),
		Handler: srv.Handler(),
		// 只设读头超时：合成是同步的短请求，进度流是长连接（SSE），
		// 写超时会把 SSE 掐断（那正是"进度走到一半停住"的成因）。
		ReadHeaderTimeout: 20 * time.Second,
	}

	// 启动时如实报一次引擎状态：引擎不可用也要把服务起起来 ——
	// 界面本身要能打开并明确告诉用户"引擎不可用、为什么"，
	// 而不是让端口连不上、用户只能猜。
	h := eng.Health(context.Background())
	if h.OK {
		fmt.Fprintf(os.Stderr, "zizpanel %s：语音合成网页界面 http://%s/（%s，%d 个音色，中文 %d）\n",
			version.Full(), srv.Listen(), eng.SayBin, h.Voices, h.ChineseVoices)
	} else {
		fmt.Fprintf(os.Stderr, "zizpanel %s：语音合成网页界面 http://%s/（引擎不可用：%s）\n",
			version.Full(), srv.Listen(), h.Reason)
	}
	if eng.FfmpegPath() == "" {
		fmt.Fprintf(os.Stderr, "提示：没有找到 ffmpeg → mp3 格式不可用（aiff / wav / m4a 不受影响）\n")
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
		fmt.Fprintf(os.Stderr, "收到信号 %s，正在关闭语音合成界面…\n", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		return nil
	}
}
