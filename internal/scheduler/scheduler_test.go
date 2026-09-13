package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/store"
)

func TestParseCronBasic(t *testing.T) {
	c, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Minute) != 1 || c.Minute[0] != 0 {
		t.Fatalf("分钟解析错误: %v", c.Minute)
	}
	if len(c.Hour) != 1 || c.Hour[0] != 3 {
		t.Fatalf("小时解析错误: %v", c.Hour)
	}
	if len(c.Day) != 31 {
		t.Fatalf("日应为全范围，实际 %d 个值", len(c.Day))
	}
	if !c.IsDaily {
		t.Fatal("0 3 * * * 应被识别为每天执行")
	}
	if d := c.Describe(); !strings.Contains(d, "03:00") || !strings.Contains(d, "每天") {
		t.Fatalf("描述不自然: %s", d)
	}
}

func TestParseCronListsRangesAndSteps(t *testing.T) {
	cases := []struct {
		expr            string
		wantMin, wantHr int
	}{
		{"*/5 * * * *", 12, 24},    // 每 5 分钟
		{"0,30 * * * *", 2, 24},    // 每小时 0 与 30 分
		{"0 9,21 * * *", 1, 2},     // 每天 9 点与 21 点
		{"0-30/10 * * * *", 4, 24}, // 0,10,20,30
		{"15 1-5 * * *", 1, 5},     // 1-5 点
	}
	for _, tc := range cases {
		c, err := ParseCron(tc.expr)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", tc.expr, err)
		}
		if len(c.Minute) != tc.wantMin {
			t.Fatalf("%s：分钟应有 %d 个值，实际 %d（%v）", tc.expr, tc.wantMin, len(c.Minute), c.Minute)
		}
		if len(c.Hour) != tc.wantHr {
			t.Fatalf("%s：小时应有 %d 个值，实际 %d（%v）", tc.expr, tc.wantHr, len(c.Hour), c.Hour)
		}
	}
}

func TestParseCronWeekdayNames(t *testing.T) {
	c, err := ParseCron("0 3 * * mon-fri")
	if err != nil {
		t.Fatalf("英文缩写周几解析失败: %v", err)
	}
	want := []int{1, 2, 3, 4, 5}
	if len(c.Weekday) != len(want) {
		t.Fatalf("周几应有 %d 个值，实际 %v", len(want), c.Weekday)
	}
	for i, w := range want {
		if c.Weekday[i] != w {
			t.Fatalf("周几解析错误: %v", c.Weekday)
		}
	}
	if !strings.Contains(c.Describe(), "周一") {
		t.Fatalf("描述应包含周几: %s", c.Describe())
	}
}

// cron 里 7 也表示周日，必须归一化为 0（launchd 只认 0-6）。
func TestParseCronSundayNormalization(t *testing.T) {
	c, err := ParseCron("0 3 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Weekday) != 1 || c.Weekday[0] != 0 {
		t.Fatalf("周日的 7 应归一化为 0，实际 %v", c.Weekday)
	}
	// 0 与 7 同时出现时不应重复
	c2, err := ParseCron("0 3 * * 0,7")
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Weekday) != 1 {
		t.Fatalf("0 与 7 应视为同一天，实际 %v", c2.Weekday)
	}
}

func TestParseCronRejectsInvalid(t *testing.T) {
	bad := []string{
		"",              // 空
		"* * * *",       // 只有 4 段
		"* * * * * *",   // 6 段
		"60 * * * *",    // 分钟越界
		"* 24 * * *",    // 小时越界
		"* * 0 * *",     // 日从 1 开始，0 非法
		"* * * 13 *",    // 月份越界
		"* * * * 8",     // 周几越界
		"*/0 * * * *",   // 步长不能为 0
		"*/abc * * * *", // 步长非数字
		"5-1 * * * *",   // 区间颠倒
		"a * * * *",     // 非数字
		"1,,2 * * * *",  // 空项
	}
	for _, e := range bad {
		if _, err := ParseCron(e); err == nil {
			t.Fatalf("非法表达式未被拒绝: %q", e)
		}
	}
}

