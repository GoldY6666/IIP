package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/task"
	"github.com/yu/iip/internal/testsupport"
	"github.com/yu/iip/internal/worker"
)

// 本文件是 M4 B8 的**真库集成用例**：事件 → 任务入队 → worker 领取 →
// 采集 → 证据落库 → 游标推进 → 再次入队只采增量。
//
// 为什么必须真库，四条都只能真库验：
//  1. `window_end` 列的写入与读取（0007 迁移），以及 `LastCollectWindowEnd`
//     的 ORDER BY window_end DESC —— 内存替身给不出「排序依据选错」这类错误。
//  2. `CompleteTask` 的 narg NULL 语义：传零值时**不能**把游标写成 NULL，
//     而传真实值时必须写进去。这是「首次采集 vs 后续采集」的分岔点。
//  3. 游标与 payload 的一致性：游标在 tasks 表，payload 也在 tasks 表，
//     两者由不同的 SQL 维护，改一处忘另一处只有真库能看出来。
//  4. CollectTargets 复用 GetIncidentByID —— 「事件不存在」必须映射成
//     哨兵错误而不是零值 Target，内存替身天然给不出 ErrNoRows。
//
// 采集器用内存替身（而非 mock server）：本文件验的是**队列与游标的链路**，
// 采集器自身的解析逻辑由 internal/collectors 的 mock server 用例覆盖，
// 在这里再起 HTTP server 只会让用例变慢而不增加覆盖。

// m4Base 是本文件用的固定时间基准。
var m4Base = time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)

// m4IncidentStart 是造事件时的 started_at。
var m4IncidentStart = m4Base.Add(-30 * time.Minute)

// countingStore 包装 Tasks，记录 Complete 的调用参数。
//
// 为什么要包一层：Complete 是否写了游标，直接查库也能验；
// 但这里还要验「零值时不写」这个分支 —— 那个分支在库里表现为
// window_end IS NULL，与「根本没调用 Complete」无法区分。
type countingStore struct {
	*Tasks
	completed []completeCall
}

type completeCall struct {
	id        int64
	windowEnd time.Time
}

func (c *countingStore) Complete(ctx context.Context, id int64, windowEnd time.Time) error {
	c.completed = append(c.completed, completeCall{id: id, windowEnd: windowEnd})
	return c.Tasks.Complete(ctx, id, windowEnd)
}

// fakeCollector 产出可预测的证据。
type fakeCollector struct {
	src string
	n   int
}

func (f fakeCollector) Source() evidence.Source { return f.src }

func (f fakeCollector) Collect(_ context.Context, in evidence.Input) ([]evidence.Evidence, error) {
	out := make([]evidence.Evidence, 0, f.n)
	for i := 0; i < f.n; i++ {
		// 时间戳落在**被请求的窗口内**且各不相同 —— 若时间戳相同，
		// 证据在库里无法区分，而「同一批证据被采了两遍」这类缺陷
		// 恰好是靠时间戳差异才能看出来的。
		ts := in.Window.Start.Add(time.Duration(i+1) * in.Window.Duration() / time.Duration(f.n+1))
		out = append(out, evidence.Evidence{
			Type: evidence.TypeMetric, Source: f.src,
			Query: fmt.Sprintf(`{service=%q} metric%d`, in.Service, i),
			// 值里带上窗口起点：两次采集的窗口不同，值就该不同。
			// 断言可以直接比对值，而不必反推窗口。
			Value:     fmt.Sprintf("窗口起点=%s", in.Window.Start.UTC().Format(time.RFC3339)),
			Timestamp: ts.UTC(),
		})
	}
	return out, nil
}

func newM4Processor(t *testing.T, pool *pgxpool.Pool, reg *evidence.Registry, now time.Time) (*worker.Processor, *countingStore) {
	t.Helper()
	store := &countingStore{Tasks: NewTasks(pool)}
	p := worker.NewProcessor(
		store, NewEvidence(pool), NewCollectTargets(pool), reg,
		NewLifecycle(pool),
		worker.Config{Batch: 10, MaxPerIncident: 50, ClaimedBy: "test-worker"},
		nil,
	)
	p.SetClock(func() time.Time { return now })
	return p, store
}

