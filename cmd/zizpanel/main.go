// Command zizpanel 是面板主程序。
//
// 单文件二进制，既是 Web 服务，也是运维 CLI：
//
//	zizpanel serve                 启动面板（launchd 调用）
//	zizpanel status                查看运行状态
//	zizpanel reset-password <user> 重置密码（忘记密码时在终端自救）
//	zizpanel hash-password <pwd>   生成 bcrypt 哈希
//	zizpanel gen-cert              重新生成自签证书
//	zizpanel info                  打印环境路径（排障用）
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/auth"
	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/logx"
	"github.com/zizdog/zizpanel/internal/services"
	"github.com/zizdog/zizpanel/internal/store"
	"github.com/zizdog/zizpanel/internal/sysinfo"
	"github.com/zizdog/zizpanel/internal/tlsx"
	"github.com/zizdog/zizpanel/internal/upgrade"
	"github.com/zizdog/zizpanel/internal/version"
	"github.com/zizdog/zizpanel/internal/web"
)

// panelLaunchLabel 是面板自己的 LaunchDaemon 标签（与 install.sh 保持一致）。
const panelLaunchLabel = "cn.zizpanel.panel"

// defaultConfig 是面板配置文件的默认路径。
//
// 用函数而不是常量：必须跟随 ZIZPANEL_ROOT。
// 踩过的坑：面板以 root 运行时 $HOME 是 /var/root，若路径写死成 /opt/zizpanel，
// 那么自定义根目录安装后 `zizpanel status` 会报"未初始化"，
// 而实际上服务正在正常运行 —— 安装脚本的就绪校验因此永远失败。
func defaultConfigPath() string { return config.DefaultConfigPath() }

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "serve", "run":
		err = cmdServe(rest)
	case "status":
		err = cmdStatus(rest)
	case "info":
		err = cmdInfo(rest)
	case "reset-password", "passwd":
		err = cmdResetPassword(rest)
	case "hash-password":
		err = cmdHashPassword(rest)
	case "gen-cert":
		err = cmdGenCert(rest)
	case "version", "-v", "--version":
		// --json 是给「升级自检」用的机器可读输出。
		// 升级流程会在**替换二进制之前**先跑一次 `version --json`，
		// 确认新包真的能执行、且版本号与清单一致。
		// 所以这个输出格式一旦改动，升级自检会立刻失败（这是好事，能早发现）。
		if len(rest) > 0 && (rest[0] == "--json" || rest[0] == "json") {
			out, jerr := json.Marshal(map[string]string{
				"version":    version.Version,
				"commit":     version.Commit,
				"build_time": version.BuildTime,
				"runtime":    runtime.Version(),
				"os":         runtime.GOOS,
				"arch":       runtime.GOARCH,
				// 内嵌的发布公钥。放这里有两个作用：
				//  1) 让 main 直接引用它，避免被 -trimpath 的死代码消除剪掉
				//     （否则 -ldflags -X 会静默失效，见 upgrade.PublicKeyHex 注释）
				//  2) 发布流程与用户可以核对"这个面板到底信任哪把公钥"
				"upgrade_pubkey": upgrade.PublicKeyHex(),
			})
			if jerr != nil {
				fmt.Fprintf(os.Stderr, "错误: %v\n", jerr)
				os.Exit(1)
			}
			fmt.Println(string(out))
			return
		}
		fmt.Printf("zizpanel %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
	case "sign-manifest":
		// 发布流程使用：给 manifest.json 生成 Ed25519 签名。
		// 放在主程序里而不是单独的脚本，是为了让签名与校验共用同一份代码 ——
		// 两处各写一份迟早会不一致，那种 bug 只会在用户升级失败时暴露。
		err = cmdSignManifest(rest)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`zizpanel - macOS 网站与服务管理面板

用法:
  zizpanel serve                   启动面板服务（前台运行，由 launchd 托管）
  zizpanel status                  查看运行状态与访问地址
  zizpanel info                    打印环境路径与配置摘要
  zizpanel reset-password <用户名>  重置账号密码（忘记密码时使用）
  zizpanel hash-password <密码>     生成 bcrypt 哈希（手工配置用）
  zizpanel gen-cert                重新生成自签 HTTPS 证书
  zizpanel version                 显示版本

常用参数:
  --config <路径>   指定配置文件（默认 ` + config.DefaultConfigPath() + `）
  --listen <地址>   覆盖监听地址，如 :8443 或 127.0.0.1:8443
  --log-level <级别> debug|info|warn|error
  --no-tls          以纯 HTTP 启动（仅调试）
`)
}

