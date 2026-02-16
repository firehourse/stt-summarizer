package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"stt-gateway/internal/middleware"
	"stt-gateway/internal/proxy"
	"stt-gateway/internal/sse"

	"github.com/redis/go-redis/v9"
)

// main 啟動 Gateway 服務。
// Gateway 是系統的長連接錨點，負責 SSE 連線管理、Cookie 核發、反向代理 API 請求。
// 此服務必須 always-on，不可 scale-to-zero。
func main() {
	redisHost := getEnv("REDIS_HOST", "redis")
	redisPort := getEnv("REDIS_PORT", "6379")
	apiServiceURL := getEnv("API_SERVICE_URL", "http://api-service:3000")
	port := getEnv("GATEWAY_PORT", "8081")

	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", redisHost, redisPort),
	})

	// 啟動時驗證 Redis 連線，失敗則終止進程
	if err := verifyRedisConnection(rdb); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Println("Redis connected")

	sseHandler := sse.NewHandler(rdb)
	apiProxy := proxy.NewAPIProxy(apiServiceURL)

	mux := http.NewServeMux()

	// SSE 端點由 Gateway 直接處理，不經過反向代理
	mux.Handle("GET /api/tasks/{id}/events", sseHandler)

	// 其餘 /api/* 請求代理至 API Service
	mux.Handle("/api/", apiProxy)

	// 健康檢查端點
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := verifyRedisConnection(rdb); err != nil {
			http.Error(w, "Redis unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// 套用 UserIdentity middleware（Cookie → X-User-Id）
	handler := middleware.UserIdentity(mux)

	log.Printf("Gateway starting on :%s", port)
	log.Printf("API Service: %s", apiServiceURL)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  0, // SSE 與串流上傳需要無限讀取
		WriteTimeout: 0, // SSE 需要無限寫入
		IdleTimeout:  120 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// verifyRedisConnection 以 5 秒 timeout 執行 PING 驗證 Redis 連線。
func verifyRedisConnection(rdb *redis.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %v", err)
	}
	return nil
}

// getEnv 取得環境變數，不存在時返回 fallback 預設值。
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
