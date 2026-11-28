"use client";

// 一笔账的表单：交小票读出的草稿（看不清、拿不准的标黄）、手记、改账共用。金额都是十进制的字符串，校验交给服务端（契约 ExpenseInput）。
import clsx from "clsx";
import { Plus, X } from "lucide-react";
import { useTranslations } from "next-intl";
import { useState } from "react";

import type { Expense, ExpenseCategory, ExpenseDraft, ExpenseInput, ReceiptField } from "@/api/generated/model";
import { Button, Field } from "@/components/ui";
import { dayKeyOf, timedISO, timeOf } from "@/lib/calendarDays";

interface ItemRow {
  name: string;
  quantity: string;
  unit_price: string;
  amount: string;
  tax_rate: string;
}

export interface ExpenseFormState {
  merchant: string;
  category: string;
  day: string; // YYYY-MM-DD（空 = 读不出来，要主人填）
  time: string;
  currency: string;
  items: ItemRow[];
  subtotal: string;
  tax: string;
  total: string;
  payment: string;
  note: string;
}

const emptyItem = (): ItemRow => ({ name: "", quantity: "", unit_price: "", amount: "", tax_rate: "" });

export function blankExpense(currency: string): ExpenseFormState {
  const now = new Date();
  return { merchant: "", category: "", day: dayKeyOf(now), time: timeOf(now.toISOString()), currency, items: [], subtotal: "", tax: "", total: "", payment: "", note: "" };
}

export function stateOfDraft(draft: ExpenseDraft): ExpenseFormState {
  return {
    merchant: draft.merchant,
    category: draft.category ?? "",
    day: draft.spent_at ? dayKeyOf(new Date(draft.spent_at)) : "",
    time: draft.spent_at ? timeOf(draft.spent_at) : "",
    currency: draft.currency,
    items: draft.items.map((item) => ({ name: item.name, quantity: item.quantity ?? "", unit_price: item.unit_price ?? "", amount: item.amount, tax_rate: item.tax_rate ?? "" })),
    subtotal: draft.subtotal ?? "",
    tax: draft.tax ?? "",
    total: draft.total,
    payment: draft.payment_method ?? "",
    note: draft.note ?? "",
  };
}

export function stateOfExpense(expense: Expense): ExpenseFormState {
  return {
    merchant: expense.merchant,
    category: expense.category?.key ?? "",
    day: dayKeyOf(new Date(expense.spent_at)),
    time: timeOf(expense.spent_at),
    currency: expense.total.currency,
    items: expense.items.map((item) => ({
      name: item.name,
      quantity: item.quantity ?? "",
      unit_price: item.unit_price?.amount ?? "",
      amount: item.amount.amount,
      tax_rate: item.tax_rate ?? "",
    })),
    subtotal: expense.subtotal?.amount ?? "",
    tax: expense.tax?.amount ?? "",
    total: expense.total.amount,
    payment: expense.payment_method ?? "",
    note: expense.note ?? "",
  };
}

const optional = (value: string) => (value.trim() ? value.trim() : undefined);

export function inputOf(state: ExpenseFormState): ExpenseInput {
  return {
    merchant: state.merchant.trim(),
    spent_at: timedISO(state.day, state.time || "12:00"),
    currency: state.currency.trim().toUpperCase(),
    items: state.items
      .filter((item) => item.name.trim() || item.amount.trim())
      .map((item) => ({ name: item.name.trim(), amount: item.amount.trim(), quantity: optional(item.quantity), unit_price: optional(item.unit_price), tax_rate: optional(item.tax_rate) })),
    subtotal: optional(state.subtotal),
    tax: optional(state.tax),
    total: state.total.trim(),
    payment_method: optional(state.payment),
    category: optional(state.category),
    note: optional(state.note),
  };
}

