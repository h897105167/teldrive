// index.ts —— 数据面入口：验证 ticket → 取 manifest → 跨 part 流式代理 Telegram 字节。
//
// 路由：
//   OPTIONS        *               CORS 预检
//   GET   /d/:ticket                下载（支持 Range / HEAD / If-None-Match）
//   PUT   /u/:ticket                上传分块（转发 Telegram，回调 VPS 落库）
//   GET   /healthz                  存活探针
//
// 下载路径的设计约束（实测得出，改动前务必读）：
//   * 字节绝不能经过 JS。逐块 enqueue / 自定义 ReadableStream / 逐块 write 回调，
//     实测 49 MB 要 400–2000 ms CPU，在标准用量模型下会随机触发
//     「1102 Worker exceeded CPU time limit」。而把上游 body 直接 pipeTo 进
//     FixedLengthStream 只要 3–9 ms——运行时自己搬运，JS 一次都不碰。
//   * FixedLengthStream 会保留 Content-Length（Cloudflare 不会改写成 chunked），
//     所以上游中途断流时客户端能识别为截断，而不是收到一个「合法」的短 200。
//   * 因此不做逐字节的计数/续传/缓冲：那正是把 CPU 打爆的写法。重试只发生在
//     fetch 层（fetchTelegramRange 内部处理 429 与失效 file_path），此时还没写出任何字节。
//   * 但重试不能覆盖"已经开始流动后停住"的情况：原生 pipeTo 期间没有任何 JS 回调，
//     上游停住就不会有任何超时触发，整个响应会挂到客户端自己放弃（实测 20 MiB 只发出
//     290 KB 后无限期停住）。因此每个 part 配一个看门狗 signal，超时即打断 body 流。

// 单个 part（≤16 MiB）的看门狗上限。它只用来把"上游彻底不动"变成客户端可见的错误，
// 不能用平均速度来理解：pipeTo 有背压，读上游的快慢由客户端下载速度决定，慢链路读满
// 一个 16 MiB part 本来就要几分钟，所以留足余量（16 MiB / 120 s ≈ 1.4 Mbps 才算慢）。
// 误杀的代价很小：客户端按 Range 续传即可（206/后缀区间/跨 part 均已验证）。
const PART_STREAM_TIMEOUT_MS = 300_000;

// 预取深度：只提前一个 part。每个 part 都要向 api.telegram.org 新建一次连接并等响应头，
// 实测这段固定要 ~0.55 s；串行时它整段压在每一个 part 的边界上（实测 16 part 的文件
// 在每处边界都停约 0.55 s，累计多花 ~9 s），而这段等待完全可以藏在上一块的传输时间里。
// 深度 1 就够：单块传输本来就远长于握手。不加深：更深的预取只会让运行时缓冲更多
// 尚未被客户端消费的字节，收益为零，内存与失败面反而变大。
const PREFETCH_DEPTH = 1;

// 单个 part 从"发起 fetch"到"拿到响应头"的上限。必须与下面的传输看门狗分开：
// 预取时响应头会在客户端还没开始读这一块时就到，body 尚未流动，不能用传输超时去衡量。
const PART_HEADER_TIMEOUT_MS = 90_000;

import { HttpError } from "./errors";
import { SubrequestBudget, SUBREQUEST_LIMIT } from "./budget";
import { loadManifest, type Manifest, type ManifestPart } from "./manifest";
import { fetchTelegramRange, type Env } from "./telegram";
import { signSegment, verifySegment, verifyTicket } from "./ticket";
import { handleUpload } from "./upload";

// 单次调用能承担多少个 part 由 subrequest 额度决定，而额度是硬的：Standard 每调用 50 个
// 外部请求，本账户实测 45/50 通过、55 起被拒，且在 wrangler.toml 里加 [limits] 会被 API
// 打回。名义上每个 part 花 2 个（getFile 换 file_path + range 取字节），但实测运行时计数
// 比这更高（18 part 的文件在第 11 块左右被打断，此时名义消耗只有 22），所以这里按每块 5 个
// 估，取 8 块/跳 —— 40 个额度，余量留给重试。
const PARTS_PER_HOP = 8;
// 超过一跳容量时把剩余区间交给自己的下一次调用：service binding 的子调用拥有独立的 50 个
// 额度（已实测：depth0/1/2 各 45 次外部 fetch，合计 135 次）。链深本身也有上限，实测
// 第 31 层报 "Subrequest depth limit exceeded"，所以取 24 留足余量：
// 25 次调用 × 8 块 = 200 块 ≈ 3.1 GiB，覆盖任何现实文件。
const MAX_HOP_DEPTH = 24;
// 续跳必须被证明是"当前票据区间内的子区间"，否则任何人都能借它读到任意字节。
const HOP_HEADER = "X-Gateway-Segment";
const HOP_SIGNATURE_HEADER = "X-Gateway-Segment-Sig";

