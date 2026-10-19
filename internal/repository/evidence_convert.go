package repository

import (
	"math/big"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/evidence"
)

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
