package evidence

import (
	"errors"
	"strings"
	"testing"
)

// 本文件覆盖 evidence 包的**纯领域规则**（零 SQL、零连接）。
//
// 这些规则最容易在换存储或改响应格式时被无意破坏，
// 而它们出错的后果都是「用户看到错误的事实」而不是「报错」。

// TestValidIncidentIDRejectsNonPositive 事件 id 必须为正整数。
//
// 放行 0 会让查询返回空结果，表现成「这个事件没有证据」——
// 与「事件不存在」「还没采过」三种情况在页面上完全一样，无法区分。
func TestValidIncidentIDRejectsNonPositive(t *testing.T) {
	for _, id := range []int64{0, -1, -99999} {
		err := ValidIncidentID(id)
		if err == nil {
			t.Errorf("ValidIncidentID(%d) = nil，期望报错", id)
			continue
		}
		if !errors.Is(err, ErrInvalidIncidentID) {
			t.Errorf("ValidIncidentID(%d) 错误 = %v，期望包装 ErrInvalidIncidentID", id, err)
		}
	}
	for _, id := range []int64{1, 42, 1 << 40} {
		if err := ValidIncidentID(id); err != nil {
			t.Errorf("ValidIncidentID(%d) = %v，期望 nil", id, err)
		}
	}
}

// TestValidFilterTypeAllowsEmpty 空串表示不过滤，必须放行。
func TestValidFilterTypeAllowsEmpty(t *testing.T) {
	if err := ValidFilterType(""); err != nil {
		t.Errorf("ValidFilterType(\"\") = %v，期望 nil（空串 = 不过滤）", err)
	}
}

// TestValidFilterTypeRejectsUnknown 未知类型必须报错并列出合法值。
func TestValidFilterTypeRejectsUnknown(t *testing.T) {
	err := ValidFilterType(Type("metirc"))
	if err == nil {
		t.Fatal("未知类型应报错")
	}
	if !errors.Is(err, ErrInvalidType) {
		t.Errorf("错误 = %v，期望包装 ErrInvalidType", err)
	}
	// 错误信息必须列出合法值：用户拼错参数时只有这个能告诉他正确拼法。
	for _, legal := range SortedTypes() {
		if !strings.Contains(err.Error(), string(legal)) {
			t.Errorf("错误信息未列出合法类型 %q：%v", legal, err)
		}
	}
}

// TestValidFilterTypeAcceptsAll 六种合法类型全部放行。
func TestValidFilterTypeAcceptsAll(t *testing.T) {
	for _, typ := range SortedTypes() {
		if err := ValidFilterType(typ); err != nil {
			t.Errorf("ValidFilterType(%q) = %v，期望 nil", typ, err)
		}
	}
}

// TestMarshalMetadataEmptyIsObject 空 metadata 序列化成 {} 而非 null。
func TestMarshalMetadataEmptyIsObject(t *testing.T) {
	for _, md := range []map[string]string{nil, {}} {
		got, err := MarshalMetadata(md)
		if err != nil {
			t.Fatalf("MarshalMetadata(%v) 失败: %v", md, err)
		}
		if string(got) != "{}" {
			t.Errorf("MarshalMetadata(%v) = %s，期望 {} —— null 会让"+
				"「无 metadata」与「写入路径有问题」在库上无法区分", md, got)
		}
	}
}

