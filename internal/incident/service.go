package incident

import (
	"context"
	"strings"
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

// List 取事件列表（过滤 + 分页），返回本页数据与「是否还有下一页」。
//
// 两件容易做错的事，这里都刻意做对：
//  1. **脏分页参数退化为默认值而不是报错**：列表是只读查询，参数脏不该挡掉页面加载
//     （手册 §4.3）。故 limit ≤ 0 → 默认 100，超上限 → 夹紧 1000，offset < 0 → 0。
//  2. **has_more 用 limit+1 探测**，不做 COUNT：全表 COUNT 在大表上是全扫描，
//     且返回的 total 立刻就会过期，不如给一个「还有没有」的布尔。
func (s *Service) List(ctx context.Context, f ListFilter) (ListResult, error) {
	f = normalizeListFilter(f)

	probe := f
	probe.Limit = f.Limit + 1
	if len(probe.Statuses) == 0 {
		probe.Statuses = AllStatuses()
	}
	if len(probe.Severities) == 0 {
		probe.Severities = AllSeverities()
	}

	rows, err := s.tx.List(ctx, probe)
	if err != nil {
		return ListResult{}, err
	}
	hasMore := len(rows) > f.Limit
	if hasMore {
		rows = rows[:f.Limit]
	}
	return ListResult{Items: rows, HasMore: hasMore}, nil
}

// Detail 取事件详情：本体 + 关联告警 + 时间线。
//
// 两个内联列表各按上限 +1 查询，超出即截断并把 truncated 置真——
// 与列表的 has_more 同理：宁可明确告诉前端「还有更多」，也不要静默丢数据。
func (s *Service) Detail(ctx context.Context, id int64) (Detail, error) {
	snap, err := s.tx.GetByID(ctx, id)
	if err != nil {
		return Detail{}, err
	}

	alerts, err := s.tx.Alerts(ctx, id, MaxIncidentAlerts+1)
	if err != nil {
		return Detail{}, err
	}
	truncatedAlerts := len(alerts) > MaxIncidentAlerts
	if truncatedAlerts {
		alerts = alerts[:MaxIncidentAlerts]
	}

	timeline, err := s.tx.Timeline(ctx, id, MaxTimelineRows+1)
	if err != nil {
		return Detail{}, err
	}
	truncatedTimeline := len(timeline) > MaxTimelineRows
	if truncatedTimeline {
		timeline = timeline[:MaxTimelineRows]
	}

	needsReview := 0
	for i := range alerts {
		if alerts[i].NeedsReview {
			needsReview++
		}
	}

	// 两个计数字段都必须用**真值**，不能用截断后的切片长度：
	// 上面已把 alerts 截到 200 条，拿 len(alerts) 当总数，250 条的事件会显示
	//「200 条关联告警」—— 前端据此渲染，把「只显示了前 200」说成了「一共 200 条」。
	// 列表端的 AlertCount 走 SQL count(*) 已是真值，两个端点必须一致。
	//
	// 未截断时切片长度本身就是真值，直接复用，省掉两次 count 查询 ——
	// 详情页是值守时最常打开的接口，而绝大多数事件远不到 200 条关联告警。
	alertCount := len(alerts)
	needsReviewTotal := needsReview
	if truncatedAlerts {
		total, errCount := s.tx.CountAlerts(ctx, id)
		if errCount != nil {
			return Detail{}, errCount
		}
		alertCount = total

		// 待确认数同样要查真值：被截掉的那部分里可能也有待确认的边。
		needs, errNeeds := s.tx.CountNeedsReviewAlerts(ctx, id)
		if errNeeds != nil {
			return Detail{}, errNeeds
		}
		needsReviewTotal = needs
	}

	return Detail{
		Summary: Summary{
			ID:               snap.ID,
			Title:            snap.Title,
			Severity:         snap.Severity,
			Status:           snap.Status,
			Service:          snap.Service,
			StartedAt:        snap.StartedAt,
			ResolvedAt:       snap.ResolvedAt,
			UpdatedAt:        snap.UpdatedAt,
			AlertCount:       alertCount,
			NeedsReviewCount: needsReviewTotal,
		},
		Alerts:            alerts,
		Timeline:          timeline,
		TruncatedAlerts:   truncatedAlerts,
		TruncatedTimeline: truncatedTimeline,
	}, nil
}

// Comment 给事件加一条备注；返回写入的留痕。
//
// 先读事件是为了把「事件不存在」与「留痕写失败」区分开：
// 前者是 404，后者是 503——混在一起会让前端无法判断该提示什么。
func (s *Service) Comment(ctx context.Context, id int64, actor, note string) (Entry, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return Entry{}, ErrEmptyNote
	}
	if _, err := s.tx.GetByID(ctx, id); err != nil {
		return Entry{}, err
	}

	entry := Entry{
		IncidentID:  id,
		EventType:   EventUserComment,
		Description: note,
		Actor:       actor,
		Timestamp:   s.now(),
	}
	if err := s.tx.AppendTimelineEntry(ctx, entry); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// normalizeListFilter 夹紧分页参数；规范化后可直接用于查询。
func normalizeListFilter(f ListFilter) ListFilter {
	if f.Limit <= 0 {
		f.Limit = DefaultListLimit
	}
	if f.Limit > MaxListLimit {
		f.Limit = MaxListLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
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
