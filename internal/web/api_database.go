package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/zizdog/zizpanel/internal/tasks"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/mysql"
)

// ============================================================================
//  数据库管理
//
//  安全要点：
//    - 连接凭据从 ~/www/.env.local 读取（与 LNMP 安装脚本保持一致），
//      不落数据库、不下发给前端
//    - 所有标识符（库名/用户名/主机名/权限）在 mysql 包内做白名单校验
//    - 破坏性操作（删库/删账号/改密码）全部写审计
//    - 面板不允许删除系统库与 root 账号
// ============================================================================

// mysqlClient 构造 MySQL 客户端（用面板当前持有的口令）。
func (s *Server) mysqlClient() (*mysql.Client, error) {
	return s.mysqlClientWithPassword(s.mysqlPassword())
}

// mysqlClientWithPassword 用**指定**口令构造客户端。
//
// 用途是"改口令之后立刻拿新口令自检"：那一步必须绕开配置里还存着的旧口令，
// 否则永远自检不通过（真机事故就是这么发生的，见 mysql/admin.go）。
//
// 客户端目录**按当前生效的引擎解析**（MySQL 8.4 / MariaDB）：过去写死
// opt/mysql@8.4/bin，装了 MariaDB 的机器上那条路径不存在，页面会报
// "未找到 mysql 客户端"，看起来像面板不支持。解析读不到时沿用历史回退路径
// （opt/mysql@8.4 → <brew>/bin），让页面如实报"找不到客户端"。
func (s *Server) mysqlClientWithPassword(password string) (*mysql.Client, error) {
	eng, err := s.databaseEngine()
	if err != nil {
		// 两个引擎都装着（无法判断该连哪个）：如实拒绝，不挑一个默认值。
		return nil, err
	}
	binDir := eng.BinDir
	if binDir == "" {
		binDir = fallbackMySQLBinDir(s.Cfg.BrewPrefix)
	}
	host, port, socket, user := s.mysqlConn()
	if strings.TrimSpace(eng.Socket) != "" {
		socket = eng.Socket
	}
	return mysql.NewClient(mysql.Options{
		BinDir:   binDir,
		Host:     host,
		Port:     port,
		Socket:   socket,
		User:     user,
		Password: password,
		Timeout:  30 * time.Second,
		UserName: s.Cfg.User,
		UserHome: s.Cfg.UserHome,
	}), nil
}

// fallbackMySQLBinDir 是"一个引擎的 keg 都没读到"时的历史回退路径。
// 它不是"猜引擎"，只是让页面能继续找一个手工安装的客户端，并在真的没有时报错。
func fallbackMySQLBinDir(brewPrefix string) string {
	dir := filepath.Join(brewPrefix, "opt", "mysql@8.4", "bin")
	if _, err := os.Stat(filepath.Join(dir, "mysql")); err != nil {
		dir = filepath.Join(brewPrefix, "bin")
	}
	return dir
}

// databaseEngine 解析当前生效的数据库引擎（只读磁盘；两个都装着时按 3306 判）。
func (s *Server) databaseEngine() (mysql.EnginePaths, error) {
	return s.svcManager().ResolveDBEngine(s.Cfg.BrewPrefix, s.Cfg.MySQLSocket)
}

// databaseEngineInfo 是给页面看的引擎块：读不到也如实回（verified=false + note）。
func (s *Server) databaseEngineInfo() any {
	eng, err := s.databaseEngine()
	if err != nil {
		return map[string]any{"verified": false, "note": err.Error()}
	}
	return eng
}

// mysqlPassword 从 .env.local 读取 root 密码。
//
// 为什么要读这个文件：LNMP 安装脚本把凭据写在那里，
// 面板沿用同一份来源，避免"面板里再存一份密码"带来的同步问题。
// 该文件权限是 600，只有真实用户与 root 可读。
// mysqlConn 返回连接参数，带回默认值。
func (s *Server) mysqlConn() (host string, port int, socket, user string) {
	host, port, socket, user = s.Cfg.MySQLHost, s.Cfg.MySQLPort, s.Cfg.MySQLSocket, s.Cfg.MySQLUser
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 3306
	}
	if socket == "" {
		socket = "/tmp/mysql.sock"
	}
	if user == "" {
		user = "root"
	}
	return
}

