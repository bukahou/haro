-- 每个账号的主人资料和家（第 15 段 15b）。没有这一行 = 这个账号没在 App 里设过（管理员账号这时用服务端的配置）。
-- +goose Up
CREATE TABLE profiles (
	account_id TEXT    PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
	household  TEXT    NOT NULL,                    -- 全家（这个账号家里的人、认不出声音的也带上）
	owner      TEXT    NOT NULL,                    -- 本人（认出是账号本人时才带上）
	updated_at INTEGER NOT NULL                     -- Unix 秒
);
CREATE TABLE homes (
	account_id       TEXT    PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
	place_name       TEXT,                          -- 家的位置；整组为 NULL = 没设（问天气时要说地名）
	place_region     TEXT,
	place_country    TEXT,                          -- 国家名（按设的时候的语言）
	country_code     TEXT,                          -- ISO 3166-1 两位
	latitude         REAL,
	longitude        REAL,
	holidays_country TEXT,                          -- 告诉 haro 哪个国家的节假日；NULL = 不告诉
	updated_at       INTEGER NOT NULL
);
