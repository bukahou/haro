-- 主人口头确认后临时提高的月度上限：每次一行，这个月的上限 = 配置的上限 + 这个月提高的合计（下个月自然回到配置的数）。
-- +goose Up
CREATE TABLE cap_raises (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	at     INTEGER NOT NULL,           -- Unix 秒
	yen    REAL    NOT NULL,           -- 这次提高了多少
	person TEXT    NOT NULL DEFAULT '' -- 谁确认的（对话名）
);
CREATE INDEX cap_raises_at ON cap_raises (at);
