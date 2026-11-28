"use client";

// 外壳要的账号信息：名字、角色、管理员的待处理数（侧栏和底部标签上的红点）。打开时读一次，窗口回到前台时、管理员处理完一项时再读。
import { create } from "zustand";

import { getOverview } from "@/api/generated/me/me";
import type { Overview } from "@/api/generated/model";

interface AccountState {
  overview: Overview | null;
  refresh: () => Promise<void>;
}

export const useAccount = create<AccountState>((set) => ({
  overview: null,
  refresh: async () => {
    try {
      set({ overview: await getOverview() });
    } catch {
      // 外壳上的名字和红点读不到时先不显示；各页自己会显示出错说明
    }
  },
}));

export function pendingCount(overview: Overview | null): number {
  return overview?.admin ? overview.admin.pending_accounts + overview.admin.pending_terminals : 0;
}
