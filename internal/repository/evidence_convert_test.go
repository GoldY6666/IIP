package repository

import (
	"math"
	"testing"
)

// 本文件覆盖 confidenceToPG —— **纯函数，零 SQL、零连接**。
//
// 为什么单独给它一份单测：这个函数的第一版用
// `pgtype.Numeric.Scan(float64)`，而实测该方法对 float64
// **一律返回 `cannot scan float64`**（numeric 的 Scan 只接受
// string / []byte / 整数类型）。当时我写了「Scan 失败就退化成未评估」
// 的兜底，结果是**所有置信度都被静默写成 NULL**：
//
//	证据照样落库、读写都不报错、只有真库跑往返才发现置信度全丢了。
//
// 教训：**凡「转换失败就退化成安全默认值」的兜底，都要问一句
// 「默认值是不是和「真的什么都没有」长得一样」** —— 这里两者都是
// NULL，于是兜底把 bug 变成了静默数据丢失。真库往返用例已锁住
// 这个行为（TestEvidenceConfidenceZeroIsDistinctFromNull），
// 本文件则保证转换本身在**不开库**的情况下也是对的。

// TestConfidenceToPGPreservesValue 转换后的值必须能读回原数。
func TestConfidenceToPGPreservesValue(t *testing.T) {
	cases := []float64{0, 0.0001, 0.25, 0.5, 0.75, 0.9, 1}
	for _, want := range cases {
		v := confidenceToPG(&want)
		if !v.Valid {
			t.Errorf("置信度 %v 转换后 Valid = false —— 被当成「未评估」丢掉了", want)
			continue
		}
		got, err := v.Float64Value()
		if err != nil {
			t.Errorf("置信度 %v 读回失败: %v", want, err)
			continue
		}
		// 列是 DECIMAL(5,4)，只保 4 位小数，比较时同精度。
		if math.Abs(got.Float64-want) > 5e-5 {
			t.Errorf("置信度 %v 往返后 = %v", want, got.Float64)
		}
	}
}

// TestConfidenceToPGZeroIsNotNull 显式 0 必须与「未评估」区分开。
//
// 这是本函数存在的全部理由：两者语义相反 ——
// 0 是「这条证据明确没有支撑力」，NULL 是「没有判断能力」。
// 若转换把 0 当成 NULL，页面上两者就长得一样了。
func TestConfidenceToPGZeroIsNotNull(t *testing.T) {
	zero := 0.0
	v := confidenceToPG(&zero)
	if !v.Valid {
		t.Fatal("显式置信度 0 被当成 NULL —— 「0」与「未评估」被混同了")
	}
	got, err := v.Float64Value()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Float64 != 0 {
		t.Errorf("读回 = %v，期望 0", got.Float64)
	}
}

// TestConfidenceToPGNilIsNull nil 必须是 NULL。
func TestConfidenceToPGNilIsNull(t *testing.T) {
	if v := confidenceToPG(nil); v.Valid {
		t.Error("nil 置信度应转成 NULL（Valid = false）")
	}
}

// TestConfidenceToPGHandlesPreciseValues 小数值不能因截断而失真。
//
// 列是 DECIMAL(5,4)，最多 4 位小数。若转换过程里先转成 int 再除，
// 0.0001 会变成 0 —— 那就把「极低置信度」与「零」混同了。
func TestConfidenceToPGHandlesPreciseValues(t *testing.T) {
	small := 0.0001
	v := confidenceToPG(&small)
	if !v.Valid {
		t.Fatal("0.0001 被当成未评估")
	}
	got, err := v.Float64Value()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Float64 != small {
		t.Errorf("0.0001 往返后 = %v，期望 0.0001（被截断成 0 会与「零」混同）", got.Float64)
	}
}
