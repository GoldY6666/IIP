package alert

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 本文件用**内存假仓储**覆盖 Service 的全部去重分支。
//
// 为什么不在 internal/repository 里覆盖完就算了：Go 的 -cover 只统计本包的测试，
// Service 的分支若只能靠跨包集成用例触达，本包覆盖率会长期偏低，
// 且每次验证都要连真库。领域层自测不依赖 DB，正是手册 §4.5 把 Repository
// 定义为接口的目的——可注入假实现。

// fakeRepo 是 Repository 的内存实现，可按需注入故障与冲突。
type fakeRepo struct {
	rows map[string]StoredAlert
	seq  int64

	// 三个故障注入点，对应 Repository 的三个方法。
	failGet     error
	failInsert  error
	failResolve error

	// insertConflict 为真时 Insert 返回 inserted=false —— 模拟 0002 的
	// 部分唯一索引在并发下拦住了重复 firing（DB 层兜底分支）。
	insertConflict bool

	// insertCalls 记录调用次数，用于断言"应用层去重确实不落库"。
	insertCalls int
}

var _ Repository = (*fakeRepo)(nil)

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[string]StoredAlert{}}
}

func (f *fakeRepo) LatestByFingerprint(_ context.Context, fingerprint string) (StoredAlert, error) {
	if f.failGet != nil {
		return StoredAlert{}, f.failGet
	}
	row, ok := f.rows[fingerprint]
	if !ok {
		return StoredAlert{}, ErrNotFound
	}
	return row, nil
}

func (f *fakeRepo) Insert(_ context.Context, in *Alert) (StoredAlert, bool, error) {
	f.insertCalls++
	if f.failInsert != nil {
		return StoredAlert{}, false, f.failInsert
	}
	if f.insertConflict {
		return StoredAlert{}, false, nil
	}
	if row, ok := f.rows[in.Fingerprint]; ok && row.Status == StatusFiring {
		return StoredAlert{}, false, nil
	}
	f.seq++
	stored := StoredAlert{ID: f.seq, Alert: *in}
	f.rows[in.Fingerprint] = stored
	return stored, true, nil
}

func (f *fakeRepo) Resolve(_ context.Context, id int64, endsAt *time.Time) error {
	if f.failResolve != nil {
		return f.failResolve
	}
	for fp, row := range f.rows {
		if row.ID == id {
			row.Status = StatusResolved
			row.EndsAt = endsAt
			f.rows[fp] = row
			return nil
		}
	}
	return ErrNotFound
}

// List 仅用于满足 Repository 接口；本文件用例不依赖它（列表逻辑另有专有用例）。
// 简单规整 limit/offset，顺序不保证（内存 map 迭代本就无序），仅供接口合规。
func (f *fakeRepo) List(_ context.Context, limit, offset int32) ([]StoredAlert, error) {
	if f.failGet != nil {
		return nil, f.failGet
	}
	out := make([]StoredAlert, 0, len(f.rows))
	for _, row := range f.rows {
		out = append(out, row)
	}
	if offset >= int32(len(out)) {
		return []StoredAlert{}, nil
	}
	out = out[offset:]
	if limit > 0 && int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// makePayload 构造带若干条原始告警的 payload。
func makePayload(topStatus string, rawAlerts ...string) *Payload {
	alerts := make([]json.RawMessage, 0, len(rawAlerts))
	for _, a := range rawAlerts {
		alerts = append(alerts, json.RawMessage(a))
	}
	return &Payload{Version: "4", Status: topStatus, Alerts: alerts}
}

// 同一条告警的 firing / resolved 两种形态，startsAt 刻意相同
// —— 真实上游重发时就是这样，是历史行并列的来源。
const (
	rawFiring = `{"status":"firing",
	  "labels":{"alertname":"HighLatency","service":"checkout","instance":"pod-a","severity":"critical"},
	  "annotations":{"summary":"p99 高"},
	  "startsAt":"2026-09-24T18:31:00Z","endsAt":"0001-01-01T00:00:00Z"}`

	rawResolved = `{"status":"resolved",
	  "labels":{"alertname":"HighLatency","service":"checkout","instance":"pod-b","severity":"critical"},
	  "annotations":{},
	  "startsAt":"2026-09-24T18:31:00Z","endsAt":"2026-09-24T19:00:00Z"}`

	rawNoAlertname = `{"status":"firing","labels":{"service":"checkout"},
	  "startsAt":"2026-09-24T18:31:00Z"}`

	rawBadStartsAt = `{"status":"firing","labels":{"alertname":"BadTime"},
	  "startsAt":"2026/09/24 18:31"}`
)

// fingerprintOf 算出这批 payload 共用指纹（instance 属动态键，被排除）。
func fingerprintOf(t *testing.T) string {
	t.Helper()
	return GenerateFingerprint(map[string]string{
		"alertname": "HighLatency",
		"service":   "checkout",
		"severity":  "critical",
	})
}

func assertResult(t *testing.T, label string, got Result, accepted, skipped, updated, rejected int) {
	t.Helper()
	if got.Accepted != accepted || got.Skipped != skipped || got.Updated != updated || got.Rejected != rejected {
		t.Fatalf("%s: 期望 accepted=%d skipped=%d updated=%d rejected=%d，实际 accepted=%d skipped=%d updated=%d rejected=%d (%v)",
			label, accepted, skipped, updated, rejected,
			got.Accepted, got.Skipped, got.Updated, got.Rejected, got.Errors)
	}
}

func newTestService(repo Repository) *Service {
	return NewService(repo, nil)
}

// 分支 1：首次 firing，库中无任何历史 → accepted，落库。
func TestServiceFirstFiringAccepted(t *testing.T) {
	repo := newFakeRepo()
	got, err := newTestService(repo).Ingest(context.Background(), makePayload("firing", rawFiring))
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	assertResult(t, "首次 firing", got, 1, 0, 0, 0)
	if len(repo.rows) != 1 {
		t.Errorf("应落库 1 行，实际 %d 行", len(repo.rows))
	}
}

// 分支 2：库中已有 firing，再次 firing → skipped，且不发起插入。
func TestServiceDuplicateFiringSkipped(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)

	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatalf("首次投递报错: %v", err)
	}
	callsAfterFirst := repo.insertCalls

	got, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring))
	if err != nil {
		t.Fatalf("重复投递不应报错: %v", err)
	}
	assertResult(t, "重复 firing", got, 0, 1, 0, 0)
	if repo.insertCalls != callsAfterFirst {
		t.Errorf("应用层的重复判定不应发起插入，insertCalls=%d，首次后=%d", repo.insertCalls, callsAfterFirst)
	}
	if len(repo.rows) != 1 {
		t.Errorf("重复投递不得增行，实际 %d 行", len(repo.rows))
	}
}

