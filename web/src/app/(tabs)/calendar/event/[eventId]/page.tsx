"use client";

// 一条日程（?occurrence= 是从哪一次点进来的）。haro 自己的能改、能删：重复的先问改 / 删哪几次（只这一次、这次和以后、全部）；
// 从系统日历同步进来的只能看。
import { AlertTriangle, Pencil, Trash2 } from "lucide-react";
import { useFormatter, useTranslations } from "next-intl";
import { useParams, useRouter, useSearchParams } from "next/navigation";
import { Suspense, useCallback, useState } from "react";

import { deleteEvent, getEvent, updateEvent } from "@/api/generated/calendar/calendar";
import type { CalendarEvent, EditScopeParameter, EventInput } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { draftOf, EventForm, inputOf, useAlertLabel } from "@/components/calendar/EventForm";
import { occurrenceHref, useRepeatLabel } from "@/components/calendar/repeat";
import { confirmDialog, openDialog, showToast } from "@/components/overlay";
import { Banner, Button, Failure, KV, KVRow, Loading, Page } from "@/components/ui";
import { addDays, allDayKeyOf, dateOfKey, timeOf } from "@/lib/calendarDays";
import { useApi } from "@/lib/useApi";

const shift = (iso: string, millis: number) => new Date(Date.parse(iso) + millis).toISOString();

// 改「全部」时：主人在这一次上挪了多少，整个系列就挪多少（不把系列的开始改成这一次）
function seriesInput(event: CalendarEvent, input: EventInput, start: string, end: string): EventInput {
  return { ...input, start: shift(event.start, Date.parse(input.start) - Date.parse(start)), end: shift(event.end, Date.parse(input.end) - Date.parse(end)) };
}

