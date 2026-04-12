package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"tts-worker/internal/ai"
	"tts-worker/internal/audio"
	"tts-worker/internal/db"
	"tts-worker/internal/models"
	rdb_lib "tts-worker/internal/redis"

	"github.com/redis/go-redis/v9"
)

const (
	queueSTT        = "stt:queue"
	queueSummary    = "summary:queue"
	processingSTT   = "stt:processing"
	processingSummary = "summary:processing"
)

// Worker 任務處理器，持有所有外部依賴的連線。
// activeCancels 儲存進行中任務的 cancel 函數，供取消信號觸發時使用。
type Worker struct {
	DB            *sql.DB
	Redis         *redis.Client
	STT           ai.STTService
	LLM           ai.Summarizer
	activeCancels sync.Map
}

// NewWorker 建立 Worker 實例，注入所有外部依賴。
func NewWorker(postgres *sql.DB, rdb *redis.Client, sttSvc ai.STTService, llmSvc ai.Summarizer) *Worker {
	return &Worker{
		DB:    postgres,
		Redis: rdb,
		STT:   sttSvc,
		LLM:   llmSvc,
	}
}

// StartCancellationListener 訂閱 Redis cancel_channel，
// 收到取消信號時呼叫對應任務的 context.Cancel() 終止進行中的 STT/LLM 作業。
// 內建自動重訂閱：Pub/Sub 斷線後等待 3 秒重新訂閱，直到 ctx 被取消。
func (w *Worker) StartCancellationListener(ctx context.Context) {
	for {
		w.listenCancellations(ctx)

		if ctx.Err() != nil {
			log.Println("Cancellation listener stopped (context cancelled)")
			return
		}

		log.Println("Cancellation listener disconnected, resubscribing in 3s...")
		time.Sleep(3 * time.Second)
	}
}

func (w *Worker) listenCancellations(ctx context.Context) {
	pubsub := rdb_lib.SubscribeToCancellations(w.Redis, ctx)
	defer pubsub.Close()

	for msg := range pubsub.Channel() {
		var cancelMsg struct {
			TaskID string `json:"taskId"`
		}
		if err := json.Unmarshal([]byte(msg.Payload), &cancelMsg); err == nil {
			if cancel, ok := w.activeCancels.Load(cancelMsg.TaskID); ok {
				log.Printf("Received cancellation for task %s", cancelMsg.TaskID)
				cancel.(context.CancelFunc)()
			}
		}
	}
}

// ConsumeSTTQueue 阻塞消費 stt:queue，每個任務在獨立 goroutine 中處理。
// BLPOP 原子取出後立即 ZADD 至 stt:processing ZSET 供 Reaper 追蹤。
func (w *Worker) ConsumeSTTQueue(ctx context.Context) {
	log.Println("STT queue consumer started")
	for {
		result, err := w.Redis.BLPop(ctx, 0, queueSTT).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("ConsumeSTTQueue: BLPOP error: %v, retrying in 1s...", err)
			time.Sleep(time.Second)
			continue
		}

		rawPayload := result[1]
		w.Redis.ZAdd(ctx, processingSTT, redis.Z{
			Score:  float64(time.Now().Unix()),
			Member: rawPayload,
		})

		var payload models.STTPayload
		if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
			log.Printf("ConsumeSTTQueue: unmarshal error: %v, discarding message", err)
			w.Redis.ZRem(ctx, processingSTT, rawPayload)
			continue
		}

		go func(p models.STTPayload, raw string) {
			taskCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.activeCancels.Store(p.TaskID, cancel)
			defer w.activeCancels.Delete(p.TaskID)
			w.handleSTT(taskCtx, p, raw)
		}(payload, rawPayload)
	}
}

