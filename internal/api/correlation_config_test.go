package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yu/iip/internal/correlation"
)

// 本文件验证 GET/PUT /api/v1/correlation/config 的 HTTP 契约。
//
// 重点不是"能读能写"，而是**错误码的可判别性**：
//   400 = 请求体本身不成形（非法 JSON / 未知字段）
//   422 = 请求体成形但语义非法（权重和不为 1、merge < review）
//   503 = 存储故障，可重试
// 三者混用会让调用方无从判断该改请求还是该重试。

// fakeCorrelationConfigStore 是 CorrelationConfigStore 的内存替身。
type fakeCorrelationConfigStore struct {
	cfg    correlation.Config
	getErr error
	putErr error

	puts     []correlation.Config
	getCalls int
}

func (f *fakeCorrelationConfigStore) GetConfig(context.Context) (correlation.Config, error) {
	f.getCalls++
	if f.getErr != nil {
		return correlation.Config{}, f.getErr
	}
	return f.cfg, nil
}

func (f *fakeCorrelationConfigStore) PutConfig(_ context.Context, cfg correlation.Config) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, cfg)
	f.cfg = cfg
	return nil
}

var _ CorrelationConfigStore = (*fakeCorrelationConfigStore)(nil)

func newConfigStore() *fakeCorrelationConfigStore {
	return &fakeCorrelationConfigStore{cfg: correlation.DefaultConfig()}
}

// doPut 发一次 PUT，返回 recorder。
func doPut(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/correlation/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h(rec, req)
	return rec
}

// C-01：GET 返回当前配置，字段名与前端契约一致。
func TestCorrelationConfigGetC01(t *testing.T) {
	store := newConfigStore()
	h := CorrelationConfigGet(store, discardLogger())

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/correlation/config", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}

	// 按契约的字段名断言，而不是反序列化成结构体——
	// 后者会在字段名改动时"碰巧"仍然通过。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v（原始: %s）", err, rec.Body.String())
	}
	for _, key := range []string{"weights", "thresholds", "enabled"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("响应体缺少字段 %q（原始: %s）", key, rec.Body.String())
		}
	}

	var got correlation.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if got != correlation.DefaultConfig() {
		t.Fatalf("GET 应返回当前配置：\n实得 %+v\n期望 %+v", got, correlation.DefaultConfig())
	}
}

// C-02：GET 遇存储故障 → 503（可重试），而不是回一个"空配置"误导前端。
func TestCorrelationConfigGetC02_StorageErrorIs503(t *testing.T) {
	store := newConfigStore()
	store.getErr = errors.New("模拟：存储不可用")
	h := CorrelationConfigGet(store, discardLogger())

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/correlation/config", http.NoBody))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("存储故障期望 503，实际 %d", rec.Code)
	}
	if msg := decodeError(t, rec); msg != msgStorageUnavailable {
		t.Errorf("响应体应为 %q，实际 %q", msgStorageUnavailable, msg)
	}
}

// C-03：合法全量 PUT → 200，且写入内容与请求一致。
func TestCorrelationConfigPutC03_FullUpdate(t *testing.T) {
	store := newConfigStore()
	h := CorrelationConfigPut(store, discardLogger())

	body := `{"weights":{"time":0.30,"service":0.30,"label":0.15,"dependency":0.15,"historical":0.10},
	          "thresholds":{"merge":0.75,"review":0.45},"enabled":false}`
	rec := doPut(h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	if len(store.puts) != 1 {
		t.Fatalf("应写入 1 次，实际 %d 次", len(store.puts))
	}

	want := correlation.Config{
		Weights:    correlation.Weights{Time: 0.30, Service: 0.30, Label: 0.15, Dependency: 0.15, Historical: 0.10},
		Thresholds: correlation.Thresholds{Merge: 0.75, Review: 0.45},
		Enabled:    false,
	}
	if store.puts[0] != want {
		t.Fatalf("写入内容不符：\n实得 %+v\n期望 %+v", store.puts[0], want)
	}

	// 响应体应回显生效后的配置，省去前端二次 GET。
	var echoed correlation.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &echoed); err != nil {
		t.Fatalf("响应体非法: %v", err)
	}
	if echoed != want {
		t.Errorf("响应回显不符：%+v", echoed)
	}
}

// C-04：部分更新——只提交 merge 阈值时，权重与 review 必须保持现值。
//
// 这条是"以当前配置为底覆盖"语义的核心保障：若实现改成了反序列化到零值结构体，
// 权重会被静默清零，关联引擎随即整体失效。
func TestCorrelationConfigPutC04_PartialUpdateKeepsRest(t *testing.T) {
	store := newConfigStore()
	h := CorrelationConfigPut(store, discardLogger())

	rec := doPut(h, `{"thresholds":{"merge":0.9}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	if len(store.puts) != 1 {
		t.Fatalf("应写入 1 次，实际 %d 次", len(store.puts))
	}

	got := store.puts[0]
	want := correlation.DefaultConfig()
	want.Thresholds.Merge = 0.9

	if got != want {
		t.Fatalf("部分更新结果不符：\n实得 %+v\n期望 %+v（权重与 review 应保持现值）", got, want)
	}
}

// C-05：权重之和不为 1 → 422。这是最容易配错、且后果最隐蔽的一类非法配置。
func TestCorrelationConfigPutC05_InvalidWeightsIs422(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"权重和偏小", `{"weights":{"time":0.10}}`, "权重之和必须为 1.0"},
		{"权重超区间", `{"weights":{"time":1.5}}`, "必须在 [0,1] 区间"},
		{"merge小于review", `{"thresholds":{"merge":0.3,"review":0.6}}`, "不能小于 review 阈值"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newConfigStore()
			h := CorrelationConfigPut(store, discardLogger())

			rec := doPut(h, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("非法配置期望 422，实际 %d（体: %s）", rec.Code, rec.Body.String())
			}
			if len(store.puts) != 0 {
				t.Fatal("校验失败时绝不能落库")
			}
			if msg := decodeError(t, rec); !strings.Contains(msg, tc.want) {
				t.Errorf("错误信息 %q 未包含 %q（错误须可读，值班人员要据此改配置）", msg, tc.want)
			}
		})
	}
}

// C-06：请求体不是合法 JSON → 400。
func TestCorrelationConfigPutC06_MalformedJSONIs400(t *testing.T) {
	store := newConfigStore()
	h := CorrelationConfigPut(store, discardLogger())

	rec := doPut(h, `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 期望 400，实际 %d", rec.Code)
	}
	if len(store.puts) != 0 {
		t.Fatal("非法 JSON 不应落库")
	}
}

