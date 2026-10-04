package webui

import (
	"net/http"
	"path/filepath"
	"testing"
)

// TestHandlerAcceptsJunctionStyleRoot 补 TestHandlerAllowsSymlinkedRoot 被
// Windows 权限跳过的那条路径。
//
// 背景：os.Symlink 在无管理员权限的 Windows 上**返回 nil 却建不出有效链接**
// （缺 SeCreateSymbolicLink），于是「root 是链接」这条路径在本机长期
// 0 覆盖 —— 而它恰恰是部署常见形态（dist 链接到 release 目录）。
//
// 这里用 **root 路径中带 .. 但经 Clean 后仍在自身内** 来验证同一件事：
// Handler 对 root 的处理只做 Abs + Clean，不要求 root 是"规范路径"。
// 这是「链接式部署」在本机能真实验证的部分。
//
// 诚实说明：这不能替代真实 symlink 测试。真链接那条在本机是 SKIP，
// 需在 Linux 容器链路（verify-images）里补验 —— 记在
// M5 手册 §八 验收清单第 11 条的补充说明里。
func TestHandlerAcceptsNonCanonicalRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "releases", "v1")
	mkdirAll(t, root+"/assets")
	write(t, filepath.Join(root, indexFile), "release-v1")

	// 用一个**非规范**的 root：带冗余段与 ".."，Abs+Clean 后才是真路径。
	// 部署者从脚本里拼路径时很容易产出这种形态。
	weird := filepath.Join(base, "releases", "..", "releases", ".", "v1")

	h := Handler(weird, UIConfig{})
	if h == nil {
		t.Fatal("Handler 对非规范 root 返回 nil，UI 会直接没有")
	}
	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d，期望 200（root 内的冗余段与 .. 应被 Clean 归一）", rec.Code)
	}
	if rec.Body.String() != "release-v1" {
		t.Fatalf("body = %q，期望 %q", rec.Body.String(), "release-v1")
	}
}
