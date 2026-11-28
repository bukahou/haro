-- 终端（第 15 段 15c）：每台终端属于一个账号，用 haro 发的设备凭证连进来（只存 SHA-256）。
-- 凭证在终端第一次轮询登记结果时才生成、只交出去一次（库里不存明文）：配对 / 批准之后、终端取走之前 credential_hash 为 NULL。
-- +goose Up
CREATE TABLE terminals (
	id              TEXT    PRIMARY KEY,           -- 随机编号
	account_id      TEXT    NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
	name            TEXT    NOT NULL,
	credential_hash BLOB    UNIQUE,
	created_at      INTEGER NOT NULL,              -- Unix 秒
	last_seen_at    INTEGER,
	last_remote     TEXT,                          -- 最近一次从哪里连来的（App 里显示局域网 / 外网）
	version         TEXT
);
CREATE INDEX terminals_account ON terminals (account_id);
-- 登记中的终端（照 RFC 8628：终端拿配对码给人看，自己拿轮询凭据定时问结果）。配对码、轮询凭据都只存 SHA-256。
CREATE TABLE terminal_registrations (
	id          TEXT    PRIMARY KEY,
	code_hash   BLOB    NOT NULL UNIQUE,
	poll_hash   BLOB    NOT NULL UNIQUE,
	name        TEXT    NOT NULL,                  -- 终端报的名字（主机名）
	version     TEXT,
	remote      TEXT,                              -- 从哪里连来的（给管理员看）
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	status      TEXT    NOT NULL,                  -- pending / approved / rejected
	terminal_id TEXT    REFERENCES terminals (id) ON DELETE CASCADE -- 配对 / 批准后建好的终端
);
