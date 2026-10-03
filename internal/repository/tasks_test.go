package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/task"
	"github.com/yu/iip/internal/testsupport"
)

// 本文件覆盖 M4 手册 §七「必测的边界」中属于仓储层的那几条。
//
// 必须真库的理由，逐条：
//   - SKIP LOCKED 是否真的不重复领取，是**数据库锁语义**的性质，
//     任何内存替身都能「假装通过」；
//   - 退避的可见性依赖 available_at 与 NOW() 的比较，同理只能在真库验；
//   - 超时回收依赖 claimed_at 的比较与 status 的原子更新。
//
// 全部经 testsupport.NewDB 拿到独立 schema 隔离。

// enqueueCollect 造一个采集任务，返回其领域对象。
func enqueueCollect(t *testing.T, repo *Tasks, incidentID int64) task.Task {
	t.Helper()
	payload, err := task.NewCollectEvidencePayload(incidentID)
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}
	tk, err := repo.Enqueue(context.Background(), task.KindCollectEvidence, payload)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	return tk
}

// 核心用例：两个 worker 并发领取 10 条任务，每条**恰好**被领一次。
//
// 这是手册 §4.1 决策 1 的唯一可观测证据，也是 T4.1 的核心断言。
// 断言用「每个 id 出现次数恰好 1」而非「总共领到 10 条」——
// 后者在「一个没领到 + 一个领了两次」时会假通过（总数仍可能是 10）。
func TestClaimDoesNotDuplicateUnderConcurrency(t *testing.T) {
	pool := testsupport.NewDB(t)
	repoA := NewTasks(pool)
	repoB := NewTasks(pool)
	ctx := context.Background()

	const total = 10
	for i := 0; i < total; i++ {
		enqueueCollect(t, repoA, int64(i+1))
	}

	// 两个 worker 同时开抢，各领 10 条（batch 给足，
	// 靠 SKIP LOCKED 而不是靠 LIMIT 制造不重叠）。
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = map[int64]int{}
	)
	start := make(chan struct{})
	for name, repo := range map[string]*Tasks{"A": repoA, "B": repoB} {
		wg.Add(1)
		go func(worker string, r *Tasks) {
			defer wg.Done()
			<-start // 让两个 goroutine 尽量同时发起，放大竞争窗口
			got, err := r.Claim(ctx, total, "host-"+worker)
			if err != nil {
				t.Errorf("worker %s 领取失败: %v", worker, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, tk := range got {
				claimed[tk.ID]++
				if tk.ClaimedBy != "host-"+worker {
					t.Errorf("任务 %d 的 claimed_by = %q，期望 %q", tk.ID, tk.ClaimedBy, "host-"+worker)
				}
				if tk.Attempts != 1 {
					t.Errorf("任务 %d 首次领取的 attempts = %d，期望 1", tk.ID, tk.Attempts)
				}
			}
		}(name, repo)
	}
	close(start)
	wg.Wait()

	// 前置条件：确实抢出了全部10 条。少了说明上面的分布是残缺的，
	// 此时「每条恰好一次」会因为没被覆盖而**假通过**。
	if len(claimed) != total {
		t.Fatalf("共领到 %d 条，期望 %d 条——领取分布不完整，后续断言无意义", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("任务 %d 被领取 %d 次，期望恰好 1 次（SKIP LOCKED 失效）", id, n)
		}
	}
}

// 退避可见性：失败后 available_at 推到未来，**立刻**再领必须领不到。
func TestFailBackoffHidesTaskUntilDue(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	tk := enqueueCollect(t, repo, 1)
	claimed, err := repo.Claim(ctx, 1, "host-1")
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("首次领取到 %d 条，期望 1 条", len(claimed))
	}

	// 退避 base=5s：立刻再领必须为空，否则「退避」形同虚设。
	backed, err := repo.Fail(ctx, tk.ID, 5*time.Second, task.DefaultMaxAttempts, "模拟失败")
	if err != nil {
		t.Fatalf("标记失败返回错误 = %v，期望 nil", err)
	}
	if backed.Status != task.StatusPending {
		t.Errorf("第 1 次失败后 status = %q，期望 %q（未达上限不该进死信）",
			backed.Status, task.StatusPending)
	}
	if backed.Attempts != 1 {
		t.Errorf("第 1 次失败后 attempts = %d，期望 1", backed.Attempts)
	}
	if !backed.AvailableAt.After(time.Now()) {
		t.Errorf("第 1 次失败后 available_at = %v，未推到未来——退避未生效", backed.AvailableAt)
	}

	got, err := repo.Claim(ctx, 10, "host-1")
	if err != nil {
		t.Fatalf("退避期内领取失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("退避期内领到 %d 条，期望 0 条——available_at 未被尊重", len(got))
	}
}

// 第 3 次失败进死信，且行**保留**（不删）。
//
// 「保留」是要断言的：删掉就看不出「曾经试过、试了几次、为什么失败」，
// 而排查证据缺失时这恰恰是唯一线索（手册 §4.1 决策 3）。
func TestThirdFailureGoesToDeadLetterAndKeepsRow(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	tk := enqueueCollect(t, repo, 7)

	// 前两次：失败后应回到 pending（可重试）。
	// 每轮都是「先把退避拨回现在 → 领取 → 失败」，
	// 顺序不能颠倒：上一轮 Fail 已把 available_at 推到未来，
	// 不拨回就领不到（那正是退避该有的行为）。
	for attempt := 1; attempt <= 2; attempt++ {
		makeAvailableNow(t, pool, tk.ID)
		claimed, err := repo.Claim(ctx, 1, "host-1")
		if err != nil {
			t.Fatalf("第 %d 次领取失败: %v", attempt, err)
		}
		if len(claimed) != 1 {
			t.Fatalf("第 %d 次领取到 %d 条，期望 1 条", attempt, len(claimed))
		}
		delay := task.BackoffDelay(claimed[0].Attempts, task.BackoffBase, task.BackoffMax)
		got, err := repo.Fail(ctx, tk.ID, delay, task.DefaultMaxAttempts, fmt.Sprintf("第 %d 次失败", attempt))
		if err != nil {
			t.Fatalf("第 %d 次标记失败返回错误 = %v，期望 nil", attempt, err)
		}
		if got.Status != task.StatusPending {
			t.Fatalf("第 %d 次失败后 status = %q，期望 %q", attempt, got.Status, task.StatusPending)
		}
	}

	// 第三次：进死信。
	makeAvailableNow(t, pool, tk.ID)
	claimed, err := repo.Claim(ctx, 1, "host-1")
	if err != nil {
		t.Fatalf("第 3 次领取失败: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("第 3 次领取到 %d 条，期望 1 条", len(claimed))
	}
	delay := task.BackoffDelay(claimed[0].Attempts, task.BackoffBase, task.BackoffMax)
	dead, err := repo.Fail(ctx, tk.ID, delay, task.DefaultMaxAttempts, "第 3 次失败")
	if err != nil {
		t.Fatalf("第 3 次标记失败返回错误 = %v，期望 nil", err)
	}
	if dead.Status != task.StatusDead {
		t.Errorf("第 3 次失败后 status = %q，期望 %q（>= 3 即判死信）", dead.Status, task.StatusDead)
	}
	if !task.ShouldDeadLetter(dead.Attempts, task.DefaultMaxAttempts) {
		t.Errorf("attempts = %d 未达死信条件，与 status=dead 自相矛盾", dead.Attempts)
	}
	if dead.LastError != "第 3 次失败" {
		t.Errorf("last_error = %q，期望保留失败原因 %q", dead.LastError, "第 3 次失败")
	}

	// 死信不可再领（终态）。
	// 刻意**只**把 available_at 拨到当前、不动 status：
	// 若用会顺带把 status 拨回 pending 的辅助函数，这条断言就变成
	// 「我把它改回 pending 后它当然不再是死信」——自证，什么都没验到。
	// 这里真正要验的是：**dead 是终态，退避到期也不会被领**。
	makeDueNow(t, pool, tk.ID)
	got, err := repo.Claim(ctx, 10, "host-1")
	if err != nil {
		t.Fatalf("死信后领取失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("死信任务被领到 %d 条，期望 0 条——终态仍可被领取", len(got))
	}

	// 行还在。
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id = $1`, tk.ID).Scan(&n); err != nil {
		t.Fatalf("统计死信行失败: %v", err)
	}
	if n != 1 {
		t.Errorf("死信行 count = %d，期望 1——死信必须保留行，删掉就看不出曾试过", n)
	}
}

// 超时回收：worker 领完崩溃（claimed_at 停在过去）→ 能被别人再领。
//
// 同时验证 attempts 继续累加：手册 §4.1 决策 4 要求回收时也推进 attempts，
// 否则「反复崩溃」的任务会永远重试而进不了死信。
func TestReclaimStaleMakesCrashedTaskClaimable(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	tk := enqueueCollect(t, repo, 42)
	if _, err := repo.Claim(ctx, 1, "host-crashed"); err != nil {
		t.Fatalf("领取失败: %v", err)
	}

	// 尚未超时：不该被回收。
	n, err := repo.ReclaimStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("回收未超时任务返回错误 = %v，期望 nil", err)
	}
	if n != 0 {
		t.Errorf("未超时的任务被回收 %d 条，期望 0 条", n)
	}

	// 把 claimed_at 拨到过去，模拟持有者崩溃。
	backdateClaim(t, pool, tk.ID, 10*time.Minute)

	n, err = repo.ReclaimStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("回收超时任务失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("回收 %d 条，期望 1 条", n)
	}

	// 回收后应立即可领（超时的任务没有退避理由）。
	got, err := repo.Claim(ctx, 1, "host-alive")
	if err != nil {
		t.Fatalf("回收后领取失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("回收后领到 %d 条，期望 1 条", len(got))
	}
	if got[0].ID != tk.ID {
		t.Errorf("领到任务 %d，期望 %d", got[0].ID, tk.ID)
	}
	if got[0].ClaimedBy != "host-alive" {
		t.Errorf("claimed_by = %q，期望 %q", got[0].ClaimedBy, "host-alive")
	}
	// attempts 累计到 2：领取时 +1，回收不+1，再领取又 +1。
	if got[0].Attempts != 2 {
		t.Errorf("回收再领后 attempts = %d，期望 2", got[0].Attempts)
	}
}

// EnqueueIfAbsent 幂等：同一事件重复入队只落一行。
func TestEnqueueIfAbsentIsIdempotent(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	payload, err := task.NewCollectEvidencePayload(99)
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}

	first, err := repo.EnqueueIfAbsent(ctx, task.KindCollectEvidence, payload)
	if err != nil {
		t.Fatalf("首次条件入队失败: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("首次条件入队应真的插入了行，实际返回零值ID")
	}

	second, err := repo.EnqueueIfAbsent(ctx, task.KindCollectEvidence, payload)
	if err != nil {
		t.Fatalf("重复条件入队返回错误 = %v，期望 nil（幂等命中不是错误）", err)
	}
	if second.ID != 0 {
		t.Errorf("重复入队返回了任务 ID %d，期望零值（应被幂等挡下）", second.ID)
	}

	// 不同事件应各入一条（否则幂等键就过宽，退化成全局只入一条）。
	otherPayload, err := task.NewCollectEvidencePayload(100)
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}
	other, err := repo.EnqueueIfAbsent(ctx, task.KindCollectEvidence, otherPayload)
	if err != nil {
		t.Fatalf("不同事件条件入队失败: %v", err)
	}
	if other.ID == 0 {
		t.Error("不同事件的条件入队应成功插行，实际被误判为重复")
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatalf("统计任务数失败: %v", err)
	}
	if n != 2 {
		t.Errorf("tasks 行数 = %d，期望 2", n)
	}
}

// Complete 命中 0 行时报ErrTaskNotFound，而不是静默成功。
//
// 这条是「别让不确定被说成确定」：任务若已被超时回收并被别人领走，
// 本 worker 的「完成」不该被当作权威结果。
func TestCompleteOnRecycledTaskReportsNotFound(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	tk := enqueueCollect(t, repo, 5)
	if _, err := repo.Claim(ctx, 1, "host-1"); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	backdateClaim(t, pool, tk.ID, time.Hour)
	if _, err := repo.ReclaimStale(ctx, 5*time.Minute); err != nil {
		t.Fatalf("回收超时任务失败: %v", err)
	}

	err := repo.Complete(ctx, tk.ID)
	if !errors.Is(err, task.ErrTaskNotFound) {
		t.Errorf("已被回收的任务 Complete 返回 %v，期望 ErrTaskNotFound", err)
	}
}

// Fail 命中 0 行时同样报 ErrTaskNotFound。
func TestFailOnRecycledTaskReportsNotFound(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	tk := enqueueCollect(t, repo, 6)
	if _, err := repo.Claim(ctx, 1, "host-1"); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	backdateClaim(t, pool, tk.ID, time.Hour)
	if _, err := repo.ReclaimStale(ctx, 5*time.Minute); err != nil {
		t.Fatalf("回收超时任务失败: %v", err)
	}

	_, err := repo.Fail(ctx, tk.ID, time.Second, task.DefaultMaxAttempts, "迟到的失败")
	if !errors.Is(err, task.ErrTaskNotFound) {
		t.Errorf("已被回收的任务 Fail 返回 %v，期望 ErrTaskNotFound", err)
	}
}

// CountByStatus 必须四个键齐全（含 0 值），否则调用方无法区分「0」与「无此项」。
func TestCountByStatusAlwaysReturnsAllFourKeys(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	// 空表：四个键都应为 0，且**都存在**。
	counts, err := repo.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("统计空表失败: %v", err)
	}
	for _, st := range []task.Status{
		task.StatusPending, task.StatusInFlight, task.StatusDone, task.StatusDead,
	} {
		v, ok := counts[st]
		if !ok {
			t.Errorf("空表时缺少 %q 的计数键——缺键与 0 含义不同", st)
		}
		if v != 0 {
			t.Errorf("空表时 %q 计数 = %d，期望 0", st, v)
		}
	}

	// 造出四种状态各一条。
	enqueueCollect(t, repo, 1)                                // pending
	enqueueCollect(t, repo, 2)                                // pending
	if _, cErr := repo.Claim(ctx, 1, "host-1"); cErr != nil { // 一条转 in_flight
		t.Fatalf("领取失败: %v", cErr)
	}

	counts, err = repo.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if counts[task.StatusPending] != 1 {
		t.Errorf("pending = %d，期望 1（共入队 2 条、领走 1 条）", counts[task.StatusPending])
	}
	if counts[task.StatusInFlight] != 1 {
		t.Errorf("in_flight = %d，期望 1", counts[task.StatusInFlight])
	}
	if counts[task.StatusDone] != 0 || counts[task.StatusDead] != 0 {
		t.Errorf("done = %d / dead = %d，期望均为 0", counts[task.StatusDone], counts[task.StatusDead])
	}
}

// --- 参数校验：不合法输入应在碰数据库前被拒 ---

func TestTasksRejectsInvalidArguments(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()
	payload, err := task.NewCollectEvidencePayload(1)
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}

	if _, err := repo.Enqueue(ctx, "", payload); !errors.Is(err, task.ErrEmptyKind) {
		t.Errorf("空 kind 入队返回 %v，期望 ErrEmptyKind", err)
	}
	if _, err := repo.Enqueue(ctx, task.KindCollectEvidence, nil); !errors.Is(err, task.ErrEmptyPayload) {
		t.Errorf("空 payload 入队返回 %v，期望 ErrEmptyPayload", err)
	}
	if _, err := repo.Claim(ctx, 0, "host-1"); err == nil {
		t.Error("batch=0 领取应报错，实际返回 nil")
	}
	if _, err := repo.Claim(ctx, 10, ""); err == nil {
		t.Error("空 claimedBy 领取应报错——库上会看不出这批任务被谁领走")
	}
	if _, err := repo.Fail(ctx, 1, -time.Second, 3, "x"); err == nil {
		t.Error("负退避时长应报错")
	}
	if _, err := repo.Fail(ctx, 1, time.Second, 0, "x"); err == nil {
		t.Error("maxAttempts=0 应报错")
	}
	if _, err := repo.ReclaimStale(ctx, 0); err == nil {
		t.Error("零超时阈值应报错——否则会把所有 in_flight 立刻回收")
	}
}

// 退避封顶在真库侧同样成立：人为把 attempts 设大，available_at 不会跑到天荒地老。
func TestBackoffIsCappedInRealDatabase(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewTasks(pool)
	ctx := context.Background()

	tk := enqueueCollect(t, repo, 3)
	if _, err := repo.Claim(ctx, 1, "host-1"); err != nil {
		t.Fatalf("领取失败: %v", err)
	}

	// 模拟「第 1 次失败后回到 pending」：必须同时把 status 拨回 pending
	// **和** 把 available_at 拨到当前。只拨 available_at 是领不到的——
	// 领取条件是 status='pending' AND available_at <= NOW()，两者缺一不可。
	// （我第一版就是这么写的，跑到「领到 0 条」才发现。）
	releaseTask(t, pool, tk.ID)
	claimed, err := repo.Claim(ctx, 1, "host-1")
	if err != nil {
		t.Fatalf("第 2 次领取失败: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("第 2 次领取到 %d 条，期望 1 条", len(claimed))
	}

	// 传入一个远超封顶的延迟，验证仓储**原样落库**、不私自改写调用方的意图。
	// 封顶本身是业务决策，放在 task.BackoffDelay 里（纯函数可测），
	// 仓储层若在这里二次夹紧，就成了「上限散落两处」的隐患。
	absurd := 365 * 24 * time.Hour
	before := time.Now()
	backed, err := repo.Fail(ctx, tk.ID, absurd, task.DefaultMaxAttempts, "超长退避")
	if err != nil {
		t.Fatalf("标记失败返回错误 = %v，期望 nil", err)
	}
	if gap := backed.AvailableAt.Sub(before); gap < absurd-time.Minute {
		t.Errorf("available_at 距今 %v，短于传入的退避 %v —— 仓储擅自改写了调用方的延迟", gap, absurd)
	}
	// attempts=2 未达上限（最大 3 次），不该进死信。
	if backed.Status != task.StatusPending {
		t.Errorf("attempts=2 后 status = %q，期望 %q —— attempts 计数或死信阈值有误",
			backed.Status, task.StatusPending)
	}

	// 封顶本身在真库侧同样成立：让 attempts 很大时，available_at 不得跑到天荒地老。
	if d := task.BackoffDelay(50, task.BackoffBase, task.BackoffMax); d != task.BackoffMax {
		t.Errorf("attempts=50 的退避 = %v，期望封顶为 %v", d, task.BackoffMax)
	}
}

// --- 测试辅助 ---

// makeAvailableNow 让任务立刻回到「可领」状态。
//
// 必须**同时**拨 status 与 available_at：领取条件是
// `status = 'pending' AND available_at <= NOW()`，两者缺一不可。
// 只拨 available_at 看起来「够用」，实际任务还停在 in_flight 领不到 ——
// 这个坑我第一版就踩了。
func makeAvailableNow(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	releaseTask(t, pool, id)
}

// releaseTask 把任务拨回 pending 且立即可领，绕过退避等待。
func releaseTask(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tasks
		    SET status = 'pending', available_at = NOW(),
		        claimed_at = NULL, claimed_by = NULL
		  WHERE id = $1`, id); err != nil {
		t.Fatalf("把任务 %d 拨回可领状态失败: %v", id, err)
	}
}

// makeDueNow 只把 available_at 拨到当前，**不动 status**。
//
// 用于验证「终态任务即便退避到期也不会被领」：如果顺带把 status 改了，
// 就等于自己把死信改回 pending，断言失去意义。
func makeDueNow(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tasks SET available_at = NOW() WHERE id = $1`, id); err != nil {
		t.Fatalf("把任务 %d 的 available_at 拨到当前失败: %v", id, err)
	}
}

// backdateClaim 把claimed_at 拨到过去，模拟持有者崩溃后无人推进。
func backdateClaim(t *testing.T, pool *pgxpool.Pool, id int64, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tasks SET claimed_at = NOW() - make_interval(secs => $2) WHERE id = $1`,
		id, d.Seconds()); err != nil {
		t.Fatalf("回拨 claimed_at 失败: %v", err)
	}
}
