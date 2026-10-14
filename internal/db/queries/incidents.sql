-- M2 关联引擎：Incident 创建、候选集查询、关联写入。
-- 规格依据：docs/IIP-M2详细开发手册.md §4.3 / §4.7。

-- name: CreateIncident :one
-- NEW 档：为一条无法与既有 Incident 关联的 firing 告警新建 Incident。
-- status 固定 'open'（M3 状态机负责后续流转）；service 取自告警 labels.service。
INSERT INTO incidents (title, severity, status, service, started_at)
VALUES ($1, $2, 'open', $3, $4)
RETURNING id, title, severity, status, service, started_at, resolved_at,
          root_causes, recommendations, created_at, updated_at;

-- name: ListOpenIncidentsWithAlerts :many
-- 取所有 open / investigating 的 Incident 及其已关联告警（供逐对评分）。
-- 每行是「一条已关联告警」；调用方按 incident_id 聚合。
-- ends_at 可能为 NULL（仍 firing），由 Go 侧按当前时刻兜底。
--
-- **每个 incident 只返回最近 limit_per_incident 条 + 该事件的种子告警**（R7，2026-10-12 实测加）：
-- 旧版无上限 ⇒ 候选集 = 全部 open incident 的成员总数，且随告警量只增不减。
-- 实测 1 万条告警（8 个 incident）时候选集 7555 条：本查询单次 11ms、
-- correlateOne 每条新告警评分 7555 次 ⇒ 吞吐从 1785 条/s 崩到 ~4.5 条/s。
-- 限额后候选集 ≤ incident 数 × (K+1)，把 O(成员总数) 降为 O(incident 数 × K)。
--
-- 取「最近」而非「最早」：ROW_NUMBER 按 a.id 降序，而 id 自增 ⇒ 留下的是
-- 最新加入的告警，它们最能代表该事件当前的形态（时间窗、服务、标签）。
-- limit_per_incident <= 0 视为不限额（保持旧行为，便于对比与回归）。
--
-- **种子锚定（R7 收口，2026-10-12 补）**：只取最近 K 条会漏合并——新告警的最佳匹配
-- 若是 K 条以前的老成员，裁掉后 max 得分可能跌破阈值（异质探针实证：不限额 1 组 /
-- 限额 2 组，见 internal/correlation/replay_realtime_test.go）。种子 = 该事件的
-- 首条告警（min(a.id)，即 NEW 档建事件时 link 的那条），事件的标题/severity 都由
-- 它派生，是事件身份的来源。候选集恒含种子 ⇒「与事件同根因的新告警」绝不漏合并，
-- 成本是每事件仅多 1 条候选（WHERE 过滤不产生重复行）。
WITH ranked AS (
    SELECT i.id           AS incident_id,
           i.status       AS incident_status,
           i.severity     AS incident_severity,
           a.id           AS alert_id,
           a.fingerprint  AS alert_fingerprint,
           a.severity     AS alert_severity,
           a.labels       AS alert_labels,
           a.starts_at    AS alert_starts_at,
           a.ends_at      AS alert_ends_at,
           ROW_NUMBER() OVER (PARTITION BY i.id ORDER BY a.id DESC) AS rn,
           ROW_NUMBER() OVER (PARTITION BY i.id ORDER BY a.id ASC)  AS rn_asc
    FROM incidents i
    JOIN incident_alerts ia ON ia.incident_id = i.id
    JOIN alerts a           ON a.id = ia.alert_id
    WHERE i.status IN ('open', 'investigating')
)
SELECT incident_id, incident_status, incident_severity,
       alert_id, alert_fingerprint, alert_severity, alert_labels,
       alert_starts_at, alert_ends_at
FROM ranked
WHERE sqlc.arg('limit_per_incident')::int4 <= 0
   OR rn <= sqlc.arg('limit_per_incident')::int4
   OR rn_asc = 1
ORDER BY incident_id, alert_id;

-- name: LinkAlertToIncident :exec
-- 关联一条告警到 Incident。(incident_id, alert_id) 为主键，重复链接静默跳过（幂等）。
-- correlation_score 为 0.0000-1.0000；NEW 档传 NULL（无关联对象）。
-- needs_review = true 表示 REVIEW 档，待人工确认。
INSERT INTO incident_alerts (incident_id, alert_id, correlation_score, needs_review)
VALUES ($1, $2, sqlc.narg('score'), $3)
ON CONFLICT (incident_id, alert_id) DO NOTHING;

