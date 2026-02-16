package models

import "time"

// TaskPayload RabbitMQ 佇列中的任務訊息格式。
// STT 任務攜帶 FilePath，SUMMARY 任務攜帶 Transcript。
type TaskPayload struct {
	TaskID     string `json:"taskId"`
	CreatorID  string `json:"creatorId"`
	FilePath   string `json:"filePath,omitempty"`
	Type       string `json:"type"` // "STT" 或 "SUMMARY"
	Transcript string `json:"transcript,omitempty"`
	Config     struct {
		Language      string `json:"language"`
		STTModel      string `json:"sttModel"`
		SummaryPrompt string `json:"summaryPrompt"`
	} `json:"config"`
}

// TaskStatus 對應 DB tasks 表結構，用於查詢任務狀態。
type TaskStatus struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	Status       string    `json:"status"`
	FilePath     string    `json:"file_path"`
	Transcript   string    `json:"transcript"`
	Summary      string    `json:"summary"`
	ErrorMessage string    `json:"error_message"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SSEEvent 透過 Redis Pub/Sub 發布的統一事件格式，Gateway 接收後轉發至 SSE。
// Type 可為 "progress", "summary_chunk", "completed", "failed", "cancelled"。
type SSEEvent struct {
	TaskID   string `json:"taskId"`
	Type     string `json:"type"`
	Status   string `json:"status,omitempty"`
	Progress int    `json:"progress,omitempty"`
	Message  string `json:"message,omitempty"`
	Content  string `json:"content,omitempty"` // summary_chunk 專用
}
