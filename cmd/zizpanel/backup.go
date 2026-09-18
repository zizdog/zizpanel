package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/backup"
	"github.com/zizdog/zizpanel/internal/config"
	"github.com/zizdog/zizpanel/internal/store"
)

// cmdBackup 是备份/恢复的命令行入口。
//
// 为什么要有 CLI 子命令（而不是全塞进 Web）：
//   - 定时备份由 launchd 以**独立进程**执行，脚本里只能调用这个二进制；
//   - 面板挂掉/换机器时，用户至少还能在终端里验证一个归档是不是好的
//     （`zizpanel backup verify <file>`）。
func cmdBackup(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: zizpanel backup create|verify|list [参数]（zizpanel help 查看说明）")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return cmdBackupCreate(rest)
	case "verify":
		return cmdBackupVerify(rest)
	case "list":
		return cmdBackupList(rest)
	case "help", "-h", "--help":
		printBackupUsage()
		return nil
	default:
		return fmt.Errorf("未知的 backup 子命令: %s（可用: create / verify / list）", sub)
	}
}

func printBackupUsage() {
	fmt.Print(`zizpanel backup - 备份与恢复

用法:
  zizpanel backup create [--out <目录>] [--targets sites,mysql,nginx,panel,apps:ddns-go,...]
                         [--keep-days N] [--name <文件名>] [--config <配置文件>]
  zizpanel backup verify <归档.tar.gz>
  zizpanel backup list   [--out <目录>]

说明:
  · 数据库用 VACUUM INTO 取一致性快照（不会打包 WAL 中间状态）；
  · 归档里带 manifest.json（逐文件 size+sha256），verify 会整包校验；
  · 归档权限 0600：含面板配置、ACME 账号私钥与站点私钥。
`)
}

func cmdBackupCreate(args []string) error {
	fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	out := fs.String("out", "", "备份输出目录（默认 <WorkDir>/backup）")
	targetsFlag := fs.String("targets", "sites,nginx,panel", "备份范围，逗号分隔")
	keepDays := fs.Int("keep-days", 7, "保留天数（<=0 表示不清理）")
	name := fs.String("name", "", "归档文件名（默认按时间生成）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	targets := splitList(*targetsFlag)
	if len(targets) == 0 {
		return errors.New("--targets 不能为空")
	}
	// 目标名拼错必须**立即报错**，不能悄悄少备一项（那是"谎报成功"）。
	for _, t := range targets {
		if !backup.IsKnownTarget(t) {
			return fmt.Errorf("未知的备份范围 %q；可用范围: %s", t, strings.Join(backup.AllTargets(), ", "))
		}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("读取面板配置失败: %w", err)
	}
	cfg.ReconcilePaths()

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("打开面板数据库失败: %w", err)
	}
	defer func() { _ = st.Close() }()

	dir := *out
	if dir == "" {
		dir = filepath.Join(cfg.WorkDir, "backup")
	}
	req := backup.NewRequest(cfg, st, targets, dir, *keepDays)
	req.FileName = *name

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	res, err := backup.Create(ctx, st, req)
	if err != nil {
		return err
	}
	fmt.Printf("[%s] 备份完成：%s\n", time.Now().Format("2006-01-02 15:04:05"), res.Path)
	fmt.Printf("  范围：%s\n", strings.Join(res.Manifest.Targets, ","))
	fmt.Printf("  文件：%d 个，大小 %.1f MB\n", len(res.Manifest.Files), float64(res.Size)/1024/1024)
	if res.Manifest.ContainsSecrets {
		fmt.Println("  ⚠️ 归档含明文口令/私钥，请当机密文件保管（权限已设为 0600）")
	}
	for _, w := range res.Manifest.Warnings {
		fmt.Printf("  提示：%s\n", w)
	}
	return nil
}

func cmdBackupVerify(args []string) error {
	fs := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("用法: zizpanel backup verify <归档.tar.gz>")
	}
	path := fs.Arg(0)
	m, err := backup.Verify(path)
	if err != nil {
		return err
	}
	known := store.KnownTables()
	compat := backup.CheckCompatibility(m, known)
	fmt.Printf("归档：%s\n", path)
	fmt.Printf("  格式：%s\n", m.Format)
	fmt.Printf("  创建：%s  来源机器：%s  面板版本：%s\n", m.CreatedAt, m.Hostname, m.PanelVersion)
	fmt.Printf("  范围：%s\n", strings.Join(m.Targets, ","))
	fmt.Printf("  文件：%d 个，表：%d 张\n", len(m.Files), len(m.SchemaTables))
	if m.ContainsSecrets {
		fmt.Printf("  含明文口令/私钥的文件 %d 个（恢复前会二次确认）\n", len(m.SecretFiles()))
	}
	if !compat.OK {
		return fmt.Errorf("兼容性检查不通过：%s", compat.Reason)
	}
	if compat.Older {
		fmt.Printf("  ⚠️ 备份比本程序旧（缺表：%s），恢复后会自动重放迁移补齐\n",
			strings.Join(compat.MissingTables, ", "))
	}
	fmt.Println("✅ 校验通过：所有文件 sha256 一致")
	return nil
}

func cmdBackupList(args []string) error {
	fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	out := fs.String("out", "", "备份目录（默认 <WorkDir>/backup）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := *out
	if dir == "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return fmt.Errorf("读取面板配置失败: %w", err)
		}
		dir = filepath.Join(cfg.WorkDir, "backup")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("备份目录尚不存在：%s\n", dir)
			return nil
		}
		return err
	}
	type row struct {
		name string
		size int64
		at   time.Time
	}
	var rows []row
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows = append(rows, row{e.Name(), info.Size(), info.ModTime()})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].at.After(rows[j].at) })
	if len(rows) == 0 {
		fmt.Printf("备份目录为空：%s\n", dir)
		return nil
	}
	for _, r := range rows {
		fmt.Printf("%s  %8.1f MB  %s\n", r.name, float64(r.size)/1024/1024, r.at.Format("2006-01-02 15:04"))
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
