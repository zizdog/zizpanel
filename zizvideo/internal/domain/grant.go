package domain

// UserLibraryGrant 是 user_libraries 的一行 + 库名 + 来源。
// source（admin/default）只做显示/审计，绝不参与判据（B.8 不变量）。
type UserLibraryGrant struct {
	LibraryID string `json:"library_id"`
	Name      string `json:"name"`
	Source    string `json:"source"`
}

// DefaultBackfillResult 是补发默认可见库的计数；与 DB 行数一致，不许谎报。
type DefaultBackfillResult struct {
	UsersTotal   int `json:"users_total"`
	UsersGranted int `json:"users_granted"`
	UsersSkipped int `json:"users_skipped"`
	RowsWritten  int `json:"rows_written"`
}
