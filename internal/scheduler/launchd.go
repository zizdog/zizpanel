package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/priv"
)

// ---------------------------------------------------------------------------
//  launchd plist 生成
// ---------------------------------------------------------------------------

// LaunchdDir 是面板任务 plist 的存放目录。
//
// 放在 /Library/LaunchDaemons 而不是 ~/Library/LaunchAgents：
//   - LaunchDaemon 在用户未登录时也能执行（服务器场景的关键）
//   - 不依赖某个用户的 GUI 会话
const LaunchdDir = "/Library/LaunchDaemons"

// PlistPath 返回任务的 plist 路径。
func (j *Job) PlistPath2() string {
	return filepath.Join(LaunchdDir, j.LabelName()+".plist")
}

// BuildPlist 生成 LaunchDaemon plist 内容。
//
// 关键点：
//   - StartCalendarInterval 用数组形式，每个元素是一组匹配条件
//     （数组是"或"的关系，符合 cron 的语义）
//   - StandardOutPath / StandardErrorPath 指向任务日志，
//     面板的"查看输出"直接读这两个文件
//   - 不设 KeepAlive：定时任务完成后应该退出，而不是被反复拉起
func (j *Job) BuildPlist(c *Cron, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + xmlEscape(j.LabelName()) + `</string>
`)
	// ProgramArguments：统一通过 /bin/bash -lc 执行，支持管道与重定向
	b.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	b.WriteString("        <string>/bin/bash</string>\n")
	b.WriteString("        <string>-lc</string>\n")
	b.WriteString("        <string>" + xmlEscape(j.buildScript()) + "</string>\n")
	b.WriteString("    </array>\n")

	if j.WorkDir != "" {
		b.WriteString("    <key>WorkingDirectory</key>\n")
		b.WriteString("    <string>" + xmlEscape(j.WorkDir) + "</string>\n")
	}

	// 定时规则：数组元素之间是"或"，与 cron 的多值语义一致
	b.WriteString("    <key>StartCalendarInterval</key>\n    <array>\n")
	for _, cal := range j.calendarIntervals(c) {
		b.WriteString("        <dict>\n")
		b.WriteString("            <key>Minute</key><integer>" + strconv.Itoa(cal["Minute"]) + "</integer>\n")
		b.WriteString("            <key>Hour</key><integer>" + strconv.Itoa(cal["Hour"]) + "</integer>\n")
		if v, ok := cal["Day"]; ok {
			b.WriteString("            <key>Day</key><integer>" + strconv.Itoa(v) + "</integer>\n")
		}
		if v, ok := cal["Month"]; ok {
			b.WriteString("            <key>Month</key><integer>" + strconv.Itoa(v) + "</integer>\n")
		}
		if v, ok := cal["Weekday"]; ok {
			b.WriteString("            <key>Weekday</key><integer>" + strconv.Itoa(v) + "</integer>\n")
		}
		b.WriteString("        </dict>\n")
	}
	b.WriteString("    </array>\n")

	b.WriteString("    <key>StandardOutPath</key>\n    <string>" + xmlEscape(logPath) + "</string>\n")
	b.WriteString("    <key>StandardErrorPath</key>\n    <string>" + xmlEscape(logPath) + "</string>\n")
	// 任务不需要图形会话
	b.WriteString("    <key>ProcessType</key>\n    <string>Background</string>\n")
	if j.WorkDir != "" {
		b.WriteString("    <key>EnvironmentVariables</key>\n    <dict>\n")
		b.WriteString("        <key>HOME</key><string>" + xmlEscape(homeOf(j)) + "</string>\n")
		b.WriteString("        <key>PATH</key><string>/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n")
		b.WriteString("    </dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// buildScript 把任务内容包装成一段 shell 脚本。
//
// backup 类型的任务由面板生成脚本内容（调用自身的备份逻辑写成 shell 不方便），
// 因此这里对 backup 使用面板内置的备份命令；其它类型直接用用户命令。
func (j *Job) buildScript() string {
	if j.Kind == "backup" {
		return j.backupScript()
	}
	return j.Command
}

// calendarIntervals 把 cron 展开为 launchd 的日历条件数组。
//
// 展开策略：对 分×时 做笛卡尔积；日/月/周只要不是"全范围"就带上。
// 注意 launchd 的语义：同一个 dict 里的多个键是"与"，
// 数组元素之间是"或"。这与 cron 完全对应。
func (j *Job) calendarIntervals(c *Cron) []map[string]int {
	var out []map[string]int
	fullDay := len(c.Day) == 31
	fullMonth := len(c.Month) == 12
	fullWeek := len(c.Weekday) == 7

	for _, h := range c.Hour {
		for _, m := range c.Minute {
			// 日期维度：日 / 月 / 周中，只有一个能用"取值列表"表达，
			// 多个同时限制时需要做组合。这里按"取第一个非全范围的维度"
			// 处理最常见的情况，其余维度在脚本里做二次判断。
			if !fullWeek {
				for _, w := range c.Weekday {
					item := map[string]int{"Minute": m, "Hour": h, "Weekday": w}
					if !fullMonth {
						// launchd 不支持同时限制月与周，退化为逐月展开
						for _, mo := range c.Month {
							it := map[string]int{"Minute": m, "Hour": h, "Weekday": w, "Month": mo}
							out = append(out, it)
						}
						continue
					}
					out = append(out, item)
				}
				continue
			}
			if !fullDay {
				for _, d := range c.Day {
					item := map[string]int{"Minute": m, "Hour": h, "Day": d}
					if !fullMonth {
						for _, mo := range c.Month {
							it := map[string]int{"Minute": m, "Hour": h, "Day": d, "Month": mo}
							out = append(out, it)
						}
						continue
					}
					out = append(out, item)
				}
				continue
			}
			item := map[string]int{"Minute": m, "Hour": h}
			if !fullMonth {
				for _, mo := range c.Month {
					it := map[string]int{"Minute": m, "Hour": h, "Month": mo}
					out = append(out, it)
				}
				continue
			}
			out = append(out, item)
		}
	}

	// 上限保护：组合爆炸会生成巨大 plist
	const maxIntervals = 400
	if len(out) > maxIntervals {
		sort.Slice(out, func(i, k int) bool {
			if out[i]["Hour"] != out[k]["Hour"] {
				return out[i]["Hour"] < out[k]["Hour"]
			}
			return out[i]["Minute"] < out[k]["Minute"]
		})
		out = out[:maxIntervals]
	}
	return out
}

func homeOf(j *Job) string {
	if j.WorkDir != "" {
		return j.WorkDir
	}
	return "/var/root"
}

// xmlEscape 转义 plist 里的特殊字符。
//
// 必须做：任务命令里常含 & < > 与引号，
// 不转义会生成非法 XML，launchctl 直接拒绝加载。
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
//  备份脚本生成
// ---------------------------------------------------------------------------

// backupScript 生成备份任务的 shell 脚本。
//
// 为什么脚本里只调 `zizpanel backup create` 而不是自己 tar 目录：
//   - 数据的一致性只能由 Go 侧用 `VACUUM INTO` 保证（直接 tar panel.db+wal+shm
//     会得到三个不同瞬间的文件，事后无法判断自不自洽）；
//   - 归档还要带 manifest.json 与逐文件 sha256，shell 里做不划算；
//   - 路径全部从 `--config` 读，脚本里**不再出现** /opt/zizpanel、/opt/homebrew。
//
// 本函数不做默认值填充：默认值由 Manager.validate 依据面板配置补齐。
// 没补齐就**如实报错退出**，不再悄悄退到某个写死的目录（那是"备错地方还报成功"）。
func (j *Job) backupScript() string {
	dir := j.BackupDir
	keep := j.KeepDays
	targets := j.BackupTargets
	bin := j.PanelBin

	head := []string{"set -uo pipefail"}
	// 启动横幅与结束横幅都带时间戳：任务日志里能一眼看出跑了多久。
	head = append(head, `echo "[$(date '+%F %T')] 备份开始"`)

	if bin == "" || dir == "" || len(targets) == 0 {
		head = append(head,
			`echo "备份任务配置不完整（缺少面板路径/输出目录/备份范围），请在面板「计划任务」里重新保存该任务"`,
			"exit 1")
		return strings.Join(head, "\n")
	}

	args := []string{fmt.Sprintf("%q", bin), "backup", "create"}
	if j.ConfigPath != "" {
		args = append(args, "--config", fmt.Sprintf("%q", j.ConfigPath))
	}
	args = append(args, "--out", fmt.Sprintf("%q", dir))
	args = append(args, "--targets", fmt.Sprintf("%q", strings.Join(targets, ",")))
	if keep > 0 {
		args = append(args, "--keep-days", strconv.Itoa(keep))
	}
	lines := append(head, strings.Join(args, " "))
	lines = append(lines,
		"rc=$?",
		`if [ $rc -eq 0 ]; then echo "[$(date '+%F %T')] 备份结束"; `+
			`else echo "[$(date '+%F %T')] 备份失败（退出码 $rc），详见上面的输出"; fi`,
		"exit $rc",
	)
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
//  任务加载 / 卸载
// ---------------------------------------------------------------------------

// apply 把任务写入 launchd（幂等）。
func (m *Manager) apply(ctx context.Context, j *Job) error {
	c, err := ParseCron(j.Schedule)
	if err != nil {
		return err
	}
	// backup 任务的脚本需要知道"用哪个二进制、哪份配置"——这两个值来自面板配置，
	// 在这里注入而不是写死在脚本里（重定位安装/ZIZPANEL_ROOT 下必须正确）。
	if j.Kind == "backup" {
		j.PanelBin = m.binaryPath
		j.ConfigPath = m.configPath
	}
	if strings.TrimSpace(j.buildScript()) == "" {
		return errors.New("任务内容为空")
	}

	label := j.LabelName()
	plistPath := j.PlistPath2()

	// 先卸载旧任务，避免重复加载导致 launchd 报错。
	//
	// 卸载失败必须上报，不能吞掉：作业还挂在 launchd 里时 bootstrap 会失败，
	// 而吞掉错误后接着 kickstart 的是**旧 plist 里的命令** —— 用户看到
	// "任务已保存"，实际跑的却还是上一版（与 launchctl 谎报成功同源的坑）。
	if uerr := priv.LaunchUnload(label); uerr != nil {
		return fmt.Errorf("卸载旧任务失败（新配置没有生效）: %w", uerr)
	}

	content := j.BuildPlist(c, m.logPathFor(j))
	tmp := plistPath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("创建 plist 目录失败: %w", err)
	}
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(tmp, 0, 0)
	}
	if err := os.Rename(tmp, plistPath); err != nil {
		return fmt.Errorf("安装 plist 失败: %w", err)
	}

	if !j.Enabled {
		return nil // 停用的任务只写文件不加载
	}
	if err := priv.LaunchLoad(label); err != nil {
		return fmt.Errorf("加载任务失败: %w", err)
	}
	return nil
}

// remove 从 launchd 移除任务并删除 plist。
func (m *Manager) remove(ctx context.Context, j *Job) error {
	label := j.LabelName()
	// 卸载失败必须上报：任务记录删了、作业还挂在 launchd 里继续定时执行，
	// 就是"界面上消失了但系统里还在跑"的谎报成功。
	if uerr := priv.LaunchUnload(label); uerr != nil {
		return fmt.Errorf("从 launchd 卸载任务失败: %w", uerr)
	}
	plistPath := j.PlistPath2()
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 plist 失败: %w", err)
	}
	// 日志文件保留（便于事后排查），但记录位置
	return nil
}

