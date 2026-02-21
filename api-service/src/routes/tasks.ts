// routes/tasks.ts — 任務 CRUD 路由，所有請求經由 Gateway 代理進入
import { FastifyInstance, FastifyPluginOptions, FastifyRequest, FastifyReply } from 'fastify';
import fs from 'fs';
import path from 'path';
import { v4 as uuidv4 } from 'uuid';
import { pipeline } from 'stream/promises';
import { Readable } from 'stream';
import { db } from '../lib/db.js';
import redis from '../lib/redis.js';
import { TaskMessage } from '../types/index.js';
import { fileTypeFromBuffer } from 'file-type';

/** 上傳根路徑，與 Worker 共享的 volume 掛載點 */
const UPLOAD_BASE = '/app/uploads';

/**
 * 任務路由插件，包含完整的任務生命週期操作。
 * Gateway 已從 Cookie 提取 userId 並注入 X-User-Id header，此處直接信任該 header。
 */
export default async function taskRoutes(fastify: FastifyInstance, options: FastifyPluginOptions) {
  /** 前置攔截：從 X-User-Id header 提取 userId，缺失時返回 401 */
  fastify.addHook('preHandler', async (request, reply) => {
    const userId = request.headers['x-user-id'] as string;
    if (!userId) {
      return reply.code(401).send({ error: 'Missing X-User-Id header' });
    }
    (request as any).userId = userId;
  });

  /**
   * POST /tasks — 預註冊任務。
   * 在 DB 建立 pending 紀錄並返回 taskId，同時寫入 Redis 供 Gateway SSE 驗證 ownership。
   */
  fastify.post('/tasks', async (request: FastifyRequest, reply: FastifyReply) => {
    const taskId = uuidv4();
    const userId = (request as any).userId;

    try {
      await db.query(
        'INSERT INTO tasks (id, user_id, status) VALUES ($1, $2, $3)',
        [taskId, userId, 'pending']
      );

      await redis.set(`task:owner:${taskId}`, userId);

      return { taskId, status: 'pending' };
    } catch (err) {
      fastify.log.error(err);
      return reply.code(500).send({ error: 'Failed to create task' });
    }
  });

  /**
   * PUT /tasks/:id/upload — 串流上傳音檔。
   * 使用 stream.pipeline 直接寫入磁碟（不經過記憶體），完成後發布 STT Task 至 RabbitMQ。
   * 路徑隔離格式：/app/uploads/{userId}/{taskId}/filename
   */
  fastify.put('/tasks/:id/upload', async (request: FastifyRequest<{ Params: { id: string } }>, reply: FastifyReply) => {
    const { id: taskId } = request.params;
    const userId = (request as any).userId;

    const data = await request.file();
    if (!data) return reply.code(400).send({ error: 'No file uploaded' });

    const userDir = path.join(UPLOAD_BASE, userId);
    const taskDir = path.join(userDir, taskId);

    if (!fs.existsSync(userDir)) fs.mkdirSync(userDir, { recursive: true });
    if (!fs.existsSync(taskDir)) fs.mkdirSync(taskDir, { recursive: true });

    const filePath = path.join(taskDir, data.filename);

    try {
      // 讀取前 4KB 用於偵測檔案類型 (Magic Number)
      // 注意：讀取 stream 會消耗它，所以後續需要重新封裝
      const buffer = await data.file.read(4100) || await new Promise<Buffer | null>((resolve) => {
        data.file.once('readable', () => {
          const chunk = data.file.read(4100);
          resolve(chunk);
        });
      });

      if (buffer) {
        const type = await fileTypeFromBuffer(buffer);
        const isDev = process.env.APP_ENV === 'dev';
        const isAudio = type?.mime.startsWith('audio/');
        const isVideo = type?.mime.startsWith('video/');

        // In dev mode, allow both audio and video. In prod, only audio.
        const isValid = isDev ? (isAudio || isVideo) : isAudio;

        if (!type || !isValid) {
          return reply.code(400).send({ 
            error: `Invalid file type: ${type?.mime || 'unknown'}. ${isDev ? 'Audio and Video' : 'Only audio'} files are allowed in this environment.` 
          });
        }
      }

      // 重新封裝 stream，把剛才讀出來的 buffer 補回去，確保檔案完整性
      const combinedStream = Readable.from((async function* () {
        if (buffer) yield buffer;
        for await (const chunk of data.file) {
          yield chunk;
        }
      })());

      await pipeline(combinedStream, fs.createWriteStream(filePath));

      const client = await db.pool.connect();
      try {
        await client.query('BEGIN');

        await client.query(
          'UPDATE tasks SET file_path = $1 WHERE id = $2 AND user_id = $3',
          [filePath, taskId, userId]
        );

        const message: TaskMessage = {
          type: 'STT',
          taskId,
          creatorId: userId,
          filePath,
          config: { language: 'zh-TW' }
        };

        await client.query(
          'INSERT INTO outbox_events (aggregate_id, event_type, payload) VALUES ($1, $2, $3)',
          [taskId, 'STT', JSON.stringify(message)]
        );

        await client.query('COMMIT');
      } catch (e) {
        await client.query('ROLLBACK');
        throw e;
      } finally {
        client.release();
      }

      return { status: 'upload_complete', taskId };
    } catch (err) {
      // 上傳失敗時清理已寫入的檔案，防止殘留
      if (fs.existsSync(filePath)) fs.unlinkSync(filePath);
      fastify.log.error(err);
      await db.query('UPDATE tasks SET status = $1, error_message = $2 WHERE id = $3', ['failed', 'Upload streaming failed', taskId]);
      return reply.code(500).send({ error: 'Upload streaming failed' });
    }
  });

  /**
   * GET /tasks/:id — 查詢單一任務詳情。
   * LEFT JOIN task_results 同時返回 transcript 與 summary（如已產生）。
   */
  fastify.get('/tasks/:id', async (request: FastifyRequest<{ Params: { id: string } }>, reply: FastifyReply) => {
    const { id } = request.params;
    const userId = (request as any).userId;

    const res = await db.query(
      `SELECT t.*, r.transcript, r.summary 
       FROM tasks t 
       LEFT JOIN task_results r ON t.id = r.task_id 
       WHERE t.id = $1 AND t.user_id = $2`,
      [id, userId]
    );

    if (res.rows.length === 0) return reply.code(404).send({ error: 'Task not found' });
    return res.rows[0];
  });

  /**
   * GET /tasks — 查詢用戶所有任務（支援分頁）。
   * @query limit - 每頁筆數，預設 10
   * @query offset - 偏移量，預設 0
   */
  fastify.get('/tasks', async (request: FastifyRequest<{ Querystring: { limit?: string, offset?: string } }>, reply: FastifyReply) => {
    const userId = (request as any).userId;
    const limit = parseInt(request.query.limit || '10');
    const offset = parseInt(request.query.offset || '0');

    const res = await db.query(
      'SELECT id, status, created_at FROM tasks WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3',
      [userId, limit, offset]
    );
    return res.rows;
  });

  /**
   * DELETE /tasks/:id — 取消任務。
   * 透過 Atomic Check 更新 DB 狀態，並發布 Cancel Signal 至 Redis，
   * Worker 訂閱 cancel_channel 後以 context.Cancel() 終止進行中的作業。
   */
  fastify.delete('/tasks/:id', async (request: FastifyRequest<{ Params: { id: string } }>, reply: FastifyReply) => {
    const { id } = request.params;
    const userId = (request as any).userId;

    const result = await db.query(
      "UPDATE tasks SET status = 'cancelled', updated_at = NOW() WHERE id = $1 AND user_id = $2 AND status IN ('pending', 'processing')",
      [id, userId]
    );

    if (result.rowCount === 0) {
      return reply.code(404).send({ error: 'Task not found or cannot be cancelled' });
    }

    await redis.publish('cancel_channel', JSON.stringify({ taskId: id }));

    return { status: 'cancelled' };
  });

  /**
   * POST /tasks/:id/summarize — 重新摘要。
   * 僅限 completed 狀態的任務，透過 Atomic Check (completed → processing) 後發布 SUMMARY Task。
   */
  fastify.post('/tasks/:id/summarize', async (request: FastifyRequest<{ Params: { id: string } }>, reply: FastifyReply) => {
    const { id: taskId } = request.params;
    const userId = (request as any).userId;

    const res = await db.query(
      'SELECT r.transcript FROM tasks t JOIN task_results r ON t.id = r.task_id WHERE t.id = $1 AND t.user_id = $2',
      [taskId, userId]
    );
    if (res.rows.length === 0) return reply.code(404).send({ error: 'Task result not found' });

    // Atomic: completed → processing + Outbox Event
    const client = await db.pool.connect();
    try {
      await client.query('BEGIN');

      const updateResult = await client.query(
        "UPDATE tasks SET status = 'processing', updated_at = NOW() WHERE id = $1 AND status = 'completed'",
        [taskId]
      );

      if (updateResult.rowCount === 0) {
        await client.query('ROLLBACK');
        return reply.code(409).send({ error: 'Task is not in completed state' });
      }

      const message: TaskMessage = {
        type: 'SUMMARY',
        taskId,
        creatorId: userId,
        transcript: res.rows[0].transcript,
        config: { language: 'zh-TW' }
      };

      await client.query(
        'INSERT INTO outbox_events (aggregate_id, event_type, payload) VALUES ($1, $2, $3)',
        [taskId, 'SUMMARY', JSON.stringify(message)]
      );

      await client.query('COMMIT');
      return { status: 'summary_requested' };
    } catch (e) {
      await client.query('ROLLBACK');
      fastify.log.error(e);
      return reply.code(500).send({ error: 'Failed to request summary' });
    } finally {
      client.release();
    }
  });
}
