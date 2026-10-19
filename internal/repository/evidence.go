package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/evidence"
)

// Evidence 是 evidence.Store 的 PostgreSQL 实现。
type Evidence struct {
	// pool 只在 Save 里用到（要开事务），List/Count 走 queries。
	//
	// 刻意**同时**持有两者：db.DBTX 既能是连接池也能是事务，
	// 但 Save 必须开事务才能保证「整批要么都写要么都不写」，
	// 而 NewEvidence 允许传 DBTX 以便测试注入事务。
	pool    *pgxpool.Pool
	queries *db.Queries
}

// 编译期断言：必须满足 evidence 包定义的持久化契约。
var _ evidence.Store = (*Evidence)(nil)

// NewEvidence 构造证据仓储。conn 取 db.DBTX（可接连接池或事务）。
func NewEvidence(conn db.DBTX) *Evidence {
	e := &Evidence{queries: db.New(conn)}
	// 事务也能开事务（pgx 的 Begin 会开 savepoint），
	// 因此能拿到 *pgxpool.Pool 就用池，否则沿用传入的 DBTX。
	if p, ok := conn.(*pgxpool.Pool); ok {
		e.pool = p
	}
	return e
}

// maxListLimit 是单次查询返回条数的硬上限。
//
// 夹紧而不让调用方保证：limit 会从 HTTP query string 来，
// 而 LIMIT 负数在 PG 里是「一条都取不到」且**不报错**，
// LIMIT 极大则是把整张表拉进内存。两个都是静默失败。
const maxListLimit = 1000

// Save 落库一批证据，整批在一个事务里。
//
// 逐条 Validate 前置：非法证据（空 timestamp、非法 type）在这里就被拒，
// **不写进库再让 PG 抛 CHECK 违规** —— 后者发生在批量插入中途，
// 会把前面已写的证据一起拖下水。
//
// 返回的丢弃条数不进错误里：调用方需要区分「整个采集器挂了（该重试）」
// 与「拿到 8 条其中 1 条时间戳非法（不该重试）」。整批在此返回错误即可 ——
// 剩下的合法证据照样提交，因为它们本身没问题。
func (r *Evidence) Save(ctx context.Context, incidentID int64, list []evidence.Evidence) ([]evidence.StoredEvidence, error) {
	if err := evidence.ValidIncidentID(incidentID); err != nil {
		return nil, fmt.Errorf("repository: 证据落库失败: %w", err)
	}
	if len(list) == 0 {
		// 空批次不是错误：采集器「采到了但无内容」是正常结果（手册 §七）。
		// 开一个空事务再提交纯属浪费。
		return nil, nil
	}

	// 先全部校验，再开事务。
	//
	// 顺序反了的话，事务里会边写边发现非法证据并回滚 ——
	// 功能上没错，但错误信息会指向「事务失败」而不是「第 3 条 timestamp 为空」，
	// 排查时要多绕一层。
	type pending struct {
		row db.InsertEvidenceParams
		ev  evidence.Evidence
	}
	rows := make([]pending, 0, len(list))
	for i, e := range list {
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("repository: 第 %d 条证据非法: %w", i+1, err)
		}
		md, err := evidence.MarshalMetadata(e.Metadata)
		if err != nil {
			return nil, fmt.Errorf("repository: 第 %d 条证据 metadata: %w", i+1, err)
		}
		rows = append(rows, pending{
			row: db.InsertEvidenceParams{
				IncidentID: incidentID,
				Type:       string(e.Type),
				Source:     e.Source,
				Query:      textFromString(e.Query),
				Value:      textFromString(e.Value),
				Timestamp:  pgtype.Timestamptz{Time: e.Timestamp, Valid: true},
				Confidence: confidenceToPG(e.Confidence),
				Metadata:   md,
			},
			ev: e,
		})
	}

	tx, err := r.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := db.New(tx)
	out := make([]evidence.StoredEvidence, 0, len(rows))
	// 按索引取而非值拷贝：pending 内含一个 db.InsertEvidenceParams，
	// 值拷贝 264 字节/次，证据一多就是纯浪费。
	for i := range rows {
		got, insErr := qtx.InsertEvidence(ctx, rows[i].row)
		if insErr != nil {
			return nil, fmt.Errorf("repository: 第 %d 条证据落库失败: %w", i+1, insErr)
		}
		out = append(out, toStoredEvidence(&got))
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("repository: 证据批量落库提交失败: %w", err)
	}
	return out, nil
}

