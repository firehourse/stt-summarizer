-- 000002_redis_queue_split.down.sql

-- 還原 status constraint（加回 processing，移除 stt_completed）
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS status_check;
ALTER TABLE tasks ADD CONSTRAINT status_check
    CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'cancelled'));

-- 還原 outbox_events 表
CREATE TABLE IF NOT EXISTS outbox_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    event_type VARCHAR(50) NOT NULL,
    payload JSONB NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    processed_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_outbox_status ON outbox_events(status);
CREATE INDEX IF NOT EXISTS idx_outbox_created_at ON outbox_events(created_at);
