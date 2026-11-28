"use client";

// 改一笔账（原图不变）。
import { useTranslations } from "next-intl";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useState } from "react";

import { getExpense, updateExpense } from "@/api/generated/expenses/expenses";
import { problemMessage } from "@/api/problem";
import { ExpenseForm, stateOfExpense } from "@/components/expenses/ExpenseForm";
import { useCategories } from "@/components/expenses/useCategories";
import { showToast } from "@/components/overlay";
import { Button, Failure, Loading, Page } from "@/components/ui";
import { useApi } from "@/lib/useApi";

export default function EditExpensePage() {
  const t = useTranslations();
  const router = useRouter();
  const { expenseId } = useParams<{ expenseId: string }>();
  const categories = useCategories();
  const { data, error, loading } = useApi(useCallback(() => getExpense(expenseId), [expenseId]));
  const [busy, setBusy] = useState(false);
  const back = `/expenses/${expenseId}`;
  return (
    <Page title={t("expenses.editTitle")} back={back}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <div className="card">
          <ExpenseForm
            initial={stateOfExpense(data)}
            categories={categories}
            busy={busy}
            submitLabel={t("common.save")}
            secondary={
              <Button kind="plain" href={back}>
                {t("common.cancel")}
              </Button>
            }
            onSubmit={async (input) => {
              setBusy(true);
              try {
                await updateExpense(expenseId, input);
                showToast(t("common.saved"));
                router.replace(back);
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
