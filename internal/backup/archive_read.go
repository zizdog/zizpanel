package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// tablesInSQLite 读出一个 SQLite 文件里的表清单（排除 sqlite_ 内部表）。
func tablesInSQLite(path string) ([]string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("打开数据库快照失败: %w", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("读取表清单失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReadManifest 只读清单（列表页展示"来自哪台机器/含不含明文口令"用），
// 不做全包校验（列表页要快；恢复前才做逐文件 sha256）。
func ReadManifest(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("不是有效的 tar.gz 归档: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败: %w", err)
		}
		if filepath.ToSlash(strings.TrimPrefix(hdr.Name, "./")) != ManifestName {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 8<<20))
		if err != nil {
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("清单不是合法 JSON: %w", err)
		}
		return &m, nil
	}
	return nil, errors.New("归档里没有 manifest.json —— 不是本面板生成的备份")
}

// Verify 校验整个归档：清单存在且格式认识、每个文件 size/sha256 一致、
// 没有清单外的多余文件。任何一条不符都返回错误（调用方必须**整包拒绝**）。
func Verify(path string) (*Manifest, error) {
	m, err := ReadManifest(path)
	if err != nil {
		return nil, err
	}
	if m.Format != Format {
		return nil, fmt.Errorf("归档格式 %q 不是本程序支持的 %q", m.Format, Format)
	}
	expect := map[string]FileEntry{}
	for _, f := range m.Files {
		if _, dup := expect[f.Path]; dup {
			return nil, fmt.Errorf("清单里 %s 重复登记", f.Path)
		}
		if err := checkArchivePath(f.Path); err != nil {
			return nil, err
		}
		expect[f.Path] = f
	}
	found := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("不是有效的 tar.gz 归档: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var problems []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败: %w", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		if name == ManifestName {
			continue
		}
		e, ok := expect[name]
		if !ok {
			problems = append(problems, "归档里有清单未登记的文件: "+name)
			continue
		}
		sum, size, err := hashArchiveEntry(tr)
		if err != nil {
			return nil, err
		}
		if size != e.Size {
			problems = append(problems, fmt.Sprintf("%s 大小不符（清单 %d，实际 %d）", name, e.Size, size))
		}
		if sum != e.SHA256 {
			problems = append(problems, fmt.Sprintf("%s sha256 不符（清单 %s…，实际 %s…）",
				name, short(e.SHA256), short(sum)))
		}
		found[name] = true
	}
	var missing []string
	for p := range expect {
		if !found[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		problems = append(problems, "清单登记但归档里缺失: "+p)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("归档校验不通过（%d 项），已整包拒绝:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	return m, nil
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func hashArchiveEntry(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Extract 把归档解到 destDir（自动先做整包校验）。
func Extract(path, destDir string) (*Manifest, error) {
	m, err := Verify(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		if err := checkArchivePath(name); err != nil {
			return nil, err
		}
		dest := filepath.Join(destDir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o700); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return nil, err
			}
			if err := out.Close(); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("归档里有不支持的特殊文件类型: %s", name)
		}
	}
	return m, nil
}

// checkArchivePath 挡住路径穿越（绝对路径、..、空路径）。
func checkArchivePath(p string) error {
	if p == "" || p == "." {
		return fmt.Errorf("归档里有空的文件路径")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("归档里有绝对路径，拒绝: %s", p)
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return fmt.Errorf("归档里有路径穿越（..），拒绝: %s", p)
		}
	}
	return nil
}

// EnsureFileMode 把解出来的敏感文件收成 0600（私钥/凭据）。
func EnsureFileMode(path string, mode os.FileMode) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
