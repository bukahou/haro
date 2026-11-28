"use client";

// 各条对话已经读到的几轮，和正在答的那一轮（live）。放在 store 里而不是组件里：新对话发出第一句后地址从 /chat/new 换成
// /chat/<编号>、切到别的页再回来，都不打断正在流回来的回答。
import { create } from "zustand";

import { createConversation, listTurns, stopTurn, uploadAttachment } from "@/api/generated/conversations/conversations";
import type { ChatEvent, ConfirmationCard, Conversation, ResearchCard, Turn } from "@/api/generated/model";
import { problemMessage, problemOf } from "@/api/problem";
import { decideStream, sendMessageStream, StreamCutError } from "@/api/stream";

import { useConversations } from "./conversations";

export type LiveFailure =
  | { kind: "problem"; message: string } // 服务端写好的说明（额度用完、出错……）
  | { kind: "cut" }; // 流没收尾就断了（服务端照样会答完，重新读这条对话就能看到）

// 正在答的这一轮里，按发生的顺序：文字、工具（在做 / 做完）、卡片
export type LiveBlock =
  | { type: "text"; text: string }
  | { type: "tool"; callId: string; label: string; done: boolean }
  | { type: "research"; research: ResearchCard }
  | { type: "confirmation"; confirmation: ConfirmationCard };

// 这一轮是怎么来的：说一句（可以带图），或者在确认卡片上点了确认 / 取消
export type LiveRequest = { kind: "message"; text: string; image: File | null } | { kind: "decision"; actionId: string; confirm: boolean };

export interface LiveTurn {
  request: LiveRequest;
  imageURL: string | null;
  blocks: LiveBlock[];
  turnId: string | null; // started 之后才有，停止时用
  answering: boolean;
  ending: "incomplete" | "cancelled" | null; // 不是正常答完时怎么结束的
  failure: LiveFailure | null;
  notSent: boolean; // 服务端没收下（上传失败、额度用完、上一句还在答……）：可以重发
  settled: boolean; // 已经读回服务端存的记录：只剩出错 / 没说完的说明
}

export interface ThreadState {
  turns: Turn[]; // 旧的在前
  nextBefore: number | null; // 再往前翻时带的 before；null = 到头了
  loaded: boolean;
  missing: boolean; // 这条对话不在了（删了 / 不是自己的）
  error: string | null;
  loadingOlder: boolean;
  live: LiveTurn | null;
}

const emptyThread: ThreadState = { turns: [], nextBefore: null, loaded: false, missing: false, error: null, loadingOlder: false, live: null };

// 起标题是后台做的，第一轮答完后过一会儿才有：答完时读一次列表，过一会儿再读一次
const titleRecheckMillis = 8000;

// 一个事件落到这一轮的几块上（不认识的事件跳过：契约说以后会加）
function applyEvent(blocks: LiveBlock[], event: ChatEvent): LiveBlock[] {
  switch (event.type) {
    case "text": {
      const last = blocks[blocks.length - 1];
      if (last?.type === "text") return [...blocks.slice(0, -1), { type: "text", text: last.text + (event.text ?? "") }];
      return [...blocks, { type: "text", text: event.text ?? "" }];
    }
    case "tool_started":
      return [...blocks, { type: "tool", callId: event.call_id ?? "", label: event.label ?? "", done: false }];
    case "tool_finished":
      return blocks.map((block) => (block.type === "tool" && block.callId === event.call_id ? { ...block, done: true } : block));
    case "research":
      return event.research ? [...blocks, { type: "research", research: event.research }] : blocks;
    case "confirmation":
      return event.confirmation ? [...blocks, { type: "confirmation", confirmation: event.confirmation }] : blocks;
    default:
      return blocks;
  }
}

interface ChatState {
  threads: Record<string, ThreadState>;
  load: (conversationId: string) => Promise<void>;
  loadOlder: (conversationId: string) => Promise<void>;
  start: (incognito: boolean) => Promise<Conversation>;
  send: (conversationId: string, text: string, image: File | null) => Promise<void>;
  decide: (conversationId: string, actionId: string, confirm: boolean) => Promise<void>;
  retry: (conversationId: string) => Promise<void>;
  stop: (conversationId: string) => Promise<void>;
  dismiss: (conversationId: string) => void;
}

