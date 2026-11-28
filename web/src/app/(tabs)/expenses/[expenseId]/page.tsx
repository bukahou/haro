"use client";

// 一笔账：合计、时间、分类、付款、说明、明细、原图；能改、能删（原图一起删）。
import { Pencil, Trash2 } from "lucide-react";
import { useFormatter, useTranslations } from "next-intl";
import { useParams, useRouter } from "next/navigation";
import { useCallback } from "react";

import { deleteExpense, getExpense, getExpenseImage } from "@/api/generated/expenses/expenses";
import type { Expense } from "@/api/generated/model";
import { BlobImage } from "@/components/BlobImage";
import { confirmDialog, showToast } from "@/components/overlay";
import { Button, Card, Empty, Failure, KV, KVRow, Loading, Page } from "@/components/ui";
import { timeOf } from "@/lib/calendarDays";
import { useMoneyFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

function ExpenseView({ expense }: { expense: Expense }) {
  const t = useTranslations();
  const format = useFormatter();
  const money = useMoneyFormat();
  const router = useRouter();
  const loadImage = useCallback(() => getExpenseImage(expense.expense_id), [expense.expense_id]);
  return (
    <Page
      title={expense.merchant}
      back="/expenses"
      actions={
        <Button small icon={Pencil} href={`/expenses/${expense.expense_id}/edit`}>
          {t("common.edit")}
        </Button>
      }
    >
      <div className="draft-layout">
        <div>
          {expense.has_image ? (
            <>
              <BlobImage load={loadImage} alt={t("expenses.receiptImage")} className="receipt-img" />
              <p className="cap mt-1.5">{t("expenses.imageNote")}</p>
            </>
          ) : (
            <Empty>{t("expenses.noImage")}</Empty>
          )}
        </div>
        <div className="stack">
          <div className="card m-0">
            <span className="stat num">{money(expense.total)}</span>
            <div className="mt-3">
              <KV>
                <KVRow label={t("expenses.form.time")}>
                  <span className="num">
                    {format.dateTime(new Date(expense.spent_at), { month: "long", day: "numeric", weekday: "short" })} {timeOf(expense.spent_at)}
                  </span>
                </KVRow>
                <KVRow label={t("expenses.form.category")}>{expense.category?.name ?? t("expenses.uncategorized")}</KVRow>
                {expense.payment_method && <KVRow label={t("expenses.form.payment")}>{expense.payment_method}</KVRow>}
                {expense.note && <KVRow label={t("expenses.form.note")}>{expense.note}</KVRow>}
              </KV>
            </div>
          </div>
          {expense.items.length > 0 && (
            <Card title={t("expenses.form.items")} className="m-0">
              <table className="items-table">
                <tbody>
                  {expense.items.map((item, index) => (
                    <tr key={index}>
                      <td>
                        {item.name}
                        {item.quantity && item.quantity !== "1" && <span className="cap"> × {item.quantity}</span>}
                        {item.tax_rate && <span className="cap"> {t("expenses.taxRateShort", { rate: item.tax_rate })}</span>}
                      </td>
                      <td className="num text-right">{money(item.amount)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {expense.subtotal && (
                <div className="between mt-2 px-1.5">
                  <span className="sub">{t("expenses.form.subtotal")}</span>
                  <span className="num">{money(expense.subtotal)}</span>
                </div>
              )}
              {expense.tax && (
                <div className="between px-1.5">
                  <span className="sub">{t("expenses.form.tax")}</span>
                  <span className="num">{money(expense.tax)}</span>
                </div>
              )}
            </Card>
          )}
          <div>
            <Button
              kind="danger"
              icon={Trash2}
              onClick={() =>
                confirmDialog({
                  title: t("expenses.deleteTitle"),
                  body: expense.has_image ? t("expenses.deleteBodyImage") : t("expenses.deleteBody"),
                  confirm: t("common.delete"),
                  cancel: t("common.cancel"),
                  run: async () => {
                    await deleteExpense(expense.expense_id);
                    showToast(t("expenses.deleted"));
                    router.replace("/expenses");
                  },
                })
              }
            >
              {t("expenses.delete")}
            </Button>
          </div>
        </div>
      </div>
    </Page>
  );
}

export default function ExpensePage() {
  const t = useTranslations("expenses");
  const { expenseId } = useParams<{ expenseId: string }>();
  const { data, error, loading } = useApi(useCallback(() => getExpense(expenseId), [expenseId]));
  if (data) return <ExpenseView expense={data} />;
  return (
    <Page title={t("expense")} back="/expenses">
      {loading ? <Loading /> : <Failure message={error} />}
    </Page>
  );
}
