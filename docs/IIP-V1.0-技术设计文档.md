## IIP V1.0 — Incident Intelligence Platform

### 技术设计文档

版本: V1.0日期: 2026-09-24状态: Draft

### 1. 项目概述

#### 1.1 产品定义

IIP（Incident Intelligence Platform）是一个开源的事件智能平台，专为自托管可观测性栈设计。核心能力：

Alert → Incident：将海量异构告警自动关联聚合为可操作事件

Incident → Evidence：为每个事件自动收集、组织可追溯的证据链

Evidence → RCA：基于证据生成可解释的根因假设与修复建议

#### 1.2 设计原则


| 原则 | 说明 |
| Modular Monolith | V1.0  不做微服务，单体架构  + Worker  模式 |
| Evidence-First | 每条结论必须可追溯到原始数据 |
| No AI Required | V1.0  核心价值不依赖   LLM，AI   是增强层 |
| Self-Hosted | 支持  Docker Compose  一键部署，K8s  Helm  可选 |
| Prometheus-Native | 原生集成  Prometheus / Alertmanager / Loki / OTel |


### 2. 系统架构

#### 2.1 整体架构图

┌─────────────────────────────────────────────────────────────┐│                        External Sources                      ││  Prometheus  │  Alertmanager  │  Kubernetes  │  Loki  │ OTel │└──────────────────────┬──────────────────────────────────────┘                       │ Webhook / API                       ▼┌─────────────────────────────────────────────────────────────┐│                      iip-server (HTTP API)                    ││  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────────┐  ││  │  Alert   │  │Incident  │  │ Evidence │  │Investigate │  ││  │ Ingest   │  │  Query   │  │  Query   │  │  (V0.3)    │  ││  └──────────┘  └──────────┘  └──────────┘  └────────────┘  │└──────────────────────┬──────────────────────────────────────┘                       │                       ▼┌─────────────────────────────────────────────────────────────┐│                      iip-worker (Background)                  ││  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  ││  │   Alert      │  │ Correlation  │  │   Evidence       │  ││  │ Normalizer   │  │   Engine     │  │   Collector      │  ││  └──────────────┘  └──────────────┘  └──────────────────┘  │└──────────────────────┬──────────────────────────────────────┘                       │          ┌────────────┼────────────┐          ▼            ▼            ▼   ┌────────────┐ ┌────────┐ ┌──────────┐   │PostgreSQL  │ │ Redis  │ │  Loki    │   │ (Primary)  │ │(Queue) │ │ (Logs)   │   └────────────┘ └────────┘ └──────────┘

#### 2.2 组件职责


| 组件 | 职责 |
| iip-server | HTTP API  服务，处理  Alert  接收、Incident/Evidence   查询、状态更新 |
| iip-worker | 后台   Worker，执行告警归一化、去重、关联、证据收集等异步任务 |
| PostgreSQL | 主数据库，存储  Alert、Incident、Evidence、Timeline  等核心数据 |
| Redis | 消息队列（V1.0   可选，初期可用  DB polling  替代） |
| Loki | 日志存储，IIP   自身日志  +  可选的关联日志查询 |


### 3. 核心数据模型

#### 3.1 ER 图

┌──────────────┐       ┌──────────────────┐       ┌───────────────┐│   Alert      │       │    Incident      │       │   Evidence    │├──────────────┤       ├──────────────────┤       ├───────────────┤│ id (PK)      │◄──────│ id (PK)          │       │ id (PK)       ││ fingerprint  │  M:N  │ title            │       │ incident_id   ││ source       │       │ severity         │       │ type          ││ severity     │       │ status           │       │ source        ││ status       │       │ service          │       │ query         ││ labels (JSON)│       │ started_at       │       │ value         ││ annotations  │       │ resolved_at      │       │ timestamp     ││ starts_at    │       │ root_causes[]    │       │ confidence    ││ ends_at      │       │ recommendations[]│       │ metadata (JSON)││ raw_payload  │       └──────────────────┘       └───────────────┘│ incident_id  │              │└──────────────┘              │                     ┌────────▼────────┐                     │   Timeline      │                     ├─────────────────┤                     │ id (PK)         │                     │ incident_id     │                     │ event_type      │                     │ description     │                     │ timestamp       │                     │ actor           │                     └─────────────────┘

