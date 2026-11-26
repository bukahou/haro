-- 完整对话记录的向量（v10 D5：以前的对话也能按意思找）：一轮一个；这一轮的文字后来变了（被打断、改成主人听到的部分），
-- 存下的摘要对不上，就重算。
-- +goose Up
CREATE TABLE transcript_vectors (
	person TEXT    NOT NULL,
	seq    INTEGER NOT NULL,
	model  TEXT    NOT NULL, -- 算向量用的模型：换了模型就重算
	digest TEXT    NOT NULL, -- 算向量时这一轮文字的摘要（sha256，十六进制）
	vector BLOB    NOT NULL, -- float32 小端（embedding.EncodeVector）
	PRIMARY KEY (person, seq),
	FOREIGN KEY (person, seq) REFERENCES transcript (person, seq) -- 对话库开着外键检查
);

-- +goose Down
DROP TABLE transcript_vectors;
