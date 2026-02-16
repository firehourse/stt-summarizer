import amqp, { ChannelModel, Channel } from 'amqplib';
import { TaskMessage } from '../types/index.js';

const MAX_RETRIES = 10;
const BASE_DELAY_MS = 1000;
const MAX_DELAY_MS = 30 * 1000;

let channelModel: ChannelModel | null = null;
let channel: Channel | null = null;
let isReconnecting = false;

/** 透過 docker bridge network 組裝 RabbitMQ 連線 URL */
function getUrl(): string {
  return `amqp://${process.env.RABBITMQ_USER}:${process.env.RABBITMQ_PASS}@${process.env.RABBITMQ_HOST}`;
}

/** 計算指數退避延遲，含 jitter 防止多服務同時重連踩踏 */
function getBackoffDelay(attempt: number): number {
  const exponential = Math.min(BASE_DELAY_MS * Math.pow(2, attempt), MAX_DELAY_MS);
  const jitter = Math.random() * exponential;
  return Math.floor(exponential + jitter);
}

/** Promise-based delay */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** 註冊 connection/channel 錯誤與斷線事件，觸發自動重連 */
function setupEventListeners(): void {
  if (!channelModel || !channel) return;

  channelModel.on('error', (err) => {
    console.error('[Queue] Connection error:', err.message);
  });

  channelModel.on('close', () => {
    console.warn('[Queue] Connection closed, reconnecting...');
    channelModel = null;
    channel = null;
    handleReconnect();
  });

  channel.on('error', (err) => {
    console.error('[Queue] Channel error:', err.message);
  });

  channel.on('close', () => {
    console.warn('[Queue] Channel closed');
    channel = null;
  });
}

/** 自動重連入口，透過 isReconnecting flag 避免重複觸發 */
async function handleReconnect(): Promise<void> {
  if (isReconnecting) return;
  isReconnecting = true;

  try {
    await connect();
  } catch (err) {
    console.error('[Queue] Reconnect exhausted all retries');
  } finally {
    isReconnecting = false;
  }
}

/** 建立 RabbitMQ 連線，失敗時以 exponential backoff 重試最多 MAX_RETRIES 次 */
export async function connect(): Promise<void> {
  const url = getUrl();

  for (let attempt = 0; attempt < MAX_RETRIES; attempt++) {
    try {
      channelModel = await amqp.connect(url);
      channel = await channelModel.createChannel();
      await channel.assertQueue('tasks', { durable: true });

      setupEventListeners();
      console.log(`[Queue] Connected to RabbitMQ${attempt > 0 ? ` (attempt ${attempt + 1})` : ''}`);
      return;
    } catch (err) {
      const delay = getBackoffDelay(attempt);
      const errorMessage = err instanceof Error ? err.message : String(err);
      console.error(`[Queue] Connect failed (${attempt + 1}/${MAX_RETRIES}): ${errorMessage}, retrying in ${delay / 1000}s...`);

      // 清理半開連線
      if (channelModel) {
        try { await channelModel.close(); } catch { /* noop */ }
        channelModel = null;
        channel = null;
      }

      if (attempt === MAX_RETRIES - 1) {
        throw new Error(`[Queue] Failed after ${MAX_RETRIES} attempts: ${errorMessage}`);
      }

      await sleep(delay);
    }
  }
}

/** 發送任務訊息至 RabbitMQ，channel 不可用時自動重連 */
export async function sendToQueue(data: TaskMessage): Promise<void> {
  if (!channel) {
    console.warn('[Queue] Channel unavailable, reconnecting...');
    await connect();
  }

  if (!channel) {
    throw new Error('[Queue] Unable to establish channel');
  }

  channel.sendToQueue('tasks', Buffer.from(JSON.stringify(data)), { persistent: true });
}
