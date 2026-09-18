package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/backup"
	"github.com/zizdog/zizpanel/internal/store"
)

// Manager 管理计划任务。
type Manager struct {
	repo   *Repository
	logDir string
	// 以下三项是 backup 任务生成脚本时要用的**配置派生路径**。
	//
	// 为什么必须由调用方注入：脚本里过去写死了 /opt/zizpanel 与 /opt/homebrew，
	// 面板用 ZIZPANEL_ROOT 重定位或跑在 Intel 前缀（/usr/local）下时会备错东西
	// 或什么都不备 —— 而任务日志里看不出来（脚本只是"没找到目录"）。
	backupDir  string
	binaryPath string
	configPath string
}

// Options 是构造参数。
type Options struct {
	// LogDir 是任务运行日志目录
	LogDir string
	// BackupDir 是 backup 任务的默认输出目录（应传 <WorkDir>/backup）
	BackupDir string
	// BinaryPath 是面板二进制的绝对路径（backup 任务靠它执行真正的备份）
	BinaryPath string
	// ConfigPath 是面板配置文件路径（脚本用 --config 传给 CLI）
	ConfigPath string
}

// NewManager 创建计划任务管理器。
func NewManager(st *store.Store, opt Options) *Manager {
	logDir := opt.LogDir
	if logDir == "" {
		// 兜底：不再写死 /opt/zizpanel（重定位安装下会写到别人的目录里）。
		// 生产路径由 web 层显式传入，这里只服务直接调用本包的场景。
		logDir = filepath.Join(os.TempDir(), "zizpanel-cron")
	}
	bin := opt.BinaryPath
	if bin == "" {
		if exe, err := os.Executable(); err == nil {
			bin = exe
		}
	}
	return &Manager{
		repo:       NewRepository(st),
		logDir:     logDir,
		backupDir:  opt.BackupDir,
		binaryPath: bin,
		configPath: opt.ConfigPath,
	}
}

func (m *Manager) logPathFor(j *Job) string {
	return filepath.Join(m.logDir, j.LabelName()+".log")
}

// LogPath 返回任务日志路径（供接口读取）。
func (m *Manager) LogPath(id int64) (string, error) {
	j, err := m.repo.Get(context.Background(), id)
	if err != nil {
		return "", err
	}
	return m.logPathFor(j), nil
}

// EnsureLogDir 确保日志目录存在。
func (m *Manager) EnsureLogDir() error {
	return os.MkdirAll(m.logDir, 0o755)
}

// List 返回所有任务及其实时状态。
func (m *Manager) List(ctx context.Context) ([]*Job, error) {
	jobs, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		j.Label = j.LabelName()
		j.PlistPath = j.PlistPath2()
		if c, err := ParseCron(j.Schedule); err == nil {
			j.NextRunHint = formatNextRuns(c)
		}
		// 查 launchd 的真实加载状态（不信任数据库）
		if st, err := launchStatus(j.LabelName()); err == nil {
			j.Loaded = st
		}
	}
	return jobs, nil
}

// Get 返回单个任务。
func (m *Manager) Get(ctx context.Context, id int64) (*Job, error) {
	j, err := m.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	j.Label = j.LabelName()
	j.PlistPath = j.PlistPath2()
	if c, err := ParseCron(j.Schedule); err == nil {
		j.NextRunHint = formatNextRuns(c)
	}
	if st, err := launchStatus(j.LabelName()); err == nil {
		j.Loaded = st
	}
	return j, nil
}

func formatNextRuns(c *Cron) string {
	runs := NextRuns(c, time.Now(), 3)
	if len(runs) == 0 {
		return "（一年内不会触发，请检查计划设置）"
	}
	var parts []string
	for _, r := range runs {
		parts = append(parts, r.Format("01-02 15:04"))
	}
	return strings.Join(parts, " → ")
}

// Create 新建任务。
func (m *Manager) Create(ctx context.Context, j *Job) error {
	if err := m.validate(j); err != nil {
		return err
	}
	if err := m.repo.Create(ctx, j); err != nil {
		return err
	}
	// 落库后再写 launchd：即使 launchd 失败，任务记录仍在，用户可在界面上重试
	if err := m.apply(ctx, j); err != nil {
		return fmt.Errorf("任务已保存，但注册到系统失败: %w", err)
	}
	return nil
}

