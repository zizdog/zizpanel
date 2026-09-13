package scheduler

import "github.com/zizdog/zizpanel/internal/priv"

// privLaunchStatus 是对 priv.LaunchStatus 的薄封装。
//
// 单独抽出来是为了让 scheduler 包不直接依赖 priv 的具体实现细节，
// 也便于将来替换为其它进程管理器。
func privLaunchStatus(label string) (priv.LaunchState, error) {
	return priv.LaunchStatus(label)
}
