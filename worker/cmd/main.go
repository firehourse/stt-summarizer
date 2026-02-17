package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"
	"tts-worker/internal/ai"
	"tts-worker/internal/db"
	"tts-worker/internal/models"
	rdb_lib "tts-worker/internal/redis"
	"tts-worker/internal/worker"

	"context"

	"github.com/joho/godotenv"
	"github.com/streadway/amqp"
)

const (
	baseReconnectDelay = 1 * time.Second
	maxReconnectDelay  = 30 * time.Second
)

// main 啟動 Worker 服務。
// 啟動順序：PostgreSQL → Redis → AI Service → RabbitMQ 重連迴圈 → 消費任務。
// DB/Redis 不可達時以 Fatal 終止（由 Docker restart 策略重啟），
// RabbitMQ 斷線時自動重連，不需要重啟容器。
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

	// 建立 Redis 連線
	rdb := rdb_lib.Connect()

	// 根據環境變數選擇 AI 服務實作（Mock / OpenAI）
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

		// URL 與 Model 是絕對必要的配置
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

	// 建立 Worker（分別注入 STT 與 LLM 專家）
	w := worker.NewWorker(postgres, rdb, sttSvc, llmSvc, nil)

	// 啟動取消信號監聽（自帶重訂閱機制）
	go w.StartCancellationListener(context.Background())

	// Graceful Shutdown：監聯 SIGINT/SIGTERM
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)

	// RabbitMQ 重連迴圈（獨立 goroutine）
	go consumeLoop(w, shutdownCh)

	<-shutdownCh
	log.Println("Received shutdown signal, exiting...")
}

// consumeLoop 持續維護 RabbitMQ 連線，斷線時自動以 exponential backoff 重連。
// 每次成功連線後呼叫 consume() 開始消費，consume() 返回代表連線中斷。
func consumeLoop(w *worker.Worker, shutdownCh chan os.Signal) {
	for {
		conn, err := connectRabbitMQ()
		if err != nil {
			// connectRabbitMQ 內部無限重試，理論上不會走到這
			log.Printf("RabbitMQ connect failed: %v", err)
			continue
		}

		err = consume(w, conn)
		conn.Close()
		log.Printf("RabbitMQ consumer stopped: %v, reconnecting...", err)

		// 檢查是否收到 shutdown 信號
		select {
		case sig := <-shutdownCh:
			// 把信號放回去讓 main 的 <-shutdownCh 也能收到
			shutdownCh <- sig
			return
		default:
		}
	}
}

// connectRabbitMQ 嘗試連線 RabbitMQ，失敗時以 exponential backoff 無限重試。
// 含 jitter 防止多 Worker 同時重連造成踩踏。
func connectRabbitMQ() (*amqp.Connection, error) {
	rabbitURL := fmt.Sprintf(
		"amqp://%s:%s@%s:5672/",
		os.Getenv("RABBITMQ_USER"),
		os.Getenv("RABBITMQ_PASS"),
		os.Getenv("RABBITMQ_HOST"),
	)

	for attempt := 0; ; attempt++ {
		conn, err := amqp.Dial(rabbitURL)
		if err == nil {
			if attempt > 0 {
				log.Printf("RabbitMQ connected (after %d retries)", attempt)
			} else {
				log.Println("RabbitMQ connected")
			}
			return conn, nil
		}

		delay := backoffDelay(attempt)
		log.Printf("RabbitMQ connect failed (attempt %d): %v, retrying in %v...", attempt+1, err, delay)
		time.Sleep(delay)
	}
}

// consume 在已建立的連線上建立 channel 並開始消費任務。
// 監聽 conn.NotifyClose 感知連線中斷，返回 error 觸發外層重連迴圈。
func consume(w *worker.Worker, conn *amqp.Connection) error {
	// 監聽連線斷開事件
	connCloseCh := conn.NotifyClose(make(chan *amqp.Error, 1))

	// 獨立的 publish channel，用於 Worker 將 SUMMARY Task 發布回 Queue
	publishCh, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to create publish channel: %v", err)
	}
	defer publishCh.Close()

	// 更新 Worker 的 publishCh（重連後舊 channel 已失效）
	w.SetPublishChannel(publishCh)

	// 獨立的 consume channel，與 publish 分離避免併發存取問題
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to create consume channel: %v", err)
	}
	defer ch.Close()

	// QoS prefetch = 5，控制單一 Worker 的併發上限
	if err := ch.Qos(5, 0, false); err != nil {
		return fmt.Errorf("failed to set QoS: %v", err)
	}

	// 宣告 queue（冪等操作，使啟動順序不受限制）
	if _, err := ch.QueueDeclare("tasks", true, false, false, false, nil); err != nil {
		return fmt.Errorf("failed to declare queue: %v", err)
	}

	// autoAck = false，使用 Manual Ack 確保任務可靠投遞
	msgs, err := ch.Consume("tasks", "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("failed to start consumer: %v", err)
	}

	log.Println("Worker ready for tasks...")

	// 同時監聽 消費訊息 與 連線斷開事件
	for {
		select {
		case amqpErr, ok := <-connCloseCh:
			if !ok {
				return fmt.Errorf("connection closed gracefully")
			}
			return fmt.Errorf("connection error: %v", amqpErr)

		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("consume channel closed")
			}

			var payload models.TaskPayload
			if err := json.Unmarshal(d.Body, &payload); err != nil {
				log.Printf("Error decoding message: %v", err)
				// 無法解析的訊息 Nack 且不重新入列，防止毒訊息阻塞佇列
				d.Nack(false, false)
				continue
			}

			go func(p models.TaskPayload, delivery amqp.Delivery) {
				w.ProcessTask(p)
				delivery.Ack(false)
			}(payload, d)
		}
	}
}

// backoffDelay 計算指數退避延遲，含 jitter 防止踩踏效應。
func backoffDelay(attempt int) time.Duration {
	exp := math.Min(
		float64(baseReconnectDelay)*math.Pow(2, float64(attempt)),
		float64(maxReconnectDelay),
	)
	jitter := rand.Float64() * exp * 0.5
	return time.Duration(exp + jitter)
}