#### 3.2 PostgreSQL Schema

-- ============================================-- Alerts Table-- ============================================CREATE TABLE alerts (    id              BIGSERIAL PRIMARY KEY,    fingerprint     VARCHAR(128) NOT NULL,    source          VARCHAR(64) NOT NULL,          -- alertmanager, prometheus, k8s, etc.    severity        VARCHAR(16) NOT NULL,           -- critical, warning, info    status          VARCHAR(16) NOT NULL DEFAULT 'firing',  -- firing, resolved, suppressed    labels          JSONB NOT NULL DEFAULT '{}',    annotations     JSONB NOT NULL DEFAULT '{}',    starts_at       TIMESTAMPTZ NOT NULL,    ends_at         TIMESTAMPTZ,    raw_payload     JSONB NOT NULL,    incident_id     BIGINT,    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW());CREATE INDEX idx_alerts_fingerprint ON alerts(fingerprint);CREATE INDEX idx_alerts_incident_id ON alerts(incident_id);CREATE INDEX idx_alerts_status ON alerts(status);CREATE INDEX idx_alerts_starts_at ON alerts(starts_at);CREATE INDEX idx_alerts_labels ON alerts USING GIN(labels);-- ============================================-- Incidents Table-- ============================================CREATE TABLE incidents (    id              BIGSERIAL PRIMARY KEY,    title           VARCHAR(512) NOT NULL,    severity        VARCHAR(16) NOT NULL,           -- critical, warning, info    status          VARCHAR(16) NOT NULL DEFAULT 'open',  -- open, investigating, mitigated, resolved, ignored    service         VARCHAR(256),    started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),    resolved_at     TIMESTAMPTZ,    root_causes     JSONB DEFAULT '[]',             -- [{ "hypothesis": "...", "confidence": 0.87 }]    recommendations JSONB DEFAULT '[]',             -- [{ "action": "...", "priority": "high" }]    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW());CREATE INDEX idx_incidents_status ON incidents(status);CREATE INDEX idx_incidents_severity ON incidents(severity);CREATE INDEX idx_incidents_service ON incidents(service);CREATE INDEX idx_incidents_started_at ON incidents(started_at);-- ============================================-- Alert-Incident Junction (M:N)-- ============================================CREATE TABLE incident_alerts (    incident_id     BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,    alert_id        BIGINT NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,    correlated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),    correlation_score DECIMAL(5,4),    PRIMARY KEY (incident_id, alert_id));-- ============================================-- Evidence Table-- ============================================CREATE TABLE evidence (    id              BIGSERIAL PRIMARY KEY,    incident_id     BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,    type            VARCHAR(32) NOT NULL,             -- metric, log, trace, deployment, config_change, historical    source          VARCHAR(64) NOT NULL,             -- prometheus, loki, k8s, git, etc.    query           TEXT,                             -- The query used to collect this evidence    value           TEXT,                             -- Collected value / summary    timestamp       TIMESTAMPTZ NOT NULL,    confidence      DECIMAL(5,4),                     -- How strongly this supports the hypothesis    metadata        JSONB DEFAULT '{}',               -- Source-specific metadata    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW());CREATE INDEX idx_evidence_incident_id ON evidence(incident_id);CREATE INDEX idx_evidence_type ON evidence(type);CREATE INDEX idx_evidence_timestamp ON evidence(timestamp);-- ============================================-- Timeline Table-- ============================================CREATE TABLE timeline (    id              BIGSERIAL PRIMARY KEY,    incident_id     BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,    event_type      VARCHAR(32) NOT NULL,             -- alert_fired, alert_resolved, status_change, evidence_found, user_comment    description     TEXT NOT NULL,    timestamp       TIMESTAMPTZ NOT NULL DEFAULT NOW(),    actor           VARCHAR(128),                     -- system, user_id, webhook_source    metadata        JSONB DEFAULT '{}');CREATE INDEX idx_timeline_incident_id ON timeline(incident_id);CREATE INDEX idx_timeline_timestamp ON timeline(timestamp);-- ============================================-- Correlation Rules Table (configurable)-- ============================================CREATE TABLE correlation_rules (    id              BIGSERIAL PRIMARY KEY,    name            VARCHAR(256) NOT NULL,    description     TEXT,    enabled         BOOLEAN NOT NULL DEFAULT TRUE,    rule_config     JSONB NOT NULL,                   -- Rule-specific configuration    weight          DECIMAL(5,4) NOT NULL DEFAULT 1.0,    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW());

