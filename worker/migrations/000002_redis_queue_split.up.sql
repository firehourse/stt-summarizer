-- 000002_redis_queue_split.up.sql
-- Remove Outbox pattern; switch queue from RabbitMQ to Redis LIST+ZSET.
-- Add stt_completed as a DB-visible intermediate status.

-- 1. 擴充 status 合法值：加入 stt_completed，移除 processing（中間態改由 Redis 管理）
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS status_check;
ALTER TABLE tasks ADD CONSTRAINT status_check
    CHECK (status IN ('pending', 'stt_completed', 'completed', 'failed', 'cancelled'));

-- 2. 移除 Outbox 表（Redis queue 取代 Transactional Outbox）
DROP INDEX IF EXISTS idx_outbox_status;
DROP INDEX IF EXISTS idx_outbox_created_at;
DROP TABLE IF EXISTS outbox_events;
