package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"tts-worker/internal/ai"
	"tts-worker/internal/db"
	rdb_lib "tts-worker/internal/redis"
	"tts-worker/internal/worker"

	"github.com/joho/godotenv"
)

// main 啟動 Worker 服務。
// 啟動順序：PostgreSQL → Redis → AI Service → STT consumer / Summary consumer / Reaper goroutines。
// DB/Redis 不可達時以 Fatal 終止（由 Docker restart 策略重啟）。
func main() {
	godotenv.Load(".env")

	// 建立 PostgreSQL 連線並驗證
	postgres, err := db.Connect()
	if err != nil {
		log.Fatal(err)
	}
	if err := postgres.Ping(); err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	log.Println("Database connected")

	// 套用所有尚未執行的 migrations
	if err := db.RunMigrations(postgres, "./migrations"); err != nil {
		log.Fatal("Migration failed:", err)
	}

	// 建立 Redis 連線
	rdb := rdb_lib.Connect()

	// 根據環境變數選擇 AI 服務實作（Mock / Standard）
	var sttSvc ai.STTService
	var llmSvc ai.Summarizer

	if os.Getenv("MOCK") == "true" {
		mock := &ai.MockAIService{}
		sttSvc = mock
		llmSvc = mock
		log.Println("Mock AI Services enabled")
	} else {
		sttURL := os.Getenv("AI_STT_URL")
		llmURL := os.Getenv("AI_LLM_URL")
		sttModel := os.Getenv("AI_STT_MODEL")
		llmModel := os.Getenv("AI_LLM_MODEL")
		sttKey := os.Getenv("AI_STT_KEY")
		llmKey := os.Getenv("AI_LLM_KEY")
		llmPrompt := os.Getenv("AI_LLM_PROMPT")

		if sttURL == "" || llmURL == "" || sttModel == "" || llmModel == "" {
			log.Fatal("Necessary AI configurations missing: AI_STT_URL/MODEL and AI_LLM_URL/MODEL must be provided")
		}

		provider := &ai.StandardAIProvider{
			STTApiKey: sttKey,
			STTURL:    sttURL,
			STTModel:  sttModel,
			LLMApiKey: llmKey,
			LLMURL:    llmURL,
			LLMModel:  llmModel,
			LLMPrompt: llmPrompt,
		}
		sttSvc = provider
		llmSvc = provider
		log.Println("Standard AI Services enabled (STT + LLM)")
	}

	w := worker.NewWorker(postgres, rdb, sttSvc, llmSvc)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 取消信號監聽（自帶重訂閱機制）
	go w.StartCancellationListener(ctx)

	// STT queue consumer
	go w.ConsumeSTTQueue(ctx)

	// Summary queue consumer
	go w.ConsumeSummaryQueue(ctx)

	// Reaper：回收 stt:processing 超時任務
	sttReaper := worker.NewReaper(rdb)
	go sttReaper.Start(ctx, "stt:processing", "stt:queue")

	// Reaper：回收 summary:processing 超時任務
	summaryReaper := worker.NewReaper(rdb)
	go summaryReaper.Start(ctx, "summary:processing", "summary:queue")

	log.Println("Worker ready (STT + Summary consumers, Reaper active)")

	<-ctx.Done()
	log.Println("Received shutdown signal, exiting...")
}