// ---------- serve ----------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	listen := fs.String("listen", "", "覆盖监听地址")
	logLevel := fs.String("log-level", "info", "日志级别")
	noTLS := fs.Bool("no-tls", false, "以 HTTP 启动")
	// 安全后缀：显式传 --panel-suffix ""（空串）表示"这次不要后缀"，
	// 供本地开发与端到端测试使用（它们需要访问根路径）。
	// 只有**显式传了**才覆盖配置 —— 否则会把配置里已有的后缀抹掉。
	panelSuffix := fs.String("panel-suffix", "", "覆盖面板安全后缀（传空串表示不启用）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, created, err := config.Bootstrap(*cfgPath)
	if err != nil {
		return err
	}
	// 只有显式传了 --panel-suffix 才覆盖（flag.Visit 能区分"没传"与"传了空串"）
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "panel-suffix" {
			cfg.PanelSuffix = config.NormalizePanelSuffix(*panelSuffix)
		}
	})

	// 目录先建好，再做归属自愈 —— 顺序反了的话，日志文件被别的身份占用时
	// 连修复的机会都没有。
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	// 在打开日志之前修复"面板自己不在意的那些文件"的归属。
	//
	// 真机事故：mini 上 TLS 私钥是 root:600，而面板这次以真实用户身份被拉起，
	// 读证书直接 permission denied，进程在写下任何日志之前就退出 ——
	// launchd 只报 `last exit code = 78: EX_CONFIG`，远程看到的现象就是
	// "升级完面板没了"，而日志是空的、完全无从查起。
	// 修得动就修，修不动也不阻断启动（后面打警告）。
	accessWarnings := cfg.RepairRuntimeAccess()

	if err := logx.Init(cfg.LogDir, logx.ParseLevel(*logLevel)); err != nil {
		return err
	}
	log := logx.New("main")
	if created {
		log.Info("首次初始化完成，配置文件: %s", cfg.Path())
	}
	log.Info("ZizPanel %s 启动中 (数据目录 %s)", version.Full(), cfg.DataDir)
	for _, w := range accessWarnings {
		log.Warn("运行时文件自愈：%s", w)
	}

	// 清理升级看门狗的残骸。
	//
	// 看门狗通常会自我清理，但它运行在"新版面板可能已经崩溃"的环境里，
	// 完全可能被强杀或赶上断电。残留的 job 会让下一次升级的 bootstrap
	// 撞上旧注册，因此每次启动都顺手扫一遍 —— 比事后排查便宜得多。
	// 有升级正在进行时它自己会跳过，不会打断正在工作的看门狗。
	upgrade.CleanupStaleWatchdog(context.Background(), upgrade.Options{
		BinDir:  cfg.BinDir,
		WorkDir: cfg.WorkDir,
		Label:   "cn.zizpanel.panel",
	})

	if *listen != "" {
		cfg.Listen = *listen
	}
	if *noTLS {
		cfg.TLSEnable = false
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// 启动时清一次过期会话，避免表无限增长
	if n, err := st.PurgeExpiredSessions(context.Background()); err == nil && n > 0 {
		log.Info("清理过期会话 %d 条", n)
	}

	am := auth.New(st, cfg.Secret, cfg.SessionHours, cfg.LoginMaxFail, cfg.LoginLockMins)
	col := sysinfo.NewCollector(cfg.WWWRoot)

	// 系统指标改为后台常驻采样 + 请求读缓存。
	// 原因：CPU 只能靠 `top -l 1 -n 0` 取，实测约 0.5 秒；若每个请求同步采集，
	// 并发请求会在互斥锁上排队导致延迟线性累加（实测 5 并发 = 0.65→3.28 秒）。
	col.Start(context.Background(), 3*time.Second)

	// 让「服务管理」如实反映本机已装的服务。
	// 只在"安装那一刻"登记是不够的：换机器、重装面板、或服务是用户自己装的，
	// 都会出现"明明在跑却不在列表里"。启动时对齐一次（幂等，已登记会跳过）。
	svcMgr := services.NewManager(services.NewRepository(st), services.Options{
		HelperBin: cfg.ServicePath("zizpanel-helper"),
		BrewBin:   cfg.BrewBin,
		UserHome:  cfg.UserHome,
		UserName:  cfg.User,
		UID:       cfg.UserUID,
		WorkDir:   cfg.WorkDir,
	})
	if n := svcMgr.AutoRegisterKnown(context.Background()); n > 0 {
		log.Info("已自动登记 %d 个本机服务到「服务管理」", n)
	}

	// phpMyAdmin 的配置文件由面板以 root 写、由 php-fpm 以真实用户读。
	// 归属一旦漂移（brew 升级重建、旧版本面板写入），页面就会报
	// "configuration file is not readable."，而该错误页的状态码同样是 200。
	// 启动时对齐一次，避免出现"一台机器正常、另一台报错"这种不一致。
	if fixed, err := svcMgr.RepairPhpMyAdminConfigPerm(); err != nil {
		log.Warn("检查 phpMyAdmin 配置权限失败: %v", err)
	} else if fixed {
		log.Info("已修正 phpMyAdmin 配置文件的归属（php-fpm 之前读不到它）")
	}

	// Docker 运行时（Colima）：补上缺失的开机自启，并登记到「服务管理」。
	// 这样换一台 Mac 也能自动获得与 mini 相同的 Docker 环境，不必手工建 plist。
	if reg, created := svcMgr.EnsureColimaRuntime(context.Background()); reg > 0 {
		if created {
			log.Info("已配置 Docker 运行时开机自启并登记到「服务管理」")
		} else {
			log.Info("已登记 Docker 运行时到「服务管理」")
		}
	}

	srv, err := web.New(cfg, st, am, col)
	if err != nil {
		return err
	}

	// 写 pid 文件，供 status 命令与 launchd 之外的场景使用
	pidPath := filepath.Join(cfg.RunDir, "panel.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		log.Warn("写 pid 文件失败: %v", err)
	}
	defer func() { _ = os.Remove(pidPath) }()

	// 启动时做一次环境准备与自愈（站点日志目录、nginx 的 WebSocket map 等）。
	// 只调用一次：Startup 内部会跑 nginx -t 并可能重载，重复调用纯属白做一遍。
	srv.Startup(context.Background())

	// 预热市场缓存。`brew list` 要约 1.5 秒，放在这里异步做掉，
	// 用户第一次打开市场就能命中缓存，不必等 brew。
	go srv.WarmMarket(context.Background())

	// Qwen 守温：mlx-audio 把模型放在进程内存的 dict 里，服务一重启
	// 两个模型就全变冷，而冷加载实测要 25 秒。这里常驻补载，让网站
	// 无论何时发来哪个 model 名都能立刻出声。首轮立即执行，
	// 覆盖"机器重启后面板与 Qwen 一起起来"这个最常见的场景。
	go svcMgr.StartQwenKeepWarm(context.Background())

	httpSrv := newHTTPServer(cfg, srv, log)

	// TLS：自签证书缺失或即将过期时自动重新生成
	if cfg.TLSEnable {
		hosts := []string{}
		if hn, err := os.Hostname(); err == nil {
			hosts = append(hosts, hn)
		}
		gen, err := tlsx.EnsureSelfSigned(cfg.TLSCert, cfg.TLSKey, hosts, 3650)
		if err != nil {
			return fmt.Errorf("准备 HTTPS 证书失败: %w", err)
		}
		if gen {
			log.Info("已生成自签证书: %s", cfg.TLSCert)
		}
		if exp, err := tlsx.CertExpiry(cfg.TLSCert); err == nil {
			log.Info("HTTPS 证书到期时间: %s", exp.Format("2006-01-02"))
		}
	}

	// 优雅退出：收到信号后停止接收新请求，给在途请求 10 秒
	errCh := make(chan error, 1)
	go func() {
		log.Info("监听 %s (TLS=%v, 访问模式=%s)", cfg.Listen, cfg.TLSEnable, cfg.AccessMode)
		for _, u := range panelURLs(cfg) {
			log.Info("面板地址: %s", u)
		}
		errCh <- listenAndServe(cfg, httpSrv)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case s := <-sig:
		log.Info("收到信号 %s，正在关闭…", s)
		// 先关掉终端会话（它们持有 shell 子进程），再停 HTTP 服务
		srv.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Warn("关闭超时: %v", err)
		}
		log.Info("已停止")
		return nil
	}
}

