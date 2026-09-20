package services

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/mysql"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  MySQL root 凭据闭环
//
//  目标一句话：**装完 MySQL 之后，面板一定知道 root 的口令**，
//  且"面板配置 / MySQL 实际 / 页面显示"三处一致。
//
//  真机事故（2026-09-16 mini，证据见 mysql/admin.go 的注释）：
//    · initMySQLDataDir 用 --initialize-insecure 把 root 建成了空口令；
//    · 面板配置里同样是空口令 —— 此刻"一致但危险"；
//    · 用户在页面上点"改密码"：ALTER USER 成功，但紧随其后的 FLUSH PRIVILEGES
//      用旧口令重新连接被拒，面板报"认证失败"，既没写回新口令也没告诉用户；
//    · 从此面板以为无口令、MySQL 要口令，所有库操作 1045。
//
//  所以这里做四件事，顺序不能变：
//    1. **只在本次是我们初始化**（--initialize-insecure）时才动 root 口令。
//       数据目录本来就存在的机器上，root 口令可能正被别的程序用着，
//       替用户改它属于破坏性操作（真机上有站点连着自己的库）。
//    2. 限时询问用户要不要自己定口令；超时或留空 → 生成强随机口令。
//    3. 设置完成后**用新口令真连一次**自检（ALTER 之后不 FLUSH 了，
//       所以这一步就是权威判定）。
//    4. 把口令写回 panel 配置（0600）。写回失败＝MySQL 已改而面板没记住，
//       这是最危险的状态，必须如实失败并把新口令放进"一次性凭据区块"。
// ============================================================================

// mysqlAdmin 是这一段需要的两个最小能力。
//
// 抽成接口只有一个目的：单测注入假实现（不许连真实 MySQL）。
// 生产实现 realMySQLAdmin 真执行 mysql CLI。
type mysqlAdmin interface {
	// Ping 用给定口令真连一次（成功返回 nil；认证失败会包住 mysql.ErrAuth）。
	Ping(ctx context.Context, password string) error
	// SetRootPassword 用"面板当前持有的口令"连上去，把 root@localhost 改成 newPassword。
	SetRootPassword(ctx context.Context, newPassword string) error
}

// mysqlCredState 是"面板持有的口令能不能用"的三种判定。
type mysqlCredState int

const (
	// mysqlCredOK：用面板持有的口令连上了（口令为空也能连上 ⇒ 服务器确实无口令）
	mysqlCredOK mysqlCredState = iota
	// mysqlCredAuthFailed：服务器有回应，但拒绝了这个口令/空口令
	mysqlCredAuthFailed
	// mysqlCredUnreachable：连不上服务器（没启动、socket 不存在、端口不通）
	mysqlCredUnreachable
)

// isMySQLFormula 判断某个 brew formula 是不是"面板要闭环 root 凭据的数据库引擎"。
//
// 覆盖 MySQL 8.4 与 MariaDB：两条安装路径（一键 LNMP 与市场单独安装）都必须覆盖到，
// 否则"从市场单独装"就会绕过凭据闭环，又回到"面板不知道 root 口令"的状态。
func isMySQLFormula(formula string) bool {
	return dbEngineOfFormula(formula) != ""
}

// mysqlAdminFor 返回生产实现（单测可用 mysqlAdminOverride 替换）。
func (m *Manager) mysqlAdminFor(cred MySQLCredential) mysqlAdmin {
	if m.mysqlAdminOverride != nil {
		return m.mysqlAdminOverride
	}
	return realMySQLAdmin{m: m, cred: cred}
}

// realMySQLAdmin 用 internal/mysql 的客户端执行真实的 mysql 命令。
//
// 为什么不在这里直接拼 exec.Command：那会把"口令怎么传（MYSQL_PWD）、
// 标识符怎么转义、1045 怎么翻译成人话"再实现一遍，
// 而这三件事已经有经过测试的一份（internal/mysql）。
type realMySQLAdmin struct {
	m    *Manager
	cred MySQLCredential
}

