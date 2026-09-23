// 内建插件表：随面板二进制一起发布的声明（`go:embed`）。
//
// 为什么内建也要走插件表：B1 的目标是**证明这张表能替代 Go 里的目录定义**。
// 内建表与 Go 定义**同时存在**，由 internal/services/plugin_equiv_test.go 逐字段断言
// 两者一致（不一致即红）—— 这是从"手写目录"迁到"表驱动"的过渡期安全带。
//
// 外部插件目录（P2）会复用同一套解析与校验，只是来源不同（文件系统 + 签名）。
package plugins

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

//go:embed builtin/*.json
var builtinFS embed.FS

var (
	builtinOnce sync.Once
	builtinSpec map[string]*Spec
)

func loadBuiltin() {
	builtinSpec = map[string]*Spec{}
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			continue
		}
		s, err := Parse(raw, "builtin/"+e.Name())
		if err != nil {
			// 内建表不合法属于**打包事故**：这里不 panic（面板要能起来），
			// 由门禁（TestBuiltinSpecsAreValid）保证它在 CI/发版前一定被发现。
			continue
		}
		builtinSpec[s.ID] = s
	}
}

// Builtin 返回某个应用的内建插件声明（不存在则 ok=false，调用方回落到 Go 定义）。
func Builtin(id string) (*Spec, bool) {
	builtinOnce.Do(loadBuiltin)
	s, ok := builtinSpec[strings.TrimSpace(id)]
	return s, ok
}

// BuiltinIDs 返回全部内建声明的 id（排序，便于稳定输出）。
func BuiltinIDs() []string {
	builtinOnce.Do(loadBuiltin)
	out := make([]string, 0, len(builtinSpec))
	for id := range builtinSpec {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Home 展开 `~/` 前缀（插件表里允许写 ~/，运行期统一按真实家目录解析）。
func Home(path, home string) string {
	p := strings.TrimSpace(path)
	if strings.HasPrefix(p, "~/") && strings.TrimSpace(home) != "" {
		return strings.TrimRight(home, "/") + p[1:]
	}
	return p
}
