"use client";

// 记账：一个月的账目（合计、和上个月比、按分类筛、按天列），还没入账的小票在顶上提醒。合计、换算都是后端算好的。
import { BarChart3, Camera, Pencil, Receipt as ReceiptIcon } from "lucide-react";
import Link from "next/link";
import { useFormatter, useTranslations } from "next-intl";
import { useCallback, useMemo, useState } from "react";

import { listExpenses, listReceipts, summarizeExpenses } from "@/api/generated/expenses/expenses";
import type { Expense } from "@/api/generated/model";
import { MonthSwitch } from "@/components/expenses/month";
import { Banner, Button, Chip, Empty, Failure, Ico, Loading, MoneyAmount, Page } from "@/components/ui";
import { dateOfKey, dayKeyOf, timeOf } from "@/lib/calendarDays";
import { useMoneyFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

const loadReceipts = () => listReceipts();

function ExpenseRow({ expense }: { expense: Expense }) {
  const t = useTranslations("expenses");
  const money = useMoneyFormat();
  return (
    <Link className="row" href={`/expenses/${expense.expense_id}`}>
      <Ico icon={expense.has_image ? ReceiptIcon : Pencil} tone={expense.has_image ? "accent" : "neutral"} />
      <span className="grow">
        <span className="ell block">{expense.merchant}</span>
        <span className="cap">
          {expense.category?.name ?? t("uncategorized")} · {timeOf(expense.spent_at)}
          {expense.payment_method && ` · ${expense.payment_method}`}
        </span>
      </span>
      <span className="amount">{money(expense.total)}</span>
    </Link>
  );
}

function OpenReceipts() {
  const t = useTranslations("expenses");
  const { data } = useApi(loadReceipts);
  if (!data?.length) return null;
  const parsing = data.filter((receipt) => receipt.status === "parsing").length;
  const waiting = data.length - parsing;
  return (
    <div className="mb-3">
      <Banner tone="accent" icon={ReceiptIcon}>
        <div className="between flex-wrap">
          <span>{t("openReceipts", { parsing, waiting })}</span>
          <Button small href="/expenses/new">
            {t("review")}
          </Button>
        </div>
      </Banner>
    </div>
  );
}

export default function ExpensesPage() {
  const t = useTranslations("expenses");
  const format = useFormatter();
  const money = useMoneyFormat();
  const [month, setMonth] = useState<string | undefined>(undefined); // 不带 = 服务端的这个月
  const [category, setCategory] = useState<string | null>(null);
  const summary = useApi(useCallback(() => summarizeExpenses(month ? { month } : undefined), [month]));
  const shownMonth = month ?? summary.data?.month;
  const list = useApi(useCallback(() => listExpenses({ ...(month ? { month } : {}), ...(category ? { category } : {}) }), [month, category]));
  const days = useMemo(() => {
    const groups = new Map<string, Expense[]>();
    for (const expense of list.data ?? []) {
      const day = dayKeyOf(new Date(expense.spent_at));
      groups.set(day, [...(groups.get(day) ?? []), expense]);
    }
    return [...groups.entries()];
  }, [list.data]);
  return (
    <Page
      title={t("title")}
      actions={
        <>
          <Button small href="/expenses/stats" icon={BarChart3}>
            {t("stats")}
          </Button>
          <Button small kind="solid" href="/expenses/new" icon={Camera}>
            {t("new")}
          </Button>
        </>
      }
    >
      <OpenReceipts />
      <div className="card">
        <div className="between">
          {shownMonth ? (
            <MonthSwitch
              month={shownMonth}
              onChange={(next) => {
                setMonth(next);
                setCategory(null);
              }}
            />
          ) : (
            <span />
          )}
          <Button small kind="plain" href="/expenses/manual" icon={Pencil}>
            {t("manual")}
          </Button>
        </div>
        {summary.data && (
          <div className="mt-2">
            <MoneyAmount total={summary.data.total} large />
            <p className="cap">
              {t("count", { count: summary.data.count })} · {t("previous", { amount: money(summary.data.previous.total) })}
            </p>
          </div>
        )}
        <Failure message={summary.error} />
      </div>
      {summary.data && summary.data.categories.some((group) => group.key) && (
        <div className="wrap mt-3.5 mb-0.5">
          <Chip on={!category} onClick={() => setCategory(null)}>
            {t("all")}
          </Chip>
          {summary.data.categories
            .filter((group) => group.key)
            .map((group) => (
              <Chip key={group.key} on={category === group.key} onClick={() => setCategory(group.key ?? null)}>
                {group.name}
              </Chip>
            ))}
        </div>
      )}
      {list.loading && !list.data && (
        <div className="mt-3">
          <Loading />
        </div>
      )}
      <Failure message={list.error} />
      {list.data &&
        (days.length ? (
          days.map(([day, expenses]) => (
            <div key={day}>
              <div className="ledger-day">
                <span>{format.dateTime(dateOfKey(day), { month: "long", day: "numeric", weekday: "short" })}</span>
              </div>
              <div className="group">
                {expenses.map((expense) => (
                  <ExpenseRow key={expense.expense_id} expense={expense} />
                ))}
              </div>
            </div>
          ))
        ) : (
          <div className="mt-3">
            <Empty icon={ReceiptIcon}>
              <p>{t("empty")}</p>
              <div className="mt-3">
                <Button small href="/expenses/new">
                  {t("new")}
                </Button>
              </div>
            </Empty>
          </div>
        ))}
    </Page>
  );
}