func (r realMySQLAdmin) client(password string) (*mysql.Client, error) {
	binDir := r.binDir()
	user := strings.TrimSpace(r.cred.User)
	if user == "" {
		user = "root"
	}
	socket := strings.TrimSpace(r.cred.Socket)
	if socket == "" {
		socket = "/tmp/mysql.sock"
	}
	host := strings.TrimSpace(r.cred.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	port := r.cred.Port
	if port == 0 {
		port = 3306
	}
	return mysql.NewClient(mysql.Options{
		BinDir:   binDir,
		Host:     host,
		Port:     port,
		Socket:   socket,
		User:     user,
		Password: password,
		Timeout:  20 * time.Second,
		UserName: r.m.opt.UserName,
		UserHome: r.m.opt.UserHome,
	}), nil
}

// binDir 解析客户端目录：优先"这次在装/在跑的那个引擎"（cred.Formula），
// 再看装着哪个引擎的 keg，最后退回 <brew>/bin。
//
// 刻意**不**因为"两个引擎都装着"就报错：这一步只需要一个能跑的客户端，
// 连的是谁由 host/socket 决定，而两个引擎的客户端协议兼容（MariaDB 还提供
// mysql 名字的兼容符号）。真正"该连哪个引擎"的判断在网站运行时与数据库页。
func (r realMySQLAdmin) binDir() string {
	prefix := r.m.brewPrefix()
	if f := strings.TrimSpace(r.cred.Formula); f != "" {
		if p, ok := mysql.ResolveEngineFor(prefix, r.cred.Socket, f); ok {
			return p.BinDir
		}
	}
	for _, f := range []string{"mysql@8.4", "mariadb"} {
		if p, ok := mysql.ResolveEngineFor(prefix, r.cred.Socket, f); ok {
			return p.BinDir
		}
	}
	return filepath.Join(prefix, "bin")
}

func (r realMySQLAdmin) Ping(ctx context.Context, password string) error {
	c, err := r.client(password)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.Ping(pctx)
}

func (r realMySQLAdmin) SetRootPassword(ctx context.Context, newPassword string) error {
	c, err := r.client(r.cred.Password)
	if err != nil {
		return err
	}
	user := strings.TrimSpace(r.cred.User)
	if user == "" {
		user = "root"
	}
	// 只改 'root'@'localhost'：面板自己走 socket/127.0.0.1 连的就是这个身份。
	// 刻意不动 'root'@'%' —— 那可能是留给别的机器远程用的，
	// 一起改掉会连累正在用它连接的程序。
	return c.SetUserPassword(ctx, user, "localhost", newPassword)
}

// ensureMySQLRootCredential 是安装收尾的"凭据闭环"。
//
// 它**必须如实失败**（返回 error）：让用户当场看到"面板的口令和 MySQL 不一致"
// 以及怎么补救，好过之后在数据库页面撞一句看不懂的 1045。
//
// 闭环成功后再校正一次 phpMyAdmin 的 AllowNoPassword（D43）：LNMP 里 phpMyAdmin
// 先装、root 口令后定，安装期写下的 AllowNoPassword 到这里已经过期。
func (m *Manager) ensureMySQLRootCredential(ctx context.Context, result *InstallResult) error {
	if err := m.ensureMySQLRootCredentialInner(ctx, result); err != nil {
		return err
	}
	m.syncPMAAllowNoPasswordAfterCredential(ctx, result)
	return nil
}

// ensureMySQLRootCredentialInner 是凭据闭环本体（拆出来只为了让 D43 的校正
// 无论走哪条成功分支都能跑一次）。
func (m *Manager) ensureMySQLRootCredentialInner(ctx context.Context, result *InstallResult) error {
	if m.opt.MySQLCredential == nil || m.opt.SetMySQLRootPassword == nil {
		// 未接入面板配置（理论上只有测试/命令行触发会这样）：
		// 如实说明跳过了，绝不假装"凭据已闭环"。
		result.step(ctx, "未接入面板配置，跳过数据库 root 凭据闭环（面板可能连不上数据库）")
		return nil
	}
	cred := m.opt.MySQLCredential()
	if strings.TrimSpace(cred.Formula) == "" {
		// 本次装的是哪个引擎：由 initMySQLDataDir 记在结果里（web 层注入的凭据
		// 只知道 host/socket，不知道引擎）。空 = 未接入，文案退回"数据库"。
		cred.Formula = result.mysqlFormula
	}
	if strings.TrimSpace(cred.User) == "" {
		cred.User = "root"
	}
	// 用户可见文案里的引擎名：装了 MariaDB 就不许写 MySQL（否则用户以为装错了）。
	name := dbEngineFormulaDisplay(cred.Formula)
	if name == "" {
		name = "数据库"
	}
	admin := m.mysqlAdminFor(cred)

	// 刚注册完 LaunchDaemon 的服务要几秒才 bind socket，
	// 所以"连不上"要在 30 秒内重试；而"口令不对"是立刻有结论的，不重试。
	state, probeErr := m.probeUntilAnswered(ctx, admin, cred.Password, m.mysqlProbeWait())
	switch state {
	case mysqlCredOK:
		if strings.TrimSpace(cred.Password) != "" {
			result.step(ctx, name+" root 凭据自检通过：面板持有的口令可以正常连接")
			return nil
		}
		// 连上了、而面板持有的口令是空的 —— 服务器此刻确实接受空口令。
		if !result.mysqlFreshInit {
			// 不是本次初始化出来的库：**不动**用户的 root 口令（那属于破坏性操作），
			// 只如实提醒。空口令意味着任何能连到 3306 的程序都能拿到全部库。
			msg := name + " root 当前没有设置口令（面板用空口令可以连上）。功能不受影响，" +
				"但任何能连到 3306 的程序都能读写全部数据库；" +
				"建议到「账号与权限」里给 root 点「改密码」设一个（面板会一并记进自己的配置）"
			result.step(ctx, "警告："+msg)
			result.Warning = appendLNMPWarning(result.Warning, msg)
			return nil
		}
		// 本次初始化（root 空口令）：现在就把口令定下来并记住。
		return m.setAndRecordMySQLPassword(ctx, result, admin, cred)

	case mysqlCredAuthFailed:
		msg := name + " root 凭据与服务器不一致：" + probeErr.Error() +
			"；面板不会自动改一台已有数据机器上的 root 口令。" + mysql.RecoveryGuide(cred.User)
		result.Warning = appendLNMPWarning(result.Warning, msg)
		return errors.New(msg)

	default: // mysqlCredUnreachable
		where := dbEngineFormulaDisplay(cred.Formula)
		if where == "" {
			where = "数据库服务"
		}
		msg := "无法连接 " + name + "（服务可能没起来或没在监听）：" + probeErr.Error() +
			"；可在「服务管理」查看 " + where + " 的日志，确认 socket/3306 就绪后重跑本任务"
		result.Warning = appendLNMPWarning(result.Warning, msg)
		return errors.New(msg)
	}
}

// mysqlProbeWait 返回"等 MySQL 给出回应"的时长。
//
// 测试里用 mysqlProbeWaitOverride 调小：否则"服务没起来"那条分支的单测
// 要真的等 30 秒（单测不许等真实时间）。
func (m *Manager) mysqlProbeWait() time.Duration {
	if m.mysqlProbeWaitOverride > 0 {
		return m.mysqlProbeWaitOverride
	}
	return 30 * time.Second
}

// probeUntilAnswered 在 timeout 内反复探测，直到服务器**给出回应**
// （无论口令对不对）。超时后返回最后一次的错误。
func (m *Manager) probeUntilAnswered(ctx context.Context, admin mysqlAdmin, password string, timeout time.Duration) (mysqlCredState, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		state, err := m.probeMySQLCredential(ctx, admin, password)
		if state != mysqlCredUnreachable {
			return state, err
		}
		lastErr = err
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return mysqlCredUnreachable, lastErr
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return mysqlCredUnreachable, lastErr
		}
	}
}