// NextRuns 必须真的按计划推算，且"每小时"这类要能算出多个点。
func TestNextRuns(t *testing.T) {
	from := time.Date(2026, 3, 15, 10, 0, 0, 0, time.Local)

	c, _ := ParseCron("0 3 * * *")
	runs := NextRuns(c, from, 3)
	if len(runs) != 3 {
		t.Fatalf("应算出 3 个执行时间，实际 %d", len(runs))
	}
	// 第一次应是次日 03:00（当天 3:00 已过）
	if runs[0].Hour() != 3 || runs[0].Minute() != 0 || runs[0].Day() != 16 {
		t.Fatalf("首次执行时间错误: %s", runs[0])
	}
	// 三次应相隔一天
	if runs[1].Sub(runs[0]) != 24*time.Hour {
		t.Fatalf("执行间隔应为 24 小时，实际 %v", runs[1].Sub(runs[0]))
	}

	// 每 5 分钟：接下来三个点应间隔 5 分钟
	c2, _ := ParseCron("*/5 * * * *")
	runs2 := NextRuns(c2, from, 3)
	if len(runs2) != 3 {
		t.Fatalf("应算出 3 个点，实际 %d", len(runs2))
	}
	if runs2[0].Minute()%5 != 0 {
		t.Fatalf("每 5 分钟的首次执行分钟应为 5 的倍数: %s", runs2[0])
	}
	if d := runs2[1].Sub(runs2[0]); d != 5*time.Minute {
		t.Fatalf("间隔应为 5 分钟，实际 %v", d)
	}
}

// 工作日判断：周末不应触发。
func TestNextRunsWeeklySkipsWeekend(t *testing.T) {
	// 2026-03-20 是周五
	from := time.Date(2026, 3, 20, 10, 0, 0, 0, time.Local)
	c, err := ParseCron("0 3 * * mon-fri")
	if err != nil {
		t.Fatal(err)
	}
	runs := NextRuns(c, from, 3)
	if len(runs) != 3 {
		t.Fatalf("应算出 3 个点，实际 %d", len(runs))
	}
	for _, r := range runs {
		wd := r.Weekday()
		if wd == time.Saturday || wd == time.Sunday {
			t.Fatalf("工作日计划不应落在周末: %s (%s)", r, wd)
		}
	}
}

// 每月 31 号：只有含 31 号的月份才该触发。
func TestNextRunsMonthDay31(t *testing.T) {
	from := time.Date(2026, 1, 31, 5, 0, 0, 0, time.Local)
	c, err := ParseCron("0 3 31 * *")
	if err != nil {
		t.Fatal(err)
	}
	runs := NextRuns(c, from, 3)
	if len(runs) < 2 {
		t.Fatalf("应能算出后续执行时间，实际 %d 个", len(runs))
	}
	for _, r := range runs {
		if r.Day() != 31 {
			t.Fatalf("应只在 31 号触发，实际 %s", r)
		}
	}
}

// 描述文本要能读懂。
func TestDescribe(t *testing.T) {
	cases := map[string][]string{
		"0 3 * * *":    {"03:00", "每天"},
		"*/5 * * * *":  {"每 5 分钟"},
		"0 * * * *":    {"每小时"},
		"0 9,21 * * *": {"09:00", "21:00"},
		"0 3 1 * *":    {"每月", "1"},
	}
	for expr, wants := range cases {
		c, err := ParseCron(expr)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", expr, err)
		}
		d := c.Describe()
		for _, w := range wants {
			if !strings.Contains(d, w) {
				t.Fatalf("%s 的描述 %q 应包含 %q", expr, d, w)
			}
		}
	}
}

// ---------------------------------------------------------------------------
//  plist 生成
// ---------------------------------------------------------------------------

