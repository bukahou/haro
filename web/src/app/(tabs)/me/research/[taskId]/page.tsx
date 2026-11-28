"use client";

// 一个调查：问题、状态、结论（Markdown）、来源（点了在新标签页打开）、没查完的原因。
import { ExternalLink } from "lucide-react";
import { useTranslations } from "next-intl";
import { useParams } from "next/navigation";
import { useCallback } from "react";

import { getResearchTask } from "@/api/generated/memory/memory";
import { Markdown } from "@/components/chat/Markdown";
import { researchTone } from "@/components/me/researchTone";
import { Banner, Card, Failure, Loading, Page, Spinner, Tag } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

export default function ResearchTaskPage() {
  const t = useTranslations("research");
  const time = useTimeFormat();
  const { taskId } = useParams<{ taskId: string }>();
  const { data, error, loading } = useApi(useCallback(() => getResearchTask(Number(taskId)), [taskId]));
  return (
    <Page title={t("detailTitle")} back="/me/research">
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <Card>
          <div className="stack">
            <div className="between items-start">
              <span className="b text-lg">{data.question}</span>
              <Tag tone={researchTone[data.status]}>{data.status_label}</Tag>
            </div>
            <p className="cap">{time.dateTime(data.created_at)}</p>
            {data.conclusion ? (
              <div className="ai-msg">
                <Markdown text={data.conclusion} />
              </div>
            ) : (data.status === "queued" || data.status === "running") && (
              <p className="hstack sub">
                <Spinner />
                {t("running")}
              </p>
            )}
            {data.stop_reason && <Banner tone={data.status === "partial" ? "warn" : "danger"}>{data.stop_reason}</Banner>}
            {data.sources.length > 0 && (
              <div>
                <h2 className="cap mb-2">{t("sources")}</h2>
                <div className="wrap">
                  {data.sources.map((source) => {
                    const link = webLink(source);
                    return link ? (
                      <a key={source} className="chip max-w-full" href={link.href} target="_blank" rel="noopener noreferrer">
                        <span className="ell">{link.host}</span>
                        <ExternalLink size={13} aria-hidden />
                      </a>
                    ) : (
                      <span key={source} className="tag max-w-full">
                        <span className="ell">{source}</span>
                      </span>
                    );
                  })}
                </div>
              </div>
            )}
          </div>
        </Card>
      )}
    </Page>
  );
}

// 来源是模型给的网址：解析得了、是 http / https 的才做成链接（显示网站名，整个地址在链接里）；别的照字面显示
function webLink(source: string): { href: string; host: string } | null {
  try {
    const url = new URL(source);
    return url.protocol === "https:" || url.protocol === "http:" ? { href: url.href, host: url.host } : null;
  } catch {
    return null;
  }
}
