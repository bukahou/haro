"use client";

// 界面文字（next-intl）：先只有中文，文字都在 messages/ 里，每种语言一份。时间按看的这台设备的时区显示。
import { NextIntlClientProvider } from "next-intl";

import zh from "../../messages/zh.json";

export const interfaceLocale = "zh";

export function IntlProvider({ children }: { children: React.ReactNode }) {
  const timeZone = typeof Intl !== "undefined" ? Intl.DateTimeFormat().resolvedOptions().timeZone : "UTC";
  return (
    <NextIntlClientProvider locale={interfaceLocale} messages={zh} timeZone={timeZone}>
      {children}
    </NextIntlClientProvider>
  );
}
