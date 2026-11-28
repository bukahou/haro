"use client";

// 只做显示的格式化（数都是后端算好的）：金额按币种的写法、时间按这台设备的时区。
import { useFormatter } from "next-intl";

import type { Money } from "@/api/generated/model";

export function useMoneyFormat() {
  const format = useFormatter();
  return (money: Money | null | undefined) =>
    money ? format.number(Number(money.amount), { style: "currency", currency: money.currency }) : "—";
}

export function useTimeFormat() {
  const format = useFormatter();
  return {
    dateTime: (iso: string | null | undefined) =>
      iso ? format.dateTime(new Date(iso), { year: "numeric", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "—",
    // 相对于现在（「5 分钟前」）：现在要显式给，不让 next-intl 自己猜
    relative: (iso: string | null | undefined) => (iso ? format.relativeTime(new Date(iso), new Date()) : "—"),
  };
}
