package videoopt

import (
	"fmt"
	"sync"
)

// ============================================================================
//  首次扫描的实时进度（用户报障："视频多的时候真的以为卡死了"）
//
//  /video-plan 是同步的（首次打开要逐个 ffprobe），只回一个转圈文案看不出是死是活。
//  这里把"读到第几个/共几个 + 当前文件名"记在进程级的小表里，前端在请求飞行期间
//  轮询 /video-plan-progress 拿它 —— 不改 /video-plan 的同步契约。
//
//  只在扫描真的进行中才有条目（扫描开始登记、结束即删）：缓存命中时整个扫描是
//  毫秒级、前端根本轮询不到 ⇒ **改配置不再出现这个过程**（复用上一轮的探测缓存）。
//  nil 接收者 = 没有进度（不报错）。
// ============================================================================

// ScanProgress 是一次目录扫描的进度快照。
type ScanProgress struct {
	// Done 是**已探测完**的个数，Total 是本目录的候选视频数。
	Done  int `json:"done"`
	Total int `json:"total"`
	// Name 是当前正在读的文件名（Done+1 号）。
	Name string `json:"name,omitempty"`
	// Probed 是这次扫描真跑了 ffprobe 的次数（缓存命中不算）。
	Probed int `json:"probed"`
	// Message 是给用户看的那一行（唯一措辞来源，见 ScanProgressText）。
	Message string `json:"message"`
}

// ScanProgressText 是扫描进度的唯一文案：`正在读取视频信息 3/20：xxx.mp4`。
//
// done 是已完成的个数（0 基的当前下标 + 1 = 正在读第几个）。
func ScanProgressText(done, total int, name string) string {
	return fmt.Sprintf("正在读取视频信息 %d/%d：%s", done+1, total, name)
}

// ScanProgressStore 按目录记录"最近一次进行中的扫描"。
type ScanProgressStore struct {
	mu sync.Mutex
	m  map[string]ScanProgress
}

// NewScanProgressStore 造一份进度表。
func NewScanProgressStore() *ScanProgressStore {
	return &ScanProgressStore{m: map[string]ScanProgress{}}
}

// Begin 登记一次扫描开始（done=0，还没有文件名）。
func (s *ScanProgressStore) Begin(dir string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[dir] = ScanProgress{Total: 0, Message: "正在读取视频信息…"}
}

// Update 记录"正在读第 done+1 个（共 total）"。
func (s *ScanProgressStore) Update(dir string, done, total int, name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[dir]
	e.Done, e.Total, e.Name = done, total, name
	e.Message = ScanProgressText(done, total, name)
	s.m[dir] = e
}

// CountProbe 记一次真探测（缓存命中不调用）。
func (s *ScanProgressStore) CountProbe(dir string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[dir]
	e.Probed++
	s.m[dir] = e
}

// End 删除该目录的进度条目（扫描结束/失败都调它，绝不留陈旧进度）。
func (s *ScanProgressStore) End(dir string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, dir)
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
