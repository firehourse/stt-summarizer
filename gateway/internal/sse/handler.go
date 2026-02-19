package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/redis/go-redis/v9"
)

// Handler SSE 連線管理器，處理 GET /api/tasks/{id}/events。
//
// 連線流程（防止 race condition）：
//  1. 先訂閱 Redis Pub/Sub channel，確保不遺漏事件
//  2. 再讀取 summary buffer，恢復已產生的摘要內容
//  3. 持續將 Redis 事件轉發至 SSE 回應流
//  4. 客戶端斷線時取消訂閱，釋放資源
type Handler struct {
	Redis *redis.Client
}

// NewHandler 建立 SSE Handler 實例。
func NewHandler(rdb *redis.Client) *Handler {
	return &Handler{Redis: rdb}
}

// ServeHTTP 處理單一 SSE 連線。
// 驗證 task ownership 後建立長連接，訂閱 Redis Pub/Sub 並即時推送事件至前端。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		http.Error(w, "Missing task ID", http.StatusBadRequest)
		return
	}

	// 驗證 task ownership：比對 Redis 中的 task:owner:{taskId} 與 X-User-Id
	userID := r.Header.Get("X-User-Id")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ownerKey := fmt.Sprintf("task:owner:%s", taskID)
	owner, err := h.Redis.Get(r.Context(), ownerKey).Result()
	if err == redis.Nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("SSE: failed to verify task ownership: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if owner != userID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 關閉 nginx 緩衝

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Step 1: 先訂閱，防止 buffer 讀取與新事件之間的 race condition
	channel := fmt.Sprintf("progress:%s", taskID)
	pubsub := h.Redis.Subscribe(ctx, channel)
	defer pubsub.Close()

	if _, err := pubsub.Receive(ctx); err != nil {
		log.Printf("SSE: failed to subscribe to %s: %v", channel, err)
		http.Error(w, "Failed to subscribe", http.StatusInternalServerError)
		return
	}

	// Step 2: 讀取 buffer，恢復 SSE 重連時遺失的內容
	// 2a. 轉譯內容恢復
	transBufferKey := fmt.Sprintf("transcript:buffer:%s", taskID)
	if tBuf, err := h.Redis.Get(ctx, transBufferKey).Result(); err == nil && tBuf != "" {
		event := map[string]string{
			"type":    "transcript_update",
			"content": tBuf,
		}
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}

	// 2b. 摘要內容恢復
	summaryBufferKey := fmt.Sprintf("summary:buffer:%s", taskID)
	if sBuf, err := h.Redis.Get(ctx, summaryBufferKey).Result(); err == nil && sBuf != "" {
		event := map[string]string{
			"type":    "summary_chunk",
			"content": sBuf,
		}
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	// Step 3: 持續轉發 Redis Pub/Sub 事件至 SSE
	ch := pubsub.Channel()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg.Payload)
			flusher.Flush()

		case <-ctx.Done():
			log.Printf("SSE: client disconnected for task %s", taskID)
			return
		}
	}
}
