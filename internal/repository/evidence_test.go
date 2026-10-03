package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/testsupport"
)

// 本文件覆盖 M4 T4.6 的证据仓储（**依赖真实 PostgreSQL**，见 testsupport）。
//
// 为什么必须真库，三条都只能真库验：
//  1. `sqlc.narg` 的可空参数在 PG 里的类型推断 —— 无 Cast 时
//     `type = $1` 会被推断成 text 而列是 VARCHAR(32)，
//     内存替身给不出「运行期才发现」这类错误（与 M4 B1 的 kind 参数同源教训）。
//  2. 截断边界（「第 51 条才截断」）—— 内存替身只能造出返回值造不出 SQL 行为。
//  3. JSONB 往返 —— metadata 的 map ↔ jsonb 转换有精度与空值语义。
//
// 本文件与 truncation_test.go（M3 的 AlertCount）同源：那个文件因为
// 「要触发截断就得造 200+ 条」而长期 0 覆盖，本文件**真的造了 80 条**。

// evidenceBase 是本文件用的固定时间基准。
var evidenceBase = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

// mkEvidence 造一条待落库的证据。
func mkEvidence(typ evidence.Type, src evidence.Source, offset time.Duration) evidence.Evidence {
	return evidence.Evidence{
		Type:      typ,
		Source:    src,
		Query:     fmt.Sprintf(`{service="api"} |~ "%s"`, src),
		Value:     fmt.Sprintf("%s 证据 @%v", src, offset),
		Timestamp: evidenceBase.Add(offset),
		Metadata:  map[string]string{"src": src},
	}
}

// insertEvidenceIncident 造一个事件。
func insertEvidenceIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, title string) int64 {
	t.Helper()
	return insertIncidentFull(t, ctx, pool, title, "critical", "open", "checkout", evidenceBase)
}

// TestEvidenceSaveAndListRoundTrip 落库再读回，字段必须无损。
func TestEvidenceSaveAndListRoundTrip(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "证据往返事件")

	conf := 0.7500
	in := []evidence.Evidence{
		{
			Type: evidence.TypeMetric, Source: "prometheus",
			Query: `{service="api"}`, Value: "5xx 错误速率：0.25",
			Timestamp: evidenceBase, Confidence: &conf,
			Metadata: map[string]string{"metric": "errors", "change_ratio": "5.0000"},
		},
		{
			// Confidence 留空（OPEN-M4-1），必须落成 NULL 而不是 0。
			Type: evidence.TypeLog, Source: "loki",
			Query: `{service="api"} |~ "error"`, Value: "第 1 类错误（3 条）",
			Timestamp: evidenceBase.Add(time.Minute),
			Metadata:  map[string]string{"pattern": "db timeout"},
		},
	}

	saved, err := repo.Save(ctx, inc, in)
	if err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("Save 返回 %d 条，期望 2", len(saved))
	}
	// 落库时刻由 DB 生成，必须非零 —— 否则 API 层的 collected_at 会是空串。
	for i, s := range saved {
		if s.CreatedAt.IsZero() {
			t.Errorf("第 %d 条落库后 CreatedAt 为零 —— API 的 collected_at 会返回空串", i)
		}
		if s.ID <= 0 {
			t.Errorf("第 %d 条落库后 ID = %d，期望 > 0", i, s.ID)
		}
	}

	got, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if got.Total != 2 {
		t.Errorf("Total = %d，期望 2", got.Total)
	}
	if got.Truncated {
		t.Error("仅 2 条不该报 Truncated")
	}
	if len(got.Items) != 2 {
		t.Fatalf("返回 %d 条，期望 2", len(got.Items))
	}

	// 排序是 created_at DESC → 后插入的（时间更晚的）在前
	if got.Items[0].Type != evidence.TypeLog {
		t.Errorf("第 0 条 type = %q，期望 log（created_at 较晚）", got.Items[0].Type)
	}

	first := got.Items[0]
	if first.Metadata["pattern"] != "db timeout" {
		t.Errorf("metadata.pattern = %q，期望 \"db timeout\"（JSONB 往返丢了内容）", first.Metadata["pattern"])
	}
	// **未评估的置信度必须仍是「未评估」** —— 不能变成 0。
	// 「未评估」与「置信度 0」在页面上必须能区分（OPEN-M4-1）。
	if first.Valid {
		t.Errorf("未填置信度的证据读回来 Valid = true（值 %v）—— 0 被当成了「已评估」", first.Confidence)
	}

	second := got.Items[1]
	if !second.Valid {
		t.Error("已填置信度 0.75 的证据读回来 Valid = false")
	}
	if second.Confidence != 0.75 {
		t.Errorf("Confidence = %v，期望 0.75（numeric 往返失真）", second.Confidence)
	}
}

