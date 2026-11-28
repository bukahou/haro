"use client";

// 日历：月视图（点一天看当天的安排）和列表（今天起 30 天）。haro 自己的日程能改；从系统日历同步进来的只读。
// 重复的日程由后端展开成每一次（occurrences），这里只按天摆。
import clsx from "clsx";
import { Bell, ChevronLeft, ChevronRight, Plus, RefreshCw, Repeat } from "lucide-react";
import Link from "next/link";
import { useFormatter, useTranslations } from "next-intl";
import { useCallback, useMemo, useState } from "react";

import { listOccurrences } from "@/api/generated/calendar/calendar";
import type { Occurrence } from "@/api/generated/model";
import { occurrenceHref, useRepeatLabel } from "@/components/calendar/repeat";
import { Button, Empty, Failure, IconButton, Loading, Page, SecTitle, Seg } from "@/components/ui";
import { addDays, dateOfKey, daysOf, monthCells, rangeOf, timeOf, todayKey, type DayKey } from "@/lib/calendarDays";
import { useApi } from "@/lib/useApi";

type View = "month" | "list";
const listDays = 30;

// haro 自己的绿色，同步进来的蓝色
const colorOf = (occurrence: Occurrence) => (occurrence.event.source === "haro" ? "var(--accent)" : "var(--blue)");

function OccurrenceRow({ occurrence }: { occurrence: Occurrence }) {
  const t = useTranslations("calendar");
  const repeatLabel = useRepeatLabel();
  const event = occurrence.event;
  const alerts = event.alerts.length ? t("alertCount", { count: event.alerts.length }) : null;
  return (
    <Link className="row" href={occurrenceHref(occurrence)}>
      <span className="ev-time num">
        {event.all_day ? (
          t("allDay")
        ) : (
          <>
            {timeOf(occurrence.start)}
            <br />
            {timeOf(occurrence.end)}
          </>
        )}
      </span>
      <span className="dot" style={{ background: colorOf(occurrence) }} />
      <span className="grow">
        <span className="block">{event.title}</span>
        <span className="ev-src">
          {event.source === "system" ? (
            t("syncedFrom", { name: event.calendar_name ?? "" })
          ) : (
            <>
              {alerts && (
                <>
                  <Bell size={13} aria-hidden />
                  {alerts}
                </>
              )}
              {occurrence.recurring && (
                <>
                  {alerts && " · "}
                  <Repeat size={13} aria-hidden />
                  {repeatLabel(event.rrule)}
                </>
              )}
              {!alerts && !occurrence.recurring && event.location}
            </>
          )}
        </span>
      </span>
      <span className="chev">
        <ChevronRight size={18} />
      </span>
    </Link>
  );
}

function DayList({ day, occurrences }: { day: DayKey; occurrences: Occurrence[] }) {
  const t = useTranslations("calendar");
  const format = useFormatter();
  return (
    <>
      <div className="between px-1 pt-0.5 pb-2.5">
        <span className="b">{format.dateTime(dateOfKey(day), { month: "long", day: "numeric", weekday: "short" })}</span>
        <Button small href={`/calendar/new?date=${day}`} icon={Plus}>
          {t("new")}
        </Button>
      </div>
      {occurrences.length ? (
        <div className="group">
          {occurrences.map((occurrence) => (
            <OccurrenceRow key={`${occurrence.event.event_id}:${occurrence.start}`} occurrence={occurrence} />
          ))}
        </div>
      ) : (
        <Empty>{t("noEvents")}</Empty>
      )}
    </>
  );
}

function byDay(occurrences: Occurrence[]): Map<DayKey, Occurrence[]> {
  const map = new Map<DayKey, Occurrence[]>();
  for (const occurrence of occurrences) {
    for (const day of daysOf(occurrence)) {
      const list = map.get(day) ?? [];
      list.push(occurrence);
      map.set(day, list);
    }
  }
  // 全天的在前，其余按开始的时刻（接口已经按开始排好，全天的往前提）
  for (const list of map.values()) list.sort((a, b) => Number(b.event.all_day) - Number(a.event.all_day));
  return map;
}

