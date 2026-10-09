package webui

import (
	"net/http"
	"path/filepath"
	"testing"
)

// TestHandlerAcceptsNonCanonicalRoot 补 TestHandlerAllowsSymlinkedRoot 被
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
// 诚实说明：这不能替代真实 symlink 测试。真链接那条在宿主 Windows 上是 SKIP，
// 已在 **Linux 容器链路**（`verify-images.py` 4.8 段）补验—— 实测
// 「PASS 24 条，SKIP 0 条」且 `TestHandlerAllowsSymlinkedRoot` 真 PASS 非 SKIP。
// 本用例的定位是**宿主侧兜底**：即使 Linux 侧不可用，宿主也有一层
// 「root 是链接式部署」的真实覆盖，而不是整条路径 0 覆盖。
//
// **判据不是恒绿的**（2026-10-09 变异测试证明，报告见 D:\_iip_setup\_mut_noncanon2.py）：
// 原实现用 `filepath.Join(base, "releases", "..", "releases", ".", "v1")`
// 构造「非规范 root」，但 **`filepath.Join` 自身就做 Clean** ——
// 传进 `Handler` 的已经是规范路径，非规范这个前提**从未成立**。
// 双层变异（坏掉 `Handler` 的 `Abs` 与 `withinRoot` 的 `Rel` 两处归一）
// 仍然全绿，就是这条恒绿的直接证据。
// 修法：**用字符串拼接绕开 `filepath.Join` 的隐式 Clean**，
// 并显式断言 `weird != root` —— 前提不成立就直接 FAIL，
// 杜绝「以后有人改回Join 又悄悄恒绿」。
func TestHandlerAcceptsNonCanonicalRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "releases", "v1")
	mkdirAll(t, root+"/assets")
	write(t, filepath.Join(root, indexFile), "release-v1")

	// 用**字符串拼接**而非 filepath.Join：后者会 Clean 掉 ".." 与 "."，
	// 非规范路径在进Handler 之前就被归一了，前提不成立。
	weird := base + string(filepath.Separator) + "releases" +
		string(filepath.Separator) + ".." +
		string(filepath.Separator) + "releases" +
		string(filepath.Separator) + "." +
		string(filepath.Separator) + "v1"

	// **前提守卫**：非规范 root 必须真的非规范，且 Clean 后等于真路径。
	// 少了这两行，本用例会退化成「用规范路径测规范路径」的恒绿。
	if weird == root {
		t.Fatalf("构造出的 root 不是非规范形态（weird == root = %q），"+
			"本用例的前提不成立，测不到任何东西", weird)
	}
	if filepath.Clean(weird) != root {
		t.Fatalf("Clean(weird) = %q，应等于 root = %q",
			filepath.Clean(weird), root)
	}

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
