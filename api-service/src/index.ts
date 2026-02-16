import Fastify from 'fastify';
import cors from '@fastify/cors';
import multipart from '@fastify/multipart';
import path from 'path';
import { fileURLToPath } from 'url';
import dotenv from 'dotenv';
import taskRoutes from './routes/tasks.js';
import { connect as connectQueue } from './lib/queue.js';
import { db } from './lib/db.js';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

dotenv.config({ path: path.join(__dirname, '../../.env') });

const fastify = Fastify({
  logger: true,
  bodyLimit: 1024 * 1024 * 1024, // 1GB，支援大檔案串流上傳
});

fastify.register(cors);
fastify.register(multipart, {
  limits: {
    fileSize: 1024 * 1024 * 1024 // 1GB
  }
});

fastify.register(taskRoutes);

/** 健康檢查端點，驗證 PostgreSQL 可達 */
fastify.get('/health', async () => {
  await db.query('SELECT 1');
  return { status: 'ok' };
});

/**
 * 啟動流程：依序驗證 DB → RabbitMQ → 開始監聽。
 * 任一依賴不可達時直接終止進程，由 Docker restart 策略自動重啟。
 */
const start = async () => {
  try {
    await db.query('SELECT 1');
    console.log('Database connected');

    await connectQueue();
    const port = process.env.PORT ? parseInt(process.env.PORT) : 3000;
    await fastify.listen({ port, host: '0.0.0.0' });
    console.log(`API Service listening on port ${port}`);
  } catch (err) {
    fastify.log.error(err);
    process.exit(1);
  }
};

start();
