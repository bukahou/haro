"use client";

// 宽屏（≥ 900px，有侧栏）还是窄屏（底部标签）。和 haro.css 里的分界一致。
import { useSyncExternalStore } from "react";

const query = "(min-width: 900px)";

const subscribe = (onChange: () => void) => {
  const media = window.matchMedia(query);
  media.addEventListener("change", onChange);
  return () => media.removeEventListener("change", onChange);
};

export function useWide(): boolean {
  return useSyncExternalStore(subscribe, () => window.matchMedia(query).matches, () => true);
}
