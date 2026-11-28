"use client";

// 只做显示的格式化（数都是后端算好的）：金额按币种的写法、时间按这台设备的时区。
import { useFormatter } from "next-intl";

import type { Money } from "@/api/generated/model";


// 金额的符号用国际通用的写法（¥ 是日元、CN¥ 是人民币、$ 是美元），不随界面语言变：
// 按中文的写法日元会显示成 JP¥、人民币显示成 ¥，在日本用的家庭会看反。
const currencyFormats = new Map<string, Intl.NumberFormat>();

function currencyFormat(currency: string): Intl.NumberFormat {
  let found = currencyFormats.get(currency);
  if (!found) {
    found = new Intl.NumberFormat("en", { style: "currency", currency });
    currencyFormats.set(currency, found);
  }
  return found;
}

export function useMoneyFormat() {
  return (money: Money | null | undefined) => {
    if (!money) return "—";
    try {
      return currencyFormat(money.currency).format(Number(money.amount));
    } catch {
      return `${money.amount} ${money.currency}`; // 不认识的币种代码：照原样
    }
  };
}

const dayMillis = 24 * 60 * 60 * 1000;
const startOfDay = (at: Date) => new Date(at.getFullYear(), at.getMonth(), at.getDate()).getTime();

export function useTimeFormat() {
  const format = useFormatter();
  return {
    dateTime: (iso: string | null | undefined) =>
      iso ? format.dateTime(new Date(iso), { year: "numeric", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "—",
    // 相对于现在（「5 分钟前」）：现在要显式给，不让 next-intl 自己猜
    relative: (iso: string | null | undefined) => (iso ? format.relativeTime(new Date(iso), new Date()) : "—"),
    // 列表里的短时间：今天的只写时刻，一周内写星期，更早的写日期
    short: (iso: string | null | undefined) => {
      if (!iso) return "";
      const at = new Date(iso);
      const now = new Date();
      const days = (startOfDay(now) - startOfDay(at)) / dayMillis;
      if (days <= 0) return format.dateTime(at, { hour: "2-digit", minute: "2-digit" });
      if (days < 7) return format.dateTime(at, { weekday: "short" });
      return format.dateTime(at, { month: "numeric", day: "numeric" });
    },
  };
}
