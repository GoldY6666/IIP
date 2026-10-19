// Package migrations_test 以**外部测试包**形式编写。
//
// 必须用外部测试包：本用例要通过 testsupport 拿到一次性库，而 testsupport 依赖
// internal/store，store 又 import 本包（embed 迁移文件）——若写成包内测试
// （package migrations）就会构成 import cycle。外部测试包正是为切断此类环而存在。
package migrations_test

import (
	"testing"

	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动

	"github.com/yu/iip/internal/testsupport"
)

// dedupeIndex 是 0002 建立的部分唯一索引名。
const dedupeIndex = "uq_alerts_fingerprint_firing"

// needsReviewIndex 是 0003 建立的部分索引名（只索引 needs_review 为真的关联边）。
const needsReviewIndex = "idx_incident_alerts_needs_review"

// timelineIncidentTSIndex / reviewSuppressionsIndex 是 0004 建立的两个索引名。
const (
	timelineIncidentTSIndex = "idx_timeline_incident_ts"
	reviewSuppressionsIndex = "idx_review_suppressions_fingerprint"
	reviewSuppressionsTable = "review_suppressions"
)

// newTimelineEventTypes 是 0004 为 timeline.event_type 新增的 7 个取值（手册 §4.2）。
//
// 刻意用切片而非 map：断言要按固定顺序逐个插入并报错，map 的随机迭代顺序
// 会让失败信息时左时右。
var newTimelineEventTypes = []string{
	"alert_linked", "acknowledged", "investigation_started",
	"mitigated", "resolved", "ignored", "reopened",
}

// expectedIndexes 列出「up 之后必须存在、down-all 之后必须消失」的索引。
//
// 逐个断言而非只查总数：索引被漏建或漏删时，错误信息能直接点出是哪一个。
var expectedIndexes = []string{
	dedupeIndex, needsReviewIndex,
	timelineIncidentTSIndex, reviewSuppressionsIndex, // 0004
}

// TestMigrateUpDownUp 是 M1 手册 §五 的验收：迁移可重复执行、可完整回滚。
//
// 走 up → down-all → up 三连，并在每个节点断言版本号与索引存在性——
// 只断言「命令没报错」会漏掉「down 没真的删掉」这类问题。
func TestMigrateUpDownUp(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) up
	mustUp(t, m)
	latest := latestVersion(t)
	if v, dirty := currentVersion(t, m); v != latest || dirty {
		t.Fatalf("up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latest, v, dirty)
	}
	assertIndexesExist(t, dsn, true)

	// 2) down-all
	if err := m.Down(); err != nil {
		t.Fatalf("down-all 失败: %v", err)
	}
	if v, dirty := currentVersion(t, m); v != 0 || dirty {
		t.Fatalf("down-all 后版本应为 0 且非 dirty，实际 version=%d dirty=%v", v, dirty)
	}
	assertIndexesExist(t, dsn, false)

	// 3) 再 up —— 证明可重复执行（"down 未真正回滚" 会在此暴露）
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latest || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latest, v, dirty)
	}
	assertIndexesExist(t, dsn, true)
}

// TestCorrelationM2Migration 覆盖 0003 的两项改动在**数据库层**确实生效且可完整回滚：
// incident_alerts.needs_review 列（REVIEW 档标记）与 correlation_rules 的默认配置行。
//
// 刻意**只回滚 0003**，而不是 Down() 全量回滚：
// 全量回滚会把 0001 建的表一并删掉，届时"默认配置行不存在"是必然的，
// 根本测不出 0003 的 down 是否漏删了种子行。隔离到单步才有判别力。
func TestCorrelationM2Migration(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) 全量 up
	mustUp(t, m)
	assertNeedsReviewColumn(t, dsn, true)
	assertDefaultCorrelationRule(t, dsn, true)

	// 2) 只回滚 0003：0001/0002 保持不动，correlation_rules 表仍在。
	//
	// 用 mustDownTo(绝对版本) 而不是 Steps(-N)：后者「退几步」的语义随迁移总数
	// 漂移。M4 新增 0005/0006/0007 之后，Steps(-2) 落到的是 0005 而不是 0003，
	// 断言「版本应为 latestVersion-2」也跟着一起漂 —— 于是本用例转红，
	// 而被测的 0003 的 down 从头到尾没被测过。绝对版本不随库变化。
	mustDownTo(t, m, migrationCorrelation)
	assertNeedsReviewColumn(t, dsn, false)
	assertDefaultCorrelationRule(t, dsn, false)
	assertIndexExists(t, dsn, needsReviewIndex, false)
	assertIndexExists(t, dsn, dedupeIndex, true) // 0002 未被回滚，其索引必须还在

	// 3) 重新 up 到最新：验证 0003 可重复执行（幂等）。
	mustUp(t, m)
	want := latestVersion(t)
	if v, dirty := currentVersion(t, m); v != want || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", want, v, dirty)
	}
	assertNeedsReviewColumn(t, dsn, true)
	assertDefaultCorrelationRule(t, dsn, true)
}
