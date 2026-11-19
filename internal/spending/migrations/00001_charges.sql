-- 用量账本：每一笔花费一行（大模型调用、搜索积分）。
-- 用 IF NOT EXISTS：改用迁移之前建的账本已经有这张表，这一步对它们什么都不做。
-- +goose Up
CREATE TABLE IF NOT EXISTS charges (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	at                 INTEGER NOT NULL, -- Unix 秒
	purpose            TEXT    NOT NULL,
	item               TEXT    NOT NULL,
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	credits            INTEGER NOT NULL DEFAULT 0,
	yen                REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS charges_at ON charges (at);
