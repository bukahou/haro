"use client";

// 这台浏览器自己的界面偏好（只是方便，存不住也照常能用）：外观（跟系统 / 浅色 / 深色）、侧栏收没收起。
import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";

import { uiStorageKey } from "@/lib/themeScript";

export type Theme = "system" | "light" | "dark";

interface UIState {
  theme: Theme;
  sidebarCollapsed: boolean;
  setTheme: (theme: Theme) => void;
  toggleSidebar: () => void;
  expandSidebar: () => void;
}

export const useUI = create<UIState>()(
  persist(
    (set) => ({
      theme: "system",
      sidebarCollapsed: false,
      setTheme: (theme) => set({ theme }),
      toggleSidebar: () => set((state) => ({ sidebarCollapsed: !state.sidebarCollapsed })),
      expandSidebar: () => set({ sidebarCollapsed: false }),
    }),
    { name: uiStorageKey, storage: createJSONStorage(() => localStorage) },
  ),
);

export function applyTheme(theme: Theme) {
  if (theme === "system") {
    delete document.documentElement.dataset.theme;
  } else {
    document.documentElement.dataset.theme = theme;
  }
}
