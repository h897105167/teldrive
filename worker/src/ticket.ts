// ticket.ts —— 数据面票据的解析与 HMAC-SHA256 校验，必须与服务端 internal/gateway 完全一致。
//
// 格式： base64url(payloadJson) "." base64url(HMAC_SHA256(secret, base64url(payloadJson)))
// payloadJson 为紧凑 JSON： {
//   v: 1, s: "d" | "u", f: <fileUUID>, r?: "start-end", a?: 1, u: <uid>, n: <nonce>, e: <expUnix>,
//   p?: <partNo>, b?: <botSlot>, z?: <partBytes>, l?: <leaseToken>   // 仅 s="u" 的上传票据携带
// }

import { HttpError } from "./errors";

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export interface TicketPayload {
  v: number;
  s: "d" | "u";
  f: string;
  r?: string;
  a?: number;
  u: number;
  n: string;
  e: number;
  /** 上传分块序号（1 基），仅 s="u" */
  p?: number;
  /** bot token 槽位（1 基），仅 s="u" */
  b?: number;
  /** 分块明文字节数，仅 s="u" */
  z?: number;
  /** 服务端租约令牌，回调落库时原样带回，仅 s="u" */
  l?: string;
}

function base64URLToBytes(value: string): Uint8Array {
  const normalized = value.replace(/-/g, "+").replace(/_/g, "/");
  const remainder = normalized.length % 4;
  const padded = remainder === 0 ? normalized : normalized.padEnd(normalized.length + (4 - remainder), "=");
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}

function bytesToBase64URL(bytes: Uint8Array): string {
  let binary = "";
  for (let index = 0; index < bytes.length; index += 1) {
    binary += String.fromCharCode(bytes[index] as number);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// 对 payload 段（字符串原文）计算 HMAC-SHA256 并做 base64url 编码。
export async function signSegment(secret: string, segment: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    "raw",
    encoder.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign("HMAC", key, encoder.encode(segment));
  return bytesToBase64URL(new Uint8Array(signature));
}

function timingSafeEqual(left: string, right: string): boolean {
  if (left.length !== right.length) return false;
  let diff = 0;
  for (let index = 0; index < left.length; index += 1) {
    diff |= left.charCodeAt(index) ^ right.charCodeAt(index);
  }
  return diff === 0;
}

// 给非票据的派生签名复用同一套比较：续跳头用它验证签名，调用方不必自己写比较。
export function verifySegment(expected: string, provided: string): boolean {
  return timingSafeEqual(expected, provided);
}

export async function verifyTicket(token: string, secret: string, nowUnix: number): Promise<TicketPayload> {  if (!secret) throw new HttpError(500, "gateway_secret_unset");

  const separator = token.indexOf(".");
  if (separator <= 0 || separator === token.length - 1) {
    throw new HttpError(400, "ticket_malformed");
  }
  const segment = token.slice(0, separator);
  const signature = token.slice(separator + 1);

  const expected = await signSegment(secret, segment);
  if (!timingSafeEqual(expected, signature)) {
    throw new HttpError(403, "ticket_signature_invalid");
  }

  let payload: TicketPayload;
  try {
    payload = JSON.parse(decoder.decode(base64URLToBytes(segment))) as TicketPayload;
  } catch {
    throw new HttpError(400, "ticket_payload_invalid");
  }

  if (
    !payload ||
    payload.v !== 1 ||
    (payload.s !== "d" && payload.s !== "u") ||
    typeof payload.f !== "string" ||
    typeof payload.e !== "number"
  ) {
    throw new HttpError(400, "ticket_payload_invalid");
  }
  if (payload.e <= nowUnix) {
    throw new HttpError(410, "ticket_expired");
  }
  return payload;
}
