-- 被打断、出错的调用也记账：加「不完整」标记，token 允许为空（不知道就是 NULL，不记成 0）。
-- SQLite 改不了列的 NOT NULL：照新结构建一张表、搬数据、换名。
-- +goose Up
CREATE TABLE charges_new (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	at                 INTEGER NOT NULL, -- Unix 秒
	purpose            TEXT    NOT NULL,
	item               TEXT    NOT NULL,
	input_tokens       INTEGER,          -- 输入的总数（含缓存读、缓存写）；NULL = 不知道
	output_tokens      INTEGER,          -- NULL = 不知道（被打断时 Claude 还没报输出数）
	cache_read_tokens  INTEGER,
	cache_write_tokens INTEGER,
	credits            INTEGER NOT NULL DEFAULT 0, -- 搜索服务的积分
	yen                REAL    NOT NULL DEFAULT 0, -- 只按知道的部分算
	incomplete         INTEGER NOT NULL DEFAULT 0  -- 1 = 调用被打断或出错，用量只知道一部分
);
INSERT INTO charges_new (id, at, purpose, item, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, credits, yen)
	SELECT id, at, purpose, item, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, credits, yen FROM charges;
DROP TABLE charges;
ALTER TABLE charges_new RENAME TO charges;
CREATE INDEX charges_at ON charges (at);
