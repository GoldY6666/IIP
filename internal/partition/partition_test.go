package partition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yu/iip/internal/db"
)

// ---------------------------------------------------------------------------
// IsNoPartition：错误分类判据
//
// 这条判据是插入路径 23514 重试的唯一入口，判错的两种方向都致命：
//   - 假阳性（把别的 23514 当无分区）→ 对 CHECK 违反也去补建分区再重试，
//     第二次仍失败，错误信息被包装成「补建失败」，真实原因被掩埋；
//   - 假阴性（漏判真正的无分区）→ 重试不发生，插入直接失败。
// 故逐类构造：真 23514、同码不同因、邻码、包装、非 PG 错误。
// ---------------------------------------------------------------------------

func TestIsNoPartition(t *testing.T) {
	noPart := &pgconn.PgError{Code: "23514", Message: "no partition of relation found"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil 错误", nil, false},
		{"裸 23514", noPart, true},
		{"包装一层", fmt.Errorf("repository: 插入告警失败: %w", noPart), true},
		{"包装两层", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", noPart)), true},
		{"邻码 23505（唯一冲突）", &pgconn.PgError{Code: "23505"}, false},
		{"邻码 23503（外键违反）", &pgconn.PgError{Code: "23503"}, false},
		{"邻码 23502（非空违反）", &pgconn.PgError{Code: "23502"}, false},
		{"邻码 42P01（表不存在）", &pgconn.PgError{Code: "42P01"}, false},
		{"非 PG 错误", errors.New("connection reset"), false},
		{"包装的非 PG 错误", fmt.Errorf("wrap: %w", errors.New("boom")), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsNoPartition(c.err); got != c.want {
				t.Errorf("IsNoPartition(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NewMaintainer：构造期校验
// ---------------------------------------------------------------------------

func TestNewMaintainer_ValidatesInputs(t *testing.T) {
	if _, err := NewMaintainer(nil, nil, 2, 90); err == nil {
		t.Error("Querier 为 nil 必须报错（否则 Ensure 时 nil panic）")
	}
	q := &fakeQuerier{}
	if _, err := NewMaintainer(q, nil, 2, 0); err == nil {
		t.Error("retentionDays=0 必须报错：维护 tick 会删全表")
	}
	if _, err := NewMaintainer(q, nil, 2, -5); err == nil {
		t.Error("retentionDays<0 必须报错")
	}
	// 错误信息必须点出「删全表」的后果，否则运维不知道该改哪。
	_, err := NewMaintainer(q, nil, 2, 0)
	if err == nil || !strings.Contains(err.Error(), "删除全部告警") {
		t.Errorf("错误信息应说明会删全表，实际: %v", err)
	}
}

func TestNewMaintainer_NegativeAheadConvergesToDefault(t *testing.T) {
	q := &fakeQuerier{}
	m, err := NewMaintainer(q, nil, -1, 90)
	if err != nil {
		t.Fatalf("负 ahead 应被收敛而非报错: %v", err)
	}
	if m.aheadMonths != DefaultAheadMonths {
		t.Errorf("负 ahead 应收敛到 DefaultAheadMonths=%d，实际 %d", DefaultAheadMonths, m.aheadMonths)
	}
	// 收敛后的值必须真的下发到查询参数（而不是只存字段）。
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if q.ensureArg.AheadMonths != int32(DefaultAheadMonths) {
		t.Errorf("Ensure 下发的 ahead = %d，期望 %d", q.ensureArg.AheadMonths, DefaultAheadMonths)
	}
}

func TestNewMaintainer_ZeroAheadIsLegal(t *testing.T) {
	m, err := NewMaintainer(&fakeQuerier{}, nil, 0, 90)
	if err != nil {
		t.Fatalf("ahead=0（只建当月）应合法: %v", err)
	}
	if m.aheadMonths != 0 {
		t.Errorf("ahead 应保持 0，实际 %d", m.aheadMonths)
	}
}

// ---------------------------------------------------------------------------
// RunMaintenance：编排顺序与错误传播
//
// 假实现记录调用顺序。顺序是硬约束：ensure 必须先于 drop/scan
// （它俩假设分区在位）；三者缺一即「维护没跑完」，必须整体报错。
// ---------------------------------------------------------------------------

func TestRunMaintenance_CallsInOrder(t *testing.T) {
	q := &fakeQuerier{
		ensureNames: []string{"alerts_2027_03"},
		dropped:     7,
		scanRows: []db.ScanFiringDuplicatesRow{{
			Fingerprint: "fp-dup",
			FiringCount: 2,
			AlertIds:    []int64{11, 42},
			MinStartsAt: pgtype.Timestamptz{Time: mustTime("2026-10-01T00:00:00Z"), Valid: true},
			MaxStartsAt: pgtype.Timestamptz{Time: mustTime("2026-11-05T08:30:00Z"), Valid: true},
		}},
	}
	m, err := NewMaintainer(q, nil, 2, 90)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if err := m.RunMaintenance(context.Background()); err != nil {
		t.Fatalf("RunMaintenance 应成功: %v", err)
	}

	want := []string{"ensure", "drop", "scan"}
	if len(q.order) != len(want) {
		t.Fatalf("调用序列 = %v，期望 %v", q.order, want)
	}
	for i := range want {
		if q.order[i] != want[i] {
			t.Fatalf("第 %d 步 = %s，期望 %s（完整序列 %v）", i, q.order[i], want[i], q.order)
		}
	}
	// 参数透传：retentionDays 必须原样到查询，不能被默认值覆盖。
	if q.dropArg != 90 {
		t.Errorf("DropExpired 收到的 retention_days = %d，期望 90", q.dropArg)
	}
	if q.ensureArg.AheadMonths != 2 {
		t.Errorf("Ensure 收到的 ahead = %d，期望 2", q.ensureArg.AheadMonths)
	}
}

func TestRunMaintenance_PropagatesEachStepError(t *testing.T) {
	boom := errors.New("db down")
	cases := []struct {
		name   string
		q      *fakeQuerier
		failAt string
	}{
		{"ensure 失败", &fakeQuerier{ensureErr: boom}, "ensure"},
		{"drop 失败", &fakeQuerier{dropErr: boom}, "drop"},
		{"scan 失败", &fakeQuerier{scanErr: boom}, "scan"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := NewMaintainer(c.q, nil, 2, 90)
			if err != nil {
				t.Fatalf("构造失败: %v", err)
			}
			err = m.RunMaintenance(context.Background())
			if err == nil {
				t.Fatal("任一步失败都必须整体返回错误（不能静默吞掉）")
			}
			// 失败点必须真的被执行到（没执行到却说失败 = 假红），
			// 且其后的步骤不应再执行——上游断了还跑下游是纯浪费。
			idx := -1
			for i, step := range c.q.order {
				if step == c.failAt {
					idx = i
					break
				}
			}
			if idx == -1 {
				t.Fatalf("失败点 %s 未被执行，实际序列 %v", c.failAt, c.q.order)
			}
			for _, step := range c.q.order[idx+1:] {
				t.Errorf("失败点 %s 之后的 %s 不应执行，实际序列 %v", c.failAt, step, c.q.order)
			}
		})
	}
}

func TestRunMaintenance_EmptyResultsAreQuiet(t *testing.T) {
	// 无新建分区、无过期行、无重复时不应报错也不应有多余动作。
	q := &fakeQuerier{}
	m, err := NewMaintainer(q, nil, 2, 90)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if err := m.RunMaintenance(context.Background()); err != nil {
		t.Fatalf("空结果应正常返回: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeQuerier 实现 partition.Querier，记录调用序列与参数。
type fakeQuerier struct {
	ensureNames []string
	ensureErr   error
	ensureCalls int
	ensureArg   db.EnsureAlertPartitionsParams

	dropped   int64
	dropErr   error
	dropCalls int
	dropArg   int32

	scanRows  []db.ScanFiringDuplicatesRow
	scanErr   error
	scanCalls int

	order []string
}

func (f *fakeQuerier) EnsureAlertPartitions(_ context.Context, arg db.EnsureAlertPartitionsParams) ([]string, error) {
	f.ensureCalls++
	f.ensureArg = arg
	f.order = append(f.order, "ensure")
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	return f.ensureNames, nil
}

func (f *fakeQuerier) DropExpiredAlertPartitions(_ context.Context, retentionDays int32) (int64, error) {
	f.dropCalls++
	f.dropArg = retentionDays
	f.order = append(f.order, "drop")
	if f.dropErr != nil {
		return 0, f.dropErr
	}
	return f.dropped, nil
}

func (f *fakeQuerier) ScanFiringDuplicates(_ context.Context) ([]db.ScanFiringDuplicatesRow, error) {
	f.scanCalls++
	f.order = append(f.order, "scan")
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	return f.scanRows, nil
}

// mustTime 解析 RFC3339 时间，失败即测试失败。
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
