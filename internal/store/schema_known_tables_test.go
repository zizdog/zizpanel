package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKnownTablesCoversEverySchemaSource 是"备份能不能恢复"的兜底门禁。
//
// 为什么必须有：KnownTables() 是备份兼容性判定的唯一依据。任何**自带建表语句**的
// 文件（比如导航页的 nav.go）如果忘了把自己登记进去，备份里的那批表就会被判成
// "当前程序不认识的表" → 整包拒绝恢复（2026-09-18 导航页接入时正是如此：
// 升级后所有新备份都恢复不了，而单测全绿）。
//
// 判据故意**扫源码**而不是问调用方：这样新加一个 schema 文件时，忘了登记就会红。
func TestKnownTablesCoversEverySchemaSource(t *testing.T) {
	known := map[string]bool{}
	for _, n := range KnownTables() {
		known[n] = true
	}
	if len(known) == 0 {
		t.Fatal("KnownTables() 返回空 —— 判据本身坏了")
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fromSource := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		// 剥掉 // 行注释：store.go 里有一句注释写着"CREATE TABLE IF NOT EXISTS 建不出来"，
		// 不剥的话会被当成一张叫 IF 的表（第一版门禁就是这么误报的）。
		var lines []string
		for _, ln := range strings.Split(string(b), "\n") {
			if i := strings.Index(ln, "//"); i >= 0 {
				ln = ln[:i]
			}
			lines = append(lines, ln)
		}
		// 用**生产代码的解析器**（splitStatements + createTableName）而不是另写正则：
		// 两套解析必然走样，而 KnownTables() 用的就是生产那套。
		for _, stmt := range splitStatements(strings.Join(lines, "\n")) {
			tbl := createTableName(stmt)
			if tbl == "" {
				continue
			}
			fromSource[tbl] = true
			if !known[tbl] {
				t.Errorf("%s 里的 CREATE TABLE %q 没进 KnownTables()：备份兼容性判定会把它当成「程序不认识的表」，"+
					"含这张表的备份会整包拒绝恢复。修法：在该文件里 registerSchemaSource(它所属的 schema 变量)。", name, tbl)
			}
		}
	}
	if len(fromSource) == 0 {
		t.Fatal("扫源码一张表都没找到 —— 扫描判据本身坏了")
	}
	// 导航页这两张表必须在（本轮真事故的回归锚点）
	for _, n := range []string{"nav_groups", "nav_items"} {
		if !known[n] {
			t.Errorf("KnownTables() 缺 %s —— 导航页的备份会无法恢复", n)
		}
	}
}
