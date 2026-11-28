"use client";

// W1 只有骨架：这一页的画面在 W2 / W3 做（接口已经在契约里）。
import { useTranslations } from "next-intl";

export default function ExpensesPage() {
  const t = useTranslations();
  return (
    <section>
      <h1 className="text-xl font-semibold">{t("tabs.expenses")}</h1>
      <p className="mt-2 text-muted">{t("common.notYet")}</p>
    </section>
  );
}
