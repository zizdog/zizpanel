// Package scheduler 实现计划任务（定时脚本 / 备份 / URL 调用）。
//
// 实现机制选择：**launchd 的 StartCalendarInterval**，不用 crontab。
//
// 为什么不用 crontab：
//   - macOS 上 crontab 受 SIP 与 Full Disk Access 限制，
//     面板进程改 crontab 经常静默失败（任务不执行也不报错）
//   - crontab 在机器睡眠期间错过的任务**不会补跑**；
//     launchd 会在唤醒后补跑错过的 StartCalendarInterval 任务
//   - launchd 的日志、退出码、加载状态都能查询，
//     crontab 出问题时几乎无从排查
//
// 每个任务生成一个 LaunchDaemon plist（系统级，不依赖用户登录），
// 因此面板重启、用户注销都不会影响任务执行。
package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Job 是一条计划任务。
type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`     // shell / backup / url
	Schedule   string `json:"schedule"` // 5 段 cron
	Command    string `json:"command"`
	WorkDir    string `json:"work_dir"`
	Enabled    bool   `json:"enabled"`
	LastRun    string `json:"last_run"`
	LastStatus string `json:"last_status"`
	LastOutput string `json:"last_output"`
	RunCount   int    `json:"run_count"`
	CreatedAt  string `json:"created_at"`

	// 运行时信息（不入库）
	Label       string `json:"label"`
	PlistPath   string `json:"plist_path"`
	Loaded      bool   `json:"loaded"`
	NextRunHint string `json:"next_run_hint"`

	// backup 专用
	BackupTargets []string `json:"backup_targets"`
	BackupDir     string   `json:"backup_dir"`
	KeepDays      int      `json:"keep_days"`
}

// LabelPrefix 是所有面板任务 label 的前缀。
//
// 统一前缀便于：
//   - 在 launchctl 列表里一眼区分哪些是面板创建的
//   - 卸载时精确清理，不会误删用户自己的服务
const LabelPrefix = "cn.zizpanel.cron."

// Label 返回任务的 launchd label。
func (j *Job) LabelName() string {
	return LabelPrefix + Slug(j.Name)
}

// Slug 把任务名转成 label 安全的片段。
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		out = "job"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// ---------------------------------------------------------------------------
//  cron 表达式解析
// ---------------------------------------------------------------------------

// Cron 是一条解析后的 5 段 cron 表达式。
//
// 字段含义（与标准 cron 一致）：
//
//	分 0-59 | 时 0-23 | 日 1-31 | 月 1-12 | 周 0-7（0 与 7 都是周日）
type Cron struct {
	Minute     []int
	Hour       []int
	Day        []int
	Month      []int
	Weekday    []int
	IsDaily    bool // 每天固定时间（用于生成更自然的描述）
	Normalized string
}

// fieldRange 描述一个字段的取值范围。
type fieldRange struct {
	min, max int
	name     string
	// names 支持周几的英文缩写（mon/tue/...）
	names map[string]int
}

var (
	fieldMinute  = fieldRange{0, 59, "分钟", nil}
	fieldHour    = fieldRange{0, 23, "小时", nil}
	fieldDay     = fieldRange{1, 31, "日", nil}
	fieldMonth   = fieldRange{1, 12, "月", nil}
	fieldWeekday = fieldRange{0, 7, "星期", map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

// ParseCron 解析 5 段 cron 表达式。
//
// 支持：`*`、`5`、`1,3,5`、`9-17`、`*/15`、`0-30/5`，
// 以及周几的英文缩写（mon-fri）。
func ParseCron(expr string) (*Cron, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 表达式需要 5 段（分 时 日 月 周），当前有 %d 段", len(fields))
	}
	minute, err := expandField(fields[0], fieldMinute)
	if err != nil {
		return nil, err
	}
	hour, err := expandField(fields[1], fieldHour)
	if err != nil {
		return nil, err
	}
	day, err := expandField(fields[2], fieldDay)
	if err != nil {
		return nil, err
	}
	month, err := expandField(fields[3], fieldMonth)
	if err != nil {
		return nil, err
	}
	weekday, err := expandField(fields[4], fieldWeekday)
	if err != nil {
		return nil, err
	}
	// 归一化 7 → 0（周日）
	weekday = normalizeWeekday(weekday)

	c := &Cron{
		Minute: minute, Hour: hour, Day: day, Month: month, Weekday: weekday,
		Normalized: strings.Join(fields, " "),
	}
	c.IsDaily = len(day) == 31 && len(month) == 12 && isAllDaysOfWeek(weekday)
	return c, nil
}

func isAllDaysOfWeek(w []int) bool {
	if len(w) != 7 {
		return false
	}
	seen := map[int]bool{}
	for _, v := range w {
		seen[v] = true
	}
	for i := 0; i < 7; i++ {
		if !seen[i] {
			return false
		}
	}
	return true
}

func normalizeWeekday(w []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range w {
		if v == 7 {
			v = 0
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// expandField 展开一个 cron 字段为具体值列表。
func expandField(f string, r fieldRange) ([]int, error) {
	f = strings.TrimSpace(f)
	if f == "" {
		return nil, fmt.Errorf("%s 字段为空", r.name)
	}
	var out []int
	seen := map[int]bool{}
	add := func(v int) error {
		if v < r.min || v > r.max {
			return fmt.Errorf("%s 值 %d 超出范围 %d-%d", r.name, v, r.min, r.max)
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
		return nil
	}

	for _, part := range strings.Split(f, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%s 字段含空的取值项", r.name)
		}

		// 步长：*/N 或 A-B/N
		step := 1
		base := part
		if i := strings.Index(part, "/"); i >= 0 {
			base = strings.TrimSpace(part[:i])
			stepStr := strings.TrimSpace(part[i+1:])
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s 的步长不合法: %q", r.name, stepStr)
			}
			step = n
		}

		// 通配
		if base == "*" {
			for v := r.min; v <= r.max; v += step {
				if err := add(v); err != nil {
					return nil, err
				}
			}
			continue
		}

		// 区间
		if i := strings.Index(base, "-"); i > 0 {
			loStr := strings.TrimSpace(base[:i])
			hiStr := strings.TrimSpace(base[i+1:])
			lo, err := parseValue(loStr, r)
			if err != nil {
				return nil, err
			}
			hi, err := parseValue(hiStr, r)
			if err != nil {
				return nil, err
			}
			if lo > hi {
				return nil, fmt.Errorf("%s 的区间起止颠倒: %s", r.name, base)
			}
			for v := lo; v <= hi; v += step {
				if err := add(v); err != nil {
					return nil, err
				}
			}
			continue
		}

		// 单值
		v, err := parseValue(base, r)
		if err != nil {
			return nil, err
		}
		if err := add(v); err != nil {
			return nil, err
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s 字段没有解析出任何有效值", r.name)
	}
	sort.Ints(out)
	return out, nil
}

func parseValue(s string, r fieldRange) (int, error) {
	if r.names != nil {
		if v, ok := r.names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s 的值不合法: %q", r.name, s)
	}
	return v, nil
}

// Describe 把 cron 表达式翻译成中文描述（前端展示用）。
func (c *Cron) Describe() string {
	var parts []string

	// 时间部分
	allHours := len(c.Hour) == 24
	allMinutes := len(c.Minute) == 60
	minuteStep, minuteIsStep := uniformStep(c.Minute)
	hourStep, hourIsStep := uniformStep(c.Hour)

	switch {
	case allMinutes && allHours:
		parts = append(parts, "每分钟")
	case allHours && minuteIsStep:
		// 形如 */5 * * * *：每 N 分钟
		parts = append(parts, fmt.Sprintf("每 %d 分钟", minuteStep))
	case allHours && len(c.Minute) == 1:
		parts = append(parts, fmt.Sprintf("每小时第 %d 分钟", c.Minute[0]))
	case hourIsStep && minuteIsStep && len(c.Minute) == 1:
		parts = append(parts, fmt.Sprintf("每 %d 小时（第 %d 分钟）", hourStep, c.Minute[0]))
	case len(c.Minute) > 0 && len(c.Minute) <= 6 && len(c.Hour) > 0 && len(c.Hour) <= 6:
		var times []string
		for _, h := range c.Hour {
			for _, m := range c.Minute {
				times = append(times, fmt.Sprintf("%02d:%02d", h, m))
			}
		}
		sort.Strings(times)
		parts = append(parts, strings.Join(times, "、"))
	default:
		parts = append(parts, fmt.Sprintf("分=%s 时=%s", intsToStr(c.Minute), intsToStr(c.Hour)))
	}

	// 日期部分
	if c.IsDaily {
		parts = append(parts, "每天")
	} else {
		if len(c.Weekday) > 0 && len(c.Weekday) < 7 {
			names := []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
			var ns []string
			for _, w := range c.Weekday {
				if w >= 0 && w < len(names) {
					ns = append(ns, names[w])
				}
			}
			parts = append(parts, strings.Join(ns, "、"))
		}
		if len(c.Day) < 31 {
			parts = append(parts, fmt.Sprintf("每月 %s 号", intsToStr(c.Day)))
		}
		if len(c.Month) < 12 {
			parts = append(parts, fmt.Sprintf("%s 月", intsToStr(c.Month)))
		}
	}
	return strings.Join(parts, " ")
}

// uniformStep 判断一组值是否为"从 0 开始、等间隔"的序列，
// 并返回步长。用于把 */5 这类表达式翻译成"每 5 分钟"。
func uniformStep(v []int) (int, bool) {
	if len(v) < 2 || v[0] != 0 {
		return 0, false
	}
	step := v[1] - v[0]
	if step <= 0 {
		return 0, false
	}
	for i := 1; i < len(v); i++ {
		if v[i]-v[i-1] != step {
			return 0, false
		}
	}
	return step, true
}

func intsToStr(v []int) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = strconv.Itoa(x)
	}
	if len(s) > 8 {
		return strings.Join(s[:8], ",") + "…"
	}
	return strings.Join(s, ",")
}

// ParseCronErr 便于上层判断是否为解析错误。
var ParseCronErr = errors.New("cron 表达式不合法")

// ValidateCron 校验表达式并返回友好错误。
func ValidateCron(expr string) error {
	if _, err := ParseCron(expr); err != nil {
		return fmt.Errorf("%w: %v", ParseCronErr, err)
	}
	return nil
}

// CommonSchedules 是常用预设（前端下拉）。
type CommonSchedule struct {
	Label    string `json:"label"`
	Schedule string `json:"schedule"`
}

// CommonSchedules 返回常用计划。
func CommonSchedules() []CommonSchedule {
	return []CommonSchedule{
		{Label: "每天凌晨 3:00", Schedule: "0 3 * * *"},
		{Label: "每天凌晨 2:30", Schedule: "30 2 * * *"},
		{Label: "每小时", Schedule: "0 * * * *"},
		{Label: "每 5 分钟", Schedule: "*/5 * * * *"},
		{Label: "每周一凌晨 3:00", Schedule: "0 3 * * 1"},
		{Label: "每月 1 号凌晨 3:00", Schedule: "0 3 1 * *"},
		{Label: "每天 9:00 与 21:00", Schedule: "0 9,21 * * *"},
	}
}
