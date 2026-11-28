-- 日历（第 18 段）：日程要在终端响的提醒存在这张表里，每一行记属于哪条日程、提前多久；这种提醒存成「下一次到点」的单次提醒
-- （重复规则的展开只在日历一处），响完 / 错过之后问日历要再下一次。原来的提醒启动时由日历收编（建日程、挂上 event_id）。
-- +goose Up
ALTER TABLE reminders ADD COLUMN event_id TEXT;                          -- 属于哪条日程；NULL = 还没收编的旧提醒
ALTER TABLE reminders ADD COLUMN lead_seconds INTEGER NOT NULL DEFAULT 0; -- 比日程开始提前多少秒
CREATE INDEX reminders_event ON reminders (event_id) WHERE event_id IS NOT NULL;
