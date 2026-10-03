package videoopt

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
//  规划期的实时进度（用户报障："视频多的时候真的以为卡死了"）
//
//  /video-plan 是同步的，只回一个转圈文案看不出是死是活。这里把两个阶段的
//  真实进度记在进程级的小表里，前端在请求飞行期间轮询 /video-plan-progress 拿它
//  —— 不改 /video-plan 的同步契约，也**不重新扫描**（轮询只读内存快照）：
//    · scan  阶段：已扫描目录数 / 已发现视频数 / 当前相对目录（递归才有）；
//    · probe 阶段：已读取第几个 / 共几个 + 当前文件名。
//
//  只在扫描进行中才有条目（开始登记、结束即删）：缓存命中时整个扫描是毫秒级、
//  前端根本轮询不到 ⇒ **改配置不再出现这个过程**（复用上一轮的探测缓存）。
//  nil 接收者 = 没有进度（不报错）。
// ============================================================================

// 进度阶段（Phase 的取值）。
const (
	PlanPhaseScan  = "scan"
	PlanPhaseProbe = "probe"
)

// ScanProgress 是一次目录扫描的进度快照。
type ScanProgress struct {
	// Phase 是当前阶段：PlanPhaseScan（递归扫描目录树）或 PlanPhaseProbe（逐个读信息）。
	Phase string `json:"phase,omitempty"`
	// Scanning 表示这次规划正在递归扫描目录树（前端据此显示扫描计数）。
	Scanning bool `json:"scanning,omitempty"`
	// DirsScanned / VideosFound / CurrentPath 是 scan 阶段的真实计数。
	DirsScanned int    `json:"dirs_scanned"`
	VideosFound int    `json:"videos_found"`
	CurrentPath string `json:"current_path,omitempty"`
	// Done 是**已探测完**的个数，Total 是本目录的候选视频数。
	Done  int `json:"done"`
	Total int `json:"total"`
	// Name 是当前正在读的文件名（Done+1 号）。
	Name string `json:"name,omitempty"`
	// Probed 是这次扫描真跑了 ffprobe 的次数（缓存命中不算）。
	Probed int `json:"probed"`
	// Message 是给用户看的那一行（唯一措辞来源，见 ScanProgressText / ScanTreeProgressText）。
	Message string `json:"message"`
	// start 是这次扫描的登记时刻（用来算"已用 N 秒"，不出现在响应里）。
	start time.Time
	// gen 是这次扫描的代号：连点选项会中断上一次请求，上一次的收尾（End）绝不许
	// 删掉/覆盖这一次已经登记好的进度（否则新一轮的进度会凭空消失）。
	gen uint64
}

// ScanProgressText 是探测进度的唯一文案：`正在读取视频信息 3/20：xxx.mp4`。
//
// done 是已完成的个数（0 基的当前下标 + 1 = 正在读第几个）。
func ScanProgressText(done, total int, name string) string {
	return fmt.Sprintf("正在读取视频信息 %d/%d：%s", done+1, total, name)
}

// ScanTreeProgressText 是递归扫描进度的唯一文案：
// `已扫描 1,234 个目录 · 发现 3,456 个视频 · 已用 12 秒 · 当前：第1季/动漫/`。
//
// 三个计数都是真的数出来的；"已用"由服务端按登记时刻算（前端不重复写文案）。
func ScanTreeProgressText(dirs, videos int, elapsed time.Duration, cur string) string {
	sec := int64(elapsed / time.Second)
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("已扫描 %s 个目录 · 发现 %s 个视频 · 已用 %d 秒 · 当前：%s",
		commaInt(dirs), commaInt(videos), sec, displayDirPath(cur))
}

// displayDirPath 把相对目录写成用户看得懂的形态（基准目录 = `./`，其余带结尾斜杠）。
func displayDirPath(rel string) string {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" || rel == "." {
		return "./"
	}
	return strings.TrimSuffix(rel, "/") + "/"
}

// commaInt 给整数加千分位（1,234）—— 纯展示用。
func commaInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// ScanProgressStore 按目录记录"最近一次进行中的扫描"。
type ScanProgressStore struct {
	mu   sync.Mutex
	m    map[string]ScanProgress
	next uint64
}

// NewScanProgressStore 造一份进度表。
func NewScanProgressStore() *ScanProgressStore {
	return &ScanProgressStore{m: map[string]ScanProgress{}}
}

// Begin 登记一次扫描开始并返回本次代号；recursive=true 时前端立刻能看到"正在扫描子目录"。
//
// 代号必须原样传给 UpdateScan/Update/CountProbe/End：被中断的旧请求拿着旧代号，
// 改不动也删不掉新一轮的进度条目。
func (s *ScanProgressStore) Begin(dir string, recursive bool) uint64 {
	if s == nil {
		return 0
	}
	msg := "正在读取视频信息…"
	if recursive {
		msg = "正在扫描子目录…"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	gen := s.next
	s.m[dir] = ScanProgress{Scanning: recursive, Message: msg, start: time.Now(), gen: gen}
	return gen
}

// live 取本次代号对应的条目；条目已被新一轮取代（或已结束）时 ok=false。
func (s *ScanProgressStore) live(dir string, gen uint64) (ScanProgress, bool) {
	e, ok := s.m[dir]
	if !ok || e.gen != gen {
		return ScanProgress{}, false
	}
	return e, true
}

// UpdateScan 记录递归扫描的真实进度（dirs 已读目录数 / videos 已发现视频数 / cur 当前目录）。
func (s *ScanProgressStore) UpdateScan(dir string, gen uint64, dirs, videos int, cur string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(dir, gen)
	if !ok {
		return
	}
	e.Phase, e.Scanning = PlanPhaseScan, true
	e.DirsScanned, e.VideosFound, e.CurrentPath = dirs, videos, cur
	e.Message = ScanTreeProgressText(dirs, videos, time.Since(e.start), cur)
	s.m[dir] = e
}

// Update 记录"正在读第 done+1 个（共 total）"。
func (s *ScanProgressStore) Update(dir string, gen uint64, done, total int, name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(dir, gen)
	if !ok {
		return
	}
	e.Phase, e.Scanning = PlanPhaseProbe, false
	e.Done, e.Total, e.Name = done, total, name
	e.Message = ScanProgressText(done, total, name)
	s.m[dir] = e
}

// CountProbe 记一次真探测（缓存命中不调用）。
func (s *ScanProgressStore) CountProbe(dir string, gen uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(dir, gen)
	if !ok {
		return
	}
	e.Probed++
	s.m[dir] = e
}

// End 删除该目录的进度条目（扫描结束/失败/取消都调它，绝不留陈旧进度）。
// 只有**本次代号**还活着时才删：被中断的旧请求不许误删新一轮的进度。
func (s *ScanProgressStore) End(dir string, gen uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[dir]; ok && e.gen == gen {
		delete(s.m, dir)
	}
}

// Get 取该目录当前进行中的扫描进度；没有（扫描没在跑）时 ok=false。
func (s *ScanProgressStore) Get(dir string) (ScanProgress, bool) {
	if s == nil {
		return ScanProgress{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[dir]
	return e, ok
}

// Active 表示这份快照值不值得给用户看：正在递归扫描、或真的在跑 ffprobe。
// 缓存命中的改配置扫描（毫秒级、0 次探测）返回 false ⇒ 前端不闪进度。
func (p ScanProgress) Active() bool { return p.Scanning || p.Probed > 0 }
