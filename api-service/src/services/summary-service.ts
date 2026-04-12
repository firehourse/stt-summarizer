import { db } from '../lib/db.js';
import redis from '../lib/redis.js';
import { pushSummaryTask } from '../lib/redis-queue.js';
import { SummaryPayload, TaskStatus } from '../types/index.js';

/**
 * 觸發摘要：驗證任務處於 stt_completed → 取 DB transcript → Redis HSET summary_queued → LPUSH summary:queue。
 * 非 stt_completed 狀態回傳 409，任務不存在回傳 404。
 */
export async function triggerSummary(taskId: string, userId: string, prompt?: string): Promise<void> {
  // 以 Redis live 狀態為主（response 時間短），fallback DB
  const liveStatus = await redis.hget(`task:${taskId}`, 'status');
  const effectiveStatus = liveStatus || await fetchDbStatus(taskId, userId);

  if (effectiveStatus !== TaskStatus.SttCompleted) {
    const err = new Error('Task is not in stt_completed state');
    (err as any).statusCode = 409;
    throw err;
  }

  const res = await db.query(
    `SELECT r.transcript FROM tasks t
     JOIN task_results r ON t.id = r.task_id
     WHERE t.id = $1 AND t.user_id = $2`,
    [taskId, userId]
  );
  if (res.rows.length === 0) {
    const err = new Error('Task result not found');
    (err as any).statusCode = 404;
    throw err;
  }

  const payload: SummaryPayload = {
    taskId,
    userId,
    transcript: res.rows[0].transcript,
    config: { summaryPrompt: prompt ?? '' },
  };

  await redis.hset(`task:${taskId}`, 'status', TaskStatus.SummaryQueued);
  await pushSummaryTask(payload);
}

async function fetchDbStatus(taskId: string, userId: string): Promise<string | null> {
  const res = await db.query(
    'SELECT status FROM tasks WHERE id = $1 AND user_id = $2',
    [taskId, userId]
  );
  return res.rows[0]?.status ?? null;
}