interface HopRequest {
  start: number;
  end: number;
  depth: number;
}

function hopSegment(ticket: string, start: number, end: number, depth: number): string {
  return `seg:${ticket}:${start}:${end}:${depth}`;
}

// 解析续跳头并验签。任何不自洽（缺签名、越界、深度超限）都当作普通请求处理，
// 让票据本身的范围与过期时间继续说话。
async function readHop(
  request: Request,
  env: Env,
  ticket: string,
  selection: RangeSelection,
  size: number,
): Promise<HopRequest | null> {
  const raw = request.headers.get(HOP_HEADER);
  const signature = request.headers.get(HOP_SIGNATURE_HEADER);
  if (!raw || !signature) return null;

  const match = /^(\d+)-(\d+)-(\d+)$/.exec(raw.trim());
  if (!match) return null;
  const start = Number.parseInt(match[1] as string, 10);
  const end = Number.parseInt(match[2] as string, 10);
  const depth = Number.parseInt(match[3] as string, 10);
  if (!Number.isFinite(start) || !Number.isFinite(end) || start > end || start >= size) return null;
  if (end >= size || depth < 1 || depth > MAX_HOP_DEPTH) return null;
  // 只允许落在本票据自己的区间内，且只能向内收窄，不能借续跳扩大读取范围。
  if (start < selection.start || end > selection.end) return null;

  const expected = await signSegment(env.GATEWAY_SECRET ?? "", hopSegment(ticket, start, end, depth));
  if (!verifySegment(expected, signature)) return null;

  return { start, end, depth };
}

interface RangeSelection {
  start: number;
  end: number;
  length: number;
  partial: boolean;
}

// 一个已发起但可能尚未被消费的 part 上游。abort 同时约束"等响应头"和"传输字节"。
interface PartStream {
  response: Promise<Response>;
  abort: AbortController;
  headerTimer: ReturnType<typeof setTimeout>;
}

function startPartFetch(
  env: Env,
  part: ManifestPart,
  low: number,
  high: number,
  budget: SubrequestBudget,
): PartStream {
  const abort = new AbortController();
  // 响应头预算与传输预算分开：预取时这块的响应头会在客户端还没读它时就到，
  // 若沿用 300 s 的传输超时会把它误杀；反之握手也可能卡住，所以这里单独给 90 s。
  const headerTimer = setTimeout(
    () => abort.abort(new Error("part_header_timeout")),
    PART_HEADER_TIMEOUT_MS,
  );
  const response = fetchTelegramRange(env, part.bot, part.fid, low, high, budget, 3, abort.signal, 0);
  return { response, abort, headerTimer };
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    try {
      return await route(request, env);
    } catch (error) {
      if (error instanceof HttpError) {
        return errorResponse(error);
      }
      console.error("gateway_unhandled_error", error instanceof Error ? error.message : "unknown");
      return errorResponse(new HttpError(500, "internal_error"));
    }
  },
};

async function route(request: Request, env: Env): Promise<Response> {
  const url = new URL(request.url);

  if (request.method === "OPTIONS") {
    return new Response(null, { status: 204, headers: preflightHeaders() });
  }
  if (url.pathname === "/healthz") {
    return jsonResponse({ ok: true, service: "teldrive-gateway" }, 200);
  }
  // 上传分块：接受 PUT（与 VPS legacy 上传一致）与 POST。
  if ((request.method === "PUT" || request.method === "POST") && url.pathname.startsWith("/u/")) {
    const ticket = decodeURIComponent(url.pathname.slice(3));
    const response = await handleUpload(request, env, ticket);
    applyCors(response.headers);
    return response;
  }
  if (request.method !== "GET" && request.method !== "HEAD") {
    throw new HttpError(405, "method_not_allowed");
  }
  if (url.pathname.startsWith("/d/")) {
    const ticket = decodeURIComponent(url.pathname.slice(3));
    return handleDownload(request, env, ticket, request.method === "HEAD");
  }
  throw new HttpError(404, "not_found");
}

