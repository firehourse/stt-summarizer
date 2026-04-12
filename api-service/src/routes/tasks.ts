// routes/tasks.ts — 任務路由薄層，解析 HTTP 邊界後委派至 service 層
import { FastifyInstance, FastifyPluginOptions, FastifyRequest, FastifyReply } from 'fastify';
import * as taskService from '../services/task-service.js';
import * as sttService from '../services/stt-service.js';
import * as summaryService from '../services/summary-service.js';

/**
 * 任務路由插件。
 * Gateway 已從 Cookie 提取 userId 並注入 X-User-Id header，此處直接信任該 header。
 */
export default async function taskRoutes(fastify: FastifyInstance, options: FastifyPluginOptions) {
  /** 前置攔截：從 X-User-Id header 提取 userId，缺失時返回 401 */
  fastify.addHook('preHandler', async (request: FastifyRequest, reply: FastifyReply) => {
    const userId = request.headers['x-user-id'] as string;
    if (!userId) return reply.code(401).send({ error: 'Missing X-User-Id header' });
    (request as any).userId = userId;
  });

  /** POST /tasks — 預註冊任務，回傳 taskId */
  fastify.post('/tasks', async (request: FastifyRequest, reply: FastifyReply) => {
    try {
      const taskId = await taskService.createTask((request as any).userId);
      return { taskId, status: 'pending' };
    } catch (err) {
      fastify.log.error(err);
      return reply.code(500).send({ error: 'Failed to create task' });
    }
  });

  /**
   * PUT /tasks/:id/upload — 串流上傳音檔。
   * MIME 驗證後存檔，推送 STT 任務至 Redis queue。
   */
  fastify.put('/tasks/:id/upload', async (
    request: FastifyRequest<{ Params: { id: string } }>,
    reply: FastifyReply
  ) => {
    const { id: taskId } = request.params;
    const userId = (request as any).userId;

    const data = await request.file();
    if (!data) return reply.code(400).send({ error: 'No file uploaded' });

    try {
      await sttService.handleUpload(taskId, userId, data);
      return { status: 'upload_complete', taskId };
    } catch (err: any) {
      fastify.log.error(err);
      if (err.statusCode === 400) return reply.code(400).send({ error: err.message });
      return reply.code(500).send({ error: 'Upload streaming failed' });
    }
  });

  /**
   * GET /tasks/:id — 查詢單一任務詳情。
   * 合併 Redis live 狀態與 DB 持久欄位（transcript / summary / file_path）。
   */
  fastify.get('/tasks/:id', async (
    request: FastifyRequest<{ Params: { id: string } }>,
    reply: FastifyReply
  ) => {
    const task = await taskService.getTask(request.params.id, (request as any).userId);
    if (!task) return reply.code(404).send({ error: 'Task not found' });
    return task;
  });

  /** GET /tasks — 查詢用戶所有任務（支援分頁） */
  fastify.get('/tasks', async (
    request: FastifyRequest<{ Querystring: { limit?: string; offset?: string } }>,
    reply: FastifyReply
  ) => {
    const limit = parseInt(request.query.limit ?? '10', 10);
    const offset = parseInt(request.query.offset ?? '0', 10);
    return taskService.listTasks((request as any).userId, limit, offset);
  });

  /**
   * DELETE /tasks/:id — 取消任務。
   * Atomic Check 更新 DB，並發布 Cancel Signal 至 Redis。
   */
  fastify.delete('/tasks/:id', async (
    request: FastifyRequest<{ Params: { id: string } }>,
    reply: FastifyReply
  ) => {
    const cancelled = await taskService.cancelTask(request.params.id, (request as any).userId);
    if (!cancelled) return reply.code(404).send({ error: 'Task not found or cannot be cancelled' });
    return { status: 'cancelled' };
  });

  /**
   * POST /tasks/:id/summarize — 使用者手動觸發摘要。
   * 僅限 stt_completed 狀態，推送 Summary 任務至 Redis queue。
   */
  fastify.post('/tasks/:id/summarize', async (
    request: FastifyRequest<{ Params: { id: string } }>,
    reply: FastifyReply
  ) => {
    const { id: taskId } = request.params;
    const body = (request.body as any) ?? {};

    try {
      await summaryService.triggerSummary(taskId, (request as any).userId, body.prompt);
      return { status: 'summary_requested' };
    } catch (err: any) {
      fastify.log.error(err);
      if (err.statusCode === 404) return reply.code(404).send({ error: err.message });
      if (err.statusCode === 409) return reply.code(409).send({ error: err.message });
      return reply.code(500).send({ error: 'Failed to request summary' });
    }
  });
}
