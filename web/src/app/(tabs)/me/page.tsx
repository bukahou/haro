"use client";

// 「我的」：概览（后端一次算好：账号、终端、记录、调查、这个月的花费、管理员的待处理）+ 各个设置页的入口 + 登出。
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";

import { logout } from "@/api/generated/auth/auth";
import { getOverview } from "@/api/generated/me/me";
import { useSession } from "@/auth/session";
import { MonthSpendingCard } from "@/components/me/MonthSpendingCard";
import { Button, Card, Failure, LinkRow, Loading, Note, Page } from "@/components/ui";
import { useApi } from "@/lib/useApi";

const loadOverview = () => getOverview();

export default function MePage() {
  const t = useTranslations();
  const router = useRouter();
  const { data, error, loading } = useApi(loadOverview);

  const signOut = async () => {
    const tokens = useSession.getState().tokens;
    if (tokens) {
      await logout({ refresh_token: tokens.refreshToken }).catch(() => undefined); // 服务端作废失败也照样在本机登出
    }
    useSession.getState().signOut();
    router.replace("/login");
  };

  return (
    <Page title={t("me.title")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          <Card>
            <p className="text-lg font-medium">{data.account.display_name}</p>
            <Note>{t(`me.roles.${data.account.role}`)}</Note>
          </Card>
          <MonthSpendingCard month={data.spending} />
          <Card>
            <div className="flex flex-col gap-1 text-sm">
              <p>
                {t("me.overview.terminals", { online: data.terminals.online, total: data.terminals.total })}
                {data.terminals.needs_upgrade > 0 && ` · ${t("me.overview.needsUpgrade", { count: data.terminals.needs_upgrade })}`}
              </p>
              <p>
                {t("me.overview.memory", {
                  background: data.memory.background_items,
                  resident: data.memory.resident,
                  notes: data.memory.notes,
                })}
              </p>
              <p>{t("me.overview.research", { count: data.research.running })}</p>
            </div>
          </Card>
          <Card>
            <div className="divide-y divide-border">
              <LinkRow href="/me/spending" label={t("me.menu.spending")} />
              <LinkRow href="/me/terminals" label={t("me.menu.terminals")} />
              <LinkRow href="/me/profile" label={t("me.menu.profile")} />
              <LinkRow href="/me/home" label={t("me.menu.home")} />
              <LinkRow href="/me/settings" label={t("me.menu.settings")} />
            </div>
          </Card>
          {data.admin && (
            <Card title={t("me.menu.admin")}>
              <Note>{t("me.overview.pending", { terminals: data.admin.pending_terminals, accounts: data.admin.pending_accounts })}</Note>
              <div className="mt-1 divide-y divide-border">
                <LinkRow href="/me/admin/accounts" label={t("me.menu.accounts")} note={data.admin.pending_accounts || undefined} />
                <LinkRow href="/me/admin/terminals" label={t("me.menu.pendingTerminals")} note={data.admin.pending_terminals || undefined} />
                <LinkRow href="/me/admin/invites" label={t("me.menu.invites")} />
              </div>
            </Card>
          )}
        </>
      )}
      <div>
        <Button onClick={signOut}>{t("me.logout")}</Button>
      </div>
    </Page>
  );
}
