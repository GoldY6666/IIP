# 5 分钟接入 Alertmanager

把**你的** Prometheus Alertmanager 接到 IIP，让告警落库、关联成事件、采集证据。

读完你会拿到：一条可粘的 Alertmanager receiver 配置、一条能立刻验证的手发命令、
一张排错表。**不需要重启 IIP，不需要改代码。**

> 本文只讲「怎么接」。端点契约的完整规格（含全部字段与校验规则）在
> [`IIP-M1详细开发手册.md`](IIP-M1详细开发手册.md) §4，本文与它冲突时以手册为准。

---

## 前置条件

| 项 | 怎么确认 | 说明 |
|---|---|---|
| IIP 在跑 | `curl -sS localhost:8080/healthz` 返回 200 | `/healthz` **不需要** token |
| 是否启用了鉴权 | 看 IIP 进程有没有 `IIP_AUTH_TOKEN` | 启用了则本文件所有请求都要带 `X-IIP-Token` |
| Alertmanager 能连到 IIP | 从 Alertmanager 所在主机 `curl` 一下 | 容器里跑 Alertmanager 时不要用 `localhost` |

后文用两个环境变量占位：

```bash
# 手动发命令与查看数据都用这两个变量；真正配在 Alertmanager 里的是 URL 本身
export IIP=http://localhost:8080
export IIP_TOKEN=你的token        # 未启用鉴权时留空即可
```

## 第 1 步：给 Alertmanager 加一个 receiver

在你的 `alertmanager.yml` 里加一条 webhook receiver，并在 `route` 上指向它：

<!-- verify:receiver -->
```yaml
route:
  receiver: iip-webhook
  group_by: ["alertname", "namespace", "service"]
  group_wait: 10s
  group_interval: 30s
  repeat_interval: 4h

receivers:
  - name: iip-webhook
    webhook_configs:
      # 路径必须精确到 /api/v1/alerts/webhook
      - url: "http://iip-host:8080/api/v1/alerts/webhook"
        # 强烈建议开启：否则告警恢复了 IIP 也不知道，事件会一直挂在「进行中」
        send_resolved: true
```

**启用了鉴权时，再补这一段**（完整形态见下，注意缩进层级）：

```yaml
receivers:
  - name: iip-webhook
    webhook_configs:
      - url: "http://iip-host:8080/api/v1/alerts/webhook"
        send_resolved: true
        http_config:
          http_headers:
            X-IIP-Token:
              values: ["你的token"]
```

两个容易踩的点：

- **`send_resolved: true` 不是可选项。** 关掉它，IIP 永远收不到 `resolved`，
  `alerts` 表里那条永远是 `firing`，事件也不会自动走向结束。
  （IIP 对 `resolved` 的处理是「把同指纹的 firing 置为 resolved」并计入 `updated`。）
- **鉴权头名是 `X-IIP-Token`，不是 `Authorization`。** Alertmanager 有原生的
  `http_config.authorization`（发 `Authorization: Bearer <token>`），写法更顺手，
  但 IIP **不认**它 —— 用了会稳定得到 `401`，而且日志里连一条告警都不会出现
  （请求在鉴权层就被挡掉了，压根没进 handler）。必须用上面的 `http_headers` 发自定义头。

改完 reload（或重启）Alertmanager，然后进入第 2 步 —— **先别等真告警**。

## 第 2 步：手发一条，30 秒内看到结果

把下面的内容存成 `alert.json`（这就是 Alertmanager 会发给你的东西，字段一个不少）：

<!-- verify:payload -->
```json
{
  "version": "4",
  "groupKey": "{}:{alertname=\"DemoHighCPU\"}",
  "status": "firing",
  "receiver": "iip-webhook",
  "groupLabels": {"alertname": "DemoHighCPU"},
  "commonLabels": {"alertname": "DemoHighCPU"},
  "commonAnnotations": {"summary": "演示告警：CPU 持续 95%"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "DemoHighCPU",
        "severity": "page",
        "instance": "demo-1:9100",
        "service": "checkout"
      },
      "annotations": {"summary": "演示告警：CPU 持续 95%", "value": "0.95"},
      "startsAt": "2026-10-18T06:00:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus.local/graph"
    }
  ]
}
```

然后执行（`-d @alert.json` 表示从文件读请求体）：

<!-- verify:curl -->
```bash
curl -sS -X POST "$IIP/api/v1/alerts/webhook" \
  -H 'Content-Type: application/json' \
  -H "X-IIP-Token: $IIP_TOKEN" \
  -d @alert.json
```

期望输出（字段顺序可能不同，数值必须一样）：

```json
{"accepted":1,"skipped":0,"updated":0,"rejected":0}
```

四个计数的含义：

| 字段 | 含义 | 常见数值 |
|---|---|---|
| `accepted` | 新落库的 firing 告警 | 首次投递为 1 |
| `skipped` | 指纹已存在（去重命中），未重复落库 | 同一告警重发时为 1 |
| `updated` | 把已有的 firing 置为 resolved | 收到 `resolved` 时为 1 |
| `rejected` | 单条字段不合格，被丢弃 | 见 `errors[]` 的下标与原因 |