// C-07：未知字段 → 400。
//
// 这一条防的是"键名打错却回 200"：`threshods` 之类拼写错误若被静默忽略，
// 调用方会以为配置已生效，而引擎行为毫无变化——属于最难排查的一类故障。
func TestCorrelationConfigPutC07_UnknownFieldIs400(t *testing.T) {
	store := newConfigStore()
	h := CorrelationConfigPut(store, discardLogger())

	for _, body := range []string{
		`{"threshods":{"merge":0.9}}`,
		`{"weights":{"time":0.25,"servcie":0.25}}`,
		`{"enabled":true,"unknown":1}`,
	} {
		rec := doPut(h, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("请求体 %s 期望 400（未知字段），实际 %d（体: %s）", body, rec.Code, rec.Body.String())
		}
	}
	if len(store.puts) != 0 {
		t.Fatal("含未知字段的请求不应落库")
	}
}

// C-08：请求体超限 → 413（该端点是可写的，不能留"超大 body 吃内存"的口子）。
func TestCorrelationConfigPutC08_OversizeBodyIs413(t *testing.T) {
	store := newConfigStore()
	h := CorrelationConfigPut(store, discardLogger())

	// 构造一个超过 16 KiB 的合法 JSON 前缀。
	big := `{"enabled":true,"padding":"` + strings.Repeat("x", int(correlationConfigMaxBytes)+1024) + `"}`
	rec := doPut(h, big)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body 期望 413，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	if len(store.puts) != 0 {
		t.Fatal("超限请求不应落库")
	}
}

// C-09：PUT 的前置读取失败 → 503，且不落库。
//
// 前置读取是为了拿到"覆盖基底"，读不到就无法保证部分更新语义，只能显性失败。
func TestCorrelationConfigPutC09_ReadFailureIs503(t *testing.T) {
	store := newConfigStore()
	store.getErr = errors.New("模拟：存储不可用")
	h := CorrelationConfigPut(store, discardLogger())

	rec := doPut(h, `{"thresholds":{"merge":0.9}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("前置读取失败期望 503，实际 %d", rec.Code)
	}
	if len(store.puts) != 0 {
		t.Fatal("前置读取失败时不应落库")
	}
}

// C-10：写入失败 → 503，不能让调用方以为配置已生效。
func TestCorrelationConfigPutC10_WriteFailureIs503(t *testing.T) {
	store := newConfigStore()
	store.putErr = errors.New("模拟：写库失败")
	h := CorrelationConfigPut(store, discardLogger())

	rec := doPut(h, `{"thresholds":{"merge":0.9}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("写库失败期望 503，实际 %d", rec.Code)
	}
}

// C-11：GET 出来的配置原样 PUT 回去必须成功。
//
// 前端"读-改-写"是最常见的用法，若 GET 的输出含 PUT 不接受的字段
// （本例中新加的 DisallowUnknownFields 极易造成这种回归），该用法会直接 400。
func TestCorrelationConfigPutC11_GetThenPutRoundTrip(t *testing.T) {
	store := newConfigStore()
	getH := CorrelationConfigGet(store, discardLogger())
	putH := CorrelationConfigPut(store, discardLogger())

	getRec := httptest.NewRecorder()
	getH(getRec, httptest.NewRequest(http.MethodGet, "/api/v1/correlation/config", http.NoBody))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET 期望 200，实际 %d", getRec.Code)
	}

	putRec := doPut(putH, getRec.Body.String())
	if putRec.Code != http.StatusOK {
		t.Fatalf("把 GET 的输出原样 PUT 回去应成功，实际 %d（体: %s）", putRec.Code, putRec.Body.String())
	}
	if len(store.puts) != 1 {
		t.Fatalf("应写入 1 次，实际 %d 次", len(store.puts))
	}
	if store.puts[0] != correlation.DefaultConfig() {
		t.Errorf("往返后配置应不变，实得 %+v", store.puts[0])
	}
}

// C-12：nil logger 不得 panic（handler 构造方可能不传）。
func TestCorrelationConfigNilLoggerIsSafe(t *testing.T) {
	store := newConfigStore()

	rec := httptest.NewRecorder()
	CorrelationConfigGet(store, nil)(rec, httptest.NewRequest(http.MethodGet, "/api/v1/correlation/config", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 期望 200，实际 %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	CorrelationConfigPut(store, nil)(rec2,
		httptest.NewRequest(http.MethodPut, "/api/v1/correlation/config", strings.NewReader(`{"enabled":true}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("PUT 期望 200，实际 %d", rec2.Code)
	}
}
