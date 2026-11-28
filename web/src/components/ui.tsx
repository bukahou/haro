"use client";

// 画面里共用的部件。样式都在 haro.css（类名 = 那里的组件），这里只管结构，页面里不再各写一套颜色和间距。
import clsx from "clsx";
import { AlertTriangle, ChevronLeft, ChevronRight, type LucideIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import Link from "next/link";

import type { MoneyTotal } from "@/api/generated/model";
import { useMoneyFormat } from "@/lib/format";

import type { ButtonKind } from "./overlay";

// ---------- 页面 ----------

// Page 是一页的内容区：页头（返回、标题、右边的操作）、一句说明、内容。wide = 日历这类要更宽的页。
export function Page({
  title,
  back,
  actions,
  sub,
  wide,
  children,
}: {
  title: string;
  back?: string;
  actions?: React.ReactNode;
  sub?: React.ReactNode;
  wide?: boolean;
  children: React.ReactNode;
}) {
  const t = useTranslations("common");
  return (
    <div className={clsx("content", wide && "wide")}>
      <div className="page-head">
        {back && (
          <Link className="back" href={back} aria-label={t("back")}>
            <ChevronLeft size={22} />
          </Link>
        )}
        <h1>{title}</h1>
        {actions}
      </div>
      {sub && <p className="sub page-sub">{sub}</p>}
      {children}
    </div>
  );
}

export function SecTitle({ children }: { children: React.ReactNode }) {
  return <div className="sec-title">{children}</div>;
}

// ---------- 卡片、列表 ----------

export function Card({ title, children, className }: { title?: React.ReactNode; children: React.ReactNode; className?: string }) {
  return (
    <div className={clsx("card", className)}>
      {title && <h2>{title}</h2>}
      {children}
    </div>
  );
}

export function Group({ children, className }: { children: React.ReactNode; className?: string }) {
  return <div className={clsx("group", className)}>{children}</div>;
}

export type IcoTone = "accent" | "neutral" | "danger";

export function Ico({ icon: Icon, tone = "accent", size = 18 }: { icon: LucideIcon; tone?: IcoTone; size?: number }) {
  return (
    <span className={clsx("ico", tone === "neutral" && "n", tone === "danger" && "d")}>
      <Icon size={size} aria-hidden />
    </span>
  );
}

// ListRow 是列表里的一行：左边图标、中间标题和一行说明、右边补充；给 href 是链接（带箭头），给 onClick 是按钮。
export function ListRow({
  href,
  onClick,
  icon,
  tone,
  title,
  sub,
  right,
  top,
}: {
  href?: string;
  onClick?: () => void;
  icon?: LucideIcon;
  tone?: IcoTone;
  title: React.ReactNode;
  sub?: React.ReactNode;
  right?: React.ReactNode;
  top?: boolean; // 内容高的行：上对齐
}) {
  const inner = (
    <>
      {icon && <Ico icon={icon} tone={tone} />}
      <span className="grow">
        <span className="block">{title}</span>
        {sub && <span className="cap block">{sub}</span>}
      </span>
      {right}
      {href && (
        <span className="chev">
          <ChevronRight size={18} />
        </span>
      )}
    </>
  );
  const style = top ? { alignItems: "flex-start" } : undefined;
  if (href) {
    return (
      <Link className="row" href={href} style={style}>
        {inner}
      </Link>
    );
  }
  if (onClick) {
    return (
      <button type="button" className="row" onClick={onClick} style={style}>
        {inner}
      </button>
    );
  }
  return (
    <div className="row" style={{ ...style, cursor: "default" }}>
      {inner}
    </div>
  );
}

// KV 是两列的「名称 — 内容」。
export function KV({ children }: { children: React.ReactNode }) {
  return <dl className="kv">{children}</dl>;
}

export function KVRow({ label, children }: { label: React.ReactNode; children: React.ReactNode }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </>
  );
}

export function Avatar({ name, large }: { name: string; large?: boolean }) {
  return <span className={clsx("avatar", large && "lg")}>{[...name][0] ?? "?"}</span>;
}

export function Grid2({ children }: { children: React.ReactNode }) {
  return <div className="grid2">{children}</div>;
}

// ---------- 按钮、输入 ----------

export function Button({
  children,
  onClick,
  href,
  disabled,
  kind = "outline",
  small,
  block,
  type = "button",
  icon: Icon,
}: {
  children: React.ReactNode;
  onClick?: () => void;
  href?: string;
  disabled?: boolean;
  kind?: ButtonKind;
  small?: boolean;
  block?: boolean;
  type?: "button" | "submit";
  icon?: LucideIcon;
}) {
  const className = clsx("btn", kind !== "outline" && kind, small && "sm", block && "block");
  const inner = (
    <>
      {Icon && <Icon size={16} aria-hidden />}
      {children}
    </>
  );
  if (href) {
    return (
      <Link className={className} href={href}>
        {inner}
      </Link>
    );
  }
  return (
    <button type={type} className={className} onClick={onClick} disabled={disabled}>
      {inner}
    </button>
  );
}

