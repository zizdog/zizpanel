// Package logs 实现日志中心：聚合、检索、实时尾随、轮转。
//
// 设计取舍：
//
//  1. **动态发现而不是硬编码清单**。已知目录（站点日志、面板日志、任务日志、
//     PHP/MySQL 日志）用"扫描 + 分类"的方式列出，这样新增站点或任务后
//     日志会自动出现，不需要改代码。只有少数固定路径（系统日志）写死在目录里。
//
//  2. **读取用 tail 而不是全量**。php-fpm.log 这类文件动辄几十上百 MB，
//     全量读会瞬间吃掉大量内存。读取上限同时限制行数与字节数。
//
//  3. **实时尾随用轮询偏移量**，与服务的日志流实现一致：
//     简单、可取消，且天然支持日志轮转（文件被替换时检测到 size 变小就回到开头）。
//
//  4. **轮转（truncate）需要显式调用**。日志是排障依据，
//     清理必须由用户主动触发，不能在后台自动删。
package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Entry 是一份日志文件的描述。
type Entry struct {
	// Key 是稳定标识，用于接口寻址（避免把绝对路径暴露给前端）
	Key string `json:"key"`
	// Name 展示名
	Name string `json:"name"`
	// Category 分类：nginx / site / php / mysql / panel / cron / service / system
	Category string `json:"category"`
	// Path 实际路径
	Path string `json:"path"`
	Size int64  `json:"size"`
	// ModTime 最后修改时间
	ModTime string `json:"mod_time"`
	// Exists 为 false 表示该日志尚未生成
	Exists bool `json:"exists"`
	// Available 为 false 表示当前无权限读取
	Available bool `json:"available"`
	// Rotatable 表示支持清空（面板自身产生的日志）
	Rotatable bool `json:"rotatable"`
	// Description 补充说明
	Description string `json:"description"`
}

// CategoryInfo 是分类元信息。
type CategoryInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

// Catalog 负责发现与读取日志。
type Catalog struct {
	// dirs 是动态扫描的目录集合
	siteLogDir  string
	panelLogDir string
	cronLogDir  string
	// extra 是额外的固定日志（系统日志、PHP/MySQL 等）
	extra []Entry
	// serviceLogs 是纳管服务的日志路径（由调用方注入）
	serviceLogs []Entry
}

// Options 是构造参数。
type Options struct {
	SiteLogDir  string
	PanelLogDir string
	CronLogDir  string
	BrewPrefix  string
	// ServiceLogs 是可选的额外日志条目（例如纳管服务的 plist 日志）
	ServiceLogs []Entry
}

// NewCatalog 创建日志目录。
func NewCatalog(opt Options) *Catalog {
	c := &Catalog{
		siteLogDir:  opt.SiteLogDir,
		panelLogDir: opt.PanelLogDir,
		cronLogDir:  opt.CronLogDir,
		serviceLogs: opt.ServiceLogs,
	}
	brew := opt.BrewPrefix
	if brew == "" {
		brew = "/opt/homebrew"
	}
	// 固定位置：这些日志的位置由各自软件决定，不做扫描
	c.extra = []Entry{
		{
			Key: "php-fpm", Name: "PHP-FPM 进程日志", Category: "php",
			Path:        filepath.Join(brew, "var", "log", "php-fpm.log"),
			Rotatable:   false,
			Description: "PHP 进程池的启停与错误记录。文件增长较快，可在终端用 truncate 清理。",
		},
		{
			Key: "mysql-error", Name: "MySQL 错误日志", Category: "mysql",
			Path:        filepath.Join(brew, "var", "mysql", hostName()+".err"),
			Description: "MySQL 启动、崩溃与警告信息。",
		},
		{
			Key: "nginx-global-error", Name: "nginx 主错误日志", Category: "nginx",
			Path:        filepath.Join(brew, "var", "log", "nginx", "error.log"),
			Description: "nginx 全局错误日志（站点错误在各自的站点日志里）。",
		},
	}
	return c
}

func hostName() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	return h
}

// Categories 返回分类定义（用于前端分组）。
func Categories() []CategoryInfo {
	return []CategoryInfo{
		{"nginx", "nginx", "🌐"},
		{"site", "站点访问/错误日志", "📄"},
		{"php", "PHP", "🐘"},
		{"mysql", "MySQL", "🗄️"},
		{"panel", "面板", "🛡️"},
		{"cron", "计划任务", "⏰"},
		{"service", "纳管服务", "⚙️"},
		{"system", "系统", "🖥️"},
	}
}

