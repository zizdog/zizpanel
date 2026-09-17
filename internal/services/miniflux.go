package services

// ============================================================================
//  Miniflux（RSS 阅读器）安装器
//
//  为什么不能走通用 brew 流程：miniflux 的瓶装服务只会"跑一个二进制"，
//  它自己不会建库。装完立刻 `brew services start` 会因连不上 PostgreSQL 而退出，
//  用户看到的是"装好了但打不开"。所以这里把数据库这件事一起做掉：
//
//    1. brew install miniflux（30 分钟超时）
//    2. brew install postgresql@17 + brew services start + pg_isready 轮询
//       （formula 的 postinstall 已经 initdb 建好集群，绝不重复初始化）
//    3. 幂等建角色 / 建库（先 SELECT 判断，再 CREATE 或 ALTER）
//    4. 写 0600 配置（口令只进配置文件与安装结果的一次性凭据区块）
//    5. miniflux -migrate（显式迁移，非零退出即失败）
//    6. brew services start miniflux + /healthz 轮询（超时即失败）
//    7. GET /v1/me 基本认证自检（失败只告警，不谎报、也不推翻已起来的服务）
//
//  两处刻意的安全设计：
//    · 口令全部由 crypto/rand 从 [A-Za-z0-9] 生成（没有引号 / 转义风险）；
//    · **带口令的 SQL 只走 psql 的 stdin**。不能走 `psql -c`/`-tAc`：本项目的
//      runAsUser 底层是 streamCmd，它会把 cmd.Args 逐字写进任务日志
//      （"命令标签必须从 cmd.Args 派生"），把 ALTER ROLE … PASSWORD '<pw>'
//      放进 argv 就等于把口令打进任务日志与审计日志。
// ============================================================================

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

const (
	minifluxFormula  = "miniflux"
	minifluxLabel    = "homebrew.mxcl.miniflux"
	minifluxPort     = 8087
	minifluxDBRole   = "miniflux"
	minifluxDBName   = "miniflux"
	minifluxAdmin    = "admin"
	minifluxLogLevel = "info"

	minifluxPostgresFormula = "postgresql@17"

	// 口令长度：数据库口令 24 位、管理员口令 20 位（都是 [A-Za-z0-9]）。
	minifluxDBPasswordLen    = 24
	minifluxAdminPasswordLen = 20
)

// minifluxSecretAlphabet 是随机口令的字符集。
//
// 刻意只用大小写字母与数字：配置文件是 KEY=VALUE 单行格式，SQL 里口令要带单引号，
// 少掉一切 quote / shell 元字符就等于少掉一整类"偶发语法错误"。
const minifluxSecretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ---------- 路径（一律从 brew 前缀推导，单测才能落在临时目录里） ----------

func (m *Manager) minifluxConfigPath() string {
	return filepath.Join(m.brewPrefix(), "etc", "miniflux.conf")
}

func (m *Manager) minifluxLogPath() string {
	return filepath.Join(m.brewPrefix(), "var", "log", "miniflux.log")
}

func (m *Manager) minifluxPostgresLogPath() string {
	return filepath.Join(m.brewPrefix(), "var", "log", minifluxPostgresFormula+".log")
}

func (m *Manager) minifluxBinary() string {
	return filepath.Join(m.brewPrefix(), "opt", minifluxFormula, "bin", minifluxFormula)
}

func (m *Manager) minifluxPsql() string {
	return filepath.Join(m.brewPrefix(), "opt", minifluxPostgresFormula, "bin", "psql")
}

func (m *Manager) minifluxPgIsReady() string {
	return filepath.Join(m.brewPrefix(), "opt", minifluxPostgresFormula, "bin", "pg_isready")
}

// ---------- 纯函数：随机口令 / 配置 / SQL 决策 ----------

