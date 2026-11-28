// 收一轮回答的事件流：说一句（POST /conversations/{id}/messages）、点确认卡片（POST …/confirmations/{actionId}），
// 响应都是 text/event-stream。浏览器的 EventSource 不能 POST、不能带令牌，所以用 fetch 读响应体，交给 eventsource-parser
// 切成事件。令牌和刷新与别的接口同一套（request.ts）。
//
// 约定（契约 ChatEvent）：started → 按发生的顺序若干 text / tool_started / tool_finished / research / confirmation → done 或 failed。
// 流没以 done / failed 收尾 = 失败（连接断了），由调用方当出错处理（服务端照样会答完、存好，重新读这条对话就能看到）。
import { createParser } from "eventsource-parser";

import { useSession } from "@/auth/session";

import { apiBaseURL } from "./config";
import type { ChatEvent, ConfirmationDecision, MessageSend, Problem } from "./generated/model";
import { ProblemError } from "./problem";
import { interfaceLanguage, refreshTokens } from "./request";

export class StreamCutError extends Error {}

async function post(path: string, body: MessageSend | ConfirmationDecision, accessToken: string | undefined, signal: AbortSignal | undefined) {
  return fetch(`${apiBaseURL}${path}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "text/event-stream",
      "Accept-Language": interfaceLanguage,
      ...(accessToken ? { Authorization: `Bearer ${accessToken}` } : {}),
    },
    body: JSON.stringify(body),
    signal,
  });
}

async function problemFrom(response: Response): Promise<ProblemError> {
  try {
    return new ProblemError((await response.json()) as Problem);
  } catch {
    // 不是 problem 的回应（网关回的页面之类）：没有说明可给，用状态码
    return new ProblemError({ type: "about:blank", code: "internal", title: response.statusText || String(response.status), status: response.status });
  }
}

const conversationPath = (conversationId: string) => `/api/v1/conversations/${encodeURIComponent(conversationId)}`;

// sendMessageStream 发一句；decideStream 是在确认卡片上点了确认 / 取消。每收到一个事件调一次 onEvent；
// 以 done / failed 收尾时正常返回，否则抛 StreamCutError。
export function sendMessageStream(conversationId: string, body: MessageSend, onEvent: (event: ChatEvent) => void, signal?: AbortSignal) {
  return readStream(`${conversationPath(conversationId)}/messages`, body, onEvent, signal);
}

export function decideStream(conversationId: string, actionId: string, body: ConfirmationDecision, onEvent: (event: ChatEvent) => void, signal?: AbortSignal) {
  return readStream(`${conversationPath(conversationId)}/confirmations/${encodeURIComponent(actionId)}`, body, onEvent, signal);
}

async function readStream(
  path: string,
  body: MessageSend | ConfirmationDecision,
  onEvent: (event: ChatEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  const tokens = useSession.getState().tokens;
  let response = await post(path, body, tokens?.accessToken, signal);
  if (response.status === 401 && tokens) {
    const renewed = await refreshTokens(tokens.accessToken);
    if (renewed) {
      response = await post(path, body, renewed.accessToken, signal);
    }
  }
  if (!response.ok || !response.body) {
    throw await problemFrom(response);
  }
  let finished = false;
  const parser = createParser({
    onEvent: (message) => {
      const event = JSON.parse(message.data) as ChatEvent;
      if (event.type === "done" || event.type === "failed") finished = true;
      onEvent(event);
    },
  });
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  // done / failed 是最后一个事件：收到就不再读（不等服务端关连接，中间的代理可能还留着它）
  while (!finished) {
    const { value, done } = await reader.read();
    if (done) break;
    parser.feed(value);
  }
  if (finished) await reader.cancel().catch(() => undefined);
  if (!finished) {
    throw new StreamCutError("stream ended without done / failed");
  }
}