// List 返回所有已知日志（含未生成的，便于用户知道去哪找）。
func (c *Catalog) List() []Entry {
	var out []Entry

	// 1) 站点日志：按 nginx 的 *_logs 目录扫描
	//    命名约定：<域名>.access.log / <域名>.error.log
	if c.siteLogDir != "" {
		out = append(out, c.scanDir(c.siteLogDir, "site", "")...)
	}

	// 2) 面板日志：panel-YYYYMMDD.log + launchd 输出
	if c.panelLogDir != "" {
		out = append(out, c.scanDir(c.panelLogDir, "panel", "面板")...)
	}

	// 3) 任务日志：每个任务一个 <label>.log
	if c.cronLogDir != "" {
		out = append(out, c.scanDir(c.cronLogDir, "cron", "任务")...)
	}

	// 4) 固定位置的日志
	for i := range c.extra {
		e := c.extra[i]
		c.fill(&e)
		out = append(out, e)
	}

	// 5) 纳管服务日志（由调用方注入）
	for i := range c.serviceLogs {
		e := c.serviceLogs[i]
		c.fill(&e)
		out = append(out, e)
	}

	// 排序：分类顺序固定，同类内按修改时间倒序（最近有动静的排前面）
	catOrder := map[string]int{}
	for i, ci := range Categories() {
		catOrder[ci.Key] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := catOrder[out[i].Category], catOrder[out[j].Category]
		if oi != oj {
			return oi < oj
		}
		return out[i].ModTime > out[j].ModTime
	})
	return out
}

// scanDir 扫描一个目录并把文件登记为日志条目。
func (c *Catalog) scanDir(dir, category, prefix string) []Entry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		// 只收 .log 结尾的文件，避免把配置或数据文件混进来
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		key := category + ":" + name
		e := Entry{
			Key:      key,
			Name:     displayName(name, category, prefix),
			Category: category,
			Path:     filepath.Join(dir, name),
			// 面板与任务日志由面板自己产生，支持清空
			Rotatable: category == "panel" || category == "cron",
		}
		c.fill(&e)
		out = append(out, e)
	}
	return out
}

// displayName 生成友好的展示名。
func displayName(name, category, prefix string) string {
	base := strings.TrimSuffix(name, ".log")
	switch category {
	case "site":
		// zizdog.cn.access.log → zizdog.cn · 访问日志
		if strings.HasSuffix(base, ".access") {
			return strings.TrimSuffix(base, ".access") + " · 访问日志"
		}
		if strings.HasSuffix(base, ".error") {
			return strings.TrimSuffix(base, ".error") + " · 错误日志"
		}
	case "panel":
		if strings.HasPrefix(base, "panel-") {
			d := strings.TrimPrefix(base, "panel-")
			if len(d) == 8 {
				return fmt.Sprintf("面板日志 %s-%s-%s", d[:4], d[4:6], d[6:])
			}
			return "面板日志 " + d
		}
		return "launchd 输出（" + base + "）"
	case "cron":
		// cn.zizpanel.cron.xxx → 任务：xxx
		if strings.HasPrefix(base, "cn.zizpanel.cron.") {
			return "任务：" + strings.TrimPrefix(base, "cn.zizpanel.cron.")
		}
	}
	if prefix != "" {
		return prefix + " " + base
	}
	return base
}

// fill 补齐文件的实时信息。
func (c *Catalog) fill(e *Entry) {
	st, err := os.Stat(e.Path)
	if err != nil {
		e.Exists = false
		e.Available = false
		return
	}
	e.Exists = true
	e.Size = st.Size()
	e.ModTime = st.ModTime().Format("2006-01-02 15:04:05")
	// 探测可读性：以只读方式打开一次，避免列表里显示"可读"但点击才报错
	f, err := os.Open(e.Path)
	if err != nil {
		e.Available = false
		return
	}
	_ = f.Close()
	e.Available = true
}

