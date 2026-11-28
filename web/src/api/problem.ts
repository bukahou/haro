// 服务端的错误是 RFC 9457 的 problem（title 是按界面语言写好的说明，code 是稳定的错误码）：显示 title，程序要判断时看 code。
import { AxiosError } from "axios";

import type { Problem } from "./generated/model";

export function problemOf(error: unknown): Problem | null {
  if (error instanceof AxiosError && error.response?.data && typeof error.response.data === "object" && "code" in error.response.data) {
    return error.response.data as Problem;
  }
  return null;
}

// problemMessage 是给人看的一句：服务端给的说明；连不上之类没有 problem 时用错误本身的说明。
export function problemMessage(error: unknown): string {
  return problemOf(error)?.title ?? (error instanceof Error ? error.message : String(error));
}
