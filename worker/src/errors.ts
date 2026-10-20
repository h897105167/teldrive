// errors.ts —— 统一的、可安全外泄的错误类型。
// 只有稳定的机器可读代码会进入响应体，绝不携带 token / file_path / 内部 URL。

export class HttpError extends Error {
  readonly status: number;
  readonly headers: Record<string, string>;

  constructor(status: number, message: string, headers: Record<string, string> = {}) {
    super(message);
    this.name = "HttpError";
    this.status = status;
    this.headers = headers;
  }
}