async function handleDownload(
  request: Request,
  env: Env,
  ticket: string,
  headOnly: boolean,
): Promise<Response> {
  if (!ticket) throw new HttpError(400, "ticket_missing");

  const payload = await verifyTicket(ticket, env.GATEWAY_SECRET, Math.floor(Date.now() / 1000));
  if (payload.s !== "d") throw new HttpError(400, "ticket_kind_mismatch");

  const manifest = await loadManifest(env, ticket);
  if (!manifest) throw new HttpError(404, "file_not_found");
  if (manifest.fileId !== payload.f) throw new HttpError(500, "manifest_file_mismatch");
  if (manifest.encrypted) throw new HttpError(409, "encrypted_file_unsupported");
  if (!Array.isArray(manifest.parts) || manifest.parts.length === 0) {
    throw new HttpError(409, "file_has_no_parts");
  }
  if (!Number.isFinite(manifest.size) || manifest.size < 0) {
    throw new HttpError(409, "invalid_file_size");
  }

  const attachment = payload.a === 1;
  const etag = manifest.etag ?? "";

  if (manifest.size === 0) {
    const headers = responseHeaders(manifest, attachment, etag);
    headers.set("Content-Length", "0");
    return new Response(headOnly ? null : new Uint8Array(0), { status: 200, headers });
  }

  const ticketSelection = resolveRange(payload.r, request.headers.get("Range"), manifest.size);

  // 续跳：本跳只负责票据区间内的一段，由上一跳签名指定，不能被调用方自行扩大。
  const hop = await readHop(request, env, ticket, ticketSelection, manifest.size);
  const selection: RangeSelection = hop
    ? { start: hop.start, end: hop.end, length: hop.end - hop.start + 1, partial: true }
    : ticketSelection;
  const depth = hop ? hop.depth : 0;

  // 条件请求：区间请求不做 304，避免客户端拿到不完整的缓存。
  if (!selection.partial && etag && request.headers.get("If-None-Match") === etag) {
    const headers = new Headers({ ETag: etag });
    applyCors(headers);
    return new Response(null, { status: 304, headers });
  }

  const headers = responseHeaders(manifest, attachment, etag);
  if (selection.partial) {
    headers.set("Content-Range", `bytes ${selection.start}-${selection.end}/${manifest.size}`);
  }
  const status = selection.partial ? 206 : 200;

  if (headOnly) {
    headers.set("Content-Length", String(selection.length));
    return new Response(null, { status, headers });
  }

  const needed = selectParts(manifest.parts, selection.start, selection.end);
  if (needed.length === 0) throw new HttpError(500, "part_layout_mismatch");
  const first = needed[0] as ManifestPart;
  const last = needed[needed.length - 1] as ManifestPart;
  if (first.off > selection.start || last.off + last.len - 1 < selection.end) {
    throw new HttpError(500, "part_layout_mismatch");
  }

  // 一跳能承担多少块由 subrequest 额度决定，所以这里把区间切成本跳 + 后续跳。
  // 只有本跳自己的块要花额度，尾段交给下一次调用（各自一份新额度）。
  const canDelegate = depth < MAX_HOP_DEPTH && needed.length > PARTS_PER_HOP;
  const localParts = canDelegate ? needed.slice(0, PARTS_PER_HOP) : needed;
  // 尾段从下一块的开头开始；区间起点若落在块中间，本跳已覆盖到该块末尾。
  const tailStart = canDelegate ? Math.max(selection.start, (needed[PARTS_PER_HOP] as ManifestPart).off) : -1;

  // 预算必须在写响应头之前立好。每个 part 固定要 2 个 subrequest：getFile 换 file_path
  // （每个 part 的 telegram_file_id 不同，缓存键也不同，所以省不掉）、range 取字节。
  // 若连"每块各取一次、一次都不重试"都放不下，就现在拒绝——此时响应头还没发，
  // 客户端能拿到真正的 503 并自行改用 Range 分段请求，而不是收到传了一半的 200。
  const budget = new SubrequestBudget();
  if (!budget.canAfford(2 * localParts.length)) {
    throw new HttpError(
      503,
      `subrequest_budget_too_small_parts_${localParts.length}_limit_${SUBREQUEST_LIMIT}`,
    );
  }

  // 定长流：运行时替我们维护 Content-Length 与已写字节数。
  // 写少了会在 close 时报错，客户端因此能看到截断而不是静默的短响应。
  const { readable, writable } = new FixedLengthStream(selection.length);

  // 后台泵送，不阻塞响应返回；body 直接交给 pipeTo，JS 不触碰任何字节。
  //
  // 关键：相邻 part 的上游握手要重叠。若在循环里"取一块、等它、再取下一块"，
  // 每换一次 part 都要空等一次 api.telegram.org 的响应头（实测 ~0.55 s，16 part
  // 的文件被边界拖慢 ~9 s）。这里让下一块的 fetch 在上一块开始传输时就发出去，
  // 握手完全藏在传输时间里；同时仍只有一块的字节真正经过 pipeTo，CPU 不变。
  void (async () => {
    const inflight: PartStream[] = [];
    const startPartAt = (index: number): void => {
      if (index >= localParts.length) return;
      const part = localParts[index] as ManifestPart;
      // 先替后面的块把额度占住：每一块至少要 1 次 range fetch（file_path 命中缓存时
      // getFile 不花钱），所以留下 2 * 剩余块数是下限。重试只能花超出这部分的空间，
      // 否则前面某一块的抖动会把后面块的基本额度吃光，把局部问题放大成整条响应失败。
      budget.hold(2 * (localParts.length - index - 1));
      inflight.push(
        startPartFetch(
          env,
          part,
          Math.max(selection.start, part.off) - part.off,
          Math.min(selection.end, part.off + part.len - 1) - part.off,
          budget,
        ),
      );
    };

    try {
      for (let index = 0; index < localParts.length; index += 1) {
        // 让第 index 块以及它后面的 PREFETCH_DEPTH 块都已在飞。
        while (inflight.length <= PREFETCH_DEPTH && index + inflight.length < localParts.length) {
          startPartAt(index + inflight.length);
        }
        const current = inflight.shift() as PartStream;
        const upstream = await current.response;
        // 响应头已到，握手预算用完了；接下来的预算属于字节传输。
        clearTimeout(current.headerTimer);
        const body = upstream.body;
        if (!body) throw new HttpError(502, "telegram_short_read");
        // 传输看门狗：上游一旦停止送字节，fetch 自带的超时不会再生效（它只覆盖到
        // 响应头为止），必须由我们自己打断 body 流，否则响应会永远挂着。
        const watchdog = setTimeout(
          () => current.abort.abort(new Error("part_stream_timeout")),
          PART_STREAM_TIMEOUT_MS,
        );
        try {
          // 上游忽略 Range 而多给的字节由 FixedLengthStream 兜住（超出会直接报错）。
          await body.pipeTo(writable, {
            preventClose: true,
            preventAbort: true,
            preventCancel: true,
          });
        } finally {
          clearTimeout(watchdog);
        }
      }

      // 尾段交给下一次调用：它拥有独立的 50 个 subrequest 额度，因此一次响应能覆盖
      // 的 part 数不再受单次调用限制。段边界与深度都签在头里，下一跳只能读到本票据
      // 区间内、且比本跳更靠后的一段。
      if (tailStart >= 0) {
        const segment = `${tailStart}-${selection.end}-${depth + 1}`;
        const signature = await signSegment(
          env.GATEWAY_SECRET ?? "",
          hopSegment(ticket, tailStart, selection.end, depth + 1),
        );
        const response = await env.SELF.fetch(
          new Request(`https://teldrive-gateway.invalid/d/${encodeURIComponent(ticket)}`, {
            method: "GET",
            headers: { [HOP_HEADER]: segment, [HOP_SIGNATURE_HEADER]: signature },
          }),
        );
        if (!response.ok || !response.body) {
          throw new HttpError(502, `hop_failed_${response.status}`);
        }
        const expected = selection.end - tailStart + 1;
        const declared = Number.parseInt(response.headers.get("Content-Length") ?? "", 10);
        // 下一跳算错长度就是它自己的 bug；这里只信它声明的字节数必须正好补满尾段。
        if (Number.isFinite(declared) && declared !== expected) {
          throw new HttpError(502, "hop_length_mismatch");
        }
        await response.body.pipeTo(writable, {
          preventClose: true,
          preventAbort: true,
          preventCancel: true,
        });
      }

      await writable.close();
    } catch (error) {
      // 已经预取但还没消费的块必须显式丢弃，否则它们的响应体会一直挂在运行时里。
      for (const pending of inflight) {
        clearTimeout(pending.headerTimer);
        pending.abort.abort(error instanceof Error ? error : new Error("stream_aborted"));
      }
      // 响应头早已提交，这里唯一正确的收尾就是把流打断，让客户端看到传输未完成。
      console.error(
        "gateway_stream_aborted",
        error instanceof Error ? error.message : "unknown",
      );
      try {
        await writable.abort(error);
      } catch {
        // 已中止，忽略
      }
    }
  })();

  // Content-Length 交给 FixedLengthStream 自己产生。
  headers.delete("Content-Length");
  return new Response(readable, { status, headers });
}

