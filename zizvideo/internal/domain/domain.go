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
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	RootPath    string   `json:"root_path"`
	Recursive   bool     `json:"recursive"`
	Enabled     bool     `json:"enabled"`
	IgnoreRules []string `json:"ignore_rules"`
	MountID     string   `json:"-"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
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
	LibraryID    string `json:"library_id,omitempty"`
	SortOrder    int    `json:"sort_order"`
	EpisodeCount int    `json:"episode_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
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

	ErrSeriesTitle     = New("SERIES_TITLE_REQUIRED", "剧场标题不能为空", 400)
	ErrSeriesDuplicate = New("SERIES_DUPLICATE_MEDIA", "该媒体已在本剧场中", 409)
	ErrSeriesOrder     = New("SERIES_ORDER_MISMATCH", "排序列表与剧场成员不一致，请刷新后重试", 409)
)
