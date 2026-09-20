package domain

// LibraryScope 是调用方可见的库集合；零值表示什么都看不到（fail-closed）。
// 唯一构造点在 api/authz.go：别处 new 出 All 就等于绕过判据（ITERATION-2 B.3）。
type LibraryScope struct {
	All bool
	IDs map[string]bool
}

// Allows 报告一个库是否在范围内。
func (sc LibraryScope) Allows(libraryID string) bool {
	if sc.All {
		return true
	}
	return sc.IDs[libraryID]
}

// Empty 报告范围是否什么都看不到。
func (sc LibraryScope) Empty() bool { return !sc.All && len(sc.IDs) == 0 }

// LibraryBrief 是用户端的库形状：只有 id/name，root_path 绝不外泄（E.2 #4）。
type LibraryBrief struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