// Update 更新任务。
func (m *Manager) Update(ctx context.Context, j *Job) error {
	if err := m.validate(j); err != nil {
		return err
	}
	// 名称变化会导致 label 变化，需要先移除旧任务
	old, err := m.repo.Get(ctx, j.ID)
	if err == nil && old.Name != j.Name {
		_ = m.remove(ctx, old)
	}
	if err := m.repo.Update(ctx, j); err != nil {
		return err
	}
	if err := m.apply(ctx, j); err != nil {
		return fmt.Errorf("任务已保存，但注册到系统失败: %w", err)
	}
	return nil
}

// Delete 删除任务。
//
// 同时清理它的日志文件：任务都不存在了，日志留在日志中心里只是噪音
// （用户会看到一堆已删除任务的日志，不知道能不能删）。
func (m *Manager) Delete(ctx context.Context, id int64) error {
	j, err := m.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := m.remove(ctx, j); err != nil {
		return err
	}
	logPath := m.logPathFor(j)
	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		// 日志删不掉不算致命，任务删除成功才是关键
		fmt.Fprintf(os.Stderr, "清理任务日志失败 %s: %v\n", logPath, err)
	}
	_ = os.Remove(logPath + ".1")
	return m.repo.Delete(ctx, id)
}

// Toggle 启用/停用任务。
func (m *Manager) Toggle(ctx context.Context, id int64, enabled bool) error {
	j, err := m.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	j.Enabled = enabled
	if err := m.repo.Update(ctx, j); err != nil {
		return err
	}
	if enabled {
		return m.apply(ctx, j)
	}
	return m.remove(ctx, j)
}

// SyncAll 把数据库里的所有任务重新注册到 launchd。
//
// 用途：
//   - 面板升级后批量修复
//   - 系统重装/迁移后恢复任务
//   - 任务在不同步状态（数据库有、launchd 没有）时一键修复
func (m *Manager) SyncAll(ctx context.Context) (applied int, failed []string, err error) {
	jobs, err := m.repo.List(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, j := range jobs {
		if err := m.apply(ctx, j); err != nil {
			failed = append(failed, j.Name+": "+err.Error())
			continue
		}
		applied++
	}
	return applied, failed, nil
}

// validate 校验任务字段。
func (m *Manager) validate(j *Job) error {
	j.Name = strings.TrimSpace(j.Name)
	if j.Name == "" {
		return errors.New("请填写任务名称")
	}
	if Slug(j.Name) == "job" && len([]rune(j.Name)) > 0 {
		// 名称里没有任何可用字符（例如纯中文）
		return errors.New("任务名称需要包含字母或数字（用于生成系统标识），例如 daily-backup 或 备份1")
	}
	if err := ValidateCron(j.Schedule); err != nil {
		return err
	}
	switch j.Kind {
	case "shell", "":
		j.Kind = "shell"
		if strings.TrimSpace(j.Command) == "" {
			return errors.New("请填写要执行的命令")
		}
	case "backup":
		if len(j.BackupTargets) == 0 {
			j.BackupTargets = defaultBackupTargets()
		}
		for _, t := range j.BackupTargets {
			if !backup.IsKnownTarget(t) {
				return fmt.Errorf("未知的备份范围: %s（可用: %s）", t,
					strings.Join(backup.AllTargets(), ", "))
			}
		}
		if j.BackupDir == "" {
			// 默认值来自配置（<WorkDir>/backup），不再写死 /opt/zizpanel/work/backup。
			if m.backupDir == "" {
				return errors.New("备份目录未配置：面板配置里没有可用的工作目录")
			}
			j.BackupDir = m.backupDir
		}
		if j.KeepDays <= 0 {
			j.KeepDays = 7
		}
	case "url":
		if !strings.HasPrefix(j.Command, "http://") && !strings.HasPrefix(j.Command, "https://") {
			return errors.New("URL 任务必须以 http:// 或 https:// 开头")
		}
	default:
		return fmt.Errorf("未知的任务类型: %s", j.Kind)
	}
	return nil
}

// launchStatus 查询 launchd 是否已加载该任务（薄封装，便于测试替换）。
func launchStatus(label string) (bool, error) {
	st, err := privLaunchStatus(label)
	if err != nil {
		return false, err
	}
	return st.Loaded, nil
}

// ---------------------------------------------------------------------------
//  存储层
// ---------------------------------------------------------------------------

// Repository 负责任务的持久化。
type Repository struct {
	st *store.Store
}

// NewRepository 创建仓库。
func NewRepository(st *store.Store) *Repository { return &Repository{st: st} }

const jobCols = `id,name,kind,schedule,command,work_dir,enabled,last_run,last_status,last_output,run_count,created_at,backup_targets,backup_dir,keep_days`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var enabled int
	var targets string
	err := sc.Scan(&j.ID, &j.Name, &j.Kind, &j.Schedule, &j.Command, &j.WorkDir,
		&enabled, &j.LastRun, &j.LastStatus, &j.LastOutput, &j.RunCount, &j.CreatedAt,
		&targets, &j.BackupDir, &j.KeepDays)
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled == 1
	j.BackupTargets = splitTargets(targets)
	return &j, nil
}

