# STT-AI: Speech-to-Text Summarization System

語音轉錄與 AI 摘要系統，採用 Go (Gateway)、Node.js (API Service) 與 Go (Worker) 的微服務架構。透過全異步設計，支援大檔案上傳與即時串流摘要。

## 快速啟動

### 1. 配置環境變數

將專案根目錄下的 `.env.example` 複製為 `.env` 並根據需求修改。

```bash
cp .env.example .env
```

- **MOCK=true** (預設): 使用模擬 AI 服務，無需 API Key 即可快速測試系統流程。
- **MOCK=false**: 系統將連接真實 AI 供應商，需配置以下各項：
  - **AI_STT_URL**: STT 服務端點（例如 OpenAI 官方或本地 Whisper Server）。
  - **AI_STT_MODEL**: 選擇模型（如 `whisper-1` 或自建 `large-v3-turbo`）。
  - **AI_STT_KEY**: 填寫對應的 API 授權 Key。
  - **AI_LLM_URL**: LLM 服務端點。
  - **AI_LLM_MODEL**: 選擇模型（如 `gemini-2.5-flash-lite` 或 `gpt-4`）。
  - **AI_LLM_KEY**: 填寫對應的 API 授權 Key。

### 2. 啟動服務

```bash
docker-compose up --build -d
```

### 3. 使用方式

- 前端介面: http://localhost:8090
- RabbitMQ 管理後台: http://localhost:15673 (guest/guest)

---

## 系統架構與設計

### 核心組件職責

- Gateway (Go): 系統長連接錨點。負責 SSE 連線管理、用戶 Cookie 識別與 API 反向代理。
- API Service (Node.js/TS): 業務邏輯服務。負責任務建立、1GB 串流上傳、任務分發至 Queue。
- Worker (Go): 智能處理核心。負責音檔切片 (VAD)、併發 STT 轉譯、LLM 串流摘要生成。
- Broker (RabbitMQ): 實現 STT 與 LLM 處理的非同步排隊與解耦。
- State (Redis): 負責即時進度推送 (Pub/Sub) 與串流摘要快取。
- DB (PostgreSQL): 任務狀態與結果的持久化儲存。

詳細架構圖與流程說明請參閱: [diagram.md](./diagram.md)

---

## API 端點說明

### 任務管理

| Method | Endpoint                  | Description                           |
| :----- | :------------------------ | :------------------------------------ |
| POST   | /api/tasks                | 初始化任務，獲取 taskId               |
| PUT    | /api/tasks/{id}/upload    | 串流上傳音檔 (支援 1GB)               |
| GET    | /api/tasks                | 查詢用戶歷史任務列表                  |
| GET    | /api/tasks/{id}           | 查詢特定任務詳情 (Transcript/Summary) |
| DELETE | /api/tasks/{id}           | 取消進行中的任務                      |
| POST   | /api/tasks/{id}/summarize | 對現有轉錄稿重新發起摘要              |

### 即時事件

| Method | Endpoint               | Description                      |
| :----- | :--------------------- | :------------------------------- |
| GET    | /api/tasks/{id}/events | SSE 端點，接收進度更新與摘要片段 |

---

## 技術亮點與實作細節

1. 非同步解耦架構:
   將 STT 與 LLM 拆分為獨立 Task。若 LLM 階段失敗，可直接重發 SUMMARY Task 而無需重新執行耗時且昂貴的 STT 轉譯。

2. 串流處理優化:
   全系統採用 Streaming Pipeline。音檔從前端上傳到 Worker 處理均不佔用記憶體空間。LLM 摘要採用串流輸出，透過 Redis Pub/Sub 即時推送至 Gateway 輸出給用戶。

3. 智能音檔切片:
   Worker 整合 VAD (Voice Activity Detection) 策略。針對大型音檔自動在靜音段進行切割，確保每段轉譯都在 25MB 限制內且語句不中斷。

4. 原子狀態保證:
   所有任務狀態變更均符合資料庫交易原則。Worker 在消費任務時具備原子性檢查，防止重複消費或狀態混亂。

---

## 專案結構

```
stt-ai/
├── gateway/             # Go: SSE 與反向代理層
├── api-service/         # Node.js: 業務邏輯與上傳處理
├── worker/              # Go: 音檔處理與 AI 模型整合
├── webapp-vue/          # Vue 3: 前端互動介面
├── infrastructure/      # SQL 與 Nginx 配置
└── docker-compose.yml   # 容器編排
```