func TestBuildPlistIsValidXML(t *testing.T) {
	j := &Job{
		Name: "daily backup", Kind: "shell",
		Schedule: "0 3 * * *",
		Command:  `echo "a & b" > /tmp/x; ls -la | grep 'x<y>'`,
		WorkDir:  "/Users/test/www",
		Enabled:  true,
	}
	c, err := ParseCron(j.Schedule)
	if err != nil {
		t.Fatal(err)
	}
	plist := j.BuildPlist(c, "/tmp/cron.log")

	// 必须包含关键结构
	for _, want := range []string{
		"<key>Label</key>", "cn.zizpanel.cron.daily-backup",
		"<key>StartCalendarInterval</key>", "<key>Minute</key><integer>0</integer>",
		"<key>Hour</key><integer>3</integer>",
		"<key>StandardOutPath</key>", "<key>StandardErrorPath</key>",
		"<key>WorkingDirectory</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist 缺少 %q\n%s", want, plist)
		}
	}
	// 命令里的特殊字符必须被转义，否则是非法 XML
	if strings.Contains(plist, `"a & b"`) {
		t.Fatal("& 未被转义，plist 会是非法 XML")
	}
	if !strings.Contains(plist, "&amp;") {
		t.Fatal("& 应被转义为 &amp;")
	}
	if !strings.Contains(plist, "&lt;y&gt;") {
		t.Fatal("< > 应被转义")
	}
	// 不应设 KeepAlive：定时任务执行完应退出，不该被反复拉起
	if strings.Contains(plist, "KeepAlive") {
		t.Fatal("定时任务不应设置 KeepAlive")
	}
}

// launchd 的日历条件数组必须与 cron 语义一致。
func TestCalendarIntervalsMapping(t *testing.T) {
	// 每天 3:00 → 恰好一个条件
	c, _ := ParseCron("0 3 * * *")
	j := &Job{Name: "x", Kind: "shell", Command: "true"}
	if got := len(j.calendarIntervals(c)); got != 1 {
		t.Fatalf("每天一次应生成 1 个条件，实际 %d", got)
	}
	// 9:00 与 21:00 → 两个条件
	c2, _ := ParseCron("0 9,21 * * *")
	if got := len(j.calendarIntervals(c2)); got != 2 {
		t.Fatalf("两个时间点应生成 2 个条件，实际 %d", got)
	}
	// 工作日 3:00 → 5 个条件（周一到周五）
	c3, _ := ParseCron("0 3 * * mon-fri")
	if got := len(j.calendarIntervals(c3)); got != 5 {
		t.Fatalf("工作日应生成 5 个条件，实际 %d", got)
	}
	// 每 5 分钟 → 12（分）× 24（时）= 288 个条件
	c4, _ := ParseCron("*/5 * * * *")
	got := len(j.calendarIntervals(c4))
	if got != 288 {
		t.Fatalf("每 5 分钟应生成 288 个条件，实际 %d", got)
	}
	// 组合爆炸必须被截断（否则 plist 会巨大）
	c5, _ := ParseCron("*/1 * * * *")
	if got := len(j.calendarIntervals(c5)); got > 400 {
		t.Fatalf("条件数应被限制在 400 以内，实际 %d", got)
	}
}

// 备份脚本必须包含所选范围，并做旧备份清理。
func TestBackupScriptContainsTargets(t *testing.T) {
	j := &Job{
		Name: "nightly", Kind: "backup", Schedule: "0 3 * * *",
		BackupTargets: []string{"sites", "mysql", "nginx", "panel"},
		BackupDir:     "/tmp/backups", KeepDays: 14,
	}
	s := j.backupScript()
	for _, want := range []string{
		"/tmp/backups", "sites.tar.gz", "mysql/all.sql",
		"nginx.tar.gz", "panel.tar.gz", "mtime +14",
		"set -uo pipefail", "trap",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("备份脚本缺少 %q\n%s", want, s)
		}
	}
	// 凭证必须从 .env.local 读取，不能硬编码在脚本里
	if strings.Contains(s, "PanelTestPw-9x!") {
		t.Fatal("备份脚本不应包含明文密码")
	}
	if !strings.Contains(s, ".env.local") {
		t.Fatal("应从未 .env.local 读取数据库密码")
	}
}

// ---------------------------------------------------------------------------
//  校验逻辑
// ---------------------------------------------------------------------------

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(st, Options{LogDir: t.TempDir()})
}

