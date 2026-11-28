"use client";

// 记一笔：交小票、发票、收据或订单页的截图（可以先点分类、写一句说明），后台读成草稿；还没入账的小票列在上面。
import { Receipt as ReceiptIcon, Upload } from "lucide-react";
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useState } from "react";

import { listReceipts, uploadReceipt } from "@/api/generated/expenses/expenses";
import type { ReceiptStatus } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { useCategories } from "@/components/expenses/useCategories";
import { showToast } from "@/components/overlay";
import { Card, Field, Group, ListRow, Page, SecTitle, Spinner, Tag } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";
import { useClientConfig } from "@/stores/clientConfig";

const loadReceipts = () => listReceipts();
const statusTone: Record<ReceiptStatus, "warn" | "accent" | "danger" | "neutral"> = { parsing: "warn", ready: "accent", failed: "danger", confirmed: "neutral" };

export default function NewReceiptPage() {
  const t = useTranslations("expenses");
  const router = useRouter();
  const time = useTimeFormat();
  const categories = useCategories();
  const maxBytes = useClientConfig((state) => state.config?.limits.attachment_max_bytes);
  const open = useApi(loadReceipts);
  const [category, setCategory] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [dragging, setDragging] = useState(false);

  const send = async (file: File | undefined) => {
    if (!file) return;
    if (maxBytes && file.size > maxBytes) {
      showToast(t("imageTooLarge", { mb: Math.floor(maxBytes / 1024 / 1024) }));
      return;
    }
    setBusy(true);
    try {
      const receipt = await uploadReceipt(file, { ...(category ? { category } : {}), ...(note.trim() ? { note: note.trim() } : {}) });
      router.push(`/expenses/receipt/${receipt.receipt_id}`);
    } catch (failure) {
      showToast(problemMessage(failure));
      setBusy(false);
    }
  };

  return (
    <Page title={t("new")} back="/expenses">
      <label
        className="drop"
        style={dragging ? { borderColor: "var(--accent-ink)" } : undefined}
        onDragOver={(event) => {
          event.preventDefault();
          setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(event) => {
          event.preventDefault();
          setDragging(false);
          void send([...event.dataTransfer.files].find((file) => file.type.startsWith("image/")));
        }}
      >
        {busy ? <Spinner large /> : <Upload size={32} aria-hidden />}
        <span className="b">{busy ? t("uploading") : t("dropTitle")}</span>
        <span className="sub">{t("dropHint")}</span>
        <span className="btn sm solid mt-1.5">{t("pick")}</span>
        <input type="file" accept="image/jpeg,image/png,image/webp" hidden disabled={busy} onChange={(event) => void send(event.target.files?.[0])} />
      </label>
      <Card title={t("hintTitle")} className="mt-4">
        <div className="form-grid">
          <Field label={t("form.category")}>
            <select className="input" value={category} onChange={(event) => setCategory(event.target.value)}>
              <option value="">{t("letHaroGuess")}</option>
              {categories.map((item) => (
                <option key={item.key} value={item.key}>
                  {item.name}
                </option>
              ))}
            </select>
          </Field>
          <Field label={t("hintNote")}>
            <input className="input" value={note} maxLength={1000} placeholder={t("hintNotePlaceholder")} onChange={(event) => setNote(event.target.value)} />
          </Field>
        </div>
      </Card>
      <p className="cap mt-3">{t("receiptPolicy")}</p>
      {open.data && open.data.length > 0 && (
        <>
          <SecTitle>{t("openTitle")}</SecTitle>
          <Group>
            {open.data.map((receipt) => (
              <ListRow
                key={receipt.receipt_id}
                href={`/expenses/receipt/${receipt.receipt_id}`}
                icon={ReceiptIcon}
                tone={receipt.status === "ready" ? "accent" : "neutral"}
                title={receipt.draft?.merchant || t("receiptUntitled")}
                sub={time.dateTime(receipt.created_at)}
                right={<Tag tone={statusTone[receipt.status]}>{t(`receiptStatus.${receipt.status}`)}</Tag>}
              />
            ))}
          </Group>
        </>
      )}
    </Page>
  );
}
