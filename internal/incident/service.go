package incident

import (
	"context"
	"time"
)

// Transactor 在 Store 之上提供事务边界。
//
// 为什么必须有它：状态更新与 timeline 写入是**一件事的两半**——只改状态不留痕，
// 事后无法追溯是谁在什么时候改的；只留痕不改状态，页面会显示一条并不存在的流转。
// 分两次调用让「一半成功」成为可能，故必须包进同一事务。
type Transactor interface {
	Store
	// InTx 在一个事务内执行 fn；fn 返回错误则回滚，否则提交。
	InTx(ctx context.Context, fn func(Store) error) error
}

// Service 提供事件生命周期的处置能力（M3 T3.2）。
//
// 依赖只取 Transactor 一个接口：服务层不认识 pgx，也不认识 HTTP，
// 于是状态流转的全部规则都能在不起数据库、不起服务的情况下被测到。
type Service struct {
	tx Transactor
	// now 是可替换的时钟。默认 time.Now；测试注入固定值后，
	// 「timeline 事件时间」与「resolved_at」才能被精确断言而不是「大约此刻」。
	now func() time.Time
}

// NewService 构造处置服务。
func NewService(tx Transactor) *Service {
	return &Service{tx: tx, now: time.Now}
}

// Transition 执行一次状态流转，返回流转后的最新快照。
//
// 全流程都在一个事务内完成（读当前状态 → 判合法性 → 改状态 → 写留痕 → 回读）：
//   - 读与写放在同一事务，是为了避免「读到 open、写时已是 resolved」的检查时序漏洞；
//     即便如此仍保留 `status = from` 的乐观条件，因为两个并发事务可能同时读到 open。
//   - 非法流转返回 *InvalidTransitionError（API 层映射 422）；
//     并发冲突返回 *ConflictError（409）；事件不存在返回 ErrIncidentNotFound（404）。
func (s *Service) Transition(ctx context.Context, id int64, to Status, actor string) (Snapshot, error) {
	var out Snapshot
	err := s.tx.InTx(ctx, func(st Store) error {
		snap, err := st.GetByID(ctx, id)
		if err != nil {
			return err
		}
		// 用独立变量名而非复用 err：这里的 err 只在 if 内有效，
		// 复用会触发 gocritic 的 sloppyReassign，遮蔽又会让外层错误被吞。
		if transErr := NewTransitionError(snap.Status, to); transErr != nil {
			return transErr
		}

		entry, err := NewTransitionEntry(id, snap.Status, to, actor, s.now())
		if err != nil {
			return err
		}

		ok, err := st.UpdateStatus(ctx, UpdateStatusArg{
			ID:     id,
			From:   snap.Status,
			To:     to,
			Effect: ResolveAtEffect(snap.Status, to),
		})
		if err != nil {
			return err
		}
		if !ok {
			// 影响 0 行 = 有别的请求抢先改了状态。不重试：
			// 重试等于替调用方猜一个新状态，而新状态该由人来决定。
			return &ConflictError{ID: id, Expected: snap.Status}
		}

		// 留痕与状态更新同事务：这里返回错误会让整个事务回滚，
		// 从而不会出现「状态变了却没留痕」的幽灵流转。
		if appendErr := st.AppendTimelineEntry(ctx, entry); appendErr != nil {
			return appendErr
		}

		// 回读而不是自己拼返回值：resolved_at / updated_at 由数据库写入，
		// 只有读回来才能保证返回给调用方的是真实落库值。
		out, err = st.GetByID(ctx, id)
		return err
	})
	if err != nil {
		return Snapshot{}, err
	}
	return out, nil
}
