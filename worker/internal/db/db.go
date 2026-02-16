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

// GetTaskStatus 查詢任務的當前狀態與擁有者。
func GetTaskStatus(db *sql.DB, taskID string) (string, string, error) {
	var status, userID string
	err := db.QueryRow("SELECT status, user_id FROM tasks WHERE id = $1", taskID).Scan(&status, &userID)
	return status, userID, err
}

// SaveTranscript 在 STT 完成後持久化 transcript，不改變任務狀態。
// 這確保 STT 結果在 SUMMARY 階段開始前已安全儲存於 DB，
// 即使 LLM 失敗也不會遺失已轉錄的內容。
func SaveTranscript(db *sql.DB, taskID string, transcript string) error {
	_, err := db.Exec(`
		INSERT INTO task_results (task_id, transcript, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (task_id) DO UPDATE SET
			transcript = $2,
			updated_at = NOW()`, taskID, transcript)
	return err
}
