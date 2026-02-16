import pg from 'pg';
const { Pool } = pg;

/** PostgreSQL 連線池，環境變數由 docker-compose 注入 */
const pool = new Pool({
  host: process.env.DB_HOST,
  user: process.env.DB_USER,
  password: process.env.DB_PASSWORD,
  database: process.env.DB_NAME,
  port: parseInt(process.env.DB_PORT || '5432'),
});

/** 封裝 query 與 pool，供各模組統一使用 */
export const db = {
  query: (text: string, params?: any[]) => pool.query(text, params),
  pool
};