// TestEvidenceConfidenceZeroIsDistinctFromNull 置信度 0 与「未评估」必须可区分。
//
// 这是本文件最容易写错的地方：两者在库里都可能是「看起来没值」，
// 但语义相反 —— 前者是「这条证据明确没有支撑力」，后者是「没有判断能力」。
func TestEvidenceConfidenceZeroIsDistinctFromNull(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "置信度零与空")
	zero := 0.0
	if _, err := repo.Save(ctx, inc, []evidence.Evidence{
		{Type: evidence.TypeMetric, Source: "prometheus", Value: "零",
			Timestamp: evidenceBase, Confidence: &zero},
		{Type: evidence.TypeMetric, Source: "prometheus", Value: "未评估",
			Timestamp: evidenceBase.Add(time.Minute)},
	}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("返回 %d 条，期望 2", len(got.Items))
	}
	// 第 0 条是后插入的「未评估」
	if got.Items[0].Valid {
		t.Errorf("未评估的那条 Valid = true —— 0 被当成了「已评估」")
	}
	if !got.Items[1].Valid {
		t.Error("显式置信度 0 的那条 Valid = false —— 「0」与「未评估」被混同了")
	}
	if got.Items[1].Confidence != 0 {
		t.Errorf("Confidence = %v，期望 0", got.Items[1].Confidence)
	}
}

// TestEvidenceListTruncatesAtLimitAndReportsTrueTotal 是本文件的核心用例：
// 造 80 条证据、limit=50，断言「只回 50 条 + truncated=true + total=80」。
//
// 缺陷背景：M3 的详情页用 len(alerts) 当 AlertCount，而 alerts 已截断到 200，
// 结果 250 条的事件显示「200 条关联告警」（见 truncation_test.go）。
// 证据侧若同样用 len(items) 当 total，50 条的预算会把 80 条说成 50 条 ——
// 而手册 §4.2 明确要求 truncated 与总数必须能被 API 读到。
func TestEvidenceListTruncatesAtLimitAndReportsTrueTotal(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "证据截断事件")
	const total = 80
	for i := range total {
		typ := evidence.TypeMetric
		if i%2 == 1 {
			typ = evidence.TypeLog
		}
		e := mkEvidence(typ, "prometheus", time.Duration(i)*time.Second)
		if _, err := repo.Save(ctx, inc, []evidence.Evidence{e}); err != nil {
			t.Fatalf("插入第 %d 条证据失败: %v", i, err)
		}
	}

	got, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if len(got.Items) != 50 {
		t.Errorf("返回 %d 条，期望 50（limit）", len(got.Items))
	}
	if !got.Truncated {
		t.Error("Truncated = false，期望 true —— 超限被静默吞掉了")
	}
	if got.Total != total {
		t.Errorf("Total = %d，期望 %d —— 必须是真计数，不能是 len(items) 的 50；"+
			"否则前端会显示「一共 50 条证据」", got.Total, total)
	}
}

// TestEvidenceListExactlyAtLimitIsNotTruncated 边界：**正好等于上限**不算截断。
//
// 这是「取 limit+1 条探测」的价值所在：若恰好取 limit 条，
// 「正好 50 条」与「超了一点」返回的条数完全一样，截断会被静默吞掉。
func TestEvidenceListExactlyAtLimitIsNotTruncated(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "证据正好达上限")
	for i := range 50 {
		if _, err := repo.Save(ctx, inc, []evidence.Evidence{
			mkEvidence(evidence.TypeMetric, "prometheus", time.Duration(i)*time.Second),
		}); err != nil {
			t.Fatalf("插入第 %d 条失败: %v", i, err)
		}
	}

	got, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if got.Truncated {
		t.Error("正好 50 条、上限 50 —— 不该报 Truncated（边界判定错）")
	}
	if got.Total != 50 {
		t.Errorf("Total = %d，期望 50", got.Total)
	}
	if len(got.Items) != 50 {
		t.Errorf("返回 %d 条，期望 50", len(got.Items))
	}

	// 再加一条立刻变成截断 —— 证明判定不是恒定值。
	if _, sErr := repo.Save(ctx, inc, []evidence.Evidence{
		mkEvidence(evidence.TypeMetric, "prometheus", 999*time.Second),
	}); sErr != nil {
		t.Fatalf("追加第 51 条失败: %v", sErr)
	}
	after, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if !after.Truncated {
		t.Error("51 条、上限 50 —— 应报 Truncated")
	}
	if after.Total != 51 {
		t.Errorf("Total = %d，期望 51", after.Total)
	}
}

