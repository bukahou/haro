"use client";

// 花费页：这个月（合计、上限、状态）、按用途分、今天的调查和搜索额度。数都是后端算好的。
import { useTranslations } from "next-intl";

import { getSpending } from "@/api/generated/spending/spending";
import { MonthSpendingCard } from "@/components/me/MonthSpendingCard";
import { Card, Failure, Loading, MoneyAmount, Note, Page, Progress, Row, Rows } from "@/components/ui";
import { useMoneyFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

const loadSpending = () => getSpending();

export default function SpendingPage() {
  const t = useTranslations("spending");
  const money = useMoneyFormat();
  const { data, error, loading } = useApi(loadSpending);
  return (
    <Page title={t("title")} back="/me">
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          <MonthSpendingCard month={data.month} />
          <Card title={t("byPurpose")}>
            <ul className="flex flex-col gap-3">
              {data.by_purpose.map((purpose) => (
                <li key={purpose.purpose} className="flex flex-col gap-1">
                  <div className="flex items-baseline justify-between gap-3 text-sm">
                    <span>{purpose.label}</span>
                    <MoneyAmount total={purpose.spent} />
                  </div>
                  <Progress ratio={purpose.share} />
                </li>
              ))}
            </ul>
          </Card>
          <Card title={t("today")}>
            <Rows>
              <Row label={t("research")}>
                <MoneyAmount total={data.today.research.spent} />
                {data.today.research.cap && (
                  <Note>
                    {t("cap")} {money(data.today.research.cap)}
                    {data.today.research.remaining && ` · ${t("remaining")} ${money(data.today.research.remaining)}`}
                  </Note>
                )}
              </Row>
              <Row label={t("search")}>
                {t("searchUsed", { used: data.today.search.used })}
                {data.today.search.cap != null && ` · ${t("searchCap", { cap: data.today.search.cap })}`}
              </Row>
            </Rows>
          </Card>
        </>
      )}
    </Page>
  );
}
