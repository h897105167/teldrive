# teldrive gateway Worker（数据面代理）

本目录是 teldrive 重构后的**数据面**：一个部署在 Cloudflare 上的 Worker，代理 Telegram Bot API 的文件读写。
目标是让**文件字节永不经过用户 VPS**——VPS 只做控制面（元数据、票据签发、索引、UI），Worker 直接与 `api.telegram.org` 交换字节。

```
浏览器 / rclone
   │  ① 请求下载（带 JWT）
   ▼
VPS teldrive server（控制面，不过字节）
   │  ② 校验权限 → 签发 ticket → 302 到 Worker
   ▼
Cloudflare Worker（数据面）
   │  ③ 用 ticket 向 VPS 取 manifest（Bearer 自证）
   │  ④ getFile 换 file_path → GET api.telegram.org/file/bot<token>/<path>
   ▼
Telegram 服务器
```

## 契约

`GATEWAY_SECRET` 为服务端与 Worker 共享的 HMAC 密钥，两侧算法必须逐字节一致。

### Ticket（下载）

格式：

```
base64url(payloadJson) "." base64url(HMAC_SHA256(GATEWAY_SECRET, base64url(payloadJson)))
```

payload 为**紧凑 JSON**（无空格、键序固定不重要但字段名固定）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `v` | int | 固定 `1`，用于未来演进 |
| `s` | string | `"d"` = 下载；`"u"` = 上传（P2 使用） |
| `f` | string | 文件 UUID（teldrive `files.id`） |
| `r` | string? | 可选，`"start-end"`，闭区间**字节偏移**（基于明文全文件）。缺省表示全文件 |
| `a` | int? | `1` 表示要求 `Content-Disposition: attachment`（对应请求的 `?download=1`） |
| `u` | int | 用户 ID（仅审计用，Worker 不做鉴权，鉴权在签发时已完成） |
| `n` | string | 随机 nonce，防重放观测 |
| `e` | int | 过期时间（Unix 秒）。Worker 校验 `e > now`，否则 410 |
| `p` | int? | 上传分块序号（1 基），仅 `s="u"` |
| `b` | int? | bot token 槽位（1 基），仅 `s="u"` |
| `z` | int? | 分块明文字节数（≤16 MiB），仅 `s="u"` |
| `l` | string? | 分块租约令牌，回调落库时原样带回，仅 `s="u"` |

规则的 base64url：`+`→`-`、`/`→`_`、去掉 `=` 填充。签名对**未解码的 payload 段字符串**计算。

Worker 路由：`GET|HEAD /d/:ticket` 下载；`POST /u/:ticket` 上传分块。

上传流程（`s="u"`）：验票 → 校验请求体字节数 == `z` → multipart `sendDocument` 到 `api.telegram.org`（chat_id=`CHANNEL_ID`，文件名为随机 hex）→ 429 退避 → 取 `file_id/message_id/file_size` 回调 VPS `UPLOAD_COMPLETE_URL`（Bearer ticket，JSON `{fid,mid,size}`）→ 落库成功才 200 给客户端。**`file_id` 绝不返回给客户端**。

**绝对禁止**写入 ticket、响应体、错误体或 `wrangler tail` 日志的内容：Telegram `file_path`（约 1 小时过期）、任何含 bot token 的 `api.telegram.org` URL。

### Manifest（Worker → 服务端内部端点）

- 方法：`GET {MANIFEST_URL}`，头 `Authorization: Bearer <ticket>`；ticket 即自证（HMAC 校验通过即视为内部请求）。
- 404：文件不存在或尚未攒够 parts。
- 200 响应体：

```json
{
  "v": 1,
  "fileId": "0f2c...-uuid",
  "name": "movie.mkv",
  "size": 12345678901,
  "contentType": "video/x-matroska",
  "etag": "\"a1b2c3\"",
  "lastModified": "Wed, 15 Oct 2026 03:04:05 GMT",
  "partSize": 16777216,
  "encrypted": false,
  "parts": [
    { "n": 1, "off": 0,         "len": 16777216, "bot": 1, "fid": "BQACAgQAAxk..." },
    { "n": 2, "off": 16777216,  "len": 16777216, "bot": 2, "fid": "BQACAgQAAxk..." }
  ]
}
```