// ConsumeSummaryQueue 阻塞消費 summary:queue，每個任務在獨立 goroutine 中處理。
func (w *Worker) ConsumeSummaryQueue(ctx context.Context) {
	log.Println("Summary queue consumer started")
	for {
		result, err := w.Redis.BLPop(ctx, 0, queueSummary).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("ConsumeSummaryQueue: BLPOP error: %v, retrying in 1s...", err)
			time.Sleep(time.Second)
			continue
		}

		rawPayload := result[1]
		w.Redis.ZAdd(ctx, processingSummary, redis.Z{
			Score:  float64(time.Now().Unix()),
			Member: rawPayload,
		})

		var payload models.SummaryPayload
		if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
			log.Printf("ConsumeSummaryQueue: unmarshal error: %v, discarding message", err)
			w.Redis.ZRem(ctx, processingSummary, rawPayload)
			continue
		}

		go func(p models.SummaryPayload, raw string) {
			taskCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.activeCancels.Store(p.TaskID, cancel)
			defer w.activeCancels.Delete(p.TaskID)
			w.handleSummary(taskCtx, p, raw)
		}(payload, rawPayload)
	}
}

// handleSTT 執行 STT 階段：音檔切片 → 並發轉錄（retry x3）→ mergeTranscripts → 儲存 transcript → 通知 stt_completed。
func (w *Worker) handleSTT(ctx context.Context, payload models.STTPayload, rawPayload string) {
	log.Printf("Processing STT task: %s", payload.TaskID)

	w.Redis.HSet(ctx, "task:"+payload.TaskID, "status", models.StatusSttProcessing, "startedAt", fmt.Sprintf("%d", time.Now().Unix()))
	w.notifyProgress(payload.TaskID, 10, "音檔處理中...")

	// 1. 音檔切片（VAD 優先）
	const defaultMaxChunkDuration = 30.0
	chunks, err := audio.SplitAudio(payload.FilePath, defaultMaxChunkDuration)
	if err != nil {
		w.handleSTTError(ctx, payload, rawPayload, err)
		return
	}
	defer audio.CleanupChunks(chunks)

	w.notifyProgress(payload.TaskID, 30, fmt.Sprintf("語音轉譯中（%d 段）...", len(chunks)))

	// 2. 並發轉錄（Semaphore = 2，降低本地 GPU 壓力）
	transcripts := make([]string, len(chunks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 2)

	sttCtx, sttCancel := context.WithCancel(ctx)
	defer sttCancel()

	var firstErr atomic.Value
	var completedChunks int32

	var streamingMu sync.Mutex
	nextToStream := 0
	currentFullTranscript := ""

	for i, chunk := range chunks {
		wg.Add(1)
		go func(idx int, c audio.Chunk) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-sttCtx.Done():
				return
			}

			chunkCtx, chunkCancel := context.WithTimeout(sttCtx, 5*time.Minute)
			defer chunkCancel()

			var chunkTranscript string
			var sttErr error
			for attempt := 0; attempt < 3; attempt++ {
				chunkTranscript, sttErr = w.STT.STT(chunkCtx, c.FilePath)
				if sttErr == nil {
					break
				}
				log.Printf("STT attempt %d failed for chunk %d of task %s: %v, retrying in 2s...", attempt+1, idx, payload.TaskID, sttErr)
				time.Sleep(2 * time.Second)
			}

			if sttErr != nil {
				if firstErr.CompareAndSwap(nil, sttErr) {
					sttCancel()
				}
				return
			}
			transcripts[idx] = chunkTranscript

			completed := atomic.AddInt32(&completedChunks, 1)
			w.notifyProgress(payload.TaskID, 30+int(completed)*40/len(chunks), "語音轉譯中...")

			// 累進式順序推送轉錄文字至前端
			streamingMu.Lock()
			if idx == nextToStream {
				for nextToStream < len(chunks) && transcripts[nextToStream] != "" {
					currentFullTranscript = mergeTranscripts(currentFullTranscript, transcripts[nextToStream])
					nextToStream++
				}
				w.notifyTranscriptUpdate(payload.TaskID, currentFullTranscript)
				w.Redis.Set(ctx, fmt.Sprintf("transcript:buffer:%s", payload.TaskID), currentFullTranscript, 10*time.Minute)
			}
			streamingMu.Unlock()
		}(i, chunk)
	}

	wg.Wait()

	if storedErr := firstErr.Load(); storedErr != nil {
		w.handleSTTError(ctx, payload, rawPayload, storedErr.(error))
		return
	}

	if ctx.Err() != nil {
		w.handleSTTError(ctx, payload, rawPayload, ctx.Err())
		return
	}

	// 3. 智能合併轉錄結果
	fullTranscript := ""
	if len(transcripts) > 0 {
		fullTranscript = transcripts[0]
		for i := 1; i < len(transcripts); i++ {
			fullTranscript = mergeTranscripts(fullTranscript, transcripts[i])
		}
	}

	// 4. 持久化：transcript 寫入 DB，tasks.status=stt_completed
	if err := db.SaveTranscript(w.DB, payload.TaskID, fullTranscript); err != nil {
		w.handleSTTError(ctx, payload, rawPayload, fmt.Errorf("SaveTranscript: %w", err))
		return
	}

	// 5. Redis 狀態更新：HSET stt_completed → ZREM → PUBLISH
	w.Redis.HSet(ctx, "task:"+payload.TaskID, "status", models.StatusSttCompleted)
	w.Redis.ZRem(ctx, processingSTT, rawPayload)
	w.notifySTTCompleted(payload.TaskID)
	w.notifyProgress(payload.TaskID, 75, "轉錄完成，等待觸發摘要...")
	w.cleanup(payload.FilePath)
}

