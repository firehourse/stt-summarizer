import redis from './redis.js';
import { STTPayload, SummaryPayload } from '../types/index.js';

/** 將 STT 任務推送至 stt:queue（Redis LIST LPUSH） */
export async function pushSTTTask(payload: STTPayload): Promise<void> {
  await redis.lpush('stt:queue', JSON.stringify(payload));
}

/** 將 Summary 任務推送至 summary:queue（Redis LIST LPUSH） */
export async function pushSummaryTask(payload: SummaryPayload): Promise<void> {
  await redis.lpush('summary:queue', JSON.stringify(payload));
}
