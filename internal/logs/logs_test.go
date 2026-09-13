package logs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestCatalog 搭一个包含各类日志的临时环境。
func newTestCatalog(t *testing.T) (*Catalog, string) {
	t.Helper()
	root := t.TempDir()
	siteDir := filepath.Join(root, "site-logs")
	panelDir := filepath.Join(root, "panel-logs")
	cronDir := filepath.Join(root, "cron-logs")
	for _, d := range []string{siteDir, panelDir, cronDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(siteDir, "demo.test.access.log", "1.1.1.1 - - [1] \"GET /\" 200\n2.2.2.2 - - [2] \"GET /x\" 404\n")
	write(siteDir, "demo.test.error.log", "[error] something failed\n[warn] be careful\n")
	write(panelDir, "panel-20260315.log", "[INFO] [main] started\n[ERROR] [web] boom\n")
	write(cronDir, "cn.zizpanel.cron.daily-backup.log", "backup done\n")
	// 非 .log 文件不应出现在列表里
	write(siteDir, "notes.txt", "不是日志")

	c := NewCatalog(Options{
		SiteLogDir: siteDir, PanelLogDir: panelDir, CronLogDir: cronDir,
		BrewPrefix: root,
	})
	return c, root
}

func TestListDiscoversLogs(t *testing.T) {
	c, _ := newTestCatalog(t)
	list := c.List()
	byKey := map[string]Entry{}
	for _, e := range list {
		byKey[e.Key] = e
	}
	for _, want := range []string{
		"site:demo.test.access.log",
		"site:demo.test.error.log",
		"panel:panel-20260315.log",
		"cron:cn.zizpanel.cron.daily-backup.log",
	} {
		if _, ok := byKey[want]; !ok {
			t.Fatalf("未发现日志 %s（实际有 %d 条）", want, len(list))
		}
	}
	// 非日志文件不应被收录
	if _, ok := byKey["site:notes.txt"]; ok {
		t.Fatal("非 .log 文件不应出现在日志列表里")
	}
	// 固定位置的日志即使不存在也要列出来（让用户知道去哪找）
	if _, ok := byKey["php-fpm"]; !ok {
		t.Fatal("固定位置的日志（php-fpm）应始终列出")
	}
}

func TestDisplayNames(t *testing.T) {
	c, _ := newTestCatalog(t)
	for _, e := range c.List() {
		switch e.Key {
		case "site:demo.test.access.log":
			if !strings.Contains(e.Name, "demo.test") || !strings.Contains(e.Name, "访问") {
				t.Fatalf("站点访问日志展示名不友好: %s", e.Name)
			}
		case "panel:panel-20260315.log":
			if !strings.Contains(e.Name, "2026-03-15") {
				t.Fatalf("面板日志应显示日期: %s", e.Name)
			}
		case "cron:cn.zizpanel.cron.daily-backup.log":
			if !strings.Contains(e.Name, "daily-backup") {
				t.Fatalf("任务日志应显示任务名: %s", e.Name)
			}
		}
	}
}

func TestReadTail(t *testing.T) {
	c, _ := newTestCatalog(t)
	res, err := c.Read("site:demo.test.access.log", ReadOptions{Lines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("应只返回 1 行，实际 %d", len(res.Lines))
	}
	if !strings.Contains(res.Lines[0], "404") {
		t.Fatalf("末尾行应为最后一条日志: %q", res.Lines[0])
	}
	if res.Total != 2 {
		t.Fatalf("总行数应为 2，实际 %d", res.Total)
	}
}

func TestReadFilterAndLevel(t *testing.T) {
	c, _ := newTestCatalog(t)
	// 关键字过滤
	res, err := c.Read("site:demo.test.access.log", ReadOptions{Filter: "404"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 || !strings.Contains(res.Lines[0], "404") {
		t.Fatalf("关键字过滤失败: %+v", res.Lines)
	}
	// 级别过滤
	res2, err := c.Read("site:demo.test.error.log", ReadOptions{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Matched != 1 || !strings.Contains(res2.Lines[0], "failed") {
		t.Fatalf("级别过滤失败: %+v", res2.Lines)
	}
	// 正则过滤
	res3, err := c.Read("panel:panel-20260315.log", ReadOptions{Filter: `\[(ERROR|WARN)\]`, IsRegex: true})
	if err != nil {
		t.Fatal(err)
	}
	if res3.Matched != 1 {
		t.Fatalf("正则过滤失败: %+v", res3.Lines)
	}
}

// 过滤词里的正则元字符必须按字面量处理，否则用户搜 "a.b" 会匹配到 "axb"。
func TestReadFilterTreatsInputLiterally(t *testing.T) {
	c, root := newTestCatalog(t)
	p := filepath.Join(root, "site-logs", "literal.test.access.log")
	if err := os.WriteFile(p, []byte("a.b here\naxb here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := c.Read("site:literal.test.access.log", ReadOptions{Filter: "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 {
		t.Fatalf("应按字面量匹配，只命中 1 行，实际 %d：%v", res.Matched, res.Lines)
	}
}

// 大文件必须只读末尾，不能全量载入。
func TestReadLargeFileIsTruncated(t *testing.T) {
	c, root := newTestCatalog(t)
	p := filepath.Join(root, "site-logs", "big.test.access.log")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 200) + "\n"
	for i := 0; i < 30000; i++ {
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()

	res, err := c.Read("site:big.test.access.log", ReadOptions{Lines: 10, MaxBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("超过 MaxBytes 的读取应标记 Truncated")
	}
	if len(res.Lines) != 10 {
		t.Fatalf("应返回 10 行，实际 %d", len(res.Lines))
	}
}

func TestReadMissingFile(t *testing.T) {
	c, _ := newTestCatalog(t)
	// 未生成的固定日志：应返回空结果而不是错误
	res, err := c.Read("php-fpm", ReadOptions{Lines: 10})
	if err != nil {
		t.Fatalf("读取未生成的日志不应报错: %v", err)
	}
	if len(res.Lines) != 0 {
		t.Fatalf("应为空，实际 %d 行", len(res.Lines))
	}
	if _, err := c.Find("不存在的key"); err == nil {
		t.Fatal("查找不存在的日志应报错")
	}
}

// Follow 必须能推送新增内容。
func TestFollowPushesNewLines(t *testing.T) {
	c, root := newTestCatalog(t)
	p := filepath.Join(root, "site-logs", "follow.test.access.log")
	if err := os.WriteFile(p, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := c.Follow(ctx, "site:follow.test.access.log", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 首帧应带上下文
	select {
	case got := <-ch:
		if !strings.Contains(got, "first") {
			t.Fatalf("首帧应包含已有内容: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到首帧")
	}

	// 追加内容后应被推送
	time.Sleep(700 * time.Millisecond)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("NEWLINE-MARKER\n")
	_ = f.Close()

	deadline := time.After(4 * time.Second)
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatal("通道提前关闭")
			}
			if strings.Contains(got, "NEWLINE-MARKER") {
				return // 通过
			}
		case <-deadline:
			t.Fatal("未推送新增的日志行")
		}
	}
}

func TestTruncateKeepsBackup(t *testing.T) {
	c, root := newTestCatalog(t)
	p := filepath.Join(root, "panel-logs", "panel-20260316.log")
	content := strings.Repeat("old log line\n", 100)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, err := c.Truncate("panel:panel-20260316.log")
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if !strings.Contains(msg, "备份") {
		t.Fatalf("应提示已备份: %s", msg)
	}
	// 原文件应为空
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		t.Fatalf("清空后文件应为 0 字节，实际 %d", st.Size())
	}
	// 备份文件应存在且含原内容
	b, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	if !strings.Contains(string(b), "old log line") {
		t.Fatal("备份内容不正确")
	}
}

// 系统或第三方日志不允许删除（只能清空）。
func TestDeleteRefusesNonRotatable(t *testing.T) {
	c, _ := newTestCatalog(t)
	if err := c.Delete("site:demo.test.access.log"); err == nil {
		t.Fatal("站点日志由 nginx 产生，面板不应允许删除")
	}
	// 面板自己的日志可以删
	if err := c.Delete("panel:panel-20260315.log"); err != nil {
		t.Fatalf("面板日志应允许删除: %v", err)
	}
}

func TestStats(t *testing.T) {
	c, _ := newTestCatalog(t)
	s := c.Stats()
	if s.TotalFiles < 4 {
		t.Fatalf("应统计到至少 4 个文件，实际 %d", s.TotalFiles)
	}
	if s.TotalSize <= 0 {
		t.Fatal("总大小应大于 0")
	}
	if s.Missing == 0 {
		t.Fatal("应统计到未生成的日志数量")
	}
}

func TestCategoriesHaveLabels(t *testing.T) {
	for _, c := range Categories() {
		if c.Key == "" || c.Label == "" {
			t.Fatalf("分类缺少 key 或 label: %+v", c)
		}
	}
}
