"use client";

// 新建 / 改一条 haro 自己的日程：标题、全天、开始和结束、重复、提醒（档位来自 client-config）、在哪里提醒、地点、备注。
// 校验交给服务端（契约 EventInput），这里只把填的东西换成接口要的样子。
import { Bell, MonitorSmartphone } from "lucide-react";
import { useTranslations } from "next-intl";
import { useState } from "react";

import type { CalendarEvent, EventInput } from "@/api/generated/model";
import { Button, Chip, Field, Toggle } from "@/components/ui";
import { addDays, allDayISO, allDayKeyOf, dayKeyOf, timedISO, timeOf, type DayKey } from "@/lib/calendarDays";
import { useClientConfig } from "@/stores/clientConfig";

import { isCustomRule, repeatChoices, useRepeatLabel } from "./repeat";

export interface EventDraft {
  title: string;
  allDay: boolean;
  startDay: DayKey;
  startTime: string;
  endDay: DayKey;
  endTime: string;
  rule: string | null;
  alerts: number[];
  alertTerminal: boolean;
  alertPhone: boolean;
  location: string;
  notes: string;
}

export function newDraft(day: DayKey): EventDraft {
  return { title: "", allDay: false, startDay: day, startTime: "09:00", endDay: day, endTime: "10:00", rule: null, alerts: [], alertTerminal: true, alertPhone: true, location: "", notes: "" };
}

// 一条日程（的某一次：start / end 是那一次的）→ 表单
export function draftOf(event: CalendarEvent, start: string, end: string): EventDraft {
  return {
    title: event.title,
    allDay: event.all_day,
    startDay: event.all_day ? allDayKeyOf(start) : dayKeyOf(new Date(start)),
    startTime: event.all_day ? "09:00" : timeOf(start),
    endDay: event.all_day ? addDays(allDayKeyOf(end), -1) : dayKeyOf(new Date(end)),
    endTime: event.all_day ? "10:00" : timeOf(end),
    rule: event.rrule,
    alerts: [...event.alerts],
    alertTerminal: event.alert_terminal,
    alertPhone: event.alert_phone,
    location: event.location,
    notes: event.notes,
  };
}

// 表单 → 接口（全天的按契约写成 00:00Z、结束是最后一天的第二天）
export function inputOf(draft: EventDraft): EventInput {
  return {
    title: draft.title.trim(),
    all_day: draft.allDay,
    start: draft.allDay ? allDayISO(draft.startDay) : timedISO(draft.startDay, draft.startTime),
    end: draft.allDay ? allDayISO(addDays(draft.endDay, 1)) : timedISO(draft.endDay, draft.endTime),
    rrule: draft.rule,
    alerts: draft.alerts,
    alert_terminal: draft.alertTerminal,
    alert_phone: draft.alertPhone,
    location: draft.location.trim(),
    notes: draft.notes.trim(),
  };
}

export function useAlertLabel() {
  const t = useTranslations("calendar.alerts");
  return (seconds: number) => {
    if (seconds === 0) return t("atTime");
    if (seconds % 86400 === 0) return t("days", { count: seconds / 86400 });
    if (seconds % 3600 === 0) return t("hours", { count: seconds / 3600 });
    return t("minutes", { count: Math.round(seconds / 60) });
  };
}

