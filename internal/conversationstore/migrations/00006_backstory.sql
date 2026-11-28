-- 背景记录池（第 16 段 16c）：后台把一个人的对话（无痕的、删了的除外）压成几条「关于主人的背景」，每轮带给模型。
-- 主人在 App 里改过的条目（edited）后台不改、不删；merged_seq 是并到这个人的第几轮了。
-- +goose Up
CREATE TABLE backstory_items (
	id            TEXT    PRIMARY KEY,
	person        TEXT    NOT NULL,
	text          TEXT    NOT NULL,
	source_thread TEXT,                           -- 从哪条对话的哪一轮来（后台合并时代理给的；不知道时为 NULL）
	source_seq    INTEGER,
	edited        INTEGER NOT NULL DEFAULT 0,     -- 主人在 App 里改过
	position      INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE INDEX backstory_items_person ON backstory_items (person, position);
CREATE TABLE backstory_state (
	person          TEXT    PRIMARY KEY,
	merged_seq      INTEGER NOT NULL DEFAULT 0,
	enabled         INTEGER NOT NULL DEFAULT 1,   -- App 里可以整个关掉：关了就不带、也不更新
	consolidated_at INTEGER                       -- 最近一次整理（合并重复、去掉过时的）的时间
);
