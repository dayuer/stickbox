// Package apidoc 校验 docs/API.md 的端点总表与真实路由表一致。
//
// 文档漂移是这类项目最常见的坑：代码里加了端点、删了端点，文档还停在上一版，
// 固件和 agent 照着文档写就会对不上。这里把 api.go 里的 mux 注册和文档里
// 的表格行都当成数据解析，在 go test 里直接比对集合。
package apidoc

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// mux.HandleFunc("POST /api/...", ...) 或 mux.Handle("GET /api/...", ...)
var routeRe = regexp.MustCompile(`mux\.Handle(?:Func)?\(\s*"([A-Z]+) ([^"]+)"`)

// 端点表的一行：| POST | `/api/device/register` | 无 | ... |
// 路径前缀要和 api.go 里真实注册的保持一致。/fw/ 是固件下载（不在 /api
// 下，因为它不鉴权、也不返回 JSON），漏掉它会让这条测试放过未写文档的端点。
var rowRe = regexp.MustCompile("^\\|\\s*(GET|POST)\\s*\\|\\s*`(/(?:api|healthz|fw)[^`]*)`\\s*\\|")

const (
	startMarker = "<!-- endpoints:start -->"
	endMarker   = "<!-- endpoints:end -->"
)

// repoRoot 从本测试文件的位置推仓库根：server/go/internal/apidoc → 上四级。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件路径")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return string(b)
}

// docEndpoints 取文档标记之间的端点集合。
func docEndpoints(t *testing.T, doc string) map[string]bool {
	t.Helper()
	start := strings.Index(doc, startMarker)
	if start < 0 {
		t.Fatalf("docs/API.md 缺少 %s", startMarker)
	}
	rest := doc[start+len(startMarker):]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		t.Fatalf("docs/API.md 缺少 %s", endMarker)
	}
	if strings.Contains(rest[end+len(endMarker):], startMarker) {
		t.Fatalf("docs/API.md 有多个 %s", startMarker)
	}

	set := map[string]bool{}
	for _, line := range strings.Split(rest[:end], "\n") {
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1] + " " + m[2]
		if set[key] {
			t.Errorf("docs/API.md 重复的端点: %s", key)
		}
		set[key] = true
	}
	return set
}

// routeEndpoints 取 api.go 里注册的端点集合。
func routeEndpoints(t *testing.T, src string) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, m := range routeRe.FindAllStringSubmatch(src, -1) {
		key := m[1] + " " + m[2]
		if set[key] {
			t.Errorf("httpapi 重复注册的端点: %s", key)
		}
		set[key] = true
	}
	return set
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAPIDocMatchesRoutes(t *testing.T) {
	root := repoRoot(t)
	routes := routeEndpoints(t, readFile(t, filepath.Join(root, "server", "go", "internal", "httpapi", "api.go")))
	docs := docEndpoints(t, readFile(t, filepath.Join(root, "docs", "API.md")))

	if len(routes) == 0 {
		t.Fatal("没有从 api.go 解析出任何路由，检查正则或 api.go 的写法")
	}
	if len(docs) == 0 {
		t.Fatal("没有从 docs/API.md 解析出任何端点，检查标记与表格格式")
	}

	var missing, extra []string
	for _, k := range sortedKeys(routes) {
		if !docs[k] {
			missing = append(missing, k)
		}
	}
	for _, k := range sortedKeys(docs) {
		if !routes[k] {
			extra = append(extra, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("docs/API.md 缺少端点:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("docs/API.md 多出代码里不存在的端点:\n  %s", strings.Join(extra, "\n  "))
	}
}
