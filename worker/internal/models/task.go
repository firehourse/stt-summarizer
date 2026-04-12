package models

import "time"

// TaskStatus 對應 DB tasks 表與 Redis HSET status 欄位的所有合法值。
const (
	StatusPending           = "pending"
	StatusSttQueued         = "stt_queued"
	StatusSttProcessing     = "stt_processing"
	StatusSttCompleted      = "stt_completed"
	StatusSummaryQueued     = "summary_queued"
	StatusSummaryProcessing = "summary_processing"
	StatusCompleted         = "completed"
	StatusFailed            = "failed"
	StatusCancelled         = "cancelled"
)

// STTPayload stt:queue 中的任務訊息格式。
// JSON tags 與 API Service 的 STTPayload interface 對齊。
type STTPayload struct {
	TaskID   string `json:"taskId"`
	UserID   string `json:"userId"`
	FilePath string `json:"filePath"`
	Config   struct {
		Language string `json:"language"`
		STTModel string `json:"sttModel"`
	} `json:"config"`
}

// SummaryPayload summary:queue 中的任務訊息格式。
// JSON tags 與 API Service 的 SummaryPayload interface 對齊。
type SummaryPayload struct {
	TaskID     string `json:"taskId"`
	UserID     string `json:"userId"`
	Transcript string `json:"transcript"`
	Config     struct {
		SummaryPrompt string `json:"summaryPrompt"`
	} `json:"config"`
}

// TaskStatus 對應 DB tasks 表結構，用於查詢任務狀態。
type TaskStatus struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	Status       string    `json:"status"`
	FilePath     string    `json:"file_path"`
	ErrorMessage string    `json:"error_message"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SSEEvent 透過 Redis Pub/Sub 發布的統一事件格式，Gateway 接收後轉發至 SSE。
// Type 可為 "progress", "transcript_update", "stt_completed", "summary_chunk", "completed", "failed", "cancelled"。
type SSEEvent struct {
	TaskID   string `json:"taskId"`
	Type     string `json:"type"`
	Status   string `json:"status,omitempty"`
	Progress int    `json:"progress,omitempty"`
	Message  string `json:"message,omitempty"`
	Content  string `json:"content,omitempty"`
}
