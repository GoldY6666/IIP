// 测试全局初始化：在每个测试文件加载前执行。
//
// 1. jest-dom 的 matcher（toBeInTheDocument 等）—— 它是扩展而非替换，
//    不 import 就用不了断言语法。
// 2. matchMedia 桩 —— **不装 jsdom 不支持 matchMedia**，而 antd 的
//    响应式组件（Grid 的 Col/Row、useBreakpoint）会在渲染时调用它。
//    不打这个桩，报错是「matchMedia is not a function」，
//    看起来像代码 bug，其实是环境缺功能 —— 极难定位。
// 3. ResizeObserver 桩 —— antd 的 Select/Table 用它观察容器尺寸，
//    jsdom 同样不提供。
//
// **这三条桩不是「为了让测试通过」，而是「让环境达到组件真正依赖的最小集」**。
// 若某个桩被写成 no-op 后测试反而失败，说明组件真的用它做了逻辑判断，
// 那种情况下应该去补真实实现而不是加大桩。
import '@testing-library/jest-dom/vitest'
import { afterEach, vi } from 'vitest'
import { cleanup } from '@testing-library/react'

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

// --- matchMedia ---
if (!window.matchMedia) {
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    // addListener/removeListener 是废弃 API 但 antd v5 的部分 hook 仍在调，
    // 不给会被 TypeError。给成 no-op 而不删：删掉会让 antd 走到 undefined 分支。
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
}

// --- ResizeObserver ---
if (!globalThis.ResizeObserver) {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
}

// **刻意不桩 getComputedStyle 的 scrollWidth**：antd Table 在 jsdom 下会读到 0，
// 一旦桩成固定值，「测的就是桩的行为而不是组件行为」，比失败更糟。
// 真遇到 Table 相关的渲染失败时，应改用更细的组件级断言，
// 或直接为该场景准备可测的结构，而不是把环境补成「什么都能过」。
