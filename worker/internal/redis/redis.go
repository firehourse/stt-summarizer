package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/redis/go-redis/v9"
)

// Connect 建立 Redis 連線，透過 docker bridge network 連接。
func Connect() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", os.Getenv("REDIS_HOST"), os.Getenv("REDIS_PORT")),
	})
}

// PublishProgress 將進度事件發布至 Redis Pub/Sub channel（progress:{taskID}）。
// Gateway 訂閱該 channel 後即時推送 SSE 至前端。
func PublishProgress(rdb *redis.Client, ctx context.Context, taskID string, progress interface{}) error {
	payload, _ := json.Marshal(progress)
	return rdb.Publish(ctx, fmt.Sprintf("progress:%s", taskID), payload).Err()
}

// SubscribeToCancellations 訂閱 cancel_channel，接收 API Service 發出的取消信號。
// Worker 收到信號後透過 context.Cancel() 終止進行中的 STT/LLM 作業。
func SubscribeToCancellations(rdb *redis.Client, ctx context.Context) *redis.PubSub {
	return rdb.Subscribe(ctx, "cancel_channel")
}
