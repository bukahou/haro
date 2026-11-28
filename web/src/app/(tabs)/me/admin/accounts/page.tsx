"use client";

// 管理员：账号（只看运维信息：状态、来源、这个月的用量、月上限、在线终端、最近用；看不到对话和资料）。
// 能做的按状态给：同意（在等的）、停用、恢复（也撤销删除）、删除（软删除，30 天后彻底删）、定月上限（档位来自 client-config）。
import { AlertTriangle } from "lucide-react";
import { useTranslations } from "next-intl";
import { useState } from "react";

import { approveAccount, deleteAccount, disableAccount, enableAccount, listAccounts, updateAccount } from "@/api/generated/admin/admin";
import type { AccountStatus, AccountSummary, Money } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { confirmDialog, showToast } from "@/components/overlay";
import { Avatar, Banner, Button, Chip, Empty, Failure, KV, KVRow, Loading, MoneyAmount, Page, Tag } from "@/components/ui";
import { useMoneyFormat, useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";
import { useAccount } from "@/stores/account";
import { useClientConfig } from "@/stores/clientConfig";

const loadAccounts = () => listAccounts();
const statusTone = { active: "accent", pending_approval: "warn", disabled: "neutral" } as const;
const moneyKey = (money: Money) => `${money.amount} ${money.currency}`;

function AccountCard({ account, reload }: { account: AccountSummary; reload: () => void }) {
  const t = useTranslations();
  const money = useMoneyFormat();
  const time = useTimeFormat();
  const capOptions = useClientConfig((state) => state.config?.monthly_cap_options ?? []);
  const [busy, setBusy] = useState(false);
  const id = account.account_id;
  const name = account.display_name || t("admin.accounts.anonymous");
  const admin = account.role === "admin";
  const done = () => {
    reload();
    void useAccount.getState().refresh(); // 侧栏上的红点
  };
  const run = async (work: () => Promise<unknown>, text: string) => {
    setBusy(true);
    try {
      await work();
      showToast(text);
      done();
    } catch (failure) {
      showToast(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="card m-0">
      <div className="between">
        <span className="hstack">
          <Avatar name={name} />
          <span>
            <span className="b block">{name}</span>
            <span className="cap">
              {admin ? `${t("me.roles.admin")} · ` : ""}
              {t(`admin.accounts.joinedVia.${account.joined_via}`)} · {t("admin.accounts.lastUsed")} {time.relative(account.last_used_at)}
            </span>
          </span>
        </span>
        <Tag tone={statusTone[account.status]}>{t(`admin.accounts.status.${account.status}`)}</Tag>
      </div>
      {account.deletion_scheduled_at && (
        <div className="mt-2.5">
          <Banner tone="danger" icon={AlertTriangle}>
            {t("admin.accounts.deletionAt", { time: time.dateTime(account.deletion_scheduled_at) })}
          </Banner>
        </div>
      )}
      <div className="mt-3">
        <KV>
          <KVRow label={t("admin.accounts.spent")}>
            <MoneyAmount total={account.spent_this_month} />
          </KVRow>
          <KVRow label={t("admin.accounts.cap")}>
            {admin ? (
              <span className="sub">{t("admin.accounts.adminCap")}</span>
            ) : (
              <select
                className="input"
                style={{ minHeight: 34, padding: "4px 8px", maxWidth: 180 }}
                value={account.cap ? moneyKey(account.cap) : ""}
                disabled={busy}
                aria-label={t("admin.accounts.cap")}
                onChange={(event) => {
                  const chosen = capOptions.find((option) => moneyKey(option) === event.target.value) ?? null;
                  void run(() => updateAccount(id, { monthly_cap: chosen }), chosen ? t("admin.accounts.capSet", { cap: money(chosen) }) : t("admin.accounts.capCleared"));
                }}
              >
                <option value="">{t("admin.accounts.noCap")}</option>
                {account.cap && !capOptions.some((option) => moneyKey(option) === moneyKey(account.cap as Money)) && (
                  <option value={moneyKey(account.cap)}>{money(account.cap)}</option>
                )}
                {capOptions.map((option) => (
                  <option key={moneyKey(option)} value={moneyKey(option)}>
                    {money(option)}
                  </option>
                ))}
              </select>
            )}
          </KVRow>
          <KVRow label={t("admin.accounts.terminalsOnline")}>{account.terminals_online}</KVRow>
        </KV>
      </div>
      {!admin && (
        <div className="actions">
          {account.status === "pending_approval" && (
            <Button small kind="solid" disabled={busy} onClick={() => void run(() => approveAccount(id), t("admin.accounts.approved", { name }))}>
              {t("admin.accounts.approve")}
            </Button>
          )}
          {account.status !== "disabled" ? (
            <Button
              small
              disabled={busy}
              onClick={() =>
                confirmDialog({
                  title: t("admin.accounts.disableTitle", { name }),
                  body: t("admin.accounts.disableBody"),
                  confirm: t("admin.accounts.disable"),
                  cancel: t("common.cancel"),
                  run: async () => {
                    await disableAccount(id);
                    showToast(t("admin.accounts.disabled"));
                    done();
                  },
                })
              }
            >
              {t("admin.accounts.disable")}
            </Button>
          ) : (
            <Button small disabled={busy} onClick={() => void run(() => enableAccount(id), t("admin.accounts.enabled", { name }))}>
              {account.deletion_scheduled_at ? t("admin.accounts.undoDelete") : t("admin.accounts.enable")}
            </Button>
          )}
          {!account.deletion_scheduled_at && (
            <Button
              small
              kind="danger"
              disabled={busy}
              onClick={() =>
                confirmDialog({
                  title: t("admin.accounts.deleteTitle", { name }),
                  body: t("admin.accounts.deleteConfirm"),
                  confirm: t("admin.accounts.delete"),
                  cancel: t("common.cancel"),
                  run: async () => {
                    await deleteAccount(id);
                    showToast(t("admin.accounts.deleted"));
                    done();
                  },
                })
              }
            >
              {t("admin.accounts.delete")}
            </Button>
          )}
        </div>
      )}
    </div>
  );
}

export default function AccountsPage() {
  const t = useTranslations("admin.accounts");
  const { data, error, loading, reload } = useApi(loadAccounts);
  const [filter, setFilter] = useState<AccountStatus | "">("");
  const shown = data?.filter((account) => !filter || account.status === filter) ?? [];
  const waiting = data?.filter((account) => account.status === "pending_approval").length ?? 0;
  const filters: { value: AccountStatus | ""; label: string }[] = [
    { value: "", label: t("all") },
    { value: "pending_approval", label: waiting ? `${t("status.pending_approval")} ${waiting}` : t("status.pending_approval") },
    { value: "active", label: t("status.active") },
    { value: "disabled", label: t("status.disabled") },
  ];
  return (
    <Page title={t("title")} back="/me" sub={t("hint")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          <div className="wrap mb-3">
            {filters.map((option) => (
              <Chip key={option.value} on={filter === option.value} onClick={() => setFilter(option.value)}>
                {option.label}
              </Chip>
            ))}
          </div>
          {shown.length ? (
            <div className="stack">
              {shown.map((account) => (
                <AccountCard key={account.account_id} account={account} reload={reload} />
              ))}
            </div>
          ) : (
            <Empty>{t("none")}</Empty>
          )}
        </>
      )}
    </Page>
  );
}
