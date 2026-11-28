"use client";

// 一张小票：在读时等着（每隔一会儿问一次），读好了核对草稿（看不清的标黄）再入账，没读成的说原因（重拍 / 丢掉）。
import { AlertTriangle } from "lucide-react";
import { useTranslations } from "next-intl";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";

import { confirmReceipt, discardReceipt, getReceipt, getReceiptImage } from "@/api/generated/expenses/expenses";
import type { Receipt } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { BlobImage } from "@/components/BlobImage";
import { ExpenseForm, stateOfDraft } from "@/components/expenses/ExpenseForm";
import { useCategories } from "@/components/expenses/useCategories";
import { confirmDialog, showToast } from "@/components/overlay";
import { Banner, Button, Failure, Loading, Page, Spinner } from "@/components/ui";

const pollMillis = 2000;

export default function ReceiptPage() {
  const t = useTranslations();
  const router = useRouter();
  const { receiptId } = useParams<{ receiptId: string }>();
  const categories = useCategories();
  const [receipt, setReceipt] = useState<Receipt | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const loadImage = useCallback(() => getReceiptImage(receiptId), [receiptId]);

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      try {
        const latest = await getReceipt(receiptId);
        if (!alive) return;
        setReceipt(latest);
        if (latest.status === "parsing") timer = setTimeout(() => void poll(), pollMillis);
      } catch (failure) {
        if (alive) setError(problemMessage(failure));
      }
    };
    void poll();
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [receiptId]);

  const discard = () =>
    confirmDialog({
      title: t("expenses.discardTitle"),
      body: t("expenses.discardBody"),
      confirm: t("expenses.discard"),
      cancel: t("common.cancel"),
      run: async () => {
        await discardReceipt(receiptId);
        router.replace("/expenses");
      },
    });

  const image = <BlobImage load={loadImage} alt={t("expenses.receiptImage")} className="receipt-img" />;

  if (error) {
    return (
      <Page title={t("expenses.receipt")} back="/expenses/new">
        <Failure message={error} />
      </Page>
    );
  }
  if (!receipt) {
    return (
      <Page title={t("expenses.receipt")} back="/expenses/new">
        <Loading />
      </Page>
    );
  }
  if (receipt.status === "parsing") {
    return (
      <Page title={t("expenses.receipt")} back="/expenses/new">
        <div className="card flex flex-wrap items-center gap-5">
          <div className="w-40">{image}</div>
          <div className="stack" style={{ gap: 8 }}>
            <span className="hstack b">
              <Spinner large />
              {t("expenses.parsing")}
            </span>
            <span className="sub">{t("expenses.parsingHint")}</span>
            <span className="cap">{t("expenses.parsingLeave")}</span>
          </div>
        </div>
      </Page>
    );
  }
  if (receipt.status === "failed") {
    return (
      <Page title={t("expenses.receipt")} back="/expenses/new">
        <div className="card stack items-center text-center">
          <Banner tone="danger" icon={AlertTriangle}>
            {receipt.failure_message ?? t("expenses.failedFallback")}
          </Banner>
          <div className="w-40">{image}</div>
          <div className="actions justify-center">
            <Button kind="solid" href="/expenses/new">
              {t("expenses.retake")}
            </Button>
            <Button kind="plain" onClick={discard}>
              {t("expenses.discard")}
            </Button>
          </div>
        </div>
      </Page>
    );
  }
  if (receipt.status === "confirmed" || !receipt.draft) {
    return (
      <Page title={t("expenses.receipt")} back="/expenses">
        <Banner tone="accent">{t("expenses.alreadyConfirmed")}</Banner>
        {receipt.expense_id && (
          <div className="actions">
            <Button href={`/expenses/${receipt.expense_id}`}>{t("expenses.openExpense")}</Button>
          </div>
        )}
      </Page>
    );
  }
  return (
    <Page title={t("expenses.reviewTitle")} back="/expenses/new" wide>
      <div className="mb-3.5">
        <Banner>{t("expenses.reviewHint")}</Banner>
      </div>
      <div className="draft-layout">
        {image}
        <div className="card">
          <ExpenseForm
            initial={stateOfDraft(receipt.draft)}
            uncertain={receipt.draft.uncertain}
            categories={categories}
            busy={busy}
            submitLabel={t("expenses.confirm")}
            secondary={
              <Button kind="plain" onClick={discard}>
                {t("expenses.discard")}
              </Button>
            }
            onSubmit={async (input) => {
              setBusy(true);
              try {
                const expense = await confirmReceipt(receiptId, input);
                showToast(t("expenses.confirmed"));
                router.replace(`/expenses/${expense.expense_id}`);
              } catch (failure) {
                showToast(problemMessage(failure));
                setBusy(false);
              }
            }}
          />
        </div>
      </div>
    </Page>
  );
}
