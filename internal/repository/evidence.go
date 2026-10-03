package repository

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

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

// typeToFilter 把可选类型转成 sqlc 的可空参数。
//
// 空类型 → Valid=false 的 Text，SQL 里 `IS NULL OR ...` 短路，
// 走纯 incident_id 路径。刻意不传空串：那会变成
// `type = ”` 从而永远匹配不到任何行。
func typeToFilter(t evidence.Type) pgtype.Text {
	if t == "" {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: string(t), Valid: true}
}

// confidenceToPG 把可选置信度转成 pgtype.Numeric。
//
// nil（未评估，OPEN-M4-1）→ Valid=false，即 SQL NULL ——
// **不能写 0**：「未评估」与「置信度 0」在页面上必须能区分，
// 前者是不确定性，后者是「这条证据明确没用」。
//
// **不能用 pgtype.Numeric.Scan(float64)**：实测该方法对 float64
// 一律返回 `cannot scan float64`（numeric 的 Scan 接受 string/[]byte/
// 整数类型，不接受 float64）。而我最初写成「Scan 失败就退化成未评估」，
// 结果是**所有置信度都被静默写成 NULL** —— 证据照样落库、
// 读写都不报错，只有真库跑往返才看得出置信度全丢了。
//
// 直接构造 Int/Exp 而不走 Scan：numeric 的十进制尾数用 big.Int 存，
// 0.75 = 75 × 10^-2，规律是 `Int = 原值去掉小数点`、`Exp = -小数位数`。
// 走字符串 → Scan 也能过，但多一次 strconv；且真正需要绕开的是
// 「Scan 的支持类型列表」这个隐藏契约，不是性能。
func confidenceToPG(c *float64) pgtype.Numeric {
	if c == nil {
		return pgtype.Numeric{Valid: false}
	}
	// 置信度是 0–1 的小数值（Evidence.Validate 已保证），
	// 用 %.4f 定到四位小数即可 —— 表列本身是 DECIMAL(5,4)，
	// 多写的位数会被 PG 舍入，不会溢出。
	s := strconv.FormatFloat(*c, 'f', 4, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	intPart, fracPart, _ := strings.Cut(s, ".")
	// 去掉前导零："0.7500" -> intPart="0"，
	// 而 big.Int 期望 "0" 而不是 "0000"（后者是合法但多余的表示）。
	trimmed := strings.TrimLeft(intPart, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	digits := trimmed + fracPart
	// 去掉尾部零以缩短 big.Int："0.7500" -> "07500" -> "7500"。
	//
	// **每去掉一个零，指数必须同步 +1**，否则值会缩 10 倍：
	// "0.25" -> digits "025" -> "25"（去掉 1 个零），
	// 若 Exp 仍是 -2 就成了 25×10^-2 = 0.25… 的十分之一 = 0.0025。
	// 尾零与指数是同一个数的两种表示，必须一起动。
	exp := -int32(len(fracPart))
	for len(digits) > 1 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		exp++
	}

	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		// 理论不可达（FormatFloat 的输出必是十进制数字）。
		// 走到这里宁可写「未评估」也不能写错数字 ——
		// 错的置信度比没有置信度危害更大。
		return pgtype.Numeric{Valid: false}
	}
	if neg {
		n.Neg(n)
	}
	return pgtype.Numeric{Int: n, Exp: exp, Valid: true}
}

// textFromString 把字符串转成 pgtype.Text。
//
// 与 convert.go 的 textOrEmpty（pgtype.Text → string）方向相反，
// 故用不同名—— 同名反义的函数是最难查的一类错误。
//
// query 与 value 在表里都是可空 TEXT，但我们的写入路径永远给非 NULL ——
// 采集器产出的 Evidence 这两个字段要么有值要么是空串，
// 让「NULL」只出现在手工插入的数据上，读取时就不必处理第三种情况。
func textFromString(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

// toStoredEvidence 把 db 行转成领域视图。
func toStoredEvidence(row *db.Evidence) evidence.StoredEvidence {
	out := evidence.StoredEvidence{
		ID:         row.ID,
		IncidentID: row.IncidentID,
		Type:       evidence.Type(row.Type),
		Source:     row.Source,
		Query:      row.Query.String,
		Value:      row.Value.String,
		Metadata:   evidence.UnmarshalMetadata(row.Metadata),
	}
	if row.Timestamp.Valid {
		out.Timestamp = row.Timestamp.Time
	}
	if row.CreatedAt.Valid {
		out.CreatedAt = row.CreatedAt.Time
	}
	if row.Confidence.Valid {
		if f, err := row.Confidence.Float64Value(); err == nil {
			out.Confidence = f.Float64
			out.Valid = true
		}
	}
	return out
}
