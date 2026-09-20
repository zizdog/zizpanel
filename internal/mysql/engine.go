package mysql

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ============================================================================
//  数据库引擎解析（MySQL 8.4 / MariaDB）
//
//  面板的「数据库」页原来写死 /opt/homebrew/opt/mysql@8.4/bin。装的是 MariaDB 时
//  那条路径根本不存在，页面会报"未找到 mysql 客户端"——看起来像面板不支持。
//  这里把"当前生效的引擎"解析成**磁盘上真实存在**的路径，读到什么说什么：
//  两个引擎都装着（谁也没法确定该连哪个）时如实返回错误，绝不挑一个默认值。
//
//  两个引擎**默认共用数据目录 <brew>/var/mysql 与 3306**（见 homebrew formula：
//  mariadb 的 cmake 参数 -DMYSQL_DATADIR=#{var}/mysql），所以同一时刻只能跑一个。
// ============================================================================

// DBEngine 是面板能接的两种 MySQL 协议引擎。
type DBEngine string

const (
	EngineMySQL   DBEngine = "mysql"
	EngineMariaDB DBEngine = "mariadb"
)

// engineKeg 是一个候选引擎的静态信息。
type engineKeg struct {
	Engine DBEngine
	Name   string
	// Formula 是 keg 目录名。
	Formula string
	// Label 是 Homebrew 7 的 canonical launchd 名（service.rb 的
	// canonical_plist_name = sh.brew.<formula>；本机 mysql@8.4 的真实 plist
	// 就是 sh.brew.mysql@8.4，实测）。
	Label string
	// Proc 是它在 lsof 里的进程名（mariadbd 与 mysqld 互不包含）。
	Proc string
}

var engineKegs = []engineKeg{
	{EngineMySQL, "MySQL", "mysql@8.4", "sh.brew.mysql@8.4", "mysqld"},
	{EngineMariaDB, "MariaDB", "mariadb", "sh.brew.mariadb", "mariadbd"},
}

// EnginePaths 是"当前生效的数据库引擎"在磁盘上的真实位置。
//
// Verified=false 表示**没读到**（不是猜的默认值）：调用方必须如实说"未复核"，
// 不许拿一个默认引擎顶上。
type EnginePaths struct {
	Engine  DBEngine `json:"engine"`
	Name    string   `json:"name"`
	Formula string   `json:"formula"`
	// BinDir 是客户端目录（mysql / mysqldump / mariadb / mariadb-dump 都在这里）。
	BinDir string `json:"bin_dir"`
	// DataDir 是两个引擎共用的默认数据目录（<brew>/var/mysql）。
	DataDir string `json:"data_dir"`
	// Socket 是**真实存在**的 socket 文件；空 = 没读到（客户端会改用 TCP）。
	Socket string `json:"socket"`
	// ServiceLabel 是 Homebrew 的 canonical launchd 名（运行期仍以磁盘 plist 为准）。
	ServiceLabel string `json:"service_label"`
	// ClientFound / DataDirFound / SocketFound 是三条独立证据。
	ClientFound  bool `json:"client_found"`
	DataDirFound bool `json:"data_dir_found"`
	SocketFound  bool `json:"socket_found"`
	// Verified = ClientFound（"能不能连"的前提是客户端真的在）。
	Verified bool `json:"verified"`
	// Note 是给人看的实话（少了什么、为什么不算数）。
	Note string `json:"note,omitempty"`
}

