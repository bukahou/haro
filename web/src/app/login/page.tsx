"use client";

// 登录页：跳到服务端的登录入口（服务端经 akasha 登录，网页不直接调 akasha），带着链接里的邀请码。
// 登录没成时（回调页带着 ?error=<错误码> 回来）按码说明（错误码是契约里的 OidcError，按码显示、不看文字）。
import { useTranslations } from "next-intl";
import { useSearchParams } from "next/navigation";
import { Suspense } from "react";

import { loginURL } from "@/api/config";

const knownErrors: readonly string[] = ["pending_approval", "account_disabled", "invite_invalid", "login_cancelled", "login_failed", "missing"];

function LoginPanel() {
  const t = useTranslations();
  const params = useSearchParams();
  const invite = params.get("invite");
  const error = params.get("error");
  const failure = error ? (knownErrors.includes(error) ? error : "login_failed") : null;
  return (
    <main className="mx-auto flex min-h-dvh max-w-sm flex-col justify-center gap-6 px-6">
      <div>
        <h1 className="text-2xl font-semibold">{t("login.title")}</h1>
        <p className="mt-1 text-muted">{t("login.subtitle")}</p>
      </div>
      {failure && <p className="rounded-lg border border-border bg-panel p-3 text-sm">{t(`callback.errors.${failure}`)}</p>}
      {invite && <p className="text-sm text-muted">{t("login.invite", { code: invite })}</p>}
      <button
        type="button"
        className="rounded-lg bg-accent px-4 py-3 font-medium text-white"
        onClick={() => window.location.assign(loginURL(invite))}
      >
        {t("login.button")}
      </button>
    </main>
  );
}

export default function LoginPage() {
  return (
    <Suspense>
      <LoginPanel />
    </Suspense>
  );
}