export function IconButton({
  icon: Icon,
  label,
  onClick,
  href,
  muted,
  size = 20,
}: {
  icon: LucideIcon;
  label: string;
  onClick?: () => void;
  href?: string;
  muted?: boolean;
  size?: number;
}) {
  const className = clsx("icon-btn", muted && "mut");
  if (href) {
    return (
      <Link className={className} href={href} aria-label={label} title={label}>
        <Icon size={size} />
      </Link>
    );
  }
  return (
    <button type="button" className={className} onClick={onClick} aria-label={label} title={label}>
      <Icon size={size} />
    </button>
  );
}

export function Chip({ on, onClick, children }: { on?: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button type="button" className={clsx("chip", on && "on")} onClick={onClick} aria-pressed={on}>
      {children}
    </button>
  );
}

export function Seg<T extends string>({ value, options, onChange }: { value: T; options: { value: T; label: string }[]; onChange: (value: T) => void }) {
  return (
    <div className="seg" role="tablist">
      {options.map((option) => (
        <button key={option.value} type="button" role="tab" aria-selected={value === option.value} className={clsx(value === option.value && "on")} onClick={() => onChange(option.value)}>
          {option.label}
        </button>
      ))}
    </div>
  );
}

export type TagTone = "accent" | "warn" | "danger" | "neutral";

export function Tag({ tone = "neutral", children }: { tone?: TagTone; children: React.ReactNode }) {
  return <span className={clsx("tag", tone === "accent" && "a", tone === "warn" && "w", tone === "danger" && "d")}>{children}</span>;
}

export function Badge({ count }: { count: number | null | undefined }) {
  return count ? <span className="badge">{count}</span> : null;
}

export function Toggle({ on, onChange, label, disabled }: { on: boolean; onChange: (on: boolean) => void; label: string; disabled?: boolean }) {
  return <button type="button" className={clsx("toggle", on && "on")} role="switch" aria-checked={on} aria-label={label} disabled={disabled} onClick={() => onChange(!on)} />;
}

export function Field({ label, children, note }: { label: React.ReactNode; children: React.ReactNode; note?: React.ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {note && <span className="cap self-end">{note}</span>}
    </label>
  );
}

// ---------- 进度、提示 ----------

export function Bar({ ratio, warn }: { ratio: number; warn?: boolean }) {
  return (
    <div className={clsx("bar", warn && "w")}>
      <i style={{ width: `${Math.round(Math.min(Math.max(ratio, 0), 1) * 100)}%` }} />
    </div>
  );
}

export type BannerTone = "warn" | "danger" | "accent";

export function Banner({ tone = "warn", icon: Icon = AlertTriangle, children }: { tone?: BannerTone; icon?: LucideIcon; children: React.ReactNode }) {
  return (
    <div className={clsx("banner", tone === "danger" && "d", tone === "accent" && "a")}>
      <Icon size={18} className="mt-0.5 shrink-0" aria-hidden />
      <div className="grow">{children}</div>
    </div>
  );
}

export function Empty({ icon: Icon, children }: { icon?: LucideIcon; children: React.ReactNode }) {
  return (
    <div className="card empty">
      {Icon && (
        <div className="big">
          <Icon size={26} />
        </div>
      )}
      <div>{children}</div>
    </div>
  );
}

export function Spinner({ large }: { large?: boolean }) {
  return <span className={clsx("spin", large && "lg")} aria-hidden />;
}

// Loading 是读接口时的占位（几条灰条，不跳版面）。
export function Loading({ lines = 3 }: { lines?: number }) {
  const t = useTranslations("common");
  return (
    <div className="card stack" aria-busy="true" aria-label={t("loading")}>
      {Array.from({ length: lines }, (_, index) => (
        <div key={index} className="sk" style={{ width: `${90 - index * 18}%` }} />
      ))}
    </div>
  );
}

// Failure 显示服务端写好的出错说明（没有就不显示）。
export function Failure({ message }: { message: string | null | undefined }) {
  return message ? <Banner tone="danger">{message}</Banner> : null;
}

export function Note({ children }: { children: React.ReactNode }) {
  return <p className="sub">{children}</p>;
}

// MoneyAmount 是一笔合计：换成默认货币的总数，按币种分开的原始金额（多于一种时）和换算说明（后端写好的）。
export function MoneyAmount({ total, large }: { total: MoneyTotal; large?: boolean }) {
  const money = useMoneyFormat();
  return (
    <div>
      <span className={clsx("num", large ? "stat" : "amount")}>{money(total.total)}</span>
      {total.by_currency.length > 1 && <p className="cap">{total.by_currency.map(money).join(" + ")}</p>}
      {total.conversion_note && <p className="cap">{total.conversion_note}</p>}
    </div>
  );
}
