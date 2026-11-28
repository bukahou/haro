"use client";

// 日历同步：系统日历（iCloud、Google、Outlook……）在 iPhone 的 haro App 里同步，网页读不到电脑上的日历，
// 这里只看同步了哪些、上次什么时候。同步的范围（往前、往后多少天）由后端给。
import { RefreshCw } from "lucide-react";
import { useTranslations } from "next-intl";

import { listCalendarSyncs } from "@/api/generated/calendar/calendar";
import { Banner, Empty, Failure, Group, ListRow, Loading, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";
import { useClientConfig } from "@/stores/clientConfig";

const loadSyncs = () => listCalendarSyncs();

export default function CalendarSyncPage() {
  const t = useTranslations("calendar.sync");
  const time = useTimeFormat();
  const limits = useClientConfig((state) => state.config?.limits);
  const { data, error, loading } = useApi(loadSyncs);
  return (
    <Page
      title={t("title")}
      back="/calendar"
      sub={limits ? t("hintRange", { back: limits.calendar_sync_days_back, ahead: limits.calendar_sync_days_ahead }) : t("hint")}
    >
      <div className="mb-3">
        <Banner tone="accent">{t("webNote")}</Banner>
      </div>
      {loading && <Loading />}
      <Failure message={error} />
      {data &&
        (data.length ? (
          <Group>
            {data.map((sync) => (
              <ListRow key={sync.calendar_id} icon={RefreshCw} tone="neutral" title={sync.name} sub={t("syncedAt", { time: time.relative(sync.synced_at) })} />
            ))}
          </Group>
        ) : (
          <Empty icon={RefreshCw}>{t("none")}</Empty>
        ))}
    </Page>
  );
}