### 4. Alert 接入与处理流程

#### 4.1 Alertmanager Webhook 协议

IIP 通过 HTTP Webhook 接收 Alertmanager 推送的告警：

POST /api/v1/alerts/webhook{  "version": "4",  "groupKey": "namespace:production/alertname:HighLatency",  "status": "firing",  "receiver": "iip-webhook",  "groupLabels": {    "alertname": "HighLatency",    "namespace": "production"  },  "commonLabels": {    "service": "checkout",    "team": "platform"  },  "commonAnnotations": {    "summary": "Checkout API latency is above threshold",    "description": "p99 latency > 500ms for 5 minutes"  },  "alerts": [    {      "status": "firing",      "labels": {        "alertname": "HighLatency",        "service": "checkout",        "instance": "checkout-abc123",        "namespace": "production",        "severity": "critical"      },      "annotations": {        "summary": "High latency on checkout-abc123",        "value": "1250ms"      },      "startsAt": "2026-09-24T18:31:00Z",      "endsAt": "0001-01-01T00:00:00Z",      "fingerprint": "abc123def456"    }  ]}

#### 4.2 Alert 处理流水线

Webhook Received       │       ▼┌─────────────┐│  Validate   │  → 校验 JSON 结构、必填字段└──────┬──────┘       │       ▼┌─────────────┐│ Normalize   │  → 统一字段命名、severity 映射、label 标准化└──────┬──────┘       │       ▼┌─────────────┐│ Fingerprint │  → 基于 labels 生成唯一指纹（SHA256）└──────┬──────┘       │       ▼┌─────────────┐│ Deduplicate │  → 相同 fingerprint + firing 状态 → 跳过└──────┬──────┘       │       ▼┌─────────────┐│  Persist    │  → 写入 PostgreSQL alerts 表└──────┬──────┘       │       ▼┌─────────────┐│ Correlate   │  → 触发关联引擎，尝试合并到现有 Incident 或创建新 Incident└──────┬──────┘       │       ▼┌─────────────┐│  Collect    │  → 异步触发 Evidence Collector│  Evidence   │└─────────────┘

#### 4.3 Fingerprint 算法

