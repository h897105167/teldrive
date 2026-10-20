// telegram.ts —— 与 Telegram Bot API 的字节交换。
// 关键约束：
//   1. file_id 按 bot 隔离，必须按 part 的 bot 槽位取对应 token；
//   2. file_path 约 1 小时失效，缓存 50 分钟；
//   3. 下载文件端点支持 Range（206）；
//   4. token 与 file_path 绝不可出现在日志、响应体或错误信息中。

import { HttpError } from "./errors";
import type { SubrequestBudget } from "./budget";

export interface Env {
  CHANNEL_ID: string;
  MANIFEST_URL: string;
  GATEWAY_SECRET: string;
  /** 上传完成回调端点；缺省从 MANIFEST_URL 推导同源 /internal/gateway/upload-complete。 */
  UPLOAD_COMPLETE_URL?: string;
  /**
   * BOT_TOKEN_1 .. BOT_TOKEN_N —— 每个网关 bot 槽位对应一个 secret。
   * 槽位号取自清单 part.bot（由服务端 gateway.bot-slots 顺序决定），
   * 数量不固定，因此这里用索引签名而不是写死的 BOT_TOKEN_1 / BOT_TOKEN_2。
   */
  [key: string]: string | undefined;
}

const FILE_PATH_TTL_SECONDS = 3000; // 50 分钟

// Bot API 的 getFile 是纯 JSON 请求，很快；给它一个短超时，避免卡住整个下载。
const BOT_API_TIMEOUT_MS = 15_000;
// 单个 part 的字节区间最大可能 16 MiB；给它一个有界预算，卡住的连接必须失败而不是永久挂起。
const RANGE_TIMEOUT_MS = 90_000;

export function botToken(env: Env, slot: number): string {
  const token = env[`BOT_TOKEN_${slot}`];
  if (!token) throw new HttpError(500, "bot_slot_unconfigured");
  return token;
}

function filePathCacheKey(slot: number, fileID: string): Request {
  return new Request(`https://teldrive-gateway.invalid/getfile/${slot}/${fileID}`, { method: "GET" });
}

export function sleep(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

// 解析退避时长：优先 Retry-After 响应头，其次 Telegram 返回体里的 parameters.retry_after。
export async function retryAfterSeconds(response: Response, attempt: number): Promise<number> {
  const header = response.headers.get("Retry-After");
  const fromHeader = header ? Number.parseInt(header, 10) : Number.NaN;
  if (Number.isFinite(fromHeader) && fromHeader > 0) {
    return Math.min(fromHeader, 10);
  }
  try {
    const text = await response.text();
    const match = /"retry_after"\s*:\s*(\d+)/.exec(text);
    if (match?.[1]) {
      return Math.min(Number.parseInt(match[1], 10), 10);
    }
  } catch {
    // 忽略解析失败，退化为指数退避
  }
  return Math.min(2 ** attempt, 5);
}

async function callBotAPI<T>(url: string, budget: SubrequestBudget, attempts = 3): Promise<T> {
  let rateLimited = false;
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    budget.take("botapi");
    const response = await fetch(url, { signal: AbortSignal.timeout(BOT_API_TIMEOUT_MS) });
    if (response.status === 429) {
      rateLimited = true;
      const delay = await retryAfterSeconds(response, attempt);
      if (attempt < attempts - 1) {
        await sleep(delay * 1000);
        continue;
      }
      break;
    }
    if (!response.ok) {
      await response.body?.cancel();
      throw new HttpError(502, `telegram_api_${response.status}`);
    }
    const body = (await response.json()) as { ok?: boolean; result?: T };
    if (!body.ok || body.result === undefined) {
      throw new HttpError(502, "telegram_api_not_ok");
    }
    return body.result;
  }
  throw new HttpError(429, rateLimited ? "telegram_rate_limited" : "telegram_api_error");
}

