package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zizdog/zizpanel/internal/store"
)

// ErrServiceNotFound 表示注册表里没有这条服务记录。
// HTTP 层据此返回 404，而不是把"记录本来就没有"报成 500。
var ErrServiceNotFound = errors.New("服务不存在")

// Repository 负责服务注册表的持久化。
//
// 只存"这个服务是什么、怎么管"，不存运行状态 ——
// 状态必须每次实时查询，否则面板会显示与服务实际情况不符的信息。
type Repository struct {
	st *store.Store
}

// NewRepository 创建仓库。
func NewRepository(st *store.Store) *Repository { return &Repository{st: st} }

const cols = `id,name,display_name,kind,category,icon,description,port,
	launch_label,plist_path,work_dir,start_cmd,container,compose_file,image,
	health_url,health_expect,log_path,autostart,enabled,managed,created_at,updated_at`

func scanService(sc interface{ Scan(...any) error }) (*Service, error) {
	var s Service
	var kind string
	var autostart, enabled, managed int
	err := sc.Scan(&s.ID, &s.Name, &s.DisplayName, &kind, &s.Category, &s.Icon,
		&s.Description, &s.Port, &s.LaunchLabel, &s.PlistPath, &s.WorkDir, &s.StartCmd,
		&s.Container, &s.ComposeFile, &s.Image, &s.HealthURL, &s.HealthExpect,
		&s.LogPath, &autostart, &enabled, &managed, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.Kind = Kind(kind)
	s.Autostart = autostart == 1
	s.Enabled = enabled == 1
	s.Managed = managed == 1
	// 显示名兜底：老记录里 display_name 存的可能就是 launchd 标签
	// （sh.brew.mysql@8.4），界面上该显示「MySQL 8.4」。
	// 在这里做而不是在注册时做，是为了让已登记的服务立刻变好看，不用重新纳管。
	s.DisplayName = displayNameOf(&s)
	return &s, nil
}

// List 返回所有服务。
func (r *Repository) List(ctx context.Context) ([]*Service, error) {
	rows, err := r.st.DB().QueryContext(ctx,
		`SELECT `+cols+` FROM services ORDER BY
		 CASE category WHEN 'lnmp' THEN 0 WHEN 'ai' THEN 1 WHEN 'tool' THEN 2 ELSE 3 END,
		 display_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Service
	for rows.Next() {
		s, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if out == nil {
		out = []*Service{}
	}
	return out, rows.Err()
}

// Get 按服务名查询。
func (r *Repository) Get(ctx context.Context, name string) (*Service, error) {
	row := r.st.DB().QueryRowContext(ctx,
		`SELECT `+cols+` FROM services WHERE name=?`, name)
	s, err := scanService(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("服务 %s 不存在", name)
	}
	return s, err
}

// Exists 判断服务名是否已占用。
func (r *Repository) Exists(ctx context.Context, name string) (bool, error) {
	var n int
	err := r.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM services WHERE name=?`, name).Scan(&n)
	return n > 0, err
}

// Create 写入一条服务记录。
func (r *Repository) Create(ctx context.Context, s *Service) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("服务名不能为空")
	}
	if s.Kind == "" {
		s.Kind = KindNative
	}
	if s.DisplayName == "" {
		s.DisplayName = s.Name
	}
	if s.Category == "" {
		s.Category = "other"
	}
	res, err := r.st.DB().ExecContext(ctx,
		`INSERT INTO services(name,display_name,kind,category,icon,description,port,
		 launch_label,plist_path,work_dir,start_cmd,container,compose_file,image,
		 health_url,health_expect,log_path,autostart,enabled,managed)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.Name, s.DisplayName, string(s.Kind), s.Category, s.Icon, s.Description, s.Port,
		s.LaunchLabel, s.PlistPath, s.WorkDir, s.StartCmd, s.Container, s.ComposeFile, s.Image,
		s.HealthURL, s.HealthExpect, s.LogPath, boolInt(s.Autostart), boolInt(s.Enabled), boolInt(s.Managed))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("服务名 %s 已存在", s.Name)
		}
		return err
	}
	s.ID, _ = res.LastInsertId()
	return nil
}

// Update 更新服务记录（不改变 managed 与 name）。
func (r *Repository) Update(ctx context.Context, s *Service) error {
	_, err := r.st.DB().ExecContext(ctx,
		`UPDATE services SET display_name=?,kind=?,category=?,icon=?,description=?,port=?,
		 launch_label=?,plist_path=?,work_dir=?,start_cmd=?,container=?,compose_file=?,image=?,
		 health_url=?,health_expect=?,log_path=?,autostart=?,enabled=?,
		 updated_at=datetime('now','localtime')
		 WHERE name=?`,
		s.DisplayName, string(s.Kind), s.Category, s.Icon, s.Description, s.Port,
		s.LaunchLabel, s.PlistPath, s.WorkDir, s.StartCmd, s.Container, s.ComposeFile, s.Image,
		s.HealthURL, s.HealthExpect, s.LogPath, boolInt(s.Autostart), boolInt(s.Enabled), s.Name)
	return err
}

// Delete 删除服务记录。
func (r *Repository) Delete(ctx context.Context, name string) error {
	res, err := r.st.DB().ExecContext(ctx, `DELETE FROM services WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 用哨兵错误把"不存在"和真正的数据库故障分开：HTTP 层要据此返回 404，
		// 而不是把"记录本来就没有"报成 500（曾让清理脚本/UI 测试看到假的服务端错误）。
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	return nil
}

// CountByPort 统计占用指定端口的服务（用于端口冲突检测）。
func (r *Repository) CountByPort(ctx context.Context, port int, excludeName string) ([]string, error) {
	rows, err := r.st.DB().QueryContext(ctx,
		`SELECT name FROM services WHERE port=? AND name<>?`, port, excludeName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

// Count 返回服务总数（仪表盘用）。
func (r *Repository) Count(ctx context.Context) (int, error) {
	var n int
	err := r.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM services`).Scan(&n)
	return n, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
