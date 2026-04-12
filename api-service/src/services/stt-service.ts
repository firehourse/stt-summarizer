import fs from 'fs';
import path from 'path';
import { pipeline } from 'stream/promises';
import { Readable } from 'stream';
import { fileTypeFromBuffer } from 'file-type';
import { db } from '../lib/db.js';
import redis from '../lib/redis.js';
import { pushSTTTask } from '../lib/redis-queue.js';
import { STTPayload, TaskStatus } from '../types/index.js';

const UPLOAD_BASE = '/app/uploads';

/**
 * 音檔上傳處理：MIME 驗證 → 寫檔 → DB UPDATE file_path → Redis HSET stt_queued → LPUSH stt:queue。
 * 失敗時自動清理已寫入的檔案並更新 DB status=failed，再 rethrow。
 */
export async function handleUpload(taskId: string, userId: string, fileData: {
  filename: string;
  file: any;
}): Promise<void> {
  const userDir = path.join(UPLOAD_BASE, userId);
  const taskDir = path.join(userDir, taskId);

  if (!fs.existsSync(userDir)) fs.mkdirSync(userDir, { recursive: true });
  if (!fs.existsSync(taskDir)) fs.mkdirSync(taskDir, { recursive: true });

  const filePath = path.join(taskDir, fileData.filename);

  try {
    // 讀取前 4KB 用於 MIME 偵測（magic bytes），不消耗整個 stream
    const buffer: Buffer | null = fileData.file.read(4100) ?? await new Promise<Buffer | null>((resolve) => {
      fileData.file.once('readable', () => resolve(fileData.file.read(4100)));
    });

    if (buffer) {
      const type = await fileTypeFromBuffer(buffer);
      const isDev = process.env.APP_ENV === 'dev';
      const isAudio = type?.mime.startsWith('audio/');
      const isVideo = type?.mime.startsWith('video/');
      const isValid = isDev ? (isAudio || isVideo) : isAudio;

      if (!type || !isValid) {
        const err = new Error(
          `Invalid file type: ${type?.mime ?? 'unknown'}. ${isDev ? 'Audio and Video' : 'Only audio'} files are allowed.`
        );
        (err as any).statusCode = 400;
        throw err;
      }
    }

    // 把剛才讀出的 buffer 補回 stream，確保檔案完整性
    const combinedStream = Readable.from((async function* () {
      if (buffer) yield buffer;
      for await (const chunk of fileData.file) yield chunk;
    })());

    await pipeline(combinedStream, fs.createWriteStream(filePath));

    await db.query(
      'UPDATE tasks SET file_path = $1 WHERE id = $2 AND user_id = $3',
      [filePath, taskId, userId]
    );

    const payload: STTPayload = {
      taskId,
      userId,
      filePath,
      config: {
        language: process.env.STT_LANGUAGE ?? 'zh-TW',
        sttModel: process.env.AI_STT_MODEL ?? '',
      },
    };

    await redis.hset(`task:${taskId}`, { status: TaskStatus.SttQueued, filePath });
    await pushSTTTask(payload);
  } catch (err) {
    // 清理殘留檔案
    if (fs.existsSync(filePath)) fs.unlinkSync(filePath);
    // 若非 MIME 錯誤（400），更新 DB status
    if ((err as any).statusCode !== 400) {
      await db.query(
        'UPDATE tasks SET status = $1, error_message = $2 WHERE id = $3',
        [TaskStatus.Failed, 'Upload failed', taskId]
      );
    }
    throw err;
  }
}