export const useChat = create<ChatState>((set, get) => {
  const thread = (conversationId: string) => get().threads[conversationId] ?? emptyThread;
  const patch = (conversationId: string, change: Partial<ThreadState>) =>
    set((state) => ({ threads: { ...state.threads, [conversationId]: { ...(state.threads[conversationId] ?? emptyThread), ...change } } }));
  const patchLive = (conversationId: string, change: Partial<LiveTurn> | ((live: LiveTurn) => Partial<LiveTurn>)) => {
    const live = thread(conversationId).live;
    if (live) patch(conversationId, { live: { ...live, ...(typeof change === "function" ? change(live) : change) } });
  };

  // run 开一轮（说一句或点卡片），收事件流，答完以服务端存的记录为准
  const run = async (conversationId: string, request: LiveRequest) => {
    const previous = thread(conversationId).live;
    const image = request.kind === "message" ? request.image : null;
    const reuse = previous?.request.kind === "message" && previous.request.image === image && previous.imageURL;
    if (previous?.imageURL && !reuse) URL.revokeObjectURL(previous.imageURL);
    const imageURL = image ? (reuse ? previous.imageURL : URL.createObjectURL(image)) : null;
    patch(conversationId, {
      live: { request, imageURL, blocks: [], turnId: null, answering: true, ending: null, failure: null, notSent: false, settled: false },
    });
    let accepted = false;
    const onEvent = (event: ChatEvent) => {
      switch (event.type) {
        case "started":
          accepted = true;
          patchLive(conversationId, { turnId: event.turn_id ?? null });
          return;
        case "done":
          patchLive(conversationId, { ending: event.status === "completed" || !event.status ? null : event.status });
          return;
        case "failed":
          patchLive(conversationId, { failure: { kind: "problem", message: event.message ?? "" } });
          return;
        default:
          patchLive(conversationId, (live) => ({ blocks: applyEvent(live.blocks, event) }));
      }
    };
    try {
      if (request.kind === "message") {
        const attachment = request.image ? (await uploadAttachment(conversationId, request.image)).attachment_id : null;
        await sendMessageStream(conversationId, { text: request.text, attachment }, onEvent);
      } else {
        await decideStream(conversationId, request.actionId, { decision: request.confirm ? "confirm" : "cancel" }, onEvent);
      }
    } catch (failure) {
      patchLive(conversationId, {
        failure: failure instanceof StreamCutError ? { kind: "cut" } : { kind: "problem", message: problemMessage(failure) },
        notSent: !accepted,
      });
    }
    patchLive(conversationId, { answering: false });
    // 收下了的这一轮以服务端存的为准（含工具和卡片）：重新读最新一页；没收下的那句留着，可以重发
    if (accepted || !thread(conversationId).live?.notSent) {
      await get().load(conversationId);
      const settled = thread(conversationId).live;
      if (settled && !settled.failure && !settled.ending) {
        if (settled.imageURL) URL.revokeObjectURL(settled.imageURL);
        patch(conversationId, { live: null });
      } else if (settled) {
        patch(conversationId, { live: { ...settled, settled: true } }); // 话和回答已经在读回来的记录里了，只留说明
      }
    }
    void useConversations.getState().load();
    setTimeout(() => void useConversations.getState().load(), titleRecheckMillis);
  };

  return {
    threads: {},

    load: async (conversationId) => {
      try {
        const page = await listTurns(conversationId);
        patch(conversationId, { turns: page.turns, nextBefore: page.next_before, loaded: true, missing: false, error: null });
      } catch (failure) {
        if (problemOf(failure)?.code === "not_found") {
          patch(conversationId, { loaded: true, missing: true });
        } else {
          patch(conversationId, { loaded: true, error: problemMessage(failure) });
        }
      }
    },

    loadOlder: async (conversationId) => {
      const current = thread(conversationId);
      if (current.nextBefore === null || current.loadingOlder) return;
      patch(conversationId, { loadingOlder: true });
      try {
        const page = await listTurns(conversationId, { before: current.nextBefore });
        patch(conversationId, { turns: [...page.turns, ...thread(conversationId).turns], nextBefore: page.next_before, loadingOlder: false });
      } catch (failure) {
        patch(conversationId, { loadingOlder: false, error: problemMessage(failure) });
      }
    },

    start: async (incognito) => {
      const conversation = await createConversation({ incognito });
      useConversations.getState().upsert(conversation);
      patch(conversation.conversation_id, { loaded: true });
      return conversation;
    },

    send: (conversationId, text, image) => run(conversationId, { kind: "message", text, image }),

    decide: (conversationId, actionId, confirm) => run(conversationId, { kind: "decision", actionId, confirm }),

    retry: async (conversationId) => {
      const live = thread(conversationId).live;
      if (live) await run(conversationId, live.request);
    },

    stop: async (conversationId) => {
      const live = thread(conversationId).live;
      if (live?.turnId) {
        await stopTurn(conversationId, live.turnId).catch(() => undefined); // 已经答完了也算停了
      }
    },

    dismiss: (conversationId) => {
      const live = thread(conversationId).live;
      if (live?.imageURL) URL.revokeObjectURL(live.imageURL);
      patch(conversationId, { live: null });
    },
  };
});