func TestValidateRejectsBadJobs(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	bad := []*Job{
		{Name: "", Schedule: "0 3 * * *", Command: "true"},                                  // 无名
		{Name: "x", Schedule: "bad cron", Command: "true"},                                  // 计划错误
		{Name: "x", Schedule: "0 3 * * *", Kind: "shell", Command: ""},                      // 命令为空
		{Name: "x", Schedule: "0 3 * * *", Kind: "url", Command: "ftp://x"},                 // URL 协议错误
		{Name: "x", Schedule: "0 3 * * *", Kind: "backup", BackupTargets: []string{"nope"}}, // 未知范围
	}
	for i, j := range bad {
		if err := m.validate(j); err == nil {
			t.Fatalf("第 %d 个非法任务未被拒绝: %+v", i+1, j)
		}
	}
	_ = ctx
}

func TestValidateAcceptsGoodJobs(t *testing.T) {
	m := newTestManager(t)
	good := []*Job{
		{Name: "nightly-backup", Schedule: "0 3 * * *", Kind: "backup"},
		{Name: "clear-cache", Schedule: "*/30 * * * *", Kind: "shell", Command: "rm -rf /tmp/x"},
		{Name: "ping-url", Schedule: "0 * * * *", Kind: "url", Command: "https://example.com/hook"},
	}
	for _, j := range good {
		if err := m.validate(j); err != nil {
			t.Fatalf("合法任务被拒绝: %+v (%v)", j, err)
		}
	}
	// 备份任务的默认值应被填上
	b := &Job{Name: "b", Schedule: "0 3 * * *", Kind: "backup"}
	if err := m.validate(b); err != nil {
		t.Fatal(err)
	}
	if len(b.BackupTargets) == 0 || b.BackupDir == "" || b.KeepDays <= 0 {
		t.Fatalf("备份任务应补齐默认值: %+v", b)
	}
}

// 纯中文名会生成无意义的 label，必须拒绝并给出提示。
func TestValidateRejectsNameWithoutAscii(t *testing.T) {
	m := newTestManager(t)
	j := &Job{Name: "每日备份", Schedule: "0 3 * * *", Kind: "backup"}
	if err := m.validate(j); err == nil {
		t.Fatal("纯中文名无法生成系统标识，应被拒绝")
	}
	// 中文 + 数字可以
	j2 := &Job{Name: "备份1", Schedule: "0 3 * * *", Kind: "backup"}
	if err := m.validate(j2); err != nil {
		t.Fatalf("含数字的中文名应可用: %v", err)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Daily Backup":   "daily-backup",
		"clear_cache.v1": "clear-cache-v1",
		"My  Job":        "my-job",
		"---":            "job",
		"备份1":            "1",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Fatalf("Slug(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestCommonSchedulesAreValid(t *testing.T) {
	for _, cs := range CommonSchedules() {
		if _, err := ParseCron(cs.Schedule); err != nil {
			t.Fatalf("预设计划 %q（%s）不合法: %v", cs.Label, cs.Schedule, err)
		}
	}
}

// 存储层往返测试。
func TestRepositoryRoundTrip(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	j := &Job{Name: "test-job", Kind: "shell", Schedule: "0 3 * * *",
		Command: "echo hi", WorkDir: "/tmp", Enabled: true}
	if err := m.repo.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	if j.ID == 0 {
		t.Fatal("创建后应拿到 ID")
	}
	got, err := m.repo.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != j.Name || got.Command != j.Command || !got.Enabled {
		t.Fatalf("往返不一致: %+v", got)
	}
	// 更新
	got.Command = "echo changed"
	got.Enabled = false
	if err := m.repo.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := m.repo.Get(ctx, j.ID)
	if got2.Command != "echo changed" || got2.Enabled {
		t.Fatalf("更新未生效: %+v", got2)
	}
	// 记录执行
	if err := m.repo.MarkRun(ctx, j.ID, "ok", "输出内容"); err != nil {
		t.Fatal(err)
	}
	got3, _ := m.repo.Get(ctx, j.ID)
	if got3.RunCount != 1 || got3.LastStatus != "ok" {
		t.Fatalf("执行记录未写入: %+v", got3)
	}
	// 删除
	if err := m.repo.Delete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.repo.Get(ctx, j.ID); err == nil {
		t.Fatal("删除后应查不到")
	}
}