// ---------- status ----------

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrNotInstalled) {
			fmt.Println("状态: 未初始化（请先运行安装脚本）")
			return nil
		}
		return err
	}
	scheme := "https"
	if !cfg.TLSEnable {
		scheme = "http"
	}
	_ = scheme

	// 双证据判断运行状态：
	//   1) pid 文件 + ps 校验（覆盖"手工前台启动、没有 launchd"的情况）
	//   2) launchd 报告（覆盖"pid 文件缺失但服务被 launchd 托管"的情况）
	pid := readPID(filepath.Join(cfg.RunDir, "panel.pid"))
	if pid > 0 && !processAlive(pid) {
		pid = 0
	}
	source := "pid 文件"
	if pid == 0 {
		if lpid, ok := launchdRunning(panelLaunchLabel); ok {
			pid = lpid
			source = "launchd"
		}
	}
	state := "已停止"
	if pid > 0 {
		state = fmt.Sprintf("运行中 (pid %d，来自 %s)", pid, source)
	}
	urls := panelURLs(cfg)

	fmt.Printf("状态    : %s\n", state)
	fmt.Printf("版本    : %s\n", version.Full())
	fmt.Printf("监听    : %s (TLS=%v)\n", cfg.Listen, cfg.TLSEnable)
	for i, u := range urls {
		label := "本机访问"
		if i == 1 {
			label = "远程访问"
		}
		fmt.Printf("%s: %s\n", label, u)
	}
	fmt.Printf("访问模式: %s\n", cfg.AccessMode)
	fmt.Printf("网站目录: %s\n", cfg.WWWRoot)
	fmt.Printf("数据目录: %s\n", cfg.DataDir)
	fmt.Printf("日志目录: %s\n", cfg.LogDir)
	return nil
}

