package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/testsupport"
)

// TestEnsureAlertPartitionsSurvivesConcurrentCreate 钉死 0010 的修复：
// 并发补建同一个月分区时，落败者不得把 42P07 当成失败抛出。
//
// 根因链（CI 首跑 run#4，2026-10-19）：
//
//	repository.Insert 的告警 starts_at 落在预建范围外（如测试库的
//	2026-09-24）→ 插入撞 23514 → ensurePartitionFor 调
//	iip_ensure_alert_partitions() 补建 → 8 个并发插入同时补建同一个月，
//	落败者的 pg_class 存在性检查看不到对方**未提交**的建表（快照隔离），
//	CREATE 阻塞到对方提交后撞 42P07 → 被当成「补建失败」→ 整个 Insert 报错。
//	CI 的 -race 拖慢时序放大竞争窗口；本机无 cgo 跑不了 -race 故复现不出。
//
// 判据的**两个方向**（缺一个就是恒绿或恒红）：
//
//	反向（先跑）：把 0010 down 掉、函数体还原成 0008 原版，同一竞争
//	  harness 下必须报 42P07 —— 证明竞争真的发生了、harness 有鉴别力。
//	  没有这条，正向的绿可能只是「竞争根本没发生」。
//	正向（后跑）：重新 up 0010，同一竞争下必须成功，且只把未建月
//	  记入返回值 —— 证明异常兜底没有把循环和记账一起吞掉。
//
// 为什么用 down/up 而不是在测试里贴一份旧函数体：顺便把 0010 的 down
// 「精确还原 0008」也钉住；代价是本用例假设 0010 是最新迁移（见下）。
func TestEnsureAlertPartitionsSurvivesConcurrentCreate(t *testing.T) {
	// 本用例钉死「0010 是最新迁移」：mustDownTo(t, m, 9) 的语义是
	// 「回滚到 0009 之后」，只有 0010 之上没有别的迁移时，它才恰好只
	// 回滚 0010（把函数体还原成 0008 原版）。新增 0011 后必须先更新
	// 这个版本目标，否则回滚的不是要测的那个迁移——那会让反向判据
	// 静默失效（阶段 1 测的还是新函数，永不报错，直接红在这里说清楚）。
	if v := latestVersion(t); v != 10 {
		t.Fatalf("本用例假设 0010 是最新迁移，实际最新为 %d；请更新 mustDownTo 的版本目标后再跑", v)
	}

	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)
	mustUp(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	defer pool.Close()

	now := time.Now().UTC()

	// ---------- 阶段 1（反向判据）：旧函数在竞争下必报 42P07 ----------
	// 月份取相对值（前 2 个月）：与跑测试的日历时间无关。迁移只预建
	// [当月, 当月+2]，前几个月的分区必然不存在 ⇒ 补建路径一定被走到。
	mustDownTo(t, m, 9) // 回滚 0010：函数体回到 0008 原版（无 duplicate_table 兜底）
	oldFrom := time.Date(now.Year(), now.Month(), 15, 0, 0, 0, 0, time.UTC).AddDate(0, -2, 0)
	_, err = runPartitionCreateRace(ctx, t, pool, oldFrom)
	if err == nil {
		t.Fatal("旧函数（0008 原版）在并发补建竞争下未报错：harness 没有复现竞争，阶段 2 的绿将不可信")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("旧函数竞争错误不是 *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "42P07" {
		t.Fatalf("旧函数竞争错误应为 42P07 duplicate_table，实际 %s（%v）——CI run#4 的失败形态就是这个码", pgErr.Code, err)
	}

	// ---------- 阶段 2（正向判据）：新函数同竞争下必须成功且记账正确 ----------
	mustUp(t, m) // 重新应用 0010：异常兜底在位
	newFrom := time.Date(now.Year(), now.Month(), 15, 0, 0, 0, 0, time.UTC).AddDate(0, -3, 0)
	names, err := runPartitionCreateRace(ctx, t, pool, newFrom)
	if err != nil {
		t.Fatalf("新函数（0010）在并发补建竞争下报错: %v", err)
	}
	// 期望只新建「前 1 个月」一个分区：
	//   前 3 个月 = 竞争月（holder 未提交占位 ⇒ 静默跳过，不记账）；
	//   前 2 个月 = 阶段 1 的 holder 已建（跳过）；
	//   前 1 个月 = 首次出现 ⇒ 新建并记账；
	//   当月 ~ 当月+2 = 迁移预建（跳过）。
	// 这条断言同时防两种坏修法：把整个循环 break 掉（names 为空）、
	// 把异常分支的 created 记账也留着（names 会含竞争月）。
	want := "alerts_" + time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006_01")
	if len(names) != 1 || names[0] != want {
		t.Errorf("新函数竞争后应只新建 %s（竞争月静默跳过、循环继续、记账只记真新建），实际 %v", want, names)
	}
}

// runPartitionCreateRace 复现 CI run#4 的竞争形态，返回 racer 的调用结果。
//
// 竞争双方（对应 CI 里 8 个并发插入中的「先行者」与「落败者」）：
//
//	holder —— 未提交事务里手工建占位分区（先行者；未提交 ⇒ 对 racer 不可见）；
//	racer  —— 经生产同一条 sqlc 路径并发调 EnsureAlertPartitions（落败者）。
//
// 流程：holder 建占位（不提交）→ 等 racer 确认进入等锁 → holder 提交 →
// racer 的 CREATE 解除阻塞后撞上已提交的同名分区（旧函数报 42P07，
// 新函数静默跳过并继续处理后续月份）。
//
// 为什么必须「确认等锁再提交」：不等待就提交，racer 可能还没走到 CREATE，
// 提交后它的存在性检查反而看见分区、直接跳过——竞争根本没发生，
// 「旧函数必红」这条反向判据会假红（harness 失效而非产品缺陷）。
func runPartitionCreateRace(ctx context.Context, t *testing.T, pool *pgxpool.Pool, fromTs time.Time) ([]string, error) {
	t.Helper()

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("获取 holder 连接失败: %v", err)
	}
	defer holder.Release()
	racer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("获取 racer 连接失败: %v", err)
	}
	defer racer.Release()
	watcher, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("获取 watcher 连接失败: %v", err)
	}
	defer watcher.Release()

	var racerPID uint32
	// 复用外层 err（不新建）：本函数上方 pool.Acquire 已声明 err，
	// 再写 if err := ... 会触发 govet shadow。
	if err = racer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&racerPID); err != nil {
		t.Fatalf("读取 racer backend pid 失败: %v", err)
	}

	// holder：未提交事务里建占位分区。分区名与月界由 fromTs 推导
	//（'alerts_YYYY_MM' + UTC 月初，与函数口径一致），不是外部输入。
	//
	// 月界**必须内联成字面值**而不能用绑定参数：PG 的 utility 语句不认
	// 参数——服务器对 CREATE TABLE 里的 $N 返回的 ParameterDescription
	// 恒为空（参数在转换期被丢弃，实测 paramOIDs=[]），pgx 随即以
	// "mismatched param and argument count" 拒绝发送。DML 才可以用 $N
	// （watcher / racer 的查询都是 DML，参数照用）。
	first := time.Date(fromTs.Year(), fromTs.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := first.AddDate(0, 1, 0)
	part := "alerts_" + first.Format("2006_01")
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder 开事务失败: %v", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF alerts
		 FOR VALUES FROM ('%s'::timestamp AT TIME ZONE 'UTC') TO ('%s'::timestamp AT TIME ZONE 'UTC')`,
		part,
		first.Format("2006-01-02 15:04:05"),
		next.Format("2006-01-02 15:04:05"))); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("holder 建占位分区 %s 失败: %v", part, err)
	}

	// racer：并发调用（db.New(racer) 即生产路径，见 internal/db/partition.sql.go）。
	type raceResult struct {
		names []string
		err   error
	}
	done := make(chan raceResult, 1)
	go func() {
		names, err := db.New(racer).EnsureAlertPartitions(ctx, db.EnsureAlertPartitionsParams{
			FromTs:      pgtype.Timestamptz{Time: fromTs, Valid: true},
			AheadMonths: 2,
		})
		done <- raceResult{names: names, err: err}
	}()

	if err := waitSessionWaiting(ctx, watcher, racerPID, 15*time.Second); err != nil {
		// 回滚让 racer 解除阻塞（父表锁释放后它的 CREATE 会成功），
		// 再收结果，然后判红——harness 失效必须响亮地失败。
		tx.Rollback(ctx)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		t.Fatalf("racer 未进入等锁状态: %v", err)
	}

	// holder 提交：racer 的 CREATE 解除阻塞，撞上已提交的同名分区。
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("holder 提交失败: %v", err)
	}

	select {
	case r := <-done:
		return r.names, r.err
	case <-time.After(15 * time.Second):
		t.Fatal("holder 提交后 racer 15s 未返回")
		return nil, nil // 不可达（t.Fatal 已终止本 goroutine）
	}
}

// waitSessionWaiting 轮询 pg_stat_activity，直到 pid 对应会话进入
// 「active 且等锁」状态。返回 nil = 确认阻塞；超时返回错误（含最后一次
// 观测值，便于区分「harness 失效」与「产品行为变化」）。
//
// 谓词为什么是 state='active' 且 wait_event_type 非空：函数执行期间
// 后端要么在跑（wait_event_type 为空），要么阻塞在锁上。等客户端输入
// 的状态是 idle，不会误判。
//
// racer 具体阻塞在哪（2026-10-21 两会话探针实证，pg_stat_activity +
// pg_locks 双边 dump）：CREATE ... PARTITION OF 对父表 alerts 取
// **AccessExclusiveLock**（自冲突），而 holder 未提交的建表正持着它——
// racer 的 pg_locks 行是 `AccessExclusiveLock granted=f alerts`。
// 注意它**已经过了 pg_class 存在性检查**（检查时 holder 的建表未提交、
// 不可见），所以 42P07 发生在拿到父表锁之后的 CREATE 上。
func waitSessionWaiting(ctx context.Context, watcher *pgxpool.Conn, pid uint32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := "（尚未观测到该会话）"
	for time.Now().Before(deadline) {
		var state, waitType string
		err := watcher.QueryRow(ctx, `
SELECT COALESCE(state, ''), COALESCE(wait_event_type, '')
FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&state, &waitType)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("查询 pg_stat_activity 失败: %w", err)
		}
		last = fmt.Sprintf("state=%q wait_event_type=%q", state, waitType)
		if state == "active" && waitType != "" {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("%s 内未见目标会话进入等锁状态（最后一次观测: %s）", timeout, last)
}
