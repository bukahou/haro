"use client";

// 后台调查：派到后台去查的问题（新的在前），点进去看结论和来源。
import { FlaskConical } from "lucide-react";
import { useTranslations } from "next-intl";

import { listResearchTasks } from "@/api/generated/memory/memory";
import { researchTone } from "@/components/me/researchTone";
import { Empty, Failure, Group, ListRow, Loading, Page, Tag } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

const loadTasks = () => listResearchTasks();

export default function ResearchPage() {
  const t = useTranslations("research");
  const time = useTimeFormat();
  const { data, error, loading } = useApi(loadTasks);
  return (
    <Page title={t("title")} back="/me" sub={t("hint")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data &&
        (data.length ? (
          <Group>
            {data.map((task) => (
              <ListRow
                key={task.task_id}
                href={`/me/research/${task.task_id}`}
                icon={FlaskConical}
                tone={task.status === "done" ? "accent" : "neutral"}
                title={<span className="ell block">{task.question}</span>}
                sub={time.dateTime(task.created_at)}
                right={<Tag tone={researchTone[task.status]}>{task.status_label}</Tag>}
              />
            ))}
          </Group>
        ) : (
          <Empty icon={FlaskConical}>{t("empty")}</Empty>
        ))}
    </Page>
  );
}