export function EventForm({ initial, busy, onSave, onCancel }: { initial: EventDraft; busy: boolean; onSave: (draft: EventDraft) => void; onCancel: () => void }) {
  const t = useTranslations();
  const repeatLabel = useRepeatLabel();
  const alertLabel = useAlertLabel();
  const options = useClientConfig((state) => state.config?.calendar_alert_options ?? []);
  const [draft, setDraft] = useState(initial);
  const set = (change: Partial<EventDraft>) => setDraft((current) => ({ ...current, ...change }));
  const alertChoices = [...new Set([...options, ...draft.alerts])].sort((a, b) => a - b); // 语音定的、不在档位里的也列出来
  const keepCustom = isCustomRule(initial.rule);
  return (
    <form
      className="card"
      onSubmit={(event) => {
        event.preventDefault();
        if (draft.title.trim()) onSave(draft);
      }}
    >
      <div className="stack" style={{ gap: 14 }}>
        <Field label={t("calendar.form.title")}>
          <input className="input" value={draft.title} maxLength={200} placeholder={t("calendar.form.titlePlaceholder")} autoFocus onChange={(event) => set({ title: event.target.value })} />
        </Field>
        <div className="between">
          <span>{t("calendar.allDay")}</span>
          <Toggle on={draft.allDay} onChange={(allDay) => set({ allDay })} label={t("calendar.allDay")} />
        </div>
        <div className="form-grid">
          <Field label={t("calendar.form.start")}>
            <div className="hstack">
              <input
                className="input"
                type="date"
                value={draft.startDay}
                onChange={(event) => event.target.value && set({ startDay: event.target.value, endDay: draft.endDay < event.target.value ? event.target.value : draft.endDay })}
              />
              {!draft.allDay && <input className="input" type="time" value={draft.startTime} onChange={(event) => set({ startTime: event.target.value })} aria-label={t("calendar.form.startTime")} />}
            </div>
          </Field>
          <Field label={t("calendar.form.end")}>
            <div className="hstack">
              <input className="input" type="date" value={draft.endDay} min={draft.startDay} onChange={(event) => event.target.value && set({ endDay: event.target.value })} />
              {!draft.allDay && <input className="input" type="time" value={draft.endTime} onChange={(event) => set({ endTime: event.target.value })} aria-label={t("calendar.form.endTime")} />}
            </div>
          </Field>
        </div>
        <Field label={t("calendar.form.repeat")}>
          <select className="input" value={draft.rule ?? ""} onChange={(event) => set({ rule: event.target.value || null })}>
            <option value="">{repeatLabel(null)}</option>
            {repeatChoices.map((rule) => (
              <option key={rule} value={rule}>
                {repeatLabel(rule)}
              </option>
            ))}
            {keepCustom && initial.rule && <option value={initial.rule}>{t("calendar.form.keepRule")}</option>}
          </select>
        </Field>
        <div className="field">
          <span>{t("calendar.form.alerts")}</span>
          <div className="wrap">
            {alertChoices.map((seconds) => (
              <Chip
                key={seconds}
                on={draft.alerts.includes(seconds)}
                onClick={() => set({ alerts: draft.alerts.includes(seconds) ? draft.alerts.filter((value) => value !== seconds) : [...draft.alerts, seconds].sort((a, b) => a - b) })}
              >
                {alertLabel(seconds)}
              </Chip>
            ))}
          </div>
        </div>
        <div className="field">
          <span>{t("calendar.form.where")}</span>
          <div className="between">
            <span className="hstack">
              <MonitorSmartphone size={18} aria-hidden />
              {t("calendar.alertTerminal")}
            </span>
            <Toggle on={draft.alertTerminal} onChange={(alertTerminal) => set({ alertTerminal })} label={t("calendar.alertTerminal")} />
          </div>
          <div className="between">
            <span className="hstack">
              <Bell size={18} aria-hidden />
              {t("calendar.alertPhone")}
            </span>
            <Toggle on={draft.alertPhone} onChange={(alertPhone) => set({ alertPhone })} label={t("calendar.alertPhone")} />
          </div>
        </div>
        <Field label={t("calendar.form.location")}>
          <input className="input" value={draft.location} maxLength={2000} placeholder={t("calendar.form.optional")} onChange={(event) => set({ location: event.target.value })} />
        </Field>
        <Field label={t("calendar.form.notes")}>
          <textarea className="input" style={{ minHeight: 80 }} value={draft.notes} maxLength={2000} placeholder={t("calendar.form.optional")} onChange={(event) => set({ notes: event.target.value })} />
        </Field>
      </div>
      <div className="actions">
        <Button kind="solid" type="submit" disabled={busy || !draft.title.trim()}>
          {t("common.save")}
        </Button>
        <Button kind="plain" onClick={onCancel}>
          {t("common.cancel")}
        </Button>
      </div>
    </form>
  );
}
