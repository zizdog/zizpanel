// 图片压缩 Web UI 的独立服务进程（`zizpanel imgcompress-serve`）。
//
// 它是应用「图片压缩（libvips）」的一个组成部分：由 internal/services 的
// 安装器注册成系统级 launchd（com.zizdog.imgcompress），默认只听
// 127.0.0.1:8890。面板的 internal/appproxy 会把 /imgcompress/ 反代到这里，
// 所以"端口"与"别名"两条路都能用（见 internal/web/imgcompress_web.go 顶部）。
//
// 刻意**不读面板配置、不开数据库、不需要 root**：它只做一件事 ——
// 提供压缩界面与 API，引擎用 `vips` 命令行（由 --brew-prefix 定位）。
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

func cmdImgCompressServe(args []string) error {
	fs := flag.NewFlagSet("imgcompress-serve", flag.ContinueOnError)
	listen := fs.String("listen", fmt.Sprintf("127.0.0.1:%d", services.ImgCompressPort),
		"监听地址（默认只绑回环）")
	brewPrefix := fs.String("brew-prefix", "",
		"Homebrew 前缀（用于定位 vips；为空时按环境/PATH 探测）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	srv := web.NewImgCompressServer(web.ImgCompressOptions{
		Listen:     *listen,
		BrewPrefix: *brewPrefix,
	})
	httpSrv := &http.Server{
		Addr:    srv.Listen(),
		Handler: srv.Handler(),
		// 只设读头超时：上传大图可能很慢，SSE 进度流更是长连接，
		// 写超时会把它们掐断（那正是"点了没反应/进度走到一半停住"的成因）。
		ReadHeaderTimeout: 20 * time.Second,
	}

	// 启动时如实报一次引擎状态：引擎不在也要把服务起起来 ——
	// 界面本身要能打开并明确告诉用户"引擎不可用、去应用市场装"，
	// 而不是让端口连不上、用户只能猜。
	eng := web.ImgCompressEngine(*brewPrefix)
	if eng.Available() {
		fmt.Fprintf(os.Stderr, "zizpanel %s：图片压缩网页界面 http://%s/（引擎 %s）\n",
			version.Full(), srv.Listen(), eng.Version)
	} else {
		fmt.Fprintf(os.Stderr, "zizpanel %s：图片压缩网页界面 http://%s/（引擎不可用：%s）\n",
			version.Full(), srv.Listen(), eng.Reason)
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
		fmt.Fprintf(os.Stderr, "收到信号 %s，正在关闭图片压缩界面…\n", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		return nil
	}
}