function selectParts(parts: ManifestPart[], start: number, end: number): ManifestPart[] {
  return parts
    .filter((part) => part.off <= end && part.off + part.len - 1 >= start)
    .sort((left, right) => left.off - right.off);
}

function resolveRange(ticketRange: string | undefined, headerRange: string | null, size: number): RangeSelection {
  if (ticketRange) {
    const bounds = /^(\d+)-(\d+)$/.exec(ticketRange.trim());
    if (!bounds) throw rangeError(size);
    const start = Number.parseInt(bounds[1] as string, 10);
    const rawEnd = Number.parseInt(bounds[2] as string, 10);
    if (!Number.isFinite(start) || !Number.isFinite(rawEnd) || start > rawEnd || start >= size) {
      throw rangeError(size);
    }
    const end = Math.min(rawEnd, size - 1);
    return { start, end, length: end - start + 1, partial: start !== 0 || end !== size - 1 };
  }

  if (headerRange) {
    const match = /^bytes=(\d*)-(\d*)$/.exec(headerRange.trim());
    if (!match) throw rangeError(size);
    const rawStart = match[1] as string;
    const rawEnd = match[2] as string;
    if (rawStart === "" && rawEnd === "") throw rangeError(size);

    let start: number;
    let end: number;
    if (rawStart === "") {
      // 后缀区间：最后 N 字节
      const suffix = Number.parseInt(rawEnd, 10);
      if (!Number.isFinite(suffix) || suffix <= 0) throw rangeError(size);
      start = Math.max(0, size - suffix);
      end = size - 1;
    } else {
      start = Number.parseInt(rawStart, 10);
      end = rawEnd === "" ? size - 1 : Math.min(Number.parseInt(rawEnd, 10), size - 1);
      if (!Number.isFinite(start) || !Number.isFinite(end) || start > end || start >= size) {
        throw rangeError(size);
      }
    }
    return { start, end, length: end - start + 1, partial: start !== 0 || end !== size - 1 };
  }

  return { start: 0, end: size - 1, length: size, partial: false };
}

