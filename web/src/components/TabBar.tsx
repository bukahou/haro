"use client";

// 底部的四个标签（和 iOS 一致）：聊天、日历、记账、我的。电脑上宽屏时也在底部（先简单，W2 起再按画面调）。
import { CalendarDays, MessageCircle, Receipt, User } from "lucide-react";
import { useTranslations } from "next-intl";
import Link from "next/link";
import { usePathname } from "next/navigation";
import clsx from "clsx";

const tabs = [
  { href: "/chat", key: "chat", icon: MessageCircle },
  { href: "/calendar", key: "calendar", icon: CalendarDays },
  { href: "/expenses", key: "expenses", icon: Receipt },
  { href: "/me", key: "me", icon: User },
] as const;

export function TabBar() {
  const t = useTranslations("tabs");
  const pathname = usePathname();
  return (
    <nav className="fixed inset-x-0 bottom-0 border-t border-border bg-panel pb-[env(safe-area-inset-bottom)]">
      <ul className="mx-auto flex max-w-3xl">
        {tabs.map(({ href, key, icon: Icon }) => {
          const active = pathname.startsWith(href);
          return (
            <li key={key} className="flex-1">
              <Link
                href={href}
                className={clsx("flex flex-col items-center gap-1 py-2 text-xs", active ? "text-accent" : "text-muted")}
                aria-current={active ? "page" : undefined}
              >
                <Icon size={22} aria-hidden />
                {t(key)}
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
