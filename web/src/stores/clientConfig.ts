"use client";

// 服务端给的业务参数（上传大小、字数上限、月上限的档位、版本……）：读一次，各页从这里取，⛔ 不在前端另写一份。
import { create } from "zustand";

import { getClientConfig } from "@/api/generated/me/me";
import type { ClientConfig } from "@/api/generated/model";

interface ClientConfigState {
  config: ClientConfig | null;
  load: () => Promise<void>;
}

export const useClientConfig = create<ClientConfigState>((set, get) => ({
  config: null,
  load: async () => {
    if (get().config) return;
    try {
      set({ config: await getClientConfig() });
    } catch {
      // 读不到时用到它的地方不做本地检查，交给服务端校验
    }
  },
}));
