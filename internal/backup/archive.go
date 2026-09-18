package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ArchiveName 返回一次备份的归档文件名（与既有「已有备份」列表的命名一致）。
func ArchiveName(t time.Time) string { return "backup-" + stampForFile(t) + ".tar.gz" }

// SnapshotName 返回"恢复前自动快照"的归档文件名。
//
// 与普通备份区分开：它不是用户要的备份，而是回滚凭据。
func SnapshotName(t time.Time) string { return "pre-restore-" + stampForFile(t) + ".tar.gz" }

// Snapshotter 是"生成一致性数据库快照"的能力（由 store.Store 实现）。
type Snapshotter interface {
	SnapshotToFile(ctx context.Context, dest string) error
}

// CreateRequest 描述一次归档生成。
type CreateRequest struct {
	OutDir       string
	FileName     string // 空 = 自动按时间生成 backup-<stamp>.tar.gz
	Targets      []string
	KeepDays     int
	PanelVersion string
	Plan         PlanOptions
	// Generate 是现场生成的条目（mysqldump / sites 打包）。Write 收到一个
	// "目标文件路径"，负责把内容写到那里。
	Generate []GeneratedFile
	// SnapshotNamespace 决定快照条目的归档内路径前缀（默认 db/panel.db）。
	Now func() time.Time
}

// Result 是一次备份的结果。
type Result struct {
	Path     string    `json:"path"`
	FileName string    `json:"file_name"`
	Size     int64     `json:"size"`
	Manifest *Manifest `json:"manifest"`
}

// Create 生成一个归档：一致性数据库快照 + 逐文件 sha256 + manifest.json。
//
// 流程刻意"先在临时目录拼好、再原子 rename 成成品"：中途失败不会在备份目录里
// 留下半个归档（半个归档看起来像备份，恢复时才发现是坏的）。
func Create(ctx context.Context, snap Snapshotter, req CreateRequest) (*Result, error) {
	targets := NormalizeTargets(req.Targets)
	if len(targets) == 0 {
		return nil, errors.New("没有选择任何备份范围")
	}
	now := time.Now
	if req.Now != nil {
		now = req.Now
	}
	if err := os.MkdirAll(req.OutDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建备份目录失败: %w", err)
	}
	name := req.FileName
	if name == "" {
		name = ArchiveName(now())
	}
	name = filepath.Base(name)
	if !strings.HasSuffix(name, ".tar.gz") {
		return nil, fmt.Errorf("归档文件名必须以 .tar.gz 结尾: %s", name)
	}
	out := filepath.Join(req.OutDir, name)

	// 临时目录放在输出目录内：rename 才是原子的（跨文件系统会失败）。
	stage, err := os.MkdirTemp(req.OutDir, ".zpb-stage-")
	if err != nil {
		return nil, fmt.Errorf("创建暂存目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	man := &Manifest{
		Format:        Format,
		CreatedAt:     nowStamp(),
		PanelVersion:  req.PanelVersion,
		Hostname:      hostname(),
		SourceDataDir: req.Plan.DataDir,
		BrewPrefix:    req.Plan.BrewPrefix,
		Targets:       targets,
	}

	// 1) 数据库一致性快照（VACUUM INTO）。没有它整个归档就没有意义 ——
	//    宁可整包失败，也不要产出一个数据库不可恢复的备份。
	if snap == nil {
		return nil, errors.New("内部错误：缺少数据库快照能力")
	}
	dbDest := filepath.Join(stage, "db", "panel.db")
	if err := snap.SnapshotToFile(ctx, dbDest); err != nil {
		return nil, err
	}

	// 2) 逐条复制来源（目录递归）。
	for _, it := range Plan(req.Plan) {
		if !Selected(it.Targets, targets) {
			continue
		}
		if !Exists(it.SourcePath) {
			continue // 可选项目不存在很正常（例如没装 PHP、没建过站点证书）
		}
		if err := copyInto(stage, it.ArchivePath, it.SourcePath, it.Secrets, man); err != nil {
			return nil, err
		}
	}

	// 3) 现场生成的条目（mysqldump / sites）。
	for _, g := range req.Generate {
		if !Selected(g.Targets, targets) {
			continue
		}
		dest := filepath.Join(stage, filepath.FromSlash(g.ArchivePath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return nil, err
		}
		if err := g.Write(dest); err != nil {
			return nil, fmt.Errorf("生成 %s 失败: %w", g.ArchivePath, err)
		}
		if err := recordFile(man, stage, g.ArchivePath, dest, g.Secrets); err != nil {
			return nil, err
		}
	}

	// 4) 表清单（兼容性判定用）。
	tables, err := tablesInSQLite(dbDest)
	if err != nil {
		return nil, err
	}
	man.SchemaTables = tables

	// 5) 清单里加上数据库快照本身。
	if err := recordFile(man, stage, "db/panel.db", dbDest, true); err != nil {
		return nil, err
	}
	sort.Slice(man.Files, func(i, j int) bool { return man.Files[i].Path < man.Files[j].Path })
	for _, f := range man.Files {
		if f.Secrets {
			man.ContainsSecrets = true
		}
	}
	if err := writeManifest(stage, man); err != nil {
		return nil, fmt.Errorf("写入清单失败: %w", err)
	}

	// 6) 打包 + 原子落位 + 权限 0600（含明文口令与私钥）。
	tmpOut := out + ".tmp"
	if err := tarGzDir(stage, tmpOut); err != nil {
		_ = os.Remove(tmpOut)
		return nil, err
	}
	if err := os.Chmod(tmpOut, 0o600); err != nil {
		_ = os.Remove(tmpOut)
		return nil, err
	}
	if err := os.Rename(tmpOut, out); err != nil {
		_ = os.Remove(tmpOut)
		return nil, fmt.Errorf("落位归档失败: %w", err)
	}
	st, err := os.Stat(out)
	if err != nil {
		return nil, err
	}

	if req.KeepDays > 0 {
		if _, err := Prune(req.OutDir, req.KeepDays, now()); err != nil {
			man.Warnings = append(man.Warnings, "清理旧备份失败: "+err.Error())
		}
	}
	return &Result{Path: out, FileName: name, Size: st.Size(), Manifest: man}, nil
}

// Prune 删除超过 keepDays 的归档（备份与恢复快照都算）。
func Prune(dir string, keepDays int, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	var removed []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		if !strings.HasPrefix(e.Name(), "backup-") && !strings.HasPrefix(e.Name(), "pre-restore-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				removed = append(removed, e.Name())
			}
		}
	}
	sort.Strings(removed)
	return removed, nil
}

