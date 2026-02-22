# 語音轉錄摘要系統：架構與流程圖

## Workflow 總覽

1. 用戶透過前端上傳音檔，請求進入 Gateway
2. Gateway 核發或讀取 Cookie (userId)，注入 `X-User-Id` Header，代理請求至 API Service
3. API Service 建立任務 (POST /api/tasks)，DB 寫入 pending 紀錄，返回 taskId
4. 前端攜帶 taskId 串流上傳音檔 (PUT /api/tasks/{id}/upload)，API Service 以 stream.pipeline 寫入磁碟
5. 上傳完成後 API Service 發布 STT Task 至 RabbitMQ
6. 前端連接 Gateway SSE 端點 (GET /api/tasks/{id}/events)，Gateway 訂閱 Redis Pub/Sub
7. Worker 消費 STT Task，執行音檔切片 (VAD + ffmpeg)，併發 STT 轉錄 (Semaphore = 5)
8. STT 完成，Worker 儲存 transcript 至 DB，發布 SUMMARY Task 回 RabbitMQ
9. Worker 消費 SUMMARY Task，執行 LLM 串流摘要
10. 每個 summary chunk 透過 Redis Pub/Sub 發布，Gateway 接收後即時推送 SSE 至前端
11. 摘要完成，Worker 儲存 summary 至 DB，標記 completed，發布 completed 事件
12. 用戶可隨時查詢歷史任務與結果 (GET /api/tasks, GET /api/tasks/{id})

---

## 1. 系統架構圖

```mermaid
---
config:
  layout: elk
---
graph TB
    FE[Frontend<br/>Vue.js]

    subgraph "Gateway Layer — always-on"
        GW[Gateway<br/>Go]
    end

    subgraph "Application Layer — stateless"
        API[API Service<br/>Node.js / TS]
    end

    subgraph "Infrastructure"
        MQ[RabbitMQ<br/>Message Broker]
        RD[Redis<br/>Pub/Sub + Buffer]
        PG[(PostgreSQL<br/>SSOT)]
        VOL[(Shared Volume<br/>音檔儲存)]
    end

    subgraph "Processing Layer"
        WK[Worker<br/>Go]
    end

    FE -- "HTTP Request" --> GW
    FE -. "SSE 長連接" .-> GW
    GW -- "反向代理<br/>注入 X-User-Id" --> API
    GW -- "單線程 PSUBSCRIBE<br/>Multi-plexing" --> RD

    API -- "任務 CRUD" --> PG
    API -- "發布 STT Task" --> MQ
    API -- "Cancel Signal" --> RD
    API -- "串流寫入音檔" --> VOL

    MQ -- "消費 Task" --> WK
    WK -- "讀取音檔" --> VOL
    WK -- "進度 / summary_chunk" --> RD
    WK -- "儲存 transcript + summary" --> PG
    WK -- "發布 SUMMARY Task" --> MQ
```

---

## 2. 核心任務 Pipeline — Sequence Diagram

```mermaid
sequenceDiagram
    participant FE as Frontend
    participant GW as Gateway (Go)
    participant API as API Service (Node.js)
    participant PG as PostgreSQL
    participant MQ as RabbitMQ
    participant WK as Worker (Go)
    participant RD as Redis

    Note over FE,RD: Phase 1 — Task Creation & Upload

    FE->>GW: POST /api/tasks
    GW->>GW: Cookie → userId (核發或讀取)
    GW->>API: Proxy + X-User-Id Header
    API->>PG: INSERT task (status = pending)
    API-->>GW: { taskId, status: pending }
    GW-->>FE: { taskId, status: pending }

    FE->>GW: PUT /api/tasks/{id}/upload (stream)
    GW->>API: Proxy stream body
    API->>VOL: stream.pipeline → 寫入磁碟
    API->>PG: UPDATE file_path
    API->>MQ: Publish { type: STT, taskId, filePath }
    API-->>GW: { status: upload_complete }
    GW-->>FE: { status: upload_complete }

    Note over FE,RD: Phase 2 — SSE Connection

    Note over GW,RD: Gateway 背景 Broadcaster 持續 PSUBSCRIBE progress:*
    FE->>GW: GET /api/tasks/{id}/events (SSE)
    GW->>GW: 註冊 SSE Channel 至 Broadcaster
    GW->>RD: GET summary:buffer:{taskId}
    GW-->>FE: SSE: buffer recovery (if exists)

    Note over FE,RD: Phase 3 — STT Processing

    MQ->>WK: Deliver STT Task
    WK->>PG: UPDATE status = processing<br/>WHERE status = pending (Atomic)
    WK->>RD: Publish progress (10%, 音檔處理中)
    RD-->>GW: progress event
    GW-->>FE: SSE: progress

    WK->>WK: Audio Chunking (VAD + ffmpeg)
    WK->>WK: Concurrent STT × N chunks<br/>(Goroutine + Semaphore = 5)

    loop 每完成一個 chunk
        WK->>RD: Publish progress (30%~70%)
        RD-->>GW: progress event
        GW-->>FE: SSE: progress
    end

    WK->>PG: INSERT transcript (persist before summary)
    WK->>MQ: Publish { type: SUMMARY, taskId, transcript }
    WK->>RD: Publish progress (75%, 轉錄完成)

    Note over FE,RD: Phase 4 — LLM Streaming Summary

    MQ->>WK: Deliver SUMMARY Task
    WK->>RD: Publish progress (80%, 摘要生成中)
    RD-->>GW: progress event
    GW-->>FE: SSE: progress

    loop LLM 串流回應
        WK->>RD: Publish summary_chunk
        WK->>RD: SET summary:buffer:{taskId} (累積)
        RD-->>GW: summary_chunk event
        GW-->>FE: SSE: summary_chunk
    end

    WK->>PG: UPDATE status = completed + Save summary<br/>(Transaction)
    WK->>RD: Publish completed
    RD-->>GW: completed event
    GW-->>FE: SSE: completed
```