export function ExpenseForm({
  initial,
  uncertain = [],
  categories,
  busy,
  submitLabel,
  onSubmit,
  secondary,
}: {
  initial: ExpenseFormState;
  uncertain?: ReceiptField[];
  categories: ExpenseCategory[];
  busy: boolean;
  submitLabel: string;
  onSubmit: (input: ExpenseInput) => void;
  secondary?: React.ReactNode;
}) {
  const t = useTranslations("expenses.form");
  const [state, setState] = useState(initial);
  const set = (change: Partial<ExpenseFormState>) => setState((current) => ({ ...current, ...change }));
  const setItem = (index: number, change: Partial<ItemRow>) => set({ items: state.items.map((item, at) => (at === index ? { ...item, ...change } : item)) });
  const unsure = (field: ReceiptField) => uncertain.includes(field);
  const label = (text: string, field?: ReceiptField) => (
    <>
      {text}
      {field && unsure(field) && <span className="wrn text-xs"> · {t("unsure")}</span>}
    </>
  );
  const ready = state.merchant.trim() && state.day && state.total.trim() && state.currency.trim().length === 3;
  const itemField = (index: number, key: keyof ItemRow, placeholder: string, numeric?: boolean) => (
    <input className={clsx("input", numeric && "num")} value={state.items[index][key]} placeholder={placeholder} aria-label={placeholder} onChange={(event) => setItem(index, { [key]: event.target.value })} />
  );
  return (
    <form
      className="stack"
      style={{ gap: 14 }}
      onSubmit={(event) => {
        event.preventDefault();
        if (ready) onSubmit(inputOf(state));
      }}
    >
      <div className="form-grid">
        <Field label={label(t("merchant"), "merchant")}>
          <input className={clsx("input", unsure("merchant") && "uncertain")} value={state.merchant} maxLength={200} onChange={(event) => set({ merchant: event.target.value })} />
        </Field>
        <Field label={t("category")}>
          <select className="input" value={state.category} onChange={(event) => set({ category: event.target.value })}>
            <option value="">{t("noCategory")}</option>
            {categories.map((category) => (
              <option key={category.key} value={category.key}>
                {category.name}
              </option>
            ))}
          </select>
        </Field>
        <Field label={label(t("date"), "spent_at")}>
          <input className={clsx("input", unsure("spent_at") && "uncertain")} type="date" value={state.day} onChange={(event) => set({ day: event.target.value })} />
        </Field>
        <Field label={label(t("time"), "spent_at")}>
          <input className={clsx("input", unsure("spent_at") && "uncertain")} type="time" value={state.time} onChange={(event) => set({ time: event.target.value })} />
        </Field>
      </div>
      <div className="field">
        <span>{label(t("items"), "items")}</span>
        <table className="items-table">
          <thead>
            <tr>
              <th style={{ width: "40%" }}>{t("itemName")}</th>
              <th>{t("quantity")}</th>
              <th>{t("unitPrice")}</th>
              <th>{t("amount")}</th>
              <th>{t("taxRate")}</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {state.items.map((_, index) => (
              <tr key={index}>
                <td>{itemField(index, "name", t("itemName"))}</td>
                <td>{itemField(index, "quantity", t("quantity"), true)}</td>
                <td>{itemField(index, "unit_price", t("unitPrice"), true)}</td>
                <td>{itemField(index, "amount", t("amount"), true)}</td>
                <td>{itemField(index, "tax_rate", t("taxRate"), true)}</td>
                <td>
                  <button type="button" className="icon-btn mut" onClick={() => set({ items: state.items.filter((__, at) => at !== index) })} aria-label={t("removeItem")}>
                    <X size={16} />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <div>
          <Button small kind="plain" icon={Plus} onClick={() => set({ items: [...state.items, emptyItem()] })}>
            {t("addItem")}
          </Button>
        </div>
      </div>
      <div className="form-grid">
        <Field label={label(t("subtotal"), "subtotal")}>
          <input className={clsx("input num", unsure("subtotal") && "uncertain")} value={state.subtotal} inputMode="decimal" onChange={(event) => set({ subtotal: event.target.value })} />
        </Field>
        <Field label={label(t("tax"), "tax")}>
          <input className={clsx("input num", unsure("tax") && "uncertain")} value={state.tax} inputMode="decimal" onChange={(event) => set({ tax: event.target.value })} />
        </Field>
        <Field label={label(t("total"), "total")}>
          <input className={clsx("input num", unsure("total") && "uncertain")} value={state.total} inputMode="decimal" onChange={(event) => set({ total: event.target.value })} />
        </Field>
        <Field label={label(t("currency"), "currency")}>
          <input className={clsx("input uppercase", unsure("currency") && "uncertain")} value={state.currency} maxLength={3} onChange={(event) => set({ currency: event.target.value })} />
        </Field>
        <Field label={label(t("payment"), "payment_method")}>
          <input className={clsx("input", unsure("payment_method") && "uncertain")} value={state.payment} maxLength={200} placeholder={t("paymentPlaceholder")} onChange={(event) => set({ payment: event.target.value })} />
        </Field>
      </div>
      <Field label={t("note")}>
        <input className="input" value={state.note} maxLength={1000} placeholder={t("optional")} onChange={(event) => set({ note: event.target.value })} />
      </Field>
      <div className="actions">
        <Button kind="solid" type="submit" disabled={busy || !ready}>
          {submitLabel}
        </Button>
        {secondary}
      </div>
    </form>
  );
}
