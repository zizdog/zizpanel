package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/mysql"
	"github.com/zizdog/zizpanel/internal/version"
)

// run.go 把「面板配置」翻译成一次备份请求（路径、外部命令、生成项）。
//
// 这是 CLI（launchd 定时任务）与 Web（立即备份 / 恢复前快照）**共用**的装配层：
// 两处各写一份迟早会不一致，而"定时备份备的东西和界面里点的不一样"
// 是用户完全看不出来的那种坏。

// NewRequest 依据面板配置构造一次备份请求。
//
// outDir 为空时用 <WorkDir>/backup（沿用既有目录，不新开概念）。
func NewRequest(cfg *config.Config, snap Snapshotter, targets []string, outDir string, keepDays int) CreateRequest {
	if outDir == "" {
		outDir = filepath.Join(cfg.WorkDir, "backup")
	}
	req := CreateRequest{
		OutDir:   outDir,
		Targets:  targets,
		KeepDays: keepDays,
		// 用**二进制自己的版本号**，不用 cfg.Version —— 配置里的 version 字段是
		// 首次安装时写进去的，升级后不会跟着变（界面会显示一个假版本）。
		PanelVersion: version.Version,
		Plan: PlanOptions{
			DataDir:    cfg.DataDir,
			WorkDir:    cfg.WorkDir,
			BrewPrefix: cfg.BrewPrefix,
			UserHome:   cfg.UserHome,
		},
	}
	req.Generate = append(req.Generate, sitesGenerators(cfg, targets)...)
	req.Generate = append(req.Generate, mysqlGenerators(cfg, targets)...)
	return req
}

// sitesGenerators 生成 `sites` 目标（<UserHome>/www 的内容）。
//
// 用户 2026-09-21 拍板：~/www 站点文件**不默认进备份**（几 GB 级），
// 但既有 backup 任务的 sites 选项保持原样、用户勾了就照打。
func sitesGenerators(cfg *config.Config, targets []string) []GeneratedFile {
	if !Selected([]string{TargetSites}, targets) {
		return nil
	}
	if cfg.UserHome == "" {
		return nil
	}
	www := filepath.Join(cfg.UserHome, "www")
	if !Exists(www) {
		return nil
	}
	return []GeneratedFile{{
		ArchivePath: "sites/www.tar.gz",
		Targets:     []string{TargetSites},
		// 站点文件里常有 wp-config.php / .env（含数据库口令），按含密标注。
		Secrets: true,
		Write: func(dest string) error {
			// 与既有备份脚本一致：排除依赖目录，只留站点内容。
			args := []string{"-czf", dest, "-C", cfg.UserHome,
				"--exclude=*/node_modules", "--exclude=*/.git", "--exclude=*/vendor", "www"}
			out, err := exec.Command("tar", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("打包网站目录失败: %s", strings.TrimSpace(string(out)))
			}
			return nil
		},
	}}
}

// mysqlGenerators 生成 `mysql` 目标：每个业务库一份 mysqldump。
//
// 为什么逐库导出而不是 `--all-databases`：恢复时能按库选择，
// 也不会把 mysql 系统库（含账号口令哈希）混进归档。
func mysqlGenerators(cfg *config.Config, targets []string) []GeneratedFile {
	if !Selected([]string{TargetMySQL}, targets) {
		return nil
	}
	cli := clientFor(cfg)
	if cli == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dbs, err := cli.ListDatabases(ctx)
	if err != nil {
		// MySQL 连不上时**让整次备份如实失败**，而不是产出一个"看起来有数据库"
		// 却只有一个说明文件的备份（那正是"谎报成功"）。
		return []GeneratedFile{{
			ArchivePath: "mysql/export-failed",
			Targets:     []string{TargetMySQL},
			Secrets:     true,
			Write: func(dest string) error {
				return fmt.Errorf("连接 MySQL 失败，无法导出数据库（选了「数据库」范围就必须备到）: %w", err)
			},
		}}
	}
	var out []GeneratedFile
	for _, d := range dbs {
		name := d.Name
		if isSystemDB(name) {
			continue
		}
		out = append(out, GeneratedFile{
			ArchivePath: "mysql/" + name + ".sql",
			Targets:     []string{TargetMySQL},
			Secrets:     true,
			Write: func(dest string) error {
				tmp, err := os.MkdirTemp("", "zp-dump-")
				if err != nil {
					return err
				}
				defer func() { _ = os.RemoveAll(tmp) }()
				res, err := cli.DumpDatabase(context.Background(), name, tmp)
				if err != nil {
					return err
				}
				return copyFileTo(res.Path, dest)
			},
		})
	}
	return out
}

func isSystemDB(n string) bool {
	switch strings.ToLower(n) {
	case "information_schema", "performance_schema", "mysql", "sys":
		return true
	}
	return false
}

// clientFor 按面板配置构造 MySQL 客户端（与「数据库」页同一套路径解析）。
func clientFor(cfg *config.Config) *mysql.Client {
	if cfg.BrewPrefix == "" {
		return nil
	}
	binDir := filepath.Join(cfg.BrewPrefix, "opt", "mysql@8.4", "bin")
	if !Exists(filepath.Join(binDir, "mysql")) {
		binDir = filepath.Join(cfg.BrewPrefix, "bin")
	}
	host, port, socket, user := cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLSocket, cfg.MySQLUser
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
	return mysql.NewClient(mysql.Options{
		BinDir:   binDir,
		Host:     host,
		Port:     port,
		Socket:   socket,
		User:     user,
		Password: cfg.MySQLPasswordValue(),
		Timeout:  30 * time.Second,
		UserName: cfg.User,
		UserHome: cfg.UserHome,
	})
}
