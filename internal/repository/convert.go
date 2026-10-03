package repository

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yu/iip/internal/incident"
)

// 本文件集中放置「领域类型 ↔ pgx 类型」的转换。
//
// 集中在一处的理由：这些转换散落在各仓储方法里时，一旦某处漏判 Valid 标志，
// 表现是「读出来一个零值」而不是报错——这类问题在真库上很难复现，
// 集中在同一个文件里便于逐个检查。

// statusesToStrings 把领域状态切片转成 SQL 需要的字符串切片。
func statusesToStrings(ss []incident.Status) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// timestampPtr 把可选时间转成 pgtype.Timestamptz；nil 即 SQL NULL。
func timestampPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// parseStringMap 解析 jsonb 形态的 map[string]string 字段。
//
// 解析失败或非对象一律退化为空 map：labels / metadata 是展示用字段，
// 脏数据不该让整条详情查询失败——无非是页面上少显示几个标签。
func parseStringMap(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]string{}
	}
	if m == nil {
		return map[string]string{}
	}
	return m
}

// numericToFloatPtr 把 DECIMAL 列转成 *float64；NULL 即 nil。
//
// 关联分数为 NULL 是有语义的（NEW 档无关联对象），不能退化成 0——
// 那样前端会把「没有关联对象」显示成「关联度 0」，含义完全相反。
func numericToFloatPtr(n pgtype.Numeric) *float64 {
	if !n.Valid {
		return nil
	}
	f8, err := n.Float64Value()
	if err != nil {
		return nil
	}
	v := f8.Float64
	return &v
}