// getFile 换 file_path，结果进 Cache API。
export async function telegramFilePath(
  env: Env,
  slot: number,
  fileID: string,
  budget: SubrequestBudget,
): Promise<string> {
  const token = botToken(env, slot);
  if (!token) throw new HttpError(500, "bot_token_unset");

  const cache = caches.default;
  const cacheKey = filePathCacheKey(slot, fileID);
  const cached = await cache.match(cacheKey);
  if (cached) {
    const cachedBody = (await cached.json()) as { path?: string };
    if (cachedBody.path) return cachedBody.path;
  }

  const result = await callBotAPI<{ file_path?: string }>(
    `https://api.telegram.org/bot${token}/getFile?file_id=${encodeURIComponent(fileID)}`,
    budget,
  );
  if (!result.file_path) throw new HttpError(404, "telegram_file_missing");

  await cache.put(
    cacheKey,
    new Response(JSON.stringify({ path: result.file_path }), {
      headers: { "Content-Type": "application/json", "Cache-Control": `max-age=${FILE_PATH_TTL_SECONDS}` },
    }),
  );
  return result.file_path;
}

export async function invalidateFilePath(slot: number, fileID: string): Promise<void> {
  try {
    await caches.default.delete(filePathCacheKey(slot, fileID));
  } catch {
    // 缓存失效失败不影响主流程
  }
}

// 取指定 part 的字节区间（闭区间，基于该 part 自身偏移）。返回的响应体必须由调用方消费或取消。
//
// externalSignal 由调用方提供，用来给"响应体已经开始流动之后"设上限。fetch 自带的
// AbortSignal.timeout 只覆盖到响应头返回为止；字节交给原生 pipeTo 之后，上游卡住不会
// 触发任何超时，整个响应就会一直挂到客户端自己放弃。调用方因此按 part 传入一个
// 有界的 signal，卡住时让它把 body 流打断。
export async function fetchTelegramRange(
  env: Env,
  slot: number,
  fileID: string,
  low: number,
  high: number,
  budget: SubrequestBudget,
  attempts = 3,
  externalSignal?: AbortSignal,
  rangeTimeoutMs = RANGE_TIMEOUT_MS,
): Promise<Response> {
  const token = botToken(env, slot);
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    const filePath = await telegramFilePath(env, slot, fileID, budget);
    // rangeTimeoutMs === 0 表示调用方自己掌握预算（预取时握手提前发起，等字节的时间
    // 取决于客户端消费速度，不能在这里按固定的 90 s 砍断）。传 0 而不是 Infinity：
    // AbortSignal.timeout(0) 会立即中止，所以必须走"不设内部 deadline"的分支。
    const deadline = rangeTimeoutMs > 0 ? AbortSignal.timeout(rangeTimeoutMs) : undefined;
    const signal = externalSignal && deadline
      ? AbortSignal.any([externalSignal, deadline])
      : (externalSignal ?? deadline);
    // 每次尝试都要真花掉一个 subrequest，超预算时在这里就失败，而不是让运行时掐断响应。
    budget.take("range");
    const response = await fetch(`https://api.telegram.org/file/bot${token}/${filePath}`, {
      method: "GET",
      headers: { Range: `bytes=${low}-${high}` },
      // 连接或读取卡住时必须失败：调用方会重试，而不是让整个响应永久挂起。
      signal,
    });

    if (response.status === 206 || response.status === 200) {
      return response;
    }
    if (response.status === 429) {
      const delay = await retryAfterSeconds(response, attempt);
      if (attempt < attempts - 1) {
        await sleep(delay * 1000);
        continue;
      }
      throw new HttpError(429, "telegram_rate_limited");
    }
    await response.body?.cancel();
    if (response.status === 404 || response.status === 400) {
      // file_path 可能已过期，丢弃缓存后重试一次拿新的 file_path。
      await invalidateFilePath(slot, fileID);
      if (attempt < attempts - 1) continue;
      throw new HttpError(502, "telegram_file_unavailable");
    }
    throw new HttpError(502, `telegram_file_${response.status}`);
  }
  throw new HttpError(429, "telegram_rate_limited");
}