func insertM4Incident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, title, service string, startedAt time.Time) int64 {
	t.Helper()
	return insertIncidentFull(t, ctx, pool, title, "critical", "open", service, startedAt)
}

// TestM4EnqueueThenCollectThenCursor 完整链路：
// 入队 → 领取 → 采集 → 落库 → 游标写入。
//
// 这是 B8 的核心断言。缺任何一环，M4 的 DoD 第 4 条
// 「事件创建后 2 分钟内≥5 条证据」都不成立。
func TestM4EnqueueThenCollectThenCursor(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	inc := insertM4Incident(t, ctx, pool, "M4 链路事件", "checkout", m4IncidentStart)

	tasks := NewTasks(pool)
	// 第一步：入队（首次，游标为零值）。
	if err := tasks.EnqueueEvidence(ctx, inc); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	// 入队后应恰好一行 pending，且 payload 带 incident_id。
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE kind = 'collect_evidence' AND status = 'pending'`,
	).Scan(&cnt); err != nil {
		t.Fatalf("统计任务失败: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("pending 任务 %d 条，期望 1", cnt)
	}

	// 第二步：worker 跑一轮。
	reg := evidence.NewRegistry()
	reg.MustRegister(fakeCollector{src: "prometheus", n: 3})
	reg.MustRegister(fakeCollector{src: "loki", n: 2})
	proc, store := newM4Processor(t, pool, reg, m4Base)

	n, err := proc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}
	if n != 1 {
		t.Fatalf("处理任务数 = %d，期望 1", n)
	}

	// 第三步：证据落库 5 条。
	evRepo := NewEvidence(pool)
	list, err := evRepo.ListByIncident(ctx, inc, "", 100)
	if err != nil {
		t.Fatalf("读证据失败: %v", err)
	}
	if len(list.Items) != 5 {
		t.Fatalf("证据 %d 条，期望 5（Prometheus 3 + Loki 2）", len(list.Items))
	}
	if list.Total != 5 {
		t.Errorf("证据真计数 = %d，期望 5 —— Total 必须是真值而非 len(Items)", list.Total)
	}
	if list.Truncated {
		t.Error("5 条证据不该被标记为截断")
	}
	// query 字段必须可复制复现（DoD 第 2 条）。
	for i, e := range list.Items {
		if e.Query == "" {
			t.Errorf("第 %d 条证据 query 为空 —— 无法复现采集", i)
		}
		if e.Timestamp.IsZero() {
			t.Errorf("第 %d 条证据时间为空", i)
		}
	}

	// 第四步：游标写入，且等于本轮窗口终点。
	if len(store.completed) != 1 {
		t.Fatalf("Complete 被调用 %d 次，期望 1", len(store.completed))
	}
	if !store.completed[0].windowEnd.Equal(m4Base) {
		t.Errorf("Complete 传入的窗口终点 = %v，期望 %v（= now）",
			store.completed[0].windowEnd, m4Base)
	}

	cursor, err := tasks.LastWindowEnd(ctx, inc)
	if err != nil {
		t.Fatalf("读取游标失败: %v", err)
	}
	if !cursor.Equal(m4Base) {
		t.Errorf("库里的游标 = %v，期望 %v", cursor, m4Base)
	}

	// 任务已是终态。
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id = $1`, store.completed[0].id).Scan(&status); err != nil {
		t.Fatalf("读任务状态失败: %v", err)
	}
	if status != string(task.StatusDone) {
		t.Errorf("任务状态 = %q，期望 %q", status, task.StatusDone)
	}
}

