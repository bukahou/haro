-- 记事：每条一行；删掉只打标记（deleted_at），可以恢复。
-- 用 IF NOT EXISTS：改用迁移之前建的记事库已经有这张表，这一步对它们什么都不做（记成已是第 1 版）。
-- +goose Up
CREATE TABLE IF NOT EXISTS notes (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	content    TEXT    NOT NULL,
	created_at TEXT    NOT NULL, -- RFC 3339
	deleted_at TEXT              -- RFC 3339；NULL = 没删
);
CREATE INDEX IF NOT EXISTS notes_created_at ON notes (created_at);