// ---------- info ----------

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("配置文件 : %s\n", cfg.Path())
	fmt.Printf("数据目录 : %s\n", cfg.DataDir)
	fmt.Printf("工作目录 : %s\n", cfg.WorkDir)
	fmt.Printf("日志目录 : %s\n", cfg.LogDir)
	fmt.Printf("网站根   : %s\n", cfg.WWWRoot)
	fmt.Printf("Homebrew : %s\n", cfg.BrewPrefix)
	fmt.Printf("Nginx    : bin=%s conf=%s vhosts=%s\n", cfg.NginxBin, cfg.NginxConf, cfg.VhostDir)
	fmt.Printf("PHP      : svc=%s ver=%s etc=%s\n", cfg.PHPSvc, cfg.PHPVer, cfg.PHPEtc)
	fmt.Printf("MySQL    : svc=%s bin=%s\n", cfg.MySQLSvc, cfg.MySQLBin)
	fmt.Printf("Docker   : socket=%s\n", cfg.DockerSocket)
	fmt.Printf("面板版本 : %s\n", version.Full())
	return nil
}

// ---------- reset-password ----------

func cmdResetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	fromStdin := fs.Bool("stdin", false, "从标准输入读取新密码（避免密码出现在 shell 历史中）")
	fs.Usage = func() {
		fmt.Println("用法: zizpanel reset-password <用户名> [新密码]")
		fmt.Println("      zizpanel reset-password <用户名> --stdin   # 从管道读密码")
		fmt.Println()
		fmt.Println("说明: 不提供密码时进入交互输入（不回显，推荐）。")
		fmt.Println("      直接写密码会把明文留在 shell 历史里，仅建议临时使用。")
	}
	// 用户名可能出现在参数任意位置，手工挑出非 flag 参数
	// 手工分离位置参数与 flag。
	// 不能直接用 fs.Parse：用户名可能出现在 flag 之后（zizpanel reset-password -stdin admin），
	// Go 的 flag 包会在第一个非 flag 参数处停止解析，导致 --stdin 被当成用户名。
	// 因此这里用一个显式的"无值开关"集合来正确切分。
	valueless := map[string]bool{"--stdin": true, "-stdin": true}
	var pos []string
	var flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if valueless[a] {
				flags = append(flags, a)
				continue
			}
			flags = append(flags, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(pos) == 0 {
		fs.Usage()
		return errors.New("缺少用户名")
	}
	username := pos[0]
	newPwd := ""
	if len(pos) > 1 {
		newPwd = pos[1]
	}
	if *fromStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1024))
		if err != nil {
			return fmt.Errorf("读取标准输入失败: %w", err)
		}
		newPwd = strings.TrimRight(string(b), "\r\n")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := logx.Init(cfg.LogDir, logx.LevelWarn); err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	am := auth.New(st, cfg.Secret, cfg.SessionHours, cfg.LoginMaxFail, cfg.LoginLockMins)
	ctx := context.Background()
	u, err := am.UserByName(ctx, username)
	if err != nil {
		return fmt.Errorf("找不到账号 %q", username)
	}
	if newPwd == "" {
		p1, err := promptPassword("请输入新密码: ")
		if err != nil {
			return err
		}
		p2, err := promptPassword("请再次输入: ")
		if err != nil {
			return err
		}
		if p1 != p2 {
			return errors.New("两次输入不一致")
		}
		newPwd = p1
	}
	if err := am.SetPassword(ctx, u.ID, newPwd); err != nil {
		return err
	}
	fmt.Printf("✅ 账号 %s 的密码已重置，该账号的所有登录会话已失效。\n", username)
	return nil
}

