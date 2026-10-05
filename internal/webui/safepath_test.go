package webui

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandlerRejectsPathEscape 是本包**最重要的测试**。
//
// 静态托管最典型的漏洞就是目录穿越：请求 /../../etc/passwd
// 把容器里的文件读出来。这里穷举所有能让路径「往上走」的形式。
//
// 判据用「状态码 + 响应体不含宿主文件内容」双重断言。
// 只断言状态码不够：404 和 400 都是「没给文件」，
// 但如果实现是「先 Stat 再判断」，某些形态会漏过 ——
// 而它一旦漏过，测试会因为恰好返回了 404 而 PASS。
func TestHandlerRejectsPathEscape(t *testing.T) {
	root := newTestRoot(t)
	h := Handler(root, UIConfig{})
	if h == nil {
		t.Fatal("Handler 返回 nil，静态托管未启用")
	}

	// 在 root 之外放一个「敏感文件」，内容唯一可辨。
	// 若任何逃逸形态读到了它的内容，测试必须失败。
	outside := filepath.Join(filepath.Dir(root), "SECRET.txt")
	const secret = "TOP-SECRET-CANARY-8f3a2b"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatalf("准备 canary 文件失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	cases := []struct {
		name string
		path string
	}{
		{"上跳一级", "/../SECRET.txt"},
		{"上跳绝对", "/../../etc/passwd"},
		{"多段上跳", "/assets/../../SECRET.txt"},
		{"上跳后回跳", "/../assets/../SECRET.txt"},
		{"双斜杠开头", "//../SECRET.txt"},
		{"点号开头", "/./../SECRET.txt"},
		// 反斜杠是 Windows 上最危险的一种：filepath 在 Windows 上
		// 把它当分隔符，而 URL 规范里它不是。只在 Linux 上测不出来，
		// 所以这里必须显式带。
		{"反斜杠上跳", `\..\SECRET.txt`},
		{"反斜杠绝对", `\..\..\etc\passwd`},
		{"正斜杠反斜杠混用", `/..\SECRET.txt`},
		{"尾段上跳", "/assets/../.."},
		{"深层上跳", "/a/b/c/../../../../SECRET.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRaw(t, h, tc.path)
			body := rec.Body.String()

			if strings.Contains(body, secret) {
				t.Fatalf("目录穿越成功！路径 %q 读到了 root 外的文件内容", tc.path)
			}
			// 允许 400（明确拒绝）或 404（当作不存在）。
			// 两者都是安全结果；**不允许 200**。
			if rec.Code == http.StatusOK {
				t.Fatalf("路径 %q 返回 200，逃逸未被拦截。body 前 120 字节: %q",
					tc.path, truncate(body, 120))
			}
		})
	}
}

// TestHandlerAllowsDotsInsideFilename 验「符号链接逃逸」这条**反面**：
// 合法文件名里带 .. 的（"foo..bar"）必须能正常取到 ——
// 这是「用 strings.Contains(p, "..") 判穿越」的写法会误伤的情形，
// 也是本包刻意逐段判而非整串匹配的理由。
func TestHandlerAllowsDotsInsideFilename(t *testing.T) {
	root := newTestRoot(t)
	const body = "dots-in-name"
	write(t, filepath.Join(root, "a..b"), body)
	// 目录名里带 .. 也应放行。
	mkdirAll(t, filepath.Join(root, "v1.0..next"))
	write(t, filepath.Join(root, "v1.0..next", "index.html"), "nested-index")

	h := Handler(root, UIConfig{})
	if h == nil {
		t.Fatal("Handler 返回 nil")
	}

	t.Run("文件名含..可访问", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/a..b")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /a..b = %d，期望 200（合法文件名不该被当穿越拒绝）", rec.Code)
		}
		if rec.Body.String() != body {
			t.Fatalf("body = %q，期望 %q", rec.Body.String(), body)
		}
	})

	t.Run("目录名含..可访问", func(t *testing.T) {
		// 刻意用**不带** index.html 的路径：http.ServeFile 对以
		// "/index.html" 结尾的 URL 会 301 规范化到 "./"
		// （这是标准库的既定行为，不是缺陷）。断言 200 会把
		// 标准库的正确行为记成产品 bug。
		write(t, filepath.Join(root, "v1.0..next", "data.json"), "nested-file")
		rec := do(t, h, http.MethodGet, "/v1.0..next/data.json")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1.0..next/data.json = %d，期望 200", rec.Code)
		}
		if rec.Body.String() != "nested-file" {
			t.Fatalf("body = %q，期望 %q", rec.Body.String(), "nested-file")
		}
	})
}

// TestHandlerRejectsEscapeWhenRootIsSymlinkTarget 验「root 本身是符号链接」
// 的情形 —— 部署时 dist 常被链接到 release 目录。
//
// 这不该被判成穿越：root 是显式配置的，指向哪由部署者决定。
// 误判会让「用 symlink 管理版本」这种常见做法直接不可用。
func TestHandlerAllowsSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "releases", "v1")
	mkdirAll(t, real)
	write(t, filepath.Join(real, indexFile), "release-v1")

	link := filepath.Join(base, "current")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("本环境不支持创建符号链接（Windows 需管理员权限）: %v", err)
	}
	// Windows 上 os.Symlink **可能返回 nil 却没建出有效链接**
	// （目录符号链接需 SeCreateSymbolicLink 权限，无管理员时静默失败）。
	// 所以必须验一下，else 这个用例会因「链接不存在」得到 500 而 FAIL ——
	// 那是环境问题，不是产品问题。
	if _, err := os.Stat(link); err != nil {
		t.Skipf("符号链接创建无效（Windows 无 SeCreateSymbolicLink 权限时静默失败）: %v", err)
	}

	h := Handler(link, UIConfig{})
	if h == nil {
		t.Fatal("Handler 对符号链接 root 返回了 nil，部署会直接没有 UI")
	}
	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d，期望 200", rec.Code)
	}
	if rec.Body.String() != "release-v1" {
		t.Fatalf("body = %q，期望 %q", rec.Body.String(), "release-v1")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