// copyInto 把一个文件或目录复制进暂存目录，并在清单里逐文件登记。
func copyInto(stage, archivePath, src string, secrets bool, man *Manifest) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", src, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		man.Warnings = append(man.Warnings, "跳过符号链接: "+src)
		return nil
	}
	if !info.IsDir() {
		dest := filepath.Join(stage, filepath.FromSlash(archivePath))
		if err := copyFileTo(src, dest); err != nil {
			return err
		}
		return recordFile(man, stage, archivePath, dest, secrets)
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("遍历 %s 失败: %w", src, err)
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			man.Warnings = append(man.Warnings, "跳过符号链接: "+p)
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		ap := filepath.ToSlash(filepath.Join(archivePath, rel))
		dest := filepath.Join(stage, filepath.FromSlash(ap))
		if err := copyFileTo(p, dest); err != nil {
			return err
		}
		return recordFile(man, stage, ap, dest, secrets)
	})
}

func recordFile(man *Manifest, stage, archivePath, realPath string, secrets bool) error {
	sum, size, err := hashFile(realPath)
	if err != nil {
		return fmt.Errorf("计算 %s 校验和失败: %w", archivePath, err)
	}
	man.Files = append(man.Files, FileEntry{
		Path: filepath.ToSlash(archivePath), Size: size, SHA256: sum, Secrets: secrets,
	})
	return nil
}

// copyFileTo 把 src 复制到 dest，**一律 0600**。
//
// 为什么固定 0600 而不是沿用源权限：备份/恢复流动的是面板配置、ACME 账号私钥、
// 站点私钥 —— 把源文件的 0644 带过来等于把私钥摊开给同机其它用户。
// nginx 配置文件在恢复时需要被真实用户读到，那是调用方（relaxPerms）单独放宽的事，
// 不是这个通用复制函数的默认行为。
func copyFileTo(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func tarGzDir(dir, out string) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	werr := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		// 归档里不带原始权限位：解包时由恢复流程决定（0600/0644），
		// 避免把来源机器的 0777 之类带过来。
		if info.Mode().IsRegular() {
			hdr.Mode = 0o600
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		_, err = io.Copy(tw, in)
		return err
	})
	if werr != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
		return fmt.Errorf("打包失败: %w", werr)
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}
