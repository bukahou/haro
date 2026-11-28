-- 记账（第 19 段）：每个人的账目和交上来的小票。和 haro 自己的花费账本（模型、搜索的费用）是两回事，分开存。
-- 金额都是 micros（一个货币单位的百万分之一）+ ISO 4217 币种（和 internal/money 同一套）；时间是 UTC 的 Unix 秒。
-- +goose Up
CREATE TABLE receipts (
	id            TEXT    PRIMARY KEY,
	person        TEXT    NOT NULL,
	attachment_id TEXT    NOT NULL,              -- 原图（internal/attachments 的文件）
	category_hint TEXT    NOT NULL DEFAULT '',    -- 上传时主人点的分类
	note_hint     TEXT    NOT NULL DEFAULT '',    -- 上传时主人写的一句说明
	status        TEXT    NOT NULL,              -- parsing / ready / failed / confirmed
	draft         TEXT,                          -- 解析出的草稿（JSON）
	failure       TEXT    NOT NULL DEFAULT '',    -- 解析失败的原因（错误码）
	expense_id    TEXT,                          -- 确认后入的那笔账
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE INDEX receipts_person ON receipts (person, created_at);
CREATE TABLE expenses (
	id             TEXT    PRIMARY KEY,
	person         TEXT    NOT NULL,
	merchant       TEXT    NOT NULL,
	spent_at       INTEGER NOT NULL,
	currency       TEXT    NOT NULL,
	total_micros   INTEGER NOT NULL,
	subtotal_micros INTEGER,
	tax_micros     INTEGER,
	payment_method TEXT    NOT NULL DEFAULT '',
	category       TEXT    NOT NULL DEFAULT '',
	note           TEXT    NOT NULL DEFAULT '',
	items          TEXT    NOT NULL DEFAULT '[]', -- 明细行（JSON）
	attachment_id  TEXT,                          -- 原图；手记的没有
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX expenses_person ON expenses (person, spent_at);
-- 主人自己加的分类（固定的几类在代码里）
CREATE TABLE categories (
	person     TEXT    NOT NULL,
	key        TEXT    NOT NULL,
	name       TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (person, key)
);