// handleSummary 執行 LLM 摘要階段：串流生成摘要 → 每個 chunk 即時推送 SSE → 儲存 summary。
func (w *Worker) handleSummary(ctx context.Context, payload models.SummaryPayload, rawPayload string) {
	log.Printf("Processing Summary task: %s", payload.TaskID)

	w.Redis.HSet(ctx, "task:"+payload.TaskID, "status", models.StatusSummaryProcessing)
	w.notifyProgress(payload.TaskID, 80, "摘要生成中...")

	var summaryBuffer strings.Builder

	err := w.LLM.SummarizeStream(ctx, payload.Transcript, payload.Config.SummaryPrompt, func(chunk string) {
		summaryBuffer.WriteString(chunk)
		w.notifySummaryChunk(payload.TaskID, chunk)
		w.Redis.Set(ctx, fmt.Sprintf("summary:buffer:%s", payload.TaskID), summaryBuffer.String(), 10*time.Minute)
	})

	if err != nil {
		w.handleSummaryError(ctx, payload, rawPayload, err)
		return
	}

	// 持久化：summary 寫入 DB，tasks.status=completed
	if err := db.SaveSummary(w.DB, payload.TaskID, summaryBuffer.String()); err != nil {
		w.handleSummaryError(ctx, payload, rawPayload, fmt.Errorf("SaveSummary: %w", err))
		return
	}

	w.Redis.HSet(ctx, "task:"+payload.TaskID, "status", models.StatusCompleted)
	w.Redis.ZRem(ctx, processingSummary, rawPayload)
	w.notifyCompleted(payload.TaskID)
}

// handleSTTError 統一 STT 錯誤處理：區分 Canceled（用戶取消）與其他錯誤，清理音檔。
func (w *Worker) handleSTTError(ctx context.Context, payload models.STTPayload, rawPayload string, err error) {
	eventType := models.StatusFailed
	if errors.Is(err, context.Canceled) {
		eventType = models.StatusCancelled
		log.Printf("STT task %s cancelled", payload.TaskID)
	} else {
		log.Printf("STT task %s failed: %v", payload.TaskID, err)
	}
	if dbErr := db.SetTaskStatus(w.DB, payload.TaskID, eventType, err.Error()); dbErr != nil {
		log.Printf("STT task %s: failed to persist terminal status: %v", payload.TaskID, dbErr)
	}
	w.Redis.HSet(ctx, "task:"+payload.TaskID, "status", eventType)
	w.Redis.ZRem(ctx, processingSTT, rawPayload)
	w.notifyEvent(payload.TaskID, eventType, err.Error())
	w.cleanup(payload.FilePath)
}

