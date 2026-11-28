"use client";

// 这个月的花费（概览、花费页共用）：合计、上限、还剩、进度、状态（都是后端算好的）。概览里整张卡是去花费页的链接。
import Link from "next/link";
import { useTranslations } from "next-intl";

import type { MonthSpending } from "@/api/generated/model";
import { Bar, MoneyAmount, Tag } from "@/components/ui";
import { useMoneyFormat } from "@/lib/format";

const statusTone = { ok: "accent", near: "warn", reached: "danger" } as const;

export function MonthSpendingCard({ month, href }: { month: MonthSpending; href?: string }) {
  const t = useTranslations("spending");
  const money = useMoneyFormat();
  const body = (
    <>
      <div className="between">
        <h2 className="m-0">{t("month")}</h2>
        <Tag tone={statusTone[month.status]}>{t(`status.${month.status}`)}</Tag>
      </div>
      <div className="mt-2 mb-2.5 flex flex-wrap items-baseline gap-x-2">
        <MoneyAmount total={month.spent} large />
        <span className="sub">
          {month.cap ? `/ ${t("cap")} ${money(month.cap)}` : t("noCap")}
          {month.remaining && ` · ${t("remaining")} ${money(month.remaining)}`}
        </span>
      </div>
      {month.used_ratio !== null && <Bar ratio={month.used_ratio} warn={month.status !== "ok"} />}
      {month.can_raise && month.raise_amount && <p className="cap mt-2">{t("canRaise", { amount: money(month.raise_amount) })}</p>}
    </>
  );
  return href ? (
    <Link className="card block text-ink no-underline" href={href}>
      {body}
    </Link>
  ) : (
    <div className="card">{body}</div>
  );
}