// ListByIncident 返回该事件的证据与真计数。
func (r *Evidence) ListByIncident(
	ctx context.Context, incidentID int64, typ evidence.Type, limit int,
) (evidence.EvidenceList, error) {
	if err := evidence.ValidIncidentID(incidentID); err != nil {
		return evidence.EvidenceList{}, fmt.Errorf("repository: 查询证据失败: %w", err)
	}
	if err := evidence.ValidFilterType(typ); err != nil {
		return evidence.EvidenceList{}, fmt.Errorf("repository: 查询证据失败: %w", err)
	}
	if limit <= 0 {
		limit = evidence.DefaultMaxPerIncident
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	// **多取一条探测截断**：limit+1 条说明超限了。
	// 若只取 limit 条，「刚好等于上限」与「超了一点」返回的条数完全一样，
	// 截断就会被静默吞掉 —— 而手册 §4.2 要求截断必须能被 API 读到。
	rows, err := r.queries.ListIncidentEvidence(ctx, db.ListIncidentEvidenceParams{
		IncidentID: incidentID,
		Type:       typeToFilter(typ),
		Limit:      int32(limit + 1),
	})
	if err != nil {
		return evidence.EvidenceList{}, fmt.Errorf("repository: 查询证据失败: %w", err)
	}

	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}

	// 真计数单独查，不靠「已取出的条数」推算。
	//
	// 为什么不能省这次查询：拿不到 total 就无法区分
	// 「正好 50 条」与「有 80 条只给了 50 条」，
	// 页面会把截断说成「一共 50 条」（M3 的 AlertCount 就栽在这里）。
	total, err := r.queries.CountIncidentEvidence(ctx, db.CountIncidentEvidenceParams{
		IncidentID: incidentID,
		Type:       typeToFilter(typ),
	})
	if err != nil {
		return evidence.EvidenceList{}, fmt.Errorf("repository: 统计证据条数失败: %w", err)
	}

	// total 与实际取到的条数不一致（并发落库）时以 total 为准，
	// 并据此重算 truncated —— total 才是「用户应该看到的总数」。
	if int(total) < len(rows) {
		// 取到的比统计还多：说明统计之后又被删了（ON DELETE CASCADE）。
		// 这种情况下报 total 会比实际能拿到的多，宁可少报不可多报。
		truncated = false
	}

	out := make([]evidence.StoredEvidence, 0, len(rows))
	for i := range rows {
		out = append(out, toStoredEvidence(&rows[i]))
	}
	return evidence.EvidenceList{Items: out, Total: int(total), Truncated: truncated}, nil
}

// CountByType 返回各类型的证据条数（含 0 条的类型）。
func (r *Evidence) CountByType(ctx context.Context, incidentID int64) (map[evidence.Type]int, error) {
	if err := evidence.ValidIncidentID(incidentID); err != nil {
		return nil, fmt.Errorf("repository: 统计证据类型失败: %w", err)
	}
	rows, err := r.queries.CountIncidentEvidenceByType(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("repository: 统计证据类型失败: %w", err)
	}
	counts := make(map[evidence.Type]int, len(rows))
	for _, r := range rows {
		counts[evidence.Type(r.Type)] = int(r.N)
	}
	// 补齐 0 条的类型：前端 Tab 角标必须显示 0，而不是让整个 Tab 消失 ——
	// 「没采到」与「压根没查过」在页面上必须长得不一样。
	return evidence.FillTypeCounts(counts), nil
}

// beginTx 开一个事务。
func (r *Evidence) beginTx(ctx context.Context) (pgx.Tx, error) {
	if r.pool != nil {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("repository: 开事务失败: %w", err)
		}
		return tx, nil
	}
	return nil, errors.New("repository: 构造证据仓储时未提供连接池，无法开事务")
}