// Find 按 key 查找日志条目。
func (c *Catalog) Find(key string) (Entry, error) {
	for _, e := range c.List() {
		if e.Key == key {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("未找到日志：%s", key)
}

// ---------------------------------------------------------------------------
//  读取
// ---------------------------------------------------------------------------

// ReadOptions 控制读取行为。
type ReadOptions struct {
	// Lines 读取末尾行数（默认 300）
	Lines int
	// Filter 是关键字过滤（子串匹配，大小写不敏感）
	Filter string
	// IsRegex 为 true 时 Filter 按正则解释
	IsRegex bool
	// Level 过滤日志级别（error / warn / info），仅对可识别的行生效
	Level string
	// MaxBytes 读取上限（默认 4MB）
	MaxBytes int64
}

// ReadResult 是读取结果。
type ReadResult struct {
	Key       string   `json:"key"`
	Path      string   `json:"path"`
	Lines     []string `json:"lines"`
	Total     int      `json:"total"`     // 文件总行数（近似）
	Matched   int      `json:"matched"`   // 过滤后剩余行数
	Truncated bool     `json:"truncated"` // 是否因大小上限截断
	Size      int64    `json:"size"`
	ModTime   string   `json:"mod_time"`
}

// Read 读取日志内容。
//
// 读取策略：从文件末尾往前读最多 MaxBytes，切成行后再做过滤与截断。
// 从末尾读是关键 —— 排障关心的是"刚刚发生了什么"。
func (c *Catalog) Read(key string, opt ReadOptions) (*ReadResult, error) {
	e, err := c.Find(key)
	if err != nil {
		return nil, err
	}
	if !e.Exists {
		return &ReadResult{Key: key, Path: e.Path, Lines: []string{}}, nil
	}
	if !e.Available {
		return nil, fmt.Errorf("没有权限读取该日志：%s", e.Path)
	}
	if opt.Lines <= 0 {
		opt.Lines = 300
	}
	if opt.Lines > 5000 {
		opt.Lines = 5000
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = 4 << 20
	}

	f, err := os.Open(e.Path)
	if err != nil {
		return nil, fmt.Errorf("打开日志失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	start := int64(0)
	truncated := false
	if size > opt.MaxBytes {
		start = size - opt.MaxBytes
		truncated = true
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, err
	}

	// 统计总行数（只扫已读取的区间，避免为了精确数字读全文件）
	var all []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil, err
	}
	// 从中间开始读时，第一行可能被截断
	if start > 0 && len(all) > 0 {
		all = all[1:]
	}
	total := len(all)

	// 先过滤，再取末尾 N 行
	filtered := all
	if opt.Filter != "" || opt.Level != "" {
		filtered = filterLines(all, opt)
	}
	matched := len(filtered)
	if len(filtered) > opt.Lines {
		filtered = filtered[len(filtered)-opt.Lines:]
	}
	if filtered == nil {
		filtered = []string{}
	}

	return &ReadResult{
		Key: key, Path: e.Path, Lines: filtered,
		Total: total, Matched: matched, Truncated: truncated,
		Size: size, ModTime: st.ModTime().Format("2006-01-02 15:04:05"),
	}, nil
}

// levelPatterns 用于识别常见日志级别。
var levelPatterns = map[string]*regexp.Regexp{
	"error": regexp.MustCompile(`(?i)\b(error|err|fatal|emerg|crit|alert|fail|panic|exception)\b|\[error\]|\b5\d\d\b`),
	"warn":  regexp.MustCompile(`(?i)\b(warn|warning|notice)\b|\[warn\]`),
	"info":  regexp.MustCompile(`(?i)\b(info|notice)\b|\[info\]`),
}

func filterLines(lines []string, opt ReadOptions) []string {
	var re *regexp.Regexp
	if opt.Filter != "" {
		if opt.IsRegex {
			compiled, err := regexp.Compile(opt.Filter)
			if err == nil {
				re = compiled
			}
		} else {
			re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(opt.Filter))
		}
	}
	var levelRe *regexp.Regexp
	if opt.Level != "" {
		levelRe = levelPatterns[strings.ToLower(opt.Level)]
	}

	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if re != nil && !re.MatchString(ln) {
			continue
		}
		if levelRe != nil && !levelRe.MatchString(ln) {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// ---------------------------------------------------------------------------
//  实时尾随
// ---------------------------------------------------------------------------

// Follow 持续输出日志新增内容（供 SSE 使用）。
//
// 实现：每 500ms 检查文件大小。
//   - 变大：读取新增部分
//   - 变小：判定为轮转或清空，从头读
//
// 首帧会带上末尾若干行，让用户立刻看到上下文。
func (c *Catalog) Follow(ctx context.Context, key string, tailLines int) (<-chan string, error) {
	e, err := c.Find(key)
	if err != nil {
		return nil, err
	}
	if !e.Available && e.Exists {
		return nil, fmt.Errorf("没有权限读取该日志：%s", e.Path)
	}
	if tailLines <= 0 {
		tailLines = 50
	}

	ch := make(chan string, 128)
	go func() {
		defer close(ch)

		var offset int64
		// 先推送一段上下文
		if e.Exists {
			if res, err := c.Read(key, ReadOptions{Lines: tailLines, MaxBytes: 256 << 10}); err == nil && len(res.Lines) > 0 {
				select {
				case ch <- strings.Join(res.Lines, "\n") + "\n":
				case <-ctx.Done():
					return
				}
			}
			if st, err := os.Stat(e.Path); err == nil {
				offset = st.Size()
			}
		}

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				st, err := os.Stat(e.Path)
				if err != nil {
					continue
				}
				size := st.Size()
				if size < offset {
					// 文件被清空或轮转：回到开头
					offset = 0
				}
				if size == offset {
					continue
				}
				// 单次最多读 512KB，避免日志暴涨时一次性吃掉内存
				const maxChunk = 512 << 10
				readFrom := offset
				if size-readFrom > maxChunk {
					readFrom = size - maxChunk
				}
				f, err := os.Open(e.Path)
				if err != nil {
					continue
				}
				if _, err := f.Seek(readFrom, 0); err != nil {
					_ = f.Close()
					continue
				}
				buf := make([]byte, size-readFrom)
				n, _ := f.Read(buf)
				_ = f.Close()
				offset = readFrom + int64(n)
				if n > 0 {
					select {
					case ch <- string(buf[:n]):
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch, nil
}

// ---------------------------------------------------------------------------
//  清理 / 轮转
// ---------------------------------------------------------------------------

// Truncate 清空一个日志文件。
//
// 用 O_TRUNC 而不是删除文件：删除后正在写入的进程会继续往已删除的 inode 写，
// 直到重启才重建文件，期间日志全部丢失且占用不释放。
// 清空（truncate）能让写入进程无感地继续写，是更正确的做法。
//
// 清空前会把最后一部分另存为 .1 备份，避免"手一抖把刚发生的事故现场清了"。
func (c *Catalog) Truncate(key string) (string, error) {
	e, err := c.Find(key)
	if err != nil {
		return "", err
	}
	if !e.Exists {
		return "", fmt.Errorf("日志文件不存在：%s", e.Path)
	}
	if !e.Available {
		return "", fmt.Errorf("没有权限操作该日志：%s", e.Path)
	}

	// 备份最后 512KB
	var backup string
	if e.Size > 0 {
		backup = e.Path + ".1"
		if err := copyTail(e.Path, backup, 512<<10); err != nil {
			return "", fmt.Errorf("备份日志尾部失败: %w", err)
		}
	}

	// 保留原权限，清空内容
	st, err := os.Stat(e.Path)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(e.Path, os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return "", fmt.Errorf("清空日志失败: %w", err)
	}
	_ = f.Close()

	if backup != "" {
		return fmt.Sprintf("已清空日志（尾部内容备份为 %s）", filepath.Base(backup)), nil
	}
	return "已清空日志", nil
}

// copyTail 把文件末尾 maxBytes 字节拷贝到目标。
func copyTail(src, dst string, maxBytes int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	start := int64(0)
	if st.Size() > maxBytes {
		start = st.Size() - maxBytes
	}
	if _, err := in.Seek(start, 0); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	buf := make([]byte, 64*1024)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			break
		}
	}
	return nil
}

// Delete 删除一个日志文件（仅面板自身产生的日志允许）。
func (c *Catalog) Delete(key string) error {
	e, err := c.Find(key)
	if err != nil {
		return err
	}
	if !e.Rotatable {
		return fmt.Errorf("该日志由系统或第三方软件产生，面板不提供删除。可用「清空」而不是删除")
	}
	if err := os.Remove(e.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// 一并清掉清空时产生的 .1 备份
	_ = os.Remove(e.Path + ".1")
	return nil
}

// Stats 返回日志总体情况（用于页面顶部概览）。
type Stats struct {
	TotalFiles  int    `json:"total_files"`
	TotalSize   int64  `json:"total_size"`
	Largest     string `json:"largest"`
	LargestSize int64  `json:"largest_size"`
	Missing     int    `json:"missing"`
}

// Stats 统计日志占用。
func (c *Catalog) Stats() Stats {
	var s Stats
	for _, e := range c.List() {
		if !e.Exists {
			s.Missing++
			continue
		}
		s.TotalFiles++
		s.TotalSize += e.Size
		if e.Size > s.LargestSize {
			s.LargestSize = e.Size
			s.Largest = e.Name
		}
	}
	return s
}
