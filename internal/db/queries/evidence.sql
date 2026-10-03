-- ============================================================================
-- IIP M4 · evidence 表查询（证据落库与读取）
-- 规格来源：docs/IIP-M4详细开发手册.md §4.2（采集预算）/ §4.4（时间语义）/ T4.6
--
-- 要点复述（因为 SQL 本身不解释「为什么」）：
--   * timestamp 是**证据自身的时间**，不用 NOW()兜底（ADR-003 / §4.4）。
--     采集不到真实时间的证据在 Go 侧就被 Validate 拦掉了，不进这里。
--   * 预算截断用「取 limit+1 条探测」而非「先 count 再取」——
--     见 ListIncidentEvidence 的注释，那是本文件最容易写错的地方。
--   * confidence 允许 NULL：本期无采集器填它（OPEN-M4-1），
--     NULL 的语义是「未评估」而非「置信度 0」。
-- ============================================================================

-- name: InsertEvidence :one
-- 落库一条证据。
--
-- metadata 传 jsonb：采集器给的 map[string]string 在 Go 侧序列化成 JSONB。
-- 用 sqlc.arg 而非直接列名 —— 生成的参数类型由 schema 推断，
-- 显式声明能挡住「把 jsonb 列传成 string」这类编译期可过的错误。
--
-- ON CONFLICT 不做：evidence 表没有唯一约束（手册 §4.2 明确不去重 ——
-- 同一 (type, source, timestamp, query) 出现两次可能真的是两条）。
-- 「同一事件被重复采集」由上层 EnqueueIfAbsent 保证不重复入队，
-- 而不是靠这里的去重 —— 数据库层去重会把「真的采到两条」也吃掉。
INSERT INTO evidence (incident_id, type, source, query, value, timestamp, confidence, metadata)
VALUES (sqlc.arg('incident_id'),
        sqlc.arg('type'),
        sqlc.arg('source'),
        sqlc.arg('query'),
        sqlc.arg('value'),
        sqlc.arg('timestamp'),
        sqlc.arg('confidence'),
        sqlc.arg('metadata'))
RETURNING *;

-- name: ListIncidentEvidence :many
-- 取某事件的证据，按「证据时间倒序」返回，可选按 type 过滤。
--
-- **取 limit+1 条而不是恰好 limit 条**：多取一条纯粹是为了探测「有没有超限」。
-- 调用方看到返回了 limit+1 条就知道被截断了，**不必再查一次 count(*)**。
-- 少不了一行 —— 若恰好取 limit 条，「刚好等于上限」与「超了一点」
-- 这两种情况返回的条数完全一样，截断就会被静默吞掉。
--
-- 用 created_at DESC 而非 timestamp DESC 排序：这个查询的用途是
-- 「展示该事件最新的证据」，而created_at 是「我们什么时候采到的」，
-- 语义上更贴近「最新」。用 timestamp 排会让一条很老但刚被重新采到的
-- 证据（补采场景）沉到列表底部，用户会以为没采到。
--
-- id DESC 收尾：created_at 相同（同批采集同秒落库）时提供确定性次序。
-- 没有它，同秒证据的顺序随执行计划变化，页面列表会在刷新间跳动。
--
-- type 用 sqlc.narg（可空）：不传时该条件为 NULL，整个 OR 分支短路，
-- 走纯 incident_id 路径。这是可选过滤的惯用写法。
SELECT id, incident_id, type, source, query, value, timestamp,
       confidence, metadata, created_at
  FROM evidence
 WHERE incident_id = sqlc.arg('incident_id')
   AND (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg('limit');

-- name: CountIncidentEvidence :one
-- 某事件的证据真计数（可按 type 过滤）。
--
-- **这个真值是必需的，不是优化**：M3 已经踩过一次——
-- 详情页的 AlertCount 取 len(alerts)，而 alerts 已被截断到 200，
-- 结果 250 条的事件显示「200 条关联告警」（见 truncation_test.go）。
-- 证据侧同理：把「只保留了 50 条」说成「一共 50 条」，
-- 用户会以为没有更多证据，而那正是手册 §4.2 要求 truncated 可被API 读到的原因。
--
-- 用 count(*) FILTER 而不是 GROUP BY + :many：调用方要的是一个标量快照，
-- GROUP BY 会让「返回几行」随库里type 分布变化，漏读哪行都可能被误当成 0。
--
-- 用 count(*) OVER() 也能在同一次查询里拿到真值，
-- 但那样 ListIncidentEvidence 就必须返回「数据行 + 一个只在首行有值的窗口列」，
-- 形状怪异且 sqlc 生成的类型是数组；两次查询的代价在这个表上可忽略，
-- 而 M3 已验证过「列表 + 计数」双查询的模式。
SELECT count(*)::int
  FROM evidence
 WHERE incident_id = sqlc.arg('incident_id')
   AND (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text);

-- name: CountIncidentEvidenceByType :many
-- 按类型分组的计数，供页面 Tab 角标使用。
--
-- **只返回库里实际存在的类型**，缺失的由 Go 侧用 evidence.SortedTypes() 补 0。
-- 不在 SQL 里展开类型全集，两个原因：
--   1) 类型清单的唯一真相在 Go 侧（evidence.validTypes，与 chk_evidence_type 对应）。
--      在 SQL 里再写一份，新增枚举值时忘了改这里，
--      表现为「某个 Tab 永远不出现」—— 极难排查，且不报错。
--   2) 用 unnest 展开全集还要额外 LEFT JOIN，比「查已有的+ 补缺」多一层。
--
-- 补 0 这一步很关键：「一个证据都没有的事件」也必须让每个类型都返回 0，
-- 否则前端 Tab 角标会直接消失而不是显示 0 ——
-- 那是「没采到」与「压根没查过」两种情况的可见性差异。
SELECT type, count(*)::int AS n
  FROM evidence
 WHERE incident_id = sqlc.arg('incident_id')
 GROUP BY type
 ORDER BY type;
