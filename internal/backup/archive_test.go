package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/store"
)

// fixture 造一棵"面板真实布局"的临时目录树，返回 PlanOptions 与数据目录。
func fixture(t *testing.T) (PlanOptions, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	workDir := filepath.Join(root, "work")
	brew := filepath.Join(root, "brew")
	home := filepath.Join(root, "home")
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(dataDir, "config.json"), `{"panel_suffix":"abc","secret":"s"}`)
	write(filepath.Join(dataDir, "tls", "panel.key"), "PANEL-KEY")
	write(filepath.Join(dataDir, "tls", "panel.crt"), "PANEL-CRT")
	write(filepath.Join(dataDir, "certs", "example.com", "fullchain.pem"), "CHAIN")
	write(filepath.Join(dataDir, "certs", "example.com", "privkey.pem"), "KEY")
	write(filepath.Join(dataDir, "certs", "example.com", "meta.json"), `{"primary":"example.com"}`)
	write(filepath.Join(dataDir, "site-certs", "example.com", "fullchain.pem"), "CHAIN2")
	write(filepath.Join(dataDir, "proxy-certs", "proxy-x", "privkey.pem"), "PKEY")
	write(filepath.Join(dataDir, "acme", "credentials.json"), `{"dns":"token"}`)
	write(filepath.Join(dataDir, "acme", "accounts", "abc.json"), `{"account":"x"}`)
	write(filepath.Join(dataDir, "acme", "attempts", "example.com.json"), `{"ok":false}`)
	write(filepath.Join(dataDir, "default-site.json"), `{"created":true}`)
	write(filepath.Join(workDir, "compose", "myapp", "docker-compose.yml"), "services: {}")
	write(filepath.Join(workDir, "compose", "myapp", ".env"), "MYSQL_PASSWORD=secret")
	write(filepath.Join(brew, "etc", "nginx", "nginx.conf"), "events {}")
	write(filepath.Join(brew, "etc", "nginx", "nginx.conf.zizpanel.bak"), "events {} #bak")
	write(filepath.Join(brew, "etc", "nginx", "conf.d", "upgrade-map.conf"), "map $http_upgrade $connection_upgrade {}")
	write(filepath.Join(brew, "etc", "nginx", "includes", "php-fpm.conf"), "fastcgi_pass 127.0.0.1:9000;")
	write(filepath.Join(brew, "etc", "nginx", "vhosts", "example.com.conf"), "server {}")
	write(filepath.Join(brew, "etc", "php", "8.4", "conf.d", "99-zizpanel-limits.ini"), "upload_max_filesize=512M")
	write(filepath.Join(brew, "etc", "phpmyadmin.config.inc.php"), "<?php $cfg['blowfish_secret']='x';")

	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO sites(domain,root) VALUES('example.com','/tmp/example')`); err != nil {
		t.Fatal(err)
	}
	return PlanOptions{DataDir: dataDir, WorkDir: workDir, BrewPrefix: brew, UserHome: home}, st, home
}

func TestCreateVerifyRoundTrip(t *testing.T) {
	ctx := context.Background()
	plan, st, _ := fixture(t)
	outDir := filepath.Join(t.TempDir(), "backup")
	req := CreateRequest{
		OutDir: outDir, Targets: []string{TargetPanel, TargetNginx, TargetCompose},
		PanelVersion: "9.9.9", Plan: plan,
	}
	res, err := Create(ctx, st, req)
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	if !strings.HasPrefix(res.FileName, "backup-") || !strings.HasSuffix(res.FileName, ".tar.gz") {
		t.Fatalf("归档命名不符合既有约定: %s", res.FileName)
	}
	stt, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if stt.Mode().Perm() != 0o600 {
		t.Fatalf("归档权限必须是 0600（含明文口令与私钥），实际 %o", stt.Mode().Perm())
	}

	m, err := Verify(res.Path)
	if err != nil {
		t.Fatalf("自产归档应能通过校验: %v", err)
	}
	if !m.ContainsSecrets {
		t.Fatal("含 config.json/私钥的归档必须标记 contains_secrets")
	}
	// 必进清单的路径
	for _, want := range []string{
		"db/panel.db", "data/config.json", "data/tls/panel.key",
		"data/certs/example.com/fullchain.pem", "data/acme/credentials.json",
		"data/default-site.json",
		"nginx/nginx.conf", "nginx/conf.d/upgrade-map.conf", "nginx/vhosts/example.com.conf",
		"php/8.4/99-zizpanel-limits.ini", "phpmyadmin/config.inc.php",
		"apps/compose/myapp/docker-compose.yml",
	} {
		if _, ok := m.FileByPath(want); !ok {
			t.Fatalf("清单里缺少应当备份的 %s；实际清单：%v", want, fileNames(m))
		}
	}
	// 不该进清单的
	for _, bad := range []string{"data/panel.db", "data/panel.db-wal", "data/logs/x"} {
		if _, ok := m.FileByPath(bad); ok {
			t.Fatalf("清单里不该出现 %s", bad)
		}
	}
	if len(m.SchemaTables) == 0 {
		t.Fatal("清单必须记录备份库的表清单（兼容性判定靠它）")
	}
}

func fileNames(m *Manifest) []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	return out
}

func TestVerifyRejectsTamperedFile(t *testing.T) {
	ctx := context.Background()
	plan, st, _ := fixture(t)
	outDir := filepath.Join(t.TempDir(), "backup")
	res, err := Create(ctx, st, CreateRequest{
		OutDir: outDir, Targets: []string{TargetPanel}, Plan: plan,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 解包 → 改一个字节 → 用**原清单**重新打包：sha256 必然对不上。
	stage := t.TempDir()
	if _, err := Extract(res.Path, stage); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(stage, "data", "config.json")
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(b, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "tampered.tar.gz")
	if err := tarGzDir(stage, tampered); err != nil {
		t.Fatal(err)
	}
	_, err = Verify(tampered)
	if err == nil {
		t.Fatal("被改动的归档必须整包拒绝")
	}
	if !strings.Contains(err.Error(), "sha256") && !strings.Contains(err.Error(), "大小不符") {
		t.Fatalf("拒绝原因应说明校验和不符，实际: %v", err)
	}
}

func TestVerifyRejectsMissingFile(t *testing.T) {
	ctx := context.Background()
	plan, st, _ := fixture(t)
	outDir := filepath.Join(t.TempDir(), "backup")
	res, err := Create(ctx, st, CreateRequest{OutDir: outDir, Targets: []string{TargetPanel}, Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if _, err := Extract(res.Path, stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stage, "data", "default-site.json")); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "missing.tar.gz")
	if err := tarGzDir(stage, broken); err != nil {
		t.Fatal(err)
	}
	_, err = Verify(broken)
	if err == nil || !strings.Contains(err.Error(), "缺失") {
		t.Fatalf("缺文件必须被拒绝并说明缺哪个，实际: %v", err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "garbage.tar.gz")
	if err := os.WriteFile(p, []byte("not a tar.gz at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(p); err == nil {
		t.Fatal("非 tar.gz 必须被拒绝")
	}
}

// TestVerifyRejectsPathTraversal：归档里带 .. 的条目必须拒绝（解包写文件前的硬门槛）。
func TestVerifyRejectsPathTraversal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "evil.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("x")
	man := `{"format":"` + Format + `","files":[{"path":"../evil.txt","size":1,"sha256":"` +
		hashBytes(body) + `"}]}`
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(man))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(man)); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()

	if _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "路径穿越") {
		t.Fatalf("路径穿越必须被拒绝，实际: %v", err)
	}
}

