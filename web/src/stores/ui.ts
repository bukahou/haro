"use client";

// 这台浏览器自己的界面偏好（只是方便，存不住也照常能用）：外观（跟系统 / 浅色 / 深色）、侧栏收没收起；
// 还有窄屏的侧栏开没开（只在这一页里，不存）。
import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";

import { uiStorageKey } from "@/lib/themeScript";

export type Theme = "system" | "light" | "dark";

interface UIState {
  theme: Theme;
  sidebarCollapsed: boolean; // 宽屏：侧栏收成图标栏
  drawerOpen: boolean; // 窄屏：侧栏从左边滑出来了（不存）
  setTheme: (theme: Theme) => void;
  toggleSidebar: () => void;
  expandSidebar: () => void;
  setDrawer: (open: boolean) => void;
}

export const useUI = create<UIState>()(
  persist(
    (set) => ({
      theme: "system",
      sidebarCollapsed: false,
      drawerOpen: false,
      setTheme: (theme) => set({ theme }),
      toggleSidebar: () => set((state) => ({ sidebarCollapsed: !state.sidebarCollapsed })),
      expandSidebar: () => set({ sidebarCollapsed: false }),
      setDrawer: (drawerOpen) => set({ drawerOpen }),
    }),
    {
      name: uiStorageKey,
      storage: createJSONStorage(() => localStorage),
      partialize: (state) => ({ theme: state.theme, sidebarCollapsed: state.sidebarCollapsed }),
    },
  ),
);

export function applyTheme(theme: Theme) {
  if (theme === "system") {
    delete document.documentElement.dataset.theme;
  } else {
    document.documentElement.dataset.theme = theme;
  }
}
