"use client";

// 登录的令牌（haro 发的访问令牌 + 刷新令牌）：⭐ 唯一的出处是这个 store，浏览器的 localStorage 只是它的持久化副本。
// ⛔ 别处不直接读写 localStorage 里的令牌（否则 persist 下次写回时会用内存里的旧值把新令牌盖掉）。
// 这个文件不引用接口代码（接口代码引用它，反过来会成环）。
import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";

export interface Tokens {
  accessToken: string;
  refreshToken: string;
}

interface SessionState {
  tokens: Tokens | null;
  signIn: (tokens: Tokens) => void;
  signOut: () => void;
}

export const useSession = create<SessionState>()(
  persist(
    (set) => ({
      tokens: null,
      signIn: (tokens) => set({ tokens }),
      signOut: () => set({ tokens: null }),
    }),
    { name: "haro-session", storage: createJSONStorage(() => localStorage) },
  ),
);

// 别的标签页登录 / 刷新 / 登出时，这边跟着换（persist 不会自己同步）。
if (typeof window !== "undefined") {
  window.addEventListener("storage", (event) => {
    if (event.key === "haro-session") {
      void useSession.persist.rehydrate();
    }
  });
}
