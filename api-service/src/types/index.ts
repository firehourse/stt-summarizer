/** 任務配置，隨 TaskMessage 一起傳入 Queue */
export interface TaskConfig {
  /** 轉錄語言，例如 'zh-TW' */
  language: string;
  /** STT 模型名稱，例如 'whisper-large' */
  sttModel: string;
  /** 自訂摘要 Prompt，空字串使用預設 */
  summaryPrompt: string;
}

/**
 * RabbitMQ 佇列中的任務訊息格式。
 * STT 任務攜帶 filePath，SUMMARY 任務攜帶 transcript。
 */
export interface TaskMessage {
  taskId: string;
  creatorId: string;
  /** STT 階段使用：音檔在 shared volume 上的路徑 */
  filePath?: string;
  /** SUMMARY 階段使用：STT 完成後的轉錄文字 */
  transcript?: string;
  /** 任務類型，決定 Worker 執行 STT 或 LLM 摘要 */
  type: 'STT' | 'SUMMARY';
  config: TaskConfig;
}

/** 任務狀態，對應 DB tasks 表結構 */
export interface TaskStatus {
  id: string;
  creator_id: string;
  file_path: string;
  status: 'pending' | 'processing' | 'completed' | 'failed' | 'cancelled';
  progress: number;
  result_text?: string;
  summary?: string;
  created_at: Date;
  updated_at: Date;
}
