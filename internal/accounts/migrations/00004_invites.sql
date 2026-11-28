-- 第 20 段：邀请码、软删除。
-- +goose Up
-- 邀请码：管理员生成，有时效、用一次；只存 SHA-256（码只在生成时给一次）
CREATE TABLE invites (
	id          TEXT    PRIMARY KEY,                -- 随机编号（列出、撤销用；不是码）
	code_hash   BLOB    NOT NULL UNIQUE,
	note        TEXT    NOT NULL DEFAULT '',        -- 管理员写的备注（给谁的）
	created_by  TEXT    NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	used_at     INTEGER,
	used_by     TEXT    REFERENCES accounts (id) ON DELETE SET NULL,
	revoked_at  INTEGER
);
-- 软删除：到这个时间彻底删掉这个账号和它的数据；NULL = 没删
ALTER TABLE accounts ADD COLUMN deletion_due_at INTEGER;