// TestM4SecondEnqueueOnlyCollectsIncrement 第二次入队必须只采增量。
//
// 这是「同一事件被重复采集」的核心防线：新告警合入会触发第二轮采集，
// 若窗口又从事故起点开始，同一批指标会以完全相同的
// (source, query, timestamp) 落库两份，页面上出现重复证据。
func TestM4SecondEnqueueOnlyCollectsIncrement(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	inc := insertM4Incident(t, ctx, pool, "M4 增量事件", "checkout", m4IncidentStart)

	tasks := NewTasks(pool)
	first := m4Base
	reg := evidence.NewRegistry()
	reg.MustRegister(fakeCollector{src: "prometheus", n: 2})

	// ---- 第一轮 ----
	if err := tasks.EnqueueEvidence(ctx, inc); err != nil {
		t.Fatalf("首次入队失败: %v", err)
	}
	proc1, _ := newM4Processor(t, pool, reg, first)
	if _, err := proc1.RunOnce(ctx); err != nil {
		t.Fatalf("首轮 RunOnce 失败: %v", err)
	}

	// ---- 第二轮：时刻推后 10 分钟 ----
	second := first.Add(10 * time.Minute)
	// 首轮已把任务标成 done，EnqueueIfAbsent 只在「无未完成任务」时才插，
	// 因此这里不需要额外释放 —— 加一句 releaseAllTasks 反而会让用例
	// 在「首轮没成功」时静默插进第二轮，掩盖首轮的失败。
	if err := tasks.EnqueueEvidence(ctx, inc); err != nil {
		t.Fatalf("二次入队失败: %v", err)
	}

	// 新任务的 payload 必须带上轮的游标。
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM tasks WHERE kind = 'collect_evidence' AND status = 'pending' LIMIT 1`,
	).Scan(&payload); err != nil {
		t.Fatalf("读任务参数失败: %v", err)
	}
	var pl task.CollectEvidencePayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		t.Fatalf("解析任务参数失败: %v（payload=%s）", err, payload)
	}
	if !pl.WindowStart.Equal(first) {
		t.Errorf("第二轮的游标 = %v，期望 %v（= 上轮窗口终点）", pl.WindowStart, first)
	}

	proc2, _ := newM4Processor(t, pool, reg, second)
	if _, err := proc2.RunOnce(ctx); err != nil {
		t.Fatalf("第二轮 RunOnce 失败: %v", err)
	}

	// ---- 断言：第二轮采到的证据窗口起点是游标，不是事故起点 ----
	evRepo := NewEvidence(pool)
	list, err := evRepo.ListByIncident(ctx, inc, "", 100)
	if err != nil {
		t.Fatalf("读证据失败: %v", err)
	}
	if len(list.Items) != 4 {
		t.Fatalf("证据 %d 条，期望 4（两轮各 2 条）", len(list.Items))
	}

	// 按 value 里的窗口起点分组核对。
	//
	// 断言「恰好两个不同窗口」而不是「第二个窗口等于游标」：
	// 后者需要先知道证据的返回顺序，而顺序由 created_at DESC 决定 ——
	// 一旦排序改了断言就会假失败。前者与顺序无关，且同样能抓住
	// 「两轮采了同一区间」（那时只有一个不同的起点）。
	starts := map[string]int{}
	for _, e := range list.Items {
		starts[e.Value]++
	}
	if len(starts) != 2 {
		t.Fatalf("证据落在 %d 个不同窗口，期望 2 —— 两轮应采不同区间，实得 %v",
			len(starts), starts)
	}
	// 且第二个窗口必须是游标起点（= 第一轮的终点），否则第二轮等于白采。
	wantSecond := fmt.Sprintf("窗口起点=%s", first.UTC().Format(time.RFC3339))
	if starts[wantSecond] == 0 {
		t.Errorf("没有任何证据的窗口起点等于游标 %v（期望 value=%q），实得 %v —— "+
			"第二轮没有只采增量", first.UTC().Format(time.RFC3339), wantSecond, starts)
	}
	// 第一轮仍应从事故起点前 1h 开始（这是首轮回补的语义，不能被游标逻辑污染）。
	wantFirst := fmt.Sprintf("窗口起点=%s", m4IncidentStart.Add(-time.Hour).Format(time.RFC3339))
	if starts[wantFirst] == 0 {
		t.Errorf("没有证据落在首轮窗口 %q，实得 %v", wantFirst, starts)
	}
}

// TestM4CompleteWithZeroWindowEndDoesNotWriteCursor 传零值时不写游标。
//
// 写成零值（0001-01-01）会让下一轮的窗口从 1970 年开始 ——
// 那是一次能跑完但会把整个历史重采一遍的查询。
func TestM4CompleteWithZeroWindowEndDoesNotWriteCursor(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	inc := insertM4Incident(t, ctx, pool, "M4 零值游标事件", "checkout", m4IncidentStart)

	tasks := NewTasks(pool)
	payload, err := task.NewCollectEvidencePayload(inc, time.Time{})
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}
	tk, err := tasks.Enqueue(ctx, task.KindCollectEvidence, payload)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if _, err = tasks.Claim(ctx, 1, "w1"); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if err = tasks.Complete(ctx, tk.ID, time.Time{}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}

	var null bool
	if err = pool.QueryRow(ctx,
		`SELECT window_end IS NULL FROM tasks WHERE id = $1`, tk.ID).Scan(&null); err != nil {
		t.Fatalf("读 window_end 失败: %v", err)
	}
	if !null {
		t.Error("传零值时 window_end 被写成了非 NULL —— 下一轮会从 1970 年开始采")
	}

	cursor, err := tasks.LastWindowEnd(ctx, inc)
	if err != nil {
		t.Fatalf("读游标失败: %v", err)
	}
	if !cursor.IsZero() {
		t.Errorf("游标 = %v，期望零值（当作从未采过）", cursor)
	}
}

// TestM4LastWindowEndIgnoresNonDoneTasks 未完成的任务不能提供游标。
//
// pending/in_flight 还没采完，dead 是采失败了 —— 拿它们的游标当起点，
// 会让下一轮跳过还没采过的区间，那段数据就永远丢了。
func TestM4LastWindowEndIgnoresNonDoneTasks(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	inc := insertM4Incident(t, ctx, pool, "M4 游标过滤事件", "checkout", m4IncidentStart)

	tasks := NewTasks(pool)
	for _, status := range []string{"pending", "in_flight", "dead"} {
		payload, err := task.NewCollectEvidencePayload(inc, time.Time{})
		if err != nil {
			t.Fatalf("构造参数失败: %v", err)
		}
		if _, err := tasks.Enqueue(ctx, task.KindCollectEvidence, payload); err != nil {
			t.Fatalf("入队失败: %v", err)
		}
		// 参数从 $1 起编号、且都显式 Cast：
		// ① 无 Cast 时 PG 无法推断只出现在 SET 右侧的参数类型，
		//    报 42P18「could not determine data type of parameter $1」；
		// ② 若从 $2 起编号而 $1 在语句里缺席，PG 同样报 42P18 ——
		//    参数编号不允许有空洞，哪怕客户端确实按序传了值。
		// ①是 B1 那三处 SQL 类型推断缺陷的同源问题，只是这次在测试里。
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET status = $1::text, window_end = $2::timestamptz WHERE id = (
			     SELECT id FROM tasks WHERE kind='collect_evidence' AND status='pending'
			     ORDER BY id DESC LIMIT 1)`,
			status, m4Base); err != nil {
			t.Fatalf("改状态失败: %v", err)
		}
	}

	cursor, err := tasks.LastWindowEnd(ctx, inc)
	if err != nil {
		t.Fatalf("读游标失败: %v", err)
	}
	if !cursor.IsZero() {
		t.Errorf("游标 = %v，期望零值 —— pending/in_flight/dead 都不该提供游标", cursor)
	}
}