// splitTargets 把库里的逗号分隔范围还原成切片（空串 = 还没选过）。
func splitTargets(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinTargets(list []string) string { return strings.Join(list, ",") }

// defaultBackupTargets 是没选范围时的默认（与既有行为一致，含 sites）。
func defaultBackupTargets() []string { return []string{"sites", "nginx", "panel"} }

// List 返回全部任务。
func (r *Repository) List(ctx context.Context) ([]*Job, error) {
	rows, err := r.st.DB().QueryContext(ctx, `SELECT `+jobCols+` FROM cron_jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if out == nil {
		out = []*Job{}
	}
	return out, rows.Err()
}

// Get 按 ID 查询。
func (r *Repository) Get(ctx context.Context, id int64) (*Job, error) {
	row := r.st.DB().QueryRowContext(ctx, `SELECT `+jobCols+` FROM cron_jobs WHERE id=?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("任务 %d 不存在", id)
	}
	return j, err
}

// Create 写入任务。
func (r *Repository) Create(ctx context.Context, j *Job) error {
	res, err := r.st.DB().ExecContext(ctx,
		`INSERT INTO cron_jobs(name,kind,schedule,command,work_dir,enabled,backup_targets,backup_dir,keep_days)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		j.Name, j.Kind, j.Schedule, j.Command, j.WorkDir, boolToInt(j.Enabled),
		joinTargets(j.BackupTargets), j.BackupDir, j.KeepDays)
	if err != nil {
		return err
	}
	j.ID, _ = res.LastInsertId()
	return nil
}

// Update 更新任务。
//
// 必须落库 backup_targets/backup_dir/keep_days：不写它们的话，用户在界面上选的
// 备份范围/目录/保留天数**面板一重启就回落默认**，而界面显示的是用户选的值
// ——"我明明选了 mysql，它却没备"就是这么来的。
func (r *Repository) Update(ctx context.Context, j *Job) error {
	_, err := r.st.DB().ExecContext(ctx,
		`UPDATE cron_jobs SET name=?,kind=?,schedule=?,command=?,work_dir=?,enabled=?,
		 backup_targets=?,backup_dir=?,keep_days=? WHERE id=?`,
		j.Name, j.Kind, j.Schedule, j.Command, j.WorkDir, boolToInt(j.Enabled),
		joinTargets(j.BackupTargets), j.BackupDir, j.KeepDays, j.ID)
	return err
}

// Delete 删除任务。
func (r *Repository) Delete(ctx context.Context, id int64) error {
	_, err := r.st.DB().ExecContext(ctx, `DELETE FROM cron_jobs WHERE id=?`, id)
	return err
}

// MarkRun 记录一次执行。
//
// 注意：launchd 触发的任务面板无法直接感知，
// 因此这里主要用于"手动触发"的记录；定时执行的结果通过读取日志体现。
func (r *Repository) MarkRun(ctx context.Context, id int64, status, output string) error {
	if len(output) > 4000 {
		output = output[:4000] + "…"
	}
	_, err := r.st.DB().ExecContext(ctx,
		`UPDATE cron_jobs SET last_run=datetime('now','localtime'), last_status=?,
		 last_output=?, run_count=run_count+1 WHERE id=?`, status, output, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
