// upload.ts —— 上传数据面：客户端 PUT 分块 → Telegram sendDocument → 回调 VPS 落库。
// 关键约束：
//   1. ticket 的 scope 必须为 "u"，且携带 p/b/z/l 四个上传字段；
//   2. 分块最大 16 MiB，整体缓冲于内存（绝不写盘），字节数必须与票据 z 一致；
//   3. sendDocument 得到的 file_id 只回调给 VPS，绝不出现在对客户端的响应中；
//   4. 只有 VPS 落库成功，才对客户端返回 200。

import { HttpError } from "./errors";
import { botToken, retryAfterSeconds, sleep, type Env } from "./telegram";
import { verifyTicket } from "./ticket";

const MAX_PART_BYTES = 16 * 1024 * 1024;

interface SendDocumentResult {
  message_id?: number;
  document?: {
    file_id?: string;
    file_size?: number;
  };
}

export async function handleUpload(request: Request, env: Env, ticket: string): Promise<Response> {
  if (!ticket) throw new HttpError(400, "ticket_missing");
  const payload = await verifyTicket(ticket, env.GATEWAY_SECRET, Math.floor(Date.now() / 1000));
  if (payload.s !== "u") throw new HttpError(400, "ticket_kind_mismatch");
  if (
    typeof payload.p !== "number" ||
    payload.p <= 0 ||
    typeof payload.b !== "number" ||
    payload.b <= 0 ||
    typeof payload.z !== "number" ||
    payload.z <= 0 ||
    typeof payload.l !== "string" ||
    payload.l === ""
  ) {
    throw new HttpError(400, "ticket_payload_invalid");
  }
  if (payload.z > MAX_PART_BYTES) throw new HttpError(413, "part_too_large");

  const token = botToken(env, payload.b);
  if (!token) throw new HttpError(500, "bot_token_unset");

  // 分块 ≤16 MiB，整体缓冲可接受；VPS 侧以票据 z 复核 Telegram 回报的字节数。
  const body = await request.arrayBuffer();
  if (body.byteLength !== payload.z) throw new HttpError(400, "part_size_mismatch");

  const result = await sendDocument(env, token, body);
  const fileID = result.document?.file_id ?? "";
  const messageID = result.message_id ?? 0;
  const storedSize = result.document?.file_size ?? body.byteLength;
  if (!fileID || messageID <= 0) throw new HttpError(502, "telegram_api_not_ok");

  // 回调 VPS 落库；只有落库成功才对客户端返回成功，保证客户端重试语义安全。
  await notifyUploadComplete(env, ticket, fileID, messageID, storedSize);

  return new Response(JSON.stringify({ ok: true }), {
    status: 200,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

// multipart sendDocument，429 退避复用下载路径的解析逻辑。
async function sendDocument(env: Env, token: string, body: ArrayBuffer, attempts = 3): Promise<SendDocumentResult> {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    const form = new FormData();
    form.set("chat_id", env.CHANNEL_ID);
    form.set("document", new Blob([body], { type: "application/octet-stream" }), randomFilename());
    form.set("disable_content_type_detection", "true");
    const response = await fetch(`https://api.telegram.org/bot${token}/sendDocument`, {
      method: "POST",
      body: form,
    });
    if (response.status === 429) {
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
    const parsed = (await response.json()) as { ok?: boolean; result?: SendDocumentResult };
    if (!parsed.ok || !parsed.result) {
      throw new HttpError(502, "telegram_api_not_ok");
    }
    return parsed.result;
  }
  throw new HttpError(429, "telegram_rate_limited");
}

// 回调 VPS 内部端点：Bearer ticket 自证，载荷只含 fid/mid/size。
async function notifyUploadComplete(
  env: Env,
  ticket: string,
  fileID: string,
  messageID: number,
  size: number,
): Promise<void> {
  const response = await fetch(uploadCompleteURL(env), {
    method: "POST",
    headers: {
      Authorization: `Bearer ${ticket}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ fid: fileID, mid: messageID, size }),
  });
  await response.body?.cancel();
  if (response.status === 409) throw new HttpError(409, "lease_lost");
  if (response.status === 410) throw new HttpError(410, "ticket_expired");
  if (!response.ok) throw new HttpError(502, "upload_complete_failed");
}

function uploadCompleteURL(env: Env): string {
  const explicit = (env.UPLOAD_COMPLETE_URL ?? "").trim();
  if (explicit) return explicit;
  const base = new URL(env.MANIFEST_URL);
  return `${base.origin}/internal/gateway/upload-complete`;
}

function randomFilename(): string {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  let hex = "";
  for (const byte of bytes) {
    hex += byte.toString(16).padStart(2, "0");
  }
  return `${hex}.bin`;
}