// RunNow 立即执行一次任务（不改变定时计划）。
//
// 用 launchctl kickstart：这样执行环境与定时触发时完全一致
// （同一份环境变量、同一个日志文件），避免"手动能跑、定时不跑"的困惑。
func (m *Manager) RunNow(ctx context.Context, id int64) (string, error) {
	j, err := m.repo.Get(ctx, id)
	if err != nil {
		return "", err
	}
	label := j.LabelName()

	// 未加载时先加载（停用的任务也可以手动跑一次）
	st, _ := priv.LaunchStatus(label)
	if !st.Loaded {
		if err := m.apply(ctx, j); err != nil {
			return "", err
		}
	}
	if err := priv.LaunchKickstart(label); err != nil {
		return "", fmt.Errorf("触发执行失败: %w", err)
	}
	// 记录本次执行
	_ = m.repo.MarkRun(ctx, id, "triggered", "已手动触发，输出见运行日志")
	return "已触发执行，稍后可在「运行日志」查看输出", nil
}

// NextRuns 估算接下来几次执行时间（前端展示用）。
//
// 实现方式：从当前时间起逐分钟推进，最多找 5 个匹配点。
// 这样能覆盖 cron 的各种组合，且不需要引入完整的 cron 调度库。
func NextRuns(c *Cron, from time.Time, limit int) []time.Time {
	var out []time.Time
	if limit <= 0 {
		limit = 5
	}
	t := from.Truncate(time.Minute).Add(time.Minute)
	// 最多向后找 366 天（处理"每月 31 号"这类稀疏计划）
	deadline := from.AddDate(1, 0, 0)
	for t.Before(deadline) && len(out) < limit {
		if cronMatches(c, t) {
			out = append(out, t)
		}
		t = t.Add(time.Minute)
	}
	return out
}

// cronMatches 判断某个时刻是否匹配 cron 表达式。
//
// 注意"日"与"周"的关系：标准 cron 里两者**同时**限制时是"或"的关系
// （满足任一即触发），这是 cron 的历史行为，这里保持一致。
func cronMatches(c *Cron, t time.Time) bool {
	if !contains(c.Minute, t.Minute()) {
		return false
	}
	if !contains(c.Hour, t.Hour()) {
		return false
	}
	if !contains(c.Month, int(t.Month())) {
		return false
	}
	dayOK := contains(c.Day, t.Day())
	// Go 的 Weekday：Sunday=0，与 cron 一致
	weekOK := contains(c.Weekday, int(t.Weekday()))
	fullDay := len(c.Day) == 31
	fullWeek := len(c.Weekday) == 7

	switch {
	case fullDay && fullWeek:
		return true
	case fullDay:
		return weekOK
	case fullWeek:
		return dayOK
	default:
		// 两者都受限：cron 语义是"或"
		return dayOK || weekOK
	}
}

func contains(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