// handleSummaryError 統一 Summary 錯誤處理。
func (w *Worker) handleSummaryError(ctx context.Context, payload models.SummaryPayload, rawPayload string, err error) {
	eventType := models.StatusFailed
	if errors.Is(err, context.Canceled) {
		eventType = models.StatusCancelled
		log.Printf("Summary task %s cancelled", payload.TaskID)
	} else {
		log.Printf("Summary task %s failed: %v", payload.TaskID, err)
	}
	if dbErr := db.SetTaskStatus(w.DB, payload.TaskID, eventType, err.Error()); dbErr != nil {
		log.Printf("Summary task %s: failed to persist terminal status: %v", payload.TaskID, dbErr)
	}
	w.Redis.HSet(ctx, "task:"+payload.TaskID, "status", eventType)
	w.Redis.ZRem(ctx, processingSummary, rawPayload)
	w.notifyEvent(payload.TaskID, eventType, err.Error())
}

// --- SSE 事件輔助函式 ---

func (w *Worker) notifyProgress(taskID string, progress int, msg string) {
	event := models.SSEEvent{
		TaskID:   taskID,
		Type:     "progress",
		Status:   "processing",
		Progress: progress,
		Message:  msg,
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

func (w *Worker) notifySTTCompleted(taskID string) {
	event := models.SSEEvent{
		TaskID: taskID,
		Type:   "stt_completed",
		Status: "stt_completed",
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

func (w *Worker) notifySummaryChunk(taskID, content string) {
	event := models.SSEEvent{
		TaskID:  taskID,
		Type:    "summary_chunk",
		Content: content,
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

func (w *Worker) notifyCompleted(taskID string) {
	event := models.SSEEvent{
		TaskID: taskID,
		Type:   "completed",
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

func (w *Worker) notifyTranscriptUpdate(taskID, content string) {
	event := models.SSEEvent{
		TaskID:  taskID,
		Type:    "transcript_update",
		Content: content,
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

func (w *Worker) notifyEvent(taskID, eventType, msg string) {
	event := models.SSEEvent{
		TaskID:  taskID,
		Type:    eventType,
		Status:  eventType,
		Message: msg,
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

// mergeTranscripts 智能合併兩段具有重疊可能的文字（最多 10 個 token 窗口）。
func mergeTranscripts(t1, t2 string) string {
	t1 = strings.TrimSpace(t1)
	t2 = strings.TrimSpace(t2)
	if t1 == "" {
		return t2
	}
	if t2 == "" {
		return t1
	}

	w1 := strings.Fields(t1)
	w2 := strings.Fields(t2)

	maxMatch := 10
	if len(w1) < maxMatch {
		maxMatch = len(w1)
	}
	if len(w2) < maxMatch {
		maxMatch = len(w2)
	}

	bestMatchLen := 0
	for i := 1; i <= maxMatch; i++ {
		match := true
		for j := 0; j < i; j++ {
			if w1[len(w1)-i+j] != w2[j] {
				match = false
				break
			}
		}
		if match {
			bestMatchLen = i
		}
	}

	remainingW2 := w2[bestMatchLen:]
	if len(remainingW2) == 0 {
		return t1
	}
	return t1 + " " + strings.Join(remainingW2, " ")
}

// cleanup 刪除已處理完成的音檔，釋放磁碟空間。
func (w *Worker) cleanup(filePath string) {
	if _, err := os.Stat(filePath); err == nil {
		os.Remove(filePath)
	}
}