// TestEvidenceListTypeFilterLimitsScope 过滤后的真计数只算该类型。
//
// 最容易搞错的地方：limit=50 全量超限时，type=metric 的子集可能只有 40 条 ——
// 此时 truncated 必须是 false。若用全量总数判截断，
// 页面会在「metric 明明只有 40 条」时显示「已截断」。
func TestEvidenceListTypeFilterLimitsScope(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "证据类型过滤")
	// 60 条 metric + 5 条 log
	for i := range 60 {
		if _, err := repo.Save(ctx, inc, []evidence.Evidence{
			mkEvidence(evidence.TypeMetric, "prometheus", time.Duration(i)*time.Second),
		}); err != nil {
			t.Fatalf("插入第 %d 条 metric 失败: %v", i, err)
		}
	}
	for i := range 5 {
		if _, err := repo.Save(ctx, inc, []evidence.Evidence{
			mkEvidence(evidence.TypeLog, "loki", time.Duration(i)*time.Second),
		}); err != nil {
			t.Fatalf("插入第 %d 条 log 失败: %v", i, err)
		}
	}

	// 全量：65 条 > 50 → 截断
	all, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("全量查询失败: %v", err)
	}
	if !all.Truncated || all.Total != 65 {
		t.Errorf("全量：truncated=%v total=%d，期望 true/65", all.Truncated, all.Total)
	}

	// 只看 log：5 条 → 不截断，且真计数是 5 而非 65
	onlyLog, err := repo.ListByIncident(ctx, inc, evidence.TypeLog, 50)
	if err != nil {
		t.Fatalf("按类型查询失败: %v", err)
	}
	if len(onlyLog.Items) != 5 {
		t.Errorf("log 返回 %d 条，期望 5", len(onlyLog.Items))
	}
	if onlyLog.Truncated {
		t.Error("log 只有 5 条却报 Truncated —— 截断判定用了全量总数")
	}
	if onlyLog.Total != 5 {
		t.Errorf("log Total = %d，期望 5（必须按过滤后的集合统计）", onlyLog.Total)
	}
	for _, e := range onlyLog.Items {
		if e.Type != evidence.TypeLog {
			t.Errorf("返回了 type=%q 的证据 —— 过滤没生效", e.Type)
		}
	}

	// 只看 metric：60 条 > 50 → 截断，真计数 60
	onlyMetric, err := repo.ListByIncident(ctx, inc, evidence.TypeMetric, 50)
	if err != nil {
		t.Fatalf("按类型查询失败: %v", err)
	}
	if !onlyMetric.Truncated || onlyMetric.Total != 60 {
		t.Errorf("metric：truncated=%v total=%d，期望 true/60", onlyMetric.Truncated, onlyMetric.Total)
	}
}

// TestEvidenceListNoFilterIsNotTypeEmptyString 传空类型 = 不过滤，
// 而不是「type = ”」一条都匹配不到。
//
// 这正是 sqlc.narg 存在的理由：空串与「不过滤」在 SQL 里必须能区分，
// 传空串会变成 `type = ”` 从而永远返回空列表。
func TestEvidenceListNoFilterIsNotTypeEmptyString(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "空类型过滤")
	for i := range 3 {
		if _, err := repo.Save(ctx, inc, []evidence.Evidence{
			mkEvidence(evidence.TypeMetric, "prometheus", time.Duration(i)*time.Second),
		}); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	got, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if len(got.Items) != 3 {
		t.Errorf("空类型过滤返回 %d 条，期望 3 —— 变成了 type='' 匹配", len(got.Items))
	}
}

