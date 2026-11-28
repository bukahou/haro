"use client";

// 这个月的花费（概览、花费页共用）：合计、上限、还剩、进度、状态（后端算好的）。
import { useTranslations } from "next-intl";

import type { MonthSpending } from "@/api/generated/model";
import { Card, MoneyAmount, Note, Progress } from "@/components/ui";
import { useMoneyFormat } from "@/lib/format";

export function MonthSpendingCard({ month }: { month: MonthSpending }) {
  const t = useTranslations("spending");
  const money = useMoneyFormat();
  return (
    <Card title={t("month")}>
      <div className="flex flex-col gap-3">
        <MoneyAmount total={month.spent} large />
        {month.used_ratio !== null && <Progress ratio={month.used_ratio} warn={month.status !== "ok"} />}
        <p className="text-sm">
          {month.cap ? `${t("cap")} ${money(month.cap)}` : t("noCap")}
          {month.remaining && ` · ${t("remaining")} ${money(month.remaining)}`}
          {` · ${t(`status.${month.status}`)}`}
        </p>
        {month.can_raise && month.raise_amount && <Note>{t("canRaise", { amount: money(month.raise_amount) })}</Note>}
      </div>
    </Card>
  );
}