// generateMinifluxSecret 生成 n 位 [A-Za-z0-9] 随机串。
//
// 用拒绝采样（丢弃 >= 248 的字节）而不是直接取模：62 不整除 256，
// 取模会让前 8 个字符出现概率略高 —— 密码学上无所谓，但这种"我知道它有点偏"
// 的东西不该出现在凭据生成代码里。
func generateMinifluxSecret(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("随机串长度必须为正数，实际 %d", n)
	}
	const limit = 256 - (256 % len(minifluxSecretAlphabet)) // 248
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, minifluxSecretAlphabet[int(b)%len(minifluxSecretAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// minifluxConfigValue 读 env 风格配置（KEY=VALUE）里某个键的值。
//
// 只认精确的键名：miniflux 读的是环境变量名，大小写不匹配就是读不到，
// 这里若做大小写折叠反而会"以为读到了、其实服务读不到"。
func minifluxConfigValue(conf, key string) string {
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// reuseMinifluxDBPassword 从已有配置的 DATABASE_URL 里取出数据库口令；取不到返回空串。
//
// 为什么要复用：重跑安装时若"配置存在则保留"，却把角色口令 ALTER 成新值，
// miniflux 读到的仍是配置里的旧口令 —— 一个本来正常的安装会被这一次重跑弄坏。
func reuseMinifluxDBPassword(conf string) string {
	raw := minifluxConfigValue(conf, "DATABASE_URL")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return ""
	}
	pw, ok := u.User.Password()
	if !ok {
		return ""
	}
	return pw
}

// minifluxConfigContent 生成配置文件内容（纯函数，便于单测把"形状"钉死）。
func minifluxConfigContent(primaryIP, dbPassword, adminPassword string) string {
	return strings.Join([]string{
		"DATABASE_URL=postgres://" + minifluxDBRole + ":" + dbPassword +
			"@127.0.0.1:5432/" + minifluxDBName + "?sslmode=disable",
		"LISTEN_ADDR=:" + strconv.Itoa(minifluxPort),
		"BASE_URL=http://" + primaryIP + ":" + strconv.Itoa(minifluxPort),
		"RUN_MIGRATIONS=1",
		"CREATE_ADMIN=1",
		"ADMIN_USERNAME=" + minifluxAdmin,
		"ADMIN_PASSWORD=" + adminPassword,
		"LOG_LEVEL=" + minifluxLogLevel,
		"",
	}, "\n")
}

// minifluxSQLStatement 是 provisioning 里的一条语句。
//
// Desc 是给用户看的一句话，**绝不含口令**；SQL 可能含口令，只允许走 stdin。
type minifluxSQLStatement struct {
	Desc string
	SQL  string
	Run  bool
}

// minifluxProvisionStatements 决定幂等的建角色 / 建库要执行哪些语句。
//
// 纯函数：单测用它把"角色在就 ALTER、不在就 CREATE；库在就跳过"钉死，
// 同时锁住"Desc 里不许出现口令"。
func minifluxProvisionStatements(roleExists, dbExists bool, dbPassword string) []minifluxSQLStatement {
	var out []minifluxSQLStatement
	if roleExists {
		out = append(out, minifluxSQLStatement{
			Desc: "角色 " + minifluxDBRole + " 已存在：更新它的登录口令",
			SQL:  "ALTER ROLE " + minifluxDBRole + " WITH LOGIN PASSWORD '" + dbPassword + "'",
			Run:  true,
		})
	} else {
		out = append(out, minifluxSQLStatement{
			Desc: "创建角色 " + minifluxDBRole,
			SQL:  "CREATE ROLE " + minifluxDBRole + " WITH LOGIN PASSWORD '" + dbPassword + "'",
			Run:  true,
		})
	}
	if dbExists {
		out = append(out, minifluxSQLStatement{Desc: "数据库 " + minifluxDBName + " 已存在（跳过创建）"})
	} else {
		out = append(out, minifluxSQLStatement{
			Desc: "创建数据库 " + minifluxDBName + "（属主 " + minifluxDBRole + "）",
			SQL:  "CREATE DATABASE " + minifluxDBName + " OWNER " + minifluxDBRole,
			Run:  true,
		})
	}
	return out
}

// pgIsReadyOutput 判断 pg_isready 的输出是不是"已接受连接"。
//
// ⚠️ 这只作**兜底**：pg_isready 的消息是本地化的，中文系统上打的是"接受连接"，
// 只看英文串会把一台已经就绪的机器判成永远不就绪（2026-09-17 mini 真机就是这样，
// 任务在 60 秒后失败，而 PostgreSQL 日志里每分钟都是"接受连接"的成功探测）。
// 真正的判据是**退出码**（pg_isready: 0=accepting、1=rejecting、2=no response），
// 见 waitMinifluxPostgres。
func pgIsReadyOutput(out string) bool {
	return strings.Contains(out, "accepting connections")
}

// redactSecrets 把已知口令从**将要写进任务日志 / 错误信息**的文本里抹掉。
//
// 外部命令与日志文件的内容不受我们控制（psql 报错、miniflux 日志都可能回显
// 连接串），而这些文本会进任务中心、SSE 与审计 —— 所以只要出现就替换掉。
func redactSecrets(text string, secrets ...string) string {
	for _, s := range secrets {
		// 太短的值替换起来只会误伤正常文本（"info" / "admin" / "true" 这类会出现在
		// 日志里）；我们生成的口令是 20~24 位，阈值取 8 足够安全。
		if len(s) >= 8 {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// ---------- 命令与 HTTP 出口（单测用 Manager 上的 override 替换） ----------

// minifluxExec 以真实用户身份执行命令；stdin 非空时作为子进程标准输入。
//
// 为什么需要 stdin 这一支：带口令的 SQL 必须避开 argv（见文件头说明），
// 而既有的 runAsUser 不支持 stdin。不带 stdin 的调用仍然直接走 runAsUser，
// 不另造一套执行路径。
func (m *Manager) minifluxExec(ctx context.Context, timeout time.Duration, stdin, name string, args ...string) (string, error) {
	if m.minifluxExecOverride != nil {
		return m.minifluxExecOverride(ctx, timeout, stdin, name, args...)
	}
	if stdin == "" {
		return m.runAsUser(ctx, timeout, name, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{"-n", "-u", m.opt.UserName, name}, args...)
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", full...)
	if m.opt.UserHome != "" {
		cmd.Env = append(os.Environ(), "HOME="+m.opt.UserHome)
	}
	cmd.Stdin = strings.NewReader(stdin)
	return streamCmd(ctx, cmd)
}

// minifluxHTTP 发一次 GET，返回状态码。user 非空时带 HTTP basic auth。
//
// 用 Go 的 HTTP 客户端而不是 `curl -u user:pass`：后者会把口令放进 argv
// （同 argv 泄露问题）。代理设置为空，保证 127.0.0.1 一定直连。
func (m *Manager) minifluxHTTP(ctx context.Context, rawURL, user, password string) (int, error) {
	if m.minifluxHTTPOverride != nil {
		return m.minifluxHTTPOverride(ctx, rawURL, user, password)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	client := &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// minifluxHealthTimeout 是"等 /healthz 就绪"的时长（单测可缩短）。
func (m *Manager) minifluxHealthTimeout() time.Duration {
	if m.minifluxHealthTimeoutOverride > 0 {
		return m.minifluxHealthTimeoutOverride
	}
	return 60 * time.Second
}

// waitMinifluxHealthy 轮询等 /healthz 返回 200。
func (m *Manager) waitMinifluxHealthy(ctx context.Context, timeout time.Duration) bool {
	healthURL := "http://127.0.0.1:" + strconv.Itoa(minifluxPort) + "/healthz"
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if code, err := m.minifluxHTTP(ctx, healthURL, "", ""); err == nil && code == http.StatusOK {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// waitMinifluxPostgres 轮询 pg_isready，直到它真的报"接受连接"。
//
// 判据是**退出码**而不是输出文本：pg_isready 的 0/1/2 是协议级约定，与语言无关；
// 而文本在中文系统上是"接受连接"（真机踩过 —— 见 pgIsReadyOutput 的注释）。
// 输出文本只作为兜底（万一某个版本对退出码的处理不同）+ 失败时的诊断信息。
//
// -d postgres：不指定库名时 pg_isready 会去连"与登录用户同名的库"，而 brew 的
// cluster 里只有 postgres / template0 / template1 —— 那会让服务端回 FATAL
// （日志里 `database "zizdog" does not exist`），某些版本据此判成 rejecting。
func (m *Manager) waitMinifluxPostgres(ctx context.Context, timeout time.Duration) bool {
	bin := m.minifluxPgIsReady()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := m.minifluxExec(ctx, 10*time.Second, "", bin,
			"-h", "127.0.0.1", "-p", "5432", "-d", "postgres")
		if err == nil {
			return true
		}
		if pgIsReadyOutput(out) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// ---------- 安装 ----------

// InstallMiniflux 安装 Miniflux 及其 PostgreSQL 17 数据库，并纳入服务管理。
//
// 幂等：包已装就跳过 brew install；配置存在就原样保留（口令按 DATABASE_URL 复用）；
// 角色/库先查询再决定 CREATE 还是 ALTER；迁移与服务启动本身可重复执行。
func (m *Manager) InstallMiniflux(ctx context.Context, res *InstallResult) error {
	if res == nil {
		res = &InstallResult{App: minifluxFormula}
	}
	if res.Name == "" {
		res.Name = "Miniflux（RSS 阅读器）"
	}

	// ---- 1. Homebrew 是硬前提 ----
	if strings.TrimSpace(m.opt.BrewBin) == "" {
		return fmt.Errorf("未配置 Homebrew 路径，无法自动安装 Miniflux。请先安装 Homebrew")
	}
	if _, err := os.Stat(m.opt.BrewBin); err != nil {
		return fmt.Errorf("未安装 Homebrew（%s 不存在），无法自动安装 Miniflux。请先安装 Homebrew", m.opt.BrewBin)
	}

	// ---- 2. miniflux 本体 ----
	if !m.brewHas(ctx, minifluxFormula) {
		res.step(ctx, "正在 brew install "+minifluxFormula+"（首次可能需要几分钟）")
		if _, err := m.brewRun(ctx, 30*time.Minute, "install", minifluxFormula); err != nil {
			return err
		}
		res.step(ctx, minifluxFormula+" 已安装")
	} else {
		res.step(ctx, minifluxFormula+" 已安装（跳过 brew install）")
	}

	// ---- 3. PostgreSQL 17 ----
	if err := m.ensureMinifluxPostgres(ctx, res); err != nil {
		return err
	}

	// ---- 4. 口令：能复用就复用，否则新生成 ----
	confPath := m.minifluxConfigPath()
	existingRaw, readErr := os.ReadFile(confPath)
	hasExisting := false
	switch {
	case readErr == nil:
		hasExisting = true
	case os.IsNotExist(readErr):
	default:
		return fmt.Errorf("读取现有配置 %s 失败: %w", confPath, readErr)
	}
	existing := string(existingRaw)

	// 数据库口令：复用现有配置里的（否则重跑会把一个能用的安装弄坏）。
	dbPassword := ""
	dbPasswordReused := false
	if hasExisting {
		dbPassword = reuseMinifluxDBPassword(existing)
		dbPasswordReused = dbPassword != ""
	}
	if dbPassword == "" {
		pw, err := generateMinifluxSecret(minifluxDBPasswordLen)
		if err != nil {
			return fmt.Errorf("生成数据库口令失败: %w", err)
		}
		dbPassword = pw
	}

	// 管理员口令：配置存在且有 ADMIN_PASSWORD 就用它（那才是真正生效的那个）；
	// 配置存在却没有这个键时**不生成也不上报**一个并不存在的口令（不谎报凭据）。
	adminPassword := ""
	adminFromConfig := false
	if hasExisting {
		adminPassword = minifluxConfigValue(existing, "ADMIN_PASSWORD")
		adminFromConfig = adminPassword != ""
	}
	if !hasExisting {
		pw, err := generateMinifluxSecret(minifluxAdminPasswordLen)
		if err != nil {
			return fmt.Errorf("生成管理员口令失败: %w", err)
		}
		adminPassword = pw
	}

	// ---- 5. 幂等建角色 / 建库 ----
	if err := m.provisionMinifluxDatabase(ctx, res, dbPassword); err != nil {
		return err
	}

	// ---- 6. 写配置（只在不存在时写，保留用户改动） ----
	if !hasExisting {
		content := minifluxConfigContent(m.primaryIP(), dbPassword, adminPassword)
		if err := m.writeMinifluxConfig(confPath, content); err != nil {
			return err
		}
		res.step(ctx, "已写入配置 "+confPath+"（权限 0600，仅真实登录用户可读；口令不会写进任务日志）")
	} else {
		res.step(ctx, "已保留现有配置 "+confPath+"（面板不覆盖你的改动）")
		if !dbPasswordReused {
			res.step(ctx, "⚠️ 现有配置的 DATABASE_URL 里没有可用口令，本次新生成的口令不会被写进该配置；"+
				"若 Miniflux 连不上库，请手工把 DATABASE_URL 改成 postgres://"+minifluxDBRole+
				":<本次凭据区块里的数据库口令>@127.0.0.1:5432/"+minifluxDBName+"?sslmode=disable")
		}
	}

	// 一次性凭据区块：**在迁移 / 启动之前**挂上 —— 后面任何一步失败，
	// 口令的唯一记录也能到达用户（与 MySQL 凭据闭环同一约定）。
	// 只上报"真实会被用到"的口令：配置被保留时，没写进配置的新口令不上报。
	if adminPassword != "" {
		label := "Miniflux 管理员口令（用户名 " + minifluxAdmin + "）"
		if adminFromConfig {
			label = "Miniflux 管理员口令（复用现有配置文件里的值）"
		}
		res.Credentials = append(res.Credentials, Credential{
			Key: "miniflux_admin_password", Value: adminPassword, Label: label,
		})
	}
	if !hasExisting || dbPasswordReused {
		label := "Miniflux 数据库口令（角色 " + minifluxDBRole + "，面板自动管理）"
		if dbPasswordReused {
			label = "Miniflux 数据库口令（复用现有配置里的值，未被改动）"
		}
		res.Credentials = append(res.Credentials, Credential{
			Key: "miniflux_db_password", Value: dbPassword, Label: label,
		})
	}

	// ---- 7. 显式迁移 ----
	res.step(ctx, "执行数据库迁移（miniflux -c <配置> -migrate）")
	if out, err := m.minifluxExec(ctx, 2*time.Minute, "", m.minifluxBinary(),
		"-c", confPath, "-migrate"); err != nil {
		return fmt.Errorf("Miniflux 数据库迁移失败: %v；命令输出（已脱敏）：%s",
			err, redactSecrets(tailText(out, 400), dbPassword, adminPassword))
	}

	// ---- 8. 启动服务 ----
	// 用 StartBrewService 而不是裸 brew services：服务一旦已经系统化（上一次
	// 安装搬到 /Library/LaunchDaemons 了），`brew services start` 会写出**第二份**
	// 用户级 plist 并把服务拉成两份抢端口。
	res.step(ctx, "注册为后台服务并启动（"+minifluxFormula+"）")
	if err := m.StartBrewService(ctx, minifluxFormula); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", minifluxFormula, err)
	}

	// ---- 9. 健康检查（失败就是失败，绝不写"已安装"） ----
	res.step(ctx, "等待 Miniflux 就绪（GET http://127.0.0.1:"+strconv.Itoa(minifluxPort)+"/healthz，最多 60 秒）")
	if !m.waitMinifluxHealthy(ctx, m.minifluxHealthTimeout()) {
		logPath := m.minifluxLogPath()
		tail, _ := tailFile(logPath, 120)
		tail = redactSecrets(strings.TrimSpace(tail), dbPassword, adminPassword)
		msg := fmt.Sprintf("Miniflux 在 60 秒内没有通过健康检查（http://127.0.0.1:%d/healthz 未返回 200）", minifluxPort)
		if tail != "" {
			msg += "；日志 " + logPath + " 末尾：" + tail
		} else {
			msg += "；日志 " + logPath + " 为空或不存在（失败可能发生在日志初始化之前）"
		}
		return fmt.Errorf("%s", msg)
	}
	res.step(ctx, "健康检查通过：http://127.0.0.1:"+strconv.Itoa(minifluxPort)+"/healthz 返回 200")

	// ---- 9.5 装成系统级服务（开机自启） ----
	//
	// 无头 macOS 开机**不会**加载 ~/Library/LaunchAgents（坑 130），所以
	// Miniflux 与它依赖的 PostgreSQL 都必须落到系统域，否则用户重启机器后
	// 面板显示"已安装"、服务却一个都没起来。
	systemized := true
	for _, id := range []string{"postgresql17", "miniflux"} {
		app, found := FindApp(id)
		if !found || !systemDaemonNeeded(app) {
			continue
		}
		if _, _, err := systemDaemonEnsureFn(m, ctx, app, res); err != nil {
			systemized = false
			res.Warning = appendWarning(res.Warning,
				app.Name+"没能装成系统级服务（重启后不会自动起来）："+err.Error())
			res.step(ctx, "警告："+app.Name+"仍以用户级服务运行（重启后需手动启动）")
		}
	}
	if systemized {
		// 搬迁会重启服务：结论必须重新测，不能沿用搬迁前的健康检查。
		if !m.waitMinifluxHealthy(ctx, m.minifluxHealthTimeout()) {
			return fmt.Errorf("装成系统级服务后 Miniflux 没有恢复健康"+
				"（http://127.0.0.1:%d/healthz 未返回 200）；看日志 %s", minifluxPort, m.minifluxLogPath())
		}
		res.step(ctx, "系统级服务复核通过：重启机器后 Miniflux 与 PostgreSQL 会自动起来")
	}

	// ---- 10. 凭据自检（失败只告警，不推翻已起来的服务） ----
	if err := m.verifyMinifluxCredentials(ctx, res, confPath, adminPassword, adminFromConfig); err != nil {
		return err
	}

	// ---- 11. 登记进服务管理 ----
	label, _, _ := m.brewServiceInfo(ctx, minifluxFormula)
	if label == "" {
		label = minifluxLabel
	}
	if err := m.RegisterInstalledService(ctx, label, "Miniflux", "📰", "tool", minifluxPort); err != nil {
		// 登记失败不让整个部署失败（与 Qwen3 TTS / 音色接收端同一取舍）：
		// launchd 服务本身是好的，只是面板列表里暂时没有它，可手工纳管。
		res.step(ctx, "（自动登记到服务管理失败："+err.Error()+"，可在「可纳管」里手动加入）")
	}

	host := m.primaryIP()
	res.Address = "http://" + host + ":" + strconv.Itoa(minifluxPort)
	res.Message = "「Miniflux（RSS 阅读器）」已安装并纳入管理"
	res.step(ctx,
		"打开 http://"+host+":"+strconv.Itoa(minifluxPort)+"，用用户名 "+minifluxAdmin+" 与上方凭据区块里的口令登录",
		"登录后建议到 Settings → Change Password 改成自己的口令")
	return nil
}

// ensureMinifluxPostgres 确保 postgresql@17 已装、已启动、真的在监听。
func (m *Manager) ensureMinifluxPostgres(ctx context.Context, res *InstallResult) error {
	if !m.brewHas(ctx, minifluxPostgresFormula) {
		res.step(ctx, "正在 brew install "+minifluxPostgresFormula+"（Miniflux 的数据库）")
		if _, err := m.brewRun(ctx, 30*time.Minute, "install", minifluxPostgresFormula); err != nil {
			return err
		}
		res.step(ctx, minifluxPostgresFormula+" 已安装")
	} else {
		res.step(ctx, minifluxPostgresFormula+" 已安装（跳过 brew install）")
	}
	// formula 的 postinstall 已经 initdb 建好集群；**绝不重复 initdb**（会清掉数据）。
	res.step(ctx, "启动 PostgreSQL（"+minifluxPostgresFormula+"）")
	if err := m.StartBrewService(ctx, minifluxPostgresFormula); err != nil {
		return fmt.Errorf("启动 %s 失败: %w；日志：%s",
			minifluxPostgresFormula, err, m.minifluxPostgresLogPath())
	}
	res.step(ctx, "等待 PostgreSQL 接受连接（pg_isready -h 127.0.0.1 -p 5432，最多 60 秒）")
	if !m.waitMinifluxPostgres(ctx, 60*time.Second) {
		return fmt.Errorf("PostgreSQL 在 60 秒内没有就绪（pg_isready 未报 accepting connections）；"+
			"请查看日志 %s。常见原因：数据目录没初始化、端口 5432 被别的实例占用，"+
			"或服务刚起来还在恢复", m.minifluxPostgresLogPath())
	}
	res.step(ctx, "PostgreSQL 已就绪")
	return nil
}

// provisionMinifluxDatabase 幂等地建角色与数据库。
func (m *Manager) provisionMinifluxDatabase(ctx context.Context, res *InstallResult, dbPassword string) error {
	user := strings.TrimSpace(m.opt.UserName)
	if user == "" {
		return fmt.Errorf("面板不知道真实登录用户（UserName 为空），无法用本机信任认证连接 PostgreSQL")
	}
	psql := m.minifluxPsql()

	roleExists, err := m.minifluxPSQLExists(ctx, psql, user,
		"SELECT 1 FROM pg_roles WHERE rolname='"+minifluxDBRole+"'")
	if err != nil {
		return err
	}
	dbExists, err := m.minifluxPSQLExists(ctx, psql, user,
		"SELECT 1 FROM pg_database WHERE datname='"+minifluxDBName+"'")
	if err != nil {
		return err
	}

	for _, st := range minifluxProvisionStatements(roleExists, dbExists, dbPassword) {
		res.step(ctx, st.Desc)
		if !st.Run {
			continue
		}
		// 带口令的语句走 stdin，命令行上只有 `-f -`。
		out, err := m.minifluxExec(ctx, 30*time.Second, st.SQL, psql,
			"-h", "127.0.0.1", "-p", "5432", "-U", user, "-d", "postgres",
			"-v", "ON_ERROR_STOP=1", "-f", "-")
		if err != nil {
			return fmt.Errorf("%s 失败: %v；输出（已脱敏）：%s",
				st.Desc, err, redactSecrets(tailText(out, 300), dbPassword))
		}
	}
	return nil
}

// minifluxPSQLExists 跑一条只读存在性查询（语句里没有口令，可以走 -tAc）。
func (m *Manager) minifluxPSQLExists(ctx context.Context, psql, user, query string) (bool, error) {
	out, err := m.minifluxExec(ctx, 20*time.Second, "", psql,
		"-h", "127.0.0.1", "-p", "5432", "-U", user, "-d", "postgres",
		"-v", "ON_ERROR_STOP=1", "-tAc", query)
	if err != nil {
		return false, fmt.Errorf("查询 PostgreSQL 失败（%s）: %v；输出：%s",
			query, err, tailText(out, 300))
	}
	return strings.TrimSpace(out) == "1", nil
}

// writeMinifluxConfig 写配置文件：0600 + 真实用户属主。
//
// 权限必须是 0600 —— 文件里是数据库口令与管理员口令；权限不对（或属主是 root）
// 时 miniflux 以真实用户身份根本读不到，服务会静默起不来。
func (m *Manager) writeMinifluxConfig(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("设置 %s 权限失败: %w", path, err)
	}
	if m.opt.UserName != "" {
		if err := chownTo(m.opt.UserName, path); err != nil {
			return fmt.Errorf("把 %s 归属改为 %s 失败: %w", path, m.opt.UserName, err)
		}
	}
	return nil
}

// verifyMinifluxCredentials 用配置里的管理员凭据打一次 /v1/me。
//
// 语义（与用户要求一致）：401/其它 → **不**让整个安装失败，但必须显式告警说清
//
//	"面板生成的口令没有生效（多半是已有同名用户）"以及怎么修。
//
// 为什么 miniflux 只在用户不存在时才用 ADMIN_PASSWORD 创建：上游
// internal/cli/create_admin.go 先 UserExists(username)，存在就跳过、绝不重置口令 ——
// 所以这三个键留在配置文件里长期是安全且幂等的。
func (m *Manager) verifyMinifluxCredentials(ctx context.Context, res *InstallResult, confPath, adminPassword string, adminFromConfig bool) error {
	if adminPassword == "" {
		msg := "现有配置文件里没有 ADMIN_PASSWORD，面板无法自检管理员凭据（也不会覆盖你的配置）；" +
			"若要重置口令，请在终端执行：" + m.minifluxBinary() + " -c " + confPath + " -reset-password"
		res.Warning = appendWarning(res.Warning, msg)
		res.step(ctx, "⚠️ "+msg)
		return nil
	}
	meURL := "http://127.0.0.1:" + strconv.Itoa(minifluxPort) + "/v1/me"
	code, err := m.minifluxHTTP(ctx, meURL, minifluxAdmin, adminPassword)
	if err == nil && code == http.StatusOK {
		src := "面板生成"
		if adminFromConfig {
			src = "现有配置里"
		}
		res.step(ctx, "已用"+src+"的管理员凭据自检成功（GET /v1/me → 200）")
		return nil
	}
	detail := "HTTP " + strconv.Itoa(code)
	if err != nil {
		detail = err.Error()
	}
	msg := "Miniflux 已启动，但用配置里的管理员凭据自检失败（" + detail + "）：" +
		"多半是这台机器上**已经存在**同名的 " + minifluxAdmin + " 用户 —— " +
		"Miniflux 只在用户不存在时才用 ADMIN_PASSWORD 创建，从不重置已有口令。" +
		"请在终端执行 `" + m.minifluxBinary() + " -c " + confPath + " -reset-password` 重置，" +
		"或用原来的口令登录 Web 界面后修改。"
	res.Warning = appendWarning(res.Warning, msg)
	res.step(ctx, "⚠️ "+msg)
	return nil
}

// ---------- 卸载 ----------

// uninstallMiniflux 卸载 Miniflux：停服务 + brew uninstall。
//
// 刻意**保留** PostgreSQL 与 miniflux 数据库（订阅、已读状态都在库里），
// 也保留配置文件。面板绝不替用户删数据 —— 要清库请自行在终端处理。
func (m *Manager) uninstallMiniflux(ctx context.Context, removeData bool, result *InstallResult) error {
	label, plist, _ := m.brewServiceInfo(ctx, minifluxFormula)
	if label == "" {
		label = minifluxLabel
	}
	if plist == "" && m.opt.UserHome != "" {
		plist = filepath.Join(m.opt.UserHome, "Library", "LaunchAgents", label+".plist")
	}
	if result != nil {
		result.step(ctx, "停止并删除 launchd 服务 "+label)
	}
	// priv.LaunchUnload 按 label 推断正确的域（brew services 起的是 gui/<uid> 用户代理），
	// 比写死 system/<label> 可靠；未加载时它自己会当成成功。
	if err := priv.LaunchUnload(label); err != nil {
		return fmt.Errorf("停止 %s 失败: %w", label, err)
	}
	if plist != "" {
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败: %w", plist, err)
		}
	}
	if _, err := m.ForgetByLabel(ctx, label); err != nil {
		return fmt.Errorf("删除面板服务记录失败: %w", err)
	}

	if m.brewHas(ctx, minifluxFormula) {
		if result != nil {
			result.step(ctx, "正在 brew uninstall "+minifluxFormula)
		}
		if _, err := m.brewRun(ctx, 5*time.Minute, "uninstall", minifluxFormula); err != nil {
			return fmt.Errorf("brew uninstall %s 失败: %w", minifluxFormula, err)
		}
	} else if result != nil {
		result.step(ctx, minifluxFormula+" 未安装，跳过 brew uninstall")
	}

	if result != nil {
		result.step(ctx, "已保留 PostgreSQL（"+minifluxPostgresFormula+"）与 "+minifluxDBName+
			" 数据库；配置文件也保留。要彻底删库请自行在终端处理（面板不会替你删数据）")
	}
	return nil
}