func TestCheckCompatibility(t *testing.T) {
	known := []string{"users", "sites", "sessions"}
	if c := CheckCompatibility(&Manifest{Format: Format, SchemaTables: []string{"users", "sites"}}, known); !c.OK || !c.Older {
		t.Fatalf("备份更旧应允许且标记 Older，实际 %+v", c)
	}
	if c := CheckCompatibility(&Manifest{Format: Format, SchemaTables: []string{"users", "future_table"}}, known); c.OK {
		t.Fatalf("备份含未知表必须拒绝，实际 %+v", c)
	}
	if c := CheckCompatibility(&Manifest{Format: "other/9", SchemaTables: known}, known); c.OK {
		t.Fatal("格式不认识必须拒绝")
	}
	if c := CheckCompatibility(&Manifest{Format: Format}, known); c.OK {
		t.Fatal("没有表信息必须拒绝（不能靠 schema_version）")
	}
}

func TestPruneRemovesOldArchives(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "backup-20200101-000000.tar.gz")
	keep := filepath.Join(dir, "backup-new.tar.gz")
	for _, p := range []string{old, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().AddDate(0, 0, -30)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	removed, err := Prune(dir, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != filepath.Base(old) {
		t.Fatalf("应只删过期归档，实际删除 %v", removed)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("未过期的归档被误删")
	}
}
