package alert

import (
	"context"
	"time"
)

// Repository 是 M1 所需的告警持久化能力。
//
// 接口定义在**消费方**（本包），实现放在 internal/repository —— 这样 Service 的单测
// 可以注入假实现，不必连库；也避免领域层反向依赖数据库层。
type Repository interface {
	// LatestByFingerprint 取该指纹最新一条告警（按 starts_at 倒序）。
	//
	// 查不到时返回 ErrNotFound，而不是零值——调用方必须能区分
	// 「该指纹从未出现」与「查到了一条内容为零值的记录」，二者的处置完全不同。
	LatestByFingerprint(ctx context.Context, fingerprint string) (StoredAlert, error)

	// Insert 落库一条新告警。
	//
	// 第二个返回值表示是否真的插入：false 表示数据库的唯一约束判定
	// 「同一指纹已有 firing 行」，本次被跳过。**这是正常业务分支，不是错误**——
	// 该兜底由 0002 的部分唯一索引提供，用于防住并发投递（手册 §4.4）。
	Insert(ctx context.Context, a *Alert) (StoredAlert, bool, error)

	// Resolve 把指定行置为 resolved 并写入结束时间（endsAt 为 nil 时由调用方补当前时间）。
	Resolve(ctx context.Context, id int64, endsAt *time.Time) error
}