func cmdHashPassword(args []string) error {
	if len(args) < 1 {
		return errors.New("用法: zizpanel hash-password <密码>")
	}
	h, err := auth.HashPassword(args[0])
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

// ---------- gen-cert ----------

func cmdGenCert(args []string) error {
	fs := flag.NewFlagSet("gen-cert", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	extra := fs.String("hosts", "", "额外域名，逗号分隔")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	hosts := []string{}
	if hn, err := os.Hostname(); err == nil {
		hosts = append(hosts, hn)
	}
	for _, h := range strings.Split(*extra, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if err := tlsx.GenerateSelfSigned(cfg.TLSCert, cfg.TLSKey, hosts, 3650); err != nil {
		return err
	}
	exp, _ := tlsx.CertExpiry(cfg.TLSCert)
	fmt.Printf("✅ 证书已生成\n   证书: %s\n   私钥: %s\n   到期: %s\n",
		cfg.TLSCert, cfg.TLSKey, exp.Format("2006-01-02 15:04:05"))
	fmt.Println("   重启面板生效: sudo launchctl kickstart -k system/cn.zizpanel.panel")
	return nil
}

// ---------- 小工具 ----------

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil {
		return 0
	}
	return pid
}

// processAlive 判断进程是否存在。
//
// 不能用 `p.Signal(syscall.Signal(0))`：面板由 LaunchDaemon 以 root 启动，
// 普通用户（这是常态 —— 用户在自己的终端里跑 `zizpanel status`）
// 无权向 root 进程发信号，signal 0 会返回 EPERM，于是明明在运行却报"已停止"。
// 决策：改用 `ps -p <pid>`，macOS 允许任何用户查询任意进程，结果可靠。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "pid=").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// launchdRunning 查询 LaunchDaemon 是否报告运行中。
//
// 这是 pid 文件之外的第二个证据来源。pid 文件可能因为权限、手工删除、
// 或面板被其它方式启动而失真；launchd 的视图才是"服务真实被托管"的权威。
func launchdRunning(label string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// launchctl print 对普通用户也能看到系统域的状态（只读查询）
	out, err := exec.CommandContext(ctx, "/bin/launchctl", "print", "system/"+label).Output()
	if err != nil {
		return 0, false
	}
	pid := 0
	running := false
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if v, ok := strings.CutPrefix(ln, "pid = "); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				pid = n
				running = true
			}
		}
		if ln == "state = running" {
			running = true
		}
	}
	return pid, running
}

func promptPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	defer fmt.Println()
	return readPassword()
}