function EventView({ event, start, end, reload }: { event: CalendarEvent; start: string; end: string; reload: () => void }) {
  const t = useTranslations();
  const format = useFormatter();
  const router = useRouter();
  const repeatLabel = useRepeatLabel();
  const alertLabel = useAlertLabel();
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const recurring = event.rrule !== null;

  const save = async (input: EventInput, scope: EditScopeParameter) => {
    setBusy(true);
    try {
      const body = scope === "all" ? seriesInput(event, input, start, end) : input;
      const updated = await updateEvent(event.event_id, body, scope === "all" ? undefined : { scope, occurrence: start });
      showToast(t(`calendar.savedScope.${recurring ? scope : "all"}`));
      setEditing(false);
      const next = occurrenceHref({ event: updated, start: scope === "all" ? input.start : updated.start, end: updated.end, recurring: updated.rrule !== null });
      router.replace(next);
      reload();
    } catch (failure) {
      showToast(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  const remove = async (scope: EditScopeParameter) => {
    await deleteEvent(event.event_id, scope === "all" ? undefined : { scope, occurrence: start });
    showToast(t(`calendar.deletedScope.${recurring ? scope : "all"}`));
    router.replace("/calendar");
  };
  const askScope = (title: string, danger: boolean, run: (scope: EditScopeParameter) => Promise<void>) =>
    openDialog({
      title,
      stacked: true,
      body: t("calendar.scopeBody", { title: event.title, rule: repeatLabel(event.rrule) }),
      buttons: [
        { label: t("calendar.scope.this"), kind: danger ? "danger" : "outline", run: () => run("this") },
        { label: t("calendar.scope.future"), kind: danger ? "danger" : "outline", run: () => run("future") },
        { label: t("calendar.scope.all"), kind: danger ? "danger" : "outline", run: () => run("all") },
        { label: t("common.cancel"), kind: "plain" },
      ],
    });

  if (editing) {
    return (
      <Page title={t("calendar.editEvent")} back={occurrenceHref({ event, start, end, recurring })}>
        <EventForm
          initial={draftOf(event, start, end)}
          busy={busy}
          onCancel={() => setEditing(false)}
          onSave={(draft) => {
            const input = inputOf(draft);
            if (recurring) askScope(t("calendar.editScopeTitle"), false, (scope) => save(input, scope));
            else void save(input, "all");
          }}
        />
      </Page>
    );
  }

  const day = (iso: string) => (event.all_day ? dateOfKey(allDayKeyOf(iso)) : new Date(iso));
  const lastDay = event.all_day ? dateOfKey(addDays(allDayKeyOf(end), -1)) : new Date(end);
  const sameDay = day(start).toDateString() === lastDay.toDateString();
  const dayText = (date: Date) => format.dateTime(date, { year: "numeric", month: "long", day: "numeric", weekday: "short" });
  const when = event.all_day
    ? `${dayText(day(start))}${sameDay ? "" : ` – ${dayText(lastDay)}`} · ${t("calendar.allDay")}`
    : `${dayText(day(start))} ${timeOf(start)} – ${sameDay ? "" : `${dayText(lastDay)} `}${timeOf(end)}`;
  const where = [event.alert_terminal && t("calendar.alertTerminal"), event.alert_phone && t("calendar.alertPhone")].filter(Boolean).join("、");

  return (
    <Page
      title={event.title}
      back="/calendar"
      actions={
        event.editable && (
          <Button small icon={Pencil} onClick={() => setEditing(true)}>
            {t("common.edit")}
          </Button>
        )
      }
    >
      {!event.editable && (
        <div className="mb-3">
          <Banner icon={AlertTriangle}>{t("calendar.readOnly", { name: event.calendar_name ?? "" })}</Banner>
        </div>
      )}
      <div className="card">
        <KV>
          <KVRow label={t("calendar.form.time")}>
            <span className="num">{when}</span>
          </KVRow>
          {recurring && <KVRow label={t("calendar.form.repeat")}>{repeatLabel(event.rrule)}</KVRow>}
          {event.location && <KVRow label={t("calendar.form.location")}>{event.location}</KVRow>}
          <KVRow label={t("calendar.form.calendar")}>
            <span className="hstack">
              <span className="dot" style={{ background: event.source === "haro" ? "var(--accent)" : "var(--blue)" }} />
              {event.source === "haro" ? t("calendar.legendHaro") : event.calendar_name}
            </span>
          </KVRow>
          {event.editable && (
            <>
              <KVRow label={t("calendar.alertsLabel")}>{event.alerts.length ? event.alerts.map(alertLabel).join("、") : t("calendar.noAlerts")}</KVRow>
              {event.alerts.length > 0 && <KVRow label={t("calendar.form.where")}>{where || "—"}</KVRow>}
            </>
          )}
          {event.notes && <KVRow label={t("calendar.form.notes")}>{event.notes}</KVRow>}
        </KV>
      </div>
      {event.editable && (
        <div className="actions">
          <Button
            kind="danger"
            icon={Trash2}
            onClick={() =>
              recurring
                ? askScope(t("calendar.deleteScopeTitle"), true, remove)
                : confirmDialog({
                    title: t("calendar.deleteTitle"),
                    body: t("calendar.deleteBody", { title: event.title }),
                    confirm: t("common.delete"),
                    cancel: t("common.cancel"),
                    run: () => remove("all"),
                  })
            }
          >
            {t("common.delete")}
          </Button>
        </div>
      )}
    </Page>
  );
}

function EventDetail() {
  const t = useTranslations("calendar");
  const { eventId } = useParams<{ eventId: string }>();
  const occurrence = useSearchParams().get("occurrence");
  const { data, error, loading, reload } = useApi(useCallback(() => getEvent(eventId), [eventId]));
  if (loading) {
    return (
      <Page title={t("event")} back="/calendar">
        <Loading />
      </Page>
    );
  }
  if (!data) {
    return (
      <Page title={t("event")} back="/calendar">
        <Failure message={error} />
      </Page>
    );
  }
  // 这一次的开始和结束（重复的：点进来的那一次；时长和系列一样）
  const start = occurrence ?? data.start;
  const end = occurrence ? shift(occurrence, Date.parse(data.end) - Date.parse(data.start)) : data.end;
  return <EventView key={`${data.event_id}:${start}`} event={data} start={start} end={end} reload={reload} />;
}

export default function EventPage() {
  return (
    <Suspense>
      <EventDetail />
    </Suspense>
  );
}
