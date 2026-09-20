// Package domain holds entities and coded errors; it depends on nothing else.
package domain

// Role and status enums. Kept as plain strings so they round-trip through JSON.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Media status values.
const (
	MediaPending   = "pending"
	MediaReady     = "ready"
	MediaProbeFail = "probe_failed"
	MediaMissing   = "missing"
)

// Scan task status values.
const (
	TaskPending     = "pending"
	TaskRunning     = "running"
	TaskSuccess     = "success"
	TaskFailed      = "failed"
	TaskInterrupted = "interrupted"
)

// Probe error classes stored in media.error_class.
const (
	ClassNotFound      = "NOT_FOUND"
	ClassUnreadable    = "UNREADABLE"
	ClassInvalidFormat = "INVALID_FORMAT"
	ClassProbeTimeout  = "PROBE_TIMEOUT"
)

// User is a local account. PasswordHash never leaves the storage layer.
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	PasswordHash string `json:"-"`
	LastLoginAt  string `json:"last_login_at"`
	CreatedAt    string `json:"created_at"`
}

// Library is a registered media root.
type Library struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	RootPath           string   `json:"root_path"`
	Recursive          bool     `json:"recursive"`
	Enabled            bool     `json:"enabled"`
	IgnoreRules        []string `json:"ignore_rules"`
	DefaultForNewUsers bool     `json:"default_for_new_users"`
	MountID            string   `json:"-"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
}

// Codecs holds the stream codec names we act on.
type Codecs struct {
	Video string `json:"video"`
	Audio string `json:"audio"`
}

// Media is one video file on disk plus its probed metadata.
type Media struct {
	ID           string  `json:"id"`
	LibraryID    string  `json:"library_id"`
	Path         string  `json:"path,omitempty"`
	Title        string  `json:"title"`
	Size         int64   `json:"size"`
	MtimeNS      int64   `json:"-"`
	Container    string  `json:"container"`
	Codecs       Codecs  `json:"codecs"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	DurationMS   int64   `json:"duration_ms"`
	Bitrate      int64   `json:"bitrate"`
	FPS          float64 `json:"fps"`
	Status       string  `json:"status"`
	ErrorClass   string  `json:"error_class,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	MissingSince string  `json:"-"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// Series is a 剧场（短剧）: an ordered list of already-scanned media items.
// It never owns a file; episodes only reference existing media rows.
type Series struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	CoverMediaID string `json:"cover_media_id,omitempty"`
	// CoverLibraryID 只用于判据：封面可能属于另一个库（S4），不外发。
	CoverLibraryID string `json:"-"`
	LibraryID      string `json:"library_id,omitempty"`
	SortOrder      int    `json:"sort_order"`
	EpisodeCount   int    `json:"episode_count"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// episode_source values for series_media (补丁 R1).
const (
	EpisodeSourceFilename = "filename"
	EpisodeSourceManual   = "manual"
)

// SeriesEpisode places one existing media item at a 1-based position; Position
// doubles as the admin order, while season/episode carry the filename-derived
// numbers (nil = 未识别, never guessed).
type SeriesEpisode struct {
	SeriesID      string `json:"series_id"`
	MediaID       string `json:"media_id"`
	Position      int    `json:"position"`
	Season        *int   `json:"season"`
	Episode       *int   `json:"episode"`
	EpisodeSource string `json:"episode_source,omitempty"`
	AddedAt       string `json:"added_at"`
}

// ScanTask is a persisted scan job's state.
type ScanTask struct {
	ID         string `json:"id"`
	LibraryID  string `json:"library_id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Total      int    `json:"total"`
	Scanned    int    `json:"scanned"`
	Updated    int    `json:"updated"`
	Failed     int    `json:"failed"`
	Missing    int    `json:"missing"`
	Suspected  int    `json:"suspected"`
	Error      string `json:"error"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	UpdatedAt  string `json:"updated_at"`
}

// Job kinds and triggers for the cross-library job_tasks table (0008).
const (
	JobKindEpisodeDetect = "episode_detect"

	JobTriggerManualBatch  = "manual_batch"
	JobTriggerScanFinished = "scan_finished"
)

// JobTask is a persisted cross-library background job (一键识别 / 扫描后自动识别）。
// Degraded + DegradeReason 是 A.5 语义：降级必须显式给理由，理由为空即错误。
type JobTask struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Trigger       string `json:"trigger"`
	Status        string `json:"status"`
	Total         int    `json:"total"`
	Processed     int    `json:"processed"`
	Updated       int    `json:"updated"`
	Failed        int    `json:"failed"`
	ManualSkipped int    `json:"manual_skipped"`
	Unidentified  int    `json:"unidentified"`
	Degraded      bool   `json:"degraded"`
	DegradeReason string `json:"degrade_reason"`
	Error         string `json:"error"`
	Summary       string `json:"-"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// AutoScan 触发来源与运行终态（迁移 0009）。status 的四种取值必须能区分
// "扫了"、"部分失败"、"没扫但如实说明原因"、"触发即失败"（不许把跳过报成成功）。
const (
	AutoScanTriggerStartup = "startup"
	AutoScanTriggerTimer   = "timer"
	AutoScanTriggerEvents  = "events"
	AutoScanTriggerManual  = "manual"

	AutoScanSuccess = "success"
	AutoScanPartial = "partial"
	AutoScanSkipped = "skipped"
	AutoScanFailed  = "failed"

	AutoScanIntervalMin = 1
	AutoScanIntervalMax = 1440
	AutoScanDebounceMin = 5
	AutoScanDebounceMax = 10
)