-- name: SetAlertIncident :exec
-- 回写 alerts.incident_id（反规范化指针），便于后续按 incident 直查告警。
UPDATE alerts
SET incident_id = $2, updated_at = NOW()
WHERE id = $1;

-- name: GetIncidentByID :one
-- 详情本体。M3 的状态流转需要「先读到当前 status 再决定能否转」，
-- 故按主键直取单行；不存在时 sqlc 返回 pgx.ErrNoRows，由调用方映射为 404。
SELECT id, title, severity, status, service, started_at, resolved_at,
       root_causes, recommendations, created_at, updated_at
FROM incidents
WHERE id = sqlc.arg('id');

-- name: ListIncidents :many
-- 列表（手册 §4.3）：过滤 + 排序 + limit/offset。
--
-- 三个过滤口径的取舍：
--   * status / severity 用**数组全量兜底**：调用方不过滤时传入全部合法取值，
--     而不是在 SQL 里写「参数为 NULL 则不过滤」的分支——后者让同一条 SQL 有两种执行形态，
--     计划缓存与索引选择都跟着摇摆。
--   * service / from / to 用 narg 的 NULL 分支：这三个是真正的「可选」条件，
--     用 NULL 表达「不限」比传哨兵值（如空串、零时间）更不容易歧义。
--   * 关联告警数与待确认数用 LEFT JOIN 子查询聚合（手册 §4.4：这两个计数在关联边上，
--     incidents 表没有），没有关联边的事件按 0 计而不是被 JOIN 掉。
--
-- 排序以 id DESC 收尾（ADR-007 的口径）：starts_at 可能完全相同，
-- 只按它排序会让同一时刻的事件在翻页时顺序漂移。
SELECT i.id, i.title, i.severity, i.status, i.service, i.started_at, i.resolved_at, i.updated_at,
       COALESCE(agg.alert_count, 0)        AS alert_count,
       COALESCE(agg.needs_review_count, 0) AS needs_review_count
FROM incidents i
LEFT JOIN (
    SELECT incident_id,
           count(*)                                  AS alert_count,
           count(*) FILTER (WHERE needs_review)      AS needs_review_count
    FROM incident_alerts
    GROUP BY incident_id
) agg ON agg.incident_id = i.id
WHERE i.status   = ANY(sqlc.arg('statuses')::text[])
  AND i.severity = ANY(sqlc.arg('severities')::text[])
  AND (sqlc.narg('service')::text IS NULL OR i.service = sqlc.narg('service')::text)
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR i.started_at >= sqlc.narg('from_ts')::timestamptz)
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR i.started_at <= sqlc.narg('to_ts')::timestamptz)
ORDER BY i.started_at DESC, i.id DESC
LIMIT sqlc.arg('row_limit')::int OFFSET sqlc.arg('row_offset')::int;

-- name: UpdateIncidentStatus :execrows
-- M3 状态流转的唯一写入点（手册 §4.1 / §4.7）。
--
-- 两个关键点：
--   * **乐观并发**：WHERE 里带 `status = from_status`。若并发下另一个事务已经把状态
--     改走，本条影响 0 行——调用方据此返回 409，而不是「后写覆盖先写」。
--     返回影响行数而非只报错，故用 :execrows。
--   * **resolved_at 三态**：由 clear_resolved / set_resolved_now 两个开关表达，
--     二者互斥且由领域层计算（incident.ResolveAtEffect）——业务规则留在 Go 侧可单测，
--     SQL 只负责执行。三态含义：
--       进入 resolved        → set_resolved_now = true  → 写 NOW()
--       从 resolved 重开     → clear_resolved  = true   → 置 NULL
--       进入 ignored 及其余  → 两者皆 false             → 该列保持不动
--     ignored 不写 resolved_at 是有意为之：误报关闭不该被算进 MTTR（手册 §4.1 决策 D1）。
UPDATE incidents
SET status      = sqlc.arg('to_status'),
    resolved_at = CASE
        WHEN sqlc.arg('clear_resolved')::boolean   THEN NULL::timestamptz
        WHEN sqlc.arg('set_resolved_now')::boolean THEN NOW()
        ELSE resolved_at
    END,
    updated_at  = NOW()
WHERE id     = sqlc.arg('incident_id')
  AND status = sqlc.arg('from_status');
