"use client";

// 设置与关于：名字、默认货币（后端校验）、外观（这台浏览器自己的）、版本（服务端、接口、网页）、登出。
import { LogOut } from "lucide-react";
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useState } from "react";

import { getMe, updateMe } from "@/api/generated/me/me";
import type { Me } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { signOut } from "@/auth/logout";
import { confirmDialog, showToast } from "@/components/overlay";
import { Button, Card, Failure, Field, KV, KVRow, Loading, Page, Seg } from "@/components/ui";
import { useApi } from "@/lib/useApi";
import { useAccount } from "@/stores/account";
import { useClientConfig } from "@/stores/clientConfig";
import { useUI, type Theme } from "@/stores/ui";

const loadMe = () => getMe();

function AccountForm({ me, reload }: { me: Me; reload: () => void }) {
  const t = useTranslations();
  const maxName = useClientConfig((state) => state.config?.limits.display_name_max_chars);
  const [name, setName] = useState(me.display_name);
  const [currency, setCurrency] = useState(me.default_currency);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    try {
      await updateMe({ display_name: name.trim(), default_currency: currency.trim().toUpperCase() });
      showToast(t("common.saved"));
      reload();
      void useAccount.getState().refresh(); // 侧栏上的名字
    } catch (failure) {
      showToast(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="card stack"
      style={{ gap: 14 }}
      onSubmit={(event) => {
        event.preventDefault();
        void save();
      }}
    >
      <h2 className="m-0">{t("settings.account")}</h2>
      <div className="form-grid">
        <Field label={t("settings.displayName")}>
          <input className="input" value={name} maxLength={maxName} onChange={(event) => setName(event.target.value)} />
        </Field>
        <Field label={t("settings.currency")} note={t("settings.currencyHint")}>
          <input className="input uppercase" value={currency} maxLength={3} onChange={(event) => setCurrency(event.target.value)} />
        </Field>
      </div>
      <div>
        <Button small kind="solid" type="submit" disabled={busy || !name.trim()}>
          {t("common.save")}
        </Button>
      </div>
    </form>
  );
}

export default function SettingsPage() {
  const t = useTranslations();
  const router = useRouter();
  const me = useApi(loadMe);
  const config = useClientConfig((state) => state.config);
  const theme = useUI((state) => state.theme);
  const setTheme = useUI((state) => state.setTheme);
  return (
    <Page title={t("settings.title")} back="/me">
      {me.loading && <Loading />}
      <Failure message={me.error} />
      {me.data && <AccountForm key={me.data.display_name + me.data.default_currency} me={me.data} reload={me.reload} />}
      <Card title={t("settings.appearance")}>
        <Seg<Theme>
          value={theme}
          onChange={setTheme}
          options={[
            { value: "system", label: t("settings.themeSystem") },
            { value: "light", label: t("settings.themeLight") },
            { value: "dark", label: t("settings.themeDark") },
          ]}
        />
        <p className="cap mt-2.5">{t("settings.appearanceHint")}</p>
      </Card>
      <Card title={t("settings.about")}>
        <KV>
          <KVRow label={t("settings.serverVersion")}>
            <span className="num">{config?.server_version ?? "—"}</span>
          </KVRow>
          <KVRow label={t("settings.apiVersion")}>
            <span className="num">{config?.api_version ?? "—"}</span>
          </KVRow>
          <KVRow label={t("settings.webVersion")}>
            <span className="num">{process.env.NEXT_PUBLIC_BUILD_SHA ?? "dev"}</span>
          </KVRow>
        </KV>
      </Card>
      <div className="actions">
        <Button
          kind="danger"
          icon={LogOut}
          onClick={() =>
            confirmDialog({
              title: t("me.logoutConfirm.title"),
              body: t("me.logoutConfirm.body"),
              confirm: t("me.logout"),
              cancel: t("common.cancel"),
              run: async () => {
                await signOut();
                router.replace("/login");
              },
            })
          }
        >
          {t("me.logout")}
        </Button>
      </div>
    </Page>
  );
}