// TestEvidenceListOrdersByCreatedAtDesc 排序必须是「落库时间倒序」。
//
// 依赖 created_at 而非 timestamp：补采一条很老但刚采到的日志时，
// 用 timestamp 排会让它沉到列表底部，用户以为没采到。
func TestEvidenceListOrdersByCreatedAtDesc(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "证据排序")
	// 证据时间**递增**插入，因此 id 与 timestamp 同序。
	// 若 SQL 按 id DESC 排（本该同序），结果一样 —— 真正能区分的是
	// 下面那次「乱序插入」。
	for i := range 3 {
		if _, err := repo.Save(ctx, inc, []evidence.Evidence{
			mkEvidence(evidence.TypeMetric, "prometheus", time.Duration(i)*time.Minute),
		}); err != nil {
			t.Fatalf("插入第 %d 条失败: %v", i, err)
		}
	}
	// 插入一条证据时间**很老**的（补采场景）
	old := mkEvidence(evidence.TypeMetric, "prometheus, 补采", -10*time.Hour)
	old.Timestamp = evidenceBase.Add(-10 * time.Hour)
	if _, err := repo.Save(ctx, inc, []evidence.Evidence{old}); err != nil {
		t.Fatalf("补采落库失败: %v", err)
	}

	got, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if len(got.Items) != 4 {
		t.Fatalf("返回 %d 条，期望 4", len(got.Items))
	}
	// 补采的那条虽然证据时间最老，但落库最新 → 必须排第一。
	// 若按 timestamp DESC 排，它会垫底，用户会以为没采到。
	if !got.Items[0].Timestamp.Equal(evidenceBase.Add(-10 * time.Hour)) {
		t.Errorf("第 0 条证据时间 = %v，期望补采的那条 %v —— 排序没用 created_at DESC",
			got.Items[0].Timestamp.UTC(), evidenceBase.Add(-10*time.Hour).UTC())
	}
	// 逐条校验倒序
	for i := 1; i < len(got.Items); i++ {
		if got.Items[i-1].CreatedAt.Before(got.Items[i].CreatedAt) {
			t.Errorf("第 %d 条的 created_at 早于第 %d 条 —— 不是倒序",
				i-1, i)
		}
	}
}

// TestEvidenceListRejectsInvalidArgs 非法参数必须报错，且不查库。
func TestEvidenceListRejectsInvalidArgs(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	cases := []struct {
		name string
		id   int64
		typ  evidence.Type
	}{
		{"事件 id 为 0", 0, ""},
		{"事件 id 为负", -1, ""},
		{"类型非法", 1, evidence.Type("metirc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := repo.ListByIncident(ctx, tc.id, tc.typ, 50); err == nil {
				t.Error("应报错")
			}
		})
	}
}

// TestEvidenceCountByTypeFillsZeros 无证据的事件也必须每个类型都返回 0。
func TestEvidenceCountByTypeFillsZeros(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "类型统计补零")
	if _, err := repo.Save(ctx, inc, []evidence.Evidence{
		mkEvidence(evidence.TypeMetric, "prometheus", 0),
		mkEvidence(evidence.TypeMetric, "prometheus", time.Second),
		mkEvidence(evidence.TypeLog, "loki", 2*time.Second),
	}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	counts, err := repo.CountByType(ctx, inc)
	if err != nil {
		t.Fatalf("CountByType 失败: %v", err)
	}
	if len(counts) != 6 {
		t.Errorf("返回 %d 个类型，期望 6（合法类型全集，缺失的补 0）：%v", len(counts), counts)
	}
	if counts[evidence.TypeMetric] != 2 {
		t.Errorf("metric = %d，期望 2", counts[evidence.TypeMetric])
	}
	if counts[evidence.TypeLog] != 1 {
		t.Errorf("log = %d，期望 1", counts[evidence.TypeLog])
	}
	// 关键：本期无采集器的类型必须是 0 而不是缺失
	if n, present := counts[evidence.TypeTrace]; !present {
		t.Error("缺 trace —— 前端 Tab 角标会直接消失而不是显示 0")
	} else if n != 0 {
		t.Errorf("trace = %d，期望 0", n)
	}
}

// TestEvidenceCountByTypeOnEmptyIncident 零证据事件返回全 0。
func TestEvidenceCountByTypeOnEmptyIncident(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "零证据事件")
	counts, err := repo.CountByType(ctx, inc)
	if err != nil {
		t.Fatalf("CountByType 失败: %v", err)
	}
	if len(counts) != 6 {
		t.Errorf("零证据事件返回 %d 个类型，期望 6（全 0）", len(counts))
	}
	for typ, n := range counts {
		if n != 0 {
			t.Errorf("%s = %d，期望 0", typ, n)
		}
	}
}

