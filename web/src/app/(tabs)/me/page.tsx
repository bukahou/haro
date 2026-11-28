"use client";

// 「我的」：W1 先显示这个账号是谁、登出（证明登录、令牌、接口这一整条通了）；概览、花费、终端、管理员这些在 W2。
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";

import { logout } from "@/api/generated/auth/auth";
import { getMe } from "@/api/generated/me/me";
import type { Me } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { useSession } from "@/auth/session";

export default function MePage() {
  const t = useTranslations();
  const router = useRouter();
  const [me, setMe] = useState<Me | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  useEffect(() => {
    getMe().then(setMe, (error: unknown) => setFailure(problemMessage(error)));
  }, []);

  const signOut = async () => {
    const tokens = useSession.getState().tokens;
    if (tokens) {
      await logout({ refresh_token: tokens.refreshToken }).catch(() => undefined); // 服务端作废失败也照样在本机登出
    }
    useSession.getState().signOut();
    router.replace("/login");
  };

  return (
    <section className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">{t("me.title")}</h1>
      {failure && <p className="text-red-500">{t("common.failed", { message: failure })}</p>}
      {!me && !failure && <p className="text-muted">{t("common.loading")}</p>}
      {me && (
        <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 rounded-lg border border-border bg-panel p-4">
          <dt className="text-muted">{t("me.displayName")}</dt>
          <dd>{me.display_name}</dd>
          <dt className="text-muted">{t("me.role")}</dt>
          <dd>{t(`me.roles.${me.role}`)}</dd>
          <dt className="text-muted">{t("me.currency")}</dt>
          <dd>{me.default_currency}</dd>
        </dl>
      )}
      <button type="button" onClick={signOut} className="self-start rounded-lg border border-border px-4 py-2">
        {t("me.logout")}
      </button>
    </section>
  );
}