// mysqlPassword 解析 root 密码。顺序即优先级：
//
//  1. 面板配置里填的（用户在「数据库」页填过就以此为准）
//  2. ZIZPANEL_MYSQL_PASSWORD 环境变量（部署脚本可注入）
//  3. ~/www/.env.local 里的 MYSQL_ROOT_PASSWORD（某个项目的既有约定）
//  4. 都没有 → 空密码（brew 全新初始化的 MySQL 就是无密码）
//
// 早先只有第 3、4 步，于是一台没有那个文件的机器永远连不上。
//
// 注意：这里返回空串**只表示"面板手里没有口令"**，绝不等于"MySQL 没有口令"。
// 把两者混为一谈正是 2026-09-16 那次误导（页面显示 root 无密码，
// 实际 MySQL 要口令）。是否真的无口令必须靠一次真连接来判定，
// 见 mysqlCredentialState。
func (s *Server) mysqlPassword() string {
	// 1. 面板配置优先
	if v := strings.TrimSpace(s.Cfg.MySQLPasswordValue()); v != "" {
		return v
	}
	// 2. 环境变量
	if v := os.Getenv("ZIZPANEL_MYSQL_PASSWORD"); v != "" {
		return v
	}
	// 3. 项目约定文件
	envFile := filepath.Join(s.Cfg.WWWRoot, ".env.local")
	b, err := os.ReadFile(envFile)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(ln, "MYSQL_ROOT_PASSWORD="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// mysqlCredentialView 描述"面板持有的 MySQL 凭据"这一件事的**验证过的**状态。
//
// 为什么要三态而不是一个布尔：布尔只能表达"配置里有没有写口令"，
// 而用户真正需要知道的是"这个口令现在还能不能用"。
// 真机事故里页面就是拿"配置为空"当成了"root 无密码"，把用户引向了错误的结论：
//   - ok             面板持有的口令（非空）真连成功 —— 已验证可用
//   - ok_no_password  面板持有的口令为空且真连成功 —— **已验证**服务器确实无口令
//   - unconfigured    面板没有口令、服务器却要口令（1045 using password: NO）
//   - auth_failed     面板有口令，但服务器拒绝（口令不对）
//   - unreachable     MySQL 没起来/连不上（与口令无关）
type mysqlCredentialView struct {
	State      string `json:"state"`
	Configured bool   `json:"configured"`
	Verified   bool   `json:"verified"`
	User       string `json:"user"`
	Error      string `json:"error,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// mysqlCredentialState 依据"刚才那次真连接的结果"判定凭据状态。
//
// verr 来自客户端的 Version/Ping —— 也就是说：这个状态是**验证出来的**，
// 不是猜的。这正是本函数存在的理由。
func (s *Server) mysqlCredentialState(verr error) mysqlCredentialView {
	user := strings.TrimSpace(s.Cfg.MySQLUser)
	if user == "" {
		user = "root"
	}
	configured := strings.TrimSpace(s.mysqlPassword()) != ""
	v := mysqlCredentialView{Configured: configured, User: user}
	switch {
	case verr == nil:
		v.Verified = true
		if configured {
			v.State = "ok"
			v.Hint = "面板持有的口令可以正常连接"
		} else {
			// 空口令真的连上了 —— 这才是"确实无口令"，是验证过的结论。
			v.State = "ok_no_password"
			v.Hint = "MySQL 当前**确实**没有设置口令（面板用空口令连上了）。" +
				"任何能连到 3306 的程序都能读写全部数据库；" +
				"建议到「账号与权限」里给 " + user + " 点「改密码」设一个（面板会把它写进自己的配置）"
		}
	case errors.Is(verr, mysql.ErrAuth) && !configured:
		v.State = "unconfigured"
		v.Error = verr.Error()
		v.Hint = "面板手里没有 " + user + " 的口令，而 MySQL 要求口令 —— 两者不一致，" +
			"所以**不能**认为 " + user + " 没有密码。请在「数据库 → 连接设置」填入正确口令并保存" +
			"（这台机器如果是面板装的 MySQL，口令应当在安装时就已经写入面板配置）。" +
			mysql.RecoveryGuide(user)
	case errors.Is(verr, mysql.ErrAuth):
		v.State = "auth_failed"
		v.Error = verr.Error()
		v.Hint = "面板持有的口令被 MySQL 拒绝（口令不对，或与这个账号不匹配）。" +
			"请在「数据库 → 连接设置」更新口令并保存；" + mysql.RecoveryGuide(user)
	default:
		v.State = "unreachable"
		v.Error = verr.Error()
		name, service := "数据库", "数据库服务"
		if eng, err := s.databaseEngine(); err == nil {
			if strings.TrimSpace(eng.Name) != "" {
				name = eng.Name
			}
			if strings.TrimSpace(eng.Formula) != "" {
				service = eng.Formula
			}
		}
		v.Hint = name + " 服务没有响应（与口令无关）：请确认服务正在运行" +
			"（可在「服务管理」里启动 " + service + " 并查看日志）"
	}
	return v
}

// handleDatabaseOverview 返回数据库概览：库列表、账号列表、服务状态。
func (s *Server) handleDatabaseOverview(w http.ResponseWriter, r *http.Request) {
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	version, verr := c.Version(ctx)
	// 凭据状态由这次真连接得出：连不上时它是"未配置/认证失败/连不上"，
	// 而不是一句会让用户误判的"root 无密码"。
	cred := s.mysqlCredentialState(verr)
	if verr != nil {
		// 连不上是常见情况（服务没启动、口令不一致），要给出**能照着做**的东西，
		// 而不是一个空白错误页。
		body := map[string]any{
			"connected":  false,
			"error":      verr.Error(),
			"databases":  []any{},
			"users":      []any{},
			"credential": cred,
			// engine 是"当前生效的引擎 + 它的 bin/datadir/socket/label"（真实磁盘解析）。
			// verified=false 时页面必须显示"未复核"，不许假装知道。
			"engine": s.databaseEngineInfo(),
			// has_password 的语义**只是**"面板配置里有没有口令"。
			// 页面不要再把它当成"MySQL 有没有口令"来显示 —— 那是两件事。
			"has_password": cred.Configured,
			"hint":         cred.Hint,
		}
		// 认证类失败（面板口令与服务器不一致）用 **400**，不是 200。
		//
		// 为什么：数据库页只在 `api.database()` **抛错**时才渲染"连接设置"表单
		// （database.js 的 catch 分支）。返回 200 的话，页面只会显示一段说明，
		// 用户拿着"口令不一致"的结论却**没有任何输入框**可以救命 ——
		// 而这正是 2026-09-16 mini 上用户走不出来的原因。
		// 本轮前端冻结，所以让"该去改凭据"这件事走它本来就走得通的那条路。
		if errors.Is(verr, mysql.ErrAuth) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "msg": verr.Error(), "data": body,
			})
			return
		}
		// 服务没起来（与口令无关）：保持 200，页面给出"去服务管理启动 MySQL"的路。
		ok(w, body)
		return
	}

	dbs, dbErr := c.ListDatabases(ctx)
	users, userErr := c.ListUsers(ctx)

	res := map[string]any{
		"connected":  true,
		"version":    version,
		"databases":  dbs,
		"users":      []any{},
		"credential": cred,
		"engine":     s.databaseEngineInfo(),
	}
	if dbErr != nil {
		res["databases_error"] = dbErr.Error()
	}
	if userErr != nil {
		res["users_error"] = userErr.Error()
	} else {
		// 把 MySQL 里的真实账号与面板保管的口令合并成页面视图。
		// 合并失败（读 SQLite 出错）时仍然返回账号列表（password_known 一律 false，
		// 页面会显示"不可回显"），并把原因如实带给页面 ——
		// 宁可少显示一列信息，也不能让整个"账号与权限"页打不开。
		views, credErr := s.databaseUsersView(ctx, users, cred)
		res["users"] = views
		if credErr != nil {
			res["credentials_error"] = "读取面板保存的数据库口令失败，密码列已降级为" +
				"「不可回显」：" + credErr.Error()
		}
	}
	// 备份目录（导出用）
	res["backup_dir"] = filepath.Join(s.Cfg.WorkDir, "db-backup")
	res["has_password"] = cred.Configured
	ok(w, res)
}

// handleDatabaseTables 列出某库的表。
func (s *Server) handleDatabaseTables(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	tables, err := c.ListTables(r.Context(), name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"database": name, "tables": tables})
}

type dbCreateReq struct {
	Name      string `json:"name"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
}

func (s *Server) handleDatabaseCreate(w http.ResponseWriter, r *http.Request) {
	var req dbCreateReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := c.CreateDatabase(r.Context(), req.Name, req.Charset, req.Collation); err != nil {
		s.audit(r, "db_create", req.Name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "db_create", req.Name, "创建数据库（"+orDefault(req.Charset, "utf8mb4")+"）", true, "")
	ok(w, map[string]any{"msg": "数据库 " + req.Name + " 已创建"})
}

func (s *Server) handleDatabaseDrop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := c.DropDatabase(r.Context(), name); err != nil {
		s.audit(r, "db_drop", name, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "db_drop", name, "删除数据库（含全部数据）", true, "")
	ok(w, map[string]any{"msg": "数据库 " + name + " 已删除"})
}

type dbUserReq struct {
	User       string   `json:"user"`
	Host       string   `json:"host"`
	Password   string   `json:"password"`
	Privileges []string `json:"privileges"`
	Databases  []string `json:"databases"`
}

func (s *Server) handleDatabaseUserCreate(w http.ResponseWriter, r *http.Request) {
	var req dbUserReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Host == "" {
		req.Host = "localhost"
	}
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := c.CreateUser(r.Context(), req.User, req.Host, req.Password, req.Privileges, req.Databases); err != nil {
		s.audit(r, "db_user_create", req.User+"@"+req.Host, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// **顺序是关键**：MySQL 侧确实创建成功之后才记口令。
	// 反过来（先记后做）一旦 MySQL 失败，页面就会显示一个其实没生效的口令 ——
	// 那正是这个功能最不能犯的错。
	rememberErr := s.RememberDBPassword(r.Context(), req.User, req.Host, req.Password, DBCredSourcePanel)
	detail := fmt.Sprintf("创建账号并授权 %v 到 %v", req.Privileges, req.Databases)
	if rememberErr != nil {
		// 账号是真实存在的：不能因为"回显用的口令没存上"就报成创建失败。
		// 但必须**大声**说出来（审计 + 响应里的 warning），并让页面把口令亮给用户。
		detail += "（口令未能保存以便回显）"
	} else {
		detail += fmt.Sprintf("（口令已保存以便回显，source=%s）", DBCredSourcePanel)
	}
	s.audit(r, "db_user_create", req.User+"@"+req.Host, detail, true, "")
	resp := map[string]any{"msg": fmt.Sprintf("账号 %s@%s 已创建", req.User, req.Host)}
	if rememberErr != nil {
		resp["warning"] = "账号已在 MySQL 中创建成功，但面板无法保存该口令供日后回显：" +
			rememberErr.Error() + "。请立即复制这个口令，关闭窗口后无法再显示。"
	}
	ok(w, resp)
}

func (s *Server) handleDatabaseUserDrop(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	host := r.URL.Query().Get("host")
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := c.DropUser(r.Context(), user, host); err != nil {
		s.audit(r, "db_user_drop", user+"@"+host, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 账号在 MySQL 里已经不存在了，面板保存的明文口令必须同步清掉，
	// 不留一份永远不会再用到的秘密。
	forgetErr := s.ForgetDBPassword(r.Context(), user, host)
	detail := "删除数据库账号"
	if forgetErr != nil {
		detail += "（清理保存的口令失败）"
	}
	s.audit(r, "db_user_drop", user+"@"+host, detail, true, "")
	resp := map[string]any{"msg": "账号已删除"}
	if forgetErr != nil {
		// 账号确实已删除（主操作成功），只是清理失败；不能报成"删除失败"，
		// 但也不能不说 —— 否则面板里会留着一份看不见的明文口令。
		resp["warning"] = "账号已在 MySQL 中删除，但面板清理它保存的口令失败：" +
			forgetErr.Error() + "。这不影响账号已删除的事实，可稍后重试清理。"
	}
	ok(w, resp)
}

type dbPasswordReq struct {
	User     string `json:"user"`
	Host     string `json:"host"`
	Password string `json:"password"`
}

func (s *Server) handleDatabaseUserPassword(w http.ResponseWriter, r *http.Request) {
	var req dbPasswordReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := c.SetUserPassword(r.Context(), req.User, req.Host, req.Password); err != nil {
		s.audit(r, "db_user_password", req.User+"@"+req.Host, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 口令已经在 MySQL 里真的生效了，**这时**才允许记下来供日后回显。
	// 即使下面"写回面板配置"失败，这个口令也确实生效，记下来不撒谎。
	rememberErr := s.RememberDBPassword(r.Context(), req.User, req.Host, req.Password, DBCredSourcePanel)
	// 改的是**面板自己正在用的那个账号**时，必须把新口令写回面板配置。
	//
	// 不写回的后果就是 2026-09-16 mini 的事故：MySQL 的口令变了、面板手里还是旧的，
	// 从下一次操作开始所有库操作都是 1045，而且用户再点一次"改密码"也改不回来
	// （那需要先连上去）。这里把它彻底堵死。
	note := ""
	if s.isPanelMySQLAccount(req.User, req.Host) {
		n, err := s.persistPanelMySQLPassword(r.Context(), req.Password)
		if err != nil {
			s.audit(r, "db_user_password", req.User+"@"+req.Host,
				"MySQL 口令已改，但面板配置未同步: "+err.Error(), false, "")
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		note = n
	} else if strings.EqualFold(strings.TrimSpace(req.User), s.panelMySQLUser()) {
		// 改的是同名的另一个身份（如 root@%）：面板连的是 localhost，配置不该动。
		// 必须说清楚，否则用户会以为"面板的口令也跟着换了"。
		note = "（改的是 " + req.User + "@" + req.Host + "，不是面板当前使用的登录身份；面板配置未改动）"
	}
	// 审计里不记录密码本身；source 这类元信息可以写。
	detail := "重置账号密码"
	if rememberErr != nil {
		detail += "（口令未能保存以便回显）"
	} else {
		detail += fmt.Sprintf("（口令已保存以便回显，source=%s）", DBCredSourcePanel)
	}
	s.audit(r, "db_user_password", req.User+"@"+req.Host, detail, true, "")
	resp := map[string]any{"msg": "密码已重置" + note}
	if rememberErr != nil {
		// MySQL（以及面板账号时的配置）都已同步，只有"日后回显"这一步失败。
		// 报成"改密码失败"会误导用户以为没生效，所以按成功返回 + warning。
		resp["warning"] = "口令已在 MySQL 中生效" + note + "，但面板无法保存它供日后回显：" +
			rememberErr.Error() + "。请立即记录这个口令。"
	}
	ok(w, resp)
}

// panelMySQLUser 返回面板连接 MySQL 用的账号名（缺省 root）。
func (s *Server) panelMySQLUser() string {
	if u := strings.TrimSpace(s.Cfg.MySQLUser); u != "" {
		return u
	}
	return "root"
}

// isPanelMySQLAccount 判断被改的账号是不是"面板自己连接用的身份"。
//
// 判定刻意保守，只认本机身份（localhost / 127.0.0.1 / ::1）：
// 改 `root@%` 不影响面板的 `root@localhost` 连接，那种情况下写回配置
// 反而会把面板的口令改成错的（等于自己制造事故）。
func (s *Server) isPanelMySQLAccount(user, host string) bool {
	if !strings.EqualFold(strings.TrimSpace(user), s.panelMySQLUser()) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1":
	default:
		return false
	}
	// 面板自己也得是真的连本机：用 socket，或连的地址就是本机。
	if strings.TrimSpace(s.Cfg.MySQLSocket) != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(s.Cfg.MySQLHost)) {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// persistPanelMySQLPassword 把新口令写回面板配置，并做一次真连接自检。
//
// 顺序很关键：**先用新口令连一次，再写配置**。
// 反过来的话，一旦上面那个"这是不是面板自己的账号"判断有偏差
// （例如面板其实连的是 root@127.0.0.1 而不是 root@localhost），
// 就会把面板的口令改成错的，从"能用"变成"不能用"。
// 用旧口令还能连上 = 改的不是面板这个身份 → 配置保持不变。
func (s *Server) persistPanelMySQLPassword(ctx context.Context, newPassword string) (string, error) {
	probe := func(pw string) error {
		c, err := s.mysqlClientWithPassword(pw)
		if err != nil {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return c.Ping(pctx)
	}
	user := strings.TrimSpace(s.Cfg.MySQLUser)
	if user == "" {
		user = "root"
	}
	oldPassword := s.mysqlPassword()

	// 用新口令重试几次再下结论：ALTER 之后 MySQL 偶尔会有极短的一瞬
	// （连接池/权限表刷新）连不上，一次失败就判"这个身份不是面板的"
	// 会把配置留在旧口令上，重启后照样锁死。
	var newErr error
	for attempt := 0; attempt < 3; attempt++ {
		if newErr = probe(newPassword); newErr == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if newErr != nil {
		if probe(oldPassword) == nil {
			// 改的是另一个身份（例如 root@127.0.0.1），面板连接不受影响。
			return "（该账号不是面板当前使用的登录身份，面板配置未改动）", nil
		}
		// 新旧口令都连不上：说明这个身份确实被改动了，但新口令也不通。
		return "", fmt.Errorf("MySQL 口令已下发，但用新口令连接自检失败：%v。"+
			"面板配置**没有**改动（旧口令也已失效）。%s", newErr, mysql.RecoveryGuide(user))
	}

	s.Cfg.SetMySQLPassword(newPassword)
	if err := s.Cfg.Save(); err != nil {
		// 不回滚内存值：回滚会让面板立刻连不上，而现在至少还能用。
		// 但必须把话说清楚 —— 重启面板后配置里还是旧口令。
		return "", fmt.Errorf("MySQL 口令已改成功，但写入面板配置失败：%v。"+
			"当前进程仍按新口令工作，**重启面板后会再次认证失败**；"+
			"请立刻到「数据库 → 连接设置」手工填入刚才输入的新口令", err)
	}
	return "（这是面板使用的登录账号，新口令已写入面板配置）", nil
}

type dbGrantReq struct {
	User       string   `json:"user"`
	Host       string   `json:"host"`
	Database   string   `json:"database"`
	Privileges []string `json:"privileges"`
}

func (s *Server) handleDatabaseGrant(w http.ResponseWriter, r *http.Request) {
	var req dbGrantReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := c.GrantPrivileges(r.Context(), req.User, req.Host, req.Database, req.Privileges); err != nil {
		s.audit(r, "db_grant", req.User+"@"+req.Host, "失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "db_grant", req.User+"@"+req.Host,
		fmt.Sprintf("授权 %v 到 %s", req.Privileges, req.Database), true, "")
	ok(w, map[string]any{"msg": "授权已更新"})
}

// handleDatabaseGrants 查看账号的授权语句。
func (s *Server) handleDatabaseGrants(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	host := r.URL.Query().Get("host")
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	grants, err := c.ShowGrants(r.Context(), user, host)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"grants": grants})
}

type dbQueryReq struct {
	Database string `json:"database"`
	SQL      string `json:"sql"`
}

// handleDatabaseQuery 执行 SQL（高级功能）。
func (s *Server) handleDatabaseQuery(w http.ResponseWriter, r *http.Request) {
	var req dbQueryReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := c.Execute(ctx, req.Database, req.SQL)
	if err != nil {
		s.audit(r, "db_query", req.Database, "SQL 执行失败: "+err.Error(), false, "")
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 审计里只记录语句开头，避免把敏感数据写进日志
	s.audit(r, "db_query", req.Database, "执行 SQL: "+truncateSQL(req.SQL), true, "")
	ok(w, res)
}

func truncateSQL(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// handleDatabaseDump 导出数据库。
func (s *Server) handleDatabaseDump(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	dir := filepath.Join(s.Cfg.WorkDir, "db-backup")
	res, err := c.DumpDatabase(r.Context(), req.Name, dir)
	if err != nil {
		s.audit(r, "db_dump", req.Name, "导出失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "db_dump", req.Name,
		fmt.Sprintf("导出 %d 张表，%d 字节，耗时 %dms", res.Tables, res.Size, res.Elapsed), true, "")
	ok(w, res)
}

// handleDatabaseImport 导入 SQL 文件到指定库。
func (s *Server) handleDatabaseImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 导入文件必须落在允许的目录内（复用文件管理器的白名单思路）
	fm := s.fileManager()
	if _, err := fm.Resolve(req.File, false); err != nil {
		fail(w, http.StatusBadRequest, "导入文件位置不被允许："+err.Error())
		return
	}
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 只读预检：文件在不在、是不是目录 —— 参数问题当场 400，不建任务。
	st, serr := os.Stat(req.File)
	if serr != nil {
		fail(w, http.StatusBadRequest, "找不到 SQL 文件："+serr.Error())
		return
	}
	if st.IsDir() {
		fail(w, http.StatusBadRequest, "这是一个目录，请选择 .sql 文件")
		return
	}
	// 走**任务中心**（2026-09-18 用户报障：4MB 的 SQL 导入时页面完全没输出，
	// 只能看着像"卡死"）。现在：立刻返回 task_id，任务里按**真实字节数**报进度
	// （已导入 1.2 MB / 4.0 MB），随时可以在任务中心里中断。
	s.launchTask(w, r, "db_import", req.Name,
		"导入 "+filepath.Base(req.File)+" → "+req.Name, "db_import",
		func(ctx context.Context, log tasks.LogFunc) (any, error) {
			log(tasks.LevelStep, fmt.Sprintf("开始导入 %s（%.2f MB）到 %s",
				filepath.Base(req.File), float64(st.Size())/1024/1024, req.Name))
			lastPct := -1
			msg, ierr := c.ImportDatabaseProgress(ctx, req.Name, req.File,
				func(done, total int64) {
					if total <= 0 {
						return
					}
					pct := int(done * 100 / total)
					// 每 5% 报一行：既能看出在动，也不会把日志刷爆。
					if pct == lastPct || pct%5 != 0 {
						return
					}
					lastPct = pct
					log(tasks.LevelOut, fmt.Sprintf("已送入 %.2f MB / %.2f MB（%d%%）",
						float64(done)/1024/1024, float64(total)/1024/1024, pct))
				})
			if ierr != nil {
				return nil, ierr
			}
			log(tasks.LevelOK, msg)
			return map[string]any{"msg": msg}, nil
		})
}

// handleDatabaseBackups 列出已导出的备份文件。
func (s *Server) handleDatabaseBackups(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.Cfg.WorkDir, "db-backup")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			ok(w, map[string]any{"list": []any{}, "dir": dir})
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		ModTime string `json:"mod_time"`
	}
	var list []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, item{
			Name: e.Name(), Path: filepath.Join(dir, e.Name()),
			Size: info.Size(), ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	if list == nil {
		list = []item{}
	}
	ok(w, map[string]any{"list": list, "dir": dir})
}

// handleDatabaseSlowQueries 返回慢查询（如果开启了慢日志）。
func (s *Server) handleDatabaseSlowQueries(w http.ResponseWriter, r *http.Request) {
	c, err := s.mysqlClient()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// 读慢日志配置
	lines, err := c.Execute(ctx, "", "SHOW VARIABLES LIKE 'slow_query_log_file'")
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var logFile string
	if len(lines.Rows) > 0 && len(lines.Rows[0]) > 1 {
		logFile = lines.Rows[0][1]
	}
	res := map[string]any{"log_file": logFile}
	if logFile != "" {
		if content, total, err := tailFile(logFile, 200); err == nil {
			res["content"] = content
			res["lines"] = total
		} else if os.IsNotExist(err) {
			res["content"] = ""
			res["msg"] = "慢查询日志尚未生成（可能未开启慢查询）"
		} else {
			res["msg"] = "无法读取慢查询日志：" + err.Error()
		}
	}
	ok(w, res)
}

// 保留：便于将来增加"连接数 / 进程列表"面板
var _ = sql.ErrNoRows
var _ = errors.Is
