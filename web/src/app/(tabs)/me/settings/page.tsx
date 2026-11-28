"use client";

// 设置与关于：名字、默认货币（后端校验），版本（服务端、接口、网页）。
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import { getClientConfig, getMe, updateMe } from "@/api/generated/me/me";
import type { Me } from "@/api/generated/model";
import { Button, Card, Failure, Loading, Page, Row, Rows } from "@/components/ui";
import { useAction, useApi } from "@/lib/useApi";

const loadMe = () => getMe();
const loadConfig = () => getClientConfig();

function AccountForm({ me, reload }: { me: Me; reload: () => void }) {
  const t = useTranslations("settings");
  const tc = useTranslations("common");
  const [name, setName] = useState(me.display_name);
  const [currency, setCurrency] = useState(me.default_currency);
  const [saved, setSaved] = useState(false);
  const save = useAction(
    useCallback(async () => {
      await updateMe({ display_name: name.trim(), default_currency: currency.trim().toUpperCase() });
      setSaved(true);
    }, [name, currency]),
    reload,
  );
  return (
    <Card>
      <form
        className="flex flex-col gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          void save.act();
        }}
      >
        <label className="flex flex-col gap-1">
          <span className="text-sm text-muted">{t("displayName")}</span>
          <input className="rounded-lg border border-border bg-surface px-3 py-2" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="flex flex-col gap-1">
          <span className="text-sm text-muted">{t("currency")}</span>
          <input
            className="rounded-lg border border-border bg-surface px-3 py-2 uppercase"
            value={currency}
            maxLength={3}
            onChange={(e) => setCurrency(e.target.value)}
          />
          <span className="text-xs text-muted">{t("currencyHint")}</span>
        </label>
        <div className="flex items-center gap-3">
          <Button type="submit" kind="primary" disabled={save.busy}>
            {tc("save")}
          </Button>
          {saved && <span className="text-sm text-green-600">{tc("saved")}</span>}
        </div>
        <Failure message={save.error} />
      </form>
    </Card>
  );
}

export default function SettingsPage() {
  const t = useTranslations("settings");
  const me = useApi(loadMe);
  const config = useApi(loadConfig);
  return (
    <Page title={t("title")} back="/me">
      {me.loading && <Loading />}
      <Failure message={me.error} />
      {me.data && <AccountForm key={me.data.display_name + me.data.default_currency} me={me.data} reload={me.reload} />}
      <Card title={t("about")}>
        <Rows>
          <Row label={t("serverVersion")}>{config.data?.server_version ?? "—"}</Row>
          <Row label={t("apiVersion")}>{config.data?.api_version ?? "—"}</Row>
          <Row label={t("webVersion")}>{process.env.NEXT_PUBLIC_BUILD_SHA ?? "dev"}</Row>
        </Rows>
        <Failure message={config.error} />
      </Card>
    </Page>
  );
}
