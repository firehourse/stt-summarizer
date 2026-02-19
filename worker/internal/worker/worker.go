package worker

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/streadway/amqp"
)

// Worker 任務處理器，持有所有外部依賴的連線。
// activeCancels 儲存進行中任務的 cancel 函數，供取消信號觸發時使用。
// publishCh 與 publishMu 確保發布 SUMMARY Task 回 Queue 時的執行緒安全。
type Worker struct {
	DB            *sql.DB
	Redis         *redis.Client
	STT           ai.STTService
	LLM           ai.Summarizer
	activeCancels sync.Map
	publishCh     *amqp.Channel
	publishMu     sync.Mutex
}

// NewWorker 建立 Worker 實例，注入所有外部依賴。
// publishCh 可為 nil，後續透過 SetPublishChannel 設定（支援重連場景）。
func NewWorker(postgres *sql.DB, rdb *redis.Client, sttSvc ai.STTService, llmSvc ai.Summarizer, publishCh *amqp.Channel) *Worker {
	return &Worker{
		DB:        postgres,
		Redis:     rdb,
		STT:       sttSvc,
		LLM:       llmSvc,
		publishCh: publishCh,
	}
}

// SetPublishChannel 更新 publish channel（RabbitMQ 重連後舊 channel 已失效）。
// 透過 mutex 確保與 publishTask 的執行緒安全。
func (w *Worker) SetPublishChannel(ch *amqp.Channel) {
	w.publishMu.Lock()
	defer w.publishMu.Unlock()
	w.publishCh = ch
}

// StartCancellationListener 訂閱 Redis cancel_channel，
// 收到取消信號時呼叫對應任務的 context.Cancel() 終止進行中的 STT/LLM 作業。
// 內建自動重訂閱：Pub/Sub 斷線後等待 3 秒重新訂閱，直到 ctx 被取消。
func (w *Worker) StartCancellationListener(ctx context.Context) {
	for {
		w.listenCancellations(ctx)

		// ctx 被取消代表 Worker 正在關閉，不需要重訂閱
		if ctx.Err() != nil {
			log.Println("Cancellation listener stopped (context cancelled)")
			return
		}

		log.Println("Cancellation listener disconnected, resubscribing in 3s...")
		time.Sleep(3 * time.Second)
	}
}

// listenCancellations 單次訂閱 Redis cancel_channel 並處理取消信號。
// channel 被關閉或斷線時返回，由 StartCancellationListener 決定是否重訂閱。
func (w *Worker) listenCancellations(ctx context.Context) {
	pubsub := rdb_lib.SubscribeToCancellations(w.Redis, ctx)
	defer pubsub.Close()

	ch := pubsub.Channel()
	for msg := range ch {
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

// ProcessTask 任務處理入口，根據 Type 分派至 handleSTT 或 handleSummary。
// 每個任務擁有獨立的 context，支援透過 Cancel Signal 即時終止。
func (w *Worker) ProcessTask(payload models.TaskPayload) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w.activeCancels.Store(payload.TaskID, cancel)
	defer w.activeCancels.Delete(payload.TaskID)

	if payload.Type == "SUMMARY" {
		w.handleSummary(ctx, payload)
	} else {
		w.handleSTT(ctx, payload)
	}
}

// handleSTT 執行 STT 階段：音檔切片 → 併發轉錄 → 儲存 transcript → 發布 SUMMARY Task。
// 流程中的每個階段都會透過 Redis Pub/Sub 發布進度事件。
func (w *Worker) handleSTT(ctx context.Context, payload models.TaskPayload) {
	log.Printf("Processing STT task: %s", payload.TaskID)
	err := db.UpdateTaskStatus(w.DB, payload.TaskID, "processing", "", "", "")
	if err != nil {
		w.notifyEvent(payload.TaskID, "failed", "Task already cancelled or invalid state")
		return
	}
	w.notifyProgress(payload.TaskID, 10, "音檔處理中...")

	// 1. 音檔切片（VAD 優先）
	const defaultMaxChunkDuration = 30.0
	chunks, err := audio.SplitAudio(payload.FilePath, defaultMaxChunkDuration)
	if err != nil {
		w.handleError(payload, err)
		return
	}
	defer audio.CleanupChunks(chunks)

	w.notifyProgress(payload.TaskID, 30, fmt.Sprintf("語音轉譯中（%d 段）...", len(chunks)))

	// 2. 併發轉錄（Goroutine + Semaphore = 5，任一失敗即取消全部）
	transcripts := make([]string, len(chunks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	sttCtx, sttCancel := context.WithCancel(ctx)
	defer sttCancel()

	var firstErr atomic.Value
	var completedChunks int32

	// 累進式推送狀態
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

			chunkTranscript, err := w.STT.STT(chunkCtx, c.FilePath)
			if err != nil {
				if firstErr.CompareAndSwap(nil, err) {
					// 發生錯誤，通知同組的其他 goroutine 取消
					sttCancel()
				}
				return
			}
			transcripts[idx] = chunkTranscript

			completed := atomic.AddInt32(&completedChunks, 1)
			w.notifyProgress(payload.TaskID, 30+int(completed)*40/len(chunks), "語音轉譯中...")

			// 累進式順序推送文字給前端
			streamingMu.Lock()
			if idx == nextToStream {
				// 如果完成的是我們正在等待的下一個分片，就開始往後推
				for nextToStream < len(chunks) && transcripts[nextToStream] != "" {
					currentFullTranscript = mergeTranscripts(currentFullTranscript, transcripts[nextToStream])
					nextToStream++
				}
				w.notifyTranscriptUpdate(payload.TaskID, currentFullTranscript)
			}
			streamingMu.Unlock()
		}(i, chunk)
	}

	wg.Wait()

	if storedErr := firstErr.Load(); storedErr != nil {
		w.handleError(payload, storedErr.(error))
		return
	}

	if ctx.Err() != nil {
		w.handleError(payload, ctx.Err())
		return
	}

	// 3. 智能合併轉錄結果（移除 Overlap 產生的重複詞彙）
	fullTranscript := ""
	if len(transcripts) > 0 {
		fullTranscript = transcripts[0]
		for i := 1; i < len(transcripts); i++ {
			fullTranscript = mergeTranscripts(fullTranscript, transcripts[i])
		}
	}

	if err := db.SaveTranscript(w.DB, payload.TaskID, fullTranscript); err != nil {
		w.handleError(payload, fmt.Errorf("failed to save transcript: %v", err))
		return
	}

	w.notifyProgress(payload.TaskID, 75, "轉錄完成，準備生成摘要...")
	w.cleanup(payload.FilePath)

	// 4. 發布 SUMMARY Task 回 Queue（解耦 Pipeline）
	summaryPayload := models.TaskPayload{
		TaskID:     payload.TaskID,
		CreatorID:  payload.CreatorID,
		Type:       "SUMMARY",
		Transcript: fullTranscript,
		Config:     payload.Config,
	}
	if err := w.publishTask(summaryPayload); err != nil {
		w.handleError(payload, fmt.Errorf("failed to enqueue summary task: %v", err))
		return
	}
}

// handleSummary 執行 LLM 摘要階段：串流生成摘要，每個 chunk 即時推送 SSE 並更新 Redis buffer。
func (w *Worker) handleSummary(ctx context.Context, payload models.TaskPayload) {
	log.Printf("Processing Summary task: %s", payload.TaskID)
	w.notifyProgress(payload.TaskID, 80, "摘要生成中...")

	var summaryBuffer strings.Builder

	err := w.LLM.SummarizeStream(ctx, payload.Transcript, "", func(chunk string) {
		summaryBuffer.WriteString(chunk)
		w.notifySummaryChunk(payload.TaskID, chunk)
		// 更新 Redis buffer，供 SSE 重連時恢復已產生的摘要
		w.Redis.Set(ctx, fmt.Sprintf("summary:buffer:%s", payload.TaskID), summaryBuffer.String(), 10*time.Minute)
	})

	if err != nil {
		w.handleError(payload, err)
		return
	}

	summary := summaryBuffer.String()

	// 原子更新：processing → completed + 儲存 summary（Transaction）
	if err := db.UpdateTaskStatus(w.DB, payload.TaskID, "completed", "", summary, ""); err != nil {
		log.Printf("Failed to complete task %s: %v", payload.TaskID, err)
		return
	}

	w.notifyCompleted(payload.TaskID)
}

// publishTask 將任務訊息發布回 RabbitMQ tasks 佇列。
// 使用獨立的 publishCh 與 mutex 確保執行緒安全。
func (w *Worker) publishTask(payload models.TaskPayload) error {
	w.publishMu.Lock()
	defer w.publishMu.Unlock()

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return w.publishCh.Publish("", "tasks", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	})
}

