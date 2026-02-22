package sse

import (
	"context"
	"log"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Broadcaster 負責維持「唯一一條」 Redis Pub/Sub 連線 (Pattern 訂閱)。
// 將接收到的事件依照 taskId 分發給記憶體中註冊的 SSE 監聽者。
type Broadcaster struct {
	rdb         *redis.Client
	mu          sync.RWMutex
	clientChans map[string]map[chan string]bool
}

// NewBroadcaster 初始化 Multiplexer。
func NewBroadcaster(rdb *redis.Client) *Broadcaster {
	return &Broadcaster{
		rdb:         rdb,
		clientChans: make(map[string]map[chan string]bool),
	}
}

// Run 啟動背景監聽服務，此方法應設計為常駐 Goroutine。
func (b *Broadcaster) Run(ctx context.Context) {
	// 透過 PSUBSCRIBE 訂閱所有任務的進度頻道
	pubsub := b.rdb.PSubscribe(ctx, "progress:*")
	defer pubsub.Close()

	ch := pubsub.Channel()
	log.Println("Broadcaster started, listening to progress:*")

	for {
		select {
		case <-ctx.Done():
			log.Println("Broadcaster shutting down")
			return
		case msg, ok := <-ch:
			if !ok {
				log.Println("Redis pubsub channel closed")
				return
			}

			// 頻道名稱通常為 "progress:{taskId}"
			// 剝離前綴取得 taskId
			taskID := strings.TrimPrefix(msg.Channel, "progress:")

			b.mu.RLock()
			listeners := b.clientChans[taskID]
			// 分發給所有監聽這個 taskID 的 Client
			for c := range listeners {
				select {
				case c <- msg.Payload:
				default:
					// 如果某個 client 網路太慢導致 channel 滿載，
					// 則略過該訊息，確保不會 blocking (防範慢用戶拖垮系統)
				}
			}
			b.mu.RUnlock()
		}
	}
}

// Subscribe 讓 SSE Handler 向 Broadcaster 註冊，取得專屬的接受 Channel。
func (b *Broadcaster) Subscribe(taskID string) chan string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.clientChans[taskID] == nil {
		b.clientChans[taskID] = make(map[chan string]bool)
	}

	// channel 帶有適量 Buffer，抵抗瞬發流量
	ch := make(chan string, 16)
	b.clientChans[taskID][ch] = true
	return ch
}

// Unsubscribe 從記憶體中註銷並關閉 Channel，確保資源回收。
func (b *Broadcaster) Unsubscribe(taskID string, ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if listeners, ok := b.clientChans[taskID]; ok {
		delete(listeners, ch)
		if len(listeners) == 0 {
			delete(b.clientChans, taskID) // 避免 memory leak
		}
	}
	close(ch)
}