function MonthView({ year, month, selected, onSelect, onMonth }: { year: number; month: number; selected: DayKey; onSelect: (day: DayKey) => void; onMonth: (delta: number | null) => void }) {
  const t = useTranslations("calendar");
  const format = useFormatter();
  const cells = useMemo(() => monthCells(year, month), [year, month]);
  const load = useCallback(() => {
    const range = rangeOf(cells[0], cells[cells.length - 1]);
    return listOccurrences(range);
  }, [cells]);
  const { data, error, loading } = useApi(load);
  const days = useMemo(() => byDay(data ?? []), [data]);
  const today = todayKey();
  const weekdays = cells.slice(0, 7).map((day) => format.dateTime(dateOfKey(day), { weekday: "narrow" }));
  return (
    <div className="cal-layout">
      <div className="card">
        <div className="month-head">
          <h2 className="num">{format.dateTime(new Date(year, month, 1), { year: "numeric", month: "long" })}</h2>
          <Button small kind="plain" onClick={() => onMonth(null)}>
            {t("today")}
          </Button>
          <IconButton icon={ChevronLeft} label={t("prevMonth")} onClick={() => onMonth(-1)} />
          <IconButton icon={ChevronRight} label={t("nextMonth")} onClick={() => onMonth(1)} />
        </div>
        <div className="weekdays">
          {weekdays.map((name, index) => (
            <span key={index}>{name}</span>
          ))}
        </div>
        <div className="days">
          {cells.map((day) => {
            const list = days.get(day) ?? [];
            const other = dateOfKey(day).getMonth() !== month;
            return (
              <button
                key={day}
                type="button"
                className={clsx("day", other && "other", day === today && "today", day === selected && "sel")}
                onClick={() => onSelect(day)}
                aria-label={t("dayLabel", { day: format.dateTime(dateOfKey(day), { month: "long", day: "numeric" }), count: list.length })}
                aria-pressed={day === selected}
              >
                <span className="n num">{dateOfKey(day).getDate()}</span>
                <span className="dots">
                  {list.slice(0, 3).map((occurrence) => (
                    <i key={`${occurrence.event.event_id}:${occurrence.start}`} style={{ background: colorOf(occurrence) }} />
                  ))}
                </span>
              </button>
            );
          })}
        </div>
        <div className="wrap mt-3">
          <span className="hstack cap">
            <span className="dot" style={{ background: "var(--accent)" }} />
            {t("legendHaro")}
          </span>
          <span className="hstack cap">
            <span className="dot" style={{ background: "var(--blue)" }} />
            {t("legendSynced")}
          </span>
        </div>
      </div>
      <div>
        {loading && !data ? <Loading /> : <DayList day={selected} occurrences={days.get(selected) ?? []} />}
        <Failure message={error} />
      </div>
    </div>
  );
}

function ListView() {
  const t = useTranslations("calendar");
  const format = useFormatter();
  const from = todayKey();
  const load = useCallback(() => listOccurrences(rangeOf(from, addDays(from, listDays - 1))), [from]);
  const { data, error, loading } = useApi(load);
  const days = useMemo(() => [...byDay(data ?? []).entries()].filter(([day]) => day >= from).sort(([a], [b]) => a.localeCompare(b)), [data, from]);
  if (loading) return <Loading />;
  if (error) return <Failure message={error} />;
  if (!days.length) return <Empty>{t("nothingAhead", { days: listDays })}</Empty>;
  return (
    <>
      {days.map(([day, list]) => (
        <div key={day}>
          <SecTitle>
            {day === from && `${t("today")} · `}
            {format.dateTime(dateOfKey(day), { month: "long", day: "numeric", weekday: "short" })}
          </SecTitle>
          <div className="group">
            {list.map((occurrence) => (
              <OccurrenceRow key={`${occurrence.event.event_id}:${occurrence.start}`} occurrence={occurrence} />
            ))}
          </div>
        </div>
      ))}
    </>
  );
}

export default function CalendarPage() {
  const t = useTranslations("calendar");
  const [view, setView] = useState<View>("month");
  const now = new Date();
  const [cursor, setCursor] = useState({ year: now.getFullYear(), month: now.getMonth() });
  const [selected, setSelected] = useState<DayKey>(todayKey());
  const moveMonth = (delta: number | null) => {
    if (delta === null) {
      const today = new Date();
      setCursor({ year: today.getFullYear(), month: today.getMonth() });
      setSelected(todayKey());
      return;
    }
    setCursor(({ year, month }) => {
      const next = new Date(year, month + delta, 1);
      return { year: next.getFullYear(), month: next.getMonth() };
    });
  };
  return (
    <Page
      title={t("title")}
      wide
      actions={
        <>
          <Seg<View>
            value={view}
            onChange={setView}
            options={[
              { value: "month", label: t("month") },
              { value: "list", label: t("list") },
            ]}
          />
          <IconButton icon={RefreshCw} label={t("sync.title")} href="/calendar/sync" />
          <Button small href={`/calendar/new?date=${selected}`} icon={Plus}>
            {t("newEvent")}
          </Button>
        </>
      }
    >
      {view === "month" ? (
        <MonthView
          year={cursor.year}
          month={cursor.month}
          selected={selected}
          onMonth={moveMonth}
          onSelect={(day) => {
            setSelected(day);
            const date = dateOfKey(day);
            if (date.getMonth() !== cursor.month) setCursor({ year: date.getFullYear(), month: date.getMonth() });
          }}
        />
      ) : (
        <ListView />
      )}
    </Page>
  );
}
