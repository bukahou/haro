"use client";

// 从浏览器里读回令牌之前不知道登没登录：读完（hydrated）之前什么都不判断，免得刚打开就被当成没登录踢回登录页。
import { useSyncExternalStore } from "react";

import { useSession } from "./session";

const subscribeHydration = (onChange: () => void) => useSession.persist.onFinishHydration(onChange);

export function useSignedIn(): { hydrated: boolean; signedIn: boolean } {
  const hydrated = useSyncExternalStore(subscribeHydration, () => useSession.persist.hasHydrated(), () => false);
  const signedIn = useSession((state) => state.tokens !== null);
  return { hydrated, signedIn };
}
