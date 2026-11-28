"use client";

// 登录回来：服务端把令牌放在 # 后面（不发给服务器、不进日志），失败时是 ?oidc_error=<错误码>。
// 成功：存下令牌、把地址栏里的令牌擦掉、进聊天；失败：带着错误码回登录页，由登录页按码说明。
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useEffect } from "react";

import { useSession } from "@/auth/session";

export default function AuthCallback() {
  const t = useTranslations("callback");
  const router = useRouter();
  const signIn = useSession((state) => state.signIn);

  useEffect(() => {
    const fragment = new URLSearchParams(window.location.hash.slice(1));
    const accessToken = fragment.get("access_token");
    const refreshToken = fragment.get("refresh_token");
    window.history.replaceState(null, "", window.location.pathname); // 令牌不留在地址栏和历史记录里
    if (accessToken && refreshToken) {
      signIn({ accessToken, refreshToken });
      router.replace("/chat");
      return;
    }
    const code = new URLSearchParams(window.location.search).get("oidc_error") ?? "missing";
    router.replace(`/login?${new URLSearchParams({ error: code }).toString()}`);
  }, [router, signIn]);

  return <p className="p-6 text-muted">{t("working")}</p>;
}