// 分支 3：库中为 firing 时收到 resolved → updated，原地改行不新增。
func TestServiceResolvedUpdates(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)

	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatalf("首次投递报错: %v", err)
	}
	got, err := svc.Ingest(context.Background(), makePayload("resolved", rawResolved))
	if err != nil {
		t.Fatalf("resolved 投递报错: %v", err)
	}
	assertResult(t, "resolved", got, 0, 0, 1, 0)

	fp := fingerprintOf(t)
	if repo.rows[fp].Status != StatusResolved {
		t.Errorf("行状态应为 resolved，实际 %s", repo.rows[fp].Status)
	}
	if repo.rows[fp].EndsAt == nil {
		t.Error("resolved 行应写入 ends_at")
	}
	if len(repo.rows) != 1 {
		t.Errorf("resolved 不得增行，实际 %d 行", len(repo.rows))
	}
}

// 分支 4：库中已是 resolved，再次 resolved → skipped。
func TestServiceRepeatedResolvedSkipped(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)

	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatalf("firing 报错: %v", err)
	}
	if _, err := svc.Ingest(context.Background(), makePayload("resolved", rawResolved)); err != nil {
		t.Fatalf("resolved 报错: %v", err)
	}
	got, err := svc.Ingest(context.Background(), makePayload("resolved", rawResolved))
	if err != nil {
		t.Fatalf("重复 resolved 报错: %v", err)
	}
	assertResult(t, "重复 resolved", got, 0, 1, 0, 0)
}

// 分支 5：已 resolved 后重新起火 → accepted 并新增历史行。
func TestServiceRefireAfterResolvedInserts(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)

	for _, body := range []string{rawFiring, rawResolved} {
		if _, err := svc.Ingest(context.Background(), makePayload("firing", body)); err != nil {
			t.Fatalf("投递 %s 报错: %v", body, err)
		}
	}
	got, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring))
	if err != nil {
		t.Fatalf("重新起火报错: %v", err)
	}
	assertResult(t, "重新起火", got, 1, 0, 0, 0)
	if repo.seq != 2 {
		t.Errorf("重新起火应新增历史行（第 2 行），实际 seq=%d", repo.seq)
	}
}

// 分支 6：孤立 resolved（库中从无该指纹）→ skipped，不插孤儿行。
func TestServiceOrphanResolvedSkipped(t *testing.T) {
	repo := newFakeRepo()
	got, err := newTestService(repo).Ingest(context.Background(), makePayload("resolved", rawResolved))
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	assertResult(t, "孤立 resolved", got, 0, 1, 0, 0)
	if len(repo.rows) != 0 {
		t.Errorf("孤立 resolved 不得写入任何行，实际 %d 行", len(repo.rows))
	}
}

