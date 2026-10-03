package evidence

import (
	"testing"
	"time"
)

// 测试窗口推导的全部输入都是**固定时刻**，不用 time.Now()：
// 窗口推导的每个分支都跟 now 有关，用真实时钟会让「等一会儿再断言」时
// 结论漂移，而漂移的方向恰好与被测逻辑无关，排查时极具误导性。

// now 是表驱动用例的基准时刻，提取成函数供断言复用。
func now() time.Time { return winNow }

var (
	winNow    = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	winAnchor = winNow.Add(-30 * time.Minute) // 「半小时前开始的事故」
)

func TestResolveWindow(t *testing.T) {
	cases := []struct {
		name string
		req  WindowRequest
		want Window
	}{
		{
			// 核心场景：新告警合入，第二轮采集。
			// 游标存在时**只采新增区间**，且不套用「事故起点 - 1h」——
			// 那个规则只在首次采集时需要，用来定位事故前后的边界。
			name: "有游标时只采新增区间",
			req: WindowRequest{
				IncidentStart: winAnchor,
				Cursor:        winNow.Add(-2 * time.Minute),
				Now:           winNow,
			},
			want: Window{Start: winNow.Add(-2 * time.Minute), End: winNow},
		},
		{
			// 首次采集，事故刚开始不久：起点 = 事故起点 - 1h。
			name: "首次采集覆盖事故前一小时",
			req: WindowRequest{
				IncidentStart: winAnchor,
				Now:           winNow,
			},
			want: Window{Start: winAnchor.Add(-time.Hour), End: winNow},
		},
		{
			// 关键用例：长事故**仍然回溯到事故起点**。
			//
			// 若这里写成 [now-1h, now]，则「3 小时前才发现的事故」
			// 永远采不到事故起点 —— 游标只往前走，那段数据永久丢失，
			// 而用户看到的是「指标一直很平稳」，与事实相反。
			// 宽度封顶只管后续轮次（见「后续轮次宽度等于两次采集间隔」）。
			name: "长事故仍回溯到事故起点",
			req: WindowRequest{
				IncidentStart: winNow.Add(-5 * time.Hour),
				Now:           winNow,
			},
			want: Window{Start: winNow.Add(-6 * time.Hour), End: winNow},
		},
		{
			// 护栏：started_at 被填成 1970 年这类输入异常时，
			// 回溯被 maxLookback 夹住，不让首次采集拉出 56 年的区间。
			name: "回溯超过上限时被夹住",
			req: WindowRequest{
				IncidentStart: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
				Now:           winNow,
			},
			want: Window{Start: winNow.Add(-maxLookback), End: winNow},
		},
		{
			name: "拿不到事故起点时退化为固定宽度",
			req:  WindowRequest{Now: winNow},
			want: Window{Start: winNow.Add(-time.Hour), End: winNow},
		},
		{
			name: "游标恰好等于 now 时退回事故起点推导",
			// 退化而非返回 [now, now]：空窗口会被采集器查成「零条数据」，
			// 看起来像「这段时间没有异常」，实际是窗口无效。
			req: WindowRequest{
				IncidentStart: winAnchor,
				Cursor:        winNow,
				Now:           winNow,
			},
			want: Window{Start: winAnchor.Add(-time.Hour), End: winNow},
		},
		{
			name: "游标晚于 now 视为时钟漂移并退回",
			req: WindowRequest{
				IncidentStart: winAnchor,
				Cursor:        winNow.Add(time.Hour),
				Now:           winNow,
			},
			want: Window{Start: winAnchor.Add(-time.Hour), End: winNow},
		},
		{
			name: "自定义宽度只影响退化分支",
			// 拿不到事故起点时才有 Width 参与；有锚点时 Width 不参与。
			req: WindowRequest{
				Now:   winNow,
				Width: 10 * time.Minute,
			},
			want: Window{Start: winNow.Add(-10 * time.Minute), End: winNow},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveWindow(tc.req)
			if err != nil {
				t.Fatalf("ResolveWindow 返回错误: %v", err)
			}
			if !got.Start.Equal(tc.want.Start) || !got.End.Equal(tc.want.End) {
				t.Errorf("窗口 = [%v, %v)，期望 [%v, %v)",
					got.Start, got.End, tc.want.Start, tc.want.End)
			}
			// 窗口必须永远合法 —— 非法窗口会让采集器静默返回空结果，
			// 把「窗口坏了」伪装成「这段时间没有异常」。
			if err := got.Validate(); err != nil {
				t.Errorf("推导出的窗口不合法: %v", err)
			}
			// 回溯护栏必须永远生效：不加这条，一个填错的 started_at
			// 就能让首次采集拉出跨 decades 的区间。
			if back := now().Sub(got.Start); back > maxLookback {
				t.Errorf("窗口回溯 %v，超过护栏 %v", back, maxLookback)
			}
		})
	}
}

// TestResolveWindowRejectsZeroNow 基准时刻为空必须报错。
//
// 退化成一个零值窗口会让后续所有时间运算都基于 0001-01-01，
// 查出来的区间没有任何数据，而表现是「采集完成，0 条证据」。
func TestResolveWindowRejectsZeroNow(t *testing.T) {
	if _, err := ResolveWindow(WindowRequest{IncidentStart: winAnchor}); err == nil {
		t.Fatal("Now 为零值时应报错")
	}
}

// TestResolveWindowHandlesFutureIncidentStart 事故开始时间晚于 now 的兜底。
//
// 这在现实中会出现（告警的 startsAt 由告警源提供，源时钟快于本机）。
// 起点若被算到 now 之后，窗口就成负区间。
func TestResolveWindowHandlesFutureIncidentStart(t *testing.T) {
	got, err := ResolveWindow(WindowRequest{
		IncidentStart: winNow.Add(2 * time.Hour),
		Now:           winNow,
	})
	if err != nil {
		t.Fatalf("ResolveWindow 返回错误: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("事故开始时间在将来时推导出的窗口不合法: %v（窗口 %v）", err, got)
	}
}

// TestResolveWindowIsDeterministic 同一输入必须给出同一结果。
//
// 窗口会被写进证据的 metadata 与 timeline 留痕；
// 若推导含随机性或依赖 map 遍历，同一次采集在库里会留下两个不同的窗口，
// 而复核时看不出哪个是真的。
func TestResolveWindowIsDeterministic(t *testing.T) {
	req := WindowRequest{IncidentStart: winAnchor, Now: winNow}
	first, err := ResolveWindow(req)
	if err != nil {
		t.Fatalf("首次推导失败: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := ResolveWindow(req)
		if err != nil {
			t.Fatalf("第 %d 次推导失败: %v", i, err)
		}
		if !got.Start.Equal(first.Start) || !got.End.Equal(first.End) {
			t.Fatalf("第 %d 次窗口 = [%v, %v)，首次 = [%v, %v) —— 推导不确定",
				i, got.Start, got.End, first.Start, first.End)
		}
	}
}
