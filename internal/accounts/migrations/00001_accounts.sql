-- 账号与登录令牌（第 15 段）。账号下面挂两个「人」：本人（owner_person）、访客（guest_person）；对话、记忆、提醒等存储按人存，
-- 每个请求先解析成账号，再只用它的这两个人取数据。管理员账号的人就是原来的声纹名和全家共用（现有数据不用改）。
-- +goose Up
CREATE TABLE accounts (
	id               TEXT    PRIMARY KEY,           -- 随机编号（不是自增：接口里露出去，不让人猜出有几个账号）
	subject          TEXT    UNIQUE,                -- akasha 的 sub；匿名终端的账号为 NULL
	display_name     TEXT    NOT NULL,
	role             TEXT    NOT NULL,              -- admin / member（管理员由配置定，启动时校正）
	status           TEXT    NOT NULL,              -- active / pending_approval / disabled
	joined_via       TEXT    NOT NULL,              -- admin / invite / approval / terminal
	default_currency TEXT    NOT NULL DEFAULT '',   -- ISO 4217；空 = 用服务端的默认货币
	owner_person     TEXT    NOT NULL UNIQUE,
	guest_person     TEXT    NOT NULL UNIQUE,
	created_at       INTEGER NOT NULL,              -- Unix 秒
	last_used_at     INTEGER,
	app_version      TEXT,
	disabled_at      INTEGER
);
-- 登录令牌：只存 SHA-256。同一次登录换出来的令牌是一串（family），刷新令牌被重复使用时整串作废（判定为被盗）。
CREATE TABLE tokens (
	hash        BLOB    PRIMARY KEY,
	kind        TEXT    NOT NULL,                   -- access / refresh
	account_id  TEXT    NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
	family      TEXT    NOT NULL,
	expires_at  INTEGER NOT NULL,
	used_at     INTEGER,                            -- 刷新令牌用过的时间（用一次就换新）
	revoked_at  INTEGER
);
CREATE INDEX tokens_family ON tokens (family);
CREATE INDEX tokens_account ON tokens (account_id);
