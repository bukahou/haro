"use client";

// 「我的」：概览（后端一次算好：账号、这个月的花费、终端、记录、调查、管理员的待处理）+ 各页的入口。
// 宽屏时设置和管理员也在侧栏左下角的菜单里；手机上没有那个菜单，所以这里也列着。
import { BookOpen, FlaskConical, Home, KeyRound, MonitorSmartphone, Settings, Sparkles, User, Users } from "lucide-react";
import Link from "next/link";
import { useTranslations } from "next-intl";

import { getOverview } from "@/api/generated/me/me";
import { MonthSpendingCard } from "@/components/me/MonthSpendingCard";
import { Avatar, Badge, Button, Failure, Grid2, Group, ListRow, Loading, Page, SecTitle } from "@/components/ui";
import { useApi } from "@/lib/useApi";

const loadOverview = () => getOverview();

export default function MePage() {
  const t = useTranslations("me");
  const { data, error, loading } = useApi(loadOverview);
  return (
    <Page title={t("title")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          <div className="card hstack" style={{ gap: 14 }}>
            <Avatar name={data.account.display_name} large />
            <div className="grow">
              <div className="b text-lg">{data.account.display_name}</div>
              <div className="sub">{t(`roles.${data.account.role}`)}</div>
            </div>
            <Button small kind="plain" href="/me/settings" icon={Settings}>
              {t("menu.settings")}
            </Button>
          </div>
          <div className="mt-3">
            <Grid2>
              <MonthSpendingCard month={data.spending} href="/me/spending" />
              <Link className="card block text-ink no-underline" href="/me/terminals">
                <h2>{t("menu.terminals")}</h2>
                <div>
                  <span className="stat num">{data.terminals.online}</span> <span className="sub">{t("overview.online", { total: data.terminals.total })}</span>
                </div>
                {data.terminals.needs_upgrade > 0 ? (
                  <p className="wrn mt-1.5 text-sm">{t("overview.needsUpgrade", { count: data.terminals.needs_upgrade })}</p>
                ) : (
                  <p className="cap mt-1.5">{t("overview.upToDate")}</p>
                )}
              </Link>
            </Grid2>
          </div>
          <SecTitle>{t("records")}</SecTitle>
          <Group>
            <ListRow href="/chat/backstory" icon={BookOpen} title={t("menu.backstory")} sub={t("overview.backstory", { count: data.memory.background_items })} />
            <ListRow href="/me/memory" icon={Sparkles} title={t("menu.memory")} sub={t("overview.memory", { resident: data.memory.resident, notes: data.memory.notes })} />
            <ListRow href="/me/research" icon={FlaskConical} title={t("menu.research")} sub={t("overview.research", { count: data.research.running })} />
          </Group>
          <SecTitle>{t("aboutHome")}</SecTitle>
          <Group>
            <ListRow href="/me/profile" icon={User} tone="neutral" title={t("menu.profile")} sub={t("overview.profile")} />
            <ListRow href="/me/home" icon={Home} tone="neutral" title={t("menu.home")} sub={t("overview.home")} />
          </Group>
          {data.admin && (
            <>
              <SecTitle>{t("menu.admin")}</SecTitle>
              <Group>
                <ListRow href="/me/admin/accounts" icon={Users} tone="neutral" title={t("menu.accounts")} right={<Badge count={data.admin.pending_accounts} />} />
                <ListRow href="/me/admin/terminals" icon={MonitorSmartphone} tone="neutral" title={t("menu.pendingTerminals")} right={<Badge count={data.admin.pending_terminals} />} />
                <ListRow href="/me/admin/invites" icon={KeyRound} tone="neutral" title={t("menu.invites")} />
              </Group>
            </>
          )}
        </>
      )}
    </Page>
  );
}
