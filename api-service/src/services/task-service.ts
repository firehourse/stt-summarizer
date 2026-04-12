import { v4 as uuidv4 } from 'uuid';
import { db } from '../lib/db.js';
import redis from '../lib/redis.js';

/** 建立任務：DB INSERT + Redis HSET task owner */
export async function createTask(userId: string): Promise<string> {
  const taskId = uuidv4();
  await db.query(
    'INSERT INTO tasks (id, user_id, status) VALUES ($1, $2, $3)',
    [taskId, userId, 'pending']
  );
  await redis.set(`task:owner:${taskId}`, userId);
  await redis.hset(`task:${taskId}`, { status: 'pending', userId });
  return taskId;
}

/**
 * 查詢任務：先讀 Redis Hash 取得 live 狀態，再 merge DB row（transcript / summary / file_path）。
 * 任務不存在或不屬於該用戶時回傳 null。
 */
export async function getTask(taskId: string, userId: string): Promise<Record<string, unknown> | null> {
  const liveData = await redis.hgetall(`task:${taskId}`);

  const res = await db.query(
    `SELECT t.*, r.transcript, r.summary
     FROM tasks t
     LEFT JOIN task_results r ON t.id = r.task_id
     WHERE t.id = $1 AND t.user_id = $2`,
    [taskId, userId]
  );
  if (res.rows.length === 0) return null;

  const row = res.rows[0];
  const status = liveData?.status || row.status;
  const progress = liveData?.progress ? parseInt(liveData.progress, 10) : (row.progress ?? 0);
  return { ...row, status, progress };
}

/** 列出用戶所有任務（支援分頁） */
export async function listTasks(userId: string, limit: number, offset: number): Promise<unknown[]> {
  const res = await db.query(
    'SELECT id, status, created_at FROM tasks WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3',
    [userId, limit, offset]
  );
  return res.rows;
}

/**
 * 取消任務：Atomic DB UPDATE（status NOT IN terminal states）+ Redis cancel signal。
 * 回傳 false 代表任務不存在或已是終態。
 */
export async function cancelTask(taskId: string, userId: string): Promise<boolean> {
  const result = await db.query(
    "UPDATE tasks SET status = 'cancelled', updated_at = NOW() WHERE id = $1 AND user_id = $2 AND status NOT IN ('completed', 'failed', 'cancelled')",
    [taskId, userId]
  );
  if (result.rowCount === 0) return false;
  await redis.publish('cancel_channel', JSON.stringify({ taskId }));
  return true;
}