// probeMySQLCredential 用面板持有的口令探一次，并把失败归类。
func (m *Manager) probeMySQLCredential(ctx context.Context, admin mysqlAdmin, password string) (mysqlCredState, error) {
	err := admin.Ping(ctx, password)
	switch {
	case err == nil:
		return mysqlCredOK, nil
	case errors.Is(err, mysql.ErrAuth):
		return mysqlCredAuthFailed, err
	default:
		// "连不上/没找到客户端"都归到这里：对用户来说下一步动作是一样的
		// （去服务管理看日志、确认服务在跑）。
		return mysqlCredUnreachable, err
	}
}

// setAndRecordMySQLPassword 限时问一次口令，然后把数据库与面板配置一起对齐。
func (m *Manager) setAndRecordMySQLPassword(ctx context.Context, result *InstallResult, admin mysqlAdmin, cred MySQLCredential) error {
	name := dbEngineFormulaDisplay(cred.Formula)
	if name == "" {
		name = "数据库"
	}
	timeout := m.opt.MySQLInputTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	raw, provided := m.askMySQLRootPassword(ctx, timeout, name)
	// 不 Trim 用户输入的值本身：口令里的空格是合法的，悄悄改掉它会让用户
	// "照着自己输入的连不上"。只有"全是空白"才当成留空（＝自动生成）。
	password := raw
	byUser := provided && strings.TrimSpace(raw) != ""

	var source string
	switch {
	case byUser:
		source = "使用你输入的口令"
	case provided:
		source = "你选择留空，已自动生成强随机口令"
	default:
		source = "等待输入超时，已自动生成强随机口令"
	}
	if !byUser {
		gen, err := randomMySQLPassword()
		if err != nil {
			return fmt.Errorf("生成随机口令失败: %w", err)
		}
		password = gen
	}

	// 这一步的文案里绝不带口令本身：任务步骤会进 InstallResult.Steps，
	// 而 Steps 会被写进审计（summarizeResult）并被永久保存。
	result.step(ctx, "正在为 "+name+" root 设置口令（"+source+"；口令不会出现在日志或审计里）")
	if err := admin.SetRootPassword(ctx, password); err != nil {
		return fmt.Errorf("设置 %s root 口令失败: %w", name, err)
	}
	// 自检：用**新口令**真连一次。ALTER USER 之后不再 FLUSH（见 mysql/admin.go），
	// 所以这里就是权威判定 —— 旧的"改完再 FLUSH"正是把面板锁在门外的原因。
	if err := admin.Ping(ctx, password); err != nil {
		msg := "已在 " + name + " 上设置 root 口令，但用新口令连接自检失败：" + err.Error() +
			"。" + mysql.RecoveryGuide(cred.User)
		result.Warning = appendLNMPWarning(result.Warning, msg)
		return errors.New(msg)
	}
	// 一次性凭据区块：沿用面板"安装结果里给凭据"的既有做法。
	// **在写配置之前就挂上去** —— 万一下面写盘失败，这里是口令唯一的记录。
	result.Credentials = append(result.Credentials, Credential{
		Key: "mysql_root_password", Value: password,
		Label: name + " root 口令（" + source + "）",
	})
	if err := m.opt.SetMySQLRootPassword(password); err != nil {
		msg := name + " root 口令已设置成功，但写入面板配置失败：" + err.Error() +
			"；面板重启后会连不上数据库。请立刻复制本次任务结果里的一次性凭据，并在「数据库 → 连接设置」手工填入"
		result.Warning = appendLNMPWarning(result.Warning, msg)
		return errors.New(msg)
	}
	result.step(ctx, name+" root 凭据已闭环：口令已写入面板配置（0600），不写日志/审计；"+
		"之后面板的所有库操作、以及「数据库」页面都用它。要换成别的口令，到「账号与权限」给 root 改密码即可")
	return nil
}

