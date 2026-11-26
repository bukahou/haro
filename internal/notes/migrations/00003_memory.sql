-- 记事并进记忆（v10 D2 c，2026-11-22 裁定）：标明记给谁、来源、要不要每轮记着；向量另存一张表。
-- 之前记的都算全家共用（person 为空）、主人让记的（source = owner）。
-- +goose Up
ALTER TABLE notes ADD COLUMN person TEXT NOT NULL DEFAULT '';      -- 声纹认出的名字；空 = 全家共用
ALTER TABLE notes ADD COLUMN source TEXT NOT NULL DEFAULT 'owner'; -- owner = 主人让记的；model = 模型觉得该记的
ALTER TABLE notes ADD COLUMN resident INTEGER NOT NULL DEFAULT 0;  -- 1 = 常驻（每轮附在情况说明里）
CREATE INDEX notes_person ON notes (person, deleted_at);
-- note_id 就是 notes.id（一条记事一个向量；库没开外键检查，不写 REFERENCES，由代码保证）
CREATE TABLE note_vectors (
	note_id INTEGER PRIMARY KEY,
	model   TEXT NOT NULL, -- 算向量用的模型：换了模型就重算
	vector  BLOB NOT NULL  -- float32 小端（embedding.EncodeVector）
);

-- +goose Down
DROP TABLE note_vectors;
DROP INDEX notes_person;
ALTER TABLE notes DROP COLUMN resident;
ALTER TABLE notes DROP COLUMN source;
ALTER TABLE notes DROP COLUMN person;