function rangeError(size: number): HttpError {
  return new HttpError(416, "range_not_satisfiable", { "Content-Range": `bytes */${size}` });
}

function responseHeaders(manifest: Manifest, attachment: boolean, etag: string): Headers {
  const headers = new Headers({
    "Accept-Ranges": "bytes",
    "Content-Type": manifest.contentType || "application/octet-stream",
    "Content-Disposition": contentDisposition(manifest.name || "file", attachment),
    "X-Accel-Buffering": "no",
    "Cache-Control": "private, no-cache",
  });
  if (etag) headers.set("ETag", etag);
  if (manifest.lastModified) headers.set("Last-Modified", manifest.lastModified);
  applyCors(headers);
  return headers;
}

function contentDisposition(name: string, attachment: boolean): string {
  const ascii = name.replace(/[^\x20-\x7e]/g, "_").replace(/["\\]/g, "_");
  const encoded = encodeURIComponent(name);
  return `${attachment ? "attachment" : "inline"}; filename="${ascii}"; filename*=UTF-8''${encoded}`;
}

function applyCors(headers: Headers): void {
  headers.set("Access-Control-Allow-Origin", "*");
  headers.set(
    "Access-Control-Expose-Headers",
    "Content-Length, Content-Range, Content-Disposition, ETag, Accept-Ranges, Last-Modified",
  );
}

function preflightHeaders(): Headers {
  const headers = new Headers({
    "Access-Control-Allow-Methods": "GET, HEAD, POST, PUT, OPTIONS",
    "Access-Control-Allow-Headers": "Range, If-None-Match, If-Modified-Since, Authorization, Content-Type",
    "Access-Control-Max-Age": "86400",
  });
  applyCors(headers);
  return headers;
}

function jsonResponse(body: unknown, status: number): Response {
  const headers = new Headers({ "Content-Type": "application/json" });
  applyCors(headers);
  return new Response(JSON.stringify(body), { status, headers });
}

function errorResponse(error: HttpError): Response {
  const headers = new Headers({ "Content-Type": "application/json" });
  for (const [name, value] of Object.entries(error.headers)) {
    headers.set(name, value);
  }
  applyCors(headers);
  if (error.status >= 500) {
    console.error("gateway_error", error.status, error.message);
  }
  return new Response(JSON.stringify({ error: error.message }), { status: error.status, headers });
}
