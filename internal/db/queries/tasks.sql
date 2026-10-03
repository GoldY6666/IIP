-- ============================================================================
-- IIP M4 · tasks 表查询（任务队列）
-- 规格来源：docs/IIP-M4详细开发手册.md §4.1
--
-- 队列语义见手册 §4.1 的五点设计。要点复述（因为 SQL 本身不解释「为什么」）：
--   * 领取用 SKIP LOCKED —— 多 worker 并行时各自取到不重叠的行，互不阻塞。
--   * 退避用 available_at（可见性）而非 sleep —— 进程重启不丢退避状态。
--   * 死信保留行（status='dead'），不删 —— 删掉就看不出「曾经试过」。
--   * in_flight 超时可回收（ReclaimStaleTasks）—— worker 崩溃不会让任务永久卡住。
-- ============================================================================

-- name: EnqueueTask :one
-- 入队一个任务。幂等由调用方保证（payload 里的 incident_id + 「未采集过」判定）。
INSERT INTO tasks (kind, payload)
VALUES (sqlc.arg('kind'), sqlc.arg('payload'))
RETURNING *;

-- name: EnqueueTaskIfAbsent :one
-- 条件入队：仅当该事件没有「未完成」的采集任务时才插入。
--
-- 用 INSERT ... SELECT ... WHERE NOT EXISTS 而非「先查再插」：
-- 后者在两个 worker 同时判定时会插出两条重复任务，而这个重复
-- 会表现为「同一事件被采集两次，证据翻倍」。
--
-- NOT EXISTS 的判定范围刻意包含 in_flight —— 已领取但未完成的也算，
-- 否则重试逻辑可能在采集进行中又插一条。
--
-- incident_id 显式 ::text：payload->>'incident_id' 的结果类型是 text，
-- 而 sqlc 对无Cast 的命名参数会推断成 jsonb。`text = jsonb` 在 PG 里
-- **没有对应运算符**，运行期会报 operator does not exist —— 且只在真库
-- 跑到这条查询时才暴露。写Cast 让比较两侧同为 text。
--
-- kind 必须**四处**都显式 ::text（下面每处都标了）：
-- 同一个 $1 既出现在 INSERT 的目标列（kind VARCHAR(32)）、
-- 又出现在 NOT EXISTS 的比较里（t.kind = $1）。
-- PG 对无 Cast 的参数按**首次出现**处推断类型，两处推断不一致时直接报
--「inconsistent types deduced for parameter $1」(SQLSTATE 42P08)，
-- 整条查询用不了。凡「同一参数在多处出现且上下文类型可能不同」，
-- 全部显式 Cast，别指望推断一致。
INSERT INTO tasks (kind, payload)
SELECT sqlc.arg('kind')::text, sqlc.arg('payload')
WHERE NOT EXISTS (
    SELECT 1 FROM tasks t
     WHERE t.kind = sqlc.arg('kind')::text
       AND t.payload->>'incident_id' = sqlc.arg('incident_id')::text
       AND t.status IN ('pending', 'in_flight')
)
RETURNING *;

-- name: ClaimTasks :many
-- 领取一批可执行任务：FOR UPDATE SKIP LOCKED。
--
-- SKIP LOCKED 是本查询的核心：没有它，两个 worker 并发领取时第二个会
-- 阻塞在第一个的事务上（而不是各拿各的），任务吞吐会退化成串行；
-- 更糟的是若第一个 worker 崩溃持锁，第二个会一直等到超时。
--
-- ORDER BY available_at 保证退避中的任务按「到期先后」被领，
-- 不加排序会让长尾任务被无限期饿死。
--
-- attempts 在**领取时就 +1**（而不是失败时）。理由：worker 领完就崩溃的情况
-- 同样消耗了一次机会，把它记在失败上会漏计；而 attempts 同时被
-- 「超时回收」复用（回收时也 +1），这样任何一次「被领走但没成功」
-- 都会推进 attempts，最终能进死信而不是无限重试。
--
-- COALESCE(claimed_by, '') 用于让 RETURNING 的列类型确定为非空，
-- 避免 sqlc 因两处类型不一致生成不出结构体。
UPDATE tasks t
   SET status      = 'in_flight',
       attempts    = t.attempts + 1,
       claimed_at  = NOW(),
       claimed_by  = sqlc.arg('claimed_by'),
       updated_at  = NOW()
 WHERE t.id IN (
       SELECT id
         FROM tasks
        WHERE status = 'pending'
          AND available_at <= NOW()
        ORDER BY available_at
        LIMIT sqlc.arg('batch')
          FOR UPDATE SKIP LOCKED
       )
RETURNING t.*;

-- name: CompleteTask :execrows
-- 任务成功：置 done，并清掉 claimed_by。
--
-- 返回受影响行数（:execrows）让调用方能判断「这行是否还存在」——
-- 若任务已被超时回收并被别人领走，本 UPDATE 会命中 0 行，
-- 此时本次执行的结果不该被当作权威（可能有另一个 worker 也在做同一件事）。
--
-- window_end 写入本任务**实际采到的窗口终点**（0007 新增列），它是下一轮
-- 采集的起点游标。三点必须坚持：
--   1) **采到 0 条证据时也要写**。否则空窗口的游标永不推进，
--      下一轮又从同一个起点重采一遍，事件会永远卡在「采不到东西」的状态，
--      而每轮都白白消耗一次 attempts 直到进死信。
--   2) 由**调用方**传窗口终点而非用 NOW()：窗口是采集器实际查询的区间，
--      任务的完成时刻可能比窗口终点晚几十秒（落库 + 写留痕），
--      用 NOW() 会让游标跳过这段没采过的区间。
--   3) narg：首次采集的窗口终点可能不存在（拿不到窗口时不该写游标，
--      否则下一轮会从一个空区间开始，永远采不到东西）。
UPDATE tasks
   SET status      = 'done',
       claimed_at  = NULL,
       claimed_by  = NULL,
       last_error  = NULL,
       window_end  = sqlc.narg('window_end'),
       updated_at  = NOW()
 WHERE id = sqlc.arg('id')
   AND status = 'in_flight';

