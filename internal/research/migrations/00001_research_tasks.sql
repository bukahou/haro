-- 后台调查任务：每个任务一行。
-- 用 IF NOT EXISTS：改用迁移之前建的任务库已经有这张表，这一步对它们什么都不做（记成已是第 1 版）。
-- +goose Up
CREATE TABLE IF NOT EXISTS research_tasks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	person      TEXT    NOT NULL,
	question    TEXT    NOT NULL,
	status      TEXT    NOT NULL,
	conclusion  TEXT    NOT NULL DEFAULT '',
	sources     TEXT    NOT NULL DEFAULT '', -- 换行分隔
	stop_reason TEXT    NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,            -- Unix 秒
	finished_at INTEGER NOT NULL DEFAULT 0,  -- Unix 秒；0 = 还没结束
	reported    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS research_tasks_person ON research_tasks (person, id);