func GenerateFingerprint(labels map[string]string) string {    // 1. 排除动态 label（instance, pod, container 等）    stableLabels := make(map[string]string)    excludeKeys := map[string]bool{        "instance": true,        "pod": true,        "container": true,        "node": true,    }    for k, v := range labels {        if !excludeKeys[k] {            stableLabels[k] = v        }    }    // 2. 排序后序列化    keys := make([]string, 0, len(stableLabels))    for k := range stableLabels {        keys = append(keys, k)    }    sort.Strings(keys)    var buf bytes.Buffer    for _, k := range keys {        buf.WriteString(k)        buf.WriteString("=")        buf.WriteString(stableLabels[k])        buf.WriteString(",")    }    // 3. SHA256    hash := sha256.Sum256(buf.Bytes())    return hex.EncodeToString(hash[:])}

### 5. Correlation Engine

#### 5.1 关联评分模型

关联评分采用加权多维相似度模型：

CorrelationScore = Wt × TimeSimilarity                 + Ws × ServiceSimilarity                 + Wl × LabelSimilarity                 + Wd × DependencySimilarity                 + Wh × HistoricalSimilarity

默认权重配置：


| 维度 | 权重 | 说明 |
| TimeSimilarity | 0.25 | 时间窗口重叠度 |
| ServiceSimilarity | 0.25 | 服务/命名空间匹配度 |
| LabelSimilarity | 0.20 | 标签集合相似度 |
| DependencySimilarity | 0.20 | 拓扑依赖关联度 |
| HistoricalSimilarity | 0.10 | 历史共现频率 |


#### 5.2 各维度计算逻辑

##### TimeSimilarity

func TimeSimilarity(a, b Alert) float64 {    // 计算两个告警时间窗口的重叠比例    startMax := max(a.StartsAt, b.StartsAt)    endMin := min(a.EndsAtOrDefault(), b.EndsAtOrDefault())        if endMin.Before(startMax) {        return 0.0    }        overlap := endMin.Sub(startMax)    union := max(a.Duration(), b.Duration())        if union == 0 {        return 0.0    }        return float64(overlap) / float64(union)}

##### ServiceSimilarity

func ServiceSimilarity(a, b Alert) float64 {    // 精确匹配 service label    if a.Labels["service"] == b.Labels["service"] && a.Labels["service"] != "" {        return 1.0    }    // 同一 namespace    if a.Labels["namespace"] == b.Labels["namespace"] && a.Labels["namespace"] != "" {        return 0.6    }    return 0.0}

##### LabelSimilarity

func LabelSimilarity(a, b Alert) float64 {    // Jaccard 相似度    setA := labelSet(a.Labels)    setB := labelSet(b.Labels)        intersection := setA.Intersect(setB)    union := setA.Union(setB)        if len(union) == 0 {        return 0.0    }        return float64(len(intersection)) / float64(len(union))}

##### DependencySimilarity

func DependencySimilarity(a, b Alert, topo *TopologyGraph) float64 {    svcA := a.Labels["service"]    svcB := b.Labels["service"]        if svcA == "" || svcB == "" {        return 0.0    }        // 检查拓扑图中是否存在依赖关系    if topo.HasEdge(svcA, svcB) || topo.HasEdge(svcB, svcA) {        return 0.9    }        // 检查是否有共同上游/下游    common := topo.CommonNeighbors(svcA, svcB)    if len(common) > 0 {        return 0.5    }        return 0.0}

##### HistoricalSimilarity

func HistoricalSimilarity(a, b Alert, db *DB) float64 {    // 查询历史中这两个 fingerprint 共现于同一 Incident 的频率    coOccurrenceCount := db.CountCoOccurrence(a.Fingerprint, b.Fingerprint)    totalOccurrences := db.CountTotalOccurrence(a.Fingerprint, b.Fingerprint)        if totalOccurrences == 0 {        return 0.0    }        return float64(coOccurrenceCount) / float64(totalOccurrences)}

#### 5.3 关联决策逻辑

const (    MergeThreshold   = 0.80   // 高于此值 → 合并到同一 Incident    ReviewThreshold  = 0.50   // 介于两者之间 → 标记为"待人工确认"    IgnoreThreshold  = 0.50   // 低于此值 → 不关联)func Correlate(newAlert Alert, openIncidents []Incident) CorrelationResult {    var bestScore float64    var bestIncident *Incident        for i := range openIncidents {        for _, existingAlert := range openIncidents[i].Alerts {            score := CalculateCorrelationScore(newAlert, existingAlert)            if score > bestScore {                bestScore = score                bestIncident = &openIncidents[i]            }        }    }        if bestScore >= MergeThreshold {        return CorrelationResult{            Action:     ACTION_MERGE,            IncidentID: bestIncident.ID,            Score:      bestScore,        }    }        if bestScore >= ReviewThreshold {        return CorrelationResult{            Action:     ACTION_REVIEW,            IncidentID: bestIncident.ID,            Score:      bestScore,        }    }        return CorrelationResult{        Action: ACTION_NEW_INCIDENT,        Score:  bestScore,    }}

### 6. Incident 状态机

#### 6.1 状态定义

    ┌─────────┐    │  OPEN   │ ◄── 初始状态，Incident 刚创建    └────┬────┘         │ assign / auto-assign         ▼┌───────────────┐│ INVESTIGATING │ ◄── 有人开始调查└───────┬───────┘        │ mitigate        ▼┌─────────────┐│  MITIGATED  │ ◄── 临时缓解，根因未完全解决└──────┬──────┘       │ resolve       ▼┌───────────┐│ RESOLVED  │ ◄── 最终关闭└───────────┘    ┌─────────┐    │  OPEN   │    └────┬────┘         │ ignore         ▼┌───────────┐│  IGNORED  │ ◄── 确认为误报/无需处理└───────────┘

#### 6.2 状态转换规则


| 当前状态 | 允许转换 | 触发方式 |
| OPEN | → INVESTIGATING | 人工分配  /  自动分配 |
| OPEN | → IGNORED | 人工标记 |
| INVESTIGATING | → MITIGATED | 人工标记 |
| INVEST |  |  |


(AI生成)