// TestEvidenceSaveRejectsInvalidEvidence 非法证据整批被拒。
//
// 关键在「整批」：半批落库的失败态最难处理 ——
// 页面上会看到「3 条 Prometheus + 0 条 Loki」而没有任何错误提示。
func TestEvidenceSaveRejectsInvalidEvidence(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "非法证据")
	good := mkEvidence(evidence.TypeMetric, "prometheus", 0)

	bad := []struct {
		name string
		ev   evidence.Evidence
	}{
		{"时间为空", evidence.Evidence{
			Type: evidence.TypeMetric, Source: "prometheus"}},
		{"类型非法", evidence.Evidence{
			Type: "bogus", Source: "prometheus", Timestamp: evidenceBase}},
		{"来源为空", evidence.Evidence{
			Type: evidence.TypeMetric, Timestamp: evidenceBase}},
	}
	// 置信度越界单独构造：它需要一个越界的指针值，
	// 放进上面的表里会被测试自己的字面量写法掩盖。
	over := 1.5
	bad = append(bad, struct {
		name string
		ev   evidence.Evidence
	}{"置信度越界", evidence.Evidence{
		Type: evidence.TypeMetric, Source: "prometheus",
		Timestamp: evidenceBase, Confidence: &over,
	}})

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			// 把好证据放在前面：若实现是「逐条插、遇错才停」，
			// 好证据会留在库里 —— 而这里要验的是**整批回滚**。
			if _, err := repo.Save(ctx, inc, []evidence.Evidence{good, tc.ev}); err == nil {
				t.Error("应报错")
			}
			got, err := repo.ListByIncident(ctx, inc, "", 50)
			if err != nil {
				t.Fatalf("ListByIncident 失败: %v", err)
			}
			if got.Total != 0 {
				t.Errorf("库里残留 %d 条证据 —— 整批回滚没生效：%s",
					got.Total, tc.name)
			}
		})
	}
}

// TestEvidenceSaveRejectsBadIncidentID 事件 id 非法必须报错。
func TestEvidenceSaveRejectsBadIncidentID(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	for _, id := range []int64{0, -1} {
		if _, err := repo.Save(ctx, id, []evidence.Evidence{
			mkEvidence(evidence.TypeMetric, "prometheus", 0),
		}); err == nil {
			t.Errorf("incident_id = %d 时应报错", id)
		}
	}
}

// TestEvidenceSaveEmptyBatch 空批次不是错误。
//
// 采集器「采到了但无内容」是正常结果（手册 §七），
// 开一个空事务再提交纯属浪费，且会把正常情况记成失败日志。
func TestEvidenceSaveEmptyBatch(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "空批次")
	got, err := repo.Save(ctx, inc, nil)
	if err != nil {
		t.Fatalf("空批次不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("返回 %d 条，期望 0", len(got))
	}
}

// TestEvidenceListLimitClamped 超大 limit 被夹紧而非照单全收。
//
// LIMIT 负数在 PG 里是「一条都取不到」且**不报错**，
// LIMIT 极大则是把整张表拉进内存 —— 两个都是静默失败。
func TestEvidenceListLimitClamped(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "limit 夹紧")
	for i := range 3 {
		if _, err := repo.Save(ctx, inc, []evidence.Evidence{
			mkEvidence(evidence.TypeMetric, "prometheus", time.Duration(i)*time.Second),
		}); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	// limit=0 → 落到默认值 50
	zero, err := repo.ListByIncident(ctx, inc, "", 0)
	if err != nil {
		t.Fatalf("limit=0 失败: %v", err)
	}
	if len(zero.Items) != 3 {
		t.Errorf("limit=0 返回 %d 条，期望 3（应落到默认值而非返回空）", len(zero.Items))
	}

	// 超大 limit → 夹到上限，仍返回全部 3 条
	huge, err := repo.ListByIncident(ctx, inc, "", 1<<40)
	if err != nil {
		t.Fatalf("超大 limit 失败: %v", err)
	}
	if len(huge.Items) != 3 {
		t.Errorf("超大 limit 返回 %d 条，期望 3", len(huge.Items))
	}
	if huge.Truncated {
		t.Error("3 条不该报 Truncated")
	}
}

// TestEvidenceSaveKeepsDuplicateRows 同一事件重复采集时**不去重**。
//
// 手册 §4.2 明确不去重：同一 (type, source, timestamp, query) 出现两次，
// 可能是真的采到了两条（比如两个分片都返回了该样本），
// 数据库层去重会把「真的采到两条」也吃掉 —— 那是静默丢数据。
func TestEvidenceSaveKeepsDuplicateRows(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewEvidence(pool)
	ctx := context.Background()

	inc := insertEvidenceIncident(t, ctx, pool, "重复证据")
	dup := mkEvidence(evidence.TypeMetric, "prometheus", 0)
	if _, err := repo.Save(ctx, inc, []evidence.Evidence{dup, dup, dup}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := repo.ListByIncident(ctx, inc, "", 50)
	if err != nil {
		t.Fatalf("ListByIncident 失败: %v", err)
	}
	if got.Total != 3 {
		t.Errorf("Total = %d，期望 3 —— 层做了去重，那是静默丢数据（手册 §4.2）", got.Total)
	}
}
