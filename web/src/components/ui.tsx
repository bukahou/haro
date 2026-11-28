"use client";

// 画面里共用的几样：页面（标题 + 返回）、卡片、一行一行的说明、按钮、提示、金额合计、进度条。颜色都用 globals.css 里的变量。
import clsx from "clsx";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { useTranslations } from "next-intl";
import Link from "next/link";

import type { MoneyTotal } from "@/api/generated/model";
import { useMoneyFormat } from "@/lib/format";

export function Page({ title, back, children }: { title: string; back?: string; children: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-4">
      <header className="flex items-center gap-2">
        {back && (
          <Link href={back} className="-ml-2 p-1 text-muted" aria-label="back">
            <ChevronLeft size={22} />
          </Link>
        )}
        <h1 className="text-xl font-semibold">{title}</h1>
      </header>
      {children}
    </section>
  );
}

export function Card({ title, children, className }: { title?: string; children: React.ReactNode; className?: string }) {
  return (
    <div className={clsx("rounded-xl border border-border bg-panel p-4", className)}>
      {title && <h2 className="mb-3 text-sm font-medium text-muted">{title}</h2>}
      {children}
    </div>
  );
}

export function Rows({ children }: { children: React.ReactNode }) {
  return <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2">{children}</dl>;
}

export function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  );
}

export function LinkRow({ href, label, note }: { href: string; label: string; note?: React.ReactNode }) {
  return (
    <Link href={href} className="flex items-center justify-between gap-3 py-3">
      <span>{label}</span>
      <span className="flex items-center gap-2 text-sm text-muted">
        {note}
        <ChevronRight size={18} />
      </span>
    </Link>
  );
}

export function Button({
  children,
  onClick,
  disabled,
  kind = "plain",
  type = "button",
}: {
  children: React.ReactNode;
  onClick?: () => void;
  disabled?: boolean;
  kind?: "primary" | "plain" | "danger";
  type?: "button" | "submit";
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={clsx(
        "rounded-lg px-3 py-2 text-sm disabled:opacity-50",
        kind === "primary" && "bg-accent font-medium text-white",
        kind === "plain" && "border border-border",
        kind === "danger" && "border border-red-500 text-red-500",
      )}
    >
      {children}
    </button>
  );
}

export function Loading() {
  const t = useTranslations("common");
  return <p className="text-muted">{t("loading")}</p>;
}

export function Failure({ message }: { message: string | null }) {
  const t = useTranslations("common");
  return message ? <p className="text-sm text-red-500">{t("failed", { message })}</p> : null;
}

export function Note({ children }: { children: React.ReactNode }) {
  return <p className="text-sm text-muted">{children}</p>;
}

// MoneyAmount 是一笔合计：换成默认货币的总数，按币种分开的原始金额（多于一种时）和换算说明（后端写好的）。
export function MoneyAmount({ total, large }: { total: MoneyTotal; large?: boolean }) {
  const money = useMoneyFormat();
  return (
    <div>
      <p className={large ? "text-2xl font-semibold" : undefined}>{money(total.total)}</p>
      {total.by_currency.length > 1 && <p className="text-xs text-muted">{total.by_currency.map(money).join(" + ")}</p>}
      {total.conversion_note && <p className="text-xs text-muted">{total.conversion_note}</p>}
    </div>
  );
}

export function Progress({ ratio, warn }: { ratio: number; warn?: boolean }) {
  return (
    <div className="h-2 overflow-hidden rounded-full bg-border">
      <div className={clsx("h-full", warn ? "bg-red-500" : "bg-accent")} style={{ width: `${Math.round(Math.min(ratio, 1) * 100)}%` }} />
    </div>
  );
}
