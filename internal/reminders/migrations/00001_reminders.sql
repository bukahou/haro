-- 提醒：每条一行；取消只打标记（cancelled_at），可以恢复。时间都是 UTC 的 Unix 秒。
-- 和记事本在同一个库文件里，版本记在自己的表（goose_db_version_reminders），两边的迁移互不影响。
-- +goose Up
CREATE TABLE reminders (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	person       TEXT    NOT NULL, -- 定的人（声纹认出的名字）：到点说给他听，只有他能查、取消、恢复
	text         TEXT    NOT NULL, -- 到点要说的话（模型交的完整一句，直接合成，不经模型）
	rrule        TEXT,             -- 重复规则（RFC 5545 RRULE）；NULL = 一次性
	starts_at    INTEGER NOT NULL, -- 第一次到点（重复规则的起点）
	next_at      INTEGER,          -- 下一次到点；NULL = 不会再到点（一次性的说过了或错过了、重复的到头了）
	created_at   INTEGER NOT NULL,
	cancelled_at INTEGER,          -- 取消的时间；NULL = 在用
	spoken_at    INTEGER,          -- 最近一次说出来的时间
	missed_at    INTEGER           -- 错过了（那时没能说出来）、还没告诉主人的那一次到点；NULL = 没有
);
CREATE INDEX reminders_due ON reminders (next_at) WHERE cancelled_at IS NULL;
CREATE INDEX reminders_person ON reminders (person, cancelled_at);
