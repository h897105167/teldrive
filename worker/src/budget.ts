// budget.ts —— 单次 Worker 调用的外部 subrequest 预算。
//
// 为什么需要它：Free/Standard 计划下每个 Worker 调用最多 50 个外部 subrequest（实测确认：
// 顺序 45/50 次成功，55 起报 "Too many subrequests"，且 wrangler.toml 里的
// [limits] subrequests 在免费账户上被 API 拒绝，无法调高）。而一次整文件下载要为每个
// part 花 2 个 subrequest（getFile 换 file_path + range 取字节），18 part 的文件冷缓存
// 就是 37 个，只剩 13 次余量给重试。重试一旦堆起来，运行时会直接把响应从中间掐断，
// 客户端只能看到 "transfer closed"——没有错误码，也无法判断该不该重试。
//
// 所以把预算显式化：
//   1. 每次 fetch 前先领额度，不足就抛带名字的错误，而不是等运行时来杀；
//   2. 调用方可以登记"后面还有 N 块必须留够"，让重试不能吃掉后续 part 的额度。
// 这样超限会变成一个可识别、可重试、可告警的事件。

import { HttpError } from "./errors";

/** 本账户实测的单调用外部 subrequest 上限。 */
export const SUBREQUEST_LIMIT = 50;

/** 预留给清单请求等零散调用的额度，避免压着边界把预算刚好用尽。 */
export const SUBREQUEST_RESERVE = 2;

export class SubrequestBudget {
  private used = 0;
  private reserved = 0;

  constructor(private readonly limit: number = SUBREQUEST_LIMIT - SUBREQUEST_RESERVE) {}

  get consumed(): number {
    return this.used;
  }

  get remaining(): number {
    return Math.max(0, this.limit - this.used - this.reserved);
  }

  /**
   * 声明后续工作至少还需要 count 个额度。重试只能花超出这部分的部分，
   * 否则前面的块会吃掉后面块的预算，把一次局部抖动放大成整条响应失败。
   */
  hold(count: number): void {
    this.reserved = Math.max(0, count);
  }

  /** 领一个额度；不足时抛出可诊断的 503，而不是让运行时掐断响应。 */
  take(reason: string): void {
    if (this.used + this.reserved >= this.limit) {
      throw new HttpError(503, `subrequest_budget_exhausted_${reason}`);
    }
    this.used += 1;
  }

  /** 不做检查也允许占用的最大次数，供调用方决定还能不能再试。 */
  canAfford(count: number): boolean {
    return this.used + this.reserved + count <= this.limit;
  }
}