// 分支 7：DB 层兜底 —— Insert 返回 inserted=false 时计为 skipped 且不报错。
//
// 这条分支只在并发下才可能走到，靠集成测试不可靠，必须用假仓储钉死。
func TestServiceInsertConflictIsSkipped(t *testing.T) {
	repo := newFakeRepo()
	repo.insertConflict = true

	got, err := newTestService(repo).Ingest(context.Background(), makePayload("firing", rawFiring))
	if err != nil {
		t.Fatalf("唯一约束命中属正常分支，不应报错: %v", err)
	}
	assertResult(t, "唯一约束命中", got, 0, 1, 0, 0)
}

// 分支 8：单条非法只影响该条，带下标与原因，不牵连同批。
func TestServicePartialRejection(t *testing.T) {
	repo := newFakeRepo()
	got, err := newTestService(repo).Ingest(context.Background(),
		makePayload("firing", rawFiring, rawNoAlertname, rawBadStartsAt))
	if err != nil {
		t.Fatalf("部分非法不应导致整批失败: %v", err)
	}
	assertResult(t, "混合批", got, 1, 0, 0, 2)
	if len(got.Errors) != 2 {
		t.Fatalf("期望 2 条错误明细，实际 %v", got.Errors)
	}
	if got.Errors[0].Index != 1 || got.Errors[1].Index != 2 {
		t.Errorf("错误明细下标应为 [1 2]，实际 %v", got.Errors)
	}
	if len(repo.rows) != 1 {
		t.Errorf("应只落库 1 条合法告警，实际 %d 条", len(repo.rows))
	}
}

// 空 alerts 属正常（Alertmanager 的空 group）→ 零计数、零错误。
func TestServiceEmptyAlerts(t *testing.T) {
	repo := newFakeRepo()
	got, err := newTestService(repo).Ingest(context.Background(), makePayload("firing"))
	if err != nil {
		t.Fatalf("空 alerts 不应报错: %v", err)
	}
	assertResult(t, "空 alerts", got, 0, 0, 0, 0)
	if len(got.Errors) != 0 {
		t.Errorf("空 alerts 不应产生错误明细，实际 %v", got.Errors)
	}
}

// 存储层故障必须向上冒泡为 error —— handler 据此回 503 触发上游重试。
// 三个故障点逐一验证，避免某一条路径被漏掉。
func TestServiceStorageErrorsPropagate(t *testing.T) {
	storageErr := errors.New("模拟：存储不可用")

	cases := []struct {
		name string
		prep func(r *fakeRepo)
		body string
	}{
		{"查询阶段故障", func(r *fakeRepo) { r.failGet = storageErr }, rawFiring},
		{"插入阶段故障", func(r *fakeRepo) { r.failInsert = storageErr }, rawFiring},
		{"更新阶段故障", func(r *fakeRepo) { r.failResolve = storageErr }, rawResolved},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			// 更新阶段需要先有一条 firing 行，才会走到 Resolve。
			if tc.name == "更新阶段故障" {
				if _, err := newTestService(repo).Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
					t.Fatalf("准备 firing 行失败: %v", err)
				}
			}
			tc.prep(repo)

			_, err := newTestService(repo).Ingest(context.Background(), makePayload("firing", tc.body))
			if err == nil {
				t.Fatal("存储故障必须返回 error，否则告警会被静默丢弃")
			}
			if !errors.Is(err, storageErr) {
				t.Errorf("应保留原始错误以便定位，实际: %v", err)
			}
		})
	}
}

// payload 为 nil → 报错（防御调用方），而不是 panic 或静默成功。
func TestServiceNilPayload(t *testing.T) {
	if _, err := newTestService(newFakeRepo()).Ingest(context.Background(), nil); err == nil {
		t.Fatal("nil payload 应报错")
	}
}

// 归一化产生的告警必须携带固定字段：source 恒为 alertmanager、incident_id 恒为 nil。
func TestServiceNormalizedFields(t *testing.T) {
	repo := newFakeRepo()
	if _, err := newTestService(repo).Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	fp := fingerprintOf(t)
	row := repo.rows[fp]
	if row.Source != SourceAlertmanager {
		t.Errorf("source 应为 %s，实际 %s", SourceAlertmanager, row.Source)
	}
	if row.IncidentID != nil {
		t.Errorf("M1 的 incident_id 必须恒为 nil（关联属 M2），实际 %v", row.IncidentID)
	}
	if row.Fingerprint != fp {
		t.Errorf("指纹应为自算值 %s，实际 %s（不得采用上游 fingerprint）", fp, row.Fingerprint)
	}
}
