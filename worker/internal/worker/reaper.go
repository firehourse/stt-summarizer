package worker

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	reaperInterval    = 10 * time.Minute
	reaperLockTTL     = 2 * time.Minute
	reaperLockKeyBase = "worker:reaper:lock"
	taskTimeout       = 30 * time.Minute
)

// reaperScript 原子執行「掃描超時任務 → ZREM → LPUSH 重新入列」。
// KEYS[1] = processingZSet, KEYS[2] = queue, ARGV[1] = cutoff Unix timestamp（string）
// 回傳重新入列的任務數量。
var reaperScript = redis.NewScript(`
local members = redis.call('ZRANGEBYSCORE', KEYS[1], '0', ARGV[1])
for _, member in ipairs(members) do
    redis.call('ZREM', KEYS[1], member)
    redis.call('LPUSH', KEYS[2], member)
end
return #members
`)

// Reaper 負責定期掃描 processing ZSET，將超時卡死的任務重新入列。
// 透過 Redis SetNX leader election 確保多 Worker 部署時只有一個執行。
type Reaper struct {
	rdb *redis.Client
}

// NewReaper 建立 Reaper 實例。
func NewReaper(rdb *redis.Client) *Reaper {
	return &Reaper{rdb: rdb}
}

// Start 啟動 Reaper，定期掃描指定的 processingKey（ZSET）並將超時任務重新推回 queueKey（LIST）。
// 到 ctx 取消時退出。
func (r *Reaper) Start(ctx context.Context, processingKey, queueKey string) {
	log.Printf("Reaper started: %s → %s", processingKey, queueKey)
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Reaper stopped: %s", processingKey)
			return
		case <-ticker.C:
			// 每條 queue 使用獨立的 lock key，兩個 Reaper 可以並行執行
			lockKey := fmt.Sprintf("%s:%s", reaperLockKeyBase, processingKey)
			acquired, err := r.rdb.SetNX(ctx, lockKey, "locked", reaperLockTTL).Result()
			if err != nil {
				log.Printf("Reaper (%s): failed to acquire lock: %v", processingKey, err)
				continue
			}
			if !acquired {
				continue
			}

			cutoff := time.Now().Add(-taskTimeout).Unix()
			count, err := reaperScript.Run(ctx, r.rdb, []string{processingKey, queueKey}, cutoff).Int()
			if err != nil {
				log.Printf("Reaper (%s): requeue script failed: %v", processingKey, err)
			} else if count > 0 {
				log.Printf("Reaper (%s): requeued %d timed-out tasks → %s", processingKey, count, queueKey)
			}
		}
	}
}
