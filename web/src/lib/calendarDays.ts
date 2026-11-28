// 日历画面用的日期换算（只是显示和把主人填的日期时间交给接口；重复的展开、提醒都在后端）。
// 契约：全天的日程 start 是那一天的 00:00Z、end 是最后一天的第二天 00:00Z（不含）；有时刻的是真实的时刻，按这台设备的时区显示。
import type { Occurrence } from "@/api/generated/model";

export type DayKey = string; // YYYY-MM-DD

const pad = (value: number) => String(value).padStart(2, "0");

export const dayKeyOf = (date: Date): DayKey => `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;

export const dateOfKey = (key: DayKey): Date => {
  const [year, month, day] = key.split("-").map(Number);
  return new Date(year, month - 1, day);
};

export const addDays = (key: DayKey, days: number): DayKey => {
  const date = dateOfKey(key);
  date.setDate(date.getDate() + days);
  return dayKeyOf(date);
};

export const todayKey = (): DayKey => dayKeyOf(new Date());

// 全天日程的日期（契约里的 00:00Z）↔ 这一天
export const allDayKeyOf = (iso: string): DayKey => iso.slice(0, 10);
export const allDayISO = (key: DayKey): string => `${key}T00:00:00Z`;

// 有时刻的：这一天的这个时刻（这台设备的时区）→ 接口要的时刻
export const timedISO = (key: DayKey, time: string): string => {
  const [hour, minute] = time.split(":").map(Number);
  const date = dateOfKey(key);
  date.setHours(hour, minute, 0, 0);
  return date.toISOString();
};

export const timeOf = (iso: string): string => {
  const date = new Date(iso);
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
};

// 一次日程占的每一天（跨天的、全天好几天的，每天都算上）
export function daysOf(occurrence: Occurrence): DayKey[] {
  const allDay = occurrence.event.all_day;
  const first = allDay ? allDayKeyOf(occurrence.start) : dayKeyOf(new Date(occurrence.start));
  // 结束不含：全天的 end 是第二天 00:00Z；有时刻的往前退 1 毫秒再取日期（10:00–00:00 不算到第二天）
  const last = allDay ? addDays(allDayKeyOf(occurrence.end), -1) : dayKeyOf(new Date(new Date(occurrence.end).getTime() - 1));
  const days: DayKey[] = [];
  for (let key = first; key <= last && days.length < 62; key = addDays(key, 1)) days.push(key);
  return days.length ? days : [first];
}

// 月视图的格子：从这个月第一天所在那周的星期日，到最后一天所在那周的星期六
export function monthCells(year: number, month: number): DayKey[] {
  const first = new Date(year, month, 1);
  const start = new Date(year, month, 1 - first.getDay());
  const last = new Date(year, month + 1, 0);
  const end = new Date(year, month + 1, 0 + (6 - last.getDay()));
  const cells: DayKey[] = [];
  for (const date = start; date <= end; date.setDate(date.getDate() + 1)) cells.push(dayKeyOf(date));
  return cells;
}

// 一段日子在接口里的范围（这台设备时区的 00:00 起，到最后一天的第二天 00:00，不含）
export const rangeOf = (from: DayKey, toInclusive: DayKey) => ({
  from: dateOfKey(from).toISOString(),
  to: dateOfKey(addDays(toInclusive, 1)).toISOString(),
});
