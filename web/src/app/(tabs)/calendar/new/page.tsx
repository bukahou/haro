"use client";

// 新建一条 haro 自己的日程（?date= 是从哪一天点进来的）。
import { useTranslations } from "next-intl";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";

import { createEvent } from "@/api/generated/calendar/calendar";
import { problemMessage } from "@/api/problem";
import { EventForm, inputOf, newDraft } from "@/components/calendar/EventForm";
import { occurrenceHref } from "@/components/calendar/repeat";
import { showToast } from "@/components/overlay";
import { Page } from "@/components/ui";
import { todayKey } from "@/lib/calendarDays";

function NewEvent() {
  const t = useTranslations("calendar");
  const router = useRouter();
  const date = useSearchParams().get("date");
  const [busy, setBusy] = useState(false);
  const day = date && /^\d{4}-\d{2}-\d{2}$/.test(date) ? date : todayKey();
  return (
    <Page title={t("newEvent")} back="/calendar">
      <EventForm
        initial={newDraft(day)}
        busy={busy}
        onCancel={() => router.push("/calendar")}
        onSave={async (draft) => {
          setBusy(true);
          try {
            const created = await createEvent(inputOf(draft));
            showToast(t("saved"));
            router.replace(occurrenceHref({ event: created, start: created.start, end: created.end, recurring: created.rrule !== null }));
          } catch (failure) {
            showToast(problemMessage(failure));
            setBusy(false);
          }
        }}
      />
    </Page>
  );
}

export default function NewEventPage() {
  return (
    <Suspense>
      <NewEvent />
    </Suspense>
  );
}
