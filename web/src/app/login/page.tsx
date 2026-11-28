"use client";

// 登录页：跳到服务端的登录入口（服务端经 akasha 登录，网页不直接调 akasha），带着链接里的邀请码。
// 登录没成时（回调页带着 ?error=<错误码> 回来）按码说明（错误码是契约里的 OidcError，按码显示、不看文字）。
import { AlertTriangle, KeyRound, Sparkles } from "lucide-react";
import { useTranslations } from "next-intl";
import { useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";

import { loginURL } from "@/api/config";
import { Banner, Spinner } from "@/components/ui";

const knownErrors: readonly string[] = ["pending_approval", "account_disabled", "invite_invalid", "login_cancelled", "login_failed", "missing"];

function LoginPanel() {
  const t = useTranslations();
  const params = useSearchParams();
  const invite = params.get("invite");
  const error = params.get("error");
  const failure = error ? (knownErrors.includes(error) ? error : "login_failed") : null;
  const [leaving, setLeaving] = useState(false);
  return (
    <main className="flex min-h-dvh items-center justify-center p-6">
      <div className="card flex w-full max-w-[400px] flex-col gap-[18px]" style={{ padding: "32px 28px" }}>
        <div className="hstack" style={{ gap: 12 }}>
          <span className="avatar lg" style={{ background: "var(--accent)", color: "var(--accent-fg)" }}>
            <Sparkles size={24} aria-hidden />
          </span>
          <div>
            <h1 className="text-2xl font-semibold">haro</h1>
            <p className="sub">{t("login.subtitle")}</p>
          </div>
        </div>
        {failure && (
          <Banner tone={failure === "pending_approval" ? "warn" : "danger"} icon={AlertTriangle}>
            {t(`callback.errors.${failure}`)}
          </Banner>
        )}
        {invite && (
          <Banner tone="accent" icon={KeyRound}>
            <div className="b">{t("login.withInvite")}</div>
            <div className="num">{t("login.invite", { code: invite })}</div>
          </Banner>
        )}
        <p className="sub">{t("login.hint")}</p>
        <button
          type="button"
          className="btn solid block"
          style={{ height: 48 }}
          disabled={leaving}
          onClick={() => {
            setLeaving(true);
            window.location.assign(loginURL(invite));
          }}
        >
          {leaving && <Spinner />}
          {leaving ? t("login.leaving") : t("login.button")}
        </button>
        <p className="cap text-center">{t("login.via")}</p>
      </div>
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