---

## 3. 用戶取消流程

```mermaid
sequenceDiagram
    participant FE as Frontend
    participant GW as Gateway (Go)
    participant API as API Service (Node.js)
    participant RD as Redis
    participant WK as Worker (Go)
    participant PG as PostgreSQL

    FE->>GW: DELETE /api/tasks/{id}
    GW->>API: Proxy + X-User-Id
    API->>PG: UPDATE status = cancelled
    API->>RD: PUBLISH cancel_channel { taskId }
    API-->>GW: { status: cancelled }
    GW-->>FE: { status: cancelled }

    Note over WK: Worker 已訂閱 cancel_channel

    RD-->>WK: Cancel Signal
    WK->>WK: context.Cancel() → 終止進行中的 STT/LLM
    WK->>PG: UPDATE status = cancelled<br/>WHERE status = processing (Atomic)
    WK->>RD: Publish { type: cancelled }
    RD-->>GW: cancelled event
    GW-->>FE: SSE: cancelled
```

---

## 4. 任務狀態機

```mermaid
stateDiagram-v2
    [*] --> pending: POST /api/tasks

    pending --> processing: Worker 消費 (Atomic Check)
    pending --> cancelled: DELETE /api/tasks/{id}

    processing --> completed: STT + LLM 全部完成
    processing --> failed: STT/LLM 失敗
    processing --> cancelled: Cancel Signal (Atomic Check)

    completed --> processing: POST /api/tasks/{id}/summarize (重新摘要)

    completed --> [*]
    failed --> [*]
    cancelled --> [*]
```

---

## 5. Gateway 分層架構（雲端部署視角）

```mermaid
---
config:
  layout: elk
---
graph LR
    subgraph "Internet"
        User[用戶瀏覽器]
    end

    subgraph "Edge Layer"
        CDN[CDN / Cloudflare]
        LB[Load Balancer<br/>ALB / Cloud LB]
    end

    subgraph "Gateway Layer — always-on"
        GW1[Gateway #1]
        GW2[Gateway #2]
    end

    subgraph "Application Layer — scalable / serverless"
        API1[API Service #1]
        API2[API Service #2]
        API3[API Service #3]
    end

    subgraph "Processing Layer — scalable"
        WK1[Worker #1<br/>STT-heavy]
        WK2[Worker #2<br/>LLM-heavy]
    end

    User --> CDN --> LB
    LB --> GW1
    LB --> GW2
    GW1 --> API1
    GW1 --> API2
    GW2 --> API2
    GW2 --> API3
    API1 --> MQ[(RabbitMQ)]
    API2 --> MQ
    API3 --> MQ
    MQ --> WK1
    MQ --> WK2
```

此圖展示生產環境的水平擴展策略：

- Gateway 層 always-on，透過 LB 分散 SSE 連線：不可 scale-to-zero
- API Service 層可獨立擴展或以 Serverless 部署：無狀態，可 scale-to-zero
- Worker 層可依任務類型獨立擴展：STT-heavy 與 LLM-heavy 分開調度
