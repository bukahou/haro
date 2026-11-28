-- App 里发的图片（第 16 段 16b）：图片文件在 HARO_ATTACHMENTS_DIR（internal/attachments），这里记谁的、哪条对话的。
-- +goose Up
CREATE TABLE attachments (
	id         TEXT    PRIMARY KEY,                -- 随机编号（也是文件名）
	person     TEXT    NOT NULL,
	thread     TEXT    NOT NULL,                   -- 哪条对话（'' = 语音那条）
	width      INTEGER NOT NULL,
	height     INTEGER NOT NULL,
	bytes      INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX attachments_thread ON attachments (person, thread);