## 第 3 步：三处确认数据真的进去了

```bash
# ① 数据层：告警已在库里，且 severity 被归一过（page → critical）
curl -sS "$IIP/api/v1/alerts?limit=5" -H "X-IIP-Token: $IIP_TOKEN"

# ② 指标层：入库计数 +1（server 的 /metrics 不需要 token）
curl -sS "$IIP/metrics" | grep 'iip_alert_ingest_total'
# 期望看到 result="stored" 的值为 1
```

收尾：把这条演示告警置为 resolved，别留在库里。

<!-- verify:resolve -->
```bash
python -c "import json;d=json.load(open('alert.json',encoding='utf-8'));a=d['alerts'][0];a['status']='resolved';a['endsAt']='2026-10-18T06:05:00Z';json.dump(d,open('resolved.json','w',encoding='utf-8'),ensure_ascii=False)"
curl -sS -X POST "$IIP/api/v1/alerts/webhook" \
  -H 'Content-Type: application/json' -H "X-IIP-Token: $IIP_TOKEN" -d @resolved.json
# 期望 {"accepted":0,"skipped":0,"updated":1,"rejected":0}
```

**第 ③ 步别省。** 演示告警在库里就是一条真实告警：它会被关联、可能建出事件、
worker 会去给它采证据。留着它，以后看面板时那条 `DemoHighCPU` 会一直是个谜。

> 上面所有命令按 **bash / Git Bash / WSL** 写。Windows PowerShell 里 `curl` 是
> `Invoke-WebRequest` 的别名，参数完全不同（`-d @file` 会直接报参数错误）——
> 在 PowerShell 里请显式写 `curl.exe`，或者直接换到 Git Bash。

至此接入完成。剩下的工作是把 `route` 真的指向这个 receiver。

---

## payload 契约

### 必填与可选

| 位置 | 字段 | 规则 |
|---|---|---|
| 顶层 | `version` | **必填**，非空字符串（Alertmanager v4 为 `"4"`）。空 → 整批 400 |
| 顶层 | `alerts` | **必填**，必须是数组，可以是 `[]`。缺失/为 null → 整批 400 |
| 顶层 | `status` `groupKey` `receiver` `groupLabels` `commonLabels` `commonAnnotations` | 可选，IIP 不依赖（`status` 仅用于和单条不一致时打 WARN） |
| 单条 | `labels.alertname` | **必填**。缺失 → 该条 `rejected`，不影响同批其他条 |
| 单条 | `status` | **必填**，只接受 `firing` / `resolved`。其他值（含 `suppressed`）→ 该条 `rejected` |
| 单条 | `startsAt` | **必填**，RFC3339。非法 → 该条 `rejected` |
| 单条 | `endsAt` | 可选。空或 `0001-01-01T00:00:00Z`（Alertmanager 的「尚无结束时间」哨兵）→ 存 NULL |
| 单条 | `fingerprint` | **忽略**。Alertmanager 自算的指纹含 `instance`，实例重建就变，IIP 一律自算 |
| 单条 | `generatorURL` `annotations` | 可选，原样保留（annotation 里的值是高频变化的，不参与指纹） |

### severity 怎么归一

IIP 的库约束只允许三个值，上游的命名会被映射：

| 上游 `severity` | IIP 存为 |
|---|---|
| `critical` `page` `emergency` `fatal` **`error`** | `critical` |
| `warning` `warn` | `warning` |
| `info` `notice` `none` `debug` | `info` |
| 其他任何值 / 缺失 | `info`，并打一条 WARN 日志 |

两处反直觉的地方，提前说清：

- **`error` → `critical`**，不是 `warning`。
- **不认识的值不报错，兜底成 `info`。** 这是刻意的：上游把 `page` 改成 `p1`
  不应该等于「告警全部丢失」。若你的环境大量出现兜底，去搜日志里
  `severity ... 无法识别` 这条 WARN。

### 指纹（决定去重与关联）

`SHA256` over「剔除动态标签后的 label 集合」，键**排序**后以 `k=v,` 拼接：

- 被剔除的标签：`instance`、`pod`、`container`、`node`
- 参与计算的：其余全部标签 —— **包括 `severity`**

由此推出两个必须知道的行为：

1. **实例重建不会产生新告警。** 容器重启后 `instance` 变了，指纹不变，
   所以重发是 `skipped` 而不是重复落库。这正是你想要的。
2. **`severity` 变更会产生新告警。** 同一故障从 `warning` 升级到 `critical`，
   指纹不同 → 会作为一条**新**告警落库（旧的仍是 firing）。
   如果你的告警规则会频繁改 severity，先想清楚这是不是你要的。

### 状态码

| 码 | 什么时候 | Alertmanager 会怎样 |
|---|---|---|
| `200` | 处理完（可能含 `rejected`） | 投递成功，不再重发 |
| `400` | 非 JSON / 缺 `version` / 缺 `alerts` | 视为失败并**重试**。修好 payload 前会一直重试 |
| `401` | 启用了鉴权但 `X-IIP-Token` 缺失或不对 | 同上 |
| `413` | 请求体超 `IIP_WEBHOOK_MAX_BYTES`（默认 1 MiB） | 同上。上千条一批时调大该值 |
| `503` | 存储层故障 | 同上，IIP 恢复后自然成功 |

