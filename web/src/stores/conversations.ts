"use client";

// 对话列表（侧栏、手机的聊天页共用一份）：读一次，之后新建 / 删除 / 答完一轮时更新或重新读。
import { create } from "zustand";

import { listConversations } from "@/api/generated/conversations/conversations";
import type { Conversation } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";

interface ConversationsState {
  list: Conversation[] | null;
  error: string | null;
  load: () => Promise<void>;
  upsert: (conversation: Conversation) => void;
  remove: (conversationId: string) => void;
}

export const useConversations = create<ConversationsState>((set) => ({
  list: null,
  error: null,
  load: async () => {
    try {
      set({ list: await listConversations(), error: null });
    } catch (failure) {
      set({ error: problemMessage(failure) });
    }
  },
  upsert: (conversation) =>
    set((state) => {
      const rest = (state.list ?? []).filter((item) => item.conversation_id !== conversation.conversation_id);
      // 语音那条永远在最前（和服务端的排序一致），新的接在它后面
      const voice = rest.filter((item) => item.kind === "voice");
      const chats = rest.filter((item) => item.kind !== "voice");
      return { list: conversation.kind === "voice" ? [conversation, ...chats] : [...voice, conversation, ...chats] };
    }),
  remove: (conversationId) => set((state) => ({ list: (state.list ?? []).filter((item) => item.conversation_id !== conversationId) })),
}));
