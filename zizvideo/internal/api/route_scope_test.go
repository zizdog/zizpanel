package api_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 路由源扫描门禁（D.1）：Go ServeMux 不可反射，这里只做源码级分类，如实说明。
// 新增路由必须在 publicAllowlist / authedUnscopedAllowlist 里显式登记，否则测试红。

type routeSpec struct {
	key  string // 方法+路径，如 "GET /api/v1/media"
	expr string // mux.HandleFunc(...) 的完整实参文本
}

var handleFuncRe = regexp.MustCompile(`mux\.HandleFunc\(`)

// routeRegistrations 用括号配平取出每条注册的实参文本。
func routeRegistrations(t *testing.T) []routeSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "web", "router.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	out := []routeSpec{}
	for _, loc := range handleFuncRe.FindAllStringIndex(src, -1) {
		depth, i := 1, loc[1]
		for ; i < len(src) && depth > 0; i++ {
			switch src[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		if depth != 0 {
			t.Fatal("router.go 括号不配平，源码扫描器需要修")
		}
		expr := src[loc[1] : i-1]
		key := firstString(expr)
		if key == "" {
			t.Fatalf("找不到路由字面量: %.60s", expr)
		}
		out = append(out, routeSpec{key: key, expr: expr})
	}
	if len(out) < 40 {
		t.Fatalf("只解析到 %d 条路由，源码扫描器可能失效", len(out))
	}
	return out
}

func firstString(expr string) string {
	start := strings.IndexByte(expr, '"')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(expr[start+1:], '"')
	if end < 0 {
		return ""
	}
	return expr[start+1 : start+1+end]
}

// 登录但不带库范围：只动自己的数据、不返回媒体内容（ITERATION-2 B.4 尾部说明）。
var authedUnscopedAllowlist = map[string]bool{
	"POST /api/v1/auth/logout":    true,
	"GET /api/v1/auth/me":         true,
	"GET /api/v1/feed/settings":   true,
	"PATCH /api/v1/feed/settings": true,
	"DELETE /api/v1/me/progress":  true,
	"DELETE /api/v1/me/favorites": true,
	"DELETE /api/v1/me/likes":     true,
}

// 匿名可访问：新增公开接口必须显式登记，否则测试红（防止悄悄公开）。
var publicAllowlist = map[string]bool{
	"GET /healthz":               true,
	"GET /readyz":                true,
	"GET /api/v1/setup/status":   true,
	"POST /api/v1/setup":         true,
	"POST /api/v1/auth/login":    true,
	"POST /api/v1/auth/register": true,
	"/api/":                      true,
}

var contentPathRe = regexp.MustCompile(`/(media|series|feed|me)(/|$)`)

// TestEveryRouteIsClassified：每条路由要么管理端、要么走 WithLibraryScope、
// 要么在豁免表里；没分类就红（防止以后新增接口忘了过滤）。
func TestEveryRouteIsClassified(t *testing.T) {
	routes := routeRegistrations(t)
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r.key] = true
		scoped := strings.Contains(r.expr, "s.WithLibraryScope(")
		admin := strings.Contains(r.expr, "s.RequireAdmin(")
		authed := strings.Contains(r.expr, "s.RequireAuth(")
		switch {
		case admin:
			if scoped {
				t.Errorf("%s：管理端路由不该包 WithLibraryScope（scope 恒 All）", r.key)
			}
		case scoped:
			if !authed {
				t.Errorf("%s：WithLibraryScope 必须挂在 RequireAuth 内层", r.key)
			}
		case authed:
			if !authedUnscopedAllowlist[r.key] {
				t.Errorf("路由 %s 已登录但没走 WithLibraryScope，也没登记为豁免", r.key)
			}
		default:
			if !publicAllowlist[r.key] {
				t.Errorf("路由 %s 未分类：匿名可访问必须显式登记", r.key)
			}
			if contentPathRe.MatchString(r.key) {
				t.Errorf("路由 %s 含媒体路径却匿名可访问", r.key)
			}
		}
	}
	for key := range authedUnscopedAllowlist {
		if !seen[key] {
			t.Errorf("豁免表里的 %s 已不存在，删掉它以免掩盖同名新路由", key)
		}
	}
	for key := range publicAllowlist {
		if !seen[key] {
			t.Errorf("公开表里的 %s 已不存在，删掉它以免掩盖同名新路由", key)
		}
	}
}

// TestLibraryScopeConstructedOnlyInAuthz：All/IDs 字面量只允许在 authz.go，
// 别处 new LibraryScope 就等于绕过唯一判据（B.3 / D.1）。
func TestLibraryScopeConstructedOnlyInAuthz(t *testing.T) {
	needle := "LibraryScope" + "{"
	allowed := map[string]bool{"api/authz.go": true}
	err := filepath.Walk("../", func(path string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), needle) {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../")
		if !allowed[rel] {
			t.Errorf("%s 构造了 LibraryScope：判据只允许在 internal/api/authz.go", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSourceNotInScopeCriteria：user_libraries.source 只做显示/审计，
// 判据文件里出现它就是第二个过滤分支（B.8 不变量 / D.5）。
func TestSourceNotInScopeCriteria(t *testing.T) {
	for _, name := range []string{"authz.go", filepath.Join("..", "domain", "scope.go")} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(body)), "source") {
			t.Errorf("%s 出现 source：来源不得参与可见性判据", name)
		}
	}
}