// TestM4EnqueueEvidenceIsIdempotent 同一事件重复入队不产生多行。
//
// 关联路径会对每次合入都调 EnqueueEvidence，而一批告警里可能有多条
// 合入同一个事件。若不去重，队列会被同一个事件的任务填满。
func TestM4EnqueueEvidenceIsIdempotent(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	inc := insertM4Incident(t, ctx, pool, "M4 幂等事件", "checkout", m4IncidentStart)

	tasks := NewTasks(pool)
	for i := 0; i < 5; i++ {
		if err := tasks.EnqueueEvidence(ctx, inc); err != nil {
			t.Fatalf("第 %d 次入队失败: %v", i+1, err)
		}
	}

	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE kind = 'collect_evidence'`).Scan(&cnt); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if cnt != 1 {
		t.Errorf("任务 %d 行，期望 1 —— 重复入队未被幂等拦下", cnt)
	}
}

// TestM4CollectTargetsReadsServiceAndStart 采集锚点读取正确。
func TestM4CollectTargetsReadsServiceAndStart(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	inc := insertM4Incident(t, ctx, pool, "M4 锚点事件", "payment-api", m4IncidentStart)

	targets := NewCollectTargets(pool)
	got, err := targets.Target(ctx, inc)
	if err != nil {
		t.Fatalf("读取采集锚点失败: %v", err)
	}
	if got.Service != "payment-api" {
		t.Errorf("Service = %q，期望 %q", got.Service, "payment-api")
	}
	if !got.StartedAt.Equal(m4IncidentStart) {
		t.Errorf("StartedAt = %v，期望 %v", got.StartedAt, m4IncidentStart)
	}
}

// TestM4CollectTargetsMissingIncidentIsError 事件不存在必须报错。
//
// 返回零值 Target 会让调用方推导出退化窗口，把「事件不存在」
// 伪装成「采集不到数据」—— 后者让用户去查数据源，而真因是数据没了。
func TestM4CollectTargetsMissingIncidentIsError(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewCollectTargets(pool)

	// 用一个肯定不存在的 id：测试库是本用例独占 schema，序列从 1 起。
	_, err := repo.Target(context.Background(), 999999)
	if err == nil {
		t.Fatal("事件不存在时应报错")
	}
	if !errors.Is(err, incident.ErrIncidentNotFound) {
		t.Errorf("错误 = %v，期望包装 incident.ErrIncidentNotFound（调用方据此判可重试）", err)
	}
}

// TestM4ProcessorWithRealCollectorsEndToEnd 走真采集器（内存 registry，
// 但用真的 evidence.Apply 预算与真库落库）验证「一个采集器失败不影响另一个」。
//
// 与 internal/worker 的编排单测互补：那条验规则，这条验规则在真库上
// 真的能落库（metadata 的 JSONB 往返、CHECK 约束、事务原子性）。
func TestM4ProcessorWithRealCollectorsEndToEnd(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	inc := insertM4Incident(t, ctx, pool, "M4 端到端事件", "checkout", m4IncidentStart)

	tasks := NewTasks(pool)
	if err := tasks.EnqueueEvidence(ctx, inc); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	reg := evidence.NewRegistry()
	reg.MustRegister(fakeCollector{src: "prometheus", n: 3})
	// 第二个采集器恒失败，模拟数据源宕机。
	reg.MustRegister(failingCollector{src: "loki"})

	proc, _ := newM4Processor(t, pool, reg, m4Base)
	if _, err := proc.RunOnce(ctx); err != nil {
		t.Fatalf("一个采集器失败却让整轮失败: %v", err)
	}

	list, err := NewEvidence(pool).ListByIncident(ctx, inc, "", 100)
	if err != nil {
		t.Fatalf("读证据失败: %v", err)
	}
	if len(list.Items) != 3 {
		t.Errorf("证据 %d 条，期望 3（失败的采集器不影响其余）", len(list.Items))
	}

	// 时间线必须有 evidence_found 留痕。
	var evType string
	if err := pool.QueryRow(ctx,
		`SELECT event_type FROM timeline WHERE incident_id = $1 ORDER BY id DESC LIMIT 1`, inc,
	).Scan(&evType); err != nil {
		t.Fatalf("读时间线失败: %v", err)
	}
	if evType != "evidence_found" {
		t.Errorf("最新留痕 event_type = %q，期望 %q", evType, "evidence_found")
	}
}

// failingCollector 恒失败的采集器。
type failingCollector struct{ src string }

func (f failingCollector) Source() evidence.Source { return f.src }
func (f failingCollector) Collect(context.Context, evidence.Input) ([]evidence.Evidence, error) {
	return nil, fmt.Errorf("%s 不可达", f.src)
}
