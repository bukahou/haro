"use client";

// 各条对话已经读到的几轮，和正在答的那一轮（live）。放在 store 里而不是组件里：新对话发出第一句后地址从 /chat/new 换成
// /chat/<编号>、切到别的页再回来，都不打断正在流回来的回答。
import { create } from "zustand";

import { createConversation, listTurns, stopTurn, uploadAttachment } from "@/api/generated/conversations/conversations";
import type { Conversation, Turn } from "@/api/generated/model";
import { problemMessage, problemOf } from "@/api/problem";
import { sendMessageStream, StreamCutError } from "@/api/stream";

import { useConversations } from "./conversations";

export type LiveFailure =
  | { kind: "problem"; message: string } // 服务端写好的说明（额度用完、出错……）
  | { kind: "cut" }; // 流没收尾就断了（服务端照样会答完，重新读这条对话就能看到）

export interface LiveTurn {
  said: string;
  image: File | null; // 这句带的图片（重发时还要用）
  imageURL: string | null;
  text: string; // 已经流回来的回答（Markdown）
  turnId: string | null; // started 之后才有，停止时用
  answering: boolean;
  ending: "incomplete" | "cancelled" | null; // 不是正常答完时怎么结束的
  failure: LiveFailure | null;
  notSent: boolean; // 这一句服务端没收下（上传失败、额度用完、上一句还在答……）：可以重发
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

interface ChatState {
  threads: Record<string, ThreadState>;
  load: (conversationId: string) => Promise<void>;
  loadOlder: (conversationId: string) => Promise<void>;
  start: (incognito: boolean) => Promise<Conversation>;
  send: (conversationId: string, text: string, image: File | null) => Promise<void>;
  stop: (conversationId: string) => Promise<void>;
  dismiss: (conversationId: string) => void;
}

export const useChat = create<ChatState>((set, get) => {
  const thread = (conversationId: string) => get().threads[conversationId] ?? emptyThread;
  const patch = (conversationId: string, change: Partial<ThreadState>) =>
    set((state) => ({ threads: { ...state.threads, [conversationId]: { ...(state.threads[conversationId] ?? emptyThread), ...change } } }));
  const patchLive = (conversationId: string, change: Partial<LiveTurn>) => {
    const live = thread(conversationId).live;
    if (live) patch(conversationId, { live: { ...live, ...change } });
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

    send: async (conversationId, text, image) => {
      const previous = thread(conversationId).live;
      if (previous?.imageURL && previous.image !== image) URL.revokeObjectURL(previous.imageURL);
      const imageURL = image ? (previous?.image === image && previous.imageURL ? previous.imageURL : URL.createObjectURL(image)) : null;
      patch(conversationId, {
        live: { said: text, image, imageURL, text: "", turnId: null, answering: true, ending: null, failure: null, notSent: false },
      });
      let accepted = false;
      try {
        const attachment = image ? (await uploadAttachment(conversationId, image)).attachment_id : null;
        await sendMessageStream(conversationId, { text, attachment }, (event) => {
          switch (event.type) {
            case "started":
              accepted = true;
              patchLive(conversationId, { turnId: event.turn_id ?? null });
              break;
            case "text":
              patchLive(conversationId, { text: (thread(conversationId).live?.text ?? "") + (event.text ?? "") });
              break;
            case "done":
              patchLive(conversationId, { ending: event.status === "completed" || !event.status ? null : event.status });
              break;
            case "failed":
              patchLive(conversationId, { failure: { kind: "problem", message: event.message ?? "" } });
              break;
          }
        });
      } catch (failure) {
        patchLive(conversationId, {
          failure: failure instanceof StreamCutError ? { kind: "cut" } : { kind: "problem", message: problemMessage(failure) },
          notSent: !accepted,
        });
      }
      patchLive(conversationId, { answering: false });
      const live = thread(conversationId).live;
      // 收下了的这一轮以服务端存的为准（含工具往来）：重新读最新一页；没收下的那句留着，可以重发
      if (accepted || !live?.notSent) {
        await get().load(conversationId);
        const settled = thread(conversationId).live;
        if (settled && !settled.failure && !settled.ending) {
          if (settled.imageURL) URL.revokeObjectURL(settled.imageURL);
          patch(conversationId, { live: null });
        } else if (settled) {
          // 出错或没说完：话和回答已经在读回来的记录里了，只留说明
          patch(conversationId, { live: { ...settled, said: "", text: "", image: null } });
        }
      }
      const conversations = useConversations.getState();
      void conversations.load();
      setTimeout(() => void useConversations.getState().load(), titleRecheckMillis);
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
