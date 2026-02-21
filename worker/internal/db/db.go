package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"tts-worker/internal/models"

	_ "github.com/lib/pq"
)

// Connect 建立 PostgreSQL 連線，透過 docker bridge network 連接。
func Connect() (*sql.DB, error) {
	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		os.Getenv("DB_HOST"),
		os.Getenv("DB_PORT"),
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASSWORD"),
		os.Getenv("DB_NAME"),
	)
	return sql.Open("postgres", connStr)
}

// UpdateTaskStatus 以 Transaction 原子更新任務狀態與結果。
// 狀態轉換帶有 Atomic Check，避免與 API Service 的取消指令產生 Race Condition：
//   - pending → processing：Worker 開始處理
//   - processing → completed/failed/cancelled：Worker 完成或異常
//
// 若 transcript 或 summary 非空，同步 UPSERT 至 task_results 表。
func UpdateTaskStatus(db *sql.DB, taskID string, status string, transcript string, summary string, errStr string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var result sql.Result
	if status == "processing" {
		result, err = tx.Exec(
			`UPDATE tasks SET status = $1, updated_at = NOW() WHERE id = $2 AND status = 'pending'`,
			status, taskID,
		)
	} else {
		result, err = tx.Exec(
			`UPDATE tasks SET status = $1, error_message = $2, updated_at = NOW() WHERE id = $3 AND status = 'processing'`,
			status, errStr, taskID,
		)
	}
	if err != nil {
		return err
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		// 無更新代表狀態衝突（例如任務已被取消），Worker 應放棄後續操作
		return fmt.Errorf("task %s state transition rejected (current status might not be compatible)", taskID)
	}

	if transcript != "" || summary != "" {
		resQuery := `
			INSERT INTO task_results (task_id, transcript, summary, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (task_id) DO UPDATE SET
				transcript = CASE WHEN $2 <> '' THEN $2 ELSE task_results.transcript END,
				summary = CASE WHEN $3 <> '' THEN $3 ELSE task_results.summary END,
				updated_at = NOW()`
		_, err = tx.Exec(resQuery, taskID, transcript, summary)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// SaveSTTResultsWithOutbox 將 STT 轉錄結果與 Outbox 事件綁定在同一個事務中。
// 這確保了「結果儲存」與「摘要請求」的原子性。
func SaveSTTResultsWithOutbox(db *sql.DB, taskID string, transcript string, outboxPayload interface{}) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 持久化轉錄結果
	_, err = tx.Exec(`
		INSERT INTO task_results (task_id, transcript, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (task_id) DO UPDATE SET
			transcript = $2,
			updated_at = NOW()`, taskID, transcript)
	if err != nil {
		return err
	}

	// 2. 插入 Outbox 事件
	payloadJSON, err := json.Marshal(outboxPayload)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`
		INSERT INTO outbox_events (aggregate_id, event_type, payload)
		VALUES ($1, $2, $3)`, taskID, "SUMMARY", payloadJSON)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ProcessOutboxEvents 以 Transaction 鎖定並取得待處理的 Outbox 事件（使用 SKIP LOCKED 避免 Worker 爭用）。
// 處理完成後，更新狀態為 sent。
func ProcessOutboxEvents(db *sql.DB, limit int, processFn func(event models.OutboxEvent) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id, event_type, payload, status, created_at
		FROM outbox_events
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return err
	}

	var events []models.OutboxEvent
	for rows.Next() {
		var e models.OutboxEvent
		if err := rows.Scan(&e.ID, &e.EventType, &e.Payload, &e.Status, &e.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		events = append(events, e)
	}
	rows.Close()

	if len(events) == 0 {
		return sql.ErrNoRows
	}

	for _, event := range events {
		if err := processFn(event); err != nil {
			continue // 單筆處理失敗，保留 pending 狀態以便重試
		}

		_, err = tx.Exec(`
			UPDATE outbox_events
			SET status = 'sent', processed_at = NOW()
			WHERE id = $1`, event.ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// StartSummaryCheck 針對 SUMMARY 任務執行 Atomic Check。
// 確保任務處於 processing 狀態，避免因 RabbitMQ 重複投遞而造成多次 LLM 調用。
func StartSummaryCheck(db *sql.DB, taskID string) error {
	result, err := db.Exec(`UPDATE tasks SET updated_at = NOW() WHERE id = $1 AND status = 'processing'`, taskID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("task %s state transition rejected (not in processing state)", taskID)
	}
	return nil
}

// CleanTimedOutTasks 尋找並標記已超時的「處理中」任務（Reaper Pattern）。
func CleanTimedOutTasks(db *sql.DB, timeoutMinutes int) (int64, error) {
	result, err := db.Exec(`
		UPDATE tasks
		SET status = 'failed', error_message = 'Task timed out (system recovery)', updated_at = NOW()
		WHERE status = 'processing' AND updated_at < NOW() - ($1 || ' minutes')::INTERVAL`, timeoutMinutes)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
