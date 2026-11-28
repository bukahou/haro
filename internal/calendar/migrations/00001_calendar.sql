-- 日历（第 18 段）：每个人的日程。和提醒、记事在同一个库文件里，版本记在自己的表（goose_db_version_calendar）。
-- 时间都是 UTC 的 Unix 秒；全天的日程存那一天 00:00 UTC（只当日期用），结束是第二天（不含）。
-- +goose Up
CREATE TABLE events (
	id             TEXT    PRIMARY KEY,
	person         TEXT    NOT NULL,
	title          TEXT    NOT NULL,
	location       TEXT    NOT NULL DEFAULT '',
	notes          TEXT    NOT NULL DEFAULT '',
	starts_at      INTEGER NOT NULL,
	ends_at        INTEGER NOT NULL,
	all_day        INTEGER NOT NULL DEFAULT 0,
	rrule          TEXT,                          -- 重复规则（RFC 5545 RRULE 一行）；NULL = 一次
	exdates        TEXT    NOT NULL DEFAULT '[]', -- 排除的那几次的开始时间（JSON，Unix 秒）：「只改这一次」时把那一次排除
	alerts         TEXT    NOT NULL DEFAULT '[]', -- 提醒：提前多少秒（JSON）
	alert_terminal INTEGER NOT NULL DEFAULT 1,    -- 在终端响
	alert_phone    INTEGER NOT NULL DEFAULT 1,    -- 在手机响
	source         TEXT    NOT NULL,              -- haro（haro 自己的，能改）/ system（从系统日历同步进来的，只读）
	calendar       TEXT    NOT NULL DEFAULT '',   -- 系统日历的编号（source = system）
	calendar_name  TEXT    NOT NULL DEFAULT '',
	external_id    TEXT    NOT NULL DEFAULT '',   -- 系统给的编号（同步时去重）
	origin         TEXT    NOT NULL DEFAULT '',   -- reminder = 语音定提醒时建的（取消提醒时跟着删）
	language       TEXT    NOT NULL DEFAULT '',   -- 建的时候的语言（到点说的话按它选固定句式）
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL,
	deleted_at     INTEGER                        -- 软删除；NULL = 在用
);
CREATE INDEX events_person ON events (person, deleted_at);
CREATE UNIQUE INDEX events_external ON events (person, calendar, external_id) WHERE source = 'system';
-- 系统日历最近一次同步（设置里显示上次同步的时间）
CREATE TABLE calendar_syncs (
	person    TEXT    NOT NULL,
	calendar  TEXT    NOT NULL,
	name      TEXT    NOT NULL,
	synced_at INTEGER NOT NULL,
	PRIMARY KEY (person, calendar)
);
