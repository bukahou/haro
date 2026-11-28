"use client";

// 手记一笔（没有小票）。币种先填账号的默认货币。
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useState } from "react";

import { createExpense } from "@/api/generated/expenses/expenses";
import { getMe } from "@/api/generated/me/me";
import { problemMessage } from "@/api/problem";
import { blankExpense, ExpenseForm } from "@/components/expenses/ExpenseForm";
import { useCategories } from "@/components/expenses/useCategories";
import { showToast } from "@/components/overlay";
import { Failure, Loading, Page } from "@/components/ui";
import { useApi } from "@/lib/useApi";

const loadMe = () => getMe();

export default function ManualExpensePage() {
  const t = useTranslations("expenses");
  const router = useRouter();
  const categories = useCategories();
  const me = useApi(loadMe);
  const [busy, setBusy] = useState(false);
  return (
    <Page title={t("manualTitle")} back="/expenses">
      {me.loading && <Loading />}
      <Failure message={me.error} />
      {me.data && (
        <div className="card">
          <ExpenseForm
            initial={blankExpense(me.data.default_currency)}
            categories={categories}
            busy={busy}
            submitLabel={t("record")}
            onSubmit={async (input) => {
              setBusy(true);
              try {
                const expense = await createExpense(input);
                showToast(t("recorded"));
                router.replace(`/expenses/${expense.expense_id}`);
              } catch (failure) {
                showToast(problemMessage(failure));
                setBusy(false);
              }
            }}
          />
        </div>
      )}
    </Page>
  );
}