// ResolveEngine 解析当前生效的数据库引擎，**只读磁盘、不猜默认值**。
//
// socketOverride 是面板配置里的 socket（默认 /tmp/mysql.sock）；它存在就用它。
// 探测范围只跟 brewPrefix 与这个 override 有关，所以单测可以在临时目录里完全隔离。
//
// 返回 error 的唯一场景：**两个引擎都装着**。这时磁盘上看不出该连哪个（两者不能
// 同时运行），挑一个默认值就是谎报 —— 调用方要么用 ResolveEngineFor 带上线索
// （例如 3306 上的进程名），要么把这句话原样告诉用户。
func ResolveEngine(brewPrefix, socketOverride string) (EnginePaths, error) {
	brewPrefix = strings.TrimSpace(brewPrefix)
	found := make([]EnginePaths, 0, len(engineKegs))
	for _, k := range engineKegs {
		if e, ok := ResolveEngineFor(brewPrefix, socketOverride, k.Formula); ok {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return EnginePaths{
			DataDir: filepath.Join(brewPrefix, "var", "mysql"),
			Note: "没有在 " + brewPrefix + "/opt 下找到 mysql 或 mariadb 客户端：" +
				"面板读不到当前生效的数据库引擎（未复核），不猜默认值",
		}, nil
	case 1:
		return found[0], nil
	default:
		return EnginePaths{}, errors.New("这台机器同时装着 mysql@8.4 与 mariadb：" +
			"两者默认共用数据目录与 3306，不能同时运行，面板无法判断该连哪一个。" +
			"请到「应用市场 → 网站环境」卸载其中一个后重试")
	}
}

// ResolveEnginePreferring 与 ResolveEngine 相同，但在"两个引擎都装着"时用
// holders（端口监听者，lsof 的 "名字 (pid N)" 形式）判开：mariadbd → MariaDB，
// mysqld → MySQL。判不开就把 ResolveEngine 的原话带回 —— 绝不挑默认值。
//
// holders 由调用方注入（备份/站点运行时/数据库页各自有现成的端口探测），
// 所以本包不自己起进程。
func ResolveEnginePreferring(brewPrefix, socketOverride string, holders []string) (EnginePaths, error) {
	paths, err := ResolveEngine(brewPrefix, socketOverride)
	if err == nil {
		return paths, nil
	}
	if k, ok := engineForHolders(holders); ok {
		if p, found := ResolveEngineFor(brewPrefix, socketOverride, k.Formula); found {
			note := "两个引擎都装着，按 3306 上的 " + k.Proc + " 判定当前生效的是 " + k.Name
			if strings.TrimSpace(p.Note) != "" {
				p.Note += "；" + note
			} else {
				p.Note = note
			}
			return p, nil
		}
	}
	return EnginePaths{}, err
}

// engineForHolders 在端口监听者里找"进程名吻合"的引擎。
func engineForHolders(holders []string) (engineKeg, bool) {
	for _, k := range engineKegs {
		for _, h := range holders {
			name := strings.ToLower(strings.TrimSpace(strings.SplitN(h, "(", 2)[0]))
			if name != "" && strings.Contains(name, k.Proc) {
				return k, true
			}
		}
	}
	return engineKeg{}, false
}

// EngineProcessName 返回引擎在 lsof 里的进程名（"mysqld" / "mariadbd"）。
// 认不出时返回空串（调用方据此"不猜"）。
func EngineProcessName(e DBEngine) string {
	for _, k := range engineKegs {
		if k.Engine == e {
			return k.Proc
		}
	}
	return ""
}

// ResolveEngineFor 只解析**指定 formula** 的引擎路径；客户端不在时 ok=false。
//
// 调用方已经知道该连哪个（例如 3306 的占用者是 mariadbd）时用这个，
// 免得被"两个都装着"卡住。
func ResolveEngineFor(brewPrefix, socketOverride, formula string) (EnginePaths, bool) {
	f := strings.TrimSpace(formula)
	brewPrefix = strings.TrimSpace(brewPrefix)
	binDir := filepath.Join(brewPrefix, "opt", f, "bin")
	if f == "" || clientBinary(binDir, "mysql") == "" {
		return EnginePaths{}, false
	}
	e := EnginePaths{
		Formula: f, BinDir: binDir,
		DataDir:     filepath.Join(brewPrefix, "var", "mysql"),
		ClientFound: true,
	}
	for _, k := range engineKegs {
		if k.Formula == f {
			e.Engine, e.Name, e.ServiceLabel = k.Engine, k.Name, k.Label
		}
	}
	e.DataDirFound = isDir(e.DataDir)
	if s, ok := resolveSocket(brewPrefix, socketOverride); ok {
		e.Socket, e.SocketFound = s, true
	}
	e.Verified = true
	var missing []string
	if !e.DataDirFound {
		missing = append(missing, "数据目录 "+e.DataDir+" 不存在")
	}
	if !e.SocketFound {
		missing = append(missing, "socket 文件未读到（客户端会改用 TCP）")
	}
	if len(missing) > 0 {
		e.Note = strings.Join(missing, "；")
	}
	return e, true
}

// resolveSocket 按"配置里的值 → brew 前缀下的候选"顺序找一个真实存在的 socket 文件。
//
// 不把 /tmp/mysql.sock 写进候选：它由调用方通过 socketOverride 传（面板配置默认值
// 就是它）——这样单测才能在完全隔离的临时目录里断言"读不到"。
func resolveSocket(brewPrefix, socketOverride string) (string, bool) {
	cands := []string{}
	if s := strings.TrimSpace(socketOverride); s != "" {
		cands = append(cands, s)
	}
	cands = append(cands,
		filepath.Join(brewPrefix, "var", "run", "mysqld", "mysqld.sock"),
		filepath.Join(brewPrefix, "var", "mysql", "mysql.sock"),
	)
	for _, c := range cands {
		if isSocketFile(c) {
			return c, true
		}
	}
	return "", false
}

// isSocketFile 判断路径是不是一个真实的 Unix socket（普通文件不算）。
func isSocketFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// clientBinary 解析一个客户端可执行文件，**同时认 MySQL 与 MariaDB 两套名字**。
//
// MariaDB 现在两套都装（mysql / mariadb、mysqldump / mariadb-dump），但新版本
// 明确在往 mariadb-* 收敛。面板只用 mysql 一套名字，将来上游一旦去掉兼容符号，
// 所有数据库功能会一起报"未找到 mysql 客户端"——那看起来像面板不支持 MariaDB。
// 返回 "" = 两套都没有。
func clientBinary(binDir, name string) string {
	if strings.TrimSpace(binDir) == "" {
		return ""
	}
	cands := []string{name}
	switch name {
	case "mysql":
		cands = append(cands, "mariadb")
	case "mysqldump":
		cands = append(cands, "mariadb-dump")
	case "mysqladmin":
		cands = append(cands, "mariadb-admin")
	}
	for _, c := range cands {
		p := filepath.Join(binDir, c)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
