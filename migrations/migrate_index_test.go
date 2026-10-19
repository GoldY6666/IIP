package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/testsupport"
)

// 被单步回滚用例引用的迁移版本号。
//
// 写常量而非从文件名推导是刻意的：用例要断言的是「回滚到**某个具体迁移之后**」，
// 那个「之后」必须由用例作者明确指定。latestVersion(t) 是「库现在到哪」，
// 两者语义不同：前者是本用例的测试目标，后者是仓库现状。
const (
	// migrationCorrelation 是 0003（correlation：needs_review 列 + 种子行）。
	migrationCorrelation = 2
	// migrationIncidentLifecycle 是 0004（incident 生命周期：status/timeline 放宽 + 抑制表）。
	//
	// 「回滚到 0004 之后」= 版本 3，因为 migrate 的版本号是**已应用的最大编号**。
	// 写成 3 而不是 4 是这一行的全部含义，写错会让回滚多退或少退一个迁移，
	// 而断言仍可能通过——测的就已经不是 0004 的 down 了。
	migrationIncidentLifecycle = 3
)

// migrationIndexTuning 是 0009（T7.2 索引调优：tasks 表达式索引 + 删 labels GIN）。
// 「回滚到 0009 之后」= 版本 8，理由同 migrationIncidentLifecycle。
const migrationIndexTuning = 8

// tasksPayloadIncidentIndex / alertsLabelsGINIndex 是 0009 直接操作的两个索引名。
const (
	tasksPayloadIncidentIndex = "idx_tasks_payload_incident"
	alertsLabelsGINIndex      = "idx_alerts_labels"
)

// TestIndexTuningMigration 覆盖 0009 的两项改动在**数据库层**确实生效且可单步回滚：
// 建 tasks 的部分表达式索引、删 alerts labels 的 GIN（父表 + 全部分区副本）。
//
// 与 0003 / 0004 / 0008 的单步回滚用例同理：刻意只回滚 0009 —— 全量回滚会把
// 0001 建的表一并删掉，届时「索引不在」是必然的，测不出 0009 的 down 是否真的
// 把 GIN 还原了。隔离到单步才有判别力。
func TestIndexTuningMigration(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) 全量 up：新表达式索引在、GIN 没（父表与分区副本都没）。
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion(t) || dirty {
		t.Fatalf("up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion(t), v, dirty)
	}
	assertTasksPayloadIncidentIndex(t, dsn, true)
	assertLabelsGIN(t, dsn, false)

	// 2) 只回滚 0009：0001-0008 保持不动，tasks 索引消失、GIN 还原。
	mustDownTo(t, m, migrationIndexTuning)
	assertTasksPayloadIncidentIndex(t, dsn, false)
	assertLabelsGIN(t, dsn, true)

	// 3) 重新 up：证明可重复执行（down 没真还原会在此撞「索引已存在」）。
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion(t) || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion(t), v, dirty)
	}
	assertTasksPayloadIncidentIndex(t, dsn, true)
	assertLabelsGIN(t, dsn, false)
}

// assertTasksPayloadIncidentIndex 断言 idx_tasks_payload_incident 的存在性与定义。
//
// 为什么断言**定义**而不只查存在性：0009 的裁定全部来自 EXPLAIN 实测（ADR-023）——
// 键列或谓词写错（运算符用 -> 而非 ->>、漏掉 WHERE kind、键名拼错）都会让索引
// 建出来却罩不住目标查询，而「索引存在」的断言对此全绿。定义里必须同时含
// 表达式键与部分谓词，缺一即红。
//
// 用子串包含而非整串相等：pg_get_indexdef 的确切格式随 PG 小版本有差异
// （括号层数、::text 渲染位置），而不变量是「运算符 + 键名 + 谓词」这三样。
func assertTasksPayloadIncidentIndex(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var def string
		err := pool.QueryRow(ctx, `
SELECT pg_get_indexdef(i.indexrelid)
FROM pg_index i
WHERE i.indexrelid = to_regclass($1)`, tasksPayloadIncidentIndex).Scan(&def)
		if errors.Is(err, pgx.ErrNoRows) {
			if want {
				t.Errorf("up 后 %s 应存在", tasksPayloadIncidentIndex)
			}
			return
		}
		if err != nil {
			t.Fatalf("查询 %s 定义失败: %v", tasksPayloadIncidentIndex, err)
		}
		if !want {
			t.Errorf("回滚后 %s 应被删除，实际仍在: %s", tasksPayloadIncidentIndex, def)
			return
		}
		// 键列必须是 ->>（取 text）：用 -> 会得到 jsonb，与消费查询传入的
		// incident_id 参数类型对不上，索引建了也走不得。
		if !strings.Contains(def, "payload ->> 'incident_id'") {
			t.Errorf("索引键应为表达式 (payload->>'incident_id')，实际定义: %s", def)
		}
		// 谓词必须落在 kind 上且值为 collect_evidence：两条消费查询
		// （LastCollectWindowEnd / EnqueueTaskIfAbsent）都带该条件，
		// 漏掉它会把其他任务类型吸进索引。
		// 判法刻意不绑完整子串：pg_get_indexdef 把谓词渲染为
		// `((kind)::text = 'collect_evidence'::text)`（列名带括号与 ::text 投射），
		// 且括号层数随 PG 小版本有差异 —— 绑死完整串就是假红判据
		// （2026-10-17 实测踩过：产品正确、断言先红）。
		// 不变量取「WHERE 存在 + 谓词同时提到 kind 与 collect_evidence」：
		// 定义里 kind 只可能出现在谓词中（键是 payload 表达式），三者齐备即命中。
		if !strings.Contains(def, "WHERE") ||
			!strings.Contains(def, "kind") ||
			!strings.Contains(def, "'collect_evidence'") {
			t.Errorf("索引应带部分谓词 kind='collect_evidence'，实际定义: %s", def)
		}
	})
}

// assertLabelsGIN 断言 idx_alerts_labels（alerts 分区父表上的 GIN）的存在性。
//
// 父表与分区副本一起数，且要求副本数 == 分区数：0008 重建的 GIN 是**分区索引**，
// 每个分区各有一份副本。只查父表名会漏掉「副本残留」；不比分区数则会漏掉
// 「还原出来的其实不是分区索引」。down 重建 → up 再删的往返必须两头都归零/齐全。
func assertLabelsGIN(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var parentExists bool
		var childCount, partitionCount int
		err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass($1)),
       (SELECT count(*) FROM pg_inherits i
          JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = to_regclass($1)),
       (SELECT count(*) FROM pg_inherits i
          JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = 'alerts'::regclass)`,
			alertsLabelsGINIndex).Scan(&parentExists, &childCount, &partitionCount)
		if err != nil {
			t.Fatalf("查询 %s 存在性失败: %v", alertsLabelsGINIndex, err)
		}
		if want {
			if !parentExists {
				t.Errorf("回滚后 %s 应被还原", alertsLabelsGINIndex)
			}
			if childCount != partitionCount || partitionCount == 0 {
				t.Errorf("还原后的 %s 应为覆盖全部分区的分区索引，"+
					"实际分区副本 %d 个 / 分区 %d 个", alertsLabelsGINIndex, childCount, partitionCount)
			}
			return
		}
		if parentExists {
			t.Errorf("up 后 %s（零读者 GIN）应被删除，实际仍在父表上", alertsLabelsGINIndex)
		}
		if childCount != 0 {
			t.Errorf("up 后 %s 的全部分区副本应随父索引一并删除，实际残留 %d 个",
				alertsLabelsGINIndex, childCount)
		}
	})
}
