"use client";

// 重复：App 里新建 / 改时给的几种都是 RFC 5545 只带 FREQ 的规则 —— 星期几、几号、几月几日都跟着开始那天走（标准规定的），
// 所以这几个是定值，不按日期拼字符串。别的规则（语音里定的「每周一三五」之类）原样留着，显示成「按设定的规则重复」。
import { useTranslations } from "next-intl";

import type { Occurrence } from "@/api/generated/model";

// 规则 → 文字的键
const choiceKeys = { "FREQ=DAILY": "daily", "FREQ=WEEKLY": "weekly", "FREQ=MONTHLY": "monthly", "FREQ=YEARLY": "yearly" } as const;
export type RepeatChoice = keyof typeof choiceKeys;
export const repeatChoices = Object.keys(choiceKeys) as RepeatChoice[];

const isChoice = (rule: string): rule is RepeatChoice => rule in choiceKeys;

export function useRepeatLabel() {
  const t = useTranslations("calendar.repeat");
  return (rule: string | null) => {
    if (!rule) return t("none");
    return isChoice(rule) ? t(choiceKeys[rule]) : t("custom");
  };
}

export function isCustomRule(rule: string | null): boolean {
  return !!rule && !isChoice(rule);
}

// 一次日程的详情页地址（重复的带上是哪一次：改、删「只这一次 / 这次和以后」时要）
export function occurrenceHref(occurrence: Occurrence) {
  const query = new URLSearchParams({ occurrence: occurrence.start });
  return `/calendar/event/${encodeURIComponent(occurrence.event.event_id)}?${query.toString()}`;
}
