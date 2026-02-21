CREATE TABLE IF NOT EXISTS tasks (
    id UUID PRIMARY KEY,
    user_id VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    file_path TEXT,
    error_message TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT status_check CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'cancelled'))
);

CREATE TABLE IF NOT EXISTS task_results (
    task_id UUID PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    transcript TEXT,
    summary TEXT,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_tasks_user_id ON tasks(user_id);
CREATE INDEX idx_tasks_status ON tasks(status);
 
 CREATE TABLE IF NOT EXISTS outbox_events (
     id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
     aggregate_id UUID NOT NULL,
     event_type VARCHAR(50) NOT NULL,     -- 'STT', 'SUMMARY'
     payload JSONB NOT NULL,
     status VARCHAR(20) NOT NULL DEFAULT 'pending', -- 'pending', 'sent', 'failed'
     created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
     processed_at TIMESTAMP WITH TIME ZONE
 );
 
 CREATE INDEX idx_outbox_status ON outbox_events(status);
 CREATE INDEX idx_outbox_created_at ON outbox_events(created_at);
