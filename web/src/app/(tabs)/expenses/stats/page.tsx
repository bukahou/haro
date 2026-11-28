"use client";

// 查账：一个月的合计（换算说明、上个月）、按分类（占的比例由后端算好）、按店。
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import { summarizeExpenses } from "@/api/generated/expenses/expenses";
import { MonthSwitch } from "@/components/expenses/month";
import { Bar, Card, Failure, Grid2, Loading, MoneyAmount, Page } from "@/components/ui";
import { useMoneyFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

export default function ExpenseStatsPage() {
  const t = useTranslations("expenses");
  const money = useMoneyFormat();
  const [month, setMonth] = useState<string | undefined>(undefined);
  const { data, error, loading } = useApi(useCallback(() => summarizeExpenses(month ? { month } : undefined), [month]));
  return (
    <Page title={t("stats")} back="/expenses">
      {loading && !data && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          <div className="card">
            <MonthSwitch month={data.month} onChange={setMonth} />
            <div className="mt-2">
              <MoneyAmount total={data.total} large />
              <p className="cap">
                {t("count", { count: data.count })} · {t("previous", { amount: money(data.previous.total) })}
              </p>
            </div>
          </div>
          <div className="mt-3">
            <Grid2>
              <Card title={t("byCategory")}>
                {data.categories.length ? (
                  <div className="stack" style={{ gap: 10 }}>
                    {data.categories.map((group) => (
                      <div key={group.key ?? "none"} className="cat-bar">
                        <span className="ell">{group.name}</span>
                        <Bar ratio={group.share} />
                        <span className="num sub">{money(group.total.total)}</span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="sub">{t("empty")}</p>
                )}
              </Card>
              <Card title={t("byMerchant")}>
                {data.merchants.length ? (
                  data.merchants.map((group) => (
                    <div key={group.name} className="row px-0 py-2" style={{ cursor: "default", minHeight: 0 }}>
                      <span className="grow ell">{group.name}</span>
                      <span className="cap">{t("count", { count: group.count })}</span>
                      <span className="amount">{money(group.total.total)}</span>
                    </div>
                  ))
                ) : (
                  <p className="sub">{t("empty")}</p>
                )}
              </Card>
            </Grid2>
          </div>
        </>
      )}
    </Page>
  );
}