// TestUnmarshalMetadataNeverReturnsNil 三种输入都必须给出非 nil map。
//
// 返回 nil map 的危害是**后续往里写会 panic**，而 panic 发生在离病因很远的地方
// （装配 worker 或序列化响应时），排查成本极高。
func TestUnmarshalMetadataNeverReturnsNil(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"空字节", nil},
		{"空对象", []byte(`{}`)},
		{"JSON null", []byte(`null`)},
		{"非法 JSON", []byte(`{not json`)},
		{"非对象", []byte(`[1,2,3]`)},
		{"正常内容", []byte(`{"a":"1"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := UnmarshalMetadata(tc.raw)
			if md == nil {
				t.Fatalf("UnmarshalMetadata(%s) = nil map", tc.raw)
			}
			// 关键：能安全写入
			md["probe"] = "ok"
		})
	}
}

// TestUnmarshalMetadataInvalidIsRecorded 非法 JSONB 必须留下痕迹而不是静默变空。
//
// 静默返回空 map 会让「库里存了脏数据」与「这条证据本来就没metadata」
// 变成同一件事 —— 而前者是需要有人去修的。
func TestUnmarshalMetadataInvalidIsRecorded(t *testing.T) {
	md := UnmarshalMetadata([]byte(`{not json`))
	if _, marked := md["_metadata_parse_error"]; !marked {
		t.Errorf("非法 JSONB 应记 _metadata_parse_error，实际 %v —— 静默变空会"+
			"让「库里有脏数据」与「本来就没 metadata」无法区分", md)
	}
}

// TestUnmarshalMetadataValidHasNoErrorMarker 正常 JSONB 不该带错误标记。
func TestUnmarshalMetadataValidHasNoErrorMarker(t *testing.T) {
	md := UnmarshalMetadata([]byte(`{"metric":"errors"}`))
	if _, marked := md["_metadata_parse_error"]; marked {
		t.Errorf("正常 JSONB 不该带错误标记：%v", md)
	}
	if md["metric"] != "errors" {
		t.Errorf("metric = %q，期望 errors", md["metric"])
	}
}

// TestFillTypeCountsFillsZeros 缺失的类型必须补 0。
//
// 「一个证据都没有的事件」也必须让每个类型都返回 0，
// 否则前端 Tab 角标会直接消失而不是显示 0 ——
// 那是「没采到」与「压根没查过」两种情况的可见性差异。
func TestFillTypeCountsFillsZeros(t *testing.T) {
	got := FillTypeCounts(map[Type]int{TypeMetric: 3, TypeLog: 1})

	if len(got) != len(validTypes) {
		t.Errorf("补零后有 %d 个类型，期望 %d（合法类型全集）", len(got), len(validTypes))
	}
	for _, typ := range SortedTypes() {
		if _, present := got[typ]; !present {
			t.Errorf("缺少类型 %q（应补 0）", typ)
		}
	}
	if got[TypeMetric] != 3 || got[TypeLog] != 1 {
		t.Errorf("已有计数被覆盖：metric=%d log=%d", got[TypeMetric], got[TypeLog])
	}
	if got[TypeTrace] != 0 {
		t.Errorf("trace = %d，期望 0（本期无采集器）", got[TypeTrace])
	}
}

// TestFillTypeCountsIgnoresUnknownTypes 库里出现的未知类型不该进结果。
//
// （正常路径不会发生——表上有 CHECK 约束。若真发生了，
// 说明有人手工塞了行或在 CHECK 之外写入了。）
// 补零后的结果应恰好等于合法类型全集，多一个都是噪声。
func TestFillTypeCountsIgnoresUnknownTypes(t *testing.T) {
	got := FillTypeCounts(map[Type]int{TypeMetric: 1, Type("bogus"): 99})
	if _, present := got[Type("bogus")]; present {
		t.Error("未知类型不应出现在补零结果里")
	}
	if len(got) != len(validTypes) {
		t.Errorf("结果有 %d 个类型，期望恰好 %d", len(got), len(validTypes))
	}
}

// TestSortedTypeNames 供错误信息与前端提示使用，必须与 SortedTypes 同源。
func TestSortedTypeNames(t *testing.T) {
	got := SortedTypeNames()
	types := SortedTypes()
	if len(got) != len(types) {
		t.Fatalf("有 %d 个名字，期望 %d", len(got), len(types))
	}
	for i := range types {
		if got[i] != string(types[i]) {
			t.Errorf("第 %d 个 = %q，期望 %q", i, got[i], types[i])
		}
	}
}
