package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// mysqlClient 构造 MySQL 客户端。
func (s *Server) mysqlClient() (*mysql.Client, error) {
	binDir := filepath.Join(s.Cfg.BrewPrefix, "opt", "mysql@8.4", "bin")
	if _, err := os.Stat(filepath.Join(binDir, "mysql")); err != nil {
		// 回退到 PATH 常见位置
		binDir = filepath.Join(s.Cfg.BrewPrefix, "bin")
	}
	host, port, socket, user := s.mysqlConn()
	return mysql.NewClient(mysql.Options{
		BinDir:   binDir,
		Host:     host,
		Port:     port,
		Socket:   socket,
		User:     user,
		Password: s.mysqlPassword(),
		Timeout:  30 * time.Second,
		UserName: s.Cfg.User,
		UserHome: s.Cfg.UserHome,
	}), nil
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
func (s *Server) mysqlPassword() string {
	// 1. 面板配置优先
	if v := strings.TrimSpace(s.Cfg.MySQLPassword); v != "" {
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
	if verr != nil {
		// 连不上是常见情况（服务没启动），返回 200 + 明确说明，
		// 让前端能展示"如何修复"而不是一个空白错误页
		ok(w, map[string]any{
			"connected": false,
			"error":     verr.Error(),
			"databases": []any{},
			"users":     []any{},
			"hint":      "请确认 MySQL 服务正在运行（可在「服务管理」里启动 mysql@8.4），并检查 ~/www/.env.local 中的 root 密码是否正确",
		})
		return
	}

	dbs, dbErr := c.ListDatabases(ctx)
	users, userErr := c.ListUsers(ctx)

	res := map[string]any{
		"connected": true,
		"version":   version,
		"databases": dbs,
		"users":     users,
	}
	if dbErr != nil {
		res["databases_error"] = dbErr.Error()
	}
	if userErr != nil {
		res["users_error"] = userErr.Error()
	}
	// 备份目录（导出用）
	res["backup_dir"] = filepath.Join(s.Cfg.WorkDir, "db-backup")
	res["has_password"] = s.mysqlPassword() != ""
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
	s.audit(r, "db_user_create", req.User+"@"+req.Host,
		fmt.Sprintf("创建账号并授权 %v 到 %v", req.Privileges, req.Databases), true, "")
	ok(w, map[string]any{"msg": fmt.Sprintf("账号 %s@%s 已创建", req.User, req.Host)})
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
	s.audit(r, "db_user_drop", user+"@"+host, "删除数据库账号", true, "")
	ok(w, map[string]any{"msg": "账号已删除"})
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
	// 审计里不记录密码本身
	s.audit(r, "db_user_password", req.User+"@"+req.Host, "重置账号密码", true, "")
	ok(w, map[string]any{"msg": "密码已重置"})
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
	msg, err := c.ImportDatabase(r.Context(), req.Name, req.File)
	if err != nil {
		s.audit(r, "db_import", req.Name, "导入失败: "+err.Error(), false, "")
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "db_import", req.Name, msg, true, "")
	ok(w, map[string]any{"msg": msg})
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
