-- 对话：每人一行（性格、摘要、工具调用编号、还没收尾的一轮），每一轮一行（说话的时间 + 这一轮的消息）。
-- +goose Up
CREATE TABLE conversations (
	person          TEXT    PRIMARY KEY,
	persona_id      TEXT    NOT NULL DEFAULT '',
	summary         TEXT    NOT NULL DEFAULT '',  -- 滚动摘要
	tool_call_count INTEGER NOT NULL DEFAULT 0,   -- 生成对话内唯一的工具调用编号
	open_turn       TEXT,                         -- 还没收尾的一轮（JSON）；NULL = 没有
	updated_at      INTEGER NOT NULL              -- Unix 秒
);
CREATE TABLE turns (
	person   TEXT    NOT NULL REFERENCES conversations (person) ON DELETE CASCADE,
	part     TEXT    NOT NULL CHECK (part IN ('history', 'unsummarized')), -- 最近几轮 / 掉出窗口、等压进摘要的
	position INTEGER NOT NULL,                    -- 在这一部分里的顺序（0 起）
	at       INTEGER,                             -- 主人说话的时间（Unix 秒）；NULL = 不知道（开始记时间之前的旧记录）
	messages TEXT    NOT NULL,                    -- 这一轮的消息（JSON：主人的话、工具往来、回答；不含图片）
	PRIMARY KEY (person, part, position)
);