// --- SSE 事件輔助函式 ---

// notifyProgress 發布進度更新事件至 Redis Pub/Sub。
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

// notifySummaryChunk 發布摘要片段事件，前端收到後即時渲染。
func (w *Worker) notifySummaryChunk(taskID, content string) {
	event := models.SSEEvent{
		TaskID:  taskID,
		Type:    "summary_chunk",
		Content: content,
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

// notifyCompleted 發布任務完成事件。
func (w *Worker) notifyCompleted(taskID string) {
	event := models.SSEEvent{
		TaskID: taskID,
		Type:   "completed",
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

// notifyTranscriptUpdate 發布累進式的轉錄結果，前端收到後直接替換顯示內容。
func (w *Worker) notifyTranscriptUpdate(taskID, content string) {
	event := models.SSEEvent{
		TaskID:  taskID,
		Type:    "transcript_update",
		Content: content,
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

// notifyEvent 發布通用事件（如 failed、cancelled）。
func (w *Worker) notifyEvent(taskID, eventType, msg string) {
	event := models.SSEEvent{
		TaskID:  taskID,
		Type:    eventType,
		Status:  eventType,
		Message: msg,
	}
	rdb_lib.PublishProgress(w.Redis, context.Background(), taskID, event)
}

// handleError 統一錯誤處理：區分 context.Canceled（用戶取消）與其他錯誤。
// 更新 DB 狀態、發布 SSE 事件，STT 階段額外清理音檔。
func (w *Worker) handleError(payload models.TaskPayload, err error) {
	eventType := "failed"
	if err == context.Canceled {
		eventType = "cancelled"
		log.Printf("Task %s cancelled", payload.TaskID)
	} else {
		log.Printf("Task %s failed: %v", payload.TaskID, err)
	}

	db.UpdateTaskStatus(w.DB, payload.TaskID, eventType, "", "", err.Error())
	w.notifyEvent(payload.TaskID, eventType, err.Error())

	if payload.Type == "STT" {
		w.cleanup(payload.FilePath)
	}
}

// mergeTranscripts 智能合併兩段具有重疊可能的文字。
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

	// 設定最大比對窗口（例如 10 個單詞，足以涵蓋 1.5s 的重疊）
	maxMatch := 10
	if len(w1) < maxMatch {
		maxMatch = len(w1)
	}
	if len(w2) < maxMatch {
		maxMatch = len(w2)
	}

	bestMatchLen := 0
	for i := 1; i <= maxMatch; i++ {
		// 取 t1 最後 i 個單詞與 t2 最前 i 個單詞比較
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

	// 合併：t1 + t2(去掉重複部分)
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