字段约定：

- `off`：该 part 在**明文全文件**中的起始偏移；`len`：该 part 的明文字节数。`parts` 按 `off` 升序、连续覆盖 `[0, size)`。
- `bot`：token 槽位（`1` → `BOT_TOKEN_1`，`2` → `BOT_TOKEN_2`）。**file_id 按 bot 隔离**，必须按 part 注明归属，用错 token 会 400。
- `fid`：Telegram `file_id`（不是 `file_path`）。
- `etag` / `lastModified`：与 VPS 直连路径保持一致，便于客户端复用。
- `encrypted`：网关模式要求为 `false`（服务端加密关闭，校验和由客户端计算）。为 `true` 时 Worker 一律拒绝。
- `partSize`：约定 16 MiB（Telegram Bot API 下载上限 20 MiB，留余量）。

Worker 侧缓存：`getFile` 结果 TTL 50 分钟（`file_path` 约 1 小时失效）；manifest TTL 120 秒。

### 请求 / 响应行为

- `Range`：支持单区间 `bytes=a-b`、`bytes=a-`、`bytes=-n`；跨 part 自动拼接；不满足返回 `416` 并带 `Content-Range: bytes */<size>`。
- ticket 里带 `r` 时以 `r` 为准（服务端已限定区间）。
- `HEAD`：只回头，不触碰 Telegram。
- `If-None-Match` 命中且非区间请求 → `304` + `ETag`。
- `429`：读取 Telegram 的 `retry_after`（优先响应头，其次 JSON `parameters.retry_after`），退避重试（上限 3 次 / 10 秒）。
- CORS：`Access-Control-Allow-Origin: *`，暴露 `Content-Length`、`Content-Range`、`Content-Disposition`、`ETag`、`Accept-Ranges`、`Last-Modified`；处理 `OPTIONS` 预检。
- 全程流式（`new Response(upstream.body / ReadableStream, …)`），**绝不缓冲整个文件**。

### 错误响应

```json
{ "error": "ticket_expired" }
```

只暴露稳定的机器可读代码，不含任何服务端内部信息。

## 环境变量

| 名称 | 类型 | 说明 |
| --- | --- | --- |
| `CHANNEL_ID` | var | 存储频道 ID |
| `MANIFEST_URL` | var | 服务端 manifest 端点完整 URL |
| `UPLOAD_COMPLETE_URL` | var | 上传完成回调端点；缺省从 `MANIFEST_URL` 推导同源 `/internal/gateway/upload-complete` |
| `BOT_TOKEN_1` | secret | bot 槽位 1 的 token |
| `BOT_TOKEN_2` | secret | bot 槽位 2 的 token |
| `GATEWAY_SECRET` | secret | 与 teldrive server 共享的 HMAC 密钥 |

本地开发用 `.dev.vars`（已 gitignore）；生产用：

```powershell
bunx wrangler secret put BOT_TOKEN_1
bunx wrangler secret put BOT_TOKEN_2
bunx wrangler secret put GATEWAY_SECRET
```

## 部署与本地调试

```powershell
bun install
bun run typecheck
bun run dev      # 本地起 Worker，读取 .dev.vars
bun run deploy   # 部署到 Cloudflare
bun run tail     # 观察线上日志（注意不要打印 file_path）
```

`wrangler` 版本在 `package.json` 中**精确固定**为 `4.151.0`，避免隐式升级改变行为。

## 已知限制 / 后续

- 现存 part 全部是 MTProto 的 `channel_id + message_id`，Bot API 无法由 `message_id` 反查 `file_id`，因此**存量文件无法 302 卸载**，仍走 VPS fallback。Worker 只服务新写入（`storage_kind = 'bot'`）的 part。
- 服务端加密文件不走 Worker。
- 上传（`s: "u"`）已实现：`POST /u/:ticket` 将分块经 `sendDocument` 直送 Telegram，回调 VPS 落库后返回；声明了期望哈希或加密的上传会话不会签发上传票据。