// askMySQLRootPassword 限时询问 root 口令。
//
// 返回值 (口令, 是否由用户提供)。**没有输入通道时直接返回 false（＝自动生成）**：
// 从命令行/脚本触发的无人值守安装不该在这里白等 60 秒。
func (m *Manager) askMySQLRootPassword(ctx context.Context, timeout time.Duration, name string) (string, bool) {
	provider := inputFrom(ctx)
	if provider == nil {
		emit(ctx, tasks.LevelStep, "当前没有可用的输入通道，直接自动生成强随机口令")
		return "", false
	}
	// 这条 Level=Input 的日志是给用户看的提示；结构化的 input_required
	// （key/倒计时/截止时间）由任务中心单独下发给前端，见 tasks.InputRequest。
	emit(ctx, tasks.LevelInput, fmt.Sprintf(
		"请在 %d 秒内输入 %s root 口令（留空或超时＝自动生成强随机口令后继续）",
		int(timeout/time.Second), name))
	return provider.WaitInput(ctx, tasks.InputRequest{
		Key:    "mysql_root_password",
		Label:  name + " root 口令",
		Hint:   "留空或超时＝自动生成强随机口令。口令只写进面板配置（0600），不进日志、不进审计。",
		Secret: true,
	}, timeout)
}

// randomMySQLPassword 生成强随机口令。
//
// 用 crypto/rand 的 Text()：26 个字符的 base32（A-Z2-7），约 130 bit 熵。
// 选它还有两个工程上的理由：
//   - 字符集里没有任何引号/反斜杠/空格，所以经 SQL 字符串、MYSQL_PWD、
//     config.json 三条路都不会遇到转义问题；
//   - 去掉小写与易混字符（0/O/1/l/I），用户抄写时不会"看着一样其实不对"
//     （这个口令是要给用户看一眼、必要时手工填的）。
func randomMySQLPassword() (string, error) {
	// rand.Text() 不会失败（它内部 panic 的唯一条件是系统熵源彻底坏掉，
	// 那种情况下面板也没有继续安装的意义）；保留 error 返回值是为了让
	// 调用方的"生成失败"分支将来换成别的实现时不用改。
	return rand.Text(), nil
}
