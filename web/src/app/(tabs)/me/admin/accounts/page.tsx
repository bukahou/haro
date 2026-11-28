"use client";

// 管理员：账号（运维信息：状态、来源、这个月的用量、月上限、在线终端、最近用；看不到内容）。
// 能做的按状态给：同意（在等的）、停用、恢复（也撤销删除）、删除（软删除，30 天后彻底删）、定月上限（档位来自 client-config）。
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import {
  approveAccount,
  deleteAccount,
  disableAccount,
  enableAccount,
  listAccounts,
  updateAccount,
} from "@/api/generated/admin/admin";
import { getClientConfig } from "@/api/generated/me/me";
import type { AccountSummary, Money } from "@/api/generated/model";
import { Button, Card, Failure, Loading, MoneyAmount, Page, Row, Rows } from "@/components/ui";
import { useMoneyFormat, useTimeFormat } from "@/lib/format";
import { useAction, useApi } from "@/lib/useApi";

const loadAccounts = () => listAccounts();
const loadConfig = () => getClientConfig();

function AccountCard({ account, capOptions, reload }: { account: AccountSummary; capOptions: Money[]; reload: () => void }) {
  const t = useTranslations("admin.accounts");
  const tc = useTranslations("common");
  const money = useMoneyFormat();
  const time = useTimeFormat();
  const [deleting, setDeleting] = useState(false);
  const id = account.account_id;
  const approve = useAction(useCallback(() => approveAccount(id), [id]), reload);
  const disable = useAction(useCallback(() => disableAccount(id), [id]), reload);
  const enable = useAction(useCallback(() => enableAccount(id), [id]), reload);
  const remove = useAction(useCallback(() => deleteAccount(id), [id]), reload);
  const setCap = useAction(useCallback((cap: Money | null) => updateAccount(id, { monthly_cap: cap }), [id]), reload);
  const busy = approve.busy || disable.busy || enable.busy || remove.busy || setCap.busy;
  const admin = account.role === "admin";
  return (
    <Card>
      <div className="flex flex-col gap-3">
        <div className="flex items-baseline justify-between gap-3">
          <span className="font-medium">{account.display_name || t("anonymous")}</span>
          <span className="text-sm text-muted">
            {t(`status.${account.status}`)} · {t(`joinedVia.${account.joined_via}`)}
          </span>
        </div>
        {account.deletion_scheduled_at && (
          <p className="text-sm text-red-500">{t("deletionAt", { time: time.dateTime(account.deletion_scheduled_at) })}</p>
        )}
        <Rows>
          <Row label={t("spent")}>
            <MoneyAmount total={account.spent_this_month} />
          </Row>
          {!admin && (
            <Row label={t("cap")}>
              <select
                className="rounded-lg border border-border bg-surface px-2 py-1"
                value={account.cap ? `${account.cap.amount} ${account.cap.currency}` : ""}
                disabled={busy}
                onChange={(e) => {
                  const chosen = capOptions.find((option) => `${option.amount} ${option.currency}` === e.target.value) ?? null;
                  void setCap.act(chosen);
                }}
              >
                <option value="">{t("noCap")}</option>
                {account.cap && !capOptions.some((o) => o.amount === account.cap?.amount && o.currency === account.cap?.currency) && (
                  <option value={`${account.cap.amount} ${account.cap.currency}`}>{money(account.cap)}</option>
                )}
                {capOptions.map((option) => (
                  <option key={`${option.amount} ${option.currency}`} value={`${option.amount} ${option.currency}`}>
                    {money(option)}
                  </option>
                ))}
              </select>
            </Row>
          )}
          <Row label={t("terminalsOnline")}>{account.terminals_online}</Row>
          <Row label={t("lastUsed")}>{time.relative(account.last_used_at)}</Row>
        </Rows>
        {!admin &&
          (deleting ? (
            <div className="flex flex-col gap-2">
              <p className="text-sm">{t("deleteConfirm")}</p>
              <div className="flex gap-2">
                <Button kind="danger" disabled={busy} onClick={() => void remove.act().then(() => setDeleting(false))}>
                  {tc("confirm")}
                </Button>
                <Button onClick={() => setDeleting(false)}>{tc("cancel")}</Button>
              </div>
            </div>
          ) : (
            <div className="flex flex-wrap gap-2">
              {account.status === "pending_approval" && (
                <Button kind="primary" disabled={busy} onClick={() => void approve.act()}>
                  {t("approve")}
                </Button>
              )}
              {account.status !== "disabled" && (
                <Button disabled={busy} onClick={() => void disable.act()}>
                  {t("disable")}
                </Button>
              )}
              {account.status === "disabled" && (
                <Button disabled={busy} onClick={() => void enable.act()}>
                  {t("enable")}
                </Button>
              )}
              {!account.deletion_scheduled_at && (
                <Button kind="danger" disabled={busy} onClick={() => setDeleting(true)}>
                  {t("delete")}
                </Button>
              )}
            </div>
          ))}
        <Failure message={approve.error ?? disable.error ?? enable.error ?? remove.error ?? setCap.error} />
      </div>
    </Card>
  );
}

export default function AccountsPage() {
  const t = useTranslations("admin.accounts");
  const accounts = useApi(loadAccounts);
  const config = useApi(loadConfig);
  return (
    <Page title={t("title")} back="/me">
      {accounts.loading && <Loading />}
      <Failure message={accounts.error} />
      {accounts.data?.map((account) => (
        <AccountCard key={account.account_id} account={account} capOptions={config.data?.monthly_cap_options ?? []} reload={accounts.reload} />
      ))}
    </Page>
  );
}
