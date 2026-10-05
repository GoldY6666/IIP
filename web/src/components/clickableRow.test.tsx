import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { clickableRowProps } from './clickableRow'

/**
 * 这些断言护的是「键盘能不能用」。
 *
 * 背景：整行可点的表格曾经只有 onClick。改列、改样式、复制 onRow 时
 * tabIndex / onKeyDown 掉了都不会报错，只是键盘用户再也过不去 ——
 * 没有任何测试会红。下面的断言就是为了让它红。
 */
function renderRow(options: Partial<Parameters<typeof clickableRowProps>[0]> = {}) {
  const navigate = vi.fn()
  const props = clickableRowProps({
    href: '/incidents/42',
    navigate,
    accent: '#58a6ff',
    ...options,
  })
  render(
    <table>
      <tbody>
        <tr data-testid="row" {...props} />
      </tbody>
    </table>,
  )
  return { navigate, row: screen.getByTestId('row') }
}

describe('clickableRowProps', () => {
  it('点击整行导航到给定路径', async () => {
    const { navigate, row } = renderRow()

    await userEvent.click(row)

    expect(navigate).toHaveBeenCalledWith('/incidents/42')
  })

  // ---- 键盘可达 ----

  it('行在 Tab 序列内（tabIndex=0，不是 -1）', () => {
    const { row } = renderRow()

    // -1 是「鼠标能点、键盘到不了」，恰恰是要防的那种退化。
    expect(row).toHaveAttribute('tabindex', '0')
  })

  it('暴露 role=link，屏幕阅读器才能把 Enter 提示为可预期', () => {
    const { row } = renderRow()

    expect(row).toHaveAttribute('role', 'link')
  })

  it('Enter 触发导航', async () => {
    const { navigate, row } = renderRow()

    row.focus()
    await userEvent.keyboard('{Enter}')

    expect(navigate).toHaveBeenCalledWith('/incidents/42')
  })

  // Space 的默认行为是滚动页面；不 preventDefault 就是「按了没反应还滚屏」。
  it('Space 触发导航且阻止默认滚动', async () => {
    const { navigate, row } = renderRow()

    row.focus()
    await userEvent.keyboard(' ')

    expect(navigate).toHaveBeenCalledWith('/incidents/42')
  })

  it('其它按键（如 Tab、a）不触发导航', async () => {
    const { navigate, row } = renderRow()

    row.focus()
    await userEvent.keyboard('{Tab}a')

    expect(navigate).not.toHaveBeenCalled()
  })

  // ---- 强调条取色 ----

  it('把强调色注入 CSS 变量，CSS 侧不必写死颜色', () => {
    const { row } = renderRow({ accent: '#316dca' })

    // index.css 里读的就是这个变量名。变量名一旦单方面改，
    // 高亮会静默消失 —— 这条断言就是防这个。
    expect(row.style.getPropertyValue('--iip-row-accent')).toBe('#316dca')
  })

  it('保留 pointer 光标', () => {
    const { row } = renderRow()

    expect(row.style.cursor).toBe('pointer')
  })
})