> 非 2xx 都会被当成投递失败重试（节奏取决于 Alertmanager 版本与配置）。
> 因为 IIP 按指纹去重，**重复投递是安全的** —— 这正是不需要在 IIP 前面加队列的原因。

---

## 排错表

| 现象 | 真因 | 处置 |
|---|---|---|
| `401`，日志里没有告警记录 | 头名写错。IIP 只认 `X-IIP-Token`；Alertmanager 的 `authorization` 发的是 `Authorization: Bearer` | 改用 `http_config.http_headers`（见第 1 步） |
| `404` | 路径错。少写 `/api/v1` 或多写斜杠 | 必须是 `/api/v1/alerts/webhook` |
| `400 缺少 version 字段` | 上游不是 Alertmanager v4 格式（自研脚本常见） | 补 `"version": "4"` |
| `400 缺少 alerts 字段` | 发的是单个告警对象，不是 `{alerts:[...]}` | 包一层 |
| `200` 但 `rejected=1` | 单条字段不合格 | 看响应里的 `errors[0].error`（会写明 `labels 缺少 alertname` 之类） |
| `accepted=0` 且 `skipped>0` | **不是故障**，是去重命中（同指纹已在库里） | 想强制新增就改一个非动态标签 |
| `updated=0` 但确实发了 `resolved` | 库里没有该指纹的 firing 记录（可能已被清理，或从没成功落过） | 先确认 firing 那次真的 `accepted=1` |
| 库里告警一直是 `firing` | `send_resolved: false`，或 resolved 的 labels 与 firing 不完全一致（指纹不同 → 成了两条告警） | 开启 `send_resolved`；确保 resolved 消息的 labels 与 firing 一致（Alertmanager 默认如此） |
| `/metrics` 里 `result="error"` 增长 | 存储层故障（看 server 日志的 `告警 webhook 处理失败`） | 查 PG 连通性；这是 503 的来源 |

---

## 附：不用 Alertmanager 的源

IIP 只认上面那个 JSON 结构，**不关心谁发的**。Grafana Alerting 的 webhook 触点
或自研脚本，只要拼出同样的形状即可。最小可用体（3 个必填字段做到位就能落库）：

<!-- verify:generic -->
```bash
curl -sS -X POST "$IIP/api/v1/alerts/webhook" \
  -H 'Content-Type: application/json' \
  -H "X-IIP-Token: $IIP_TOKEN" \
  -d '{
    "version": "4",
    "alerts": [
      {
        "status": "firing",
        "labels": {"alertname": "MyScriptCheck", "severity": "warning", "service": "billing"},
        "annotations": {"summary": "disk usage 92% on billing-1"},
        "startsAt": "2026-10-18T06:00:00Z"
      }
    ]
  }'
```

自研源要注意三件事：

- **中文注解别内联在命令行里。** 上面示例的注解刻意用英文：Windows 各终端
  （PowerShell / cmd / MSYS bash）对命令行参数的编码处理不一致，内联的中文很可能
  被转成 GBK 字节发出去，PG 会拒收并报 `无效的 "UTF8" 编码字节顺序`，
  而接口这时回的是 **503**（看起来像 IIP 的存储坏了，其实是请求体编码不对）。
  要带中文，就把 body 存成 UTF-8 文件用 `-d @body.json` 传 —— 第 2 步的
  `alert.json` 就是这么做的，里面的中文注解没有问题。
- `startsAt` 要稳定。每次重发都换成「当前时间」的话，同一条告警会被算作
  不同实例的重复投递，去重不会生效（指纹虽同，但你会看到 `skipped` 而非 `updated`，
  语义上是「又报了一次」而不是「还在持续」）。
- 恢复时发 `status: "resolved"` 且**labels 与告警时完全一致**，否则 IIP 无法把它
  和原来那条对上。

---

## 5 分钟 checklist

- [ ] `curl $IIP/healthz` 返回 200
- [ ] 第 1 步的 receiver 已加进 `alertmanager.yml` 并 reload
- [ ] 鉴权头用的是 `X-IIP-Token`（不是 `Authorization`）
- [ ] 手发 `alert.json` 得到 `accepted=1`
- [ ] `GET /api/v1/alerts?limit=5` 能看到它，且 `severity` 已归一
- [ ] `/metrics` 里 `iip_alert_ingest_total{result="stored"}` 为 1
- [ ] 已发 `resolved` 收尾（`updated=1`）

---

> **别把自己接成环**：IIP 自身也会产出自监控告警（`IIPTargetDown` 等，见
> `deploy/prometheus/rules/iip-self.yml`）。这些告警应走独立 receiver（邮件/Slack），
> **不要路由回 IIP 自己** —— 其中「入库停滞」那条一旦送达 IIP，它本身就是一次入库，
> 条件随即消失、下个周期自动 resolve。详见 [`deploy/README.md`](../deploy/README.md)
> 的「自监控指标与面板」一节。
