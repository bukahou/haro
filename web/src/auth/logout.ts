"use client";

// 登出：先请服务端作废刷新令牌（失败也照样在本机登出），再清掉本机的令牌。
import { logout } from "@/api/generated/auth/auth";

import { useSession } from "./session";

export async function signOut() {
  const tokens = useSession.getState().tokens;
  if (tokens) {
    await logout({ refresh_token: tokens.refreshToken }).catch(() => undefined);
  }
  useSession.getState().signOut();
}
