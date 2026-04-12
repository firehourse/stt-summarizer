package db

import (
	"database/sql"
	"fmt"
	"os"

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

// SaveTranscript 以 Transaction 原子寫入轉錄結果並將 tasks.status 更新為 stt_completed。
// Worker 在 STT 階段完成後呼叫，中間態（stt_processing）僅存在 Redis Hash 中。
func SaveTranscript(db *sql.DB, taskID, transcript string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("SaveTranscript: begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO task_results (task_id, transcript, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (task_id) DO UPDATE SET transcript = $2, updated_at = NOW()`,
		taskID, transcript)
	if err != nil {
		return fmt.Errorf("SaveTranscript: upsert transcript: %w", err)
	}

	_, err = tx.Exec(
		`UPDATE tasks SET status = 'stt_completed', updated_at = NOW() WHERE id = $1`,
		taskID)
	if err != nil {
		return fmt.Errorf("SaveTranscript: update status: %w", err)
	}

	return tx.Commit()
}

// SaveSummary 以 Transaction 原子寫入摘要結果並將 tasks.status 更新為 completed。
// Worker 在 Summary 階段完成後呼叫。
func SaveSummary(db *sql.DB, taskID, summary string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("SaveSummary: begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO task_results (task_id, summary, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (task_id) DO UPDATE SET summary = $2, updated_at = NOW()`,
		taskID, summary)
	if err != nil {
		return fmt.Errorf("SaveSummary: upsert summary: %w", err)
	}

	_, err = tx.Exec(
		`UPDATE tasks SET status = 'completed', updated_at = NOW() WHERE id = $1`,
		taskID)
	if err != nil {
		return fmt.Errorf("SaveSummary: update status: %w", err)
	}

	return tx.Commit()
}

// SetTaskStatus 更新任務至終態（failed / cancelled）。
// 不帶 Atomic Check：Worker 已透過 Redis BLPOP 保證不重複消費。
func SetTaskStatus(db *sql.DB, taskID, status, errMsg string) error {
	_, err := db.Exec(
		`UPDATE tasks SET status = $1, error_message = $2, updated_at = NOW() WHERE id = $3`,
		status, errMsg, taskID)
	if err != nil {
		return fmt.Errorf("SetTaskStatus(%s, %s): %w", taskID, status, err)
	}
	return nil
}