// AutoScanSettings is the single-row auto-scan configuration.
type AutoScanSettings struct {
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	EventsEnabled   bool   `json:"events_enabled"`
	DebounceSeconds int    `json:"debounce_seconds"`
	UpdatedAt       string `json:"updated_at"`
}

// AutoScanRun is one automatic-scan round with its honest outcome.
type AutoScanRun struct {
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Libraries  int    `json:"libraries"`
	Started    int    `json:"started"`
	Skipped    int    `json:"skipped"`
	Updated    int    `json:"updated"`
	NewMedia   int    `json:"new_media"`
	Failed     int    `json:"failed"`
	Missing    int    `json:"missing"`
	EventsOK   bool   `json:"events_ok"`
	EventsNote string `json:"events_note"`
	Note       string `json:"note"`
}

// Progress is one user's playback position for one media item.
type Progress struct {
	UserID     string `json:"-"`
	MediaID    string `json:"media_id"`
	PositionMS int64  `json:"position_ms"`
	DurationMS int64  `json:"duration_ms"`
	Completed  bool   `json:"completed"`
	UpdatedAt  string `json:"updated_at"`
}

// Error is a coded domain error; HTTP maps code -> status.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// New builds a coded error.
func New(code, msg string, status int) *Error {
	return &Error{Code: code, Message: msg, Status: status}
}

// Predeclared errors reused across handlers and services.
var (
	ErrUnauthorized = New("AUTH_UNAUTHORIZED", "未登录或会话已过期", 401)
	ErrForbidden    = New("FORBIDDEN_ROLE", "没有权限执行该操作", 403)
	ErrCSRF         = New("FORBIDDEN_CSRF", "CSRF 校验失败，请刷新页面", 403)
	ErrBadRequest   = New("VALIDATION_BAD_REQUEST", "请求格式不正确", 400)
	ErrNotFound     = New("VALIDATION_NOT_FOUND", "对象不存在", 404)
	ErrConflict     = New("VALIDATION_CONFLICT", "对象已存在", 409)
	ErrRateLimited  = New("RATE_LIMITED", "尝试过于频繁，请稍后再试", 429)
	ErrInternal     = New("INTERNAL_ERROR", "服务内部错误", 500)

	ErrPathNotAbsolute = New("MEDIA_PATH_NOT_ABSOLUTE", "必须是绝对路径", 400)
	ErrPathNotClean    = New("MEDIA_PATH_NOT_CLEAN", "路径含冗余片段，请用规范写法", 400)
	ErrPathNotAllowed  = New("MEDIA_PATH_NOT_ALLOWED", "路径不在允许的媒体根目录内", 403)
	ErrPathNotExist    = New("MEDIA_PATH_NOT_FOUND", "路径不存在或不可访问", 400)
	ErrPathNotDir      = New("MEDIA_PATH_NOT_DIR", "路径不是目录", 400)
	ErrPathUnreadable  = New("MEDIA_PATH_UNREADABLE", "路径不可读", 403)
	ErrPathNullByte    = New("MEDIA_PATH_NULL_BYTE", "路径含非法字符", 400)
	ErrPathEscapes     = New("MEDIA_PATH_ESCAPES_ROOT", "文件解析后落在媒体库之外", 403)
	ErrPathRootBusy    = New("MEDIA_ROOT_IN_USE", "该允许根正被媒体库使用", 409)
	ErrPathRootExists  = New("MEDIA_ROOT_EXISTS", "该路径已在允许根里", 409)

	ErrScanRunning  = New("SCAN_ALREADY_RUNNING", "该媒体库已有扫描在进行", 409)
	ErrTaskNotFound = New("TASK_NOT_FOUND", "任务不存在", 404)

	// 自动扫描设置校验（迁移 0009）。
	ErrAutoScanInterval = New("AUTOSCAN_INTERVAL_INVALID", "间隔需在 1-1440 分钟之间", 400)
	ErrAutoScanDebounce = New("AUTOSCAN_DEBOUNCE_INVALID", "去抖需在 5-10 秒之间", 400)
	ErrAutoScanDisabled = New("AUTOSCAN_DISABLED", "自动扫描已关闭", 409)
	// 如实语义：跳过/失败必须带原因；监听不可用必须带原因。
	ErrAutoScanNoteRequired   = New("AUTOSCAN_NOTE_REQUIRED", "跳过或失败必须带原因", 409)
	ErrAutoScanEventsRequired = New("AUTOSCAN_EVENTS_REASON_REQUIRED", "监听不可用必须带原因", 409)

	// 任务中心如实语义（A.5）：写入终态前校验，空理由一律拒绝。
	ErrJobErrorRequired  = New("JOB_ERROR_REQUIRED", "失败任务必须带错误原因", 409)
	ErrJobDegradeReason  = New("JOB_DEGRADE_REASON_REQUIRED", "降级任务必须带降级原因", 409)
	ErrJobErrorOnSuccess = New("JOB_ERROR_ON_SUCCESS", "成功任务不能带错误原因", 409)

	ErrSeriesTitle     = New("SERIES_TITLE_REQUIRED", "剧场标题不能为空", 400)
	ErrSeriesDuplicate = New("SERIES_DUPLICATE_MEDIA", "该媒体已在本剧场中", 409)
	ErrSeriesOrder     = New("SERIES_ORDER_MISMATCH", "排序列表与剧场成员不一致，请刷新后重试", 409)
)
