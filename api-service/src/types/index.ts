/** 任務狀態常數，對應 DB tasks 表與 Redis HSET status 欄位 */
export enum TaskStatus {
  Pending           = 'pending',
  SttQueued         = 'stt_queued',
  SttProcessing     = 'stt_processing',
  SttCompleted      = 'stt_completed',
  SummaryQueued     = 'summary_queued',
  SummaryProcessing = 'summary_processing',
  Completed         = 'completed',
  Failed            = 'failed',
  Cancelled         = 'cancelled',
}

/** STT 任務訊息，推送至 stt:queue */
export interface STTPayload {
  taskId: string;
  userId: string;
  filePath: string;
  config: {
    language: string;
    sttModel: string;
  };
}

/** Summary 任務訊息，推送至 summary:queue */
export interface SummaryPayload {
  taskId: string;
  userId: string;
  transcript: string;
  config: {
    summaryPrompt: string;
  };
}

/** 任務記錄，對應 DB tasks 表結構 */
export interface TaskRecord {
  id: string;
  user_id: string;
  file_path: string;
  status: TaskStatus;
  error_message?: string;
  created_at: Date;
  updated_at: Date;
}