-- name: LastCollectWindowEnd :one
-- 取该事件最近一次**成功采集**到的窗口终点，作为下一轮的起点游标。
--
-- 只看 status='done'：pending/in_flight 的任务还没采完，
-- 拿它们的游标会让下一轮跳过一段还没采过的区间 —— 那段数据就永远丢了。
-- dead 同理：它采失败了（数据源挂了），用它当游标等于宣布那段区间已采过。
--
-- 排除 window_end IS NULL 的行：那表示「完成时没拿到窗口」，
-- 拿它当游标会让下一轮从零值开始，等于每次都重采整个历史区间。
--
-- ORDER BY window_end DESC 而非 id DESC：游标语义是「时间上最远采到哪儿」，
-- 按 id 排会在并发补采（两个任务都成功，游标一个比一个远）时取错。
--
-- 找不到时 sqlc 返回 pgx.ErrNoRows，由调用方映射成「首次采集」。
SELECT window_end
  FROM tasks
 WHERE kind = 'collect_evidence'
   AND payload->>'incident_id' = sqlc.arg('incident_id')::text
   AND status = 'done'
   AND window_end IS NOT NULL
 ORDER BY window_end DESC
 LIMIT 1;

-- name: FailTask :one
-- 任务失败：重试或进死信。
--
-- 用 SQL 表达式而不是在 Go 里算延迟，原因有二：
--   1) available_at 用数据库的 NOW() 计算，与「是否可见」的判断同一时钟源；
--      若在 Go 侧算，两边时钟不一致会让任务刚退避完就立刻可领（退避形同虚设）
--      或迟迟不可领。
--   2) 「是否进死信」需要同时看 attempts 与 max_attempts，二者在同一条 UPDATE
--      的 WHERE/表达式里判定，不会出现「读到 attempts=2 时另一个 worker 已改成 3」
--      的竞态。
--
-- delay_seconds 由调用方按指数退避算好（手册 §4.1 决策 3：base * 2^(n-1)，封顶 5 分钟）。
-- 封顶在 Go 侧做而非 SQL，因为「封顶值」是业务决策、不该散落到 SQL 里。
UPDATE tasks
   SET status      = CASE WHEN attempts >= sqlc.arg('max_attempts') THEN 'dead' ELSE 'pending' END,
       available_at = NOW() + make_interval(secs => sqlc.arg('delay_seconds')),
       claimed_at   = NULL,
       claimed_by   = NULL,
       last_error   = sqlc.arg('last_error'),
       updated_at   = NOW()
 WHERE id = sqlc.arg('id')
   AND status = 'in_flight'
RETURNING *;

-- name: ReclaimStaleTasks :execrows
-- 回收超时的 in_flight 任务：worker 领走任务后崩溃，行会永远停在 in_flight。
--
-- 这里把它们**置回 pending**（而不是直接删）：任务本身没做错，重新排队即可。
-- attempts 不在此处 +1 —— ClaimTasks 已经在领取时 +1 了；此处再 +1 会让
-- 「领取即计数」与「回收即计数」混在一处，无法从表上判断真实尝试次数。
--
-- available_at 设为 NOW()：超时的任务没有退避理由（不是它失败，
-- 是持有它的进程没了），立即可领。
--
-- last_error 写明是被回收而非业务失败 —— 否则日志里会出现
-- 「任务失败原因：持有者超时回收」这种自相矛盾的记录。
UPDATE tasks
   SET status       = 'pending',
       available_at = NOW(),
       claimed_at   = NULL,
       claimed_by   = NULL,
       last_error   = '持有者超时未完成，已回收重排',
       updated_at   = NOW()
 WHERE status = 'in_flight'
   AND claimed_at < NOW() - make_interval(secs => sqlc.arg('stale_seconds'));

-- name: CountTasksByStatus :one
-- 按状态计数，供 worker 心跳与端到端断言使用。
--
-- **用 FILTER 聚成一行，而不是 GROUP BY + :many**：
-- 调用方要的是「一个快照」，而 GROUP BY 会让「有几种状态」随运行时刻变化
-- （刚启动只有 pending，跑一轮后出现 done 和 dead），调用方就得处理
-- 「返回几行不确定」——而漏读某一行会被误当成「该状态为 0」。
-- 聚成固定 4 列后，「这一行永远是 4 个计数」是可断言的契约。
--
-- 刻意**不写 WHERE status <> 'done'`**：done 是正常运行产物，
-- 而「done 为 0」本身就是异常信号（没有任务被成功处理过），压掉就看不见了。
SELECT count(*) FILTER (WHERE status = 'pending')::int   AS pending,
       count(*) FILTER (WHERE status = 'in_flight')::int AS in_flight,
       count(*) FILTER (WHERE status = 'done')::int      AS done,
       count(*) FILTER (WHERE status = 'dead')::int      AS dead
  FROM tasks;