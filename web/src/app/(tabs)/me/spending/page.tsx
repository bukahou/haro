"use client";

// 花费：haro 自己用掉的钱（大模型、搜索），和记的账是两回事。这个月、按用途分、今天的调查和搜索额度。数都是后端算好的。
import { useTranslations } from "next-intl";

import { getSpending } from "@/api/generated/spending/spending";
import { MonthSpendingCard } from "@/components/me/MonthSpendingCard";
import { Bar, Card, Failure, KV, KVRow, Loading, MoneyAmount, Page } from "@/components/ui";
import { useMoneyFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

const loadSpending = () => getSpending();

export default function SpendingPage() {
  const t = useTranslations("spending");
  const money = useMoneyFormat();
  const { data, error, loading } = useApi(loadSpending);
  return (
    <Page title={t("title")} back="/me" sub={t("hint")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          <MonthSpendingCard month={data.month} />
          <Card title={t("byPurpose")}>
            <div className="stack" style={{ gap: 10 }}>
              {data.by_purpose.map((purpose) => (
                <div key={purpose.purpose} className="cat-bar">
                  <span className="ell">{purpose.label}</span>
                  <Bar ratio={purpose.share} />
                  <MoneyAmount total={purpose.spent} />
                </div>
              ))}
            </div>
          </Card>
          <Card title={t("today")}>
            <KV>
              <KVRow label={t("research")}>
                <MoneyAmount total={data.today.research.spent} />
                {data.today.research.cap && (
                  <span className="cap">
                    {t("cap")} {money(data.today.research.cap)}
                    {data.today.research.remaining && ` · ${t("remaining")} ${money(data.today.research.remaining)}`}
                  </span>
                )}
              </KVRow>
              <KVRow label={t("search")}>
                {t("searchUsed", { used: data.today.search.used })}
                {data.today.search.cap != null && ` · ${t("searchCap", { cap: data.today.search.cap })}`}
              </KVRow>
            </KV>
          </Card>
        </>
      )}
    </Page>
  );
}
