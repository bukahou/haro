-- 对话分线程（第 16 段）：一个人可以有好几条对话 —— 语音那条是线程 ''（原来的数据都归它），App 里新建的各是一个线程（随机编号）。
-- 轮次编号是每个人一套、所有线程共用（完整记录、向量、长期记忆的索引都还按（人, 编号）唯一，不用动），transcript 只多记一列线程。
-- SQLite 改不了主键：conversations、turns 建新表、原样搬数据、换名（goose 的迁移在事务里，失败整个回滚；迁移前库已备份）。
-- +goose Up
CREATE TABLE conversations_threaded (
	person          TEXT    NOT NULL,
	thread          TEXT    NOT NULL DEFAULT '',  -- '' = 语音那条
	persona_id      TEXT    NOT NULL DEFAULT '',
	summary         TEXT    NOT NULL DEFAULT '',
	tool_call_count INTEGER NOT NULL DEFAULT 0,
	turn_count      INTEGER NOT NULL DEFAULT 0,   -- 这条线程最近一轮的编号（编号是这个人所有线程共用的一套）
	open_turn       TEXT,
	updated_at      INTEGER NOT NULL,
	PRIMARY KEY (person, thread)
);
INSERT INTO conversations_threaded (person, thread, persona_id, summary, tool_call_count, turn_count, open_turn, updated_at)
	SELECT person, '', persona_id, summary, tool_call_count, turn_count, open_turn, updated_at FROM conversations;
CREATE TABLE turns_threaded (
	person   TEXT    NOT NULL,
	thread   TEXT    NOT NULL DEFAULT '',
	part     TEXT    NOT NULL CHECK (part IN ('history', 'unsummarized')),
	position INTEGER NOT NULL,
	at       INTEGER,
	messages TEXT    NOT NULL,
	seq      INTEGER,
	PRIMARY KEY (person, thread, part, position),
	FOREIGN KEY (person, thread) REFERENCES conversations_threaded (person, thread) ON DELETE CASCADE
);
INSERT INTO turns_threaded (person, thread, part, position, at, messages, seq)
	SELECT person, '', part, position, at, messages, seq FROM turns;
DROP TABLE turns;
DROP TABLE conversations;
ALTER TABLE conversations_threaded RENAME TO conversations;
ALTER TABLE turns_threaded RENAME TO turns;
ALTER TABLE transcript ADD COLUMN thread TEXT NOT NULL DEFAULT '';
-- 线程的信息（语音那条没有这一行）：标题（第一轮后后台起）、无痕（不进长期记忆的检索、不进背景记录池）、软删除
CREATE TABLE threads (
	person     TEXT    NOT NULL,
	thread     TEXT    NOT NULL,
	title      TEXT    NOT NULL DEFAULT '',
	incognito  INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	deleted_at INTEGER,                           -- 软删除的时间；NULL = 没删
	PRIMARY KEY (person, thread)
);
