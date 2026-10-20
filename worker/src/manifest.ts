// manifest.ts —— 向 VPS 控制面取文件分片清单（part 偏移 / bot 槽位 / file_id）。
// 清单只有几百字节，走 VPS 不违背"字节不过服务端"的约束。

import { HttpError } from "./errors";
import type { Env } from "./telegram";

const MANIFEST_TTL_SECONDS = 120;

export interface ManifestPart {
  n: number;
  off: number;
  len: number;
  bot: number;
  fid: string;
}

export interface Manifest {
  v: number;
  fileId: string;
  name: string;
  size: number;
  contentType: string;
  etag: string;
  lastModified: string;
  partSize: number;
  encrypted: boolean;
  parts: ManifestPart[];
}

async function digestKey(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

// 返回 null 表示文件不存在（404）；其余失败一律抛 HttpError。
export async function loadManifest(env: Env, ticket: string): Promise<Manifest | null> {
  if (!env.MANIFEST_URL) throw new HttpError(500, "manifest_url_unset");

  const cache = caches.default;
  const cacheKey = new Request(`https://teldrive-gateway.invalid/manifest/${await digestKey(ticket)}`, {
    method: "GET",
  });
  const cached = await cache.match(cacheKey);
  if (cached) {
    return (await cached.json()) as Manifest;
  }

  const response = await fetch(env.MANIFEST_URL, {
    method: "GET",
    headers: { Authorization: `Bearer ${ticket}`, Accept: "application/json" },
  });

  if (response.status === 404) {
    await response.body?.cancel();
    return null;
  }
  if (response.status === 401 || response.status === 403) {
    await response.body?.cancel();
    throw new HttpError(502, "manifest_unauthorized");
  }
  if (!response.ok) {
    await response.body?.cancel();
    throw new HttpError(502, "manifest_unavailable");
  }

  const manifest = (await response.json()) as Manifest;
  await cache.put(
    cacheKey,
    new Response(JSON.stringify(manifest), {
      headers: { "Content-Type": "application/json", "Cache-Control": `max-age=${MANIFEST_TTL_SECONDS}` },
    }),
  );
  return manifest;
}
