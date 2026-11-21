-- 完整的对话记录：每人每一轮一行，按轮次编号（seq），只增不删（发给模型的仍是摘要 + 最近几轮，这里留原始材料）。
-- 每次保存对话时把最近几轮写进来：已有的那一轮只在被打断、改成主人听到的部分时跟着更新。
-- +goose Up
ALTER TABLE conversations ADD COLUMN turn_count INTEGER NOT NULL DEFAULT 0; -- 这个人的轮次编到第几了
ALTER TABLE turns ADD COLUMN seq INTEGER;                                     -- 这一轮的编号（完整记录里的 seq）
-- 已有的轮次按先后编号：掉出窗口、等压进摘要的在前，最近几轮在后
UPDATE turns SET seq = 1 + (SELECT COUNT(*) FROM turns AS earlier WHERE earlier.person = turns.person AND
	(CASE earlier.part WHEN 'unsummarized' THEN 0 ELSE 1 END, earlier.position) <
	(CASE turns.part WHEN 'unsummarized' THEN 0 ELSE 1 END, turns.position));
UPDATE conversations SET turn_count = (SELECT COUNT(*) FROM turns WHERE turns.person = conversations.person);
CREATE TABLE transcript (
	person   TEXT    NOT NULL,
	seq      INTEGER NOT NULL,
	at       INTEGER,          -- 主人说话的时间（Unix 秒）；NULL = 不知道
	messages TEXT    NOT NULL, -- 这一轮的消息（JSON，和 turns.messages 一样；不含图片）
	PRIMARY KEY (person, seq)
);
INSERT INTO transcript (person, seq, at, messages) SELECT person, seq, at, messages FROM turns;

-- +goose Down
DROP TABLE transcript;
ALTER TABLE turns DROP COLUMN seq;
ALTER TABLE conversations DROP COLUMN turn_count;
