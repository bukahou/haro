"use client";

// 月份（YYYY-MM，按服务端的时区，由汇总接口告诉现在是哪个月）的前后翻。
import { ChevronLeft, ChevronRight } from "lucide-react";
import { useFormatter, useTranslations } from "next-intl";

import { IconButton } from "@/components/ui";

export function shiftMonth(month: string, delta: number): string {
  const [year, number] = month.split("-").map(Number);
  const date = new Date(year, number - 1 + delta, 1);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}`;
}

export function MonthSwitch({ month, onChange }: { month: string; onChange: (month: string) => void }) {
  const t = useTranslations("expenses");
  const format = useFormatter();
  const [year, number] = month.split("-").map(Number);
  return (
    <div className="hstack">
      <IconButton icon={ChevronLeft} label={t("prevMonth")} onClick={() => onChange(shiftMonth(month, -1))} />
      <span className="b num min-w-24 text-center">{format.dateTime(new Date(year, number - 1, 1), { year: "numeric", month: "long" })}</span>
      <IconButton icon={ChevronRight} label={t("nextMonth")} onClick={() => onChange(shiftMonth(month, 1))} />
    </div>
  );
}
